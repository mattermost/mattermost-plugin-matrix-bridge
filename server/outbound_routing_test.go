package main

import (
	"encoding/json"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
)

const (
	routeServerA  = "aaaaaaaaaaaaaaaaaaaaaaaaaa"
	routeServerB  = "bbbbbbbbbbbbbbbbbbbbbbbbbb"
	routeRemoteA  = "remote-a"
	routeRemoteB  = "remote-b"
	routeRoomA    = "!rooma:a.example.org"
	routeRoomB    = "!roomb:b.example.org"
	routeUnknownR = "remote-unknown"
)

// newRoutingTestPlugin builds a plugin wired with an in-memory KV store and a
// two-server registry (A and B) plus the corresponding remote-ID maps.
func newRoutingTestPlugin(t *testing.T) *Plugin {
	t.Helper()
	p := setupPluginForTest()
	p.kvstore = NewMemoryKVStore()
	p.pendingFiles = NewPendingFileTracker()
	p.postTracker = NewPostTracker(DefaultPostTrackerMaxEntries)
	p.configuration = &configuration{}

	servers := []kvstore.ServerConfig{
		{ServerID: routeServerA, ServerURL: "https://a.example.org", ServerName: "a.example.org", UsernamePrefix: "matrixa", Enabled: true, RemoteID: routeRemoteA},
		{ServerID: routeServerB, ServerURL: "https://b.example.org", ServerName: "b.example.org", UsernamePrefix: "matrixb", Enabled: true, RemoteID: routeRemoteB, SiteURL: "https://b.example.org"},
	}
	data, err := json.Marshal(servers)
	require.NoError(t, err)
	require.NoError(t, p.kvstore.Set(kvstore.KeyServersConfig, data))

	p.matrixClientsLock.Lock()
	p.remoteToServerID = map[string]string{routeRemoteA: routeServerA, routeRemoteB: routeServerB}
	p.ownRemoteIDs = map[string]struct{}{routeRemoteA: {}, routeRemoteB: {}}
	p.matrixClientsLock.Unlock()

	return p
}

func seedChannelMapping(t *testing.T, p *Plugin, channelID string, mappings []kvstore.ChannelServerMapping) {
	t.Helper()
	data, err := kvstore.MarshalChannelServerMappings(mappings)
	require.NoError(t, err)
	require.NoError(t, p.kvstore.Set(kvstore.BuildChannelMappingKey(channelID), data))
}

func TestIsOwnRemoteID(t *testing.T) {
	p := newRoutingTestPlugin(t)

	assert.True(t, p.isOwnRemoteID(routeRemoteA))
	assert.True(t, p.isOwnRemoteID(routeRemoteB))
	assert.False(t, p.isOwnRemoteID(routeUnknownR))
	assert.False(t, p.isOwnRemoteID(""))

	// With no own-remote map there is no fallback: nothing is recognized as own.
	p.ownRemoteIDs = nil
	assert.False(t, p.isOwnRemoteID(routeRemoteA))
	assert.False(t, p.isOwnRemoteID(routeRemoteB))
}

func TestServerIDForRemoteID(t *testing.T) {
	p := newRoutingTestPlugin(t)

	id, ok := p.serverIDForRemoteID(routeRemoteB)
	assert.True(t, ok)
	assert.Equal(t, routeServerB, id)

	_, ok = p.serverIDForRemoteID(routeUnknownR)
	assert.False(t, ok)

	_, ok = p.serverIDForRemoteID("")
	assert.False(t, ok)
}

func TestRemoteIDForServer(t *testing.T) {
	p := newRoutingTestPlugin(t)

	assert.Equal(t, routeRemoteB, p.remoteIDForServer(routeServerB))
	// Unknown server has no remote (no fallback).
	assert.Equal(t, "", p.remoteIDForServer("no-such-server"))
}

func TestResolveOutboundServers_MappedChannel(t *testing.T) {
	p := newRoutingTestPlugin(t)
	channelID := model.NewId()
	seedChannelMapping(t, p, channelID, []kvstore.ChannelServerMapping{
		{ServerID: routeServerA, RoomID: routeRoomA},
		{ServerID: routeServerB, RoomID: routeRoomB},
	})

	serverIDs, err := p.resolveOutboundServers(channelID)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{routeServerA, routeServerB}, serverIDs)
}

func TestResolveOutboundServers_UnmappedNonDM(t *testing.T) {
	p := newRoutingTestPlugin(t)
	channelID := model.NewId()

	api := p.API.(*plugintest.API)
	api.On("GetChannel", channelID).Return(&model.Channel{Id: channelID, Type: model.ChannelTypeOpen}, nil)

	serverIDs, err := p.resolveOutboundServers(channelID)
	require.NoError(t, err)
	assert.Empty(t, serverIDs)
}

func TestResolveOutboundServers_UnmappedDMRoutesToRemoteParticipantServer(t *testing.T) {
	p := newRoutingTestPlugin(t)
	channelID := model.NewId()
	localUserID := model.NewId()
	remoteUserID := model.NewId()

	api := p.API.(*plugintest.API)
	api.On("GetChannel", channelID).Return(&model.Channel{Id: channelID, Type: model.ChannelTypeDirect}, nil)
	api.On("GetChannelMembers", channelID, 0, 10).Return(model.ChannelMembers{
		{ChannelId: channelID, UserId: localUserID},
		{ChannelId: channelID, UserId: remoteUserID},
	}, nil)
	api.On("GetUser", localUserID).Return(&model.User{Id: localUserID}, nil)
	remoteB := routeRemoteB
	api.On("GetUser", remoteUserID).Return(&model.User{Id: remoteUserID, RemoteId: &remoteB}, nil)

	serverIDs, err := p.resolveOutboundServers(channelID)
	require.NoError(t, err)
	assert.Equal(t, []string{routeServerB}, serverIDs)
}

func TestReconstructMatrixUserIDFromUsername_PerServer(t *testing.T) {
	p := newRoutingTestPlugin(t)
	api := p.API.(*plugintest.API)
	api.On("LogWarn", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()

	// A bridge scoped to server B must reconstruct against B's domain and prefix,
	// not the flat config or server A.
	bridge := p.newMattermostToMatrixBridge(routeServerB)
	got := bridge.reconstructMatrixUserIDFromUsername("matrixb:alice")
	assert.Equal(t, "@alice:b.example.org", got)

	// A username that does not carry server B's prefix is not a Matrix user there.
	assert.Empty(t, bridge.reconstructMatrixUserIDFromUsername("matrixa:alice"))
}
