package main

import (
	"bytes"
	"strconv"
	"strings"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
	"github.com/pkg/errors"
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
				reverseKey := kvstore.BuildMattermostUserKey(mattermostUserID)

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

				roomIdentifier := string(roomIdentifierBytes)
				channelID := strings.TrimPrefix(key, channelMappingPrefix)

				// Create reverse mapping: room_mapping_<roomIdentifier> -> channelID
				reverseKey := kvstore.BuildRoomMappingKey(roomIdentifier)

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
				if strings.HasPrefix(roomIdentifier, "#") && p.matrixClient != nil {
					if resolvedRoomID, resolveErr := p.matrixClient.ResolveRoomAlias(roomIdentifier); resolveErr == nil {
						roomIDKey := kvstore.BuildRoomMappingKey(resolvedRoomID)

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
			reverseKey := kvstore.BuildRoomMappingKey(matrixRoomID)
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
			unifiedReverseKey := kvstore.BuildRoomMappingKey(matrixRoomID)

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
