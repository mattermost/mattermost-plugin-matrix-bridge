package main

import (
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/pkg/errors"
)

// OnSharedChannelsSyncMsg is called when messages need to be synced from Mattermost to Matrix
func (p *Plugin) OnSharedChannelsSyncMsg(msg *model.SyncMsg, _ *model.RemoteCluster) (model.SyncResponse, error) {
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
func (p *Plugin) OnSharedChannelsPing(_ *model.RemoteCluster) bool {
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
func (p *Plugin) OnSharedChannelsAttachmentSyncMsg(fi *model.FileInfo, post *model.Post, _ *model.RemoteCluster) error {
	config := p.getConfiguration()
	if !config.EnableSync {
		return nil
	}

	if p.matrixClient == nil {
		return errors.New("matrix client not initialized")
	}

	p.API.LogDebug("Received attachment sync", "file_id", fi.Id, "post_id", post.Id, "filename", fi.Name)

	// Check if this is a file deletion
	if fi.DeleteAt != 0 {
		return p.deleteFileFromMatrix(fi, post)
	}

	// Get the Matrix room identifier for this channel
	matrixRoomIdentifier, err := p.getMatrixRoomID(post.ChannelId)
	if err != nil {
		return errors.Wrap(err, "failed to get Matrix room identifier for attachment")
	}

	if matrixRoomIdentifier == "" {
		p.API.LogWarn("No Matrix room mapped for channel", "channel_id", post.ChannelId)
		return nil
	}

	// Get the file data from Mattermost
	fileData, appErr := p.API.GetFile(fi.Id)
	if appErr != nil {
		return errors.Wrap(appErr, "failed to get file data from Mattermost")
	}

	// Upload file to Matrix but don't post it yet - just store the mxc:// URI
	mxcURI, err := p.matrixClient.UploadMedia(fileData, fi.Name, fi.MimeType)
	if err != nil {
		return errors.Wrap(err, "failed to upload file to Matrix")
	}

	// Store the uploaded file as pending for this post
	pendingFile := &PendingFile{
		FileID:   fi.Id,
		Filename: fi.Name,
		MxcURI:   mxcURI,
		MimeType: fi.MimeType,
		Size:     fi.Size,
	}
	p.pendingFiles.AddFile(post.Id, pendingFile)

	p.API.LogInfo("Successfully uploaded attachment to Matrix (pending post)", "filename", fi.Name, "size", fi.Size, "post_id", post.Id, "mxc_uri", mxcURI)
	return nil
}

// deleteFileFromMatrix handles deleting a file attachment from Matrix
func (p *Plugin) deleteFileFromMatrix(fi *model.FileInfo, post *model.Post) error {
	p.API.LogDebug("Deleting file attachment from Matrix", "file_id", fi.Id, "post_id", post.Id, "filename", fi.Name)

	// First, try to remove from pending files (if the post hasn't been synced yet)
	if p.pendingFiles.RemoveFile(post.Id, fi.Id) {
		p.API.LogInfo("Removed file from pending uploads", "filename", fi.Name, "file_id", fi.Id, "post_id", post.Id)
		return nil
	}

	// If not in pending files, the file was already posted to Matrix - need to delete from Matrix
	// Get the Matrix room identifier for this channel
	matrixRoomIdentifier, err := p.getMatrixRoomID(post.ChannelId)
	if err != nil {
		return errors.Wrap(err, "failed to get Matrix room identifier for file deletion")
	}

	if matrixRoomIdentifier == "" {
		p.API.LogWarn("No Matrix room mapped for channel", "channel_id", post.ChannelId)
		return nil
	}

	// Resolve room alias to room ID if needed
	matrixRoomID, err := p.matrixClient.ResolveRoomAlias(matrixRoomIdentifier)
	if err != nil {
		return errors.Wrap(err, "failed to resolve Matrix room identifier for file deletion")
	}

	// Get Matrix event ID from post properties - this is the message the file was attached to
	config := p.getConfiguration()
	serverDomain := p.extractServerDomain(config.MatrixServerURL)
	propertyKey := "matrix_event_id_" + serverDomain

	var postEventID string
	if post.Props != nil {
		if eventID, ok := post.Props[propertyKey].(string); ok {
			postEventID = eventID
		}
	}

	if postEventID == "" {
		p.API.LogWarn("No Matrix event ID found for post with file attachment", "post_id", post.Id, "file_id", fi.Id)
		return nil // Can't find related file attachments without the post's Matrix event ID
	}

	// Get the user who posted this attachment
	user, appErr := p.API.GetUser(post.UserId)
	if appErr != nil {
		return errors.Wrap(appErr, "failed to get user for file deletion")
	}

	// Check if ghost user exists
	ghostUserID, exists := p.getGhostUser(user.Id)
	if !exists {
		p.API.LogWarn("No ghost user found for file deletion", "user_id", post.UserId, "file_id", fi.Id)
		return nil // Can't delete a file from a user that doesn't have a ghost user
	}

	// Find and delete the file message from Matrix
	err = p.findAndDeleteFileMessage(matrixRoomID, ghostUserID, fi.Name, postEventID)
	if err != nil {
		return errors.Wrap(err, "failed to find and delete file message in Matrix")
	}

	p.API.LogInfo("Successfully deleted file attachment from Matrix", "filename", fi.Name, "file_id", fi.Id, "post_id", post.Id)
	return nil
}

// OnSharedChannelsProfileImageSyncMsg is called when user profile images need to be synced
func (p *Plugin) OnSharedChannelsProfileImageSyncMsg(user *model.User, _ *model.RemoteCluster) error {
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
