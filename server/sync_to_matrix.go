package main

import (
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/pkg/errors"
	"github.com/wiggin77/mattermost-plugin-matrix-bridge/server/matrix"
)

// syncUserToMatrix handles syncing user changes (like display name) to Matrix ghost users
func (p *Plugin) syncUserToMatrix(user *model.User) error {
	p.API.LogDebug("Syncing user to Matrix", "user_id", user.Id, "username", user.Username)

	// Check if we have a ghost user for this Mattermost user
	ghostUserID, exists := p.getGhostUser(user.Id)
	if !exists {
		p.API.LogDebug("No ghost user found for user sync", "user_id", user.Id, "username", user.Username)
		return nil // No ghost user exists yet, nothing to update
	}

	p.API.LogDebug("Found ghost user for user sync", "user_id", user.Id, "ghost_user_id", ghostUserID)

	// Update display name
	displayName := user.GetDisplayName(model.ShowFullName)
	if displayName != "" {
		err := p.matrixClient.SetDisplayName(ghostUserID, displayName)
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
	// Check if this is a post deletion
	if post.DeleteAt != 0 {
		return p.deletePostFromMatrix(post, channelID)
	}

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
		if storedUpdateAt, exists := p.postTracker.Get(post.Id); exists {
			if post.UpdateAt == storedUpdateAt {
				// This post's UpdateAt matches the timestamp we stored when adding Matrix event ID
				// This is the redundant edit from adding the Matrix event ID property
				p.postTracker.Delete(post.Id)
				p.API.LogDebug("Skipping redundant edit after post creation", "post_id", post.Id, "matrix_event_id", existingEventID, "stored_update_at", storedUpdateAt, "current_update_at", post.UpdateAt)
				return nil
			}
			// This is a genuine edit that happened after we added the Matrix event ID
			// Remove the tracking entry since we're processing a real edit now
			p.postTracker.Delete(post.Id)
			p.API.LogDebug("Processing genuine edit after post creation", "post_id", post.Id, "matrix_event_id", existingEventID, "stored_update_at", storedUpdateAt, "current_update_at", post.UpdateAt)
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
	// Create or get ghost user
	ghostUserID, err := p.CreateOrGetGhostUser(user.Id)
	if err != nil {
		return errors.Wrap(err, "failed to create or get ghost user")
	}

	// Ensure ghost user is joined to the room
	err = p.ensureGhostUserInRoom(ghostUserID, matrixRoomID, user.Id)
	if err != nil {
		return errors.Wrap(err, "failed to ensure ghost user is in room")
	}

	// Convert post content to Matrix format
	plainText, htmlContent := convertMattermostToMatrix(post.Message)

	// Check if this is a threaded post (reply to another post)
	var threadEventID string
	if post.RootId != "" {
		// This is a reply - find the Matrix event ID of the root post
		rootPost, appErr := p.API.GetPost(post.RootId)
		if appErr != nil {
			p.API.LogWarn("Failed to get root post for thread", "error", appErr, "post_id", post.Id, "root_id", post.RootId)
			// Continue without threading - send as regular message
		} else {
			// Get Matrix event ID from root post properties
			if rootPost.Props != nil {
				if eventID, ok := rootPost.Props[propertyKey].(string); ok {
					threadEventID = eventID
				}
			}
			if threadEventID == "" {
				p.API.LogWarn("Root post has no Matrix event ID for threading", "post_id", post.Id, "root_id", post.RootId)
				// Continue without threading - send as regular message
			}
		}
	}

	// Check for pending file attachments for this post
	pendingFiles := p.pendingFiles.GetFiles(post.Id)

	// Send message as ghost user (formatted if HTML content exists, threaded if threadEventID is provided)
	var sendResponse *matrix.SendEventResponse
	if len(pendingFiles) > 0 {
		// Convert pending files to the format expected by SendMessageWithFilesAsGhost
		var files []map[string]interface{}
		for _, file := range pendingFiles {
			files = append(files, map[string]interface{}{
				"filename": file.Filename,
				"mxc_uri":  file.MxcURI,
				"mimetype": file.MimeType,
				"size":     file.Size,
			})
		}

		sendResponse, err = p.matrixClient.SendMessageWithFilesAsGhost(matrixRoomID, plainText, htmlContent, threadEventID, ghostUserID, files)
		if err != nil {
			return errors.Wrap(err, "failed to send message with files as ghost user")
		}

		p.API.LogInfo("Posted message with file attachments to Matrix", "post_id", post.Id, "file_count", len(pendingFiles))
	} else {
		// No files, send regular message
		sendResponse, err = p.matrixClient.SendMessageAsGhost(matrixRoomID, plainText, htmlContent, threadEventID, ghostUserID)
		if err != nil {
			return errors.Wrap(err, "failed to send message as ghost user")
		}
	}

	// Store the Matrix event ID as a post property for reaction mapping
	if sendResponse != nil && sendResponse.EventID != "" {
		if post.Props == nil {
			post.Props = make(map[string]interface{})
		}
		post.Props[propertyKey] = sendResponse.EventID

		updatedPost, appErr := p.API.UpdatePost(post)
		if appErr != nil {
			p.API.LogWarn("Failed to update post with Matrix event ID", "error", appErr, "post_id", post.Id, "event_id", sendResponse.EventID)
			// Continue anyway, the message was sent successfully
		} else {
			// Store the UpdateAt timestamp in memory to detect redundant edits
			err = p.postTracker.Put(post.Id, updatedPost.UpdateAt)
			if err != nil {
				p.API.LogWarn("Failed to store post tracking for redundant edit detection", "error", err, "post_id", post.Id, "update_at", updatedPost.UpdateAt)
				// Continue anyway - this is just an optimization to avoid redundant edits
			} else {
				p.API.LogDebug("Stored post tracking for redundant edit detection", "post_id", post.Id, "update_at", updatedPost.UpdateAt)
			}
		}
	}

	p.API.LogDebug("Successfully created post in Matrix", "post_id", post.Id, "ghost_user_id", ghostUserID, "event_id", sendResponse.EventID)
	return nil
}

// updatePostInMatrix updates an existing post in Matrix
func (p *Plugin) updatePostInMatrix(post *model.Post, matrixRoomID string, eventID string, user *model.User) error {
	// Create or get ghost user
	ghostUserID, err := p.CreateOrGetGhostUser(user.Id)
	if err != nil {
		return errors.Wrap(err, "failed to create or get ghost user")
	}

	// Ensure ghost user is still in the room
	err = p.ensureGhostUserInRoom(ghostUserID, matrixRoomID, user.Id)
	if err != nil {
		return errors.Wrap(err, "failed to ensure ghost user is in room")
	}

	// Convert post content to Matrix format
	plainText, htmlContent := convertMattermostToMatrix(post.Message)

	// Send edit as ghost user with proper HTML formatting support
	_, err = p.matrixClient.EditMessageAsGhost(matrixRoomID, eventID, plainText, htmlContent, ghostUserID)
	if err != nil {
		return errors.Wrap(err, "failed to edit message as ghost user")
	}

	p.API.LogDebug("Successfully updated post in Matrix", "post_id", post.Id, "ghost_user_id", ghostUserID, "matrix_event_id", eventID)
	return nil
}

// deletePostFromMatrix handles deleting a post from Matrix by redacting the Matrix message
func (p *Plugin) deletePostFromMatrix(post *model.Post, channelID string) error {
	// Get Matrix room identifier
	matrixRoomIdentifier, err := p.getMatrixRoomID(channelID)
	if err != nil {
		return errors.Wrap(err, "failed to get Matrix room identifier for post deletion")
	}

	if matrixRoomIdentifier == "" {
		p.API.LogWarn("No Matrix room mapped for channel", "channel_id", channelID)
		return nil
	}

	// Resolve room alias to room ID if needed
	matrixRoomID, err := p.matrixClient.ResolveRoomAlias(matrixRoomIdentifier)
	if err != nil {
		return errors.Wrap(err, "failed to resolve Matrix room identifier for post deletion")
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
		p.API.LogWarn("No Matrix event ID found for post deletion", "post_id", post.Id, "property_key", propertyKey)
		return nil // Can't delete a message that wasn't synced to Matrix
	}

	// Get user for ghost user lookup
	user, appErr := p.API.GetUser(post.UserId)
	if appErr != nil {
		return errors.Wrap(appErr, "failed to get user for post deletion")
	}

	// Check if ghost user exists (needed for redaction)
	ghostUserID, exists := p.getGhostUser(user.Id)
	if !exists {
		p.API.LogWarn("No ghost user found for post deletion", "user_id", post.UserId, "post_id", post.Id)
		return nil // Can't delete a message from a user that doesn't have a ghost user
	}

	// First, find and delete any file attachment replies to this message
	err = p.deleteAllFileReplies(matrixRoomID, matrixEventID, ghostUserID)
	if err != nil {
		p.API.LogWarn("Failed to delete file attachment replies", "error", err, "post_id", post.Id, "matrix_event_id", matrixEventID)
		// Continue anyway - we'll still delete the main message
	}

	// Redact the main message event
	_, err = p.matrixClient.RedactEventAsGhost(matrixRoomID, matrixEventID, ghostUserID)
	if err != nil {
		return errors.Wrap(err, "failed to redact post in Matrix")
	}

	p.API.LogDebug("Successfully deleted post and file attachments from Matrix", "post_id", post.Id, "ghost_user_id", ghostUserID, "matrix_event_id", matrixEventID)
	return nil
}

// syncReactionToMatrix handles syncing a reaction from Mattermost to Matrix
func (p *Plugin) syncReactionToMatrix(reaction *model.Reaction, channelID string) error {
	// Check if this is a reaction deletion
	if reaction.DeleteAt != 0 {
		return p.removeReactionFromMatrix(reaction, channelID)
	}

	// This is a new reaction - add it to Matrix
	return p.addReactionToMatrix(reaction, channelID)
}

// addReactionToMatrix adds a new reaction to Matrix
func (p *Plugin) addReactionToMatrix(reaction *model.Reaction, channelID string) error {
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

	// Create or get ghost user
	ghostUserID, err := p.CreateOrGetGhostUser(user.Id)
	if err != nil {
		return errors.Wrap(err, "failed to create or get ghost user for reaction")
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

// removeReactionFromMatrix removes a reaction from Matrix by finding and redacting the matching reaction event
func (p *Plugin) removeReactionFromMatrix(reaction *model.Reaction, channelID string) error {
	// Get Matrix room identifier
	matrixRoomIdentifier, err := p.getMatrixRoomID(channelID)
	if err != nil {
		return errors.Wrap(err, "failed to get Matrix room identifier for reaction removal")
	}

	if matrixRoomIdentifier == "" {
		p.API.LogWarn("No Matrix room mapped for channel", "channel_id", channelID)
		return nil
	}

	// Resolve room alias to room ID if needed
	matrixRoomID, err := p.matrixClient.ResolveRoomAlias(matrixRoomIdentifier)
	if err != nil {
		return errors.Wrap(err, "failed to resolve Matrix room identifier for reaction removal")
	}

	// Get the post to find the Matrix event ID
	post, appErr := p.API.GetPost(reaction.PostId)
	if appErr != nil {
		return errors.Wrap(appErr, "failed to get post for reaction removal")
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
		return nil // Can't remove reaction from a message that wasn't synced to Matrix
	}

	// Get user for ghost user creation
	user, appErr := p.API.GetUser(reaction.UserId)
	if appErr != nil {
		return errors.Wrap(appErr, "failed to get user for reaction removal")
	}

	// Check if ghost user exists (needed for determining which reaction to remove)
	ghostUserID, exists := p.getGhostUser(user.Id)
	if !exists {
		p.API.LogWarn("No ghost user found for reaction removal", "user_id", reaction.UserId, "post_id", reaction.PostId)
		return nil // Can't remove a reaction from a user that doesn't have a ghost user
	}

	// Convert Mattermost emoji name to Matrix reaction format for matching
	emoji := p.convertEmojiForMatrix(reaction.EmojiName)

	// Get all reactions for this message from Matrix (as the ghost user)
	relations, err := p.matrixClient.GetEventRelationsAsUser(matrixRoomID, matrixEventID, ghostUserID)
	if err != nil {
		return errors.Wrap(err, "failed to get event relations from Matrix")
	}

	// Find the matching reaction event to redact
	var reactionEventID string
	for _, event := range relations {
		// Check if this is a reaction event
		eventType, ok := event["type"].(string)
		if !ok || eventType != "m.reaction" {
			continue
		}

		// Check if this reaction is from our ghost user
		sender, ok := event["sender"].(string)
		if !ok || sender != ghostUserID {
			continue
		}

		// Check if this reaction has the matching emoji
		content, ok := event["content"].(map[string]interface{})
		if !ok {
			continue
		}

		relatesTo, ok := content["m.relates_to"].(map[string]interface{})
		if !ok {
			continue
		}

		key, ok := relatesTo["key"].(string)
		if !ok || key != emoji {
			continue
		}

		// Found the matching reaction event
		eventID, ok := event["event_id"].(string)
		if ok {
			reactionEventID = eventID
			break
		}
	}

	if reactionEventID == "" {
		p.API.LogWarn("No matching reaction found in Matrix to remove", "post_id", reaction.PostId, "emoji", reaction.EmojiName, "ghost_user_id", ghostUserID)
		return nil // No matching reaction found to remove
	}

	// Redact the reaction event
	_, err = p.matrixClient.RedactEventAsGhost(matrixRoomID, reactionEventID, ghostUserID)
	if err != nil {
		return errors.Wrap(err, "failed to redact reaction in Matrix")
	}

	p.API.LogDebug("Successfully removed reaction from Matrix", "post_id", reaction.PostId, "emoji", reaction.EmojiName, "ghost_user_id", ghostUserID, "reaction_event_id", reactionEventID)
	return nil
}
