package test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	matrixtest "github.com/mattermost/mattermost-plugin-matrix-bridge/testcontainers/matrix"
)

func TestUserSessionActsAsUser(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping container-backed integration test in -short mode")
	}

	mc := matrixtest.StartMatrixContainer(t, matrixtest.DefaultMatrixConfig())
	t.Cleanup(func() { mc.Cleanup(t) })

	unique := fmt.Sprintf("%d", time.Now().UnixNano())
	user := mc.CreateUser(t, "session"+unique, "password123")
	require.NotEmpty(t, user.AccessToken)

	t.Run("SendEventAsUser", func(t *testing.T) {
		roomID, alias := mc.CreateRoomAsUser(t, user, "Session room", "session-"+unique)
		resolved, err := mc.Client.ResolveRoomAlias(alias)
		require.NoError(t, err)
		require.Equal(t, roomID, resolved)
		mc.JoinRoom(t, roomID)

		body := "hello from " + unique
		eventID := mc.SendEventAsUser(t, user, roomID, "m.room.message", map[string]any{"msgtype": "m.text", "body": body})

		event := mc.WaitForRoomEvent(t, roomID, func(e matrixtest.Event) bool {
			return e.Type == "m.room.message" && e.Content["body"] == body
		})
		require.Equal(t, eventID, event.EventID)
		require.Equal(t, user.UserID, event.Sender)

		mc.RequireNoRoomEvent(t, roomID, func(e matrixtest.Event) bool {
			return e.Content["body"] == "never sent "+unique
		}, time.Second)
	})

	t.Run("JoinRoomAsUser", func(t *testing.T) {
		roomID := mc.CreateRoom(t, "Invite room "+unique)
		require.NoError(t, mc.Client.InviteUserToRoom(roomID, user.UserID))
		require.NoError(t, mc.JoinRoomAsUser(t, user.UserID, roomID))

		var membership string
		for _, member := range mc.GetRoomMembers(t, roomID) {
			if member.UserID == user.UserID {
				membership = member.Membership
			}
		}
		require.Equal(t, "join", membership)
	})
}
