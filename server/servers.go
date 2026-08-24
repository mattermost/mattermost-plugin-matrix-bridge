package main

import (
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/pkg/errors"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/servers"
	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
)

// serverDomainForID returns the ServerName (Matrix ID domain) for a registered server,
// resolved through serverConfigForRouting rather than servers.Service.Domain: isGhostUser
// calls this once per inbound Matrix event, and Service.Domain would unmarshal the whole
// registry out of KV every time. The snapshot is refreshed by every registry mutation, so
// this is never staler than a direct registry read would have been.
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
// Deliberately does not call servers.Service.Add: this entry needs SiteURL == "" (so
// it resolves to the pre-upgrade plugin_<PluginID> remote) and an EventDomain derived
// the way master derived it (portless URL hostname), neither of which is what Add
// produces. Remote registration is also deferred - registerForSharedChannels runs once
// for every entry immediately after all migrations complete.
//
// This stays in main (rather than moving into server/servers) because it calls
// p.API.LoadPluginConfiguration - a platform dependency that package must not acquire.
func (p *Plugin) materializeServerFromLegacyConfig() (string, error) {
	var legacy legacyServerConfig
	if err := p.API.LoadPluginConfiguration(&legacy); err != nil {
		return "", errors.Wrap(err, "failed to load legacy plugin configuration")
	}

	if legacy.MatrixServerURL == "" {
		return "", nil
	}

	endpoint, err := servers.NormalizeEndpoint(legacy.MatrixServerURL)
	if err != nil {
		return "", errors.Wrap(err, "failed to normalize legacy Matrix server URL")
	}

	if existing, err := p.servers.List(); err == nil {
		for _, s := range existing {
			if s.Endpoint == endpoint {
				return s.ServerID, nil // already materialized
			}
		}
	}

	serverName, err := p.servers.ResolveServerName(legacy.MatrixServerURL, legacy.MatrixServerName)
	if err != nil {
		return "", errors.Wrap(err, "failed to resolve legacy server name")
	}

	usernamePrefix := legacy.MatrixUsernamePrefix
	if usernamePrefix == "" {
		usernamePrefix = servers.DefaultUsernamePrefix
	}

	entry := kvstore.ServerConfig{
		ServerID:       model.NewId(),
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

	return p.servers.Seed(entry)
}
