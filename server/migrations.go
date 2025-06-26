package main

import (
	"strconv"
	"strings"

	"github.com/pkg/errors"
)

const (
	// KVStoreVersionKey tracks the current KV store schema version
	KVStoreVersionKey = "kv_store_version"
	// CurrentKVStoreVersion is the current version requiring migrations
	CurrentKVStoreVersion = 1
	// MigrationBatchSize is the number of keys to process in each batch
	MigrationBatchSize = 1000
)

// runKVStoreMigrations checks the KV store version and runs necessary migrations
func (p *Plugin) runKVStoreMigrations() error {
	// Get current KV store version
	versionBytes, err := p.kvstore.Get(KVStoreVersionKey)
	currentVersion := 0
	if err == nil && len(versionBytes) > 0 {
		if version, parseErr := strconv.Atoi(string(versionBytes)); parseErr == nil {
			currentVersion = version
		}
	}

	p.logger.LogInfo("Checking KV store migrations", "current_version", currentVersion, "target_version", CurrentKVStoreVersion)

	// Run migrations if needed
	if currentVersion < CurrentKVStoreVersion {
		p.logger.LogInfo("Running KV store migrations", "from_version", currentVersion, "to_version", CurrentKVStoreVersion)

		if err := p.runMigrationToVersion1(); err != nil {
			return errors.Wrap(err, "failed to migrate to version 1")
		}

		// Update version marker
		if err := p.kvstore.Set(KVStoreVersionKey, []byte(strconv.Itoa(CurrentKVStoreVersion))); err != nil {
			return errors.Wrap(err, "failed to update KV store version")
		}

		p.logger.LogInfo("KV store migrations completed successfully", "new_version", CurrentKVStoreVersion)
	} else {
		p.logger.LogDebug("KV store is up to date", "version", currentVersion)
	}

	return nil
}

// runMigrationToVersion1 migrates to version 1: adds reverse mappings for users and channels
func (p *Plugin) runMigrationToVersion1() error {
	p.logger.LogInfo("Running migration to version 1: adding reverse mappings")

	// Migrate user mappings
	if err := p.migrateUserMappings(); err != nil {
		return errors.Wrap(err, "failed to migrate user mappings")
	}

	// Migrate channel mappings
	if err := p.migrateChannelMappings(); err != nil {
		return errors.Wrap(err, "failed to migrate channel mappings")
	}

	return nil
}

// migrateUserMappings creates reverse mappings for existing user mappings
func (p *Plugin) migrateUserMappings() error {
	p.logger.LogInfo("Migrating user mappings to add reverse lookups")

	userMappingPrefix := "matrix_user_"
	totalMigratedCount := 0
	page := 0

	for {
		// Get keys in batches
		keys, err := p.kvstore.ListKeys(page, MigrationBatchSize)
		if err != nil {
			return errors.Wrap(err, "failed to list KV store keys")
		}

		if len(keys) == 0 {
			break // No more keys
		}

		batchMigratedCount := 0
		batchSkippedCount := 0
		batchProcessedCount := 0
		for _, key := range keys {
			if strings.HasPrefix(key, userMappingPrefix) {
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
				reverseKey := "mattermost_user_" + mattermostUserID

				// Check if reverse mapping already exists
				existingData, err := p.kvstore.Get(reverseKey)
				if err == nil && len(existingData) > 0 {
					batchSkippedCount++
					continue // Already exists, skip
				}

				// Create the reverse mapping
				if err := p.kvstore.Set(reverseKey, []byte(matrixUserID)); err != nil {
					p.logger.LogWarn("Failed to create reverse user mapping during migration", "mattermost_user_id", mattermostUserID, "matrix_user_id", matrixUserID, "error", err)
					continue
				}

				batchMigratedCount++
				p.logger.LogDebug("Created reverse user mapping", "mattermost_user_id", mattermostUserID, "matrix_user_id", matrixUserID)
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
	return nil
}

// migrateChannelMappings creates reverse mappings for existing channel mappings
func (p *Plugin) migrateChannelMappings() error {
	p.logger.LogInfo("Migrating channel mappings to add reverse lookups")

	channelMappingPrefix := "channel_mapping_"
	totalMigratedCount := 0
	page := 0

	for {
		// Get keys in batches
		keys, err := p.kvstore.ListKeys(page, MigrationBatchSize)
		if err != nil {
			return errors.Wrap(err, "failed to list KV store keys")
		}

		if len(keys) == 0 {
			break // No more keys
		}

		batchMigratedCount := 0
		batchSkippedCount := 0
		batchProcessedCount := 0
		for _, key := range keys {
			if strings.HasPrefix(key, channelMappingPrefix) {
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
				reverseKey := "room_mapping_" + roomIdentifier

				// Check if reverse mapping already exists
				existingData, err := p.kvstore.Get(reverseKey)
				if err == nil && len(existingData) > 0 {
					batchSkippedCount++
					continue // Already exists, skip
				}

				// Create the reverse mapping
				if err := p.kvstore.Set(reverseKey, []byte(channelID)); err != nil {
					p.logger.LogWarn("Failed to create reverse channel mapping during migration", "channel_id", channelID, "room_identifier", roomIdentifier, "error", err)
					continue
				}

				batchMigratedCount++
				p.logger.LogDebug("Created reverse channel mapping", "channel_id", channelID, "room_identifier", roomIdentifier)

				// If roomIdentifier is an alias, also resolve to room ID and create that mapping
				if strings.HasPrefix(roomIdentifier, "#") && p.matrixClient != nil {
					if resolvedRoomID, resolveErr := p.matrixClient.ResolveRoomAlias(roomIdentifier); resolveErr == nil {
						roomIDKey := "room_mapping_" + resolvedRoomID

						// Check if room ID mapping already exists
						if _, err := p.kvstore.Get(roomIDKey); err != nil {
							// Create room ID mapping
							if err := p.kvstore.Set(roomIDKey, []byte(channelID)); err != nil {
								p.logger.LogWarn("Failed to create room ID mapping during migration", "channel_id", channelID, "room_id", resolvedRoomID, "error", err)
							} else {
								p.logger.LogDebug("Created room ID mapping", "channel_id", channelID, "room_id", resolvedRoomID)
							}
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

	p.logger.LogInfo("Channel mapping migration completed", "total_migrated", totalMigratedCount, "pages_processed", page+1)
	return nil
}
