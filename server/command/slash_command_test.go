package command

import (
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/matrix"
	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
)

// TestDispatchEdges covers the top-level and server-group dispatch fall-throughs.
func TestDispatchEdges(t *testing.T) {
	handler, _ := newServerCommandHandler(t, true)

	t.Run("BareMatrixShowsUsage", func(t *testing.T) {
		resp := runServer(handler, "/matrix")
		assert.Contains(t, resp.Text, "Usage: /matrix")
		// The usage now advertises the server subcommand.
		assert.Contains(t, resp.Text, "server")
	})

	t.Run("UnknownSubcommand", func(t *testing.T) {
		resp := runServer(handler, "/matrix bogus")
		assert.Contains(t, resp.Text, "Unknown subcommand")
		assert.Contains(t, resp.Text, "server")
	})

	t.Run("StatusIsStatic", func(t *testing.T) {
		resp := runServer(handler, "/matrix status")
		assert.Contains(t, resp.Text, "Matrix Bridge Status")
	})

	t.Run("ServerWithoutSubcommandShowsUsage", func(t *testing.T) {
		resp := runServer(handler, "/matrix server")
		assert.Contains(t, resp.Text, "Usage: /matrix server")
	})

	t.Run("ServerUnknownSubcommandShowsUsage", func(t *testing.T) {
		resp := runServer(handler, "/matrix server frobnicate")
		assert.Contains(t, resp.Text, "Usage: /matrix server")
	})
}

// TestServerUnmapCommand exercises the `/matrix server unmap [server_id]` path.
func TestServerUnmapCommand(t *testing.T) {
	t.Run("UnknownServerErrors", func(t *testing.T) {
		handler, mp := newServerCommandHandler(t, true)
		seedServers(t, mp, kvstore.ServerConfig{ServerID: "s1", ServerName: "a.example.org", ServerURL: "https://a.example.org", Enabled: true})
		resp := handler.executeMatrixCommand(&model.CommandArgs{
			Command:   "/matrix server unmap does-not-exist",
			UserId:    "admin-user",
			ChannelId: "chan-1",
		})
		assert.Contains(t, resp.Text, "No server found")
	})

	t.Run("UnmappedChannelReportsNoMapping", func(t *testing.T) {
		handler, mp := newServerCommandHandler(t, true)
		seedServers(t, mp, kvstore.ServerConfig{ServerID: "s1", ServerName: "a.example.org", ServerURL: "https://a.example.org", Enabled: true})
		env := handler.pluginAPI.(*plugintest.API)
		env.On("GetChannel", "chan-1").Return(&model.Channel{Id: "chan-1", Name: "town"}, nil).Maybe()

		// server_id omitted -> resolves to the sole server; channel has no mapping.
		resp := handler.executeMatrixCommand(&model.CommandArgs{
			Command:   "/matrix server unmap",
			UserId:    "admin-user",
			ChannelId: "chan-1",
		})
		assert.Contains(t, resp.Text, "No Mapping Found")
	})
}

// TestServerMapUpsertPreservesOtherServer verifies that mapping a channel to a
// room on one server preserves an existing mapping to a different server (the
// multi-server upsert behavior). JoinRoom is best-effort, so an unreachable
// client still results in the mapping being written.
func TestServerMapUpsertPreservesOtherServer(t *testing.T) {
	handler, mp := newServerCommandHandler(t, true)
	seedServers(t, mp,
		kvstore.ServerConfig{ServerID: "srv_a.example.org", ServerName: "a.example.org", ServerURL: "https://a.example.org", Enabled: true, SiteURL: "https://a.example.org"},
		kvstore.ServerConfig{ServerID: "srv_b.example.org", ServerName: "b.example.org", ServerURL: "https://b.example.org", Enabled: true, SiteURL: "https://b.example.org"},
	)
	// Pre-existing mapping to server B on this channel.
	valB, err := kvstore.BuildSingleChannelMapping("srv_b.example.org", "!roomB:b.example.org")
	require.NoError(t, err)
	require.NoError(t, mp.kvstore.Set(kvstore.BuildChannelMappingKey("chan-1"), valB))

	// Unreachable client so network calls fail fast and best-effort.
	mp.matrixClient = matrix.NewClientWithLoggerAndRateLimit("http://127.0.0.1:1", "as", "remote", "a.example.org", matrix.NewTestLogger(t), matrix.TestRateLimitConfig())
	env := handler.pluginAPI.(*plugintest.API)
	env.On("GetChannel", "chan-1").Return(&model.Channel{Id: "chan-1", Name: "town-square"}, nil).Maybe()
	env.On("GetChannelMembers", "chan-1", mock.Anything, mock.Anything).Return(model.ChannelMembers{}, nil).Maybe()
	env.On("ShareChannel", mock.Anything).Return(nil, nil).Maybe()
	env.On("UpdateChannel", mock.Anything).Return(nil, nil).Maybe()
	env.On("InviteRemoteToChannel", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()

	resp := handler.executeMatrixCommand(&model.CommandArgs{
		Command:   "/matrix server map srv_a.example.org !roomA:a.example.org",
		UserId:    "admin-user",
		ChannelId: "chan-1",
		TeamId:    "team-1",
	})
	require.Contains(t, resp.Text, "Mapping Saved")

	// Both servers' entries must be present in the channel mapping.
	fwd, err := mp.kvstore.Get(kvstore.BuildChannelMappingKey("chan-1"))
	require.NoError(t, err)
	mappings, err := kvstore.ParseChannelServerMappings(fwd)
	require.NoError(t, err)
	assert.Equal(t, "!roomA:a.example.org", kvstore.RoomIDForServer(mappings, "srv_a.example.org"), "new server mapping added")
	assert.Equal(t, "!roomB:b.example.org", kvstore.RoomIDForServer(mappings, "srv_b.example.org"), "existing server mapping preserved")
}

// TestServerRegistrationOmittedServerID verifies the server_id may be omitted for
// registration when exactly one server is configured.
func TestServerRegistrationOmittedServerID(t *testing.T) {
	handler, mp := newServerCommandHandler(t, true)
	seedServers(t, mp, kvstore.ServerConfig{ServerID: "s1", ServerURL: "https://matrix.example.com", ServerName: "example.com", ASToken: "as", HSToken: "hs", Enabled: true})
	mp.pluginAPI.On("GetConfig").Return(&model.Config{ServiceSettings: model.ServiceSettings{SiteURL: model.NewPointer("https://mm.example.com")}}).Maybe()

	resp := runServer(handler, "/matrix server registration")
	require.NotNil(t, resp)
	assert.Contains(t, resp.Text, `as_token: "as"`)
	assert.Contains(t, resp.Text, "@_mattermost_.*:example.com")
}

// TestServerListShowsMappedChannelCount verifies the list output includes the
// per-server mapped-channel count.
func TestServerListShowsMappedChannelCount(t *testing.T) {
	handler, mp := newServerCommandHandler(t, true)
	seedServers(t, mp, kvstore.ServerConfig{ServerID: "srv1", ServerName: "a.example.org", ServerURL: "https://a.example.org", Enabled: true})

	// One channel mapped to srv1.
	val, err := kvstore.BuildSingleChannelMapping("srv1", "!room:a.example.org")
	require.NoError(t, err)
	require.NoError(t, mp.kvstore.Set(kvstore.BuildChannelMappingKey("chan-1"), val))

	resp := runServer(handler, "/matrix server list")
	assert.Contains(t, resp.Text, "Mapped channels: 1")
}

// TestStatusCommand covers /matrix status across the registry.
func TestStatusCommand(t *testing.T) {
	t.Run("NoServersConfigured", func(t *testing.T) {
		handler, _ := newServerCommandHandler(t, true)
		resp := runServer(handler, "/matrix status")
		require.NotNil(t, resp)
		assert.Contains(t, resp.Text, "Matrix Bridge Status")
		assert.Contains(t, resp.Text, "No Matrix servers are configured")
	})

	t.Run("EnabledServerShowsConnectionAndMappedCount", func(t *testing.T) {
		handler, mp := newServerCommandHandler(t, true)
		seedServers(t, mp, kvstore.ServerConfig{
			ServerID: "srv1", ServerName: "a.example.org", ServerURL: "https://a.example.org", Enabled: true,
		})
		// A channel mapped to srv1.
		val, err := kvstore.BuildSingleChannelMapping("srv1", "!room:a.example.org")
		require.NoError(t, err)
		require.NoError(t, mp.kvstore.Set(kvstore.BuildChannelMappingKey("chan-1"), val))
		// No Matrix client wired -> connection reports "client not initialized".
		resp := runServer(handler, "/matrix status")
		assert.Contains(t, resp.Text, "**a.example.org**")
		assert.Contains(t, resp.Text, "Status: enabled")
		assert.Contains(t, resp.Text, "client not initialized")
		assert.Contains(t, resp.Text, "Mapped channels: 1")
	})

	t.Run("DisabledServerIsNotProbed", func(t *testing.T) {
		handler, mp := newServerCommandHandler(t, true)
		seedServers(t, mp, kvstore.ServerConfig{
			ServerID: "srv1", ServerName: "a.example.org", ServerURL: "https://a.example.org", Enabled: false,
		})
		resp := runServer(handler, "/matrix status")
		assert.Contains(t, resp.Text, "disabled (not syncing)")
		assert.NotContains(t, resp.Text, "Connection:", "a disabled server is not connection-probed")
	})

	t.Run("ServerStatusSubcommandTargetsOne", func(t *testing.T) {
		handler, mp := newServerCommandHandler(t, true)
		seedServers(t, mp,
			kvstore.ServerConfig{ServerID: "srv1", ServerName: "a.example.org", ServerURL: "https://a.example.org", Enabled: false},
			kvstore.ServerConfig{ServerID: "srv2", ServerName: "b.example.org", ServerURL: "https://b.example.org", Enabled: false, SiteURL: "https://b.example.org"},
		)
		resp := runServer(handler, "/matrix server status srv2")
		assert.Contains(t, resp.Text, "Matrix Server Status")
		assert.Contains(t, resp.Text, "**b.example.org**")
		assert.NotContains(t, resp.Text, "a.example.org", "only the targeted server is shown")
	})
}

// TestServerListFallsBackToHostWhenNameEmpty verifies the migrated single-server
// entry (which typically has an empty ServerName because it relies on .well-known
// discovery) is displayed with its URL host rather than a blank label.
func TestServerListFallsBackToHostWhenNameEmpty(t *testing.T) {
	handler, mp := newServerCommandHandler(t, true)
	seedServers(t, mp, kvstore.ServerConfig{
		ServerID:  "jgmy53ceb4ggo7bwnh8se7uymc",
		ServerURL: "http://localhost:8888",
		// ServerName intentionally empty (migrated single-server entry).
		UsernamePrefix: "matrix",
		Enabled:        true,
	})

	resp := runServer(handler, "/matrix server list")
	assert.Contains(t, resp.Text, "**localhost**", "empty ServerName should fall back to the URL host")
	assert.NotContains(t, resp.Text, "****", "the label must never be blank")
}
