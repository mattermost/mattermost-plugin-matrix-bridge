package matrix

import (
	"fmt"
	"strings"
	"testing"

	matrixtest "github.com/mattermost/mattermost-plugin-matrix-bridge/testcontainers/matrix"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
)

// MatrixClientTestSuite contains integration tests for Matrix client operations.
type MatrixClientTestSuite struct {
	suite.Suite
	matrixContainer *matrixtest.MatrixContainer
	client          *Client
}

// SetupSuite starts the Matrix container before running tests
func (suite *MatrixClientTestSuite) SetupSuite() {
	suite.matrixContainer = matrixtest.StartMatrixContainer(suite.T(), matrixtest.DefaultMatrixConfig())
}

// TearDownSuite cleans up the Matrix container after tests
func (suite *MatrixClientTestSuite) TearDownSuite() {
	if suite.matrixContainer != nil {
		suite.matrixContainer.Cleanup(suite.T())
	}
}

// SetupTest prepares each test with fresh Matrix client and ensures AS bot is created
func (suite *MatrixClientTestSuite) SetupTest() {
	// Create a test room to ensure AS bot user is provisioned
	_ = suite.matrixContainer.CreateRoom(suite.T(), "AS Bot Provisioning Room")

	// Create Matrix client
	suite.client = NewClientWithLogger(
		suite.matrixContainer.ServerURL,
		suite.matrixContainer.ASToken,
		"test-remote-id",
		NewTestLogger(suite.T()),
	)
	suite.client.SetServerDomain(suite.matrixContainer.ServerDomain)
}

// TestMatrixClientOperations tests core Matrix client operations that affect change
// and then query the server to verify the changes were applied correctly.
func (suite *MatrixClientTestSuite) TestMatrixClientOperations() {
	// Test server connectivity first
	suite.Run("TestConnection", func() {
		err := suite.client.TestConnection()
		require.NoError(suite.T(), err, "Should connect to Matrix server")
	})

	// Test Application Service permissions
	suite.Run("TestApplicationServicePermissions", func() {
		err := suite.client.TestApplicationServicePermissions()
		require.NoError(suite.T(), err, "Should have proper AS permissions")
	})

	// Test server information retrieval
	suite.Run("GetServerInfo", func() {
		serverInfo, err := suite.client.GetServerInfo()
		require.NoError(suite.T(), err, "Should retrieve server info")
		assert.NotEmpty(suite.T(), serverInfo.Name, "Server name should not be empty")
		suite.T().Logf("Server: %s %s", serverInfo.Name, serverInfo.Version)
	})

	// Run room operation tests
	suite.Run("RoomOperations", func() {
		suite.testRoomOperations()
	})

	// Run user operation tests
	suite.Run("UserOperations", func() {
		suite.testUserOperations()
	})

	// Run message operation tests
	suite.Run("MessageOperations", func() {
		suite.testMessageOperations()
	})

	// Run media operation tests
	suite.Run("MediaOperations", func() {
		suite.testMediaOperations()
	})
}

// testRoomOperations tests room creation, joining, and management operations
func (suite *MatrixClientTestSuite) testRoomOperations() {
	suite.Run("CreateRoom", func() {
		// Create a room and verify it exists
		roomName := "Test Room"
		roomTopic := "Integration test room"
		mattermostChannelID := "test-channel-123"

		roomIdentifier, err := suite.client.CreateRoom(roomName, roomTopic, suite.matrixContainer.ServerDomain, true, mattermostChannelID)
		require.NoError(suite.T(), err, "Should create room successfully")
		assert.NotEmpty(suite.T(), roomIdentifier, "Room identifier should not be empty")
		suite.T().Logf("Created room: %s", roomIdentifier)
	})

	suite.Run("JoinRoom", func() {
		// Create a room first
		roomIdentifier, err := suite.client.CreateRoom("Join Test Room", "Testing room joining", suite.matrixContainer.ServerDomain, false, "test-join-channel")
		require.NoError(suite.T(), err, "Should create room for join test")

		// Join the room as the AS bot
		err = suite.client.JoinRoom(roomIdentifier)
		require.NoError(suite.T(), err, "Should join room successfully")
	})

	suite.Run("ResolveRoomAlias", func() {
		// Create a room first
		roomIdentifier, err := suite.client.CreateRoom("Resolve Test Room", "Testing alias resolution", suite.matrixContainer.ServerDomain, false, "test-resolve-channel")
		require.NoError(suite.T(), err, "Should create room for alias test")

		// Resolve the alias to room ID
		roomID, err := suite.client.ResolveRoomAlias(roomIdentifier)
		require.NoError(suite.T(), err, "Should resolve room alias")
		assert.NotEmpty(suite.T(), roomID, "Room ID should not be empty")
		assert.True(suite.T(), strings.HasPrefix(roomID, "!"), "Room ID should start with !")
		suite.T().Logf("Resolved %s to %s", roomIdentifier, roomID)
	})

	suite.Run("CreateDirectRoom", func() {
		// Create ghost users for DM
		ghostUser1, err := suite.client.CreateGhostUser("dm-user-1", "DM User 1", nil, "")
		require.NoError(suite.T(), err, "Should create first ghost user")

		ghostUser2, err := suite.client.CreateGhostUser("dm-user-2", "DM User 2", nil, "")
		require.NoError(suite.T(), err, "Should create second ghost user")

		// Create direct room between the users
		ghostUserIDs := []string{ghostUser1.UserID, ghostUser2.UserID}
		roomID, err := suite.client.CreateDirectRoom(ghostUserIDs, "Test Direct Message")
		require.NoError(suite.T(), err, "Should create direct room")
		assert.NotEmpty(suite.T(), roomID, "Room ID should not be empty")
		assert.True(suite.T(), strings.HasPrefix(roomID, "!"), "Room ID should start with !")
		suite.T().Logf("Created DM room: %s", roomID)
	})
}

// testUserOperations tests user creation and profile management operations
func (suite *MatrixClientTestSuite) testUserOperations() {
	suite.Run("CreateGhostUser", func() {
		mattermostUserID := "test-user-123"
		displayName := "Test User"
		avatarData := []byte("fake-avatar-data")
		avatarContentType := "image/png"

		ghostUser, err := suite.client.CreateGhostUser(mattermostUserID, displayName, avatarData, avatarContentType)
		require.NoError(suite.T(), err, "Should create ghost user")
		assert.NotNil(suite.T(), ghostUser, "Ghost user should not be nil")
		assert.NotEmpty(suite.T(), ghostUser.UserID, "Ghost user ID should not be empty")
		assert.Contains(suite.T(), ghostUser.UserID, mattermostUserID, "Ghost user ID should contain Mattermost user ID")
		suite.T().Logf("Created ghost user: %s", ghostUser.UserID)

		// Verify user profile was set correctly
		profile, err := suite.client.GetUserProfile(ghostUser.UserID)
		require.NoError(suite.T(), err, "Should get user profile")
		assert.Equal(suite.T(), displayName, profile.DisplayName, "Display name should match")
		assert.NotEmpty(suite.T(), profile.AvatarURL, "Avatar URL should be set")
	})

	suite.Run("UpdateUserProfile", func() {
		// Create a ghost user first
		mattermostUserID := "test-user-456"
		ghostUser, err := suite.client.CreateGhostUser(mattermostUserID, "Original Name", nil, "")
		require.NoError(suite.T(), err, "Should create ghost user for profile test")

		// Update display name
		newDisplayName := "Updated Display Name"
		err = suite.client.SetDisplayName(ghostUser.UserID, newDisplayName)
		require.NoError(suite.T(), err, "Should update display name")

		// Verify the change
		profile, err := suite.client.GetUserProfile(ghostUser.UserID)
		require.NoError(suite.T(), err, "Should get updated profile")
		assert.Equal(suite.T(), newDisplayName, profile.DisplayName, "Display name should be updated")

		// Update avatar
		newAvatarData := []byte("new-fake-avatar-data")
		err = suite.client.UpdateGhostUserAvatar(ghostUser.UserID, newAvatarData, "image/jpeg")
		require.NoError(suite.T(), err, "Should update avatar")

		// Verify avatar was updated
		updatedProfile, err := suite.client.GetUserProfile(ghostUser.UserID)
		require.NoError(suite.T(), err, "Should get profile after avatar update")
		assert.NotEmpty(suite.T(), updatedProfile.AvatarURL, "Avatar URL should be set")
		// Avatar URL should be different from the original (if there was one)
		if profile.AvatarURL != "" {
			assert.NotEqual(suite.T(), profile.AvatarURL, updatedProfile.AvatarURL, "Avatar URL should be different")
		}
	})
}

// testMessageOperations tests message sending, editing, reactions, and redactions
func (suite *MatrixClientTestSuite) testMessageOperations() {
	// Create a test room and ghost user for message tests
	roomIdentifier, err := suite.client.CreateRoom("Message Test Room", "Testing messages", suite.matrixContainer.ServerDomain, false, "test-message-channel")
	require.NoError(suite.T(), err, "Should create room for message tests")

	roomID, err := suite.client.ResolveRoomAlias(roomIdentifier)
	require.NoError(suite.T(), err, "Should resolve room identifier")

	// Create ghost user for sending messages
	mattermostUserID := "test-msg-user"
	ghostUser, err := suite.client.CreateGhostUser(mattermostUserID, "Message Test User", nil, "")
	require.NoError(suite.T(), err, "Should create ghost user for messages")

	// Join the ghost user to the room
	err = suite.client.JoinRoomAsUser(roomID, ghostUser.UserID)
	require.NoError(suite.T(), err, "Should join ghost user to room")

	suite.Run("SendMessage", func() {
		// Send a text message
		messageReq := MessageRequest{
			RoomID:      roomID,
			GhostUserID: ghostUser.UserID,
			Message:     "Hello, this is a test message!",
			HTMLMessage: "<p>Hello, this is a <strong>test</strong> message!</p>",
			PostID:      "test-post-123",
		}

		response, err := suite.client.SendMessage(messageReq)
		require.NoError(suite.T(), err, "Should send message")
		assert.NotEmpty(suite.T(), response.EventID, "Event ID should not be empty")
		suite.T().Logf("Sent message with event ID: %s", response.EventID)

		// Verify the message was sent by retrieving it
		event, err := suite.client.GetEvent(roomID, response.EventID)
		require.NoError(suite.T(), err, "Should retrieve sent message")
		assert.Equal(suite.T(), "m.room.message", event["type"], "Event type should be m.room.message")

		content, ok := event["content"].(map[string]any)
		require.True(suite.T(), ok, "Event should have content")
		assert.Equal(suite.T(), messageReq.Message, content["body"], "Message body should match")
		assert.Equal(suite.T(), messageReq.HTMLMessage, content["formatted_body"], "HTML message should match")
		assert.Equal(suite.T(), messageReq.PostID, content["mattermost_post_id"], "Post ID should match")
	})

	suite.Run("EditMessage", func() {
		// Send original message
		messageReq := MessageRequest{
			RoomID:      roomID,
			GhostUserID: ghostUser.UserID,
			Message:     "Original message",
			PostID:      "test-edit-post",
		}

		response, err := suite.client.SendMessage(messageReq)
		require.NoError(suite.T(), err, "Should send original message")

		// Edit the message
		newMessage := "Edited message content"
		newHTMLMessage := "<p>Edited <em>message</em> content</p>"
		editResponse, err := suite.client.EditMessageAsGhost(roomID, response.EventID, newMessage, newHTMLMessage, ghostUser.UserID)
		require.NoError(suite.T(), err, "Should edit message")
		assert.NotEmpty(suite.T(), editResponse.EventID, "Edit event ID should not be empty")

		// Verify the edit was applied by retrieving the edit event
		editEvent, err := suite.client.GetEvent(roomID, editResponse.EventID)
		require.NoError(suite.T(), err, "Should retrieve edit event")

		content, ok := editEvent["content"].(map[string]any)
		require.True(suite.T(), ok, "Edit event should have content")

		newContent, ok := content["m.new_content"].(map[string]any)
		require.True(suite.T(), ok, "Edit should have m.new_content")
		assert.Equal(suite.T(), newMessage, newContent["body"], "Edited message body should match")
		assert.Equal(suite.T(), newHTMLMessage, newContent["formatted_body"], "Edited HTML should match")
	})

	suite.Run("SendReaction", func() {
		// Send a message to react to
		messageReq := MessageRequest{
			RoomID:      roomID,
			GhostUserID: ghostUser.UserID,
			Message:     "React to this message!",
			PostID:      "test-reaction-post",
		}

		response, err := suite.client.SendMessage(messageReq)
		require.NoError(suite.T(), err, "Should send message to react to")

		// Send a reaction
		emoji := "👍"
		reactionResponse, err := suite.client.SendReactionAsGhost(roomID, response.EventID, emoji, ghostUser.UserID)
		require.NoError(suite.T(), err, "Should send reaction")
		assert.NotEmpty(suite.T(), reactionResponse.EventID, "Reaction event ID should not be empty")

		// Verify the reaction by getting relations
		relations, err := suite.client.GetEventRelationsAsUser(roomID, response.EventID, ghostUser.UserID)
		require.NoError(suite.T(), err, "Should get event relations")
		assert.NotEmpty(suite.T(), relations, "Should have at least one relation (the reaction)")

		// Find our reaction in the relations
		foundReaction := false
		for _, relation := range relations {
			if relation["type"] == "m.reaction" {
				content, ok := relation["content"].(map[string]any)
				if ok {
					relatesTo, ok := content["m.relates_to"].(map[string]any)
					if ok && relatesTo["key"] == emoji {
						foundReaction = true
						break
					}
				}
			}
		}
		assert.True(suite.T(), foundReaction, "Should find the reaction in relations")
	})

	suite.Run("RedactEvent", func() {
		// Send a message to redact
		messageReq := MessageRequest{
			RoomID:      roomID,
			GhostUserID: ghostUser.UserID,
			Message:     "This message will be deleted",
			PostID:      "test-redact-post",
		}

		response, err := suite.client.SendMessage(messageReq)
		require.NoError(suite.T(), err, "Should send message to redact")

		// Redact the message
		redactResponse, err := suite.client.RedactEventAsGhost(roomID, response.EventID, ghostUser.UserID)
		require.NoError(suite.T(), err, "Should redact message")
		assert.NotEmpty(suite.T(), redactResponse.EventID, "Redaction event ID should not be empty")

		// Verify the original message is now redacted
		// Note: Redacted events still exist but with limited content
		event, err := suite.client.GetEvent(roomID, response.EventID)
		require.NoError(suite.T(), err, "Should still be able to get redacted event")

		// Check if the event has been redacted (content should be empty or limited)
		content, hasContent := event["content"].(map[string]any)
		if hasContent {
			// If content exists, it should be empty or only contain redaction-safe fields
			body, hasBody := content["body"].(string)
			if hasBody {
				// Redacted events typically have empty body or placeholder text
				assert.True(suite.T(), body == "" || strings.Contains(body, "redacted"),
					"Redacted message should have empty or redacted body")
			}
		}
	})
}

// testMediaOperations tests media upload and download operations
func (suite *MatrixClientTestSuite) testMediaOperations() {
	suite.Run("UploadAndDownloadMedia", func() {
		// Test data
		testData := []byte("This is test file content for upload/download testing")
		filename := "test-file.txt"
		contentType := "text/plain"

		// Upload media
		mxcURI, err := suite.client.UploadMedia(testData, filename, contentType)
		require.NoError(suite.T(), err, "Should upload media")
		assert.NotEmpty(suite.T(), mxcURI, "MXC URI should not be empty")
		assert.True(suite.T(), strings.HasPrefix(mxcURI, "mxc://"), "Should return valid MXC URI")
		suite.T().Logf("Uploaded media: %s", mxcURI)

		// Download the media back
		downloadedData, err := suite.client.DownloadFile(mxcURI, int64(len(testData)*2), "text/")
		require.NoError(suite.T(), err, "Should download media")
		assert.Equal(suite.T(), testData, downloadedData, "Downloaded data should match uploaded data")
	})

	suite.Run("UploadAvatar", func() {
		// Test avatar upload
		avatarData := []byte("fake-avatar-image-data")
		contentType := "image/png"

		mxcURI, err := suite.client.UploadAvatarFromData(avatarData, contentType)
		require.NoError(suite.T(), err, "Should upload avatar")
		assert.NotEmpty(suite.T(), mxcURI, "Avatar MXC URI should not be empty")
		assert.True(suite.T(), strings.HasPrefix(mxcURI, "mxc://"), "Should return valid MXC URI")

		// Verify we can download it back
		downloadedData, err := suite.client.DownloadFile(mxcURI, int64(len(avatarData)*2), "image/")
		require.NoError(suite.T(), err, "Should download avatar")
		assert.Equal(suite.T(), avatarData, downloadedData, "Downloaded avatar should match uploaded data")
	})
}

// BenchmarkMatrixClientOperations provides performance benchmarks for key operations
func BenchmarkMatrixClientOperations(b *testing.B) {
	// Only run benchmarks if explicitly requested
	if testing.Short() {
		b.Skip("Skipping benchmark in short mode")
	}

	matrixContainer := matrixtest.StartMatrixContainer(&testing.T{}, matrixtest.DefaultMatrixConfig())
	defer matrixContainer.Cleanup(&testing.T{})

	// Create a test room to ensure AS bot user is provisioned
	_ = matrixContainer.CreateRoom(&testing.T{}, "AS Bot Provisioning Room")

	client := NewClientWithLogger(
		matrixContainer.ServerURL,
		matrixContainer.ASToken,
		"bench-remote-id",
		NewTestLogger(&testing.T{}),
	)
	client.SetServerDomain(matrixContainer.ServerDomain)

	b.Run("CreateRoom", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			roomName := fmt.Sprintf("Benchmark Room %d", i)
			_, err := client.CreateRoom(roomName, "Benchmark test", matrixContainer.ServerDomain, false, fmt.Sprintf("bench-channel-%d", i))
			if err != nil {
				b.Fatalf("Failed to create room: %v", err)
			}
		}
	})

	b.Run("SendMessage", func(b *testing.B) {
		// Create room and user once
		roomID, _ := client.CreateRoom("Benchmark Messages", "Message benchmark", matrixContainer.ServerDomain, false, "bench-msg-channel")
		resolvedRoomID, _ := client.ResolveRoomAlias(roomID)
		ghostUser, _ := client.CreateGhostUser("bench-user", "Bench User", nil, "")
		err := client.JoinRoomAsUser(resolvedRoomID, ghostUser.UserID)
		require.NoError(&testing.T{}, err, "Should join ghost user to room")

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			messageReq := MessageRequest{
				RoomID:      resolvedRoomID,
				GhostUserID: ghostUser.UserID,
				Message:     fmt.Sprintf("Benchmark message %d", i),
				PostID:      fmt.Sprintf("bench-post-%d", i),
			}
			_, err := client.SendMessage(messageReq)
			if err != nil {
				b.Fatalf("Failed to send message: %v", err)
			}
		}
	})
}

// TestMatrixClientErrorHandling tests error handling and edge cases
func (suite *MatrixClientTestSuite) TestMatrixClientErrorHandling() {
	suite.Run("EmptyTokenClient", func() {
		// Test client with empty token
		emptyTokenClient := NewClientWithLogger(
			suite.matrixContainer.ServerURL,
			"", // Empty token
			"test-remote-id",
			NewTestLogger(suite.T()),
		)

		// Should fail operations that require authentication
		_, err := emptyTokenClient.CreateRoom("Test", "Test", suite.matrixContainer.ServerDomain, false, "test")
		assert.Error(suite.T(), err, "Should fail with empty token")
		assert.Contains(suite.T(), err.Error(), "not configured", "Error should mention configuration issue")
	})

	suite.Run("InvalidServerURL", func() {
		// Test client with invalid server URL
		invalidURLClient := NewClientWithLogger(
			"http://nonexistent.invalid:1234",
			suite.matrixContainer.ASToken,
			"test-remote-id",
			NewTestLogger(suite.T()),
		)

		err := invalidURLClient.TestConnection()
		assert.Error(suite.T(), err, "Should fail with invalid server URL")
	})

	suite.Run("InvalidRoomOperations", func() {
		// Test joining non-existent room
		err := suite.client.JoinRoom("!nonexistent:test.matrix.local")
		assert.Error(suite.T(), err, "Should fail to join non-existent room")

		// Test resolving non-existent alias
		_, err = suite.client.ResolveRoomAlias("#nonexistent:test.matrix.local")
		assert.Error(suite.T(), err, "Should fail to resolve non-existent alias")

		// Test creating DM with insufficient users
		_, err = suite.client.CreateDirectRoom([]string{"@user1:test.matrix.local"}, "Test DM")
		assert.Error(suite.T(), err, "Should fail DM creation with only one user")
		assert.Contains(suite.T(), err.Error(), "at least 2 users", "Error should mention user requirement")
	})

	suite.Run("InvalidUserOperations", func() {
		// Test setting display name for non-existent user (should fail)
		err := suite.client.SetDisplayName("@nonexistent:test.matrix.local", "Test Name")
		assert.Error(suite.T(), err, "Should fail to set display name for non-existent user")

		// Test getting profile for non-existent user
		profile, err := suite.client.GetUserProfile("@nonexistent:test.matrix.local")
		require.NoError(suite.T(), err, "Should not error for non-existent user profile") // Matrix returns empty profile
		assert.Empty(suite.T(), profile.DisplayName, "Non-existent user should have empty profile")
	})

	suite.Run("InvalidMessageOperations", func() {
		// Test sending message to non-existent room
		messageReq := MessageRequest{
			RoomID:      "!nonexistent:test.matrix.local",
			GhostUserID: "@_mattermost_test:test.matrix.local",
			Message:     "Test message",
		}

		_, err := suite.client.SendMessage(messageReq)
		assert.Error(suite.T(), err, "Should fail to send message to non-existent room")

		// Test getting non-existent event
		_, err = suite.client.GetEvent("!nonexistent:test.matrix.local", "$nonexistent")
		assert.Error(suite.T(), err, "Should fail to get non-existent event")

		// Test empty message request
		emptyReq := MessageRequest{
			RoomID:      "!test:test.matrix.local",
			GhostUserID: "@_mattermost_test:test.matrix.local",
			// No message content
		}

		_, err = suite.client.SendMessage(emptyReq)
		assert.Error(suite.T(), err, "Should fail with empty message content")
		assert.Contains(suite.T(), err.Error(), "no message content", "Error should mention missing content")
	})

	suite.Run("InvalidMediaOperations", func() {
		// Test download with invalid MXC URI
		_, err := suite.client.DownloadFile("invalid-uri", 1024, "")
		assert.Error(suite.T(), err, "Should fail with invalid MXC URI")
		assert.Contains(suite.T(), err.Error(), "invalid Matrix MXC URI", "Error should mention invalid URI")

		// Test download with malformed MXC URI
		_, err = suite.client.DownloadFile("mxc://", 1024, "")
		assert.Error(suite.T(), err, "Should fail with malformed MXC URI")

		// Test avatar upload with empty data
		_, err = suite.client.UploadAvatarFromData([]byte{}, "image/png")
		assert.Error(suite.T(), err, "Should fail with empty avatar data")
		assert.Contains(suite.T(), err.Error(), "image data is empty", "Error should mention empty data")
	})

	suite.Run("ParameterValidation", func() {
		// Test required parameter validation
		messageReq := MessageRequest{
			// Missing RoomID
			GhostUserID: "@_mattermost_test:test.matrix.local",
			Message:     "Test",
		}

		_, err := suite.client.SendMessage(messageReq)
		assert.Error(suite.T(), err, "Should fail with missing room ID")
		assert.Contains(suite.T(), err.Error(), "room_id is required", "Error should mention room ID")

		messageReq.RoomID = "!test:test.matrix.local"
		messageReq.GhostUserID = "" // Missing ghost user ID

		_, err = suite.client.SendMessage(messageReq)
		assert.Error(suite.T(), err, "Should fail with missing ghost user ID")
		assert.Contains(suite.T(), err.Error(), "ghost_user_id is required", "Error should mention ghost user ID")
	})

	suite.Run("NetworkErrorHandling", func() {
		// Test with client pointing to a port that definitely doesn't exist
		networkFailClient := NewClientWithLogger(
			"http://localhost:99999", // Port that should be unused
			suite.matrixContainer.ASToken,
			"test-remote-id",
			NewTestLogger(suite.T()),
		)

		// All operations should fail with network errors
		err := networkFailClient.TestConnection()
		assert.Error(suite.T(), err, "Should fail with network error")

		_, err = networkFailClient.CreateRoom("Test", "Test", "test.local", false, "test")
		assert.Error(suite.T(), err, "Should fail with network error")

		_, err = networkFailClient.GetServerInfo()
		assert.Error(suite.T(), err, "Should fail with network error")
	})
}

// TestMatrixClientWithFiles tests file attachment handling
func (suite *MatrixClientTestSuite) TestMatrixClientWithFiles() {
	// Create test room and user
	roomIdentifier, err := suite.client.CreateRoom("File Test Room", "Testing file attachments", suite.matrixContainer.ServerDomain, false, "test-file-channel")
	require.NoError(suite.T(), err, "Should create room for file tests")

	roomID, err := suite.client.ResolveRoomAlias(roomIdentifier)
	require.NoError(suite.T(), err, "Should resolve room identifier")

	ghostUser, err := suite.client.CreateGhostUser("test-file-user", "File Test User", nil, "")
	require.NoError(suite.T(), err, "Should create ghost user for file tests")

	err = suite.client.JoinRoomAsUser(roomID, ghostUser.UserID)
	require.NoError(suite.T(), err, "Should join ghost user to room")

	suite.Run("SendMessageWithFiles", func() {
		// Upload test files first
		testFileData := []byte("This is a test file content for attachment testing")
		testImageData := []byte("fake-image-data-for-testing")

		fileMxcURI, err := suite.client.UploadMedia(testFileData, "test-document.txt", "text/plain")
		require.NoError(suite.T(), err, "Should upload test file")

		imageMxcURI, err := suite.client.UploadMedia(testImageData, "test-image.png", "image/png")
		require.NoError(suite.T(), err, "Should upload test image")

		// Send message with file attachments
		messageReq := MessageRequest{
			RoomID:      roomID,
			GhostUserID: ghostUser.UserID,
			Message:     "Here are some file attachments",
			PostID:      "test-file-post-123",
			Files: []FileAttachment{
				{
					Filename: "test-document.txt",
					MxcURI:   fileMxcURI,
					MimeType: "text/plain",
					Size:     int64(len(testFileData)),
				},
				{
					Filename: "test-image.png",
					MxcURI:   imageMxcURI,
					MimeType: "image/png",
					Size:     int64(len(testImageData)),
				},
			},
		}

		response, err := suite.client.SendMessage(messageReq)
		require.NoError(suite.T(), err, "Should send message with files")
		assert.NotEmpty(suite.T(), response.EventID, "Event ID should not be empty")

		// Verify we can download the files back
		downloadedFile, err := suite.client.DownloadFile(fileMxcURI, int64(len(testFileData)*2), "text/")
		require.NoError(suite.T(), err, "Should download file")
		assert.Equal(suite.T(), testFileData, downloadedFile, "Downloaded file should match uploaded data")

		downloadedImage, err := suite.client.DownloadFile(imageMxcURI, int64(len(testImageData)*2), "image/")
		require.NoError(suite.T(), err, "Should download image")
		assert.Equal(suite.T(), testImageData, downloadedImage, "Downloaded image should match uploaded data")
	})

	suite.Run("SendOnlyFiles", func() {
		// Upload test file
		testData := []byte("File-only message test content")
		mxcURI, err := suite.client.UploadMedia(testData, "file-only.txt", "text/plain")
		require.NoError(suite.T(), err, "Should upload test file")

		// Send message with only files (no text)
		messageReq := MessageRequest{
			RoomID:      roomID,
			GhostUserID: ghostUser.UserID,
			// No Message text
			PostID: "test-file-only-post",
			Files: []FileAttachment{
				{
					Filename: "file-only.txt",
					MxcURI:   mxcURI,
					MimeType: "text/plain",
					Size:     int64(len(testData)),
				},
			},
		}

		response, err := suite.client.SendMessage(messageReq)
		require.NoError(suite.T(), err, "Should send file-only message")
		assert.NotEmpty(suite.T(), response.EventID, "Event ID should not be empty")
	})
}

// Test path traversal protection functions
func TestValidatePathComponent(t *testing.T) {
	tests := []struct {
		name      string
		component string
		wantErr   bool
		errMsg    string
	}{
		// Valid cases
		{
			name:      "valid matrix room ID",
			component: "!abc123:matrix.org",
			wantErr:   false,
		},
		{
			name:      "valid matrix event ID",
			component: "$def456:matrix.org",
			wantErr:   false,
		},
		{
			name:      "valid matrix user ID",
			component: "@user:matrix.org",
			wantErr:   false,
		},
		{
			name:      "valid alphanumeric string",
			component: "valid123",
			wantErr:   false,
		},
		{
			name:      "empty string",
			component: "",
			wantErr:   false,
		},
		{
			name:      "special characters",
			component: "test-_.:@#$%",
			wantErr:   false,
		},

		// Path traversal attacks
		{
			name:      "simple path traversal",
			component: "../",
			wantErr:   true,
			errMsg:    "path traversal detected",
		},
		{
			name:      "double path traversal",
			component: "../../",
			wantErr:   true,
			errMsg:    "path traversal detected",
		},
		{
			name:      "path traversal in middle",
			component: "valid/../evil",
			wantErr:   true,
			errMsg:    "path traversal detected",
		},
		{
			name:      "path traversal at start",
			component: "../evil",
			wantErr:   true,
			errMsg:    "path traversal detected",
		},
		{
			name:      "path traversal at end",
			component: "evil/..",
			wantErr:   true,
			errMsg:    "path traversal detected",
		},
		{
			name:      "multiple path traversals",
			component: "../../../etc/passwd",
			wantErr:   true,
			errMsg:    "path traversal detected",
		},
		{
			name:      "disguised path traversal with matrix ID",
			component: "!room../../../admin:matrix.org",
			wantErr:   true,
			errMsg:    "path traversal detected",
		},
		{
			name:      "path traversal in server name",
			component: "../../evil:matrix.org",
			wantErr:   true,
			errMsg:    "path traversal detected",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validatePathComponent(tt.component)

			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errMsg)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestBuildSecureURL(t *testing.T) {
	tests := []struct {
		name           string
		baseURL        string
		pathComponents []string
		expectedURL    string
		wantErr        bool
		errMsg         string
	}{
		// Valid cases
		{
			name:           "simple matrix room path",
			baseURL:        "https://matrix.org/_matrix/client/v3/rooms/",
			pathComponents: []string{"!abc123:matrix.org", "event", "$def456:matrix.org"},
			expectedURL:    "https://matrix.org/_matrix/client/v3/rooms/%21abc123:matrix.org/event/$def456:matrix.org",
			wantErr:        false,
		},
		{
			name:           "simple components",
			baseURL:        "https://example.com/api/",
			pathComponents: []string{"users", "123", "profile"},
			expectedURL:    "https://example.com/api/users/123/profile",
			wantErr:        false,
		},
		{
			name:           "empty components",
			baseURL:        "https://example.com/api/",
			pathComponents: []string{},
			expectedURL:    "https://example.com/api/",
			wantErr:        false,
		},
		{
			name:           "single component with special chars",
			baseURL:        "https://matrix.org/media/",
			pathComponents: []string{"@user:matrix.org"},
			expectedURL:    "https://matrix.org/media/@user:matrix.org",
			wantErr:        false,
		},

		// Path traversal attacks
		{
			name:           "path traversal in first component",
			baseURL:        "https://example.com/api/",
			pathComponents: []string{"../admin", "secret"},
			wantErr:        true,
			errMsg:         "path traversal detected",
		},
		{
			name:           "path traversal in middle component",
			baseURL:        "https://example.com/api/",
			pathComponents: []string{"users", "../../../admin", "secret"},
			wantErr:        true,
			errMsg:         "path traversal detected",
		},
		{
			name:           "path traversal in last component",
			baseURL:        "https://example.com/api/",
			pathComponents: []string{"users", "123", "../../../etc/passwd"},
			wantErr:        true,
			errMsg:         "path traversal detected",
		},
		{
			name:           "multiple path traversals",
			baseURL:        "https://matrix.org/_matrix/client/v3/rooms/",
			pathComponents: []string{"../../../admin", "../../../etc", "passwd"},
			wantErr:        true,
			errMsg:         "path traversal detected",
		},
		{
			name:           "disguised matrix room with traversal",
			baseURL:        "https://matrix.org/_matrix/client/v3/rooms/",
			pathComponents: []string{"!room:../../evil", "event", "$event:matrix.org"},
			wantErr:        true,
			errMsg:         "path traversal detected",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := buildSecureURL(tt.baseURL, tt.pathComponents...)

			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errMsg)
				assert.Empty(t, result)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.expectedURL, result)
			}
		})
	}
}

func TestValidateMXCComponents(t *testing.T) {
	tests := []struct {
		name       string
		serverName string
		mediaID    string
		wantErr    bool
		errMsg     string
	}{
		// Valid cases
		{
			name:       "valid matrix.org server and media ID",
			serverName: "matrix.org",
			mediaID:    "abc123def456",
			wantErr:    false,
		},
		{
			name:       "valid server with port and complex media ID",
			serverName: "matrix.example.com:8448",
			mediaID:    "GCmhgzMPRjqgpODLsNQzVuHZ_jplHLNpqLy6_fJPmO",
			wantErr:    false,
		},
		{
			name:       "localhost server",
			serverName: "localhost:8008",
			mediaID:    "media123",
			wantErr:    false,
		},
		{
			name:       "IP address server",
			serverName: "192.168.1.100:8448",
			mediaID:    "test_media_file",
			wantErr:    false,
		},

		// Path traversal attacks in server name
		{
			name:       "path traversal in server name - simple",
			serverName: "../admin",
			mediaID:    "validmedia",
			wantErr:    true,
			errMsg:     "invalid server name in MXC URI",
		},
		{
			name:       "path traversal in server name - complex",
			serverName: "../../etc/passwd",
			mediaID:    "validmedia",
			wantErr:    true,
			errMsg:     "invalid server name in MXC URI",
		},
		{
			name:       "path traversal in server name - disguised",
			serverName: "matrix.org/../../../admin",
			mediaID:    "validmedia",
			wantErr:    true,
			errMsg:     "invalid server name in MXC URI",
		},

		// Path traversal attacks in media ID
		{
			name:       "path traversal in media ID - simple",
			serverName: "matrix.org",
			mediaID:    "../secret",
			wantErr:    true,
			errMsg:     "invalid media ID in MXC URI",
		},
		{
			name:       "path traversal in media ID - complex",
			serverName: "matrix.org",
			mediaID:    "../../../etc/passwd",
			wantErr:    true,
			errMsg:     "invalid media ID in MXC URI",
		},
		{
			name:       "path traversal in media ID - with valid prefix",
			serverName: "matrix.org",
			mediaID:    "validprefix../../../evil",
			wantErr:    true,
			errMsg:     "invalid media ID in MXC URI",
		},

		// Path traversal in both components
		{
			name:       "path traversal in both components",
			serverName: "../admin",
			mediaID:    "../../../etc/passwd",
			wantErr:    true,
			errMsg:     "invalid server name in MXC URI", // First error caught
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateMXCComponents(tt.serverName, tt.mediaID)

			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errMsg)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestPathTraversalAttackVectors(t *testing.T) {
	// Test comprehensive attack vectors that could bypass naive validation
	attackVectors := []struct {
		name      string
		component string
		wantErr   bool
	}{
		// Basic traversal patterns
		{"dot-dot-slash", "..", true},
		{"dot-dot-slash-trailing", "../", true},
		{"double-dot-dot", "../..", true},
		{"triple-dot-dot", "../../..", true},

		// Mixed with valid content
		{"traversal-prefix", "../validcontent", true},
		{"traversal-suffix", "validcontent/..", true},
		{"traversal-middle", "valid/../content", true},

		// Case variations (should still detect)
		{"uppercase-traversal", "VALID/../EVIL", true},
		{"mixed-case-traversal", "Valid/../Evil", true},

		// Real-world Matrix ID examples with traversal
		{"matrix-room-traversal", "!room:../../admin.com", true},
		{"matrix-event-traversal", "$event/../../../etc:matrix.org", true},
		{"matrix-user-traversal", "@user:../.../../etc", true},

		// Valid cases that should pass
		{"valid-matrix-room", "!abc123:matrix.org", false},
		{"valid-matrix-event", "$def456:matrix.org", false},
		{"valid-matrix-user", "@alice:matrix.org", false},
		{"valid-media-id", "GCmhgzMPRjqgpODLsNQzVuHZ", false},
		{"valid-server-with-port", "matrix.org:8448", false},
		{"valid-with-dots", "file.name.ext", false}, // Single dots are OK
		{"valid-with-underscores", "valid_file_name", false},
		{"valid-with-hyphens", "valid-file-name", false},
	}

	for _, tt := range attackVectors {
		t.Run(tt.name, func(t *testing.T) {
			err := validatePathComponent(tt.component)

			if tt.wantErr {
				assert.Error(t, err, "Expected path traversal to be detected in: %s", tt.component)
				assert.Contains(t, err.Error(), "path traversal detected")
			} else {
				assert.NoError(t, err, "Valid component should not trigger path traversal detection: %s", tt.component)
			}
		})
	}
}

func TestBuildSecureURLIntegration(t *testing.T) {
	// Test realistic Matrix API endpoint construction
	testCases := []struct {
		name           string
		baseURL        string
		pathComponents []string
		expectedResult bool
		description    string
	}{
		{
			name:           "matrix-room-event-valid",
			baseURL:        "https://matrix.org/_matrix/client/v3/rooms/",
			pathComponents: []string{"!abc123:matrix.org", "event", "$def456:matrix.org"},
			expectedResult: true,
			description:    "Valid Matrix room event URL construction",
		},
		{
			name:           "matrix-room-send-valid",
			baseURL:        "https://matrix.org/_matrix/client/v3/rooms/",
			pathComponents: []string{"!room:matrix.org", "send", "m.room.message", "txn123"},
			expectedResult: true,
			description:    "Valid Matrix room send message URL construction",
		},
		{
			name:           "matrix-profile-valid",
			baseURL:        "https://matrix.org/_matrix/client/v3/profile/",
			pathComponents: []string{"@user:matrix.org", "displayname"},
			expectedResult: true,
			description:    "Valid Matrix profile URL construction",
		},
		{
			name:           "matrix-media-download-valid",
			baseURL:        "https://matrix.org/_matrix/media/v3/download/",
			pathComponents: []string{"matrix.org", "abc123def456"},
			expectedResult: true,
			description:    "Valid Matrix media download URL construction",
		},

		// Attack scenarios
		{
			name:           "matrix-room-traversal-attack",
			baseURL:        "https://matrix.org/_matrix/client/v3/rooms/",
			pathComponents: []string{"../../../admin", "secret"},
			expectedResult: false,
			description:    "Path traversal attack in room ID should be blocked",
		},
		{
			name:           "matrix-event-traversal-attack",
			baseURL:        "https://matrix.org/_matrix/client/v3/rooms/",
			pathComponents: []string{"!validroom:matrix.org", "event", "../../../admin/secret"},
			expectedResult: false,
			description:    "Path traversal attack in event ID should be blocked",
		},
		{
			name:           "matrix-media-traversal-attack",
			baseURL:        "https://matrix.org/_matrix/media/v3/download/",
			pathComponents: []string{"../../etc/passwd", "evil"},
			expectedResult: false,
			description:    "Path traversal attack in media server name should be blocked",
		},
	}

	for _, tt := range testCases {
		t.Run(tt.name, func(t *testing.T) {
			result, err := buildSecureURL(tt.baseURL, tt.pathComponents...)

			if tt.expectedResult {
				assert.NoError(t, err, tt.description)
				assert.NotEmpty(t, result)
				assert.Contains(t, result, tt.baseURL)
				// Ensure no path traversal sequences in final URL
				assert.NotContains(t, result, "../")
			} else {
				assert.Error(t, err, tt.description)
				assert.Empty(t, result)
				assert.Contains(t, err.Error(), "path traversal detected")
			}
		})
	}
}

func TestURLEscapingBehavior(t *testing.T) {
	// Test that our URL escaping properly handles Matrix-specific characters
	testCases := []struct {
		name      string
		component string
		expected  string
	}{
		{
			name:      "matrix room ID escaping",
			component: "!abc123:matrix.org",
			expected:  "%21abc123:matrix.org",
		},
		{
			name:      "matrix event ID escaping",
			component: "$def456:matrix.org",
			expected:  "$def456:matrix.org",
		},
		{
			name:      "matrix user ID escaping",
			component: "@alice:matrix.org",
			expected:  "@alice:matrix.org",
		},
		{
			name:      "server with port escaping",
			component: "matrix.org:8448",
			expected:  "matrix.org:8448",
		},
	}

	for _, tt := range testCases {
		t.Run(tt.name, func(t *testing.T) {
			result, err := buildSecureURL("https://example.com/", tt.component)
			require.NoError(t, err)

			// Extract the escaped component from the result
			expectedURL := "https://example.com/" + tt.expected
			assert.Equal(t, expectedURL, result)
		})
	}
}

// Test runner functions to connect the suite to Go's testing framework
func TestMatrixClientTestSuite(t *testing.T) {
	suite.Run(t, new(MatrixClientTestSuite))
}
