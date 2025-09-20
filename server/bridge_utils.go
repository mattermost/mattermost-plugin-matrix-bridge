package main

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/matrix"
	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
	"github.com/pkg/errors"
)

var (
	// Compiled regex patterns for HTML detection
	// htmlTagRegex matches HTML tags with proper attribute validation:
	// - <tag>, <tag attr="value">, <tag attr="value" attr2="value2">, </tag>, <tag/>
	// - Allows attributes with optional quoted values
	// - Rejects invalid attribute names (must start with letter, can contain letters/hyphens)
	// - Does not validate tag names or attribute values beyond basic syntax
	htmlTagRegex = regexp.MustCompile(`</?[a-zA-Z][a-zA-Z0-9]*(?:\s+[a-zA-Z-]+(?:="[^"]*")?)*\s*/?>`)

	// htmlEntityRegex matches HTML entities like &amp;, &lt;, &#39;, etc.
	htmlEntityRegex = regexp.MustCompile(`&[a-zA-Z0-9#]+;`)
)

// ConfigurationGetter interface for getting plugin configuration
type ConfigurationGetter interface {
	getConfiguration() *configuration
}

// BridgeUtilsConfig contains all dependencies needed for BridgeUtils
type BridgeUtilsConfig struct {
	Logger              Logger
	API                 plugin.API
	KVStore             kvstore.KVStore
	MatrixClient        *matrix.Client
	RemoteID            string
	MaxProfileImageSize int64
	MaxFileSize         int64
	ConfigGetter        ConfigurationGetter
}

// BridgeUtils contains common utilities used by both bridge types
type BridgeUtils struct {
	logger              Logger
	API                 plugin.API
	kvstore             kvstore.KVStore
	matrixClient        *matrix.Client
	remoteID            string
	maxProfileImageSize int64
	maxFileSize         int64
	configGetter        ConfigurationGetter
}

// NewBridgeUtils creates a new BridgeUtils instance
func NewBridgeUtils(config BridgeUtilsConfig) *BridgeUtils {
	return &BridgeUtils{
		logger:              config.Logger,
		API:                 config.API,
		kvstore:             config.KVStore,
		matrixClient:        config.MatrixClient,
		remoteID:            config.RemoteID,
		maxProfileImageSize: config.MaxProfileImageSize,
		maxFileSize:         config.MaxFileSize,
		configGetter:        config.ConfigGetter,
	}
}

// Shared utility methods that both bridge types need

// GetMatrixRoomID retrieves the Matrix room ID for a given Mattermost channel ID
func (s *BridgeUtils) GetMatrixRoomID(channelID string) (string, error) {
	roomID, err := s.kvstore.Get(kvstore.BuildChannelMappingKey(channelID))
	if err != nil {
		// KV store error (typically key not found) - unmapped channels are expected
		return "", nil
	}
	return string(roomID), nil
}

func (s *BridgeUtils) setChannelRoomMapping(channelID, matrixRoomIdentifier string) error {
	// Always resolve to room ID for consistent forward mapping storage
	var roomID string
	var err error

	// Resolve room identifier to room ID (handles both aliases and room IDs)
	roomID, err = s.matrixClient.ResolveRoomAlias(matrixRoomIdentifier)
	if err != nil {
		s.logger.LogWarn("Failed to resolve room identifier during mapping creation", "room_identifier", matrixRoomIdentifier, "error", err)
		// Fallback: store the original identifier (better than failing completely)
		roomID = matrixRoomIdentifier
	}

	// Store forward mapping: channel_mapping_<channelID> -> room_id (always room ID)
	err = s.kvstore.Set(kvstore.BuildChannelMappingKey(channelID), []byte(roomID))
	if err != nil {
		return errors.Wrap(err, "failed to store channel room mapping")
	}

	// Store reverse mapping for the room ID
	err = s.kvstore.Set(kvstore.BuildRoomMappingKey(roomID), []byte(channelID))
	if err != nil {
		return errors.Wrap(err, "failed to store reverse room mapping")
	}

	// If we started with an alias, also create reverse mapping for the alias
	// This allows lookups by both alias and room ID
	if strings.HasPrefix(matrixRoomIdentifier, "#") && roomID != matrixRoomIdentifier {
		err = s.kvstore.Set(kvstore.BuildRoomMappingKey(matrixRoomIdentifier), []byte(channelID))
		if err != nil {
			s.logger.LogWarn("Failed to create alias reverse mapping", "channel_id", channelID, "room_alias", matrixRoomIdentifier, "error", err)
		} else {
			s.logger.LogDebug("Created reverse mappings for alias", "channel_id", channelID, "room_alias", matrixRoomIdentifier, "room_id", roomID)
		}
	}

	return nil
}

func (s *BridgeUtils) getConfiguration() *configuration {
	return s.configGetter.getConfiguration()
}

func (s *BridgeUtils) extractMattermostMetadata(event MatrixEvent) (postID string, remoteID string) {
	if event.Content != nil {
		if id, ok := event.Content["mattermost_post_id"].(string); ok {
			postID = id
		}
		if id, ok := event.Content["mattermost_remote_id"].(string); ok {
			remoteID = id
		}
	}
	return postID, remoteID
}

// isHTML checks if content contains HTML tags or entities
func isHTML(content string) bool {
	// Check for HTML tags using pre-compiled regex
	if htmlTagRegex.MatchString(content) {
		return true
	}

	// Check for HTML entities using pre-compiled regex
	return htmlEntityRegex.MatchString(content)
}

// isHTMLContent checks if content should be treated as HTML based on Matrix format field or content analysis
func (s *BridgeUtils) isHTMLContent(content string, event MatrixEvent) bool {
	// Check Matrix format field first (most reliable)
	if format, ok := event.Content["format"].(string); ok {
		return format == "org.matrix.custom.html"
	}
	// Fall back to content analysis
	return isHTML(content)
}

func (s *BridgeUtils) extractMatrixMessageContent(event MatrixEvent) string {
	if event.Content == nil {
		return ""
	}

	var content string

	// For edit events, extract content from m.new_content instead of top-level body/formatted_body
	if relatesTo, ok := event.Content["m.relates_to"].(map[string]any); ok {
		if relType, ok := relatesTo["rel_type"].(string); ok && relType == "m.replace" {
			// This is an edit event - get content from m.new_content
			if newContent, ok := event.Content["m.new_content"].(map[string]any); ok {
				// Extract from m.new_content using same logic
				if body, ok := newContent["body"].(string); ok {
					content = body
				}

				if formattedBody, ok := newContent["formatted_body"].(string); ok {
					// Only use formatted_body if it's different from body (indicating actual formatting)
					if formattedBody != content {
						content = formattedBody
					}
				}

				// Create a temporary event for HTML detection with the new_content. Shallow copy the entire event to preserve metadata (m.mentions, etc.)
				tempEvent := event
				tempEvent.Content = newContent
				if s.isHTMLContent(content, tempEvent) {
					content = s.convertHTMLToMarkdownWithMentions(content, tempEvent)
				}

				return content
			}
		}
	}

	// For non-edit events, use the existing logic
	// Start with body as the default content
	if body, ok := event.Content["body"].(string); ok {
		content = body
	}

	// Prefer formatted_body if available and different from body
	if formattedBody, ok := event.Content["formatted_body"].(string); ok {
		// Only use formatted_body if it's different from body (indicating actual formatting)
		if formattedBody != content {
			content = formattedBody
		}
	}

	// Convert HTML to Markdown with mention processing if needed
	if s.isHTMLContent(content, event) {
		content = s.convertHTMLToMarkdownWithMentions(content, event)
	}

	return content
}

// processMatrixMentions processes Matrix mentions in HTML content and converts them to Mattermost @mentions
func (s *BridgeUtils) processMatrixMentions(htmlContent string, event MatrixEvent) string {
	// Get mentioned users from m.mentions field
	mentionedUsers := s.extractMentionedUsers(event)
	if len(mentionedUsers) == 0 {
		return htmlContent
	}

	// Process HTML content to replace mention links with @mentions
	processed := htmlContent
	for _, matrixUserID := range mentionedUsers {
		// Look up Mattermost username for this Matrix user
		mattermostUsername := s.getMattermostUsernameFromMatrix(matrixUserID)
		if mattermostUsername != "" {
			// Replace HTML mention links for this user
			processed = s.replaceMatrixMentionHTML(processed, matrixUserID, mattermostUsername)
		}
	}

	return processed
}

// extractMentionedUsers extracts Matrix user IDs from the m.mentions field
func (s *BridgeUtils) extractMentionedUsers(event MatrixEvent) []string {
	mentionsField, hasMentions := event.Content["m.mentions"]
	if !hasMentions {
		return nil
	}

	mentions, ok := mentionsField.(map[string]any)
	if !ok {
		s.logger.LogDebug("m.mentions field is not a map", "event_id", event.EventID)
		return nil
	}

	// Get user_ids array from mentions
	userIDsField, hasUserIDs := mentions["user_ids"]
	if !hasUserIDs {
		return nil
	}

	userIDsArray, ok := userIDsField.([]any)
	if !ok {
		s.logger.LogDebug("user_ids field is not an array", "event_id", event.EventID)
		return nil
	}

	// Convert to string array
	var userIDs []string
	for _, userIDInterface := range userIDsArray {
		if userID, ok := userIDInterface.(string); ok {
			userIDs = append(userIDs, userID)
		}
	}

	s.logger.LogDebug("Extracted mentioned users from Matrix event", "event_id", event.EventID, "user_ids", userIDs)
	return userIDs
}

// getMattermostUsernameFromMatrix looks up the Mattermost username for a Matrix user ID
func (s *BridgeUtils) getMattermostUsernameFromMatrix(matrixUserID string) string {
	var mattermostUserID string

	// Check if this is a ghost user (Mattermost user represented in Matrix)
	if ghostMattermostUserID := s.extractMattermostUserIDFromGhost(matrixUserID); ghostMattermostUserID != "" {
		s.logger.LogDebug("Found ghost user for mention", "matrix_user_id", matrixUserID, "mattermost_user_id", ghostMattermostUserID)
		mattermostUserID = ghostMattermostUserID
	} else {
		// Check if we have a mapping for this regular Matrix user
		userMapKey := "matrix_user_" + matrixUserID
		userIDBytes, err := s.kvstore.Get(userMapKey)
		if err != nil || len(userIDBytes) == 0 {
			s.logger.LogDebug("No Mattermost user found for Matrix mention", "matrix_user_id", matrixUserID)
			return ""
		}
		mattermostUserID = string(userIDBytes)
	}

	// Get the Mattermost user to retrieve username
	user, appErr := s.API.GetUser(mattermostUserID)
	if appErr != nil {
		s.logger.LogWarn("Failed to get Mattermost user for mention", "error", appErr, "user_id", mattermostUserID, "matrix_user_id", matrixUserID)
		return ""
	}

	s.logger.LogDebug("Found Mattermost username for Matrix mention", "matrix_user_id", matrixUserID, "mattermost_username", user.Username)
	return user.Username
}

// extractMattermostUserIDFromGhost extracts the Mattermost user ID from a Matrix ghost user ID
// Ghost users follow the pattern: @_mattermost_<mattermost_user_id>:<server_domain>
func (s *BridgeUtils) extractMattermostUserIDFromGhost(ghostUserID string) string {
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

	s.logger.LogDebug("Extracted Mattermost user ID from ghost user", "ghost_user_id", ghostUserID, "mattermost_user_id", mattermostUserID)
	return mattermostUserID
}

// replaceMatrixMentionHTML replaces Matrix mention HTML links with Mattermost @mentions
func (s *BridgeUtils) replaceMatrixMentionHTML(htmlContent, matrixUserID, mattermostUsername string) string {
	// Matrix mention links typically look like:
	// <a href="https://matrix.to/#/@user:server.com">Display Name</a>
	// We want to replace these with @username

	// Create pattern to match Matrix mention links for this specific user
	// Pattern matches: <a href="https://matrix.to/#/USERID">any text</a>
	escapedUserID := regexp.QuoteMeta(matrixUserID)
	pattern := fmt.Sprintf(`<a\s+href=["']https://matrix\.to/#/%s["'][^>]*>([^<]+)</a>`, escapedUserID)

	regex, err := regexp.Compile(pattern)
	if err != nil {
		s.logger.LogWarn("Failed to compile mention regex", "error", err, "pattern", pattern)
		return htmlContent
	}

	// Replace with @username
	replacement := "@" + mattermostUsername
	result := regex.ReplaceAllString(htmlContent, replacement)

	s.logger.LogDebug("Replaced Matrix mention HTML", "matrix_user_id", matrixUserID, "mattermost_username", mattermostUsername, "original", htmlContent, "result", result)
	return result
}

// convertHTMLToMarkdownWithMentions converts Matrix HTML to Mattermost markdown with mention processing
func (s *BridgeUtils) convertHTMLToMarkdownWithMentions(htmlContent string, event MatrixEvent) string {
	// First, process Matrix mentions and convert HTML mention links to Mattermost @mentions
	processedHTML := s.processMatrixMentions(htmlContent, event)

	// Then convert the processed HTML to markdown
	return convertHTMLToMarkdown(s.logger, processedHTML)
}

func (s *BridgeUtils) downloadMatrixFile(mxcURL string) ([]byte, error) {
	data, err := s.matrixClient.DownloadFile(mxcURL, s.maxFileSize, "")
	if err != nil {
		return nil, errors.Wrap(err, "failed to download Matrix media")
	}
	return data, nil
}

func (s *BridgeUtils) isGhostUser(matrixUserID string) bool {
	// Ghost users follow the pattern: @_mattermost_<user_id>:<server_domain>
	return strings.HasPrefix(matrixUserID, "@_mattermost_")
}

// DM channel detection and handling utilities

func (s *BridgeUtils) isDirectChannel(channelID string) (bool, []string, error) {
	channel, appErr := s.API.GetChannel(channelID)
	if appErr != nil {
		return false, nil, errors.Wrap(appErr, "failed to get channel")
	}

	if channel.Type == model.ChannelTypeDirect {
		// Get the two users in the DM
		members, appErr := s.API.GetChannelMembers(channelID, 0, 10)
		if appErr != nil {
			return false, nil, errors.Wrap(appErr, "failed to get channel members")
		}

		userIDs := make([]string, len(members))
		for i, member := range members {
			userIDs[i] = member.UserId
		}
		return true, userIDs, nil
	}

	if channel.Type == model.ChannelTypeGroup {
		// Handle group DMs - get all members with pagination to handle large groups
		var allMembers []model.ChannelMember
		offset := 0
		limit := 100

		for {
			pageMembers, appErr := s.API.GetChannelMembers(channelID, offset, limit)
			if appErr != nil {
				return false, nil, errors.Wrap(appErr, "failed to get group channel members")
			}
			if len(pageMembers) == 0 {
				break
			}
			allMembers = append(allMembers, pageMembers...)
			offset += limit
		}

		userIDs := make([]string, len(allMembers))
		for i, member := range allMembers {
			userIDs[i] = member.UserId
		}
		return true, userIDs, nil
	}

	return false, nil, nil
}

// reconstructMatrixUserIDFromUsername reconstructs a Matrix user ID from a Mattermost username
// This handles cases where Matrix users exist in channels but don't have KV mappings yet
func (s *BridgeUtils) reconstructMatrixUserIDFromUsername(mattermostUsername string) string {
	// Mattermost usernames for Matrix users follow the pattern: "prefix:username"
	// We need to reverse this to get "@username:server.com"

	config := s.configGetter.getConfiguration()
	prefix := config.GetMatrixUsernamePrefixForServer(config.GetMatrixServerURL())

	// Check if username has the expected prefix
	expectedPrefix := prefix + ":"
	if !strings.HasPrefix(mattermostUsername, expectedPrefix) {
		return "" // Not a Matrix-originated user
	}

	// Extract the original Matrix username
	matrixUsername := strings.TrimPrefix(mattermostUsername, expectedPrefix)
	if matrixUsername == "" {
		return "" // Empty username
	}

	// Extract server domain from Matrix server URL
	serverURL := config.GetMatrixServerURL()
	serverDomain := strings.TrimPrefix(serverURL, "https://")
	serverDomain = strings.TrimPrefix(serverDomain, "http://")

	// Remove any path components (e.g., "server.com:8008/_matrix" -> "server.com:8008")
	if idx := strings.Index(serverDomain, "/"); idx != -1 {
		serverDomain = serverDomain[:idx]
	}

	// Reconstruct the full Matrix user ID
	return "@" + matrixUsername + ":" + serverDomain
}
