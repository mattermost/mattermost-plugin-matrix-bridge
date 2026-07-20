package main

import (
	"bytes"
	"strconv"
	"strings"

	"github.com/pkg/errors"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
)

// MigrationResult holds the results of a migration operation
type MigrationResult struct {
	UserMappingsCreated      int
	ChannelMappingsCreated   int
	RoomMappingsCreated      int
	DMMappingsCreated        int
	ReverseDMMappingsCreated int
}

const (
	// MigrationBatchSize is the number of keys to process in each batch
	MigrationBatchSize = 1000
)

// runKVStoreMigrations checks the KV store version and runs necessary migrations
func (p *Plugin) runKVStoreMigrations() error {
	_, err := p.runKVStoreMigrationsWithResults()
	return err
}

// runKVStoreMigrationsWithResults checks the KV store version and runs necessary migrations, returning detailed results
func (p *Plugin) runKVStoreMigrationsWithResults() (*MigrationResult, error) {
	// Get current KV store version
	versionBytes, err := p.kvstore.Get(kvstore.KeyStoreVersion)
	currentVersion := 0
	if err == nil && len(versionBytes) > 0 {
		if version, parseErr := strconv.Atoi(string(versionBytes)); parseErr == nil {
			currentVersion = version
		}
	}

	p.logger.LogInfo("Checking KV store migrations", "current_version", currentVersion, "target_version", kvstore.CurrentKVStoreVersion)

	result := &MigrationResult{}

	// Run migrations if needed
	if currentVersion < kvstore.CurrentKVStoreVersion {
		p.logger.LogInfo("Running KV store migrations", "from_version", currentVersion, "target_version", kvstore.CurrentKVStoreVersion)

		if currentVersion < 1 {
			v1Result, err := p.runMigrationToVersion1WithResults()
			if err != nil {
				return nil, errors.Wrap(err, "failed to migrate to version 1")
			}
			result.UserMappingsCreated += v1Result.UserMappingsCreated
			result.ChannelMappingsCreated += v1Result.ChannelMappingsCreated
			result.RoomMappingsCreated += v1Result.RoomMappingsCreated
		}

		if currentVersion < 2 {
			v2Result, err := p.runMigrationToVersion2WithResults()
			if err != nil {
				return nil, errors.Wrap(err, "failed to migrate to version 2")
			}
			result.DMMappingsCreated += v2Result.DMMappingsCreated
			result.ReverseDMMappingsCreated += v2Result.ReverseDMMappingsCreated
		}

		if currentVersion < 3 {
			v3Result, err := p.runMigrationToVersion3WithResults()
			if err != nil {
				return nil, errors.Wrap(err, "failed to migrate to version 3")
			}
			result.ChannelMappingsCreated += v3Result.ChannelMappingsCreated
		}

		// Update version marker
		if err := p.kvstore.Set(kvstore.KeyStoreVersion, []byte(strconv.Itoa(kvstore.CurrentKVStoreVersion))); err != nil {
			return nil, errors.Wrap(err, "failed to update KV store version")
		}

		p.logger.LogInfo("KV store migrations completed successfully", "new_version", kvstore.CurrentKVStoreVersion)
	} else {
		p.logger.LogDebug("KV store is up to date", "version", currentVersion)
	}

	return result, nil
}

// runMigrationToVersion1WithResults migrates to version 1: adds reverse mappings for users and channels, returning results
func (p *Plugin) runMigrationToVersion1WithResults() (*MigrationResult, error) {
	p.logger.LogInfo("Running migration to version 1: adding reverse mappings")

	result := &MigrationResult{}

	// Migrate user mappings
	userResult, err := p.migrateUserMappingsWithResults()
	if err != nil {
		return nil, errors.Wrap(err, "failed to migrate user mappings")
	}
	result.UserMappingsCreated = userResult.UserMappingsCreated

	// Migrate channel mappings
	channelResult, err := p.migrateChannelMappingsWithResults()
	if err != nil {
		return nil, errors.Wrap(err, "failed to migrate channel mappings")
	}
	result.ChannelMappingsCreated = channelResult.ChannelMappingsCreated
	result.RoomMappingsCreated = channelResult.RoomMappingsCreated

	return result, nil
}

// migrateUserMappingsWithResults creates reverse mappings for existing user mappings, returning results
func (p *Plugin) migrateUserMappingsWithResults() (*MigrationResult, error) {
	p.logger.LogInfo("Migrating user mappings to add reverse lookups")

	userMappingPrefix := kvstore.KeyPrefixMatrixUser
	// Keys already namespaced by v3 must be skipped so this legacy repair is a
	// no-op when re-run over migrated data (e.g. via the /matrix migrate command).
	namespace := p.serverIDNamespace()
	totalMigratedCount := 0
	page := 0

	for {
		// Get keys in batches using prefix filtering for efficiency
		keys, err := p.kvstore.ListKeysWithPrefix(page, MigrationBatchSize, userMappingPrefix)
		if err != nil {
			return nil, errors.Wrap(err, "failed to list KV store keys with prefix")
		}

		if len(keys) == 0 {
			break // No more keys
		}

		batchMigratedCount := 0
		batchSkippedCount := 0
		batchProcessedCount := 0
		for _, key := range keys {
			// No need to check prefix since ListKeysWithPrefix already filters
			{
				batchProcessedCount++

				// Get the Mattermost user ID
				mattermostUserIDBytes, err := p.kvstore.Get(key)
				if err != nil {
					p.logger.LogWarn("Failed to get user mapping during migration", "key", key, "error", err)
					continue
				}

				mattermostUserID := string(mattermostUserIDBytes)
				matrixUserID := strings.TrimPrefix(key, userMappingPrefix)

				// Skip keys already namespaced by v3; deriving a reverse mapping from
				// them would embed the serverID in the value and corrupt the record.
				if namespace != "" && strings.HasPrefix(matrixUserID, namespace) {
					continue
				}

				// Create reverse mapping: mattermost_user_<mattermostUserID> -> matrixUserID.
				// v1/v2 reconstruct the historical (un-namespaced) key layout; the v3
				// migration is the single authority that namespaces legacy keys by
				// serverID. (New runtime writes are namespaced directly by the key
				// builders in the kvstore package.)
				reverseKey := kvstore.KeyPrefixMattermostUser + mattermostUserID

				// Check if reverse mapping already exists with correct value
				existingData, err := p.kvstore.Get(reverseKey)
				if err == nil && bytes.Equal(existingData, []byte(matrixUserID)) {
					batchSkippedCount++
					continue // Already correct, skip
				}

				// Create/update the reverse mapping (overwrites incorrect values)
				if err := p.kvstore.Set(reverseKey, []byte(matrixUserID)); err != nil {
					p.logger.LogWarn("Failed to create/update reverse user mapping during migration", "mattermost_user_id", mattermostUserID, "matrix_user_id", matrixUserID, "error", err)
					continue
				}

				batchMigratedCount++
				if err == nil && len(existingData) > 0 {
					p.logger.LogDebug("Updated incorrect reverse user mapping", "mattermost_user_id", mattermostUserID, "matrix_user_id", matrixUserID, "old_value", string(existingData))
				} else {
					p.logger.LogDebug("Created reverse user mapping", "mattermost_user_id", mattermostUserID, "matrix_user_id", matrixUserID)
				}
			}
		}

		totalMigratedCount += batchMigratedCount
		p.logger.LogDebug("Processed user mapping batch", "page", page, "batch_size", len(keys), "processed_in_batch", batchProcessedCount, "migrated_in_batch", batchMigratedCount, "skipped_in_batch", batchSkippedCount)

		// If we got fewer keys than the batch size, we've reached the end
		if len(keys) < MigrationBatchSize {
			break
		}

		page++
	}

	p.logger.LogInfo("User mapping migration completed", "total_migrated", totalMigratedCount, "pages_processed", page+1)
	return &MigrationResult{UserMappingsCreated: totalMigratedCount}, nil
}

// migrateChannelMappingsWithResults creates reverse mappings for existing channel mappings, returning results
func (p *Plugin) migrateChannelMappingsWithResults() (*MigrationResult, error) {
	p.logger.LogInfo("Migrating channel mappings to add reverse lookups")

	channelMappingPrefix := kvstore.KeyPrefixChannelMapping
	totalMigratedCount := 0
	totalRoomMappingsCount := 0
	page := 0

	for {
		// Get keys in batches using prefix filtering for efficiency
		keys, err := p.kvstore.ListKeysWithPrefix(page, MigrationBatchSize, channelMappingPrefix)
		if err != nil {
			return nil, errors.Wrap(err, "failed to list KV store keys with prefix")
		}

		if len(keys) == 0 {
			break // No more keys
		}

		batchMigratedCount := 0
		batchSkippedCount := 0
		batchProcessedCount := 0
		for _, key := range keys {
			// No need to check prefix since ListKeysWithPrefix already filters
			{
				batchProcessedCount++

				// Get the room identifier (alias or room ID)
				roomIdentifierBytes, err := p.kvstore.Get(key)
				if err != nil {
					p.logger.LogWarn("Failed to get channel mapping during migration", "key", key, "error", err)
					continue
				}

				// Skip values already converted to the v3 []ChannelServerMapping shape;
				// treating that JSON as a room identifier would create a garbage reverse
				// mapping. Keeps this legacy repair a no-op when re-run over migrated data.
				if mappings, perr := kvstore.ParseChannelServerMappings(roomIdentifierBytes); perr == nil && len(mappings) > 0 {
					continue
				}

				roomIdentifier := string(roomIdentifierBytes)
				channelID := strings.TrimPrefix(key, channelMappingPrefix)

				// Create reverse mapping: room_mapping_<roomIdentifier> -> channelID
				reverseKey := kvstore.KeyPrefixRoomMapping + roomIdentifier

				// Check if reverse mapping already exists with correct value
				existingData, err := p.kvstore.Get(reverseKey)
				if err == nil && bytes.Equal(existingData, []byte(channelID)) {
					batchSkippedCount++
				} else {
					// Create/update the reverse mapping (overwrites incorrect values)
					if err := p.kvstore.Set(reverseKey, []byte(channelID)); err != nil {
						p.logger.LogWarn("Failed to create/update reverse channel mapping during migration", "channel_id", channelID, "room_identifier", roomIdentifier, "error", err)
					} else {
						batchMigratedCount++
						if len(existingData) > 0 {
							p.logger.LogDebug("Updated incorrect reverse channel mapping", "channel_id", channelID, "room_identifier", roomIdentifier, "old_value", string(existingData))
						} else {
							p.logger.LogDebug("Created reverse channel mapping", "channel_id", channelID, "room_identifier", roomIdentifier)
						}
					}
				}

				// Always try room ID mapping for aliases, regardless of reverse mapping result
				matrixClient := p.GetMatrixClient()
				if strings.HasPrefix(roomIdentifier, "#") && matrixClient != nil {
					if resolvedRoomID, resolveErr := matrixClient.ResolveRoomAlias(roomIdentifier); resolveErr == nil {
						roomIDKey := kvstore.KeyPrefixRoomMapping + resolvedRoomID

						// Always update room ID mapping to match alias mapping
						if err := p.kvstore.Set(roomIDKey, []byte(channelID)); err != nil {
							p.logger.LogWarn("Failed to create/update room ID mapping during migration", "channel_id", channelID, "room_id", resolvedRoomID, "error", err)
						} else {
							totalRoomMappingsCount++
							p.logger.LogDebug("Created/updated room ID mapping", "channel_id", channelID, "room_id", resolvedRoomID)
						}
					} else {
						p.logger.LogWarn("Failed to resolve room alias during migration", "room_alias", roomIdentifier, "error", resolveErr)
					}
				}
			}
		}

		totalMigratedCount += batchMigratedCount
		p.logger.LogDebug("Processed channel mapping batch", "page", page, "batch_size", len(keys), "processed_in_batch", batchProcessedCount, "migrated_in_batch", batchMigratedCount, "skipped_in_batch", batchSkippedCount)

		// If we got fewer keys than the batch size, we've reached the end
		if len(keys) < MigrationBatchSize {
			break
		}

		page++
	}

	p.logger.LogInfo("Channel mapping migration completed", "total_migrated", totalMigratedCount, "room_mappings_created", totalRoomMappingsCount, "pages_processed", page+1)
	return &MigrationResult{ChannelMappingsCreated: totalMigratedCount, RoomMappingsCreated: totalRoomMappingsCount}, nil
}

// runMigrationToVersion2WithResults migrates to version 2: unify DM and regular channel mappings, returning results
func (p *Plugin) runMigrationToVersion2WithResults() (*MigrationResult, error) {
	p.logger.LogInfo("Running migration to version 2: unifying DM and channel mappings")

	// Migrate DM mappings to use unified channel_mapping_ prefix
	result, err := p.migrateDMMappingsWithResults()
	if err != nil {
		return nil, errors.Wrap(err, "failed to migrate DM mappings")
	}

	return result, nil
}

// migrateDMMappingsWithResults moves DM mappings from dm_mapping_ prefix to channel_mapping_ prefix, returning results
func (p *Plugin) migrateDMMappingsWithResults() (*MigrationResult, error) {
	p.logger.LogInfo("Migrating DM mappings to unified channel mapping prefix")

	dmMappingPrefix := kvstore.KeyPrefixLegacyDMMapping
	matrixDMMappingPrefix := kvstore.KeyPrefixLegacyMatrixDMMapping
	totalMigratedCount := 0
	totalReverseMigratedCount := 0

	// First, migrate dm_mapping_ keys
	page := 0
	for {
		// Get keys in batches using prefix filtering for efficiency
		keys, err := p.kvstore.ListKeysWithPrefix(page, MigrationBatchSize, dmMappingPrefix)
		if err != nil {
			return nil, errors.Wrap(err, "failed to list KV store keys with prefix")
		}

		if len(keys) == 0 {
			break // No more keys
		}

		batchMigratedCount := 0
		batchReverseMigratedCount := 0
		batchProcessedCount := 0
		for _, key := range keys {
			batchProcessedCount++

			// Get the Matrix room ID
			matrixRoomIDBytes, err := p.kvstore.Get(key)
			if err != nil {
				p.logger.LogWarn("Failed to get DM mapping during migration", "key", key, "error", err)
				continue
			}

			matrixRoomID := string(matrixRoomIDBytes)
			channelID := strings.TrimPrefix(key, dmMappingPrefix)

			// Create unified mapping: channel_mapping_<channelID> -> matrixRoomID
			unifiedKey := kvstore.BuildChannelMappingKey(channelID)

			// Check if unified mapping already exists
			existingData, err := p.kvstore.Get(unifiedKey)
			if err == nil && len(existingData) > 0 {
				p.logger.LogDebug("Unified mapping already exists, skipping", "channel_id", channelID, "matrix_room_id", matrixRoomID)
			} else {
				// Create the unified mapping
				if err := p.kvstore.Set(unifiedKey, []byte(matrixRoomID)); err != nil {
					p.logger.LogWarn("Failed to create unified DM mapping during migration", "channel_id", channelID, "matrix_room_id", matrixRoomID, "error", err)
					continue
				}
				batchMigratedCount++
				p.logger.LogDebug("Created unified DM mapping", "channel_id", channelID, "matrix_room_id", matrixRoomID)
			}

			// Also create reverse mapping for room_mapping_ if it doesn't exist
			reverseKey := kvstore.KeyPrefixRoomMapping + matrixRoomID
			existingReverse, err := p.kvstore.Get(reverseKey)
			if err != nil || len(existingReverse) == 0 {
				if err := p.kvstore.Set(reverseKey, []byte(channelID)); err != nil {
					p.logger.LogWarn("Failed to create reverse DM mapping during migration", "matrix_room_id", matrixRoomID, "channel_id", channelID, "error", err)
				} else {
					batchReverseMigratedCount++
					p.logger.LogDebug("Created reverse DM mapping", "matrix_room_id", matrixRoomID, "channel_id", channelID)
				}
			}

			// Remove old DM mapping
			if err := p.kvstore.Delete(key); err != nil {
				p.logger.LogWarn("Failed to delete old DM mapping during migration", "key", key, "error", err)
			} else {
				p.logger.LogDebug("Deleted old DM mapping", "key", key)
			}
		}

		totalMigratedCount += batchMigratedCount
		totalReverseMigratedCount += batchReverseMigratedCount

		p.logger.LogDebug("Migrated DM batch", "page", page, "processed", batchProcessedCount, "migrated", batchMigratedCount, "reverse_migrated", batchReverseMigratedCount)

		// If we got fewer keys than the batch size, we've reached the end
		if len(keys) < MigrationBatchSize {
			break
		}

		page++
	}

	// Second, migrate matrix_dm_mapping_ keys
	page = 0
	for {
		// Get keys in batches using prefix filtering for efficiency
		keys, err := p.kvstore.ListKeysWithPrefix(page, MigrationBatchSize, matrixDMMappingPrefix)
		if err != nil {
			return nil, errors.Wrap(err, "failed to list KV store keys with prefix")
		}

		if len(keys) == 0 {
			break // No more keys
		}

		batchReverseMigratedCount := 0
		batchProcessedCount := 0
		for _, key := range keys {
			// Also migrate reverse DM mappings from matrix_dm_mapping_ to room_mapping_
			batchProcessedCount++

			// Get the channel ID
			channelIDBytes, err := p.kvstore.Get(key)
			if err != nil {
				p.logger.LogWarn("Failed to get reverse DM mapping during migration", "key", key, "error", err)
				continue
			}

			channelID := string(channelIDBytes)
			matrixRoomID := strings.TrimPrefix(key, matrixDMMappingPrefix)

			// Create unified reverse mapping: room_mapping_<matrixRoomID> -> channelID
			unifiedReverseKey := kvstore.KeyPrefixRoomMapping + matrixRoomID

			// Check if unified reverse mapping already exists
			existingReverseData, err := p.kvstore.Get(unifiedReverseKey)
			if err == nil && len(existingReverseData) > 0 {
				p.logger.LogDebug("Unified reverse mapping already exists, skipping", "matrix_room_id", matrixRoomID, "channel_id", channelID)
			} else {
				// Create the unified reverse mapping
				if err := p.kvstore.Set(unifiedReverseKey, []byte(channelID)); err != nil {
					p.logger.LogWarn("Failed to create unified reverse DM mapping during migration", "matrix_room_id", matrixRoomID, "channel_id", channelID, "error", err)
					continue
				}
				batchReverseMigratedCount++
				p.logger.LogDebug("Created unified reverse DM mapping", "matrix_room_id", matrixRoomID, "channel_id", channelID)
			}

			// Remove old reverse DM mapping
			if err := p.kvstore.Delete(key); err != nil {
				p.logger.LogWarn("Failed to delete old reverse DM mapping during migration", "key", key, "error", err)
			} else {
				p.logger.LogDebug("Deleted old reverse DM mapping", "key", key)
			}
		}

		totalReverseMigratedCount += batchReverseMigratedCount
		p.logger.LogDebug("Processed reverse DM mapping batch", "page", page, "batch_size", len(keys), "processed_in_batch", batchProcessedCount, "reverse_migrated_in_batch", batchReverseMigratedCount)

		// If we got fewer keys than the batch size, we've reached the end
		if len(keys) < MigrationBatchSize {
			break
		}

		page++
	}

	p.logger.LogInfo("DM mapping migration completed", "total_migrated", totalMigratedCount, "total_reverse_migrated", totalReverseMigratedCount, "pages_processed", page+1)
	return &MigrationResult{DMMappingsCreated: totalMigratedCount, ReverseDMMappingsCreated: totalReverseMigratedCount}, nil
}

// Migration invariant (important for adding future versions):
//   - v1/v2 always (re)produce the historical UN-namespaced key layout; they
//     deliberately hand-build legacy keys (e.g. KeyPrefixMattermostUser + id)
//     rather than the serverID key builders, and skip keys already namespaced.
//   - v3 is the SOLE authority that adds the serverID namespace, and its rekey /
//     value-conversion steps are idempotent, so re-running the whole chain (e.g.
//     via the /matrix migrate command) is a no-op on already-migrated data.
// A future v4 must preserve this: legacy steps stay un-namespaced, and the
// namespacing/idempotency guards must account for the v3 layout.

// v3NamespacedPrefixes are the per-server KV key prefixes that gain a serverID
// dimension in version 3. channel_mapping_ is intentionally excluded: its key
// stays server-agnostic and only its value shape changes (see below).
var v3NamespacedPrefixes = []string{
	kvstore.KeyPrefixMatrixUser,
	kvstore.KeyPrefixMattermostUser,
	kvstore.KeyPrefixGhostUser,
	kvstore.KeyPrefixGhostRoom,
	kvstore.KeyPrefixMatrixEventPost,
	kvstore.KeyPrefixMatrixReaction,
	kvstore.KeyPrefixRoomMapping,
}

// runMigrationToVersion3WithResults migrates to version 3: namespaces every
// per-server KV key by the single server's serverID and converts each
// channel_mapping_ value from a bare room ID string into a []ChannelServerMapping
// JSON array. It is deterministic and idempotent: keys already namespaced and
// values already converted are skipped, so a direct re-run is a no-op.
func (p *Plugin) runMigrationToVersion3WithResults() (*MigrationResult, error) {
	p.logger.LogInfo("Running migration to version 3: namespacing keys by serverID")

	// The server registry (and thus the serverID) is established by
	// reconcileServerConfig during initMatrixClient, which runs before
	// migrations. Reconcile again defensively in case migrations run first.
	serverID := p.getSingleServerID()
	if serverID == "" {
		if _, err := p.reconcileServerConfig(); err != nil {
			return nil, errors.Wrap(err, "failed to establish server registry for v3 migration")
		}
		serverID = p.getSingleServerID()
	}
	if serverID == "" {
		// No Matrix server is configured yet (e.g. a fresh install with sync
		// disabled and no server URL). There are no per-server keys to namespace,
		// so the v3 layout is trivially satisfied. The registry entry and its
		// namespaced keys are created later, once a server URL is configured and
		// reconcileServerConfig derives the serverID.
		p.logger.LogInfo("v3 migration: no Matrix server configured; nothing to namespace")
		return &MigrationResult{}, nil
	}

	result := &MigrationResult{}

	for _, prefix := range v3NamespacedPrefixes {
		migrated, err := p.rekeyPrefixWithServerID(prefix, serverID)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to namespace keys for prefix %q", prefix)
		}
		p.logger.LogDebug("Namespaced keys for prefix", "prefix", prefix, "migrated", migrated)
	}

	converted, err := p.convertChannelMappingsToServerScoped(serverID)
	if err != nil {
		return nil, errors.Wrap(err, "failed to convert channel mappings")
	}
	result.ChannelMappingsCreated = converted

	p.logger.LogInfo("Version 3 migration completed", "server_id", serverID, "channel_mappings_converted", converted)
	return result, nil
}

// rekeyPrefixWithServerID rewrites every key under the given prefix from
// "<prefix><id>" to "<prefix><serverID>_<id>". Existing keys are fully
// enumerated before any writes so newly namespaced keys (which share the prefix)
// are not re-processed, and keys already carrying the serverID namespace are
// skipped for idempotency.
func (p *Plugin) rekeyPrefixWithServerID(prefix, serverID string) (int, error) {
	keys, err := p.listAllKeysWithPrefix(prefix)
	if err != nil {
		return 0, err
	}

	namespace := serverID + "_"
	migrated := 0
	failures := 0
	for _, oldKey := range keys {
		id := strings.TrimPrefix(oldKey, prefix)
		if strings.HasPrefix(id, namespace) {
			continue // already namespaced (idempotent re-run)
		}

		value, err := p.kvstore.Get(oldKey)
		if err != nil {
			p.logger.LogWarn("Failed to read key during v3 namespacing", "key", oldKey, "error", err)
			failures++
			continue
		}

		newKey := prefix + namespace + id
		if err := p.kvstore.Set(newKey, value); err != nil {
			p.logger.LogWarn("Failed to write namespaced key during v3 migration", "key", newKey, "error", err)
			failures++
			continue
		}
		// A failed Delete only leaves an orphaned legacy key; the namespaced key
		// is written, so reads succeed. Treat it as non-fatal.
		if err := p.kvstore.Delete(oldKey); err != nil {
			p.logger.LogWarn("Failed to delete legacy key during v3 migration", "key", oldKey, "error", err)
		}
		migrated++
	}

	if failures > 0 {
		// Return an error so the version marker is not advanced to 3 and the
		// migration retries on the next activation. Otherwise a transient KV
		// failure would leave some mappings in the legacy namespace forever.
		return migrated, errors.Errorf("failed to namespace %d key(s) for prefix %q", failures, prefix)
	}
	return migrated, nil
}

// convertChannelMappingsToServerScoped rewrites each channel_mapping_ value from
// a bare room-ID string into a single-entry []ChannelServerMapping JSON array
// attributed to serverID. Values already stored as a non-empty JSON array are
// left untouched, making the conversion idempotent.
func (p *Plugin) convertChannelMappingsToServerScoped(serverID string) (int, error) {
	keys, err := p.listAllKeysWithPrefix(kvstore.KeyPrefixChannelMapping)
	if err != nil {
		return 0, err
	}

	converted := 0
	failures := 0
	for _, key := range keys {
		value, err := p.kvstore.Get(key)
		if err != nil {
			p.logger.LogWarn("Failed to read channel mapping during v3 migration", "key", key, "error", err)
			failures++
			continue
		}
		if len(value) == 0 {
			continue
		}

		// Skip values already in the new []ChannelServerMapping shape.
		if existing, perr := kvstore.ParseChannelServerMappings(value); perr == nil && len(existing) > 0 {
			continue
		}

		newValue, err := kvstore.BuildSingleChannelMapping(serverID, string(value))
		if err != nil {
			p.logger.LogWarn("Failed to marshal channel mapping during v3 migration", "key", key, "error", err)
			failures++
			continue
		}
		if err := p.kvstore.Set(key, newValue); err != nil {
			p.logger.LogWarn("Failed to write converted channel mapping during v3 migration", "key", key, "error", err)
			failures++
			continue
		}
		converted++
	}

	if failures > 0 {
		// Do not let the version marker advance to 3 with un-converted values,
		// which GetMatrixRoomID would then read as unmapped. Retry next activation.
		return converted, errors.Errorf("failed to convert %d channel mapping(s)", failures)
	}
	return converted, nil
}

// listAllKeysWithPrefix enumerates every key for a prefix by paging through the
// KV store. The full list is materialized so callers can safely mutate keys
// sharing the prefix without disturbing pagination.
func (p *Plugin) listAllKeysWithPrefix(prefix string) ([]string, error) {
	var keys []string
	page := 0
	for {
		batch, err := p.kvstore.ListKeysWithPrefix(page, MigrationBatchSize, prefix)
		if err != nil {
			return nil, errors.Wrap(err, "failed to list KV store keys with prefix")
		}
		keys = append(keys, batch...)
		if len(batch) < MigrationBatchSize {
			break
		}
		page++
	}
	return keys, nil
}
