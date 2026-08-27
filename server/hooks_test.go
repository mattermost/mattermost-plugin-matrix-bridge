package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
)

// newTestPluginForHooks returns a Plugin with an in-memory KV store, ready for
// hooks.go/serverIDForSyncMsg tests. Mocks PublishPluginClusterEvent and every log
// method so SetServerEnabled (used to flip enablement mid-test) doesn't panic on an
// unmocked call.
func newTestPluginForHooks(t *testing.T) *Plugin {
	t.Helper()
	plugin := setupPluginForTest()
	plugin.kvstore = NewMemoryKVStore()
	api := plugin.API.(*plugintest.API)
	api.On("PublishPluginClusterEvent", mock.Anything, mock.Anything).Return(nil).Maybe()
	mockAnyLogCalls(api)
	return plugin
}

// TestServerIDForSyncMsg covers §5.1's outbound-routing requirements: every resolution
// step of §3.6, including the disabled-server skip that is now the only thing stopping
// outbound traffic for a disabled server, and the operational-failure/no-op distinction
// that both outbound hooks rely on to avoid silently losing a sync batch.
func TestServerIDForSyncMsg(t *testing.T) {
	t.Run("nil rc is a no-op, not an error", func(t *testing.T) {
		plugin := newTestPluginForHooks(t)
		_, shouldSync, err := plugin.serverIDForSyncMsg("channel1", nil)
		require.NoError(t, err)
		assert.False(t, shouldSync)
	})

	t.Run("rc with empty RemoteId is a no-op, not an error", func(t *testing.T) {
		plugin := newTestPluginForHooks(t)
		_, shouldSync, err := plugin.serverIDForSyncMsg("channel1", &model.RemoteCluster{RemoteId: ""})
		require.NoError(t, err)
		assert.False(t, shouldSync)
	})

	t.Run("unknown remote is a no-op, not an error", func(t *testing.T) {
		plugin := newTestPluginForHooks(t)
		_, shouldSync, err := plugin.serverIDForSyncMsg("channel1", &model.RemoteCluster{RemoteId: "unknown-remote"})
		require.NoError(t, err)
		assert.False(t, shouldSync)
	})

	t.Run("mapped to this server returns it", func(t *testing.T) {
		plugin := newTestPluginForHooks(t)
		serverID, remoteID := registerTestServer(t, plugin, "https://a.example.com", "a.example.com", nil)

		mappingData, err := kvstore.BuildSingleChannelMapping(serverID, "!room:a.example.com")
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.BuildChannelMappingKey("channel1"), mappingData))

		got, shouldSync, err := plugin.serverIDForSyncMsg("channel1", &model.RemoteCluster{RemoteId: remoteID})
		require.NoError(t, err)
		assert.True(t, shouldSync)
		assert.Equal(t, serverID, got)
	})

	t.Run("mapped to a different server is skipped, not relayed elsewhere", func(t *testing.T) {
		plugin := newTestPluginForHooks(t)
		_, remoteIDA := registerTestServer(t, plugin, "https://a.example.com", "a.example.com", nil)
		serverIDB, _ := registerTestServer(t, plugin, "https://b.example.com", "b.example.com", nil)

		mappingData, err := kvstore.BuildSingleChannelMapping(serverIDB, "!room:b.example.com")
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.BuildChannelMappingKey("channel1"), mappingData))

		_, shouldSync, err := plugin.serverIDForSyncMsg("channel1", &model.RemoteCluster{RemoteId: remoteIDA})
		require.NoError(t, err)
		assert.False(t, shouldSync)
	})

	t.Run("unmapped DM channel returns rc's server so the room can be auto-created", func(t *testing.T) {
		plugin := newTestPluginForHooks(t)
		serverID, remoteID := registerTestServer(t, plugin, "https://a.example.com", "a.example.com", nil)
		api := plugin.API.(*plugintest.API)
		api.On("GetChannel", "channel1").Return(&model.Channel{Type: model.ChannelTypeDirect}, nil)

		got, shouldSync, err := plugin.serverIDForSyncMsg("channel1", &model.RemoteCluster{RemoteId: remoteID})
		require.NoError(t, err)
		assert.True(t, shouldSync)
		assert.Equal(t, serverID, got)
	})

	t.Run("unmapped non-DM channel is skipped", func(t *testing.T) {
		plugin := newTestPluginForHooks(t)
		_, remoteID := registerTestServer(t, plugin, "https://a.example.com", "a.example.com", nil)
		api := plugin.API.(*plugintest.API)
		api.On("GetChannel", "channel1").Return(&model.Channel{Type: model.ChannelTypeOpen}, nil)

		_, shouldSync, err := plugin.serverIDForSyncMsg("channel1", &model.RemoteCluster{RemoteId: remoteID})
		require.NoError(t, err)
		assert.False(t, shouldSync)
	})

	// Removing a server leaves its channel_mapping_ entry behind, so a mapping is only
	// evidence of ownership while its server is still registered. Counting a dead entry
	// stranded the channel: RoomIDForServer found nothing for the new server, the stale
	// entry made it look "mapped elsewhere", and the DM never got auto-created.
	t.Run("a mapping to a removed server does not block auto-creation on the new server", func(t *testing.T) {
		plugin := newTestPluginForHooks(t)
		api := plugin.API.(*plugintest.API)
		api.On("UnregisterPluginRemoteForSharedChannels", mock.Anything).Return(nil).Maybe()
		api.On("GetChannel", "channel1").Return(&model.Channel{Type: model.ChannelTypeDirect}, nil)

		serverIDA, _ := registerTestServer(t, plugin, "https://a.example.com", "a.example.com", nil)
		mappingData, err := kvstore.BuildSingleChannelMapping(serverIDA, "!room:a.example.com")
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.BuildChannelMappingKey("channel1"), mappingData))

		removed, err := plugin.RemoveServer(serverIDA)
		require.NoError(t, err)
		require.True(t, removed)

		serverIDB, remoteIDB := registerTestServer(t, plugin, "https://b.example.com", "b.example.com", nil)

		// The dead entry for A is still on the channel.
		mappings, err := plugin.getChannelServerMappings("channel1")
		require.NoError(t, err)
		require.Len(t, mappings, 1)
		require.Equal(t, serverIDA, mappings[0].ServerID)

		got, shouldSync, err := plugin.serverIDForSyncMsg("channel1", &model.RemoteCluster{RemoteId: remoteIDB})
		require.NoError(t, err)
		assert.True(t, shouldSync, "a channel holding only a dead mapping must read as unmapped, not as owned by another server")
		assert.Equal(t, serverIDB, got)
	})

	t.Run("a mapping to an unregistered server falls through to the unmapped path", func(t *testing.T) {
		plugin := newTestPluginForHooks(t)
		api := plugin.API.(*plugintest.API)
		api.On("GetChannel", "channel1").Return(&model.Channel{Type: model.ChannelTypeOpen}, nil)

		_, remoteID := registerTestServer(t, plugin, "https://a.example.com", "a.example.com", nil)

		mappingData, err := kvstore.BuildSingleChannelMapping(model.NewId(), "!room:gone.example.com")
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.BuildChannelMappingKey("channel1"), mappingData))

		_, shouldSync, err := plugin.serverIDForSyncMsg("channel1", &model.RemoteCluster{RemoteId: remoteID})
		require.NoError(t, err)
		assert.False(t, shouldSync)
		// Reaching the channel-type lookup is what proves the stale entry was ignored -
		// the "mapped elsewhere" branch returns before it.
		api.AssertCalled(t, "GetChannel", "channel1")
	})

	// Only registration is checked, not Enabled: a disabled server still owns its
	// channels, and remapping them is an explicit operator action.
	t.Run("a mapping to a registered but disabled server still counts as owned", func(t *testing.T) {
		plugin := newTestPluginForHooks(t)
		api := plugin.API.(*plugintest.API)

		_, remoteIDA := registerTestServer(t, plugin, "https://a.example.com", "a.example.com", nil)
		serverIDB, _ := registerTestServer(t, plugin, "https://b.example.com", "b.example.com", nil)

		mappingData, err := kvstore.BuildSingleChannelMapping(serverIDB, "!room:b.example.com")
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.BuildChannelMappingKey("channel1"), mappingData))

		require.NoError(t, plugin.SetServerEnabled(serverIDB, false))

		_, shouldSync, err := plugin.serverIDForSyncMsg("channel1", &model.RemoteCluster{RemoteId: remoteIDA})
		require.NoError(t, err)
		assert.False(t, shouldSync, "a disabled server's mapping must not be treated as stale")
		api.AssertNotCalled(t, "GetChannel", "channel1")
	})

	t.Run("disabled server is skipped even when mapped to it", func(t *testing.T) {
		plugin := newTestPluginForHooks(t)
		serverID, remoteID := registerTestServer(t, plugin, "https://a.example.com", "a.example.com", nil)

		mappingData, err := kvstore.BuildSingleChannelMapping(serverID, "!room:a.example.com")
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.BuildChannelMappingKey("channel1"), mappingData))

		require.NoError(t, plugin.SetServerEnabled(serverID, false))

		_, shouldSync, err := plugin.serverIDForSyncMsg("channel1", &model.RemoteCluster{RemoteId: remoteID})
		require.NoError(t, err)
		assert.False(t, shouldSync, "a disabled server must be skipped - this is the only thing stopping its outbound traffic")
	})

	t.Run("per-server enablement: A enabled and mapped syncs, B disabled and mapped does not", func(t *testing.T) {
		plugin := newTestPluginForHooks(t)
		serverIDA, remoteIDA := registerTestServer(t, plugin, "https://a.example.com", "a.example.com", nil)
		serverIDB, remoteIDB := registerTestServer(t, plugin, "https://b.example.com", "b.example.com", nil)
		require.NoError(t, plugin.SetServerEnabled(serverIDB, false))

		mappingA, err := kvstore.BuildSingleChannelMapping(serverIDA, "!room:a.example.com")
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.BuildChannelMappingKey("channelA"), mappingA))

		mappingB, err := kvstore.BuildSingleChannelMapping(serverIDB, "!room:b.example.com")
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.BuildChannelMappingKey("channelB"), mappingB))

		gotA, shouldSyncA, err := plugin.serverIDForSyncMsg("channelA", &model.RemoteCluster{RemoteId: remoteIDA})
		require.NoError(t, err)
		assert.True(t, shouldSyncA)
		assert.Equal(t, serverIDA, gotA)

		_, shouldSyncB, err := plugin.serverIDForSyncMsg("channelB", &model.RemoteCluster{RemoteId: remoteIDB})
		require.NoError(t, err)
		assert.False(t, shouldSyncB, "server B is disabled; its channel must not sync")
	})

	t.Run("channel mapping KV read failure is an operational error, not a silent no-op", func(t *testing.T) {
		plugin := newTestPluginForHooks(t)
		_, remoteID := registerTestServer(t, plugin, "https://a.example.com", "a.example.com", nil)
		plugin.kvstore = &erroringKVStore{KVStore: plugin.kvstore, errOnGetKey: kvstore.BuildChannelMappingKey("channel1")}

		_, shouldSync, err := plugin.serverIDForSyncMsg("channel1", &model.RemoteCluster{RemoteId: remoteID})
		require.Error(t, err, "a KV read failure must surface as an error so Mattermost retries the batch instead of dropping it")
		assert.False(t, shouldSync)
	})

	t.Run("corrupt channel mapping JSON is an operational error, not a silent no-op", func(t *testing.T) {
		plugin := newTestPluginForHooks(t)
		_, remoteID := registerTestServer(t, plugin, "https://a.example.com", "a.example.com", nil)
		require.NoError(t, plugin.kvstore.Set(kvstore.BuildChannelMappingKey("channel1"), []byte("not-json")))

		_, shouldSync, err := plugin.serverIDForSyncMsg("channel1", &model.RemoteCluster{RemoteId: remoteID})
		require.Error(t, err)
		assert.False(t, shouldSync)
	})

	t.Run("GetChannel failure for an unmapped channel is an operational error, not a silent no-op", func(t *testing.T) {
		plugin := newTestPluginForHooks(t)
		_, remoteID := registerTestServer(t, plugin, "https://a.example.com", "a.example.com", nil)
		api := plugin.API.(*plugintest.API)
		api.On("GetChannel", "channel1").Return(nil, model.NewAppError("GetChannel", "id", nil, "boom", http.StatusInternalServerError))

		_, shouldSync, err := plugin.serverIDForSyncMsg("channel1", &model.RemoteCluster{RemoteId: remoteID})
		require.Error(t, err)
		assert.False(t, shouldSync)
	})

	t.Run("remote-ID cache miss resolves after a refresh catches up a lagging node", func(t *testing.T) {
		plugin := newTestPluginForHooks(t)
		serverID, remoteID := registerTestServer(t, plugin, "https://a.example.com", "a.example.com", nil)

		mappingData, err := kvstore.BuildSingleChannelMapping(serverID, "!room:a.example.com")
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.BuildChannelMappingKey("channel1"), mappingData))

		// Simulate this node's remoteToServerID cache lagging a registry mutation made on
		// another node: the registry (KV) already has the server, but this node's cache
		// doesn't know about it yet.
		delete(plugin.remoteToServerID, remoteID)

		got, shouldSync, err := plugin.serverIDForSyncMsg("channel1", &model.RemoteCluster{RemoteId: remoteID})
		require.NoError(t, err)
		assert.True(t, shouldSync, "a stale remote cache must be refreshed from the registry before giving up")
		assert.Equal(t, serverID, got)
	})

	// initMatrixClients is a full registry read plus a client per registered server, run
	// under a mutex that serializes every other rebuild. A remote that will never resolve
	// - a server whose unregister failed, or a remote that was never ours - must never
	// pay for it, since the registry read alone already proves the remote is not ours.
	t.Run("an unresolvable remote is settled by a registry read, never a client rebuild", func(t *testing.T) {
		plugin := newTestPluginForHooks(t)
		serverID, _ := registerTestServer(t, plugin, "https://a.example.com", "a.example.com", nil)

		counter := &readCountingKVStore{KVStore: plugin.kvstore, countKey: kvstore.KeyServersConfig}
		plugin.kvstore = counter

		for range 5 {
			_, shouldSync, err := plugin.serverIDForSyncMsg("channel1", &model.RemoteCluster{RemoteId: "never-ours"})
			require.NoError(t, err)
			assert.False(t, shouldSync)
		}

		assert.Equal(t, 5, counter.readCount(), "each message may read the registry once and no more")
		// registerTestServer seeds a nil client; only a rebuild would construct a real one.
		assert.Nil(t, plugin.matrixClients[serverID], "no message may trigger a client rebuild")
	})

	// A remote found absent from the registry is re-checked against the registry on its
	// very next message, so a server registered moments later is picked up immediately.
	// Nothing caches the "absent" answer, so no window exists in which its traffic is
	// silently dropped.
	t.Run("a remote registered after a skip resolves on its next message", func(t *testing.T) {
		plugin := newTestPluginForHooks(t)
		serverID, remoteID := registerTestServer(t, plugin, "https://a.example.com", "a.example.com", nil)

		mappingData, err := kvstore.BuildSingleChannelMapping(serverID, "!room:a.example.com")
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.BuildChannelMappingKey("channel1"), mappingData))

		// Hide the server from the registry so the first message finds nothing to resolve.
		servers, err := plugin.getServers()
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.KeyServersConfig, []byte("[]")))
		require.NoError(t, plugin.initMatrixClients())

		_, shouldSync, err := plugin.serverIDForSyncMsg("channel1", &model.RemoteCluster{RemoteId: remoteID})
		require.NoError(t, err)
		require.False(t, shouldSync)

		// Put it back in the registry only - no cluster event, no rebuild on this node.
		restored, err := kvstore.MarshalServersConfig(servers)
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.KeyServersConfig, restored))

		got, shouldSync, err := plugin.serverIDForSyncMsg("channel1", &model.RemoteCluster{RemoteId: remoteID})
		require.NoError(t, err)
		assert.True(t, shouldSync, "the registry read must pick the server up without a cluster event")
		assert.Equal(t, serverID, got)
	})

	t.Run("a refresh failure on remote-ID cache miss is an operational error, not a silent no-op", func(t *testing.T) {
		plugin := newTestPluginForHooks(t)
		_, remoteID := registerTestServer(t, plugin, "https://a.example.com", "a.example.com", nil)
		delete(plugin.remoteToServerID, remoteID)
		plugin.kvstore = &erroringKVStore{KVStore: plugin.kvstore, errOnGetKey: kvstore.KeyServersConfig}

		_, shouldSync, err := plugin.serverIDForSyncMsg("channel1", &model.RemoteCluster{RemoteId: remoteID})
		require.Error(t, err, "a failed cache refresh must surface as an error, not be conflated with 'not one of our remotes'")
		assert.False(t, shouldSync)
	})
}

// TestOnSharedChannelsPing covers §5.1's ping requirements: healthy-but-idle for every
// case that isn't "enabled server, reachable," and no probe attempt for a disabled or
// unconfigured server.
func TestOnSharedChannelsPing(t *testing.T) {
	t.Run("nil rc is healthy-but-idle", func(t *testing.T) {
		plugin := newTestPluginForHooks(t)
		assert.True(t, plugin.OnSharedChannelsPing(nil))
	})

	t.Run("rc with empty RemoteId is healthy-but-idle", func(t *testing.T) {
		plugin := newTestPluginForHooks(t)
		assert.True(t, plugin.OnSharedChannelsPing(&model.RemoteCluster{RemoteId: ""}))
	})

	t.Run("unknown remote is healthy-but-idle", func(t *testing.T) {
		plugin := newTestPluginForHooks(t)
		assert.True(t, plugin.OnSharedChannelsPing(&model.RemoteCluster{RemoteId: "unknown"}))
	})

	t.Run("disabled server is healthy-but-idle without a client", func(t *testing.T) {
		plugin := newTestPluginForHooks(t)
		// No matrix client at all for this server (nil) - if the disabled check didn't
		// short-circuit before the client lookup, this would panic/error instead of
		// returning true.
		serverID, remoteID := registerTestServer(t, plugin, "https://a.example.com", "a.example.com", nil)
		require.NoError(t, plugin.SetServerEnabled(serverID, false))

		assert.True(t, plugin.OnSharedChannelsPing(&model.RemoteCluster{RemoteId: remoteID}))
	})

	t.Run("enabled server with no client returns false", func(t *testing.T) {
		plugin := newTestPluginForHooks(t)
		_, remoteID := registerTestServer(t, plugin, "https://a.example.com", "a.example.com", nil)

		assert.False(t, plugin.OnSharedChannelsPing(&model.RemoteCluster{RemoteId: remoteID}))
	})

	t.Run("enabled server whose connection test fails returns false", func(t *testing.T) {
		plugin := newTestPluginForHooks(t)
		client := createMatrixClientWithTestLogger(t, unreachableURL, "as", "")
		_, remoteID := registerTestServer(t, plugin, unreachableURL, "a.example.com", client)

		assert.False(t, plugin.OnSharedChannelsPing(&model.RemoteCluster{RemoteId: remoteID}))
	})

	t.Run("enabled server with a healthy connection returns true", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		}))
		defer server.Close()

		plugin := newTestPluginForHooks(t)
		client := createMatrixClientWithTestLogger(t, server.URL, "as", "")
		_, remoteID := registerTestServer(t, plugin, server.URL, "a.example.com", client)

		assert.True(t, plugin.OnSharedChannelsPing(&model.RemoteCluster{RemoteId: remoteID}))
	})
}

// TestUserHasJoinedChannelPerServerEnablement covers §5.1's last per-server-enablement
// hook: UserHasJoinedChannel has no *model.RemoteCluster to resolve a single server from
// (unlike the sync hooks), so it loops over every server the channel is mapped to and
// must skip a disabled one while still acting on an enabled one - exercised here with a
// single channel mapped to both, only reachable by writing the KV record directly since
// the maxServersPerChannel=1 policy forbids it via the normal write path (the mapping
// helpers are already list-shaped for exactly this reason, per §3.3/§6).
//
// Both servers' ghost users are pre-seeded in KV so CreateOrGetGhostUser is a cache hit
// for either one - the Enabled check this test targets happens before any Matrix client
// is even consulted, so the fake servers only need to answer the room-join calls that
// follow, and only server A's should ever receive one.
func TestUserHasJoinedChannelPerServerEnablement(t *testing.T) {
	var aRequests, bRequests int32
	joinRuleHandler := func(counter *int32) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt32(counter, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"join_rule":"public"}`))
		}
	}
	serverAStub := httptest.NewServer(joinRuleHandler(&aRequests))
	defer serverAStub.Close()
	serverBStub := httptest.NewServer(joinRuleHandler(&bRequests))
	defer serverBStub.Close()

	plugin := newTestPluginForHooks(t)
	api := plugin.API.(*plugintest.API)

	clientA := createMatrixClientWithTestLogger(t, serverAStub.URL, "as-a", "")
	clientB := createMatrixClientWithTestLogger(t, serverBStub.URL, "as-b", "")
	serverIDA, _ := registerTestServer(t, plugin, serverAStub.URL, "a.example.com", clientA)
	serverIDB, _ := registerTestServer(t, plugin, serverBStub.URL, "b.example.com", clientB)

	// Disable B via setTestServerEnabled, NOT via SetServerEnabled - that would call
	// refreshServersAndBroadcast -> initMatrixClients, which rebuilds matrixClients from
	// the registry's own ASToken/ServerURL fields (empty here) and would silently replace
	// clientA/clientB above with non-functional clients pointed at nothing.
	// setTestServerEnabled updates both KV and the serverConfigs cache without touching
	// matrixClients, matching what UserHasJoinedChannel's routing check now reads.
	setTestServerEnabled(t, plugin, serverIDB, false)

	userID := model.NewId()
	joiner := &model.User{Id: userID, Username: "joiner"}
	api.On("GetUser", userID).Return(joiner, nil).Maybe()

	require.NoError(t, plugin.kvstore.Set(kvstore.BuildGhostUserKey(serverIDA, userID), []byte("@_mattermost_"+userID+":a.example.com")))
	require.NoError(t, plugin.kvstore.Set(kvstore.BuildGhostUserKey(serverIDB, userID), []byte("@_mattermost_"+userID+":b.example.com")))

	channelID := model.NewId()
	mappings := kvstore.UpsertChannelServerMapping(nil, kvstore.ChannelServerMapping{ServerID: serverIDA, RoomID: "!room:a.example.com"})
	mappings = kvstore.UpsertChannelServerMapping(mappings, kvstore.ChannelServerMapping{ServerID: serverIDB, RoomID: "!room:b.example.com"})
	mappingData, err := kvstore.MarshalChannelServerMappings(mappings)
	require.NoError(t, err)
	require.NoError(t, plugin.kvstore.Set(kvstore.BuildChannelMappingKey(channelID), mappingData))

	plugin.UserHasJoinedChannel(nil, &model.ChannelMember{ChannelId: channelID, UserId: userID}, joiner)

	assert.Positive(t, atomic.LoadInt32(&aRequests), "enabled server A must be acted on")
	assert.Zero(t, atomic.LoadInt32(&bRequests), "disabled server B must never be contacted - this is what would fail if the Enabled check were removed")
}
