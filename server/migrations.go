package main

import (
	"bytes"
	"strconv"
	"strings"

	"github.com/pkg/errors"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/matrix"
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
			migrationServerID, err := p.resolveMigrationServerID()
			if err != nil {
				return nil, errors.Wrap(err, "failed to resolve the implicit server owning the pre-v3 KV layout")
			}

			if err := p.runMigrationToVersion3WithResults(migrationServerID); err != nil {
				return nil, errors.Wrap(err, "failed to migrate to version 3")
			}
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

				// Create reverse mapping: mattermost_user_<mattermostUserID> -> matrixUserID
				reverseKey := kvstore.KeyPrefixMattermostUser + mattermostUserID // legacy (pre-v3) un-namespaced key

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

	// Best-effort client for resolving room aliases to room IDs, built from whatever
	// legacy flat configuration is still present. This runs before the v3 migration has
	// necessarily materialized a registry entry, so there may be no server to build a
	// client from yet - alias resolution is then simply skipped, matching the existing
	// "continue on resolve failure" behavior below.
	legacyClient := p.legacyMatrixClientForMigration()

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

				roomIdentifier := string(roomIdentifierBytes)
				channelID := strings.TrimPrefix(key, channelMappingPrefix)

				// Create reverse mapping: room_mapping_<roomIdentifier> -> channelID
				reverseKey := kvstore.KeyPrefixRoomMapping + roomIdentifier // legacy (pre-v3) un-namespaced key

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
				if strings.HasPrefix(roomIdentifier, "#") && legacyClient != nil {
					if resolvedRoomID, resolveErr := legacyClient.ResolveRoomAlias(roomIdentifier); resolveErr == nil {
						roomIDKey := kvstore.KeyPrefixRoomMapping + resolvedRoomID // legacy (pre-v3) un-namespaced key

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
			reverseKey := kvstore.KeyPrefixRoomMapping + matrixRoomID // legacy (pre-v3) un-namespaced key
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
			unifiedReverseKey := kvstore.KeyPrefixRoomMapping + matrixRoomID // legacy (pre-v3) un-namespaced key

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

// legacyMatrixClientForMigration builds a best-effort Matrix client from whatever
// legacy flat plugin configuration is still present, for the pre-v3 migration steps
// that need to resolve a room alias to a room ID. Returns nil (not an error) when no
// legacy server URL/token is configured - callers must treat that as "skip, and
// continue without this optimization".
func (p *Plugin) legacyMatrixClientForMigration() *matrix.Client {
	var legacy legacyServerConfig
	if err := p.API.LoadPluginConfiguration(&legacy); err != nil {
		return nil
	}
	if legacy.MatrixServerURL == "" || legacy.MatrixASToken == "" {
		return nil
	}
	return matrix.NewClientWithRateLimit(legacy.MatrixServerURL, legacy.MatrixASToken, "", legacy.MatrixServerName, p.API, matrix.RateLimitConfig{})
}

// resolveMigrationServerID identifies the single implicit owner of the pre-v3
// un-namespaced KV layout. The v1/v2 schema predates multi-server support, so exactly
// one server can ever be its owner:
//   - 1 registered server -> that one (an install that has already run AddServer/
//     server add, or a previous partial v3 run that materialized the legacy entry).
//   - 0 -> materializeServerFromLegacyConfig(), which returns "" on a genuinely fresh
//     install with no legacy configuration - not an error.
//   - >=2 -> a hard error. Migrating would rekey one server's records into another's
//     namespace, which is exactly the corruption this function exists to prevent.
//
// The v3 migration must not be run when this returns an error - see the call site.
func (p *Plugin) resolveMigrationServerID() (string, error) {
	servers, err := p.getServers()
	if err != nil {
		return "", errors.Wrap(err, "failed to read servers config")
	}

	switch len(servers) {
	case 1:
		return servers[0].ServerID, nil
	case 0:
		return p.materializeServerFromLegacyConfig()
	default:
		return "", errors.New("2 or more Matrix servers are already registered; the v3 migration refuses to run to avoid rekeying one server's records into another's namespace")
	}
}

// runMigrationToVersion3WithResults migrates the pre-v3 un-namespaced KV layout to the
// per-server namespaced layout, for the single implicit owner resolved by
// resolveMigrationServerID. serverID == "" means a genuinely fresh install with nothing
// to migrate - a no-op, not an error, so the version marker still advances.
func (p *Plugin) runMigrationToVersion3WithResults(serverID string) error {
	if serverID == "" {
		p.logger.LogInfo("No legacy Matrix configuration found; nothing to migrate to KV store version 3")
		return nil
	}

	p.logger.LogInfo("Running migration to version 3: namespacing KV records per server", "server_id", serverID)

	if err := p.rekeyNamespacedPrefixesToVersion3(serverID); err != nil {
		return errors.Wrap(err, "failed to rekey namespaced KV prefixes")
	}

	if err := p.convertChannelMappingsToVersion3(serverID); err != nil {
		return errors.Wrap(err, "failed to convert channel mappings")
	}

	p.logger.LogInfo("KV store version 3 migration completed", "server_id", serverID)
	return nil
}

// rekeyNamespacedPrefixesToVersion3 rewrites every key under the seven namespaced
// prefixes from the legacy <prefix><id> shape to <prefix><serverID>_<id>: get the value
// under the old key, write it under the new key, then delete the old key. The full key
// list for every prefix is enumerated before any writes happen, and keys already in the
// new shape (this call already ran, partially or fully) are skipped, making the whole
// operation idempotent. A failed delete of a legacy key is logged and does not fail the
// migration - the record is simply readable under both keys until the next run.
func (p *Plugin) rekeyNamespacedPrefixesToVersion3(serverID string) error {
	keysByPrefix, err := kvstore.ListAllKeysByPrefix(p.kvstore, MigrationBatchSize, kvstore.NamespacedKeyPrefixes...)
	if err != nil {
		return errors.Wrap(err, "failed to enumerate namespaced KV prefixes")
	}

	for _, prefix := range kvstore.NamespacedKeyPrefixes {
		alreadyNamespacedPrefix := prefix + serverID + "_"

		for _, oldKey := range keysByPrefix[prefix] {
			if strings.HasPrefix(oldKey, alreadyNamespacedPrefix) {
				continue // already migrated
			}

			suffix := strings.TrimPrefix(oldKey, prefix)
			newKey := prefix + serverID + "_" + suffix

			value, err := p.kvstore.Get(oldKey)
			if err != nil {
				return errors.Wrapf(err, "failed to read legacy key %q", oldKey)
			}
			if len(value) == 0 {
				continue // nothing to migrate (key vanished between listing and read)
			}

			if err := p.kvstore.Set(newKey, value); err != nil {
				return errors.Wrapf(err, "failed to write namespaced key %q", newKey)
			}

			if err := p.kvstore.Delete(oldKey); err != nil {
				p.logger.LogWarn("Failed to delete legacy key after rekeying; it will remain readable alongside the namespaced key", "old_key", oldKey, "new_key", newKey, "error", err)
			}
		}
	}

	return nil
}

// convertChannelMappingsToVersion3 converts every channel_mapping_ value from the
// legacy bare-room-ID-string shape to the new []ChannelServerMapping JSON array shape.
// channel_mapping_ keys are NOT rekeyed (the server lives in the value, not the key) -
// only their value's shape changes.
func (p *Plugin) convertChannelMappingsToVersion3(serverID string) error {
	keys, err := kvstore.ListAllKeysWithPrefix(p.kvstore, kvstore.KeyPrefixChannelMapping, MigrationBatchSize)
	if err != nil {
		return errors.Wrap(err, "failed to enumerate channel mappings")
	}

	for _, key := range keys {
		value, err := p.kvstore.Get(key)
		if err != nil {
			return errors.Wrapf(err, "failed to read channel mapping %q", key)
		}
		if len(value) == 0 {
			continue
		}

		// A successful parse means this value is already in the new shape (a
		// zero-length array or null both mean "unmapped", not "bare room ID") -
		// skip it. This is what makes the conversion idempotent.
		if _, err := kvstore.ParseChannelServerMappings(value); err == nil {
			continue
		}

		roomIdentifier := string(value)
		if !kvstore.IsPlausibleRoomIdentifier(roomIdentifier) {
			p.logger.LogWarn("Skipping channel mapping with a value that is neither valid JSON nor a plausible room identifier",
				"key", key, "value", roomIdentifier)
			continue
		}

		newValue, err := kvstore.BuildSingleChannelMapping(serverID, roomIdentifier)
		if err != nil {
			return errors.Wrapf(err, "failed to build converted channel mapping for %q", key)
		}

		if err := p.kvstore.Set(key, newValue); err != nil {
			return errors.Wrapf(err, "failed to write converted channel mapping for %q", key)
		}
	}

	return nil
}
