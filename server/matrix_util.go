package main

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	md "github.com/JohannesKaufmann/html-to-markdown"
	"github.com/pkg/errors"
)

// Logger interface for logging operations
type Logger interface {
	LogDebug(message string, keyValuePairs ...any)
	LogInfo(message string, keyValuePairs ...any)
	LogWarn(message string, keyValuePairs ...any)
	LogError(message string, keyValuePairs ...any)
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

// downloadMatrixFile downloads a file from Matrix using the MXC URI
func (b *MatrixToMattermostBridge) downloadMatrixFile(mxcURL string) ([]byte, error) {
	if b.matrixClient == nil {
		return nil, errors.New("Matrix client not configured")
	}

	// Delegate to the Matrix client's download method with plugin's max file size
	return b.matrixClient.DownloadFile(mxcURL, b.maxFileSize, "")
}
