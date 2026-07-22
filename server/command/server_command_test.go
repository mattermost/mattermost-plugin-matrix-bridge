package command

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

// newServerCommandHandler builds a command handler backed by the in-memory
// mockPlugin, with the command-issuer granted (or denied) system-admin.
func newServerCommandHandler(t *testing.T, isAdmin bool) (*Handler, *mockPlugin) {
	t.Helper()
	env := setupTest()
	setupCommandRegistration(env)
	env.api.On("HasPermissionTo", "admin-user", model.PermissionManageSystem).Return(isAdmin).Maybe()
	env.api.On("HasPermissionTo", mock.Anything, mock.Anything).Return(isAdmin).Maybe()
	env.api.On("LogInfo", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()
	env.api.On("LogError", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()

	mp := &mockPlugin{
		client:    env.client,
		kvstore:   newMemKV(),
		config:    &mockConfiguration{serverURL: "http://test.com"},
		pluginAPI: env.api,
	}
	handler := NewCommandHandler(mp).(*Handler)
	return handler, mp
}

func runServer(h *Handler, command string) *model.CommandResponse {
	return h.executeMatrixCommand(&model.CommandArgs{Command: command, UserId: "admin-user"})
}

func TestServerCommandRequiresAdmin(t *testing.T) {
	handler, _ := newServerCommandHandler(t, false)
	resp := runServer(handler, "/matrix server list")
	require.NotNil(t, resp)
	assert.Contains(t, resp.Text, "System Administrator")
}

func TestServerCommandAddListRemove(t *testing.T) {
	handler, mp := newServerCommandHandler(t, true)

	// list is empty initially
	resp := runServer(handler, "/matrix server list")
	assert.Contains(t, resp.Text, "No Matrix servers are registered")

	// add
	resp = runServer(handler, "/matrix server add http://synapse2.localhost:8889 synapse2.localhost as_tok hs_tok matrix2")
	assert.Contains(t, resp.Text, "Server registered")
	assert.Contains(t, resp.Text, "srv_synapse2.localhost")

	// the registry now holds the injected server
	servers, err := mp.GetManagedServers()
	require.NoError(t, err)
	require.Len(t, servers, 1)
	assert.Equal(t, "synapse2.localhost", servers[0].ServerName)
	assert.Equal(t, "http://synapse2.localhost:8889", servers[0].ServerURL)
	assert.Equal(t, "hs_tok", servers[0].HSToken)
	assert.Equal(t, "matrix2", servers[0].UsernamePrefix)

	// list shows it
	resp = runServer(handler, "/matrix server list")
	assert.Contains(t, resp.Text, "synapse2.localhost")
	assert.Contains(t, resp.Text, "enabled")

	// remove
	resp = runServer(handler, "/matrix server remove srv_synapse2.localhost")
	assert.Contains(t, resp.Text, "Removed server")

	servers, err = mp.GetManagedServers()
	require.NoError(t, err)
	assert.Empty(t, servers)
}

func TestServerCommandAddValidatesArgs(t *testing.T) {
	handler, _ := newServerCommandHandler(t, true)
	// Missing required tokens -> usage message, no server added.
	resp := runServer(handler, "/matrix server add http://x.localhost synapse")
	assert.Contains(t, resp.Text, "Usage: /matrix server")
}

func TestServerCommandRemoveUnknown(t *testing.T) {
	handler, _ := newServerCommandHandler(t, true)
	resp := runServer(handler, "/matrix server remove does-not-exist")
	assert.Contains(t, resp.Text, "No server found")
}

func TestMapCommandRefusesWithMultipleServers(t *testing.T) {
	handler, mp := newServerCommandHandler(t, true)

	servers := []kvstore.ServerConfig{
		{ServerID: "s1", ServerName: "a.example.org", Enabled: true},
		{ServerID: "s2", ServerName: "b.example.org", Enabled: true, Injected: true},
	}
	data, err := json.Marshal(servers)
	require.NoError(t, err)
	require.NoError(t, mp.kvstore.Set(kvstore.KeyServersConfig, data))

	resp := handler.executeMatrixCommand(&model.CommandArgs{
		Command:   "/matrix map #room:a.example.org",
		UserId:    "admin-user",
		ChannelId: "chan-1",
	})
	require.NotNil(t, resp)
	assert.Contains(t, resp.Text, "Multiple Matrix servers")
	assert.Contains(t, resp.Text, "/matrix server map")
}

func TestMapCommandAllowedWithSingleServer(t *testing.T) {
	handler, mp := newServerCommandHandler(t, true)

	servers := []kvstore.ServerConfig{{ServerID: "s1", ServerName: "a.example.org", Enabled: true}}
	data, err := json.Marshal(servers)
	require.NoError(t, err)
	require.NoError(t, mp.kvstore.Set(kvstore.KeyServersConfig, data))

	// With a single server the guard does not fire; the command proceeds past it
	// (and then fails later because the mock has no live Matrix client).
	resp := handler.executeMatrixCommand(&model.CommandArgs{
		Command:   "/matrix map #room:a.example.org",
		UserId:    "admin-user",
		ChannelId: "chan-1",
	})
	require.NotNil(t, resp)
	assert.NotContains(t, resp.Text, "Multiple Matrix servers")
}

func TestServerCommandMapUnknownServer(t *testing.T) {
	handler, _ := newServerCommandHandler(t, true)
	resp := handler.executeMatrixCommand(&model.CommandArgs{
		Command:   "/matrix server map does-not-exist !room:synapse2.localhost",
		UserId:    "admin-user",
		ChannelId: "chan-1",
	})
	assert.Contains(t, resp.Text, "No server found")
}

func TestServerCommandMapStoresMappingUnderServer(t *testing.T) {
	handler, mp := newServerCommandHandler(t, true)

	// Register a server (no live client -> map stores the identifier as given).
	require.Contains(t, runServer(handler, "/matrix server add http://synapse2.localhost:8889 synapse2.localhost as hs matrix2").Text, "Server registered")

	// Channel + share mocks used by the map handler.
	env := handler.pluginAPI.(*plugintest.API)
	env.On("GetChannel", "chan-1").Return(&model.Channel{Id: "chan-1", Name: "town-square"}, nil).Maybe()
	env.On("ShareChannel", mock.Anything).Return(nil, nil).Maybe()
	env.On("UpdateChannel", mock.Anything).Return(nil, nil).Maybe()
	env.On("InviteRemoteToChannel", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()

	resp := handler.executeMatrixCommand(&model.CommandArgs{
		Command:   "/matrix server map srv_synapse2.localhost !room2:synapse2.localhost",
		UserId:    "admin-user",
		ChannelId: "chan-1",
		TeamId:    "team-1",
	})
	require.Contains(t, resp.Text, "Mapping saved")

	// Forward mapping stored under the target server, reverse mapping keyed by it.
	fwd, err := mp.kvstore.Get(kvstore.BuildChannelMappingKey("chan-1"))
	require.NoError(t, err)
	mappings, err := kvstore.ParseChannelServerMappings(fwd)
	require.NoError(t, err)
	assert.Equal(t, "!room2:synapse2.localhost", kvstore.RoomIDForServer(mappings, "srv_synapse2.localhost"))

	rev, err := mp.kvstore.Get(kvstore.BuildRoomMappingKey("srv_synapse2.localhost", "!room2:synapse2.localhost"))
	require.NoError(t, err)
	assert.Equal(t, "chan-1", string(rev))
}
