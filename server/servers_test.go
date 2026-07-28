package main

import (
	"encoding/json"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
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

func TestMaterializeServerFromLegacyConfig(t *testing.T) {
	t.Run("MintsFromLegacyFlatConfig", func(t *testing.T) {
		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		plugin.logger = &testLogger{t: t}
		mockLegacyPluginConfig(plugin.API.(*plugintest.API), legacyServerConfig{
			MatrixServerURL:      "https://matrix.example.com",
			MatrixServerName:     "example.com",
			MatrixASToken:        "as-token",
			MatrixHSToken:        "hs-token",
			MatrixUsernamePrefix: "mx",
			EnableSync:           true,
		})

		serverID, err := plugin.materializeServerFromLegacyConfig()
		require.NoError(t, err)
		expectedID, err := deriveServerID("https://matrix.example.com")
		require.NoError(t, err)
		assert.Equal(t, expectedID, serverID, "serverID is derived from the URL hostname")

		servers, err := plugin.getServers()
		require.NoError(t, err)
		require.Len(t, servers, 1)
		s := servers[0]
		assert.Equal(t, "https://matrix.example.com", s.ServerURL)
		assert.Equal(t, "example.com", s.ServerName)
		assert.Equal(t, "as-token", s.ASToken)
		assert.Equal(t, "hs-token", s.HSToken)
		assert.Equal(t, "mx", s.UsernamePrefix)
		assert.True(t, s.Enabled)
		assert.Empty(t, s.SiteURL, "the migrated server keeps the legacy plugin remote (empty SiteURL)")
		assert.Empty(t, s.RemoteID, "RemoteID is assigned later by registration, not materialization")
	})

	t.Run("NoEntryWhenLegacyURLEmpty", func(t *testing.T) {
		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		plugin.logger = &testLogger{t: t}
		mockLegacyPluginConfig(plugin.API.(*plugintest.API), legacyServerConfig{})

		serverID, err := plugin.materializeServerFromLegacyConfig()
		require.NoError(t, err)
		assert.Empty(t, serverID, "no legacy URL means no server to materialize")

		reloaded, err := plugin.getServers()
		require.NoError(t, err)
		assert.Empty(t, reloaded)
	})

	t.Run("IdempotentWhenEntryExists", func(t *testing.T) {
		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		plugin.logger = &testLogger{t: t}
		mockLegacyPluginConfig(plugin.API.(*plugintest.API), legacyServerConfig{MatrixServerURL: "https://matrix.example.com"})

		serverID, err := plugin.materializeServerFromLegacyConfig()
		require.NoError(t, err)

		// Simulate registration having assigned a RemoteID to the entry.
		servers, err := plugin.getServers()
		require.NoError(t, err)
		servers[0].RemoteID = "assigned-remote"
		require.NoError(t, plugin.persistServers(servers))

		// A second materialize is a no-op: it must not overwrite the existing entry.
		again, err := plugin.materializeServerFromLegacyConfig()
		require.NoError(t, err)
		assert.Equal(t, serverID, again)

		after, err := plugin.getServers()
		require.NoError(t, err)
		require.Len(t, after, 1, "no duplicate entry")
		assert.Equal(t, "assigned-remote", after[0].RemoteID, "existing entry is preserved")
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

	t.Run("GetSingleServerIDAmbiguousWithMultipleServers", func(t *testing.T) {
		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		plugin.logger = &testLogger{t: t}
		data, err := json.Marshal([]kvstore.ServerConfig{
			{ServerID: "s1", ServerURL: "https://a.example.com"},
			{ServerID: "s2", ServerURL: "https://b.example.com"},
		})
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.KeyServersConfig, data))

		assert.Empty(t, plugin.getSingleServerID(), "with multiple servers there is no single server")

		// Exactly one server resolves.
		single, err := json.Marshal([]kvstore.ServerConfig{{ServerID: "only", ServerURL: "https://a.example.com"}})
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.KeyServersConfig, single))
		assert.Equal(t, "only", plugin.getSingleServerID())
	})
}

// TestDeriveServerID verifies the deterministic serverID derivation: same
// hostname (regardless of scheme/port/path/case) yields the same 26-char base32
// ID, distinct hostnames yield distinct IDs, and an unusable URL errors.
func TestManagedServers(t *testing.T) {
	const primaryURL = "https://primary.example.com"
	const extraURL = "https://extra.example.com"

	// remoteIDForSiteURL models the server-side contract of
	// RegisterPluginForSharedChannels: the remote is keyed by SiteURL, so the same
	// SiteURL always yields the same remoteID (idempotent) and distinct SiteURLs
	// yield distinct remoteIDs. The empty SiteURL is the primary's legacy remote.
	remoteIDForSiteURL := func(siteURL string) string {
		if siteURL == "" {
			return "rid-primary"
		}
		return "rid:" + siteURL
	}
	// injectedRemoteID is the remote the extra server must end up with: derived from
	// its homeserver hostname, matching siteURLForServer's hostname-based SiteURL.
	injectedRemoteID := remoteIDForSiteURL("https://extra.example.com")

	newPlugin := func(t *testing.T) *Plugin {
		t.Helper()
		api := &plugintest.API{}
		stubAllLogging(api)
		// AddServer registers every server for shared channels. Stub the
		// creator lookup and return a remoteID keyed by SiteURL, faithfully modeling
		// the real API so distinct servers get distinct remotes and re-registration
		// is idempotent.
		api.On("GetUserByUsername", "mattermost-bridge").Return(&model.User{Id: "bot-user-id"}, nil).Maybe()
		api.On("RegisterPluginForSharedChannels", mock.Anything).
			Return(func(o model.RegisterPluginOpts) string { return remoteIDForSiteURL(o.SiteURL) }, nil).Maybe()
		plugin := &Plugin{}
		plugin.SetAPI(api)
		plugin.logger = &testLogger{t: t}
		plugin.kvstore = NewMemoryKVStore()
		plugin.configuration = &configuration{EnableSync: true}
		// Seed a "primary" server (the migrated single server: legacy empty SiteURL).
		primaryID, err := deriveServerID(primaryURL)
		require.NoError(t, err)
		seedServerEntry(plugin, kvstore.ServerConfig{
			ServerID: primaryID, ServerURL: primaryURL, ServerName: "primary.example.com",
			Enabled: true, RemoteID: "rid-primary", SiteURL: "",
		})
		return plugin
	}

	t.Run("AddStampsSiteURLAndPersists", func(t *testing.T) {
		plugin := newPlugin(t)

		serverID, err := plugin.AddServer(extraURL, "extra.example.com", "as-x", "hs-x", "mx")
		require.NoError(t, err)
		expectedID, err := deriveServerID(extraURL)
		require.NoError(t, err)
		assert.Equal(t, expectedID, serverID, "serverID is derived from the URL hostname")

		servers, err := plugin.GetManagedServers()
		require.NoError(t, err)
		require.Len(t, servers, 2, "primary plus the added extra")

		extra, ok := kvstore.ServerConfigForID(servers, serverID)
		require.True(t, ok)
		assert.Equal(t, "https://extra.example.com", extra.SiteURL, "an added server derives its SiteURL from the hostname")
		assert.Equal(t, "hs-x", extra.HSToken)
		assert.Equal(t, "as-x", extra.ASToken)
		assert.Equal(t, "mx", extra.UsernamePrefix)
		assert.True(t, extra.Enabled)

		// The added server is a permanent registry record (registry is authoritative).
		reloaded, err := plugin.getServers()
		require.NoError(t, err)
		_, stillThere := kvstore.ServerConfigForID(reloaded, serverID)
		assert.True(t, stillThere, "added server persists")
	})

	t.Run("AddUpsertsInPlaceAndKeepsStableRemoteID", func(t *testing.T) {
		plugin := newPlugin(t)

		serverID, err := plugin.AddServer(extraURL, "extra.example.com", "as-1", "hs-1", "mx")
		require.NoError(t, err)

		// AddServer registers the injected server, which assigns its distinct
		// remote ID immediately.
		servers, err := plugin.getServers()
		require.NoError(t, err)
		extra, ok := kvstore.ServerConfigForID(servers, serverID)
		require.True(t, ok)
		assert.Equal(t, injectedRemoteID, extra.RemoteID, "an added server is registered for its own distinct remote")

		// Re-adding the same URL updates fields in place; registration is idempotent
		// so the remote ID is unchanged.
		_, err = plugin.AddServer(extraURL, "extra.example.com", "as-2", "hs-2", "mx2")
		require.NoError(t, err)

		after, err := plugin.GetManagedServers()
		require.NoError(t, err)
		assert.Len(t, after, 2, "re-adding the same server does not create a duplicate")
		extra, ok = kvstore.ServerConfigForID(after, serverID)
		require.True(t, ok)
		assert.Equal(t, "hs-2", extra.HSToken, "tokens are updated in place")
		assert.Equal(t, injectedRemoteID, extra.RemoteID, "remote ID is stable across re-add")
	})

	t.Run("AddAssignsDistinctRemoteResolvableToServer", func(t *testing.T) {
		plugin := newPlugin(t)

		serverID, err := plugin.AddServer(extraURL, "extra.example.com", "as", "hs", "mx")
		require.NoError(t, err)

		servers, err := plugin.GetManagedServers()
		require.NoError(t, err)
		extra, ok := kvstore.ServerConfigForID(servers, serverID)
		require.True(t, ok)

		assert.NotEqual(t, "rid-primary", extra.RemoteID, "the added server's remote differs from the primary's")

		// The property this wiring fixes: loop attribution resolves the added server's
		// remote to the added server, immediately (no restart).
		resolved, ok := plugin.serverIDForRemoteID(extra.RemoteID)
		require.True(t, ok)
		assert.Equal(t, serverID, resolved)
	})

	t.Run("DuplicateServerNameStillGetsDistinctRemotePerServer", func(t *testing.T) {
		plugin := newPlugin(t)

		// Two homeservers with different hostnames but the SAME free-form ServerName.
		// The SiteURL (and thus the remote) must key off the unique hostname, not the
		// shared ServerName, or the two servers would collapse onto one remoteID.
		const dupName = "shared.example.com"
		id1, err := plugin.AddServer("https://one.example.com", dupName, "as1", "hs1", "mx")
		require.NoError(t, err)
		id2, err := plugin.AddServer("https://two.example.com", dupName, "as2", "hs2", "mx")
		require.NoError(t, err)
		require.NotEqual(t, id1, id2, "distinct hostnames yield distinct serverIDs")

		servers, err := plugin.GetManagedServers()
		require.NoError(t, err)
		s1, ok := kvstore.ServerConfigForID(servers, id1)
		require.True(t, ok)
		s2, ok := kvstore.ServerConfigForID(servers, id2)
		require.True(t, ok)

		require.NotEmpty(t, s1.RemoteID)
		assert.NotEqual(t, s1.RemoteID, s2.RemoteID, "servers sharing a ServerName must not share a remote ID")

		// Each remote resolves back to its own server.
		r1, ok := plugin.serverIDForRemoteID(s1.RemoteID)
		require.True(t, ok)
		assert.Equal(t, id1, r1)
		r2, ok := plugin.serverIDForRemoteID(s2.RemoteID)
		require.True(t, ok)
		assert.Equal(t, id2, r2)
	})

	t.Run("Remove", func(t *testing.T) {
		plugin := newPlugin(t)
		serverID, err := plugin.AddServer(extraURL, "extra.example.com", "as", "hs", "mx")
		require.NoError(t, err)

		// Removing an injected server drops it.
		removed, err := plugin.RemoveServer(serverID)
		require.NoError(t, err)
		assert.True(t, removed)
		servers, err := plugin.GetManagedServers()
		require.NoError(t, err)
		_, present := kvstore.ServerConfigForID(servers, serverID)
		assert.False(t, present, "injected server is removed")

		// Removing an unknown server reports not-found.
		removed, err = plugin.RemoveServer("no-such-server")
		require.NoError(t, err)
		assert.False(t, removed)
	})

	t.Run("RemoveIsPermanent", func(t *testing.T) {
		plugin := newPlugin(t)
		primaryID, err := deriveServerID(primaryURL)
		require.NoError(t, err)

		removed, err := plugin.RemoveServer(primaryID)
		require.NoError(t, err)
		assert.True(t, removed, "the entry is found and removed")

		// The registry is authoritative: a removed server stays removed (no reconcile
		// re-adds it from a flat config that no longer exists).
		servers, err := plugin.GetManagedServers()
		require.NoError(t, err)
		_, present := kvstore.ServerConfigForID(servers, primaryID)
		assert.False(t, present, "the removed server does not reappear")
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
// resolved from the per-server registry entry, and that the flat global config
// is not consulted at resolution time.
func TestMatrixUsernamePrefixResolvesPerServer(t *testing.T) {
	newBridge := func(t *testing.T) *Plugin {
		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		plugin.logger = &testLogger{t: t}
		plugin.maxProfileImageSize = DefaultMaxProfileImageSize
		plugin.maxFileSize = DefaultMaxFileSize
		plugin.postTracker = NewPostTracker(DefaultPostTrackerMaxEntries)
		plugin.pendingFiles = NewPendingFileTracker()
		// The username prefix is resolved only from the registry entry (or the static
		// default); there is no global prefix config anymore.
		plugin.configuration = &configuration{}
		setTestMatrixClient(plugin, createMatrixClientWithTestLogger(t, "", "", ""))
		return plugin
	}

	t.Run("UsesRegistryEntryPrefix", func(t *testing.T) {
		plugin := newBridge(t)
		servers := []kvstore.ServerConfig{{ServerID: testServerID, UsernamePrefix: "serverprefix"}}
		data, err := json.Marshal(servers)
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.KeyServersConfig, data))

		assert.Equal(t, "serverprefix", testOutboundBridge(plugin).matrixUsernamePrefix())
	})

	t.Run("DefaultsWhenNoRegistryEntry", func(t *testing.T) {
		plugin := newBridge(t)
		// No servers_config seeded: resolves to the static default, NOT the
		// configured global prefix.
		assert.Equal(t, DefaultMatrixUsernamePrefix, testOutboundBridge(plugin).matrixUsernamePrefix())
	})

	t.Run("DefaultsWhenEntryPrefixEmpty", func(t *testing.T) {
		plugin := newBridge(t)
		servers := []kvstore.ServerConfig{{ServerID: testServerID, UsernamePrefix: ""}}
		data, err := json.Marshal(servers)
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.KeyServersConfig, data))

		assert.Equal(t, DefaultMatrixUsernamePrefix, testOutboundBridge(plugin).matrixUsernamePrefix())
	})

	t.Run("IgnoresOtherServersEntry", func(t *testing.T) {
		plugin := newBridge(t)
		// The registry only has an entry for a DIFFERENT server; this bridge's
		// server must not pick up another server's prefix.
		servers := []kvstore.ServerConfig{{ServerID: "some-other-server", UsernamePrefix: "otherprefix"}}
		data, err := json.Marshal(servers)
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.KeyServersConfig, data))

		assert.Equal(t, DefaultMatrixUsernamePrefix, testOutboundBridge(plugin).matrixUsernamePrefix())
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
	bridge := testOutboundBridge(plugin)

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
