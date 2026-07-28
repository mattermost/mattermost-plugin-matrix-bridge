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

// getSingleServerID returns the serverID when exactly one Matrix server is
// registered, and "" otherwise (zero or multiple servers).
// It backs the convenience commands that let an operator omit the
// server_id argument when only one server exists; with multiple servers the
// caller must target one explicitly. It is only called from cold paths (bridge
// setup and migrations), so the registry read is not on any hot path.
func (p *Plugin) getSingleServerID() string {
	servers, err := p.getServers()
	if err != nil || len(servers) != 1 {
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
	// Ghost users are named with the homeserver's Matrix server name (e.g.
	// @_mattermost_<id>:example.com), which is the configured ServerName — not
	// necessarily the connection URL host (the two differ under delegation, or in
	// tests where the URL is localhost:<port> but the server name is example.com).
	// Prefer ServerName so ghost recognition matches how ghosts are actually
	// created; fall back to the URL host only when no server name is configured.
	if server.ServerName != "" {
		return server.ServerName, true
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

// legacyServerConfig mirrors the flat single-server plugin.json fields that
// existed before the multi-server registry. It is read only by the v3 migration
// to seed the registry from a pre-multi-server (v2) install; the live
// configuration struct no longer carries these fields.
type legacyServerConfig struct {
	MatrixServerURL      string `json:"matrix_server_url"`
	MatrixServerName     string `json:"matrix_server_name"`
	MatrixASToken        string `json:"matrix_as_token"`
	MatrixHSToken        string `json:"matrix_hs_token"`
	MatrixUsernamePrefix string `json:"matrix_username_prefix"`
	EnableSync           bool   `json:"enable_sync"`
}

// materializeServerFromLegacyConfig seeds the managed server registry from the
// legacy flat plugin.json configuration during the v3 migration. It is the
// one-time bridge from single-server config to the registry-as-sole-source-of-
// truth model. The migrated server is stored with an empty SiteURL so it
// re-adopts the legacy "plugin_<PluginID>" shared-channels remote, preserving
// existing shared channels across the upgrade. It is idempotent: if an entry for
// the derived serverID already exists it is left untouched. Returns the serverID,
// or "" when no legacy server was configured (fresh install).
func (p *Plugin) materializeServerFromLegacyConfig() (string, error) {
	var legacy legacyServerConfig
	if err := p.API.LoadPluginConfiguration(&legacy); err != nil {
		return "", errors.Wrap(err, "failed to load legacy plugin configuration")
	}
	if legacy.MatrixServerURL == "" {
		return "", nil // fresh install: no server to migrate
	}

	serverID, err := deriveServerID(legacy.MatrixServerURL)
	if err != nil {
		return "", err
	}

	servers, err := p.getServers()
	if err != nil {
		return "", err
	}
	if _, ok := kvstore.ServerConfigForID(servers, serverID); ok {
		return serverID, nil // already materialized (idempotent)
	}

	prefix := legacy.MatrixUsernamePrefix
	if prefix == "" {
		prefix = DefaultMatrixUsernamePrefix
	}
	serverName := legacy.MatrixServerName
	if serverName != "" {
		// Defensive: v2 stored an already-normalized name, but re-normalize in case.
		if normalized, nerr := matrix.NormalizeServerName(serverName); nerr == nil {
			serverName = normalized
		}
	}
	entry := kvstore.ServerConfig{
		ServerID:       serverID,
		ServerURL:      legacy.MatrixServerURL,
		ServerName:     serverName,
		ASToken:        legacy.MatrixASToken,
		HSToken:        legacy.MatrixHSToken,
		UsernamePrefix: prefix,
		Enabled:        legacy.EnableSync,
		// Empty SiteURL re-adopts the legacy single-server shared-channels remote.
		SiteURL: "",
	}
	servers = append(servers, entry)
	if err := p.persistServers(servers); err != nil {
		return "", err
	}
	p.logger.LogInfo("Seeded managed server registry from legacy single-server config", "server_id", serverID)
	return serverID, nil
}

// hostSiteURL returns the shared-channels SiteURL for a server added to the
// registry: a stable, unique-per-host value derived from the homeserver hostname
// (the same input serverID is derived from). It is never empty for a valid URL,
// so it never collides with the reserved empty SiteURL of the legacy migrated
// server.
func hostSiteURL(serverURL string) string {
	if host, err := matrix.ExtractServerDomain(serverURL); err == nil && host != "" {
		return "https://" + host
	}
	return serverURL
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

// AddServer upserts a Matrix server into the managed registry and rebuilds
// the live client and bridge set so the server is usable without a restart. It
// backs the `/matrix server add` admin command, the supported way to manage
// homeservers until the admin UI lands. The serverID is derived from serverURL
// (see deriveServerID), so adding a URL whose hostname matches an existing server
// updates that entry in place.
func (p *Plugin) AddServer(serverURL, serverName, asToken, hsToken, usernamePrefix string) (string, error) {
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
		SiteURL:        hostSiteURL(serverURL),
	}

	replaced := false
	for i := range servers {
		if servers[i].ServerID == serverID {
			// Keep the existing RemoteID and SiteURL so re-adding a server does not
			// drop its shared-channels registration or re-key its remote. In
			// particular this preserves the reserved empty SiteURL of the migrated
			// server if it is re-added by URL.
			if servers[i].RemoteID != "" {
				entry.RemoteID = servers[i].RemoteID
			}
			entry.SiteURL = servers[i].SiteURL
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

	// Register the added server for shared channels now so it is assigned its own
	// distinct remote ID immediately, rather than sharing the primary's remote until
	// the next activation. This makes loop-prevention attribution for messages
	// originating on the added server correct without a restart. The call is
	// idempotent and Mattermost-side only (it does not contact the homeserver); a
	// failure leaves the server usable with the primary's remote (the prior
	// approximate behavior), so warn and continue rather than failing the add.
	if err := p.registerForSharedChannels(); err != nil {
		p.logger.LogWarn("Failed to register added Matrix server for shared channels; loop attribution stays approximate until next activation",
			"server_id", serverID, "error", err)
	}

	// Rebuild the client registry from the updated registry (now carrying the
	// added server's distinct remote ID) so the new server is live immediately.
	// Per-server bridges are built on demand, so there is nothing else to refresh.
	if err := p.initMatrixClients(); err != nil {
		return "", errors.Wrap(err, "failed to rebuild Matrix clients after adding server")
	}

	return serverID, nil
}

// RemoveServer removes a server from the managed registry by serverID and
// rebuilds the live client registry. It returns false if no entry matched. The
// registry is authoritative, so the removal is permanent.
func (p *Plugin) RemoveServer(serverID string) (bool, error) {
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

	if err := p.initMatrixClients(); err != nil {
		return true, errors.Wrap(err, "failed to rebuild Matrix clients after removing server")
	}

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
