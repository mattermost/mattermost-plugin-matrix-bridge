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

	for _, post := range msg.Posts {
		if err := p.syncPostToMatrix(post, msg.ChannelId); err != nil {
			p.API.LogError("Failed to sync post to Matrix", "error", err, "post_id", post.Id)
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

	p.API.LogDebug("Received profile image sync", "user_id", user.Id, "username", user.Username)
	
	// For now, we'll log the profile image sync but not implement it
	// TODO: Implement Matrix avatar sync for user profile images
	p.API.LogInfo("Profile image sync not yet implemented", "username", user.Username)
	
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
	// Get or create ghost user with display name
	displayName := user.GetDisplayName(model.ShowFullName)
	ghostUserID, err := p.getOrCreateGhostUser(user.Id, user.Username, displayName)
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

// getOrCreateGhostUser retrieves or creates a Matrix ghost user for a Mattermost user
func (p *Plugin) getOrCreateGhostUser(mattermostUserID, mattermostUsername, displayName string) (string, error) {
	// Check if we already have this ghost user cached
	ghostUserKey := "ghost_user_" + mattermostUserID
	ghostUserIDBytes, err := p.kvstore.Get(ghostUserKey)
	if err == nil && len(ghostUserIDBytes) > 0 {
		// Ghost user already exists
		return string(ghostUserIDBytes), nil
	}

	// Create new ghost user with display name
	ghostUser, err := p.matrixClient.CreateGhostUser(mattermostUserID, mattermostUsername, displayName)
	if err != nil {
		// Check if this is a display name error (user was created but display name failed)
		if ghostUser != nil && ghostUser.UserID != "" {
			p.API.LogWarn("Ghost user created but display name setting failed", "error", err, "ghost_user_id", ghostUser.UserID, "display_name", displayName)
			// Continue with caching - user creation was successful
		} else {
			return "", errors.Wrap(err, "failed to create ghost user")
		}
	}

	// Cache the ghost user ID
	err = p.kvstore.Set(ghostUserKey, []byte(ghostUser.UserID))
	if err != nil {
		p.API.LogWarn("Failed to cache ghost user ID", "error", err, "ghost_user_id", ghostUser.UserID)
		// Continue anyway, the ghost user was created successfully
	}

	if displayName != "" {
		p.API.LogInfo("Created new ghost user with display name", "mattermost_user_id", mattermostUserID, "ghost_user_id", ghostUser.UserID, "display_name", displayName)
	} else {
		p.API.LogInfo("Created new ghost user", "mattermost_user_id", mattermostUserID, "ghost_user_id", ghostUser.UserID)
	}
	return ghostUser.UserID, nil
}

// ensureGhostUserInRoom ensures that a ghost user is joined to a specific Matrix room
func (p *Plugin) ensureGhostUserInRoom(ghostUserID, matrixRoomID, mattermostUserID string) error {
	// Check if we've already confirmed this ghost user is in this room
	roomMembershipKey := "ghost_room_" + mattermostUserID + "_" + matrixRoomID
	membershipBytes, err := p.kvstore.Get(roomMembershipKey)
	if err == nil && len(membershipBytes) > 0 && string(membershipBytes) == "joined" {
		// Ghost user is already confirmed to be in this room
		return nil
	}

	// Attempt to join the ghost user to the room
	err = p.matrixClient.JoinRoomAsUser(matrixRoomID, ghostUserID)
	if err != nil {
		p.API.LogWarn("Failed to join ghost user to room", "error", err, "ghost_user_id", ghostUserID, "room_id", matrixRoomID)
		return errors.Wrap(err, "failed to join ghost user to room")
	}

	// Cache the successful join
	err = p.kvstore.Set(roomMembershipKey, []byte("joined"))
	if err != nil {
		p.API.LogWarn("Failed to cache ghost user room membership", "error", err, "ghost_user_id", ghostUserID, "room_id", matrixRoomID)
		// Continue anyway, the join was successful
	}

	p.API.LogDebug("Ghost user joined room successfully", "ghost_user_id", ghostUserID, "room_id", matrixRoomID)
	return nil
}