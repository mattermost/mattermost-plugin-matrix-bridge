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

	// Check if this post already has a Matrix event ID (indicating it's an edit)
	config := p.getConfiguration()
	serverDomain := p.extractServerDomain(config.MatrixServerURL)
	propertyKey := "matrix_event_id_" + serverDomain
	
	var existingEventID string
	if post.Props != nil {
		if eventID, ok := post.Props[propertyKey].(string); ok {
			existingEventID = eventID
		}
	}

	if existingEventID != "" {
		// Check if this is a redundant edit from adding the Matrix event ID property
		createdKey := "post_created_id_" + post.Id
		_, err := p.kvstore.Get(createdKey)
		if err == nil {
			// This post was just created and got its Matrix event ID property added
			// Delete the tracking key and skip this sync to avoid redundant edit
			p.kvstore.Delete(createdKey)
			p.API.LogDebug("Skipping redundant edit after post creation", "post_id", post.Id, "matrix_event_id", existingEventID)
			return nil
		}

		// This is a genuine post edit - update the existing Matrix message
		err = p.updatePostInMatrix(post, matrixRoomID, existingEventID, user)
		if err != nil {
			return errors.Wrap(err, "failed to update post in Matrix")
		}
		p.API.LogDebug("Successfully updated post in Matrix", "post_id", post.Id, "matrix_event_id", existingEventID)
	} else {
		// This is a new post - create new Matrix message
		err = p.createPostInMatrix(post, matrixRoomID, user, propertyKey)
		if err != nil {
			return errors.Wrap(err, "failed to create post in Matrix")
		}
		p.API.LogDebug("Successfully created new post in Matrix", "post_id", post.Id)
	}

	return nil
}

// createPostInMatrix creates a new post in Matrix and stores the event ID
func (p *Plugin) createPostInMatrix(post *model.Post, matrixRoomID string, user *model.User, propertyKey string) error {
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

	// Send message as ghost user
	sendResponse, err := p.matrixClient.SendMessageAsGhost(matrixRoomID, post.Message, ghostUserID)
	if err != nil {
		return errors.Wrap(err, "failed to send message as ghost user")
	}

	// Store the Matrix event ID as a post property for reaction mapping
	if sendResponse != nil && sendResponse.EventID != "" {
		if post.Props == nil {
			post.Props = make(map[string]interface{})
		}
		post.Props[propertyKey] = sendResponse.EventID
		
		// Mark this post as newly created to skip redundant edit sync
		createdKey := "post_created_id_" + post.Id
		err = p.kvstore.Set(createdKey, []byte("1"))
		if err != nil {
			p.API.LogWarn("Failed to set post creation tracking key", "error", err, "post_id", post.Id)
			// Continue anyway, this is just optimization
		}
		
		_, appErr := p.API.UpdatePost(post)
		if appErr != nil {
			p.API.LogWarn("Failed to update post with Matrix event ID", "error", appErr, "post_id", post.Id, "event_id", sendResponse.EventID)
			// If the update failed, clean up the tracking key since no redundant sync will occur
			p.kvstore.Delete(createdKey)
			// Continue anyway, the message was sent successfully
		}
	}

	p.API.LogDebug("Successfully created post in Matrix", "post_id", post.Id, "ghost_user_id", ghostUserID, "event_id", sendResponse.EventID)
	return nil
}

// updatePostInMatrix updates an existing post in Matrix
func (p *Plugin) updatePostInMatrix(post *model.Post, matrixRoomID string, eventID string, user *model.User) error {
	// Get or create ghost user for the update
	displayName := user.GetDisplayName(model.ShowFullName)
	
	// Get user's avatar image data
	var avatarData []byte
	var avatarContentType string
	if imageData, appErr := p.API.GetProfileImage(user.Id); appErr == nil {
		avatarData = imageData
		avatarContentType = "image/png"
	}
	
	ghostUserID, err := p.getOrCreateGhostUser(user.Id, user.Username, displayName, avatarData, avatarContentType)
	if err != nil {
		return errors.Wrap(err, "failed to get or create ghost user")
	}

	// Ensure ghost user is still in the room
	err = p.ensureGhostUserInRoom(ghostUserID, matrixRoomID, user.Id)
	if err != nil {
		return errors.Wrap(err, "failed to ensure ghost user is in room")
	}

	// Send edit as ghost user
	_, err = p.matrixClient.EditMessageAsGhost(matrixRoomID, eventID, post.Message, ghostUserID)
	if err != nil {
		return errors.Wrap(err, "failed to edit message as ghost user")
	}

	p.API.LogDebug("Successfully updated post in Matrix", "post_id", post.Id, "ghost_user_id", ghostUserID, "matrix_event_id", eventID)
	return nil
}

// syncReactionToMatrix handles syncing a reaction from Mattermost to Matrix
func (p *Plugin) syncReactionToMatrix(reaction *model.Reaction, channelID string) error {
	// Get Matrix room identifier
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

	// Get the post to find the Matrix event ID
	post, appErr := p.API.GetPost(reaction.PostId)
	if appErr != nil {
		return errors.Wrap(appErr, "failed to get post for reaction")
	}

	// Get Matrix event ID from post properties
	config := p.getConfiguration()
	serverDomain := p.extractServerDomain(config.MatrixServerURL)
	propertyKey := "matrix_event_id_" + serverDomain
	
	var matrixEventID string
	if post.Props != nil {
		if eventID, ok := post.Props[propertyKey].(string); ok {
			matrixEventID = eventID
		}
	}

	if matrixEventID == "" {
		p.API.LogWarn("No Matrix event ID found for post", "post_id", reaction.PostId, "property_key", propertyKey)
		return nil // Can't react to a message that wasn't synced to Matrix
	}

	// Get user for ghost user creation
	user, appErr := p.API.GetUser(reaction.UserId)
	if appErr != nil {
		return errors.Wrap(appErr, "failed to get user for reaction")
	}

	// Get or create ghost user
	displayName := user.GetDisplayName(model.ShowFullName)
	var avatarData []byte
	var avatarContentType string
	if imageData, appErr := p.API.GetProfileImage(user.Id); appErr == nil {
		avatarData = imageData
		avatarContentType = "image/png"
	}
	
	ghostUserID, err := p.getOrCreateGhostUser(user.Id, user.Username, displayName, avatarData, avatarContentType)
	if err != nil {
		return errors.Wrap(err, "failed to get or create ghost user for reaction")
	}

	// Ensure ghost user is in the room
	err = p.ensureGhostUserInRoom(ghostUserID, matrixRoomID, user.Id)
	if err != nil {
		return errors.Wrap(err, "failed to ensure ghost user is in room for reaction")
	}

	// Convert Mattermost emoji name to Matrix reaction format
	emoji := p.convertEmojiForMatrix(reaction.EmojiName)

	// Send reaction as ghost user
	_, err = p.matrixClient.SendReactionAsGhost(matrixRoomID, matrixEventID, emoji, ghostUserID)
	if err != nil {
		return errors.Wrap(err, "failed to send reaction as ghost user")
	}

	p.API.LogDebug("Successfully synced reaction as ghost user", "post_id", reaction.PostId, "emoji", reaction.EmojiName, "ghost_user_id", ghostUserID, "matrix_event_id", matrixEventID)
	return nil
}