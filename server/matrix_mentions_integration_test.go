package main

import (
	"testing"
	"time"

	matrixtest "github.com/mattermost/mattermost-plugin-matrix-bridge/testcontainers/matrix"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMatrixMentionProcessing tests mention processing with real Matrix server
func TestMatrixMentionProcessing(t *testing.T) {
	// Start Matrix container
	matrixContainer := matrixtest.StartMatrixContainer(t, matrixtest.DefaultMatrixConfig())
	defer matrixContainer.Cleanup(t)

	// Create test room (this will be throttled automatically)
	_ = matrixContainer.CreateRoom(t, "Mention Test Room")

	// Set up plugin
	setup := setupTestPlugin(t, matrixContainer)

	// Create mentioned users
	user1ID := model.NewId()
	user2ID := model.NewId()
	ghost1ID := "@_mattermost_" + user1ID + ":" + matrixContainer.ServerDomain
	ghost2ID := "@_mattermost_" + user2ID + ":" + matrixContainer.ServerDomain

	testCases := []struct {
		name                 string
		message              string
		expectedMentions     int
		expectedUserIDs      []string
		expectedHTMLSnippets []string
	}{
		{
			name:             "single_mention",
			message:          "Hello @alice, how are you?",
			expectedMentions: 1,
			expectedUserIDs:  []string{ghost1ID},
			expectedHTMLSnippets: []string{
				`<a href="https://matrix.to/#/` + ghost1ID + `">@alice</a>`,
			},
		},
		{
			name:             "multiple_mentions",
			message:          "Hey @alice and @bob, let's meet up!",
			expectedMentions: 2,
			expectedUserIDs:  []string{ghost1ID, ghost2ID},
			expectedHTMLSnippets: []string{
				`<a href="https://matrix.to/#/` + ghost1ID + `">@alice</a>`,
				`<a href="https://matrix.to/#/` + ghost2ID + `">@bob</a>`,
			},
		},
		{
			name:             "mention_with_markdown",
			message:          "**Important:** @alice please review this",
			expectedMentions: 1,
			expectedUserIDs:  []string{ghost1ID},
			expectedHTMLSnippets: []string{
				`<strong>Important:</strong>`,
				`<a href="https://matrix.to/#/` + ghost1ID + `">@alice</a>`,
			},
		},
		{
			name:             "mention_at_start",
			message:          "@alice started a new project",
			expectedMentions: 1,
			expectedUserIDs:  []string{ghost1ID},
			expectedHTMLSnippets: []string{
				`<a href="https://matrix.to/#/` + ghost1ID + `">@alice</a>`,
			},
		},
		{
			name:             "mention_at_end",
			message:          "Great work by @alice",
			expectedMentions: 1,
			expectedUserIDs:  []string{ghost1ID},
			expectedHTMLSnippets: []string{
				`<a href="https://matrix.to/#/` + ghost1ID + `">@alice</a>`,
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Create fresh room for each test case to ensure isolation
			// The CreateRoom method now includes automatic throttling to prevent rate limits
			freshRoomID := matrixContainer.CreateRoom(t, "Mention Room - "+tc.name)

			// Update KV store mapping for this fresh room
			_ = setup.Plugin.kvstore.Set("channel_mapping_"+setup.ChannelID, []byte(freshRoomID))

			// Clear previous mock expectations
			clearMockExpectations(setup.API)

			// Reset common mocks with fresh room
			setupBasicMocks(setup.API, setup.UserID)

			// Set up fresh mention mocks for this test case
			setupMentionMocks(setup.API, user1ID, "alice")
			setupMentionMocks(setup.API, user2ID, "bob")

			// Create test post with mentions
			post := &model.Post{
				Id:        model.NewId(),
				UserId:    setup.UserID,
				ChannelId: setup.ChannelID,
				Message:   tc.message,
				CreateAt:  time.Now().UnixMilli(),
			}

			// Sync post to Matrix
			err := setup.Plugin.mattermostToMatrixBridge.SyncPostToMatrix(post, setup.ChannelID)
			require.NoError(t, err)

			// Wait for Matrix to process with polling
			var messageEvent *matrixtest.Event
			require.Eventually(t, func() bool {
				events := matrixContainer.GetRoomEvents(t, freshRoomID)
				messageEvent = matrixtest.FindEventByPostID(events, post.Id)
				return messageEvent != nil
			}, 10*time.Second, 500*time.Millisecond, "Should find message event within timeout")

			// Debug: Print the actual message content
			t.Logf("Message content: %+v", messageEvent.Content)
			if mentions, hasMentions := messageEvent.Content["m.mentions"]; hasMentions {
				t.Logf("Found mentions: %+v", mentions)
			} else {
				t.Logf("No m.mentions field found")
			}

			// Validate mention structure
			validator := matrixtest.NewEventValidation(t, matrixContainer.ServerDomain, setup.Plugin.remoteID)
			validator.ValidateMessageWithMentions(*messageEvent, post, tc.expectedMentions)

			// Verify specific mention details
			mentions := messageEvent.Content["m.mentions"].(map[string]any)
			userIDs := mentions["user_ids"].([]any)

			// Check mentioned user IDs
			require.Len(t, userIDs, len(tc.expectedUserIDs), "Should have correct number of user mentions")
			for _, expectedUserID := range tc.expectedUserIDs {
				assert.Contains(t, userIDs, expectedUserID, "Should mention expected user")
			}

			// Check HTML formatting
			formattedBody := messageEvent.Content["formatted_body"].(string)
			for _, snippet := range tc.expectedHTMLSnippets {
				assert.Contains(t, formattedBody, snippet, "Should contain expected HTML snippet")
			}
		})
	}
}

// TestMatrixMentionEdgeCases tests mention processing edge cases
func TestMatrixMentionEdgeCases(t *testing.T) {
	// Start Matrix container
	matrixContainer := matrixtest.StartMatrixContainer(t, matrixtest.DefaultMatrixConfig())
	defer matrixContainer.Cleanup(t)

	// Create test room (this will be throttled automatically)
	_ = matrixContainer.CreateRoom(t, "Mention Edge Cases Room")

	// Set up plugin
	setup := setupTestPlugin(t, matrixContainer)

	testCases := []struct {
		name             string
		message          string
		setupMocks       func(api *plugintest.API)
		expectedMentions int
		shouldHaveHTML   bool
	}{
		{
			name:    "mention_nonexistent_user",
			message: "Hello @nonexistent, are you there?",
			setupMocks: func(api *plugintest.API) {
				api.On("GetUserByUsername", "nonexistent").Return(nil, &model.AppError{})
			},
			expectedMentions: 0,
			shouldHaveHTML:   false,
		},
		{
			name:    "mention_user_without_ghost",
			message: "Hey @noghost, what's up?",
			setupMocks: func(api *plugintest.API) {
				user := &model.User{Id: "noghost123", Username: "noghost"}
				api.On("GetUserByUsername", "noghost").Return(user, nil)
				api.On("GetProfileImage", "noghost123").Return([]byte("fake-image-data"), nil)
			},
			expectedMentions: 1, // Ghost user will be created and mention should work
			shouldHaveHTML:   true,
		},
		{
			name:    "channel_mentions",
			message: "Attention @channel and @here everyone!",
			setupMocks: func(_ *plugintest.API) {
				// No setup needed - channel mentions are ignored
			},
			expectedMentions: 0,
			shouldHaveHTML:   false,
		},
		{
			name:    "email_like_pattern",
			message: "Send email to user@domain.com please",
			setupMocks: func(api *plugintest.API) {
				// Mock the false positive that our regex will catch
				api.On("GetUserByUsername", "domain.com").Return(nil, &model.AppError{})
			},
			expectedMentions: 0,
			shouldHaveHTML:   false,
		},
		{
			name:    "mention_in_code_block",
			message: "Run `echo @alice` in terminal",
			setupMocks: func(api *plugintest.API) {
				userID := model.NewId()
				user := &model.User{Id: userID, Username: "alice"}
				api.On("GetUserByUsername", "alice").Return(user, nil)
				api.On("GetProfileImage", userID).Return([]byte("fake-image-data"), nil)
			},
			expectedMentions: 1, // Mentions are processed even in code blocks for now
			shouldHaveHTML:   true,
		},
		{
			name:    "mention_substring_in_email",
			message: "Contact alice@example.com about @alice meeting",
			setupMocks: func(api *plugintest.API) {
				userID := model.NewId()
				user := &model.User{Id: userID, Username: "alice"}
				api.On("GetUserByUsername", "alice").Return(user, nil)
				api.On("GetProfileImage", userID).Return([]byte("fake-image-data"), nil)
				// Should NOT match alice in alice@example.com
				api.On("GetUserByUsername", "example.com").Return(nil, &model.AppError{})
			},
			expectedMentions: 1, // Only @alice should be matched, not alice in email
			shouldHaveHTML:   true,
		},
		{
			name:    "mention_substring_in_username",
			message: "Tell @bobby that @bob is here",
			setupMocks: func(api *plugintest.API) {
				bobbyID := model.NewId()
				bobbyUser := &model.User{Id: bobbyID, Username: "bobby"}
				api.On("GetUserByUsername", "bobby").Return(bobbyUser, nil)
				api.On("GetProfileImage", bobbyID).Return([]byte("fake-image-data"), nil)

				bobID := model.NewId()
				bobUser := &model.User{Id: bobID, Username: "bob"}
				api.On("GetUserByUsername", "bob").Return(bobUser, nil)
				api.On("GetProfileImage", bobID).Return([]byte("fake-image-data"), nil)
			},
			expectedMentions: 2, // Both @bobby and @bob should be matched correctly
			shouldHaveHTML:   true,
		},
		{
			name:    "email_corruption_test",
			message: "Send to alice@company.com and mention @alice too",
			setupMocks: func(api *plugintest.API) {
				userID := model.NewId()
				user := &model.User{Id: userID, Username: "alice"}
				api.On("GetUserByUsername", "alice").Return(user, nil)
				api.On("GetProfileImage", userID).Return([]byte("fake-image-data"), nil)
				// With proper word boundaries, company.com should not be extracted at all
			},
			expectedMentions: 1, // Only @alice should be matched
			shouldHaveHTML:   true,
		},
		{
			name:    "user_with_existing_name_edge_case",
			message: "User company@example.com exists but @company should still work",
			setupMocks: func(api *plugintest.API) {
				// This tests the case where 'company' is both part of an email AND a real username
				companyUserID := model.NewId()
				companyUser := &model.User{Id: companyUserID, Username: "company"}
				api.On("GetUserByUsername", "company").Return(companyUser, nil)
				api.On("GetProfileImage", companyUserID).Return([]byte("fake-image-data"), nil)
			},
			expectedMentions: 1, // Only @company should be matched, not company from email
			shouldHaveHTML:   true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Create fresh room for each test case to ensure isolation
			// The CreateRoom method now includes automatic throttling to prevent rate limits
			freshRoomID := matrixContainer.CreateRoom(t, "Edge Case Room - "+tc.name)

			// Update KV store mapping for this fresh room
			_ = setup.Plugin.kvstore.Set("channel_mapping_"+setup.ChannelID, []byte(freshRoomID))

			// Clear previous mock expectations
			clearMockExpectations(setup.API)

			// Reset common mocks with fresh room
			setupBasicMocks(setup.API, setup.UserID)

			// Set up test-specific mocks
			tc.setupMocks(setup.API)

			// Create test post
			post := &model.Post{
				Id:        model.NewId(),
				UserId:    setup.UserID,
				ChannelId: setup.ChannelID,
				Message:   tc.message,
				CreateAt:  time.Now().UnixMilli(),
			}

			// Sync post to Matrix
			err := setup.Plugin.mattermostToMatrixBridge.SyncPostToMatrix(post, setup.ChannelID)
			require.NoError(t, err)

			// Wait for processing with polling
			var messageEvent *matrixtest.Event
			require.Eventually(t, func() bool {
				events := matrixContainer.GetRoomEvents(t, freshRoomID)
				messageEvent = matrixtest.FindEventByPostID(events, post.Id)
				return messageEvent != nil
			}, 10*time.Second, 500*time.Millisecond, "Should find message event within timeout")

			if tc.expectedMentions > 0 {
				// Should have mentions
				mentions, hasMentions := messageEvent.Content["m.mentions"].(map[string]any)
				require.True(t, hasMentions, "Should have m.mentions field")

				userIDs := mentions["user_ids"].([]any)
				assert.Len(t, userIDs, tc.expectedMentions, "Should have expected mention count")
			} else {
				// Should not have mentions
				_, hasMentions := messageEvent.Content["m.mentions"]
				assert.False(t, hasMentions, "Should not have m.mentions field")
			}

			if tc.shouldHaveHTML {
				_, hasHTML := messageEvent.Content["formatted_body"]
				assert.True(t, hasHTML, "Should have HTML formatted body")
			}

			// Special validation for email corruption test
			if tc.name == "email_corruption_test" {
				formattedBody, hasFormatted := messageEvent.Content["formatted_body"].(string)
				require.True(t, hasFormatted, "Should have formatted_body for email corruption test")

				// Debug: Print the actual formatted body
				t.Logf("Email corruption test - formatted_body: %s", formattedBody)

				// The email should NOT be corrupted by mention replacement
				assert.Contains(t, formattedBody, "alice@company.com", "Email should remain intact")
				// But @alice should be properly converted to a mention
				assert.Contains(t, formattedBody, `<a href="https://matrix.to/#/`, "Should have proper mention link")
				// Ensure we don't have mangled email like "alice<a href...>@company.com</a>"
				assert.NotContains(t, formattedBody, "alice<a href", "Email should not be corrupted by mention replacement")
			}

			// Special validation for edge case with real user matching email domain
			if tc.name == "user_with_existing_name_edge_case" {
				formattedBody, hasFormatted := messageEvent.Content["formatted_body"].(string)
				require.True(t, hasFormatted, "Should have formatted_body for edge case test")

				// Debug: Print the actual formatted body
				t.Logf("Edge case test - formatted_body: %s", formattedBody)

				// The email should remain intact
				assert.Contains(t, formattedBody, "company@example.com", "Email should remain intact")
				// @company should be properly converted to a mention
				assert.Contains(t, formattedBody, `<a href="https://matrix.to/#/`, "Should have proper mention link")
				// Ensure email is not corrupted (email should not contain mention links)
				assert.NotContains(t, formattedBody, "company<a href", "Email should not be corrupted by mention replacement")
			}
		})
	}
}
