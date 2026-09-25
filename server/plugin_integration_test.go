package main

import (
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/matrix"
	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/servers"
	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
	matrixtest "github.com/mattermost/mattermost-plugin-matrix-bridge/testcontainers/matrix"
)

// PluginIntegrationTestSuite contains integration tests for plugin-level Matrix operations
type PluginIntegrationTestSuite struct {
	suite.Suite
	matrixContainer *matrixtest.Container
	plugin          *Plugin
	api             *plugintest.API
	serverID        string
	remoteID        string
	m2mx            *MattermostToMatrixBridge
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
	suite.plugin = &Plugin{}
	suite.plugin.SetAPI(suite.api)

	// Initialize KV store with in-memory implementation
	suite.plugin.kvstore = NewMemoryKVStore()
	suite.plugin.servers = servers.New(suite.plugin.kvstore, pluginLogger{suite.plugin}, pluginHost{suite.plugin})

	// Initialize required components
	suite.plugin.pendingFiles = NewPendingFileTracker()
	suite.plugin.postTracker = NewPostTracker(DefaultPostTrackerMaxEntries)
	suite.plugin.logger = &testLogger{t: suite.T()}
	suite.plugin.configuration = &configuration{}

	// Register a single server backed by the container, reusing the container's Matrix
	// client to share rate limiting state (prevents rate limit conflicts between
	// container setup and plugin operations).
	suite.serverID, suite.remoteID = registerTestServer(suite.T(), suite.plugin, suite.matrixContainer.ServerURL, suite.matrixContainer.ServerDomain, suite.matrixContainer.Client)

	// Build the Mattermost->Matrix bridge for this server
	suite.m2mx, _ = suite.plugin.testBridges(suite.T(), suite.serverID)
}

// matrixClient returns this test's registered Matrix client.
func (suite *PluginIntegrationTestSuite) matrixClient() *matrix.Client {
	return suite.plugin.getMatrixClient(suite.serverID)
}

// TestPluginMatrixOperations tests plugin-level Matrix operations
func (suite *PluginIntegrationTestSuite) TestPluginMatrixOperations() {
	suite.Run("InviteRemoteUserToMatrixRoom", func() {
		suite.testInviteRemoteUserToMatrixRoom()
	})
}

// testInviteRemoteUserToMatrixRoom tests inviting remote users to Matrix rooms
func (suite *PluginIntegrationTestSuite) testInviteRemoteUserToMatrixRoom() {
	// Create a test room
	roomIdentifier := suite.matrixContainer.CreateRoom(suite.T(), "Remote User Test Room")
	roomID, err := suite.matrixClient().ResolveRoomAlias(roomIdentifier)
	require.NoError(suite.T(), err, "Should resolve room identifier")

	// Create test channel and set up mapping
	testChannelID := model.NewId()
	err = suite.m2mx.setChannelRoomMapping(testChannelID, roomID)
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
		remoteUser.RemoteId = &suite.remoteID

		// Set up API mocks
		suite.api.On("GetUser", mattermostUserID).Return(remoteUser, nil)

		// Set up KV store mapping from Mattermost user to Matrix user
		userMapKey := kvstore.BuildMatrixUserKey(suite.serverID, testUser.UserID)
		err = suite.plugin.kvstore.Set(userMapKey, []byte(mattermostUserID))
		require.NoError(suite.T(), err, "Should set up user mapping")

		// Set up reverse mapping
		reverseMapKey := kvstore.BuildMattermostUserKey(suite.serverID, mattermostUserID)
		err = suite.plugin.kvstore.Set(reverseMapKey, []byte(testUser.UserID))
		require.NoError(suite.T(), err, "Should set up reverse user mapping")

		// Test inviting remote user to Matrix room
		err = suite.plugin.inviteRemoteUserToMatrixRoom(suite.serverID, remoteUser, testChannelID)
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
		nonExistentUser.RemoteId = &suite.remoteID

		suite.api.On("GetUser", nonExistentUserID).Return(nonExistentUser, nil)

		// This should fail because there's no Matrix user mapping
		err := suite.plugin.inviteRemoteUserToMatrixRoom(suite.serverID, nonExistentUser, testChannelID)
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
		remoteUser.RemoteId = &suite.remoteID

		suite.api.On("GetUser", mattermostUserID).Return(remoteUser, nil)

		// Set up user mapping but don't create channel mapping
		userMapKey := kvstore.BuildMatrixUserKey(suite.serverID, testUser.UserID)
		err = suite.plugin.kvstore.Set(userMapKey, []byte(mattermostUserID))
		require.NoError(suite.T(), err, "Should set up user mapping")

		reverseMapKey := kvstore.BuildMattermostUserKey(suite.serverID, mattermostUserID)
		err = suite.plugin.kvstore.Set(reverseMapKey, []byte(testUser.UserID))
		require.NoError(suite.T(), err, "Should set up reverse user mapping")

		nonExistentChannelID := model.NewId()
		err = suite.plugin.inviteRemoteUserToMatrixRoom(suite.serverID, remoteUser, nonExistentChannelID)
		assert.NoError(suite.T(), err, "Should gracefully skip invite to non-bridged channel")
	})
}

// Test runner function
func TestPluginIntegrationTestSuite(t *testing.T) {
	skipIfShort(t)
	suite.Run(t, new(PluginIntegrationTestSuite))
}
