package main

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
	matrixtest "github.com/mattermost/mattermost-plugin-matrix-bridge/testcontainers/matrix"
)

// This file extends MultiServerIntegrationTestSuite with INBOUND (Matrix→Mattermost)
// data-flow coverage across two live homeservers: message create, edit, delete,
// reaction add/remove, file attachments, profile change, and own-ghost loop
// prevention — the counterparts to the outbound tests in
// multi_server_outbound_integration_test.go, in the same order. Events are injected
// through the full HTTP stack via putTransaction, and each per-event test runs
// against BOTH homeservers (bothServers) so a handler accidentally bound to one
// server cannot pass. Results are asserted by capturing Mattermost API mutations
// and reading per-server KV mappings.

// inboundServer identifies one homeserver endpoint for a parametrized inbound test.
type inboundServer struct {
	name     string
	serverID string
	c        *matrixtest.Container
	hsToken  string
}

// bothServers returns both configured homeservers so an inbound test can prove the
// same behavior for each (data flows in from A and from B, each in its own namespace).
func (suite *MultiServerIntegrationTestSuite) bothServers() []inboundServer {
	return []inboundServer{
		{name: "server A", serverID: multiServerAID, c: suite.containerA, hsToken: suite.containerA.HSToken},
		{name: "server B", serverID: multiServerBID, c: suite.containerB, hsToken: suite.containerB.HSToken},
	}
}

// seedRoomMapping maps a Matrix room to a Mattermost channel under a server's namespace.
func (suite *MultiServerIntegrationTestSuite) seedRoomMapping(serverID, roomID, channelID string) {
	suite.Require().NoError(suite.plugin.kvstore.Set(kvstore.BuildRoomMappingKey(serverID, roomID), []byte(channelID)))
}

// seedChannelMapping writes the forward channel→server mapping (the reverse of
// seedRoomMapping), which the redaction path reads via GetMatrixRoomID.
func (suite *MultiServerIntegrationTestSuite) seedChannelMapping(channelID, serverID, roomID string) {
	value, err := kvstore.MarshalChannelServerMappings([]kvstore.ChannelServerMapping{{ServerID: serverID, RoomID: roomID}})
	suite.Require().NoError(err)
	suite.Require().NoError(suite.plugin.kvstore.Set(kvstore.BuildChannelMappingKey(channelID), value))
}

// TestInboundMessageCreateRoutesToOwnServer is the definitive inbound routing check:
// with two live homeservers configured at once and two channels each bridged to a
// room on a different server, a message from each server lands in that server's
// channel and namespace only — and the same room presented with the wrong server's
// token creates nothing.
func (suite *MultiServerIntegrationTestSuite) TestInboundMessageCreateRoutesToOwnServer() {
	t := suite.T()

	channelA := model.NewId()
	channelB := model.NewId()
	require.NotEqual(t, channelA, channelB)

	userA := model.NewId()
	userB := model.NewId()
	senderA := "@sender_a:" + suite.containerA.ServerDomain
	senderB := "@sender_b:" + suite.containerB.ServerDomain
	roomA := "!create-room-a:" + suite.containerA.ServerDomain
	roomB := "!create-room-b:" + suite.containerB.ServerDomain
	const eventAID = "$create-event-a"
	const eventBID = "$create-event-b"

	suite.seedRoomMapping(multiServerAID, roomA, channelA)
	suite.seedRoomMapping(multiServerBID, roomB, channelB)
	require.NoError(t, suite.plugin.kvstore.Set(kvstore.BuildMatrixUserKey(multiServerAID, senderA), []byte(userA)))
	require.NoError(t, suite.plugin.kvstore.Set(kvstore.BuildMatrixUserKey(multiServerBID, senderB), []byte(userB)))

	suite.api.On("GetUser", mock.AnythingOfType("string")).Return(&model.User{Id: "u", Username: "sender"}, nil).Maybe()
	suite.api.On("GetChannel", mock.AnythingOfType("string")).Return(func(id string) *model.Channel { return &model.Channel{Id: id} }, nil).Maybe()

	createdPosts := map[string]*model.Post{}
	suite.api.On("CreatePost", mock.AnythingOfType("*model.Post")).Return(func(post *model.Post) *model.Post {
		if post.Id == "" {
			post.Id = model.NewId()
		}
		createdPosts[post.Id] = post
		return post
	}, nil).Maybe()

	msg := func(eventID, sender, roomID, body string) MatrixEvent {
		return MatrixEvent{Type: "m.room.message", EventID: eventID, Sender: sender, RoomID: roomID, Content: map[string]any{"msgtype": "m.text", "body": body}, Timestamp: 1}
	}

	require.Equal(t, http.StatusOK, suite.putTransaction(suite.containerA.HSToken, "txn-create-a", []MatrixEvent{msg(eventAID, senderA, roomA, "from A")}))
	require.Equal(t, http.StatusOK, suite.putTransaction(suite.containerB.HSToken, "txn-create-b", []MatrixEvent{msg(eventBID, senderB, roomB, "from B")}))

	postIDA, err := suite.plugin.kvstore.Get(kvstore.BuildMatrixEventPostKey(multiServerAID, eventAID))
	require.NoError(t, err)
	postIDB, err := suite.plugin.kvstore.Get(kvstore.BuildMatrixEventPostKey(multiServerBID, eventBID))
	require.NoError(t, err)
	require.NotEmpty(t, postIDA)
	require.NotEmpty(t, postIDB)
	postA := createdPosts[string(postIDA)]
	postB := createdPosts[string(postIDB)]
	require.NotNil(t, postA)
	require.NotNil(t, postB)

	// Positive: each message landed in its own server's channel.
	assert.Equal(t, channelA, postA.ChannelId, "server A message routes to channel A")
	assert.Equal(t, "from A", postA.Message)
	assert.Equal(t, channelB, postB.ChannelId, "server B message routes to channel B")
	assert.Equal(t, "from B", postB.Message)

	// Negative: neither event appears in the other server's namespace.
	crossA, err := suite.plugin.kvstore.Get(kvstore.BuildMatrixEventPostKey(multiServerBID, eventAID))
	require.NoError(t, err)
	assert.Empty(t, crossA, "server A's event must not appear under server B's namespace")
	crossB, err := suite.plugin.kvstore.Get(kvstore.BuildMatrixEventPostKey(multiServerAID, eventBID))
	require.NoError(t, err)
	assert.Empty(t, crossB, "server B's event must not appear under server A's namespace")

	// Wrong-token isolation: server A's room presented with server B's token is
	// unmapped in B's namespace, so no post is created.
	postCountBefore := len(createdPosts)
	require.Equal(t, http.StatusOK, suite.putTransaction(suite.containerB.HSToken, "txn-create-wrong", []MatrixEvent{msg("$create-event-wrong", senderA, roomA, "wrong token")}))
	assert.Equal(t, postCountBefore, len(createdPosts), "a room presented with the wrong server's token must not create a post")
}

// TestInboundMessageEditUpdatesPost verifies an inbound m.replace edit updates the
// mapped Mattermost post, on each homeserver.
func (suite *MultiServerIntegrationTestSuite) TestInboundMessageEditUpdatesPost() {
	t := suite.T()

	var updatedPost *model.Post
	suite.api.On("GetChannel", mock.AnythingOfType("string")).Return(func(id string) *model.Channel { return &model.Channel{Id: id} }, nil).Maybe()
	suite.api.On("GetPost", mock.AnythingOfType("string")).Return(func(id string) *model.Post { return &model.Post{Id: id, Message: "original"} }, nil).Maybe()
	suite.api.On("UpdatePost", mock.AnythingOfType("*model.Post")).Return(func(p *model.Post) *model.Post {
		updatedPost = p
		return p
	}, nil).Maybe()

	for _, srv := range suite.bothServers() {
		updatedPost = nil
		channelID := model.NewId()
		postID := model.NewId()
		origEventID := "$edit-orig-" + srv.serverID
		room := "!edit-room-" + srv.serverID + ":" + srv.c.ServerDomain
		editor := "@editor:" + srv.c.ServerDomain
		newBody := "edited on " + srv.name

		suite.seedRoomMapping(srv.serverID, room, channelID)
		require.NoError(t, suite.plugin.kvstore.Set(kvstore.BuildMatrixEventPostKey(srv.serverID, origEventID), []byte(postID)))
		require.NoError(t, suite.plugin.kvstore.Set(kvstore.BuildMatrixUserKey(srv.serverID, editor), []byte(model.NewId())))

		edit := MatrixEvent{
			Type:    "m.room.message",
			EventID: "$edit-event-" + srv.serverID,
			Sender:  editor,
			RoomID:  room,
			Content: map[string]any{
				"msgtype":       "m.text",
				"body":          "* " + newBody,
				"m.relates_to":  map[string]any{"rel_type": "m.replace", "event_id": origEventID},
				"m.new_content": map[string]any{"msgtype": "m.text", "body": newBody},
			},
			Timestamp: 3,
		}
		require.Equal(t, http.StatusOK, suite.putTransaction(srv.hsToken, "txn-edit-"+srv.serverID, []MatrixEvent{edit}))
		require.NotNilf(t, updatedPost, "%s edit should update the post", srv.name)
		assert.Equal(t, postID, updatedPost.Id, srv.name)
		assert.Equal(t, newBody, updatedPost.Message, srv.name)
	}
}

// TestInboundRedactionDeletesPost verifies that redacting a synced message deletes
// the corresponding Mattermost post, on each homeserver.
func (suite *MultiServerIntegrationTestSuite) TestInboundRedactionDeletesPost() {
	t := suite.T()

	var createdPost *model.Post
	var deletedPostID string
	suite.api.On("GetUser", mock.AnythingOfType("string")).Return(&model.User{Id: "u", Username: "redactor"}, nil).Maybe()
	suite.api.On("GetChannel", mock.AnythingOfType("string")).Return(func(id string) *model.Channel { return &model.Channel{Id: id} }, nil).Maybe()
	suite.api.On("CreatePost", mock.AnythingOfType("*model.Post")).Return(func(post *model.Post) *model.Post {
		if post.Id == "" {
			post.Id = model.NewId()
		}
		createdPost = post
		return post
	}, nil).Maybe()
	suite.api.On("DeletePost", mock.AnythingOfType("string")).Return(func(id string) *model.AppError {
		deletedPostID = id
		return nil
	}).Maybe()

	for _, srv := range suite.bothServers() {
		createdPost, deletedPostID = nil, ""
		channelID := model.NewId()
		room := "!redact-room-" + srv.serverID + ":" + srv.c.ServerDomain
		sender := "@redactor:" + srv.c.ServerDomain
		msgEventID := "$redact-msg-" + srv.serverID

		suite.seedRoomMapping(srv.serverID, room, channelID)
		suite.seedChannelMapping(channelID, srv.serverID, room) // redaction reads GetMatrixRoomID
		require.NoError(t, suite.plugin.kvstore.Set(kvstore.BuildMatrixUserKey(srv.serverID, sender), []byte(model.NewId())))

		// Sync a message so a post + event→post mapping exist.
		msg := MatrixEvent{Type: "m.room.message", EventID: msgEventID, Sender: sender, RoomID: room, Content: map[string]any{"msgtype": "m.text", "body": "delete me"}, Timestamp: 4}
		require.Equal(t, http.StatusOK, suite.putTransaction(srv.hsToken, "txn-redact-msg-"+srv.serverID, []MatrixEvent{msg}))
		require.NotNilf(t, createdPost, "%s message should create a post to delete", srv.name)

		// Redact it. The redacted event does not exist on the live homeserver, so
		// the handler falls back to treating it as a post deletion.
		redaction := MatrixEvent{Type: "m.room.redaction", EventID: "$redact-event-" + srv.serverID, Sender: sender, RoomID: room, Content: map[string]any{"redacts": msgEventID}, Timestamp: 5}
		require.Equal(t, http.StatusOK, suite.putTransaction(srv.hsToken, "txn-redact-"+srv.serverID, []MatrixEvent{redaction}))
		require.Eventuallyf(t, func() bool { return deletedPostID != "" }, 10*time.Second, 200*time.Millisecond, "%s redaction should delete the post", srv.name)
		assert.Equal(t, createdPost.Id, deletedPostID, srv.name)
	}
}

// TestInboundReactionAddRoutesToOwnServer verifies an inbound reaction resolves its
// target post and applies the Mattermost reaction, on each homeserver, scoped to
// that server's namespace.
func (suite *MultiServerIntegrationTestSuite) TestInboundReactionAddRoutesToOwnServer() {
	t := suite.T()

	var addedReaction *model.Reaction
	suite.api.On("GetUser", mock.AnythingOfType("string")).Return(&model.User{Id: "u", Username: "reactor"}, nil).Maybe()
	suite.api.On("GetChannel", mock.AnythingOfType("string")).Return(func(id string) *model.Channel { return &model.Channel{Id: id} }, nil).Maybe()
	suite.api.On("AddReaction", mock.AnythingOfType("*model.Reaction")).Return(func(r *model.Reaction) *model.Reaction {
		addedReaction = r
		return r
	}, nil).Maybe()

	for _, srv := range suite.bothServers() {
		addedReaction = nil
		channelID := model.NewId()
		postID := model.NewId()
		reactor := "@reactor:" + srv.c.ServerDomain
		room := "!reaction-room-" + srv.serverID + ":" + srv.c.ServerDomain
		targetEventID := "$reaction-target-" + srv.serverID
		reactionEventID := "$reaction-event-" + srv.serverID

		suite.seedRoomMapping(srv.serverID, room, channelID)
		require.NoError(t, suite.plugin.kvstore.Set(kvstore.BuildMatrixEventPostKey(srv.serverID, targetEventID), []byte(postID)))
		require.NoError(t, suite.plugin.kvstore.Set(kvstore.BuildMatrixUserKey(srv.serverID, reactor), []byte(model.NewId())))

		reaction := MatrixEvent{
			Type:      "m.reaction",
			EventID:   reactionEventID,
			Sender:    reactor,
			RoomID:    room,
			Content:   map[string]any{"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": targetEventID, "key": "👍"}},
			Timestamp: 2,
		}
		require.Equal(t, http.StatusOK, suite.putTransaction(srv.hsToken, "txn-reaction-"+srv.serverID, []MatrixEvent{reaction}))
		require.NotNilf(t, addedReaction, "%s reaction should be applied", srv.name)
		assert.Equal(t, postID, addedReaction.PostId, srv.name)

		stored, err := suite.plugin.kvstore.Get(kvstore.BuildMatrixReactionKey(srv.serverID, reactionEventID))
		require.NoError(t, err)
		assert.NotEmpty(t, stored, "%s reaction mapping should be stored under its own namespace", srv.name)
	}
}

// TestInboundReactionRemove verifies that redacting a Matrix reaction removes the
// corresponding Mattermost reaction, on each homeserver. It uses a real reaction
// event on the homeserver so the redaction handler can type it as m.reaction.
func (suite *MultiServerIntegrationTestSuite) TestInboundReactionRemove() {
	t := suite.T()

	var removedReaction *model.Reaction
	suite.api.On("GetChannel", mock.AnythingOfType("string")).Return(func(id string) *model.Channel { return &model.Channel{Id: id} }, nil).Maybe()
	suite.api.On("RemoveReaction", mock.AnythingOfType("*model.Reaction")).Return(func(r *model.Reaction) *model.AppError {
		removedReaction = r
		return nil
	}).Maybe()

	for _, srv := range suite.bothServers() {
		removedReaction = nil
		channelID := model.NewId()
		postID := model.NewId()
		reactorMMUserID := model.NewId()

		// A real room + message + reaction on this server (reaction sent as the AS
		// bot, which already belongs to the room it created).
		roomID := srv.c.CreateRoom(t, "react-rm-"+srv.serverID[:6]+"-"+model.NewId()[:8])
		msgEventID := srv.c.SendMessage(t, roomID, "target message")
		botUserID := srv.c.GetApplicationServiceBotUserID()
		reactionResp, err := srv.c.Client.SendReactionAsGhost(roomID, msgEventID, "👍", botUserID)
		require.NoError(t, err)
		reactionEventID := reactionResp.EventID
		require.NotEmpty(t, reactionEventID)

		suite.seedRoomMapping(srv.serverID, roomID, channelID)
		suite.seedChannelMapping(channelID, srv.serverID, roomID)
		reactionInfo, err := json.Marshal(map[string]string{"post_id": postID, "user_id": reactorMMUserID, "emoji_name": "thumbsup"})
		require.NoError(t, err)
		require.NoError(t, suite.plugin.kvstore.Set(kvstore.BuildMatrixReactionKey(srv.serverID, reactionEventID), reactionInfo))

		redaction := MatrixEvent{Type: "m.room.redaction", EventID: "$reaction-redact-" + srv.serverID, Sender: "@reactremover:" + srv.c.ServerDomain, RoomID: roomID, Content: map[string]any{"redacts": reactionEventID}, Timestamp: 10}
		require.Equal(t, http.StatusOK, suite.putTransaction(srv.hsToken, "txn-react-rm-"+srv.serverID, []MatrixEvent{redaction}))

		require.Eventuallyf(t, func() bool { return removedReaction != nil }, 10*time.Second, 200*time.Millisecond, "%s reaction redaction should remove the reaction", srv.name)
		assert.Equal(t, postID, removedReaction.PostId, srv.name)
		assert.Equal(t, reactorMMUserID, removedReaction.UserId, srv.name)
		assert.Equal(t, "thumbsup", removedReaction.EmojiName, srv.name)

		stored, err := suite.plugin.kvstore.Get(kvstore.BuildMatrixReactionKey(srv.serverID, reactionEventID))
		require.NoError(t, err)
		assert.Empty(t, stored, "%s reaction mapping should be deleted after removal", srv.name)
	}
}

// TestInboundFileAttachmentCreatesPost verifies an inbound file message downloads
// the media from the originating server and creates a Mattermost post carrying the
// uploaded file, on each homeserver.
func (suite *MultiServerIntegrationTestSuite) TestInboundFileAttachmentCreatesPost() {
	t := suite.T()

	var createdPost *model.Post
	var uploadedBytes []byte
	var uploadedFileID string
	suite.api.On("GetUser", mock.AnythingOfType("string")).Return(&model.User{Id: "u", Username: "filesender"}, nil).Maybe()
	suite.api.On("GetChannel", mock.AnythingOfType("string")).Return(func(id string) *model.Channel { return &model.Channel{Id: id} }, nil).Maybe()
	suite.api.On("UploadFile", mock.AnythingOfType("[]uint8"), mock.AnythingOfType("string"), "doc.txt").Return(func(data []byte, _ string, name string) *model.FileInfo {
		uploadedBytes = data
		uploadedFileID = model.NewId()
		return &model.FileInfo{Id: uploadedFileID, Name: name}
	}, nil).Maybe()
	suite.api.On("CreatePost", mock.AnythingOfType("*model.Post")).Return(func(post *model.Post) *model.Post {
		if post.Id == "" {
			post.Id = model.NewId()
		}
		createdPost = post
		return post
	}, nil).Maybe()

	for _, srv := range suite.bothServers() {
		createdPost, uploadedBytes, uploadedFileID = nil, nil, ""
		channelID := model.NewId()
		mmUserID := model.NewId()
		sender := "@filesender:" + srv.c.ServerDomain
		room := "!file-room-" + srv.serverID + ":" + srv.c.ServerDomain

		fileContents := []byte("inbound file on " + srv.name)
		mxcURI, err := srv.c.Client.UploadMedia(fileContents, "doc.txt", "text/plain")
		require.NoError(t, err)
		require.NotEmpty(t, mxcURI)

		suite.seedRoomMapping(srv.serverID, room, channelID)
		require.NoError(t, suite.plugin.kvstore.Set(kvstore.BuildMatrixUserKey(srv.serverID, sender), []byte(mmUserID)))

		fileEvent := MatrixEvent{Type: "m.room.message", EventID: "$file-event-" + srv.serverID, Sender: sender, RoomID: room, Content: map[string]any{"msgtype": "m.file", "body": "doc.txt", "url": mxcURI}, Timestamp: 6}
		require.Equal(t, http.StatusOK, suite.putTransaction(srv.hsToken, "txn-file-"+srv.serverID, []MatrixEvent{fileEvent}))

		require.NotNilf(t, createdPost, "%s file message should create a post", srv.name)
		assert.Equal(t, []string{uploadedFileID}, []string(createdPost.FileIds), "%s post should carry the uploaded file", srv.name)
		assert.Equal(t, fileContents, uploadedBytes, "%s file should be uploaded verbatim", srv.name)

		mapping, err := suite.plugin.kvstore.Get(kvstore.BuildMatrixEventPostKey(srv.serverID, fileEvent.EventID))
		require.NoError(t, err)
		assert.Equal(t, createdPost.Id, string(mapping), srv.name)
	}
}

// TestInboundProfileChangeUpdatesUser verifies an inbound m.room.member join carrying
// a new display name updates the mapped Mattermost user, on each homeserver.
func (suite *MultiServerIntegrationTestSuite) TestInboundProfileChangeUpdatesUser() {
	t := suite.T()

	var updatedUser *model.User
	suite.api.On("GetChannel", mock.AnythingOfType("string")).Return(func(id string) *model.Channel { return &model.Channel{Id: id} }, nil).Maybe()
	suite.api.On("UpdateUser", mock.AnythingOfType("*model.User")).Return(func(u *model.User) *model.User {
		updatedUser = u
		return u
	}, nil).Maybe()

	for _, srv := range suite.bothServers() {
		updatedUser = nil
		channelID := model.NewId()
		mmUserID := model.NewId()
		sender := "@profileuser:" + srv.c.ServerDomain
		room := "!profile-room-" + srv.serverID + ":" + srv.c.ServerDomain
		newNick := "New Nick " + srv.name

		suite.seedRoomMapping(srv.serverID, room, channelID)
		require.NoError(t, suite.plugin.kvstore.Set(kvstore.BuildMatrixUserKey(srv.serverID, sender), []byte(mmUserID)))
		suite.api.On("GetUser", mmUserID).Return(&model.User{Id: mmUserID, Username: "profileuser", Nickname: "Old Nick"}, nil).Maybe()
		// Already a channel member, so ensureUserInChannel is a no-op.
		suite.api.On("GetChannelMember", channelID, mmUserID).Return(&model.ChannelMember{ChannelId: channelID, UserId: mmUserID}, nil).Maybe()

		member := MatrixEvent{Type: "m.room.member", EventID: "$member-profile-" + srv.serverID, Sender: sender, RoomID: room, Content: map[string]any{"membership": "join", "displayname": newNick}, Timestamp: 9}
		require.Equal(t, http.StatusOK, suite.putTransaction(srv.hsToken, "txn-profile-"+srv.serverID, []MatrixEvent{member}))

		require.NotNilf(t, updatedUser, "%s profile change should update the user", srv.name)
		assert.Equal(t, mmUserID, updatedUser.Id, srv.name)
		assert.Equal(t, newNick, updatedUser.Nickname, srv.name)
	}
}

// TestInboundOwnGhostEventIgnored verifies that an inbound event sent by one of the
// plugin's own ghost users is ignored (loop prevention), scoped to each server's
// domain, while a genuine Matrix user's event still posts.
func (suite *MultiServerIntegrationTestSuite) TestInboundOwnGhostEventIgnored() {
	t := suite.T()

	var createdPost *model.Post
	suite.api.On("GetChannel", mock.AnythingOfType("string")).Return(func(id string) *model.Channel { return &model.Channel{Id: id} }, nil).Maybe()
	suite.api.On("CreatePost", mock.AnythingOfType("*model.Post")).Return(func(post *model.Post) *model.Post {
		if post.Id == "" {
			post.Id = model.NewId()
		}
		createdPost = post
		return post
	}, nil).Maybe()

	for _, srv := range suite.bothServers() {
		createdPost = nil
		channelID := model.NewId()
		room := "!ghost-room-" + srv.serverID + ":" + srv.c.ServerDomain
		// A ghost user is named with this server's Matrix ServerName domain.
		ghostSender := "@_mattermost_" + model.NewId() + ":" + srv.c.ServerDomain

		suite.seedRoomMapping(srv.serverID, room, channelID)

		ghostEvent := MatrixEvent{Type: "m.room.message", EventID: "$ghost-event-" + srv.serverID, Sender: ghostSender, RoomID: room, Content: map[string]any{"msgtype": "m.text", "body": "echo from our own ghost"}, Timestamp: 7}
		require.Equal(t, http.StatusOK, suite.putTransaction(srv.hsToken, "txn-ghost-"+srv.serverID, []MatrixEvent{ghostEvent}))
		assert.Nilf(t, createdPost, "%s: an event from our own ghost must not be re-imported", srv.name)

		// Control: a genuine (non-ghost) Matrix user in the same room does post.
		realSender := "@realuser:" + srv.c.ServerDomain
		realMMUserID := model.NewId()
		require.NoError(t, suite.plugin.kvstore.Set(kvstore.BuildMatrixUserKey(srv.serverID, realSender), []byte(realMMUserID)))
		suite.api.On("GetUser", realMMUserID).Return(&model.User{Id: realMMUserID, Username: "realuser"}, nil).Maybe()

		realEvent := MatrixEvent{Type: "m.room.message", EventID: "$real-event-" + srv.serverID, Sender: realSender, RoomID: room, Content: map[string]any{"msgtype": "m.text", "body": "hello from a real user"}, Timestamp: 8}
		require.Equal(t, http.StatusOK, suite.putTransaction(srv.hsToken, "txn-real-"+srv.serverID, []MatrixEvent{realEvent}))
		require.NotNilf(t, createdPost, "%s: a genuine Matrix user's message should create a post", srv.name)
	}
}
