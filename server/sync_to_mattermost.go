package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/matrix"
	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/pkg/errors"
)

// MatrixToMattermostBridge handles syncing FROM Matrix TO Mattermost
type MatrixToMattermostBridge struct {
	*BridgeUtils
}

// NewMatrixToMattermostBridge creates a new MatrixToMattermostBridge instance
func NewMatrixToMattermostBridge(utils *BridgeUtils) *MatrixToMattermostBridge {
	return &MatrixToMattermostBridge{
		BridgeUtils: utils,
	}
}

// syncMatrixMessageToMattermost handles syncing Matrix messages to Mattermost posts
func (b *MatrixToMattermostBridge) syncMatrixMessageToMattermost(event MatrixEvent, channelID string) error {
	b.logger.LogDebug("Syncing Matrix message to Mattermost", "event_id", event.EventID, "sender", event.Sender, "channel_id", channelID)

	// Extract Mattermost metadata if present
	mattermostPostID, mattermostRemoteID := b.extractMattermostMetadata(event)
	if mattermostPostID != "" || mattermostRemoteID != "" {
		b.logger.LogDebug("Found Mattermost metadata in Matrix event", "event_id", event.EventID, "mattermost_post_id", mattermostPostID, "mattermost_remote_id", mattermostRemoteID)
	}

	// Check if this Matrix event originated from Mattermost to prevent loops
	if mattermostPostID != "" {
		// This event contains a Mattermost post ID - check if we already have this post
		if existingPost, appErr := b.API.GetPost(mattermostPostID); appErr == nil && existingPost != nil {
			b.logger.LogDebug("Skipping Matrix message that originated from Mattermost", "event_id", event.EventID, "mattermost_post_id", mattermostPostID, "existing_post_id", existingPost.Id)
			return nil // Skip processing - this is our own post echoed back from Matrix
		}
		// If GetPost fails, the post might have been deleted, so continue processing
	}

	// Also check remote ID for additional loop prevention
	if mattermostRemoteID != "" && mattermostRemoteID == b.remoteID {
		b.logger.LogDebug("Skipping Matrix message from our own remote ID", "event_id", event.EventID, "remote_id", mattermostRemoteID)
		return nil // Skip processing - this originated from our bridge
	}

	// Check if this is a message edit (has m.relates_to with rel_type: m.replace)
	if relatesTo, exists := event.Content["m.relates_to"].(map[string]any); exists {
		if relType, exists := relatesTo["rel_type"].(string); exists && relType == "m.replace" {
			// This is an edit - handle separately (allow empty content for deletions)
			return b.handleMatrixMessageEdit(event, channelID)
		}
	}

	// Check if this is a file/image attachment
	if msgType, exists := event.Content["msgtype"].(string); exists {
		switch msgType {
		case "m.image", "m.file", "m.video", "m.audio":
			return b.syncMatrixFileToMattermost(event, channelID)
		}
	}

	// Extract message content (smart format detection: prefer formatted text, fallback to plain text)
	content := b.extractMatrixMessageContent(event)

	// For new messages (not edits), skip if content is empty
	if content == "" {
		b.logger.LogDebug("Matrix message has no body content, skipping new message", "event_id", event.EventID, "content", event.Content)
		return nil // Empty new messages don't need to be synced
	}

	// Get or create Mattermost user for the Matrix sender
	mattermostUserID, err := b.getOrCreateMattermostUser(event.Sender, channelID)
	if err != nil {
		return errors.Wrap(err, "failed to get or create Mattermost user")
	}

	// Convert Matrix content to Mattermost format
	mattermostContent := b.convertMatrixToMattermost(content)

	// Check if this is a threaded message (reply)
	var rootID string
	if relatesTo, exists := event.Content["m.relates_to"].(map[string]any); exists {
		// Check for Matrix reply structure (m.in_reply_to)
		if inReplyTo, hasInReplyTo := relatesTo["m.in_reply_to"].(map[string]any); hasInReplyTo {
			if parentEventID, hasEventID := inReplyTo["event_id"].(string); hasEventID {
				// Find the Mattermost post ID for this Matrix event
				var mattermostPostID string
				if mattermostPostID = b.getPostIDFromMatrixEvent(parentEventID, channelID); mattermostPostID != "" {
					// Found direct mapping, now get the actual thread root
					rootID = b.getThreadRootFromPostID(mattermostPostID)
					b.logger.LogDebug("Found Matrix reply to primary message", "matrix_event_id", event.EventID, "parent_event_id", parentEventID, "parent_post_id", mattermostPostID, "thread_root_id", rootID)
				} else {
					// Try to find if parent is a file attachment related to a primary message
					if mattermostPostID = b.getPostIDFromRelatedMatrixEvent(parentEventID, channelID); mattermostPostID != "" {
						// Found the primary message through file relation, now get the actual thread root
						rootID = b.getThreadRootFromPostID(mattermostPostID)
						b.logger.LogDebug("Found Matrix reply to file attachment, mapped to primary message", "matrix_event_id", event.EventID, "parent_event_id", parentEventID, "primary_post_id", mattermostPostID, "thread_root_id", rootID)
					} else {
						b.logger.LogDebug("Matrix reply parent not found in Mattermost", "matrix_event_id", event.EventID, "parent_event_id", parentEventID)
					}
				}
			}
		}
		// Also check for thread relation (m.thread) as fallback
		if rootID == "" {
			if relType, exists := relatesTo["rel_type"].(string); exists && relType == "m.thread" {
				if parentEventID, exists := relatesTo["event_id"].(string); exists {
					// Find the Mattermost post ID for this Matrix event
					var mattermostPostID string
					if mattermostPostID = b.getPostIDFromMatrixEvent(parentEventID, channelID); mattermostPostID != "" {
						// Found direct mapping, now get the actual thread root
						rootID = b.getThreadRootFromPostID(mattermostPostID)
						b.logger.LogDebug("Found Matrix thread to primary message", "matrix_event_id", event.EventID, "parent_event_id", parentEventID, "parent_post_id", mattermostPostID, "thread_root_id", rootID)
					} else {
						// Try to find if parent is a file attachment related to a primary message
						if mattermostPostID = b.getPostIDFromRelatedMatrixEvent(parentEventID, channelID); mattermostPostID != "" {
							// Found the primary message through file relation, now get the actual thread root
							rootID = b.getThreadRootFromPostID(mattermostPostID)
							b.logger.LogDebug("Found Matrix thread to file attachment, mapped to primary message", "matrix_event_id", event.EventID, "parent_event_id", parentEventID, "primary_post_id", mattermostPostID, "thread_root_id", rootID)
						}
					}
				}
			}
		}
	}

	// Create Mattermost post
	post := &model.Post{
		UserId:    mattermostUserID,
		ChannelId: channelID,
		Message:   mattermostContent,
		CreateAt:  event.Timestamp,
		RootId:    rootID,
		RemoteId:  &b.remoteID, // Attribute to Matrix remote
		Props:     make(map[string]any),
	}

	// Store Matrix event ID in post properties for reaction mapping and edit tracking
	config := b.getConfiguration()
	serverDomain := extractServerDomain(b.logger, config.MatrixServerURL)
	propertyKey := "matrix_event_id_" + serverDomain
	post.Props[propertyKey] = event.EventID
	post.Props["from_matrix"] = true

	// Create the post in Mattermost
	createdPost, appErr := b.API.CreatePost(post)
	if appErr != nil {
		return errors.Wrap(appErr, "failed to create Mattermost post")
	}

	// Store Matrix event ID to Mattermost post ID mapping for efficient reverse lookups
	b.storeMatrixEventPostMapping(event.EventID, createdPost.Id)

	b.logger.LogDebug("Successfully synced Matrix message to Mattermost", "matrix_event_id", event.EventID, "mattermost_post_id", createdPost.Id, "sender", event.Sender, "channel_id", channelID)
	return nil
}

// handleMatrixMessageEdit handles Matrix message edits by updating the corresponding Mattermost post
func (b *MatrixToMattermostBridge) handleMatrixMessageEdit(event MatrixEvent, channelID string) error {
	// Extract the new content from the edit event
	newContent := b.extractMatrixMessageContent(event)
	// Extract the original event ID being edited
	relatesTo, exists := event.Content["m.relates_to"].(map[string]any)
	if !exists {
		return errors.New("edit event missing m.relates_to")
	}

	originalEventID, exists := relatesTo["event_id"].(string)
	if !exists {
		return errors.New("edit event missing original event_id")
	}

	// Find the Mattermost post corresponding to the original Matrix event
	postID := b.getPostIDFromMatrixEvent(originalEventID, channelID)
	if postID == "" {
		b.logger.LogWarn("Cannot find Mattermost post for Matrix edit", "original_event_id", originalEventID, "edit_event_id", event.EventID)
		return nil // Post not found, maybe it wasn't synced
	}

	// Get the existing post
	post, appErr := b.API.GetPost(postID)
	if appErr != nil {
		return errors.Wrap(appErr, "failed to get post for edit")
	}

	// Update the post content (allow empty content - user may have deleted all text)
	post.Message = b.convertMatrixToMattermost(newContent)
	post.EditAt = event.Timestamp

	// Update the post
	updatedPost, appErr := b.API.UpdatePost(post)
	if appErr != nil {
		return errors.Wrap(appErr, "failed to update post")
	}

	b.logger.LogDebug("Successfully updated Mattermost post from Matrix edit", "original_matrix_event_id", originalEventID, "edit_matrix_event_id", event.EventID, "mattermost_post_id", updatedPost.Id)
	return nil
}

// syncMatrixFileToMattermost handles syncing Matrix file attachments to Mattermost
func (b *MatrixToMattermostBridge) syncMatrixFileToMattermost(event MatrixEvent, channelID string) error {
	b.logger.LogDebug("Syncing Matrix file to Mattermost", "event_id", event.EventID, "sender", event.Sender, "channel_id", channelID)

	// Extract file metadata
	body, exists := event.Content["body"].(string)
	if !exists {
		b.logger.LogWarn("Matrix file message missing body field", "event_id", event.EventID)
		return nil
	}

	url, exists := event.Content["url"].(string)
	if !exists {
		b.logger.LogWarn("Matrix file message missing url field", "event_id", event.EventID)
		return nil
	}

	// Get or create Mattermost user for the Matrix sender
	mattermostUserID, err := b.getOrCreateMattermostUser(event.Sender, channelID)
	if err != nil {
		return errors.Wrap(err, "failed to get or create Mattermost user for file")
	}

	// Download the file from Matrix
	fileData, err := b.downloadMatrixFile(url)
	if err != nil {
		return errors.Wrap(err, "failed to download Matrix file")
	}

	// Upload file to Mattermost
	uploadedFileInfo, appErr := b.API.UploadFile(fileData, channelID, body)
	if appErr != nil {
		return errors.Wrap(appErr, "failed to upload file to Mattermost")
	}

	// Check if this is a threaded message (reply)
	var rootID string
	if relatesTo, exists := event.Content["m.relates_to"].(map[string]any); exists {
		// Check for Matrix reply structure (m.in_reply_to)
		if inReplyTo, hasInReplyTo := relatesTo["m.in_reply_to"].(map[string]any); hasInReplyTo {
			if parentEventID, hasEventID := inReplyTo["event_id"].(string); hasEventID {
				// Find the Mattermost post ID for this Matrix event
				if mattermostPostID := b.getPostIDFromMatrixEvent(parentEventID, channelID); mattermostPostID != "" {
					rootID = mattermostPostID
					b.logger.LogDebug("Found Matrix file reply, setting root ID", "matrix_event_id", event.EventID, "parent_event_id", parentEventID, "mattermost_root_id", rootID)
				} else {
					b.logger.LogDebug("Matrix file reply parent not found in Mattermost", "matrix_event_id", event.EventID, "parent_event_id", parentEventID)
				}
			}
		}
		// Also check for thread relation (m.thread) as fallback
		if rootID == "" {
			if relType, exists := relatesTo["rel_type"].(string); exists && relType == "m.thread" {
				if parentEventID, exists := relatesTo["event_id"].(string); exists {
					// Find the Mattermost post ID for this Matrix event
					if mattermostPostID := b.getPostIDFromMatrixEvent(parentEventID, channelID); mattermostPostID != "" {
						rootID = mattermostPostID
						b.logger.LogDebug("Found Matrix file thread, setting root ID", "matrix_event_id", event.EventID, "parent_event_id", parentEventID, "mattermost_root_id", rootID)
					}
				}
			}
		}
	}

	// Create Mattermost post with file attachment (no message text needed)
	post := &model.Post{
		UserId:    mattermostUserID,
		ChannelId: channelID,
		Message:   "", // Empty message - like chess, the file attachment speaks for itself
		CreateAt:  event.Timestamp,
		RootId:    rootID,
		RemoteId:  &b.remoteID,
		FileIds:   []string{uploadedFileInfo.Id},
		Props:     make(map[string]any),
	}

	// Store Matrix event ID in post properties for reaction mapping and edit tracking
	config := b.getConfiguration()
	serverDomain := extractServerDomain(b.logger, config.MatrixServerURL)
	propertyKey := "matrix_event_id_" + serverDomain
	post.Props[propertyKey] = event.EventID
	post.Props["from_matrix"] = true

	// Create the post in Mattermost
	createdPost, appErr := b.API.CreatePost(post)
	if appErr != nil {
		return errors.Wrap(appErr, "failed to create Mattermost post with file attachment")
	}

	// Store Matrix event ID to Mattermost post ID mapping for efficient reverse lookups
	b.storeMatrixEventPostMapping(event.EventID, createdPost.Id)

	b.logger.LogDebug("Successfully synced Matrix file to Mattermost", "matrix_event_id", event.EventID, "mattermost_post_id", createdPost.Id, "filename", body, "file_id", uploadedFileInfo.Id)
	return nil
}

// syncMatrixReactionToMattermost handles syncing Matrix reactions to Mattermost
func (b *MatrixToMattermostBridge) syncMatrixReactionToMattermost(event MatrixEvent, channelID string) error {
	b.logger.LogDebug("Syncing Matrix reaction to Mattermost", "event_id", event.EventID, "sender", event.Sender, "channel_id", channelID)

	// Extract reaction key (emoji)
	relatesTo, exists := event.Content["m.relates_to"].(map[string]any)
	if !exists {
		return errors.New("reaction event missing m.relates_to")
	}

	key, exists := relatesTo["key"].(string)
	if !exists {
		return errors.New("reaction event missing key")
	}

	targetEventID, exists := relatesTo["event_id"].(string)
	if !exists {
		return errors.New("reaction event missing target event_id")
	}

	// Find the Mattermost post corresponding to the target Matrix event
	postID := b.getPostIDFromMatrixEvent(targetEventID, channelID)
	if postID == "" {
		// If we can't find a direct match, check if this is a reaction to a file message
		// that's related to a primary message
		postID = b.getPostIDFromRelatedMatrixEvent(targetEventID, channelID)
		if postID == "" {
			b.logger.LogWarn("Cannot find Mattermost post for Matrix reaction", "target_event_id", targetEventID, "reaction_event_id", event.EventID)
			return nil // Post not found, maybe it wasn't synced
		}
		b.logger.LogDebug("Found Mattermost post via related Matrix event", "target_event_id", targetEventID, "mattermost_post_id", postID)
	}

	// Get or create Mattermost user for the reaction sender
	mattermostUserID, err := b.getOrCreateMattermostUser(event.Sender, channelID)
	if err != nil {
		return errors.Wrap(err, "failed to get or create Mattermost user for reaction")
	}

	// Convert Matrix emoji to Mattermost format
	emojiName := b.convertMatrixEmojiToMattermost(key)

	// Create Mattermost reaction
	reaction := &model.Reaction{
		UserId:    mattermostUserID,
		PostId:    postID,
		EmojiName: emojiName,
		CreateAt:  event.Timestamp,
		RemoteId:  &b.remoteID, // Attribute to Matrix remote
	}

	// Add the reaction
	_, appErr := b.API.AddReaction(reaction)
	if appErr != nil {
		return errors.Wrap(appErr, "failed to add reaction to Mattermost")
	}

	// Store mapping of Matrix reaction event ID to Mattermost reaction info for deletion purposes
	// We need to store the info to reconstruct the model.Reaction object for deletion
	reactionKey := kvstore.BuildMatrixReactionKey(event.EventID)
	reactionInfo := map[string]string{
		"post_id":    postID,
		"user_id":    mattermostUserID,
		"emoji_name": emojiName,
	}
	reactionInfoBytes, _ := json.Marshal(reactionInfo)
	if err := b.kvstore.Set(reactionKey, reactionInfoBytes); err != nil {
		b.logger.LogWarn("Failed to store Matrix reaction mapping", "error", err, "reaction_event_id", event.EventID)
	} else {
		b.logger.LogDebug("Stored Matrix reaction mapping", "reaction_event_id", event.EventID, "post_id", postID, "emoji", emojiName)
	}

	b.logger.LogDebug("Successfully synced Matrix reaction to Mattermost", "matrix_event_id", event.EventID, "mattermost_post_id", postID, "emoji", emojiName, "sender", event.Sender)
	return nil
}

// syncMatrixRedactionToMattermost handles Matrix message deletions (redactions)
func (b *MatrixToMattermostBridge) syncMatrixRedactionToMattermost(event MatrixEvent, channelID string) error {
	b.logger.LogDebug("Syncing Matrix redaction to Mattermost", "event_id", event.EventID, "sender", event.Sender, "channel_id", channelID)

	// Extract the redacted event ID
	redactsEventID, exists := event.Content["redacts"].(string)
	if !exists {
		return errors.New("redaction event missing redacts field")
	}

	// Get the Matrix room ID to fetch the redacted event details
	matrixRoomIdentifier, err := b.GetMatrixRoomID(channelID)
	if err != nil {
		b.logger.LogWarn("Failed to get Matrix room identifier for redaction", "error", err, "channel_id", channelID)
		return nil
	}

	if matrixRoomIdentifier == "" {
		b.logger.LogWarn("No Matrix room mapped for channel", "channel_id", channelID)
		return nil
	}

	// Resolve room alias to room ID if needed
	matrixRoomID, err := b.matrixClient.ResolveRoomAlias(matrixRoomIdentifier)
	if err != nil {
		b.logger.LogWarn("Failed to resolve Matrix room identifier for redaction", "error", err, "room_identifier", matrixRoomIdentifier)
		return nil
	}

	// Get the redacted event to determine its type
	redactedEvent, err := b.matrixClient.GetEvent(matrixRoomID, redactsEventID)
	if err != nil {
		b.logger.LogWarn("Failed to get redacted Matrix event", "error", err, "redacted_event_id", redactsEventID)
		// If we can't get the event details, try to handle it as a post deletion (fallback)
		return b.handlePostDeletion(redactsEventID, channelID, event.EventID)
	}

	// Check the type of the redacted event
	redactedEventType, ok := redactedEvent["type"].(string)
	if !ok {
		b.logger.LogWarn("Redacted Matrix event has no type field", "redacted_event_id", redactsEventID)
		return nil
	}

	b.logger.LogDebug("Processing redaction", "redacted_event_type", redactedEventType, "redacted_event_id", redactsEventID)

	switch redactedEventType {
	case "m.reaction":
		// This is a reaction removal
		return b.handleReactionDeletion(redactedEvent, channelID, event.EventID)
	case "m.room.message":
		// This is a message deletion
		return b.handlePostDeletion(redactsEventID, channelID, event.EventID)
	default:
		b.logger.LogDebug("Ignoring redaction of unsupported event type", "redacted_event_type", redactedEventType, "redacted_event_id", redactsEventID)
		return nil
	}
}

// handleReactionDeletion handles the removal of a Matrix reaction from Mattermost
func (b *MatrixToMattermostBridge) handleReactionDeletion(redactedReactionEvent map[string]any, channelID, redactionEventID string) error {
	b.logger.LogDebug("Handling Matrix reaction deletion", "redaction_event_id", redactionEventID, "channel_id", channelID)

	// Get the redacted Matrix reaction event ID
	redactedEventID, ok := redactedReactionEvent["event_id"].(string)
	if !ok {
		return errors.New("redacted reaction event missing event_id")
	}

	// Look up the stored Mattermost reaction info using the Matrix reaction event ID
	reactionKey := kvstore.BuildMatrixReactionKey(redactedEventID)
	reactionInfoBytes, err := b.kvstore.Get(reactionKey)
	if err != nil {
		b.logger.LogWarn("Cannot find stored Matrix reaction mapping", "matrix_event_id", redactedEventID, "redaction_event_id", redactionEventID)
		return nil // Reaction mapping not found, maybe it wasn't stored or already deleted
	}

	// Parse the stored reaction info
	var reactionInfo map[string]string
	if err := json.Unmarshal(reactionInfoBytes, &reactionInfo); err != nil {
		b.logger.LogWarn("Failed to parse stored Matrix reaction info", "error", err, "matrix_event_id", redactedEventID)
		// Clean up the corrupted mapping
		_ = b.kvstore.Delete(reactionKey)
		return nil
	}

	postID := reactionInfo["post_id"]
	userID := reactionInfo["user_id"]
	emojiName := reactionInfo["emoji_name"]

	b.logger.LogDebug("Found stored Matrix reaction mapping", "matrix_event_id", redactedEventID, "post_id", postID, "user_id", userID, "emoji", emojiName)

	// Create reaction object for removal
	reaction := &model.Reaction{
		UserId:    userID,
		PostId:    postID,
		EmojiName: emojiName,
	}

	// Remove the reaction
	if appErr := b.API.RemoveReaction(reaction); appErr != nil {
		b.logger.LogWarn("Failed to remove Matrix reaction from Mattermost", "error", appErr, "post_id", postID, "emoji", emojiName)
		return errors.Wrap(appErr, "failed to remove reaction from Mattermost")
	}

	// Clean up the mapping after successful deletion
	if err := b.kvstore.Delete(reactionKey); err != nil {
		b.logger.LogWarn("Failed to clean up Matrix reaction mapping", "error", err, "key", reactionKey)
	}

	b.logger.LogDebug("Successfully removed Matrix reaction from Mattermost", "matrix_event_id", redactedEventID, "post_id", postID, "emoji", emojiName, "redaction_event_id", redactionEventID)
	return nil
}

// handlePostDeletion handles the deletion of a Matrix message from Mattermost
func (b *MatrixToMattermostBridge) handlePostDeletion(redactsEventID, channelID, redactionEventID string) error {
	b.logger.LogDebug("Handling Matrix post deletion", "redacted_event_id", redactsEventID, "redaction_event_id", redactionEventID, "channel_id", channelID)

	// Find the Mattermost post corresponding to the redacted Matrix event
	postID := b.getPostIDFromMatrixEvent(redactsEventID, channelID)
	if postID == "" {
		b.logger.LogWarn("Cannot find Mattermost post for Matrix redaction", "redacted_event_id", redactsEventID, "redaction_event_id", redactionEventID)
		return nil // Post not found, maybe it wasn't synced
	}

	// Delete the post
	if appErr := b.API.DeletePost(postID); appErr != nil {
		return errors.Wrap(appErr, "failed to delete post")
	}

	b.logger.LogDebug("Successfully deleted Mattermost post from Matrix redaction", "redacted_matrix_event_id", redactsEventID, "redaction_matrix_event_id", redactionEventID, "mattermost_post_id", postID)
	return nil
}

// getOrCreateMattermostUser gets or creates a Mattermost user for a Matrix user
// If channelID is provided, ensures the user is added to the team associated with that channel
func (b *MatrixToMattermostBridge) getOrCreateMattermostUser(matrixUserID string, channelID string) (string, error) {
	// Check if we already have a mapping for this Matrix user
	userMapKey := kvstore.BuildMatrixUserKey(matrixUserID)
	userIDBytes, err := b.kvstore.Get(userMapKey)
	if err == nil && len(userIDBytes) > 0 {
		mattermostUserID := string(userIDBytes)

		// Verify the user still exists
		if existingUser, appErr := b.API.GetUser(mattermostUserID); appErr == nil {
			// User exists, ensure they're in the team for this channel
			if channelID != "" {
				if err := b.addUserToChannelTeam(mattermostUserID, channelID); err != nil {
					b.logger.LogWarn("Failed to add existing Matrix user to team", "error", err, "user_id", mattermostUserID, "channel_id", channelID, "matrix_user_id", matrixUserID)
				}
			}

			// Check if we need to update their profile
			context := &ProfileUpdateContext{
				EventID: "",
				Source:  "api",
			}
			b.updateMattermostUserProfile(existingUser, matrixUserID, context)
			return mattermostUserID, nil
		}

		// User no longer exists, remove the mapping
		if err := b.kvstore.Delete(userMapKey); err != nil {
			b.logger.LogWarn("Failed to delete user mapping", "error", err, "key", userMapKey)
		}
	}

	// Extract username from Matrix user ID (@username:server.com -> username)
	username := b.extractUsernameFromMatrixUserID(matrixUserID)
	if username == "" {
		return "", errors.New("failed to extract username from Matrix user ID")
	}

	// Create a unique Mattermost username
	mattermostUsername := b.generateMattermostUsername(username)

	// Get real display name and avatar from Matrix profile
	var displayName string
	var firstName, lastName string
	var avatarData []byte

	if b.matrixClient != nil {
		profile, err := b.matrixClient.GetUserProfile(matrixUserID)
		if err != nil {
			b.logger.LogWarn("Failed to get Matrix user profile", "error", err, "user_id", matrixUserID)
			displayName = username // Fallback to username
		} else {
			if profile.DisplayName != "" {
				displayName = profile.DisplayName
				// Try to parse first/last name from display name
				firstName, lastName = parseDisplayName(profile.DisplayName)
			} else {
				displayName = username // Fallback to username if no display name set
			}

			// Download avatar if available
			if profile.AvatarURL != "" {
				avatarData, err = b.downloadMatrixAvatar(profile.AvatarURL)
				if err != nil {
					b.logger.LogWarn("Failed to download Matrix user avatar", "error", err, "user_id", matrixUserID, "avatar_url", profile.AvatarURL)
				}
			}
		}
	} else {
		displayName = username // Fallback if no Matrix client
	}

	// Create the user in Mattermost
	// Generate valid email by replacing colon with underscore (email local part cannot contain colons)
	emailUsername := strings.ReplaceAll(mattermostUsername, ":", "_")

	user := &model.User{
		Username:    mattermostUsername,
		Email:       fmt.Sprintf("%s@matrix.bridge", emailUsername), // Placeholder email with valid format
		Password:    model.NewId(),                                  // Generate random password (user won't use it for login)
		Nickname:    displayName,                                    // Use real Matrix display name
		FirstName:   firstName,                                      // Parsed from display name if possible
		LastName:    lastName,                                       // Parsed from display name if possible
		AuthData:    nil,
		AuthService: "",
		RemoteId:    &b.remoteID, // Attribute to Matrix remote
	}

	b.logger.LogDebug("Creating Mattermost user with remote details",
		"username", mattermostUsername,
		"email", user.Email,
		"remote_id", b.remoteID,
		"remote_id_ptr", user.RemoteId,
		"matrix_user_id", matrixUserID)

	createdUser, appErr := b.API.CreateUser(user)
	if appErr != nil {
		b.logger.LogError("Failed to create Mattermost user",
			"error", appErr.Error(),
			"username", mattermostUsername,
			"remote_id", b.remoteID,
			"remote_id_empty", b.remoteID == "",
			"matrix_user_id", matrixUserID)
		return "", errors.Wrap(appErr, "failed to create Mattermost user")
	}

	// Set avatar if we downloaded one
	if len(avatarData) > 0 {
		appErr = b.API.SetProfileImage(createdUser.Id, avatarData)
		if appErr != nil {
			b.logger.LogWarn("Failed to set Matrix user avatar", "error", appErr, "user_id", createdUser.Id, "matrix_user_id", matrixUserID)
		} else {
			b.logger.LogDebug("Successfully set avatar for Matrix user", "user_id", createdUser.Id, "matrix_user_id", matrixUserID)
		}
	}

	// Add user to team if channelID is provided
	if channelID != "" {
		if err := b.addUserToChannelTeam(createdUser.Id, channelID); err != nil {
			b.logger.LogWarn("Failed to add new Matrix user to team", "error", err, "user_id", createdUser.Id, "channel_id", channelID, "matrix_user_id", matrixUserID)
		}
	}

	// Store both directions of the mapping
	err = b.kvstore.Set(userMapKey, []byte(createdUser.Id))
	if err != nil {
		b.logger.LogWarn("Failed to store Matrix user mapping", "error", err, "matrix_user_id", matrixUserID, "mattermost_user_id", createdUser.Id)
	}

	// Store reverse mapping: mattermost_user_<mattermostUserID> -> matrixUserID
	// This mapping is critical for user lookups - treat failure as a serious issue
	mattermostUserKey := kvstore.BuildMattermostUserKey(createdUser.Id)
	err = b.kvstore.Set(mattermostUserKey, []byte(matrixUserID))
	if err != nil {
		b.logger.LogError("Failed to store critical reverse user mapping", "error", err, "mattermost_user_id", createdUser.Id, "matrix_user_id", matrixUserID)
		// Continue execution but this could cause lookup issues later
	}

	b.logger.LogDebug("Created Mattermost user for Matrix user", "matrix_user_id", matrixUserID, "mattermost_user_id", createdUser.Id, "username", mattermostUsername)
	return createdUser.Id, nil
}

// addUserToChannelTeam adds a user to the team that owns the specified channel
func (b *MatrixToMattermostBridge) addUserToChannelTeam(userID, channelID string) error {
	// Get the channel to find its team ID
	channel, appErr := b.API.GetChannel(channelID)
	if appErr != nil {
		return errors.Wrap(appErr, "failed to get channel")
	}

	// DM and group DM channels don't belong to teams
	if channel.Type == model.ChannelTypeDirect || channel.Type == model.ChannelTypeGroup {
		b.logger.LogDebug("Skipping team membership for DM channel", "user_id", userID, "channel_id", channelID, "channel_type", channel.Type)
		return nil
	}

	// Skip if team ID is empty
	if channel.TeamId == "" {
		b.logger.LogDebug("Skipping team membership for channel with no team", "user_id", userID, "channel_id", channelID)
		return nil
	}

	// Check if user is already a member of the team
	_, appErr = b.API.GetTeamMember(channel.TeamId, userID)
	if appErr == nil {
		// User is already a team member
		b.logger.LogDebug("User already member of team", "user_id", userID, "team_id", channel.TeamId, "channel_id", channelID)
		return nil
	}

	// Add user to the team
	_, appErr = b.API.CreateTeamMember(channel.TeamId, userID)
	if appErr != nil {
		return errors.Wrap(appErr, "failed to add user to team")
	}

	b.logger.LogDebug("Added Matrix user to team", "user_id", userID, "team_id", channel.TeamId, "channel_id", channelID)
	return nil
}

// extractUsernameFromMatrixUserID extracts username from Matrix user ID
func (b *MatrixToMattermostBridge) extractUsernameFromMatrixUserID(userID string) string {
	// Matrix user IDs are in format @username:server.com
	if !strings.HasPrefix(userID, "@") {
		return ""
	}

	parts := strings.Split(userID[1:], ":")
	if len(parts) == 0 {
		return ""
	}

	return parts[0]
}

// generateMattermostUsername creates a unique Mattermost username
func (b *MatrixToMattermostBridge) generateMattermostUsername(baseUsername string) string {
	// Sanitize username for Mattermost (following Shared Channels convention)
	sanitized := strings.ToLower(baseUsername)
	sanitized = regexp.MustCompile(`[^a-z0-9\-_]`).ReplaceAllString(sanitized, "_")

	// Get the configured username prefix for this server
	config := b.getConfiguration()
	prefix := config.GetMatrixUsernamePrefixForServer(config.MatrixServerURL)

	// Follow Shared Channels convention: prefix:username_sanitized
	username := prefix + ":" + sanitized

	// Ensure uniqueness by checking if username exists
	counter := 1
	originalUsername := username

	for {
		if _, appErr := b.API.GetUserByUsername(username); appErr != nil {
			// Username doesn't exist, we can use it
			break
		}

		// Username exists, try with counter
		username = fmt.Sprintf("%s_%d", originalUsername, counter)
		counter++

		// Prevent infinite loop
		if counter > 1000 {
			// Fallback to using counter
			username = fmt.Sprintf("%s:user_%d", prefix, counter)
			break
		}
	}

	return username
}

// getPostIDFromMatrixEvent finds the Mattermost post ID for a Matrix event ID.
// Uses an optimized lookup strategy: KV store first (fast for Matrix-originated events),
// then Matrix event metadata (for Mattermost-originated events).
func (b *MatrixToMattermostBridge) getPostIDFromMatrixEvent(matrixEventID, channelID string) string {
	// First check KV store for Matrix-originated events (O(1), no network calls)
	mappingKey := kvstore.BuildMatrixEventPostKey(matrixEventID)
	if postIDBytes, err := b.kvstore.Get(mappingKey); err == nil && len(postIDBytes) > 0 {
		postID := string(postIDBytes)
		b.logger.LogDebug("Found Mattermost post ID for Matrix-originated event in KV store", "matrix_event_id", matrixEventID, "mattermost_post_id", postID)
		return postID
	}

	// Fall back to Matrix event metadata for Mattermost-originated events
	return b.getPostIDFromMatrixEventMetadata(matrixEventID, channelID)
}

// storeMatrixEventPostMapping stores the mapping from Matrix event ID to Mattermost post ID
// for efficient reverse lookups. This is used by both message and file sync functions.
func (b *MatrixToMattermostBridge) storeMatrixEventPostMapping(matrixEventID, mattermostPostID string) {
	mappingKey := kvstore.BuildMatrixEventPostKey(matrixEventID)
	if err := b.kvstore.Set(mappingKey, []byte(mattermostPostID)); err != nil {
		b.logger.LogWarn("Failed to store Matrix event to post mapping", "error", err, "matrix_event_id", matrixEventID, "post_id", mattermostPostID)
		// Continue anyway - post was created successfully
	}
}

// getPostIDFromMatrixEventMetadata retrieves post ID from Matrix event content (for Mattermost-originated events)
func (b *MatrixToMattermostBridge) getPostIDFromMatrixEventMetadata(matrixEventID, channelID string) string {
	// Get the Matrix room ID for this channel
	matrixRoomIdentifier, err := b.GetMatrixRoomID(channelID)
	if err != nil {
		b.logger.LogDebug("Failed to get Matrix room identifier", "error", err, "channel_id", channelID, "matrix_event_id", matrixEventID)
		return ""
	}

	if matrixRoomIdentifier == "" {
		b.logger.LogDebug("No Matrix room identifier found for channel", "channel_id", channelID, "matrix_event_id", matrixEventID)
		return ""
	}

	// Resolve room alias to room ID if needed (handles both aliases and room IDs correctly)
	matrixRoomID, err := b.matrixClient.ResolveRoomAlias(matrixRoomIdentifier)
	if err != nil {
		b.logger.LogDebug("Failed to resolve Matrix room identifier", "error", err, "room_identifier", matrixRoomIdentifier, "matrix_event_id", matrixEventID)
		return ""
	}

	// Get the Matrix event to extract Mattermost metadata
	event, err := b.matrixClient.GetEvent(matrixRoomID, matrixEventID)
	if err != nil {
		b.logger.LogDebug("Failed to get Matrix event", "error", err, "event_id", matrixEventID, "room_id", matrixRoomID)
		return ""
	}

	// Extract Mattermost post ID from event content
	content, ok := event["content"].(map[string]any)
	if !ok {
		b.logger.LogDebug("Matrix event has no content field", "event_id", matrixEventID)
		return ""
	}

	if postID, exists := content["mattermost_post_id"].(string); exists && postID != "" {
		b.logger.LogDebug("Found Mattermost post ID in Matrix event metadata", "matrix_event_id", matrixEventID, "mattermost_post_id", postID)
		return postID
	}

	// No Mattermost post found for this Matrix event
	b.logger.LogDebug("No Mattermost post found for Matrix event", "event_id", matrixEventID)
	return ""
}

// getPostIDFromRelatedMatrixEvent finds a Mattermost post ID by checking if the Matrix event
// is related to a primary message that has a Mattermost post ID
func (b *MatrixToMattermostBridge) getPostIDFromRelatedMatrixEvent(matrixEventID, channelID string) string {
	// Get the Matrix room ID for this channel
	matrixRoomIdentifier, err := b.GetMatrixRoomID(channelID)
	if err != nil {
		b.logger.LogWarn("Failed to get Matrix room identifier for related event lookup", "error", err, "channel_id", channelID)
		return ""
	}

	if matrixRoomIdentifier == "" {
		return ""
	}

	// Resolve room alias to room ID if needed
	matrixRoomID, err := b.matrixClient.ResolveRoomAlias(matrixRoomIdentifier)
	if err != nil {
		b.logger.LogWarn("Failed to resolve Matrix room identifier for related event lookup", "error", err, "room_identifier", matrixRoomIdentifier)
		return ""
	}

	// Get the Matrix event to check if it's related to another event
	event, err := b.matrixClient.GetEvent(matrixRoomID, matrixEventID)
	if err != nil {
		b.logger.LogWarn("Failed to get Matrix event for related lookup", "error", err, "event_id", matrixEventID)
		return ""
	}

	// Check if this event is related to another event (file message related to primary message)
	content, ok := event["content"].(map[string]any)
	if !ok {
		return ""
	}

	relatesTo, ok := content["m.relates_to"].(map[string]any)
	if !ok {
		return ""
	}

	// Check if this is a file attachment relation
	relType, ok := relatesTo["rel_type"].(string)
	primaryEventID, hasEventID := relatesTo["event_id"].(string)
	if !ok || relType != "m.mattermost.post" || !hasEventID {
		return ""
	}

	b.logger.LogDebug("Found file message related to primary message", "file_event_id", matrixEventID, "primary_event_id", primaryEventID)

	// Now look up the Mattermost post ID using the primary event ID
	return b.getPostIDFromMatrixEvent(primaryEventID, channelID)
}

// getThreadRootFromPostID finds the actual thread root ID for a given Mattermost post ID.
// If the post is already a thread root, returns the same ID. If it's a thread reply, returns the RootId.
func (b *MatrixToMattermostBridge) getThreadRootFromPostID(postID string) string {
	if postID == "" {
		return ""
	}

	// Get the post to check if it's part of a thread
	post, appErr := b.API.GetPost(postID)
	if appErr != nil {
		b.logger.LogWarn("Failed to get post for thread root lookup", "error", appErr, "post_id", postID)
		return postID // Return original ID as fallback
	}

	// If this post has a RootId, return that (it's a thread reply)
	if post.RootId != "" {
		b.logger.LogDebug("Post is thread reply, returning root ID", "post_id", postID, "root_id", post.RootId)
		return post.RootId
	}

	// Otherwise, this post is either standalone or the thread root itself
	b.logger.LogDebug("Post is thread root or standalone", "post_id", postID)
	return postID
}

// convertMatrixToMattermost converts Matrix message format to Mattermost
func (b *MatrixToMattermostBridge) convertMatrixToMattermost(content string) string {
	// Basic conversion - can be enhanced later
	// Matrix uses different markdown syntax in some cases

	// Convert Matrix mentions (@username:server.com) to Mattermost format if possible
	// For now, keep as-is since we'd need user mapping

	return content
}

// convertMatrixEmojiToMattermost converts Matrix emoji format to Mattermost
func (b *MatrixToMattermostBridge) convertMatrixEmojiToMattermost(matrixEmoji string) string {
	// Matrix reactions can be Unicode emoji or custom emoji

	// If it's already a simple name (like "thumbsup"), validate it exists in Mattermost
	if regexp.MustCompile(`^[a-z0-9_+-]+$`).MatchString(matrixEmoji) {
		// Check if this emoji name exists in our mapping
		if _, exists := emojiNameToIndex[matrixEmoji]; exists {
			return matrixEmoji
		}
		// Fall through to Unicode handling if name doesn't exist
	}

	// For Unicode emoji, try to find the corresponding Mattermost name
	// Search through our emoji mappings to find a match
	for emojiName, index := range emojiNameToIndex {
		if unicodeHex, exists := emojiIndexToUnicode[index]; exists {
			unicode := hexToUnicode(unicodeHex)
			if unicode == matrixEmoji {
				return emojiName
			}
		}
	}

	// Try to find similar matches (without variation selectors)
	stripVariation := func(s string) string {
		// Remove common variation selectors
		s = strings.ReplaceAll(s, "\uFE0F", "") // variation selector-16
		s = strings.ReplaceAll(s, "\uFE0E", "") // variation selector-15
		return s
	}

	strippedMatrix := stripVariation(matrixEmoji)
	if strippedMatrix != matrixEmoji {
		for emojiName, index := range emojiNameToIndex {
			if unicodeHex, exists := emojiIndexToUnicode[index]; exists {
				unicode := stripVariation(hexToUnicode(unicodeHex))
				if unicode == strippedMatrix {
					return emojiName
				}
			}
		}
	}

	// Fallback mapping for common emojis
	switch matrixEmoji {
	case "👍":
		return "+1"
	case "👎":
		return "-1"
	case "❤️", "❤":
		return "heart"
	case "😂":
		return "joy"
	case "😢":
		return "cry"
	case "😄":
		return "smile"
	case "😃":
		return "smiley"
	case "😊":
		return "blush"
	case "🔥":
		return "fire"
	case "🎉":
		return "tada"
	case "👏":
		return "clap"
	default:
		// If we can't find a match, use a safe fallback
		// Check if this is a complex emoji sequence that we should just skip
		if len([]rune(matrixEmoji)) > 3 {
			b.logger.LogWarn("Unknown complex emoji from Matrix", "emoji", matrixEmoji)
			return "question" // Use question mark emoji as fallback
		}

		// For simple unknown emoji, try to create a safe name
		safeName := regexp.MustCompile(`[^a-z0-9]`).ReplaceAllString(strings.ToLower(matrixEmoji), "")
		if safeName == "" || len(safeName) < 2 {
			return "question" // Use question mark emoji as fallback
		}
		return safeName
	}
}

// downloadMatrixAvatar downloads an avatar image from a Matrix MXC URI
func (b *MatrixToMattermostBridge) downloadMatrixAvatar(avatarURL string) ([]byte, error) {
	if b.matrixClient == nil {
		return nil, errors.New("Matrix client not configured")
	}

	// Use the Matrix client's download method with size limit and image content type validation
	return b.matrixClient.DownloadFile(avatarURL, b.maxProfileImageSize, "image/")
}

// ProfileUpdateContext provides context information for profile updates
type ProfileUpdateContext struct {
	EventID string // Matrix event ID if update is from an event
	Source  string // "api" for proactive checks, "event" for Matrix member events
}

// updateMattermostUserProfile updates an existing Mattermost user's profile from Matrix
// Can be called either proactively (fetching current profile) or reactively (from Matrix events)
func (b *MatrixToMattermostBridge) updateMattermostUserProfile(mattermostUser *model.User, matrixUserID string, context *ProfileUpdateContext, profileData ...*matrix.UserProfile) {
	if b.matrixClient == nil {
		return
	}

	var profile *matrix.UserProfile
	var err error

	// Determine profile data source
	if len(profileData) > 0 && profileData[0] != nil {
		// Use provided profile data (from Matrix event)
		profile = profileData[0]
	} else {
		// Fetch current Matrix profile (proactive check)
		profile, err = b.matrixClient.GetUserProfile(matrixUserID)
		if err != nil {
			b.logger.LogWarn("Failed to get Matrix user profile for update", "error", err, "user_id", matrixUserID)
			return
		}
	}

	var needsUpdate bool
	updatedUser := *mattermostUser // Create a copy

	// Check display name updates
	if profile.DisplayName != "" {
		// Parse first/last name from display name
		firstName, lastName := parseDisplayName(profile.DisplayName)

		// Update nickname (display name)
		if updatedUser.Nickname != profile.DisplayName {
			if context.Source == "event" {
				b.logger.LogDebug("Updating user display name from Matrix event", "user_id", mattermostUser.Id, "old_name", mattermostUser.Nickname, "new_name", profile.DisplayName, "matrix_user_id", matrixUserID, "event_id", context.EventID)
			} else {
				b.logger.LogDebug("Updating display name from proactive check", "user_id", mattermostUser.Id, "old", mattermostUser.Nickname, "new", profile.DisplayName)
			}
			updatedUser.Nickname = profile.DisplayName
			needsUpdate = true
		}

		// Update first/last name if they changed
		if updatedUser.FirstName != firstName {
			updatedUser.FirstName = firstName
			needsUpdate = true
		}
		if updatedUser.LastName != lastName {
			updatedUser.LastName = lastName
			needsUpdate = true
		}
	}

	// Update user profile if needed
	if needsUpdate {
		if _, appErr := b.API.UpdateUser(&updatedUser); appErr != nil {
			if context.Source == "event" {
				b.logger.LogError("Failed to update Mattermost user profile from Matrix event", "error", appErr, "user_id", mattermostUser.Id, "matrix_user_id", matrixUserID, "event_id", context.EventID)
			} else {
				b.logger.LogWarn("Failed to update Mattermost user profile from proactive check", "error", appErr, "user_id", mattermostUser.Id, "matrix_user_id", matrixUserID)
			}
		} else {
			if context.Source == "event" {
				b.logger.LogDebug("Successfully updated Mattermost user profile from Matrix event", "user_id", mattermostUser.Id, "matrix_user_id", matrixUserID, "display_name", profile.DisplayName, "event_id", context.EventID)
			} else {
				b.logger.LogDebug("Updated Mattermost user profile from proactive check", "user_id", mattermostUser.Id, "matrix_user_id", matrixUserID, "display_name", profile.DisplayName)
			}
		}
	}

	// Check avatar updates
	if profile.AvatarURL != "" {
		b.updateMattermostUserAvatar(mattermostUser, matrixUserID, profile.AvatarURL, context)
	}
}

// updateMattermostUserAvatar updates a Mattermost user's profile image from Matrix
// Compares current and new image data to avoid unnecessary updates
func (b *MatrixToMattermostBridge) updateMattermostUserAvatar(mattermostUser *model.User, matrixUserID, matrixAvatarURL string, context *ProfileUpdateContext) {
	// Get current Mattermost profile image
	currentAvatarData, appErr := b.API.GetProfileImage(mattermostUser.Id)
	if appErr != nil {
		if context.Source == "event" {
			b.logger.LogWarn("Failed to get current Mattermost profile image for comparison", "error", appErr, "user_id", mattermostUser.Id, "matrix_user_id", matrixUserID, "event_id", context.EventID)
		} else {
			b.logger.LogDebug("Failed to get current Mattermost profile image for comparison", "error", appErr, "user_id", mattermostUser.Id, "matrix_user_id", matrixUserID)
		}
		// Continue with update even if we can't get current image
		currentAvatarData = nil
	}

	// Download Matrix avatar image
	newAvatarData, err := b.downloadMatrixAvatar(matrixAvatarURL)
	if err != nil {
		if context.Source == "event" {
			b.logger.LogWarn("Failed to download Matrix avatar for comparison", "error", err, "user_id", mattermostUser.Id, "matrix_user_id", matrixUserID, "avatar_url", matrixAvatarURL, "event_id", context.EventID)
		} else {
			b.logger.LogDebug("Failed to download Matrix avatar for comparison", "error", err, "user_id", mattermostUser.Id, "matrix_user_id", matrixUserID, "avatar_url", matrixAvatarURL)
		}
		return
	}

	// Compare image data to see if update is needed
	if currentAvatarData != nil && b.compareImageData(currentAvatarData, newAvatarData) {
		b.logger.LogDebug("Matrix avatar unchanged, skipping update", "user_id", mattermostUser.Id, "matrix_user_id", matrixUserID, "source", context.Source)
		return
	}

	// Images are different, update the profile image
	appErr = b.API.SetProfileImage(mattermostUser.Id, newAvatarData)
	if appErr != nil {
		if context.Source == "event" {
			b.logger.LogError("Failed to update Mattermost user avatar from Matrix event", "error", appErr, "user_id", mattermostUser.Id, "matrix_user_id", matrixUserID, "event_id", context.EventID)
		} else {
			b.logger.LogWarn("Failed to update Mattermost user avatar from proactive check", "error", appErr, "user_id", mattermostUser.Id, "matrix_user_id", matrixUserID)
		}
		return
	}

	// Log successful avatar update
	if context.Source == "event" {
		b.logger.LogDebug("Successfully updated Mattermost user avatar from Matrix event", "user_id", mattermostUser.Id, "matrix_user_id", matrixUserID, "avatar_url", matrixAvatarURL, "event_id", context.EventID, "size_bytes", len(newAvatarData))
	} else {
		b.logger.LogDebug("Updated Mattermost user avatar from proactive check", "user_id", mattermostUser.Id, "matrix_user_id", matrixUserID, "avatar_url", matrixAvatarURL, "size_bytes", len(newAvatarData))
	}
}

// compareImageData compares two image byte arrays to determine if they're the same
// Uses a simple byte comparison for now, could be enhanced with more sophisticated comparison
func (b *MatrixToMattermostBridge) compareImageData(currentData, newData []byte) bool {
	// Quick size check first
	if len(currentData) != len(newData) {
		return false
	}

	// If both are empty, they're the same
	if len(currentData) == 0 {
		return true
	}

	// Compare byte by byte
	for i := range currentData {
		if currentData[i] != newData[i] {
			return false
		}
	}

	return true
}

// syncMatrixMemberEventToMattermost handles Matrix member events (joins, leaves, profile changes)
func (b *MatrixToMattermostBridge) syncMatrixMemberEventToMattermost(event MatrixEvent, channelID string) error {
	b.logger.LogDebug("Processing Matrix member event", "event_id", event.EventID, "sender", event.Sender, "channel_id", channelID)

	// Skip events from our own ghost users to prevent loops
	if b.isGhostUser(event.Sender) {
		b.logger.LogDebug("Ignoring member event from ghost user", "sender", event.Sender, "event_id", event.EventID)
		return nil
	}

	// Extract membership state and content
	membership, ok := event.Content["membership"].(string)
	if !ok {
		b.logger.LogDebug("Member event missing membership field", "event_id", event.EventID, "sender", event.Sender)
		return nil
	}

	// Check if we have a Mattermost user for this Matrix user
	userMapKey := kvstore.BuildMatrixUserKey(event.Sender)
	userIDBytes, err := b.kvstore.Get(userMapKey)
	existingUserID := ""
	userExists := false
	if err == nil && len(userIDBytes) > 0 {
		existingUserID = string(userIDBytes)
		userExists = true
	}

	switch membership {
	case "join":
		return b.handleMatrixMemberJoin(event, channelID, existingUserID, userExists)
	case "leave", "ban":
		return b.handleMatrixMemberLeave(event, channelID, existingUserID, userExists)
	default:
		b.logger.LogDebug("Ignoring unsupported membership state", "event_id", event.EventID, "sender", event.Sender, "membership", membership)
		return nil
	}
}

// handleMatrixMemberJoin processes Matrix member join events - both new joins and profile changes
func (b *MatrixToMattermostBridge) handleMatrixMemberJoin(event MatrixEvent, channelID, existingUserID string, userExists bool) error {
	// Check if this is a profile change (has displayname or avatar_url in content)
	displayName, hasDisplayName := event.Content["displayname"].(string)
	avatarURL, hasAvatarURL := event.Content["avatar_url"].(string)
	hasProfileData := hasDisplayName || hasAvatarURL

	if userExists {
		// Existing user joining - always ensure they're in the channel first
		b.logger.LogDebug("Ensuring existing Matrix user is in Mattermost channel", "event_id", event.EventID, "sender", event.Sender, "channel_id", channelID)
		if err := b.ensureUserInChannel(existingUserID, channelID); err != nil {
			b.logger.LogWarn("Failed to ensure existing user is in channel", "error", err, "user_id", existingUserID, "channel_id", channelID)
		}

		// Also handle profile change if profile data is present
		if hasProfileData {
			b.logger.LogDebug("Detected Matrix profile change for existing user", "event_id", event.EventID, "sender", event.Sender, "display_name", displayName, "avatar_url", avatarURL)
			return b.updateExistingUserProfile(existingUserID, event.Sender, event.EventID, displayName, avatarURL)
		}

		return nil
	}

	// New user joining - create them and add to channel
	b.logger.LogDebug("New Matrix user joining room", "event_id", event.EventID, "sender", event.Sender, "channel_id", channelID)
	mattermostUserID, err := b.getOrCreateMattermostUser(event.Sender, channelID)
	if err != nil {
		return errors.Wrap(err, "failed to create Mattermost user for Matrix join")
	}

	// Add the new user to the channel
	return b.addUserToChannel(mattermostUserID, channelID)
}

// handleMatrixMemberLeave processes Matrix member leave/ban events
func (b *MatrixToMattermostBridge) handleMatrixMemberLeave(event MatrixEvent, channelID, existingUserID string, userExists bool) error {
	if !userExists {
		b.logger.LogDebug("Matrix user leaving room but no Mattermost user exists", "event_id", event.EventID, "sender", event.Sender)
		return nil
	}

	membership := event.Content["membership"].(string)
	b.logger.LogDebug("Matrix user leaving room", "event_id", event.EventID, "sender", event.Sender, "membership", membership, "channel_id", channelID)

	// Remove user from the Mattermost channel
	return b.removeUserFromChannel(existingUserID, channelID)
}

// updateExistingUserProfile updates an existing user's profile from Matrix event data
func (b *MatrixToMattermostBridge) updateExistingUserProfile(mattermostUserID, matrixUserID, eventID, displayName, avatarURL string) error {
	// Get the existing Mattermost user
	mattermostUser, appErr := b.API.GetUser(mattermostUserID)
	if appErr != nil {
		b.logger.LogWarn("Failed to get Mattermost user for profile update", "error", appErr, "user_id", mattermostUserID, "matrix_user_id", matrixUserID)
		return nil
	}

	b.logger.LogDebug("Found Mattermost user for profile update", "user_id", mattermostUser.Id, "username", mattermostUser.Username, "matrix_user_id", matrixUserID)

	// Create profile data from Matrix event
	eventProfile := &matrix.UserProfile{
		DisplayName: displayName,
		AvatarURL:   avatarURL,
	}

	// Update the user's profile using the unified method
	context := &ProfileUpdateContext{
		EventID: eventID,
		Source:  "event",
	}

	b.updateMattermostUserProfile(mattermostUser, matrixUserID, context, eventProfile)
	return nil
}

// ensureUserInChannel ensures a user is a member of the specified channel
func (b *MatrixToMattermostBridge) ensureUserInChannel(userID, channelID string) error {
	// Check if user is already a channel member
	_, appErr := b.API.GetChannelMember(channelID, userID)
	if appErr == nil {
		// User is already a channel member
		b.logger.LogDebug("User already member of channel", "user_id", userID, "channel_id", channelID)
		return nil
	}

	// Add user to the channel
	return b.addUserToChannel(userID, channelID)
}

// addUserToChannel adds a user to a Mattermost channel
func (b *MatrixToMattermostBridge) addUserToChannel(userID, channelID string) error {
	// Ensure user is in the team first
	if err := b.addUserToChannelTeam(userID, channelID); err != nil {
		return errors.Wrap(err, "failed to add user to team")
	}

	// Add user to the channel
	_, appErr := b.API.AddChannelMember(channelID, userID)
	if appErr != nil {
		return errors.Wrap(appErr, "failed to add user to channel")
	}

	b.logger.LogDebug("Added Matrix user to Mattermost channel", "user_id", userID, "channel_id", channelID)
	return nil
}

// removeUserFromChannel removes a user from a Mattermost channel
func (b *MatrixToMattermostBridge) removeUserFromChannel(userID, channelID string) error {
	// Remove user from the channel
	appErr := b.API.DeleteChannelMember(channelID, userID)
	if appErr != nil {
		return errors.Wrap(appErr, "failed to remove user from channel")
	}

	b.logger.LogDebug("Removed Matrix user from Mattermost channel", "user_id", userID, "channel_id", channelID)
	return nil
}
