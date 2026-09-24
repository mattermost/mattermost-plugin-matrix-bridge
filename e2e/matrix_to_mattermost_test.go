package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/e2e/harness"
	matrixtest "github.com/mattermost/mattermost-plugin-matrix-bridge/testcontainers/matrix"
)

// defaultUsernamePrefix mirrors servers.DefaultUsernamePrefix, the plugin's prefix for
// provisioned Matrix users when the server has none configured.
const defaultUsernamePrefix = "matrix"

func TestMatrixToMattermostTextMessage(t *testing.T) {
	env := harness.Shared(t)
	bridged := harness.NewBridgedChannel(t)
	message := "plain text " + model.NewId()

	eventID := sendText(t, bridged.MatrixUser, bridged.RoomID, message)
	post := waitForPostContaining(t, bridged, eventID, message)
	require.Equal(t, message, post.Message, "message of post %s", post.Id)

	author, _, err := env.Admin.GetUser(t.Context(), post.UserId, "")
	require.NoError(t, err, "get author %s of post %s", post.UserId, post.Id)
	require.True(t, author.IsRemote(), "author %s of post %s is not a remote user", author.Id, post.Id)
	require.Equal(t, env.RemoteID, author.GetRemoteID(), "remote ID of author %s", author.Id)
	require.Equal(t, provisionedUser(t, bridged.MatrixUser).Id, author.Id, "author of post %s", post.Id)

	require.Equal(t, true, post.GetProp("from_matrix"), "from_matrix prop of post %s", post.Id)
	eventIDKey := "matrix_event_id_" + pluginServer(t).EventDomain
	require.Equal(t, eventID, post.GetProp(eventIDKey), "%s prop of post %s", eventIDKey, post.Id)
}

func TestMatrixToMattermostHTMLFormatting(t *testing.T) {
	harness.Shared(t)
	bridged := harness.NewBridgedChannel(t)
	id := model.NewId()

	eventID := sendMessage(t, bridged.MatrixUser, bridged.RoomID, map[string]any{
		"msgtype": "m.text",
		"body":    "formatted " + id + " bold italic code link first second",
		"format":  "org.matrix.custom.html",
		"formatted_body": "<p>formatted " + id + "</p>" +
			"<p><strong>bold</strong> <em>italic</em> <code>code</code> <a href=\"https://example.com/" + id + "\">link</a></p>" +
			"<ul><li>first</li><li>second</li></ul>",
	})

	post := waitForPostContaining(t, bridged, eventID, id)
	want := "formatted " + id + "\n\n**bold** *italic* `code` [link](https://example.com/" + id + ")\n\n- first\n- second"
	require.Equal(t, want, post.Message, "message of post %s", post.Id)
}

func TestMatrixToMattermostMentions(t *testing.T) {
	harness.Shared(t)
	bridged := harness.NewBridgedChannel(t)
	other := joinBridgedRoom(t, bridged)
	otherUser := provisionedUser(t, other)
	ghost := harness.GhostUserID(bridged.MattermostUser.Id)
	id := model.NewId()

	eventID := sendMessage(t, bridged.MatrixUser, bridged.RoomID, map[string]any{
		"msgtype": "m.text",
		"body":    "mentions " + id + " Ghost and Other",
		"format":  "org.matrix.custom.html",
		"formatted_body": "mentions " + id +
			` <a href="https://matrix.to/#/` + ghost + `">Ghost</a>` +
			` and <a href="https://matrix.to/#/` + other.UserID + `">Other</a>`,
		"m.mentions": map[string]any{"user_ids": []string{ghost, other.UserID}},
	})

	post := waitForPostContaining(t, bridged, eventID, id)
	want := "mentions " + id + " @" + bridged.MattermostUser.Username + " and @" + otherUser.Username
	require.Equal(t, want, post.Message, "message of post %s", post.Id)
}

func TestMatrixToMattermostReplies(t *testing.T) {
	harness.Shared(t)
	bridged := harness.NewBridgedChannel(t)
	id := model.NewId()

	rootEventID := sendText(t, bridged.MatrixUser, bridged.RoomID, "root "+id)
	root := waitForPostContaining(t, bridged, rootEventID, "root "+id)

	threadEventID := sendMessage(t, bridged.MatrixUser, bridged.RoomID, map[string]any{
		"msgtype":      "m.text",
		"body":         "thread reply " + id,
		"m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": rootEventID},
	})
	threadReply := waitForPostContaining(t, bridged, threadEventID, "thread reply "+id)
	require.Equal(t, root.Id, threadReply.RootId, "root of post %s from m.thread event %s", threadReply.Id, threadEventID)

	// An m.in_reply_to reply without m.thread becomes a thread reply under its parent's root
	// post, not a quote.
	replyEventID := sendMessage(t, bridged.MatrixUser, bridged.RoomID, map[string]any{
		"msgtype":      "m.text",
		"body":         "plain reply " + id,
		"m.relates_to": map[string]any{"m.in_reply_to": map[string]any{"event_id": threadEventID}},
	})
	reply := waitForPostContaining(t, bridged, replyEventID, "plain reply "+id)
	require.Equal(t, root.Id, reply.RootId, "root of post %s from m.in_reply_to event %s", reply.Id, replyEventID)
}

func TestMatrixToMattermostEdit(t *testing.T) {
	env := harness.Shared(t)
	bridged := harness.NewBridgedChannel(t)
	id := model.NewId()
	original := "original " + id
	edited := "edited " + id

	eventID := sendText(t, bridged.MatrixUser, bridged.RoomID, original)
	post := waitForPostContaining(t, bridged, eventID, original)

	editEventID := sendMessage(t, bridged.MatrixUser, bridged.RoomID, map[string]any{
		"msgtype":       "m.text",
		"body":          "* " + edited,
		"m.new_content": map[string]any{"msgtype": "m.text", "body": edited},
		"m.relates_to":  map[string]any{"rel_type": "m.replace", "event_id": eventID},
	})

	require.EventuallyWithT(t, func(c *assert.CollectT) {
		got, _, err := env.Admin.GetPost(t.Context(), post.Id, "")
		if !assert.NoError(c, err) {
			return
		}
		assert.Equal(c, edited, got.Message)
		assert.Positive(c, got.EditAt)
	}, matrixtest.DefaultWaitTimeout, matrixtest.PollInterval,
		"post %s in channel %s never showed edit event %s of event %s in room %s", post.Id, bridged.Channel.Id, editEventID, eventID, bridged.RoomID)
}

func TestMatrixToMattermostReactions(t *testing.T) {
	env := harness.Shared(t)
	bridged := harness.NewBridgedChannel(t)

	targets := []struct {
		name string
		post func(t *testing.T) (post *model.Post, eventID string)
	}{
		{"matrix post", func(t *testing.T) (*model.Post, string) {
			message := "react to matrix " + model.NewId()
			eventID := sendText(t, bridged.MatrixUser, bridged.RoomID, message)
			return waitForPostContaining(t, bridged, eventID, message), eventID
		}},
		{"mattermost post", func(t *testing.T) (*model.Post, string) {
			post, _, err := bridged.MattermostClient.CreatePost(t.Context(), &model.Post{
				ChannelId: bridged.Channel.Id,
				Message:   "react to mattermost " + model.NewId(),
			})
			require.NoError(t, err, "create post in channel %s", bridged.Channel.Id)
			// The plugin finds a Mattermost-originated post through this content key.
			event := env.Synapse.WaitForRoomEvent(t, bridged.RoomID, func(e matrixtest.Event) bool {
				return e.Content["mattermost_post_id"] == post.Id
			})
			return post, event.EventID
		}},
	}

	for _, target := range targets {
		t.Run(target.name, func(t *testing.T) {
			post, eventID := target.post(t)

			reactionEventID := env.Synapse.SendEventAsUser(t, bridged.MatrixUser, bridged.RoomID, "m.reaction", map[string]any{
				"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": eventID, "key": "👍"},
			})
			reactor := provisionedUser(t, bridged.MatrixUser)

			// +1 and thumbsup share an emoji, and the plugin may map 👍 to either name.
			require.EventuallyWithT(t, func(c *assert.CollectT) {
				reactions, _, err := env.Admin.GetReactions(t.Context(), post.Id)
				if !assert.NoError(c, err) {
					return
				}
				assert.True(c, slices.ContainsFunc(reactions, func(r *model.Reaction) bool {
					return r.UserId == reactor.Id && (r.EmojiName == "+1" || r.EmojiName == "thumbsup")
				}), "reactions on post %s: %+v", post.Id, reactions)
			}, matrixtest.DefaultWaitTimeout, matrixtest.PollInterval,
				"reaction event %s on event %s in room %s by %s never reached post %s in channel %s",
				reactionEventID, eventID, bridged.RoomID, reactor.Id, post.Id, bridged.Channel.Id)

			harness.RedactAsUser(t, bridged.MatrixUser, bridged.RoomID, reactionEventID)

			require.EventuallyWithT(t, func(c *assert.CollectT) {
				reactions, _, err := env.Admin.GetReactions(t.Context(), post.Id)
				if !assert.NoError(c, err) {
					return
				}
				assert.False(c, slices.ContainsFunc(reactions, func(r *model.Reaction) bool {
					return r.UserId == reactor.Id
				}), "reactions on post %s: %+v", post.Id, reactions)
			}, matrixtest.DefaultWaitTimeout, matrixtest.PollInterval,
				"reaction by %s stayed on post %s after redacting event %s in room %s", reactor.Id, post.Id, reactionEventID, bridged.RoomID)
		})
	}
}

func TestMatrixToMattermostMessageDeletion(t *testing.T) {
	env := harness.Shared(t)
	bridged := harness.NewBridgedChannel(t)
	message := "delete me " + model.NewId()

	eventID := sendText(t, bridged.MatrixUser, bridged.RoomID, message)
	post := waitForPostContaining(t, bridged, eventID, message)

	harness.RedactAsUser(t, bridged.MatrixUser, bridged.RoomID, eventID)

	require.EventuallyWithT(t, func(c *assert.CollectT) {
		got, _, err := env.Admin.GetPostIncludeDeleted(t.Context(), post.Id, "")
		if assert.NoError(c, err) {
			assert.Positive(c, got.DeleteAt)
		}
	}, matrixtest.DefaultWaitTimeout, matrixtest.PollInterval,
		"post %s in channel %s was not deleted after redacting event %s in room %s", post.Id, bridged.Channel.Id, eventID, bridged.RoomID)
}

func sendText(t *testing.T, user *matrixtest.User, roomID, body string) string {
	t.Helper()
	return sendMessage(t, user, roomID, map[string]any{"msgtype": "m.text", "body": body})
}

func sendMessage(t *testing.T, user *matrixtest.User, roomID string, content map[string]any) string {
	t.Helper()
	return harness.Shared(t).Synapse.SendEventAsUser(t, user, roomID, "m.room.message", content)
}

func waitForPostContaining(t *testing.T, bridged *harness.BridgedChannel, eventID, text string) *model.Post {
	t.Helper()
	return waitForBridgedPost(t, bridged, eventID, func(p *model.Post) bool {
		return strings.Contains(p.Message, text)
	})
}

// waitForBridgedPost waits for the post that the Matrix event produced in the bridged channel. On
// timeout it logs the event and room, which harness.WaitForPost can't name.
func waitForBridgedPost(t *testing.T, bridged *harness.BridgedChannel, eventID string, match func(*model.Post) bool) *model.Post {
	t.Helper()
	defer func() {
		if t.Failed() {
			t.Logf("waited for the post from event %s in room %s", eventID, bridged.RoomID)
		}
	}()
	return harness.WaitForPost(t, bridged.MattermostClient, bridged.Channel.Id, match)
}

// provisionedUser waits for the Mattermost user that the plugin provisions for a Matrix user
// under the default username prefix.
func provisionedUser(t *testing.T, user *matrixtest.User) *model.User {
	t.Helper()
	return waitForUsername(t, defaultUsernamePrefix+":"+user.Username)
}

// waitForUsername looks the user up through POST /users/usernames: the plugin's usernames
// contain a colon.
func waitForUsername(t *testing.T, username string) *model.User {
	t.Helper()
	env := harness.Shared(t)
	var found *model.User
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		users, _, err := env.Admin.GetUsersByUsernames(t.Context(), []string{username})
		if assert.NoError(c, err) && assert.Len(c, users, 1) {
			found = users[0]
		}
	}, matrixtest.DefaultWaitTimeout, matrixtest.PollInterval, "Mattermost user %s was never provisioned", username)
	return found
}

// joinBridgedRoom registers a Matrix user, joins them to the bridged room with their own token,
// and waits until their provisioned user is a member of the channel.
func joinBridgedRoom(t *testing.T, bridged *harness.BridgedChannel) *matrixtest.User {
	t.Helper()
	env := harness.Shared(t)
	user := harness.NewMatrixUser(t)
	require.NoError(t, env.Synapse.JoinRoomAsUser(t, user.UserID, bridged.RoomID),
		"join %s to room %s", user.UserID, bridged.RoomID)
	requireChannelMember(t, bridged.Channel.Id, provisionedUser(t, user).Id)
	return user
}

func requireChannelMember(t *testing.T, channelID, userID string) {
	t.Helper()
	env := harness.Shared(t)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		_, _, err := env.Admin.GetChannelMember(t.Context(), channelID, userID, "")
		assert.NoError(c, err)
	}, matrixtest.DefaultWaitTimeout, matrixtest.PollInterval, "user %s never became a member of channel %s", userID, channelID)
}

// serverView is the part of GET /api/v1/servers that these tests read.
type serverView struct {
	ServerID    string `json:"server_id"`
	EventDomain string `json:"event_domain"`
}

func pluginServer(t *testing.T) serverView {
	t.Helper()
	env := harness.Shared(t)
	resp := harness.PluginRequest(t, env.Admin, http.MethodGet, "/api/v1/servers", nil)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "read server list")
	require.Equal(t, http.StatusOK, resp.StatusCode, "list servers: %s", body)

	var list struct {
		Servers []serverView `json:"servers"`
	}
	require.NoError(t, json.Unmarshal(body, &list), "decode server list: %s", body)
	i := slices.IndexFunc(list.Servers, func(s serverView) bool { return s.ServerID == env.ServerID })
	require.GreaterOrEqual(t, i, 0, "server %s not in server list", env.ServerID)
	return list.Servers[i]
}
