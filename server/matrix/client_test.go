package matrix

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	matrixtest "github.com/wiggin77/mattermost-plugin-matrix-bridge/testcontainers/matrix"
)

// TestMatrixClientOperations tests core Matrix client operations that affect change
// and then query the server to verify the changes were applied correctly.
// This approach validates compatibility across different Matrix server implementations.
func TestMatrixClientOperations(t *testing.T) {
	// Start Matrix test container
	matrixContainer := matrixtest.StartMatrixContainer(t, matrixtest.DefaultMatrixConfig())
	defer matrixContainer.Cleanup(t)

	// Create Matrix client
	client := NewClientWithLogger(
		matrixContainer.ServerURL,
		matrixContainer.ASToken,
		"test-remote-id",
		NewTestLogger(t),
	)
	client.SetServerDomain(matrixContainer.ServerDomain)

	// Test server connectivity first
	t.Run("TestConnection", func(t *testing.T) {
		err := client.TestConnection()
		require.NoError(t, err, "Should connect to Matrix server")
	})

	// Test Application Service permissions
	t.Run("TestApplicationServicePermissions", func(t *testing.T) {
		err := client.TestApplicationServicePermissions()
		require.NoError(t, err, "Should have proper AS permissions")
	})

	// Test server information retrieval
	t.Run("GetServerInfo", func(t *testing.T) {
		serverInfo, err := client.GetServerInfo()
		require.NoError(t, err, "Should retrieve server info")
		assert.NotEmpty(t, serverInfo.Name, "Server name should not be empty")
		t.Logf("Server: %s %s", serverInfo.Name, serverInfo.Version)
	})

	// Run room operation tests
	t.Run("RoomOperations", func(t *testing.T) {
		testRoomOperations(t, client, matrixContainer.ServerDomain)
	})

	// Run user operation tests
	t.Run("UserOperations", func(t *testing.T) {
		testUserOperations(t, client)
	})

	// Run message operation tests
	t.Run("MessageOperations", func(t *testing.T) {
		testMessageOperations(t, client, matrixContainer.ServerDomain)
	})

	// Run media operation tests
	t.Run("MediaOperations", func(t *testing.T) {
		testMediaOperations(t, client)
	})
}

// testRoomOperations tests room creation, joining, and management operations
func testRoomOperations(t *testing.T, client *Client, serverDomain string) {
	t.Run("CreateRoom", func(t *testing.T) {
		// Create a room and verify it exists
		roomName := "Test Room"
		roomTopic := "Integration test room"
		mattermostChannelID := "test-channel-123"

		roomIdentifier, err := client.CreateRoom(roomName, roomTopic, serverDomain, true, mattermostChannelID)
		require.NoError(t, err, "Should create room successfully")
		assert.NotEmpty(t, roomIdentifier, "Room identifier should not be empty")
		t.Logf("Created room: %s", roomIdentifier)

		// Verify room was created by resolving the identifier
		roomID, err := client.ResolveRoomAlias(roomIdentifier)
		require.NoError(t, err, "Should resolve room identifier")
		assert.NotEmpty(t, roomID, "Room ID should not be empty")
		assert.True(t, strings.HasPrefix(roomID, "!"), "Room ID should start with !")

		// Verify Mattermost channel ID was stored in room state
		retrievedChannelID, err := client.GetMattermostChannelID(roomID)
		require.NoError(t, err, "Should retrieve Mattermost channel ID")
		assert.Equal(t, mattermostChannelID, retrievedChannelID, "Channel ID should match")
	})

	t.Run("CreateDirectRoom", func(t *testing.T) {
		// Create ghost users for DM
		ghostUser1 := "@_mattermost_user1:" + serverDomain
		ghostUser2 := "@_mattermost_user2:" + serverDomain
		ghostUserIDs := []string{ghostUser1, ghostUser2}

		roomID, err := client.CreateDirectRoom(ghostUserIDs, "Test DM")
		require.NoError(t, err, "Should create direct room")
		assert.NotEmpty(t, roomID, "Room ID should not be empty")
		assert.True(t, strings.HasPrefix(roomID, "!"), "Room ID should start with !")
		t.Logf("Created DM room: %s", roomID)
	})

	t.Run("JoinRoom", func(t *testing.T) {
		// Create a room first
		roomIdentifier, err := client.CreateRoom("Join Test Room", "Test joining", serverDomain, false, "test-join-channel")
		require.NoError(t, err, "Should create room for join test")

		// Join the room (AS should already be in room, but test the API)
		err = client.JoinRoom(roomIdentifier)
		require.NoError(t, err, "Should join room successfully")
	})

	t.Run("AddRoomAlias", func(t *testing.T) {
		// Create a room
		roomIdentifier, err := client.CreateRoom("Alias Test Room", "Test aliases", serverDomain, false, "test-alias-channel")
		require.NoError(t, err, "Should create room for alias test")

		roomID, err := client.ResolveRoomAlias(roomIdentifier)
		require.NoError(t, err, "Should resolve room identifier")

		// Add an additional alias within the reserved namespace
		additionalAlias := "#_mattermost_additional-alias-test:" + serverDomain
		err = client.AddRoomAlias(roomID, additionalAlias)
		require.NoError(t, err, "Should add room alias")

		// Verify the alias resolves to the same room
		resolvedRoomID, err := client.ResolveRoomAlias(additionalAlias)
		require.NoError(t, err, "Should resolve additional alias")
		assert.Equal(t, roomID, resolvedRoomID, "Additional alias should resolve to same room")
	})
}

// testUserOperations tests user creation and profile management operations
func testUserOperations(t *testing.T, client *Client) {
	t.Run("CreateGhostUser", func(t *testing.T) {
		mattermostUserID := "test-user-123"
		displayName := "Test User"
		avatarData := []byte("fake-avatar-data")
		avatarContentType := "image/png"

		ghostUser, err := client.CreateGhostUser(mattermostUserID, displayName, avatarData, avatarContentType)
		require.NoError(t, err, "Should create ghost user")
		assert.NotNil(t, ghostUser, "Ghost user should not be nil")
		assert.NotEmpty(t, ghostUser.UserID, "Ghost user ID should not be empty")
		assert.Contains(t, ghostUser.UserID, mattermostUserID, "Ghost user ID should contain Mattermost user ID")
		t.Logf("Created ghost user: %s", ghostUser.UserID)

		// Verify user profile was set correctly
		profile, err := client.GetUserProfile(ghostUser.UserID)
		require.NoError(t, err, "Should get user profile")
		assert.Equal(t, displayName, profile.DisplayName, "Display name should match")
		assert.NotEmpty(t, profile.AvatarURL, "Avatar URL should be set")
	})

	t.Run("UpdateUserProfile", func(t *testing.T) {
		// Create a ghost user first
		mattermostUserID := "test-user-456"
		ghostUser, err := client.CreateGhostUser(mattermostUserID, "Original Name", nil, "")
		require.NoError(t, err, "Should create ghost user for profile test")

		// Update display name
		newDisplayName := "Updated Display Name"
		err = client.SetDisplayName(ghostUser.UserID, newDisplayName)
		require.NoError(t, err, "Should update display name")

		// Verify the change
		profile, err := client.GetUserProfile(ghostUser.UserID)
		require.NoError(t, err, "Should get updated profile")
		assert.Equal(t, newDisplayName, profile.DisplayName, "Display name should be updated")

		// Update avatar
		newAvatarData := []byte("new-fake-avatar-data")
		err = client.UpdateGhostUserAvatar(ghostUser.UserID, newAvatarData, "image/jpeg")
		require.NoError(t, err, "Should update avatar")

		// Verify avatar was updated
		updatedProfile, err := client.GetUserProfile(ghostUser.UserID)
		require.NoError(t, err, "Should get profile after avatar update")
		assert.NotEmpty(t, updatedProfile.AvatarURL, "Avatar URL should be set")
		// Avatar URL should be different from the original (if there was one)
		if profile.AvatarURL != "" {
			assert.NotEqual(t, profile.AvatarURL, updatedProfile.AvatarURL, "Avatar URL should be different")
		}
	})
}

// testMessageOperations tests message sending, editing, reactions, and redactions
func testMessageOperations(t *testing.T, client *Client, serverDomain string) {
	// Create a test room and ghost user for message tests
	roomIdentifier, err := client.CreateRoom("Message Test Room", "Testing messages", serverDomain, false, "test-message-channel")
	require.NoError(t, err, "Should create room for message tests")

	roomID, err := client.ResolveRoomAlias(roomIdentifier)
	require.NoError(t, err, "Should resolve room identifier")

	// Create ghost user for sending messages
	mattermostUserID := "test-msg-user"
	ghostUser, err := client.CreateGhostUser(mattermostUserID, "Message Test User", nil, "")
	require.NoError(t, err, "Should create ghost user for messages")

	// Join the ghost user to the room
	err = client.JoinRoomAsUser(roomID, ghostUser.UserID)
	require.NoError(t, err, "Should join ghost user to room")

	t.Run("SendMessage", func(t *testing.T) {
		// Send a text message
		messageReq := MessageRequest{
			RoomID:      roomID,
			GhostUserID: ghostUser.UserID,
			Message:     "Hello, this is a test message!",
			HTMLMessage: "<p>Hello, this is a <strong>test</strong> message!</p>",
			PostID:      "test-post-123",
		}

		response, err := client.SendMessage(messageReq)
		require.NoError(t, err, "Should send message")
		assert.NotEmpty(t, response.EventID, "Event ID should not be empty")
		t.Logf("Sent message with event ID: %s", response.EventID)

		// Verify the message was sent by retrieving it
		event, err := client.GetEvent(roomID, response.EventID)
		require.NoError(t, err, "Should retrieve sent message")
		assert.Equal(t, "m.room.message", event["type"], "Event type should be m.room.message")

		content, ok := event["content"].(map[string]any)
		require.True(t, ok, "Event should have content")
		assert.Equal(t, messageReq.Message, content["body"], "Message body should match")
		assert.Equal(t, messageReq.HTMLMessage, content["formatted_body"], "HTML message should match")
		assert.Equal(t, messageReq.PostID, content["mattermost_post_id"], "Post ID should match")
	})

	t.Run("EditMessage", func(t *testing.T) {
		// Send original message
		messageReq := MessageRequest{
			RoomID:      roomID,
			GhostUserID: ghostUser.UserID,
			Message:     "Original message",
			PostID:      "test-edit-post",
		}

		response, err := client.SendMessage(messageReq)
		require.NoError(t, err, "Should send original message")

		// Edit the message
		newMessage := "Edited message content"
		newHTMLMessage := "<p>Edited <em>message</em> content</p>"
		editResponse, err := client.EditMessageAsGhost(roomID, response.EventID, newMessage, newHTMLMessage, ghostUser.UserID)
		require.NoError(t, err, "Should edit message")
		assert.NotEmpty(t, editResponse.EventID, "Edit event ID should not be empty")

		// Verify the edit was applied by retrieving the edit event
		editEvent, err := client.GetEvent(roomID, editResponse.EventID)
		require.NoError(t, err, "Should retrieve edit event")

		content, ok := editEvent["content"].(map[string]any)
		require.True(t, ok, "Edit event should have content")

		newContent, ok := content["m.new_content"].(map[string]any)
		require.True(t, ok, "Edit should have m.new_content")
		assert.Equal(t, newMessage, newContent["body"], "Edited message body should match")
		assert.Equal(t, newHTMLMessage, newContent["formatted_body"], "Edited HTML should match")
	})

	t.Run("SendReaction", func(t *testing.T) {
		// Send a message to react to
		messageReq := MessageRequest{
			RoomID:      roomID,
			GhostUserID: ghostUser.UserID,
			Message:     "React to this message!",
			PostID:      "test-reaction-post",
		}

		response, err := client.SendMessage(messageReq)
		require.NoError(t, err, "Should send message to react to")

		// Send a reaction
		emoji := "👍"
		reactionResponse, err := client.SendReactionAsGhost(roomID, response.EventID, emoji, ghostUser.UserID)
		require.NoError(t, err, "Should send reaction")
		assert.NotEmpty(t, reactionResponse.EventID, "Reaction event ID should not be empty")

		// Verify the reaction by getting relations
		relations, err := client.GetEventRelationsAsUser(roomID, response.EventID, ghostUser.UserID)
		require.NoError(t, err, "Should get event relations")
		assert.NotEmpty(t, relations, "Should have at least one relation (the reaction)")

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
		assert.True(t, foundReaction, "Should find the reaction in relations")
	})

	t.Run("RedactEvent", func(t *testing.T) {
		// Send a message to redact
		messageReq := MessageRequest{
			RoomID:      roomID,
			GhostUserID: ghostUser.UserID,
			Message:     "This message will be deleted",
			PostID:      "test-redact-post",
		}

		response, err := client.SendMessage(messageReq)
		require.NoError(t, err, "Should send message to redact")

		// Redact the message
		redactResponse, err := client.RedactEventAsGhost(roomID, response.EventID, ghostUser.UserID)
		require.NoError(t, err, "Should redact message")
		assert.NotEmpty(t, redactResponse.EventID, "Redaction event ID should not be empty")

		// Verify the original message is now redacted
		// Note: Redacted events still exist but with limited content
		event, err := client.GetEvent(roomID, response.EventID)
		require.NoError(t, err, "Should still be able to get redacted event")

		// Check if the event has been redacted (content should be empty or limited)
		content, hasContent := event["content"].(map[string]any)
		if hasContent {
			// If content exists, it should be empty or only contain redaction-safe fields
			body, hasBody := content["body"].(string)
			if hasBody {
				// Redacted events typically have empty body or placeholder text
				assert.True(t, body == "" || strings.Contains(body, "redacted"),
					"Redacted message should have empty or redacted body")
			}
		}
	})
}

// testMediaOperations tests media upload and download operations
func testMediaOperations(t *testing.T, client *Client) {
	t.Run("UploadAndDownloadMedia", func(t *testing.T) {
		// Test data
		testData := []byte("This is test file content for upload/download testing")
		filename := "test-file.txt"
		contentType := "text/plain"

		// Upload media
		mxcURI, err := client.UploadMedia(testData, filename, contentType)
		require.NoError(t, err, "Should upload media")
		assert.NotEmpty(t, mxcURI, "MXC URI should not be empty")
		assert.True(t, strings.HasPrefix(mxcURI, "mxc://"), "Should return valid MXC URI")
		t.Logf("Uploaded media: %s", mxcURI)

		// Download the media back
		downloadedData, err := client.DownloadFile(mxcURI, int64(len(testData)*2), "text/")
		require.NoError(t, err, "Should download media")
		assert.Equal(t, testData, downloadedData, "Downloaded data should match uploaded data")
	})

	t.Run("UploadAvatar", func(t *testing.T) {
		// Test avatar upload
		avatarData := []byte("fake-avatar-image-data")
		contentType := "image/png"

		mxcURI, err := client.UploadAvatarFromData(avatarData, contentType)
		require.NoError(t, err, "Should upload avatar")
		assert.NotEmpty(t, mxcURI, "Avatar MXC URI should not be empty")
		assert.True(t, strings.HasPrefix(mxcURI, "mxc://"), "Should return valid MXC URI")

		// Verify we can download it back
		downloadedData, err := client.DownloadFile(mxcURI, int64(len(avatarData)*2), "image/")
		require.NoError(t, err, "Should download avatar")
		assert.Equal(t, avatarData, downloadedData, "Downloaded avatar should match uploaded data")
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
func TestMatrixClientErrorHandling(t *testing.T) {
	// Start Matrix test container
	matrixContainer := matrixtest.StartMatrixContainer(t, matrixtest.DefaultMatrixConfig())
	defer matrixContainer.Cleanup(t)

	// Create Matrix client
	client := NewClientWithLogger(
		matrixContainer.ServerURL,
		matrixContainer.ASToken,
		"test-remote-id",
		NewTestLogger(t),
	)
	client.SetServerDomain(matrixContainer.ServerDomain)

	t.Run("InvalidConfiguration", func(t *testing.T) {
		// Test client with empty token
		emptyTokenClient := NewClientWithLogger(
			matrixContainer.ServerURL,
			"", // Empty token
			"test-remote-id",
			NewTestLogger(t),
		)

		// Should fail operations that require authentication
		_, err := emptyTokenClient.CreateRoom("Test", "Test", matrixContainer.ServerDomain, false, "test")
		assert.Error(t, err, "Should fail with empty token")
		assert.Contains(t, err.Error(), "not configured", "Error should mention configuration issue")

		// Test client with invalid server URL
		invalidURLClient := NewClientWithLogger(
			"http://nonexistent.invalid:1234",
			matrixContainer.ASToken,
			"test-remote-id",
			NewTestLogger(t),
		)

		err = invalidURLClient.TestConnection()
		assert.Error(t, err, "Should fail with invalid server URL")
	})

	t.Run("InvalidRoomOperations", func(t *testing.T) {
		// Test joining non-existent room
		err := client.JoinRoom("!nonexistent:test.matrix.local")
		assert.Error(t, err, "Should fail to join non-existent room")

		// Test resolving non-existent alias
		_, err = client.ResolveRoomAlias("#nonexistent:test.matrix.local")
		assert.Error(t, err, "Should fail to resolve non-existent alias")

		// Test creating DM with insufficient users
		_, err = client.CreateDirectRoom([]string{"@user1:test.matrix.local"}, "Test DM")
		assert.Error(t, err, "Should fail DM creation with only one user")
		assert.Contains(t, err.Error(), "at least 2 users", "Error should mention user requirement")
	})

	t.Run("InvalidUserOperations", func(t *testing.T) {
		// Test setting display name for non-existent user (should fail)
		err := client.SetDisplayName("@nonexistent:test.matrix.local", "Test Name")
		assert.Error(t, err, "Should fail to set display name for non-existent user")

		// Test getting profile for non-existent user
		profile, err := client.GetUserProfile("@nonexistent:test.matrix.local")
		require.NoError(t, err, "Should not error for non-existent user profile") // Matrix returns empty profile
		assert.Empty(t, profile.DisplayName, "Non-existent user should have empty profile")
	})

	t.Run("InvalidMessageOperations", func(t *testing.T) {
		// Test sending message to non-existent room
		messageReq := MessageRequest{
			RoomID:      "!nonexistent:test.matrix.local",
			GhostUserID: "@_mattermost_test:test.matrix.local",
			Message:     "Test message",
		}

		_, err := client.SendMessage(messageReq)
		assert.Error(t, err, "Should fail to send message to non-existent room")

		// Test getting non-existent event
		_, err = client.GetEvent("!nonexistent:test.matrix.local", "$nonexistent")
		assert.Error(t, err, "Should fail to get non-existent event")

		// Test empty message request
		emptyReq := MessageRequest{
			RoomID:      "!test:test.matrix.local",
			GhostUserID: "@_mattermost_test:test.matrix.local",
			// No message content
		}

		_, err = client.SendMessage(emptyReq)
		assert.Error(t, err, "Should fail with empty message content")
		assert.Contains(t, err.Error(), "no message content", "Error should mention missing content")
	})

	t.Run("InvalidMediaOperations", func(t *testing.T) {
		// Test upload with empty data
		// Note: Some servers might accept empty files, so we don't assert error here
		_, _ = client.UploadMedia([]byte{}, "test.txt", "text/plain")

		// Test download with invalid MXC URI
		_, err := client.DownloadFile("invalid-uri", 1024, "")
		assert.Error(t, err, "Should fail with invalid MXC URI")
		assert.Contains(t, err.Error(), "invalid Matrix MXC URI", "Error should mention invalid URI")

		// Test download with malformed MXC URI
		_, err = client.DownloadFile("mxc://", 1024, "")
		assert.Error(t, err, "Should fail with malformed MXC URI")

		// Test avatar upload with empty data
		_, err = client.UploadAvatarFromData([]byte{}, "image/png")
		assert.Error(t, err, "Should fail with empty avatar data")
		assert.Contains(t, err.Error(), "image data is empty", "Error should mention empty data")
	})

	t.Run("ParameterValidation", func(t *testing.T) {
		// Test required parameter validation
		messageReq := MessageRequest{
			// Missing RoomID
			GhostUserID: "@_mattermost_test:test.matrix.local",
			Message:     "Test",
		}

		_, err := client.SendMessage(messageReq)
		assert.Error(t, err, "Should fail with missing room ID")
		assert.Contains(t, err.Error(), "room_id is required", "Error should mention room ID")

		messageReq.RoomID = "!test:test.matrix.local"
		messageReq.GhostUserID = "" // Missing ghost user ID

		_, err = client.SendMessage(messageReq)
		assert.Error(t, err, "Should fail with missing ghost user ID")
		assert.Contains(t, err.Error(), "ghost_user_id is required", "Error should mention ghost user ID")
	})

	t.Run("NetworkErrorHandling", func(t *testing.T) {
		// Test with client pointing to a port that definitely doesn't exist
		networkFailClient := NewClientWithLogger(
			"http://localhost:99999", // Port that should be unused
			matrixContainer.ASToken,
			"test-remote-id",
			NewTestLogger(t),
		)

		// All operations should fail with network errors
		err := networkFailClient.TestConnection()
		assert.Error(t, err, "Should fail with network error")

		_, err = networkFailClient.CreateRoom("Test", "Test", "test.local", false, "test")
		assert.Error(t, err, "Should fail with network error")

		_, err = networkFailClient.GetServerInfo()
		assert.Error(t, err, "Should fail with network error")
	})
}

// TestMatrixClientWithFiles tests file attachment handling
func TestMatrixClientWithFiles(t *testing.T) {
	// Start Matrix test container
	matrixContainer := matrixtest.StartMatrixContainer(t, matrixtest.DefaultMatrixConfig())
	defer matrixContainer.Cleanup(t)

	// Create Matrix client
	client := NewClientWithLogger(
		matrixContainer.ServerURL,
		matrixContainer.ASToken,
		"test-remote-id",
		NewTestLogger(t),
	)
	client.SetServerDomain(matrixContainer.ServerDomain)

	// Create test room and user
	roomIdentifier, err := client.CreateRoom("File Test Room", "Testing file attachments", matrixContainer.ServerDomain, false, "test-file-channel")
	require.NoError(t, err, "Should create room for file tests")

	roomID, err := client.ResolveRoomAlias(roomIdentifier)
	require.NoError(t, err, "Should resolve room identifier")

	ghostUser, err := client.CreateGhostUser("test-file-user", "File Test User", nil, "")
	require.NoError(t, err, "Should create ghost user for file tests")

	err = client.JoinRoomAsUser(roomID, ghostUser.UserID)
	require.NoError(t, err, "Should join ghost user to room")

	t.Run("SendMessageWithFiles", func(t *testing.T) {
		// Upload test files first
		testFileData := []byte("This is a test file content for attachment testing")
		testImageData := []byte("fake-image-data-for-testing")

		fileMxcURI, err := client.UploadMedia(testFileData, "test-document.txt", "text/plain")
		require.NoError(t, err, "Should upload test file")

		imageMxcURI, err := client.UploadMedia(testImageData, "test-image.png", "image/png")
		require.NoError(t, err, "Should upload test image")

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

		response, err := client.SendMessage(messageReq)
		require.NoError(t, err, "Should send message with files")
		assert.NotEmpty(t, response.EventID, "Event ID should not be empty")

		// Verify we can download the files back
		downloadedFile, err := client.DownloadFile(fileMxcURI, int64(len(testFileData)*2), "text/")
		require.NoError(t, err, "Should download file")
		assert.Equal(t, testFileData, downloadedFile, "Downloaded file should match uploaded data")

		downloadedImage, err := client.DownloadFile(imageMxcURI, int64(len(testImageData)*2), "image/")
		require.NoError(t, err, "Should download image")
		assert.Equal(t, testImageData, downloadedImage, "Downloaded image should match uploaded data")
	})

	t.Run("SendOnlyFiles", func(t *testing.T) {
		// Upload test file
		testData := []byte("File-only message test content")
		mxcURI, err := client.UploadMedia(testData, "file-only.txt", "text/plain")
		require.NoError(t, err, "Should upload test file")

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

		response, err := client.SendMessage(messageReq)
		require.NoError(t, err, "Should send file-only message")
		assert.NotEmpty(t, response.EventID, "Event ID should not be empty")
	})
}
