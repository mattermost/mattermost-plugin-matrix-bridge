package main

import (
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/mocks"
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
