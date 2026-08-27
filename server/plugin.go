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

	// clusterEventServersChanged is broadcast to every cluster node whenever the server
	// registry is mutated at runtime, so each node's per-node client/remote caches stay
	// in sync with the cluster-shared registry.
	clusterEventServersChanged = "servers_config_changed"
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

	// matrixClients, remoteToServerID, ownRemoteIDs and serverConfigs are per-node caches
	// rebuilt from the cluster-shared server registry by initMatrixClients. The registry
	// itself (KV store) is the source of truth; any runtime mutation must call
	// refreshServersAndBroadcast so every node's copy of these maps stays current.
	matrixClients    map[string]*matrix.Client       // serverID -> client
	remoteToServerID map[string]string               // shared-channels remoteID -> serverID
	ownRemoteIDs     map[string]struct{}             // loop prevention across all our remotes
	serverConfigs    map[string]kvstore.ServerConfig // serverID -> registry entry, for Enabled/HSToken/RemoteID reads on hot paths without a KV round trip

	// matrixClientsLock guards swapping the four maps above.
	matrixClientsLock sync.RWMutex
	// initMatrixClientsMu serializes the read-compute-swap cycle in initMatrixClients so
	// concurrent rebuilds cannot race and leave a stale snapshot installed last.
	initMatrixClientsMu sync.Mutex

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

	// Run KV store migrations first, so the server registry exists before anything else
	// touches it.
	if err := p.runKVStoreMigrations(); err != nil {
		return errors.Wrap(err, "failed to run KV store migrations")
	}

	// Register every server's shared-channels remote, then build clients - clients must
	// be built after registration so each carries its own RemoteID.
	if err := p.registerForSharedChannels(); err != nil {
		p.logger.LogWarn("Failed to register one or more servers for shared channels", "error", err)
	}

	if err := p.initMatrixClients(); err != nil {
		return errors.Wrap(err, "failed to initialize Matrix clients")
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

// initMatrixClients rebuilds the per-node matrixClients/remoteToServerID/ownRemoteIDs/
// serverConfigs caches from the cluster-shared server registry. It builds a client for
// every registered server, including disabled ones, so /matrix status can still probe
// and report their health - only routing (not client construction) consults Enabled.
//
// No-ops when p.kvstore is nil: OnConfigurationChange can fire before OnActivate has
// initialized the store. Returns an error on registry read failure rather than leaving
// the existing maps in place, since a stale-but-silent cache is worse than a visible
// activation failure.
func (p *Plugin) initMatrixClients() error {
	if p.kvstore == nil {
		return nil
	}

	p.initMatrixClientsMu.Lock()
	defer p.initMatrixClientsMu.Unlock()

	servers, err := p.getServers()
	if err != nil {
		return errors.Wrap(err, "failed to read servers config")
	}

	config := p.getConfiguration()
	rateLimitMode := matrix.ParseRateLimitingMode(config.RateLimitingMode)
	rateLimitConfig := matrix.GetRateLimitConfigByMode(rateLimitMode)

	clients := make(map[string]*matrix.Client, len(servers))
	remoteToServerID := make(map[string]string, len(servers))
	ownRemoteIDs := make(map[string]struct{}, len(servers))
	serverConfigs := make(map[string]kvstore.ServerConfig, len(servers))

	for _, s := range servers {
		clients[s.ServerID] = matrix.NewClientWithRateLimit(s.ServerURL, s.ASToken, s.RemoteID, s.ServerName, p.API, rateLimitConfig)
		if s.RemoteID != "" {
			remoteToServerID[s.RemoteID] = s.ServerID
			ownRemoteIDs[s.RemoteID] = struct{}{}
		}
		serverConfigs[s.ServerID] = s
	}

	p.matrixClientsLock.Lock()
	p.matrixClients = clients
	p.remoteToServerID = remoteToServerID
	p.ownRemoteIDs = ownRemoteIDs
	p.serverConfigs = serverConfigs
	p.matrixClientsLock.Unlock()

	return nil
}

// refreshServersAndBroadcast rebuilds this node's Matrix client caches and broadcasts a
// cluster event so every other node does the same. Every runtime registry mutation
// (AddServer, RemoveServer, server enable/disable, server map/unmap) must call this
// rather than a bare initMatrixClients, since the registry is cluster-shared KV but the
// client caches are per-node. A failed broadcast is non-fatal (single-node installs
// have no cluster) - this node's own caches are already correct at that point.
func (p *Plugin) refreshServersAndBroadcast(reason string) error {
	if err := p.initMatrixClients(); err != nil {
		return err
	}

	if appErr := p.API.PublishPluginClusterEvent(
		model.PluginClusterEvent{Id: clusterEventServersChanged, Data: []byte(reason)},
		model.PluginClusterEventSendOptions{SendType: model.PluginClusterEventSendTypeReliable},
	); appErr != nil {
		p.logger.LogWarn("Failed to broadcast servers config change to cluster", "reason", reason, "error", appErr)
	}

	return nil
}

// OnPluginClusterEvent is invoked when another cluster node broadcasts a registry
// mutation. It rebuilds this node's Matrix client caches from the now-current registry.
func (p *Plugin) OnPluginClusterEvent(_ *plugin.Context, ev model.PluginClusterEvent) {
	if ev.Id != clusterEventServersChanged {
		return
	}
	if err := p.initMatrixClients(); err != nil {
		p.logger.LogError("Failed to refresh Matrix clients after cluster event", "error", err)
	}
}

// getMatrixClient returns the client for serverID, or nil if this node has none - either
// because the server isn't registered, or because this node's cache lags a very recent
// registry mutation on another node (the cluster event will catch it up).
func (p *Plugin) getMatrixClient(serverID string) *matrix.Client {
	p.matrixClientsLock.RLock()
	defer p.matrixClientsLock.RUnlock()
	if p.matrixClients == nil {
		return nil
	}
	return p.matrixClients[serverID]
}

// serverIDForRemoteID reverse-resolves one of our own shared-channels remote IDs to the
// server it belongs to. This is the basis of rc-based outbound routing (§3.6).
func (p *Plugin) serverIDForRemoteID(remoteID string) (string, bool) {
	p.matrixClientsLock.RLock()
	defer p.matrixClientsLock.RUnlock()
	if p.remoteToServerID == nil {
		return "", false
	}
	serverID, ok := p.remoteToServerID[remoteID]
	return serverID, ok
}

// isOwnRemoteID reports whether remoteID belongs to one of our own shared-channels
// remotes (any server), for loop prevention. Must reject every one of our remotes, not
// just a single legacy one.
func (p *Plugin) isOwnRemoteID(remoteID string) bool {
	if remoteID == "" {
		return false
	}
	p.matrixClientsLock.RLock()
	defer p.matrixClientsLock.RUnlock()
	if p.ownRemoteIDs == nil {
		return false
	}
	_, ok := p.ownRemoteIDs[remoteID]
	return ok
}

// cachedServerConfig returns the cached registry entry for serverID as of the last
// initMatrixClients rebuild, without touching KV. ok is false if the cache has not been
// built yet, or serverID is not (or no longer) registered as of that rebuild.
func (p *Plugin) cachedServerConfig(serverID string) (kvstore.ServerConfig, bool) {
	p.matrixClientsLock.RLock()
	defer p.matrixClientsLock.RUnlock()
	if p.serverConfigs == nil {
		return kvstore.ServerConfig{}, false
	}
	server, ok := p.serverConfigs[serverID]
	return server, ok
}

// cachedServerConfigs returns a snapshot slice of every registered server as of the last
// initMatrixClients rebuild, without touching KV, for callers that must scan the whole
// registry (e.g. constant-time webhook token matching in MatrixAuthorizationRequired).
// ok is false if the cache has not been built yet.
func (p *Plugin) cachedServerConfigs() ([]kvstore.ServerConfig, bool) {
	p.matrixClientsLock.RLock()
	defer p.matrixClientsLock.RUnlock()
	if p.serverConfigs == nil {
		return nil, false
	}
	servers := make([]kvstore.ServerConfig, 0, len(p.serverConfigs))
	for _, server := range p.serverConfigs {
		servers = append(servers, server)
	}
	return servers, true
}

// serverConfigForRouting returns the effective registry entry for serverID for a hot
// routing path (Enabled/RemoteID checks): the cached snapshot when available, falling
// back to a fresh KV read via serverByID only when the cache has not been built yet or
// does not (yet) contain serverID. This keeps steady-state routing off KV entirely while
// never being less correct than a direct serverByID call would have been.
func (p *Plugin) serverConfigForRouting(serverID string) (kvstore.ServerConfig, error) {
	if server, ok := p.cachedServerConfig(serverID); ok {
		return server, nil
	}
	return p.serverByID(serverID)
}

// remoteIDForServer returns the shared-channels remote ID for serverID, or "" if it is
// not registered or has no remote yet.
func (p *Plugin) remoteIDForServer(serverID string) string {
	server, err := p.serverConfigForRouting(serverID)
	if err != nil {
		return ""
	}
	return server.RemoteID
}

// doRegisterPluginForSharedChannels performs the actual RegisterPluginForSharedChannels
// API call for one siteURL.
func (p *Plugin) doRegisterPluginForSharedChannels(siteURL string) (string, error) {
	botUser, err := p.API.GetUserByUsername("mattermost-bridge")
	var creatorID string
	if err != nil {
		// Fallback to getting any system admin
		users, err2 := p.API.GetUsers(&model.UserGetOptions{
			Page:    0,
			PerPage: 1,
		})
		if err2 != nil || len(users) == 0 {
			return "", errors.New("failed to find a valid creator user")
		}
		creatorID = users[0].Id
	} else {
		creatorID = botUser.Id
	}

	opts := model.RegisterPluginOpts{
		Displayname:  "Matrix_Bridge",
		PluginID:     manifest.Id,
		CreatorID:    creatorID,
		AutoShareDMs: false,
		AutoInvited:  false,
		SiteURL:      siteURL,
	}

	remoteID, appErr := p.API.RegisterPluginForSharedChannels(opts)
	if appErr != nil {
		return "", errors.Wrap(appErr, "failed to register plugin for shared channels")
	}

	return remoteID, nil
}

// registerForSharedChannels registers one shared-channels remote per registered server,
// keyed by each server's own SiteURL, and persists the returned remote IDs in a single
// registry write. The API calls happen outside the mutateServers CAS callback - a CAS
// retry would otherwise re-issue real network calls - and the merge is against whatever
// the registry looks like at write time, so a concurrent AddServer/RemoveServer is never
// clobbered. A failure registering one server is logged and does not block the others.
func (p *Plugin) registerForSharedChannels() error {
	servers, err := p.getServers()
	if err != nil {
		return errors.Wrap(err, "failed to read servers config for shared-channels registration")
	}

	remoteIDs := make(map[string]string, len(servers))
	seenRemoteIDs := make(map[string]string, len(servers)) // remoteID -> the server_id that claimed it first
	for _, s := range servers {
		remoteID, err := p.doRegisterPluginForSharedChannels(s.SiteURL)
		if err != nil {
			p.logger.LogWarn("Failed to register server for shared channels", "server_id", s.ServerID, "error", err)
			continue
		}
		// Each server's SiteURL is already unique in the registry (normalizeServerEndpoint
		// enforces it), so this should never happen in practice - but a colliding remote ID
		// would silently merge two servers' shared-channels state, so guard against it
		// regardless of what actually causes it.
		if owner, ok := seenRemoteIDs[remoteID]; ok {
			p.logger.LogWarn("Shared-channels registration returned a remote ID already claimed by another server; skipping", "server_id", s.ServerID, "remote_id", remoteID, "claimed_by", owner)
			continue
		}
		seenRemoteIDs[remoteID] = s.ServerID
		remoteIDs[s.ServerID] = remoteID
	}

	if len(remoteIDs) == 0 {
		return nil
	}

	return p.mutateServers(func(current []kvstore.ServerConfig) ([]kvstore.ServerConfig, error) {
		updated := make([]kvstore.ServerConfig, len(current))
		copy(updated, current)
		for i := range updated {
			if remoteID, ok := remoteIDs[updated[i].ServerID]; ok {
				updated[i].RemoteID = remoteID
			}
		}
		return updated, nil
	})
}

// bridgeUtilsForServer builds the shared BridgeUtils for one server, on demand. Returns
// an error (never a BridgeUtils with a nil MatrixClient) when this node has no client
// for serverID.
func (p *Plugin) bridgeUtilsForServer(serverID string) (*BridgeUtils, error) {
	client := p.getMatrixClient(serverID)
	if client == nil {
		return nil, errors.Errorf("no Matrix client configured for server %s", serverID)
	}

	return NewBridgeUtils(BridgeUtilsConfig{
		Logger:              p.logger,
		API:                 p.API,
		KVStore:             p.kvstore,
		MatrixClient:        client,
		RemoteID:            p.remoteIDForServer(serverID),
		ServerID:            serverID,
		MaxProfileImageSize: p.maxProfileImageSize,
		MaxFileSize:         p.maxFileSize,
		ChannelMapper:       p,
		ServerConfigLookup:  p.serverConfigForRouting,
	}), nil
}

// newMatrixToMattermostBridge builds a Matrix->Mattermost bridge for serverID, on
// demand. Bridges are not held as Plugin fields since there is one per server.
func (p *Plugin) newMatrixToMattermostBridge(serverID string) (*MatrixToMattermostBridge, error) {
	utils, err := p.bridgeUtilsForServer(serverID)
	if err != nil {
		return nil, err
	}
	return NewMatrixToMattermostBridge(utils), nil
}

// newMattermostToMatrixBridge builds a Mattermost->Matrix bridge for serverID, on
// demand. The post/file trackers are shared singletons keyed internally by
// (serverID, id), so every server's bridge shares the same tracker instances.
func (p *Plugin) newMattermostToMatrixBridge(serverID string) (*MattermostToMatrixBridge, error) {
	utils, err := p.bridgeUtilsForServer(serverID)
	if err != nil {
		return nil, err
	}
	return NewMattermostToMatrixBridge(utils, p.pendingFiles, p.postTracker), nil
}

// PluginAccessor interface implementation for command handlers

// GetKVStore returns the KV store instance
func (p *Plugin) GetKVStore() kvstore.KVStore {
	return p.kvstore
}

// GetPluginAPI returns the Mattermost plugin API
func (p *Plugin) GetPluginAPI() plugin.API {
	return p.API
}

// GetPluginAPIClient returns the pluginapi client
func (p *Plugin) GetPluginAPIClient() *pluginapi.Client {
	return p.client
}

// GetPluginID returns this plugin's ID from the generated manifest, so callers that build
// plugin-relative URLs do not hardcode it and cannot drift from plugin.json.
func (p *Plugin) GetPluginID() string {
	return manifest.Id
}

// GetManagedServers returns every registered Matrix homeserver.
func (p *Plugin) GetManagedServers() ([]kvstore.ServerConfig, error) {
	return p.getServers()
}

// GetMatrixClientForServer returns the Matrix client for serverID, or nil if this node
// has none configured for it.
func (p *Plugin) GetMatrixClientForServer(serverID string) *matrix.Client {
	return p.getMatrixClient(serverID)
}

// GetRemoteIDForServer returns the shared-channels remote ID for serverID.
func (p *Plugin) GetRemoteIDForServer(serverID string) string {
	return p.remoteIDForServer(serverID)
}

// CreateOrGetGhostUserForServer gets an existing ghost user or creates a new one for a
// Mattermost user on a specific Matrix server.
func (p *Plugin) CreateOrGetGhostUserForServer(serverID, mattermostUserID string) (string, error) {
	bridge, err := p.newMattermostToMatrixBridge(serverID)
	if err != nil {
		return "", err
	}
	return bridge.CreateOrGetGhostUser(mattermostUserID)
}

// GetMatrixUserIDFromMattermostUserForServer looks up the original Matrix user ID for a
// remote Mattermost user on a specific Matrix server.
func (p *Plugin) GetMatrixUserIDFromMattermostUserForServer(serverID, mattermostUserID string) (string, error) {
	bridge, err := p.newMattermostToMatrixBridge(serverID)
	if err != nil {
		return "", err
	}
	return bridge.GetMatrixUserIDFromMattermostUser(mattermostUserID)
}

// SetServerEnabled flips a server's Enabled flag and refreshes every node's caches.
// This is a pure flag flip - no re-registration, no re-invites, no cursor reset. The
// shared-channels remote stays registered and the channel invitations stay in place;
// routing alone consults Enabled (§3.11).
func (p *Plugin) SetServerEnabled(serverID string, enabled bool) error {
	err := p.mutateServers(func(servers []kvstore.ServerConfig) ([]kvstore.ServerConfig, error) {
		idx := -1
		for i := range servers {
			if servers[i].ServerID == serverID {
				idx = i
				break
			}
		}
		if idx == -1 {
			return nil, errServerNotRegistered
		}

		updated := make([]kvstore.ServerConfig, len(servers))
		copy(updated, servers)
		updated[idx].Enabled = enabled
		return updated, nil
	})
	if err != nil {
		if errors.Is(err, errServerNotRegistered) {
			return errors.Errorf("server %s is not registered", serverID)
		}
		return err
	}

	return p.refreshServersAndBroadcast("server_enabled_changed")
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

// serverIDForSyncMsg resolves the single Matrix server an outbound SyncMsg should
// target. One shared-channels remote is registered per homeserver and the platform
// invokes the outbound hooks once per invited remote, so this never fans out - it
// resolves exactly one server from rc and the caller acts on that server alone.
//
// The returned error distinguishes an intentional no-op from an operational failure.
// Both outbound hooks (server/hooks.go) must propagate a non-nil error rather than
// swallow it: Mattermost only retains the shared-channels sync cursor and retries the
// batch when the hook returns an error (the inbound webhook path applies the same
// principle with its 503/Retry-After response for the analogous cache-lag case). Turning
// an operational failure into a quiet (_, false, nil) would advance the cursor past
// posts, reactions and files that were never actually delivered.
//
// Resolution:
//  1. rc == nil || rc.RemoteId == "" -> (defensive; should not happen in production) no-op.
//  2. rc.RemoteId doesn't match one of our remotes -> refresh the client caches once, in
//     case this node's remoteToServerID lags a registry mutation made on another node
//     that hasn't reached this node's cluster event handler yet; a refresh failure is an
//     operational failure. Still unresolved after the refresh -> genuinely not one of our
//     remotes, no-op.
//  3. serverConfigForRouting fails -> operational failure (registry read failure).
//  4. The resolved server is disabled -> skip. This is the only thing stopping outbound
//     traffic for a disabled server (§3.11) - its remote stays registered and invited,
//     so the hooks keep firing, and this check is what makes that a no-op.
//  5. getChannelServerMappings fails -> operational failure (KV read/parse failure).
//  6. Membership (not equality) against channelID's mappings, counting only mappings
//     whose server is still registered - a removed server leaves its entry behind, and
//     treating that as "owned elsewhere" would strand the channel forever: mapped to
//     this server -> use it; mapped to another registered server -> skip (that remote is
//     still invited but its server is no longer mapped, do not relay its traffic
//     elsewhere); unmapped, or mapped only to removed servers -> isChannelDirect fails
//     -> operational failure; and the channel is a DM -> use this server anyway, so the
//     DM room can be auto-created on it; and not a DM -> skip.
func (p *Plugin) serverIDForSyncMsg(channelID string, rc *model.RemoteCluster) (serverID string, shouldSync bool, err error) {
	if rc == nil || rc.RemoteId == "" {
		p.logger.LogWarn("SyncMsg received without a usable RemoteCluster; skipping")
		return "", false, nil
	}

	serverID, ok := p.serverIDForRemoteID(rc.RemoteId)
	if !ok {
		if refreshErr := p.initMatrixClients(); refreshErr != nil {
			return "", false, errors.Wrap(refreshErr, "refresh Matrix client cache")
		}
		serverID, ok = p.serverIDForRemoteID(rc.RemoteId)
		if !ok {
			p.logger.LogWarn("SyncMsg received for an unrecognized remote; skipping", "remote_id", rc.RemoteId)
			return "", false, nil
		}
	}

	server, err := p.serverConfigForRouting(serverID)
	if err != nil {
		return "", false, errors.Wrap(err, "get server configuration for routing")
	}
	if !server.Enabled {
		p.logger.LogDebug("SyncMsg resolved to a disabled server; skipping", "server_id", serverID)
		return "", false, nil
	}

	mappings, err := p.getChannelServerMappings(channelID)
	if err != nil {
		return "", false, errors.Wrap(err, "get channel server mappings")
	}

	if kvstore.RoomIDForServer(mappings, serverID) != "" {
		return serverID, true, nil
	}

	// Only a mapping whose server is still registered means "owned by someone else".
	// Removal leaves a channel's entry behind, so counting stale entries here would
	// strand the channel permanently: a DM would never auto-create on the replacement
	// server, and non-DM traffic would be dropped silently rather than read as unmapped.
	// A registered-but-disabled server still owns its channels - remapping is an
	// explicit operator action - so only registration is checked, not Enabled.
	for _, mappedServerID := range kvstore.MappedServerIDs(mappings) {
		if _, err := p.serverConfigForRouting(mappedServerID); err != nil {
			p.logger.LogDebug("Ignoring stale channel mapping for unregistered server", "server_id", mappedServerID, "channel_id", channelID)
			continue
		}
		// Mapped to a live server that is not this one - the remote is still invited,
		// but its server no longer owns this channel's traffic.
		return "", false, nil
	}

	isDM, err := p.isChannelDirect(channelID)
	if err != nil {
		return "", false, errors.Wrap(err, "get channel type")
	}
	if isDM {
		return serverID, true, nil
	}

	return "", false, nil
}

// isChannelDirect reports whether channelID is a direct or group-direct channel.
func (p *Plugin) isChannelDirect(channelID string) (bool, error) {
	channel, appErr := p.API.GetChannel(channelID)
	if appErr != nil {
		return false, appErr
	}
	return channel.Type == model.ChannelTypeDirect || channel.Type == model.ChannelTypeGroup, nil
}

// userOriginatesFromServer reports whether a Matrix-originated (remote) user's home
// server is serverID, so ghosts and invites are not relayed across servers (§3.4).
func (p *Plugin) userOriginatesFromServer(user *model.User, serverID string) bool {
	origin, ok := p.serverIDForRemoteID(user.GetRemoteID())
	return ok && origin == serverID
}

// getChannelServerMappings reads and parses a channel's server mappings. A missing key
// returns (nil, nil) - "unmapped" - which is not an error; a corrupt value is.
func (p *Plugin) getChannelServerMappings(channelID string) ([]kvstore.ChannelServerMapping, error) {
	data, err := p.kvstore.Get(kvstore.BuildChannelMappingKey(channelID))
	if err != nil {
		return nil, errors.Wrap(err, "failed to read channel server mapping")
	}
	return kvstore.ParseChannelServerMappings(data)
}

// UserHasJoinedChannel is called when a user joins or is added to a channel. There is no
// RemoteCluster available here, so - unlike the sync hooks, which resolve a single
// server from rc - this loops over every server the channel is mapped to (one today,
// N once maxServersPerChannel is lifted), skipping unmapped and disabled servers.
func (p *Plugin) UserHasJoinedChannel(_ *plugin.Context, channelMember *model.ChannelMember, actor *model.User) {
	mappings, err := p.getChannelServerMappings(channelMember.ChannelId)
	if err != nil {
		p.logger.LogError("Failed to read channel server mappings", "error", err, "channel_id", channelMember.ChannelId)
		return
	}
	if len(mappings) == 0 {
		// Channel is not bridged to any Matrix server, nothing to do.
		return
	}

	var user *model.User
	if actor != nil && actor.Id == channelMember.UserId {
		user = actor
	} else {
		var appErr *model.AppError
		user, appErr = p.API.GetUser(channelMember.UserId)
		if appErr != nil {
			if actor == nil {
				p.logger.LogError("Failed to get user who joined channel - no actor provided and GetUser API call failed",
					"error", appErr, "user_id", channelMember.UserId, "channel_id", channelMember.ChannelId)
			} else {
				p.logger.LogError("Failed to get user who joined channel - actor provided but user ID mismatch, GetUser API call also failed",
					"error", appErr, "user_id", channelMember.UserId, "actor_id", actor.Id, "channel_id", channelMember.ChannelId)
			}
			return
		}
	}

	for _, serverID := range kvstore.MappedServerIDs(mappings) {
		server, err := p.serverConfigForRouting(serverID)
		if err != nil {
			p.logger.LogDebug("Skipping stale channel mapping for unregistered server", "server_id", serverID, "channel_id", channelMember.ChannelId)
			continue
		}
		if !server.Enabled {
			p.logger.LogDebug("Skipping disabled server for user join sync", "server_id", serverID, "channel_id", channelMember.ChannelId)
			continue
		}

		matrixRoomID := kvstore.RoomIDForServer(mappings, serverID)
		if matrixRoomID == "" {
			continue
		}

		client := p.getMatrixClient(serverID)
		if client == nil {
			p.logger.LogWarn("No Matrix client for mapped server; skipping user join sync", "server_id", serverID, "channel_id", channelMember.ChannelId)
			continue
		}

		p.logger.LogDebug("User joined bridged channel",
			"user_id", user.Id, "username", user.Username, "channel_id", channelMember.ChannelId,
			"server_id", serverID, "matrix_room_id", matrixRoomID, "is_remote", user.IsRemote())

		if user.IsRemote() {
			// Only re-invite a Matrix-originated user to the homeserver they actually
			// came from - inviting them to a different mapped server's room would target
			// a homeserver they have no Matrix identity on.
			if !p.userOriginatesFromServer(user, serverID) {
				continue
			}
			if err := p.inviteRemoteUserToMatrixRoom(serverID, user, channelMember.ChannelId); err != nil {
				p.logger.LogError("Failed to invite remote user to Matrix room", "error", err, "user_id", user.Id, "username", user.Username, "server_id", serverID, "channel_id", channelMember.ChannelId)
			}
			continue
		}

		bridge, err := p.newMattermostToMatrixBridge(serverID)
		if err != nil {
			p.logger.LogError("Failed to build bridge for server", "error", err, "server_id", serverID)
			continue
		}

		ghostUserID, err := bridge.CreateOrGetGhostUser(user.Id)
		if err != nil {
			p.logger.LogError("Failed to create or get ghost user", "error", err, "user_id", user.Id, "username", user.Username, "server_id", serverID)
			continue
		}

		resolvedRoomID, err := client.ResolveRoomAlias(matrixRoomID)
		if err != nil {
			p.logger.LogError("Failed to resolve Matrix room identifier", "error", err, "room_identifier", matrixRoomID, "server_id", serverID)
			continue
		}

		if err := client.InviteAndJoinGhostUser(resolvedRoomID, ghostUserID); err != nil {
			p.logger.LogError("Failed to join ghost user to Matrix room", "error", err, "ghost_user_id", ghostUserID, "room_id", resolvedRoomID, "mattermost_user_id", user.Id, "server_id", serverID)
		} else {
			p.logger.LogInfo("Successfully joined ghost user to Matrix room", "ghost_user_id", ghostUserID, "room_id", resolvedRoomID, "mattermost_user_id", user.Id, "username", user.Username, "server_id", serverID)
		}
	}
}

// See https://developers.mattermost.com/extend/plugins/server/reference/
