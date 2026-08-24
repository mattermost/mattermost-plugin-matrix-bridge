package main

import (
	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/matrix"
	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
)

// pluginHost adapts *Plugin to servers.Host without adding those methods to
// Plugin's own method set - PluginAccessor and every other consumer of *Plugin
// should not gain them just because the servers package needs them.
type pluginHost struct{ p *Plugin }

// MatrixClient returns this node's client for serverID, or nil if there is none.
func (h pluginHost) MatrixClient(serverID string) *matrix.Client {
	return h.p.getMatrixClient(serverID)
}

// RegisterRemoteForSiteURL creates the shared-channels remote for a server that is not
// persisted yet and returns its remote ID, so Service.Add can register the remote before
// writing the registry entry and never leave a registered server without one.
func (h pluginHost) RegisterRemoteForSiteURL(siteURL string) (string, error) {
	return h.p.doRegisterPluginForSharedChannels(siteURL)
}

// UnregisterRemote must convert explicitly: UnregisterPluginRemoteForSharedChannels
// returns *model.AppError, and returning a nil *AppError as an error yields a
// NON-nil error interface (the typed-nil trap) - which would make every successful
// removal log a spurious warning.
func (h pluginHost) UnregisterRemote(remoteID string) error {
	if appErr := h.p.API.UnregisterPluginRemoteForSharedChannels(remoteID); appErr != nil {
		return appErr
	}
	return nil
}

// RefreshAndBroadcast rebuilds this node's Matrix client caches and broadcasts a
// cluster event so every other node does the same.
func (h pluginHost) RefreshAndBroadcast(reason string) error {
	return h.p.refreshServersAndBroadcast(reason)
}

// SiteURL returns the configured Mattermost site URL, or "" if unset.
func (h pluginHost) SiteURL() string {
	if cfg := h.p.API.GetConfig(); cfg != nil && cfg.ServiceSettings.SiteURL != nil {
		return *cfg.ServiceSettings.SiteURL
	}
	return ""
}

// PluginID returns this plugin's ID from the generated manifest.
func (h pluginHost) PluginID() string {
	return manifest.Id
}

// pluginLogger adapts *Plugin to servers.Logger by reading p.logger at call time
// rather than capturing it once. servers.New is called from OnActivate immediately
// after p.kvstore is assigned, but some tests construct a Plugin's kvstore and
// servers before its logger; a live proxy means construction order between the two
// does not matter, only that p.logger is set by the time something actually logs.
type pluginLogger struct{ p *Plugin }

func (l pluginLogger) LogDebug(message string, keyValuePairs ...any) {
	l.p.logger.LogDebug(message, keyValuePairs...)
}

func (l pluginLogger) LogInfo(message string, keyValuePairs ...any) {
	l.p.logger.LogInfo(message, keyValuePairs...)
}

func (l pluginLogger) LogWarn(message string, keyValuePairs ...any) {
	l.p.logger.LogWarn(message, keyValuePairs...)
}

func (l pluginLogger) LogError(message string, keyValuePairs ...any) {
	l.p.logger.LogError(message, keyValuePairs...)
}

// routingServerGetter adapts *Plugin to BridgeUtils' ServerGetter so registry reads on
// the sync hot paths go through serverConfigForRouting - the per-node snapshot, with a
// KV read only as a fallback - instead of servers.Service.Get, which unmarshals the whole
// registry every call. Separate from pluginHost because the servers package must not be
// able to reach back into Plugin's routing cache: that cache is rebuilt *from* the
// registry the service owns.
type routingServerGetter struct{ p *Plugin }

func (g routingServerGetter) Get(serverID string) (kvstore.ServerConfig, error) {
	return g.p.serverConfigForRouting(serverID)
}
