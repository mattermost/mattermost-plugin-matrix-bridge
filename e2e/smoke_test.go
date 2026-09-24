package e2e

import (
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/e2e/harness"
	matrixtest "github.com/mattermost/mattermost-plugin-matrix-bridge/testcontainers/matrix"
)

func TestSmokeMattermostToMatrix(t *testing.T) {
	env := harness.Shared(t)
	bridged := harness.NewBridgedChannel(t)
	message := "hello from Mattermost " + model.NewId()

	sent := time.Now()
	post, _, err := bridged.MattermostClient.CreatePost(t.Context(), &model.Post{
		ChannelId: bridged.Channel.Id,
		Message:   message,
	})
	require.NoError(t, err)

	event := env.Synapse.WaitForRoomEvent(t, bridged.RoomID, func(e matrixtest.Event) bool {
		return e.Type == "m.room.message" && e.Content["body"] == message
	})
	t.Logf("Mattermost post reached Matrix in %s", time.Since(sent).Round(time.Millisecond))

	require.Equal(t, harness.GhostUserID(bridged.MattermostUser.Id), event.Sender)
	require.Equal(t, post.Id, event.Content["mattermost_post_id"])
}

func TestSmokeMatrixToMattermost(t *testing.T) {
	env := harness.Shared(t)
	bridged := harness.NewBridgedChannel(t)
	message := "hello from Matrix " + model.NewId()

	sent := time.Now()
	env.Synapse.SendEventAsUser(t, bridged.MatrixUser, bridged.RoomID, "m.room.message", map[string]any{
		"msgtype": "m.text",
		"body":    message,
	})

	post := harness.WaitForPost(t, bridged.MattermostClient, bridged.Channel.Id, func(p *model.Post) bool {
		return p.Message == message
	})
	t.Logf("Matrix message reached Mattermost in %s", time.Since(sent).Round(time.Millisecond))

	author, _, err := env.Admin.GetUser(t.Context(), post.UserId, "")
	require.NoError(t, err)
	require.True(t, author.IsRemote(), "post author %s is not a remote user", author.Id)
	require.Equal(t, env.RemoteID, author.GetRemoteID())
}
