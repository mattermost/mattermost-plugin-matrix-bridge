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
// outbound traffic for a disabled server.
func TestServerIDForSyncMsg(t *testing.T) {
	t.Run("nil rc is skipped", func(t *testing.T) {
		plugin := newTestPluginForHooks(t)
		_, ok := plugin.serverIDForSyncMsg("channel1", nil)
		assert.False(t, ok)
	})

	t.Run("rc with empty RemoteId is skipped", func(t *testing.T) {
		plugin := newTestPluginForHooks(t)
		_, ok := plugin.serverIDForSyncMsg("channel1", &model.RemoteCluster{RemoteId: ""})
		assert.False(t, ok)
	})

	t.Run("unknown remote is skipped", func(t *testing.T) {
		plugin := newTestPluginForHooks(t)
		_, ok := plugin.serverIDForSyncMsg("channel1", &model.RemoteCluster{RemoteId: "unknown-remote"})
		assert.False(t, ok)
	})

	t.Run("mapped to this server returns it", func(t *testing.T) {
		plugin := newTestPluginForHooks(t)
		serverID, remoteID := registerTestServer(t, plugin, "https://a.example.com", "a.example.com", nil)

		mappingData, err := kvstore.BuildSingleChannelMapping(serverID, "!room:a.example.com")
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.BuildChannelMappingKey("channel1"), mappingData))

		got, ok := plugin.serverIDForSyncMsg("channel1", &model.RemoteCluster{RemoteId: remoteID})
		assert.True(t, ok)
		assert.Equal(t, serverID, got)
	})

	t.Run("mapped to a different server is skipped, not relayed elsewhere", func(t *testing.T) {
		plugin := newTestPluginForHooks(t)
		_, remoteIDA := registerTestServer(t, plugin, "https://a.example.com", "a.example.com", nil)
		serverIDB, _ := registerTestServer(t, plugin, "https://b.example.com", "b.example.com", nil)

		mappingData, err := kvstore.BuildSingleChannelMapping(serverIDB, "!room:b.example.com")
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.BuildChannelMappingKey("channel1"), mappingData))

		_, ok := plugin.serverIDForSyncMsg("channel1", &model.RemoteCluster{RemoteId: remoteIDA})
		assert.False(t, ok)
	})

	t.Run("unmapped DM channel returns rc's server so the room can be auto-created", func(t *testing.T) {
		plugin := newTestPluginForHooks(t)
		serverID, remoteID := registerTestServer(t, plugin, "https://a.example.com", "a.example.com", nil)
		api := plugin.API.(*plugintest.API)
		api.On("GetChannel", "channel1").Return(&model.Channel{Type: model.ChannelTypeDirect}, nil)

		got, ok := plugin.serverIDForSyncMsg("channel1", &model.RemoteCluster{RemoteId: remoteID})
		assert.True(t, ok)
		assert.Equal(t, serverID, got)
	})

	t.Run("unmapped non-DM channel is skipped", func(t *testing.T) {
		plugin := newTestPluginForHooks(t)
		_, remoteID := registerTestServer(t, plugin, "https://a.example.com", "a.example.com", nil)
		api := plugin.API.(*plugintest.API)
		api.On("GetChannel", "channel1").Return(&model.Channel{Type: model.ChannelTypeOpen}, nil)

		_, ok := plugin.serverIDForSyncMsg("channel1", &model.RemoteCluster{RemoteId: remoteID})
		assert.False(t, ok)
	})

	t.Run("disabled server is skipped even when mapped to it", func(t *testing.T) {
		plugin := newTestPluginForHooks(t)
		serverID, remoteID := registerTestServer(t, plugin, "https://a.example.com", "a.example.com", nil)

		mappingData, err := kvstore.BuildSingleChannelMapping(serverID, "!room:a.example.com")
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.BuildChannelMappingKey("channel1"), mappingData))

		require.NoError(t, plugin.SetServerEnabled(serverID, false))

		_, ok := plugin.serverIDForSyncMsg("channel1", &model.RemoteCluster{RemoteId: remoteID})
		assert.False(t, ok, "a disabled server must be skipped - this is the only thing stopping its outbound traffic")
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

		gotA, okA := plugin.serverIDForSyncMsg("channelA", &model.RemoteCluster{RemoteId: remoteIDA})
		assert.True(t, okA)
		assert.Equal(t, serverIDA, gotA)

		_, okB := plugin.serverIDForSyncMsg("channelB", &model.RemoteCluster{RemoteId: remoteIDB})
		assert.False(t, okB, "server B is disabled; its channel must not sync")
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
	mappings := kvstore.UpsertChannelServerMapping(nil, serverIDA, "!room:a.example.com")
	mappings = kvstore.UpsertChannelServerMapping(mappings, serverIDB, "!room:b.example.com")
	mappingData, err := kvstore.MarshalChannelServerMappings(mappings)
	require.NoError(t, err)
	require.NoError(t, plugin.kvstore.Set(kvstore.BuildChannelMappingKey(channelID), mappingData))

	plugin.UserHasJoinedChannel(nil, &model.ChannelMember{ChannelId: channelID, UserId: userID}, joiner)

	assert.Positive(t, atomic.LoadInt32(&aRequests), "enabled server A must be acted on")
	assert.Zero(t, atomic.LoadInt32(&bRequests), "disabled server B must never be contacted - this is what would fail if the Enabled check were removed")
}
