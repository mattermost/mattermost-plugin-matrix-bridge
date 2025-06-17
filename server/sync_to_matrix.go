package main

import (
	"fmt"
	"regexp"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/pkg/errors"
	"github.com/wiggin77/mattermost-plugin-matrix-bridge/server/matrix"
)

// MattermostMentionResults represents extracted mentions from a Mattermost post
type MattermostMentionResults struct {
	UserMentions    []string // usernames mentioned
	ChannelMentions bool     // @channel/@all/@here
}

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
		p.API.LogDebug("Updated ghost user display name", "user_id", user.Id, "ghost_user_id", ghostUserID, "display_name", displayName)
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
	serverDomain := extractServerDomain(p.API, config.MatrixServerURL)
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

	// Process mentions first on the original text
	mentionData := p.extractMattermostMentions(post)

	// Convert post content to Matrix format
	plainText, htmlContent := convertMattermostToMatrix(post.Message)

	// Create Matrix message content structure
	messageContent := map[string]any{
		"msgtype": "m.text",
		"body":    plainText,
	}

	// Add formatted content if HTML is present
	if htmlContent != "" {
		messageContent["format"] = "org.matrix.custom.html"
		messageContent["formatted_body"] = htmlContent
	}

	// Add Matrix mentions if any @mentions exist in the post
	p.addMatrixMentionsWithData(messageContent, post, mentionData)

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

	// Prepare file attachments if any
	var fileAttachments []matrix.FileAttachment
	for _, file := range pendingFiles {
		fileAttachments = append(fileAttachments, matrix.FileAttachment{
			Filename: file.Filename,
			MxcURI:   file.MxcURI,
			MimeType: file.MimeType,
			Size:     file.Size,
		})
	}

	// Extract content from message structure
	finalPlainText := messageContent["body"].(string)
	finalHTMLContent, _ := messageContent["formatted_body"].(string)

	// Send message using consolidated method
	messageRequest := matrix.MessageRequest{
		RoomID:        matrixRoomID,
		GhostUserID:   ghostUserID,
		Message:       finalPlainText,
		HTMLMessage:   finalHTMLContent,
		ThreadEventID: threadEventID,
		PostID:        post.Id,
		Files:         fileAttachments,
	}

	sendResponse, err := p.matrixClient.SendMessage(messageRequest)
	if err != nil {
		return errors.Wrap(err, "failed to send message as ghost user")
	}

	if len(pendingFiles) > 0 {
		p.API.LogDebug("Posted message with file attachments to Matrix", "post_id", post.Id, "file_count", len(pendingFiles))
	}

	// Store the Matrix event ID as a post property for reaction mapping
	if sendResponse != nil && sendResponse.EventID != "" {
		if post.Props == nil {
			post.Props = make(map[string]any)
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

	// Process mentions first on the original text
	mentionData := p.extractMattermostMentions(post)

	// Convert post content to Matrix format
	plainText, htmlContent := convertMattermostToMatrix(post.Message)

	// Create Matrix message content structure
	messageContent := map[string]any{
		"msgtype": "m.text",
		"body":    plainText,
	}

	// Add formatted content if HTML is present
	if htmlContent != "" {
		messageContent["format"] = "org.matrix.custom.html"
		messageContent["formatted_body"] = htmlContent
	}

	// Add Matrix mentions if any @mentions exist in the post
	p.addMatrixMentionsWithData(messageContent, post, mentionData)

	// Extract content from message structure
	finalPlainText := messageContent["body"].(string)
	finalHTMLContent, _ := messageContent["formatted_body"].(string)

	// Check for pending file attachments for this post
	pendingFiles := p.pendingFiles.GetFiles(post.Id)
	var currentFiles []matrix.FileAttachment
	for _, file := range pendingFiles {
		currentFiles = append(currentFiles, matrix.FileAttachment{
			Filename: file.Filename,
			MxcURI:   file.MxcURI,
			MimeType: file.MimeType,
			Size:     file.Size,
		})
	}

	// Fetch the current Matrix event content to compare
	currentEvent, err := p.matrixClient.GetEvent(matrixRoomID, eventID)
	if err != nil {
		p.API.LogWarn("Failed to fetch current Matrix event for comparison", "error", err, "event_id", eventID)
		// Continue with update if we can't fetch current content
	} else {
		// Compare content and file attachments to see if anything actually changed
		if p.isMatrixContentIdentical(currentEvent, finalPlainText, finalHTMLContent, matrixRoomID, eventID, currentFiles) {
			p.API.LogDebug("Matrix message content and attachments unchanged, skipping edit", "post_id", post.Id, "matrix_event_id", eventID)
			return nil
		}
	}

	// Send edit as ghost user with proper HTML formatting support
	_, err = p.matrixClient.EditMessageAsGhost(matrixRoomID, eventID, finalPlainText, finalHTMLContent, ghostUserID)
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
	serverDomain := extractServerDomain(p.API, config.MatrixServerURL)
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
	serverDomain := extractServerDomain(p.API, config.MatrixServerURL)
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
	serverDomain := extractServerDomain(p.API, config.MatrixServerURL)
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
		content, ok := event["content"].(map[string]any)
		if !ok {
			continue
		}

		relatesTo, ok := content["m.relates_to"].(map[string]any)
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

// extractMattermostMentions extracts @mentions from Mattermost post content
func (p *Plugin) extractMattermostMentions(post *model.Post) *MattermostMentionResults {
	text := post.Message
	results := &MattermostMentionResults{}

	// Extract @username mentions (similar to Mattermost's regex)
	// Match @word where word contains letters, numbers, dots, hyphens, underscores
	userMentionRegex := regexp.MustCompile(`@([a-zA-Z0-9\.\-_]+)`)
	matches := userMentionRegex.FindAllStringSubmatch(text, -1)

	for _, match := range matches {
		if len(match) > 1 {
			username := match[1]
			switch username {
			case "here", "channel", "all":
				results.ChannelMentions = true
			default:
				results.UserMentions = append(results.UserMentions, username)
			}
		}
	}

	p.API.LogDebug("Extracted mentions from Mattermost post", "post_id", post.Id, "user_mentions", results.UserMentions, "channel_mentions", results.ChannelMentions)
	return results
}

// addMatrixMentions converts Mattermost mentions to Matrix format and adds them to content
func (p *Plugin) addMatrixMentions(content map[string]any, post *model.Post) {
	mentions := p.extractMattermostMentions(post)
	p.addMatrixMentionsWithData(content, post, mentions)
}

// addMatrixMentionsWithData converts Mattermost mentions to Matrix format using pre-extracted mention data
func (p *Plugin) addMatrixMentionsWithData(content map[string]any, post *model.Post, mentions *MattermostMentionResults) {
	// Only process if we have user mentions (ignore channel mentions for now)
	if len(mentions.UserMentions) == 0 {
		return
	}

	var matrixUserIDs []string
	var mentionReplacements []struct {
		username    string
		ghostUserID string
	}

	// First pass: collect all mention data
	for _, username := range mentions.UserMentions {
		// Look up Mattermost user by username
		user, appErr := p.API.GetUserByUsername(username)
		if appErr != nil {
			p.API.LogDebug("Failed to find Mattermost user for mention", "username", username, "error", appErr)
			continue
		}

		// Check if this user has a Matrix ghost user
		if ghostUserID, exists := p.getGhostUser(user.Id); exists {
			matrixUserIDs = append(matrixUserIDs, ghostUserID)
			mentionReplacements = append(mentionReplacements, struct {
				username    string
				ghostUserID string
			}{username, ghostUserID})

			p.API.LogDebug("Converted Mattermost mention to Matrix", "username", username, "ghost_user_id", ghostUserID)
		} else {
			p.API.LogDebug("No Matrix ghost user found for mention", "username", username, "user_id", user.Id)
		}
	}

	// Only proceed if we have Matrix users to mention
	if len(matrixUserIDs) == 0 {
		return
	}

	// Handle HTML content replacement
	updatedHTML, hasHTML := content["formatted_body"].(string)
	if !hasHTML {
		// If no HTML content, create it from plain text
		if plainText, hasPlain := content["body"].(string); hasPlain {
			updatedHTML = plainText
		} else {
			return // No content to process
		}
	}

	// Second pass: replace mentions in HTML content
	for _, replacement := range mentionReplacements {
		// Replace @username with Matrix mention link in HTML
		// Use more robust replacement that handles HTML escaping
		usernamePattern := fmt.Sprintf(`@%s\b`, regexp.QuoteMeta(replacement.username))
		usernameRegex := regexp.MustCompile(usernamePattern)
		matrixMentionLink := fmt.Sprintf(`<a href="https://matrix.to/#/%s">@%s</a>`, replacement.ghostUserID, replacement.username)
		updatedHTML = usernameRegex.ReplaceAllString(updatedHTML, matrixMentionLink)
	}

	// Add Matrix mentions structure
	content["m.mentions"] = map[string]any{
		"user_ids": matrixUserIDs,
	}
	content["formatted_body"] = updatedHTML
	content["format"] = "org.matrix.custom.html"

	p.API.LogDebug("Added Matrix mentions to message", "post_id", post.Id, "mentioned_users", len(matrixUserIDs), "matrix_user_ids", matrixUserIDs)
}

// isMatrixContentIdentical compares current Matrix event content with new content to detect if update is needed
func (p *Plugin) isMatrixContentIdentical(currentEvent map[string]any, newPlainText, newHTMLContent, matrixRoomID, eventID string, newFiles []matrix.FileAttachment) bool {
	// First check text content
	if !p.compareTextContent(currentEvent, newPlainText, newHTMLContent) {
		return false
	}

	// Then compare file attachments by checking related events
	if !p.areFileAttachmentsIdentical(matrixRoomID, eventID, newFiles) {
		p.API.LogDebug("Matrix message file attachments differ")
		return false
	}

	// Content and attachments are identical
	p.API.LogDebug("Matrix message content and attachments are identical, no update needed")
	return true
}

// compareTextContent compares text and HTML content between current and new message content
func (p *Plugin) compareTextContent(currentEvent map[string]any, newPlainText, newHTMLContent string) bool {
	// Extract current content from Matrix event
	content, ok := currentEvent["content"].(map[string]any)
	if !ok {
		p.API.LogDebug("Current Matrix event has no content field")
		return false
	}

	// Compare plain text body
	currentBody, hasBody := content["body"].(string)
	if !hasBody || currentBody != newPlainText {
		p.API.LogDebug("Matrix message body differs", "current", currentBody, "new", newPlainText)
		return false
	}

	// Compare HTML formatted content if present
	currentFormattedBody, hasFormatted := content["formatted_body"].(string)
	if newHTMLContent != "" {
		// New content has HTML, check if current content matches
		if !hasFormatted || currentFormattedBody != newHTMLContent {
			p.API.LogDebug("Matrix message formatted_body differs", "current", currentFormattedBody, "new", newHTMLContent)
			return false
		}
	} else {
		// New content has no HTML, current should also have no formatted content
		if hasFormatted && currentFormattedBody != "" {
			p.API.LogDebug("Matrix message formatted_body differs (new has none, current has some)", "current", currentFormattedBody)
			return false
		}
	}

	return true
}

// areFileAttachmentsIdentical compares current Matrix file attachments with new file attachments
func (p *Plugin) areFileAttachmentsIdentical(matrixRoomID, eventID string, newFiles []matrix.FileAttachment) bool {
	// Get current file attachments by looking at related events
	currentFiles, err := p.getCurrentMatrixFileAttachments(matrixRoomID, eventID)
	if err != nil {
		p.API.LogWarn("Failed to get current Matrix file attachments for comparison", "error", err, "event_id", eventID)
		// If we can't get current files, assume they're different to be safe
		return false
	}

	// Compare counts first
	if len(currentFiles) != len(newFiles) {
		p.API.LogDebug("File attachment count differs", "current_count", len(currentFiles), "new_count", len(newFiles))
		return false
	}

	// Compare each file attachment
	for i, newFile := range newFiles {
		if i >= len(currentFiles) {
			p.API.LogDebug("New file attachment not found in current attachments", "filename", newFile.Filename)
			return false
		}

		currentFile := currentFiles[i]
		if currentFile.Filename != newFile.Filename ||
			currentFile.MxcURI != newFile.MxcURI ||
			currentFile.MimeType != newFile.MimeType ||
			currentFile.Size != newFile.Size {
			p.API.LogDebug("File attachment differs", "current", currentFile, "new", newFile)
			return false
		}
	}

	return true
}

// getCurrentMatrixFileAttachments retrieves current file attachments for a Matrix event
func (p *Plugin) getCurrentMatrixFileAttachments(matrixRoomID, eventID string) ([]matrix.FileAttachment, error) {
	// Get related events (file attachments are sent as separate messages related to the main message)
	relations, err := p.matrixClient.GetEventRelationsAsUser(matrixRoomID, eventID, "")
	if err != nil {
		return nil, errors.Wrap(err, "failed to get event relations")
	}

	var files []matrix.FileAttachment
	for _, event := range relations {
		// Check if this is a file message related to our main message
		eventType, ok := event["type"].(string)
		if !ok || eventType != "m.room.message" {
			continue
		}

		content, ok := event["content"].(map[string]any)
		if !ok {
			continue
		}

		// Check if this is a file message
		msgType, ok := content["msgtype"].(string)
		if !ok {
			continue
		}

		// File messages have msgtype of m.file, m.image, m.video, or m.audio
		if msgType != "m.file" && msgType != "m.image" && msgType != "m.video" && msgType != "m.audio" {
			continue
		}

		// Check if this is related to our main message
		relatesTo, ok := content["m.relates_to"].(map[string]any)
		if !ok {
			continue
		}

		relType, ok := relatesTo["rel_type"].(string)
		relEventID, hasEventID := relatesTo["event_id"].(string)
		if !ok || relType != "m.mattermost.post" || !hasEventID || relEventID != eventID {
			continue
		}

		// Extract file information
		filename, hasFilename := content["body"].(string)
		mxcURI, hasMxcURI := content["url"].(string)

		if !hasFilename || !hasMxcURI {
			continue
		}

		// Extract file info
		var mimeType string
		var size int64
		if info, hasInfo := content["info"].(map[string]any); hasInfo {
			if mt, hasMT := info["mimetype"].(string); hasMT {
				mimeType = mt
			}
			if s, hasSize := info["size"]; hasSize {
				if sizeFloat, ok := s.(float64); ok {
					size = int64(sizeFloat)
				} else if sizeInt, ok := s.(int64); ok {
					size = sizeInt
				}
			}
		}

		files = append(files, matrix.FileAttachment{
			Filename: filename,
			MxcURI:   mxcURI,
			MimeType: mimeType,
			Size:     size,
		})
	}

	return files, nil
}
