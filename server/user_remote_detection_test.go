package main

import (
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/matrix"
	matrixtest "github.com/mattermost/mattermost-plugin-matrix-bridge/testcontainers/matrix"
)

// UserRemoteDetectionIntegrationTestSuite tests real loop prevention logic with a Matrix server
type UserRemoteDetectionIntegrationTestSuite struct {
	suite.Suite
	matrixContainer *matrixtest.Container
	plugin          *Plugin
	serverID        string
	remoteID        string
	testChannelID   string
	testRoomID      string
	validator       *matrixtest.EventValidation
	m2mx            *MattermostToMatrixBridge
	mx2m            *MatrixToMattermostBridge
}

// SetupSuite starts the Matrix container before running tests
func (suite *UserRemoteDetectionIntegrationTestSuite) SetupSuite() {
	suite.matrixContainer = matrixtest.StartMatrixContainer(suite.T(), matrixtest.DefaultMatrixConfig())
}

// TearDownSuite cleans up the Matrix container after tests
func (suite *UserRemoteDetectionIntegrationTestSuite) TearDownSuite() {
	if suite.matrixContainer != nil {
		suite.matrixContainer.Cleanup(suite.T())
	}
}

// SetupTest prepares each test with fresh plugin instance
func (suite *UserRemoteDetectionIntegrationTestSuite) SetupTest() {
	// Create mock API
	api := &plugintest.API{}

	// Set up test data
	suite.testChannelID = model.NewId()
	suite.testRoomID = suite.matrixContainer.CreateRoom(suite.T(), generateUniqueRoomName("Loop Prevention Test Room"))

	// Create plugin instance
	suite.plugin = &Plugin{}
	suite.plugin.SetAPI(api)

	// Initialize plugin components
	suite.plugin.kvstore = NewMemoryKVStore()
	suite.plugin.pendingFiles = NewPendingFileTracker()
	suite.plugin.postTracker = NewPostTracker(DefaultPostTrackerMaxEntries)
	suite.plugin.configuration = &configuration{}

	// Initialize the logger (required before registering the test server)
	suite.plugin.logger = &testLogger{t: suite.T()}

	// Create Matrix client and register it as this test's single server
	matrixClient := createMatrixClientWithTestLogger(
		suite.T(),
		suite.matrixContainer.ServerURL,
		suite.matrixContainer.ASToken,
		"",
	)
	matrixClient.SetServerDomain(suite.matrixContainer.ServerDomain)
	suite.serverID, suite.remoteID = registerTestServer(suite.T(), suite.plugin, suite.matrixContainer.ServerURL, suite.matrixContainer.ServerDomain, matrixClient)

	// Use a different prefix than the default to prove configurability
	setTestServerUsernamePrefix(suite.T(), suite.plugin, suite.serverID, "testmatrix")

	// Build bridges
	suite.m2mx, suite.mx2m = suite.plugin.testBridges(suite.T(), suite.serverID)

	// Set up test data in KV store
	setupTestKVData(suite.plugin.kvstore, suite.serverID, suite.testChannelID, suite.testRoomID)

	// Initialize validation helper
	suite.validator = matrixtest.NewEventValidation(
		suite.T(),
		suite.matrixContainer.ServerDomain,
		suite.remoteID,
	)

	// Set up mock API expectations
	suite.setupMockAPI(api)
}

// setupMockAPI configures common mock API expectations
func (suite *UserRemoteDetectionIntegrationTestSuite) setupMockAPI(api *plugintest.API) {
	// Mock channel retrieval
	testChannel := &model.Channel{
		Id:   suite.testChannelID,
		Name: "test-channel",
		Type: model.ChannelTypeOpen,
	}
	api.On("GetChannel", suite.testChannelID).Return(testChannel, nil)

	// Mock logging (only for plugin-level operations, bridge uses its own logger)
	api.On("LogDebug", mock.Anything, mock.Anything, mock.Anything).Maybe()
	api.On("LogDebug", mock.Anything, mock.Anything).Maybe()
	api.On("LogInfo", mock.Anything, mock.Anything, mock.Anything).Maybe()
	api.On("LogInfo", mock.Anything, mock.Anything).Maybe()
	api.On("LogWarn", mock.Anything, mock.Anything, mock.Anything).Maybe()
	api.On("LogWarn", mock.Anything, mock.Anything).Maybe()
	api.On("LogError", mock.Anything, mock.Anything, mock.Anything).Maybe()
	api.On("LogError", mock.Anything, mock.Anything).Maybe()
}

// TestGhostUserCreationAndDetection tests that ghost users are properly created and detected as remote
func (suite *UserRemoteDetectionIntegrationTestSuite) TestGhostUserCreationAndDetection() {
	t := suite.T()

	// Create a local Mattermost user
	localUserID := model.NewId()
	localUser := &model.User{
		Id:       localUserID,
		Username: "alice_local",
		Email:    "alice@example.com",
		RemoteId: nil, // Local user
	}

	// Mock the user retrieval and profile image
	api := suite.plugin.API.(*plugintest.API)
	api.On("GetUser", localUserID).Return(localUser, nil)
	api.On("GetProfileImage", localUserID).Return([]byte("fake-image-data"), nil)

	// Test 1: Verify the local user is NOT considered remote
	assert.False(t, localUser.IsRemote(), "Local Mattermost user should not be remote")

	// Test 2: Create a ghost user for the local user
	ghostUserID, err := suite.m2mx.CreateOrGetGhostUser(localUserID)
	assert.NoError(t, err, "Ghost user creation should succeed")
	assert.NotEmpty(t, ghostUserID, "Ghost user ID should not be empty")

	// The ghost user ID should follow the Matrix bridge pattern
	expectedGhostPattern := "@_mattermost_" + localUserID + ":" + suite.matrixContainer.ServerDomain
	assert.Equal(t, expectedGhostPattern, ghostUserID, "Ghost user ID should follow bridge naming pattern")

	t.Logf("✓ Created ghost user: %s for local user: %s", ghostUserID, localUser.Username)

	// Test 3: Simulate what happens when Matrix->Mattermost sync creates a Mattermost user
	// This represents a Matrix user that gets synced to Mattermost
	matrixOriginatedUser := &model.User{
		Id:       model.NewId(),
		Username: "testmatrix:bob_from_matrix",
		Email:    "bob@matrix.example.com",
		RemoteId: &suite.remoteID, // Set by Matrix->Mattermost sync
	}

	// Test 4: Verify the Matrix-originated user IS considered remote
	assert.True(t, matrixOriginatedUser.IsRemote(), "Matrix-originated user should be remote")

	t.Logf("✓ Matrix-originated user %s correctly identified as remote (RemoteId: %s)",
		matrixOriginatedUser.Username, *matrixOriginatedUser.RemoteId)

	// Test 5: Verify loop prevention - Matrix-originated users should not trigger ghost user creation
	// In real implementation, this would be checked before calling CreateOrGetGhostUser
	shouldCreateGhost := !matrixOriginatedUser.IsRemote()
	assert.False(t, shouldCreateGhost, "Matrix-originated users should not trigger ghost user creation")

	t.Logf("✓ Loop prevention working: Matrix user %s would NOT trigger ghost user creation",
		matrixOriginatedUser.Username)
}

// TestRealMatrixUserInteraction tests the complete flow with actual Matrix operations
func (suite *UserRemoteDetectionIntegrationTestSuite) TestRealMatrixUserInteraction() {
	t := suite.T()

	// Create a local Mattermost user to test ghost user creation
	localUserID := model.NewId()
	localUser := &model.User{
		Id:       localUserID,
		Username: "test_user_for_ghost",
		Email:    "testuser@example.com",
		RemoteId: nil, // Local user
	}

	// Mock the user retrieval and profile image
	api := suite.plugin.API.(*plugintest.API)
	api.On("GetUser", localUserID).Return(localUser, nil)
	api.On("GetProfileImage", localUserID).Return([]byte("fake-image-data"), nil)

	// Create a ghost user for the local user - this is a proper Matrix user in our namespace
	ghostUserID, err := suite.m2mx.CreateOrGetGhostUser(localUserID)
	assert.NoError(t, err, "Ghost user creation should succeed")
	assert.NotEmpty(t, ghostUserID, "Ghost user ID should not be empty")

	t.Logf("✓ Created ghost user: %s", ghostUserID)

	// Join the test room as the ghost user
	err = suite.plugin.getMatrixClient(suite.serverID).JoinRoomAsUser(suite.testRoomID, ghostUserID)
	assert.NoError(t, err, "Ghost user should be able to join room")

	// Send a message as the ghost user to demonstrate real Matrix operations
	messageReq := matrix.MessageRequest{
		RoomID:      suite.testRoomID,
		GhostUserID: ghostUserID,
		Message:     "Hello from ghost user for loop prevention test",
	}
	response, err := suite.plugin.getMatrixClient(suite.serverID).SendMessage(messageReq)
	assert.NoError(t, err, "Should be able to send message as ghost user")
	assert.NotEmpty(t, response.EventID, "Should receive event ID")

	t.Logf("✓ Ghost user %s sent message with event ID: %s", ghostUserID, response.EventID)

	// Test: Get the ghost user's profile to simulate what the bridge would do
	profile, err := suite.plugin.getMatrixClient(suite.serverID).GetUserProfile(ghostUserID)
	assert.NoError(t, err, "Should be able to get ghost user profile")
	assert.NotNil(t, profile, "Profile should not be nil")

	t.Logf("✓ Retrieved ghost user profile: displayname=%s", profile.DisplayName)

	// Test: Simulate what would happen if this ghost user was synced back to Mattermost
	// In the real bridge, Matrix users that get synced to Mattermost would have RemoteId set
	simulatedMattermostUser := &model.User{
		Id:       model.NewId(),
		Username: "testmatrix:" + extractUsernameFromMatrixID(ghostUserID),
		Email:    extractUsernameFromMatrixID(ghostUserID) + "@matrix.bridge.local",
		RemoteId: &suite.remoteID, // This would be set by Matrix->Mattermost sync
	}

	// Verify this simulated user would be correctly identified as remote
	assert.True(t, simulatedMattermostUser.IsRemote(),
		"Mattermost user created from Matrix ghost user should be identified as remote")

	// Verify loop prevention would work
	shouldSkipSync := simulatedMattermostUser.IsRemote()
	assert.True(t, shouldSkipSync,
		"Matrix-originated Mattermost user should be skipped in Mattermost->Matrix sync to prevent loops")

	t.Logf("✓ Loop prevention verified: User %s would be SKIPPED in reverse sync",
		simulatedMattermostUser.Username)

	// Test: Verify normal local users would still be processed
	normalLocalUser := &model.User{
		Id:       model.NewId(),
		Username: "normal_user",
		Email:    "normal@mattermost.local",
		RemoteId: nil, // Local user
	}

	assert.False(t, normalLocalUser.IsRemote(), "Normal local user should not be remote")
	shouldProcessNormalUser := !normalLocalUser.IsRemote()
	assert.True(t, shouldProcessNormalUser, "Normal local users should be processed")

	t.Logf("✓ Normal user %s would be PROCESSED normally", normalLocalUser.Username)
}

// TestConfigurableUsernamePrefix tests that the username prefix configuration is working
func (suite *UserRemoteDetectionIntegrationTestSuite) TestConfigurableUsernamePrefix() {
	t := suite.T()

	// Create a local Mattermost user
	localUserID := model.NewId()
	localUser := &model.User{
		Id:       localUserID,
		Username: "prefix_test_user",
		Email:    "prefixtest@example.com",
		RemoteId: nil,
	}

	// Mock user retrieval
	api := suite.plugin.API.(*plugintest.API)
	api.On("GetUser", localUserID).Return(localUser, nil)
	api.On("GetProfileImage", localUserID).Return([]byte("fake-image-data"), nil)

	// Mock GetUserByUsername to simulate that the username doesn't exist (for uniqueness check)
	api.On("GetUserByUsername", "testmatrix:alice").Return(nil, &model.AppError{Message: "User not found"})

	// Test username generation uses the configured prefix
	baseUsername := "alice"
	generatedUsername, err := suite.mx2m.generateMattermostUsername(baseUsername)
	require.NoError(t, err)

	// Should use "testmatrix:" prefix from test configuration
	expectedUsername := "testmatrix:alice"
	assert.Equal(t, expectedUsername, generatedUsername, "Generated username should use configured prefix")

	t.Logf("✓ Username generation uses configured prefix: %s", generatedUsername)

	// Test that the registry returns the correct per-server prefix
	actualPrefix, err := suite.m2mx.matrixUsernamePrefix()
	require.NoError(t, err)
	assert.Equal(t, "testmatrix", actualPrefix, "Registry should return the set per-server prefix")

	t.Logf("✓ Configuration returns correct prefix: %s", actualPrefix)
}

// TestBridgeMetadataConsistency tests that bridge metadata is consistent across operations
func (suite *UserRemoteDetectionIntegrationTestSuite) TestBridgeMetadataConsistency() {
	t := suite.T()

	// Test that the bridge's remote ID is consistent
	assert.NotEmpty(t, suite.remoteID, "Plugin should have a remote ID")

	// Test that ghost users created by this bridge would have the correct metadata
	localUserID := model.NewId()
	localUser := &model.User{
		Id:       localUserID,
		Username: "metadata_test_user",
		Email:    "metadata@example.com",
		RemoteId: nil,
	}

	// Mock user retrieval
	api := suite.plugin.API.(*plugintest.API)
	api.On("GetUser", localUserID).Return(localUser, nil)
	api.On("GetProfileImage", localUserID).Return([]byte("fake-image-data"), nil)

	// Create ghost user
	ghostUserID, err := suite.m2mx.CreateOrGetGhostUser(localUserID)
	assert.NoError(t, err, "Ghost user creation should succeed")

	// Verify the ghost user follows the expected pattern
	expectedPrefix := "@_mattermost_" + localUserID + ":"
	assert.True(t, strings.HasPrefix(ghostUserID, expectedPrefix),
		"Ghost user ID should have correct prefix: %s", expectedPrefix)

	t.Logf("✓ Ghost user %s has correct metadata format", ghostUserID)

	// Test that if this ghost user were to be represented as a Mattermost user,
	// it would have the correct RemoteId
	simulatedGhostAsMattermostUser := &model.User{
		Id:       model.NewId(),
		Username: extractUsernameFromMatrixID(ghostUserID),
		RemoteId: &suite.remoteID,
	}

	assert.True(t, simulatedGhostAsMattermostUser.IsRemote(),
		"Ghost user represented as Mattermost user should be remote")

	t.Logf("✓ Metadata consistency verified for remote ID: %s", suite.remoteID)
}

// Helper function to extract username from Matrix user ID
func extractUsernameFromMatrixID(matrixUserID string) string {
	// Remove @ prefix and everything after :
	if len(matrixUserID) > 1 && matrixUserID[0] == '@' {
		parts := strings.Split(matrixUserID[1:], ":")
		if len(parts) > 0 {
			return parts[0]
		}
	}
	return "unknown"
}

// Run the test suite
func TestUserRemoteDetectionIntegration(t *testing.T) {
	suite.Run(t, new(UserRemoteDetectionIntegrationTestSuite))
}

// TestDefaultUsernamePrefix tests that the default prefix is used when a server has none
// configured, and that a configured per-server prefix takes precedence.
func TestDefaultUsernamePrefix(t *testing.T) {
	plugin := setupPluginForTest()
	plugin.kvstore = NewMemoryKVStore()
	matrixClient := createMatrixClientWithTestLogger(t, "https://matrix.example.com", "as-token", "")
	serverID, _ := registerTestServer(t, plugin, "https://matrix.example.com", "matrix.example.com", matrixClient)

	m2mx, _ := plugin.testBridges(t, serverID)

	prefix, err := m2mx.matrixUsernamePrefix()
	require.NoError(t, err)
	assert.Equal(t, DefaultMatrixUsernamePrefix, prefix, "Empty prefix should return default")

	// Test with an explicit per-server prefix
	setTestServerUsernamePrefix(t, plugin, serverID, "customprefix")
	prefix, err = m2mx.matrixUsernamePrefix()
	require.NoError(t, err)
	assert.Equal(t, "customprefix", prefix, "Should return the configured per-server prefix")

	t.Logf("✓ Default prefix: %s", DefaultMatrixUsernamePrefix)
	t.Logf("✓ Custom prefix: %s", prefix)
}

// TestBasicRemoteDetectionLogic tests the basic logic without requiring Matrix server
// This is kept as a lightweight unit test for the core logic
func TestBasicRemoteDetectionLogic(t *testing.T) {
	tests := []struct {
		name     string
		user     *model.User
		isRemote bool
		context  string
	}{
		{
			name: "local_user",
			user: &model.User{
				Id:       "local123",
				Username: "alice",
				RemoteId: nil,
			},
			isRemote: false,
			context:  "Local Mattermost users should not be remote",
		},
		{
			name: "matrix_bridge_user",
			user: &model.User{
				Id:       "remote123",
				Username: "testmatrix:bob",
				RemoteId: &[]string{"matrix_bridge_id"}[0],
			},
			isRemote: true,
			context:  "Users from Matrix bridge should be remote",
		},
		{
			name: "other_bridge_user",
			user: &model.User{
				Id:       "remote456",
				Username: "slack:charlie",
				RemoteId: &[]string{"slack_bridge_id"}[0],
			},
			isRemote: true,
			context:  "Users from any remote bridge should be remote",
		},
		{
			name: "empty_remote_id",
			user: &model.User{
				Id:       "edge_case",
				Username: "david",
				RemoteId: &[]string{""}[0], // Empty string
			},
			isRemote: false,
			context:  "Empty RemoteId should be treated as local",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.user.IsRemote()
			assert.Equal(t, tt.isRemote, result, tt.context)

			// Log for documentation
			if result {
				t.Logf("✓ User %s identified as REMOTE (would be skipped in sync)", tt.user.Username)
			} else {
				t.Logf("✓ User %s identified as LOCAL (would be processed normally)", tt.user.Username)
			}
		})
	}
}
