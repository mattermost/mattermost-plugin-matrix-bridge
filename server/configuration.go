package main

import (
	"fmt"
	"reflect"

	"github.com/pkg/errors"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/matrix"
)

// DefaultMatrixUsernamePrefix is the default username prefix for Matrix-originated users
const DefaultMatrixUsernamePrefix = "matrix"

// configuration captures the plugin's external configuration as exposed in the Mattermost server
// configuration, as well as values computed from the configuration. Any public fields will be
// deserialized from the Mattermost server configuration in OnConfigurationChange.
//
// As plugins are inherently concurrent (hooks being called asynchronously), and the plugin
// configuration can change at any time, access to the configuration must be synchronized. The
// strategy used in this plugin is to guard a pointer to the configuration, and clone the entire
// struct whenever it changes. You may replace this with whatever strategy you choose.
//
// If you add non-reference types to your configuration struct, be sure to rewrite Clone as a deep
// copy appropriate for your types.
type configuration struct {
	EnableSync       bool   `json:"enable_sync"`
	RateLimitingMode string `json:"rate_limiting_mode"`
}

// Clone shallow copies the configuration. Your implementation may require a deep copy if
// your configuration has reference types.
func (c *configuration) Clone() *configuration {
	clone := *c
	return &clone
}

// getConfiguration retrieves the active configuration under lock, making it safe to use
// concurrently. The active configuration may change underneath the client of this method, but
// the struct returned by this API call is considered immutable.
func (p *Plugin) getConfiguration() *configuration {
	p.configurationLock.RLock()
	defer p.configurationLock.RUnlock()

	if p.configuration == nil {
		return &configuration{}
	}

	return p.configuration
}

// setConfiguration replaces the active configuration under lock.
//
// Do not call setConfiguration while holding the configurationLock, as sync.Mutex is not
// reentrant. In particular, avoid using the plugin API entirely, as this may in turn trigger a
// hook back into the plugin. If that hook attempts to acquire this lock, a deadlock may occur.
//
// This method panics if setConfiguration is called with the existing configuration. This almost
// certainly means that the configuration was modified without being cloned and may result in
// an unsafe access.
func (p *Plugin) setConfiguration(configuration *configuration) {
	p.configurationLock.Lock()
	defer p.configurationLock.Unlock()

	if configuration != nil && p.configuration == configuration {
		// Ignore assignment if the configuration struct is empty. Go will optimize the
		// allocation for same to point at the same memory address, breaking the check
		// above.
		if reflect.ValueOf(*configuration).NumField() == 0 {
			return
		}

		panic("setConfiguration called with the existing configuration")
	}

	p.configuration = configuration
}

// OnConfigurationChange is invoked when configuration changes may have been made.
func (p *Plugin) OnConfigurationChange() error {
	configuration := new(configuration)

	if p.GetPluginAPI().GetConfig().ConnectedWorkspacesSettings.EnableSharedChannels == nil || !*p.GetPluginAPI().GetConfig().ConnectedWorkspacesSettings.EnableSharedChannels {
		return fmt.Errorf("shared Channels is required but currently not enabled")
	}

	// Load the public configuration fields from the Mattermost server configuration.
	if err := p.API.LoadPluginConfiguration(configuration); err != nil {
		return errors.Wrap(err, "failed to load plugin configuration")
	}

	// Validate required configuration
	if err := p.validateConfiguration(configuration); err != nil {
		return errors.Wrap(err, "invalid plugin configuration")
	}

	p.setConfiguration(configuration)

	if err := p.initMatrixClients(); err != nil {
		return errors.Wrap(err, "failed to initialize Matrix client")
	}

	return nil
}

// validateConfiguration normalizes the global settings. Per-server Matrix
// connection settings are validated where they are entered (the `/matrix server`
// commands and the registry), not here.
func (p *Plugin) validateConfiguration(config *configuration) error {
	// Validate and normalize rate limiting mode using matrix package
	parsedMode := matrix.ParseRateLimitingMode(config.RateLimitingMode)
	config.RateLimitingMode = string(parsedMode)

	return nil
}
