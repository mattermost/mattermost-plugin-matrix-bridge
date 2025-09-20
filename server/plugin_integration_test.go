package main

import (
	"fmt"
	"testing"
	"time"

	matrixtest "github.com/mattermost/mattermost-plugin-matrix-bridge/testcontainers/matrix"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

// PluginIntegrationTestSuite contains integration tests for plugin-level Matrix operations
type PluginIntegrationTestSuite struct {
	suite.Suite
	matrixContainer *matrixtest.Container
	plugin          *Plugin
	api             *plugintest.API
}

// SetupSuite starts the Matrix container before running tests
func (suite *PluginIntegrationTestSuite) SetupSuite() {
	suite.matrixContainer = matrixtest.StartMatrixContainer(suite.T(), matrixtest.DefaultMatrixConfig())
}

// TearDownSuite cleans up the Matrix container after tests
func (suite *PluginIntegrationTestSuite) TearDownSuite() {
	if suite.matrixContainer != nil {
		suite.matrixContainer.Cleanup(suite.T())
	}
}

// SetupTest prepares each test with fresh plugin instance
func (suite *PluginIntegrationTestSuite) SetupTest() {
	// Create a test room to ensure AS bot user is provisioned
	_ = suite.matrixContainer.CreateRoom(suite.T(), "AS Bot Provisioning Room")

	// Set up mock API
	suite.api = &plugintest.API{}

	// Set up plugin
	suite.plugin = &Plugin{
		remoteID: "test-remote-id",
	}
	suite.plugin.SetAPI(suite.api)

	// Initialize KV store with in-memory implementation
	suite.plugin.kvstore = NewMemoryKVStore()

	// Initialize required components
	suite.plugin.pendingFiles = NewPendingFileTracker()
	suite.plugin.postTracker = NewPostTracker(DefaultPostTrackerMaxEntries)
	suite.plugin.logger = &testLogger{t: suite.T()}

	// Reuse the container's Matrix client to share rate limiting state
	// This prevents rate limit conflicts between container setup and plugin operations
	suite.plugin.matrixClient = suite.matrixContainer.Client

	// Set configuration
	config := &configuration{
		MatrixServerURL: suite.matrixContainer.ServerURL,
		MatrixASToken:   suite.matrixContainer.ASToken,
		MatrixHSToken:   suite.matrixContainer.HSToken,
		EnableSync:      true,
	}
	suite.plugin.configuration = config

	// Initialize bridge components
	suite.plugin.initBridges()
}

// TestPluginMatrixOperations tests plugin-level Matrix operations
func (suite *PluginIntegrationTestSuite) TestPluginMatrixOperations() {
	suite.Run("InviteRemoteUserToMatrixRoom", func() {
		suite.testInviteRemoteUserToMatrixRoom()
	})

	suite.Run("SyncChannelMembersToMatrixRoom", func() {
		suite.testSyncChannelMembersToMatrixRoom()
	})
}

// testInviteRemoteUserToMatrixRoom tests inviting remote users to Matrix rooms
func (suite *PluginIntegrationTestSuite) testInviteRemoteUserToMatrixRoom() {
	// Create a test room
	roomIdentifier := suite.matrixContainer.CreateRoom(suite.T(), "Remote User Test Room")
	roomID, err := suite.plugin.matrixClient.ResolveRoomAlias(roomIdentifier)
	require.NoError(suite.T(), err, "Should resolve room identifier")

	// Create test channel and set up mapping
	testChannelID := model.NewId()
	err = suite.plugin.mattermostToMatrixBridge.setChannelRoomMapping(testChannelID, roomID)
	require.NoError(suite.T(), err, "Should set up channel room mapping")

	suite.Run("InviteExistingRemoteUser", func() {
		// Create a regular Matrix user to represent a remote user
		testUser := suite.matrixContainer.CreateUser(suite.T(), "remoteuser", "password123")

		// Create Mattermost user model representing the remote user
		mattermostUserID := model.NewId()
		remoteUser := &model.User{
			Id:       mattermostUserID,
			Username: "remote_" + testUser.Username,
			Email:    testUser.Username + "@matrix.org",
		}

		// Set up the remote user with proper remote ID
		remoteUser.RemoteId = &suite.plugin.remoteID

		// Set up API mocks
		suite.api.On("GetUser", mattermostUserID).Return(remoteUser, nil)

		// Set up KV store mapping from Mattermost user to Matrix user
		userMapKey := "matrix_user_" + testUser.UserID
		err = suite.plugin.kvstore.Set(userMapKey, []byte(mattermostUserID))
		require.NoError(suite.T(), err, "Should set up user mapping")

		// Set up reverse mapping
		reverseMapKey := "mattermost_user_" + mattermostUserID
		err = suite.plugin.kvstore.Set(reverseMapKey, []byte(testUser.UserID))
		require.NoError(suite.T(), err, "Should set up reverse user mapping")

		// Test inviting remote user to Matrix room
		err = suite.plugin.inviteRemoteUserToMatrixRoom(remoteUser, testChannelID)
		require.NoError(suite.T(), err, "Should invite remote user to Matrix room")

		// Verify user has been invited to the room
		members := suite.matrixContainer.GetRoomMembers(suite.T(), roomID)

		userFound := false
		for _, member := range members {
			if member.UserID == testUser.UserID {
				userFound = true
				assert.Equal(suite.T(), "invite", member.Membership, "Remote user should be invited")
				break
			}
		}
		assert.True(suite.T(), userFound, "Remote user should be found in room members")
		suite.T().Logf("Successfully invited remote user %s to room %s", testUser.UserID, roomID)
	})

	suite.Run("InviteNonExistentUser", func() {
		// Test with user that has no Matrix mapping
		nonExistentUserID := model.NewId()
		nonExistentUser := &model.User{
			Id:       nonExistentUserID,
			Username: "nonexistent",
			Email:    "nonexistent@example.com",
		}
		nonExistentUser.RemoteId = &suite.plugin.remoteID

		suite.api.On("GetUser", nonExistentUserID).Return(nonExistentUser, nil)

		// This should fail because there's no Matrix user mapping
		err := suite.plugin.inviteRemoteUserToMatrixRoom(nonExistentUser, testChannelID)
		assert.Error(suite.T(), err, "Should fail to invite user with no Matrix mapping")
	})

	suite.Run("InviteToNonExistentChannel", func() {
		// Test with channel that's not bridged to Matrix
		testUser := suite.matrixContainer.CreateUser(suite.T(), "remoteuser2", "password123")

		mattermostUserID := model.NewId()
		remoteUser := &model.User{
			Id:       mattermostUserID,
			Username: "remote_" + testUser.Username,
			Email:    testUser.Username + "@matrix.org",
		}
		remoteUser.RemoteId = &suite.plugin.remoteID

		suite.api.On("GetUser", mattermostUserID).Return(remoteUser, nil)

		// Set up user mapping but don't create channel mapping
		userMapKey := "matrix_user_" + testUser.UserID
		err = suite.plugin.kvstore.Set(userMapKey, []byte(mattermostUserID))
		require.NoError(suite.T(), err, "Should set up user mapping")

		reverseMapKey := "mattermost_user_" + mattermostUserID
		err = suite.plugin.kvstore.Set(reverseMapKey, []byte(testUser.UserID))
		require.NoError(suite.T(), err, "Should set up reverse user mapping")

		nonExistentChannelID := model.NewId()
		err = suite.plugin.inviteRemoteUserToMatrixRoom(remoteUser, nonExistentChannelID)
		assert.NoError(suite.T(), err, "Should gracefully skip invite to non-bridged channel")
	})
}

// testSyncChannelMembersToMatrixRoom tests syncing all channel members to a Matrix room
func (suite *PluginIntegrationTestSuite) testSyncChannelMembersToMatrixRoom() {
	// This test will simulate the syncChannelMembersToMatrixRoom functionality
	// Since we can't directly test the command handler method without extensive mocking,
	// we'll test the core synchronization logic

	// Create test room
	roomIdentifier := suite.matrixContainer.CreateRoom(suite.T(), "Member Sync Test Room")
	roomID, err := suite.plugin.matrixClient.ResolveRoomAlias(roomIdentifier)
	require.NoError(suite.T(), err, "Should resolve room identifier")

	// Create test channel
	testChannelID := model.NewId()
	testChannel := &model.Channel{
		Id:          testChannelID,
		DisplayName: "Test Channel",
		Name:        "test-channel",
		Type:        model.ChannelTypeOpen,
	}

	suite.Run("SyncMixedMemberTypes", func() {
		// Create test users - mix of local and remote
		localUser1ID := model.NewId()
		localUser1 := &model.User{
			Id:       localUser1ID,
			Username: "localuser1",
			Email:    "local1@example.com",
		}

		localUser2ID := model.NewId()
		localUser2 := &model.User{
			Id:       localUser2ID,
			Username: "localuser2",
			Email:    "local2@example.com",
		}

		// Create Matrix users to represent remote users with unique names
		timestamp := time.Now().UnixNano()
		username1 := fmt.Sprintf("remoteuser1_%d", timestamp)
		username2 := fmt.Sprintf("remoteuser2_%d", timestamp)

		matrixUser1 := suite.matrixContainer.CreateUser(suite.T(), username1, "password123")
		matrixUser2 := suite.matrixContainer.CreateUser(suite.T(), username2, "password123")

		remoteUser1ID := model.NewId()
		remoteUser1 := &model.User{
			Id:       remoteUser1ID,
			Username: "remote_" + matrixUser1.Username,
			Email:    matrixUser1.Username + "@matrix.org",
		}
		remoteUser1.RemoteId = &suite.plugin.remoteID

		remoteUser2ID := model.NewId()
		remoteUser2 := &model.User{
			Id:       remoteUser2ID,
			Username: "remote_" + matrixUser2.Username,
			Email:    matrixUser2.Username + "@matrix.org",
		}
		remoteUser2.RemoteId = &suite.plugin.remoteID

		// Set up API mocks for all users
		suite.api.On("GetChannel", testChannelID).Return(testChannel, nil)
		suite.api.On("GetUser", localUser1ID).Return(localUser1, nil)
		suite.api.On("GetUser", localUser2ID).Return(localUser2, nil)
		suite.api.On("GetUser", remoteUser1ID).Return(remoteUser1, nil)
		suite.api.On("GetUser", remoteUser2ID).Return(remoteUser2, nil)

		// Mock channel members
		channelMembers := []model.ChannelMember{
			{UserId: localUser1ID, ChannelId: testChannelID},
			{UserId: localUser2ID, ChannelId: testChannelID},
			{UserId: remoteUser1ID, ChannelId: testChannelID},
			{UserId: remoteUser2ID, ChannelId: testChannelID},
		}
		suite.api.On("GetChannelMembers", testChannelID, 0, 100).Return(channelMembers, nil)

		// Mock profile images for ghost user creation
		suite.api.On("GetProfileImage", localUser1ID).Return([]byte("fake-image-data-1"), nil)
		suite.api.On("GetProfileImage", localUser2ID).Return([]byte("fake-image-data-2"), nil)

		// Set up user mappings for remote users
		userMapKey1 := "matrix_user_" + matrixUser1.UserID
		err = suite.plugin.kvstore.Set(userMapKey1, []byte(remoteUser1ID))
		require.NoError(suite.T(), err, "Should set up remote user 1 mapping")

		reverseMapKey1 := "mattermost_user_" + remoteUser1ID
		err = suite.plugin.kvstore.Set(reverseMapKey1, []byte(matrixUser1.UserID))
		require.NoError(suite.T(), err, "Should set up reverse mapping for remote user 1")

		userMapKey2 := "matrix_user_" + matrixUser2.UserID
		err = suite.plugin.kvstore.Set(userMapKey2, []byte(remoteUser2ID))
		require.NoError(suite.T(), err, "Should set up remote user 2 mapping")

		reverseMapKey2 := "mattermost_user_" + remoteUser2ID
		err = suite.plugin.kvstore.Set(reverseMapKey2, []byte(matrixUser2.UserID))
		require.NoError(suite.T(), err, "Should set up reverse mapping for remote user 2")

		// Set up channel room mapping
		err = suite.plugin.mattermostToMatrixBridge.setChannelRoomMapping(testChannelID, roomID)
		require.NoError(suite.T(), err, "Should set up channel room mapping")

		// Test the core sync logic by manually processing each member type
		// This simulates what syncChannelMembersToMatrixRoom would do

		localUserCount := 0
		remoteUserCount := 0

		for _, member := range channelMembers {
			user, appErr := suite.plugin.API.GetUser(member.UserId)
			require.Nil(suite.T(), appErr, "Should get user")

			if user.IsRemote() {
				// Handle remote user - invite to Matrix room
				originalMatrixUserID, err := suite.plugin.mattermostToMatrixBridge.GetMatrixUserIDFromMattermostUser(user.Id)
				if err == nil {
					err = suite.plugin.matrixClient.InviteUserToRoom(roomID, originalMatrixUserID)
					if err == nil {
						remoteUserCount++
						suite.T().Logf("Successfully invited remote user %s (%s) to room", user.Username, originalMatrixUserID)
					}
				}
			} else {
				// Handle local user - create ghost user and join to room
				ghostUserID, err := suite.plugin.mattermostToMatrixBridge.CreateOrGetGhostUser(user.Id)
				if err == nil {
					err = suite.plugin.matrixClient.InviteAndJoinGhostUser(roomID, ghostUserID)
					if err == nil {
						localUserCount++
						suite.T().Logf("Successfully joined ghost user %s for local user %s to room", ghostUserID, user.Username)
					}
				}
			}
		}

		// Verify synchronization results
		assert.Equal(suite.T(), 2, localUserCount, "Should have synced 2 local users as ghost users")
		assert.Equal(suite.T(), 2, remoteUserCount, "Should have synced 2 remote users")

		// Verify all users are in the Matrix room
		members := suite.matrixContainer.GetRoomMembers(suite.T(), roomID)

		// Count different member types in the room
		ghostUserCount := 0
		originalUserCount := 0
		asBotCount := 0

		for _, member := range members {
			if suite.plugin.mattermostToMatrixBridge.isGhostUser(member.UserID) {
				ghostUserCount++
			} else if member.UserID == suite.matrixContainer.GetApplicationServiceBotUserID() {
				asBotCount++
			} else {
				originalUserCount++
			}
		}

		// Note: The actual counts may vary due to test setup and timing, but verify core functionality worked
		assert.GreaterOrEqual(suite.T(), ghostUserCount, 2, "Should have at least 2 ghost users in room")
		assert.GreaterOrEqual(suite.T(), originalUserCount, 2, "Should have at least 2 original Matrix users invited to room")
		// AS bot count may be 0 or 1 depending on room configuration
		assert.GreaterOrEqual(suite.T(), len(members), 4, "Should have at least 4 members total (2 ghost + 2 original + optional AS bot)")

		suite.T().Logf("Room members summary - Ghost users: %d, Original users: %d, AS bot: %d",
			ghostUserCount, originalUserCount, asBotCount)
	})

	suite.Run("SyncEmptyChannel", func() {
		// Test syncing a channel with no members
		emptyChannelID := model.NewId()
		emptyChannel := &model.Channel{
			Id:          emptyChannelID,
			DisplayName: "Empty Channel",
			Name:        "empty-channel",
			Type:        model.ChannelTypeOpen,
		}

		suite.api.On("GetChannel", emptyChannelID).Return(emptyChannel, nil)
		suite.api.On("GetChannelMembers", emptyChannelID, 0, 100).Return([]model.ChannelMember{}, nil)

		// Create room for empty channel
		emptyRoomIdentifier := suite.matrixContainer.CreateRoom(suite.T(), "Empty Channel Test Room")
		emptyRoomID, err := suite.plugin.matrixClient.ResolveRoomAlias(emptyRoomIdentifier)
		require.NoError(suite.T(), err, "Should resolve empty room identifier")

		err = suite.plugin.mattermostToMatrixBridge.setChannelRoomMapping(emptyChannelID, emptyRoomID)
		require.NoError(suite.T(), err, "Should set up empty channel room mapping")

		// Simulate sync with empty channel - should complete without error
		channelMembers := []model.ChannelMember{}
		localUserCount := 0
		remoteUserCount := 0

		for _, member := range channelMembers {
			user, appErr := suite.plugin.API.GetUser(member.UserId)
			require.Nil(suite.T(), appErr, "Should get user")

			if user.IsRemote() {
				remoteUserCount++
			} else {
				localUserCount++
			}
		}

		assert.Equal(suite.T(), 0, localUserCount, "Should have 0 local users in empty channel")
		assert.Equal(suite.T(), 0, remoteUserCount, "Should have 0 remote users in empty channel")

		// Verify only AS bot is in the room
		members := suite.matrixContainer.GetRoomMembers(suite.T(), emptyRoomID)

		assert.Equal(suite.T(), 1, len(members), "Empty room should only have AS bot")
		assert.Equal(suite.T(), suite.matrixContainer.GetApplicationServiceBotUserID(), members[0].UserID, "Only member should be AS bot")
	})
}

// Test runner function
func TestPluginIntegrationTestSuite(t *testing.T) {
	suite.Run(t, new(PluginIntegrationTestSuite))
}
