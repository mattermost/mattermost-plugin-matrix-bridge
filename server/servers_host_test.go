package main

import (
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPluginHostUnregisterRemoteNilAppError covers the typed-nil trap: the platform
// method returns a nil *model.AppError on success, and naively returning that as an
// `error` interface would yield a NON-nil error (a nil pointer boxed into a non-nil
// interface), making every successful removal log a spurious warning.
func TestPluginHostUnregisterRemoteNilAppError(t *testing.T) {
	api := &plugintest.API{}
	api.On("UnregisterPluginRemoteForSharedChannels", "remote-1").Return(nil)

	p := &Plugin{}
	p.SetAPI(api)

	host := pluginHost{p: p}
	err := host.UnregisterRemote("remote-1")
	require.NoError(t, err, "a nil *model.AppError must convert to a nil error, not a non-nil interface wrapping a nil pointer")
}

// TestPluginHostUnregisterRemoteAppError covers the non-nil case for completeness:
// a genuine failure must still surface as a non-nil error.
func TestPluginHostUnregisterRemoteAppError(t *testing.T) {
	api := &plugintest.API{}
	api.On("UnregisterPluginRemoteForSharedChannels", "remote-1").Return(&model.AppError{Message: "boom"})

	p := &Plugin{}
	p.SetAPI(api)

	host := pluginHost{p: p}
	err := host.UnregisterRemote("remote-1")
	assert.Error(t, err)
}
