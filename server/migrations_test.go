package main

import (
	"strconv"
	"strings"
	"testing"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
)

// failOnSetKVStore wraps a KVStore and returns an error from Set for any key
// containing failKeySubstr, to simulate a transient KV backend failure.
type failOnSetKVStore struct {
	kvstore.KVStore
	failKeySubstr string
}

func (f *failOnSetKVStore) Set(key string, value []byte) error {
	if strings.Contains(key, f.failKeySubstr) {
		return errors.New("simulated KV set failure")
	}
	return f.KVStore.Set(key, value)
}

// failOnGetKVStore wraps a KVStore and returns an error from Get for any key
// containing failKeySubstr, to simulate a transient KV backend read failure.
type failOnGetKVStore struct {
	kvstore.KVStore
	failKeySubstr string
}

func (f *failOnGetKVStore) Get(key string) ([]byte, error) {
	if strings.Contains(key, f.failKeySubstr) {
		return nil, errors.New("simulated KV get failure")
	}
	return f.KVStore.Get(key)
}

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
		// Seed a deterministic server registry so v3 namespacing is predictable.
		seedTestServerConfig(plugin)

		// No version key exists (version 0)
		versionData, err := plugin.kvstore.Get(kvstore.KeyStoreVersion)
		require.NoError(t, err)
		assert.Empty(t, versionData) // Should not exist yet

		// Add some legacy (un-namespaced) test data that would need migration
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

		// Reverse mappings should be created and namespaced by serverID (v1 + v3)
		userReverseBytes, err := plugin.kvstore.Get(kvstore.BuildMattermostUserKey(testServerID, "user123"))
		assert.NoError(t, err)
		assert.Equal(t, "@alice:matrix.org", string(userReverseBytes))

		channelReverseBytes, err := plugin.kvstore.Get(kvstore.BuildRoomMappingKey(testServerID, "!room789:matrix.org"))
		assert.NoError(t, err)
		assert.Equal(t, "channel456", string(channelReverseBytes))

		// The forward user mapping is namespaced and the legacy key removed
		userForward, err := plugin.kvstore.Get(kvstore.BuildMatrixUserKey(testServerID, "@alice:matrix.org"))
		assert.NoError(t, err)
		assert.Equal(t, "user123", string(userForward))
		legacyUser, err := plugin.kvstore.Get("matrix_user_@alice:matrix.org")
		require.NoError(t, err)
		assert.Empty(t, legacyUser)

		// The channel mapping value is converted to the []ChannelServerMapping shape
		assertChannelRoom(t, plugin, "channel456", "!room789:matrix.org")
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

		// Run user migration (v1 sub-step: produces legacy un-namespaced keys)
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
		for i := range MigrationBatchSize + 100 {
			userKey := "matrix_user_@user" + strconv.Itoa(i) + ":matrix.org"
			mattermostUserID := "user" + strconv.Itoa(i)
			err := plugin.kvstore.Set(userKey, []byte(mattermostUserID))
			assert.NoError(t, err)
		}

		// Add some non-user keys
		for i := range 50 {
			otherKey := "other_key_" + strconv.Itoa(i)
			err := plugin.kvstore.Set(otherKey, []byte("value"+strconv.Itoa(i)))
			assert.NoError(t, err)
		}

		// Run migration
		_, err := plugin.migrateUserMappingsWithResults()
		assert.NoError(t, err)

		// Verify all reverse mappings were created (legacy un-namespaced key layout)
		for i := range MigrationBatchSize + 100 {
			mattermostUserID := "user" + strconv.Itoa(i)
			expectedMatrixUserID := "@user" + strconv.Itoa(i) + ":matrix.org"

			reverseKey := kvstore.KeyPrefixMattermostUser + mattermostUserID
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
		setTestMatrixClient(plugin, createMatrixClientWithTestLogger(t, "", "", ""))

		// Add test channel mappings with room IDs
		testChannels := map[string]string{
			"channel_mapping_channel123": "!room456:matrix.org",
			"channel_mapping_channel789": "!room012:matrix.org",
		}

		for key, value := range testChannels {
			err := plugin.kvstore.Set(key, []byte(value))
			assert.NoError(t, err)
		}

		// Run channel migration (v1 sub-step: produces legacy un-namespaced keys)
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
		// No Matrix client configured, simulating alias resolution failure.

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
		anyRoom, err := plugin.kvstore.Get("room_mapping_!any:matrix.org")
		require.NoError(t, err)
		assert.Empty(t, anyRoom)
	})

	t.Run("MigrateChannelsWithAliasesAndWorkingClient", func(t *testing.T) {
		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		plugin.logger = &testLogger{t: t}
		// Use real Matrix client (though it won't actually resolve without server)
		setTestMatrixClient(plugin, createMatrixClientWithTestLogger(t, "https://test.matrix.org", "test_token", "test_remote"))

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
		setTestMatrixClient(plugin, createMatrixClientWithTestLogger(t, "", "", ""))

		// Add more than one batch worth of channel mappings to test pagination
		for i := range MigrationBatchSize + 50 {
			channelKey := "channel_mapping_channel" + strconv.Itoa(i)
			roomID := "!room" + strconv.Itoa(i) + ":matrix.org"
			err := plugin.kvstore.Set(channelKey, []byte(roomID))
			assert.NoError(t, err)
		}

		// Run migration
		_, err := plugin.migrateChannelMappingsWithResults()
		assert.NoError(t, err)

		// Verify all reverse mappings were created (legacy un-namespaced key layout)
		for i := range MigrationBatchSize + 50 {
			channelID := "channel" + strconv.Itoa(i)
			roomID := "!room" + strconv.Itoa(i) + ":matrix.org"

			reverseKey := kvstore.KeyPrefixRoomMapping + roomID
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
		seedTestServerConfig(plugin)

		// Setup a complete scenario with users, channels, and other keys (legacy layout)
		testData := map[string]string{
			// User mappings
			"matrix_user_@alice:matrix.org": "user123",
			"matrix_user_@bob:matrix.org":   "user456",

			// Channel mappings
			"channel_mapping_channel789": "!room012:matrix.org",
			"channel_mapping_channel345": "#public:matrix.org",

			// Ghost user (namespaced by v3)
			"ghost_user_user123": "@_mattermost_user123:matrix.org",

			// Non per-server key (should be ignored)
			"some_other_key": "some_value",
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
		versionData, err := plugin.kvstore.Get(kvstore.KeyStoreVersion)
		require.NoError(t, err)
		assert.Empty(t, versionData)

		// Run full migration
		err = plugin.runKVStoreMigrations()
		assert.NoError(t, err)

		// Check version was set
		versionBytes, err := plugin.kvstore.Get(kvstore.KeyStoreVersion)
		assert.NoError(t, err)
		version, err := strconv.Atoi(string(versionBytes))
		assert.NoError(t, err)
		assert.Equal(t, kvstore.CurrentKVStoreVersion, version)

		// Check user reverse mappings (namespaced by serverID)
		userReverse1, err := plugin.kvstore.Get(kvstore.BuildMattermostUserKey(testServerID, "user123"))
		assert.NoError(t, err)
		assert.Equal(t, "@alice:matrix.org", string(userReverse1))

		userReverse2, err := plugin.kvstore.Get(kvstore.BuildMattermostUserKey(testServerID, "user456"))
		assert.NoError(t, err)
		assert.Equal(t, "@bob:matrix.org", string(userReverse2))

		// Check channel reverse mappings (namespaced by serverID)
		channelReverse1, err := plugin.kvstore.Get(kvstore.BuildRoomMappingKey(testServerID, "!room012:matrix.org"))
		assert.NoError(t, err)
		assert.Equal(t, "channel789", string(channelReverse1))

		channelReverse2, err := plugin.kvstore.Get(kvstore.BuildRoomMappingKey(testServerID, "#public:matrix.org"))
		assert.NoError(t, err)
		assert.Equal(t, "channel345", string(channelReverse2))

		// Forward user + ghost mappings are namespaced; legacy keys removed
		userForward, err := plugin.kvstore.Get(kvstore.BuildMatrixUserKey(testServerID, "@alice:matrix.org"))
		assert.NoError(t, err)
		assert.Equal(t, "user123", string(userForward))
		legacyUser, err := plugin.kvstore.Get("matrix_user_@alice:matrix.org")
		require.NoError(t, err)
		assert.Empty(t, legacyUser)

		ghostForward, err := plugin.kvstore.Get(kvstore.BuildGhostUserKey(testServerID, "user123"))
		assert.NoError(t, err)
		assert.Equal(t, "@_mattermost_user123:matrix.org", string(ghostForward))
		legacyGhost, err := plugin.kvstore.Get("ghost_user_user123")
		require.NoError(t, err)
		assert.Empty(t, legacyGhost)

		// Channel mapping values are converted to []ChannelServerMapping
		assertChannelRoom(t, plugin, "channel789", "!room012:matrix.org")
		assertChannelRoom(t, plugin, "channel345", "#public:matrix.org")

		// Non per-server key is untouched
		otherBytes, err := plugin.kvstore.Get("some_other_key")
		assert.NoError(t, err)
		assert.Equal(t, "some_value", string(otherBytes))

		// Verify DM mapping was migrated to unified prefix and converted
		assertChannelRoom(t, plugin, "dm123", "!dmroom456:matrix.org")

		// Verify old DM mapping was deleted
		oldDM, err := plugin.kvstore.Get("dm_mapping_dm123")
		require.NoError(t, err)
		assert.Empty(t, oldDM) // Should be deleted

		// Verify reverse DM mapping was created and namespaced
		dmReverseBytes, err := plugin.kvstore.Get(kvstore.BuildRoomMappingKey(testServerID, "!dmroom456:matrix.org"))
		assert.NoError(t, err)
		assert.Equal(t, "dm123", string(dmReverseBytes))
	})

	t.Run("RunMigrationTwiceIsIdempotent", func(t *testing.T) {
		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		plugin.logger = &testLogger{t: t}
		seedTestServerConfig(plugin)

		// Add legacy test data
		err := plugin.kvstore.Set("matrix_user_@alice:matrix.org", []byte("user123"))
		assert.NoError(t, err)
		err = plugin.kvstore.Set("channel_mapping_channel456", []byte("!room789:matrix.org"))
		assert.NoError(t, err)

		// Run migration first time
		err = plugin.runKVStoreMigrations()
		assert.NoError(t, err)

		// Capture the store size after the first run
		memStore, ok := plugin.kvstore.(*MemoryKVStore)
		require.True(t, ok)
		sizeAfterFirst := memStore.Size()

		// Verify namespaced mappings exist
		userReverse, err := plugin.kvstore.Get(kvstore.BuildMattermostUserKey(testServerID, "user123"))
		assert.NoError(t, err)
		assert.Equal(t, "@alice:matrix.org", string(userReverse))

		channelReverse, err := plugin.kvstore.Get(kvstore.BuildRoomMappingKey(testServerID, "!room789:matrix.org"))
		assert.NoError(t, err)
		assert.Equal(t, "channel456", string(channelReverse))

		// Run migration second time
		err = plugin.runKVStoreMigrations()
		assert.NoError(t, err)

		// Data is unchanged and no keys were added or duplicated
		assert.Equal(t, sizeAfterFirst, memStore.Size())

		userReverse2, err := plugin.kvstore.Get(kvstore.BuildMattermostUserKey(testServerID, "user123"))
		assert.NoError(t, err)
		assert.Equal(t, "@alice:matrix.org", string(userReverse2))

		channelReverse2, err := plugin.kvstore.Get(kvstore.BuildRoomMappingKey(testServerID, "!room789:matrix.org"))
		assert.NoError(t, err)
		assert.Equal(t, "channel456", string(channelReverse2))

		assertChannelRoom(t, plugin, "channel456", "!room789:matrix.org")

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

func TestMigrationToVersion3(t *testing.T) {
	t.Run("FreshInstallCreatesSingleServerEntry", func(t *testing.T) {
		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		plugin.logger = &testLogger{t: t}
		plugin.configuration = &configuration{MatrixServerURL: "https://matrix.example.com"}

		// No prior keys at all (fresh install)
		err := plugin.runKVStoreMigrations()
		assert.NoError(t, err)

		versionBytes, err := plugin.kvstore.Get(kvstore.KeyStoreVersion)
		assert.NoError(t, err)
		version, _ := strconv.Atoi(string(versionBytes))
		assert.Equal(t, kvstore.CurrentKVStoreVersion, version)

		// servers_config holds exactly one entry, keyed by the derived serverID.
		servers, err := plugin.getServers()
		assert.NoError(t, err)
		require.Len(t, servers, 1)
		expectedID, err := deriveServerID("https://matrix.example.com")
		require.NoError(t, err)
		assert.Equal(t, expectedID, servers[0].ServerID)
	})

	t.Run("FreshInstallWithoutServerConfiguredIsNoOp", func(t *testing.T) {
		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		plugin.logger = &testLogger{t: t}

		// Plugin enabled but no Matrix server configured (sync disabled): the v3
		// migration must complete and bump the version without creating an entry.
		err := plugin.runKVStoreMigrations()
		require.NoError(t, err)

		versionBytes, err := plugin.kvstore.Get(kvstore.KeyStoreVersion)
		require.NoError(t, err)
		version, _ := strconv.Atoi(string(versionBytes))
		assert.Equal(t, kvstore.CurrentKVStoreVersion, version)

		servers, err := plugin.getServers()
		require.NoError(t, err)
		assert.Empty(t, servers, "no server configured means no registry entry")
	})

	t.Run("UpgradeFromVersion2NamespacesKeys", func(t *testing.T) {
		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		plugin.logger = &testLogger{t: t}
		seedTestServerConfig(plugin)

		// Simulate a v2 install: version marker at 2 with legacy, un-namespaced keys.
		require.NoError(t, plugin.kvstore.Set(kvstore.KeyStoreVersion, []byte("2")))
		require.NoError(t, plugin.kvstore.Set("matrix_user_@alice:matrix.org", []byte("user123")))
		require.NoError(t, plugin.kvstore.Set("mattermost_user_user123", []byte("@alice:matrix.org")))
		require.NoError(t, plugin.kvstore.Set("room_mapping_!room789:matrix.org", []byte("channel456")))
		require.NoError(t, plugin.kvstore.Set("ghost_user_user123", []byte("@_mattermost_user123:matrix.org")))
		// ghost_room_ is a composite key: ghost_room_<mmUserID>_<roomID>.
		require.NoError(t, plugin.kvstore.Set("ghost_room_user123_!room789:matrix.org", []byte("joined")))
		require.NoError(t, plugin.kvstore.Set("matrix_reaction_$evt1:matrix.org", []byte("reaction-info")))
		require.NoError(t, plugin.kvstore.Set("matrix_event_post_$evt2:matrix.org", []byte("post999")))
		require.NoError(t, plugin.kvstore.Set("channel_mapping_channel456", []byte("!room789:matrix.org")))

		// Only v3 runs.
		err := plugin.runKVStoreMigrations()
		assert.NoError(t, err)

		// Every per-server key is now namespaced, and the legacy keys are gone.
		namespaced := map[string]string{
			kvstore.BuildMatrixUserKey(testServerID, "@alice:matrix.org"):             "user123",
			kvstore.BuildMattermostUserKey(testServerID, "user123"):                   "@alice:matrix.org",
			kvstore.BuildRoomMappingKey(testServerID, "!room789:matrix.org"):          "channel456",
			kvstore.BuildGhostUserKey(testServerID, "user123"):                        "@_mattermost_user123:matrix.org",
			kvstore.BuildGhostRoomKey(testServerID, "user123", "!room789:matrix.org"): "joined",
			kvstore.BuildMatrixReactionKey(testServerID, "$evt1:matrix.org"):          "reaction-info",
			kvstore.BuildMatrixEventPostKey(testServerID, "$evt2:matrix.org"):         "post999",
		}
		for key, expected := range namespaced {
			got, err := plugin.kvstore.Get(key)
			assert.NoError(t, err, key)
			assert.Equal(t, expected, string(got), key)
		}

		for _, legacy := range []string{
			"matrix_user_@alice:matrix.org",
			"mattermost_user_user123",
			"room_mapping_!room789:matrix.org",
			"ghost_user_user123",
			"ghost_room_user123_!room789:matrix.org",
			"matrix_reaction_$evt1:matrix.org",
			"matrix_event_post_$evt2:matrix.org",
		} {
			legacyVal, err := plugin.kvstore.Get(legacy)
			require.NoError(t, err, legacy)
			assert.Empty(t, legacyVal, legacy)
		}

		// The channel mapping value is converted to the server-scoped shape.
		assertChannelRoom(t, plugin, "channel456", "!room789:matrix.org")
	})

	t.Run("DirectReRunIsNoOp", func(t *testing.T) {
		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		plugin.logger = &testLogger{t: t}
		seedTestServerConfig(plugin)

		require.NoError(t, plugin.kvstore.Set("matrix_user_@alice:matrix.org", []byte("user123")))
		require.NoError(t, plugin.kvstore.Set("channel_mapping_channel456", []byte("!room789:matrix.org")))

		_, err := plugin.runMigrationToVersion3WithResults()
		require.NoError(t, err)

		memStore := plugin.kvstore.(*MemoryKVStore)
		sizeAfterFirst := memStore.Size()
		firstValue, err := plugin.kvstore.Get(kvstore.BuildMatrixUserKey(testServerID, "@alice:matrix.org"))
		require.NoError(t, err)

		// Running the v3 migration again must not change anything.
		_, err = plugin.runMigrationToVersion3WithResults()
		require.NoError(t, err)

		assert.Equal(t, sizeAfterFirst, memStore.Size())
		secondValue, err := plugin.kvstore.Get(kvstore.BuildMatrixUserKey(testServerID, "@alice:matrix.org"))
		require.NoError(t, err)
		assert.Equal(t, string(firstValue), string(secondValue))
		assertChannelRoom(t, plugin, "channel456", "!room789:matrix.org")
	})

	t.Run("MigrateResetReRunIsIdempotentAndDoesNotCorruptMappings", func(t *testing.T) {
		// Simulates the /matrix migrate admin command (reset version to 0 and
		// re-run the whole chain) against data that has already been migrated to
		// v3. The legacy v1/v2 repair must not mis-derive namespaced keys.
		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		plugin.logger = &testLogger{t: t}
		seedTestServerConfig(plugin)

		// A v2 install with forward + reverse user mappings and a channel mapping.
		require.NoError(t, plugin.kvstore.Set(kvstore.KeyStoreVersion, []byte("2")))
		require.NoError(t, plugin.kvstore.Set("matrix_user_@alice:matrix.org", []byte("user123")))
		require.NoError(t, plugin.kvstore.Set("mattermost_user_user123", []byte("@alice:matrix.org")))
		require.NoError(t, plugin.kvstore.Set("room_mapping_!room1:matrix.org", []byte("chan1")))
		require.NoError(t, plugin.kvstore.Set("channel_mapping_chan1", []byte("!room1:matrix.org")))

		// Normal upgrade to v3.
		require.NoError(t, plugin.runKVStoreMigrations())

		// Reset and re-run, as /matrix migrate does.
		require.NoError(t, plugin.kvstore.Set(kvstore.KeyStoreVersion, []byte("0")))
		require.NoError(t, plugin.runKVStoreMigrations())

		// Reverse user mapping must still be the Matrix user ID, not a value with
		// the serverID prefix leaked into it.
		rev, err := plugin.kvstore.Get(kvstore.BuildMattermostUserKey(testServerID, "user123"))
		require.NoError(t, err)
		assert.Equal(t, "@alice:matrix.org", string(rev))

		// Forward user mapping and channel mapping remain correct.
		fwd, err := plugin.kvstore.Get(kvstore.BuildMatrixUserKey(testServerID, "@alice:matrix.org"))
		require.NoError(t, err)
		assert.Equal(t, "user123", string(fwd))
		assertChannelRoom(t, plugin, "chan1", "!room1:matrix.org")

		// Reverse room mapping stays attributed to the server, uncorrupted.
		roomRev, err := plugin.kvstore.Get(kvstore.BuildRoomMappingKey(testServerID, "!room1:matrix.org"))
		require.NoError(t, err)
		assert.Equal(t, "chan1", string(roomRev))
	})

	t.Run("ChannelConversionSetFailureAbortsVersionBump", func(t *testing.T) {
		// Failing the channel_mapping conversion write (which runs after the
		// prefix rekey) must abort the migration so the version stays at 2 and
		// the un-converted value is retried next activation.
		base := NewMemoryKVStore()
		plugin := setupPluginForTest()
		plugin.logger = &testLogger{t: t}
		plugin.kvstore = base
		seedTestServerConfig(plugin)

		require.NoError(t, base.Set(kvstore.KeyStoreVersion, []byte("2")))
		require.NoError(t, base.Set("channel_mapping_chan1", []byte("!room1:matrix.org")))

		// Fail only channel_mapping writes; the prefix-rekey step (other prefixes)
		// still succeeds, so the failure is isolated to the conversion step.
		plugin.kvstore = &failOnSetKVStore{KVStore: base, failKeySubstr: kvstore.KeyPrefixChannelMapping}

		err := plugin.runKVStoreMigrations()
		require.Error(t, err, "a failed channel conversion must fail the migration")

		versionBytes, err := base.Get(kvstore.KeyStoreVersion)
		require.NoError(t, err)
		assert.Equal(t, "2", string(versionBytes))

		// The value is left un-converted (bare string), to be retried later.
		value, err := base.Get("channel_mapping_chan1")
		require.NoError(t, err)
		assert.Equal(t, "!room1:matrix.org", string(value))
	})

	t.Run("SetFailureAbortsVersionBumpAndPreservesLegacyKey", func(t *testing.T) {
		base := NewMemoryKVStore()
		plugin := setupPluginForTest()
		plugin.logger = &testLogger{t: t}
		plugin.kvstore = base
		seedTestServerConfig(plugin)

		// v2 install with a legacy un-namespaced key.
		require.NoError(t, base.Set(kvstore.KeyStoreVersion, []byte("2")))
		require.NoError(t, base.Set("matrix_user_@alice:matrix.org", []byte("user123")))

		// Fail writes of the namespaced matrix_user key to simulate a transient
		// KV failure partway through the rekey.
		plugin.kvstore = &failOnSetKVStore{KVStore: base, failKeySubstr: kvstore.KeyPrefixMatrixUser + testServerID}

		err := plugin.runKVStoreMigrations()
		require.Error(t, err, "a failed rekey must fail the migration")

		// The version marker must NOT advance to 3, so the migration retries later.
		versionBytes, err := base.Get(kvstore.KeyStoreVersion)
		require.NoError(t, err)
		assert.Equal(t, "2", string(versionBytes))

		// The legacy key must still be present (it was never deleted).
		legacy, err := base.Get("matrix_user_@alice:matrix.org")
		require.NoError(t, err)
		assert.Equal(t, "user123", string(legacy))
	})
}

// TestMigrateExistingSingleServerInstall exercises the full transition of an
// existing single-server install (flat plugin.json config + legacy v2 KV data,
// no registry) into the multi-server layout: the global config is projected into
// a one-entry registry with a minted serverID, all mappings are re-attributed to
// that serverID, and the per-server username prefix resolves from the registry.
func TestMigrateExistingSingleServerInstall(t *testing.T) {
	plugin := setupPluginForTest()
	plugin.kvstore = NewMemoryKVStore()
	plugin.logger = &testLogger{t: t}
	plugin.remoteID = "remote-abc"
	plugin.pendingFiles = NewPendingFileTracker()
	plugin.postTracker = NewPostTracker(DefaultPostTrackerMaxEntries)

	// Existing single-server install: flat plugin.json config is the only source,
	// and there is no server registry yet.
	plugin.configuration = &configuration{
		MatrixServerURL:      "https://matrix.example.com",
		MatrixServerName:     "example.com",
		MatrixASToken:        "as-token",
		MatrixHSToken:        "hs-token",
		MatrixUsernamePrefix: "mxprefix",
		EnableSync:           true,
	}

	// Legacy v2 KV data written by the single-server plugin (un-namespaced keys,
	// bare channel_mapping value).
	require.NoError(t, plugin.kvstore.Set(kvstore.KeyStoreVersion, []byte("2")))
	require.NoError(t, plugin.kvstore.Set("matrix_user_@alice:example.com", []byte("mmuser1")))
	require.NoError(t, plugin.kvstore.Set("mattermost_user_mmuser1", []byte("@alice:example.com")))
	require.NoError(t, plugin.kvstore.Set("ghost_user_mmuser2", []byte("@_mattermost_mmuser2:example.com")))
	require.NoError(t, plugin.kvstore.Set("room_mapping_!room1:example.com", []byte("chan1")))
	require.NoError(t, plugin.kvstore.Set("channel_mapping_chan1", []byte("!room1:example.com")))

	// No registry exists before migration.
	before, err := plugin.getServers()
	require.NoError(t, err)
	require.Empty(t, before)

	// Run migrations, mirroring activation: reconcile mints the serverID from the
	// flat config, then v3 re-attributes the existing data to it.
	require.NoError(t, plugin.runKVStoreMigrations())

	// A single registry entry now exists, projected from the flat plugin.json.
	servers, err := plugin.getServers()
	require.NoError(t, err)
	require.Len(t, servers, 1)
	entry := servers[0]
	require.NotEmpty(t, entry.ServerID)
	assert.Equal(t, "https://matrix.example.com", entry.ServerURL)
	assert.Equal(t, "example.com", entry.ServerName)
	assert.Equal(t, "as-token", entry.ASToken)
	assert.Equal(t, "hs-token", entry.HSToken)
	assert.Equal(t, "mxprefix", entry.UsernamePrefix)
	assert.True(t, entry.Enabled)
	assert.Equal(t, "remote-abc", entry.RemoteID)

	sid := entry.ServerID

	// Version advanced to current.
	versionBytes, err := plugin.kvstore.Get(kvstore.KeyStoreVersion)
	require.NoError(t, err)
	version, err := strconv.Atoi(string(versionBytes))
	require.NoError(t, err)
	assert.Equal(t, kvstore.CurrentKVStoreVersion, version)

	// Every per-server mapping is re-keyed under the minted serverID, and the
	// legacy un-namespaced keys are removed.
	assertMigrated := func(newKey, legacyKey, want string) {
		got, err := plugin.kvstore.Get(newKey)
		require.NoError(t, err)
		assert.Equal(t, want, string(got), newKey)
		old, err := plugin.kvstore.Get(legacyKey)
		require.NoError(t, err)
		assert.Empty(t, old, legacyKey)
	}
	assertMigrated(kvstore.BuildMatrixUserKey(sid, "@alice:example.com"), "matrix_user_@alice:example.com", "mmuser1")
	assertMigrated(kvstore.BuildMattermostUserKey(sid, "mmuser1"), "mattermost_user_mmuser1", "@alice:example.com")
	assertMigrated(kvstore.BuildGhostUserKey(sid, "mmuser2"), "ghost_user_mmuser2", "@_mattermost_mmuser2:example.com")
	assertMigrated(kvstore.BuildRoomMappingKey(sid, "!room1:example.com"), "room_mapping_!room1:example.com", "chan1")

	// The channel mapping value is converted to the server-scoped shape.
	data, err := plugin.kvstore.Get(kvstore.BuildChannelMappingKey("chan1"))
	require.NoError(t, err)
	mappings, err := kvstore.ParseChannelServerMappings(data)
	require.NoError(t, err)
	assert.Equal(t, "!room1:example.com", kvstore.RoomIDForServer(mappings, sid))

	// The previously-global username prefix now resolves from the per-server
	// registry entry, keyed by the minted serverID.
	plugin.initBridges()
	assert.Equal(t, "mxprefix", plugin.mattermostToMatrixBridge.matrixUsernamePrefix())
}

// assertChannelRoom checks that the channel_mapping_ value for channelID is the
// server-scoped []ChannelServerMapping shape and maps to expectedRoomID for the
// test server.
func assertChannelRoom(t *testing.T, plugin *Plugin, channelID, expectedRoomID string) {
	t.Helper()
	data, err := plugin.kvstore.Get(kvstore.BuildChannelMappingKey(channelID))
	require.NoError(t, err)
	mappings, err := kvstore.ParseChannelServerMappings(data)
	require.NoError(t, err)
	assert.Equal(t, expectedRoomID, kvstore.RoomIDForServer(mappings, testServerID))
}
