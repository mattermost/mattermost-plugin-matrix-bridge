package main

import (
	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/matrix"
)

// pluginHost adapts *Plugin to servers.Host without adding those methods to
// Plugin's own method set - PluginAccessor and every other consumer of *Plugin
// should not gain them just because the servers package needs them.
type pluginHost struct{ p *Plugin }

// MatrixClient returns this node's client for serverID, or nil if there is none.
func (h pluginHost) MatrixClient(serverID string) *matrix.Client {
	return h.p.getMatrixClient(serverID)
}

// RegisterRemote registers a shared-channels remote for a single, already-persisted
// server entry, so a newly added server gets a working remote immediately.
func (h pluginHost) RegisterRemote(serverID string) error {
	return h.p.registerServerForSharedChannels(serverID)
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
