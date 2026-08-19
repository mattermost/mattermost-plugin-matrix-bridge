package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
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

		// No version key exists (version 0)
		versionData, err := plugin.kvstore.Get(kvstore.KeyStoreVersion)
		assert.NoError(t, err)
		assert.Empty(t, versionData) // Should not exist

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

		// Verify all reverse mappings were created
		for i := range MigrationBatchSize + 100 {
			mattermostUserID := "user" + strconv.Itoa(i)
			expectedMatrixUserID := "@user" + strconv.Itoa(i) + ":matrix.org"

			reverseKey := "mattermost_user_" + mattermostUserID // legacy (pre-v3) un-namespaced key
			valueBytes, err := plugin.kvstore.Get(reverseKey)
			assert.NoError(t, err)
			assert.Equal(t, expectedMatrixUserID, string(valueBytes))
		}
	})

	t.Run("SkipsAlreadyNamespacedV3Keys", func(t *testing.T) {
		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		plugin.logger = &testLogger{t: t}

		// Register one server, as if the v3 migration has already run.
		serversData, err := kvstore.MarshalServersConfig([]kvstore.ServerConfig{
			{ServerID: "server1theserveridxxxxxxxx", ServerName: "server1.example.com", Enabled: true},
		})
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.KeyServersConfig, serversData))

		// A valid v3-namespaced forward mapping and its correct reverse mapping,
		// exactly as they'd look right after a completed v3 migration.
		require.NoError(t, plugin.kvstore.Set("matrix_user_server1theserveridxxxxxxxx_@alice:matrix.org", []byte("user123")))
		require.NoError(t, plugin.kvstore.Set("mattermost_user_server1theserveridxxxxxxxx_user123", []byte("@alice:matrix.org")))

		// A legacy (pre-v3) key should still be migrated normally alongside the
		// namespaced one, proving the skip is targeted rather than blanket.
		require.NoError(t, plugin.kvstore.Set("matrix_user_@bob:matrix.org", []byte("user456")))

		// Simulate /matrix migrate re-running the v1 migration against this
		// already-v3 store (executeMigrateCommand resets the version marker to 0).
		_, err = plugin.migrateUserMappingsWithResults()
		require.NoError(t, err)

		// The namespaced key must not be mistaken for a legacy one: no corrupted
		// legacy reverse mapping should be created from it...
		legacyReverse, err := plugin.kvstore.Get("mattermost_user_user123")
		require.NoError(t, err)
		assert.Empty(t, legacyReverse, "must not create a legacy reverse mapping from an already-namespaced key")

		// ...and the correct existing v3 reverse mapping must be untouched.
		v3Reverse, err := plugin.kvstore.Get("mattermost_user_server1theserveridxxxxxxxx_user123")
		require.NoError(t, err)
		assert.Equal(t, "@alice:matrix.org", string(v3Reverse))

		// The legacy key is still migrated normally.
		bobReverse, err := plugin.kvstore.Get("mattermost_user_user456")
		require.NoError(t, err)
		assert.Equal(t, "@bob:matrix.org", string(bobReverse))
	})

	t.Run("SkipsNamespacedKeysOfARemovedServer", func(t *testing.T) {
		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		plugin.logger = &testLogger{t: t}

		// No servers registered at all - as if the server that owns these
		// namespaced keys was removed via RemoveServer, which intentionally leaves
		// its namespaced KV records in place for later re-adoption via AddServer.
		// /matrix migrate only refuses to reset the version marker while 2+ servers
		// are *currently* registered, so this is reachable with the registry empty.
		require.NoError(t, plugin.kvstore.Set("matrix_user_removedserveridxxxxxxxxxxx_@alice:matrix.org", []byte("user123")))
		require.NoError(t, plugin.kvstore.Set("mattermost_user_removedserveridxxxxxxxxxxx_user123", []byte("@alice:matrix.org")))

		_, err := plugin.migrateUserMappingsWithResults()
		require.NoError(t, err)

		// The removed server's namespaced key must still be recognized as v3-shaped
		// from its structure alone - a registry-based check would miss it here,
		// since the registry no longer lists that server.
		legacyReverse, err := plugin.kvstore.Get("mattermost_user_user123")
		require.NoError(t, err)
		assert.Empty(t, legacyReverse, "must not create a legacy reverse mapping from a removed server's namespaced key")

		v3Reverse, err := plugin.kvstore.Get("mattermost_user_removedserveridxxxxxxxxxxx_user123")
		require.NoError(t, err)
		assert.Equal(t, "@alice:matrix.org", string(v3Reverse))
	})
}

func TestMigrateChannelMappings(t *testing.T) {
	t.Run("MigrateChannelsWithRoomIDs", func(t *testing.T) {
		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		plugin.logger = &testLogger{t: t}

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
		// No legacy configuration mocked, so legacyMatrixClientForMigration returns nil,
		// simulating alias resolution being unavailable.

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

		// Room ID mapping should not exist due to no legacy client being available
		roomIDMapping, err := plugin.kvstore.Get("room_mapping_!any:matrix.org")
		assert.NoError(t, err)
		assert.Empty(t, roomIDMapping)
	})

	t.Run("MigrateChannelsWithAliasesAndWorkingClient", func(t *testing.T) {
		const resolvedRoomID = "!resolved:matrix.org"

		// A minimal stand-in Matrix homeserver that resolves any room alias lookup,
		// so legacyMatrixClientForMigration gets a client that can actually succeed.
		matrixServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "Bearer legacy-as-token", r.Header.Get("Authorization"))
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"room_id": resolvedRoomID})
		}))
		defer matrixServer.Close()

		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		plugin.logger = &testLogger{t: t}

		// Override the default "no legacy configuration" expectation from
		// setupPluginForTest so legacyMatrixClientForMigration builds a real client
		// pointed at our stub server instead of returning nil.
		api := plugin.API.(*plugintest.API)
		clearMockExpectations(api)
		api.On("LogDebug", mock.Anything, mock.Anything).Maybe()
		api.On("LogDebug", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()
		api.On("LogDebug", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()
		api.On("LogWarn", mock.Anything).Maybe()
		api.On("LogWarn", mock.Anything, mock.Anything).Maybe()
		api.On("LoadPluginConfiguration", mock.Anything).Run(func(args mock.Arguments) {
			dest := args.Get(0).(*legacyServerConfig)
			dest.MatrixServerURL = matrixServer.URL
			dest.MatrixASToken = "legacy-as-token"
			dest.MatrixServerName = "matrix.org"
		}).Return(nil)

		// Add test channel mapping with alias
		err := plugin.kvstore.Set("channel_mapping_channel123", []byte("#test:matrix.org"))
		require.NoError(t, err)

		// Run channel migration with a working legacy Matrix client available
		_, err = plugin.migrateChannelMappingsWithResults()
		require.NoError(t, err)

		// Check that alias reverse mapping was created
		aliasReverseBytes, err := plugin.kvstore.Get("room_mapping_#test:matrix.org")
		require.NoError(t, err)
		assert.Equal(t, "channel123", string(aliasReverseBytes))

		// With a working legacy client, alias resolution succeeds, and the resolved
		// room ID must also get its own mapping written.
		roomIDMapping, err := plugin.kvstore.Get("room_mapping_" + resolvedRoomID)
		require.NoError(t, err)
		assert.Equal(t, "channel123", string(roomIDMapping))
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

		// Verify all reverse mappings were created
		for i := range MigrationBatchSize + 50 {
			channelID := "channel" + strconv.Itoa(i)
			roomID := "!room" + strconv.Itoa(i) + ":matrix.org"

			reverseKey := "room_mapping_" + roomID // legacy (pre-v3) un-namespaced key
			valueBytes, err := plugin.kvstore.Get(reverseKey)
			assert.NoError(t, err)
			assert.Equal(t, channelID, string(valueBytes))
		}
	})

	t.Run("SkipsAlreadyV3ShapedValues", func(t *testing.T) {
		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		plugin.logger = &testLogger{t: t}

		// A channel_mapping_ value already converted to the v3 []ChannelServerMapping
		// JSON shape, as it would look right after a completed v3 migration.
		v3Value, err := kvstore.BuildSingleChannelMapping("server1", "!room:example.com")
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set("channel_mapping_channel456", v3Value))

		// Simulate /matrix migrate re-running the v1 migration against this
		// already-v3 store (executeMigrateCommand resets the version marker to 0).
		_, err = plugin.migrateChannelMappingsWithResults()
		require.NoError(t, err)

		// The v3 value must not be mistaken for a legacy bare room identifier: no
		// reverse mapping built from the raw JSON text should have been created.
		bogusReverse, err := plugin.kvstore.Get("room_mapping_" + string(v3Value))
		require.NoError(t, err)
		assert.Empty(t, bogusReverse, "must not create a reverse mapping from a v3 JSON channel mapping value")

		// The original v3 value is left untouched.
		unchanged, err := plugin.kvstore.Get("channel_mapping_channel456")
		require.NoError(t, err)
		assert.Equal(t, v3Value, unchanged)
	})
}

func TestMigrationIntegration(t *testing.T) {
	t.Run("FullMigrationScenario", func(t *testing.T) {
		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		plugin.logger = &testLogger{t: t}

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
		versionData, err := plugin.kvstore.Get(kvstore.KeyStoreVersion)
		assert.NoError(t, err)
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
		oldDMMapping, err := plugin.kvstore.Get("dm_mapping_dm123")
		assert.NoError(t, err)
		assert.Empty(t, oldDMMapping) // Should be deleted

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
