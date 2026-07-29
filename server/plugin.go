package main

import (
	"net/http"
	"sync"
	"time"

	"github.com/mattermost/logr/v2"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
	"github.com/mattermost/mattermost/server/public/pluginapi"
	"github.com/mattermost/mattermost/server/public/pluginapi/cluster"
	"github.com/pkg/errors"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/command"
	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/matrix"
	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
)

const (
	// DefaultMaxProfileImageSize is the default maximum size for profile images (6MB)
	DefaultMaxProfileImageSize = 6 * 1024 * 1024
	// DefaultMaxFileSize is the default maximum size for file attachments (50MB)
	DefaultMaxFileSize = 50 * 1024 * 1024
)

// Plugin implements the interface expected by the Mattermost server to communicate between the server and plugin processes.
type Plugin struct {
	plugin.MattermostPlugin

	// kvstore is the client used to read/write KV records for this plugin.
	kvstore kvstore.KVStore

	// client is the Mattermost server API client.
	client *pluginapi.Client

	// commandClient is the client used to register and execute slash commands.
	commandClient command.Command

	// matrixClients holds one Matrix client per configured server, keyed by
	// serverID. Guarded by matrixClientsLock.
	matrixClients map[string]*matrix.Client

	// remoteToServerID maps a shared-channels remote ID to the serverID that owns
	// it, for attributing inbound/loop-prevention events to a homeserver. Rebuilt
	// from the registry in initMatrixClient. Guarded by matrixClientsLock.
	remoteToServerID map[string]string

	// ownRemoteIDs is the set of every remote ID this plugin registered (one per
	// server). A post/reaction/file carrying one of these must not be re-synced
	// (loop prevention). Rebuilt from the registry in initMatrixClient. Guarded by
	// matrixClientsLock.
	ownRemoteIDs map[string]struct{}

	// matrixClientsLock synchronizes access to matrixClients, remoteToServerID and
	// ownRemoteIDs.
	matrixClientsLock sync.RWMutex

	// postTracker tracks post creation timestamps to detect redundant edits
	postTracker *PostTracker

	// pendingFiles tracks uploaded files awaiting their posts
	pendingFiles *PendingFileTracker

	backgroundJob *cluster.Job

	// configurationLock synchronizes access to the configuration.
	configurationLock sync.RWMutex

	// configuration is the active plugin configuration. Consult getConfiguration and
	// setConfiguration for usage.
	configuration *configuration

	// Logr instance specifically for logging Matrix transactions.
	transactionLogger logr.Logger

	// logger is the main logger for the plugin
	logger Logger

	// maxProfileImageSize is the maximum size for profile images in bytes
	maxProfileImageSize int64

	// maxFileSize is the maximum size for file attachments in bytes
	maxFileSize int64
}

// OnActivate is invoked when the plugin is activated. If an error is returned, the plugin will be deactivated.
func (p *Plugin) OnActivate() error {
	var err error
	p.transactionLogger, err = CreateTransactionLogger()
	if err != nil {
		return errors.Wrap(err, "failed to create transaction logger")
	}

	p.client = pluginapi.NewClient(p.API, p.Driver)

	// Initialize the logger
	p.logger = NewPluginAPILogger(p.API)

	p.kvstore = kvstore.NewKVStore(p.client)

	p.postTracker = NewPostTracker(DefaultPostTrackerMaxEntries)
	p.pendingFiles = NewPendingFileTracker()

	// Initialize file size limits with default values
	p.maxProfileImageSize = DefaultMaxProfileImageSize
	p.maxFileSize = DefaultMaxFileSize

	// Run KV store migrations first. On upgrade from a single-server (v2) install
	// the v3 migration seeds the managed server registry from the legacy flat
	// config, so the registry is populated before we register remotes and build
	// clients from it.
	if err := p.runKVStoreMigrations(); err != nil {
		return errors.Wrap(err, "failed to run KV store migrations")
	}

	// Register each server for shared channels so every registry entry carries its
	// own remote ID before the clients are built. This needs no Matrix client.
	if err := p.registerForSharedChannels(); err != nil {
		p.logger.LogWarn("Failed to register for shared channels", "error", err)
	}

	// Build the Matrix clients from the registry (now carrying per-server remote
	// IDs).
	if err := p.initMatrixClients(); err != nil {
		return errors.Wrap(err, "failed to initialize Matrix client")
	}

	p.commandClient = command.NewCommandHandler(p)

	job, err := cluster.Schedule(
		p.API,
		"BackgroundJob",
		cluster.MakeWaitForRoundedInterval(1*time.Hour),
		p.runJob,
	)
	if err != nil {
		return errors.Wrap(err, "failed to schedule background job")
	}

	p.backgroundJob = job

	return nil
}

// OnDeactivate is invoked when the plugin is deactivated.
func (p *Plugin) OnDeactivate() error {
	if p.backgroundJob != nil {
		if err := p.backgroundJob.Close(); err != nil {
			p.logger.LogError("Failed to close background job", "err", err)
		}
	}
	return nil
}

// ExecuteCommand executes the commands that were registered in the NewCommandHandler function.
func (p *Plugin) ExecuteCommand(_ *plugin.Context, args *model.CommandArgs) (*model.CommandResponse, *model.AppError) {
	response, err := p.commandClient.Handle(args)
	if err != nil {
		return nil, model.NewAppError("ExecuteCommand", "plugin.command.execute_command.app_error", nil, err.Error(), http.StatusInternalServerError)
	}
	return response, nil
}

func (p *Plugin) initMatrixClients() error {
	// OnConfigurationChange can run before OnActivate initializes the KV store.
	// The registry lives in the KV store, so defer client setup until it exists;
	// OnActivate calls initMatrixClient once the store is ready.
	if p.kvstore == nil {
		return nil
	}

	// Configured homeservers come from the managed server registry.
	servers, err := p.getServers()
	if err != nil {
		// Surface the failure to the caller rather than leaving the client map
		// silently stale/empty; a failed read must not look like success.
		return errors.Wrap(err, "failed to read server registry")
	}

	config := p.getConfiguration()
	rateLimitMode := matrix.ParseRateLimitingMode(config.RateLimitingMode)
	rateLimitConfig := matrix.GetRateLimitConfigByMode(rateLimitMode)

	clients := make(map[string]*matrix.Client, len(servers))
	remoteToServerID := make(map[string]string, len(servers))
	ownRemoteIDs := make(map[string]struct{}, len(servers))
	for _, server := range servers {
		// Each client carries its own server's remote ID so posts/users it creates
		// attribute to the correct remote.
		clients[server.ServerID] = matrix.NewClientWithRateLimit(
			server.ServerURL,
			server.ASToken,
			server.RemoteID,
			server.ServerName,
			p.API,
			rateLimitConfig,
		)
		if server.RemoteID != "" {
			remoteToServerID[server.RemoteID] = server.ServerID
			ownRemoteIDs[server.RemoteID] = struct{}{}
		}
	}

	p.matrixClientsLock.Lock()
	p.matrixClients = clients
	p.remoteToServerID = remoteToServerID
	p.ownRemoteIDs = ownRemoteIDs
	p.matrixClientsLock.Unlock()

	return nil
}

// getMatrixClient returns the Matrix client for the given serverID, or nil if
// none is registered.
func (p *Plugin) getMatrixClient(serverID string) *matrix.Client {
	p.matrixClientsLock.RLock()
	defer p.matrixClientsLock.RUnlock()
	return p.matrixClients[serverID]
}

// isDirectChannel reports whether channelID is a direct or group DM and, if so,
// its member IDs. It is server-agnostic (inspects only the Mattermost channel),
// so it needs no Matrix server-bound bridge.
func (p *Plugin) isDirectChannel(channelID string) (bool, []string, error) {
	return detectDirectChannel(p.API, channelID)
}

// newMatrixToMattermostBridge builds an inbound bridge scoped to a specific
// serverID, carrying that server's Matrix client and KV namespace. Inbound
// webhook traffic is routed to the originating homeserver (resolved from its
// hs_token), so the bridge that handles an event must be bound to that server
// rather than the single default one. Constructed on demand, mirroring the
// per-call bridge construction in createDMChannelForGhostUser.
func (p *Plugin) newMatrixToMattermostBridge(serverID string) *MatrixToMattermostBridge {
	utils := NewBridgeUtils(BridgeUtilsConfig{
		Logger:       p.logger,
		API:          p.API,
		KVStore:      p.kvstore,
		MatrixClient: p.getMatrixClient(serverID),
		ServerID:     serverID,
		// Attribute inbound posts/users to the ORIGINATING server's remote (not the
		// primary), so a message from server B carries B's remote ID. This keeps
		// per-server loop attribution correct and matches the outbound bridge.
		// Falls back to p.remoteID in the single-server case.
		RemoteID:            p.remoteIDForServer(serverID),
		MaxProfileImageSize: p.maxProfileImageSize,
		MaxFileSize:         p.maxFileSize,
		ConfigGetter:        p,
	})
	return NewMatrixToMattermostBridge(utils)
}

// newMattermostToMatrixBridge builds an outbound bridge scoped to a specific
// serverID, carrying that server's Matrix client, KV namespace and remote ID.
// Outbound traffic is routed to the homeserver(s) the channel is mapped to, so
// the bridge that sends an event must be bound to the target server rather than
// the single default one. The pending-file and post trackers are the shared
// plugin-global instances (they coordinate work that may cross bridge instances,
// e.g. an attachment uploaded here and attached when the post syncs). Constructed
// on demand, mirroring newMatrixToMattermostBridge.
func (p *Plugin) newMattermostToMatrixBridge(serverID string) *MattermostToMatrixBridge {
	utils := NewBridgeUtils(BridgeUtilsConfig{
		Logger:              p.logger,
		API:                 p.API,
		KVStore:             p.kvstore,
		MatrixClient:        p.getMatrixClient(serverID),
		ServerID:            serverID,
		RemoteID:            p.remoteIDForServer(serverID),
		MaxProfileImageSize: p.maxProfileImageSize,
		MaxFileSize:         p.maxFileSize,
		ConfigGetter:        p,
	})
	return NewMattermostToMatrixBridge(utils, p.pendingFiles, p.postTracker)
}

// serverIDForRemoteID returns the serverID that owns a shared-channels remote ID.
func (p *Plugin) serverIDForRemoteID(remoteID string) (string, bool) {
	if remoteID == "" {
		return "", false
	}
	p.matrixClientsLock.RLock()
	serverID, ok := p.remoteToServerID[remoteID]
	p.matrixClientsLock.RUnlock()
	return serverID, ok
}

// isOwnRemoteID reports whether the given remote ID belongs to one of this
// plugin's registered Matrix servers. Used for loop prevention across N servers.
func (p *Plugin) isOwnRemoteID(remoteID string) bool {
	if remoteID == "" {
		return false
	}
	p.matrixClientsLock.RLock()
	_, ok := p.ownRemoteIDs[remoteID]
	p.matrixClientsLock.RUnlock()
	return ok
}

// remoteIDForServer returns the shared-channels remote ID for a server, or "" if
// the server is unknown or has not been registered yet.
func (p *Plugin) remoteIDForServer(serverID string) string {
	servers, err := p.getServers()
	if err == nil {
		if s, ok := kvstore.ServerConfigForID(servers, serverID); ok {
			return s.RemoteID
		}
	}
	return ""
}

// channelServerMappings returns the channel→server mappings for a channel: the
// list of (serverID, roomID) pairs it is bridged to. A missing mapping yields a
// nil slice and no error; a corrupt value is returned as an error.
func (p *Plugin) channelServerMappings(channelID string) ([]kvstore.ChannelServerMapping, error) {
	data, err := p.kvstore.Get(kvstore.BuildChannelMappingKey(channelID))
	if err != nil {
		return nil, errors.Wrap(err, "failed to read channel mapping")
	}
	mappings, err := kvstore.ParseChannelServerMappings(data)
	if err != nil {
		return nil, errors.Wrapf(err, "corrupt channel mapping value for channel %s", channelID)
	}
	return mappings, nil
}

// resolveOutboundServers returns the serverIDs an outbound event for channelID
// should be dispatched to. Resolution order:
//  1. Existing channel_mapping entries — the servers the channel is bridged to
//     (covers mapped regular channels and already-created DMs, one or many).
//  2. An unmapped DM: the homeserver owning the remote (Matrix) participant,
//     resolved from that user's remote ID. This lets a brand-new DM's room be
//     created on the correct server before any mapping exists.
//  3. Otherwise (unmapped non-DM, or a DM with no resolvable remote member): nil,
//     so the caller skips the channel.
func (p *Plugin) resolveOutboundServers(channelID string) ([]string, error) {
	mappings, err := p.channelServerMappings(channelID)
	if err != nil {
		return nil, err
	}
	if len(mappings) > 0 {
		serverIDs := make([]string, 0, len(mappings))
		for _, m := range mappings {
			serverIDs = append(serverIDs, m.ServerID)
		}
		return serverIDs, nil
	}

	// Unmapped: only DMs auto-create a room on first message. Route by the DM's
	// remote participant's homeserver. In a group DM spanning more than one
	// homeserver, the first participant whose remote resolves wins.
	isDM, userIDs, err := p.isDirectChannel(channelID)
	if err != nil {
		// A lookup failure is not the same as "not a DM": surface it rather than
		// silently dropping an outbound event that may have needed a new room.
		p.logger.LogWarn("Failed to determine whether unmapped channel is a DM; skipping outbound routing",
			"error", err, "channel_id", channelID)
		return nil, nil
	}
	if !isDM {
		return nil, nil
	}
	for _, userID := range userIDs {
		user, appErr := p.API.GetUser(userID)
		if appErr != nil {
			continue
		}
		if serverID, ok := p.serverIDForRemoteID(user.GetRemoteID()); ok {
			return []string{serverID}, nil
		}
	}
	return nil, nil
}

func (p *Plugin) registerForSharedChannels() error {
	// Get the bot user ID or use a system admin
	botUser, err := p.API.GetUserByUsername("mattermost-bridge")
	var creatorID string
	if err != nil {
		// Fallback to getting any system admin
		users, err2 := p.API.GetUsers(&model.UserGetOptions{
			Page:    0,
			PerPage: 1,
		})
		if err2 != nil || len(users) == 0 {
			return errors.New("failed to find a valid creator user")
		}
		creatorID = users[0].Id
	} else {
		creatorID = botUser.Id
	}

	servers, serversErr := p.getServers()
	if serversErr != nil {
		return errors.Wrap(serversErr, "failed to read server registry for shared-channels registration")
	}
	if len(servers) == 0 {
		// No server configured yet (e.g. sync disabled with no URL). Nothing to
		// register; leave p.remoteID as-is.
		return nil
	}

	// Register the plugin once per Matrix server so each homeserver is a distinct
	// shared-channels remote (its own remoteID, sync cursors and loop attribution).
	// Re-registration with the same SiteURL is idempotent and preserves cursors.
	// This makes one real network/API call per server, so it must run outside any
	// KV compare-and-set retry loop (a retry would just re-issue the calls). The
	// resulting remoteIDs are collected here and merged into the registry in a
	// single, separate atomic step below.
	remoteIDs := make(map[string]string, len(servers))
	for i := range servers {
		opts := model.RegisterPluginOpts{
			Displayname:  displayNameForServer(servers[i]),
			PluginID:     "com.mattermost.plugin-matrix-bridge",
			CreatorID:    creatorID,
			AutoShareDMs: false,
			AutoInvited:  false,
			SiteURL:      siteURLForServer(servers[i]),
		}

		remoteID, appErr := p.API.RegisterPluginForSharedChannels(opts)
		if appErr != nil {
			p.logger.LogWarn("Failed to register Matrix server for shared channels",
				"error", appErr, "server_id", servers[i].ServerID, "site_url", opts.SiteURL)
			continue
		}
		remoteIDs[servers[i].ServerID] = remoteID

		p.logger.LogInfo("Registered Matrix server for shared channels",
			"server_id", servers[i].ServerID, "remote_id", remoteID, "site_url", opts.SiteURL)
	}

	if len(remoteIDs) == 0 {
		return nil
	}

	// Merge the newly-assigned remote IDs into whatever the registry looks like at
	// write time (not the snapshot read above), so a concurrent AddServer/
	// RemoveServer racing with this call is never clobbered. A server removed
	// concurrently is simply absent from `current` and its remoteID is dropped,
	// which is correct.
	return p.mutateServers(func(current []kvstore.ServerConfig) ([]kvstore.ServerConfig, error) {
		for i := range current {
			if remoteID, ok := remoteIDs[current[i].ServerID]; ok {
				current[i].RemoteID = remoteID
			}
		}
		return current, nil
	})
}

// siteURLForServer returns the shared-channels SiteURL identifying a Matrix
// server's remote. RegisterPluginForSharedChannels keys a remote by SiteURL (it
// must be unique across all remote clusters, enforced by a DB unique index) and
// returns the same remoteID when re-registered with the same SiteURL, so this
// value must be unique and stable per server for each to get its own remoteID.
//
// The value is persisted on the registry entry (ServerConfig.SiteURL). An empty
// SiteURL is reserved for the server migrated from a single-server (v2) install:
// it resolves to the legacy "plugin_<PluginID>" remote, preserving that server's
// existing shared channels across the upgrade. Servers added via
// `/matrix server add` carry a hostname-derived SiteURL (see hostSiteURL).
func siteURLForServer(s kvstore.ServerConfig) string {
	return s.SiteURL
}

// displayNameForServer returns the shared-channels display name for a Matrix
// server's remote. The migrated server (empty SiteURL) keeps the historical
// "Matrix_Bridge" name; other servers include their homeserver so status reports
// are legible.
func displayNameForServer(s kvstore.ServerConfig) string {
	if s.SiteURL == "" {
		return "Matrix_Bridge"
	}
	name := s.ServerName
	if name == "" {
		name = s.ServerURL
	}
	return "Matrix Bridge (" + name + ")"
}

// PluginAccessor interface implementation for command handlers

// GetMatrixClient returns the Matrix client for the single configured server.
func (p *Plugin) GetMatrixClient() *matrix.Client {
	return p.getMatrixClient(p.getSingleServerID())
}

// GetMatrixClientForServer returns the Matrix client for the given serverID, or
// nil if none is registered. It lets commands operate on a specific server.
func (p *Plugin) GetMatrixClientForServer(serverID string) *matrix.Client {
	return p.getMatrixClient(serverID)
}

// GetKVStore returns the KV store instance
func (p *Plugin) GetKVStore() kvstore.KVStore {
	return p.kvstore
}

// CreateOrGetGhostUserForServer gets an existing ghost user or creates a new one
// for a Mattermost user on the given Matrix server.
func (p *Plugin) CreateOrGetGhostUserForServer(serverID, mattermostUserID string) (string, error) {
	return p.newMattermostToMatrixBridge(serverID).CreateOrGetGhostUser(mattermostUserID)
}

// GetMatrixUserIDFromMattermostUserForServer looks up the original Matrix user ID
// for a remote Mattermost user on the given Matrix server.
func (p *Plugin) GetMatrixUserIDFromMattermostUserForServer(serverID, mattermostUserID string) (string, error) {
	return p.newMattermostToMatrixBridge(serverID).GetMatrixUserIDFromMattermostUser(mattermostUserID)
}

// GetPluginAPI returns the Mattermost plugin API
func (p *Plugin) GetPluginAPI() plugin.API {
	return p.API
}

// GetPluginAPIClient returns the pluginapi client
func (p *Plugin) GetPluginAPIClient() *pluginapi.Client {
	return p.client
}

// GetRemoteIDForServer returns the shared-channels remote ID for the given
// Matrix server, or "" if the server is unknown or unregistered.
func (p *Plugin) GetRemoteIDForServer(serverID string) string {
	return p.remoteIDForServer(serverID)
}

// RunKVStoreMigrations exposes migration functionality to command handlers
func (p *Plugin) RunKVStoreMigrations() error {
	return p.runKVStoreMigrations()
}

// RunKVStoreMigrationsWithResults exposes migration functionality to command handlers and returns detailed results
func (p *Plugin) RunKVStoreMigrationsWithResults() (*command.MigrationResult, error) {
	result, err := p.runKVStoreMigrationsWithResults()
	if err != nil {
		return nil, err
	}

	// Convert from internal MigrationResult to command.MigrationResult
	return &command.MigrationResult{
		UserMappingsCreated:      result.UserMappingsCreated,
		ChannelMappingsCreated:   result.ChannelMappingsCreated,
		RoomMappingsCreated:      result.RoomMappingsCreated,
		DMMappingsCreated:        result.DMMappingsCreated,
		ReverseDMMappingsCreated: result.ReverseDMMappingsCreated,
	}, nil
}

// UserHasJoinedChannel is called when a user joins or is added to a channel
func (p *Plugin) UserHasJoinedChannel(_ *plugin.Context, channelMember *model.ChannelMember, actor *model.User) {
	config := p.getConfiguration()
	if !config.EnableSync {
		return
	}

	// A channel may be bridged to several Matrix servers; act on each mapped one.
	mappings, err := p.channelServerMappings(channelMember.ChannelId)
	if err != nil {
		p.logger.LogError("Failed to read channel mapping for user join sync", "error", err, "channel_id", channelMember.ChannelId)
		return
	}
	if len(mappings) == 0 {
		// Channel is not bridged to Matrix, nothing to do
		p.logger.LogDebug("Channel not bridged to Matrix, skipping user join sync", "channel_id", channelMember.ChannelId)
		return
	}

	// Get the user who joined the channel
	// If the actor is the same as the user who joined, use the provided actor to avoid API call
	var user *model.User
	if actor != nil && actor.Id == channelMember.UserId {
		user = actor
	} else {
		var appErr *model.AppError
		user, appErr = p.API.GetUser(channelMember.UserId)
		if appErr != nil {
			// Log the failure with context about both fallback methods
			if actor == nil {
				p.logger.LogError("Failed to get user who joined channel - no actor provided and GetUser API call failed",
					"error", appErr,
					"user_id", channelMember.UserId,
					"channel_id", channelMember.ChannelId,
					"troubleshooting", "both actor parameter and GetUser API call failed")
			} else {
				p.logger.LogError("Failed to get user who joined channel - actor provided but user ID mismatch, GetUser API call also failed",
					"error", appErr,
					"user_id", channelMember.UserId,
					"actor_id", actor.Id,
					"channel_id", channelMember.ChannelId,
					"troubleshooting", "actor user ID did not match channel member user ID, and GetUser API call failed")
			}
			return
		}
	}

	p.logger.LogDebug("User joined bridged channel",
		"user_id", user.Id,
		"username", user.Username,
		"channel_id", channelMember.ChannelId,
		"is_remote", user.IsRemote())

	for _, mapping := range mappings {
		serverID := mapping.ServerID
		matrixClient := p.getMatrixClient(serverID)
		if matrixClient == nil {
			p.logger.LogWarn("No Matrix client for target server; skipping user join sync", "server_id", serverID, "channel_id", channelMember.ChannelId)
			continue
		}

		// If this is a Matrix-originated user (remote), invite them to the
		// corresponding Matrix room only on the homeserver they came from.
		if user.IsRemote() {
			if nativeServerID, ok := p.serverIDForRemoteID(user.GetRemoteID()); ok && nativeServerID == serverID {
				if err := p.inviteRemoteUserToMatrixRoomForServer(serverID, user, channelMember.ChannelId); err != nil {
					p.logger.LogError("Failed to invite remote user to Matrix room", "error", err, "user_id", user.Id, "username", user.Username, "channel_id", channelMember.ChannelId, "server_id", serverID)
				}
			}
			continue
		}

		// This is a local Mattermost user - create ghost user and join them to the Matrix room on this server
		ghostUserID, err := p.newMattermostToMatrixBridge(serverID).CreateOrGetGhostUser(user.Id)
		if err != nil {
			p.logger.LogError("Failed to create or get ghost user", "error", err, "user_id", user.Id, "username", user.Username, "server_id", serverID)
			continue
		}

		// Resolve room alias to room ID if needed
		resolvedRoomID, err := matrixClient.ResolveRoomAlias(mapping.RoomID)
		if err != nil {
			p.logger.LogError("Failed to resolve Matrix room identifier", "error", err, "room_identifier", mapping.RoomID, "server_id", serverID)
			continue
		}

		// Try to join the ghost user to the Matrix room (handles both public and private rooms)
		if err := matrixClient.InviteAndJoinGhostUser(resolvedRoomID, ghostUserID); err != nil {
			p.logger.LogError("Failed to join ghost user to Matrix room", "error", err, "ghost_user_id", ghostUserID, "room_id", resolvedRoomID, "mattermost_user_id", user.Id, "server_id", serverID)
		} else {
			p.logger.LogInfo("Successfully joined ghost user to Matrix room", "ghost_user_id", ghostUserID, "room_id", resolvedRoomID, "mattermost_user_id", user.Id, "username", user.Username, "server_id", serverID)
		}
	}
}

// See https://developers.mattermost.com/extend/plugins/server/reference/
