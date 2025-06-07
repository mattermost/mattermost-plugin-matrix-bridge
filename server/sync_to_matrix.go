package main

import (
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/pkg/errors"
)

// syncUserToMatrix handles syncing user changes (like display name) to Matrix ghost users
func (p *Plugin) syncUserToMatrix(user *model.User) error {
	p.API.LogDebug("Syncing user to Matrix", "user_id", user.Id, "username", user.Username)
	
	// Check if we have a ghost user for this Mattermost user
	ghostUserKey := "ghost_user_" + user.Id
	ghostUserIDBytes, err := p.kvstore.Get(ghostUserKey)
	if err != nil || len(ghostUserIDBytes) == 0 {
		p.API.LogDebug("No ghost user found for user sync", "user_id", user.Id, "username", user.Username)
		return nil // No ghost user exists yet, nothing to update
	}

	ghostUserID := string(ghostUserIDBytes)
	p.API.LogDebug("Found ghost user for user sync", "user_id", user.Id, "ghost_user_id", ghostUserID)

	// Update display name
	displayName := user.GetDisplayName(model.ShowFullName)
	if displayName != "" {
		err = p.matrixClient.SetDisplayName(ghostUserID, displayName)
		if err != nil {
			p.API.LogError("Failed to update ghost user display name", "error", err, "user_id", user.Id, "ghost_user_id", ghostUserID, "display_name", displayName)
			return errors.Wrap(err, "failed to update ghost user display name on Matrix")
		}
		p.API.LogInfo("Updated ghost user display name", "user_id", user.Id, "ghost_user_id", ghostUserID, "display_name", displayName)
	}

	return nil
}

// syncPostToMatrix handles syncing a single post from Mattermost to Matrix
func (p *Plugin) syncPostToMatrix(post *model.Post, channelID string) error {
	matrixRoomIdentifier, err := p.getMatrixRoomID(channelID)
	if err != nil {
		return errors.Wrap(err, "failed to get Matrix room identifier")
	}

	if matrixRoomIdentifier == "" {
		p.API.LogWarn("No Matrix room mapped for channel", "channel_id", channelID)
		return nil
	}

	// Resolve room alias to room ID if needed
	matrixRoomID, err := p.matrixClient.ResolveRoomAlias(matrixRoomIdentifier)
	if err != nil {
		return errors.Wrap(err, "failed to resolve Matrix room identifier")
	}

	user, appErr := p.API.GetUser(post.UserId)
	if appErr != nil {
		return errors.Wrap(appErr, "failed to get user")
	}

	// Send message as ghost user
	err = p.syncPostAsGhostUser(post, matrixRoomID, user)
	if err != nil {
		return err
	}

	p.API.LogDebug("Successfully synced post to Matrix", "post_id", post.Id, "matrix_room_id", matrixRoomID, "matrix_room_identifier", matrixRoomIdentifier)
	return nil
}

// syncPostAsGhostUser sends a post to Matrix as a ghost user representing the Mattermost user
func (p *Plugin) syncPostAsGhostUser(post *model.Post, matrixRoomID string, user *model.User) error {
	// Get or create ghost user with display name and avatar
	displayName := user.GetDisplayName(model.ShowFullName)
	
	// Get user's avatar image data
	var avatarData []byte
	var avatarContentType string
	if imageData, appErr := p.API.GetProfileImage(user.Id); appErr == nil {
		avatarData = imageData
		avatarContentType = "image/png" // Mattermost typically returns PNG
	}
	
	ghostUserID, err := p.getOrCreateGhostUser(user.Id, user.Username, displayName, avatarData, avatarContentType)
	if err != nil {
		return errors.Wrap(err, "failed to get or create ghost user")
	}

	// Ensure ghost user is joined to the room
	err = p.ensureGhostUserInRoom(ghostUserID, matrixRoomID, user.Id)
	if err != nil {
		return errors.Wrap(err, "failed to ensure ghost user is in room")
	}

	// Send message as ghost user (no display name prefix needed since it appears from the user directly)
	_, err = p.matrixClient.SendMessageAsGhost(matrixRoomID, post.Message, ghostUserID)
	if err != nil {
		return errors.Wrap(err, "failed to send message as ghost user")
	}

	p.API.LogDebug("Successfully synced post as ghost user", "post_id", post.Id, "ghost_user_id", ghostUserID)
	return nil
}