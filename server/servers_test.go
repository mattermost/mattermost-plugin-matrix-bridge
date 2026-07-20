package main

import (
	"encoding/json"
	"testing"

	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
)

// stubAllLogging registers permissive stubs for every plugin-API log method across
// the arities used by the code under test, so a stray log call never panics the mock.
func stubAllLogging(api *plugintest.API) {
	for _, level := range []string{"LogDebug", "LogInfo", "LogWarn", "LogError"} {
		for n := 1; n <= 12; n++ {
			args := make([]any, n)
			for i := range args {
				args[i] = mock.Anything
			}
			api.On(level, args...).Maybe()
		}
	}
}

func TestReconcileServerConfig(t *testing.T) {
	t.Run("MintsAndDerivesFieldsFromFlatConfig", func(t *testing.T) {
		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		plugin.logger = &testLogger{t: t}
		plugin.remoteID = "remote-xyz"
		plugin.configuration = &configuration{
			MatrixServerURL:      "https://matrix.example.com",
			MatrixServerName:     "example.com",
			MatrixASToken:        "as-token",
			MatrixHSToken:        "hs-token",
			MatrixUsernamePrefix: "mx",
			EnableSync:           true,
		}

		servers, err := plugin.reconcileServerConfig()
		require.NoError(t, err)
		require.Len(t, servers, 1)

		s := servers[0]
		expectedID, err := deriveServerID("https://matrix.example.com")
		require.NoError(t, err)
		assert.Equal(t, expectedID, s.ServerID, "serverID is derived from the URL hostname")
		assert.Equal(t, "https://matrix.example.com", s.ServerURL)
		assert.Equal(t, "example.com", s.ServerName)
		assert.Equal(t, "as-token", s.ASToken)
		assert.Equal(t, "hs-token", s.HSToken)
		assert.Equal(t, "mx", s.UsernamePrefix)
		assert.True(t, s.Enabled)
		assert.Equal(t, "remote-xyz", s.RemoteID)

		// The entry is persisted and reloads to the same serverID.
		reloaded, err := plugin.getServers()
		require.NoError(t, err)
		require.Len(t, reloaded, 1)
		assert.Equal(t, s.ServerID, reloaded[0].ServerID)
	})

	t.Run("ServerIDStableWhenHostnameUnchanged", func(t *testing.T) {
		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		plugin.logger = &testLogger{t: t}
		plugin.configuration = &configuration{MatrixServerURL: "https://a.example.com"}

		first, err := plugin.reconcileServerConfig()
		require.NoError(t, err)
		require.Len(t, first, 1)
		originalID := first[0].ServerID
		require.NotEmpty(t, originalID)

		// A config edit that touches other fields but keeps the same hostname
		// (here also normalizing scheme/port/trailing-slash) must keep the same
		// serverID, even from a fresh process (cache cleared).
		plugin.configuration = &configuration{MatrixServerURL: "http://a.example.com:8008/", MatrixASToken: "new"}
		plugin.serverID = ""

		second, err := plugin.reconcileServerConfig()
		require.NoError(t, err)
		require.Len(t, second, 1)
		assert.Equal(t, originalID, second[0].ServerID, "serverID is stable while the hostname is unchanged")
		assert.Equal(t, "http://a.example.com:8008/", second[0].ServerURL, "other fields update from flat config")
	})

	t.Run("ServerIDChangesWhenHostnameChanges", func(t *testing.T) {
		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		plugin.logger = &testLogger{t: t}
		plugin.configuration = &configuration{MatrixServerURL: "https://a.example.com"}

		first, err := plugin.reconcileServerConfig()
		require.NoError(t, err)
		originalID := first[0].ServerID

		// Repointing to a different homeserver hostname derives a new serverID
		// (records under the old ID are orphaned; a warning is logged).
		plugin.configuration = &configuration{MatrixServerURL: "https://b.example.com"}
		plugin.serverID = ""

		second, err := plugin.reconcileServerConfig()
		require.NoError(t, err)
		require.Len(t, second, 1)
		expectedID, err := deriveServerID("https://b.example.com")
		require.NoError(t, err)
		assert.Equal(t, expectedID, second[0].ServerID)
		assert.NotEqual(t, originalID, second[0].ServerID, "a hostname change re-derives the serverID")
	})

	t.Run("NoEntryWhenServerURLEmpty", func(t *testing.T) {
		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		plugin.logger = &testLogger{t: t}
		plugin.configuration = &configuration{MatrixServerURL: ""}

		// No URL configured (e.g. sync disabled): reconcile is a no-op that must
		// not fail activation and must not write a useless entry.
		servers, err := plugin.reconcileServerConfig()
		require.NoError(t, err)
		assert.Empty(t, servers, "no server URL means no registry entry")

		reloaded, err := plugin.getServers()
		require.NoError(t, err)
		assert.Empty(t, reloaded)
	})

	t.Run("ErrorsOnUnparseableServerURL", func(t *testing.T) {
		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		plugin.logger = &testLogger{t: t}
		plugin.configuration = &configuration{MatrixServerURL: "http://"}

		_, err := plugin.reconcileServerConfig()
		require.Error(t, err, "a non-empty but unusable URL must fail loudly, not derive an empty serverID")
	})

	t.Run("ReReconcileUpdatesDerivedFields", func(t *testing.T) {
		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		plugin.logger = &testLogger{t: t}
		plugin.remoteID = "remote-1"
		plugin.configuration = &configuration{MatrixServerURL: "https://a.example.com", EnableSync: true}

		first, err := plugin.reconcileServerConfig()
		require.NoError(t, err)
		require.Len(t, first, 1)
		assert.Equal(t, "remote-1", first[0].RemoteID)
		assert.True(t, first[0].Enabled)
		originalID := first[0].ServerID

		// A later reconcile (e.g. after shared-channels registration and a config
		// edit that disables sync) refreshes the derived fields but keeps serverID.
		plugin.remoteID = "remote-2"
		plugin.configuration = &configuration{MatrixServerURL: "https://a.example.com", EnableSync: false}

		second, err := plugin.reconcileServerConfig()
		require.NoError(t, err)
		require.Len(t, second, 1)
		assert.Equal(t, originalID, second[0].ServerID)
		assert.Equal(t, "remote-2", second[0].RemoteID)
		assert.False(t, second[0].Enabled)
	})

	t.Run("PreservesInjectedServersAcrossReconcile", func(t *testing.T) {
		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		plugin.logger = &testLogger{t: t}
		plugin.configuration = &configuration{MatrixServerURL: "https://a.example.com", EnableSync: true}

		// Establish the primary, then persist an injected extra server alongside it.
		first, err := plugin.reconcileServerConfig()
		require.NoError(t, err)
		require.Len(t, first, 1)
		primaryID := first[0].ServerID

		injected := kvstore.ServerConfig{ServerID: "injected01injected01inject1", ServerURL: "https://b.example.com", ServerName: "b.example.com", HSToken: "hs-b", Enabled: true, Injected: true}
		require.NoError(t, plugin.persistServers(append(first, injected)))

		// A later reconcile (warm cache) keeps the injected server and refreshes the primary.
		second, err := plugin.reconcileServerConfig()
		require.NoError(t, err)
		require.Len(t, second, 2)
		assert.Equal(t, primaryID, second[0].ServerID, "primary stays first")
		assert.False(t, second[0].Injected)
		assert.Equal(t, "injected01injected01inject1", second[1].ServerID, "injected server preserved")
		assert.True(t, second[1].Injected)

		// A cold reconcile (cleared cache) still preserves the injected server, and a
		// primary URL change replaces only the primary — the injected extra survives.
		plugin.serverID = ""
		plugin.configuration = &configuration{MatrixServerURL: "https://c.example.com", EnableSync: true}
		third, err := plugin.reconcileServerConfig()
		require.NoError(t, err)
		require.Len(t, third, 2)
		newPrimaryID, err := deriveServerID("https://c.example.com")
		require.NoError(t, err)
		assert.Equal(t, newPrimaryID, third[0].ServerID)
		assert.NotEqual(t, primaryID, third[0].ServerID, "old primary replaced, not preserved")
		assert.Equal(t, "injected01injected01inject1", third[1].ServerID, "injected server still preserved after primary URL change")
	})

	t.Run("DoesNotMintOnReadError", func(t *testing.T) {
		base := NewMemoryKVStore()
		plugin := setupPluginForTest()
		plugin.logger = &testLogger{t: t}
		plugin.configuration = &configuration{MatrixServerURL: "https://a.example.com"}
		plugin.kvstore = base

		// An existing registry with a stable serverID.
		seedTestServerConfig(plugin)

		// Reads of servers_config now fail (transient backend error).
		plugin.kvstore = &failOnGetKVStore{KVStore: base, failKeySubstr: kvstore.KeyServersConfig}
		plugin.serverID = "" // force a registry read rather than using the cache

		_, err := plugin.getServers()
		require.Error(t, err, "a real read failure must surface, not look like an empty registry")

		_, err = plugin.reconcileServerConfig()
		require.Error(t, err, "reconcile must fail rather than mint a fresh serverID on a read error")

		// The persisted serverID is unchanged — no re-mint, no orphaned records.
		plugin.kvstore = base
		servers, err := plugin.getServers()
		require.NoError(t, err)
		require.Len(t, servers, 1)
		assert.Equal(t, testServerID, servers[0].ServerID)
	})

	t.Run("GetServersNilKVStore", func(t *testing.T) {
		plugin := setupPluginForTest()
		servers, err := plugin.getServers()
		require.NoError(t, err)
		assert.Nil(t, servers)
	})

	t.Run("GetServersMalformedJSONReturnsError", func(t *testing.T) {
		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		require.NoError(t, plugin.kvstore.Set(kvstore.KeyServersConfig, []byte("not json")))

		_, err := plugin.getServers()
		assert.Error(t, err)
	})
}

// TestDeriveServerID verifies the deterministic serverID derivation: same
// hostname (regardless of scheme/port/path/case) yields the same 26-char base32
// ID, distinct hostnames yield distinct IDs, and an unusable URL errors.
func TestManagedServers(t *testing.T) {
	const primaryURL = "https://primary.example.com"
	const extraURL = "https://extra.example.com"

	newPlugin := func(t *testing.T) *Plugin {
		t.Helper()
		api := &plugintest.API{}
		stubAllLogging(api)
		plugin := &Plugin{}
		plugin.SetAPI(api)
		plugin.logger = &testLogger{t: t}
		plugin.kvstore = NewMemoryKVStore()
		plugin.remoteID = "rid-primary"
		plugin.configuration = &configuration{MatrixServerURL: primaryURL, EnableSync: true}
		return plugin
	}

	t.Run("AddStampsInjectedAndSurvivesReconcile", func(t *testing.T) {
		plugin := newPlugin(t)

		serverID, err := plugin.AddManagedServer(extraURL, "extra.example.com", "as-x", "hs-x", "mx")
		require.NoError(t, err)
		expectedID, err := deriveServerID(extraURL)
		require.NoError(t, err)
		assert.Equal(t, expectedID, serverID, "serverID is derived from the URL hostname")

		servers, err := plugin.GetManagedServers()
		require.NoError(t, err)
		require.Len(t, servers, 2, "primary plus the injected extra")

		extra, ok := kvstore.ServerConfigForID(servers, serverID)
		require.True(t, ok)
		assert.True(t, extra.Injected, "an added server is marked injected")
		assert.Equal(t, "hs-x", extra.HSToken)
		assert.Equal(t, "as-x", extra.ASToken)
		assert.Equal(t, "mx", extra.UsernamePrefix)
		assert.True(t, extra.Enabled)

		// A later reconcile (driven by any config change) must not drop the injected
		// server: the Injected flag is exactly what makes it survive.
		reconciled, err := plugin.reconcileServerConfig()
		require.NoError(t, err)
		_, stillThere := kvstore.ServerConfigForID(reconciled, serverID)
		assert.True(t, stillThere, "injected server survives reconcile")
	})

	t.Run("AddUpsertsInPlaceAndPreservesRemoteID", func(t *testing.T) {
		plugin := newPlugin(t)

		serverID, err := plugin.AddManagedServer(extraURL, "extra.example.com", "as-1", "hs-1", "mx")
		require.NoError(t, err)

		// Give the injected entry a distinct RemoteID, as a real shared-channels
		// registration would.
		servers, err := plugin.getServers()
		require.NoError(t, err)
		for i := range servers {
			if servers[i].ServerID == serverID {
				servers[i].RemoteID = "preserved-rid"
			}
		}
		require.NoError(t, plugin.persistServers(servers))

		// Re-adding the same URL updates fields in place and keeps the RemoteID.
		_, err = plugin.AddManagedServer(extraURL, "extra.example.com", "as-2", "hs-2", "mx2")
		require.NoError(t, err)

		after, err := plugin.GetManagedServers()
		require.NoError(t, err)
		assert.Len(t, after, 2, "re-adding the same server does not create a duplicate")
		extra, ok := kvstore.ServerConfigForID(after, serverID)
		require.True(t, ok)
		assert.Equal(t, "hs-2", extra.HSToken, "tokens are updated in place")
		assert.Equal(t, "preserved-rid", extra.RemoteID, "existing RemoteID is preserved on re-add")
	})

	t.Run("Remove", func(t *testing.T) {
		plugin := newPlugin(t)
		serverID, err := plugin.AddManagedServer(extraURL, "extra.example.com", "as", "hs", "mx")
		require.NoError(t, err)

		// Removing an injected server drops it.
		removed, err := plugin.RemoveManagedServer(serverID)
		require.NoError(t, err)
		assert.True(t, removed)
		servers, err := plugin.GetManagedServers()
		require.NoError(t, err)
		_, present := kvstore.ServerConfigForID(servers, serverID)
		assert.False(t, present, "injected server is removed")

		// Removing an unknown server reports not-found.
		removed, err = plugin.RemoveManagedServer("no-such-server")
		require.NoError(t, err)
		assert.False(t, removed)
	})

	t.Run("RemovePrimaryReappearsAfterReconcile", func(t *testing.T) {
		plugin := newPlugin(t)
		// Establish the primary.
		_, err := plugin.reconcileServerConfig()
		require.NoError(t, err)
		primaryID, err := deriveServerID(primaryURL)
		require.NoError(t, err)

		removed, err := plugin.RemoveManagedServer(primaryID)
		require.NoError(t, err)
		assert.True(t, removed, "the primary entry is found and removed")

		// RemoveManagedServer rebuilds via reconcile, which re-derives the primary
		// from the flat config, so it comes back.
		servers, err := plugin.GetManagedServers()
		require.NoError(t, err)
		_, present := kvstore.ServerConfigForID(servers, primaryID)
		assert.True(t, present, "the primary is re-added from the flat configuration")
	})
}

func TestDeriveServerID(t *testing.T) {
	const base32Alphabet = "ybndrfg8ejkmcpqxot1uwisza345h769"

	t.Run("DeterministicAndFormatted", func(t *testing.T) {
		id, err := deriveServerID("https://matrix.example.com")
		require.NoError(t, err)
		assert.Len(t, id, 26, "matches model.NewId() length")
		for _, r := range id {
			assert.Contains(t, base32Alphabet, string(r), "only Mattermost base32 alphabet chars")
		}

		again, err := deriveServerID("https://matrix.example.com")
		require.NoError(t, err)
		assert.Equal(t, id, again, "same input is deterministic")
	})

	t.Run("NormalizationEquivalence", func(t *testing.T) {
		want, err := deriveServerID("https://matrix.example.com")
		require.NoError(t, err)

		for _, u := range []string{
			"http://matrix.example.com",
			"https://matrix.example.com:8008",
			"https://matrix.example.com/",
			"https://matrix.example.com/_matrix",
			"HTTPS://MATRIX.EXAMPLE.COM",
		} {
			got, err := deriveServerID(u)
			require.NoError(t, err, u)
			assert.Equal(t, want, got, "scheme/port/path/case must not change the ID: %s", u)
		}
	})

	t.Run("DistinctHostnamesDiffer", func(t *testing.T) {
		a, err := deriveServerID("https://a.example.com")
		require.NoError(t, err)
		b, err := deriveServerID("https://b.example.com")
		require.NoError(t, err)
		assert.NotEqual(t, a, b)
	})

	t.Run("ErrorsOnUnusableURL", func(t *testing.T) {
		for _, u := range []string{"", "not a url"} {
			_, err := deriveServerID(u)
			assert.Error(t, err, "expected error for %q", u)
		}
	})
}

// TestMatrixUsernamePrefixResolvesPerServer verifies the username prefix is
// resolved from the per-server registry entry (the source of truth), and that
// the flat global config is not consulted at resolution time.
func TestMatrixUsernamePrefixResolvesPerServer(t *testing.T) {
	newBridge := func(t *testing.T) *Plugin {
		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		plugin.logger = &testLogger{t: t}
		plugin.maxProfileImageSize = DefaultMaxProfileImageSize
		plugin.maxFileSize = DefaultMaxFileSize
		plugin.postTracker = NewPostTracker(DefaultPostTrackerMaxEntries)
		plugin.pendingFiles = NewPendingFileTracker()
		// A custom global prefix that must NOT leak into resolution; only the
		// registry entry (or the static default) may be returned.
		plugin.configuration = &configuration{MatrixUsernamePrefix: "globalprefix"}
		setTestMatrixClient(plugin, createMatrixClientWithTestLogger(t, "", "", ""))
		plugin.initBridges()
		return plugin
	}

	t.Run("UsesRegistryEntryPrefix", func(t *testing.T) {
		plugin := newBridge(t)
		servers := []kvstore.ServerConfig{{ServerID: testServerID, UsernamePrefix: "serverprefix"}}
		data, err := json.Marshal(servers)
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.KeyServersConfig, data))

		assert.Equal(t, "serverprefix", plugin.mattermostToMatrixBridge.matrixUsernamePrefix())
	})

	t.Run("DefaultsWhenNoRegistryEntry", func(t *testing.T) {
		plugin := newBridge(t)
		// No servers_config seeded: resolves to the static default, NOT the
		// configured global prefix.
		assert.Equal(t, DefaultMatrixUsernamePrefix, plugin.mattermostToMatrixBridge.matrixUsernamePrefix())
	})

	t.Run("DefaultsWhenEntryPrefixEmpty", func(t *testing.T) {
		plugin := newBridge(t)
		servers := []kvstore.ServerConfig{{ServerID: testServerID, UsernamePrefix: ""}}
		data, err := json.Marshal(servers)
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.KeyServersConfig, data))

		assert.Equal(t, DefaultMatrixUsernamePrefix, plugin.mattermostToMatrixBridge.matrixUsernamePrefix())
	})

	t.Run("IgnoresOtherServersEntry", func(t *testing.T) {
		plugin := newBridge(t)
		// The registry only has an entry for a DIFFERENT server; this bridge's
		// server must not pick up another server's prefix.
		servers := []kvstore.ServerConfig{{ServerID: "some-other-server", UsernamePrefix: "otherprefix"}}
		data, err := json.Marshal(servers)
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.KeyServersConfig, data))

		assert.Equal(t, DefaultMatrixUsernamePrefix, plugin.mattermostToMatrixBridge.matrixUsernamePrefix())
	})
}

// TestGetMatrixRoomIDServerIsolation exercises the per-server filtering in
// GetMatrixRoomID / RoomIDForServer, which existing tests never hit because they
// always read and write with the same serverID.
func TestGetMatrixRoomIDServerIsolation(t *testing.T) {
	plugin := setupPluginForTest()
	plugin.kvstore = NewMemoryKVStore()
	plugin.logger = &testLogger{t: t}
	plugin.maxProfileImageSize = DefaultMaxProfileImageSize
	plugin.maxFileSize = DefaultMaxFileSize
	plugin.postTracker = NewPostTracker(DefaultPostTrackerMaxEntries)
	plugin.pendingFiles = NewPendingFileTracker()
	setTestMatrixClient(plugin, createMatrixClientWithTestLogger(t, "", "", ""))
	plugin.initBridges()
	bridge := plugin.mattermostToMatrixBridge

	channelID := "channelABC"
	key := kvstore.BuildChannelMappingKey(channelID)

	t.Run("MatchingServerIDReturnsRoom", func(t *testing.T) {
		v, err := kvstore.BuildSingleChannelMapping(testServerID, "!room:hs")
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(key, v))

		got, err := bridge.GetMatrixRoomID(channelID)
		require.NoError(t, err)
		assert.Equal(t, "!room:hs", got)
	})

	t.Run("DifferentServerIDReturnsEmpty", func(t *testing.T) {
		v, err := kvstore.BuildSingleChannelMapping("some-other-server", "!other:hs")
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(key, v))

		got, err := bridge.GetMatrixRoomID(channelID)
		require.NoError(t, err)
		assert.Equal(t, "", got, "a mapping for a different server must not resolve")
	})

	t.Run("CorruptValueReturnsError", func(t *testing.T) {
		// An unparseable value is surfaced as an error rather than masked as an
		// unmapped channel (which would silently drop/mis-route messages).
		require.NoError(t, plugin.kvstore.Set(key, []byte("!not-json:hs")))

		_, err := bridge.GetMatrixRoomID(channelID)
		require.Error(t, err)
	})
}

// TestSetChannelRoomMappingPreservesOtherServers verifies the write path upserts
// only this server's entry, leaving mappings for other servers intact.
func TestSetChannelRoomMappingPreservesOtherServers(t *testing.T) {
	plugin := setupPluginForTest()
	plugin.kvstore = NewMemoryKVStore()
	plugin.logger = &testLogger{t: t}

	channelID := "channelMulti"
	key := kvstore.BuildChannelMappingKey(channelID)

	// A pre-existing mapping for a different server.
	seed, err := kvstore.MarshalChannelServerMappings([]kvstore.ChannelServerMapping{
		{ServerID: "other-server", RoomID: "!other:hs"},
	})
	require.NoError(t, err)
	require.NoError(t, plugin.kvstore.Set(key, seed))

	// A bridge for testServerID with a stub client so ResolveRoomAlias resolves.
	setTestMatrixClient(plugin, createMatrixClientWithTestLogger(t, "", "", ""))
	utils := NewBridgeUtils(BridgeUtilsConfig{
		Logger:       plugin.logger,
		API:          plugin.API,
		KVStore:      plugin.kvstore,
		MatrixClient: plugin.GetMatrixClient(),
		ServerID:     testServerID,
		ConfigGetter: plugin,
	})

	require.NoError(t, utils.setChannelRoomMapping(channelID, "!mine:hs"))

	data, err := plugin.kvstore.Get(key)
	require.NoError(t, err)
	mappings, err := kvstore.ParseChannelServerMappings(data)
	require.NoError(t, err)

	// Both servers' entries are present.
	assert.Equal(t, "!other:hs", kvstore.RoomIDForServer(mappings, "other-server"), "other server's mapping must be preserved")
	assert.Equal(t, "!mine:hs", kvstore.RoomIDForServer(mappings, testServerID), "this server's mapping must be upserted")
	assert.Len(t, mappings, 2)
}

// TestSetChannelRoomMappingRequiresServerID verifies the guard that refuses to
// persist a channel mapping when the serverID is unset, which would otherwise
// write an unroutable mapping and a corrupt reverse key.
func TestSetChannelRoomMappingRequiresServerID(t *testing.T) {
	plugin := setupPluginForTest()
	plugin.kvstore = NewMemoryKVStore()
	plugin.logger = &testLogger{t: t}

	utils := NewBridgeUtils(BridgeUtilsConfig{
		Logger:       plugin.logger,
		API:          plugin.API,
		KVStore:      plugin.kvstore,
		MatrixClient: nil, // guard returns before the client is used
		ServerID:     "",
		RemoteID:     "test-remote-id",
		ConfigGetter: plugin,
	})

	err := utils.setChannelRoomMapping("channel1", "!room:matrix.org")
	require.Error(t, err, "must not persist a channel mapping without a serverID")

	// Nothing was written.
	data, err := plugin.kvstore.Get(kvstore.BuildChannelMappingKey("channel1"))
	require.NoError(t, err)
	assert.Empty(t, data)
}
