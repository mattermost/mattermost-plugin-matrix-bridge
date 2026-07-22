package main

import (
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	matrixtest "github.com/mattermost/mattermost-plugin-matrix-bridge/testcontainers/matrix"
)

// This file covers the SHARED-CHANNEL outbound topology: one Mattermost channel is
// bridged to a room on server A AND a room on server B. A locally-authored post
// fans out to both rooms, and in each room it must be sent by that server's own
// ghost user (same Mattermost user, distinct per-server ghost identity). There is
// no cross-homeserver relay of remote-origin posts — that is asserted by
// TestOutboundLoopPreventionAcrossServers (an own-remote post reaches neither room).

// ghostFor returns the ghost Matrix user ID a Mattermost user is represented by on
// a given server: @_mattermost_<mmUserID>:<serverDomain>.
func ghostFor(mmUserID, serverDomain string) string {
	return "@_mattermost_" + mmUserID + ":" + serverDomain
}

// TestOutboundSameChannelMessageUsesPerServerGhost verifies a local post shared to
// both servers is delivered to each room as that server's own ghost user.
func (suite *MultiServerIntegrationTestSuite) TestOutboundSameChannelMessageUsesPerServerGhost() {
	t := suite.T()

	channelID := model.NewId()
	rooms := suite.mapChannelToBothServers(channelID, "sc-msg")
	userID := model.NewId()
	suite.mockLocalAuthor(userID)

	post, _ := suite.createLocalPostOnBoth(channelID, userID, "hello from a shared channel", rooms)

	for _, r := range rooms {
		ev := matrixtest.FindEventByPostID(r.c.GetRoomEvents(t, r.roomID), post.Id)
		require.NotNilf(t, ev, "post should reach %s", r.name)
		assert.Equalf(t, ghostFor(userID, r.c.ServerDomain), ev.Sender,
			"%s message must be sent by that server's ghost (same user, per-server identity)", r.name)
	}
}

// TestOutboundSameChannelReactionUsesPerServerGhost verifies a reaction on a post
// in the shared channel is applied in each room by that server's own ghost user.
func (suite *MultiServerIntegrationTestSuite) TestOutboundSameChannelReactionUsesPerServerGhost() {
	t := suite.T()

	channelID := model.NewId()
	rooms := suite.mapChannelToBothServers(channelID, "sc-rx")
	userID := model.NewId()
	suite.mockLocalAuthor(userID)

	post, eventIDs := suite.createLocalPostOnBoth(channelID, userID, "react in a shared channel", rooms)
	suite.api.On("GetPost", post.Id).Return(post, nil).Maybe()

	reaction := &model.Reaction{UserId: userID, PostId: post.Id, EmojiName: "thumbsup", CreateAt: time.Now().UnixMilli()}
	_, err := suite.plugin.OnSharedChannelsSyncMsg(&model.SyncMsg{ChannelId: channelID, Reactions: []*model.Reaction{reaction}}, nil)
	require.NoError(t, err)

	for _, r := range rooms {
		require.Eventuallyf(t, func() bool {
			return findReactionEvent(r.c.GetRoomEvents(t, r.roomID), eventIDs[r.serverID]) != nil
		}, 15*time.Second, 300*time.Millisecond, "reaction should reach %s", r.name)
		rx := findReactionEvent(r.c.GetRoomEvents(t, r.roomID), eventIDs[r.serverID])
		assert.Equalf(t, ghostFor(userID, r.c.ServerDomain), rx.Sender,
			"%s reaction must be sent by that server's ghost", r.name)
	}
}
