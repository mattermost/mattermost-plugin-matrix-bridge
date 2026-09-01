package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
)

func TestServeHTTP(t *testing.T) {
	assert := assert.New(t)
	plugin := Plugin{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/hello", nil)
	r.Header.Set("Mattermost-User-ID", "test-user-id")

	plugin.ServeHTTP(nil, w, r)

	result := w.Result()
	assert.NotNil(result)
	defer func() { _ = result.Body.Close() }()
	bodyBytes, err := io.ReadAll(result.Body)
	assert.Nil(err)
	bodyString := string(bodyBytes)

	assert.Equal("Hello, world!", bodyString)
}

// TestRefreshServersAndBroadcastPublishesEvent covers §5.1's cluster event requirements:
// refreshServersAndBroadcast must publish a reliable clusterEventServersChanged event
// carrying the reason as its Data payload.
func TestRefreshServersAndBroadcastPublishesEvent(t *testing.T) {
	plugin := newTestPluginForServers(t)
	api := plugin.API.(*plugintest.API)

	var published model.PluginClusterEvent
	var publishedOpts model.PluginClusterEventSendOptions
	api.On("PublishPluginClusterEvent", mock.Anything, mock.Anything).Run(func(args mock.Arguments) {
		published = args.Get(0).(model.PluginClusterEvent)
		publishedOpts = args.Get(1).(model.PluginClusterEventSendOptions)
	}).Return(nil)

	require.NoError(t, plugin.refreshServersAndBroadcast("test_reason"))

	assert.Equal(t, clusterEventServersChanged, published.Id)
	assert.Equal(t, "test_reason", string(published.Data))
	assert.Equal(t, model.PluginClusterEventSendTypeReliable, publishedOpts.SendType)
}

// TestRefreshServersAndBroadcastToleratesPublishFailure covers the "tolerates a publish
// failure" requirement: single-node installs have no cluster, and this node's own
// caches are already correct by the time the broadcast is attempted.
func TestRefreshServersAndBroadcastToleratesPublishFailure(t *testing.T) {
	plugin := newTestPluginForServers(t)
	api := plugin.API.(*plugintest.API)
	api.On("PublishPluginClusterEvent", mock.Anything, mock.Anything).Return(&model.AppError{Message: "boom"})

	err := plugin.refreshServersAndBroadcast("test_reason")
	require.NoError(t, err, "a failed broadcast must not fail refreshServersAndBroadcast")
}

// TestOnPluginClusterEventRebuildsOnKnownID covers §5.1: a clusterEventServersChanged
// event must trigger initMatrixClients so this node's caches pick up a registry mutation
// made on another cluster node.
func TestOnPluginClusterEventRebuildsOnKnownID(t *testing.T) {
	plugin := newTestPluginForServers(t)

	data, err := kvstore.MarshalServersConfig([]kvstore.ServerConfig{
		{ServerID: "serverA", ServerURL: "https://a.example.com", ServerName: "a.example.com", Enabled: true},
	})
	require.NoError(t, err)
	require.NoError(t, plugin.kvstore.Set(kvstore.KeyServersConfig, data))
	require.Nil(t, plugin.getMatrixClient("serverA"), "client cache must start empty")

	plugin.OnPluginClusterEvent(nil, model.PluginClusterEvent{Id: clusterEventServersChanged, Data: []byte("test")})

	assert.NotNil(t, plugin.getMatrixClient("serverA"), "a known cluster event must rebuild the client cache")
}

// TestOnPluginClusterEventIgnoresUnknownID covers §5.1: an event with any other Id must
// not trigger a rebuild.
func TestOnPluginClusterEventIgnoresUnknownID(t *testing.T) {
	plugin := newTestPluginForServers(t)

	data, err := kvstore.MarshalServersConfig([]kvstore.ServerConfig{
		{ServerID: "serverA", ServerURL: "https://a.example.com", ServerName: "a.example.com", Enabled: true},
	})
	require.NoError(t, err)
	require.NoError(t, plugin.kvstore.Set(kvstore.KeyServersConfig, data))

	plugin.OnPluginClusterEvent(nil, model.PluginClusterEvent{Id: "some_other_event", Data: []byte("test")})

	assert.Nil(t, plugin.getMatrixClient("serverA"), "an unrelated cluster event must not trigger a rebuild")
}
