package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
)

func TestHandleMatrixMemberDM_EarlyExits(t *testing.T) {
	stateKey := "@alice:matrix.example.com"

	tests := []struct {
		name  string
		event MatrixEvent
	}{
		{
			name: "nil content returns empty channel ID",
			event: MatrixEvent{
				RoomID:   "!room:matrix.example.com",
				Sender:   "@bob:matrix.example.com",
				StateKey: &stateKey,
				Content:  nil,
			},
		},
		{
			name: "leave membership returns empty channel ID",
			event: MatrixEvent{
				RoomID:   "!room:matrix.example.com",
				Sender:   "@bob:matrix.example.com",
				StateKey: &stateKey,
				Content:  map[string]any{"membership": "leave"},
			},
		},
		{
			name: "ban membership returns empty channel ID",
			event: MatrixEvent{
				RoomID:   "!room:matrix.example.com",
				Sender:   "@bob:matrix.example.com",
				StateKey: &stateKey,
				Content:  map[string]any{"membership": "ban"},
			},
		},
		{
			name: "missing membership field returns empty channel ID",
			event: MatrixEvent{
				RoomID:   "!room:matrix.example.com",
				Sender:   "@bob:matrix.example.com",
				StateKey: &stateKey,
				Content:  map[string]any{},
			},
		},
		{
			name: "nil state_key returns empty channel ID",
			event: MatrixEvent{
				RoomID:   "!room:matrix.example.com",
				Sender:   "@bob:matrix.example.com",
				StateKey: nil,
				Content:  map[string]any{"membership": "join"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plugin := &Plugin{}
			plugin.logger = &testLogger{t: t}

			// These cases all early-exit before ghost detection, so no server
			// registry is needed; any serverID is fine.
			channelID, err := plugin.handleMatrixMemberDM(testServerID, tt.event)

			require.NoError(t, err)
			assert.Equal(t, "", channelID)
		})
	}
}

// TestIsGhostUserPerServer verifies that ghost-user (loop-prevention) detection is
// scoped to each server's own domain. A ghost minted on one server's domain must be
// recognized only for events originating from that server; misresolving the domain
// would either fail to skip the plugin's own echoes (message loops) or misclassify a
// real Matrix user as a ghost (dropped inbound messages).
func TestIsGhostUserPerServer(t *testing.T) {
	const (
		serverAID = "serveraserveraserveraserv01"
		serverBID = "serverbserverbserverbserv02"
	)

	plugin := &Plugin{}
	plugin.logger = &testLogger{t: t}
	plugin.kvstore = NewMemoryKVStore()

	// Ghost IDs use the homeserver's Matrix ServerName as their domain, which is the
	// source serverDomainForID uses. Server C below deliberately has a connection
	// URL host that differs from its ServerName (delegation) to prove recognition
	// keys off ServerName, not the URL host.
	const serverCID = "servercservercservercserv03"
	servers := []kvstore.ServerConfig{
		{ServerID: serverAID, ServerURL: "https://synapse-a.local", ServerName: "synapse-a.local"},
		{ServerID: serverBID, ServerURL: "https://synapse-b.local", ServerName: "synapse-b.local"},
		{ServerID: serverCID, ServerURL: "https://matrix-internal.example.com:8448", ServerName: "example.com"},
	}
	data, err := json.Marshal(servers)
	require.NoError(t, err)
	require.NoError(t, plugin.kvstore.Set(kvstore.KeyServersConfig, data))

	ghostA := "@_mattermost_abc123:synapse-a.local"

	assert.True(t, plugin.isGhostUser(serverAID, ghostA),
		"a server-A ghost is recognized for server-A events")
	assert.False(t, plugin.isGhostUser(serverBID, ghostA),
		"a server-A ghost is NOT a ghost for server-B events (different domain)")
	assert.False(t, plugin.isGhostUser(serverAID, "@alice:synapse-a.local"),
		"a real Matrix user is not a ghost")
	assert.False(t, plugin.isGhostUser("unknown-server-id", ghostA),
		"an unresolvable server yields false rather than a false positive")

	// Delegation: ghost domain is the ServerName (example.com), not the URL host
	// (matrix-internal.example.com). Recognizing it proves loop prevention works
	// when the connection host and the Matrix server name differ.
	ghostC := "@_mattermost_def456:example.com"
	assert.True(t, plugin.isGhostUser(serverCID, ghostC),
		"a ghost is recognized by ServerName even when it differs from the URL host")
	assert.False(t, plugin.isGhostUser(serverCID, "@_mattermost_def456:matrix-internal.example.com"),
		"the URL host is NOT the ghost domain under delegation")
}

func TestHandleMatrixMemberDM_SwitchRouting(t *testing.T) {
	const matrixServerURL = "https://matrix.example.com"
	const serverDomain = "matrix.example.com"

	ghostUserID := "@_mattermost_userid123:" + serverDomain
	regularUserID := "@alice:" + serverDomain

	newPlugin := func(t *testing.T) *Plugin {
		t.Helper()
		plugin := &Plugin{}
		plugin.logger = &testLogger{t: t}
		plugin.configuration = &configuration{MatrixServerURL: matrixServerURL}
		plugin.kvstore = NewMemoryKVStore()
		// Seed the registry so isGhostUser can resolve the server's domain by ID.
		seedTestServerConfig(plugin)
		return plugin
	}

	t.Run("neither user is ghost returns empty channel ID", func(t *testing.T) {
		plugin := newPlugin(t)

		sk := regularUserID
		event := MatrixEvent{
			RoomID:   "!room:" + serverDomain,
			Sender:   "@bob:" + serverDomain,
			StateKey: &sk,
			Content:  map[string]any{"membership": "join"},
		}

		channelID, err := plugin.handleMatrixMemberDM(testServerID, event)

		require.NoError(t, err)
		assert.Equal(t, "", channelID)
	})

	t.Run("ghost user as target reaches createDMChannelForGhostUser", func(t *testing.T) {
		plugin := newPlugin(t)

		sk := ghostUserID // ghost is target (state_key)
		event := MatrixEvent{
			RoomID:   "!room:" + serverDomain,
			Sender:   regularUserID,
			StateKey: &sk,
			Content:  map[string]any{"membership": "join"},
		}

		// Ghost not registered in kvstore → createDMChannelForGhostUser returns "", nil silently
		channelID, err := plugin.handleMatrixMemberDM(testServerID, event)

		require.NoError(t, err)
		assert.Equal(t, "", channelID)
	})

	t.Run("ghost user as actor reaches createDMChannelForGhostUser", func(t *testing.T) {
		plugin := newPlugin(t)

		sk := regularUserID
		event := MatrixEvent{
			RoomID:   "!room:" + serverDomain,
			Sender:   ghostUserID, // ghost is actor (sender)
			StateKey: &sk,
			Content:  map[string]any{"membership": "join"},
		}

		// Ghost not registered in kvstore → createDMChannelForGhostUser returns "", nil silently
		channelID, err := plugin.handleMatrixMemberDM(testServerID, event)

		require.NoError(t, err)
		assert.Equal(t, "", channelID)
	})

	t.Run("invite membership is also handled", func(t *testing.T) {
		plugin := newPlugin(t)

		sk := regularUserID
		event := MatrixEvent{
			RoomID:   "!room:" + serverDomain,
			Sender:   "@bob:" + serverDomain,
			StateKey: &sk,
			Content:  map[string]any{"membership": "invite"},
		}

		// Neither user is ghost → default case → returns "", nil
		channelID, err := plugin.handleMatrixMemberDM(testServerID, event)

		require.NoError(t, err)
		assert.Equal(t, "", channelID)
	})
}
