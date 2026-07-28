package main

import (
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/assert"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
)

func TestDMChannelDetection(t *testing.T) {
	plugin := setupPluginForTest()

	// Set up required fields for bridge initialization
	plugin.maxProfileImageSize = DefaultMaxProfileImageSize
	plugin.maxFileSize = DefaultMaxFileSize
	plugin.postTracker = NewPostTracker(DefaultPostTrackerMaxEntries)
	plugin.pendingFiles = NewPendingFileTracker()
	setTestMatrixClient(plugin, createMatrixClientWithTestLogger(t, "", "", ""))

	api := plugin.API.(*plugintest.API)

	// Test DM channel detection
	t.Run("DetectDirectChannel", func(t *testing.T) {
		channelID := model.NewId()
		userID1 := model.NewId()
		userID2 := model.NewId()

		// Mock the channel as a DM
		dmChannel := &model.Channel{
			Id:   channelID,
			Type: model.ChannelTypeDirect,
		}
		api.On("GetChannel", channelID).Return(dmChannel, nil)

		// Mock channel members
		members := model.ChannelMembers{
			{UserId: userID1},
			{UserId: userID2},
		}
		api.On("GetChannelMembers", channelID, 0, 10).Return(members, nil)

		// Test detection
		isDM, userIDs, err := testOutboundBridge(plugin).isDirectChannel(channelID)
		assert.NoError(t, err)
		assert.True(t, isDM)
		assert.Len(t, userIDs, 2)
		assert.Contains(t, userIDs, userID1)
		assert.Contains(t, userIDs, userID2)
	})

	t.Run("DetectGroupChannel", func(t *testing.T) {
		channelID := model.NewId()
		userID1 := model.NewId()
		userID2 := model.NewId()
		userID3 := model.NewId()

		// Mock the channel as a group DM
		groupChannel := &model.Channel{
			Id:   channelID,
			Type: model.ChannelTypeGroup,
		}
		api.On("GetChannel", channelID).Return(groupChannel, nil)

		// Mock channel members for pagination
		members := model.ChannelMembers{
			{UserId: userID1},
			{UserId: userID2},
			{UserId: userID3},
		}
		api.On("GetChannelMembers", channelID, 0, 100).Return(members, nil)
		// Mock the second pagination call which should return empty to stop pagination
		api.On("GetChannelMembers", channelID, 100, 100).Return(model.ChannelMembers{}, nil)

		// Test detection
		isDM, userIDs, err := testOutboundBridge(plugin).isDirectChannel(channelID)
		assert.NoError(t, err)
		assert.True(t, isDM)
		assert.Len(t, userIDs, 3)
		assert.Contains(t, userIDs, userID1)
		assert.Contains(t, userIDs, userID2)
		assert.Contains(t, userIDs, userID3)
	})

	t.Run("DetectRegularChannel", func(t *testing.T) {
		channelID := model.NewId()

		// Mock the channel as a regular public channel
		publicChannel := &model.Channel{
			Id:   channelID,
			Type: model.ChannelTypeOpen,
		}
		api.On("GetChannel", channelID).Return(publicChannel, nil)

		// Test detection
		isDM, userIDs, err := testOutboundBridge(plugin).isDirectChannel(channelID)
		assert.NoError(t, err)
		assert.False(t, isDM)
		assert.Nil(t, userIDs)
	})
}

func TestDMRoomMapping(t *testing.T) {
	plugin := setupPluginForTest()

	// Set up required fields for bridge initialization
	plugin.maxProfileImageSize = DefaultMaxProfileImageSize
	plugin.maxFileSize = DefaultMaxFileSize
	plugin.postTracker = NewPostTracker(DefaultPostTrackerMaxEntries)
	plugin.pendingFiles = NewPendingFileTracker()
	plugin.kvstore = NewMemoryKVStore() // Initialize KV store for tests
	setTestMatrixClient(plugin, createMatrixClientWithTestLogger(t, "", "", ""))

	t.Run("SetAndGetDMRoomMapping", func(t *testing.T) {
		channelID := model.NewId()
		matrixRoomID := "!dmroom:matrix.example.com"

		// Test setting room mapping (unified for all channels)
		err := testOutboundBridge(plugin).setChannelRoomMapping(channelID, matrixRoomID)
		assert.NoError(t, err)

		// Test getting room mapping
		retrievedRoomID, err := testOutboundBridge(plugin).GetMatrixRoomID(channelID)
		assert.NoError(t, err)
		assert.Equal(t, matrixRoomID, retrievedRoomID)

		// Test reverse mapping (Matrix -> Mattermost)
		roomMappingKey := kvstore.BuildRoomMappingKey(testServerID, matrixRoomID)
		channelIDBytes, err := plugin.kvstore.Get(roomMappingKey)
		assert.NoError(t, err)
		assert.Equal(t, channelID, string(channelIDBytes))
	})

	t.Run("GetNonexistentRoomMapping", func(t *testing.T) {
		nonexistentChannelID := model.NewId()

		// Test getting nonexistent room mapping
		roomID, err := testOutboundBridge(plugin).GetMatrixRoomID(nonexistentChannelID)
		assert.NoError(t, err) // GetMatrixRoomID returns empty string, not error for missing keys
		assert.Empty(t, roomID)
	})
}
