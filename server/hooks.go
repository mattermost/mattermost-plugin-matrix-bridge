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

	// A channel may be shared with several Matrix servers; dispatch to each one it
	// is mapped to. An unmapped, non-DM channel resolves to no servers and is
	// skipped.
	serverIDs, err := p.resolveOutboundServers(msg.ChannelId)
	if err != nil {
		p.logger.LogError("Failed to resolve target servers for channel", "error", err, "channel_id", msg.ChannelId)
		return model.SyncResponse{}, err
	}

	for _, serverID := range serverIDs {
		if p.getMatrixClient(serverID) == nil {
			p.logger.LogWarn("No Matrix client for target server; skipping", "server_id", serverID, "channel_id", msg.ChannelId)
			continue
		}
		bridge := p.newMattermostToMatrixBridge(serverID)

		// Process user sync events first (display name changes, etc.)
		for _, user := range msg.Users {
			if user.IsRemote() {
				// Matrix-originated user: only meaningful on its own homeserver.
				// Invite their original Matrix identity to this server's room when
				// this is the server they came from; otherwise skip (we do not relay
				// one homeserver's users onto another).
				if nativeServerID, ok := p.serverIDForRemoteID(user.GetRemoteID()); ok && nativeServerID == serverID {
					if err := p.inviteRemoteUserToMatrixRoomForServer(serverID, user, msg.ChannelId); err != nil {
						p.logger.LogError("Failed to invite remote user to Matrix room", "error", err, "user_id", user.Id, "username", user.Username, "channel_id", msg.ChannelId, "server_id", serverID)
					}
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
			// Skip syncing posts that originated from any of our Matrix servers to
			// prevent loops, except for deletions.
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
			// Skip syncing reactions that originated from any of our Matrix servers to prevent loops
			if p.isOwnRemoteID(reaction.GetRemoteID()) {
				continue
			}

			if err := bridge.SyncReactionToMatrix(reaction, msg.ChannelId); err != nil {
				p.logger.LogError("Failed to sync reaction to Matrix", "error", err, "reaction_user_id", reaction.UserId, "reaction_emoji", reaction.EmojiName, "server_id", serverID)
				continue
			}
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
	matrixClient := p.GetMatrixClient()
	if matrixClient == nil {
		p.logger.LogWarn("Ping failed - Matrix client not initialized")
		return false
	}

	// Test Matrix connection health
	if config.MatrixServerURL != "" && config.MatrixASToken != "" {
		if err := matrixClient.TestConnection(); err != nil {
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

	// Skip syncing file attachments that originated from any of our Matrix servers to prevent loops, except for deletions
	if fi.RemoteId != nil && p.isOwnRemoteID(*fi.RemoteId) && fi.DeleteAt == 0 {
		return nil
	}

	p.logger.LogDebug("Received attachment sync", "file_id", fi.Id, "post_id", post.Id, "filename", fi.Name)

	// Check if this is a file deletion
	if fi.DeleteAt != 0 {
		return p.deleteFileFromMatrix(fi, post)
	}

	// A channel may be shared with several Matrix servers; the mxc URI is only
	// valid on the server it was uploaded to, so upload once per target server and
	// track the pending file per (server, post).
	serverIDs, err := p.resolveOutboundServers(post.ChannelId)
	if err != nil {
		return errors.Wrap(err, "failed to resolve target servers for attachment")
	}
	if len(serverIDs) == 0 {
		p.logger.LogWarn("No Matrix room mapped for channel", "channel_id", post.ChannelId)
		return nil
	}

	// Get the file data from Mattermost once; it is reused across servers.
	fileData, appErr := p.API.GetFile(fi.Id)
	if appErr != nil {
		return errors.Wrap(appErr, "failed to get file data from Mattermost")
	}

	for _, serverID := range serverIDs {
		matrixClient := p.getMatrixClient(serverID)
		if matrixClient == nil {
			p.logger.LogWarn("No Matrix client for target server; skipping attachment", "server_id", serverID, "channel_id", post.ChannelId)
			continue
		}

		// Upload file to Matrix but don't post it yet - just store the mxc:// URI
		mxcURI, err := matrixClient.UploadMedia(fileData, fi.Name, fi.MimeType)
		if err != nil {
			p.logger.LogError("Failed to upload file to Matrix", "error", err, "file_id", fi.Id, "server_id", serverID)
			continue
		}

		// Store the uploaded file as pending for this post on this server
		pendingFile := &PendingFile{
			FileID:   fi.Id,
			Filename: fi.Name,
			MxcURI:   mxcURI,
			MimeType: fi.MimeType,
			Size:     fi.Size,
		}
		p.pendingFiles.AddFile(serverID, post.Id, pendingFile)

		p.logger.LogDebug("Successfully uploaded attachment to Matrix (pending post)", "filename", fi.Name, "size", fi.Size, "post_id", post.Id, "mxc_uri", mxcURI, "server_id", serverID)
	}

	return nil
}

// deleteFileFromMatrix handles deleting a file attachment from every Matrix
// server the post's channel is bridged to.
func (p *Plugin) deleteFileFromMatrix(fi *model.FileInfo, post *model.Post) error {
	p.logger.LogDebug("Deleting file attachment from Matrix", "file_id", fi.Id, "post_id", post.Id, "filename", fi.Name)

	serverIDs, err := p.resolveOutboundServers(post.ChannelId)
	if err != nil {
		return errors.Wrap(err, "failed to resolve target servers for file deletion")
	}
	if len(serverIDs) == 0 {
		p.logger.LogWarn("No Matrix room mapped for channel", "channel_id", post.ChannelId)
		return nil
	}

	for _, serverID := range serverIDs {
		if err := p.deleteFileFromMatrixForServer(serverID, fi, post); err != nil {
			p.logger.LogError("Failed to delete file attachment from Matrix", "error", err, "file_id", fi.Id, "post_id", post.Id, "server_id", serverID)
			// Continue with the other servers rather than aborting.
		}
	}
	return nil
}

// deleteFileFromMatrixForServer deletes a file attachment from one Matrix server.
func (p *Plugin) deleteFileFromMatrixForServer(serverID string, fi *model.FileInfo, post *model.Post) error {
	// First, try to remove from pending files (if the post hasn't been synced yet)
	if p.pendingFiles.RemoveFile(serverID, post.Id, fi.Id) {
		p.logger.LogDebug("Removed file from pending uploads", "filename", fi.Name, "file_id", fi.Id, "post_id", post.Id, "server_id", serverID)
		return nil
	}

	matrixClient := p.getMatrixClient(serverID)
	if matrixClient == nil {
		return errors.Errorf("no Matrix client for server %s", serverID)
	}

	bridge := p.newMattermostToMatrixBridge(serverID)

	// If not in pending files, the file was already posted to Matrix - need to delete from Matrix
	// Get the Matrix room identifier for this channel
	matrixRoomIdentifier, err := bridge.GetMatrixRoomID(post.ChannelId)
	if err != nil {
		return errors.Wrap(err, "failed to get Matrix room identifier for file deletion")
	}

	if matrixRoomIdentifier == "" {
		p.logger.LogWarn("No Matrix room mapped for channel", "channel_id", post.ChannelId, "server_id", serverID)
		return nil
	}

	// Resolve room alias to room ID if needed
	matrixRoomID, err := matrixClient.ResolveRoomAlias(matrixRoomIdentifier)
	if err != nil {
		return errors.Wrap(err, "failed to resolve Matrix room identifier for file deletion")
	}

	// Get Matrix event ID from post properties - this is the message the file was
	// attached to. The property key is per-server.
	propertyKey := "matrix_event_id_" + bridge.serverDomain()

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

	// Check if ghost user exists on this server
	ghostUserID, exists := p.getGhostUserForServer(serverID, user.Id)
	if !exists {
		p.logger.LogWarn("No ghost user found for file deletion", "user_id", post.UserId, "file_id", fi.Id, "server_id", serverID)
		return nil // Can't delete a file from a user that doesn't have a ghost user
	}

	// Find and delete the file message from Matrix
	if err := p.findAndDeleteFileMessage(matrixClient, matrixRoomID, ghostUserID, fi.Name, postEventID); err != nil {
		return errors.Wrap(err, "failed to find and delete file message in Matrix")
	}

	p.logger.LogDebug("Successfully deleted file attachment from Matrix", "filename", fi.Name, "file_id", fi.Id, "post_id", post.Id, "server_id", serverID)
	return nil
}

// inviteRemoteUserToMatrixRoomForServer invites a Matrix user to their
// corresponding room on a specific server when added to a shared channel. It is
// only meaningful on the homeserver the user came from, where their original
// Matrix identity resolves.
func (p *Plugin) inviteRemoteUserToMatrixRoomForServer(serverID string, user *model.User, channelID string) error {
	matrixClient := p.getMatrixClient(serverID)
	if matrixClient == nil {
		return errors.Errorf("no Matrix client for server %s", serverID)
	}
	bridge := p.newMattermostToMatrixBridge(serverID)

	// Check if this channel is mapped to a Matrix room on this server
	matrixRoomID, err := bridge.GetMatrixRoomID(channelID)
	if err != nil {
		p.logger.LogDebug("Channel not mapped to Matrix room, skipping remote user invite", "channel_id", channelID, "user_id", user.Id, "server_id", serverID)
		return nil // Not an error - channel might not be bridged
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
	resolvedRoomID, err := matrixClient.ResolveRoomAlias(matrixRoomID)
	if err != nil {
		p.logger.LogWarn("Failed to resolve Matrix room identifier", "error", err, "room_identifier", matrixRoomID, "server_id", serverID)
		return errors.Wrap(err, "failed to resolve Matrix room identifier")
	}

	// Invite the original Matrix user to the room
	if err := matrixClient.InviteUserToRoom(resolvedRoomID, originalMatrixUserID); err != nil {
		p.logger.LogWarn("Failed to invite Matrix user to room", "error", err, "matrix_user_id", originalMatrixUserID, "room_id", resolvedRoomID, "mattermost_user_id", user.Id, "server_id", serverID)
		return errors.Wrap(err, "failed to invite Matrix user to room")
	}

	p.logger.LogInfo("Successfully invited Matrix user to room", "matrix_user_id", originalMatrixUserID, "room_id", resolvedRoomID, "mattermost_user_id", user.Id, "username", user.Username, "channel_id", channelID, "server_id", serverID)
	return nil
}

// OnSharedChannelsProfileImageSyncMsg is called when user profile images need to be synced
func (p *Plugin) OnSharedChannelsProfileImageSyncMsg(user *model.User, _ *model.RemoteCluster) error {
	config := p.getConfiguration()
	if !config.EnableSync {
		return nil
	}

	// Skip syncing profile images for users that originated from any of our Matrix servers to prevent loops
	if p.isOwnRemoteID(user.GetRemoteID()) {
		return nil
	}

	p.logger.LogDebug("Received profile image sync", "user_id", user.Id, "username", user.Username)

	// A user may have a ghost on several servers (one per homeserver they are
	// bridged into). There is no channel context here, so update the avatar on
	// every server where a ghost exists.
	servers, err := p.getServers()
	if err != nil {
		return errors.Wrap(err, "failed to read server registry for profile image sync")
	}

	var avatarData []byte
	updated := false
	for _, server := range servers {
		ghostUserID, exists := p.getGhostUserForServer(server.ServerID, user.Id)
		if !exists {
			continue
		}
		matrixClient := p.getMatrixClient(server.ServerID)
		if matrixClient == nil {
			continue
		}

		// Fetch the avatar lazily on the first server that has a ghost.
		if avatarData == nil {
			data, appErr := p.API.GetProfileImage(user.Id)
			if appErr != nil {
				p.logger.LogError("Failed to get user profile image", "error", appErr, "user_id", user.Id)
				return errors.Wrap(appErr, "failed to get user profile image")
			}
			if len(data) == 0 {
				p.logger.LogWarn("User profile image data is empty", "user_id", user.Id)
				return nil
			}
			avatarData = data
		}

		if err := matrixClient.UpdateGhostUserAvatar(ghostUserID, avatarData, "image/png"); err != nil {
			p.logger.LogError("Failed to update ghost user avatar", "error", err, "user_id", user.Id, "ghost_user_id", ghostUserID, "server_id", server.ServerID)
			continue
		}
		updated = true
		p.logger.LogDebug("Successfully updated ghost user avatar", "user_id", user.Id, "username", user.Username, "ghost_user_id", ghostUserID, "server_id", server.ServerID)
	}

	if !updated {
		p.logger.LogDebug("No ghost user found for profile image sync", "user_id", user.Id, "username", user.Username)
	}
	return nil
}
