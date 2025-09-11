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

// MatrixSyncTestSuite contains integration tests for syncing to Matrix
type MatrixSyncTestSuite struct {
	suite.Suite
	matrixContainer *matrixtest.Container
	plugin          *Plugin
	testChannelID   string
	testUserID      string
	testRoomID      string
	testGhostUserID string
	validator       *matrixtest.EventValidation
}

// SetupSuite starts the Matrix container before running tests
func (suite *MatrixSyncTestSuite) SetupSuite() {
	suite.matrixContainer = matrixtest.StartMatrixContainer(suite.T(), matrixtest.DefaultMatrixConfig())
}

// TearDownSuite cleans up the Matrix container after tests
func (suite *MatrixSyncTestSuite) TearDownSuite() {
	if suite.matrixContainer != nil {
		suite.matrixContainer.Cleanup(suite.T())
	}
}

// SetupTest prepares each test with fresh plugin instance
func (suite *MatrixSyncTestSuite) SetupTest() {
	// Create mock API
	api := &plugintest.API{}

	// Set up test data - create fresh room for each test to ensure isolation
	// The CreateRoom method now includes automatic throttling to prevent rate limits
	suite.testChannelID = model.NewId()
	suite.testUserID = model.NewId()
	// Use unique room name to avoid alias conflicts between tests
	uniqueRoomName := fmt.Sprintf("Sync Test Room %s", suite.testChannelID[:8])
	suite.testRoomID = suite.matrixContainer.CreateRoom(suite.T(), uniqueRoomName)
	suite.testGhostUserID = "@_mattermost_" + suite.testUserID + ":" + suite.matrixContainer.ServerDomain

	// Create plugin instance
	suite.plugin = &Plugin{
		remoteID: "test-remote-id",
	}
	suite.plugin.SetAPI(api)

	// Initialize kvstore with in-memory implementation for testing
	suite.plugin.kvstore = NewMemoryKVStore()

	// Initialize required plugin components
	suite.plugin.pendingFiles = NewPendingFileTracker()
	suite.plugin.postTracker = NewPostTracker(DefaultPostTrackerMaxEntries)

	// Create Matrix client pointing to test container
	suite.plugin.matrixClient = createMatrixClientWithTestLogger(
		suite.T(),
		suite.matrixContainer.ServerURL,
		suite.matrixContainer.ASToken,
		suite.plugin.remoteID,
	)
	// Set explicit server domain for testing
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

	// Initialize bridges for testing
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
func (suite *MatrixSyncTestSuite) setupMockAPI(api *plugintest.API) {
	setupBasicMocks(api, suite.testUserID)
}

// TestBasicMessageSync tests syncing a basic text message to Matrix
func (suite *MatrixSyncTestSuite) TestBasicMessageSync() {
	// Create test post
	post := &model.Post{
		Id:        model.NewId(),
		UserId:    suite.testUserID,
		ChannelId: suite.testChannelID,
		Message:   "Hello Matrix world!",
		CreateAt:  time.Now().UnixMilli(),
	}

	// Sync post to Matrix
	err := suite.plugin.mattermostToMatrixBridge.SyncPostToMatrix(post, suite.testChannelID)
	require.NoError(suite.T(), err)

	// Wait for Matrix to process the message with polling
	var messageEvent *matrixtest.Event
	require.Eventually(suite.T(), func() bool {
		events := suite.matrixContainer.GetRoomEvents(suite.T(), suite.testRoomID)
		messageEvent = matrixtest.FindEventByPostID(events, post.Id)
		return messageEvent != nil
	}, 10*time.Second, 500*time.Millisecond, "Should find synced message event within timeout")

	// Validate the message event
	suite.validator.ValidateMessageEvent(*messageEvent, post)
}

// TestMarkdownMessageSync tests syncing a message with Markdown formatting
func (suite *MatrixSyncTestSuite) TestMarkdownMessageSync() {
	// Create test post with Markdown
	post := &model.Post{
		Id:        model.NewId(),
		UserId:    suite.testUserID,
		ChannelId: suite.testChannelID,
		Message:   "**Bold text** and *italic text* and `code`",
		CreateAt:  time.Now().UnixMilli(),
	}

	// Sync post to Matrix
	err := suite.plugin.mattermostToMatrixBridge.SyncPostToMatrix(post, suite.testChannelID)
	require.NoError(suite.T(), err)

	// Wait for processing with polling
	var messageEvent *matrixtest.Event
	require.Eventually(suite.T(), func() bool {
		events := suite.matrixContainer.GetRoomEvents(suite.T(), suite.testRoomID)
		messageEvent = matrixtest.FindEventByPostID(events, post.Id)
		return messageEvent != nil
	}, 10*time.Second, 500*time.Millisecond, "Should find message event within timeout")

	// Validate basic message structure
	suite.validator.ValidateMessageEvent(*messageEvent, post)

	// Verify HTML formatting was applied
	formattedBody, hasFormatted := messageEvent.Content["formatted_body"].(string)
	require.True(suite.T(), hasFormatted, "Markdown message should have formatted_body")

	// Check HTML conversion
	assert.Contains(suite.T(), formattedBody, "<strong>Bold text</strong>", "Should convert **bold** to HTML")
	assert.Contains(suite.T(), formattedBody, "<em>italic text</em>", "Should convert *italic* to HTML")
	assert.Contains(suite.T(), formattedBody, "<code>code</code>", "Should convert `code` to HTML")
	assert.Equal(suite.T(), "org.matrix.custom.html", messageEvent.Content["format"], "Should specify HTML format")
}

// TestMessageWithMentions tests syncing a message with @mentions
func (suite *MatrixSyncTestSuite) TestMessageWithMentions() {
	// Set up mentioned user
	mentionedUserID := model.NewId()
	mentionedGhostUserID := "@_mattermost_" + mentionedUserID + ":" + suite.matrixContainer.ServerDomain

	// Mock API for mentioned user
	api := suite.plugin.API.(*plugintest.API)
	setupMentionMocks(api, mentionedUserID, "mentioned_user")

	// Create test post with mention
	post := &model.Post{
		Id:        model.NewId(),
		UserId:    suite.testUserID,
		ChannelId: suite.testChannelID,
		Message:   "Hello @mentioned_user, how are you?",
		CreateAt:  time.Now().UnixMilli(),
	}

	// Sync post to Matrix
	err := suite.plugin.mattermostToMatrixBridge.SyncPostToMatrix(post, suite.testChannelID)
	require.NoError(suite.T(), err)

	// Wait for processing with polling
	var messageEvent *matrixtest.Event
	require.Eventually(suite.T(), func() bool {
		events := suite.matrixContainer.GetRoomEvents(suite.T(), suite.testRoomID)
		messageEvent = matrixtest.FindEventByPostID(events, post.Id)
		return messageEvent != nil
	}, 10*time.Second, 500*time.Millisecond, "Should find message event within timeout")

	// Validate mention processing
	suite.validator.ValidateMessageWithMentions(*messageEvent, post, 1)

	// Verify specific mention details
	content := messageEvent.Content
	mentions := content["m.mentions"].(map[string]any)
	userIDs := mentions["user_ids"].([]any)
	assert.Contains(suite.T(), userIDs, mentionedGhostUserID, "Should mention correct ghost user")
}

// TestThreadedMessage tests syncing a threaded reply message
func (suite *MatrixSyncTestSuite) TestThreadedMessage() {
	// First, create a parent message
	now := time.Now().UnixMilli()
	parentPost := &model.Post{
		Id:        model.NewId(),
		UserId:    suite.testUserID,
		ChannelId: suite.testChannelID,
		Message:   "Parent message",
		CreateAt:  now,
		UpdateAt:  now,
	}

	// Sync parent post
	err := suite.plugin.mattermostToMatrixBridge.SyncPostToMatrix(parentPost, suite.testChannelID)
	require.NoError(suite.T(), err)

	// Get parent event ID from Matrix with polling
	var parentEvent *matrixtest.Event
	require.Eventually(suite.T(), func() bool {
		events := suite.matrixContainer.GetRoomEvents(suite.T(), suite.testRoomID)
		parentEvent = matrixtest.FindEventByPostID(events, parentPost.Id)
		return parentEvent != nil
	}, 10*time.Second, 500*time.Millisecond, "Should find parent event within timeout")
	parentEventID := parentEvent.EventID

	// Post tracking will be handled by the in-memory KV store
	api := suite.plugin.API.(*plugintest.API)

	// Create threaded reply post
	replyNow := time.Now().UnixMilli()
	replyPost := &model.Post{
		Id:        model.NewId(),
		UserId:    suite.testUserID,
		ChannelId: suite.testChannelID,
		RootId:    parentPost.Id, // This makes it a thread reply
		Message:   "Reply in thread",
		CreateAt:  replyNow,
		UpdateAt:  replyNow,
	}

	// Mock getting parent post for threading
	api.On("GetPost", parentPost.Id).Return(parentPost, nil)

	// Mock the parent post having Matrix event ID property
	parentPost.Props = map[string]any{
		"matrix_event_id_localhost": parentEventID,
	}

	// Sync reply post
	err = suite.plugin.mattermostToMatrixBridge.SyncPostToMatrix(replyPost, suite.testChannelID)
	require.NoError(suite.T(), err)

	// Verify threaded message in Matrix with polling
	var replyEvent *matrixtest.Event
	require.Eventually(suite.T(), func() bool {
		events := suite.matrixContainer.GetRoomEvents(suite.T(), suite.testRoomID)
		replyEvent = matrixtest.FindEventByPostID(events, replyPost.Id)
		return replyEvent != nil
	}, 10*time.Second, 500*time.Millisecond, "Should find reply event within timeout")

	// Validate threading
	suite.validator.ValidateThreadedMessage(*replyEvent, replyPost, parentEventID)
}

// TestMessageEdit tests editing an existing Matrix message
func (suite *MatrixSyncTestSuite) TestMessageEdit() {
	// Create and sync original post
	now := time.Now().UnixMilli()
	originalPost := &model.Post{
		Id:        model.NewId(),
		UserId:    suite.testUserID,
		ChannelId: suite.testChannelID,
		Message:   "Original message",
		CreateAt:  now,
		UpdateAt:  now, // Important: set UpdateAt for proper edit detection
	}

	err := suite.plugin.mattermostToMatrixBridge.SyncPostToMatrix(originalPost, suite.testChannelID)
	require.NoError(suite.T(), err)

	// Get original event from Matrix with polling
	var originalEvent *matrixtest.Event
	require.Eventually(suite.T(), func() bool {
		events := suite.matrixContainer.GetRoomEvents(suite.T(), suite.testRoomID)
		originalEvent = matrixtest.FindEventByPostID(events, originalPost.Id)
		return originalEvent != nil
	}, 10*time.Second, 500*time.Millisecond, "Should find original event within timeout")
	originalEventID := originalEvent.EventID

	// The first sync would have updated the post with Matrix event ID and stored UpdateAt in tracker
	// We need to simulate this by updating our original post to match what would happen
	// Note: extractServerDomain() extracts "localhost" from the server URL, not the server domain
	originalPost.Props = map[string]any{
		"matrix_event_id_localhost": originalEventID,
	}

	// Wait a bit to ensure different timestamp
	time.Sleep(100 * time.Millisecond)

	// Create edited version of post - this simulates a real user edit
	editedPost := &model.Post{
		Id:        originalPost.Id, // Same ID for edit
		UserId:    suite.testUserID,
		ChannelId: suite.testChannelID,
		Message:   "Edited message content",
		CreateAt:  originalPost.CreateAt,  // Keep original create time
		UpdateAt:  time.Now().UnixMilli(), // New update time (must be different from tracked time)
		Props:     originalPost.Props,     // Include Matrix event ID
	}

	// Post tracking will be handled by the in-memory KV store

	// Sync edited post
	err = suite.plugin.mattermostToMatrixBridge.SyncPostToMatrix(editedPost, suite.testChannelID)
	require.NoError(suite.T(), err)

	// Verify edit event in Matrix with polling
	var editEvent *matrixtest.Event
	require.Eventually(suite.T(), func() bool {
		events := suite.matrixContainer.GetRoomEvents(suite.T(), suite.testRoomID)
		// Look for edit events (m.room.message with m.relates_to.rel_type = "m.replace")
		for _, event := range events {
			if event.Type == "m.room.message" {
				if relatesTo, hasRel := event.Content["m.relates_to"].(map[string]any); hasRel {
					if relType, ok := relatesTo["rel_type"].(string); ok && relType == "m.replace" {
						editEvent = &event
						return true
					}
				}
			}
		}
		return false
	}, 10*time.Second, 500*time.Millisecond, "Should find edit event within timeout")

	// Validate edit event
	suite.validator.ValidateEditEvent(*editEvent, originalEventID, "Edited message content")
}

// TestReactionSync tests syncing reactions to Matrix
func (suite *MatrixSyncTestSuite) TestReactionSync() {
	// Create and sync original post
	now := time.Now().UnixMilli()
	post := &model.Post{
		Id:        model.NewId(),
		UserId:    suite.testUserID,
		ChannelId: suite.testChannelID,
		Message:   "Message to react to",
		CreateAt:  now,
		UpdateAt:  now,
	}

	err := suite.plugin.mattermostToMatrixBridge.SyncPostToMatrix(post, suite.testChannelID)
	require.NoError(suite.T(), err)

	// Get Matrix event ID with polling
	var messageEvent *matrixtest.Event
	require.Eventually(suite.T(), func() bool {
		events := suite.matrixContainer.GetRoomEvents(suite.T(), suite.testRoomID)
		messageEvent = matrixtest.FindEventByPostID(events, post.Id)
		return messageEvent != nil
	}, 10*time.Second, 500*time.Millisecond, "Should find message event within timeout")
	eventID := messageEvent.EventID

	// Set Matrix event ID in post properties
	post.Props = map[string]any{
		"matrix_event_id_localhost": eventID,
	}

	// Mock API for reaction
	api := suite.plugin.API.(*plugintest.API)
	api.On("GetPost", post.Id).Return(post, nil)

	// Create reaction
	reaction := &model.Reaction{
		UserId:    suite.testUserID,
		PostId:    post.Id,
		EmojiName: "thumbsup",
		CreateAt:  time.Now().UnixMilli(),
	}

	// Sync reaction to Matrix
	err = suite.plugin.mattermostToMatrixBridge.SyncReactionToMatrix(reaction, suite.testChannelID)
	require.NoError(suite.T(), err)

	// Verify reaction in Matrix with polling
	var reactionEvent *matrixtest.Event
	require.Eventually(suite.T(), func() bool {
		events := suite.matrixContainer.GetRoomEvents(suite.T(), suite.testRoomID)
		reactionEvent = matrixtest.FindEventByType(events, "m.reaction")
		return reactionEvent != nil
	}, 10*time.Second, 500*time.Millisecond, "Should find reaction event within timeout")

	// Validate reaction
	expectedEmoji := "👍" // thumbsup converts to thumbs up emoji
	suite.validator.ValidateReactionEvent(*reactionEvent, eventID, expectedEmoji)
}

// Run the test suite
func TestMatrixSyncTestSuite(t *testing.T) {
	suite.Run(t, new(MatrixSyncTestSuite))
}
