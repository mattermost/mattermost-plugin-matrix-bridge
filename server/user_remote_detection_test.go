package main

import (
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/servers"
	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
	matrixtest "github.com/mattermost/mattermost-plugin-matrix-bridge/testcontainers/matrix"
)

// UserRemoteDetectionIntegrationTestSuite checks username generation under a per-server prefix.
type UserRemoteDetectionIntegrationTestSuite struct {
	suite.Suite
	matrixContainer *matrixtest.Container
	plugin          *Plugin
	testChannelID   string
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
	testRoomID := suite.matrixContainer.CreateRoom(suite.T(), generateUniqueRoomName("Username Prefix Test Room"))

	setup := setupSingleServerIntegrationTest(suite.T(), api, suite.matrixContainer, suite.testChannelID, testRoomID,
		func(plugin *Plugin, serverID string) {
			// Use a different prefix than the default to prove configurability.
			setTestServerUsernamePrefix(suite.T(), plugin, serverID, "testmatrix")
		})
	suite.plugin = setup.Plugin
	suite.m2mx = setup.M2Mx
	suite.mx2m = setup.Mx2M

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

// Run the test suite
func TestUserRemoteDetectionIntegration(t *testing.T) {
	skipIfShort(t)
	suite.Run(t, new(UserRemoteDetectionIntegrationTestSuite))
}

// TestDefaultUsernamePrefix tests that the default prefix is used when a server has none
// configured, and that a configured per-server prefix takes precedence.
func TestDefaultUsernamePrefix(t *testing.T) {
	plugin := setupPluginForTest()
	plugin.kvstore = NewMemoryKVStore()
	plugin.servers = servers.New(plugin.kvstore, pluginLogger{plugin}, pluginHost{plugin})
	matrixClient := createMatrixClientWithTestLogger(t, "https://matrix.example.com", "as-token", "")
	serverID, _ := registerTestServer(t, plugin, "https://matrix.example.com", "matrix.example.com", matrixClient)

	m2mx, _ := plugin.testBridges(t, serverID)

	prefix, err := m2mx.matrixUsernamePrefix()
	require.NoError(t, err)
	assert.Equal(t, servers.DefaultUsernamePrefix, prefix, "Empty prefix should return default")

	// Test with an explicit per-server prefix
	setTestServerUsernamePrefix(t, plugin, serverID, "customprefix")
	prefix, err = m2mx.matrixUsernamePrefix()
	require.NoError(t, err)
	assert.Equal(t, "customprefix", prefix, "Should return the configured per-server prefix")

	t.Logf("✓ Default prefix: %s", servers.DefaultUsernamePrefix)
	t.Logf("✓ Custom prefix: %s", prefix)

	// A registry read failure must be surfaced as an error, never silently treated as
	// "no prefix configured" - see matrixUsernamePrefix's doc comment. This is the
	// entire reason matrixUsernamePrefix/generateMattermostUsername return (string,
	// error) instead of just a string. Reaching that read at all requires emptying the
	// serverConfigs snapshot: serverConfig goes through serverConfigForRouting, which
	// answers from the cache and only falls back to KV when the server isn't in it.
	plugin.kvstore = &erroringKVStore{
		KVStore:     plugin.kvstore,
		errOnGetKey: kvstore.KeyServersConfig,
	}
	plugin.servers = servers.New(plugin.kvstore, pluginLogger{plugin}, pluginHost{plugin})
	plugin.serverConfigs = nil
	m2mxErr, mx2mErr := plugin.testBridges(t, serverID)

	_, err = m2mxErr.matrixUsernamePrefix()
	require.Error(t, err, "matrixUsernamePrefix must return an error when the server registry can't be read")

	username, err := mx2mErr.generateMattermostUsername("alice")
	require.Error(t, err, "generateMattermostUsername must fail when the username prefix can't be read")
	assert.Empty(t, username, "generateMattermostUsername must return an empty username on failure")
}

// TestBasicRemoteDetectionLogic tests IsRemote on local and remote users. It needs no Matrix
// server.
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
