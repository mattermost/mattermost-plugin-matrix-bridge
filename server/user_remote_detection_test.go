package main

import (
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/assert"
)

// TestUserIsRemoteDetection tests the core logic that prevents loop creation
// by properly identifying remote users using model.User.IsRemote()
func TestUserIsRemoteDetection(t *testing.T) {
	tests := []struct {
		name        string
		user        *model.User
		isRemote    bool
		description string
	}{
		{
			name: "local_user_nil_remote_id",
			user: &model.User{
				Id:       "local_user_1",
				Username: "alice",
				RemoteId: nil, // Local user
			},
			isRemote:    false,
			description: "User with nil RemoteId should be identified as local",
		},
		{
			name: "remote_user_with_remote_id",
			user: &model.User{
				Id:       "remote_user_1",
				Username: "matrix:bob",
				RemoteId: &[]string{"matrix_bridge"}[0], // Remote user from Matrix bridge
			},
			isRemote:    true,
			description: "User with RemoteId should be identified as remote",
		},
		{
			name: "local_user_with_matrix_prefix_but_no_remote_id",
			user: &model.User{
				Id:       "user_1",
				Username: "matrix:charlie", // Has matrix prefix but no RemoteId
				RemoteId: nil,
			},
			isRemote:    false,
			description: "Username prefix alone shouldn't make user remote - RemoteId is the authoritative field",
		},
		{
			name: "remote_user_different_bridge",
			user: &model.User{
				Id:       "remote_user_2",
				Username: "slack:david",
				RemoteId: &[]string{"slack_bridge"}[0], // Remote user from different bridge
			},
			isRemote:    true,
			description: "User from any remote bridge should be identified as remote",
		},
		{
			name: "empty_remote_id_string",
			user: &model.User{
				Id:       "user_3",
				Username: "eve",
				RemoteId: &[]string{""}[0], // Empty string RemoteId
			},
			isRemote:    false, // Empty string RemoteId is treated as local
			description: "Empty string RemoteId should be treated as local user",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.user.IsRemote()
			assert.Equal(t, tt.isRemote, result, tt.description)

			// Log the details for debugging
			t.Logf("User: %s, RemoteId: %v, IsRemote(): %v",
				tt.user.Username, tt.user.RemoteId, result)
		})
	}
}

// TestLoopPreventionLogic tests that our loop prevention conditions work correctly
func TestLoopPreventionLogic(t *testing.T) {
	tests := []struct {
		name                 string
		user                 *model.User
		shouldSkipProcessing bool
		context              string
	}{
		{
			name: "matrix_originated_user_should_be_skipped",
			user: &model.User{
				Id:       "matrix_user_123",
				Username: "matrix:nathan",
				RemoteId: &[]string{"matrix_bridge_remote_id"}[0],
			},
			shouldSkipProcessing: true,
			context:              "Matrix-originated users should be skipped to prevent circular sync",
		},
		{
			name: "local_mattermost_user_should_be_processed",
			user: &model.User{
				Id:       "mattermost_user_456",
				Username: "nathan",
				RemoteId: nil,
			},
			shouldSkipProcessing: false,
			context:              "Local Mattermost users should be processed normally",
		},
		{
			name: "imported_user_with_matrix_username_but_local",
			user: &model.User{
				Id:       "imported_user_789",
				Username: "matrix:imported_account", // Might have matrix: prefix from import
				RemoteId: nil,                       // But is actually local now
			},
			shouldSkipProcessing: false,
			context:              "Imported users that are now local should be processed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// This simulates the actual condition used in our loop prevention logic
			shouldSkip := tt.user.IsRemote()

			assert.Equal(t, tt.shouldSkipProcessing, shouldSkip,
				"%s. Expected shouldSkip=%v but got %v for user %s",
				tt.context, tt.shouldSkipProcessing, shouldSkip, tt.user.Username)

			if shouldSkip {
				t.Logf("✓ User %s would be SKIPPED (preventing loop)", tt.user.Username)
			} else {
				t.Logf("✓ User %s would be PROCESSED normally", tt.user.Username)
			}
		})
	}
}

// TestRegressionPrevention ensures that the fix properly prevents the specific
// issue described in the bug report
func TestRegressionPrevention(t *testing.T) {
	t.Run("prevent_matrix_user_ghost_creation_loop", func(t *testing.T) {
		// This represents the scenario from a bug report:
		// 1. Matrix user Nathan sends a message
		// 2. Matrix->Mattermost sync creates a Mattermost user for Nathan
		// 3. SharedChannels sync should NOT try to sync this user back to Matrix

		// User created by Matrix->Mattermost sync (the problematic case)
		mattermostUserForMatrixNathan := &model.User{
			Id:       "whxnnftz3jn39ky9wxpkfapreh",  // From the logs
			Username: "matrix:nathan",               // Matrix bridge convention
			RemoteId: &[]string{"matrix_bridge"}[0], // Set by Matrix->Mattermost sync
		}

		// Our loop prevention should identify this as a remote user
		shouldSkip := mattermostUserForMatrixNathan.IsRemote()
		assert.True(t, shouldSkip,
			"Matrix-originated user should be identified as remote to prevent loop")

		t.Logf("✓ User %s (ID: %s) would be SKIPPED, preventing the ghost user creation loop",
			mattermostUserForMatrixNathan.Username, mattermostUserForMatrixNathan.Id)
	})

	t.Run("allow_normal_mattermost_users", func(t *testing.T) {
		// This represents a normal Mattermost user who should be synced to Matrix
		normalMattermostUser := &model.User{
			Id:       "normal_user_123",
			Username: "john.doe",
			RemoteId: nil, // Local Mattermost user
		}

		// Normal users should NOT be skipped
		shouldSkip := normalMattermostUser.IsRemote()
		assert.False(t, shouldSkip,
			"Normal Mattermost users should be processed and synced to Matrix")

		t.Logf("✓ User %s would be PROCESSED normally and synced to Matrix",
			normalMattermostUser.Username)
	})
}
