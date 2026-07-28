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
	ServerID            string
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
	serverID            string
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
		serverID:            config.ServerID,
		remoteID:            config.RemoteID,
		maxProfileImageSize: config.MaxProfileImageSize,
		maxFileSize:         config.MaxFileSize,
		configGetter:        config.ConfigGetter,
	}
}

// Shared utility methods that both bridge types need

// GetMatrixRoomID retrieves the Matrix room ID for a given Mattermost channel ID.
// An unmapped channel yields ("", nil): the plugin KV API returns no error for a
// missing key, so the value is simply empty. A real KV read failure or a corrupt
// (unparseable) value is returned as an error rather than being masked as
// unmapped, which would silently mis-route or drop messages.
func (s *BridgeUtils) GetMatrixRoomID(channelID string) (string, error) {
	data, err := s.kvstore.Get(kvstore.BuildChannelMappingKey(channelID))
	if err != nil {
		return "", errors.Wrap(err, "failed to read channel mapping")
	}
	mappings, err := kvstore.ParseChannelServerMappings(data)
	if err != nil {
		// After the v3 migration every stored value is well-formed JSON, so a
		// parse failure here is a corrupt record, not an unmapped channel.
		return "", errors.Wrapf(err, "corrupt channel mapping value for channel %s", channelID)
	}
	return kvstore.RoomIDForServer(mappings, s.serverID), nil
}

func (s *BridgeUtils) setChannelRoomMapping(channelID, matrixRoomIdentifier string) error {
	// Guard against persisting a mapping with an empty serverID, which would be
	// unroutable and would corrupt the room_mapping_ reverse key. This should not
	// happen once the registry is reconciled, but fail loudly rather than write it.
	if s.serverID == "" {
		return errors.New("cannot store channel mapping: server ID not initialized")
	}

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

	// Store forward mapping: channel_mapping_<channelID> -> [{serverID, room_id}].
	// Upsert only this server's entry so a channel bridged to multiple homeservers
	// keeps the others' mappings intact. Use compare-and-set with retries because
	// the value is shared across servers: two inbound events racing on the same
	// channel key would otherwise read-modify-write over each other and drop an
	// entry.
	err = s.kvstore.SetAtomicWithRetries(kvstore.BuildChannelMappingKey(channelID), func(oldValue []byte) ([]byte, error) {
		mappings, err := kvstore.ParseChannelServerMappings(oldValue)
		if err != nil {
			return nil, errors.Wrap(err, "failed to parse existing channel room mapping")
		}
		mappings = kvstore.UpsertChannelServerMapping(mappings, s.serverID, roomID)
		return kvstore.MarshalChannelServerMappings(mappings)
	})
	if err != nil {
		return errors.Wrap(err, "failed to store channel room mapping")
	}

	// Store reverse mapping for the room ID
	err = s.kvstore.Set(kvstore.BuildRoomMappingKey(s.serverID, roomID), []byte(channelID))
	if err != nil {
		return errors.Wrap(err, "failed to store reverse room mapping")
	}

	// If we started with an alias, also create reverse mapping for the alias
	// This allows lookups by both alias and room ID
	if strings.HasPrefix(matrixRoomIdentifier, "#") && roomID != matrixRoomIdentifier {
		err = s.kvstore.Set(kvstore.BuildRoomMappingKey(s.serverID, matrixRoomIdentifier), []byte(channelID))
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

// matrixUsernamePrefix returns the username prefix for this bridge's Matrix
// server, resolved from the managed server registry, which is the source of
// truth for per-server settings. reconcileServerConfig always populates each
// entry's prefix (defaulting to DefaultMatrixUsernamePrefix) before bridges run,
// so a configured server always resolves here. The static default is only for
// the degenerate case of no registered server (e.g. before the first reconcile).
// The registry is read live so prefix changes take effect without recreating the
// bridge.
func (s *BridgeUtils) matrixUsernamePrefix() string {
	data, err := s.kvstore.Get(kvstore.KeyServersConfig)
	if err != nil {
		// A transient registry read failure must not silently change the prefix:
		// a wrong prefix splits Matrix-user identity (ghosts created and matched
		// under a different prefix). Surface it loudly, mirroring GetMatrixRoomID.
		s.logger.LogError("Failed to read server registry for username prefix; using default", "server_id", s.serverID, "error", err)
		return DefaultMatrixUsernamePrefix
	}
	servers, err := kvstore.ParseServersConfig(data)
	if err != nil {
		s.logger.LogError("Corrupt server registry; using default username prefix", "server_id", s.serverID, "error", err)
		return DefaultMatrixUsernamePrefix
	}
	if server, ok := kvstore.ServerConfigForID(servers, s.serverID); ok && server.UsernamePrefix != "" {
		return server.UsernamePrefix
	}
	// No registry yet (before the first reconcile) or no entry for this server.
	return DefaultMatrixUsernamePrefix
}

// serverDomain returns the property-key-sanitized domain of this bridge's Matrix
// server, resolved from the managed server registry (which holds per-server
// settings). It is used to build the per-server post property key
// "matrix_event_id_<domain>".
//
// The domain is derived from the homeserver's Matrix server name when set (the
// canonical per-homeserver identity, matching ghost user and room domains), and
// only falls back to the connection URL's host otherwise. Using the server name
// keeps the key unique across homeservers even when several share a connection
// host but differ by port (e.g. localhost:8008 vs localhost:8009), where the
// URL-host-derived key would collide and cross-wire their event IDs. The domain
// is resolved from this bridge's registry entry; returns "" if the entry is
// missing or unreadable.
func (s *BridgeUtils) serverDomain() string {
	data, err := s.kvstore.Get(kvstore.KeyServersConfig)
	if err != nil {
		s.logger.LogError("Failed to read server registry for server domain", "server_id", s.serverID, "error", err)
		return ""
	}
	servers, err := kvstore.ParseServersConfig(data)
	if err != nil {
		s.logger.LogError("Corrupt server registry while resolving server domain", "server_id", s.serverID, "error", err)
		return ""
	}
	if server, ok := kvstore.ServerConfigForID(servers, s.serverID); ok {
		if server.ServerName != "" {
			return sanitizeDomainForPropertyKey(server.ServerName)
		}
		if server.ServerURL != "" {
			return extractServerDomain(s.logger, server.ServerURL)
		}
	}
	s.logger.LogError("No registry entry for server while resolving server domain", "server_id", s.serverID)
	return ""
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
	return detectDirectChannel(s.API, channelID)
}

// detectDirectChannel reports whether channelID is a direct or group DM and, if
// so, the IDs of its members. It is server-agnostic — it inspects only the
// Mattermost channel — so it lives as a free function usable without a Matrix
// server-bound bridge (see Plugin.isDirectChannel and BridgeUtils.isDirectChannel).
func detectDirectChannel(api plugin.API, channelID string) (bool, []string, error) {
	channel, appErr := api.GetChannel(channelID)
	if appErr != nil {
		return false, nil, errors.Wrap(appErr, "failed to get channel")
	}

	if channel.Type == model.ChannelTypeDirect {
		// Get the two users in the DM
		members, appErr := api.GetChannelMembers(channelID, 0, 10)
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
			pageMembers, appErr := api.GetChannelMembers(channelID, offset, limit)
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

	prefix := s.matrixUsernamePrefix()

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

	// Resolve the URL/name from this bridge's registry entry (the sole source of
	// truth) so a reconstruction on server B appends B's domain.
	var serverURL, configuredServerName string
	if data, regErr := s.kvstore.Get(kvstore.KeyServersConfig); regErr == nil {
		if servers, parseErr := kvstore.ParseServersConfig(data); parseErr == nil {
			if sc, ok := kvstore.ServerConfigForID(servers, s.serverID); ok {
				serverURL = sc.ServerURL
				configuredServerName = sc.ServerName
			}
		}
	}
	if serverURL == "" {
		s.logger.LogWarn("No registry entry for server; cannot reconstruct Matrix user ID",
			"server_id", s.serverID, "mattermost_username", mattermostUsername)
		return ""
	}

	logger := matrix.NewAPILogger(s.API)
	discovery := matrix.NewServerDiscovery(logger)
	serverName, err := discovery.DiscoverServerName(serverURL, configuredServerName)
	if err != nil {
		s.logger.LogWarn("Failed to discover server name; cannot reconstruct Matrix user ID",
			"error", err,
			"server_url", serverURL,
			"mattermost_username", mattermostUsername)
		return ""
	}

	if serverName == "" {
		s.logger.LogWarn("Empty server name after discovery; cannot reconstruct Matrix user ID",
			"server_url", serverURL,
			"mattermost_username", mattermostUsername)
		return ""
	}

	// Reconstruct the full Matrix user ID
	return "@" + matrixUsername + ":" + serverName
}
