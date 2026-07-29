// Package command implements slash command handlers for the Matrix Bridge plugin.
package command

import (
	"fmt"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
	"github.com/mattermost/mattermost/server/public/pluginapi"
	"github.com/pkg/errors"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/matrix"
	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
)

// MigrationResult holds the results of a migration operation
type MigrationResult struct {
	UserMappingsCreated      int
	ChannelMappingsCreated   int
	RoomMappingsCreated      int
	DMMappingsCreated        int
	ReverseDMMappingsCreated int
}

// PluginAccessor defines the interface for plugin functionality needed by command
// handlers. Matrix operations are server-scoped: configured homeservers live in
// the managed server registry, so callers resolve a serverID (explicitly via
// `/matrix server` or implicitly as the sole server) and pass it through.
type PluginAccessor interface {
	// Storage access
	GetKVStore() kvstore.KVStore

	// Mattermost API access
	GetPluginAPI() plugin.API
	GetPluginAPIClient() *pluginapi.Client

	// Migration access
	RunKVStoreMigrations() error
	RunKVStoreMigrationsWithResults() (*MigrationResult, error)

	// Managed server registry access. These back the `/matrix server` command
	// group and the single-server convenience commands.
	GetManagedServers() ([]kvstore.ServerConfig, error)
	AddServer(serverURL, serverName, asToken, hsToken, usernamePrefix string) (string, error)
	RemoveServer(serverID string) (bool, error)

	// Per-server Matrix operations.
	GetMatrixClientForServer(serverID string) *matrix.Client
	GetRemoteIDForServer(serverID string) string
	CreateOrGetGhostUserForServer(serverID, mattermostUserID string) (string, error)
	GetMatrixUserIDFromMattermostUserForServer(serverID, mattermostUserID string) (string, error)
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
func (c *Handler) syncChannelMembersToMatrixRoom(serverID, channelID, roomID string) (int, int, error) {
	matrixClient := c.plugin.GetMatrixClientForServer(serverID)
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
				originalMatrixUserID, err := c.plugin.GetMatrixUserIDFromMattermostUserForServer(serverID, user.Id)
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
				ghostUserID, err := c.plugin.CreateOrGetGhostUserForServer(serverID, user.Id)
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
	matrixCommandUsage = "Usage: /matrix [test|create|map|unmap|list|status|migrate|server] [room_name|room_alias|room_id]"

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
	serverCommandDesc  = "Manage Matrix servers (admin only)"

	// server subcommand usage
	serverCommandUsage = "Usage: /matrix server [list|add|remove|map|unmap|registration|status]\n" +
		"• `/matrix server list` - Show all registered Matrix servers\n" +
		"• `/matrix server add <server_url> <server_name> <as_token> <hs_token> [username_prefix]` - Register/replace a Matrix server\n" +
		"• `/matrix server remove <server_id>` - Remove a registered Matrix server\n" +
		"• `/matrix server map [server_id] <room_alias|room_id>` - Map the current channel to a room on a server\n" +
		"• `/matrix server unmap [server_id]` - Remove the current channel's mapping for a server\n" +
		"• `/matrix server registration [server_id]` - Print the Application Service registration YAML\n" +
		"• `/matrix server status [server_id]` - Show status for a server\n" +
		"(server_id may be omitted when only one server is configured)"
	serverAdminOnlyError = "❌ This command requires System Administrator privileges."

	// Map command usage and validation
	mapCommandUsage     = "Usage: /matrix map [room_alias|room_id]\nExample: /matrix map #test-sync:synapse-mydomain.com"
	roomIdentifierError = "Invalid room identifier format. Use either:\n• Room alias: `#roomname:server.com` (preferred for joining)\n• Room ID: `!roomid:server.com`"

	// Error messages
	matrixClientNotConfigured = "❌ Matrix client not configured. Add a Matrix server with `/matrix server add ...`."
	unknownSubcommandError    = "Unknown subcommand. Use: test, create, map, unmap, list, status, migrate, or server"

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

	// Server management command (admin only): the supported way to manage servers.
	serverCmd := model.NewAutocompleteData("server", "[list|add|remove|map|unmap|registration|status]", serverCommandDesc)
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
	serverMapCmd := model.NewAutocompleteData("map", "[server_id] <room_alias|room_id>", "Map the current channel to a room on a server")
	serverMapCmd.AddTextArgument("Server ID (optional when one server; from /matrix server list)", "[server_id]", "")
	serverMapCmd.AddTextArgument("Matrix room alias or room ID", "<room_alias|room_id>", "")
	serverCmd.AddCommand(serverMapCmd)
	serverUnmapCmd := model.NewAutocompleteData("unmap", "[server_id]", "Remove the current channel's mapping for a server")
	serverUnmapCmd.AddTextArgument("Server ID (optional when one server; from /matrix server list)", "[server_id]", "")
	serverCmd.AddCommand(serverUnmapCmd)
	serverRegistrationCmd := model.NewAutocompleteData("registration", "[server_id]", "Print the Application Service registration YAML")
	serverRegistrationCmd.AddTextArgument("Server ID (optional when one server; from /matrix server list)", "[server_id]", "")
	serverCmd.AddCommand(serverRegistrationCmd)
	serverStatusCmd := model.NewAutocompleteData("status", "[server_id]", "Show status for a Matrix server")
	serverStatusCmd.AddTextArgument("Server ID (optional when one server; from /matrix server list)", "[server_id]", "")
	serverCmd.AddCommand(serverStatusCmd)
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

// resolveServerID resolves the Matrix server a channel-scoped convenience command
// (map/create/unmap/test) should target. With exactly one server registered it
// returns that server; with none or several it returns an ephemeral error
// response guiding the operator to `/matrix server`.
func (c *Handler) resolveServerID() (string, *model.CommandResponse) {
	servers, err := c.plugin.GetManagedServers()
	if err != nil {
		c.client.Log.Error("Failed to read managed servers", "error", err)
		return "", ephemeral("❌ Failed to read the server registry. Check plugin logs for details.")
	}
	switch len(servers) {
	case 0:
		return "", ephemeral("❌ No Matrix server is configured. Add one with `/matrix server add <server_url> <server_name> <as_token> <hs_token> [username_prefix]`.")
	case 1:
		return servers[0].ServerID, nil
	default:
		return "", ephemeral("Multiple Matrix servers are configured, so this command is ambiguous. Use the `/matrix server` subcommands with an explicit server_id (see `/matrix server list`).")
	}
}

// resolveServerIDArg resolves an explicit server_id/domain argument to a serverID.
// An empty arg falls back to the sole registered server (matching the convenience
// commands). It returns an ephemeral error response when the arg is empty and the
// target is ambiguous, or when no server matches the arg.
func (c *Handler) resolveServerIDArg(arg string) (string, *model.CommandResponse) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return c.resolveServerID()
	}
	servers, err := c.plugin.GetManagedServers()
	if err != nil {
		c.client.Log.Error("Failed to read managed servers", "error", err)
		return "", ephemeral("❌ Failed to read the server registry. Check plugin logs for details.")
	}
	// Match by serverID first, then by (normalized) domain / URL host.
	if _, ok := kvstore.ServerConfigForID(servers, arg); ok {
		return arg, nil
	}
	wantHost, _ := matrix.ExtractServerDomain(arg)
	for _, s := range servers {
		if s.ServerName == arg {
			return s.ServerID, nil
		}
		if host, herr := matrix.ExtractServerDomain(s.ServerURL); herr == nil && host != "" && (host == arg || (wantHost != "" && host == wantHost)) {
			return s.ServerID, nil
		}
	}
	return "", ephemeral(fmt.Sprintf("No server found matching `%s`. Use `/matrix server list` to see registered servers.", arg))
}

// matrixClientForServerOrError returns the Matrix client for serverID, or an
// ephemeral error response if none is available.
func (c *Handler) matrixClientForServerOrError(serverID string) (*matrix.Client, *model.CommandResponse) {
	matrixClient := c.plugin.GetMatrixClientForServer(serverID)
	if matrixClient == nil {
		return nil, &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         matrixClientNotConfigured,
		}
	}
	return matrixClient, nil
}

func (c *Handler) executeMapCommand(args *model.CommandArgs, roomIdentifier string) *model.CommandResponse {
	// `/matrix map` targets the sole registered server; with multiple servers the
	// operator must use `/matrix server map <server_id> <room>`.
	serverID, errResponse := c.resolveServerID()
	if errResponse != nil {
		return errResponse
	}
	return c.mapChannelToRoom(args, serverID, roomIdentifier)
}

// mapChannelToRoom maps the current channel to a Matrix room on the given server,
// joining the AS bot and the issuer's ghost, writing both mapping directions
// (upserting so other servers' entries are preserved), and sharing the channel.
func (c *Handler) mapChannelToRoom(args *model.CommandArgs, serverID, roomIdentifier string) *model.CommandResponse {
	// Get the target server's Matrix client and fail fast if not configured
	matrixClient, errResponse := c.matrixClientForServerOrError(serverID)
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
			ghostUserID, err := c.plugin.CreateOrGetGhostUserForServer(serverID, user.Id)
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

	// Save both directions of the mapping. Upsert the forward value so a channel
	// already bridged to another server keeps that entry. Use compare-and-set with
	// retries because the value is shared across servers: two /matrix server map
	// calls for different servers racing on the same channel would otherwise
	// read-modify-write over each other and drop an entry.
	mappingKey := kvstore.BuildChannelMappingKey(args.ChannelId)
	err := c.kvstore.SetAtomicWithRetries(mappingKey, func(old []byte) ([]byte, error) {
		existing, perr := kvstore.ParseChannelServerMappings(old)
		if perr != nil {
			c.client.Log.Warn("Corrupt channel mapping value; overwriting", "error", perr, "channel_id", args.ChannelId)
			existing = nil
		}
		return kvstore.MarshalChannelServerMappings(
			kvstore.UpsertChannelServerMapping(existing, serverID, roomIdentifier))
	})
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
		serverDomain, domainErr := c.extractServerDomain(serverID)
		if domainErr != nil {
			c.client.Log.Warn("Failed to resolve server domain for bridge filtering alias", "error", domainErr, "server_id", serverID)
			// Continue - mapping still works, just no filtering alias
		} else {
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
	}

	// Sync all channel members to the Matrix room (same as /matrix create does)
	var memberSyncStatus string
	roomID := roomIdentifier
	// Resolve to actual room ID if it's an alias
	if resolvedRoomID, err := matrixClient.ResolveRoomAlias(roomIdentifier); err == nil && resolvedRoomID != "" {
		roomID = resolvedRoomID
	}

	joinedCount, totalMembers, syncErr := c.syncChannelMembersToMatrixRoom(serverID, args.ChannelId, roomID)
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
	shareStatus := c.shareChannelAndInvitePlugin(args, channelName, fmt.Sprintf("Mapped to Matrix room: %s", roomIdentifier), serverID)

	return &model.CommandResponse{
		ResponseType: model.CommandResponseTypeEphemeral,
		Text:         fmt.Sprintf("✅ **Mapping Saved**\n\n**Channel:** %s\n**Matrix Room:** `%s`%s%s%s", channelName, roomIdentifier, joinStatus, memberSyncStatus, shareStatus),
	}
}

func (c *Handler) executeUnmapCommand(args *model.CommandArgs, serverID string) *model.CommandResponse {
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
	matrixClient := c.plugin.GetMatrixClientForServer(serverID)
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

	// Remove only this server's entry from the channel->room mapping, preserving
	// any other servers this channel is bridged to. Use compare-and-set with
	// retries, re-deriving `remaining` from the value read at CAS time (not the
	// `mappings` snapshot read earlier in this function) so a concurrent map/unmap
	// for another server isn't clobbered. When no mappings remain, this stores an
	// empty value rather than deleting the key outright - every read path already
	// treats a zero-length parsed mapping list as "unmapped", so this is
	// functionally equivalent while keeping the removal itself race-free.
	var remaining []kvstore.ChannelServerMapping
	err = c.kvstore.SetAtomicWithRetries(channelMappingKey, func(old []byte) ([]byte, error) {
		current, perr := kvstore.ParseChannelServerMappings(old)
		if perr != nil {
			return nil, perr
		}
		remaining = make([]kvstore.ChannelServerMapping, 0, len(current))
		for _, m := range current {
			if m.ServerID != serverID {
				remaining = append(remaining, m)
			}
		}
		return kvstore.MarshalChannelServerMappings(remaining)
	})
	if err != nil {
		c.client.Log.Error("Failed to update channel mapping", "error", err, "channel_id", args.ChannelId, "room_identifier", matrixRoomIdentifier)
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         "❌ **Error:** Failed to update channel mapping. Check plugin logs for details.",
		}
	}

	// Remove the room->channel mapping
	roomMappingKey := kvstore.BuildRoomMappingKey(serverID, matrixRoomIdentifier)
	if err := c.kvstore.Delete(roomMappingKey); err != nil {
		c.client.Log.Warn("Failed to remove room mapping", "error", err, "room_identifier", matrixRoomIdentifier, "channel_id", args.ChannelId)
		// Continue - the main mapping was removed
	}

	// If the mapping was stored as an alias, mapChannelToRoom also wrote a second
	// reverse mapping keyed by the resolved room ID (the key inbound events match
	// on). Remove that one too so it doesn't survive the unmap.
	if strings.HasPrefix(matrixRoomIdentifier, "#") {
		if resolvedRoomID, err := matrixClient.ResolveRoomAlias(matrixRoomIdentifier); err == nil && resolvedRoomID != "" {
			if err := c.kvstore.Delete(kvstore.BuildRoomMappingKey(serverID, resolvedRoomID)); err != nil {
				c.client.Log.Warn("Failed to remove resolved room-ID mapping", "error", err, "room_id", resolvedRoomID, "channel_id", args.ChannelId)
			}
		}
	}

	c.client.Log.Info("Removed Matrix room mapping", "channel_id", args.ChannelId, "room_identifier", matrixRoomIdentifier)

	// Uninvite this plugin from the shared channel only when the channel is no
	// longer bridged to any server; otherwise the remaining server still needs it.
	remoteID := c.plugin.GetRemoteIDForServer(serverID)
	var responseIcon, responseTitle, uninviteStatus string
	if len(remaining) > 0 {
		responseIcon = "✅"
		responseTitle = "**Mapping Removed**"
		uninviteStatus = "\n\n_Channel is still bridged to another Matrix server; the shared-channel remote was left in place._"
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         fmt.Sprintf("%s %s\n\n**Channel:** %s\n**Matrix Room:** `%s`%s", responseIcon, responseTitle, channelName, matrixRoomIdentifier, uninviteStatus),
		}
	}
	uninviteErr := c.pluginAPI.UninviteRemoteFromChannel(args.ChannelId, remoteID)
	if uninviteErr != nil {
		c.client.Log.Warn("Failed to uninvite plugin from shared channel", "error", uninviteErr, "channel_id", args.ChannelId, "remote_id", remoteID)
		responseIcon = "⚠️"
		responseTitle = "**Mapping Partially Removed**"
		uninviteStatus = "\n\n⚠️ **Note:** Failed to uninvite plugin from shared channel. The channel may still receive some sync events."
	} else {
		c.client.Log.Info("Successfully uninvited plugin from shared channel", "channel_id", args.ChannelId, "remote_id", remoteID)
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
	// `/matrix create` targets the sole registered server; with multiple servers
	// the operator must add the room manually and use `/matrix server map`.
	serverID, errResponse := c.resolveServerID()
	if errResponse != nil {
		return errResponse
	}

	// Get the target server's Matrix client and fail fast if not configured
	matrixClient, errResponse := c.matrixClientForServerOrError(serverID)
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
	serverDomain, domainErr := c.extractServerDomain(serverID)
	if domainErr != nil {
		c.client.Log.Error("Failed to resolve server domain for Matrix room creation", "error", domainErr, "server_id", serverID)
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         fmt.Sprintf("❌ Failed to determine Matrix server domain for '%s'. Check plugin logs for details.", roomName),
		}
	}
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
	joinedCount, totalMembers, syncErr := c.syncChannelMembersToMatrixRoom(serverID, args.ChannelId, roomID)
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

	// Automatically map the created room to this channel (both directions). The
	// room is brand new, so a single-entry mapping is correct here.
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
	shareStatus := c.shareChannelAndInvitePlugin(args, channelName, topic, serverID)

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

	// Collect every channel's mappings across all servers; a channel may be
	// bridged to rooms on several servers.
	mappings := make(map[string][]kvstore.ChannelServerMapping)
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
			if len(channelMappings) > 0 {
				mappings[channelID] = channelMappings
			}
		}

		// If we got fewer keys than the batch size, we've reached the end
		if len(keys) < batchSize {
			break
		}

		page++
	}

	// renderRooms formats a channel's mappings as "`room` (server)" segments.
	renderRooms := func(entries []kvstore.ChannelServerMapping) string {
		parts := make([]string, 0, len(entries))
		for _, e := range entries {
			parts = append(parts, fmt.Sprintf("`%s` (`%s`)", e.RoomID, e.ServerID))
		}
		return strings.Join(parts, ", ")
	}

	if len(mappings) == 0 {
		responseText.WriteString("No channel mappings found.\n\n")
		responseText.WriteString(getStartedHelp)
	} else {
		// Show current channel first if it has a mapping
		if current := mappings[args.ChannelId]; len(current) > 0 {
			channel, appErr := c.client.Channel.Get(args.ChannelId)
			channelName := args.ChannelId
			if appErr == nil {
				channelName = channel.DisplayName
				if channelName == "" {
					channelName = channel.Name
				}
			}
			fmt.Fprintf(&responseText, "**Current Channel:** %s → %s\n\n", channelName, renderRooms(current))
		}

		// Show all mappings
		fmt.Fprintf(&responseText, "**All Mappings (%d channels):**\n", len(mappings))
		for channelID, entries := range mappings {
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

			fmt.Fprintf(&responseText, "• %s → %s%s\n", channelName, renderRooms(entries), currentMarker)
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
		serverID, errResponse := c.resolveServerID()
		if errResponse != nil {
			return errResponse
		}
		return c.executeUnmapCommand(args, serverID)
	case "list":
		return c.executeListMappingsCommand(args)
	case "status":
		return c.executeStatusCommand()
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
// the supported way to manage Matrix homeservers (the registry is the sole source
// of truth) until the admin UI lands.
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
	case "unmap":
		return c.executeServerUnmapCommand(args, fields)
	case "registration":
		return c.executeServerRegistrationCommand(fields)
	case "status":
		return c.executeServerStatusCommand(fields)
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
	counts := c.countMappedChannelsByServer()
	for _, s := range servers {
		enabled := "disabled"
		if s.Enabled {
			enabled = "enabled"
		}
		// ServerName (the Matrix domain) is optional — it may be empty when the
		// server relies on .well-known discovery (typical for the migrated
		// single-server entry). Fall back to the URL host so the row is legible.
		displayName := serverDisplayName(s)
		fmt.Fprintf(&b, "\n• **%s** (`%s`)\n", displayName, s.ServerID)
		fmt.Fprintf(&b, "  - URL: `%s`\n", s.ServerURL)
		fmt.Fprintf(&b, "  - Username prefix: `%s`\n", s.UsernamePrefix)
		fmt.Fprintf(&b, "  - Mapped channels: %d\n", counts[s.ServerID])
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

	serverID, err := c.plugin.AddServer(serverURL, serverName, asToken, hsToken, usernamePrefix)
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

	removed, err := c.plugin.RemoveServer(serverID)
	if err != nil {
		c.client.Log.Error("Failed to remove managed server", "error", err, "server_id", serverID)
		return ephemeral(fmt.Sprintf("❌ Failed to remove server: %s", err.Error()))
	}
	if !removed {
		return ephemeral(fmt.Sprintf("No server found with ID `%s`. Use `/matrix server list` to see registered servers.", serverID))
	}

	c.client.Log.Info("Removed managed Matrix server", "server_id", serverID)
	return ephemeral(fmt.Sprintf("✅ Removed server `%s`.\n\nSyncing for this server has stopped. Channel content is not deleted; re-add the same URL to resume.", serverID))
}

// executeStatusCommand shows the bridge status for every configured Matrix
// server: whether it is enabled, its live connection health, and how many
// channels are mapped to it. It is available to all users (no admin gate) and
// exposes no secrets (tokens are never shown).
func (c *Handler) executeStatusCommand() *model.CommandResponse {
	servers, err := c.plugin.GetManagedServers()
	if err != nil {
		c.client.Log.Error("Failed to read managed servers", "error", err)
		return ephemeral("❌ Failed to read the server registry. Check plugin logs for details.")
	}

	var b strings.Builder
	b.WriteString("**Matrix Bridge Status**\n")
	if len(servers) == 0 {
		b.WriteString("\nNo Matrix servers are configured. Add one with `/matrix server add <server_url> <server_name> <as_token> <hs_token> [username_prefix]`.")
		return ephemeral(b.String())
	}

	fmt.Fprintf(&b, "\n%d server(s) configured:\n", len(servers))
	connections := c.probeServerConnections(servers)
	counts := c.countMappedChannelsByServer()
	for _, s := range servers {
		b.WriteString("\n")
		c.writeServerStatus(&b, s, connections, counts)
	}
	return ephemeral(b.String())
}

// executeServerStatusCommand shows the bridge status for a single server. The
// server_id may be omitted when only one server is configured.
//
//	/matrix server status [server_id]
func (c *Handler) executeServerStatusCommand(fields []string) *model.CommandResponse {
	serverArg := ""
	if len(fields) >= 4 {
		serverArg = fields[3]
	}
	serverID, errResponse := c.resolveServerIDArg(serverArg)
	if errResponse != nil {
		return errResponse
	}
	server, ok := c.serverConfig(serverID)
	if !ok {
		return ephemeral(fmt.Sprintf("No server found with ID `%s`. Use `/matrix server list`.", serverID))
	}

	var b strings.Builder
	b.WriteString("**Matrix Server Status**\n\n")
	one := []kvstore.ServerConfig{server}
	c.writeServerStatus(&b, server, c.probeServerConnections(one), c.countMappedChannelsByServer())
	return ephemeral(b.String())
}

// writeServerStatus appends a per-server status block: display name/ID, enabled
// flag, live connection health (only probed for enabled servers), and mapped
// channel count. Connection results and mapped-channel counts are gathered once
// for all servers by the caller and passed in, so rendering performs no I/O.
func (c *Handler) writeServerStatus(b *strings.Builder, s kvstore.ServerConfig, connections map[string]string, counts map[string]int) {
	fmt.Fprintf(b, "• **%s** (`%s`)\n", serverDisplayName(s), s.ServerID)
	fmt.Fprintf(b, "  - URL: `%s`\n", s.ServerURL)
	if !s.Enabled {
		b.WriteString("  - Status: ⏸️ disabled (not syncing)\n")
	} else {
		b.WriteString("  - Status: enabled\n")
		connection, probed := connections[s.ServerID]
		if !probed {
			// Absent means the probe missed the deadline. Say so rather than
			// implying the server is healthy or reporting a failure we never saw.
			connection = "⏱️ timed out (server slow or unreachable)"
		}
		fmt.Fprintf(b, "  - Connection: %s\n", connection)
	}
	fmt.Fprintf(b, "  - Mapped channels: %d\n", counts[s.ServerID])
}

// statusProbeTimeout bounds the combined wait for all homeserver connection
// probes in the status commands. The Matrix HTTP client allows 30s per request,
// so probing several servers one after another could outlast Mattermost's
// slash-command timeout and make `/matrix status` look broken. Probes therefore
// run concurrently under this shared deadline. It is a var so tests can shorten
// it instead of waiting out the real deadline.
var statusProbeTimeout = 8 * time.Second

// probeServerConnections tests connectivity for every enabled server
// concurrently, returning the rendered result keyed by serverID. Servers whose
// probe has not completed by statusProbeTimeout are omitted, so one unreachable
// homeserver degrades a single line of output instead of stalling the command.
func (c *Handler) probeServerConnections(servers []kvstore.ServerConfig) map[string]string {
	type probe struct {
		serverID string
		status   string
	}
	// Buffered to the number of probes so a goroutine finishing after the
	// deadline can still send and exit rather than leaking on a blocked send.
	results := make(chan probe, len(servers))
	pending := 0
	for _, s := range servers {
		if !s.Enabled {
			continue
		}
		pending++
		go func(serverID string) {
			results <- probe{serverID: serverID, status: c.serverConnectionStatus(serverID)}
		}(s.ServerID)
	}

	statuses := make(map[string]string, pending)
	deadline := time.After(statusProbeTimeout)
	for range pending {
		select {
		case r := <-results:
			statuses[r.serverID] = r.status
		case <-deadline:
			return statuses
		}
	}
	return statuses
}

// serverConnectionStatus returns a human-readable connectivity result for a
// server, performing a live TestConnection against its Matrix client.
func (c *Handler) serverConnectionStatus(serverID string) string {
	client := c.plugin.GetMatrixClientForServer(serverID)
	if client == nil {
		return "❌ client not initialized"
	}
	if err := client.TestConnection(); err != nil {
		c.client.Log.Warn("Matrix server connection test failed", "server_id", serverID, "error", err)
		return "❌ connection failed (see plugin logs)"
	}
	return "✅ connected"
}

// countMappedChannelsByServer returns how many channels are bridged to each
// server, keyed by serverID. It scans the channel_mapping_ keyspace once for
// every server: counting per server separately made the status and list commands
// do O(servers × channels) KV reads for the same data.
func (c *Handler) countMappedChannelsByServer() map[string]int {
	counts := make(map[string]int)
	page := 0
	batchSize := 1000
	for {
		keys, err := c.kvstore.ListKeysWithPrefix(page, batchSize, kvstore.KeyPrefixChannelMapping)
		if err != nil {
			// Stop rather than loop, but do not let a partial count pass silently
			// as though it were complete.
			c.client.Log.Warn("Failed to list channel mapping keys; mapped channel counts may be incomplete",
				"error", err, "page", page)
			break
		}
		if len(keys) == 0 {
			break
		}
		for _, key := range keys {
			value, gErr := c.kvstore.Get(key)
			if gErr != nil || len(value) == 0 {
				continue
			}
			mappings, pErr := kvstore.ParseChannelServerMappings(value)
			if pErr != nil {
				continue
			}
			for _, m := range mappings {
				if m.RoomID != "" {
					counts[m.ServerID]++
				}
			}
		}
		if len(keys) < batchSize {
			break
		}
		page++
	}
	return counts
}

// executeServerMapCommand maps the current channel to a Matrix room on a specific
// registered server. The server_id may be omitted when only one server exists.
// It reuses mapChannelToRoom, which upserts the mapping so a channel already
// bridged to another server keeps that mapping.
//
//	/matrix server map [server_id] <room_alias|room_id>
func (c *Handler) executeServerMapCommand(args *model.CommandArgs, fields []string) *model.CommandResponse {
	serverArg, roomIdentifier, ok := parseOptionalServerArg(fields, 3)
	if !ok {
		return ephemeral(serverCommandUsage)
	}
	serverID, errResponse := c.resolveServerIDArg(serverArg)
	if errResponse != nil {
		return errResponse
	}
	return c.mapChannelToRoom(args, serverID, roomIdentifier)
}

// executeServerUnmapCommand removes the current channel's mapping for a specific
// server. The server_id may be omitted when only one server exists.
//
//	/matrix server unmap [server_id]
func (c *Handler) executeServerUnmapCommand(args *model.CommandArgs, fields []string) *model.CommandResponse {
	serverArg := ""
	if len(fields) >= 4 {
		serverArg = fields[3]
	}
	serverID, errResponse := c.resolveServerIDArg(serverArg)
	if errResponse != nil {
		return errResponse
	}
	return c.executeUnmapCommand(args, serverID)
}

// executeServerRegistrationCommand outputs the Application Service registration
// YAML for a server, built server-side from its registry entry. The server_id may
// be omitted when only one server exists.
//
//	/matrix server registration [server_id]
func (c *Handler) executeServerRegistrationCommand(fields []string) *model.CommandResponse {
	serverArg := ""
	if len(fields) >= 4 {
		serverArg = fields[3]
	}
	serverID, errResponse := c.resolveServerIDArg(serverArg)
	if errResponse != nil {
		return errResponse
	}
	servers, err := c.plugin.GetManagedServers()
	if err != nil {
		c.client.Log.Error("Failed to read managed servers", "error", err)
		return ephemeral("❌ Failed to read the server registry. Check plugin logs for details.")
	}
	server, ok := kvstore.ServerConfigForID(servers, serverID)
	if !ok {
		return ephemeral(fmt.Sprintf("No server found with ID `%s`. Use `/matrix server list`.", serverID))
	}

	siteURL := ""
	if cfg := c.pluginAPI.GetConfig(); cfg != nil && cfg.ServiceSettings.SiteURL != nil {
		siteURL = *cfg.ServiceSettings.SiteURL
	}
	yaml, err := buildRegistrationYAML(server, siteURL)
	if err != nil {
		c.client.Log.Warn("Failed to build registration YAML", "error", err, "server_id", server.ServerID)
		return ephemeral(fmt.Sprintf("❌ **Error:** %s", err))
	}
	return ephemeral(fmt.Sprintf("**Application Service registration for `%s`**\n\nSave this as `mattermost-bridge-registration.yaml` on the homeserver:\n\n```yaml\n%s\n```", server.ServerName, yaml))
}

// parseOptionalServerArg parses `[server_id] <value>` starting at index base:
// with one trailing field the server arg is empty and that field is the value;
// with two it is (server_id, value). Returns ok=false if no value is present.
func parseOptionalServerArg(fields []string, base int) (serverArg, value string, ok bool) {
	switch {
	case len(fields) == base+1:
		return "", fields[base], true
	case len(fields) >= base+2:
		return fields[base], fields[base+1], true
	default:
		return "", "", false
	}
}

// ephemeral builds an ephemeral command response with the given text.
func ephemeral(text string) *model.CommandResponse {
	return &model.CommandResponse{
		ResponseType: model.CommandResponseTypeEphemeral,
		Text:         text,
	}
}

// shareChannelAndInvitePlugin shares a channel and invites this plugin's remote
// for the given Matrix server so it receives sync messages.
func (c *Handler) shareChannelAndInvitePlugin(args *model.CommandArgs, channelName, purpose, serverID string) string {
	remoteID := c.plugin.GetRemoteIDForServer(serverID)
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
	inviteErr := c.pluginAPI.InviteRemoteToChannel(args.ChannelId, remoteID, args.UserId, false)
	if inviteErr != nil {
		c.client.Log.Error("Failed to invite plugin to shared channel - bridge will not receive sync events", "error", inviteErr, "channel_id", args.ChannelId, "remote_id", remoteID)
		return channelSharingFailed
	}

	c.client.Log.Info("Successfully invited plugin to shared channel", "channel_id", args.ChannelId, "remote_id", remoteID)
	return channelSharingEnabled
}

// extractServerDomain resolves the Matrix domain (the part after ':' in user/room
// IDs) for the given server from the registry, using .well-known discovery when no
// server name is configured. Returns an error instead of guessing a domain so
// callers do not build aliases/rooms against the wrong homeserver.
func (c *Handler) extractServerDomain(serverID string) (string, error) {
	server, ok := c.serverConfig(serverID)
	if !ok || server.ServerURL == "" {
		return "", errors.Errorf("no registry entry for server %q", serverID)
	}

	// Use ServerDiscovery to determine the server name
	// This will try: configured name -> .well-known discovery -> hostname fallback
	logger := matrix.NewAPILogger(c.pluginAPI)
	discovery := matrix.NewServerDiscovery(logger)
	serverName, err := discovery.DiscoverServerName(server.ServerURL, server.ServerName)
	if err != nil {
		return "", errors.Wrapf(err, "failed to discover Matrix server name for %q", server.ServerURL)
	}

	return serverName, nil
}

// serverConfig returns the registry entry for serverID.
func (c *Handler) serverConfig(serverID string) (kvstore.ServerConfig, bool) {
	servers, err := c.plugin.GetManagedServers()
	if err != nil {
		c.client.Log.Error("Failed to read managed servers", "error", err)
		return kvstore.ServerConfig{}, false
	}
	return kvstore.ServerConfigForID(servers, serverID)
}

// serverDisplayName returns a human-legible label for a server: its Matrix domain
// (ServerName) when set, otherwise the URL host, otherwise the raw URL, and
// finally the serverID so a row is never blank.
func serverDisplayName(s kvstore.ServerConfig) string {
	if s.ServerName != "" {
		return s.ServerName
	}
	if host, err := matrix.ExtractServerDomain(s.ServerURL); err == nil && host != "" {
		return host
	}
	if s.ServerURL != "" {
		return s.ServerURL
	}
	return s.ServerID
}

// buildRegistrationYAML builds the Application Service registration file for a
// server from its registry entry. The namespace domain is the server's Matrix
// domain (ServerName when set, else the URL host) so it matches how ghost users
// and room aliases are named. The output matches the format the webapp previously
// produced for the single-server install. It errors rather than guessing a
// domain, matching extractServerDomain's convention, so callers never emit a
// registration file claiming the wrong homeserver's namespaces.
func buildRegistrationYAML(server kvstore.ServerConfig, mattermostSiteURL string) (string, error) {
	domain := server.ServerName
	if domain == "" {
		host, err := matrix.ExtractServerDomain(server.ServerURL)
		if err != nil || host == "" {
			return "", errors.Errorf("cannot determine Matrix domain for server %q: set ServerName or use a resolvable ServerURL", server.ServerID)
		}
		domain = host
	}
	return fmt.Sprintf(`id: "mattermost-bridge"
url: "%s/plugins/com.mattermost.plugin-matrix-bridge"
as_token: "%s"
hs_token: "%s"
sender_localpart: "_mattermost_bridge"
namespaces:
  users:
    - exclusive: true
      regex: "@_mattermost_.*:%s"
  aliases:
    - exclusive: true
      regex: "#_mattermost_.*:%s"
    - exclusive: false
      regex: "#mattermost-bridge-.*:%s"
  rooms:
    - exclusive: false
      regex: "!.*:%s"
rate_limited: false
protocols: ["mattermost"]
de.sorunome.msc2409.push_ephemeral: true
permissions:
  - "m.room.directory"
  - "m.room.membership"`,
		mattermostSiteURL, server.ASToken, server.HSToken, domain, domain, domain, domain), nil
}

func (c *Handler) executeTestCommand(_ *model.CommandArgs) *model.CommandResponse {
	var responseText strings.Builder
	responseText.WriteString("🔍 **Matrix Connection Test**\n\n")

	// Resolve the target server (sole server, or error when ambiguous/none).
	serverID, errResponse := c.resolveServerID()
	if errResponse != nil {
		return errResponse
	}
	server, ok := c.serverConfig(serverID)
	if !ok || server.ServerURL == "" {
		responseText.WriteString("❌ **Configuration:** Matrix server URL not set\n")
		responseText.WriteString("📝 **Action:** Add a server with `/matrix server add ...`\n")
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         responseText.String(),
		}
	}

	fmt.Fprintf(&responseText, "✅ **Server URL:** %s\n", server.ServerURL)

	// Get the target server's Matrix client and check if configured
	matrixClient := c.plugin.GetMatrixClientForServer(serverID)
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
		fmt.Fprintf(&responseText, "🔍 **Error:** %s\n", err.Error())
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
			fmt.Fprintf(&responseText, "📊 **Matrix Server:** %s", serverInfo.Name)
			if serverInfo.Version != "Unknown" {
				fmt.Fprintf(&responseText, " v%s", serverInfo.Version)
			}
			responseText.WriteString("\n")
		}
	}

	// Test Application Service permissions without making invasive changes
	asErr := matrixClient.TestApplicationServicePermissions()
	if asErr != nil {
		responseText.WriteString("❌ **Application Service:** Permission test failed\n")
		fmt.Fprintf(&responseText, "🔍 **Error:** %s\n", asErr.Error())
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
