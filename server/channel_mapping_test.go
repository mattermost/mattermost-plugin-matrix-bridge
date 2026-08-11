package main

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/servers"
	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
)

// newTestPluginForChannelMapping returns a Plugin with an in-memory KV store and the
// given servers pre-registered, ready for SetChannelMapping tests.
func newTestPluginForChannelMapping(t *testing.T, serverIDs ...string) *Plugin {
	t.Helper()

	plugin := setupPluginForTest()
	plugin.kvstore = NewMemoryKVStore()
	plugin.servers = servers.New(plugin.kvstore, pluginLogger{plugin}, pluginHost{plugin})

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

		mappings, err := plugin.SetChannelMapping("channel1", "serverA", "!room:serverA.example.com")
		require.NoError(t, err)
		require.Len(t, mappings, 1)
		assert.Equal(t, "!room:serverA.example.com", kvstore.RoomIDForServer(mappings, "serverA"))
	})

	t.Run("already mapped to this server overwrites the room", func(t *testing.T) {
		plugin := newTestPluginForChannelMapping(t, "serverA")

		_, err := plugin.SetChannelMapping("channel1", "serverA", "!room1:serverA.example.com")
		require.NoError(t, err)

		mappings, err := plugin.SetChannelMapping("channel1", "serverA", "!room2:serverA.example.com")
		require.NoError(t, err)
		require.Len(t, mappings, 1)
		assert.Equal(t, "!room2:serverA.example.com", kvstore.RoomIDForServer(mappings, "serverA"))
	})

	t.Run("mapping a second live server is rejected", func(t *testing.T) {
		plugin := newTestPluginForChannelMapping(t, "serverA", "serverB")

		_, err := plugin.SetChannelMapping("channel1", "serverA", "!room:serverA.example.com")
		require.NoError(t, err)

		_, err = plugin.SetChannelMapping("channel1", "serverB", "!room:serverB.example.com")
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
		staleData, err := kvstore.BuildSingleChannelMapping("serverA", "!room:serverA.example.com")
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.BuildChannelMappingKey("channel1"), staleData))

		mappings, err := plugin.SetChannelMapping("channel1", "serverB", "!room:serverB.example.com")
		require.NoError(t, err, "a stale entry must not block mapping a live server")
		require.Len(t, mappings, 1)
		assert.Equal(t, "serverB", mappings[0].ServerID, "the stale serverA entry must have been dropped")
	})

	t.Run("re-adopting a server ID that has a stale mapping restores the link", func(t *testing.T) {
		plugin := newTestPluginForChannelMapping(t) // no servers registered yet

		staleData, err := kvstore.BuildSingleChannelMapping("serverA", "!room:serverA.example.com")
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

		_, err := plugin.SetChannelMapping("channel1", "serverA", "!room:serverA.example.com")
		require.NoError(t, err)

		raw, err := plugin.kvstore.Get(kvstore.BuildChannelMappingKey("channel1"))
		require.NoError(t, err)
		assert.Equal(t, `[{"server_id":"serverA","room_id":"!room:serverA.example.com"}]`, string(raw))
	})
}
