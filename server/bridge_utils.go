package main

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
	"github.com/pkg/errors"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/matrix"
	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
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

// BridgeUtilsConfig contains all dependencies needed for BridgeUtils
type BridgeUtilsConfig struct {
	Logger              Logger
	API                 plugin.API
	KVStore             kvstore.KVStore
	MatrixClient        *matrix.Client
	RemoteID            string
	ServerID            string
	MaxProfileImageSize int64
	MaxFileSize         int64
	// ChannelMapper is the one choke point for channel<->room mapping writes (see
	// channel_mapping.go). Always the Plugin itself in production.
	ChannelMapper ChannelMapper
}

// BridgeUtils contains common utilities used by both bridge types. There is one
// instance per (bridge direction, server) pair, built on demand - see
// Plugin.bridgeUtilsForServer.
type BridgeUtils struct {
	logger              Logger
	API                 plugin.API
	kvstore             kvstore.KVStore
	matrixClient        *matrix.Client
	remoteID            string
	serverID            string
	maxProfileImageSize int64
	maxFileSize         int64
	channelMapper       ChannelMapper
}

// NewBridgeUtils creates a new BridgeUtils instance
func NewBridgeUtils(config BridgeUtilsConfig) *BridgeUtils {
	return &BridgeUtils{
		logger:              config.Logger,
		API:                 config.API,
		kvstore:             config.KVStore,
		matrixClient:        config.MatrixClient,
		remoteID:            config.RemoteID,
		serverID:            config.ServerID,
		maxProfileImageSize: config.MaxProfileImageSize,
		maxFileSize:         config.MaxFileSize,
		channelMapper:       config.ChannelMapper,
	}
}

// Shared utility methods that both bridge types need

// GetMatrixRoomID retrieves the Matrix room ID mapped to this bridge's server for a
// given Mattermost channel ID. Returns ("", nil) both when the channel is entirely
// unmapped and when it is mapped only to another server - that is not an error. A
// corrupt stored value is.
func (s *BridgeUtils) GetMatrixRoomID(channelID string) (string, error) {
	data, err := s.kvstore.Get(kvstore.BuildChannelMappingKey(channelID))
	if err != nil {
		// KV store error (typically key not found) - unmapped channels are expected
		return "", nil
	}

	mappings, err := kvstore.ParseChannelServerMappings(data)
	if err != nil {
		return "", errors.Wrap(err, "failed to parse channel server mapping")
	}

	return kvstore.RoomIDForServer(mappings, s.serverID), nil
}

// setChannelRoomMapping maps channelID to matrixRoomIdentifier on this bridge's server,
// going through the ChannelMapper choke point so the one-server-per-channel policy is
// enforced consistently with the /matrix map and /matrix server map command paths.
func (s *BridgeUtils) setChannelRoomMapping(channelID, matrixRoomIdentifier string) error {
	// Capture the previous room mapped to this server (if any) so its now-stale reverse
	// key can be cleaned up when this call re-maps the same server to a new room.
	previousRoomID, _ := s.GetMatrixRoomID(channelID)

	// Always resolve to room ID for consistent forward mapping storage
	roomID, err := s.matrixClient.ResolveRoomAlias(matrixRoomIdentifier)
	if err != nil {
		s.logger.LogWarn("Failed to resolve room identifier during mapping creation", "room_identifier", matrixRoomIdentifier, "error", err)
		// Fallback: store the original identifier (better than failing completely)
		roomID = matrixRoomIdentifier
	}

	if _, err := s.channelMapper.SetChannelMapping(channelID, s.serverID, roomID); err != nil {
		return errors.Wrap(err, "failed to store channel room mapping")
	}

	if previousRoomID != "" && previousRoomID != roomID {
		if err := s.kvstore.Delete(kvstore.BuildRoomMappingKey(s.serverID, previousRoomID)); err != nil {
			s.logger.LogWarn("Failed to delete stale reverse room mapping", "channel_id", channelID, "server_id", s.serverID, "old_room_id", previousRoomID, "error", err)
		}
	}

	// Store reverse mapping for the room ID
	if err := s.kvstore.Set(kvstore.BuildRoomMappingKey(s.serverID, roomID), []byte(channelID)); err != nil {
		return errors.Wrap(err, "failed to store reverse room mapping")
	}

	// If we started with an alias, also create reverse mapping for the alias
	// This allows lookups by both alias and room ID
	if strings.HasPrefix(matrixRoomIdentifier, "#") && roomID != matrixRoomIdentifier {
		if err := s.kvstore.Set(kvstore.BuildRoomMappingKey(s.serverID, matrixRoomIdentifier), []byte(channelID)); err != nil {
			s.logger.LogWarn("Failed to create alias reverse mapping", "channel_id", channelID, "room_alias", matrixRoomIdentifier, "error", err)
		} else {
			s.logger.LogDebug("Created reverse mappings for alias", "channel_id", channelID, "room_alias", matrixRoomIdentifier, "room_id", roomID)
		}
	}

	return nil
}

// serverConfig returns this bridge's server's current registry entry, read live from
// the KV store (never cached), so a runtime change to e.g. UsernamePrefix takes effect
// immediately.
func (s *BridgeUtils) serverConfig() (kvstore.ServerConfig, error) {
	data, err := s.kvstore.Get(kvstore.KeyServersConfig)
	if err != nil {
		return kvstore.ServerConfig{}, errors.Wrap(err, "failed to read servers config")
	}
	servers, err := kvstore.ParseServersConfig(data)
	if err != nil {
		return kvstore.ServerConfig{}, err
	}
	for _, sv := range servers {
		if sv.ServerID == s.serverID {
			return sv, nil
		}
	}
	return kvstore.ServerConfig{}, errors.Errorf("server %s is not registered", s.serverID)
}

// matrixUsernamePrefix returns this bridge's server's configured username prefix,
// falling back to DefaultMatrixUsernamePrefix if the server has none set. A non-nil
// error means the server config could not be read at all - callers should treat this
// as a failure rather than silently falling back, since a transient read error must
// never be conflated with "no prefix configured" (that fallback could collide with
// another server's distinct, correctly-configured prefix).
func (s *BridgeUtils) matrixUsernamePrefix() (string, error) {
	server, err := s.serverConfig()
	if err != nil {
		return "", err
	}
	if server.UsernamePrefix == "" {
		return DefaultMatrixUsernamePrefix, nil
	}
	return server.UsernamePrefix, nil
}

// serverDomain returns this bridge's server's ServerName (the domain used in this
// homeserver's Matrix IDs), or "" if the server can't be looked up.
func (s *BridgeUtils) serverDomain() string {
	server, err := s.serverConfig()
	if err != nil {
		s.logger.LogWarn("Failed to read server config for domain lookup", "server_id", s.serverID, "error", err)
		return ""
	}
	return server.ServerName
}

// eventDomain returns this bridge's server's EventDomain - the sanitized, immutable
// value that keys the matrix_event_id_<EventDomain> post property. Always read from the
// stored registry field, never recomputed: it may legitimately differ from a live
// derivation of ServerName/endpoint (see §3.6), and recomputing would make every
// previously-written property key unreachable.
func (s *BridgeUtils) eventDomain() string {
	server, err := s.serverConfig()
	if err != nil {
		s.logger.LogWarn("Failed to read server config for event domain lookup", "server_id", s.serverID, "error", err)
		return ""
	}
	return server.EventDomain
}

// matrixEventIDPropertyKey returns the post property key under which this bridge's
// server stores the Matrix event ID for a synced post.
func (s *BridgeUtils) matrixEventIDPropertyKey() string {
	return "matrix_event_id_" + s.eventDomain()
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
		userMapKey := kvstore.BuildMatrixUserKey(s.serverID, matrixUserID)
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
	mattermostUserID, _, ok := strings.Cut(withoutPrefix, ":")
	if !ok {
		return ""
	}

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
	prefix, err := s.matrixUsernamePrefix()
	if err != nil {
		s.logger.LogWarn("Failed to read server config for username prefix lookup", "server_id", s.serverID, "error", err)
		return ""
	}

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

	// ServerName is resolved once at server add and stored - never re-derived here, so
	// this stays correct even if the connection host changes under .well-known delegation.
	serverName := s.serverDomain()
	if serverName == "" {
		s.logger.LogWarn("Empty server name for this bridge's server; cannot reconstruct Matrix user ID",
			"server_id", s.serverID,
			"mattermost_username", mattermostUsername)
		return ""
	}

	// Reconstruct the full Matrix user ID
	return "@" + matrixUsername + ":" + serverName
}
