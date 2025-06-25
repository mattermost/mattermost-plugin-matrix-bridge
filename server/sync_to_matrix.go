package main

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/pkg/errors"
	"github.com/wiggin77/mattermost-plugin-matrix-bridge/server/matrix"
)

// FileTracker interface for dependency injection
type FileTracker interface {
	GetFiles(postID string) []*PendingFile
	AddFile(postID string, file *PendingFile)
	RemoveFile(postID, fileID string) bool
}

// PostTrackerInterface provides dependency injection for post tracking operations
type PostTrackerInterface interface {
	Get(postID string) (int64, bool)
	Put(postID string, updateAt int64) error
	Delete(postID string)
}

// MattermostToMatrixBridge handles syncing FROM Mattermost TO Matrix
type MattermostToMatrixBridge struct {
	*BridgeUtils
	fileTracker FileTracker
	postTracker PostTrackerInterface
}

// NewMattermostToMatrixBridge creates a new MattermostToMatrixBridge instance
func NewMattermostToMatrixBridge(utils *BridgeUtils, fileTracker FileTracker, postTracker PostTrackerInterface) *MattermostToMatrixBridge {
	return &MattermostToMatrixBridge{
		BridgeUtils: utils,
		fileTracker: fileTracker,
		postTracker: postTracker,
	}
}

// MattermostToMatrix-specific utility methods

func (b *MattermostToMatrixBridge) getGhostUser(userID string) (string, bool) {
	ghostUserKey := "ghost_user_" + userID
	ghostUserIDBytes, err := b.kvstore.Get(ghostUserKey)
	if err == nil && len(ghostUserIDBytes) > 0 {
		return string(ghostUserIDBytes), true
	}
	return "", false
}

// CreateOrGetGhostUser creates a new Matrix ghost user for a Mattermost user, or returns existing one
func (b *MattermostToMatrixBridge) CreateOrGetGhostUser(userID string) (string, error) {
	// First check if ghost user already exists
	if ghostUserID, exists := b.getGhostUser(userID); exists {
		return ghostUserID, nil
	}

	// Ghost user doesn't exist, create a new one
	// Get the Mattermost user to fetch display name and avatar
	user, appErr := b.API.GetUser(userID)
	if appErr != nil {
		return "", errors.Wrap(appErr, "failed to get Mattermost user for ghost user creation")
	}

	// Get display name
	displayName := user.GetDisplayName(model.ShowFullName)

	// Get user's avatar image data
	var avatarData []byte
	var avatarContentType string
	if imageData, appErr := b.API.GetProfileImage(userID); appErr == nil {
		avatarData = imageData
		avatarContentType = "image/png" // Mattermost typically returns PNG
	}

	// Create new ghost user with display name and avatar
	ghostUser, err := b.matrixClient.CreateGhostUser(userID, displayName, avatarData, avatarContentType)
	if err != nil {
		// Check if this is a display name error (user was created but display name failed)
		if ghostUser != nil && ghostUser.UserID != "" {
			b.logger.LogWarn("Ghost user created but display name setting failed", "error", err, "ghost_user_id", ghostUser.UserID, "display_name", displayName)
			// Continue with caching - user creation was successful
		} else {
			return "", errors.Wrap(err, "failed to create ghost user")
		}
	}

	// Cache the ghost user ID
	ghostUserKey := "ghost_user_" + userID
	err = b.kvstore.Set(ghostUserKey, []byte(ghostUser.UserID))
	if err != nil {
		b.logger.LogWarn("Failed to cache ghost user ID", "error", err, "ghost_user_id", ghostUser.UserID)
		// Continue anyway, the ghost user was created successfully
	}

	if displayName != "" {
		b.logger.LogDebug("Created new ghost user with display name", "mattermost_user_id", userID, "ghost_user_id", ghostUser.UserID, "display_name", displayName)
	} else {
		b.logger.LogDebug("Created new ghost user", "mattermost_user_id", userID, "ghost_user_id", ghostUser.UserID)
	}
	return ghostUser.UserID, nil
}

func (b *MattermostToMatrixBridge) ensureGhostUserInRoom(ghostUserID, roomID, userID string) error {
	// Check if we've already confirmed this ghost user is in this room
	roomMembershipKey := "ghost_room_" + userID + "_" + roomID
	membershipBytes, err := b.kvstore.Get(roomMembershipKey)
	if err == nil && len(membershipBytes) > 0 && string(membershipBytes) == "joined" {
		// Already confirmed this user is in the room
		return nil
	}

	// Try to join the ghost user to the room
	err = b.matrixClient.JoinRoomAsUser(roomID, ghostUserID)
	if err != nil {
		return errors.Wrap(err, "failed to join ghost user to room")
	}

	// Cache the successful room join
	err = b.kvstore.Set(roomMembershipKey, []byte("joined"))
	if err != nil {
		b.logger.LogWarn("Failed to cache room membership", "error", err, "ghost_user_id", ghostUserID, "room_id", roomID)
		// Continue anyway, the join was successful
	}

	b.logger.LogDebug("Ghost user joined room successfully", "ghost_user_id", ghostUserID, "room_id", roomID)
	return nil
}

func (b *MattermostToMatrixBridge) convertEmojiForMatrix(emojiName string) string {
	// Remove colons if present
	cleanName := strings.Trim(emojiName, ":")

	// Get emoji index from name
	index, exists := emojiNameToIndex[cleanName]
	if !exists {
		b.logger.LogDebug("Unknown emoji name", "emoji", cleanName)
		return ":" + cleanName + ":"
	}

	// Get Unicode code point from index
	unicodeHex, exists := emojiIndexToUnicode[index]
	if !exists {
		b.logger.LogDebug("No Unicode mapping for emoji index", "emoji", cleanName, "index", index)
		return ":" + cleanName + ":"
	}

	// Convert hex string to Unicode character
	unicode := hexToUnicode(unicodeHex)
	if unicode == "" {
		b.logger.LogDebug("Failed to convert hex to Unicode", "emoji", cleanName, "hex", unicodeHex)
		return ":" + cleanName + ":"
	}

	return unicode
}

func (b *MattermostToMatrixBridge) deleteAllFileReplies(matrixRoomID, matrixEventID, ghostUserID string) error {
	// Find all file events that are replies to this main event
	fileEventIDs, err := b.getFileEventIDsFromMetadata(matrixRoomID, matrixEventID, ghostUserID)
	if err != nil {
		return errors.Wrap(err, "failed to find file events to delete")
	}

	// Delete each file event
	for _, fileEventID := range fileEventIDs {
		_, err := b.matrixClient.RedactEventAsGhost(matrixRoomID, fileEventID, ghostUserID)
		if err != nil {
			b.logger.LogWarn("Failed to delete file event", "error", err, "file_event_id", fileEventID, "main_event_id", matrixEventID)
			// Continue with other files
		} else {
			b.logger.LogDebug("Deleted file event", "file_event_id", fileEventID, "main_event_id", matrixEventID)
		}
	}

	return nil
}

func (b *MattermostToMatrixBridge) getFileEventIDsFromMetadata(matrixRoomID, postEventID, ghostUserID string) ([]string, error) {
	// Get related events for the main post event
	relatedEvents, err := b.matrixClient.GetEventRelationsAsUser(matrixRoomID, postEventID, ghostUserID)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get related events")
	}

	var fileEventIDs []string
	for _, event := range relatedEvents {
		// Check if this is a message event
		eventType, ok := event["type"].(string)
		if !ok || eventType != "m.room.message" {
			continue
		}

		// Check if this event is from our ghost user
		sender, ok := event["sender"].(string)
		if !ok || sender != ghostUserID {
			continue
		}

		// Check if this is a file message
		content, ok := event["content"].(map[string]any)
		if !ok {
			continue
		}

		msgType, ok := content["msgtype"].(string)
		if !ok {
			continue
		}

		// File messages have msgtype of m.file, m.image, m.video, or m.audio
		if msgType == "m.file" || msgType == "m.image" || msgType == "m.video" || msgType == "m.audio" {
			eventID, ok := event["event_id"].(string)
			if ok {
				fileEventIDs = append(fileEventIDs, eventID)
			}
		}
	}

	return fileEventIDs, nil
}

// MattermostMentionResults represents extracted mentions from a Mattermost post
type MattermostMentionResults struct {
	UserMentions    []string // usernames mentioned
	ChannelMentions bool     // @channel/@all/@here
}

// SyncUserToMatrix handles syncing user changes (like display name) to Matrix ghost users
func (b *MattermostToMatrixBridge) SyncUserToMatrix(user *model.User) error {
	b.logger.LogDebug("Syncing user to Matrix", "user_id", user.Id, "username", user.Username)

	// Check if we have a ghost user for this Mattermost user
	ghostUserID, exists := b.getGhostUser(user.Id)
	if !exists {
		b.logger.LogDebug("No ghost user found for user sync", "user_id", user.Id, "username", user.Username)
		return nil // No ghost user exists yet, nothing to update
	}

	b.logger.LogDebug("Found ghost user for user sync", "user_id", user.Id, "ghost_user_id", ghostUserID)

	// Update display name
	displayName := user.GetDisplayName(model.ShowFullName)
	if displayName != "" {
		err := b.matrixClient.SetDisplayName(ghostUserID, displayName)
		if err != nil {
			b.logger.LogError("Failed to update ghost user display name", "error", err, "user_id", user.Id, "ghost_user_id", ghostUserID, "display_name", displayName)
			return errors.Wrap(err, "failed to update ghost user display name on Matrix")
		}
		b.logger.LogDebug("Updated ghost user display name", "user_id", user.Id, "ghost_user_id", ghostUserID, "display_name", displayName)
	}

	return nil
}

// SyncPostToMatrix handles syncing a single post from Mattermost to Matrix
func (b *MattermostToMatrixBridge) SyncPostToMatrix(post *model.Post, channelID string) error {
	// Check if this is a post deletion
	if post.DeleteAt != 0 {
		return b.deletePostFromMatrix(post, channelID)
	}

	matrixRoomIdentifier, err := b.getMatrixRoomID(channelID)
	if err != nil {
		return errors.Wrap(err, "failed to get Matrix room identifier")
	}

	if matrixRoomIdentifier == "" {
		b.API.LogWarn("No Matrix room mapped for channel", "channel_id", channelID)
		return nil
	}

	// Resolve room alias to room ID if needed
	matrixRoomID, err := b.matrixClient.ResolveRoomAlias(matrixRoomIdentifier)
	if err != nil {
		return errors.Wrap(err, "failed to resolve Matrix room identifier")
	}

	user, appErr := b.API.GetUser(post.UserId)
	if appErr != nil {
		return errors.Wrap(appErr, "failed to get user")
	}

	// Check if this post already has a Matrix event ID (indicating it's an edit)
	config := b.getConfiguration()
	serverDomain := extractServerDomain(b.logger, config.MatrixServerURL)
	propertyKey := "matrix_event_id_" + serverDomain

	var existingEventID string
	if post.Props != nil {
		if eventID, ok := post.Props[propertyKey].(string); ok {
			existingEventID = eventID
		}
	}

	if existingEventID != "" {
		// Check if this is a redundant edit from adding the Matrix event ID property
		if storedUpdateAt, exists := b.postTracker.Get(post.Id); exists {
			if post.UpdateAt == storedUpdateAt {
				// This post's UpdateAt matches the timestamp we stored when adding Matrix event ID
				// This is the redundant edit from adding the Matrix event ID property
				b.postTracker.Delete(post.Id)
				b.API.LogDebug("Skipping redundant edit after post creation", "post_id", post.Id, "matrix_event_id", existingEventID, "stored_update_at", storedUpdateAt, "current_update_at", post.UpdateAt)
				return nil
			}
			// This is a genuine edit that happened after we added the Matrix event ID
			// Remove the tracking entry since we're processing a real edit now
			b.postTracker.Delete(post.Id)
			b.API.LogDebug("Processing genuine edit after post creation", "post_id", post.Id, "matrix_event_id", existingEventID, "stored_update_at", storedUpdateAt, "current_update_at", post.UpdateAt)
		}

		// This is a genuine post edit - update the existing Matrix message
		err = b.updatePostInMatrix(post, matrixRoomID, existingEventID, user)
		if err != nil {
			return errors.Wrap(err, "failed to update post in Matrix")
		}
		b.API.LogDebug("Successfully updated post in Matrix", "post_id", post.Id, "matrix_event_id", existingEventID)
	} else {
		// This is a new post - create new Matrix message
		err = b.createPostInMatrix(post, matrixRoomID, user, propertyKey)
		if err != nil {
			return errors.Wrap(err, "failed to create post in Matrix")
		}
		b.API.LogDebug("Successfully created new post in Matrix", "post_id", post.Id)
	}

	return nil
}

// createPostInMatrix creates a new post in Matrix and stores the event ID
func (b *MattermostToMatrixBridge) createPostInMatrix(post *model.Post, matrixRoomID string, user *model.User, propertyKey string) error {
	// Create or get ghost user
	ghostUserID, err := b.CreateOrGetGhostUser(user.Id)
	if err != nil {
		return errors.Wrap(err, "failed to create or get ghost user")
	}

	// Ensure ghost user is joined to the room
	err = b.ensureGhostUserInRoom(ghostUserID, matrixRoomID, user.Id)
	if err != nil {
		return errors.Wrap(err, "failed to ensure ghost user is in room")
	}

	// Process mentions first on the original text
	mentionData := b.extractMattermostMentions(post)

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
	b.addMatrixMentionsWithData(messageContent, post, mentionData)

	// Check if this is a threaded post (reply to another post)
	var threadEventID string
	if post.RootId != "" {
		// This is a reply - find the Matrix event ID of the root post
		rootPost, appErr := b.API.GetPost(post.RootId)
		if appErr != nil {
			b.API.LogWarn("Failed to get root post for thread", "error", appErr, "post_id", post.Id, "root_id", post.RootId)
			// Continue without threading - send as regular message
		} else {
			// Get Matrix event ID from root post properties
			if rootPost.Props != nil {
				if eventID, ok := rootPost.Props[propertyKey].(string); ok {
					threadEventID = eventID
				}
			}
			if threadEventID == "" {
				b.API.LogWarn("Root post has no Matrix event ID for threading", "post_id", post.Id, "root_id", post.RootId)
				// Continue without threading - send as regular message
			}
		}
	}

	// Check for pending file attachments for this post
	pendingFiles := b.fileTracker.GetFiles(post.Id)

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
	finalMentions, _ := messageContent["m.mentions"].(map[string]any)

	// Send message using consolidated method
	messageRequest := matrix.MessageRequest{
		RoomID:        matrixRoomID,
		GhostUserID:   ghostUserID,
		Message:       finalPlainText,
		HTMLMessage:   finalHTMLContent,
		ThreadEventID: threadEventID,
		PostID:        post.Id,
		Files:         fileAttachments,
		Mentions:      finalMentions,
	}

	sendResponse, err := b.matrixClient.SendMessage(messageRequest)
	if err != nil {
		return errors.Wrap(err, "failed to send message as ghost user")
	}

	if len(pendingFiles) > 0 {
		b.API.LogDebug("Posted message with file attachments to Matrix", "post_id", post.Id, "file_count", len(pendingFiles))
	}

	// Store the Matrix event ID as a post property for reaction mapping
	if sendResponse != nil && sendResponse.EventID != "" {
		if post.Props == nil {
			post.Props = make(map[string]any)
		}
		post.Props[propertyKey] = sendResponse.EventID

		updatedPost, appErr := b.API.UpdatePost(post)
		if appErr != nil {
			b.API.LogWarn("Failed to update post with Matrix event ID", "error", appErr, "post_id", post.Id, "event_id", sendResponse.EventID)
			// Continue anyway, the message was sent successfully
		} else {
			// Store the UpdateAt timestamp in memory to detect redundant edits
			err = b.postTracker.Put(post.Id, updatedPost.UpdateAt)
			if err != nil {
				b.API.LogWarn("Failed to store post tracking for redundant edit detection", "error", err, "post_id", post.Id, "update_at", updatedPost.UpdateAt)
				// Continue anyway - this is just an optimization to avoid redundant edits
			} else {
				b.API.LogDebug("Stored post tracking for redundant edit detection", "post_id", post.Id, "update_at", updatedPost.UpdateAt)
			}
		}
	}

	b.API.LogDebug("Successfully created post in Matrix", "post_id", post.Id, "ghost_user_id", ghostUserID, "event_id", sendResponse.EventID)
	return nil
}

// updatePostInMatrix updates an existing post in Matrix
func (b *MattermostToMatrixBridge) updatePostInMatrix(post *model.Post, matrixRoomID string, eventID string, user *model.User) error {
	// Create or get ghost user
	ghostUserID, err := b.CreateOrGetGhostUser(user.Id)
	if err != nil {
		return errors.Wrap(err, "failed to create or get ghost user")
	}

	// Ensure ghost user is still in the room
	err = b.ensureGhostUserInRoom(ghostUserID, matrixRoomID, user.Id)
	if err != nil {
		return errors.Wrap(err, "failed to ensure ghost user is in room")
	}

	// Process mentions first on the original text
	mentionData := b.extractMattermostMentions(post)

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
	b.addMatrixMentionsWithData(messageContent, post, mentionData)

	// Get file attachments for this post (for comparison purposes)
	var currentFiles []matrix.FileAttachment

	// First check pending files (for new posts that haven't been sent yet)
	pendingFiles := b.fileTracker.GetFiles(post.Id)
	for _, file := range pendingFiles {
		currentFiles = append(currentFiles, matrix.FileAttachment{
			Filename: file.Filename,
			MxcURI:   file.MxcURI,
			MimeType: file.MimeType,
			Size:     file.Size,
		})
	}

	// If no pending files, get file attachments from the post itself (for existing posts)
	if len(currentFiles) == 0 && len(post.FileIds) > 0 {
		for _, fileID := range post.FileIds {
			fileInfo, appErr := b.API.GetFileInfo(fileID)
			if appErr != nil {
				b.API.LogWarn("Failed to get file info for comparison", "error", appErr, "file_id", fileID, "post_id", post.Id)
				continue
			}
			// We don't have the MXC URI for existing files, but we have the filename which is what we need for comparison
			currentFiles = append(currentFiles, matrix.FileAttachment{
				Filename: fileInfo.Name,
				MxcURI:   "", // Not available for existing files, but not needed for text comparison
				MimeType: fileInfo.MimeType,
				Size:     fileInfo.Size,
			})
		}
	}

	// Extract content from message structure
	finalPlainText := messageContent["body"].(string)
	finalHTMLContent, _ := messageContent["formatted_body"].(string)

	b.API.LogDebug("Preparing to compare Matrix content", "post_id", post.Id, "new_plain_text", finalPlainText, "new_html_content", finalHTMLContent, "file_count", len(currentFiles))
	if len(currentFiles) > 0 {
		b.API.LogDebug("Files for comparison", "post_id", post.Id, "filenames", func() []string {
			var names []string
			for _, f := range currentFiles {
				names = append(names, f.Filename)
			}
			return names
		}())
	}

	// Fetch the current Matrix event content to compare
	currentEvent, err := b.matrixClient.GetEvent(matrixRoomID, eventID)
	if err != nil {
		b.API.LogWarn("Failed to fetch current Matrix event for comparison", "error", err, "event_id", eventID)
		// Continue with update if we can't fetch current content
	} else {
		// Compare content and file attachments to see if anything actually changed
		if b.isMatrixContentIdentical(currentEvent, finalPlainText, finalHTMLContent, matrixRoomID, eventID, currentFiles) {
			b.API.LogDebug("Matrix message content and attachments unchanged, skipping edit", "post_id", post.Id, "matrix_event_id", eventID)
			return nil
		}
	}

	// Send edit as ghost user with proper HTML formatting support
	_, err = b.matrixClient.EditMessageAsGhost(matrixRoomID, eventID, finalPlainText, finalHTMLContent, ghostUserID)
	if err != nil {
		return errors.Wrap(err, "failed to edit message as ghost user")
	}

	b.API.LogDebug("Successfully updated post in Matrix", "post_id", post.Id, "ghost_user_id", ghostUserID, "matrix_event_id", eventID)
	return nil
}

// deletePostFromMatrix handles deleting a post from Matrix by redacting the Matrix message
func (b *MattermostToMatrixBridge) deletePostFromMatrix(post *model.Post, channelID string) error {
	// Get Matrix room identifier
	matrixRoomIdentifier, err := b.getMatrixRoomID(channelID)
	if err != nil {
		return errors.Wrap(err, "failed to get Matrix room identifier for post deletion")
	}

	if matrixRoomIdentifier == "" {
		b.API.LogWarn("No Matrix room mapped for channel", "channel_id", channelID)
		return nil
	}

	// Resolve room alias to room ID if needed
	matrixRoomID, err := b.matrixClient.ResolveRoomAlias(matrixRoomIdentifier)
	if err != nil {
		return errors.Wrap(err, "failed to resolve Matrix room identifier for post deletion")
	}

	// Get Matrix event ID from post properties
	config := b.getConfiguration()
	serverDomain := extractServerDomain(b.logger, config.MatrixServerURL)
	propertyKey := "matrix_event_id_" + serverDomain

	var matrixEventID string
	if post.Props != nil {
		if eventID, ok := post.Props[propertyKey].(string); ok {
			matrixEventID = eventID
		}
	}

	if matrixEventID == "" {
		b.API.LogWarn("No Matrix event ID found for post deletion", "post_id", post.Id, "property_key", propertyKey)
		return nil // Can't delete a message that wasn't synced to Matrix
	}

	// Get user for ghost user lookup
	user, appErr := b.API.GetUser(post.UserId)
	if appErr != nil {
		return errors.Wrap(appErr, "failed to get user for post deletion")
	}

	// Check if ghost user exists (needed for redaction)
	ghostUserID, exists := b.getGhostUser(user.Id)
	if !exists {
		b.API.LogWarn("No ghost user found for post deletion", "user_id", post.UserId, "post_id", post.Id)
		return nil // Can't delete a message from a user that doesn't have a ghost user
	}

	// First, find and delete any file attachment replies to this message
	err = b.deleteAllFileReplies(matrixRoomID, matrixEventID, ghostUserID)
	if err != nil {
		b.API.LogWarn("Failed to delete file attachment replies", "error", err, "post_id", post.Id, "matrix_event_id", matrixEventID)
		// Continue anyway - we'll still delete the main message
	}

	// Redact the main message event
	_, err = b.matrixClient.RedactEventAsGhost(matrixRoomID, matrixEventID, ghostUserID)
	if err != nil {
		return errors.Wrap(err, "failed to redact post in Matrix")
	}

	b.API.LogDebug("Successfully deleted post and file attachments from Matrix", "post_id", post.Id, "ghost_user_id", ghostUserID, "matrix_event_id", matrixEventID)
	return nil
}

// SyncReactionToMatrix handles syncing a reaction from Mattermost to Matrix
func (b *MattermostToMatrixBridge) SyncReactionToMatrix(reaction *model.Reaction, channelID string) error {
	// Check if this is a reaction deletion
	if reaction.DeleteAt != 0 {
		return b.removeReactionFromMatrix(reaction, channelID)
	}

	// This is a new reaction - add it to Matrix
	return b.addReactionToMatrix(reaction, channelID)
}

// addReactionToMatrix adds a new reaction to Matrix
func (b *MattermostToMatrixBridge) addReactionToMatrix(reaction *model.Reaction, channelID string) error {
	// Get Matrix room identifier
	matrixRoomIdentifier, err := b.getMatrixRoomID(channelID)
	if err != nil {
		return errors.Wrap(err, "failed to get Matrix room identifier")
	}

	if matrixRoomIdentifier == "" {
		b.API.LogWarn("No Matrix room mapped for channel", "channel_id", channelID)
		return nil
	}

	// Resolve room alias to room ID if needed
	matrixRoomID, err := b.matrixClient.ResolveRoomAlias(matrixRoomIdentifier)
	if err != nil {
		return errors.Wrap(err, "failed to resolve Matrix room identifier")
	}

	// Get the post to find the Matrix event ID
	post, appErr := b.API.GetPost(reaction.PostId)
	if appErr != nil {
		return errors.Wrap(appErr, "failed to get post for reaction")
	}

	// Get Matrix event ID from post properties
	config := b.getConfiguration()
	serverDomain := extractServerDomain(b.logger, config.MatrixServerURL)
	propertyKey := "matrix_event_id_" + serverDomain

	var matrixEventID string
	if post.Props != nil {
		if eventID, ok := post.Props[propertyKey].(string); ok {
			matrixEventID = eventID
		}
	}

	if matrixEventID == "" {
		b.API.LogWarn("No Matrix event ID found for post", "post_id", reaction.PostId, "property_key", propertyKey)
		return nil // Can't react to a message that wasn't synced to Matrix
	}

	// Get user for ghost user creation
	user, appErr := b.API.GetUser(reaction.UserId)
	if appErr != nil {
		return errors.Wrap(appErr, "failed to get user for reaction")
	}

	// Create or get ghost user
	ghostUserID, err := b.CreateOrGetGhostUser(user.Id)
	if err != nil {
		return errors.Wrap(err, "failed to create or get ghost user for reaction")
	}

	// Ensure ghost user is in the room
	err = b.ensureGhostUserInRoom(ghostUserID, matrixRoomID, user.Id)
	if err != nil {
		return errors.Wrap(err, "failed to ensure ghost user is in room for reaction")
	}

	// Convert Mattermost emoji name to Matrix reaction format
	emoji := b.convertEmojiForMatrix(reaction.EmojiName)

	// Send reaction as ghost user
	_, err = b.matrixClient.SendReactionAsGhost(matrixRoomID, matrixEventID, emoji, ghostUserID)
	if err != nil {
		return errors.Wrap(err, "failed to send reaction as ghost user")
	}

	b.API.LogDebug("Successfully synced reaction as ghost user", "post_id", reaction.PostId, "emoji", reaction.EmojiName, "ghost_user_id", ghostUserID, "matrix_event_id", matrixEventID)
	return nil
}

// removeReactionFromMatrix removes a reaction from Matrix by finding and redacting the matching reaction event
func (b *MattermostToMatrixBridge) removeReactionFromMatrix(reaction *model.Reaction, channelID string) error {
	// Get Matrix room identifier
	matrixRoomIdentifier, err := b.getMatrixRoomID(channelID)
	if err != nil {
		return errors.Wrap(err, "failed to get Matrix room identifier for reaction removal")
	}

	if matrixRoomIdentifier == "" {
		b.API.LogWarn("No Matrix room mapped for channel", "channel_id", channelID)
		return nil
	}

	// Resolve room alias to room ID if needed
	matrixRoomID, err := b.matrixClient.ResolveRoomAlias(matrixRoomIdentifier)
	if err != nil {
		return errors.Wrap(err, "failed to resolve Matrix room identifier for reaction removal")
	}

	// Get the post to find the Matrix event ID
	post, appErr := b.API.GetPost(reaction.PostId)
	if appErr != nil {
		return errors.Wrap(appErr, "failed to get post for reaction removal")
	}

	// Get Matrix event ID from post properties
	config := b.getConfiguration()
	serverDomain := extractServerDomain(b.logger, config.MatrixServerURL)
	propertyKey := "matrix_event_id_" + serverDomain

	var matrixEventID string
	if post.Props != nil {
		if eventID, ok := post.Props[propertyKey].(string); ok {
			matrixEventID = eventID
		}
	}

	if matrixEventID == "" {
		b.API.LogWarn("No Matrix event ID found for post", "post_id", reaction.PostId, "property_key", propertyKey)
		return nil // Can't remove reaction from a message that wasn't synced to Matrix
	}

	// Get user for ghost user creation
	user, appErr := b.API.GetUser(reaction.UserId)
	if appErr != nil {
		return errors.Wrap(appErr, "failed to get user for reaction removal")
	}

	// Check if ghost user exists (needed for determining which reaction to remove)
	ghostUserID, exists := b.getGhostUser(user.Id)
	if !exists {
		b.API.LogWarn("No ghost user found for reaction removal", "user_id", reaction.UserId, "post_id", reaction.PostId)
		return nil // Can't remove a reaction from a user that doesn't have a ghost user
	}

	// Convert Mattermost emoji name to Matrix reaction format for matching
	emoji := b.convertEmojiForMatrix(reaction.EmojiName)

	// Get all reactions for this message from Matrix (as the ghost user)
	relations, err := b.matrixClient.GetEventRelationsAsUser(matrixRoomID, matrixEventID, ghostUserID)
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
		b.API.LogWarn("No matching reaction found in Matrix to remove", "post_id", reaction.PostId, "emoji", reaction.EmojiName, "ghost_user_id", ghostUserID)
		return nil // No matching reaction found to remove
	}

	// Redact the reaction event
	_, err = b.matrixClient.RedactEventAsGhost(matrixRoomID, reactionEventID, ghostUserID)
	if err != nil {
		return errors.Wrap(err, "failed to redact reaction in Matrix")
	}

	b.API.LogDebug("Successfully removed reaction from Matrix", "post_id", reaction.PostId, "emoji", reaction.EmojiName, "ghost_user_id", ghostUserID, "reaction_event_id", reactionEventID)
	return nil
}

// extractMattermostMentions extracts @mentions from Mattermost post content
func (b *MattermostToMatrixBridge) extractMattermostMentions(post *model.Post) *MattermostMentionResults {
	text := post.Message
	results := &MattermostMentionResults{}

	// Extract @username mentions (similar to Mattermost's regex)
	// Match @word where word contains letters, numbers, dots, hyphens, underscores, and colons
	// Include colon to support bridged usernames like "matrix:username"
	// Use word boundary \b to avoid matching @username in email@username.com
	userMentionRegex := regexp.MustCompile(`\B@([a-zA-Z0-9\.\-_:]+)\b`)
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

	b.API.LogDebug("Extracted mentions from Mattermost post", "post_id", post.Id, "message", text, "user_mentions", results.UserMentions, "channel_mentions", results.ChannelMentions)
	return results
}

// addMatrixMentionsWithData converts Mattermost mentions to Matrix format using pre-extracted mention data
func (b *MattermostToMatrixBridge) addMatrixMentionsWithData(content map[string]any, post *model.Post, mentions *MattermostMentionResults) {
	b.API.LogDebug("Processing mentions for Matrix", "post_id", post.Id, "user_mentions_count", len(mentions.UserMentions), "user_mentions", mentions.UserMentions)

	// Only process if we have user mentions (ignore channel mentions for now)
	if len(mentions.UserMentions) == 0 {
		b.API.LogDebug("No user mentions found, skipping mention processing", "post_id", post.Id)
		return
	}

	var matrixUserIDs []string
	var mentionReplacements []struct {
		username    string
		ghostUserID string
		displayName string
	}

	// First pass: collect all mention data
	for _, username := range mentions.UserMentions {
		// Look up Mattermost user by username
		user, appErr := b.API.GetUserByUsername(username)
		if appErr != nil {
			b.API.LogDebug("Failed to find Mattermost user for mention", "username", username, "error", appErr)
			continue
		}

		var matrixUserID string
		var displayName string

		// Check if this user has a Matrix ghost user (Mattermost user → Matrix ghost)
		if ghostUserID, exists := b.getGhostUser(user.Id); exists {
			matrixUserID = ghostUserID
			displayName = user.GetDisplayName(model.ShowFullName)
			if displayName == "" {
				displayName = user.Username // Fallback to username
			}
			b.API.LogDebug("Found existing Matrix ghost user for Mattermost user mention", "username", username, "ghost_user_id", ghostUserID, "display_name", displayName)
		} else {
			// Check if this is a Matrix user represented in Mattermost (Matrix user → Mattermost user)
			originalMatrixUserID := b.getOriginalMatrixUserID(user.Id)
			if originalMatrixUserID != "" {
				matrixUserID = originalMatrixUserID
				displayName = user.GetDisplayName(model.ShowFullName)
				if displayName == "" {
					displayName = user.Username // Fallback to username
				}
				b.API.LogDebug("Found original Matrix user for bridged user mention", "username", username, "original_matrix_user_id", originalMatrixUserID, "display_name", displayName)
			} else {
				// No existing ghost user or original Matrix user found - create new ghost user for mention
				b.API.LogDebug("Creating new ghost user for mentioned user", "username", username, "user_id", user.Id)
				ghostUserID, err := b.CreateOrGetGhostUser(user.Id)
				if err != nil {
					b.API.LogWarn("Failed to create ghost user for mentioned user", "username", username, "user_id", user.Id, "error", err)
					continue
				}
				matrixUserID = ghostUserID
				displayName = user.GetDisplayName(model.ShowFullName)
				if displayName == "" {
					displayName = user.Username // Fallback to username
				}
				b.API.LogDebug("Created new Matrix ghost user for mention", "username", username, "ghost_user_id", ghostUserID, "display_name", displayName)
			}
		}

		matrixUserIDs = append(matrixUserIDs, matrixUserID)
		mentionReplacements = append(mentionReplacements, struct {
			username    string
			ghostUserID string
			displayName string
		}{username, matrixUserID, displayName})
	}

	// Only proceed if we have Matrix users to mention
	if len(matrixUserIDs) == 0 {
		b.API.LogDebug("No Matrix ghost users found for any mentions, skipping mention processing", "post_id", post.Id, "attempted_usernames", mentions.UserMentions)
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
		// Replace @username with proper Matrix mention pill format
		// Use regex with word boundaries to avoid matching substrings (e.g., @alice in email@alice.com)
		usernamePattern := fmt.Sprintf(`\B@%s\b`, regexp.QuoteMeta(replacement.username))
		usernameRegex := regexp.MustCompile(usernamePattern)

		// Create Matrix mention pill format to match native Matrix mentions
		matrixMentionPill := fmt.Sprintf(`<a href="https://matrix.to/#/%s">@%s</a>`,
			replacement.ghostUserID, replacement.displayName)
		updatedHTML = usernameRegex.ReplaceAllString(updatedHTML, matrixMentionPill)
	}

	// Add Matrix mentions structure
	mentionsField := map[string]any{
		"user_ids": matrixUserIDs,
	}
	content["m.mentions"] = mentionsField
	content["formatted_body"] = updatedHTML
	content["format"] = "org.matrix.custom.html"

	b.API.LogDebug("Added Matrix mentions to message", "post_id", post.Id, "mentioned_users", len(matrixUserIDs), "matrix_user_ids", matrixUserIDs, "m_mentions", mentionsField)
}

// getOriginalMatrixUserID looks up the original Matrix user ID for a Mattermost user created from Matrix
func (b *MattermostToMatrixBridge) getOriginalMatrixUserID(mattermostUserID string) string {
	// Search through all matrix_user_* mappings to find one that points to this Mattermost user
	keys, err := b.kvstore.ListKeys(0, 1000)
	if err != nil {
		b.API.LogWarn("Failed to list kvstore keys for Matrix user lookup", "error", err, "mattermost_user_id", mattermostUserID)
		return ""
	}

	matrixUserPrefix := "matrix_user_"
	for _, key := range keys {
		if strings.HasPrefix(key, matrixUserPrefix) {
			userIDBytes, err := b.kvstore.Get(key)
			if err != nil {
				continue
			}

			if string(userIDBytes) == mattermostUserID {
				// Found the mapping - extract Matrix user ID from the key
				matrixUserID := strings.TrimPrefix(key, matrixUserPrefix)
				b.API.LogDebug("Found original Matrix user ID", "mattermost_user_id", mattermostUserID, "matrix_user_id", matrixUserID)
				return matrixUserID
			}
		}
	}

	return ""
}

// isMatrixContentIdentical compares current Matrix event content with new content to detect if update is needed
func (b *MattermostToMatrixBridge) isMatrixContentIdentical(currentEvent map[string]any, newPlainText, newHTMLContent, matrixRoomID, eventID string, newFiles []matrix.FileAttachment) bool {
	// First check text content
	if !b.compareTextContent(currentEvent, newPlainText, newHTMLContent, newFiles) {
		return false
	}

	// For file-only posts where text content matches (empty new text + filename match),
	// skip file attachment comparison since the text comparison already handled the equivalence
	if newPlainText == "" && len(newFiles) > 0 {
		// Extract current content to check if this is a file-only post
		if content, ok := currentEvent["content"].(map[string]any); ok {
			if currentBody, hasBody := content["body"].(string); hasBody && currentBody != "" {
				// Check if current body matches any filename (file-only post scenario)
				for _, file := range newFiles {
					if currentBody == file.Filename {
						b.API.LogDebug("File-only post detected with matching text content, skipping file attachment comparison", "current_body", currentBody, "filename", file.Filename)
						// Text comparison already verified equivalence, so content is identical
						b.API.LogDebug("Matrix message content and attachments are identical, no update needed")
						return true
					}
				}
			}
		}
	}

	// For non-file-only posts, compare file attachments by checking related events
	if !b.areFileAttachmentsIdentical(matrixRoomID, eventID, newFiles) {
		b.API.LogDebug("Matrix message file attachments differ")
		return false
	}

	// Content and attachments are identical
	b.API.LogDebug("Matrix message content and attachments are identical, no update needed")
	return true
}

// compareTextContent compares text and HTML content between current and new message content
func (b *MattermostToMatrixBridge) compareTextContent(currentEvent map[string]any, newPlainText, newHTMLContent string, newFiles []matrix.FileAttachment) bool {
	// Extract current content from Matrix event
	content, ok := currentEvent["content"].(map[string]any)
	if !ok {
		b.API.LogDebug("Current Matrix event has no content field")
		return false
	}

	// Compare plain text body
	currentBody, hasBody := content["body"].(string)
	if !hasBody || currentBody != newPlainText {
		// Special case: file-only posts
		// If new content is empty and we have files, check if current content is just a filename
		if newPlainText == "" && len(newFiles) > 0 && hasBody {
			// Check if current body matches any of the new filenames (file-only post scenario)
			filenameMatch := false
			for _, file := range newFiles {
				if currentBody == file.Filename {
					b.API.LogDebug("File-only post: current Matrix body matches filename, treating as identical", "current", currentBody, "filename", file.Filename)
					filenameMatch = true
					break
				}
			}
			// If current body doesn't match any filename, content differs
			if !filenameMatch {
				b.API.LogDebug("Matrix message body differs (not a filename match)", "current", currentBody, "new", newPlainText)
				return false
			}
			// If we found a filename match, continue to check HTML content below
		} else {
			b.API.LogDebug("Matrix message body differs", "current", currentBody, "new", newPlainText)
			return false
		}
	}

	// Compare HTML formatted content if present
	currentFormattedBody, hasFormatted := content["formatted_body"].(string)
	if newHTMLContent != "" {
		// New content has HTML, check if current content matches
		if !hasFormatted || currentFormattedBody != newHTMLContent {
			b.API.LogDebug("Matrix message formatted_body differs", "current", currentFormattedBody, "new", newHTMLContent)
			return false
		}
	} else {
		// New content has no HTML, current should also have no formatted content
		if hasFormatted && currentFormattedBody != "" {
			b.API.LogDebug("Matrix message formatted_body differs (new has none, current has some)", "current", currentFormattedBody)
			return false
		}
	}

	return true
}

// areFileAttachmentsIdentical compares current Matrix file attachments with new file attachments
func (b *MattermostToMatrixBridge) areFileAttachmentsIdentical(matrixRoomID, eventID string, newFiles []matrix.FileAttachment) bool {
	// Get current file attachments by looking at related events
	currentFiles, err := b.getCurrentMatrixFileAttachments(matrixRoomID, eventID)
	if err != nil {
		b.API.LogWarn("Failed to get current Matrix file attachments for comparison", "error", err, "event_id", eventID)
		// If we can't get current files, assume they're different to be safe
		return false
	}

	// Compare counts first
	if len(currentFiles) != len(newFiles) {
		b.API.LogDebug("File attachment count differs", "current_count", len(currentFiles), "new_count", len(newFiles))
		return false
	}

	// Compare each file attachment
	for i, newFile := range newFiles {
		if i >= len(currentFiles) {
			b.API.LogDebug("New file attachment not found in current attachments", "filename", newFile.Filename)
			return false
		}

		currentFile := currentFiles[i]
		if currentFile.Filename != newFile.Filename ||
			currentFile.MxcURI != newFile.MxcURI ||
			currentFile.MimeType != newFile.MimeType ||
			currentFile.Size != newFile.Size {
			b.API.LogDebug("File attachment differs", "current", currentFile, "new", newFile)
			return false
		}
	}

	return true
}

// getCurrentMatrixFileAttachments retrieves current file attachments for a Matrix event
func (b *MattermostToMatrixBridge) getCurrentMatrixFileAttachments(matrixRoomID, eventID string) ([]matrix.FileAttachment, error) {
	// Get related events (file attachments are sent as separate messages related to the main message)
	relations, err := b.matrixClient.GetEventRelationsAsUser(matrixRoomID, eventID, "")
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
