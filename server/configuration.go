package main

import (
	"fmt"
	"reflect"

	"github.com/pkg/errors"
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
	MatrixServerURL      string `json:"matrix_server_url"`
	MatrixASToken        string `json:"matrix_as_token"`
	MatrixHSToken        string `json:"matrix_hs_token"`
	EnableSync           bool   `json:"enable_sync"`
	MatrixUsernamePrefix string `json:"matrix_username_prefix"`
}

// Clone shallow copies the configuration. Your implementation may require a deep copy if
// your configuration has reference types.
func (c *configuration) Clone() *configuration {
	var clone = *c
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
	var configuration = new(configuration)

	if p.GetPluginAPI().GetConfig().ConnectedWorkspacesSettings.EnableSharedChannels != nil && *p.GetPluginAPI().GetConfig().ConnectedWorkspacesSettings.EnableSharedChannels {
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

	p.initMatrixClient()

	return nil
}

// validateConfiguration checks that required configuration fields are present
func (p *Plugin) validateConfiguration(config *configuration) error {
	if config.EnableSync {
		if config.MatrixServerURL == "" {
			return errors.New("Matrix Server URL is required when sync is enabled")
		}
		if config.MatrixASToken == "" {
			return errors.New("Matrix Application Service Token is required when sync is enabled")
		}
		if config.MatrixHSToken == "" {
			return errors.New("Matrix Homeserver Token is required when sync is enabled")
		}
	}
	return nil
}

// GetMatrixServerURL implements the Configuration interface for command package
func (c *configuration) GetMatrixServerURL() string {
	return c.MatrixServerURL
}

// GetMatrixUsernamePrefix returns the username prefix to use for Matrix-originated users
func (c *configuration) GetMatrixUsernamePrefix() string {
	if c.MatrixUsernamePrefix == "" {
		return DefaultMatrixUsernamePrefix
	}
	return c.MatrixUsernamePrefix
}

// GetMatrixUsernamePrefixForServer returns the username prefix for a specific Matrix server
// This allows for future extensibility to support different prefixes per server
func (c *configuration) GetMatrixUsernamePrefixForServer(_ string) string {
	// For now, return the global prefix
	// In the future, this could check a map of server-specific prefixes
	return c.GetMatrixUsernamePrefix()
}
