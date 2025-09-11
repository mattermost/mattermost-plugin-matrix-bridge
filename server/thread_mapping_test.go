package main

import (
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/mocks"
	matrixtest "github.com/mattermost/mattermost-plugin-matrix-bridge/testcontainers/matrix"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/suite"
)

// TestGetThreadRootFromPostID tests the thread root resolution functionality with mocked API
func TestGetThreadRootFromPostID(t *testing.T) {
	tests := []struct {
		name           string
		postID         string
		mockPost       *model.Post
		mockError      *model.AppError
		expectedRootID string
		description    string
	}{
		{
			name:   "thread_root_post",
			postID: "post_123",
			mockPost: &model.Post{
				Id:     "post_123",
				RootId: "", // This is the thread root
			},
			mockError:      nil,
			expectedRootID: "post_123",
			description:    "Post that is already a thread root should return its own ID",
		},
		{
			name:   "thread_reply_post",
			postID: "reply_456",
			mockPost: &model.Post{
				Id:     "reply_456",
				RootId: "post_123", // This is a reply to post_123
			},
			mockError:      nil,
			expectedRootID: "post_123",
			description:    "Thread reply should return the root post ID",
		},
		{
			name:   "standalone_post",
			postID: "standalone_789",
			mockPost: &model.Post{
				Id:     "standalone_789",
				RootId: "", // Standalone post
			},
			mockError:      nil,
			expectedRootID: "standalone_789",
			description:    "Standalone post should return its own ID",
		},
		{
			name:           "empty_post_id",
			postID:         "",
			mockPost:       nil,
			mockError:      nil,
			expectedRootID: "",
			description:    "Empty post ID should return empty string",
		},
		{
			name:           "post_not_found",
			postID:         "missing_post",
			mockPost:       nil,
			mockError:      &model.AppError{Message: "Post not found"},
			expectedRootID: "missing_post", // Should fallback to original ID
			description:    "Missing post should fallback to original ID",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create bridge instance with mock API
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockAPI := mocks.NewMockAPI(ctrl)
			bridge := &MatrixToMattermostBridge{
				BridgeUtils: &BridgeUtils{
					API:    mockAPI,
					logger: &testLogger{t: t},
				},
			}

			// Set up mock expectations
			if tt.postID != "" {
				if tt.mockError != nil {
					mockAPI.EXPECT().GetPost(tt.postID).Return(nil, tt.mockError)
				} else if tt.mockPost != nil {
					mockAPI.EXPECT().GetPost(tt.postID).Return(tt.mockPost, nil)
				}
			}

			// Test the function
			result := bridge.getThreadRootFromPostID(tt.postID)

			// Assert result
			assert.Equal(t, tt.expectedRootID, result, tt.description)
		})
	}
}

// ThreadMappingIntegrationTestSuite tests the threading fix with real Matrix server
type ThreadMappingIntegrationTestSuite struct {
	suite.Suite
	matrixContainer *matrixtest.Container
	plugin          *Plugin
	testChannelID   string
	testUserID      string
	testRoomID      string
	validator       *matrixtest.EventValidation
}

// SetupSuite starts the Matrix container before running tests
func (suite *ThreadMappingIntegrationTestSuite) SetupSuite() {
	suite.matrixContainer = matrixtest.StartMatrixContainer(suite.T(), matrixtest.DefaultMatrixConfig())
}

// TearDownSuite cleans up the Matrix container after tests
func (suite *ThreadMappingIntegrationTestSuite) TearDownSuite() {
	if suite.matrixContainer != nil {
		suite.matrixContainer.Cleanup(suite.T())
	}
}

// SetupTest prepares each test with fresh plugin instance
func (suite *ThreadMappingIntegrationTestSuite) SetupTest() {
	// Create mock API
	api := &plugintest.API{}

	// Set up test data
	suite.testChannelID = model.NewId()
	suite.testUserID = model.NewId()
	suite.testRoomID = suite.matrixContainer.CreateRoom(suite.T(), generateUniqueRoomName("Thread Test Room"))

	// Create plugin instance
	suite.plugin = &Plugin{
		remoteID: "test-remote-id",
	}
	suite.plugin.SetAPI(api)

	// Initialize plugin components
	suite.plugin.kvstore = NewMemoryKVStore()
	suite.plugin.pendingFiles = NewPendingFileTracker()
	suite.plugin.postTracker = NewPostTracker(DefaultPostTrackerMaxEntries)

	// Create Matrix client
	suite.plugin.matrixClient = createMatrixClientWithTestLogger(
		suite.T(),
		suite.matrixContainer.ServerURL,
		suite.matrixContainer.ASToken,
		suite.plugin.remoteID,
	)
	suite.plugin.matrixClient.SetServerDomain(suite.matrixContainer.ServerDomain)

	// Set up configuration
	config := &configuration{
		MatrixServerURL: suite.matrixContainer.ServerURL,
		MatrixASToken:   suite.matrixContainer.ASToken,
		MatrixHSToken:   suite.matrixContainer.HSToken,
	}
	suite.plugin.configuration = config

	// Initialize the logger (required before initBridges)
	suite.plugin.logger = &testLogger{t: suite.T()}

	// Initialize bridges
	suite.plugin.initBridges()

	// Set up test data in KV store
	setupTestKVData(suite.plugin.kvstore, suite.testChannelID, suite.testRoomID)

	// Initialize validation helper
	suite.validator = matrixtest.NewEventValidation(
		suite.T(),
		suite.matrixContainer.ServerDomain,
		suite.plugin.remoteID,
	)

	// Set up mock API expectations
	suite.setupMockAPI(api)
}

// setupMockAPI configures common mock API expectations
func (suite *ThreadMappingIntegrationTestSuite) setupMockAPI(api *plugintest.API) {
	// Mock user retrieval
	testUser := &model.User{
		Id:       suite.testUserID,
		Username: "testuser",
		Email:    "test@example.com",
	}
	api.On("GetUser", suite.testUserID).Return(testUser, nil)

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

// TestThreadRootResolution tests that the bridge correctly resolves thread roots
func (suite *ThreadMappingIntegrationTestSuite) TestThreadRootResolution() {
	t := suite.T()

	// Create test posts that simulate a thread scenario
	threadRootID := model.NewId()
	threadReplyID := model.NewId()

	// Mock the thread root post
	threadRootPost := &model.Post{
		Id:        threadRootID,
		ChannelId: suite.testChannelID,
		UserId:    suite.testUserID,
		Message:   "This is the thread root",
		RootId:    "", // This is the root
	}

	// Mock a reply in the thread
	threadReplyPost := &model.Post{
		Id:        threadReplyID,
		ChannelId: suite.testChannelID,
		UserId:    suite.testUserID,
		Message:   "This is a reply in the thread",
		RootId:    threadRootID, // Points to the thread root
	}

	// Set up mock API to return these posts
	api := suite.plugin.API.(*plugintest.API)
	api.On("GetPost", threadRootID).Return(threadRootPost, nil)
	api.On("GetPost", threadReplyID).Return(threadReplyPost, nil)

	// Test the thread root resolution function
	bridge := suite.plugin.matrixToMattermostBridge
	// Use mock logger for bridge operations
	bridge.logger = &testLogger{t: t}

	// Test 1: Thread root should return itself
	result1 := bridge.getThreadRootFromPostID(threadRootID)
	assert.Equal(t, threadRootID, result1, "Thread root should return its own ID")

	// Test 2: Thread reply should return the root ID
	result2 := bridge.getThreadRootFromPostID(threadReplyID)
	assert.Equal(t, threadRootID, result2, "Thread reply should return the root ID")

	t.Logf("✓ Thread root resolution working correctly")
	t.Logf("  - Thread root %s resolves to: %s", threadRootID, result1)
	t.Logf("  - Thread reply %s resolves to: %s", threadReplyID, result2)
}

// TestFileAttachmentThreadMapping tests the specific issue being fixed
func (suite *ThreadMappingIntegrationTestSuite) TestFileAttachmentThreadMapping() {
	t := suite.T()

	// This test validates the core fix: when Matrix users reply to file attachments,
	// the reply should be properly threaded in Mattermost

	threadRootID := model.NewId()
	postWithFileID := model.NewId()

	// Create a post with file attachment that's part of a thread
	postWithFile := &model.Post{
		Id:        postWithFileID,
		ChannelId: suite.testChannelID,
		UserId:    suite.testUserID,
		Message:   "Here's the document you requested",
		RootId:    threadRootID, // This post is a reply in an existing thread
		FileIds:   []string{"file_123"},
	}

	// Mock the thread root
	threadRoot := &model.Post{
		Id:        threadRootID,
		ChannelId: suite.testChannelID,
		UserId:    suite.testUserID,
		Message:   "Can you send me the quarterly report?",
		RootId:    "", // This is the thread root
	}

	// Set up mocks
	api := suite.plugin.API.(*plugintest.API)
	api.On("GetPost", threadRootID).Return(threadRoot, nil)
	api.On("GetPost", postWithFileID).Return(postWithFile, nil)

	bridge := suite.plugin.matrixToMattermostBridge
	// Use mock logger for bridge operations
	bridge.logger = &testLogger{t: t}

	// Test the core thread resolution functionality
	// This is the key fix: resolving the proper thread root for threaded posts with files
	resolvedThreadRoot := bridge.getThreadRootFromPostID(postWithFileID)
	assert.Equal(t, threadRootID, resolvedThreadRoot,
		"Matrix reply to file attachment should resolve to original thread root")

	t.Logf("✓ File attachment thread mapping working correctly")
	t.Logf("  - Post with file %s is part of thread root: %s", postWithFileID, threadRootID)
	t.Logf("  - Thread resolution correctly maps to root: %s", resolvedThreadRoot)
	t.Logf("✓ Fix ensures Matrix replies to file attachments maintain thread context")
}

// Run the test suite
func TestThreadMappingIntegration(t *testing.T) {
	suite.Run(t, new(ThreadMappingIntegrationTestSuite))
}
