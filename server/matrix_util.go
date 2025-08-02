package main

import (
	"net/url"
	"regexp"
	"strings"

	md "github.com/JohannesKaufmann/html-to-markdown"
	"github.com/pkg/errors"
)

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
		p.logger.LogWarn("No matching file message found to delete", "filename", filename, "post_event_id", postEventID)
		return nil
	}

	// Redact the file message
	_, err = p.matrixClient.RedactEventAsGhost(matrixRoomID, fileEventID, ghostUserID)
	if err != nil {
		return errors.Wrap(err, "failed to redact file message in Matrix")
	}

	p.logger.LogDebug("Successfully deleted file message from Matrix", "filename", filename, "file_event_id", fileEventID, "post_event_id", postEventID)
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

// cleanupMarkdown cleans up conversion artifacts from HTML-to-markdown conversion
func cleanupMarkdown(markdown string) string {
	// Remove excessive newlines
	cleaned := regexp.MustCompile(`\n{3,}`).ReplaceAllString(markdown, "\n\n")

	// Trim leading/trailing whitespace
	cleaned = strings.TrimSpace(cleaned)

	return cleaned
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

	p.logger.LogDebug("Extracted Mattermost user ID from ghost user", "ghost_user_id", ghostUserID, "mattermost_user_id", mattermostUserID)
	return mattermostUserID
}
