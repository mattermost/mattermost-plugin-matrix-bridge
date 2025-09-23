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
		p.logger.LogError("Matrix client not initialized")
		return model.SyncResponse{}, errors.New("matrix client not initialized")
	}

	// Process user sync events first (display name changes, etc.)
	for _, user := range msg.Users {
		if user.IsRemote() {
			// This is a Matrix-originated user - invite them to the Matrix room if not already there
			if err := p.inviteRemoteUserToMatrixRoom(user, msg.ChannelId); err != nil {
				p.logger.LogError("Failed to invite remote user to Matrix room", "error", err, "user_id", user.Id, "username", user.Username, "channel_id", msg.ChannelId)
			}
			continue
		}

		if err := p.mattermostToMatrixBridge.SyncUserToMatrix(user); err != nil {
			p.logger.LogError("Failed to sync user to Matrix", "error", err, "user_id", user.Id, "username", user.Username)
			continue
		}
	}

	// Then process post sync events
	for _, post := range msg.Posts {
		// Skip syncing posts that originated from Matrix to prevent loops, except for deletions
		if post.GetRemoteID() == p.remoteID && post.DeleteAt == 0 {
			continue
		}

		if err := p.mattermostToMatrixBridge.SyncPostToMatrix(post, msg.ChannelId); err != nil {
			p.logger.LogError("Failed to sync post to Matrix", "error", err, "post_id", post.Id)
			continue
		}
	}

	// Finally process reaction sync events
	for _, reaction := range msg.Reactions {
		// Skip syncing reactions that originated from Matrix to prevent loops
		if reaction.GetRemoteID() == p.remoteID {
			continue
		}

		if err := p.mattermostToMatrixBridge.SyncReactionToMatrix(reaction, msg.ChannelId); err != nil {
			p.logger.LogError("Failed to sync reaction to Matrix", "error", err, "reaction_user_id", reaction.UserId, "reaction_emoji", reaction.EmojiName)
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
		p.logger.LogDebug("Ping received but sync is disabled")
		return true
	}

	// If Matrix client is not configured, we're not healthy
	if p.matrixClient == nil {
		p.logger.LogWarn("Ping failed - Matrix client not initialized")
		return false
	}

	// Test Matrix connection health
	if config.MatrixServerURL != "" && config.MatrixASToken != "" {
		if err := p.matrixClient.TestConnection(); err != nil {
			p.logger.LogWarn("Ping failed - Matrix connection test failed", "error", err)
			return false
		}
	} else {
		p.logger.LogWarn("Ping failed - Matrix configuration incomplete")
		return false
	}

	p.logger.LogDebug("Ping successful - Matrix bridge is healthy")
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

	// Skip syncing file attachments that originated from Matrix to prevent loops, except for deletions
	if fi.RemoteId != nil && *fi.RemoteId == p.remoteID && fi.DeleteAt == 0 {
		return nil
	}

	p.logger.LogDebug("Received attachment sync", "file_id", fi.Id, "post_id", post.Id, "filename", fi.Name)

	// Check if this is a file deletion
	if fi.DeleteAt != 0 {
		return p.deleteFileFromMatrix(fi, post)
	}

	// Get the Matrix room identifier for this channel
	matrixRoomIdentifier, err := p.mattermostToMatrixBridge.GetMatrixRoomID(post.ChannelId)
	if err != nil {
		return errors.Wrap(err, "failed to get Matrix room identifier for attachment")
	}

	if matrixRoomIdentifier == "" {
		p.logger.LogWarn("No Matrix room mapped for channel", "channel_id", post.ChannelId)
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

	p.logger.LogDebug("Successfully uploaded attachment to Matrix (pending post)", "filename", fi.Name, "size", fi.Size, "post_id", post.Id, "mxc_uri", mxcURI)
	return nil
}

// deleteFileFromMatrix handles deleting a file attachment from Matrix
func (p *Plugin) deleteFileFromMatrix(fi *model.FileInfo, post *model.Post) error {
	p.logger.LogDebug("Deleting file attachment from Matrix", "file_id", fi.Id, "post_id", post.Id, "filename", fi.Name)

	// First, try to remove from pending files (if the post hasn't been synced yet)
	if p.pendingFiles.RemoveFile(post.Id, fi.Id) {
		p.logger.LogDebug("Removed file from pending uploads", "filename", fi.Name, "file_id", fi.Id, "post_id", post.Id)
		return nil
	}

	// If not in pending files, the file was already posted to Matrix - need to delete from Matrix
	// Get the Matrix room identifier for this channel
	matrixRoomIdentifier, err := p.mattermostToMatrixBridge.GetMatrixRoomID(post.ChannelId)
	if err != nil {
		return errors.Wrap(err, "failed to get Matrix room identifier for file deletion")
	}

	if matrixRoomIdentifier == "" {
		p.logger.LogWarn("No Matrix room mapped for channel", "channel_id", post.ChannelId)
		return nil
	}

	// Resolve room alias to room ID if needed
	matrixRoomID, err := p.matrixClient.ResolveRoomAlias(matrixRoomIdentifier)
	if err != nil {
		return errors.Wrap(err, "failed to resolve Matrix room identifier for file deletion")
	}

	// Get Matrix event ID from post properties - this is the message the file was attached to
	config := p.getConfiguration()
	serverDomain := extractServerDomain(p.API, config.MatrixServerURL)
	propertyKey := "matrix_event_id_" + serverDomain

	var postEventID string
	if post.Props != nil {
		if eventID, ok := post.Props[propertyKey].(string); ok {
			postEventID = eventID
		}
	}

	if postEventID == "" {
		p.logger.LogWarn("No Matrix event ID found for post with file attachment", "post_id", post.Id, "file_id", fi.Id)
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
		p.logger.LogWarn("No ghost user found for file deletion", "user_id", post.UserId, "file_id", fi.Id)
		return nil // Can't delete a file from a user that doesn't have a ghost user
	}

	// Find and delete the file message from Matrix
	err = p.findAndDeleteFileMessage(matrixRoomID, ghostUserID, fi.Name, postEventID)
	if err != nil {
		return errors.Wrap(err, "failed to find and delete file message in Matrix")
	}

	p.logger.LogDebug("Successfully deleted file attachment from Matrix", "filename", fi.Name, "file_id", fi.Id, "post_id", post.Id)
	return nil
}

// inviteRemoteUserToMatrixRoom invites a Matrix user to their corresponding Matrix room when added to a shared channel
func (p *Plugin) inviteRemoteUserToMatrixRoom(user *model.User, channelID string) error {
	// Check if this channel is mapped to a Matrix room
	matrixRoomID, err := p.mattermostToMatrixBridge.GetMatrixRoomID(channelID)
	if err != nil {
		p.logger.LogDebug("Channel not mapped to Matrix room, skipping remote user invite", "channel_id", channelID, "user_id", user.Id)
		return nil // Not an error - channel might not be bridged
	}

	if matrixRoomID == "" {
		p.logger.LogDebug("No Matrix room found for channel, skipping remote user invite", "channel_id", channelID, "user_id", user.Id)
		return nil
	}

	// Get the original Matrix user ID for this remote Mattermost user
	originalMatrixUserID, err := p.mattermostToMatrixBridge.GetMatrixUserIDFromMattermostUser(user.Id)
	if err != nil {
		p.logger.LogWarn("Failed to get original Matrix user ID for remote user", "error", err, "user_id", user.Id, "username", user.Username)
		return errors.Wrap(err, "failed to get original Matrix user ID")
	}

	// Resolve room alias to room ID (handles both aliases and room IDs)
	resolvedRoomID, err := p.matrixClient.ResolveRoomAlias(matrixRoomID)
	if err != nil {
		p.logger.LogWarn("Failed to resolve Matrix room identifier", "error", err, "room_identifier", matrixRoomID)
		return errors.Wrap(err, "failed to resolve Matrix room identifier")
	}

	// Invite the original Matrix user to the room
	if err := p.matrixClient.InviteUserToRoom(resolvedRoomID, originalMatrixUserID); err != nil {
		p.logger.LogWarn("Failed to invite Matrix user to room", "error", err, "matrix_user_id", originalMatrixUserID, "room_id", resolvedRoomID, "mattermost_user_id", user.Id)
		return errors.Wrap(err, "failed to invite Matrix user to room")
	}

	p.logger.LogInfo("Successfully invited Matrix user to room", "matrix_user_id", originalMatrixUserID, "room_id", resolvedRoomID, "mattermost_user_id", user.Id, "username", user.Username, "channel_id", channelID)
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

	// Skip syncing profile images for users that originated from Matrix to prevent loops
	if user.GetRemoteID() == p.remoteID {
		return nil
	}

	p.logger.LogDebug("Received profile image sync", "user_id", user.Id, "username", user.Username)

	// Check if we have a ghost user for this Mattermost user
	ghostUserID, exists := p.getGhostUser(user.Id)
	if !exists {
		p.logger.LogDebug("No ghost user found for profile image sync", "user_id", user.Id, "username", user.Username)
		return nil // No ghost user exists yet, nothing to update
	}

	p.logger.LogDebug("Found ghost user for profile image sync", "user_id", user.Id, "ghost_user_id", ghostUserID)

	// Get user's new avatar image data
	avatarData, appErr := p.API.GetProfileImage(user.Id)
	if appErr != nil {
		p.logger.LogError("Failed to get user profile image", "error", appErr, "user_id", user.Id)
		return errors.Wrap(appErr, "failed to get user profile image")
	}

	if len(avatarData) == 0 {
		p.logger.LogWarn("User profile image data is empty", "user_id", user.Id)
		return nil
	}

	// Update the avatar for the ghost user (upload and set)
	err := p.matrixClient.UpdateGhostUserAvatar(ghostUserID, avatarData, "image/png")
	if err != nil {
		p.logger.LogError("Failed to update ghost user avatar", "error", err, "user_id", user.Id, "ghost_user_id", ghostUserID)
		return errors.Wrap(err, "failed to update ghost user avatar on Matrix")
	}

	p.logger.LogDebug("Successfully updated ghost user avatar", "user_id", user.Id, "username", user.Username, "ghost_user_id", ghostUserID)
	return nil
}
