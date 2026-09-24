package e2e

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/assert"
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

// relatesToEvent reports whether e relates to eventID with relType.
func relatesToEvent(e matrixtest.Event, relType, eventID string) bool {
	relation, _ := e.Content["m.relates_to"].(map[string]any)
	return relation["rel_type"] == relType && relation["event_id"] == eventID
}

// postAndAwaitGhostEvent creates a post as the channel's Mattermost user and waits for the ghost's
// event for it, and for the plugin to store that event's ID on the post. The plugin writes back
// the post it sent, so editing or deleting the post before that could be undone.
func postAndAwaitGhostEvent(t *testing.T, env *harness.Env, bridged *harness.BridgedChannel, message string) (*model.Post, matrixtest.Event) {
	t.Helper()
	ghost := harness.GhostUserID(bridged.MattermostUser.Id)
	post, _, err := bridged.MattermostClient.CreatePost(t.Context(), &model.Post{
		ChannelId: bridged.Channel.Id,
		Message:   message,
	})
	require.NoError(t, err)
	event := env.Synapse.WaitForRoomEvent(t, bridged.RoomID, func(e matrixtest.Event) bool {
		return e.Type == "m.room.message" && e.Sender == ghost && e.Content["body"] == message
	})
	post = harness.WaitForPost(t, bridged.MattermostClient, bridged.Channel.Id, func(p *model.Post) bool {
		if p.Id != post.Id {
			return false
		}
		for key, value := range p.GetProps() {
			if strings.HasPrefix(key, "matrix_event_id_") && value == event.EventID {
				return true
			}
		}
		return false
	})
	return post, event
}

func TestLoopPreventionMessageMattermostToMatrix(t *testing.T) {
	env, bridged := newEchoChannel(t)
	message := "loop check from Mattermost " + model.NewId()
	post, event := postAndAwaitGhostEvent(t, env, bridged, message)
	// The ghost's send doesn't wait for the echo to reach the plugin.
	requireProcessedBefore(t, env, bridged)

	// The plugin stores the event ID on the post, and Mattermost syncs that update out again.
	// An m.replace means both the redundant-edit tracker and updatePostInMatrix's
	// identical-content check let it through.
	env.Synapse.RequireNoRoomEvent(t, bridged.RoomID, func(e matrixtest.Event) bool {
		return e.EventID != event.EventID && (e.Content["body"] == message || relatesToEvent(e, "m.replace", event.EventID))
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
		return e.EventID != eventID && (e.Content["body"] == message || relatesToEvent(e, "m.replace", eventID))
	}, echoWindow)
}

// requireReactionCount fails unless the post has exactly n reactions throughout echoWindow and in
// a final check after it; only the final check's fetch errors fail the test.
func requireReactionCount(t *testing.T, client *model.Client4, postID string, n int) {
	t.Helper()
	require.Never(t, func() bool {
		reactions, _, err := client.GetReactions(t.Context(), postID)
		return err == nil && len(reactions) != n
	}, echoWindow, matrixtest.PollInterval, "post %s stopped having %d reactions", postID, n)

	reactions, _, err := client.GetReactions(t.Context(), postID)
	require.NoError(t, err, "get reactions for post %s", postID)
	require.Len(t, reactions, n, "reactions on post %s", postID)
}

func TestLoopPreventionEdits(t *testing.T) {
	t.Run("mattermost_to_matrix", func(t *testing.T) {
		env, bridged := newEchoChannel(t)
		ghost := harness.GhostUserID(bridged.MattermostUser.Id)
		post, event := postAndAwaitGhostEvent(t, env, bridged, "edit check "+model.NewId())

		edited := "edited in Mattermost " + model.NewId()
		patched, _, err := bridged.MattermostClient.PatchPost(t.Context(), post.Id, &model.PostPatch{Message: &edited})
		require.NoError(t, err)
		replace := env.Synapse.WaitForRoomEvent(t, bridged.RoomID, func(e matrixtest.Event) bool {
			return e.Sender == ghost && relatesToEvent(e, "m.replace", event.EventID)
		})
		requireProcessedBefore(t, env, bridged)

		env.Synapse.RequireNoRoomEvent(t, bridged.RoomID, func(e matrixtest.Event) bool {
			return e.EventID != replace.EventID && relatesToEvent(e, "m.replace", event.EventID)
		}, echoWindow)
		harness.RequireNoPost(t, bridged.MattermostClient, bridged.Channel.Id, func(p *model.Post) bool {
			return p.Id == post.Id && p.EditAt != patched.EditAt
		}, echoWindow)
	})

	t.Run("matrix_to_mattermost", func(t *testing.T) {
		env, bridged := newEchoChannel(t)
		message := "edit check " + model.NewId()
		eventID := env.Synapse.SendEventAsUser(t, bridged.MatrixUser, bridged.RoomID, "m.room.message", map[string]any{
			"msgtype": "m.text",
			"body":    message,
		})
		post := harness.WaitForPost(t, bridged.MattermostClient, bridged.Channel.Id, func(p *model.Post) bool {
			return p.Message == message
		})

		edited := "edited in Matrix " + model.NewId()
		env.Synapse.SendEventAsUser(t, bridged.MatrixUser, bridged.RoomID, "m.room.message", map[string]any{
			"msgtype":       "m.text",
			"body":          "* " + edited,
			"m.new_content": map[string]any{"msgtype": "m.text", "body": edited},
			"m.relates_to":  map[string]any{"rel_type": "m.replace", "event_id": eventID},
		})
		updated := harness.WaitForPost(t, bridged.MattermostClient, bridged.Channel.Id, func(p *model.Post) bool {
			return p.Id == post.Id && p.Message == edited
		})

		harness.RequireNoPost(t, bridged.MattermostClient, bridged.Channel.Id, func(p *model.Post) bool {
			return p.Id == post.Id && p.EditAt != updated.EditAt
		}, echoWindow)
		env.Synapse.RequireNoRoomEvent(t, bridged.RoomID, func(e matrixtest.Event) bool {
			return e.Sender != bridged.MatrixUser.UserID && relatesToEvent(e, "m.replace", eventID)
		}, echoWindow)
	})
}

func TestLoopPreventionReactions(t *testing.T) {
	t.Run("mattermost_to_matrix", func(t *testing.T) {
		env, bridged := newEchoChannel(t)
		ghost := harness.GhostUserID(bridged.MattermostUser.Id)
		post, event := postAndAwaitGhostEvent(t, env, bridged, "reaction check "+model.NewId())

		_, _, err := bridged.MattermostClient.SaveReaction(t.Context(), &model.Reaction{
			UserId:    bridged.MattermostUser.Id,
			PostId:    post.Id,
			EmojiName: "thumbsup",
		})
		require.NoError(t, err)
		reaction := env.Synapse.WaitForRoomEvent(t, bridged.RoomID, func(e matrixtest.Event) bool {
			return e.Type == "m.reaction" && e.Sender == ghost && relatesToEvent(e, "m.annotation", event.EventID)
		})
		requireProcessedBefore(t, env, bridged)

		env.Synapse.RequireNoRoomEvent(t, bridged.RoomID, func(e matrixtest.Event) bool {
			return e.Type == "m.reaction" && e.EventID != reaction.EventID
		}, echoWindow)
		requireReactionCount(t, bridged.MattermostClient, post.Id, 1)
	})

	t.Run("matrix_to_mattermost", func(t *testing.T) {
		env, bridged := newEchoChannel(t)
		post, event := postAndAwaitGhostEvent(t, env, bridged, "reaction check "+model.NewId())

		env.Synapse.SendEventAsUser(t, bridged.MatrixUser, bridged.RoomID, "m.reaction", map[string]any{
			"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": event.EventID, "key": "👍"},
		})
		require.EventuallyWithT(t, func(c *assert.CollectT) {
			reactions, _, err := bridged.MattermostClient.GetReactions(t.Context(), post.Id)
			if assert.NoError(c, err) && assert.Len(c, reactions, 1) {
				assert.NotEqual(c, bridged.MattermostUser.Id, reactions[0].UserId)
			}
		}, matrixtest.DefaultWaitTimeout, matrixtest.PollInterval, "Matrix reaction never reached post %s", post.Id)

		requireReactionCount(t, bridged.MattermostClient, post.Id, 1)
		env.Synapse.RequireNoRoomEvent(t, bridged.RoomID, func(e matrixtest.Event) bool {
			return e.Type == "m.reaction" && e.Sender != bridged.MatrixUser.UserID
		}, echoWindow)
	})
}

// TestLoopPreventionDeletions matches redactions by type rather than by what they redact, which
// is exact because every room is fresh.
func TestLoopPreventionDeletions(t *testing.T) {
	t.Run("mattermost_to_matrix", func(t *testing.T) {
		env, bridged := newEchoChannel(t)
		ghost := harness.GhostUserID(bridged.MattermostUser.Id)
		message := "deletion check " + model.NewId()
		post, _ := postAndAwaitGhostEvent(t, env, bridged, message)

		_, err := bridged.MattermostClient.DeletePost(t.Context(), post.Id)
		require.NoError(t, err)
		redaction := env.Synapse.WaitForRoomEvent(t, bridged.RoomID, func(e matrixtest.Event) bool {
			return e.Type == "m.room.redaction" && e.Sender == ghost
		})
		requireProcessedBefore(t, env, bridged)

		env.Synapse.RequireNoRoomEvent(t, bridged.RoomID, func(e matrixtest.Event) bool {
			return e.Type == "m.room.redaction" && e.EventID != redaction.EventID
		}, echoWindow)
		harness.RequireNoPost(t, bridged.MattermostClient, bridged.Channel.Id, func(p *model.Post) bool {
			return p.Message == message
		}, echoWindow)
	})

	t.Run("matrix_to_mattermost", func(t *testing.T) {
		env, bridged := newEchoChannel(t)
		message := "deletion check " + model.NewId()
		eventID := env.Synapse.SendEventAsUser(t, bridged.MatrixUser, bridged.RoomID, "m.room.message", map[string]any{
			"msgtype": "m.text",
			"body":    message,
		})
		post := harness.WaitForPost(t, bridged.MattermostClient, bridged.Channel.Id, func(p *model.Post) bool {
			return p.Message == message
		})

		redactionID := harness.RedactAsUser(t, bridged.MatrixUser, bridged.RoomID, eventID)
		require.EventuallyWithT(t, func(c *assert.CollectT) {
			list, _, err := bridged.MattermostClient.GetPostsForChannel(t.Context(), bridged.Channel.Id, 0, 100, "", false, false)
			if assert.NoError(c, err) {
				assert.NotContains(c, list.Posts, post.Id)
			}
		}, matrixtest.DefaultWaitTimeout, matrixtest.PollInterval, "post %s was never deleted", post.Id)

		env.Synapse.RequireNoRoomEvent(t, bridged.RoomID, func(e matrixtest.Event) bool {
			return e.Type == "m.room.redaction" && e.EventID != redactionID
		}, echoWindow)
	})
}

func TestLoopPreventionFiles(t *testing.T) {
	data := []byte(`{"loop":"check"}`)

	t.Run("mattermost_to_matrix", func(t *testing.T) {
		env, bridged := newEchoChannel(t)
		ghost := harness.GhostUserID(bridged.MattermostUser.Id)
		name := "loop-" + model.NewId() + ".json"

		upload, _, err := bridged.MattermostClient.UploadFile(t.Context(), data, bridged.Channel.Id, name)
		require.NoError(t, err)
		require.Len(t, upload.FileInfos, 1)
		post, _, err := bridged.MattermostClient.CreatePost(t.Context(), &model.Post{
			ChannelId: bridged.Channel.Id,
			FileIds:   []string{upload.FileInfos[0].Id},
		})
		require.NoError(t, err)
		fileEvent := env.Synapse.WaitForRoomEvent(t, bridged.RoomID, func(e matrixtest.Event) bool {
			msgtype := e.Content["msgtype"]
			return e.Sender == ghost && e.Content["body"] == name && (msgtype == "m.file" || msgtype == "m.image")
		})
		requireProcessedBefore(t, env, bridged)

		env.Synapse.RequireNoRoomEvent(t, bridged.RoomID, func(e matrixtest.Event) bool {
			return e.EventID != fileEvent.EventID && e.Content["body"] == name
		}, echoWindow)
		harness.RequireNoPost(t, bridged.MattermostClient, bridged.Channel.Id, func(p *model.Post) bool {
			return p.Id != post.Id && len(p.FileIds) > 0
		}, echoWindow)
	})

	t.Run("matrix_to_mattermost", func(t *testing.T) {
		env, bridged := newEchoChannel(t)
		name := "loop-" + model.NewId() + ".json"

		mxc := harness.UploadMediaAsUser(t, bridged.MatrixUser, name, "application/json", data)
		env.Synapse.SendEventAsUser(t, bridged.MatrixUser, bridged.RoomID, "m.room.message", map[string]any{
			"msgtype": "m.file",
			"body":    name,
			"url":     mxc,
			"info":    map[string]any{"mimetype": "application/json", "size": len(data)},
		})
		post := harness.WaitForPost(t, bridged.MattermostClient, bridged.Channel.Id, func(p *model.Post) bool {
			return len(p.FileIds) == 1
		})

		harness.RequireNoPost(t, bridged.MattermostClient, bridged.Channel.Id, func(p *model.Post) bool {
			return p.Id != post.Id && len(p.FileIds) > 0
		}, echoWindow)
		env.Synapse.RequireNoRoomEvent(t, bridged.RoomID, func(e matrixtest.Event) bool {
			return e.Sender != bridged.MatrixUser.UserID && e.Content["body"] == name
		}, echoWindow)
	})
}

// sendAsGhost sends a message into the room as ghostID through appservice impersonation and
// returns its event ID.
func sendAsGhost(t *testing.T, env *harness.Env, roomID, ghostID string, content map[string]any) string {
	t.Helper()
	endpoint := "/_matrix/client/v3/rooms/" + url.PathEscape(roomID) + "/send/m.room.message/" + model.NewId() +
		"?user_id=" + url.QueryEscape(ghostID)
	result, err := env.Synapse.DoAsUser(&matrixtest.User{AccessToken: env.Synapse.ASToken}, http.MethodPut, endpoint, content)
	require.NoError(t, err, "send as %s to room %s", ghostID, roomID)
	response, _ := result.(map[string]any)
	eventID, _ := response["event_id"].(string)
	require.NotEmpty(t, eventID, "send as %s returned no event_id: %v", ghostID, result)
	return eventID
}

// ghostExists reports whether ghostID is registered on Synapse or has any membership in the room.
// Only a 404 from the profile lookup proves the ghost isn't registered; any other error is
// returned, so an unreachable Synapse can't pass for a missing ghost. It returns errors instead
// of taking t because require.Never runs its condition in another goroutine.
func ghostExists(env *harness.Env, bridged *harness.BridgedChannel, ghostID string) (bool, error) {
	_, err := env.Synapse.DoAsUser(bridged.MatrixUser, http.MethodGet, "/_matrix/client/v3/profile/"+url.PathEscape(ghostID), nil)
	if err == nil {
		return true, nil
	}
	if !strings.Contains(err.Error(), "matrix API error: 404") {
		return false, err
	}

	result, err := env.Synapse.DoAsUser(bridged.MatrixUser, http.MethodGet, "/_matrix/client/v3/rooms/"+url.PathEscape(bridged.RoomID)+"/members", nil)
	if err != nil {
		return false, err
	}
	response, _ := result.(map[string]any)
	members, _ := response["chunk"].([]any)
	for _, member := range members {
		if event, _ := member.(map[string]any); event["state_key"] == ghostID {
			return true, nil
		}
	}
	return false, nil
}

func TestLoopPreventionGhostSender(t *testing.T) {
	env, bridged := newEchoChannel(t)
	ghost := harness.GhostUserID(bridged.MattermostUser.Id)
	// /matrix map already joined the ghost; its event for this post proves it's registered and in
	// the room before the test impersonates it.
	postAndAwaitGhostEvent(t, env, bridged, "ghost setup "+model.NewId())

	message := "impersonated ghost " + model.NewId()
	eventID := sendAsGhost(t, env, bridged.RoomID, ghost, map[string]any{"msgtype": "m.text", "body": message})
	env.Synapse.WaitForRoomEvent(t, bridged.RoomID, func(e matrixtest.Event) bool {
		return e.EventID == eventID && e.Sender == ghost
	})
	requireProcessedBefore(t, env, bridged)

	harness.RequireNoPost(t, bridged.MattermostClient, bridged.Channel.Id, func(p *model.Post) bool {
		return p.Message == message
	}, echoWindow)
}

func TestLoopPreventionPostIDEcho(t *testing.T) {
	env, bridged := newEchoChannel(t)
	post, _ := postAndAwaitGhostEvent(t, env, bridged, "post ID echo target "+model.NewId())

	// No mattermost_remote_id, so the remote-ID check can't mask the post-ID check.
	message := "post ID echo " + model.NewId()
	env.Synapse.SendEventAsUser(t, bridged.MatrixUser, bridged.RoomID, "m.room.message", map[string]any{
		"msgtype":            "m.text",
		"body":               message,
		"mattermost_post_id": post.Id,
	})
	requireProcessedBefore(t, env, bridged)

	harness.RequireNoPost(t, bridged.MattermostClient, bridged.Channel.Id, func(p *model.Post) bool {
		return p.Message == message
	}, echoWindow)
}

func TestLoopPreventionRemoteIDEcho(t *testing.T) {
	env, bridged := newEchoChannel(t)

	// No mattermost_post_id, so the post-ID check can't mask the remote-ID check.
	message := "remote ID echo " + model.NewId()
	env.Synapse.SendEventAsUser(t, bridged.MatrixUser, bridged.RoomID, "m.room.message", map[string]any{
		"msgtype":              "m.text",
		"body":                 message,
		"mattermost_remote_id": env.RemoteID,
	})
	requireProcessedBefore(t, env, bridged)

	harness.RequireNoPost(t, bridged.MattermostClient, bridged.Channel.Id, func(p *model.Post) bool {
		return p.Message == message
	}, echoWindow)
}

func TestLoopPreventionRemoteUserNotGhosted(t *testing.T) {
	env, bridged := newEchoChannel(t)
	message := "remote user check " + model.NewId()
	env.Synapse.SendEventAsUser(t, bridged.MatrixUser, bridged.RoomID, "m.room.message", map[string]any{
		"msgtype": "m.text",
		"body":    message,
	})
	post := harness.WaitForPost(t, bridged.MattermostClient, bridged.Channel.Id, func(p *model.Post) bool {
		return p.Message == message
	})
	remoteUser, _, err := env.Admin.GetUser(t.Context(), post.UserId, "")
	require.NoError(t, err)
	require.Equal(t, env.RemoteID, remoteUser.GetRemoteID())

	// A profile update syncs a local user's ghost to Matrix. For a remote user core filters it
	// before the plugin, so no ghost may appear.
	name := "Loop " + model.NewId()
	_, err = env.Synapse.DoAsUser(bridged.MatrixUser, http.MethodPut,
		"/_matrix/client/v3/profile/"+url.PathEscape(bridged.MatrixUser.UserID)+"/displayname", map[string]any{"displayname": name})
	require.NoError(t, err)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		user, _, err := env.Admin.GetUser(t.Context(), remoteUser.Id, "")
		if assert.NoError(c, err) {
			assert.Equal(c, name, user.Nickname)
		}
	}, matrixtest.DefaultWaitTimeout, matrixtest.PollInterval, "profile change never reached user %s", remoteUser.Id)
	// A channel sync after the profile change, so the window follows real outbound traffic.
	postAndAwaitGhostEvent(t, env, bridged, "remote user sync "+model.NewId())

	ghost := harness.GhostUserID(remoteUser.Id)
	// Errors count as unknown here; the final check requires a real 404.
	require.Never(t, func() bool {
		exists, err := ghostExists(env, bridged, ghost)
		return err == nil && exists
	}, echoWindow, matrixtest.PollInterval, "ghost %s exists for remote user %s", ghost, remoteUser.Id)
	exists, err := ghostExists(env, bridged, ghost)
	require.NoError(t, err)
	require.False(t, exists, "ghost %s exists for remote user %s", ghost, remoteUser.Id)
}
