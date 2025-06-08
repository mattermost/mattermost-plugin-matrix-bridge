package main

import (
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/pkg/errors"
)

// OnSharedChannelsSyncMsg is called when messages need to be synced from Mattermost to Matrix
func (p *Plugin) OnSharedChannelsSyncMsg(msg *model.SyncMsg, rc *model.RemoteCluster) (model.SyncResponse, error) {
	config := p.getConfiguration()
	if !config.EnableSync {
		return model.SyncResponse{}, nil
	}

	if p.matrixClient == nil {
		p.API.LogError("Matrix client not initialized")
		return model.SyncResponse{}, errors.New("matrix client not initialized")
	}

	// Process user sync events first (display name changes, etc.)
	for _, user := range msg.Users {
		if err := p.syncUserToMatrix(user); err != nil {
			p.API.LogError("Failed to sync user to Matrix", "error", err, "user_id", user.Id, "username", user.Username)
			continue
		}
	}

	// Then process post sync events
	for _, post := range msg.Posts {
		if err := p.syncPostToMatrix(post, msg.ChannelId); err != nil {
			p.API.LogError("Failed to sync post to Matrix", "error", err, "post_id", post.Id)
			continue
		}
	}

	// Finally process reaction sync events
	for _, reaction := range msg.Reactions {
		if err := p.syncReactionToMatrix(reaction, msg.ChannelId); err != nil {
			p.API.LogError("Failed to sync reaction to Matrix", "error", err, "reaction_user_id", reaction.UserId, "reaction_emoji", reaction.EmojiName)
			continue
		}
	}

	return model.SyncResponse{}, nil
}

// OnSharedChannelsPing is called to check if the bridge is healthy and ready to process messages
func (p *Plugin) OnSharedChannelsPing(rc *model.RemoteCluster) bool {
	config := p.getConfiguration()
	
	// If sync is disabled, we're still "healthy" but not actively processing
	if !config.EnableSync {
		p.API.LogDebug("Ping received but sync is disabled")
		return true
	}

	// If Matrix client is not configured, we're not healthy
	if p.matrixClient == nil {
		p.API.LogWarn("Ping failed - Matrix client not initialized")
		return false
	}

	// Test Matrix connection health
	if config.MatrixServerURL != "" && config.MatrixASToken != "" {
		if err := p.matrixClient.TestConnection(); err != nil {
			p.API.LogWarn("Ping failed - Matrix connection test failed", "error", err)
			return false
		}
	} else {
		p.API.LogWarn("Ping failed - Matrix configuration incomplete")
		return false
	}

	p.API.LogDebug("Ping successful - Matrix bridge is healthy")
	return true
}

// OnSharedChannelsAttachmentSyncMsg is called when file attachments need to be synced
func (p *Plugin) OnSharedChannelsAttachmentSyncMsg(fi *model.FileInfo, post *model.Post, rc *model.RemoteCluster) error {
	config := p.getConfiguration()
	if !config.EnableSync {
		return nil
	}

	if p.matrixClient == nil {
		return errors.New("matrix client not initialized")
	}

	p.API.LogDebug("Received attachment sync", "file_id", fi.Id, "post_id", post.Id, "filename", fi.Name)
	
	// For now, we'll log the attachment but not sync it to Matrix
	// TODO: Implement Matrix file upload for attachments
	p.API.LogInfo("Attachment sync not yet implemented", "filename", fi.Name, "size", fi.Size)
	
	return nil
}

// OnSharedChannelsProfileImageSyncMsg is called when user profile images need to be synced
func (p *Plugin) OnSharedChannelsProfileImageSyncMsg(user *model.User, rc *model.RemoteCluster) error {
	config := p.getConfiguration()
	if !config.EnableSync {
		return nil
	}

	if p.matrixClient == nil {
		return errors.New("matrix client not initialized")
	}

	p.API.LogDebug("Received profile image sync", "user_id", user.Id, "username", user.Username)
	
	// Check if we have a ghost user for this Mattermost user
	ghostUserID, exists := p.getGhostUser(user.Id)
	if !exists {
		p.API.LogDebug("No ghost user found for profile image sync", "user_id", user.Id, "username", user.Username)
		return nil // No ghost user exists yet, nothing to update
	}

	p.API.LogDebug("Found ghost user for profile image sync", "user_id", user.Id, "ghost_user_id", ghostUserID)

	// Get user's new avatar image data
	avatarData, appErr := p.API.GetProfileImage(user.Id)
	if appErr != nil {
		p.API.LogError("Failed to get user profile image", "error", appErr, "user_id", user.Id)
		return errors.Wrap(appErr, "failed to get user profile image")
	}

	if len(avatarData) == 0 {
		p.API.LogWarn("User profile image data is empty", "user_id", user.Id)
		return nil
	}

	// Update the avatar for the ghost user (upload and set)
	err := p.matrixClient.UpdateGhostUserAvatar(ghostUserID, avatarData, "image/png")
	if err != nil {
		p.API.LogError("Failed to update ghost user avatar", "error", err, "user_id", user.Id, "ghost_user_id", ghostUserID)
		return errors.Wrap(err, "failed to update ghost user avatar on Matrix")
	}

	p.API.LogInfo("Successfully updated ghost user avatar", "user_id", user.Id, "username", user.Username, "ghost_user_id", ghostUserID)
	return nil
}