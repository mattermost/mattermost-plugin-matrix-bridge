package main

import (
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/pkg/errors"
)

// OnSharedChannelsSyncMsg is called when messages need to be synced from Mattermost to
// Matrix. One shared-channels remote is registered per homeserver, and the platform
// invokes this once per invited remote, so it resolves exactly one target server from
// rc and does no fan-out - see serverIDForSyncMsg.
func (p *Plugin) OnSharedChannelsSyncMsg(msg *model.SyncMsg, rc *model.RemoteCluster) (model.SyncResponse, error) {
	serverID, ok := p.serverIDForSyncMsg(msg.ChannelId, rc)
	if !ok {
		return model.SyncResponse{}, nil
	}

	bridge, err := p.newMattermostToMatrixBridge(serverID)
	if err != nil {
		p.logger.LogError("Matrix client not initialized for server", "server_id", serverID, "error", err)
		return model.SyncResponse{}, errors.Wrap(err, "matrix client not initialized")
	}

	// Process user sync events first (display name changes, etc.)
	for _, user := range msg.Users {
		if user.IsRemote() {
			// Matrix-originated users are only re-invited to the homeserver they
			// actually came from, never relayed across servers.
			if !p.userOriginatesFromServer(user, serverID) {
				continue
			}
			if err := p.inviteRemoteUserToMatrixRoom(serverID, user, msg.ChannelId); err != nil {
				p.logger.LogError("Failed to invite remote user to Matrix room", "error", err, "user_id", user.Id, "username", user.Username, "channel_id", msg.ChannelId, "server_id", serverID)
			}
			continue
		}

		if err := bridge.SyncUserToMatrix(user); err != nil {
			p.logger.LogError("Failed to sync user to Matrix", "error", err, "user_id", user.Id, "username", user.Username, "server_id", serverID)
			continue
		}
	}

	// Then process post sync events
	for _, post := range msg.Posts {
		// Skip syncing posts that originated from one of our own remotes to prevent
		// loops, except for deletions.
		if p.isOwnRemoteID(post.GetRemoteID()) && post.DeleteAt == 0 {
			continue
		}

		if err := bridge.SyncPostToMatrix(post, msg.ChannelId); err != nil {
			p.logger.LogError("Failed to sync post to Matrix", "error", err, "post_id", post.Id, "server_id", serverID)
			continue
		}
	}

	// Finally process reaction sync events
	for _, reaction := range msg.Reactions {
		// Skip syncing reactions that originated from one of our own remotes to prevent loops
		if p.isOwnRemoteID(reaction.GetRemoteID()) {
			continue
		}

		if err := bridge.SyncReactionToMatrix(reaction, msg.ChannelId); err != nil {
			p.logger.LogError("Failed to sync reaction to Matrix", "error", err, "reaction_user_id", reaction.UserId, "reaction_emoji", reaction.EmojiName, "server_id", serverID)
			continue
		}
	}

	return model.SyncResponse{}, nil
}

// OnSharedChannelsPing is called to check if the bridge is healthy and ready to process
// messages for the pinged remote's server. Healthy-but-idle (true) when there is no
// resolvable server (defensive) or the pinged server is disabled.
func (p *Plugin) OnSharedChannelsPing(rc *model.RemoteCluster) bool {
	if rc == nil || rc.RemoteId == "" {
		return true
	}

	serverID, ok := p.serverIDForRemoteID(rc.RemoteId)
	if !ok {
		return true
	}

	server, err := p.serverConfigForRouting(serverID)
	if err != nil || !server.Enabled {
		p.logger.LogDebug("Ping received for disabled or unregistered server; healthy-but-idle", "server_id", serverID)
		return true
	}

	client := p.getMatrixClient(serverID)
	if client == nil {
		p.logger.LogWarn("Ping failed - no Matrix client for server", "server_id", serverID)
		return false
	}

	if err := client.TestConnection(); err != nil {
		p.logger.LogWarn("Ping failed - Matrix connection test failed", "server_id", serverID, "error", err)
		return false
	}

	p.logger.LogDebug("Ping successful - Matrix bridge is healthy", "server_id", serverID)
	return true
}

// OnSharedChannelsAttachmentSyncMsg is called when file attachments need to be synced.
// Targets a single server, resolved the same way as OnSharedChannelsSyncMsg - a
// mxc:// URI is only valid on the server it was uploaded to.
func (p *Plugin) OnSharedChannelsAttachmentSyncMsg(fi *model.FileInfo, post *model.Post, rc *model.RemoteCluster) error {
	serverID, ok := p.serverIDForSyncMsg(post.ChannelId, rc)
	if !ok {
		return nil
	}

	client := p.getMatrixClient(serverID)
	if client == nil {
		return errors.Errorf("matrix client not initialized for server %s", serverID)
	}

	// Skip syncing file attachments that originated from one of our own remotes to
	// prevent loops, except for deletions.
	if fi.RemoteId != nil && p.isOwnRemoteID(*fi.RemoteId) && fi.DeleteAt == 0 {
		return nil
	}

	p.logger.LogDebug("Received attachment sync", "file_id", fi.Id, "post_id", post.Id, "filename", fi.Name, "server_id", serverID)

	// Check if this is a file deletion
	if fi.DeleteAt != 0 {
		return p.deleteFileFromMatrix(serverID, fi, post)
	}

	bridge, err := p.newMattermostToMatrixBridge(serverID)
	if err != nil {
		return err
	}

	// Get the Matrix room identifier for this channel
	matrixRoomIdentifier, err := bridge.GetMatrixRoomID(post.ChannelId)
	if err != nil {
		return errors.Wrap(err, "failed to get Matrix room identifier for attachment")
	}

	if matrixRoomIdentifier == "" {
		p.logger.LogWarn("No Matrix room mapped for channel", "channel_id", post.ChannelId, "server_id", serverID)
		return nil
	}

	// Get the file data from Mattermost
	fileData, appErr := p.API.GetFile(fi.Id)
	if appErr != nil {
		return errors.Wrap(appErr, "failed to get file data from Mattermost")
	}

	// Upload file to Matrix but don't post it yet - just store the mxc:// URI
	mxcURI, err := client.UploadMedia(fileData, fi.Name, fi.MimeType)
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
	p.pendingFiles.AddFile(serverID, post.Id, pendingFile)

	p.logger.LogDebug("Successfully uploaded attachment to Matrix (pending post)", "filename", fi.Name, "size", fi.Size, "post_id", post.Id, "mxc_uri", mxcURI, "server_id", serverID)
	return nil
}

// deleteFileFromMatrix handles deleting a file attachment from Matrix on serverID
func (p *Plugin) deleteFileFromMatrix(serverID string, fi *model.FileInfo, post *model.Post) error {
	p.logger.LogDebug("Deleting file attachment from Matrix", "file_id", fi.Id, "post_id", post.Id, "filename", fi.Name, "server_id", serverID)

	// First, try to remove from pending files (if the post hasn't been synced yet)
	if p.pendingFiles.RemoveFile(serverID, post.Id, fi.Id) {
		p.logger.LogDebug("Removed file from pending uploads", "filename", fi.Name, "file_id", fi.Id, "post_id", post.Id, "server_id", serverID)
		return nil
	}

	client := p.getMatrixClient(serverID)
	if client == nil {
		return errors.Errorf("matrix client not initialized for server %s", serverID)
	}

	bridge, err := p.newMattermostToMatrixBridge(serverID)
	if err != nil {
		return err
	}

	// If not in pending files, the file was already posted to Matrix - need to delete from Matrix
	matrixRoomIdentifier, err := bridge.GetMatrixRoomID(post.ChannelId)
	if err != nil {
		return errors.Wrap(err, "failed to get Matrix room identifier for file deletion")
	}

	if matrixRoomIdentifier == "" {
		p.logger.LogWarn("No Matrix room mapped for channel", "channel_id", post.ChannelId, "server_id", serverID)
		return nil
	}

	// Resolve room alias to room ID if needed
	matrixRoomID, err := client.ResolveRoomAlias(matrixRoomIdentifier)
	if err != nil {
		return errors.Wrap(err, "failed to resolve Matrix room identifier for file deletion")
	}

	// Get Matrix event ID from post properties - this is the message the file was attached to
	propertyKey, err := bridge.matrixEventIDPropertyKey()
	if err != nil {
		return errors.Wrap(err, "failed to get Matrix event ID property key for file deletion")
	}

	var postEventID string
	if post.Props != nil {
		if eventID, ok := post.Props[propertyKey].(string); ok {
			postEventID = eventID
		}
	}

	if postEventID == "" {
		p.logger.LogWarn("No Matrix event ID found for post with file attachment", "post_id", post.Id, "file_id", fi.Id, "server_id", serverID)
		return nil // Can't find related file attachments without the post's Matrix event ID
	}

	// Get the user who posted this attachment
	user, appErr := p.API.GetUser(post.UserId)
	if appErr != nil {
		return errors.Wrap(appErr, "failed to get user for file deletion")
	}

	// Check if ghost user exists
	ghostUserID, exists := p.getGhostUser(serverID, user.Id)
	if !exists {
		p.logger.LogWarn("No ghost user found for file deletion", "user_id", post.UserId, "file_id", fi.Id, "server_id", serverID)
		return nil // Can't delete a file from a user that doesn't have a ghost user
	}

	// Find and delete the file message from Matrix
	if err := p.findAndDeleteFileMessage(client, matrixRoomID, ghostUserID, fi.Name, postEventID); err != nil {
		return errors.Wrap(err, "failed to find and delete file message in Matrix")
	}

	p.logger.LogDebug("Successfully deleted file attachment from Matrix", "filename", fi.Name, "file_id", fi.Id, "post_id", post.Id, "server_id", serverID)
	return nil
}

// inviteRemoteUserToMatrixRoom invites a Matrix user to their corresponding Matrix room
// on serverID when added to a shared channel.
func (p *Plugin) inviteRemoteUserToMatrixRoom(serverID string, user *model.User, channelID string) error {
	client := p.getMatrixClient(serverID)
	if client == nil {
		return errors.Errorf("matrix client not initialized for server %s", serverID)
	}

	bridge, err := p.newMattermostToMatrixBridge(serverID)
	if err != nil {
		return err
	}

	// Check if this channel is mapped to a Matrix room on this server. A read error here
	// is a genuine failure, not "unbridged channel" - propagate it, consistent with the
	// other GetMatrixRoomID call sites in this file.
	matrixRoomID, err := bridge.GetMatrixRoomID(channelID)
	if err != nil {
		return errors.Wrap(err, "failed to get Matrix room identifier for remote user invite")
	}

	if matrixRoomID == "" {
		p.logger.LogDebug("No Matrix room found for channel, skipping remote user invite", "channel_id", channelID, "user_id", user.Id, "server_id", serverID)
		return nil
	}

	// Get the original Matrix user ID for this remote Mattermost user
	originalMatrixUserID, err := bridge.GetMatrixUserIDFromMattermostUser(user.Id)
	if err != nil {
		p.logger.LogWarn("Failed to get original Matrix user ID for remote user", "error", err, "user_id", user.Id, "username", user.Username, "server_id", serverID)
		return errors.Wrap(err, "failed to get original Matrix user ID")
	}

	// Resolve room alias to room ID (handles both aliases and room IDs)
	resolvedRoomID, err := client.ResolveRoomAlias(matrixRoomID)
	if err != nil {
		p.logger.LogWarn("Failed to resolve Matrix room identifier", "error", err, "room_identifier", matrixRoomID, "server_id", serverID)
		return errors.Wrap(err, "failed to resolve Matrix room identifier")
	}

	// Invite the original Matrix user to the room
	if err := client.InviteUserToRoom(resolvedRoomID, originalMatrixUserID); err != nil {
		p.logger.LogWarn("Failed to invite Matrix user to room", "error", err, "matrix_user_id", originalMatrixUserID, "room_id", resolvedRoomID, "mattermost_user_id", user.Id, "server_id", serverID)
		return errors.Wrap(err, "failed to invite Matrix user to room")
	}

	p.logger.LogInfo("Successfully invited Matrix user to room", "matrix_user_id", originalMatrixUserID, "room_id", resolvedRoomID, "mattermost_user_id", user.Id, "username", user.Username, "channel_id", channelID, "server_id", serverID)
	return nil
}

// OnSharedChannelsProfileImageSyncMsg is called when user profile images need to be
// synced. rc is available, so a single target server is resolved from it and only that
// server's ghost user is updated.
func (p *Plugin) OnSharedChannelsProfileImageSyncMsg(user *model.User, rc *model.RemoteCluster) error {
	if rc == nil || rc.RemoteId == "" {
		return nil
	}

	serverID, ok := p.serverIDForRemoteID(rc.RemoteId)
	if !ok {
		return nil
	}

	server, err := p.serverConfigForRouting(serverID)
	if err != nil || !server.Enabled {
		return nil
	}

	client := p.getMatrixClient(serverID)
	if client == nil {
		return errors.Errorf("matrix client not initialized for server %s", serverID)
	}

	// Skip syncing profile images for users that originated from one of our own remotes
	// to prevent loops.
	if p.isOwnRemoteID(user.GetRemoteID()) {
		return nil
	}

	p.logger.LogDebug("Received profile image sync", "user_id", user.Id, "username", user.Username, "server_id", serverID)

	// Check if we have a ghost user for this Mattermost user on this server
	ghostUserID, exists := p.getGhostUser(serverID, user.Id)
	if !exists {
		p.logger.LogDebug("No ghost user found for profile image sync", "user_id", user.Id, "username", user.Username, "server_id", serverID)
		return nil // No ghost user exists yet, nothing to update
	}

	p.logger.LogDebug("Found ghost user for profile image sync", "user_id", user.Id, "ghost_user_id", ghostUserID, "server_id", serverID)

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
	if err := client.UpdateGhostUserAvatar(ghostUserID, avatarData, "image/png"); err != nil {
		p.logger.LogError("Failed to update ghost user avatar", "error", err, "user_id", user.Id, "ghost_user_id", ghostUserID, "server_id", serverID)
		return errors.Wrap(err, "failed to update ghost user avatar on Matrix")
	}

	p.logger.LogDebug("Successfully updated ghost user avatar", "user_id", user.Id, "username", user.Username, "ghost_user_id", ghostUserID, "server_id", serverID)
	return nil
}
