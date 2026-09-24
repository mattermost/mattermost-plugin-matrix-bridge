package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

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

func TestMatrixToMattermostFiles(t *testing.T) {
	env := harness.Shared(t)
	bridged := harness.NewBridgedChannel(t)

	// The plugin forwards only bytes and filename, so Mattermost derives MimeType from the
	// extension and decodes the PNG for its dimensions.
	files := []struct {
		msgtype, ext, mimetype string
		data                   []byte
		info                   map[string]any
	}{
		{"m.image", ".png", "image/png", solidPNG(t, 16, 16, color.RGBA{R: 200, G: 30, B: 90, A: 255}), map[string]any{"w": 16, "h": 16}},
		{"m.file", ".json", "application/json", []byte(`{"e2e":"` + model.NewId() + `"}`), nil},
		{"m.video", ".mp4", "video/mp4", []byte("not really a video " + model.NewId()), nil},
		{"m.audio", ".mp3", "audio/mpeg", []byte("not really audio " + model.NewId()), nil},
	}

	for _, f := range files {
		t.Run(f.msgtype, func(t *testing.T) {
			name := "e2e-" + model.NewId() + f.ext
			mxc := harness.UploadMediaAsUser(t, bridged.MatrixUser, name, f.mimetype, f.data)
			info := map[string]any{"mimetype": f.mimetype, "size": len(f.data)}
			maps.Copy(info, f.info)
			eventID := sendMessage(t, bridged.MatrixUser, bridged.RoomID, map[string]any{
				"msgtype": f.msgtype, "body": name, "url": mxc, "info": info,
			})

			post := waitForBridgedPost(t, bridged, eventID, func(p *model.Post) bool {
				return p.Metadata != nil && slices.ContainsFunc(p.Metadata.Files, func(fi *model.FileInfo) bool { return fi.Name == name })
			})
			require.Equal(t, provisionedUser(t, bridged.MatrixUser).Id, post.UserId, "author of post %s from event %s", post.Id, eventID)
			require.Len(t, post.FileIds, 1, "files on post %s from event %s", post.Id, eventID)
			fileID := post.FileIds[0]

			data, _, err := env.Admin.GetFile(t.Context(), fileID)
			require.NoError(t, err, "download file %s of post %s", fileID, post.Id)
			require.Equal(t, f.data, data, "bytes of file %s", fileID)

			fileInfo, _, err := env.Admin.GetFileInfo(t.Context(), fileID)
			require.NoError(t, err, "get info of file %s", fileID)
			require.Equal(t, name, fileInfo.Name, "name of file %s", fileID)
			require.Equal(t, f.mimetype, fileInfo.MimeType, "mimetype of file %s", fileID)
			if w, ok := f.info["w"]; ok {
				require.Equal(t, []any{w, f.info["h"]}, []any{fileInfo.Width, fileInfo.Height}, "dimensions of file %s", fileID)
			}
		})
	}
}

func solidPNG(t *testing.T, w, h int, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), &image.Uniform{C: c}, image.Point{}, draw.Src)
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

func TestMatrixToMattermostUserProvisioning(t *testing.T) {
	env := harness.Shared(t)
	bridged := harness.NewBridgedChannel(t)

	user := harness.NewMatrixUser(t)
	displayName := "Provisioned " + model.NewId()
	setProfileField(t, user, "displayname", displayName)
	require.NoError(t, env.Synapse.JoinRoomAsUser(t, user.UserID, bridged.RoomID), "join %s to room %s", user.UserID, bridged.RoomID)

	firstEventID := sendText(t, user, bridged.RoomID, "first "+model.NewId())
	first := waitForPostContaining(t, bridged, firstEventID, "first ")
	secondEventID := sendText(t, user, bridged.RoomID, "second "+model.NewId())
	second := waitForPostContaining(t, bridged, secondEventID, "second ")

	provisioned := provisionedUser(t, user)
	require.Equal(t, provisioned.Id, first.UserId, "author of post %s from event %s", first.Id, firstEventID)
	require.Equal(t, provisioned.Id, second.UserId, "author of post %s from event %s", second.Id, secondEventID)
	require.Equal(t, env.RemoteID, provisioned.GetRemoteID(), "remote ID of user %s", provisioned.Id)
	require.Equal(t, displayName, provisioned.Nickname, "nickname of user %s", provisioned.Id)
	firstName, lastName, _ := strings.Cut(displayName, " ")
	require.Equal(t, firstName, provisioned.FirstName, "first name of user %s", provisioned.Id)
	require.Equal(t, lastName, provisioned.LastName, "last name of user %s", provisioned.Id)

	duplicates, _, err := env.Admin.GetUsersByUsernames(t.Context(), []string{provisioned.Username + "_1"})
	require.NoError(t, err, "look up %s_1", provisioned.Username)
	require.Empty(t, duplicates, "a second user was provisioned for %s", user.UserID)

	t.Run("prefix change", func(t *testing.T) {
		original := pluginServer(t).UsernamePrefix
		prefix := "e2e" + model.NewId()[:6]
		// Registered before the PATCH: a request that times out may still have been applied.
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := patchUsernamePrefix(ctx, env, original); err != nil {
				t.Errorf("restore username prefix %q: %v", original, err)
			}
		})
		require.NoError(t, patchUsernamePrefix(t.Context(), env, prefix), "set username prefix %q", prefix)

		fresh := harness.NewMatrixUser(t)
		require.NoError(t, env.Synapse.JoinRoomAsUser(t, fresh.UserID, bridged.RoomID), "join %s to room %s", fresh.UserID, bridged.RoomID)
		eventID := sendText(t, fresh, bridged.RoomID, "prefixed "+model.NewId())
		post := waitForPostContaining(t, bridged, eventID, "prefixed ")
		prefixed := waitForUsername(t, prefix+":"+fresh.Username)
		require.Equal(t, prefixed.Id, post.UserId, "author of post %s from event %s", post.Id, eventID)
	})
}

// kickBanBug: member events are keyed by Sender instead of state_key, so a kick or ban removes the
// kicker instead of the target (a no-op if the kicker isn't provisioned), and fails on every retry
// when a provisioned kicker isn't a channel member.
const kickBanBug = "bug: member kick/ban uses Sender instead of state_key; a failed txn stalls AS delivery"

func TestMatrixToMattermostMembership(t *testing.T) {
	env := harness.Shared(t)
	bridged := harness.NewBridgedChannel(t)
	var member *matrixtest.User
	var memberID string

	t.Run("join new user", func(t *testing.T) {
		member = joinBridgedRoom(t, bridged)
		memberID = provisionedUser(t, member).Id
	})

	t.Run("leave", func(t *testing.T) {
		require.NotEmpty(t, memberID, "join subtest failed")
		matrixRoomAction(t, member, bridged.RoomID, "leave", map[string]any{})
		requireNotChannelMember(t, bridged.Channel.Id, memberID)
	})

	t.Run("join provisioned user", func(t *testing.T) {
		require.NotEmpty(t, memberID, "join subtest failed")
		require.NoError(t, env.Synapse.JoinRoomAsUser(t, member.UserID, bridged.RoomID), "rejoin %s to room %s", member.UserID, bridged.RoomID)
		requireChannelMember(t, bridged.Channel.Id, memberID)
	})

	for _, action := range []string{"kick", "ban"} {
		t.Run(action, func(t *testing.T) {
			t.Skip(kickBanBug)

			// The room creator has power level 100, so it can kick and ban. It must be a channel
			// member so the last assertion proves the kicker is kept, and because the plugin fails
			// every retry when removing a non-member kicker, which stalls Synapse delivery for
			// later tests.
			creator := bridged.MatrixUser
			body := "creator " + model.NewId()
			eventID := sendText(t, creator, bridged.RoomID, body)
			waitForPostContaining(t, bridged, eventID, body)
			creatorID := provisionedUser(t, creator).Id
			_, _, err := env.Admin.AddChannelMember(t.Context(), bridged.Channel.Id, creatorID)
			require.NoError(t, err, "add creator %s to channel %s", creatorID, bridged.Channel.Id)

			target := joinBridgedRoom(t, bridged)
			targetID := provisionedUser(t, target).Id
			matrixRoomAction(t, creator, bridged.RoomID, action, map[string]any{"user_id": target.UserID})
			requireNotChannelMember(t, bridged.Channel.Id, targetID)
			requireChannelMember(t, bridged.Channel.Id, creatorID)
		})
	}
}

func TestMatrixToMattermostProfileChange(t *testing.T) {
	env := harness.Shared(t)
	bridged := harness.NewBridgedChannel(t)
	user := joinBridgedRoom(t, bridged)
	userID := provisionedUser(t, user).Id

	t.Run("displayname", func(t *testing.T) {
		displayName := "Renamed " + model.NewId()
		setProfileField(t, user, "displayname", displayName)
		firstName, lastName, _ := strings.Cut(displayName, " ")
		require.EventuallyWithT(t, func(c *assert.CollectT) {
			got, _, err := env.Admin.GetUser(t.Context(), userID, "")
			if assert.NoError(c, err) {
				assert.Equal(c, displayName, got.Nickname)
				assert.Equal(c, firstName, got.FirstName)
				assert.Equal(c, lastName, got.LastName)
			}
		}, matrixtest.DefaultWaitTimeout, matrixtest.PollInterval,
			"user %s never took displayname %q of %s in room %s", userID, displayName, user.UserID, bridged.RoomID)
	})

	t.Run("avatar", func(t *testing.T) {
		avatarColor := color.RGBA{R: 17, G: 99, B: 201, A: 255}
		defaultCenter, err := profileImageCenter(t, userID)
		require.NoError(t, err)
		require.NotEqual(t, avatarColor, defaultCenter, "default profile image of user %s", userID)
		before, _, err := env.Admin.GetUser(t.Context(), userID, "")
		require.NoError(t, err, "get user %s", userID)

		mxc := harness.UploadMediaAsUser(t, user, "avatar-"+model.NewId()+".png", "image/png", solidPNG(t, 64, 64, avatarColor))
		setProfileField(t, user, "avatar_url", mxc)

		// Mattermost re-encodes profile images to a 128x128 PNG, so compare pixels, not bytes.
		// It writes the image before LastPictureUpdate, so both are polled together.
		require.EventuallyWithT(t, func(c *assert.CollectT) {
			center, err := profileImageCenter(t, userID)
			if assert.NoError(c, err) {
				assert.Equal(c, avatarColor, center)
			}
			after, _, err := env.Admin.GetUser(t.Context(), userID, "")
			if assert.NoError(c, err) {
				assert.Greater(c, after.LastPictureUpdate, before.LastPictureUpdate)
			}
		}, matrixtest.DefaultWaitTimeout, matrixtest.PollInterval,
			"user %s never took avatar %s of %s in room %s", userID, mxc, user.UserID, bridged.RoomID)
	})
}

func setProfileField(t *testing.T, user *matrixtest.User, field, value string) {
	t.Helper()
	path := "/_matrix/client/v3/profile/" + url.PathEscape(user.UserID) + "/" + field
	_, err := harness.Shared(t).Synapse.DoAsUser(user, http.MethodPut, path, map[string]any{field: value})
	require.NoError(t, err, "set %s of %s", field, user.UserID)
}

func matrixRoomAction(t *testing.T, user *matrixtest.User, roomID, action string, body map[string]any) {
	t.Helper()
	path := "/_matrix/client/v3/rooms/" + url.PathEscape(roomID) + "/" + action
	_, err := harness.Shared(t).Synapse.DoAsUser(user, http.MethodPost, path, body)
	require.NoError(t, err, "%s in room %s as %s: %v", action, roomID, user.UserID, body)
}

func requireNotChannelMember(t *testing.T, channelID, userID string) {
	t.Helper()
	env := harness.Shared(t)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		_, resp, err := env.Admin.GetChannelMember(t.Context(), channelID, userID, "")
		if assert.Error(c, err) && assert.NotNil(c, resp) {
			assert.Equal(c, http.StatusNotFound, resp.StatusCode)
		}
	}, matrixtest.DefaultWaitTimeout, matrixtest.PollInterval, "user %s stayed a member of channel %s", userID, channelID)
}

func profileImageCenter(t *testing.T, userID string) (color.RGBA, error) {
	data, _, err := harness.Shared(t).Admin.GetProfileImage(t.Context(), userID, "")
	if err != nil {
		return color.RGBA{}, fmt.Errorf("get profile image of user %s: %w", userID, err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return color.RGBA{}, fmt.Errorf("decode profile image of user %s: %w", userID, err)
	}
	if want := image.Rect(0, 0, 128, 128); img.Bounds() != want {
		return color.RGBA{}, fmt.Errorf("profile image of user %s is %v, want %v", userID, img.Bounds(), want)
	}
	return color.RGBAModel.Convert(img.At(64, 64)).(color.RGBA), nil
}

// patchUsernamePrefix is built by hand because cleanup can't use harness.PluginRequest, which
// sends with t.Context().
func patchUsernamePrefix(ctx context.Context, env *harness.Env, prefix string) error {
	body, err := json.Marshal(map[string]string{"username_prefix": prefix})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch,
		env.Admin.URL+"/plugins/"+harness.PluginID+"/api/v1/servers/"+url.PathEscape(env.ServerID), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set(model.HeaderAuth, model.HeaderBearer+" "+env.Admin.AuthToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.Admin.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("status %d: %s", resp.StatusCode, respBody)
	}
	return nil
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
	ServerID       string `json:"server_id"`
	EventDomain    string `json:"event_domain"`
	UsernamePrefix string `json:"username_prefix"`
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
