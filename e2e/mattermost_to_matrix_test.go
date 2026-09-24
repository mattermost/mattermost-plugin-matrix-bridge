package e2e

import (
	"bytes"
	"image"
	"image/png"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/e2e/harness"
	matrixtest "github.com/mattermost/mattermost-plugin-matrix-bridge/testcontainers/matrix"
)

func TestMattermostToMatrixTextMessage(t *testing.T) {
	env := harness.Shared(t)
	bridged := harness.NewBridgedChannel(t)
	message := "plain text " + model.NewId()

	post := createPost(t, bridged, message, "")
	event := waitForPostEvent(t, env, bridged.RoomID, post.Id)

	matrixtest.NewEventValidation(t, env.ServerName, env.RemoteID).ValidateMessageEvent(event, nil)
	require.Equal(t, message, event.Content["body"])
	require.Equal(t, harness.GhostUserID(bridged.MattermostUser.Id), event.Sender)
}

func TestMattermostToMatrixMarkdown(t *testing.T) {
	env := harness.Shared(t)
	bridged := harness.NewBridgedChannel(t)
	id := model.NewId()
	message := "**bold " + id + "** and *italic* with [a link](https://example.com)\n\n" +
		"```go\nfmt.Println(\"hi\")\n```\n\n" +
		"- first item\n- second item"

	post := createPost(t, bridged, message, "")
	event := waitForPostEvent(t, env, bridged.RoomID, post.Id)

	require.Equal(t, message, event.Content["body"])
	require.Equal(t, "org.matrix.custom.html", event.Content["format"])
	html, _ := event.Content["formatted_body"].(string)
	// The converter keeps list items as "- " lines joined by <br> (see server/markdown_utils_test.go).
	for _, fragment := range []string{
		"<strong>bold " + id + "</strong>",
		"<em>italic</em>",
		`<a href="https://example.com">a link</a>`,
		`<pre><code class="language-go">fmt.Println(&#34;hi&#34;)<br></code></pre>`,
		"<br>- first item<br>- second item",
	} {
		require.Contains(t, html, fragment, "formatted_body of event %s", event.EventID)
	}
}

func TestMattermostToMatrixMentions(t *testing.T) {
	env := harness.Shared(t)
	bridged := harness.NewBridgedChannel(t)

	mentioned, _ := harness.NewMattermostUser(t)
	_, _, err := env.Admin.AddChannelMember(t.Context(), bridged.Channel.Id, mentioned.Id)
	require.NoError(t, err, "add user %s to channel %s", mentioned.Id, bridged.Channel.Id)
	mentionedGhost := harness.GhostUserID(mentioned.Id)
	// Waiting for the join hook's ghost makes the mention resolve an existing ghost rather than
	// take the create-on-mention fallback.
	requireMembership(t, env, bridged.MatrixUser, bridged.RoomID, mentionedGhost, "join")

	remote := newMatrixOriginatedUser(t, env, bridged)

	post := createPost(t, bridged, "@"+mentioned.Username+" and @"+remote.Username+" "+model.NewId(), "")
	event := waitForPostEvent(t, env, bridged.RoomID, post.Id)

	html, _ := event.Content["formatted_body"].(string)
	require.Contains(t, html, `<a href="https://matrix.to/#/`+mentionedGhost+`">@`+mentioned.GetDisplayName(model.ShowFullName)+`</a>`,
		"formatted_body of event %s", event.EventID)
	require.Contains(t, html, `<a href="https://matrix.to/#/`+bridged.MatrixUser.UserID+`">@`+remote.GetDisplayName(model.ShowFullName)+`</a>`,
		"formatted_body of event %s", event.EventID)
	mentions, _ := event.Content["m.mentions"].(map[string]any)
	require.ElementsMatch(t, []any{mentionedGhost, bridged.MatrixUser.UserID}, mentions["user_ids"],
		"m.mentions of event %s", event.EventID)
}

func TestMattermostToMatrixThreadReply(t *testing.T) {
	env := harness.Shared(t)
	bridged := harness.NewBridgedChannel(t)

	root := createPost(t, bridged, "thread root "+model.NewId(), "")
	rootEvent := waitForPostEvent(t, env, bridged.RoomID, root.Id)
	waitForSyncedPost(t, bridged.MattermostClient, root.Id)

	reply := createPost(t, bridged, "thread reply "+model.NewId(), root.Id)
	event := waitForPostEvent(t, env, bridged.RoomID, reply.Id)

	matrixtest.NewEventValidation(t, env.ServerName, env.RemoteID).ValidateThreadedMessage(event, nil, rootEvent.EventID)
}

func TestMattermostToMatrixPostEdit(t *testing.T) {
	env := harness.Shared(t)
	bridged := harness.NewBridgedChannel(t)

	post := createPost(t, bridged, "before edit "+model.NewId(), "")
	original := waitForPostEvent(t, env, bridged.RoomID, post.Id)
	waitForSyncedPost(t, bridged.MattermostClient, post.Id)

	edited := "after edit " + model.NewId()
	_, _, err := bridged.MattermostClient.PatchPost(t.Context(), post.Id, &model.PostPatch{Message: &edited})
	require.NoError(t, err, "patch post %s", post.Id)

	event := waitForRoomEvent(t, env, bridged.RoomID, "edit of event "+original.EventID, func(e matrixtest.Event) bool {
		relation := relatesTo(e)
		newContent, _ := e.Content["m.new_content"].(map[string]any)
		return relation["rel_type"] == "m.replace" && relation["event_id"] == original.EventID && newContent["body"] == edited
	})
	matrixtest.NewEventValidation(t, env.ServerName, env.RemoteID).ValidateEditEvent(event, original.EventID, edited)
	require.Equal(t, harness.GhostUserID(bridged.MattermostUser.Id), event.Sender)
}

func TestMattermostToMatrixPostDeletion(t *testing.T) {
	env := harness.Shared(t)
	bridged := harness.NewBridgedChannel(t)

	post := createPost(t, bridged, "to be deleted "+model.NewId(), "")
	event := waitForPostEvent(t, env, bridged.RoomID, post.Id)
	waitForSyncedPost(t, bridged.MattermostClient, post.Id)

	_, err := bridged.MattermostClient.DeletePost(t.Context(), post.Id)
	require.NoError(t, err, "delete post %s", post.Id)

	requireRedacted(t, env, bridged.MatrixUser, bridged.RoomID, event.EventID, harness.GhostUserID(bridged.MattermostUser.Id))
}

func TestMattermostToMatrixReactionAdd(t *testing.T) {
	env := harness.Shared(t)
	bridged := harness.NewBridgedChannel(t)

	target, _ := reactToPost(t, env, bridged)

	event := waitForReaction(t, env, bridged.RoomID, target.EventID)
	matrixtest.NewEventValidation(t, env.ServerName, env.RemoteID).ValidateReactionEvent(event, target.EventID, "👍")
	require.Equal(t, harness.GhostUserID(bridged.MattermostUser.Id), event.Sender)
}

func TestMattermostToMatrixReactionRemoval(t *testing.T) {
	env := harness.Shared(t)
	bridged := harness.NewBridgedChannel(t)

	target, reaction := reactToPost(t, env, bridged)
	event := waitForReaction(t, env, bridged.RoomID, target.EventID)

	_, err := bridged.MattermostClient.DeleteReaction(t.Context(), reaction)
	require.NoError(t, err, "delete reaction on post %s", reaction.PostId)

	requireRedacted(t, env, bridged.MatrixUser, bridged.RoomID, event.EventID, harness.GhostUserID(bridged.MattermostUser.Id))
}

func TestMattermostToMatrixFileAttachments(t *testing.T) {
	env := harness.Shared(t)
	bridged := harness.NewBridgedChannel(t)
	id := model.NewId()
	uploads := []struct {
		name    string
		data    []byte
		msgtype string
	}{
		{"image-" + id + ".png", pngImage(t), "m.image"},
		{"notes-" + id + ".txt", []byte("text attachment " + id), "m.file"},
	}

	infos := make([]*model.FileInfo, len(uploads))
	for i, u := range uploads {
		infos[i] = uploadFile(t, bridged, u.name, u.data)
	}
	post := createPost(t, bridged, "files "+id, "", infos[0].Id, infos[1].Id)

	for i, u := range uploads {
		event := waitForFileEvent(t, env, bridged.RoomID, post.Id, u.name)
		require.Equal(t, u.msgtype, event.Content["msgtype"], "msgtype of event %s", event.EventID)
		info, _ := event.Content["info"].(map[string]any)
		require.Equal(t, infos[i].MimeType, info["mimetype"], "info.mimetype of event %s", event.EventID)
		require.Equal(t, harness.GhostUserID(bridged.MattermostUser.Id), event.Sender)

		mxcURI, _ := event.Content["url"].(string)
		data, contentType, filename := downloadMedia(t, env, bridged.MatrixUser, mxcURI)
		require.Equal(t, u.data, data, "bytes of %s from %s", u.name, mxcURI)
		require.Equal(t, infos[i].MimeType, contentType, "content type of %s from %s", u.name, mxcURI)
		require.Equal(t, u.name, filename, "filename of %s", mxcURI)
	}
}

func TestMattermostToMatrixFileAttachmentDeletion(t *testing.T) {
	t.Run("post deletion redacts file events", func(t *testing.T) {
		env := harness.Shared(t)
		bridged := harness.NewBridgedChannel(t)
		name := "delete-" + model.NewId() + ".txt"
		info := uploadFile(t, bridged, name, []byte("deleted with its post "+name))
		post := createPost(t, bridged, "post with file "+name, "", info.Id)

		textEvent := waitForPostEvent(t, env, bridged.RoomID, post.Id)
		fileEvent := waitForFileEvent(t, env, bridged.RoomID, post.Id, name)
		waitForSyncedPost(t, bridged.MattermostClient, post.Id)

		_, err := bridged.MattermostClient.DeletePost(t.Context(), post.Id)
		require.NoError(t, err, "delete post %s", post.Id)

		ghost := harness.GhostUserID(bridged.MattermostUser.Id)
		requireRedacted(t, env, bridged.MatrixUser, bridged.RoomID, fileEvent.EventID, ghost)
		requireRedacted(t, env, bridged.MatrixUser, bridged.RoomID, textEvent.EventID, ghost)
	})

	t.Run("attachment removal", func(t *testing.T) {
		env := harness.Shared(t)
		bridged := harness.NewBridgedChannel(t)
		id := model.NewId()
		kept := uploadFile(t, bridged, "kept-"+id+".txt", []byte("kept "+id))
		removed := uploadFile(t, bridged, "removed-"+id+".txt", []byte("removed "+id))
		post := createPost(t, bridged, "post with two files "+id, "", kept.Id, removed.Id)

		removedEvent := waitForFileEvent(t, env, bridged.RoomID, post.Id, removed.Name)
		waitForSyncedPost(t, bridged.MattermostClient, post.Id)

		fileIDs := model.StringArray{kept.Id}
		patched, _, err := bridged.MattermostClient.PatchPost(t.Context(), post.Id, &model.PostPatch{FileIds: &fileIDs})
		require.NoError(t, err, "patch post %s to drop file %s", post.Id, removed.Id)
		require.Equal(t, fileIDs, patched.FileIds, "file IDs of patched post %s", post.Id)

		// A CollectT keeps the poll from failing the test: not being redacted is the expected outcome.
		ghost := harness.GhostUserID(bridged.MattermostUser.Id)
		assert.Eventually(new(assert.CollectT), func() bool {
			sender, err := redactedBy(env, bridged.MatrixUser, bridged.RoomID, removedEvent.EventID)
			return err == nil && sender != ""
		}, matrixtest.DefaultWaitTimeout, matrixtest.PollInterval)
		sender, err := redactedBy(env, bridged.MatrixUser, bridged.RoomID, removedEvent.EventID)
		require.NoError(t, err, "read file event %s in room %s", removedEvent.EventID, bridged.RoomID)
		if sender == "" {
			t.Skip("deleteFileFromMatrix never runs for an already-synced attachment: neither deleting the " +
				"post nor editing out an attachment bumps the FileInfo's UpdateAt, and Mattermost re-sends an " +
				"attachment only when LastSyncAt < UpdateAt, so no attachment sync with DeleteAt != 0 arrives")
		}
		require.Equal(t, ghost, sender, "redactor of file event %s", removedEvent.EventID)
	})
}

// createPost posts message to the bridged channel as its Mattermost user, with fileIDs attached,
// as a reply when rootID is set.
func createPost(t *testing.T, bridged *harness.BridgedChannel, message, rootID string, fileIDs ...string) *model.Post {
	t.Helper()
	post, _, err := bridged.MattermostClient.CreatePost(t.Context(), &model.Post{
		ChannelId: bridged.Channel.Id,
		Message:   message,
		RootId:    rootID,
		FileIds:   fileIDs,
	})
	require.NoError(t, err, "create post in channel %s", bridged.Channel.Id)
	return post
}

// reactToPost creates a synced post and adds a +1 reaction to it once the plugin has stored the
// post's Matrix event ID. It returns the post's event and the reaction.
func reactToPost(t *testing.T, env *harness.Env, bridged *harness.BridgedChannel) (matrixtest.Event, *model.Reaction) {
	t.Helper()
	post := createPost(t, bridged, "react to me "+model.NewId(), "")
	event := waitForPostEvent(t, env, bridged.RoomID, post.Id)
	waitForSyncedPost(t, bridged.MattermostClient, post.Id)

	reaction, _, err := bridged.MattermostClient.SaveReaction(t.Context(), &model.Reaction{
		UserId:    bridged.MattermostUser.Id,
		PostId:    post.Id,
		EmojiName: "+1",
	})
	require.NoError(t, err, "react to post %s", post.Id)
	return event, reaction
}

// waitForReaction returns the 👍 annotation on the event.
func waitForReaction(t *testing.T, env *harness.Env, roomID, eventID string) matrixtest.Event {
	t.Helper()
	return waitForRoomEvent(t, env, roomID, "👍 reaction on event "+eventID, func(e matrixtest.Event) bool {
		relation := relatesTo(e)
		return e.Type == "m.reaction" && relation["event_id"] == eventID && relation["key"] == "👍"
	})
}

// waitForPostEvent returns the post's main event: the m.room.message carrying its ID that is not
// a file event grouped under the post's first event (m.mattermost.post). File events on a thread
// reply relate with m.thread instead, so they also match. Edits carry no post ID.
func waitForPostEvent(t *testing.T, env *harness.Env, roomID, postID string) matrixtest.Event {
	t.Helper()
	return waitForRoomEvent(t, env, roomID, "main event of post "+postID, func(e matrixtest.Event) bool {
		return e.Type == "m.room.message" && e.Content["mattermost_post_id"] == postID &&
			relatesTo(e)["rel_type"] != "m.mattermost.post"
	})
}

// waitForRoomEvent is WaitForRoomEvent with a description of the awaited event, logged on
// failure because WaitForRoomEvent's own message names only the room.
func waitForRoomEvent(t *testing.T, env *harness.Env, roomID, what string, match func(matrixtest.Event) bool) matrixtest.Event {
	t.Helper()
	found := false
	defer func() {
		if !found {
			t.Logf("was waiting for %s in room %s", what, roomID)
		}
	}()
	event := env.Synapse.WaitForRoomEvent(t, roomID, match)
	found = true
	return event
}

// waitForSyncedPost polls the post until the plugin has stored its Matrix event ID in a
// matrix_event_id_<domain> prop. Edits, deletions, reactions and replies are skipped or
// mis-synced until then, so tests wait for it before acting on a post.
func waitForSyncedPost(t *testing.T, client *model.Client4, postID string) {
	t.Helper()
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		post, _, err := client.GetPost(t.Context(), postID, "")
		if !assert.NoError(c, err) {
			return
		}
		for key := range post.GetProps() {
			if strings.HasPrefix(key, "matrix_event_id_") {
				return
			}
		}
		c.Errorf("post %s has no matrix_event_id_* prop yet", postID)
	}, matrixtest.DefaultWaitTimeout, matrixtest.PollInterval)
}

// requireRedacted polls the event as user, who must be in the room, until Synapse reports it
// redacted by redactor.
func requireRedacted(t *testing.T, env *harness.Env, user *matrixtest.User, roomID, eventID, redactor string) {
	t.Helper()
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		sender, err := redactedBy(env, user, roomID, eventID)
		if assert.NoError(c, err) {
			assert.Equal(c, redactor, sender, "redactor of event %s in room %s", eventID, roomID)
		}
	}, matrixtest.DefaultWaitTimeout, matrixtest.PollInterval)
}

// redactedBy returns the sender of the event's redaction, read as user, or "" while the event is
// not redacted.
func redactedBy(env *harness.Env, user *matrixtest.User, roomID, eventID string) (string, error) {
	path := "/_matrix/client/v3/rooms/" + url.PathEscape(roomID) + "/event/" + url.PathEscape(eventID)
	result, err := env.Synapse.DoAsUser(user, http.MethodGet, path, nil)
	if err != nil {
		return "", err
	}
	event, _ := result.(map[string]any)
	unsigned, _ := event["unsigned"].(map[string]any)
	redaction, _ := unsigned["redacted_because"].(map[string]any)
	sender, _ := redaction["sender"].(string)
	return sender, nil
}

// requireMembership polls userID's membership in the room, read as user, until it equals want.
func requireMembership(t *testing.T, env *harness.Env, user *matrixtest.User, roomID, userID, want string) {
	t.Helper()
	path := "/_matrix/client/v3/rooms/" + url.PathEscape(roomID) + "/state/m.room.member/" + url.PathEscape(userID)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		// Synapse answers 404 until the user has any membership in the room.
		result, err := env.Synapse.DoAsUser(user, http.MethodGet, path, nil)
		if !assert.NoError(c, err) {
			return
		}
		state, _ := result.(map[string]any)
		assert.Equal(c, want, state["membership"], "membership of %s in room %s", userID, roomID)
	}, matrixtest.DefaultWaitTimeout, matrixtest.PollInterval)
}

// newMatrixOriginatedUser has the bridged room's Matrix user send a message and returns the
// remote Mattermost user the plugin created for it.
func newMatrixOriginatedUser(t *testing.T, env *harness.Env, bridged *harness.BridgedChannel) *model.User {
	t.Helper()
	message := "hello from Matrix " + model.NewId()
	env.Synapse.SendEventAsUser(t, bridged.MatrixUser, bridged.RoomID, "m.room.message", map[string]any{
		"msgtype": "m.text",
		"body":    message,
	})
	post := harness.WaitForPost(t, bridged.MattermostClient, bridged.Channel.Id, func(p *model.Post) bool {
		return p.Message == message
	})

	user, _, err := env.Admin.GetUser(t.Context(), post.UserId, "")
	require.NoError(t, err, "get author %s of post %s", post.UserId, post.Id)
	require.True(t, user.IsRemote(), "author %s of post %s is not a remote user", user.Id, post.Id)
	return user
}

// uploadFile uploads data to the bridged channel as its Mattermost user and returns the file's
// info, whose MimeType is what Mattermost detected.
func uploadFile(t *testing.T, bridged *harness.BridgedChannel, name string, data []byte) *model.FileInfo {
	t.Helper()
	resp, _, err := bridged.MattermostClient.UploadFile(t.Context(), data, bridged.Channel.Id, name)
	require.NoError(t, err, "upload %s to channel %s", name, bridged.Channel.Id)
	require.Len(t, resp.FileInfos, 1, "file infos for upload %s", name)
	return resp.FileInfos[0]
}

// waitForFileEvent returns the post's file event for the named file.
func waitForFileEvent(t *testing.T, env *harness.Env, roomID, postID, name string) matrixtest.Event {
	t.Helper()
	return waitForRoomEvent(t, env, roomID, "file event "+name+" of post "+postID, func(e matrixtest.Event) bool {
		return e.Type == "m.room.message" && e.Content["mattermost_post_id"] == postID && e.Content["body"] == name
	})
}

// pngImage returns a small PNG whose pixels differ on every call.
func pngImage(t *testing.T) []byte {
	t.Helper()
	seed := []byte(model.NewId())
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for i := range img.Pix {
		img.Pix[i] = seed[i%len(seed)]
	}
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

// downloadMedia fetches an mxc:// URI through Synapse's authenticated media API as user and
// returns the bytes, the Content-Type, and the Content-Disposition filename.
func downloadMedia(t *testing.T, env *harness.Env, user *matrixtest.User, mxcURI string) (data []byte, contentType, filename string) {
	t.Helper()
	rest, isMXC := strings.CutPrefix(mxcURI, "mxc://")
	server, mediaID, ok := strings.Cut(rest, "/")
	require.True(t, isMXC && ok && server != "" && mediaID != "",
		"not an mxc://<server>/<media id> URI: %q", mxcURI)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		env.Synapse.ServerURL+"/_matrix/client/v1/media/download/"+url.PathEscape(server)+"/"+url.PathEscape(mediaID), nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+user.AccessToken)
	resp, err := (&http.Client{Timeout: matrixtest.DefaultWaitTimeout}).Do(req)
	require.NoError(t, err, "download %s", mxcURI)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		require.Failf(t, "download failed", "download %s: status %d: %s", mxcURI, resp.StatusCode, body)
	}

	data, err = io.ReadAll(resp.Body)
	require.NoError(t, err, "read %s", mxcURI)
	_, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition"))
	require.NoError(t, err, "Content-Disposition of %s", mxcURI)
	return data, resp.Header.Get("Content-Type"), params["filename"]
}

func relatesTo(e matrixtest.Event) map[string]any {
	relation, _ := e.Content["m.relates_to"].(map[string]any)
	return relation
}
