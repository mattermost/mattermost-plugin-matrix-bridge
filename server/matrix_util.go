package main

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	md "github.com/JohannesKaufmann/html-to-markdown"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/pkg/errors"
)

// Logger interface for logging operations
type Logger interface {
	LogWarn(message string, keyValuePairs ...any)
}

// getGhostUser retrieves the Matrix ghost user ID for a Mattermost user if it exists
func (p *Plugin) getGhostUser(mattermostUserID string) (string, bool) {
	ghostUserKey := "ghost_user_" + mattermostUserID
	ghostUserIDBytes, err := p.kvstore.Get(ghostUserKey)
	if err == nil && len(ghostUserIDBytes) > 0 {
		return string(ghostUserIDBytes), true
	}
	return "", false
}

// createOrGetGhostUser creates a new Matrix ghost user for a Mattermost user, or returns existing one
func (p *Plugin) createOrGetGhostUser(mattermostUserID string) (string, error) {
	// First check if ghost user already exists
	if ghostUserID, exists := p.getGhostUser(mattermostUserID); exists {
		return ghostUserID, nil
	}

	// Ghost user doesn't exist, create a new one
	// Get the Mattermost user to fetch display name and avatar
	user, appErr := p.API.GetUser(mattermostUserID)
	if appErr != nil {
		return "", errors.Wrap(appErr, "failed to get Mattermost user for ghost user creation")
	}

	// Get display name
	displayName := user.GetDisplayName(model.ShowFullName)

	// Get user's avatar image data
	var avatarData []byte
	var avatarContentType string
	if imageData, appErr := p.API.GetProfileImage(mattermostUserID); appErr == nil {
		avatarData = imageData
		avatarContentType = "image/png" // Mattermost typically returns PNG
	}

	// Create new ghost user with display name and avatar
	ghostUser, err := p.matrixClient.CreateGhostUser(mattermostUserID, displayName, avatarData, avatarContentType)
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
	ghostUserKey := "ghost_user_" + mattermostUserID
	err = p.kvstore.Set(ghostUserKey, []byte(ghostUser.UserID))
	if err != nil {
		p.API.LogWarn("Failed to cache ghost user ID", "error", err, "ghost_user_id", ghostUser.UserID)
		// Continue anyway, the ghost user was created successfully
	}

	if displayName != "" {
		p.API.LogDebug("Created new ghost user with display name", "mattermost_user_id", mattermostUserID, "ghost_user_id", ghostUser.UserID, "display_name", displayName)
	} else {
		p.API.LogDebug("Created new ghost user", "mattermost_user_id", mattermostUserID, "ghost_user_id", ghostUser.UserID)
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

// getMatrixRoomID retrieves the Matrix room identifier for a given Mattermost channel
func (p *Plugin) getMatrixRoomID(channelID string) (string, error) {
	roomID, err := p.kvstore.Get("channel_mapping_" + channelID)
	if err != nil {
		return "", errors.Wrap(err, "failed to get room mapping from store")
	}
	return string(roomID), nil
}

// extractServerDomain extracts the hostname from a Matrix server URL
func extractServerDomain(logger Logger, serverURL string) string {
	if serverURL == "" {
		return "unknown"
	}

	parsedURL, err := url.Parse(serverURL)
	if err != nil {
		logger.LogWarn("Failed to parse Matrix server URL", "url", serverURL, "error", err)
		return "unknown"
	}

	hostname := parsedURL.Hostname()
	if hostname == "" {
		logger.LogWarn("Could not extract hostname from Matrix server URL", "url", serverURL)
		return "unknown"
	}

	// Replace dots and colons to make it safe for use in property keys
	return strings.ReplaceAll(strings.ReplaceAll(hostname, ".", "_"), ":", "_")
}

// findAndDeleteFileMessage finds and deletes file attachment messages that are replies to the main post
func (p *Plugin) findAndDeleteFileMessage(matrixRoomID, ghostUserID, filename, postEventID string) error {
	// Get all reply messages to the main post event
	relations, err := p.matrixClient.GetEventRelationsAsUser(matrixRoomID, postEventID, ghostUserID)
	if err != nil {
		return errors.Wrap(err, "failed to get event relations from Matrix")
	}

	// Find file messages that are replies to this post
	var fileEventID string
	for _, event := range relations {
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

		// Check if this is a reply relationship
		content, ok := event["content"].(map[string]any)
		if !ok {
			continue
		}

		relatesTo, ok := content["m.relates_to"].(map[string]any)
		if !ok {
			continue
		}

		inReplyTo, ok := relatesTo["m.in_reply_to"].(map[string]any)
		if !ok {
			continue
		}

		replyEventID, hasEventID := inReplyTo["event_id"].(string)
		if !hasEventID || replyEventID != postEventID {
			continue
		}

		// Check if this is a file message with the matching filename
		msgType, ok := content["msgtype"].(string)
		if !ok {
			continue
		}

		// File messages have msgtype of m.file, m.image, m.video, or m.audio
		if msgType != "m.file" && msgType != "m.image" && msgType != "m.video" && msgType != "m.audio" {
			continue
		}

		// Check if the filename matches
		body, ok := content["body"].(string)
		if ok && body == filename {
			// Found the matching file message
			eventID, ok := event["event_id"].(string)
			if ok {
				fileEventID = eventID
				break
			}
		}
	}

	if fileEventID == "" {
		p.API.LogWarn("No matching file message found to delete", "filename", filename, "post_event_id", postEventID)
		return nil
	}

	// Redact the file message
	_, err = p.matrixClient.RedactEventAsGhost(matrixRoomID, fileEventID, ghostUserID)
	if err != nil {
		return errors.Wrap(err, "failed to redact file message in Matrix")
	}

	p.API.LogDebug("Successfully deleted file message from Matrix", "filename", filename, "file_event_id", fileEventID, "post_event_id", postEventID)
	return nil
}

// deleteAllFileReplies finds and deletes all file attachment messages using custom metadata
func (p *Plugin) deleteAllFileReplies(matrixRoomID, postEventID, ghostUserID string) error {
	// Look for the file metadata event that contains the file attachment event IDs
	fileEventIDs, err := p.getFileEventIDsFromMetadata(matrixRoomID, postEventID, ghostUserID)
	if err != nil {
		p.API.LogWarn("Failed to get file event IDs from metadata", "error", err, "post_event_id", postEventID)
		return nil // Don't fail - the main message will still be deleted
	}

	if len(fileEventIDs) == 0 {
		p.API.LogDebug("No file attachments found in metadata for post deletion", "post_event_id", postEventID)
		return nil
	}

	p.API.LogDebug("Found file attachments from metadata", "post_event_id", postEventID, "file_count", len(fileEventIDs))

	var deletedCount int
	var firstError error

	// Delete each file attachment event
	for _, fileEventID := range fileEventIDs {
		_, err := p.matrixClient.RedactEventAsGhost(matrixRoomID, fileEventID, ghostUserID)
		if err != nil {
			p.API.LogWarn("Failed to delete file attachment", "error", err, "file_event_id", fileEventID, "post_event_id", postEventID)
			if firstError == nil {
				firstError = err
			}
		} else {
			deletedCount++
			p.API.LogDebug("Deleted file attachment", "file_event_id", fileEventID, "post_event_id", postEventID)
		}
	}

	if deletedCount > 0 {
		p.API.LogDebug("Deleted file attachments using metadata", "count", deletedCount, "post_event_id", postEventID)
	}

	return firstError
}

// getFileEventIDsFromMetadata retrieves file attachment event IDs from custom metadata
func (p *Plugin) getFileEventIDsFromMetadata(matrixRoomID, postEventID, ghostUserID string) ([]string, error) {
	// Get relations to find the metadata event
	relations, err := p.matrixClient.GetEventRelationsAsUser(matrixRoomID, postEventID, ghostUserID)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get event relations from Matrix")
	}

	p.API.LogDebug("Searching for file metadata", "post_event_id", postEventID, "relations_count", len(relations))

	// Look for our custom metadata event
	for _, event := range relations {
		p.API.LogDebug("Processing relation for metadata search", "event_type", event["type"], "sender", event["sender"])
		eventType, ok := event["type"].(string)
		if !ok || eventType != "m.mattermost.file_metadata" {
			continue
		}

		// Check if this event is from our ghost user
		sender, ok := event["sender"].(string)
		if !ok || sender != ghostUserID {
			continue
		}

		// Get the content
		content, ok := event["content"].(map[string]any)
		if !ok {
			continue
		}

		// Check if this metadata is for our message
		relatedMessage, ok := content["relates_to_message"].(string)
		if !ok || relatedMessage != postEventID {
			continue
		}

		// Extract file attachment event IDs
		fileAttachmentsRaw, ok := content["file_attachments"]
		if !ok {
			continue
		}

		fileAttachments, ok := fileAttachmentsRaw.([]any)
		if !ok {
			continue
		}

		var fileEventIDs []string
		for _, attachment := range fileAttachments {
			if eventID, ok := attachment.(string); ok {
				fileEventIDs = append(fileEventIDs, eventID)
			}
		}

		p.API.LogDebug("Found file metadata", "post_event_id", postEventID, "file_event_ids", fileEventIDs)
		return fileEventIDs, nil
	}

	return nil, errors.New("no file metadata found")
}

// parseDisplayName attempts to parse a display name into first and last name components
func parseDisplayName(displayName string) (firstName, lastName string) {
	if displayName == "" {
		return "", ""
	}

	// Trim whitespace
	displayName = strings.TrimSpace(displayName)

	// Split on spaces
	parts := strings.Fields(displayName)

	switch len(parts) {
	case 0:
		return "", ""
	case 1:
		// Only one part - could be first name or a single name
		// Use as first name
		return parts[0], ""
	case 2:
		// Two parts - likely first and last name
		return parts[0], parts[1]
	default:
		// Multiple parts - use first as first name, join rest as last name
		return parts[0], strings.Join(parts[1:], " ")
	}
}

// extractMatrixMessageContent extracts content from Matrix event, preferring formatted_body over body
func (p *Plugin) extractMatrixMessageContent(event MatrixEvent) string {
	// Check if there's HTML formatted content
	if format, hasFormat := event.Content["format"].(string); hasFormat && format == "org.matrix.custom.html" {
		if formattedBody, hasFormatted := event.Content["formatted_body"].(string); hasFormatted && formattedBody != "" {
			// Convert HTML to Markdown with mention processing
			return p.convertHTMLToMarkdownWithMentions(formattedBody, event)
		}
	}

	// Fall back to plain text body
	if body, hasBody := event.Content["body"].(string); hasBody {
		return body
	}

	return ""
}

// extractMattermostMetadata extracts Mattermost metadata from Matrix event content
func (p *Plugin) extractMattermostMetadata(event MatrixEvent) (postID string, remoteID string) {
	if mattermostPostID, exists := event.Content["mattermost_post_id"].(string); exists {
		postID = mattermostPostID
	}
	if mattermostRemoteID, exists := event.Content["mattermost_remote_id"].(string); exists {
		remoteID = mattermostRemoteID
	}
	return postID, remoteID
}

// convertHTMLToMarkdown converts Matrix HTML content to Mattermost-compatible markdown
func convertHTMLToMarkdown(logger Logger, htmlContent string) string {
	converter := md.NewConverter("", true, &md.Options{
		StrongDelimiter:  "**",     // Use ** for bold (Mattermost standard)
		EmDelimiter:      "*",      // Use * for italic
		CodeBlockStyle:   "fenced", // Use ``` code blocks
		HeadingStyle:     "atx",    // Use # headers
		HorizontalRule:   "---",    // Use --- for hr
		BulletListMarker: "-",      // Use - for bullets
	})

	markdown, err := converter.ConvertString(htmlContent)
	if err != nil {
		logger.LogWarn("Failed to convert HTML to markdown", "error", err, "html", htmlContent)
		// Return original content if conversion fails
		return htmlContent
	}

	return cleanupMarkdown(markdown)
}

// processMatrixMentions processes Matrix mentions in HTML content and converts them to Mattermost @mentions
func (p *Plugin) processMatrixMentions(htmlContent string, event MatrixEvent) string {
	// Get mentioned users from m.mentions field
	mentionedUsers := p.extractMentionedUsers(event)
	if len(mentionedUsers) == 0 {
		return htmlContent
	}

	// Process HTML content to replace mention links with @mentions
	processed := htmlContent
	for _, matrixUserID := range mentionedUsers {
		// Look up Mattermost username for this Matrix user
		mattermostUsername := p.getMattermostUsernameFromMatrix(matrixUserID)
		if mattermostUsername != "" {
			// Replace HTML mention links for this user
			processed = p.replaceMatrixMentionHTML(processed, matrixUserID, mattermostUsername)
		}
	}

	return processed
}

// extractMentionedUsers extracts Matrix user IDs from the m.mentions field
func (p *Plugin) extractMentionedUsers(event MatrixEvent) []string {
	mentionsField, hasMentions := event.Content["m.mentions"]
	if !hasMentions {
		return nil
	}

	mentions, ok := mentionsField.(map[string]any)
	if !ok {
		p.API.LogDebug("m.mentions field is not a map", "event_id", event.EventID)
		return nil
	}

	// Get user_ids array from mentions
	userIDsField, hasUserIDs := mentions["user_ids"]
	if !hasUserIDs {
		return nil
	}

	userIDsArray, ok := userIDsField.([]any)
	if !ok {
		p.API.LogDebug("user_ids field is not an array", "event_id", event.EventID)
		return nil
	}

	// Convert to string array
	var userIDs []string
	for _, userIDInterface := range userIDsArray {
		if userID, ok := userIDInterface.(string); ok {
			userIDs = append(userIDs, userID)
		}
	}

	p.API.LogDebug("Extracted mentioned users from Matrix event", "event_id", event.EventID, "user_ids", userIDs)
	return userIDs
}

// getMattermostUsernameFromMatrix looks up the Mattermost username for a Matrix user ID
func (p *Plugin) getMattermostUsernameFromMatrix(matrixUserID string) string {
	var mattermostUserID string

	// Check if this is a ghost user (Mattermost user represented in Matrix)
	if ghostMattermostUserID := p.extractMattermostUserIDFromGhost(matrixUserID); ghostMattermostUserID != "" {
		p.API.LogDebug("Found ghost user for mention", "matrix_user_id", matrixUserID, "mattermost_user_id", ghostMattermostUserID)
		mattermostUserID = ghostMattermostUserID
	} else {
		// Check if we have a mapping for this regular Matrix user
		userMapKey := "matrix_user_" + matrixUserID
		userIDBytes, err := p.kvstore.Get(userMapKey)
		if err != nil || len(userIDBytes) == 0 {
			p.API.LogDebug("No Mattermost user found for Matrix mention", "matrix_user_id", matrixUserID)
			return ""
		}
		mattermostUserID = string(userIDBytes)
	}

	// Get the Mattermost user to retrieve username
	user, appErr := p.API.GetUser(mattermostUserID)
	if appErr != nil {
		p.API.LogWarn("Failed to get Mattermost user for mention", "error", appErr, "user_id", mattermostUserID, "matrix_user_id", matrixUserID)
		return ""
	}

	p.API.LogDebug("Found Mattermost username for Matrix mention", "matrix_user_id", matrixUserID, "mattermost_username", user.Username)
	return user.Username
}

// extractMattermostUserIDFromGhost extracts the Mattermost user ID from a Matrix ghost user ID
// Ghost users follow the pattern: @_mattermost_<mattermost_user_id>:<server_domain>
func (p *Plugin) extractMattermostUserIDFromGhost(ghostUserID string) string {
	const ghostUserPrefix = "@_mattermost_"

	// Check if this looks like a ghost user
	if !strings.HasPrefix(ghostUserID, ghostUserPrefix) {
		return ""
	}

	// Extract the part after the prefix and before the server domain
	withoutPrefix := ghostUserID[len(ghostUserPrefix):]

	// Find the colon that separates user ID from server domain
	colonIndex := strings.Index(withoutPrefix, ":")
	if colonIndex == -1 {
		return ""
	}

	// Extract the Mattermost user ID
	mattermostUserID := withoutPrefix[:colonIndex]

	if mattermostUserID == "" {
		return ""
	}

	p.API.LogDebug("Extracted Mattermost user ID from ghost user", "ghost_user_id", ghostUserID, "mattermost_user_id", mattermostUserID)
	return mattermostUserID
}

// replaceMatrixMentionHTML replaces Matrix mention HTML links with Mattermost @mentions
func (p *Plugin) replaceMatrixMentionHTML(htmlContent, matrixUserID, mattermostUsername string) string {
	// Matrix mention links typically look like:
	// <a href="https://matrix.to/#/@user:server.com">Display Name</a>
	// We want to replace these with @username

	// Create pattern to match Matrix mention links for this specific user
	// Pattern matches: <a href="https://matrix.to/#/USERID">any text</a>
	escapedUserID := regexp.QuoteMeta(matrixUserID)
	pattern := fmt.Sprintf(`<a\s+href=["']https://matrix\.to/#/%s["'][^>]*>([^<]+)</a>`, escapedUserID)

	regex, err := regexp.Compile(pattern)
	if err != nil {
		p.API.LogWarn("Failed to compile mention regex", "error", err, "pattern", pattern)
		return htmlContent
	}

	// Replace with @username
	replacement := "@" + mattermostUsername
	result := regex.ReplaceAllString(htmlContent, replacement)

	p.API.LogDebug("Replaced Matrix mention HTML", "matrix_user_id", matrixUserID, "mattermost_username", mattermostUsername, "original", htmlContent, "result", result)
	return result
}

// convertHTMLToMarkdownWithMentions converts Matrix HTML to Mattermost markdown with mention processing
func (p *Plugin) convertHTMLToMarkdownWithMentions(htmlContent string, event MatrixEvent) string {
	// First, process Matrix mentions and convert HTML mention links to Mattermost @mentions
	processedHTML := p.processMatrixMentions(htmlContent, event)

	// Then convert the processed HTML to markdown
	return convertHTMLToMarkdown(p.API, processedHTML)
}

// cleanupMarkdown cleans up conversion artifacts from HTML-to-markdown conversion
func cleanupMarkdown(markdown string) string {
	// Remove excessive newlines
	cleaned := regexp.MustCompile(`\n{3,}`).ReplaceAllString(markdown, "\n\n")

	// Trim leading/trailing whitespace
	cleaned = strings.TrimSpace(cleaned)

	return cleaned
}
