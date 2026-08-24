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

// serverIDLength is the fixed length of a ServerID: 26 alphanumeric characters, the
// shape every ServerID has whether minted by model.NewId() or supplied by a caller and
// validated by model.IsValidId (see AddServer).
const serverIDLength = 26

// isNamespacedKey reports whether key is already in the v3 <prefix><serverID>_<id>
// shape, as opposed to the legacy <prefix><id> shape. This is a structural check
// against the key itself rather than a lookup against the currently registered
// servers: RemoveServer intentionally leaves a removed server's namespaced KV records
// in place (every namespaced key stays exactly where it was, addressed by a ServerID
// available for re-adoption via AddServer), and /matrix migrate only refuses to reset
// the version marker while 2+ servers are *currently* registered - so a registry-based
// check would misclassify a removed server's namespaced keys as legacy on a later
// migration re-run. Safe against false positives: ServerID is always exactly 26
// alphanumeric characters with no underscore, so "<26 alphanumeric characters>_" can
// never be a prefix of a legacy Matrix user ID (always starts with '@') or a legacy
// Mattermost user ID (itself a bare 26-character alphanumeric ID with no underscore).
func isNamespacedKey(key, prefix string) bool {
	rest := strings.TrimPrefix(key, prefix)
	return hasAlphanumericIDSegment(rest)
}

// hasAlphanumericIDSegment reports whether s starts with a 26-character alphanumeric ID
// (the shape of both a ServerID and a Mattermost user ID) followed by '_'.
func hasAlphanumericIDSegment(s string) bool {
	if len(s) <= serverIDLength || s[serverIDLength] != '_' {
		return false
	}
	for i := range serverIDLength {
		c := s[i]
		isAlphanumeric := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if !isAlphanumeric {
			return false
		}
	}
	return true
}

// isNamespacedGhostRoomKey reports whether a ghost_room_ key is already in the v3
// ghost_room_<serverID>_<mattermostUserID>_<roomID> shape, as opposed to the legacy
// ghost_room_<mattermostUserID>_<roomID> shape. Both shapes begin with a 26-character
// alphanumeric ID followed by '_' (a Mattermost user ID is exactly as long as a
// ServerID), so isNamespacedKey's single-segment check can't tell them apart here the
// way it can for the other six namespaced prefixes, whose legacy shape's next byte is a
// non-alphanumeric Matrix sigil ('@', '!', '#', or '$') rather than another ID. The
// disambiguator: a Matrix room ID/alias always starts with that kind of sigil too, so
// in the legacy shape the byte right after the first ID+'_' begins the room identifier
// (non-alphanumeric), while in the v3 shape it begins a *second* alphanumeric-ID+'_'
// segment (the mattermostUserID) ahead of the room identifier.
func isNamespacedGhostRoomKey(key, prefix string) bool {
	rest := strings.TrimPrefix(key, prefix)
	if !hasAlphanumericIDSegment(rest) {
		return false
	}
	return hasAlphanumericIDSegment(rest[serverIDLength+1:])
}

// migrateUserMappingsWithResults creates reverse mappings for existing user mappings, returning results.
func (p *Plugin) migrateUserMappingsWithResults() (*MigrationResult, error) {
	p.logger.LogInfo("Migrating user mappings to add reverse lookups")

	userMappingPrefix := kvstore.KeyPrefixMatrixUser

	// Enumerate the full raw keyspace ourselves rather than paging p.kvstore.ListKeysWithPrefix
	// directly: that helper filters by prefix after paging the raw keyspace, so stopping
	// once a page returns fewer than MigrationBatchSize matches can silently end the loop
	// early on a large, sparse keyspace. ListAllKeysWithPrefix pages the raw keyspace and
	// filters client-side, so it can't miss keys this way.
	keys, err := kvstore.ListAllKeysWithPrefix(p.kvstore, userMappingPrefix, MigrationBatchSize)
	if err != nil {
		return nil, errors.Wrap(err, "failed to list KV store keys with prefix")
	}

	totalMigratedCount := 0
	totalSkippedCount := 0
	totalAlreadyNamespacedCount := 0
	totalProcessedCount := 0

	for _, key := range keys {
		totalProcessedCount++

		// Already-namespaced v3 keys are not legacy records - trimming just the bare
		// prefix off one would leave "<serverID>_<matrixUserID>" and mangle it into a
		// corrupted legacy reverse mapping.
		if isNamespacedKey(key, userMappingPrefix) {
			totalAlreadyNamespacedCount++
			continue
		}

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
			totalSkippedCount++
			continue // Already correct, skip
		}

		// Create/update the reverse mapping (overwrites incorrect values)
		if err := p.kvstore.Set(reverseKey, []byte(matrixUserID)); err != nil {
			p.logger.LogWarn("Failed to create/update reverse user mapping during migration", "mattermost_user_id", mattermostUserID, "matrix_user_id", matrixUserID, "error", err)
			continue
		}

		totalMigratedCount++
		if err == nil && len(existingData) > 0 {
			p.logger.LogDebug("Updated incorrect reverse user mapping", "mattermost_user_id", mattermostUserID, "matrix_user_id", matrixUserID, "old_value", string(existingData))
		} else {
			p.logger.LogDebug("Created reverse user mapping", "mattermost_user_id", mattermostUserID, "matrix_user_id", matrixUserID)
		}
	}

	p.logger.LogInfo("User mapping migration completed",
		"total_processed", totalProcessedCount,
		"total_migrated", totalMigratedCount,
		"total_skipped", totalSkippedCount,
		"total_already_namespaced", totalAlreadyNamespacedCount)
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

	// Enumerate the full raw keyspace ourselves rather than paging p.kvstore.ListKeysWithPrefix
	// directly - see the identical comment in migrateUserMappingsWithResults for why.
	keys, err := kvstore.ListAllKeysWithPrefix(p.kvstore, channelMappingPrefix, MigrationBatchSize)
	if err != nil {
		return nil, errors.Wrap(err, "failed to list KV store keys with prefix")
	}

	totalMigratedCount := 0
	totalRoomMappingsCount := 0
	totalSkippedCount := 0
	totalAlreadyV3Count := 0
	totalProcessedCount := 0

	for _, key := range keys {
		totalProcessedCount++

		// Get the room identifier (alias or room ID)
		roomIdentifierBytes, err := p.kvstore.Get(key)
		if err != nil {
			p.logger.LogWarn("Failed to get channel mapping during migration", "key", key, "error", err)
			continue
		}

		// A successful parse means this value is already the v3 []ChannelServerMapping
		// JSON shape (mirrors the idempotency guard in convertChannelMappingsToVersion3) -
		// not a legacy bare room identifier string. Treating raw JSON bytes as a room
		// identifier would create a bogus room_mapping_<raw-json-text> reverse key.
		if _, parseErr := kvstore.ParseChannelServerMappings(roomIdentifierBytes); parseErr == nil {
			totalAlreadyV3Count++
			continue
		}

		roomIdentifier := string(roomIdentifierBytes)
		channelID := strings.TrimPrefix(key, channelMappingPrefix)

		// Create reverse mapping: room_mapping_<roomIdentifier> -> channelID
		reverseKey := kvstore.KeyPrefixRoomMapping + roomIdentifier // legacy (pre-v3) un-namespaced key

		// Check if reverse mapping already exists with correct value
		existingData, err := p.kvstore.Get(reverseKey)
		if err == nil && bytes.Equal(existingData, []byte(channelID)) {
			totalSkippedCount++
		} else {
			// Create/update the reverse mapping (overwrites incorrect values)
			if err := p.kvstore.Set(reverseKey, []byte(channelID)); err != nil {
				p.logger.LogWarn("Failed to create/update reverse channel mapping during migration", "channel_id", channelID, "room_identifier", roomIdentifier, "error", err)
			} else {
				totalMigratedCount++
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

	p.logger.LogInfo("Channel mapping migration completed",
		"total_processed", totalProcessedCount,
		"total_migrated", totalMigratedCount,
		"total_skipped", totalSkippedCount,
		"total_already_v3", totalAlreadyV3Count,
		"room_mappings_created", totalRoomMappingsCount)
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
//     install with no legacy configuration - not an error, UNLESS legacy un-namespaced
//     KV records still exist with no server left to own them (e.g. the legacy config
//     was cleared out from under existing data): that case is a hard error too, to avoid
//     silently stranding those records forever once the version marker advances.
//   - >=2 -> a hard error. Migrating would rekey one server's records into another's
//     namespace, which is exactly the corruption this function exists to prevent.
//
// The v3 migration must not be run when this returns an error - see the call site.
func (p *Plugin) resolveMigrationServerID() (string, error) {
	servers, err := p.servers.List()
	if err != nil {
		return "", errors.Wrap(err, "failed to read servers config")
	}

	switch len(servers) {
	case 1:
		return servers[0].ServerID, nil
	case 0:
		serverID, err := p.materializeServerFromLegacyConfig()
		if err != nil {
			return "", err
		}
		if serverID != "" {
			return serverID, nil
		}

		hasLegacyRecords, err := p.hasUnmigratedV3Records()
		if err != nil {
			return "", errors.Wrap(err, "failed to check for legacy KV records before treating this as a fresh install")
		}
		if hasLegacyRecords {
			return "", errors.New("legacy Matrix KV records exist but no Matrix server could be resolved to own them; the plugin will not activate until this is resolved - restore the legacy plugin configuration or register the correct server, then retry")
		}
		return "", nil
	default:
		return "", errors.New("2 or more Matrix servers are already registered; the v3 migration refuses to run to avoid rekeying one server's records into another's namespace")
	}
}

// hasUnmigratedV3Records reports whether any KV record still needs the v3 migration:
// a key under one of the seven namespaced prefixes not yet in the <prefix><serverID>_<id>
// shape, or a channel_mapping_ value not yet in the []ChannelServerMapping JSON shape.
// Used by resolveMigrationServerID to distinguish a genuinely empty/fresh KV store (safe
// to treat serverID == "" as a no-op) from one that still has legacy records but no
// resolvable owning server (must not silently strand them).
//
// ghost_room_ needs its own shape check (isNamespacedGhostRoomKey) rather than the
// generic isNamespacedKey: see that function's doc comment for why.
func (p *Plugin) hasUnmigratedV3Records() (bool, error) {
	prefixes := append(append([]string{}, kvstore.NamespacedKeyPrefixes...), kvstore.KeyPrefixChannelMapping)
	keysByPrefix, err := kvstore.ListAllKeysByPrefix(p.kvstore, MigrationBatchSize, prefixes...)
	if err != nil {
		return false, errors.Wrap(err, "failed to enumerate KV prefixes")
	}

	for _, prefix := range kvstore.NamespacedKeyPrefixes {
		for _, key := range keysByPrefix[prefix] {
			var namespaced bool
			if prefix == kvstore.KeyPrefixGhostRoom {
				namespaced = isNamespacedGhostRoomKey(key, prefix)
			} else {
				namespaced = isNamespacedKey(key, prefix)
			}
			if !namespaced {
				return true, nil
			}
		}
	}

	for _, key := range keysByPrefix[kvstore.KeyPrefixChannelMapping] {
		value, err := p.kvstore.Get(key)
		if err != nil {
			return false, errors.Wrapf(err, "failed to read channel mapping %q", key)
		}
		if classifyChannelMappingForV3(value) == channelMappingNeedsConversion {
			return true, nil
		}
	}

	return false, nil
}

// channelMappingConversionStatus classifies a channel_mapping_ value for the v3
// migration.
type channelMappingConversionStatus int

const (
	// channelMappingAlreadyV3 covers both an empty value (unmapped, not "bare room
	// ID") and one that already parses as the v3 []ChannelServerMapping JSON shape.
	channelMappingAlreadyV3 channelMappingConversionStatus = iota
	// channelMappingNeedsConversion is a legacy bare room identifier awaiting
	// conversion to the v3 shape.
	channelMappingNeedsConversion
	// channelMappingUnrecognized is neither valid v3 JSON nor a plausible room
	// identifier - convertChannelMappingsToVersion3 logs a warning and skips it
	// rather than treating it as a blocking legacy record.
	channelMappingUnrecognized
)

// classifyChannelMappingForV3 is the single source of truth both
// convertChannelMappingsToVersion3 and hasUnmigratedV3Records use to decide what a
// channel_mapping_ value is, so the two can't drift out of sync with each other.
func classifyChannelMappingForV3(value []byte) channelMappingConversionStatus {
	if len(value) == 0 {
		return channelMappingAlreadyV3
	}
	if _, err := kvstore.ParseChannelServerMappings(value); err == nil {
		return channelMappingAlreadyV3
	}
	if kvstore.IsPlausibleRoomIdentifier(string(value)) {
		return channelMappingNeedsConversion
	}
	return channelMappingUnrecognized
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

		switch classifyChannelMappingForV3(value) {
		case channelMappingAlreadyV3:
			continue // already in the new shape or unmapped - this is what makes the conversion idempotent
		case channelMappingUnrecognized:
			p.logger.LogWarn("Skipping channel mapping with a value that is neither valid JSON nor a plausible room identifier",
				"key", key, "value", string(value))
			continue
		}

		roomIdentifier := string(value)
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
