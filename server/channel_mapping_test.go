package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
)

// newTestPluginForChannelMapping returns a Plugin with an in-memory KV store and the
// given servers pre-registered, ready for SetChannelMapping tests.
func newTestPluginForChannelMapping(t *testing.T, serverIDs ...string) *Plugin {
	t.Helper()

	plugin := setupPluginForTest()
	plugin.kvstore = NewMemoryKVStore()

	servers := make([]kvstore.ServerConfig, 0, len(serverIDs))
	for _, id := range serverIDs {
		servers = append(servers, kvstore.ServerConfig{ServerID: id, ServerName: id + ".example.com", Enabled: true})
	}
	data, err := kvstore.MarshalServersConfig(servers)
	require.NoError(t, err)
	require.NoError(t, plugin.kvstore.Set(kvstore.KeyServersConfig, data))

	return plugin
}

func TestSetChannelMapping(t *testing.T) {
	t.Run("unmapped channel appends the entry", func(t *testing.T) {
		plugin := newTestPluginForChannelMapping(t, "serverA")

		mappings, err := plugin.SetChannelMapping("channel1", kvstore.ChannelServerMapping{ServerID: "serverA", RoomID: "!room:serverA.example.com"})
		require.NoError(t, err)
		require.Len(t, mappings, 1)
		assert.Equal(t, "!room:serverA.example.com", kvstore.RoomIDForServer(mappings, "serverA"))
	})

	t.Run("already mapped to this server overwrites the room", func(t *testing.T) {
		plugin := newTestPluginForChannelMapping(t, "serverA")

		_, err := plugin.SetChannelMapping("channel1", kvstore.ChannelServerMapping{ServerID: "serverA", RoomID: "!room1:serverA.example.com"})
		require.NoError(t, err)

		mappings, err := plugin.SetChannelMapping("channel1", kvstore.ChannelServerMapping{ServerID: "serverA", RoomID: "!room2:serverA.example.com"})
		require.NoError(t, err)
		require.Len(t, mappings, 1)
		assert.Equal(t, "!room2:serverA.example.com", kvstore.RoomIDForServer(mappings, "serverA"))
	})

	t.Run("mapping a second live server is rejected", func(t *testing.T) {
		plugin := newTestPluginForChannelMapping(t, "serverA", "serverB")

		_, err := plugin.SetChannelMapping("channel1", kvstore.ChannelServerMapping{ServerID: "serverA", RoomID: "!room:serverA.example.com"})
		require.NoError(t, err)

		_, err = plugin.SetChannelMapping("channel1", kvstore.ChannelServerMapping{ServerID: "serverB", RoomID: "!room:serverB.example.com"})
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrChannelAlreadyMapped))

		// serverA's mapping must be untouched by the rejected attempt.
		data, getErr := plugin.kvstore.Get(kvstore.BuildChannelMappingKey("channel1"))
		require.NoError(t, getErr)
		mappings, parseErr := kvstore.ParseChannelServerMappings(data)
		require.NoError(t, parseErr)
		require.Len(t, mappings, 1)
		assert.Equal(t, "serverA", mappings[0].ServerID)
	})

	t.Run("stale entry for a deregistered server does not count toward the limit and is dropped", func(t *testing.T) {
		plugin := newTestPluginForChannelMapping(t, "serverB")

		// Seed a stale entry as if serverA were mapped, but serverA is not registered.
		staleData, err := buildSingleChannelMapping("serverA", "!room:serverA.example.com")
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.BuildChannelMappingKey("channel1"), staleData))

		mappings, err := plugin.SetChannelMapping("channel1", kvstore.ChannelServerMapping{ServerID: "serverB", RoomID: "!room:serverB.example.com"})
		require.NoError(t, err, "a stale entry must not block mapping a live server")
		require.Len(t, mappings, 1)
		assert.Equal(t, "serverB", mappings[0].ServerID, "the stale serverA entry must have been dropped")
	})

	t.Run("re-adopting a server ID that has a stale mapping restores the link", func(t *testing.T) {
		plugin := newTestPluginForChannelMapping(t) // no servers registered yet

		staleData, err := buildSingleChannelMapping("serverA", "!room:serverA.example.com")
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.BuildChannelMappingKey("channel1"), staleData))

		// serverA becomes live again (e.g. re-adopted).
		data, err := kvstore.MarshalServersConfig([]kvstore.ServerConfig{{ServerID: "serverA", ServerName: "serverA.example.com", Enabled: true}})
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.KeyServersConfig, data))

		roomData, err := plugin.kvstore.Get(kvstore.BuildChannelMappingKey("channel1"))
		require.NoError(t, err)
		mappings, err := kvstore.ParseChannelServerMappings(roomData)
		require.NoError(t, err)
		assert.Equal(t, "!room:serverA.example.com", kvstore.RoomIDForServer(mappings, "serverA"),
			"the stale entry must resolve again once its server is live, with no re-mapping needed")
	})

	t.Run("stored value is a JSON array even for a single server", func(t *testing.T) {
		plugin := newTestPluginForChannelMapping(t, "serverA")

		_, err := plugin.SetChannelMapping("channel1", kvstore.ChannelServerMapping{ServerID: "serverA", RoomID: "!room:serverA.example.com"})
		require.NoError(t, err)

		raw, err := plugin.kvstore.Get(kvstore.BuildChannelMappingKey("channel1"))
		require.NoError(t, err)
		assert.Equal(t, `[{"server_id":"serverA","room_id":"!room:serverA.example.com"}]`, string(raw))
	})
}

// newMatrixStubForMapping serves the two endpoints the mapping paths touch: alias
// resolution (used by setChannelRoomMapping) and the channel-ID state write
// (used by UnmapChannelFromServer to clear Matrix room state).
func newMatrixStubForMapping(t *testing.T, aliasToRoomID map[string]string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/directory/room/") {
			alias, err := url.PathUnescape(path.Base(r.URL.Path))
			require.NoError(t, err)
			roomID, ok := aliasToRoomID[alias]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"errcode":"M_NOT_FOUND"}`))
				return
			}
			_, _ = w.Write([]byte(`{"room_id":"` + roomID + `"}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)
	return server
}

// TestChannelMappingAliasReverseKey covers the alias half of the room_mapping_ reverse
// keys. setChannelRoomMapping writes one key per identifier it was given - the resolved
// room ID, plus the alias when it started from one - and a resolved room ID cannot be
// reversed back into the alias it came from. The mapping entry is therefore the only
// record of which alias key exists, and without it unmapping or re-mapping strands that
// key in the KV store forever.
func TestChannelMappingAliasReverseKey(t *testing.T) {
	const serverDomain = "a.example.com"
	const alias = "#room:" + serverDomain
	const roomID = "!resolved:" + serverDomain

	setup := func(t *testing.T) (*Plugin, *MattermostToMatrixBridge, string) {
		t.Helper()
		stub := newMatrixStubForMapping(t, map[string]string{alias: roomID})

		plugin := setupPluginForTest()
		plugin.kvstore = NewMemoryKVStore()
		plugin.maxProfileImageSize = DefaultMaxProfileImageSize
		plugin.maxFileSize = DefaultMaxFileSize
		plugin.postTracker = NewPostTracker(DefaultPostTrackerMaxEntries)
		plugin.pendingFiles = NewPendingFileTracker()

		api := plugin.API.(*plugintest.API)
		api.On("UninviteRemoteFromChannel", mock.Anything, mock.Anything).Return(nil).Maybe()
		mockAnyLogCalls(api)

		client := createMatrixClientWithTestLogger(t, stub.URL, "as-token", "")
		serverID, _ := registerTestServer(t, plugin, stub.URL, serverDomain, client)
		m2mx, _ := plugin.testBridges(t, serverID)

		return plugin, m2mx, serverID
	}

	t.Run("mapping by alias records the alias and writes both reverse keys", func(t *testing.T) {
		plugin, m2mx, serverID := setup(t)
		channelID := model.NewId()

		require.NoError(t, m2mx.setChannelRoomMapping(channelID, alias))

		mappings, err := plugin.getChannelServerMappings(channelID)
		require.NoError(t, err)
		entry, ok := kvstore.ChannelMappingForServer(mappings, serverID)
		require.True(t, ok)
		assert.Equal(t, roomID, entry.RoomID, "the forward mapping must store the resolved room ID")
		assert.Equal(t, alias, entry.Alias, "the alias the mapping was created from must be recorded")

		for _, identifier := range []string{roomID, alias} {
			value, err := plugin.kvstore.Get(kvstore.BuildRoomMappingKey(serverID, identifier))
			require.NoError(t, err)
			assert.Equal(t, channelID, string(value), "reverse key for %q must resolve to the channel", identifier)
		}
	})

	t.Run("mapping by room ID records no alias", func(t *testing.T) {
		plugin, m2mx, serverID := setup(t)
		channelID := model.NewId()

		require.NoError(t, m2mx.setChannelRoomMapping(channelID, roomID))

		mappings, err := plugin.getChannelServerMappings(channelID)
		require.NoError(t, err)
		entry, ok := kvstore.ChannelMappingForServer(mappings, serverID)
		require.True(t, ok)
		assert.Empty(t, entry.Alias)
	})

	t.Run("unmapping deletes the alias reverse key as well as the room ID one", func(t *testing.T) {
		plugin, m2mx, serverID := setup(t)
		channelID := model.NewId()

		require.NoError(t, m2mx.setChannelRoomMapping(channelID, alias))
		require.NoError(t, plugin.UnmapChannelFromServer(serverID, channelID))

		for _, identifier := range []string{roomID, alias} {
			value, err := plugin.kvstore.Get(kvstore.BuildRoomMappingKey(serverID, identifier))
			require.NoError(t, err)
			assert.Empty(t, value, "reverse key for %q must not survive the unmap", identifier)
		}
	})

	t.Run("re-mapping the same server to another room deletes the stale alias key", func(t *testing.T) {
		plugin, m2mx, serverID := setup(t)
		channelID := model.NewId()
		const newRoomID = "!other:" + serverDomain

		require.NoError(t, m2mx.setChannelRoomMapping(channelID, alias))
		require.NoError(t, m2mx.setChannelRoomMapping(channelID, newRoomID))

		for _, identifier := range []string{roomID, alias} {
			value, err := plugin.kvstore.Get(kvstore.BuildRoomMappingKey(serverID, identifier))
			require.NoError(t, err)
			assert.Empty(t, value, "stale reverse key for %q must be cleaned up on re-map", identifier)
		}

		value, err := plugin.kvstore.Get(kvstore.BuildRoomMappingKey(serverID, newRoomID))
		require.NoError(t, err)
		assert.Equal(t, channelID, string(value))
	})
}
