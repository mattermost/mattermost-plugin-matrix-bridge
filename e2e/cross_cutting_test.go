package e2e

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/e2e/harness"
	matrixtest "github.com/mattermost/mattermost-plugin-matrix-bridge/testcontainers/matrix"
)

// echoWindow is how long every negative check in this file waits for an echo or a duplicate.
const echoWindow = 5 * time.Second

// newEchoChannel returns a bridged channel whose setup events the plugin has already processed.
// When the test ends it fails if the plugin failed to process a later Matrix event for the room.
// The webhook answers such a failure with 503 and Synapse retries, so the expected post can still
// appear and only the log shows it. Errors the webhook treats as permanent are logged differently
// and aren't caught.
func newEchoChannel(t *testing.T) (*harness.Env, *harness.BridgedChannel) {
	t.Helper()
	env := harness.Shared(t)
	bridged := harness.NewBridgedChannel(t)
	// /matrix map joins the admin's ghost before it stores the room mapping, so the plugin can
	// fail that join and Synapse retries it. Only failures logged after setup count.
	requireProcessedBefore(t, env, bridged)
	logStart := len(mattermostLog(t, env))

	t.Cleanup(func() {
		logs := mattermostLog(t, env)
		require.GreaterOrEqual(t, len(logs), logStart, "Mattermost log shrank; was it rotated?")
		var failures []string
		for line := range strings.SplitSeq(logs[logStart:], "\n") {
			if strings.Contains(line, "Failed to process Matrix event") && strings.Contains(line, bridged.RoomID) {
				failures = append(failures, line)
			}
		}
		require.Empty(t, failures, "plugin failed to process Matrix events for room %s", bridged.RoomID)
	})
	return env, bridged
}

// requireProcessedBefore sends a sentinel message from the room's Matrix user and waits for its
// post. Synapse delivers appservice transactions in order and holds later ones while it retries,
// so once it returns every earlier event in the room has reached the plugin and been handled.
func requireProcessedBefore(t *testing.T, env *harness.Env, bridged *harness.BridgedChannel) {
	t.Helper()
	sentinel := "sentinel " + model.NewId()
	env.Synapse.SendEventAsUser(t, bridged.MatrixUser, bridged.RoomID, "m.room.message", map[string]any{
		"msgtype": "m.text",
		"body":    sentinel,
	})
	harness.WaitForPost(t, bridged.MattermostClient, bridged.Channel.Id, func(p *model.Post) bool {
		return p.Message == sentinel
	})
}

// mattermostLog returns the whole Mattermost container log. It doesn't use t.Context(), which is
// already canceled when cleanups run.
func mattermostLog(t *testing.T, env *harness.Env) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	reader, err := env.Mattermost.Logs(ctx)
	require.NoError(t, err, "read Mattermost log")
	defer func() { _ = reader.Close() }()
	logs, err := io.ReadAll(reader)
	require.NoError(t, err, "read Mattermost log")
	return string(logs)
}

// relatesTo reports whether e relates to eventID with relType.
func relatesTo(e matrixtest.Event, relType, eventID string) bool {
	relation, _ := e.Content["m.relates_to"].(map[string]any)
	return relation["rel_type"] == relType && relation["event_id"] == eventID
}

func TestLoopPreventionMessageMattermostToMatrix(t *testing.T) {
	env, bridged := newEchoChannel(t)
	message := "loop check from Mattermost " + model.NewId()
	ghost := harness.GhostUserID(bridged.MattermostUser.Id)

	post, _, err := bridged.MattermostClient.CreatePost(t.Context(), &model.Post{
		ChannelId: bridged.Channel.Id,
		Message:   message,
	})
	require.NoError(t, err)
	event := env.Synapse.WaitForRoomEvent(t, bridged.RoomID, func(e matrixtest.Event) bool {
		return e.Type == "m.room.message" && e.Sender == ghost && e.Content["body"] == message
	})
	// The ghost's send doesn't wait for the echo to reach the plugin.
	requireProcessedBefore(t, env, bridged)

	// The plugin stores the event ID on the post, and Mattermost syncs that update out again.
	// An m.replace means both the redundant-edit tracker and updatePostInMatrix's
	// identical-content check let it through.
	env.Synapse.RequireNoRoomEvent(t, bridged.RoomID, func(e matrixtest.Event) bool {
		return e.EventID != event.EventID && (e.Content["body"] == message || relatesTo(e, "m.replace", event.EventID))
	}, echoWindow)
	harness.RequireNoPost(t, bridged.MattermostClient, bridged.Channel.Id, func(p *model.Post) bool {
		return p.Id != post.Id && p.Message == message
	}, echoWindow)
}

func TestLoopPreventionMessageMatrixToMattermost(t *testing.T) {
	env, bridged := newEchoChannel(t)
	message := "loop check from Matrix " + model.NewId()

	eventID := env.Synapse.SendEventAsUser(t, bridged.MatrixUser, bridged.RoomID, "m.room.message", map[string]any{
		"msgtype": "m.text",
		"body":    message,
	})
	post := harness.WaitForPost(t, bridged.MattermostClient, bridged.Channel.Id, func(p *model.Post) bool {
		return p.Message == message
	})

	harness.RequireNoPost(t, bridged.MattermostClient, bridged.Channel.Id, func(p *model.Post) bool {
		return p.Id != post.Id && p.Message == message
	}, echoWindow)
	env.Synapse.RequireNoRoomEvent(t, bridged.RoomID, func(e matrixtest.Event) bool {
		return e.EventID != eventID && (e.Content["body"] == message || relatesTo(e, "m.replace", eventID))
	}, echoWindow)
}
