package main

import (
	"strconv"
	"testing"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
	"github.com/stretchr/testify/assert"
)

func TestRunKVStoreMigrations(t *testing.T) {
	t.Run("NoMigrationNeeded", func(t *testing.T) {
		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		plugin.logger = &testLogger{t: t}

		// Set current version to target version
		err := plugin.kvstore.Set(kvstore.KeyStoreVersion, []byte(strconv.Itoa(kvstore.CurrentKVStoreVersion)))
		assert.NoError(t, err)

		// Run migrations
		err = plugin.runKVStoreMigrations()
		assert.NoError(t, err)

		// Version should remain the same
		versionBytes, err := plugin.kvstore.Get(kvstore.KeyStoreVersion)
		assert.NoError(t, err)
		version, err := strconv.Atoi(string(versionBytes))
		assert.NoError(t, err)
		assert.Equal(t, kvstore.CurrentKVStoreVersion, version)
	})

	t.Run("MigrationFromVersion0", func(t *testing.T) {
		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		plugin.logger = &testLogger{t: t}
		plugin.matrixClient = createMatrixClientWithTestLogger(t, "", "", "")

		// No version key exists (version 0)
		_, err := plugin.kvstore.Get(kvstore.KeyStoreVersion)
		assert.Error(t, err) // Should not exist

		// Add some test data that would need migration
		err = plugin.kvstore.Set("matrix_user_@alice:matrix.org", []byte("user123"))
		assert.NoError(t, err)
		err = plugin.kvstore.Set("channel_mapping_channel456", []byte("!room789:matrix.org"))
		assert.NoError(t, err)

		// Run migrations
		err = plugin.runKVStoreMigrations()
		assert.NoError(t, err)

		// Version should be updated
		versionBytes, err := plugin.kvstore.Get(kvstore.KeyStoreVersion)
		assert.NoError(t, err)
		version, err := strconv.Atoi(string(versionBytes))
		assert.NoError(t, err)
		assert.Equal(t, kvstore.CurrentKVStoreVersion, version)

		// Reverse mappings should be created
		userReverseBytes, err := plugin.kvstore.Get("mattermost_user_user123")
		assert.NoError(t, err)
		assert.Equal(t, "@alice:matrix.org", string(userReverseBytes))

		channelReverseBytes, err := plugin.kvstore.Get("room_mapping_!room789:matrix.org")
		assert.NoError(t, err)
		assert.Equal(t, "channel456", string(channelReverseBytes))
	})

	t.Run("InvalidVersionHandledGracefully", func(t *testing.T) {
		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		plugin.logger = &testLogger{t: t}

		// Set invalid version
		err := plugin.kvstore.Set(kvstore.KeyStoreVersion, []byte("invalid"))
		assert.NoError(t, err)

		// Should treat as version 0 and run migration
		err = plugin.runKVStoreMigrations()
		assert.NoError(t, err)

		// Version should be updated to current
		versionBytes, err := plugin.kvstore.Get(kvstore.KeyStoreVersion)
		assert.NoError(t, err)
		version, err := strconv.Atoi(string(versionBytes))
		assert.NoError(t, err)
		assert.Equal(t, kvstore.CurrentKVStoreVersion, version)
	})
}

func TestMigrateUserMappings(t *testing.T) {
	t.Run("MigrateMultipleUsers", func(t *testing.T) {
		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		plugin.logger = &testLogger{t: t}

		// Add test user mappings
		testUsers := map[string]string{
			"matrix_user_@alice:matrix.org": "user123",
			"matrix_user_@bob:matrix.org":   "user456",
			"matrix_user_@carol:matrix.org": "user789",
		}

		for key, value := range testUsers {
			err := plugin.kvstore.Set(key, []byte(value))
			assert.NoError(t, err)
		}

		// Add some non-user keys that should be ignored
		err := plugin.kvstore.Set("channel_mapping_test", []byte("room123"))
		assert.NoError(t, err)
		err = plugin.kvstore.Set("other_key", []byte("other_value"))
		assert.NoError(t, err)

		// Run user migration
		_, err = plugin.migrateUserMappingsWithResults()
		assert.NoError(t, err)

		// Check that reverse mappings were created
		expectedReverse := map[string]string{
			"mattermost_user_user123": "@alice:matrix.org",
			"mattermost_user_user456": "@bob:matrix.org",
			"mattermost_user_user789": "@carol:matrix.org",
		}

		for reverseKey, expectedValue := range expectedReverse {
			valueBytes, err := plugin.kvstore.Get(reverseKey)
			assert.NoError(t, err)
			assert.Equal(t, expectedValue, string(valueBytes))
		}

		// Original mappings should still exist
		for key, expectedValue := range testUsers {
			valueBytes, err := plugin.kvstore.Get(key)
			assert.NoError(t, err)
			assert.Equal(t, expectedValue, string(valueBytes))
		}
	})

	t.Run("OverwriteIncorrectReverseMappings", func(t *testing.T) {
		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		plugin.logger = &testLogger{t: t}

		// Add user mapping (source of truth)
		err := plugin.kvstore.Set("matrix_user_@alice:matrix.org", []byte("user123"))
		assert.NoError(t, err)

		// Add incorrect existing reverse mapping
		err = plugin.kvstore.Set("mattermost_user_user123", []byte("@incorrect:matrix.org"))
		assert.NoError(t, err)

		// Run migration
		_, err = plugin.migrateUserMappingsWithResults()
		assert.NoError(t, err)

		// Incorrect reverse mapping should be corrected based on forward mapping
		valueBytes, err := plugin.kvstore.Get("mattermost_user_user123")
		assert.NoError(t, err)
		assert.Equal(t, "@alice:matrix.org", string(valueBytes))
	})

	t.Run("HandlesPaginationWithManyKeys", func(t *testing.T) {
		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		plugin.logger = &testLogger{t: t}

		// Add more than one batch worth of keys to test pagination
		// Add user mappings
		for i := 0; i < MigrationBatchSize+100; i++ {
			userKey := "matrix_user_@user" + strconv.Itoa(i) + ":matrix.org"
			mattermostUserID := "user" + strconv.Itoa(i)
			err := plugin.kvstore.Set(userKey, []byte(mattermostUserID))
			assert.NoError(t, err)
		}

		// Add some non-user keys
		for i := 0; i < 50; i++ {
			otherKey := "other_key_" + strconv.Itoa(i)
			err := plugin.kvstore.Set(otherKey, []byte("value"+strconv.Itoa(i)))
			assert.NoError(t, err)
		}

		// Run migration
		_, err := plugin.migrateUserMappingsWithResults()
		assert.NoError(t, err)

		// Verify all reverse mappings were created
		for i := 0; i < MigrationBatchSize+100; i++ {
			mattermostUserID := "user" + strconv.Itoa(i)
			expectedMatrixUserID := "@user" + strconv.Itoa(i) + ":matrix.org"

			reverseKey := kvstore.BuildMattermostUserKey(mattermostUserID)
			valueBytes, err := plugin.kvstore.Get(reverseKey)
			assert.NoError(t, err)
			assert.Equal(t, expectedMatrixUserID, string(valueBytes))
		}
	})
}

func TestMigrateChannelMappings(t *testing.T) {
	t.Run("MigrateChannelsWithRoomIDs", func(t *testing.T) {
		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		plugin.logger = &testLogger{t: t}
		plugin.matrixClient = createMatrixClientWithTestLogger(t, "", "", "")

		// Add test channel mappings with room IDs
		testChannels := map[string]string{
			"channel_mapping_channel123": "!room456:matrix.org",
			"channel_mapping_channel789": "!room012:matrix.org",
		}

		for key, value := range testChannels {
			err := plugin.kvstore.Set(key, []byte(value))
			assert.NoError(t, err)
		}

		// Run channel migration
		_, err := plugin.migrateChannelMappingsWithResults()
		assert.NoError(t, err)

		// Check that reverse mappings were created
		expectedReverse := map[string]string{
			"room_mapping_!room456:matrix.org": "channel123",
			"room_mapping_!room012:matrix.org": "channel789",
		}

		for reverseKey, expectedValue := range expectedReverse {
			valueBytes, err := plugin.kvstore.Get(reverseKey)
			assert.NoError(t, err)
			assert.Equal(t, expectedValue, string(valueBytes))
		}
	})

	t.Run("MigrateChannelsWithAliases", func(t *testing.T) {
		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		plugin.logger = &testLogger{t: t}
		// Use nil Matrix client to simulate alias resolution failure
		plugin.matrixClient = nil

		// Add test channel mapping with alias
		err := plugin.kvstore.Set("channel_mapping_channel123", []byte("#test:matrix.org"))
		assert.NoError(t, err)

		// Run channel migration
		_, err = plugin.migrateChannelMappingsWithResults()
		assert.NoError(t, err)

		// Check that alias reverse mapping was created (even without Matrix client)
		aliasReverseBytes, err := plugin.kvstore.Get("room_mapping_#test:matrix.org")
		assert.NoError(t, err)
		assert.Equal(t, "channel123", string(aliasReverseBytes))

		// Room ID mapping should not exist due to nil client
		_, err = plugin.kvstore.Get("room_mapping_!any:matrix.org")
		assert.Error(t, err)
	})

	t.Run("MigrateChannelsWithAliasesAndWorkingClient", func(t *testing.T) {
		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		plugin.logger = &testLogger{t: t}
		// Use real Matrix client (though it won't actually resolve without server)
		plugin.matrixClient = createMatrixClientWithTestLogger(t, "https://test.matrix.org", "test_token", "test_remote")

		// Add test channel mapping with alias
		err := plugin.kvstore.Set("channel_mapping_channel123", []byte("#test:matrix.org"))
		assert.NoError(t, err)

		// Run channel migration - should not fail even if alias resolution fails
		_, err = plugin.migrateChannelMappingsWithResults()
		assert.NoError(t, err)

		// Check that alias reverse mapping was created
		aliasReverseBytes, err := plugin.kvstore.Get("room_mapping_#test:matrix.org")
		assert.NoError(t, err)
		assert.Equal(t, "channel123", string(aliasReverseBytes))

		// Room ID mapping may or may not exist depending on alias resolution success
		// but migration should complete either way
	})

	t.Run("OverwriteIncorrectReverseMappings", func(t *testing.T) {
		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		plugin.logger = &testLogger{t: t}

		// Add channel mapping (source of truth)
		err := plugin.kvstore.Set("channel_mapping_channel123", []byte("!room456:matrix.org"))
		assert.NoError(t, err)

		// Add incorrect existing reverse mapping
		err = plugin.kvstore.Set("room_mapping_!room456:matrix.org", []byte("incorrect_channel"))
		assert.NoError(t, err)

		// Run migration
		_, err = plugin.migrateChannelMappingsWithResults()
		assert.NoError(t, err)

		// Incorrect reverse mapping should be corrected based on forward mapping
		valueBytes, err := plugin.kvstore.Get("room_mapping_!room456:matrix.org")
		assert.NoError(t, err)
		assert.Equal(t, "channel123", string(valueBytes))
	})

	t.Run("HandlesPaginationWithManyChannels", func(t *testing.T) {
		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		plugin.logger = &testLogger{t: t}
		plugin.matrixClient = createMatrixClientWithTestLogger(t, "", "", "")

		// Add more than one batch worth of channel mappings to test pagination
		for i := 0; i < MigrationBatchSize+50; i++ {
			channelKey := "channel_mapping_channel" + strconv.Itoa(i)
			roomID := "!room" + strconv.Itoa(i) + ":matrix.org"
			err := plugin.kvstore.Set(channelKey, []byte(roomID))
			assert.NoError(t, err)
		}

		// Run migration
		_, err := plugin.migrateChannelMappingsWithResults()
		assert.NoError(t, err)

		// Verify all reverse mappings were created
		for i := 0; i < MigrationBatchSize+50; i++ {
			channelID := "channel" + strconv.Itoa(i)
			roomID := "!room" + strconv.Itoa(i) + ":matrix.org"

			reverseKey := kvstore.BuildRoomMappingKey(roomID)
			valueBytes, err := plugin.kvstore.Get(reverseKey)
			assert.NoError(t, err)
			assert.Equal(t, channelID, string(valueBytes))
		}
	})
}

func TestMigrationIntegration(t *testing.T) {
	t.Run("FullMigrationScenario", func(t *testing.T) {
		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		plugin.logger = &testLogger{t: t}
		plugin.matrixClient = createMatrixClientWithTestLogger(t, "", "", "")

		// Setup a complete scenario with users, channels, and other keys
		testData := map[string]string{
			// User mappings
			"matrix_user_@alice:matrix.org": "user123",
			"matrix_user_@bob:matrix.org":   "user456",

			// Channel mappings
			"channel_mapping_channel789": "!room012:matrix.org",
			"channel_mapping_channel345": "#public:matrix.org",

			// Other keys (should be ignored)
			"ghost_user_user123": "@_mattermost_user123:matrix.org",
			"some_other_key":     "some_value",
		}

		// DM mappings (will be migrated by version 2 migration)
		dmTestData := map[string]string{
			"dm_mapping_dm123": "!dmroom456:matrix.org",
		}

		for key, value := range testData {
			err := plugin.kvstore.Set(key, []byte(value))
			assert.NoError(t, err)
		}

		for key, value := range dmTestData {
			err := plugin.kvstore.Set(key, []byte(value))
			assert.NoError(t, err)
		}

		// Verify no version key exists initially
		_, err := plugin.kvstore.Get(kvstore.KeyStoreVersion)
		assert.Error(t, err)

		// Run full migration
		err = plugin.runKVStoreMigrations()
		assert.NoError(t, err)

		// Check version was set
		versionBytes, err := plugin.kvstore.Get(kvstore.KeyStoreVersion)
		assert.NoError(t, err)
		version, err := strconv.Atoi(string(versionBytes))
		assert.NoError(t, err)
		assert.Equal(t, kvstore.CurrentKVStoreVersion, version)

		// Check user reverse mappings
		userReverse1, err := plugin.kvstore.Get("mattermost_user_user123")
		assert.NoError(t, err)
		assert.Equal(t, "@alice:matrix.org", string(userReverse1))

		userReverse2, err := plugin.kvstore.Get("mattermost_user_user456")
		assert.NoError(t, err)
		assert.Equal(t, "@bob:matrix.org", string(userReverse2))

		// Check channel reverse mappings
		channelReverse1, err := plugin.kvstore.Get("room_mapping_!room012:matrix.org")
		assert.NoError(t, err)
		assert.Equal(t, "channel789", string(channelReverse1))

		channelReverse2, err := plugin.kvstore.Get("room_mapping_#public:matrix.org")
		assert.NoError(t, err)
		assert.Equal(t, "channel345", string(channelReverse2))

		// Verify original data is unchanged
		for key, expectedValue := range testData {
			valueBytes, err := plugin.kvstore.Get(key)
			assert.NoError(t, err)
			assert.Equal(t, expectedValue, string(valueBytes))
		}

		// Verify DM mappings were migrated to unified prefix
		dmUnifiedBytes, err := plugin.kvstore.Get("channel_mapping_dm123")
		assert.NoError(t, err)
		assert.Equal(t, "!dmroom456:matrix.org", string(dmUnifiedBytes))

		// Verify old DM mapping was deleted
		_, err = plugin.kvstore.Get("dm_mapping_dm123")
		assert.Error(t, err) // Should be deleted

		// Verify reverse DM mapping was created
		dmReverseBytes, err := plugin.kvstore.Get("room_mapping_!dmroom456:matrix.org")
		assert.NoError(t, err)
		assert.Equal(t, "dm123", string(dmReverseBytes))

		otherBytes, err := plugin.kvstore.Get("some_other_key")
		assert.NoError(t, err)
		assert.Equal(t, "some_value", string(otherBytes))
	})

	t.Run("RunMigrationTwiceIsIdempotent", func(t *testing.T) {
		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		plugin.logger = &testLogger{t: t}
		plugin.matrixClient = createMatrixClientWithTestLogger(t, "", "", "")

		// Add test data
		err := plugin.kvstore.Set("matrix_user_@alice:matrix.org", []byte("user123"))
		assert.NoError(t, err)
		err = plugin.kvstore.Set("channel_mapping_channel456", []byte("!room789:matrix.org"))
		assert.NoError(t, err)

		// Run migration first time
		err = plugin.runKVStoreMigrations()
		assert.NoError(t, err)

		// Verify reverse mappings exist
		userReverse, err := plugin.kvstore.Get("mattermost_user_user123")
		assert.NoError(t, err)
		assert.Equal(t, "@alice:matrix.org", string(userReverse))

		channelReverse, err := plugin.kvstore.Get("room_mapping_!room789:matrix.org")
		assert.NoError(t, err)
		assert.Equal(t, "channel456", string(channelReverse))

		// Run migration second time
		err = plugin.runKVStoreMigrations()
		assert.NoError(t, err)

		// Verify data is unchanged
		userReverse2, err := plugin.kvstore.Get("mattermost_user_user123")
		assert.NoError(t, err)
		assert.Equal(t, "@alice:matrix.org", string(userReverse2))

		channelReverse2, err := plugin.kvstore.Get("room_mapping_!room789:matrix.org")
		assert.NoError(t, err)
		assert.Equal(t, "channel456", string(channelReverse2))

		// Version should still be current
		versionBytes, err := plugin.kvstore.Get(kvstore.KeyStoreVersion)
		assert.NoError(t, err)
		version, err := strconv.Atoi(string(versionBytes))
		assert.NoError(t, err)
		assert.Equal(t, kvstore.CurrentKVStoreVersion, version)
	})

	t.Run("EmptyKVStoreHandledGracefully", func(t *testing.T) {
		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		plugin.logger = &testLogger{t: t}
		plugin.matrixClient = createMatrixClientWithTestLogger(t, "", "", "")

		// Run migration on empty KV store
		err := plugin.runKVStoreMigrations()
		assert.NoError(t, err)

		// Version should be set
		versionBytes, err := plugin.kvstore.Get(kvstore.KeyStoreVersion)
		assert.NoError(t, err)
		version, err := strconv.Atoi(string(versionBytes))
		assert.NoError(t, err)
		assert.Equal(t, kvstore.CurrentKVStoreVersion, version)
	})
}
