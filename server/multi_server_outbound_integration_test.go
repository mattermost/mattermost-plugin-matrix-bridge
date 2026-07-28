package main

import (
	"encoding/json"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
	matrixtest "github.com/mattermost/mattermost-plugin-matrix-bridge/testcontainers/matrix"
)

// This file extends MultiServerIntegrationTestSuite (defined in
// multi_server_integration_test.go) with outbound (Mattermost→Matrix) coverage
// across two live homeservers: message edit/delete, reactions, file attachments,
// cross-server loop prevention, profile-image fan-out, and DM routing by the
// remote participant's homeserver.

// serverRoom pairs a resolved room with the container that hosts it, for
// asserting the same outbound event landed on both servers.
type serverRoom struct {
	name     string
	serverID string
	roomID   string
	c        *matrixtest.Container
}

// mapChannelToBothServers creates a fresh room on each homeserver and maps the
// channel to both, so an outbound event for it fans out to A and B.
func (suite *MultiServerIntegrationTestSuite) mapChannelToBothServers(channelID, tag string) []serverRoom {
	t := suite.T()
	roomA := suite.containerA.CreateRoom(t, tag+"-a-"+model.NewId()[:8])
	roomB := suite.containerB.CreateRoom(t, tag+"-b-"+model.NewId()[:8])

	value, err := kvstore.MarshalChannelServerMappings([]kvstore.ChannelServerMapping{
		{ServerID: multiServerAID, RoomID: roomA},
		{ServerID: multiServerBID, RoomID: roomB},
	})
	require.NoError(t, err)
	require.NoError(t, suite.plugin.kvstore.Set(kvstore.BuildChannelMappingKey(channelID), value))
	require.NoError(t, suite.plugin.kvstore.Set(kvstore.BuildRoomMappingKey(multiServerAID, roomA), []byte(channelID)))
	require.NoError(t, suite.plugin.kvstore.Set(kvstore.BuildRoomMappingKey(multiServerBID, roomB), []byte(channelID)))

	return []serverRoom{
		{name: "server A", serverID: multiServerAID, roomID: roomA, c: suite.containerA},
		{name: "server B", serverID: multiServerBID, roomID: roomB, c: suite.containerB},
	}
}

// seedDistinctServerRemotes rewrites the registry so each server carries a
// distinct shared-channels remote ID and populates the plugin's remote-ID maps,
// which SetupTest leaves empty. Needed to exercise loop prevention and DM routing
// that key off per-server remote IDs.
func (suite *MultiServerIntegrationTestSuite) seedDistinctServerRemotes(ridA, ridB string) {
	servers := []kvstore.ServerConfig{
		{ServerID: multiServerAID, ServerURL: suite.containerA.ServerURL, ServerName: suite.containerA.ServerDomain, ASToken: suite.containerA.ASToken, HSToken: suite.containerA.HSToken, UsernamePrefix: "matrixa", Enabled: true, RemoteID: ridA},
		{ServerID: multiServerBID, ServerURL: suite.containerB.ServerURL, ServerName: suite.containerB.ServerDomain, ASToken: suite.containerB.ASToken, HSToken: suite.containerB.HSToken, UsernamePrefix: "matrixb", Enabled: true, RemoteID: ridB},
	}
	data, err := json.Marshal(servers)
	suite.Require().NoError(err)
	suite.Require().NoError(suite.plugin.kvstore.Set(kvstore.KeyServersConfig, data))

	suite.plugin.matrixClientsLock.Lock()
	suite.plugin.remoteToServerID = map[string]string{ridA: multiServerAID, ridB: multiServerBID}
	suite.plugin.ownRemoteIDs = map[string]struct{}{ridA: {}, ridB: {}}
	suite.plugin.matrixClientsLock.Unlock()
}

// mockLocalAuthor registers the API mocks needed to sync a post/reaction authored
// by a local Mattermost user through the outbound hooks.
func (suite *MultiServerIntegrationTestSuite) mockLocalAuthor(userID string) {
	suite.api.On("GetUser", userID).Return(&model.User{Id: userID, Username: "author_" + userID[:6], Email: userID + "@example.com", Nickname: "Author"}, nil).Maybe()
	suite.api.On("GetProfileImage", userID).Return([]byte("fake-image-data"), nil).Maybe()
	suite.api.On("UpdatePost", mock.AnythingOfType("*model.Post")).Return(func(post *model.Post) *model.Post {
		return post.Clone()
	}, nil).Maybe()
}

// createLocalPostOnBoth drives OnSharedChannelsSyncMsg with a fresh local post and
// waits until it lands on both servers, returning the post (now carrying each
// server's matrix_event_id_<domain> prop) and the per-server event IDs keyed by
// serverID.
func (suite *MultiServerIntegrationTestSuite) createLocalPostOnBoth(channelID, userID, message string, rooms []serverRoom) (*model.Post, map[string]string) {
	t := suite.T()
	post := &model.Post{
		Id:        model.NewId(),
		UserId:    userID,
		ChannelId: channelID,
		Message:   message,
		CreateAt:  time.Now().UnixMilli(),
	}
	_, err := suite.plugin.OnSharedChannelsSyncMsg(&model.SyncMsg{ChannelId: channelID, Posts: []*model.Post{post}}, nil)
	require.NoError(t, err)

	eventIDs := make(map[string]string, len(rooms))
	for _, r := range rooms {
		require.Eventuallyf(t, func() bool {
			ev := matrixtest.FindEventByPostID(r.c.GetRoomEvents(t, r.roomID), post.Id)
			if ev != nil {
				eventIDs[r.serverID] = ev.EventID
				return true
			}
			return false
		}, 15*time.Second, 300*time.Millisecond, "created post should reach %s", r.name)
	}
	return post, eventIDs
}

// findEditEvent returns the first m.room.message that replaces originalEventID.
func findEditEvent(events []matrixtest.Event, originalEventID string) *matrixtest.Event {
	for i := range events {
		if events[i].Type != "m.room.message" {
			continue
		}
		relatesTo, ok := events[i].Content["m.relates_to"].(map[string]any)
		if !ok {
			continue
		}
		if relatesTo["rel_type"] == "m.replace" && relatesTo["event_id"] == originalEventID {
			return &events[i]
		}
	}
	return nil
}

// findReactionEvent returns the first m.reaction annotating targetEventID.
func findReactionEvent(events []matrixtest.Event, targetEventID string) *matrixtest.Event {
	for i := range events {
		if events[i].Type != "m.reaction" {
			continue
		}
		relatesTo, ok := events[i].Content["m.relates_to"].(map[string]any)
		if !ok {
			continue
		}
		if relatesTo["event_id"] == targetEventID {
			return &events[i]
		}
	}
	return nil
}

// TestOutboundMessageSendFansOut verifies that a post in a channel shared with
// both homeservers is delivered to the mapped room on each, driven through the
// real OnSharedChannelsSyncMsg hook (channel-based server selection).
func (suite *MultiServerIntegrationTestSuite) TestOutboundMessageSendFansOut() {
	channelID := model.NewId()
	rooms := suite.mapChannelToBothServers(channelID, "send")
	userID := model.NewId()
	suite.mockLocalAuthor(userID)

	// createLocalPostOnBoth injects the post and asserts it reaches every server.
	post, eventIDs := suite.createLocalPostOnBoth(channelID, userID, "hello both servers", rooms)
	suite.Require().NotEmpty(post.Id)
	suite.Require().Len(eventIDs, len(rooms), "the post should be delivered to every mapped server")
}

// TestOutboundMessageEditFansOut verifies an edit to a post shared with both
// servers produces an m.replace edit in each server's room. This is the path
// where the per-server matrix_event_id_<domain> property key must be distinct
// (a collision previously sent server B an edit for a server-A event).
func (suite *MultiServerIntegrationTestSuite) TestOutboundMessageEditFansOut() {
	t := suite.T()

	channelID := model.NewId()
	rooms := suite.mapChannelToBothServers(channelID, "edit")
	userID := model.NewId()
	suite.mockLocalAuthor(userID)

	post, originalEventIDs := suite.createLocalPostOnBoth(channelID, userID, "original message", rooms)

	// Edit: same Id, changed message, a newer UpdateAt, carrying the props the
	// create populated (both servers' event IDs).
	editedPost := &model.Post{
		Id:        post.Id,
		UserId:    userID,
		ChannelId: channelID,
		Message:   "edited message content",
		CreateAt:  post.CreateAt,
		UpdateAt:  time.Now().UnixMilli() + 1,
		Props:     post.Props,
	}
	_, err := suite.plugin.OnSharedChannelsSyncMsg(&model.SyncMsg{ChannelId: channelID, Posts: []*model.Post{editedPost}}, nil)
	require.NoError(t, err)

	for _, r := range rooms {
		validator := matrixtest.NewEventValidation(t, r.c.ServerDomain, suite.plugin.remoteIDForServer(r.serverID))
		require.Eventuallyf(t, func() bool {
			return findEditEvent(r.c.GetRoomEvents(t, r.roomID), originalEventIDs[r.serverID]) != nil
		}, 15*time.Second, 300*time.Millisecond, "edit should reach %s", r.name)
		edit := findEditEvent(r.c.GetRoomEvents(t, r.roomID), originalEventIDs[r.serverID])
		validator.ValidateEditEvent(*edit, originalEventIDs[r.serverID], "edited message content")
	}
}

// TestOutboundMessageDeleteFansOut verifies deleting a post shared with both
// servers redacts it on each server.
func (suite *MultiServerIntegrationTestSuite) TestOutboundMessageDeleteFansOut() {
	t := suite.T()

	channelID := model.NewId()
	rooms := suite.mapChannelToBothServers(channelID, "delete")
	userID := model.NewId()
	suite.mockLocalAuthor(userID)

	post, _ := suite.createLocalPostOnBoth(channelID, userID, "to be deleted", rooms)

	deletedPost := post.Clone()
	deletedPost.DeleteAt = time.Now().UnixMilli()
	_, err := suite.plugin.OnSharedChannelsSyncMsg(&model.SyncMsg{ChannelId: channelID, Posts: []*model.Post{deletedPost}}, nil)
	require.NoError(t, err)

	for _, r := range rooms {
		require.Eventuallyf(t, func() bool {
			return matrixtest.FindEventByType(r.c.GetRoomEvents(t, r.roomID), "m.room.redaction") != nil
		}, 15*time.Second, 300*time.Millisecond, "delete should redact the message on %s", r.name)
		// The message content is gone after redaction, so it is no longer findable by post ID.
		assert.Nil(t, matrixtest.FindEventByPostID(r.c.GetRoomEvents(t, r.roomID), post.Id), "redacted post should not be findable on %s", r.name)
	}
}

// TestOutboundReactionFansOut verifies a reaction on a post shared with both
// servers is applied to the corresponding message on each server.
func (suite *MultiServerIntegrationTestSuite) TestOutboundReactionFansOut() {
	t := suite.T()

	channelID := model.NewId()
	rooms := suite.mapChannelToBothServers(channelID, "react")
	userID := model.NewId()
	suite.mockLocalAuthor(userID)

	post, eventIDs := suite.createLocalPostOnBoth(channelID, userID, "react to me", rooms)
	// The reaction path reads the post's per-server event-id props.
	suite.api.On("GetPost", post.Id).Return(post, nil).Maybe()

	reaction := &model.Reaction{UserId: userID, PostId: post.Id, EmojiName: "thumbsup", CreateAt: time.Now().UnixMilli()}
	_, err := suite.plugin.OnSharedChannelsSyncMsg(&model.SyncMsg{ChannelId: channelID, Reactions: []*model.Reaction{reaction}}, nil)
	require.NoError(t, err)

	for _, r := range rooms {
		validator := matrixtest.NewEventValidation(t, r.c.ServerDomain, suite.plugin.remoteIDForServer(r.serverID))
		require.Eventuallyf(t, func() bool {
			return findReactionEvent(r.c.GetRoomEvents(t, r.roomID), eventIDs[r.serverID]) != nil
		}, 15*time.Second, 300*time.Millisecond, "reaction should reach %s", r.name)
		got := findReactionEvent(r.c.GetRoomEvents(t, r.roomID), eventIDs[r.serverID])
		validator.ValidateReactionEvent(*got, eventIDs[r.serverID], "👍")
	}
}

// TestOutboundReactionRemoveFansOut verifies that removing a reaction (a reaction
// with DeleteAt set) redacts the corresponding Matrix reaction on every server the
// post's channel is mapped to.
func (suite *MultiServerIntegrationTestSuite) TestOutboundReactionRemoveFansOut() {
	t := suite.T()

	channelID := model.NewId()
	rooms := suite.mapChannelToBothServers(channelID, "react-rm")
	userID := model.NewId()
	suite.mockLocalAuthor(userID)

	post, eventIDs := suite.createLocalPostOnBoth(channelID, userID, "react then unreact", rooms)
	// The reaction add/remove paths read the post's per-server event-id props.
	suite.api.On("GetPost", post.Id).Return(post, nil).Maybe()

	// First add the reaction so there is a Matrix reaction event to redact.
	add := &model.Reaction{UserId: userID, PostId: post.Id, EmojiName: "thumbsup", CreateAt: time.Now().UnixMilli()}
	_, err := suite.plugin.OnSharedChannelsSyncMsg(&model.SyncMsg{ChannelId: channelID, Reactions: []*model.Reaction{add}}, nil)
	require.NoError(t, err)
	for _, r := range rooms {
		require.Eventuallyf(t, func() bool {
			return findReactionEvent(r.c.GetRoomEvents(t, r.roomID), eventIDs[r.serverID]) != nil
		}, 15*time.Second, 300*time.Millisecond, "reaction should first be applied on %s", r.name)
	}

	// Now remove it (DeleteAt set) — this must redact the reaction on both servers.
	remove := &model.Reaction{UserId: userID, PostId: post.Id, EmojiName: "thumbsup", CreateAt: add.CreateAt, DeleteAt: time.Now().UnixMilli()}
	_, err = suite.plugin.OnSharedChannelsSyncMsg(&model.SyncMsg{ChannelId: channelID, Reactions: []*model.Reaction{remove}}, nil)
	require.NoError(t, err)
	for _, r := range rooms {
		require.Eventuallyf(t, func() bool {
			// After redaction the reaction event's content is stripped, so it no
			// longer annotates the target event.
			return findReactionEvent(r.c.GetRoomEvents(t, r.roomID), eventIDs[r.serverID]) == nil
		}, 15*time.Second, 300*time.Millisecond, "reaction should be removed on %s", r.name)
	}
}

// TestOutboundAttachmentFansOut verifies a file attachment is uploaded to each
// server (a distinct mxc URI per homeserver) and attached to the post's message
// in both rooms.
func (suite *MultiServerIntegrationTestSuite) TestOutboundAttachmentFansOut() {
	t := suite.T()

	channelID := model.NewId()
	rooms := suite.mapChannelToBothServers(channelID, "file")
	userID := model.NewId()
	suite.mockLocalAuthor(userID)

	fileID := model.NewId()
	fi := &model.FileInfo{Id: fileID, Name: "hello.txt", MimeType: "text/plain", Size: 11}
	suite.api.On("GetFile", fileID).Return([]byte("hello world"), nil).Maybe()

	post := &model.Post{
		Id:        model.NewId(),
		UserId:    userID,
		ChannelId: channelID,
		Message:   "see attachment",
		CreateAt:  time.Now().UnixMilli(),
		FileIds:   []string{fileID},
	}

	// Attachment sync happens first (uploads per server, stores a pending file per
	// (serverID, postID)), then the post sync attaches it in each room.
	require.NoError(t, suite.plugin.OnSharedChannelsAttachmentSyncMsg(fi, post, nil))
	_, err := suite.plugin.OnSharedChannelsSyncMsg(&model.SyncMsg{ChannelId: channelID, Posts: []*model.Post{post}}, nil)
	require.NoError(t, err)

	mxcByServer := make(map[string]string, len(rooms))
	for _, r := range rooms {
		validator := matrixtest.NewEventValidation(t, r.c.ServerDomain, suite.plugin.remoteIDForServer(r.serverID))
		var fileEvent *matrixtest.Event
		require.Eventuallyf(t, func() bool {
			fileEvent = matrixtest.FindEventByType(r.c.GetRoomEvents(t, r.roomID), "m.room.message")
			for _, ev := range matrixtest.FindEventsByType(r.c.GetRoomEvents(t, r.roomID), "m.room.message") {
				if ev.Content["body"] == fi.Name {
					e := ev
					fileEvent = &e
					return true
				}
			}
			return false
		}, 15*time.Second, 300*time.Millisecond, "file message should reach %s", r.name)
		validator.ValidateFileMessage(*fileEvent, fi.Name, fi.MimeType)
		url, _ := fileEvent.Content["url"].(string)
		assert.NotEmpty(t, url, "file message should carry an mxc URL on %s", r.name)
		mxcByServer[r.serverID] = url
	}

	// The mxc URI is only valid on the server it was uploaded to, so the two
	// servers must have distinct URIs (the per-server pending-file tracker keying).
	assert.NotEqual(t, mxcByServer[multiServerAID], mxcByServer[multiServerBID], "each server should have its own mxc URI")
}

// ghostAvatar returns the avatar mxc URI currently set on a ghost user, or "".
func (suite *MultiServerIntegrationTestSuite) ghostAvatar(c *matrixtest.Container, ghostUserID string) string {
	profile, err := c.Client.GetUserProfile(ghostUserID)
	if err != nil || profile == nil {
		return ""
	}
	return profile.AvatarURL
}

// TestOutboundProfileImageFansOutToServersWithGhost verifies a profile-image
// change updates the user's ghost avatar on every server where a ghost exists,
// and does not touch (or create ghosts on) servers where the user has none.
func (suite *MultiServerIntegrationTestSuite) TestOutboundProfileImageFansOutToServersWithGhost() {
	t := suite.T()

	userID := model.NewId()
	avatar := []byte("avatar-v1-bytes")
	suite.api.On("GetUser", userID).Return(&model.User{Id: userID, Username: "avataruser", Email: userID + "@example.com", Nickname: "Avatar User"}, nil).Maybe()
	suite.api.On("GetProfileImage", userID).Return(func(string) []byte { return avatar }, nil).Maybe()

	// A ghost on both servers (created with avatar v1).
	ghostA, err := suite.bridgeFor(multiServerAID, suite.containerA.Client).CreateOrGetGhostUser(userID)
	require.NoError(t, err)
	ghostB, err := suite.bridgeFor(multiServerBID, suite.containerB.Client).CreateOrGetGhostUser(userID)
	require.NoError(t, err)

	beforeA := suite.ghostAvatar(suite.containerA, ghostA)
	beforeB := suite.ghostAvatar(suite.containerB, ghostB)

	// A different image; the profile-image sync must update the ghost on BOTH servers.
	avatar = []byte("avatar-v2-different-bytes")
	require.NoError(t, suite.plugin.OnSharedChannelsProfileImageSyncMsg(&model.User{Id: userID, Username: "avataruser"}, nil))

	require.Eventuallyf(t, func() bool {
		return suite.ghostAvatar(suite.containerA, ghostA) != beforeA
	}, 15*time.Second, 300*time.Millisecond, "ghost avatar should update on server A")
	require.Eventuallyf(t, func() bool {
		return suite.ghostAvatar(suite.containerB, ghostB) != beforeB
	}, 15*time.Second, 300*time.Millisecond, "ghost avatar should update on server B")

	// A user with a ghost on only server A: the sync must not create one on B.
	userAOnly := model.NewId()
	suite.api.On("GetUser", userAOnly).Return(&model.User{Id: userAOnly, Username: "onlya", Nickname: "Only A"}, nil).Maybe()
	suite.api.On("GetProfileImage", userAOnly).Return([]byte("only-a-avatar"), nil).Maybe()
	_, err = suite.bridgeFor(multiServerAID, suite.containerA.Client).CreateOrGetGhostUser(userAOnly)
	require.NoError(t, err)

	require.NoError(t, suite.plugin.OnSharedChannelsProfileImageSyncMsg(&model.User{Id: userAOnly, Username: "onlya"}, nil))
	_, existsB := suite.plugin.getGhostUserForServer(multiServerBID, userAOnly)
	assert.False(t, existsB, "profile-image sync must not create a ghost on a server that had none")
}

// TestOutboundLoopPreventionAcrossServers verifies that a post originating from
// any of the plugin's own remotes is not echoed back to any server, while a
// genuinely local post still fans out to all mapped servers.
func (suite *MultiServerIntegrationTestSuite) TestOutboundLoopPreventionAcrossServers() {
	t := suite.T()

	const ridA, ridB = "remote-a-id", "remote-b-id"
	suite.seedDistinctServerRemotes(ridA, ridB)

	channelID := model.NewId()
	rooms := suite.mapChannelToBothServers(channelID, "loop")
	userID := model.NewId()
	suite.mockLocalAuthor(userID)

	// A post that originated on server A (carries A's remote ID) must not be
	// re-synced to A or B.
	remoteA := ridA
	loopPost := &model.Post{
		Id:        model.NewId(),
		UserId:    userID,
		ChannelId: channelID,
		Message:   "originated on server A, must not echo",
		CreateAt:  time.Now().UnixMilli(),
		RemoteId:  &remoteA,
	}
	_, err := suite.plugin.OnSharedChannelsSyncMsg(&model.SyncMsg{ChannelId: channelID, Posts: []*model.Post{loopPost}}, nil)
	require.NoError(t, err)

	// A genuinely local post that must reach both servers.
	localPost := &model.Post{
		Id:        model.NewId(),
		UserId:    userID,
		ChannelId: channelID,
		Message:   "local post, should fan out",
		CreateAt:  time.Now().UnixMilli(),
	}
	_, err = suite.plugin.OnSharedChannelsSyncMsg(&model.SyncMsg{ChannelId: channelID, Posts: []*model.Post{localPost}}, nil)
	require.NoError(t, err)

	for _, r := range rooms {
		// The local post arrives...
		require.Eventuallyf(t, func() bool {
			return matrixtest.FindEventByPostID(r.c.GetRoomEvents(t, r.roomID), localPost.Id) != nil
		}, 15*time.Second, 300*time.Millisecond, "local post should reach %s", r.name)
		// ...but the own-remote post never does.
		assert.Nil(t, matrixtest.FindEventByPostID(r.c.GetRoomEvents(t, r.roomID), loopPost.Id), "own-remote post must not be echoed to %s", r.name)
	}
}

// TestOutboundDMRoutesToRemoteParticipantHomeserver verifies that a message in a
// brand-new (unmapped) DM channel is routed to — and its room created on — the
// homeserver of the DM's remote (Matrix) participant, not the primary server.
func (suite *MultiServerIntegrationTestSuite) TestOutboundDMRoutesToRemoteParticipantHomeserver() {
	t := suite.T()

	const ridA, ridB = "dm-remote-a", "dm-remote-b"
	suite.seedDistinctServerRemotes(ridA, ridB)

	channelID := model.NewId()
	localUserID := model.NewId()
	remoteUserID := model.NewId()
	remoteB := ridB

	suite.api.On("GetChannel", channelID).Return(&model.Channel{Id: channelID, Type: model.ChannelTypeDirect}, nil).Maybe()
	suite.api.On("GetChannelMembers", channelID, mock.Anything, mock.Anything).Return(model.ChannelMembers{
		{ChannelId: channelID, UserId: localUserID},
		{ChannelId: channelID, UserId: remoteUserID},
	}, nil).Maybe()
	suite.api.On("GetUser", remoteUserID).Return(&model.User{Id: remoteUserID, Username: "remotepartner", RemoteId: &remoteB}, nil).Maybe()
	suite.mockLocalAuthor(localUserID)

	// The remote participant maps to a real user on server B so the DM room's
	// invite targets an existing account.
	partner := suite.containerB.CreateUser(t, "dmpartner"+model.NewId()[:6], "password123")
	require.NoError(t, suite.plugin.kvstore.Set(kvstore.BuildMattermostUserKey(multiServerBID, remoteUserID), []byte(partner.UserID)))

	post := &model.Post{Id: model.NewId(), UserId: localUserID, ChannelId: channelID, Message: "hi from a DM", CreateAt: time.Now().UnixMilli()}
	_, err := suite.plugin.OnSharedChannelsSyncMsg(&model.SyncMsg{ChannelId: channelID, Posts: []*model.Post{post}}, nil)
	require.NoError(t, err)

	// The DM was mapped to a room on server B (and only B).
	var roomID string
	require.Eventuallyf(t, func() bool {
		data, e := suite.plugin.kvstore.Get(kvstore.BuildChannelMappingKey(channelID))
		if e != nil {
			return false
		}
		mappings, _ := kvstore.ParseChannelServerMappings(data)
		roomID = kvstore.RoomIDForServer(mappings, multiServerBID)
		return roomID != ""
	}, 20*time.Second, 300*time.Millisecond, "DM should be mapped to a room on server B")

	data, _ := suite.plugin.kvstore.Get(kvstore.BuildChannelMappingKey(channelID))
	mappings, _ := kvstore.ParseChannelServerMappings(data)
	assert.Empty(t, kvstore.RoomIDForServer(mappings, multiServerAID), "DM must not be mapped to the primary server")

	// The message was delivered to that server B room.
	require.Eventuallyf(t, func() bool {
		return matrixtest.FindEventByPostID(suite.containerB.GetRoomEvents(t, roomID), post.Id) != nil
	}, 20*time.Second, 300*time.Millisecond, "DM message should be delivered to the server B room")
}
