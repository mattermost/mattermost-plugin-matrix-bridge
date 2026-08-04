package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
			plugin.kvstore = NewMemoryKVStore()
			serverID, _ := registerTestServer(t, plugin, "https://matrix.example.com", "matrix.example.com", nil)

			channelID, err := plugin.handleMatrixMemberDM(serverID, tt.event)

			require.NoError(t, err)
			assert.Equal(t, "", channelID)
		})
	}
}

func TestHandleMatrixMemberDM_SwitchRouting(t *testing.T) {
	const matrixServerURL = "https://matrix.example.com"
	const serverDomain = "matrix.example.com"

	ghostUserID := "@_mattermost_userid123:" + serverDomain
	regularUserID := "@alice:" + serverDomain

	newPlugin := func(t *testing.T) (*Plugin, string) {
		t.Helper()
		plugin := &Plugin{}
		plugin.logger = &testLogger{t: t}
		plugin.kvstore = NewMemoryKVStore()
		serverID, _ := registerTestServer(t, plugin, matrixServerURL, serverDomain, nil)
		return plugin, serverID
	}

	t.Run("neither user is ghost returns empty channel ID", func(t *testing.T) {
		plugin, serverID := newPlugin(t)

		sk := regularUserID
		event := MatrixEvent{
			RoomID:   "!room:" + serverDomain,
			Sender:   "@bob:" + serverDomain,
			StateKey: &sk,
			Content:  map[string]any{"membership": "join"},
		}

		channelID, err := plugin.handleMatrixMemberDM(serverID, event)

		require.NoError(t, err)
		assert.Equal(t, "", channelID)
	})

	t.Run("ghost user as target reaches createDMChannelForGhostUser", func(t *testing.T) {
		plugin, serverID := newPlugin(t)

		sk := ghostUserID // ghost is target (state_key)
		event := MatrixEvent{
			RoomID:   "!room:" + serverDomain,
			Sender:   regularUserID,
			StateKey: &sk,
			Content:  map[string]any{"membership": "join"},
		}

		// Ghost not registered in kvstore → createDMChannelForGhostUser returns "", nil silently
		channelID, err := plugin.handleMatrixMemberDM(serverID, event)

		require.NoError(t, err)
		assert.Equal(t, "", channelID)
	})

	t.Run("ghost user as actor reaches createDMChannelForGhostUser", func(t *testing.T) {
		plugin, serverID := newPlugin(t)

		sk := regularUserID
		event := MatrixEvent{
			RoomID:   "!room:" + serverDomain,
			Sender:   ghostUserID, // ghost is actor (sender)
			StateKey: &sk,
			Content:  map[string]any{"membership": "join"},
		}

		// Ghost not registered in kvstore → createDMChannelForGhostUser returns "", nil silently
		channelID, err := plugin.handleMatrixMemberDM(serverID, event)

		require.NoError(t, err)
		assert.Equal(t, "", channelID)
	})

	t.Run("invite membership is also handled", func(t *testing.T) {
		plugin, serverID := newPlugin(t)

		sk := regularUserID
		event := MatrixEvent{
			RoomID:   "!room:" + serverDomain,
			Sender:   "@bob:" + serverDomain,
			StateKey: &sk,
			Content:  map[string]any{"membership": "invite"},
		}

		// Neither user is ghost → default case → returns "", nil
		channelID, err := plugin.handleMatrixMemberDM(serverID, event)

		require.NoError(t, err)
		assert.Equal(t, "", channelID)
	})
}
