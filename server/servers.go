package main

import (
	"net/url"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/pkg/errors"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/matrix"
	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
)

// getServers returns every registered server. A missing registry key is not an error -
// it returns (nil, nil), meaning "no servers registered yet".
func (p *Plugin) getServers() ([]kvstore.ServerConfig, error) {
	data, err := p.kvstore.Get(kvstore.KeyServersConfig)
	if err != nil {
		return nil, errors.Wrap(err, "failed to read servers config")
	}
	return kvstore.ParseServersConfig(data)
}

// errServerNotRegistered is returned by a mutateServers callback whose target server is
// absent from the slice it was handed.
var errServerNotRegistered = errors.New("server is not registered")

// mutateServers atomically reads-modifies-writes the server registry via compare-and-set.
// mutator must be a pure function of the slice it is given: SetAtomicWithRetries may
// invoke it more than once if a concurrent writer wins the race, so it must not perform
// network or plugin-API calls and must not leak state across invocations.
func (p *Plugin) mutateServers(mutator func([]kvstore.ServerConfig) ([]kvstore.ServerConfig, error)) error {
	return p.kvstore.SetAtomicWithRetries(kvstore.KeyServersConfig, func(oldValue []byte) ([]byte, error) {
		servers, err := kvstore.ParseServersConfig(oldValue)
		if err != nil {
			return nil, err
		}

		newServers, err := mutator(servers)
		if err != nil {
			return nil, err
		}

		return kvstore.MarshalServersConfig(newServers)
	})
}

// normalizeServerEndpoint parses rawURL and returns its "host:port" form, lowercased,
// with the default port filled in from the scheme (http->80, https->443). This is the
// registry's uniqueness key: two entries may never share an endpoint.
func normalizeServerEndpoint(rawURL string) (string, error) {
	if strings.TrimSpace(rawURL) == "" {
		return "", errors.New("server URL is required")
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", errors.Wrap(err, "failed to parse server URL")
	}

	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return "", errors.New("server URL must include a host")
	}

	port := parsed.Port()
	switch strings.ToLower(parsed.Scheme) {
	case "https":
		if port == "" {
			port = "443"
		}
	case "http":
		if port == "" {
			port = "80"
		}
	default:
		return "", errors.Errorf("server URL must use http:// or https:// (got scheme %q)", parsed.Scheme)
	}

	return host + ":" + port, nil
}

// eventDomainFromEndpoint derives the sanitized property-key suffix for a new server
// from its normalized endpoint (host:port), replacing '.' and ':' with '_'. This differs
// from the legacy migration's EventDomain, which instead preserves master's portless
// hostname-only derivation (extractServerDomain) so pre-upgrade posts stay reachable.
func eventDomainFromEndpoint(endpoint string) string {
	return strings.NewReplacer(".", "_", ":", "_").Replace(endpoint)
}

// resolveServerName resolves the domain that will appear in this homeserver's Matrix
// IDs. Resolution order, first success wins:
//  1. configuredName, if non-empty (manual override, or the legacy matrix_server_name
//     when called from the v3 migration).
//  2. GET <serverURL>/_matrix/key/v2/server's server_name field (best-effort - any
//     failure falls through rather than erroring).
//  3. Hostname(serverURL), which always succeeds for a parseable URL, so this function
//     never fails to produce a non-empty name for a parseable URL.
//
// The same function is used by server add and the v3 migration so ghost recognition
// stays consistent between the two callers.
func (p *Plugin) resolveServerName(serverURL, configuredName string) (string, error) {
	if configuredName != "" {
		normalized, err := matrix.NormalizeServerName(configuredName)
		if err != nil {
			return "", errors.Wrap(err, "invalid configured server name")
		}
		return normalized, nil
	}

	// Best-effort federation probe. Unauthenticated, so no AS token is needed (and none
	// may exist yet for a server that isn't registered).
	probe := matrix.NewClientWithLoggerAndRateLimit(serverURL, "", "", "", p.logger, matrix.RateLimitConfig{})
	if discovered, err := probe.GetServerNameFromKeyEndpoint(); err == nil && discovered != "" {
		if normalized, normErr := matrix.NormalizeServerName(discovered); normErr == nil && normalized != "" {
			return normalized, nil
		}
	}

	hostname, err := matrix.ExtractServerDomain(serverURL)
	if err != nil {
		return "", errors.Wrap(err, "failed to determine server name from URL")
	}

	p.logger.LogWarn("Could not discover Matrix server name via /_matrix/key/v2/server; falling back to URL hostname",
		"server_url", serverURL, "hostname", hostname)

	normalized, err := matrix.NormalizeServerName(hostname)
	if err != nil {
		return "", errors.Wrap(err, "failed to normalize hostname as server name")
	}
	return normalized, nil
}

// AddServer registers a new Matrix homeserver, or re-adopts a previously removed one
// when serverID is non-empty (see docs on RemoveServer for the recovery mechanism).
// Returns the server's ID (minted or re-adopted verbatim).
func (p *Plugin) AddServer(serverURL, asToken, hsToken, usernamePrefix, serverID, serverNameOverride string) (string, error) {
	endpoint, err := normalizeServerEndpoint(serverURL)
	if err != nil {
		return "", errors.Wrap(err, "invalid server URL")
	}

	if serverID != "" && !model.IsValidId(serverID) {
		return "", errors.Errorf("%q is not a valid server ID", serverID)
	}

	resolvedServerName, err := p.resolveServerName(serverURL, serverNameOverride)
	if err != nil {
		return "", errors.Wrap(err, "failed to resolve server name")
	}

	if usernamePrefix == "" {
		usernamePrefix = DefaultMatrixUsernamePrefix
	}

	eventDomain := eventDomainFromEndpoint(endpoint)
	siteURL := "https://" + endpoint

	checkConflicts := func(servers []kvstore.ServerConfig) error {
		for _, s := range servers {
			if s.Endpoint == endpoint {
				return errors.Errorf("a server is already registered at this endpoint (server_id: %s); use `/matrix server remove %s` first", s.ServerID, s.ServerID)
			}
			if s.ServerName == resolvedServerName {
				return errors.Errorf("server name %q conflicts with existing server %s; two servers cannot share a Matrix ID domain", resolvedServerName, s.ServerID)
			}
			if serverID != "" && s.ServerID == serverID {
				return errors.Errorf("server ID %s is already registered", serverID)
			}
			if hsToken != "" && s.HSToken == hsToken {
				return errors.Errorf("hs_token conflicts with existing server %s; hs_token must be unique across registered servers", s.ServerID)
			}
		}
		return nil
	}

	// Conflicts are checked here as well as inside the mutateServers callback below.
	// RegisterPluginForSharedChannels is idempotent per SiteURL, so re-adding an endpoint
	// that is already registered hands back the *existing* server's RemoteID; without this
	// pre-check the rollback would unregister that live remote, taking down the existing
	// server's channel invitations and sync cursors.
	//
	// This narrows the window rather than closing it: two concurrent adds for the same
	// endpoint can both clear the pre-check, both receive the same idempotent RemoteID,
	// and the CAS loser's rollback then unregisters the winner's live remote. Closing
	// that needs the registry entry reserved by CAS before the remote is created.
	existing, err := p.getServers()
	if err != nil {
		return "", errors.Wrap(err, "failed to read servers config")
	}
	if err := checkConflicts(existing); err != nil {
		return "", err
	}

	remoteID, err := p.doRegisterPluginForSharedChannels(siteURL)
	if err != nil {
		return "", errors.Wrap(err, "failed to register server for shared channels")
	}

	var mintedID string
	err = p.mutateServers(func(servers []kvstore.ServerConfig) ([]kvstore.ServerConfig, error) {
		// The authoritative check: only this callback sees the slice the write lands on.
		if err := checkConflicts(servers); err != nil {
			return nil, err
		}

		id := serverID
		if id == "" {
			id = model.NewId()
		}
		mintedID = id

		entry := kvstore.ServerConfig{
			ServerID:       id,
			ServerURL:      serverURL,
			Endpoint:       endpoint,
			ServerName:     resolvedServerName,
			EventDomain:    eventDomain,
			ASToken:        asToken,
			HSToken:        hsToken,
			UsernamePrefix: usernamePrefix,
			Enabled:        true,
			SiteURL:        siteURL,
			RemoteID:       remoteID,
		}

		result := make([]kvstore.ServerConfig, len(servers), len(servers)+1)
		copy(result, servers)
		return append(result, entry), nil
	})
	if err != nil {
		// The remote was created but belongs to no entry, so it must not be left
		// registered. Reaching here means a concurrent writer won the race after the
		// pre-check above passed, or the CAS itself failed.
		if appErr := p.API.UnregisterPluginRemoteForSharedChannels(remoteID); appErr != nil {
			p.logger.LogWarn("Failed to unregister shared-channels remote after a rejected AddServer", "remote_id", remoteID, "error", appErr)
		}
		return "", err
	}

	if serverID != "" {
		p.warnIfEventDomainMismatch(serverID, eventDomain)
	}

	if err := p.refreshServersAndBroadcast("server_added"); err != nil {
		p.logger.LogWarn("Failed to refresh Matrix clients after adding server", "server_id", mintedID, "error", err)
	}

	return mintedID, nil
}

// warnIfEventDomainMismatch logs a warning when a (re-)adopted serverID has surviving
// matrix_event_post_ records from a prior registration. There is no stored record of
// what EventDomain those records were created under (removal keeps no tombstone), so
// this cannot verify a mismatch precisely - it flags the one case the operator has no
// other way to discover: re-adding the same ID at a different endpoint silently orphans
// every pre-existing post's Matrix event ID property.
func (p *Plugin) warnIfEventDomainMismatch(serverID, newEventDomain string) {
	prefix := kvstore.KeyPrefixMatrixEventPost + serverID + "_"
	keys, err := kvstore.ListAllKeysWithPrefix(p.kvstore, prefix, kvstore.DefaultListKeysBatchSize)
	if err != nil {
		p.logger.LogWarn("Failed to check for surviving event-post records during server re-adoption", "server_id", serverID, "error", err)
		return
	}
	if len(keys) > 0 {
		p.logger.LogWarn("Re-adopted server ID has surviving synced-post records from a prior registration; verify this server's EventDomain matches what was used before re-adoption, or edits/deletes of pre-existing posts will silently stop working",
			"server_id", serverID, "event_domain", newEventDomain, "surviving_record_count", len(keys))
	}
}

// RemoveServer deletes serverID's registry entry and unregisters its shared-channels
// remote. It deletes no other KV records: every namespaced key stays exactly where it
// was, addressed by a ServerID that this command's caller should print as the recovery
// key for re-adoption via AddServer.
//
// Refuses to remove an entry whose SiteURL is empty - that is the migrated legacy
// server, which resolves to the pre-upgrade plugin_<PluginID> remote and cannot be
// re-created with the same identity. Disabling it is the supported way to take it out
// of service.
func (p *Plugin) RemoveServer(serverID string) (bool, error) {
	var removed *kvstore.ServerConfig

	err := p.mutateServers(func(servers []kvstore.ServerConfig) ([]kvstore.ServerConfig, error) {
		idx := -1
		for i, s := range servers {
			if s.ServerID == serverID {
				idx = i
				break
			}
		}
		if idx == -1 {
			removed = nil
			return servers, nil
		}

		if servers[idx].SiteURL == "" {
			return nil, errors.New("this server was migrated from the legacy single-server configuration and cannot be removed; use `/matrix server disable` to take it out of service")
		}

		entry := servers[idx]
		removed = &entry

		result := make([]kvstore.ServerConfig, 0, len(servers)-1)
		result = append(result, servers[:idx]...)
		result = append(result, servers[idx+1:]...)
		return result, nil
	})
	if err != nil {
		return false, err
	}

	if removed == nil {
		return false, nil
	}

	if removed.RemoteID != "" {
		if appErr := p.API.UnregisterPluginRemoteForSharedChannels(removed.RemoteID); appErr != nil {
			p.logger.LogWarn("Failed to unregister shared-channels remote for removed server; registry write already succeeded",
				"server_id", serverID, "remote_id", removed.RemoteID, "error", appErr)
		}
	}

	if err := p.refreshServersAndBroadcast("server_removed"); err != nil {
		p.logger.LogWarn("Failed to refresh Matrix clients after removing server", "server_id", serverID, "error", err)
	}

	return true, nil
}

// serverByID returns the registry entry for serverID, or an error if it is not
// registered. Reads a fresh snapshot of the registry on every call.
func (p *Plugin) serverByID(serverID string) (kvstore.ServerConfig, error) {
	servers, err := p.getServers()
	if err != nil {
		return kvstore.ServerConfig{}, err
	}
	for _, s := range servers {
		if s.ServerID == serverID {
			return s, nil
		}
	}
	return kvstore.ServerConfig{}, errors.Errorf("server %s is not registered", serverID)
}

// registeredServerIDForRemote resolves a shared-channels remote ID against the registry
// itself rather than this node's remoteToServerID cache, returning "" when no registered
// server claims it. This is the authoritative answer to "is this remote one of ours?",
// and it costs a single KV read - no client construction, no rebuild mutex - so a cache
// miss on a hot path can be disambiguated without paying for initMatrixClients.
func (p *Plugin) registeredServerIDForRemote(remoteID string) (string, error) {
	if remoteID == "" {
		return "", nil
	}
	servers, err := p.getServers()
	if err != nil {
		return "", err
	}
	for _, s := range servers {
		if s.RemoteID == remoteID {
			return s.ServerID, nil
		}
	}
	return "", nil
}

// serverDomainForID returns the ServerName (Matrix ID domain) for a registered server.
func (p *Plugin) serverDomainForID(serverID string) (string, error) {
	server, err := p.serverConfigForRouting(serverID)
	if err != nil {
		return "", err
	}
	return server.ServerName, nil
}

// legacyServerConfig mirrors the pre-v3 flat System Console fields. It intentionally
// keeps the old JSON tags so LoadPluginConfiguration can still populate it from the
// persisted plugin configuration after those keys are removed from settings_schema -
// stored configuration values survive removal of their schema entries.
type legacyServerConfig struct {
	MatrixServerURL      string `json:"matrix_server_url"`
	MatrixServerName     string `json:"matrix_server_name"`
	MatrixASToken        string `json:"matrix_as_token"`
	MatrixHSToken        string `json:"matrix_hs_token"`
	MatrixUsernamePrefix string `json:"matrix_username_prefix"`
	EnableSync           bool   `json:"enable_sync"`
}

// materializeServerFromLegacyConfig seeds one registry entry from the pre-v3 flat
// System Console configuration, for installs upgrading from the single-server layout.
// Returns ("", nil) on a fresh install with no legacy Matrix server URL configured -
// that is not an error. Idempotent: a no-op when an entry with the legacy endpoint
// already exists.
//
// Deliberately does not call AddServer: this entry needs SiteURL == "" (so it resolves
// to the pre-upgrade plugin_<PluginID> remote) and an EventDomain derived the way
// master derived it (portless URL hostname), neither of which is what AddServer
// produces. Remote registration is also deferred - registerForSharedChannels runs once
// for every entry immediately after all migrations complete.
func (p *Plugin) materializeServerFromLegacyConfig() (string, error) {
	var legacy legacyServerConfig
	if err := p.API.LoadPluginConfiguration(&legacy); err != nil {
		return "", errors.Wrap(err, "failed to load legacy plugin configuration")
	}

	if legacy.MatrixServerURL == "" {
		return "", nil
	}

	endpoint, err := normalizeServerEndpoint(legacy.MatrixServerURL)
	if err != nil {
		return "", errors.Wrap(err, "failed to normalize legacy Matrix server URL")
	}

	if servers, err := p.getServers(); err == nil {
		for _, s := range servers {
			if s.Endpoint == endpoint {
				return s.ServerID, nil // already materialized
			}
		}
	}

	serverName, err := p.resolveServerName(legacy.MatrixServerURL, legacy.MatrixServerName)
	if err != nil {
		return "", errors.Wrap(err, "failed to resolve legacy server name")
	}

	usernamePrefix := legacy.MatrixUsernamePrefix
	if usernamePrefix == "" {
		usernamePrefix = DefaultMatrixUsernamePrefix
	}

	serverID := model.NewId()
	entry := kvstore.ServerConfig{
		ServerID:       serverID,
		ServerURL:      legacy.MatrixServerURL,
		Endpoint:       endpoint,
		ServerName:     serverName,
		EventDomain:    extractServerDomain(p.logger, legacy.MatrixServerURL),
		ASToken:        legacy.MatrixASToken,
		HSToken:        legacy.MatrixHSToken,
		UsernamePrefix: usernamePrefix,
		Enabled:        legacy.EnableSync,
		SiteURL:        "", // resolves to the pre-upgrade plugin_<PluginID> remote
	}

	var resultID string
	err = p.mutateServers(func(current []kvstore.ServerConfig) ([]kvstore.ServerConfig, error) {
		for _, s := range current {
			if s.Endpoint == endpoint {
				resultID = s.ServerID
				return current, nil // materialized concurrently
			}
		}
		resultID = serverID
		result := make([]kvstore.ServerConfig, len(current), len(current)+1)
		copy(result, current)
		return append(result, entry), nil
	})
	if err != nil {
		return "", err
	}

	return resultID, nil
}
