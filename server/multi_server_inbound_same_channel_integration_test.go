package main

import (
	"net/http"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
)

// This file covers the SHARED-CHANNEL inbound topology: one Mattermost channel is
// bridged to a room on server A AND a room on server B at once. An inbound message
// (or reaction) from each server must land in that same Mattermost channel,
// attributed to the ORIGINATING server's remote and user. There is no
// cross-homeserver relay: a message from A stays in Mattermost (attributed to A)
// and is not re-sent to server B (see TestOutboundLoopPreventionAcrossServers).

// TestInboundSameChannelMessagesLandWithPerServerAttribution verifies that a
// message from each homeserver lands in the one shared channel, carrying the
// correct per-server remote ID and the correct per-server Mattermost user.
func (suite *MultiServerIntegrationTestSuite) TestInboundSameChannelMessagesLandWithPerServerAttribution() {
	t := suite.T()

	const ridA, ridB = "same-remote-a", "same-remote-b"
	suite.seedDistinctServerRemotes(ridA, ridB)

	channelC := model.NewId()
	roomA := "!same-room-a:" + suite.containerA.ServerDomain
	roomB := "!same-room-b:" + suite.containerB.ServerDomain
	// Both rooms reverse-map to the SAME Mattermost channel, each under its own
	// server's namespace.
	suite.seedRoomMapping(multiServerAID, roomA, channelC)
	suite.seedRoomMapping(multiServerBID, roomB, channelC)

	senderA := "@alice:" + suite.containerA.ServerDomain
	senderB := "@bob:" + suite.containerB.ServerDomain
	userA := model.NewId()
	userB := model.NewId()
	require.NoError(t, suite.plugin.kvstore.Set(kvstore.BuildMatrixUserKey(multiServerAID, senderA), []byte(userA)))
	require.NoError(t, suite.plugin.kvstore.Set(kvstore.BuildMatrixUserKey(multiServerBID, senderB), []byte(userB)))

	suite.api.On("GetUser", userA).Return(&model.User{Id: userA, Username: "alice"}, nil).Maybe()
	suite.api.On("GetUser", userB).Return(&model.User{Id: userB, Username: "bob"}, nil).Maybe()
	suite.api.On("GetChannel", mock.AnythingOfType("string")).Return(func(id string) *model.Channel { return &model.Channel{Id: id} }, nil).Maybe()
	created := map[string]*model.Post{}
	suite.api.On("CreatePost", mock.AnythingOfType("*model.Post")).Return(func(p *model.Post) *model.Post {
		if p.Id == "" {
			p.Id = model.NewId()
		}
		created[p.Id] = p
		return p
	}, nil).Maybe()

	const eventAID = "$same-msg-a"
	const eventBID = "$same-msg-b"
	msgA := MatrixEvent{Type: "m.room.message", EventID: eventAID, Sender: senderA, RoomID: roomA, Content: map[string]any{"msgtype": "m.text", "body": "from alice on A"}, Timestamp: 1}
	msgB := MatrixEvent{Type: "m.room.message", EventID: eventBID, Sender: senderB, RoomID: roomB, Content: map[string]any{"msgtype": "m.text", "body": "from bob on B"}, Timestamp: 1}
	require.Equal(t, http.StatusOK, suite.putTransaction(suite.containerA.HSToken, "txn-same-a", []MatrixEvent{msgA}))
	require.Equal(t, http.StatusOK, suite.putTransaction(suite.containerB.HSToken, "txn-same-b", []MatrixEvent{msgB}))

	postIDA, err := suite.plugin.kvstore.Get(kvstore.BuildMatrixEventPostKey(multiServerAID, eventAID))
	require.NoError(t, err)
	postIDB, err := suite.plugin.kvstore.Get(kvstore.BuildMatrixEventPostKey(multiServerBID, eventBID))
	require.NoError(t, err)
	postA := created[string(postIDA)]
	postB := created[string(postIDB)]
	require.NotNil(t, postA, "server A message should create a post")
	require.NotNil(t, postB, "server B message should create a post")

	// Both messages land in the SAME channel.
	assert.Equal(t, channelC, postA.ChannelId, "server A message lands in the shared channel")
	assert.Equal(t, channelC, postB.ChannelId, "server B message lands in the shared channel")

	// Correct per-server user attribution (distinct Mattermost users).
	assert.Equal(t, userA, postA.UserId, "server A message authored by server A's mapped user")
	assert.Equal(t, userB, postB.UserId, "server B message authored by server B's mapped user")

	// Correct per-server remote attribution — the post carries the ORIGINATING
	// server's remote, not the primary's.
	require.NotNil(t, postA.RemoteId)
	require.NotNil(t, postB.RemoteId)
	assert.Equal(t, ridA, *postA.RemoteId, "server A message attributed to server A's remote")
	assert.Equal(t, ridB, *postB.RemoteId, "server B message attributed to server B's remote")
}

// TestInboundSameChannelReactionsAttributeToOriginatingServer verifies that a
// reaction from each homeserver, targeting a post in the shared channel, resolves
// via that server's namespace and applies as that server's mapped user.
func (suite *MultiServerIntegrationTestSuite) TestInboundSameChannelReactionsAttributeToOriginatingServer() {
	t := suite.T()

	const ridA, ridB = "same-rx-remote-a", "same-rx-remote-b"
	suite.seedDistinctServerRemotes(ridA, ridB)

	channelC := model.NewId()
	roomA := "!same-rx-room-a:" + suite.containerA.ServerDomain
	roomB := "!same-rx-room-b:" + suite.containerB.ServerDomain
	suite.seedRoomMapping(multiServerAID, roomA, channelC)
	suite.seedRoomMapping(multiServerBID, roomB, channelC)

	// A target post already synced on each server (event→post mapping in each
	// server's namespace), representing the same shared channel.
	postA := model.NewId()
	postB := model.NewId()
	require.NoError(t, suite.plugin.kvstore.Set(kvstore.BuildMatrixEventPostKey(multiServerAID, "$rx-target-a"), []byte(postA)))
	require.NoError(t, suite.plugin.kvstore.Set(kvstore.BuildMatrixEventPostKey(multiServerBID, "$rx-target-b"), []byte(postB)))

	reactorA := "@alice:" + suite.containerA.ServerDomain
	reactorB := "@bob:" + suite.containerB.ServerDomain
	userA := model.NewId()
	userB := model.NewId()
	require.NoError(t, suite.plugin.kvstore.Set(kvstore.BuildMatrixUserKey(multiServerAID, reactorA), []byte(userA)))
	require.NoError(t, suite.plugin.kvstore.Set(kvstore.BuildMatrixUserKey(multiServerBID, reactorB), []byte(userB)))

	suite.api.On("GetUser", mock.AnythingOfType("string")).Return(func(id string) *model.User { return &model.User{Id: id} }, nil).Maybe()
	suite.api.On("GetChannel", mock.AnythingOfType("string")).Return(func(id string) *model.Channel { return &model.Channel{Id: id} }, nil).Maybe()
	reactions := map[string]*model.Reaction{}
	suite.api.On("AddReaction", mock.AnythingOfType("*model.Reaction")).Return(func(r *model.Reaction) *model.Reaction {
		reactions[r.PostId] = r
		return r
	}, nil).Maybe()

	rxA := MatrixEvent{Type: "m.reaction", EventID: "$rx-a", Sender: reactorA, RoomID: roomA, Content: map[string]any{"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": "$rx-target-a", "key": "👍"}}, Timestamp: 2}
	rxB := MatrixEvent{Type: "m.reaction", EventID: "$rx-b", Sender: reactorB, RoomID: roomB, Content: map[string]any{"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": "$rx-target-b", "key": "👍"}}, Timestamp: 2}
	require.Equal(t, http.StatusOK, suite.putTransaction(suite.containerA.HSToken, "txn-same-rx-a", []MatrixEvent{rxA}))
	require.Equal(t, http.StatusOK, suite.putTransaction(suite.containerB.HSToken, "txn-same-rx-b", []MatrixEvent{rxB}))

	require.NotNil(t, reactions[postA], "server A reaction should be applied to server A's target post")
	require.NotNil(t, reactions[postB], "server B reaction should be applied to server B's target post")
	assert.Equal(t, userA, reactions[postA].UserId, "server A reaction by server A's mapped user")
	assert.Equal(t, userB, reactions[postB].UserId, "server B reaction by server B's mapped user")
}
