package main

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"strings"

	"github.com/pkg/errors"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/matrix"
	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
)

// getServers reads the managed Matrix server registry from the KV store. A
// missing or empty registry yields a nil slice and no error.
func (p *Plugin) getServers() ([]kvstore.ServerConfig, error) {
	if p.kvstore == nil {
		return nil, nil
	}

	data, err := p.kvstore.Get(kvstore.KeyServersConfig)
	if err != nil {
		// A missing key returns (nil, nil) from the plugin KV API, so a non-nil
		// error here means a real backend failure. Surface it rather than
		// treating it as "no servers registered", so callers never persist to or
		// act on a half-read registry.
		return nil, errors.Wrap(err, "failed to read servers_config")
	}

	servers, err := kvstore.ParseServersConfig(data)
	if err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal servers_config")
	}
	return servers, nil
}

// getSingleServerID returns the serverID of the single configured Matrix server.
// It prefers the value cached during reconcileServerConfig and falls back to
// reading the registry from the KV store. Returns "" if no server is registered.
func (p *Plugin) getSingleServerID() string {
	p.matrixClientsLock.RLock()
	cached := p.serverID
	p.matrixClientsLock.RUnlock()
	if cached != "" {
		return cached
	}

	servers, err := p.getServers()
	if err != nil || len(servers) == 0 {
		return ""
	}
	return servers[0].ServerID
}

// serverDomainForID returns the raw homeserver domain (hostname, not the
// property-key-sanitized form) for the given serverID, resolved from the managed
// server registry. It is used to recognize this server's ghost users, whose IDs
// end in ":<domain>". The bool is false when the server is unknown or its URL
// cannot be parsed.
func (p *Plugin) serverDomainForID(serverID string) (string, bool) {
	servers, err := p.getServers()
	if err != nil {
		p.logger.LogWarn("Failed to read server registry while resolving server domain", "server_id", serverID, "error", err)
		return "", false
	}
	server, ok := kvstore.ServerConfigForID(servers, serverID)
	if !ok {
		return "", false
	}
	domain, err := matrix.ExtractServerDomain(server.ServerURL)
	if err != nil || domain == "" {
		return "", false
	}
	return domain, true
}

// serverIDNamespace returns the "<serverID>_" prefix used to namespace per-server
// KV keys, or "" when no server is registered yet. Migrations use it to detect
// keys already migrated to the v3 layout.
func (p *Plugin) serverIDNamespace() string {
	id := p.getSingleServerID()
	if id == "" {
		return ""
	}
	return id + "_"
}

// reconcileServerConfig derives the managed server registry from the flat
// plugin.json configuration and persists it under KeyServersConfig. The serverID
// is derived deterministically from the homeserver hostname (see deriveServerID),
// so it is stable across restarts and config edits as long as the hostname is
// unchanged. The single serverID is cached for fast lookups. It returns the
// resulting registry.
//
// This is the single authority for establishing the serverID; it needs no Matrix
// client and is idempotent, so it can run safely before migrations and on every
// configuration change.
func (p *Plugin) reconcileServerConfig() ([]kvstore.ServerConfig, error) {
	config := p.getConfiguration()

	existing, err := p.getServers()
	if err != nil {
		return nil, err
	}

	// No server URL configured yet (e.g. the plugin is enabled with sync off).
	// There is nothing to register: leave any existing registry untouched and
	// report it as-is rather than writing a useless entry or failing activation.
	// A serverID cannot be derived without a URL, and none is needed yet.
	if config.MatrixServerURL == "" {
		return existing, nil
	}

	// The serverID is derived deterministically from the homeserver hostname, so
	// a server re-created with the same URL re-adopts its namespaced KV records
	// instead of orphaning them. We always derive rather than reuse the stored
	// ID: that determinism is what makes the recovery possible.
	serverID, err := deriveServerID(config.MatrixServerURL)
	if err != nil {
		return nil, err
	}

	// The primary entry is the one derived from the flat plugin.json config. Find
	// any previously persisted primary (an entry that is not an injected extra) to
	// carry its RemoteID forward and to warn when the hostname changes.
	var persistedPrimary *kvstore.ServerConfig
	for i := range existing {
		if !existing[i].Injected {
			persistedPrimary = &existing[i]
			break
		}
	}
	persistedRemoteID := ""
	if persistedPrimary != nil {
		persistedRemoteID = persistedPrimary.RemoteID
		if persistedPrimary.ServerID != serverID {
			p.logger.LogWarn("Matrix homeserver hostname changed; KV records under the previous serverID are now orphaned and can be recovered by reverting the server URL",
				"previous_server_id", persistedPrimary.ServerID, "new_server_id", serverID)
		}
	}

	// reconcileServerConfig runs during initMatrixClient before
	// registerForSharedChannels has assigned p.remoteID. Preserve the already
	// persisted RemoteID in that window so an early reconcile (or a failed
	// registration) never erases a valid remote identity.
	remoteID := p.remoteID
	if remoteID == "" {
		remoteID = persistedRemoteID
	}

	entry := kvstore.ServerConfig{
		ServerID:       serverID,
		ServerURL:      config.MatrixServerURL,
		ServerName:     config.MatrixServerName,
		ASToken:        config.MatrixASToken,
		HSToken:        config.MatrixHSToken,
		UsernamePrefix: config.GetMatrixUsernamePrefix(),
		Enabled:        config.EnableSync,
		RemoteID:       remoteID,
	}

	// The flat plugin.json config describes only the primary server. Rebuild the
	// primary entry and preserve any injected extra servers (added via
	// `/matrix server add` for local multi-server testing). Non-injected entries
	// are the previous primary and are always replaced, so a normal single-server
	// install collapses to the same single-element list as before — no behavior
	// change regardless of whether the serverID cache is warm.
	servers := []kvstore.ServerConfig{entry}
	for _, s := range existing {
		if s.Injected && s.ServerID != serverID {
			servers = append(servers, s)
		}
	}

	data, err := json.Marshal(servers)
	if err != nil {
		return nil, errors.Wrap(err, "failed to marshal servers_config")
	}
	if err := p.kvstore.Set(kvstore.KeyServersConfig, data); err != nil {
		return nil, errors.Wrap(err, "failed to persist servers_config")
	}

	p.matrixClientsLock.Lock()
	p.serverID = serverID
	p.matrixClientsLock.Unlock()

	return servers, nil
}

// persistServers marshals and stores the managed server registry.
func (p *Plugin) persistServers(servers []kvstore.ServerConfig) error {
	data, err := json.Marshal(servers)
	if err != nil {
		return errors.Wrap(err, "failed to marshal servers_config")
	}
	if err := p.kvstore.Set(kvstore.KeyServersConfig, data); err != nil {
		return errors.Wrap(err, "failed to persist servers_config")
	}
	return nil
}

// GetManagedServers returns the current managed Matrix server registry.
func (p *Plugin) GetManagedServers() ([]kvstore.ServerConfig, error) {
	return p.getServers()
}

// AddManagedServer upserts a Matrix server into the managed registry and rebuilds
// the live client and bridge set so the server is usable without a restart. It
// backs the `/matrix server add` admin command, which exists because the System
// Console UI cannot yet manage more than one server. The serverID is derived from
// serverURL (see deriveServerID), so adding a URL whose hostname matches an
// existing server updates that entry in place. reconcileServerConfig preserves
// this entry across future config changes because it is now part of the persisted
// registry.
func (p *Plugin) AddManagedServer(serverURL, serverName, asToken, hsToken, usernamePrefix string) (string, error) {
	serverURL = strings.TrimSpace(serverURL)
	serverID, err := deriveServerID(serverURL)
	if err != nil {
		return "", err
	}

	servers, err := p.getServers()
	if err != nil {
		return "", err
	}

	if usernamePrefix == "" {
		usernamePrefix = DefaultMatrixUsernamePrefix
	}
	entry := kvstore.ServerConfig{
		ServerID:       serverID,
		ServerURL:      serverURL,
		ServerName:     serverName,
		ASToken:        asToken,
		HSToken:        hsToken,
		UsernamePrefix: usernamePrefix,
		Enabled:        true,
		RemoteID:       p.remoteID,
		Injected:       true,
	}

	replaced := false
	for i := range servers {
		if servers[i].ServerID == serverID {
			// Keep the existing RemoteID so re-adding a server does not drop its
			// shared-channels registration.
			if servers[i].RemoteID != "" {
				entry.RemoteID = servers[i].RemoteID
			}
			servers[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		servers = append(servers, entry)
	}

	if err := p.persistServers(servers); err != nil {
		return "", err
	}

	// Rebuild the client registry and bridges from the updated registry so the new
	// server is live immediately.
	if err := p.initMatrixClient(); err != nil {
		return "", errors.Wrap(err, "failed to rebuild Matrix clients after adding server")
	}
	p.initBridges()

	return serverID, nil
}

// RemoveManagedServer removes a server from the managed registry by serverID and
// rebuilds the live client and bridge set. It returns false if no entry matched.
// Removing the primary server derived from the flat plugin.json configuration does
// not persist: reconcileServerConfig re-adds it on the next rebuild.
func (p *Plugin) RemoveManagedServer(serverID string) (bool, error) {
	servers, err := p.getServers()
	if err != nil {
		return false, err
	}

	filtered := make([]kvstore.ServerConfig, 0, len(servers))
	found := false
	for _, s := range servers {
		if s.ServerID == serverID {
			found = true
			continue
		}
		filtered = append(filtered, s)
	}
	if !found {
		return false, nil
	}

	if err := p.persistServers(filtered); err != nil {
		return false, err
	}

	if err := p.initMatrixClient(); err != nil {
		return true, errors.Wrap(err, "failed to rebuild Matrix clients after removing server")
	}
	p.initBridges()

	return true, nil
}

// serverIDEncoding matches model.NewId()'s base32 alphabet so a derived serverID
// is indistinguishable in shape from a framework-minted ID.
var serverIDEncoding = base32.NewEncoding("ybndrfg8ejkmcpqxot1uwisza345h769").WithPadding(base32.NoPadding)

// deriveServerID produces the stable, deterministic serverID for a homeserver
// from its base URL. It hashes the normalized hostname (case-folded; scheme,
// port and path stripped) so the same server always yields the same ID. That
// determinism is what lets orphaned KV records be re-adopted when a registry
// entry is lost and later re-created with the same URL. The output is a
// 26-character string in Mattermost's base32 ID alphabet, a drop-in replacement
// for model.NewId() as a KV namespace key.
func deriveServerID(serverURL string) (string, error) {
	host, err := matrix.ExtractServerDomain(serverURL)
	if err != nil {
		return "", errors.Wrap(err, "cannot derive serverID from server URL")
	}
	sum := sha256.Sum256([]byte(strings.ToLower(host)))
	return serverIDEncoding.EncodeToString(sum[:16])[:26], nil
}
