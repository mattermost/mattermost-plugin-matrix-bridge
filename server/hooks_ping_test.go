package main

import (
	"encoding/json"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
)

// newPingTestPlugin builds a plugin with the given server registry (and matching
// remote maps) and sync enabled. No Matrix clients are wired, so getMatrixClient
// returns nil for every server — a server that is actually health-checked fails
// with "client not initialized", which lets these tests assert *which* server the
// ping targets without any network access.
func newPingTestPlugin(t *testing.T, servers []kvstore.ServerConfig) *Plugin {
	t.Helper()
	p := setupPluginForTest()
	p.kvstore = NewMemoryKVStore()
	p.configuration = &configuration{EnableSync: true}

	data, err := json.Marshal(servers)
	require.NoError(t, err)
	require.NoError(t, p.kvstore.Set(kvstore.KeyServersConfig, data))

	remoteToServerID := map[string]string{}
	for _, s := range servers {
		if s.RemoteID != "" {
			remoteToServerID[s.RemoteID] = s.ServerID
		}
	}
	p.matrixClientsLock.Lock()
	p.remoteToServerID = remoteToServerID
	p.matrixClientsLock.Unlock()
	return p
}

func rc(remoteID string) *model.RemoteCluster {
	return &model.RemoteCluster{RemoteId: remoteID}
}

func TestOnSharedChannelsPing(t *testing.T) {
	serverA := kvstore.ServerConfig{ServerID: "srvA", ServerURL: "https://a.example.org", ServerName: "a.example.org", Enabled: true, RemoteID: "remote-a"}
	serverBDisabled := kvstore.ServerConfig{ServerID: "srvB", ServerURL: "https://b.example.org", ServerName: "b.example.org", Enabled: false, RemoteID: "remote-b", SiteURL: "https://b.example.org"}

	t.Run("SyncDisabledIsHealthy", func(t *testing.T) {
		p := newPingTestPlugin(t, []kvstore.ServerConfig{serverA})
		p.configuration = &configuration{EnableSync: false}
		assert.True(t, p.OnSharedChannelsPing(rc("remote-a")))
	})

	t.Run("NoServersConfiguredIsHealthy", func(t *testing.T) {
		p := newPingTestPlugin(t, nil)
		assert.True(t, p.OnSharedChannelsPing(rc("remote-a")))
	})

	t.Run("PingedEnabledServerWithNoClientIsUnhealthy", func(t *testing.T) {
		p := newPingTestPlugin(t, []kvstore.ServerConfig{serverA})
		// The ping resolves remote-a -> srvA (enabled), which has no client wired.
		assert.False(t, p.OnSharedChannelsPing(rc("remote-a")))
	})

	t.Run("PingedDisabledServerIsIdleAndIgnoresOtherServers", func(t *testing.T) {
		// srvB is disabled and would-be-unhealthy srvA is also present. A ping for
		// srvB's remote must return true (idle) and must NOT health-check srvA —
		// proving the RemoteId selects the target server rather than checking all.
		p := newPingTestPlugin(t, []kvstore.ServerConfig{serverA, serverBDisabled})
		assert.True(t, p.OnSharedChannelsPing(rc("remote-b")))
	})

	t.Run("UnresolvedRemoteFallsBackToAllEnabledServers", func(t *testing.T) {
		// An unknown remote falls back to requiring every enabled server to be
		// healthy; srvA has no client, so the result is unhealthy.
		p := newPingTestPlugin(t, []kvstore.ServerConfig{serverA})
		assert.False(t, p.OnSharedChannelsPing(rc("remote-unknown")))
	})

	t.Run("NilRemoteFallsBackToAllEnabledServers", func(t *testing.T) {
		p := newPingTestPlugin(t, []kvstore.ServerConfig{serverA})
		assert.False(t, p.OnSharedChannelsPing(nil))
	})

	t.Run("NilRemoteWithOnlyDisabledServersIsHealthy", func(t *testing.T) {
		// Fallback path skips disabled servers, so a nil remote with only a disabled
		// server registered is healthy (idle).
		p := newPingTestPlugin(t, []kvstore.ServerConfig{serverBDisabled})
		assert.True(t, p.OnSharedChannelsPing(nil))
	})
}
