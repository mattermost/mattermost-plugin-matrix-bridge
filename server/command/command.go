// Package command implements slash command handlers for the Matrix Bridge plugin.
package command

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
	"github.com/mattermost/mattermost/server/public/pluginapi"
	"github.com/pkg/errors"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/matrix"
	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
)

// PluginAccessor defines the interface for plugin functionality needed by command
// handlers. It is server-scoped: every Matrix operation takes a serverID, since the
// plugin bridges N homeservers rather than exactly one.
type PluginAccessor interface {
	// Storage access
	GetKVStore() kvstore.KVStore

	// Server registry
	GetManagedServers() ([]kvstore.ServerConfig, error)
	AddServer(serverURL, asToken, hsToken, usernamePrefix, serverID, serverNameOverride string) (string, error)
	RemoveServer(serverID string) (bool, error)
	SetServerEnabled(serverID string, enabled bool) error

	// Per-server Matrix access
	GetMatrixClientForServer(serverID string) *matrix.Client
	GetRemoteIDForServer(serverID string) string
	CreateOrGetGhostUserForServer(serverID, mattermostUserID string) (string, error)
	GetMatrixUserIDFromMattermostUserForServer(serverID, mattermostUserID string) (string, error)

	// Channel <-> room mapping choke point (see server/channel_mapping.go)
	MapChannelToServer(serverID, channelID, matrixRoomIdentifier string) error
	UnmapChannelFromServer(serverID, channelID string) error

	// Mattermost API access
	GetPluginAPI() plugin.API
	GetPluginAPIClient() *pluginapi.Client
	GetPluginID() string
}

// ServerAutocompleteURL builds the plugin-relative URL the webapp fetches the registered
// Matrix servers from when autocompleting a server_id argument. It must match the route
// registered in ServeHTTP (server/api.go, autocompleteServersPath); pluginID comes from
// the generated manifest via PluginAccessor.GetPluginID rather than being hardcoded.
func ServerAutocompleteURL(pluginID string) string {
	return "/plugins/" + pluginID + "/api/v1/autocomplete/servers"
}

// statusProbeDeadline bounds how long /matrix status and /matrix server status wait for
// all server health probes together. The Matrix HTTP client allows up to 30s per
// request, so probing servers sequentially could outlast Mattermost's slash-command
// timeout; probes run concurrently under this single deadline instead. A var so tests
// can shorten it.
var statusProbeDeadline = 8 * time.Second

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
	matrixCommandUsage = "Usage: /matrix [test|create|map|unmap|list|status|server] ..."

	// Subcommand descriptions for autocomplete
	testCommandDesc    = "Test Matrix server connection and configuration (System Admin only)"
	createCommandDesc  = "Create a new Matrix room and map to current channel (uses channel name if room name not provided) (System Admin only)"
	createCommandHint  = "[room_name] [publish=true|false]"
	mapCommandDesc     = "Map current channel to Matrix room (prefer #alias:server.com) (System Admin only)"
	mapCommandHint     = "[room_alias|room_id]"
	unmapCommandDesc   = "Remove mapping between current channel and Matrix room, and uninvite plugin from shared channel (System Admin only)"
	unmapCommandHint   = ""
	listCommandDesc    = "List all channel-to-room mappings (System Admin only)"
	statusCommandDesc  = "Show bridge status for every configured Matrix server (System Admin only)"
	serverCommandDesc  = "Manage Matrix homeserver registrations (System Admin only)"
	serverCommandHint  = "[subcommand]"
	serverCommandUsage = "Usage: /matrix server [list|add|remove|map|unmap|registration|status|test|enable|disable] ..."
	adminRequiredError = "❌ You must be a System Admin to use Matrix bridge commands."

	// Map command usage and validation
	mapCommandUsage     = "Usage: /matrix map [room_alias|room_id]\nExample: /matrix map #test-sync:synapse-mydomain.com"
	roomIdentifierError = "Invalid room identifier format. Use either:\n• Room alias: `#roomname:server.com` (preferred for joining)\n• Room ID: `!roomid:server.com`"

	// Error messages
	matrixClientNotConfigured = "❌ Matrix client not configured for that server."
	unknownSubcommandError    = "Unknown subcommand. Use: test, create, map, unmap, list, status, or server"

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
		"• `/matrix server add <server_url> <as_token> <hs_token>` - Register a Matrix homeserver\n" +
		"• `/matrix create` - Create new Matrix room using channel name and map to current channel\n" +
		"• `/matrix map [room_alias|room_id]` - Map current channel to existing Matrix room\n"

	commandsHelp = "**Commands:**\n" +
		"• `/matrix map [room_alias|room_id]` - Map current channel to Matrix room\n" +
		"• `/matrix create` - Create new Matrix room using channel name and map to current channel\n" +
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

	testCmd := model.NewAutocompleteData("test", "", testCommandDesc)
	testCmd.RoleID = model.SystemAdminRoleId
	matrixData.AddCommand(testCmd)

	createCmd := model.NewAutocompleteData("create", createCommandHint, createCommandDesc)
	createCmd.AddTextArgument("Optional room name (defaults to channel name)", "[room_name]", "")
	createCmd.AddTextArgument("Optional publish flag", "[publish=true|false]", "")
	createCmd.RoleID = model.SystemAdminRoleId
	matrixData.AddCommand(createCmd)

	mapCmd := model.NewAutocompleteData("map", mapCommandHint, mapCommandDesc)
	mapCmd.AddTextArgument("Matrix room alias or room ID", "[room_alias|room_id]", "")
	mapCmd.RoleID = model.SystemAdminRoleId
	matrixData.AddCommand(mapCmd)

	unmapCmd := model.NewAutocompleteData("unmap", unmapCommandHint, unmapCommandDesc)
	unmapCmd.RoleID = model.SystemAdminRoleId
	matrixData.AddCommand(unmapCmd)

	listCmd := model.NewAutocompleteData("list", "", listCommandDesc)
	listCmd.RoleID = model.SystemAdminRoleId
	matrixData.AddCommand(listCmd)

	statusCmd := model.NewAutocompleteData("status", "", statusCommandDesc)
	statusCmd.RoleID = model.SystemAdminRoleId
	matrixData.AddCommand(statusCmd)

	serverCmd := model.NewAutocompleteData("server", serverCommandHint, serverCommandDesc)
	serverCmd.RoleID = model.SystemAdminRoleId
	serverCmd.AddCommand(model.NewAutocompleteData("list", "", "List every registered Matrix server"))
	addCmd := model.NewAutocompleteData("add", "<server_url> <as_token> <hs_token> [username_prefix]", "Register a new Matrix homeserver")
	addCmd.AddTextArgument("Homeserver base URL", "<server_url>", "")
	addCmd.AddTextArgument("Application Service token", "<as_token>", "")
	addCmd.AddTextArgument("Homeserver token", "<hs_token>", "")
	serverCmd.AddCommand(addCmd)

	// Subcommands taking a server identifier offer the registered servers as a dynamic
	// list, so an admin picks one instead of copying a 26-character ID out of
	// `/matrix server list`. resolveServerIDArg still accepts a server name or URL host
	// typed by hand.
	serversURL := ServerAutocompleteURL(plugin.GetPluginID())
	withServerID := func(name, hint, desc string, required bool) *model.AutocompleteData {
		cmd := model.NewAutocompleteData(name, hint, desc)
		cmd.AddDynamicListArgument("Matrix server", serversURL, required)
		return cmd
	}

	serverCmd.AddCommand(withServerID("remove", "<server_id>", "Remove a registered Matrix server", true))
	serverCmd.AddCommand(withServerID("unmap", "[server_id]", "Unmap current channel from a Matrix server", false))
	serverCmd.AddCommand(withServerID("registration", "[server_id]", "Print the Application Service registration YAML", false))
	serverCmd.AddCommand(withServerID("status", "[server_id]", "Show status for one Matrix server", false))
	serverCmd.AddCommand(withServerID("test", "[server_id]", "Test one Matrix server's connection and Application Service permissions", false))
	serverCmd.AddCommand(withServerID("enable", "<server_id>", "Enable syncing for a Matrix server", true))
	serverCmd.AddCommand(withServerID("disable", "<server_id>", "Disable syncing for a Matrix server", true))

	// `map` takes an optional server id followed by a required room identifier, both
	// positional. With the id omitted the server list is still offered in the first slot;
	// typing a room alias there instead is accepted (see executeServerMapDispatch).
	mapServerCmd := model.NewAutocompleteData("map", "[server_id] <room_alias|room_id>", "Map current channel to a Matrix server's room")
	mapServerCmd.AddDynamicListArgument("Matrix server", serversURL, false)
	mapServerCmd.AddTextArgument("Matrix room alias or room ID", "<room_alias|room_id>", "")
	serverCmd.AddCommand(mapServerCmd)

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

func ephemeral(text string) *model.CommandResponse {
	return &model.CommandResponse{
		ResponseType: model.CommandResponseTypeEphemeral,
		Text:         text,
	}
}

// resolveServerIDArg resolves a user-supplied server identifier. It matches, in order,
// by server_id, then by ServerName, then by URL host. An empty arg is only valid when
// exactly one server is registered.
func (c *Handler) resolveServerIDArg(arg string) (string, error) {
	servers, err := c.plugin.GetManagedServers()
	if err != nil {
		return "", errors.Wrap(err, "failed to list Matrix servers")
	}

	if arg == "" {
		switch len(servers) {
		case 0:
			return "", errors.New("no Matrix servers are registered; use `/matrix server add` first")
		case 1:
			return servers[0].ServerID, nil
		default:
			return "", errors.New("multiple Matrix servers are registered; specify a server_id (see `/matrix server list`)")
		}
	}

	for _, s := range servers {
		if s.ServerID == arg {
			return s.ServerID, nil
		}
	}
	for _, s := range servers {
		if s.ServerName == arg {
			return s.ServerID, nil
		}
	}
	for _, s := range servers {
		if host, err := matrix.ExtractServerDomain(s.ServerURL); err == nil && host != "" && host == arg {
			return s.ServerID, nil
		}
	}

	return "", errors.Errorf("no registered Matrix server matches %q", arg)
}

// resolveSoleServerID resolves the single registered server for the legacy
// single-server commands (test, create, map, unmap). Returns an ephemeral error
// response pointing at /matrix server when zero or several servers are registered.
func (c *Handler) resolveSoleServerID() (string, *model.CommandResponse) {
	servers, err := c.plugin.GetManagedServers()
	if err != nil {
		return "", ephemeral(fmt.Sprintf("❌ Failed to load Matrix servers: %v", err))
	}
	switch len(servers) {
	case 0:
		return "", ephemeral("❌ No Matrix servers are registered. Use `/matrix server add` first.")
	case 1:
		return servers[0].ServerID, nil
	default:
		return "", ephemeral("❌ Multiple Matrix servers are registered. Use `/matrix server map`/`unmap`/`status`/`test` and specify a server_id (see `/matrix server list`).")
	}
}

func (c *Handler) serverByID(serverID string) (*kvstore.ServerConfig, error) {
	servers, err := c.plugin.GetManagedServers()
	if err != nil {
		return nil, err
	}
	for i := range servers {
		if servers[i].ServerID == serverID {
			return &servers[i], nil
		}
	}
	return nil, errors.Errorf("server %s is not registered", serverID)
}

// probeServerHealth concurrently health-checks every server in servers under a single
// deadline, so N servers cost roughly one probe's worth of wall-clock time rather than
// N. Servers whose probe misses the deadline render as "timed out", never as healthy.
func (c *Handler) probeServerHealth(servers []kvstore.ServerConfig) map[string]string {
	results := make(map[string]string, len(servers))
	var mu sync.Mutex
	var wg sync.WaitGroup

	ctx, cancel := context.WithTimeout(context.Background(), statusProbeDeadline)
	defer cancel()

	setResult := func(serverID, status string) {
		mu.Lock()
		results[serverID] = status
		mu.Unlock()
	}

	for _, s := range servers {
		if !s.Enabled {
			setResult(s.ServerID, "disabled")
			continue
		}

		matrixClient := c.plugin.GetMatrixClientForServer(s.ServerID)
		if matrixClient == nil {
			setResult(s.ServerID, "unavailable")
			continue
		}

		wg.Add(1)
		go func(serverID string, client *matrix.Client) {
			defer wg.Done()
			done := make(chan error, 1)
			go func() { done <- client.TestConnection() }()

			select {
			case err := <-done:
				if err != nil {
					setResult(serverID, "unhealthy")
				} else {
					setResult(serverID, "healthy")
				}
			case <-ctx.Done():
				setResult(serverID, "timed out")
			}
		}(s.ServerID, matrixClient)
	}

	wg.Wait()
	return results
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

		for _, member := range pageMembers {
			user, appErr := c.client.User.Get(member.UserId)
			if appErr != nil {
				c.client.Log.Warn("Failed to get user for processing", "error", appErr, "user_id", member.UserId)
				continue
			}

			if user.IsRemote() {
				originalMatrixUserID, err := c.plugin.GetMatrixUserIDFromMattermostUserForServer(serverID, user.Id)
				if err != nil {
					c.client.Log.Warn("Failed to get original Matrix user ID for remote user", "error", err, "user_id", user.Id, "username", user.Username)
					continue
				}

				if err := matrixClient.InviteUserToRoom(roomID, originalMatrixUserID); err != nil {
					c.client.Log.Warn("Failed to invite Matrix user to room", "error", err, "matrix_user_id", originalMatrixUserID, "mattermost_user_id", user.Id, "room_id", roomID)
				} else {
					joinedCount++
				}
			} else {
				ghostUserID, err := c.plugin.CreateOrGetGhostUserForServer(serverID, user.Id)
				if err != nil {
					c.client.Log.Warn("Failed to create or get ghost user", "error", err, "user_id", user.Id, "username", user.Username)
					continue
				}

				if err := matrixClient.InviteAndJoinGhostUser(roomID, ghostUserID); err != nil {
					c.client.Log.Warn("Failed to join ghost user to Matrix room", "error", err, "ghost_user_id", ghostUserID, "user_id", user.Id, "room_id", roomID)
				} else {
					joinedCount++
				}
			}
		}

		offset += limit
	}

	return joinedCount, totalMembers, nil
}

// shareChannelAndInvitePluginForServer shares a channel and invites a specific server's
// shared-channels remote to receive sync messages.
func (c *Handler) shareChannelAndInvitePluginForServer(args *model.CommandArgs, serverID, purpose string) string {
	channel, appErr := c.client.Channel.Get(args.ChannelId)
	channelName := args.ChannelId
	if appErr == nil {
		channelName = channel.DisplayName
		if channelName == "" {
			channelName = channel.Name
		}
	}

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

	if _, err := c.pluginAPI.ShareChannel(sharedChannel); err != nil {
		c.client.Log.Warn("Failed to automatically share channel", "error", err, "channel_id", args.ChannelId)
		return channelSharingFailed
	}

	remoteID := c.plugin.GetRemoteIDForServer(serverID)
	if remoteID == "" {
		c.client.Log.Warn("No shared-channels remote for server; cannot invite to shared channel", "server_id", serverID)
		return channelSharingFailed
	}

	if err := c.pluginAPI.InviteRemoteToChannel(args.ChannelId, remoteID, args.UserId, false); err != nil {
		c.client.Log.Error("Failed to invite plugin to shared channel - bridge will not receive sync events", "error", err, "channel_id", args.ChannelId, "remote_id", remoteID)
		return channelSharingFailed
	}

	return channelSharingEnabled
}

func (c *Handler) mapChannelCore(args *model.CommandArgs, serverID, roomIdentifier string) *model.CommandResponse {
	matrixClient := c.plugin.GetMatrixClientForServer(serverID)
	if matrixClient == nil {
		return ephemeral(matrixClientNotConfigured)
	}

	if (!strings.HasPrefix(roomIdentifier, "!") && !strings.HasPrefix(roomIdentifier, "#")) || !strings.Contains(roomIdentifier, ":") {
		return ephemeral(roomIdentifierError)
	}

	channel, appErr := c.client.Channel.Get(args.ChannelId)
	channelName := args.ChannelId
	if appErr == nil {
		channelName = channel.DisplayName
		if channelName == "" {
			channelName = channel.Name
		}
	}

	var joinStatus string
	if err := matrixClient.JoinRoom(roomIdentifier); err != nil {
		c.client.Log.Warn("Failed to auto-join Matrix room", "error", err, "room_identifier", roomIdentifier)
		joinStatus = autoJoinFailed
	} else {
		user, appErr := c.client.User.Get(args.UserId)
		if appErr != nil {
			joinStatus = autoJoinSuccess
		} else {
			ghostUserID, err := c.plugin.CreateOrGetGhostUserForServer(serverID, user.Id)
			if err != nil {
				joinStatus = autoJoinSuccess
			} else if err := matrixClient.InviteAndJoinGhostUser(roomIdentifier, ghostUserID); err != nil {
				joinStatus = autoJoinSuccess
			} else {
				joinStatus = autoJoinWithUser
			}
		}
	}

	if err := c.plugin.MapChannelToServer(serverID, args.ChannelId, roomIdentifier); err != nil {
		if errors.Is(err, kvstore.ErrChannelAlreadyMapped) {
			return ephemeral("❌ This channel is already mapped to another Matrix server. Use `/matrix server unmap` first.")
		}
		return ephemeral(fmt.Sprintf("❌ Failed to save channel mapping: %v%s", err, joinStatus))
	}

	// Add a bridge alias for Matrix Application Service filtering, best-effort.
	var roomName string
	if strings.HasPrefix(roomIdentifier, "#") {
		parts := strings.Split(roomIdentifier[1:], ":")
		if len(parts) > 0 {
			roomName = parts[0]
		}
	} else {
		roomName = strings.ToLower(strings.ReplaceAll(channelName, " ", "-"))
		roomName = strings.ReplaceAll(roomName, "_", "-")
	}
	if roomName != "" {
		if server, err := c.serverByID(serverID); err == nil {
			bridgeAlias := matrix.CreateBridgeAlias(roomName, server.ServerName)
			if roomID, err := matrixClient.ResolveRoomAlias(roomIdentifier); err == nil && roomID != "" {
				if err := matrixClient.AddRoomAlias(roomID, bridgeAlias); err != nil {
					c.client.Log.Warn("Failed to add bridge filtering alias for manual mapping", "error", err, "bridge_alias", bridgeAlias, "room_id", roomID)
				}
			}
		}
	}

	roomID := roomIdentifier
	if resolvedRoomID, err := matrixClient.ResolveRoomAlias(roomIdentifier); err == nil && resolvedRoomID != "" {
		roomID = resolvedRoomID
	}

	joinedCount, totalMembers, syncErr := c.syncChannelMembersToMatrixRoom(serverID, args.ChannelId, roomID)
	var memberSyncStatus string
	if syncErr != nil {
		c.client.Log.Error("Failed to sync channel members to Matrix room", "error", syncErr, "room_id", roomID, "channel_id", args.ChannelId)
		memberSyncStatus = roomMemberSyncFailed
	} else {
		switch {
		case joinedCount == 0:
			memberSyncStatus = ""
		case joinedCount == 1 && totalMembers == 1:
			memberSyncStatus = roomCreatorWithUserReady
		default:
			memberSyncStatus = fmt.Sprintf("\n\n✅ **All channel members synced to Matrix** - %d of %d users joined the room.", joinedCount, totalMembers)
		}
	}

	shareStatus := c.shareChannelAndInvitePluginForServer(args, serverID, fmt.Sprintf("Mapped to Matrix room: %s", roomIdentifier))

	return ephemeral(fmt.Sprintf("✅ **Mapping Saved**\n\n**Channel:** %s\n**Matrix Room:** `%s`%s%s%s", channelName, roomIdentifier, joinStatus, memberSyncStatus, shareStatus))
}

func (c *Handler) unmapChannelCore(args *model.CommandArgs, serverID string) *model.CommandResponse {
	channel, appErr := c.client.Channel.Get(args.ChannelId)
	channelName := args.ChannelId
	if appErr == nil {
		channelName = channel.DisplayName
		if channelName == "" {
			channelName = channel.Name
		}
	}

	if err := c.plugin.UnmapChannelFromServer(serverID, args.ChannelId); err != nil {
		return ephemeral(fmt.Sprintf("❌ **Error:** %v", err))
	}

	return ephemeral(fmt.Sprintf("✅ **Mapping Removed**\n\n**Channel:** %s", channelName))
}

func (c *Handler) createRoomCore(args *model.CommandArgs, serverID, roomName string, publish bool) *model.CommandResponse {
	matrixClient := c.plugin.GetMatrixClientForServer(serverID)
	if matrixClient == nil {
		return ephemeral(matrixClientNotConfigured)
	}

	server, err := c.serverByID(serverID)
	if err != nil {
		return ephemeral(fmt.Sprintf("❌ %v", err))
	}

	channel, appErr := c.client.Channel.Get(args.ChannelId)
	channelName := args.ChannelId
	if appErr == nil {
		channelName = channel.DisplayName
		if channelName == "" {
			channelName = channel.Name
		}
	}

	if roomName == "" {
		roomName = channelName
	}

	topic := fmt.Sprintf("Matrix room for Mattermost channel: %s", channelName)

	roomID, err := matrixClient.CreateRoom(roomName, topic, server.ServerName, publish, args.ChannelId)
	if err != nil {
		c.client.Log.Error("Failed to create Matrix room", "error", err, "room_name", roomName)
		return ephemeral(fmt.Sprintf("❌ Failed to create Matrix room '%s'. Check plugin logs for details.", roomName))
	}

	joinedCount, totalMembers, syncErr := c.syncChannelMembersToMatrixRoom(serverID, args.ChannelId, roomID)
	var joinStatus string
	if syncErr != nil {
		c.client.Log.Error("Failed to sync channel members to Matrix room", "error", syncErr, "room_id", roomID, "channel_id", args.ChannelId)
		joinStatus = roomMemberSyncFailed
	} else {
		switch {
		case joinedCount == 0:
			joinStatus = roomCreatorJoined
		case joinedCount == 1 && totalMembers == 1:
			joinStatus = roomCreatorWithUserReady
		default:
			joinStatus = fmt.Sprintf("\n\n✅ **All channel members synced to Matrix** - %d of %d users joined the room.", joinedCount, totalMembers)
		}
	}

	if err := c.plugin.MapChannelToServer(serverID, args.ChannelId, roomID); err != nil {
		if errors.Is(err, kvstore.ErrChannelAlreadyMapped) {
			return ephemeral(fmt.Sprintf("✅ **Matrix Room Created:** `%s`\n\n❌ This channel is already mapped to another Matrix server. Use `/matrix server unmap` first.", roomID))
		}
		return ephemeral(fmt.Sprintf("✅ **Matrix Room Created:** `%s`\n\n❌ Failed to save channel mapping: %v", roomID, err))
	}

	shareStatus := c.shareChannelAndInvitePluginForServer(args, serverID, topic)

	publishStatus := notPublishedToDirectory
	if publish {
		publishStatus = publishedToDirectory
	}

	return ephemeral(fmt.Sprintf("✅ **Matrix Room Created & Mapped**\n\n**Room Name:** %s\n**Room ID:** `%s`\n**Channel:** %s%s%s%s", roomName, roomID, channelName, publishStatus, joinStatus, shareStatus))
}

func (c *Handler) testServerConnection(serverID string) *model.CommandResponse {
	var b strings.Builder
	b.WriteString("🔍 **Matrix Connection Test**\n\n")

	server, err := c.serverByID(serverID)
	if err != nil {
		fmt.Fprintf(&b, "❌ %v\n", err)
		return ephemeral(b.String())
	}

	fmt.Fprintf(&b, "✅ **Server URL:** %s\n", server.ServerURL)

	matrixClient := c.plugin.GetMatrixClientForServer(serverID)
	if matrixClient == nil {
		b.WriteString("❌ **Matrix Client:** Not initialized\n")
		return ephemeral(b.String())
	}
	b.WriteString("✅ **Matrix Client:** Initialized\n")

	if err := matrixClient.TestConnection(); err != nil {
		b.WriteString("❌ **Connection:** Failed to connect to Matrix server\n")
		fmt.Fprintf(&b, "🔍 **Error:** %s\n", err.Error())
		return ephemeral(b.String())
	}
	b.WriteString("✅ **Connection:** Successfully connected to Matrix server\n")

	if serverInfo, infoErr := matrixClient.GetServerInfo(); infoErr == nil && serverInfo != nil {
		if serverInfo.Name != "Matrix Server" || serverInfo.Version != "Unknown" {
			fmt.Fprintf(&b, "📊 **Matrix Server:** %s", serverInfo.Name)
			if serverInfo.Version != "Unknown" {
				fmt.Fprintf(&b, " v%s", serverInfo.Version)
			}
			b.WriteString("\n")
		}
	}

	if err := matrixClient.TestApplicationServicePermissions(); err != nil {
		b.WriteString("❌ **Application Service:** Permission test failed\n")
		fmt.Fprintf(&b, "🔍 **Error:** %s\n", err.Error())
	} else {
		b.WriteString("✅ **Application Service:** Permissions verified (can query namespace)\n")
	}

	b.WriteString(testCommandNextSteps)
	return ephemeral(b.String())
}

func (c *Handler) executeListMappingsCommand(args *model.CommandArgs) *model.CommandResponse {
	keys, err := kvstore.ListAllKeysWithPrefix(c.kvstore, kvstore.KeyPrefixChannelMapping, kvstore.DefaultListKeysBatchSize)
	if err != nil {
		return ephemeral(fmt.Sprintf("❌ Failed to retrieve mappings: %v", err))
	}

	servers, _ := c.plugin.GetManagedServers()
	serverNames := make(map[string]string, len(servers))
	for _, s := range servers {
		serverNames[s.ServerID] = s.ServerName
	}

	type row struct {
		channelID, roomID, serverID string
	}
	var rows []row
	for _, key := range keys {
		data, err := c.kvstore.Get(key)
		if err != nil {
			continue
		}
		mappings, err := kvstore.ParseChannelServerMappings(data)
		if err != nil {
			continue
		}
		channelID := strings.TrimPrefix(key, kvstore.KeyPrefixChannelMapping)
		for _, m := range mappings {
			rows = append(rows, row{channelID: channelID, roomID: m.RoomID, serverID: m.ServerID})
		}
	}

	if len(rows) == 0 {
		return ephemeral("No channel mappings found.\n\n" + getStartedHelp)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "**Channel-to-Room Mappings (%d total):**\n\n", len(rows))
	for _, r := range rows {
		channelName := r.channelID
		if channel, appErr := c.client.Channel.Get(r.channelID); appErr == nil {
			channelName = channel.DisplayName
			if channelName == "" {
				channelName = channel.Name
			}
		}

		serverLabel := serverNames[r.serverID]
		if serverLabel == "" {
			serverLabel = r.serverID
		}

		currentMarker := ""
		if r.channelID == args.ChannelId {
			currentMarker = " *(current)*"
		}

		fmt.Fprintf(&b, "• %s → `%s` (%s)%s\n", channelName, r.roomID, serverLabel, currentMarker)
	}

	b.WriteString("\n")
	b.WriteString(commandsHelp)

	return ephemeral(b.String())
}

// executeStatusCommand implements /matrix status:
// every server's enabled state, live connection health
func (c *Handler) executeStatusCommand() *model.CommandResponse {
	servers, err := c.plugin.GetManagedServers()
	if err != nil {
		return ephemeral(fmt.Sprintf("❌ Failed to load Matrix servers: %v", err))
	}
	if len(servers) == 0 {
		return ephemeral("No Matrix servers are registered. Use `/matrix server add` to add one.")
	}

	health := c.probeServerHealth(servers)

	var b strings.Builder
	fmt.Fprintf(&b, "**Matrix Bridge Status (%d server(s)):**\n\n", len(servers))
	for _, s := range servers {
		state := "disabled"
		if s.Enabled {
			state = "enabled"
		}
		fmt.Fprintf(&b, "• **%s** (`%s`) - %s, health: %s\n", s.ServerName, s.ServerID, state, health[s.ServerID])
	}

	return ephemeral(b.String())
}

// --- /matrix server ... ---

func (c *Handler) requireSystemAdmin(userID string) *model.CommandResponse {
	if !c.pluginAPI.HasPermissionTo(userID, model.PermissionManageSystem) {
		return ephemeral(adminRequiredError)
	}
	return nil
}

// stripFlags extracts --flag <value> / --flag=<value> pairs (for any name in
// flagNames) from fields, in any position, returning the remaining positional
// arguments and a map of flag name -> value. An unrecognized --flag is an error, not a
// positional value.
func stripFlags(fields []string, flagNames ...string) ([]string, map[string]string, error) {
	isFlagName := make(map[string]bool, len(flagNames))
	for _, n := range flagNames {
		isFlagName[n] = true
	}

	positional := make([]string, 0, len(fields))
	values := make(map[string]string)

	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if !strings.HasPrefix(f, "--") {
			positional = append(positional, f)
			continue
		}

		body := strings.TrimPrefix(f, "--")
		if name, value, hasEquals := strings.Cut(body, "="); hasEquals {
			if !isFlagName[name] {
				return nil, nil, errors.Errorf("unknown flag %q", f)
			}
			values[name] = value
			continue
		}

		if !isFlagName[body] {
			return nil, nil, errors.Errorf("unknown flag %q", f)
		}
		if i+1 >= len(fields) {
			return nil, nil, errors.Errorf("missing value for --%s", body)
		}
		values[body] = fields[i+1] //nolint:gosec // bounds checked immediately above
		i++
	}

	return positional, values, nil
}

// requireArgs validates a subcommand's positional argument count, returning the usage
// response when it falls outside [minArgs, maxArgs] and nil when it is acceptable.
//
// Every subcommand checks its count through this, including the ones taking no arguments
// at all: extra positionals used to be silently dropped, so a typo or a stray word could
// quietly change which server a command acted on instead of reporting the mistake.
func requireArgs(rest []string, minArgs, maxArgs int, usage string) *model.CommandResponse {
	if len(rest) < minArgs || len(rest) > maxArgs {
		return ephemeral(usage)
	}
	return nil
}

// optionalServerIDArg reads the single optional server identifier from a subcommand's
// remaining arguments.
func optionalServerIDArg(rest []string, usage string) (string, *model.CommandResponse) {
	if resp := requireArgs(rest, 0, 1, usage); resp != nil {
		return "", resp
	}
	if len(rest) == 0 {
		return "", nil
	}
	return rest[0], nil
}

func (c *Handler) executeServerAddCommand(fields []string) *model.CommandResponse {
	positional, flags, err := stripFlags(fields, "server-id", "server-name")
	if err != nil {
		return ephemeral("❌ " + err.Error())
	}

	if len(positional) < 3 || len(positional) > 4 {
		return ephemeral("Usage: /matrix server add <server_url> <as_token> <hs_token> [username_prefix] [--server-id <id>] [--server-name <name>]")
	}

	serverURL := positional[0]
	asToken := positional[1]
	hsToken := positional[2]
	usernamePrefix := ""
	if len(positional) == 4 {
		usernamePrefix = positional[3]
	}

	serverID, err := c.plugin.AddServer(serverURL, asToken, hsToken, usernamePrefix, flags["server-id"], flags["server-name"])
	if err != nil {
		return ephemeral(fmt.Sprintf("❌ Failed to add server: %v", err))
	}

	serverName := serverID
	if server, err := c.serverByID(serverID); err == nil {
		serverName = server.ServerName
	}

	return ephemeral(fmt.Sprintf("✅ **Matrix server added**\n\n**Server ID:** `%s`\n**Server name:** `%s`\n**URL:** %s\n\nUse `/matrix server map %s <room_alias|room_id>` in a channel to bridge it.",
		serverID, serverName, serverURL, serverID))
}

func (c *Handler) executeServerRemoveCommand(serverID string) *model.CommandResponse {
	removed, err := c.plugin.RemoveServer(serverID)
	if err != nil {
		return ephemeral(fmt.Sprintf("❌ Failed to remove server: %v", err))
	}
	if !removed {
		return ephemeral(fmt.Sprintf("❌ No server found with ID `%s`.", serverID))
	}

	return ephemeral(fmt.Sprintf("✅ **Server removed**\n\nServer `%s`'s channel mappings and ghost users were kept, not deleted. "+
		"To restore this server and reconnect them, run:\n`/matrix server add <server_url> <as_token> <hs_token> --server-id %s`",
		serverID, serverID))
}

func (c *Handler) executeServerListCommand() *model.CommandResponse {
	servers, err := c.plugin.GetManagedServers()
	if err != nil {
		return ephemeral(fmt.Sprintf("❌ Failed to list servers: %v", err))
	}
	if len(servers) == 0 {
		return ephemeral("No Matrix servers are registered. Use `/matrix server add` to add one.")
	}

	var b strings.Builder
	fmt.Fprintf(&b, "**Matrix Servers (%d):**\n\n", len(servers))
	for _, s := range servers {
		state := "disabled"
		if s.Enabled {
			state = "enabled"
		}
		usernamePrefix := s.UsernamePrefix
		if usernamePrefix == "" {
			usernamePrefix = "matrix"
		}
		fmt.Fprintf(&b, "• **%s** (`%s`)\n   URL: %s\n   Username prefix: `%s`\n   State: %s\n\n",
			s.ServerName, s.ServerID, s.ServerURL, usernamePrefix, state)
	}

	return ephemeral(b.String())
}

// executeServerMapDispatch takes the server identifier as an optional leading positional:
// `[server_id] <room_alias|room_id>`. One argument is the room; two are the server and the
// room.
func (c *Handler) executeServerMapDispatch(args *model.CommandArgs, rest []string) *model.CommandResponse {
	if resp := requireArgs(rest, 1, 2, "Usage: /matrix server map [server_id] <room_alias|room_id>"); resp != nil {
		return resp
	}

	var serverIDArg, roomIdentifier string
	if len(rest) == 1 {
		roomIdentifier = rest[0]
	} else {
		serverIDArg = rest[0]
		roomIdentifier = rest[1]
	}

	serverID, err := c.resolveServerIDArg(serverIDArg)
	if err != nil {
		return ephemeral("❌ " + err.Error())
	}

	return c.mapChannelCore(args, serverID, roomIdentifier)
}

func (c *Handler) executeServerRegistrationCommand(serverIDArg string) *model.CommandResponse {
	serverID, err := c.resolveServerIDArg(serverIDArg)
	if err != nil {
		return ephemeral("❌ " + err.Error())
	}

	server, err := c.serverByID(serverID)
	if err != nil {
		return ephemeral(fmt.Sprintf("❌ %v", err))
	}

	siteURL := ""
	if cfg := c.pluginAPI.GetConfig(); cfg != nil && cfg.ServiceSettings.SiteURL != nil {
		siteURL = *cfg.ServiceSettings.SiteURL
	}
	// The registration url is the plugin's base path ONLY. The homeserver appends the
	// appservice path itself ("/_matrix/app/v1/transactions/{txnId}" - see the router in
	// server/api.go), so including "/_matrix/app/v1" here produces a doubled path that
	// matches no route and silently breaks all inbound traffic for that server.
	webhookURL := strings.TrimSuffix(siteURL, "/") + "/plugins/" + c.plugin.GetPluginID()

	registrationYAML := fmt.Sprintf(`id: mattermost-bridge-%s
url: %s
as_token: %s
hs_token: %s
sender_localpart: _mattermost_bot
rate_limited: false
namespaces:
  users:
    - exclusive: true
      regex: '@_mattermost_.*'
  aliases:
    - exclusive: false
      regex: '#mattermost-bridge-.*'
  rooms: []
`, server.ServerID, webhookURL, server.ASToken, server.HSToken)

	return ephemeral(fmt.Sprintf("**Application Service registration for %s (`%s`):**\n```yaml\n%s```", server.ServerName, server.ServerID, registrationYAML))
}

func (c *Handler) executeServerStatusCommand(serverIDArg string) *model.CommandResponse {
	serverID, err := c.resolveServerIDArg(serverIDArg)
	if err != nil {
		return ephemeral("❌ " + err.Error())
	}

	server, err := c.serverByID(serverID)
	if err != nil {
		return ephemeral(fmt.Sprintf("❌ %v", err))
	}

	health := c.probeServerHealth([]kvstore.ServerConfig{*server})

	state := "disabled"
	if server.Enabled {
		state = "enabled"
	}

	return ephemeral(fmt.Sprintf("**Matrix Server Status**\n\n**Name:** %s\n**ID:** `%s`\n**URL:** %s\n**State:** %s\n**Health:** %s",
		server.ServerName, server.ServerID, server.ServerURL, state, health[server.ServerID]))
}

func (c *Handler) executeServerEnableCommand(serverID string, enabled bool) *model.CommandResponse {
	if err := c.plugin.SetServerEnabled(serverID, enabled); err != nil {
		return ephemeral(fmt.Sprintf("❌ Failed to update server: %v", err))
	}
	state := "disabled"
	if enabled {
		state = "enabled"
	}
	return ephemeral(fmt.Sprintf("✅ Server `%s` is now **%s**.", serverID, state))
}

func (c *Handler) executeServerGroup(args *model.CommandArgs, fields []string) *model.CommandResponse {
	if resp := c.requireSystemAdmin(args.UserId); resp != nil {
		return resp
	}

	if len(fields) == 0 {
		return ephemeral(serverCommandUsage)
	}

	sub := fields[0]
	rest := fields[1:]

	switch sub {
	case "list":
		if resp := requireArgs(rest, 0, 0, "Usage: /matrix server list"); resp != nil {
			return resp
		}
		return c.executeServerListCommand()
	case "add":
		return c.executeServerAddCommand(rest)
	case "remove":
		if resp := requireArgs(rest, 1, 1, "Usage: /matrix server remove <server_id>"); resp != nil {
			return resp
		}
		serverID, err := c.resolveServerIDArg(rest[0])
		if err != nil {
			return ephemeral("❌ " + err.Error())
		}
		return c.executeServerRemoveCommand(serverID)
	case "map":
		return c.executeServerMapDispatch(args, rest)
	case "unmap":
		serverIDArg, errResp := optionalServerIDArg(rest, "Usage: /matrix server unmap [server_id]")
		if errResp != nil {
			return errResp
		}
		serverID, err := c.resolveServerIDArg(serverIDArg)
		if err != nil {
			return ephemeral("❌ " + err.Error())
		}
		return c.unmapChannelCore(args, serverID)
	case "registration":
		serverIDArg, errResp := optionalServerIDArg(rest, "Usage: /matrix server registration [server_id]")
		if errResp != nil {
			return errResp
		}
		return c.executeServerRegistrationCommand(serverIDArg)
	case "status":
		serverIDArg, errResp := optionalServerIDArg(rest, "Usage: /matrix server status [server_id]")
		if errResp != nil {
			return errResp
		}
		return c.executeServerStatusCommand(serverIDArg)
	case "test":
		serverIDArg, errResp := optionalServerIDArg(rest, "Usage: /matrix server test [server_id]")
		if errResp != nil {
			return errResp
		}
		serverID, err := c.resolveServerIDArg(serverIDArg)
		if err != nil {
			return ephemeral("❌ " + err.Error())
		}
		return c.testServerConnection(serverID)
	case "enable", "disable":
		if resp := requireArgs(rest, 1, 1, fmt.Sprintf("Usage: /matrix server %s <server_id>", sub)); resp != nil {
			return resp
		}
		serverID, err := c.resolveServerIDArg(rest[0])
		if err != nil {
			return ephemeral("❌ " + err.Error())
		}
		return c.executeServerEnableCommand(serverID, sub == "enable")
	default:
		return ephemeral(serverCommandUsage)
	}
}

func (c *Handler) executeMatrixCommand(args *model.CommandArgs) *model.CommandResponse {
	fields := strings.Fields(args.Command)
	if len(fields) < 2 {
		return ephemeral(matrixCommandUsage)
	}

	subcommand := fields[1]

	// Every subcommand is System Admin only
	if resp := c.requireSystemAdmin(args.UserId); resp != nil {
		return resp
	}

	// rest is every positional after the subcommand. `create` is excluded from the strict
	// count below: its room name is deliberately variadic (unquoted multi-word names are
	// joined back together), so it validates its own arguments.
	rest := fields[2:]

	switch subcommand {
	case "test":
		if resp := requireArgs(rest, 0, 0, "Usage: /matrix test"); resp != nil {
			return resp
		}
		serverID, errResp := c.resolveSoleServerID()
		if errResp != nil {
			return errResp
		}
		return c.testServerConnection(serverID)
	case "create":
		var roomName string
		publish := false

		switch {
		case len(fields) == 2:
			roomName = ""
		case len(fields) == 3:
			arg := fields[2]
			if arg == "true" || arg == "false" || strings.HasPrefix(arg, "publish=") {
				roomName = ""
				if publishValue, ok := strings.CutPrefix(arg, "publish="); ok {
					publish = publishValue == "true"
				} else {
					publish = arg == "true"
				}
			} else {
				roomName = arg
			}
		default:
			lastField := fields[len(fields)-1]
			if lastField == "true" || lastField == "false" || strings.HasPrefix(lastField, "publish=") {
				if publishValue, ok := strings.CutPrefix(lastField, "publish="); ok {
					publish = publishValue == "true"
				} else {
					publish = lastField == "true"
				}
				roomName = strings.Join(fields[2:len(fields)-1], " ")
			} else {
				roomName = strings.Join(fields[2:], " ")
			}
		}

		roomName = strings.Trim(roomName, "\"'")

		serverID, errResp := c.resolveSoleServerID()
		if errResp != nil {
			return errResp
		}
		return c.createRoomCore(args, serverID, roomName, publish)
	case "map":
		if resp := requireArgs(rest, 1, 1, mapCommandUsage); resp != nil {
			return resp
		}
		serverID, errResp := c.resolveSoleServerID()
		if errResp != nil {
			return errResp
		}
		return c.mapChannelCore(args, serverID, rest[0])
	case "unmap":
		if resp := requireArgs(rest, 0, 0, "Usage: /matrix unmap"); resp != nil {
			return resp
		}
		serverID, errResp := c.resolveSoleServerID()
		if errResp != nil {
			return errResp
		}
		return c.unmapChannelCore(args, serverID)
	case "list":
		if resp := requireArgs(rest, 0, 0, "Usage: /matrix list"); resp != nil {
			return resp
		}
		return c.executeListMappingsCommand(args)
	case "status":
		if resp := requireArgs(rest, 0, 0, "Usage: /matrix status"); resp != nil {
			return resp
		}
		return c.executeStatusCommand()
	case "server":
		return c.executeServerGroup(args, rest)
	default:
		return ephemeral(unknownSubcommandError)
	}
}
