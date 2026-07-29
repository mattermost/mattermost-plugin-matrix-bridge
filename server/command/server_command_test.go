package command

import (
	"encoding/json"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/matrix"
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
	mockAllLogs(env.api)

	mp := &mockPlugin{
		client:    env.client,
		kvstore:   newMemKV(),
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
		{ServerID: "s2", ServerName: "b.example.org", Enabled: true, SiteURL: "https://b.example.org"},
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
	assert.Contains(t, resp.Text, "/matrix server")
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

	// Register a server and give the mock a Matrix client. The client points at an
	// unreachable address so its network calls (join/resolve/sync) fail fast and
	// best-effort; the mapping is still stored and "Mapping Saved" is returned.
	require.Contains(t, runServer(handler, "/matrix server add http://synapse2.localhost:8889 synapse2.localhost as hs matrix2").Text, "Server registered")
	mp.matrixClient = matrix.NewClientWithLoggerAndRateLimit("http://127.0.0.1:1", "as", "remote", "synapse2.localhost", matrix.NewTestLogger(t), matrix.TestRateLimitConfig())

	// Channel + share + member mocks used by the map handler.
	env := handler.pluginAPI.(*plugintest.API)
	env.On("GetChannel", "chan-1").Return(&model.Channel{Id: "chan-1", Name: "town-square"}, nil).Maybe()
	env.On("GetChannelMembers", "chan-1", mock.Anything, mock.Anything).Return(model.ChannelMembers{}, nil).Maybe()
	env.On("ShareChannel", mock.Anything).Return(nil, nil).Maybe()
	env.On("UpdateChannel", mock.Anything).Return(nil, nil).Maybe()
	env.On("InviteRemoteToChannel", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	env.On("LogWarn", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return().Maybe()

	resp := handler.executeMatrixCommand(&model.CommandArgs{
		Command:   "/matrix server map srv_synapse2.localhost !room2:synapse2.localhost",
		UserId:    "admin-user",
		ChannelId: "chan-1",
		TeamId:    "team-1",
	})
	require.Contains(t, resp.Text, "Mapping Saved")

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

// TestServerCommandMapRecoversFromCorruptMapping verifies /matrix server map
// self-heals a corrupt channel-mapping value instead of aborting with no
// recovery path, matching /matrix unmap's existing corrupt-value recovery.
func TestServerCommandMapRecoversFromCorruptMapping(t *testing.T) {
	handler, mp := newServerCommandHandler(t, true)

	require.Contains(t, runServer(handler, "/matrix server add http://synapse2.localhost:8889 synapse2.localhost as hs matrix2").Text, "Server registered")
	mp.matrixClient = matrix.NewClientWithLoggerAndRateLimit("http://127.0.0.1:1", "as", "remote", "synapse2.localhost", matrix.NewTestLogger(t), matrix.TestRateLimitConfig())

	// Seed a corrupt (unparseable) value under the shared channel-mapping key.
	require.NoError(t, mp.kvstore.Set(kvstore.BuildChannelMappingKey("chan-1"), []byte("!not-json:server")))

	env := handler.pluginAPI.(*plugintest.API)
	env.On("GetChannel", "chan-1").Return(&model.Channel{Id: "chan-1", Name: "town-square"}, nil).Maybe()
	env.On("GetChannelMembers", "chan-1", mock.Anything, mock.Anything).Return(model.ChannelMembers{}, nil).Maybe()
	env.On("ShareChannel", mock.Anything).Return(nil, nil).Maybe()
	env.On("UpdateChannel", mock.Anything).Return(nil, nil).Maybe()
	env.On("InviteRemoteToChannel", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	env.On("LogWarn", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return().Maybe()

	resp := handler.executeMatrixCommand(&model.CommandArgs{
		Command:   "/matrix server map srv_synapse2.localhost !room2:synapse2.localhost",
		UserId:    "admin-user",
		ChannelId: "chan-1",
		TeamId:    "team-1",
	})
	require.Contains(t, resp.Text, "Mapping Saved", "a corrupt existing value must not abort the map with no recovery path")

	fwd, err := mp.kvstore.Get(kvstore.BuildChannelMappingKey("chan-1"))
	require.NoError(t, err)
	mappings, err := kvstore.ParseChannelServerMappings(fwd)
	require.NoError(t, err)
	assert.Equal(t, "!room2:synapse2.localhost", kvstore.RoomIDForServer(mappings, "srv_synapse2.localhost"))
	assert.Len(t, mappings, 1, "corrupt prior data must be discarded, not preserved alongside the new entry")
}

// seedServers writes the given servers into the mock plugin's registry.
func seedServers(t *testing.T, mp *mockPlugin, servers ...kvstore.ServerConfig) {
	t.Helper()
	data, err := json.Marshal(servers)
	require.NoError(t, err)
	require.NoError(t, mp.kvstore.Set(kvstore.KeyServersConfig, data))
}

func TestResolveServerID(t *testing.T) {
	t.Run("NoneConfiguredErrors", func(t *testing.T) {
		h, _ := newServerCommandHandler(t, true)
		id, resp := h.resolveServerID()
		assert.Empty(t, id)
		require.NotNil(t, resp)
		assert.Contains(t, resp.Text, "No Matrix server")
	})

	t.Run("SoleServerResolves", func(t *testing.T) {
		h, mp := newServerCommandHandler(t, true)
		seedServers(t, mp, kvstore.ServerConfig{ServerID: "only", ServerURL: "https://a.example.com"})
		id, resp := h.resolveServerID()
		assert.Nil(t, resp)
		assert.Equal(t, "only", id)
	})

	t.Run("MultipleServersAmbiguous", func(t *testing.T) {
		h, mp := newServerCommandHandler(t, true)
		seedServers(t, mp,
			kvstore.ServerConfig{ServerID: "s1", ServerURL: "https://a.example.com"},
			kvstore.ServerConfig{ServerID: "s2", ServerURL: "https://b.example.com"},
		)
		id, resp := h.resolveServerID()
		assert.Empty(t, id)
		require.NotNil(t, resp)
		assert.Contains(t, resp.Text, "Multiple Matrix servers")
	})
}

func TestResolveServerIDArg(t *testing.T) {
	h, mp := newServerCommandHandler(t, true)
	seedServers(t, mp,
		kvstore.ServerConfig{ServerID: "s1", ServerURL: "https://a.example.com", ServerName: "a.example.com"},
		kvstore.ServerConfig{ServerID: "s2", ServerURL: "https://b.example.com", ServerName: "b.example.com"},
	)

	t.Run("ByServerID", func(t *testing.T) {
		id, resp := h.resolveServerIDArg("s2")
		assert.Nil(t, resp)
		assert.Equal(t, "s2", id)
	})

	t.Run("ByDomain", func(t *testing.T) {
		id, resp := h.resolveServerIDArg("b.example.com")
		assert.Nil(t, resp)
		assert.Equal(t, "s2", id)
	})

	t.Run("UnknownErrors", func(t *testing.T) {
		id, resp := h.resolveServerIDArg("nope.example.com")
		assert.Empty(t, id)
		require.NotNil(t, resp)
		assert.Contains(t, resp.Text, "No server found")
	})

	t.Run("EmptyWithMultipleIsAmbiguous", func(t *testing.T) {
		id, resp := h.resolveServerIDArg("")
		assert.Empty(t, id)
		require.NotNil(t, resp)
		assert.Contains(t, resp.Text, "Multiple Matrix servers")
	})
}

func TestParseOptionalServerArg(t *testing.T) {
	// /matrix server map <room>  -> no server_id, room = fields[3]
	sid, val, ok := parseOptionalServerArg([]string{"/matrix", "server", "map", "!room:hs"}, 3)
	require.True(t, ok)
	assert.Empty(t, sid)
	assert.Equal(t, "!room:hs", val)

	// /matrix server map <server_id> <room>
	sid, val, ok = parseOptionalServerArg([]string{"/matrix", "server", "map", "srv", "!room:hs"}, 3)
	require.True(t, ok)
	assert.Equal(t, "srv", sid)
	assert.Equal(t, "!room:hs", val)

	// Missing value.
	_, _, ok = parseOptionalServerArg([]string{"/matrix", "server", "map"}, 3)
	assert.False(t, ok)
}

func TestBuildRegistrationYAML(t *testing.T) {
	server := kvstore.ServerConfig{ //nolint:gosec // test fixture tokens
		ServerID:   "srv1",
		ServerURL:  "https://matrix.example.com",
		ServerName: "example.com",
		ASToken:    "as-token-value",
		HSToken:    "hs-token-value",
	}
	got, err := buildRegistrationYAML(server, "https://mm.example.com")
	require.NoError(t, err)

	// Byte-compatible with the format the webapp previously produced.
	want := `id: "mattermost-bridge"
url: "https://mm.example.com/plugins/com.mattermost.plugin-matrix-bridge"
as_token: "as-token-value"
hs_token: "hs-token-value"
sender_localpart: "_mattermost_bridge"
namespaces:
  users:
    - exclusive: true
      regex: "@_mattermost_.*:example.com"
  aliases:
    - exclusive: true
      regex: "#_mattermost_.*:example.com"
    - exclusive: false
      regex: "#mattermost-bridge-.*:example.com"
  rooms:
    - exclusive: false
      regex: "!.*:example.com"
rate_limited: false
protocols: ["mattermost"]
de.sorunome.msc2409.push_ephemeral: true
permissions:
  - "m.room.directory"
  - "m.room.membership"`
	assert.Equal(t, want, got)
}

func TestServerRegistrationCommand(t *testing.T) {
	h, mp := newServerCommandHandler(t, true)
	seedServers(t, mp, kvstore.ServerConfig{ServerID: "s1", ServerURL: "https://matrix.example.com", ServerName: "example.com", ASToken: "as", HSToken: "hs"})
	mp.pluginAPI.On("GetConfig").Return(&model.Config{ServiceSettings: model.ServiceSettings{SiteURL: model.NewPointer("https://mm.example.com")}}).Maybe()

	resp := runServer(h, "/matrix server registration s1")
	require.NotNil(t, resp)
	assert.Contains(t, resp.Text, "as_token: \"as\"")
	assert.Contains(t, resp.Text, "@_mattermost_.*:example.com")
	assert.Contains(t, resp.Text, "https://mm.example.com/plugins/com.mattermost.plugin-matrix-bridge")
}

func TestServerRegistrationCommandRequiresAdmin(t *testing.T) {
	h, _ := newServerCommandHandler(t, false)
	resp := runServer(h, "/matrix server registration s1")
	require.NotNil(t, resp)
	assert.Contains(t, resp.Text, "System Administrator")
}
