package main

import (
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/wiggin77/mattermost-plugin-matrix-bridge/server/mocks"
)

func TestExtractMattermostMentions(t *testing.T) {
	tests := []struct {
		name             string
		message          string
		expectedUsers    []string
		expectedChannels bool
	}{
		{
			name:             "no mentions",
			message:          "This is a regular message",
			expectedUsers:    []string{},
			expectedChannels: false,
		},
		{
			name:             "single user mention",
			message:          "Hello @alice, how are you?",
			expectedUsers:    []string{"alice"},
			expectedChannels: false,
		},
		{
			name:             "multiple user mentions",
			message:          "@alice and @bob please review this",
			expectedUsers:    []string{"alice", "bob"},
			expectedChannels: false,
		},
		{
			name:             "channel mention @here",
			message:          "@here please see this announcement",
			expectedUsers:    []string{},
			expectedChannels: true,
		},
		{
			name:             "channel mention @channel",
			message:          "@channel urgent update",
			expectedUsers:    []string{},
			expectedChannels: true,
		},
		{
			name:             "channel mention @all",
			message:          "@all meeting in 5 minutes",
			expectedUsers:    []string{},
			expectedChannels: true,
		},
		{
			name:             "mixed mentions",
			message:          "@alice @here @bob please join",
			expectedUsers:    []string{"alice", "bob"},
			expectedChannels: true,
		},
		{
			name:             "usernames with dots and hyphens",
			message:          "Hi @john.doe and @jane-smith",
			expectedUsers:    []string{"john.doe", "jane-smith"},
			expectedChannels: false,
		},
		{
			name:             "usernames with underscores",
			message:          "@test_user and @user_123",
			expectedUsers:    []string{"test_user", "user_123"},
			expectedChannels: false,
		},
		{
			name:             "at symbol without mention",
			message:          "Email me at john@example.com",
			expectedUsers:    []string{"example.com"},
			expectedChannels: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockAPI := mocks.NewMockAPI(ctrl)
			// Allow unexpected calls to logging methods
			mockAPI.EXPECT().LogDebug(gomock.Any(), gomock.Any()).AnyTimes()
			mockAPI.EXPECT().LogDebug(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
			mockAPI.EXPECT().LogDebug(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()

			plugin := &Plugin{}
			plugin.SetAPI(mockAPI)

			post := &model.Post{
				Id:      "test-post-id",
				Message: tt.message,
			}

			result := plugin.extractMattermostMentions(post)

			// Check user mentions
			if len(result.UserMentions) != len(tt.expectedUsers) {
				t.Errorf("Expected %d user mentions, got %d", len(tt.expectedUsers), len(result.UserMentions))
			}

			for i, expected := range tt.expectedUsers {
				if i >= len(result.UserMentions) || result.UserMentions[i] != expected {
					t.Errorf("Expected user mention '%s', got '%s'", expected, result.UserMentions[i])
				}
			}

			// Check channel mentions
			if result.ChannelMentions != tt.expectedChannels {
				t.Errorf("Expected channel mentions: %v, got: %v", tt.expectedChannels, result.ChannelMentions)
			}
		})
	}
}

func TestAddMatrixMentions(t *testing.T) {
	tests := []struct {
		name                  string
		message               string
		setupMocks            func(*mocks.MockAPI, *mocks.MockKVStore)
		expectedMentionsAdded bool
		expectedUserIDs       []string
		expectedFormattedBody string
	}{
		{
			name:    "no mentions",
			message: "This is a regular message",
			setupMocks: func(_ *mocks.MockAPI, _ *mocks.MockKVStore) {
				// No mocks needed for this test case
			},
			expectedMentionsAdded: false,
			expectedUserIDs:       []string{},
		},
		{
			name:    "single mention with ghost user",
			message: "Hello @alice, how are you?",
			setupMocks: func(mockAPI *mocks.MockAPI, mockKVStore *mocks.MockKVStore) {
				// Mock user lookup
				user := &model.User{Id: "user123", Username: "alice"}
				mockAPI.EXPECT().GetUserByUsername("alice").Return(user, nil)

				// Mock ghost user lookup
				mockKVStore.EXPECT().Get("ghost_user_user123").Return([]byte("@ghost_alice:matrix.example.com"), nil)
			},
			expectedMentionsAdded: true,
			expectedUserIDs:       []string{"@ghost_alice:matrix.example.com"},
			expectedFormattedBody: `Hello <a href="https://matrix.to/#/@ghost_alice:matrix.example.com">@alice</a>, how are you?`,
		},
		{
			name:    "multiple mentions with mixed ghost users",
			message: "@alice and @bob please review",
			setupMocks: func(mockAPI *mocks.MockAPI, mockKVStore *mocks.MockKVStore) {
				// Mock alice lookup - has ghost user
				userAlice := &model.User{Id: "user123", Username: "alice"}
				mockAPI.EXPECT().GetUserByUsername("alice").Return(userAlice, nil)
				mockKVStore.EXPECT().Get("ghost_user_user123").Return([]byte("@ghost_alice:matrix.example.com"), nil)

				// Mock bob lookup - no ghost user
				userBob := &model.User{Id: "user456", Username: "bob"}
				mockAPI.EXPECT().GetUserByUsername("bob").Return(userBob, nil)
				mockKVStore.EXPECT().Get("ghost_user_user456").Return(nil, &model.AppError{})

				// No logging mocks needed
			},
			expectedMentionsAdded: true,
			expectedUserIDs:       []string{"@ghost_alice:matrix.example.com"},
			expectedFormattedBody: `<a href="https://matrix.to/#/@ghost_alice:matrix.example.com">@alice</a> and @bob please review`,
		},
		{
			name:    "mention user not found",
			message: "@nonexistent please help",
			setupMocks: func(mockAPI *mocks.MockAPI, _ *mocks.MockKVStore) {
				// Mock user lookup failure
				mockAPI.EXPECT().GetUserByUsername("nonexistent").Return(nil, &model.AppError{})

				// No logging mocks needed
			},
			expectedMentionsAdded: false,
			expectedUserIDs:       []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockAPI := mocks.NewMockAPI(ctrl)
			mockKVStore := mocks.NewMockKVStore(ctrl)

			// Allow unexpected calls to logging methods
			mockAPI.EXPECT().LogDebug(gomock.Any(), gomock.Any()).AnyTimes()
			mockAPI.EXPECT().LogDebug(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
			mockAPI.EXPECT().LogDebug(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
			mockAPI.EXPECT().LogInfo(gomock.Any(), gomock.Any()).AnyTimes()
			mockAPI.EXPECT().LogInfo(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
			mockAPI.EXPECT().LogInfo(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()

			plugin := &Plugin{
				kvstore: mockKVStore,
			}
			plugin.SetAPI(mockAPI)

			// Setup mocks
			tt.setupMocks(mockAPI, mockKVStore)

			post := &model.Post{
				Id:      "test-post-id",
				Message: tt.message,
			}

			// Create message content structure
			content := map[string]any{
				"msgtype": "m.text",
				"body":    tt.message,
			}

			// Call the function
			plugin.addMatrixMentions(content, post)

			// Check if mentions were added
			mentions, hasMentions := content["m.mentions"]
			if tt.expectedMentionsAdded != hasMentions {
				t.Errorf("Expected mentions added: %v, got: %v", tt.expectedMentionsAdded, hasMentions)
			}

			if tt.expectedMentionsAdded {
				// Check mention structure
				mentionMap, ok := mentions.(map[string]any)
				if !ok {
					t.Errorf("Expected mentions to be a map")
					return
				}

				userIDs, ok := mentionMap["user_ids"].([]string)
				if !ok {
					t.Errorf("Expected user_ids to be a string slice")
					return
				}

				// Check user IDs
				if len(userIDs) != len(tt.expectedUserIDs) {
					t.Errorf("Expected %d user IDs, got %d", len(tt.expectedUserIDs), len(userIDs))
				}

				for i, expected := range tt.expectedUserIDs {
					if i >= len(userIDs) || userIDs[i] != expected {
						t.Errorf("Expected user ID '%s', got '%s'", expected, userIDs[i])
					}
				}

				// Check formatted body if specified
				if tt.expectedFormattedBody != "" {
					formattedBody, hasFormatted := content["formatted_body"].(string)
					if !hasFormatted {
						t.Errorf("Expected formatted_body to be present")
						return
					}
					if formattedBody != tt.expectedFormattedBody {
						t.Errorf("Expected formatted body '%s', got '%s'", tt.expectedFormattedBody, formattedBody)
					}
				}

				// Check that format is set
				format, hasFormat := content["format"].(string)
				if !hasFormat || format != "org.matrix.custom.html" {
					t.Errorf("Expected format to be 'org.matrix.custom.html', got '%s'", format)
				}
			}
		})
	}
}
