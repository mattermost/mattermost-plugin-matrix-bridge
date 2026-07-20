// Package command implements slash command handlers for the Matrix Bridge plugin.
package command

import (
	"fmt"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
	"github.com/mattermost/mattermost/server/public/pluginapi"
	"github.com/pkg/errors"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/matrix"
	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
)

// Configuration interface for accessing plugin configuration
type Configuration interface {
	GetMatrixServerURL() string
	GetMatrixServerName() string
}

// MigrationResult holds the results of a migration operation
type MigrationResult struct {
	UserMappingsCreated      int
	ChannelMappingsCreated   int
	RoomMappingsCreated      int
	DMMappingsCreated        int
	ReverseDMMappingsCreated int
}

// PluginAccessor defines the interface for plugin functionality needed by command handlers
type PluginAccessor interface {
	// Matrix client access
	GetMatrixClient() *matrix.Client

	// Storage access
	GetKVStore() kvstore.KVStore

	// Configuration access
	GetConfiguration() Configuration

	// Ghost user management
	CreateOrGetGhostUser(mattermostUserID string) (string, error)

	// Matrix user mapping access
	GetMatrixUserIDFromMattermostUser(mattermostUserID string) (string, error)

	// Mattermost API access
	GetPluginAPI() plugin.API
	GetPluginAPIClient() *pluginapi.Client

	// Shared channel access
	GetRemoteID() string

	// Server registry access
	GetServerID() string

	// Migration access
	RunKVStoreMigrations() error
	RunKVStoreMigrationsWithResults() (*MigrationResult, error)

	// Managed server registry access (local multi-server testing). The System
	// Console UI cannot yet manage more than one server, so these back the
	// `/matrix server` command that injects additional servers directly.
	GetManagedServers() ([]kvstore.ServerConfig, error)
	AddManagedServer(serverURL, serverName, asToken, hsToken, usernamePrefix string) (string, error)
	RemoveManagedServer(serverID string) (bool, error)
	GetMatrixClientForServer(serverID string) *matrix.Client
}

// sanitizeShareName creates a valid ShareName matching the regex: ^[a-z0-9]+([a-z\-\_0-9]+|(__)?)[a-z0-9]*$
func sanitizeShareName(name string) string {
	// Convert to lowercase and replace spaces with hyphens
	shareName := strings.ToLower(name)
	shareName = strings.ReplaceAll(shareName, " ", "-")

	// Remove any characters that aren't lowercase letters, numbers, hyphens, or underscores
	var validShareName strings.Builder
	for _, r := range shareName {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			validShareName.WriteRune(r)
		}
	}

	result := validShareName.String()
	if result == "" {
		return "matrixbridge" // fallback if no valid characters
	}

	// Ensure it starts with alphanumeric
	for len(result) > 0 && (result[0] == '-' || result[0] == '_') {
		result = result[1:]
	}

	// Ensure it ends with alphanumeric
	for len(result) > 0 && (result[len(result)-1] == '-' || result[len(result)-1] == '_') {
		result = result[:len(result)-1]
	}

	// Final fallback check
	if result == "" {
		return "matrixbridge"
	}

	return result
}

// syncChannelMembersToMatrixRoom creates ghost users for all channel members and joins them to the Matrix room
func (c *Handler) syncChannelMembersToMatrixRoom(channelID, roomID string) (int, int, error) {
	matrixClient := c.plugin.GetMatrixClient()
	if matrixClient == nil {
		return 0, 0, errors.New("matrix client not available")
	}

	offset := 0
	limit := 100
	totalMembers := 0
	joinedCount := 0

	c.client.Log.Info("Starting to sync channel members to Matrix room", "channel_id", channelID, "room_id", roomID)

	// Process channel members with pagination - combine fetching and processing for memory efficiency
	for {
		pageMembers, appErr := c.pluginAPI.GetChannelMembers(channelID, offset, limit)
		if appErr != nil {
			c.client.Log.Warn("Failed to get channel members for ghost user creation", "error", appErr, "channel_id", channelID, "offset", offset)
			return joinedCount, totalMembers, errors.Wrap(appErr, "failed to get channel members")
		}
		if len(pageMembers) == 0 {
			break
		}

		totalMembers += len(pageMembers)

		// Process each member in this page immediately
		for _, member := range pageMembers {
			user, appErr := c.client.User.Get(member.UserId)
			if appErr != nil {
				c.client.Log.Warn("Failed to get user for processing", "error", appErr, "user_id", member.UserId)
				continue
			}

			if user.IsRemote() {
				// This is a Matrix-originated user - invite the original Matrix user to the room
				originalMatrixUserID, err := c.plugin.GetMatrixUserIDFromMattermostUser(user.Id)
				if err != nil {
					c.client.Log.Warn("Failed to get original Matrix user ID for remote user", "error", err, "user_id", user.Id, "username", user.Username)
					continue
				}

				// Invite the original Matrix user to the room
				if err := matrixClient.InviteUserToRoom(roomID, originalMatrixUserID); err != nil {
					c.client.Log.Warn("Failed to invite Matrix user to room", "error", err, "matrix_user_id", originalMatrixUserID, "mattermost_user_id", user.Id, "room_id", roomID)
				} else {
					c.client.Log.Debug("Successfully invited Matrix user to room", "matrix_user_id", originalMatrixUserID, "mattermost_user_id", user.Id, "username", user.Username, "room_id", roomID)
					joinedCount++
				}
			} else {
				// This is a local Mattermost user - create ghost user and join to room
				ghostUserID, err := c.plugin.CreateOrGetGhostUser(user.Id)
				if err != nil {
					c.client.Log.Warn("Failed to create or get ghost user", "error", err, "user_id", user.Id, "username", user.Username)
					continue
				}

				// Join the ghost user to the room (handles both public and private rooms)
				if err := matrixClient.InviteAndJoinGhostUser(roomID, ghostUserID); err != nil {
					c.client.Log.Warn("Failed to join ghost user to Matrix room", "error", err, "ghost_user_id", ghostUserID, "user_id", user.Id, "room_id", roomID)
				} else {
					c.client.Log.Debug("Successfully joined ghost user to Matrix room", "ghost_user_id", ghostUserID, "user_id", user.Id, "username", user.Username, "room_id", roomID)
					joinedCount++
				}
			}
		}

		c.client.Log.Debug("Processed page of channel members", "processed_in_page", len(pageMembers), "total_processed", totalMembers, "total_joined", joinedCount)

		// Move to next page
		offset += limit
	}

	c.client.Log.Info("Completed syncing channel members to Matrix room", "joined_count", joinedCount, "total_members", totalMembers, "room_id", roomID)
	return joinedCount, totalMembers, nil
}

// Handler implements slash command processing for the Matrix Bridge plugin.
type Handler struct {
	plugin    PluginAccessor
	client    *pluginapi.Client
	kvstore   kvstore.KVStore
	pluginAPI plugin.API
}

// Command defines the interface for handling Matrix Bridge slash commands.
type Command interface {
	Handle(args *model.CommandArgs) (*model.CommandResponse, error)
	executeMatrixCommand(args *model.CommandArgs) *model.CommandResponse
}

// Command usage and help text constants
const (
	// Triggers
	matrixCommandTrigger = "matrix"

	// Main command usage
	matrixCommandUsage = "Usage: /matrix [test|create|map|unmap|list|status|migrate] [room_name|room_alias|room_id]"

	// Subcommand descriptions for autocomplete
	testCommandDesc    = "Test Matrix server connection and configuration"
	createCommandDesc  = "Create a new Matrix room and map to current channel (uses channel name if room name not provided)"
	createCommandHint  = "[room_name] [publish=true|false]"
	mapCommandDesc     = "Map current channel to Matrix room (prefer #alias:server.com)"
	mapCommandHint     = "[room_alias|room_id]"
	unmapCommandDesc   = "Remove mapping between current channel and Matrix room, and uninvite plugin from shared channel"
	unmapCommandHint   = ""
	listCommandDesc    = "List all channel-to-room mappings"
	statusCommandDesc  = "Show bridge status"
	migrateCommandDesc = "Reset and re-run KV store migrations to fix missing room mappings"
	serverCommandDesc  = "Manage additional Matrix servers (admin only, local multi-server testing)"

	// server subcommand usage
	serverCommandUsage = "Usage: /matrix server [list|add|remove|map]\n" +
		"• `/matrix server list` - Show all registered Matrix servers\n" +
		"• `/matrix server add <server_url> <server_name> <as_token> <hs_token> [username_prefix]` - Register/replace a Matrix server\n" +
		"• `/matrix server remove <server_id>` - Remove a registered Matrix server\n" +
		"• `/matrix server map <server_id> <room_alias|room_id>` - Map the current channel to a room on a specific server"
	serverAdminOnlyError = "❌ This command requires System Administrator privileges."

	// Map command usage and validation
	mapCommandUsage     = "Usage: /matrix map [room_alias|room_id]\nExample: /matrix map #test-sync:synapse-mydomain.com"
	roomIdentifierError = "Invalid room identifier format. Use either:\n• Room alias: `#roomname:server.com` (preferred for joining)\n• Room ID: `!roomid:server.com`"

	// Error messages
	matrixClientNotConfigured = "❌ Matrix client not configured. Please configure Matrix settings in System Console."
	unknownSubcommandError    = "Unknown subcommand. Use: test, create, map, unmap, list, status, or migrate"

	// Status messages
	autoJoinSuccess     = "\n\n✅ **Auto-joined** Matrix room successfully!"
	autoJoinWithUser    = "\n\n✅ **Auto-joined** Matrix room successfully!"
	autoJoinFailed      = "\n\n⚠️ **Note:** Could not auto-join Matrix room. You may need to manually invite the bridge user or make the room public in Matrix."
	matrixClientMissing = "\n\n⚠️ **Note:** Matrix client not configured. Please configure Matrix settings and manually invite the bridge user."

	// Room creation status messages
	roomCreatorJoined        = "\n\nMatrix room created and configured for bridging."
	roomCreatorWithUserReady = "\n\nMatrix room created and you are connected to it."
	roomMemberSyncFailed     = "\n\n⚠️ **Matrix room created, but failed to sync channel members.** Check plugin logs for details. You may need to manually invite users to the Matrix room."

	// Sharing status messages
	channelSharingEnabled = "\n\n✅ **Channel sharing enabled** - Messages will now sync to Matrix!"
	channelSharingFailed  = "\n\n⚠️ **Note:** Failed to automatically enable channel sharing. You may need to manually enable shared channels for this channel to start syncing."

	// Directory status messages
	publishedToDirectory    = "\n**Directory:** Published to public directory"
	notPublishedToDirectory = "\n**Directory:** Not published (private room)"

	// Common help text for commands
	getStartedHelp = "**Get Started:**\n" +
		"• `/matrix create` - Create new Matrix room using channel name and map to current channel\n" +
		"• `/matrix create [room_name]` - Create new Matrix room with custom name and map to current channel\n" +
		"• `/matrix map [room_alias|room_id]` - Map current channel to existing Matrix room\n"

	commandsHelp = "**Commands:**\n" +
		"• `/matrix map [room_alias|room_id]` - Map current channel to Matrix room\n" +
		"• `/matrix create` - Create new Matrix room using channel name and map to current channel\n" +
		"• `/matrix create [room_name]` - Create new Matrix room with custom name and map to current channel\n" +
		"• `/matrix status` - Check bridge status\n"

	// Status command response
	statusCommandResponse = "Matrix Bridge Status:\n- Plugin: Active\n- Configuration: Check System Console → Plugins → Matrix Bridge\n- Logs: Check plugin logs for connection status"

	// Test command next steps
	testCommandNextSteps = "\n📋 **Next Steps:**\n" +
		"   • Use `/matrix create \"Room Name\"` to create a Matrix room\n" +
		"   • The channel will be automatically configured for syncing\n"
)

// NewCommandHandler creates and registers all slash commands for the Matrix Bridge plugin.
func NewCommandHandler(plugin PluginAccessor) Command {
	// Cache frequently used services for reduced verbosity
	client := plugin.GetPluginAPIClient()
	kvstore := plugin.GetKVStore()
	pluginAPI := plugin.GetPluginAPI()

	matrixData := model.NewAutocompleteData(matrixCommandTrigger, "[subcommand]", "Matrix bridge commands")
	matrixData.AddCommand(model.NewAutocompleteData("test", "", testCommandDesc))

	// Create command with argument completion
	createCmd := model.NewAutocompleteData("create", createCommandHint, createCommandDesc)
	createCmd.AddTextArgument("Optional room name (defaults to channel name)", "[room_name]", "")
	createCmd.AddTextArgument("Optional publish flag", "[publish=true|false]", "")
	matrixData.AddCommand(createCmd)

	// Map command with argument completion
	mapCmd := model.NewAutocompleteData("map", mapCommandHint, mapCommandDesc)
	mapCmd.AddTextArgument("Matrix room alias or room ID", "[room_alias|room_id]", "")
	matrixData.AddCommand(mapCmd)

	// Unmap command
	matrixData.AddCommand(model.NewAutocompleteData("unmap", unmapCommandHint, unmapCommandDesc))

	matrixData.AddCommand(model.NewAutocompleteData("list", "", listCommandDesc))
	matrixData.AddCommand(model.NewAutocompleteData("status", "", statusCommandDesc))
	matrixData.AddCommand(model.NewAutocompleteData("migrate", "", migrateCommandDesc))

	// Server management command (admin only) for local multi-server testing.
	serverCmd := model.NewAutocompleteData("server", "[list|add|remove]", serverCommandDesc)
	serverCmd.AddCommand(model.NewAutocompleteData("list", "", "List all registered Matrix servers"))
	serverAddCmd := model.NewAutocompleteData("add", "<server_url> <server_name> <as_token> <hs_token> [username_prefix]", "Register or replace a Matrix server")
	serverAddCmd.AddTextArgument("Matrix homeserver base URL", "<server_url>", "")
	serverAddCmd.AddTextArgument("Matrix server name (domain in user IDs)", "<server_name>", "")
	serverAddCmd.AddTextArgument("Application Service token", "<as_token>", "")
	serverAddCmd.AddTextArgument("Homeserver token", "<hs_token>", "")
	serverAddCmd.AddTextArgument("Optional username prefix", "[username_prefix]", "")
	serverCmd.AddCommand(serverAddCmd)
	serverRemoveCmd := model.NewAutocompleteData("remove", "<server_id>", "Remove a registered Matrix server")
	serverRemoveCmd.AddTextArgument("Server ID (from /matrix server list)", "<server_id>", "")
	serverCmd.AddCommand(serverRemoveCmd)
	serverMapCmd := model.NewAutocompleteData("map", "<server_id> <room_alias|room_id>", "Map the current channel to a room on a specific server")
	serverMapCmd.AddTextArgument("Server ID (from /matrix server list)", "<server_id>", "")
	serverMapCmd.AddTextArgument("Matrix room alias or room ID", "<room_alias|room_id>", "")
	serverCmd.AddCommand(serverMapCmd)
	matrixData.AddCommand(serverCmd)

	err := client.SlashCommand.Register(&model.Command{
		Trigger:          matrixCommandTrigger,
		AutoComplete:     true,
		AutoCompleteDesc: "Matrix bridge commands",
		AutoCompleteHint: "[subcommand]",
		AutocompleteData: matrixData,
	})
	if err != nil {
		client.Log.Error("Failed to register matrix command", "error", err)
	}

	return &Handler{
		plugin:    plugin,
		client:    client,
		kvstore:   kvstore,
		pluginAPI: pluginAPI,
	}
}

// Handle processes slash commands registered by the Matrix Bridge plugin.
func (c *Handler) Handle(args *model.CommandArgs) (*model.CommandResponse, error) {
	trigger := strings.TrimPrefix(strings.Fields(args.Command)[0], "/")
	switch trigger {
	case matrixCommandTrigger:
		return c.executeMatrixCommand(args), nil
	default:
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         fmt.Sprintf("Unknown command: %s", args.Command),
		}, nil
	}
}

// getMatrixClientOrError gets the current Matrix client or returns an error response if not configured
func (c *Handler) getMatrixClientOrError() (*matrix.Client, *model.CommandResponse) {
	matrixClient := c.plugin.GetMatrixClient()
	if matrixClient == nil {
		return nil, &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         matrixClientNotConfigured,
		}
	}
	return matrixClient, nil
}

func (c *Handler) executeMapCommand(args *model.CommandArgs, roomIdentifier string) *model.CommandResponse {
	// Get current Matrix client and fail fast if not configured
	matrixClient, errResponse := c.getMatrixClientOrError()
	if errResponse != nil {
		return errResponse
	}

	// Validate room identifier format (should start with ! or # and contain a colon)
	if (!strings.HasPrefix(roomIdentifier, "!") && !strings.HasPrefix(roomIdentifier, "#")) || !strings.Contains(roomIdentifier, ":") {
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         roomIdentifierError,
		}
	}

	// Get channel info for display
	channel, appErr := c.client.Channel.Get(args.ChannelId)
	channelName := args.ChannelId
	if appErr == nil {
		channelName = channel.DisplayName
		if channelName == "" {
			channelName = channel.Name
		}
	}

	// Try to join the Matrix room automatically
	var joinStatus string
	// Join the AS bot to establish bridge presence
	if err := matrixClient.JoinRoom(roomIdentifier); err != nil {
		c.client.Log.Warn("Failed to auto-join Matrix room", "error", err, "room_identifier", roomIdentifier)
		joinStatus = autoJoinFailed
	} else {
		c.client.Log.Info("Successfully joined Matrix room as AS bot", "room_identifier", roomIdentifier)

		// Also join the ghost user of the command issuer for immediate messaging capability
		user, appErr := c.client.User.Get(args.UserId)
		if appErr != nil {
			c.client.Log.Warn("Failed to get command issuer for ghost user join", "error", appErr, "user_id", args.UserId)
			joinStatus = autoJoinSuccess
		} else {
			// Create or get ghost user for the command issuer
			ghostUserID, err := c.plugin.CreateOrGetGhostUser(user.Id)
			if err != nil {
				c.client.Log.Warn("Failed to create or get ghost user for command issuer", "error", err, "user_id", user.Id)
				joinStatus = autoJoinSuccess
			} else {
				// Join the ghost user to the room (handles both public and private rooms)
				if err := matrixClient.InviteAndJoinGhostUser(roomIdentifier, ghostUserID); err != nil {
					c.client.Log.Warn("Failed to join ghost user to room", "error", err, "ghost_user_id", ghostUserID, "room_identifier", roomIdentifier)
					joinStatus = autoJoinSuccess
				} else {
					c.client.Log.Info("Successfully joined ghost user to room", "ghost_user_id", ghostUserID, "room_identifier", roomIdentifier)
					joinStatus = autoJoinWithUser
				}
			}
		}
	}

	// Save both directions of the mapping
	serverID := c.plugin.GetServerID()
	mappingKey := kvstore.BuildChannelMappingKey(args.ChannelId)
	mappingValue, err := kvstore.BuildSingleChannelMapping(serverID, roomIdentifier)
	if err == nil {
		err = c.kvstore.Set(mappingKey, mappingValue)
	}
	if err != nil {
		c.client.Log.Error("Failed to save channel mapping", "error", err, "channel_id", args.ChannelId, "room_identifier", roomIdentifier)
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         fmt.Sprintf("❌ Failed to save channel mapping. Check plugin logs for details.%s", joinStatus),
		}
	}

	// Store reverse mapping: room_mapping_<serverID>_<roomIdentifier> -> channelID
	roomMappingKey := kvstore.BuildRoomMappingKey(serverID, roomIdentifier)
	err = c.kvstore.Set(roomMappingKey, []byte(args.ChannelId))
	if err != nil {
		c.client.Log.Error("Failed to save room mapping", "error", err, "room_identifier", roomIdentifier, "channel_id", args.ChannelId)
		// Continue anyway - the forward mapping was saved successfully
	}

	// If roomIdentifier is an alias, also resolve to room ID and store that mapping
	if strings.HasPrefix(roomIdentifier, "#") {
		if resolvedRoomID, err := matrixClient.ResolveRoomAlias(roomIdentifier); err == nil {
			roomIDMappingKey := kvstore.BuildRoomMappingKey(serverID, resolvedRoomID)
			if err := c.kvstore.Set(roomIDMappingKey, []byte(args.ChannelId)); err != nil {
				c.client.Log.Error("Failed to save room ID mapping", "error", err, "room_id", resolvedRoomID, "channel_id", args.ChannelId)
			}
		}
	}

	c.client.Log.Info("Channel mapping saved", "channel_id", args.ChannelId, "channel_name", channelName, "room_identifier", roomIdentifier)

	// Add bridge alias for Matrix Application Service filtering
	// Extract room name from the identifier for the bridge alias
	var roomName string
	if strings.HasPrefix(roomIdentifier, "#") {
		// Extract local part from room alias (#name:server.com -> name)
		parts := strings.Split(roomIdentifier[1:], ":")
		if len(parts) > 0 {
			roomName = parts[0]
		}
	} else {
		// For room IDs, use channel name as fallback
		roomName = strings.ToLower(strings.ReplaceAll(channelName, " ", "-"))
		roomName = strings.ReplaceAll(roomName, "_", "-")
	}

	if roomName != "" {
		// Create bridge alias
		serverDomain := c.extractServerDomain()
		bridgeAlias := "#mattermost-bridge-" + roomName + ":" + serverDomain

		// Resolve room identifier to room ID (handles both aliases and room IDs)
		roomID, err := matrixClient.ResolveRoomAlias(roomIdentifier)
		if err != nil {
			c.client.Log.Warn("Failed to resolve room identifier for bridge alias", "error", err, "room_identifier", roomIdentifier)
			roomID = ""
		}

		if roomID != "" {
			err = matrixClient.AddRoomAlias(roomID, bridgeAlias)
			if err != nil {
				c.client.Log.Warn("Failed to add bridge filtering alias for manual mapping", "error", err, "bridge_alias", bridgeAlias, "room_id", roomID)
				// Continue - mapping still works, just no filtering alias
			} else {
				c.client.Log.Info("Successfully added bridge filtering alias for manual mapping", "room_id", roomID, "bridge_alias", bridgeAlias, "original_identifier", roomIdentifier)
			}
		}
	}

	// Sync all channel members to the Matrix room (same as /matrix create does)
	var memberSyncStatus string
	roomID := roomIdentifier
	// Resolve to actual room ID if it's an alias
	if resolvedRoomID, err := matrixClient.ResolveRoomAlias(roomIdentifier); err == nil && resolvedRoomID != "" {
		roomID = resolvedRoomID
	}

	joinedCount, totalMembers, syncErr := c.syncChannelMembersToMatrixRoom(args.ChannelId, roomID)
	if syncErr != nil {
		c.client.Log.Error("Failed to sync channel members to Matrix room", "error", syncErr, "room_id", roomID, "channel_id", args.ChannelId)
		memberSyncStatus = roomMemberSyncFailed
	} else {
		// Generate appropriate status message based on sync results
		switch {
		case joinedCount == 0:
			memberSyncStatus = ""
		case joinedCount == 1 && totalMembers == 1:
			// Only one member (likely the command issuer) in a single-member channel
			memberSyncStatus = roomCreatorWithUserReady
		default:
			// Multiple members were synced
			memberSyncStatus = fmt.Sprintf("\n\n✅ **All channel members synced to Matrix** - %d of %d users joined the room.", joinedCount, totalMembers)
		}
	}

	// Share the channel and invite this plugin to receive sync messages
	shareStatus := c.shareChannelAndInvitePlugin(args, channelName, fmt.Sprintf("Mapped to Matrix room: %s", roomIdentifier))

	return &model.CommandResponse{
		ResponseType: model.CommandResponseTypeEphemeral,
		Text:         fmt.Sprintf("✅ **Mapping Saved**\n\n**Channel:** %s\n**Matrix Room:** `%s`%s%s%s", channelName, roomIdentifier, joinStatus, memberSyncStatus, shareStatus),
	}
}

func (c *Handler) executeUnmapCommand(args *model.CommandArgs) *model.CommandResponse {
	// Get channel info for display
	channel, appErr := c.client.Channel.Get(args.ChannelId)
	channelName := args.ChannelId
	if appErr == nil {
		channelName = channel.DisplayName
		if channelName == "" {
			channelName = channel.Name
		}
	}

	// Check if this channel has a Matrix room mapping
	serverID := c.plugin.GetServerID()
	channelMappingKey := kvstore.BuildChannelMappingKey(args.ChannelId)
	roomIDBytes, err := c.kvstore.Get(channelMappingKey)

	// A corrupt (unparseable) value is distinct from an unmapped channel. Clear
	// the bad record so the admin can recover with /matrix map, rather than being
	// told the channel is not mapped with no way to fix it.
	if err == nil && len(roomIDBytes) > 0 {
		if _, parseErr := kvstore.ParseChannelServerMappings(roomIDBytes); parseErr != nil {
			c.client.Log.Error("Corrupt channel mapping value; clearing it", "error", parseErr, "channel_id", args.ChannelId)
			if delErr := c.kvstore.Delete(channelMappingKey); delErr != nil {
				c.client.Log.Error("Failed to clear corrupt channel mapping", "error", delErr, "channel_id", args.ChannelId)
			}
			return &model.CommandResponse{
				ResponseType: model.CommandResponseTypeEphemeral,
				Text:         fmt.Sprintf("⚠️ **Corrupt Mapping Cleared**\n\nChannel `%s` had an unreadable Matrix room mapping, which has been removed. Use `/matrix map` to remap it if needed.", channelName),
			}
		}
	}

	mappings, _ := kvstore.ParseChannelServerMappings(roomIDBytes)
	matrixRoomIdentifier := kvstore.RoomIDForServer(mappings, serverID)
	if err != nil || matrixRoomIdentifier == "" {
		// Key not found is expected for unmapped channels
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         fmt.Sprintf("❌ **No Mapping Found**\n\nChannel `%s` is not currently mapped to any Matrix room.", channelName),
		}
	}

	// Clear the Matrix room state to prevent fallback lookups - this is critical
	matrixClient := c.plugin.GetMatrixClient()
	if matrixClient == nil {
		c.client.Log.Error("Matrix client not available, cannot clear room state", "room_id", matrixRoomIdentifier)
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         "❌ **Error:** Matrix client not configured. Cannot safely unmap - sync messages would continue.",
		}
	}

	if err := matrixClient.RemoveMattermostChannelID(matrixRoomIdentifier); err != nil {
		c.client.Log.Error("Failed to clear Matrix room state - sync messages would continue", "error", err, "room_id", matrixRoomIdentifier, "channel_id", args.ChannelId)
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         "❌ **Error:** Failed to clear Matrix room state. Cannot safely unmap - sync messages would continue. Check plugin logs for details.",
		}
	}

	c.client.Log.Info("Successfully cleared Matrix room state", "room_id", matrixRoomIdentifier)

	// Remove the channel->room mapping
	if err := c.kvstore.Delete(channelMappingKey); err != nil {
		c.client.Log.Error("Failed to remove channel mapping", "error", err, "channel_id", args.ChannelId, "room_identifier", matrixRoomIdentifier)
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         "❌ **Error:** Failed to remove channel mapping. Check plugin logs for details.",
		}
	}

	// Remove the room->channel mapping
	roomMappingKey := kvstore.BuildRoomMappingKey(serverID, matrixRoomIdentifier)
	if err := c.kvstore.Delete(roomMappingKey); err != nil {
		c.client.Log.Warn("Failed to remove room mapping", "error", err, "room_identifier", matrixRoomIdentifier, "channel_id", args.ChannelId)
		// Continue - the main mapping was removed
	}

	c.client.Log.Info("Removed Matrix room mapping", "channel_id", args.ChannelId, "room_identifier", matrixRoomIdentifier)

	// Uninvite this plugin from the shared channel
	uninviteErr := c.pluginAPI.UninviteRemoteFromChannel(args.ChannelId, c.plugin.GetRemoteID())

	var responseIcon, responseTitle, uninviteStatus string
	if uninviteErr != nil {
		c.client.Log.Warn("Failed to uninvite plugin from shared channel", "error", uninviteErr, "channel_id", args.ChannelId, "remote_id", c.plugin.GetRemoteID())
		responseIcon = "⚠️"
		responseTitle = "**Mapping Partially Removed**"
		uninviteStatus = "\n\n⚠️ **Note:** Failed to uninvite plugin from shared channel. The channel may still receive some sync events."
	} else {
		c.client.Log.Info("Successfully uninvited plugin from shared channel", "channel_id", args.ChannelId, "remote_id", c.plugin.GetRemoteID())
		responseIcon = "✅"
		responseTitle = "**Mapping Removed**"
		uninviteStatus = "\n\n✅ **Plugin uninvited** from shared channel successfully!"
	}

	return &model.CommandResponse{
		ResponseType: model.CommandResponseTypeEphemeral,
		Text:         fmt.Sprintf("%s %s\n\n**Channel:** %s\n**Matrix Room:** `%s`%s", responseIcon, responseTitle, channelName, matrixRoomIdentifier, uninviteStatus),
	}
}

func (c *Handler) executeCreateRoomCommand(args *model.CommandArgs, roomName string, publish bool) *model.CommandResponse {
	// Get current Matrix client and fail fast if not configured
	matrixClient, errResponse := c.getMatrixClientOrError()
	if errResponse != nil {
		return errResponse
	}

	// Get channel info for room name (if not provided) and topic
	channel, appErr := c.client.Channel.Get(args.ChannelId)
	channelName := args.ChannelId
	if appErr == nil {
		channelName = channel.DisplayName
		if channelName == "" {
			channelName = channel.Name
		}
	}

	// Use channel name as room name if not provided
	if roomName == "" {
		roomName = channelName
	}

	topic := fmt.Sprintf("Matrix room for Mattermost channel: %s", channelName)

	// Create the Matrix room
	// Extract server domain from Matrix server URL
	serverDomain := c.extractServerDomain()
	roomID, err := matrixClient.CreateRoom(roomName, topic, serverDomain, publish, args.ChannelId)
	if err != nil {
		c.client.Log.Error("Failed to create Matrix room", "error", err, "room_name", roomName)
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         fmt.Sprintf("❌ Failed to create Matrix room '%s'. Check plugin logs for details.", roomName),
		}
	}

	c.client.Log.Info("Created Matrix room and published to directory", "room_id", roomID, "room_name", roomName)

	// Sync all channel members to the newly created Matrix room
	var joinStatus string
	joinedCount, totalMembers, syncErr := c.syncChannelMembersToMatrixRoom(args.ChannelId, roomID)
	if syncErr != nil {
		c.client.Log.Error("Failed to sync channel members to Matrix room", "error", syncErr, "room_id", roomID, "channel_id", args.ChannelId)
		joinStatus = roomMemberSyncFailed
	} else {
		// Generate appropriate status message based on sync results
		switch {
		case joinedCount == 0:
			joinStatus = roomCreatorJoined
		case joinedCount == 1 && totalMembers == 1:
			// Only one member (likely the command issuer) in a single-member channel
			joinStatus = roomCreatorWithUserReady
		default:
			// Multiple members were synced
			joinStatus = fmt.Sprintf("\n\n✅ **All channel members synced to Matrix** - %d of %d users joined the room.", joinedCount, totalMembers)
		}
	}

	// Automatically map the created room to this channel (both directions)
	serverID := c.plugin.GetServerID()
	mappingKey := kvstore.BuildChannelMappingKey(args.ChannelId)
	mappingValue, mErr := kvstore.BuildSingleChannelMapping(serverID, roomID)
	if mErr == nil {
		mErr = c.kvstore.Set(mappingKey, mappingValue)
	}
	if mErr != nil {
		c.client.Log.Error("Failed to save channel mapping", "error", mErr, "channel_id", args.ChannelId, "room_id", roomID)
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         fmt.Sprintf("✅ **Matrix Room Created:** `%s`\n\n❌ Failed to save channel mapping. Use `/matrix map %s` to map manually.", roomID, roomID),
		}
	}

	// Store reverse mapping: room_mapping_<serverID>_<roomID> -> channelID
	roomMappingKey := kvstore.BuildRoomMappingKey(serverID, roomID)
	if err := c.kvstore.Set(roomMappingKey, []byte(args.ChannelId)); err != nil {
		c.client.Log.Error("Failed to save room mapping", "error", err, "room_id", roomID, "channel_id", args.ChannelId)
		// Continue anyway - the forward mapping was saved successfully
	}

	// Share the channel and invite this plugin to receive sync messages
	shareStatus := c.shareChannelAndInvitePlugin(args, channelName, topic)

	// Build status message based on publish parameter
	publishStatus := ""
	if publish {
		publishStatus = publishedToDirectory
	} else {
		publishStatus = notPublishedToDirectory
	}

	return &model.CommandResponse{
		ResponseType: model.CommandResponseTypeEphemeral,
		Text:         fmt.Sprintf("✅ **Matrix Room Created & Mapped**\n\n**Room Name:** %s\n**Room ID:** `%s`\n**Channel:** %s%s%s%s", roomName, roomID, channelName, publishStatus, joinStatus, shareStatus),
	}
}

func (c *Handler) executeListMappingsCommand(args *model.CommandArgs) *model.CommandResponse {
	var responseText strings.Builder
	responseText.WriteString("**Channel-to-Room Mappings:**\n\n")

	// Get channel mapping keys using efficient prefix filtering
	serverID := c.plugin.GetServerID()
	mappings := make(map[string]string)
	channelMappingPrefix := kvstore.KeyPrefixChannelMapping
	page := 0
	batchSize := 1000

	for {
		keys, err := c.kvstore.ListKeysWithPrefix(page, batchSize, channelMappingPrefix)
		if err != nil {
			c.client.Log.Error("Failed to list KV store keys with prefix", "error", err, "page", page, "prefix", channelMappingPrefix)
			responseText.WriteString("❌ Failed to retrieve mappings. Check plugin logs for details.\n")
			return &model.CommandResponse{
				ResponseType: model.CommandResponseTypeEphemeral,
				Text:         responseText.String(),
			}
		}

		if len(keys) == 0 {
			break // No more keys
		}

		// Build mappings directly (no need to filter since prefix filtering is server-side)
		for _, key := range keys {
			channelID := strings.TrimPrefix(key, channelMappingPrefix)
			roomIDBytes, err := c.kvstore.Get(key)
			if err != nil || len(roomIDBytes) == 0 {
				continue
			}
			channelMappings, parseErr := kvstore.ParseChannelServerMappings(roomIDBytes)
			if parseErr != nil {
				c.client.Log.Warn("Failed to parse channel mapping value", "channel_id", channelID, "error", parseErr)
				continue
			}
			if roomID := kvstore.RoomIDForServer(channelMappings, serverID); roomID != "" {
				mappings[channelID] = roomID
			}
		}

		// If we got fewer keys than the batch size, we've reached the end
		if len(keys) < batchSize {
			break
		}

		page++
	}

	if len(mappings) == 0 {
		responseText.WriteString("No channel mappings found.\n\n")
		responseText.WriteString(getStartedHelp)
	} else {
		// Show current channel first if it has a mapping
		currentChannelMapping := mappings[args.ChannelId]
		if currentChannelMapping != "" {
			channel, appErr := c.client.Channel.Get(args.ChannelId)
			channelName := args.ChannelId
			if appErr == nil {
				channelName = channel.DisplayName
				if channelName == "" {
					channelName = channel.Name
				}
			}
			responseText.WriteString(fmt.Sprintf("**Current Channel:** %s → `%s`\n\n", channelName, currentChannelMapping))
		}

		// Show all mappings
		responseText.WriteString(fmt.Sprintf("**All Mappings (%d total):**\n", len(mappings)))
		for channelID, roomID := range mappings {
			// Get channel info
			channel, appErr := c.client.Channel.Get(channelID)
			channelName := channelID
			if appErr == nil {
				channelName = channel.DisplayName
				if channelName == "" {
					channelName = channel.Name
				}
			}

			// Mark current channel
			currentMarker := ""
			if channelID == args.ChannelId {
				currentMarker = " *(current)*"
			}

			responseText.WriteString(fmt.Sprintf("• %s → `%s`%s\n", channelName, roomID, currentMarker))
		}
	}

	responseText.WriteString("\n")
	responseText.WriteString(commandsHelp)

	return &model.CommandResponse{
		ResponseType: model.CommandResponseTypeEphemeral,
		Text:         responseText.String(),
	}
}

func (c *Handler) executeMatrixCommand(args *model.CommandArgs) *model.CommandResponse {
	fields := strings.Fields(args.Command)
	if len(fields) < 2 {
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         matrixCommandUsage,
		}
	}

	subcommand := fields[1]
	switch subcommand {
	case "test":
		return c.executeTestCommand(args)
	case "create":
		// Parse room name and optional publish parameter
		var roomName string
		publish := false // don't publish rooms unless user explicitly requests it

		// Handle different argument patterns:
		// /matrix create
		// /matrix create true/false
		// /matrix create publish=true/false
		// /matrix create "room name"
		// /matrix create "room name" true/false
		// /matrix create "room name" publish=true/false

		switch {
		case len(fields) == 2:
			// Just "/matrix create" - use channel name, no publish
			roomName = ""
		case len(fields) == 3:
			// Check if it's a publish parameter or room name
			arg := fields[2]
			if arg == "true" || arg == "false" || strings.HasPrefix(arg, "publish=") {
				// It's a publish parameter, use channel name for room
				roomName = ""
				if publishValue, ok := strings.CutPrefix(arg, "publish="); ok {
					publish = publishValue == "true"
				} else {
					publish = arg == "true"
				}
			} else {
				// It's a room name
				roomName = arg
			}
		default:
			// Multiple arguments - check if last is publish parameter
			lastField := fields[len(fields)-1]
			if lastField == "true" || lastField == "false" || strings.HasPrefix(lastField, "publish=") {
				if publishValue, ok := strings.CutPrefix(lastField, "publish="); ok {
					publish = publishValue == "true"
				} else {
					publish = lastField == "true"
				}
				// Room name is everything except the last field
				roomName = strings.Join(fields[2:len(fields)-1], " ")
			} else {
				// No publish parameter, room name is everything after "create"
				roomName = strings.Join(fields[2:], " ")
			}
		}

		// Strip surrounding quotes that users may add around room names
		roomName = strings.Trim(roomName, "\"'")
		return c.executeCreateRoomCommand(args, roomName, publish)
	case "map":
		if len(fields) < 3 {
			return &model.CommandResponse{
				ResponseType: model.CommandResponseTypeEphemeral,
				Text:         mapCommandUsage,
			}
		}
		roomID := fields[2]
		return c.executeMapCommand(args, roomID)
	case "unmap":
		return c.executeUnmapCommand(args)
	case "list":
		return c.executeListMappingsCommand(args)
	case "status":
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         statusCommandResponse,
		}
	case "migrate":
		return c.executeMigrateCommand(args)
	case "server":
		return c.executeServerCommand(args, fields)
	default:
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         unknownSubcommandError,
		}
	}
}

// executeServerCommand handles the admin-only `/matrix server` subcommand group,
// which injects additional Matrix servers into the registry for local
// multi-server testing (the System Console UI supports only one server today).
func (c *Handler) executeServerCommand(args *model.CommandArgs, fields []string) *model.CommandResponse {
	// Gate on System Administrator: this writes bridge routing config.
	if !c.pluginAPI.HasPermissionTo(args.UserId, model.PermissionManageSystem) {
		return ephemeral(serverAdminOnlyError)
	}

	if len(fields) < 3 {
		return ephemeral(serverCommandUsage)
	}

	switch fields[2] {
	case "list":
		return c.executeServerListCommand()
	case "add":
		return c.executeServerAddCommand(fields)
	case "remove":
		return c.executeServerRemoveCommand(fields)
	case "map":
		return c.executeServerMapCommand(args, fields)
	default:
		return ephemeral(serverCommandUsage)
	}
}

func (c *Handler) executeServerListCommand() *model.CommandResponse {
	servers, err := c.plugin.GetManagedServers()
	if err != nil {
		c.client.Log.Error("Failed to read managed servers", "error", err)
		return ephemeral("❌ Failed to read the server registry. Check plugin logs for details.")
	}
	if len(servers) == 0 {
		return ephemeral("No Matrix servers are registered.")
	}

	var b strings.Builder
	fmt.Fprintf(&b, "**Registered Matrix servers (%d):**\n", len(servers))
	for _, s := range servers {
		enabled := "disabled"
		if s.Enabled {
			enabled = "enabled"
		}
		injected := ""
		if s.Injected {
			injected = " _(injected)_"
		}
		fmt.Fprintf(&b, "\n• **%s** (`%s`)%s\n", s.ServerName, s.ServerID, injected)
		fmt.Fprintf(&b, "  - URL: `%s`\n", s.ServerURL)
		fmt.Fprintf(&b, "  - Username prefix: `%s`\n", s.UsernamePrefix)
		fmt.Fprintf(&b, "  - Status: %s\n", enabled)
	}
	return ephemeral(b.String())
}

func (c *Handler) executeServerAddCommand(fields []string) *model.CommandResponse {
	// /matrix server add <server_url> <server_name> <as_token> <hs_token> [username_prefix]
	if len(fields) < 7 {
		return ephemeral(serverCommandUsage)
	}
	serverURL := fields[3]
	serverName := fields[4]
	asToken := fields[5]
	hsToken := fields[6]
	usernamePrefix := ""
	if len(fields) >= 8 {
		usernamePrefix = fields[7]
	}

	serverID, err := c.plugin.AddManagedServer(serverURL, serverName, asToken, hsToken, usernamePrefix)
	if err != nil {
		c.client.Log.Error("Failed to add managed server", "error", err, "server_url", serverURL)
		return ephemeral(fmt.Sprintf("❌ Failed to add server: %s", err.Error()))
	}

	c.client.Log.Info("Added managed Matrix server", "server_id", serverID, "server_url", serverURL, "server_name", serverName)
	return ephemeral(fmt.Sprintf("✅ **Server registered**\n\n**Name:** %s\n**Server ID:** `%s`\n**URL:** `%s`\n\nInbound events authenticated with this server's `hs_token` now route to it. Use `/matrix server list` to review.", serverName, serverID, serverURL))
}

func (c *Handler) executeServerRemoveCommand(fields []string) *model.CommandResponse {
	// /matrix server remove <server_id>
	if len(fields) < 4 {
		return ephemeral(serverCommandUsage)
	}
	serverID := fields[3]

	removed, err := c.plugin.RemoveManagedServer(serverID)
	if err != nil {
		c.client.Log.Error("Failed to remove managed server", "error", err, "server_id", serverID)
		return ephemeral(fmt.Sprintf("❌ Failed to remove server: %s", err.Error()))
	}
	if !removed {
		return ephemeral(fmt.Sprintf("No server found with ID `%s`. Use `/matrix server list` to see registered servers.", serverID))
	}

	c.client.Log.Info("Removed managed Matrix server", "server_id", serverID)
	return ephemeral(fmt.Sprintf("✅ Removed server `%s`.\n\nNote: the primary server from the System Console configuration is re-added automatically; only injected servers can be removed permanently.", serverID))
}

// executeServerMapCommand maps the current channel to a Matrix room on a specific
// registered server. Unlike `/matrix map`, which always targets the primary
// server, this stores the mapping under the chosen server's namespace so inbound
// events from that homeserver route to this channel. It upserts the channel
// mapping so a channel already bridged to another server keeps that mapping.
func (c *Handler) executeServerMapCommand(args *model.CommandArgs, fields []string) *model.CommandResponse {
	// /matrix server map <server_id> <room_alias|room_id>
	if len(fields) < 5 {
		return ephemeral(serverCommandUsage)
	}
	serverID := fields[3]
	roomIdentifier := fields[4]

	servers, err := c.plugin.GetManagedServers()
	if err != nil {
		c.client.Log.Error("Failed to read managed servers", "error", err)
		return ephemeral("❌ Failed to read the server registry. Check plugin logs for details.")
	}
	if _, ok := kvstore.ServerConfigForID(servers, serverID); !ok {
		return ephemeral(fmt.Sprintf("No server found with ID `%s`. Use `/matrix server list` to see registered servers.", serverID))
	}

	// Resolve the room alias to a room ID and join the AS bot using the target
	// server's client, so inbound events for the room reach the plugin. The client
	// is best-effort: if it is unavailable, the identifier is stored as given.
	roomID := roomIdentifier
	var joinStatus string
	if client := c.plugin.GetMatrixClientForServer(serverID); client != nil {
		if resolved, resolveErr := client.ResolveRoomAlias(roomIdentifier); resolveErr == nil && resolved != "" {
			roomID = resolved
		}
		if joinErr := client.JoinRoom(roomIdentifier); joinErr != nil {
			c.client.Log.Warn("Failed to join room on target server", "error", joinErr, "server_id", serverID, "room", roomIdentifier)
			joinStatus = "\n\n⚠️ Could not join the room as the bridge bot; make sure the room exists and is joinable on that server."
		}
	} else {
		joinStatus = "\n\n⚠️ No Matrix client is built for that server yet, so the room alias was not resolved. Pass a room ID (`!...`) if inbound events do not arrive."
	}

	channelName := args.ChannelId
	if channel, appErr := c.client.Channel.Get(args.ChannelId); appErr == nil {
		if channel.DisplayName != "" {
			channelName = channel.DisplayName
		} else if channel.Name != "" {
			channelName = channel.Name
		}
	}

	// Upsert the forward mapping so other servers' entries for this channel are
	// preserved, then store the reverse mapping under the target server.
	mappingKey := kvstore.BuildChannelMappingKey(args.ChannelId)
	existing, err := c.kvstore.Get(mappingKey)
	if err != nil {
		c.client.Log.Error("Failed to read channel mapping", "error", err, "channel_id", args.ChannelId)
		return ephemeral("❌ Failed to read the existing channel mapping. Check plugin logs for details.")
	}
	mappings, err := kvstore.ParseChannelServerMappings(existing)
	if err != nil {
		c.client.Log.Error("Corrupt channel mapping value", "error", err, "channel_id", args.ChannelId)
		return ephemeral("❌ The existing channel mapping is corrupt. Use `/matrix unmap` and try again.")
	}
	mappings = kvstore.UpsertChannelServerMapping(mappings, serverID, roomID)
	value, err := kvstore.MarshalChannelServerMappings(mappings)
	if err != nil {
		c.client.Log.Error("Failed to marshal channel mapping", "error", err, "channel_id", args.ChannelId)
		return ephemeral("❌ Failed to save the channel mapping. Check plugin logs for details.")
	}
	if err := c.kvstore.Set(mappingKey, value); err != nil {
		c.client.Log.Error("Failed to save channel mapping", "error", err, "channel_id", args.ChannelId)
		return ephemeral("❌ Failed to save the channel mapping. Check plugin logs for details.")
	}
	if err := c.kvstore.Set(kvstore.BuildRoomMappingKey(serverID, roomID), []byte(args.ChannelId)); err != nil {
		c.client.Log.Error("Failed to save reverse room mapping", "error", err, "server_id", serverID, "room_id", roomID)
		// The forward mapping is saved; inbound routing still works via it.
	}

	// Share the channel so inbound posts attribute to the bridge remote.
	shareStatus := c.shareChannelAndInvitePlugin(args, channelName, fmt.Sprintf("Mapped to Matrix room %s on server %s", roomID, serverID))

	c.client.Log.Info("Mapped channel to room on server", "channel_id", args.ChannelId, "server_id", serverID, "room_id", roomID)
	return ephemeral(fmt.Sprintf("✅ **Mapping saved**\n\n**Channel:** %s\n**Server:** `%s`\n**Matrix room:** `%s`\n\nInbound events from this room now sync to the channel. (Outbound Mattermost→Matrix for non-primary servers is not yet supported.)%s%s", channelName, serverID, roomID, joinStatus, shareStatus))
}

// ephemeral builds an ephemeral command response with the given text.
func ephemeral(text string) *model.CommandResponse {
	return &model.CommandResponse{
		ResponseType: model.CommandResponseTypeEphemeral,
		Text:         text,
	}
}

// shareChannelAndInvitePlugin shares a channel and invites this plugin to receive sync messages
func (c *Handler) shareChannelAndInvitePlugin(args *model.CommandArgs, channelName, purpose string) string {
	sharedChannel := &model.SharedChannel{
		ChannelId:        args.ChannelId,
		TeamId:           args.TeamId,
		Home:             true,
		ReadOnly:         false,
		ShareName:        sanitizeShareName(channelName),
		ShareDisplayName: channelName,
		SharePurpose:     purpose,
		ShareHeader:      "",
		CreatorId:        args.UserId,
		CreateAt:         model.GetMillis(),
		UpdateAt:         model.GetMillis(),
		RemoteId:         "",
	}

	_, shareErr := c.pluginAPI.ShareChannel(sharedChannel)
	if shareErr != nil {
		c.client.Log.Warn("Failed to automatically share channel", "error", shareErr, "channel_id", args.ChannelId)
		return channelSharingFailed
	}

	c.client.Log.Info("Automatically shared channel", "channel_id", args.ChannelId)

	// Invite this plugin to the shared channel to ensure we receive sync messages
	// This is critical - without this invitation, the channel won't receive sync events
	inviteErr := c.pluginAPI.InviteRemoteToChannel(args.ChannelId, c.plugin.GetRemoteID(), args.UserId, false)
	if inviteErr != nil {
		c.client.Log.Error("Failed to invite plugin to shared channel - bridge will not receive sync events", "error", inviteErr, "channel_id", args.ChannelId, "remote_id", c.plugin.GetRemoteID())
		return channelSharingFailed
	}

	c.client.Log.Info("Successfully invited plugin to shared channel", "channel_id", args.ChannelId, "remote_id", c.plugin.GetRemoteID())
	return channelSharingEnabled
}

// extractServerDomain extracts the domain from the Matrix server URL
func (c *Handler) extractServerDomain() string {
	// Get the current plugin configuration
	config := c.plugin.GetConfiguration()
	if config == nil {
		c.client.Log.Warn("Plugin configuration not available")
		return "matrix.org" // fallback
	}

	serverURL := config.GetMatrixServerURL()
	if serverURL == "" {
		c.client.Log.Warn("Matrix server URL not configured")
		return "matrix.org"
	}

	// Get the configured server name (if set)
	configuredServerName := config.GetMatrixServerName()

	// Use ServerDiscovery to determine the server name
	// This will try: configured name -> .well-known discovery -> hostname fallback
	logger := matrix.NewAPILogger(c.pluginAPI)
	discovery := matrix.NewServerDiscovery(logger)
	serverName, err := discovery.DiscoverServerName(serverURL, configuredServerName)
	if err != nil {
		c.client.Log.Warn("Failed to discover Matrix server name", "url", serverURL, "error", err)
		return "matrix.org"
	}

	return serverName
}

func (c *Handler) executeTestCommand(_ *model.CommandArgs) *model.CommandResponse {
	var responseText strings.Builder
	responseText.WriteString("🔍 **Matrix Connection Test**\n\n")

	// Check basic configuration
	config := c.plugin.GetConfiguration()
	if config == nil {
		responseText.WriteString("❌ **Configuration:** Plugin configuration not available\n")
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         responseText.String(),
		}
	}

	serverURL := config.GetMatrixServerURL()
	if serverURL == "" {
		responseText.WriteString("❌ **Configuration:** Matrix server URL not set\n")
		responseText.WriteString("📝 **Action:** Go to System Console → Plugins → Matrix Bridge and set your Matrix server URL\n")
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         responseText.String(),
		}
	}

	responseText.WriteString(fmt.Sprintf("✅ **Server URL:** %s\n", serverURL))

	// Get current Matrix client and check if configured
	matrixClient := c.plugin.GetMatrixClient()
	if matrixClient == nil {
		responseText.WriteString("❌ **Matrix Client:** Not initialized\n")
		responseText.WriteString("📝 **Action:** Check that Application Service and Homeserver tokens are generated\n")
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         responseText.String(),
		}
	}

	responseText.WriteString("✅ **Matrix Client:** Initialized\n")

	// Test Matrix server connection
	err := matrixClient.TestConnection()
	if err != nil {
		responseText.WriteString("❌ **Connection:** Failed to connect to Matrix server\n")
		responseText.WriteString(fmt.Sprintf("🔍 **Error:** %s\n", err.Error()))
		responseText.WriteString("📝 **Actions:**\n")
		responseText.WriteString("   • Verify Matrix server URL is correct and reachable\n")
		responseText.WriteString("   • Check that Application Service registration file is installed\n")
		responseText.WriteString("   • Ensure Matrix homeserver is running\n")
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         responseText.String(),
		}
	}

	responseText.WriteString("✅ **Connection:** Successfully connected to Matrix server\n")

	// Try to get server information (name and version)
	serverInfo, infoErr := matrixClient.GetServerInfo()
	if infoErr == nil && serverInfo != nil {
		if serverInfo.Name != "Matrix Server" || serverInfo.Version != "Unknown" {
			responseText.WriteString(fmt.Sprintf("📊 **Matrix Server:** %s", serverInfo.Name))
			if serverInfo.Version != "Unknown" {
				responseText.WriteString(fmt.Sprintf(" v%s", serverInfo.Version))
			}
			responseText.WriteString("\n")
		}
	}

	// Test Application Service permissions without making invasive changes
	asErr := matrixClient.TestApplicationServicePermissions()
	if asErr != nil {
		responseText.WriteString("❌ **Application Service:** Permission test failed\n")
		responseText.WriteString(fmt.Sprintf("🔍 **Error:** %s\n", asErr.Error()))
		responseText.WriteString("📝 **Actions:**\n")
		responseText.WriteString("   • Verify Application Service registration file is properly installed\n")
		responseText.WriteString("   • Check that homeserver and AS tokens match the registration file\n")
		responseText.WriteString("   • Restart Matrix homeserver if registration file was recently added\n")
	} else {
		responseText.WriteString("✅ **Application Service:** Permissions verified (can query namespace)\n")
	}

	// Test shared channels registration
	responseText.WriteString(testCommandNextSteps)

	return &model.CommandResponse{
		ResponseType: model.CommandResponseTypeEphemeral,
		Text:         responseText.String(),
	}
}

func (c *Handler) executeMigrateCommand(_ *model.CommandArgs) *model.CommandResponse {
	// Get current version before reset
	kvstorage := c.plugin.GetKVStore()
	versionBytes, _ := kvstorage.Get(kvstore.KeyStoreVersion)
	currentVersion := "0"
	if len(versionBytes) > 0 {
		currentVersion = string(versionBytes)
	}

	// Reset KV store version to 0 to force re-migration
	if err := kvstorage.Set(kvstore.KeyStoreVersion, []byte("0")); err != nil {
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         fmt.Sprintf("❌ Failed to reset migration version: %v", err),
		}
	}

	// Run migrations and get detailed results
	migrationResult, err := c.plugin.RunKVStoreMigrationsWithResults()
	if err != nil {
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         fmt.Sprintf("❌ Migration failed: %v", err),
		}
	}

	// Get the results from migration
	userMappingsAdded := migrationResult.UserMappingsCreated
	channelMappingsAdded := migrationResult.ChannelMappingsCreated
	roomMappingsAdded := migrationResult.RoomMappingsCreated
	dmMappingsAdded := migrationResult.DMMappingsCreated
	reverseDMMappingsAdded := migrationResult.ReverseDMMappingsCreated

	return &model.CommandResponse{
		ResponseType: model.CommandResponseTypeEphemeral,
		Text: fmt.Sprintf("✅ **Migration completed successfully!**\n\n"+
			"**Migration Results:**\n"+
			"   • Reset version: %s → %d\n"+
			"   • User reverse mappings created/updated: %d\n"+
			"   • Channel reverse mappings created/updated: %d\n"+
			"   • Room ID mappings created/updated: %d\n"+
			"   • DM mappings migrated: %d\n"+
			"   • DM reverse mappings created: %d\n\n"+
			"This should have resolved any missing or incorrect mappings.\n"+
			"Check the plugin logs for detailed migration information.",
			currentVersion, kvstore.CurrentKVStoreVersion,
			userMappingsAdded, channelMappingsAdded, roomMappingsAdded,
			dmMappingsAdded, reverseDMMappingsAdded),
	}
}
