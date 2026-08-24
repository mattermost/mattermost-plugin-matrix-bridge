package main

import (
	"fmt"
	"sync"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/servers"
	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
)

// This file covers what stays in main after the server/servers move: the legacy
// migration seeding path (materializeServerFromLegacyConfig, which must keep its
// platform LoadPluginConfiguration dependency out of the leaf package), the
// shared-channels remote-registration helpers, and the per-node Matrix client cache
// (initMatrixClients and friends). Registry reads/mutations/errors themselves are
// covered by server/servers/service_test.go against a fake Host.

// newTestPluginForServers returns a Plugin with an in-memory KV store and a wired
// servers.Service, ready for tests in this file. The unreachable-server-name-probe
// URLs used throughout intentionally point at a closed local port so the HTTP
// client fails fast instead of timing out.
func newTestPluginForServers(t *testing.T) *Plugin {
	t.Helper()
	plugin := setupPluginForTest()
	plugin.kvstore = NewMemoryKVStore()
	plugin.servers = servers.New(plugin.kvstore, pluginLogger{plugin}, pluginHost{plugin})
	plugin.configuration = &configuration{}
	mockAnyLogCalls(plugin.API.(*plugintest.API))
	return plugin
}

const unreachableURL = "http://127.0.0.1:1"

// newTestPluginForAddServer additionally mocks the shared-channels remote registration
// and refreshServersAndBroadcast side effects Servers().Add triggers, so tests can focus
// on registry semantics without those calls panicking on an unmocked expectation.
func newTestPluginForAddServer(t *testing.T) *Plugin {
	t.Helper()
	plugin := newTestPluginForServers(t)
	api := plugin.API.(*plugintest.API)

	api.On("GetUserByUsername", "mattermost-bridge").Return(nil, &model.AppError{Message: "not found"}).Maybe()
	api.On("GetUsers", mock.Anything).Return([]*model.User{{Id: model.NewId()}}, nil).Maybe()
	api.On("RegisterPluginForSharedChannels", mock.Anything).Return("remote-"+model.NewId(), nil).Maybe()
	api.On("PublishPluginClusterEvent", mock.Anything, mock.Anything).Return(nil).Maybe()
	api.On("UnregisterPluginRemoteForSharedChannels", mock.Anything).Return(nil).Maybe()
	mockAnyLogCalls(api)

	return plugin
}

// mockAnyLogCalls allows every LogInfo/LogWarn/LogError call with any arguments -
// initMatrixClients builds a real matrix.Client per server, which logs its rate-limit
// configuration at construction time.
func mockAnyLogCalls(api *plugintest.API) {
	for _, method := range []string{"LogInfo", "LogWarn", "LogError"} {
		for n := 1; n <= 11; n += 2 {
			args := make([]any, n)
			for i := range args {
				args[i] = mock.Anything
			}
			api.On(method, args...).Return().Maybe()
		}
	}
}

func TestMaterializeServerFromLegacyConfig(t *testing.T) {
	t.Run("fresh install with no legacy URL returns empty, not an error", func(t *testing.T) {
		plugin := newTestPluginForServers(t)
		api := plugin.API.(*plugintest.API)
		api.On("LoadPluginConfiguration", mock.Anything).Return(nil).Maybe()

		id, err := plugin.materializeServerFromLegacyConfig()
		require.NoError(t, err)
		assert.Empty(t, id)
	})

	t.Run("idempotent: a second call with the same endpoint returns the same ID", func(t *testing.T) {
		plugin := newTestPluginForServers(t)
		api := plugin.API.(*plugintest.API)
		// newTestPluginForServers's default LoadPluginConfiguration expectation (via
		// setupPluginForTest) is registered first and would otherwise shadow this one.
		clearMockExpectations(api)
		mockAnyLogCalls(api)
		api.On("LoadPluginConfiguration", mock.Anything).Run(func(args mock.Arguments) {
			dest := args.Get(0).(*legacyServerConfig)
			dest.MatrixServerURL = "https://legacy.example.com"
			dest.MatrixASToken = "legacy-as"
			dest.MatrixHSToken = "legacy-hs"
		}).Return(nil)

		id1, err := plugin.materializeServerFromLegacyConfig()
		require.NoError(t, err)
		require.NotEmpty(t, id1)

		id2, err := plugin.materializeServerFromLegacyConfig()
		require.NoError(t, err)
		assert.Equal(t, id1, id2)

		servers, err := plugin.servers.List()
		require.NoError(t, err)
		require.Len(t, servers, 1)
		assert.Empty(t, servers[0].SiteURL, "the migrated entry must keep SiteURL empty")
	})
}

// TestRegisterForSharedChannelsFailureIsolation covers §5.1's shared-channels remote
// registration requirement: one server's registration failing must not block the others,
// and the overall call must still succeed.
func TestRegisterForSharedChannelsFailureIsolation(t *testing.T) {
	plugin := newTestPluginForServers(t)
	api := plugin.API.(*plugintest.API)
	api.On("GetUserByUsername", "mattermost-bridge").Return(nil, &model.AppError{Message: "not found"}).Maybe()
	api.On("GetUsers", mock.Anything).Return([]*model.User{{Id: model.NewId()}}, nil).Maybe()
	mockAnyLogCalls(api)

	seed := []kvstore.ServerConfig{
		{ServerID: "serverA", ServerName: "a.example.com", Enabled: true, SiteURL: "https://a.example.com"},
		{ServerID: "serverB", ServerName: "b.example.com", Enabled: true, SiteURL: "https://b.example.com"},
	}
	data, err := kvstore.MarshalServersConfig(seed)
	require.NoError(t, err)
	require.NoError(t, plugin.kvstore.Set(kvstore.KeyServersConfig, data))

	api.On("RegisterPluginForSharedChannels", mock.MatchedBy(func(opts model.RegisterPluginOpts) bool {
		return opts.SiteURL == "https://a.example.com"
	})).Return("", &model.AppError{Message: "registration failed"})
	api.On("RegisterPluginForSharedChannels", mock.MatchedBy(func(opts model.RegisterPluginOpts) bool {
		return opts.SiteURL == "https://b.example.com"
	})).Return("remote-b", nil)

	err = plugin.registerForSharedChannels()
	require.NoError(t, err, "one server's registration failure must not fail the overall call")

	servers, err := plugin.servers.List()
	require.NoError(t, err)
	byID := map[string]kvstore.ServerConfig{}
	for _, s := range servers {
		byID[s.ServerID] = s
	}
	assert.Empty(t, byID["serverA"].RemoteID, "the failing server must keep no RemoteID")
	assert.Equal(t, "remote-b", byID["serverB"].RemoteID, "the succeeding server's RemoteID must still be persisted")
}

// TestRegisterForSharedChannelsSurvivesConcurrentRemoval covers the "merge does not
// clobber a concurrently added/removed server" requirement: registerForSharedChannels
// reads a snapshot, makes network calls, then merges the results into whatever the
// registry looks like at write time - not the stale snapshot it started with.
func TestRegisterForSharedChannelsSurvivesConcurrentRemoval(t *testing.T) {
	plugin := newTestPluginForServers(t)
	api := plugin.API.(*plugintest.API)
	api.On("GetUserByUsername", "mattermost-bridge").Return(nil, &model.AppError{Message: "not found"}).Maybe()
	api.On("GetUsers", mock.Anything).Return([]*model.User{{Id: model.NewId()}}, nil).Maybe()
	api.On("UnregisterPluginRemoteForSharedChannels", mock.Anything).Return(nil).Maybe()
	api.On("PublishPluginClusterEvent", mock.Anything, mock.Anything).Return(nil).Maybe()
	mockAnyLogCalls(api)

	seed := []kvstore.ServerConfig{
		{ServerID: "serverA", ServerName: "a.example.com", Enabled: true, SiteURL: "https://a.example.com"},
		{ServerID: "serverB", ServerName: "b.example.com", Enabled: true, SiteURL: "https://b.example.com"},
	}
	data, err := kvstore.MarshalServersConfig(seed)
	require.NoError(t, err)
	require.NoError(t, plugin.kvstore.Set(kvstore.KeyServersConfig, data))

	// Remove serverB "concurrently" - from inside the mocked network call for serverA,
	// which registerForSharedChannels has already read a snapshot including serverB for.
	api.On("RegisterPluginForSharedChannels", mock.MatchedBy(func(opts model.RegisterPluginOpts) bool {
		return opts.SiteURL == "https://a.example.com"
	})).Return("remote-a", nil).Run(func(mock.Arguments) {
		removed, removeErr := plugin.servers.Remove("serverB")
		require.NoError(t, removeErr)
		require.True(t, removed)
	})
	api.On("RegisterPluginForSharedChannels", mock.MatchedBy(func(opts model.RegisterPluginOpts) bool {
		return opts.SiteURL == "https://b.example.com"
	})).Return("remote-b", nil)

	err = plugin.registerForSharedChannels()
	require.NoError(t, err)

	servers, err := plugin.servers.List()
	require.NoError(t, err)
	require.Len(t, servers, 1, "serverB's concurrent removal must survive the merge, not be resurrected by it")
	assert.Equal(t, "serverA", servers[0].ServerID)
	assert.Equal(t, "remote-a", servers[0].RemoteID)
}

// TestInitMatrixClientsBuildsClientForDisabledServer covers §5.1: initMatrixClients must
// build a client for every registered server, including disabled ones, so /matrix status
// can still probe and report their health - only routing consults Enabled.
func TestInitMatrixClientsBuildsClientForDisabledServer(t *testing.T) {
	plugin := newTestPluginForServers(t)

	data, err := kvstore.MarshalServersConfig([]kvstore.ServerConfig{
		{ServerID: "disabled1", ServerURL: "https://disabled.example.com", ServerName: "disabled.example.com", Enabled: false},
	})
	require.NoError(t, err)
	require.NoError(t, plugin.kvstore.Set(kvstore.KeyServersConfig, data))

	require.NoError(t, plugin.initMatrixClients())
	assert.NotNil(t, plugin.getMatrixClient("disabled1"), "initMatrixClients must build a client even for a disabled server")

	server, ok := plugin.cachedServerConfig("disabled1")
	require.True(t, ok, "initMatrixClients must also populate the serverConfigs cache for a disabled server")
	assert.False(t, server.Enabled)
}

// TestInitMatrixClientsBuildsServerConfigsCache covers the serverConfigs cache
// initMatrixClients builds alongside matrixClients/remoteToServerID/ownRemoteIDs: it must
// hold every registered server's full registry entry (Enabled, HSToken, RemoteID), and
// cachedServerConfigs must return exactly that set for callers that scan the whole
// registry (e.g. Matrix webhook auth).
func TestInitMatrixClientsBuildsServerConfigsCache(t *testing.T) {
	plugin := newTestPluginForServers(t)

	data, err := kvstore.MarshalServersConfig([]kvstore.ServerConfig{
		{ServerID: "serverA", ServerURL: "https://a.example.com", ServerName: "a.example.com", HSToken: "hs-a", Enabled: true, RemoteID: "remote-a"},
		{ServerID: "serverB", ServerURL: "https://b.example.com", ServerName: "b.example.com", HSToken: "hs-b", Enabled: false, RemoteID: "remote-b"},
	})
	require.NoError(t, err)
	require.NoError(t, plugin.kvstore.Set(kvstore.KeyServersConfig, data))

	require.NoError(t, plugin.initMatrixClients())

	serverA, ok := plugin.cachedServerConfig("serverA")
	require.True(t, ok)
	assert.Equal(t, "hs-a", serverA.HSToken)
	assert.True(t, serverA.Enabled)
	assert.Equal(t, "remote-a", serverA.RemoteID)

	serverB, ok := plugin.cachedServerConfig("serverB")
	require.True(t, ok)
	assert.False(t, serverB.Enabled)

	_, ok = plugin.cachedServerConfig("nonexistent")
	assert.False(t, ok, "a server absent from the registry must not be found in the cache")

	all, ok := plugin.cachedServerConfigs()
	require.True(t, ok)
	assert.Len(t, all, 2)
}

// TestInitMatrixClientsNoopsWithNilKVStore covers OnConfigurationChange firing before
// OnActivate has initialized the store (and, equivalently, before p.servers exists).
func TestInitMatrixClientsNoopsWithNilKVStore(t *testing.T) {
	plugin := &Plugin{}
	plugin.logger = &testLogger{}

	require.NoError(t, plugin.initMatrixClients())
	assert.Nil(t, plugin.getMatrixClient("anything"))

	_, ok := plugin.cachedServerConfig("anything")
	assert.False(t, ok, "the serverConfigs cache must report not-found, not KV-fallback, when it hasn't been built")

	_, ok = plugin.cachedServerConfigs()
	assert.False(t, ok, "cachedServerConfigs must report the cache as not built")
}

// TestInitMatrixClientsReturnsErrorOnRegistryReadFailure covers §5.1: a registry read
// failure must surface as an error rather than silently leaving the existing (possibly
// nil) client maps in place.
func TestInitMatrixClientsReturnsErrorOnRegistryReadFailure(t *testing.T) {
	plugin := newTestPluginForServers(t)
	plugin.kvstore = &erroringKVStore{KVStore: NewMemoryKVStore(), errOnGetKey: kvstore.KeyServersConfig}
	plugin.servers = servers.New(plugin.kvstore, pluginLogger{plugin}, pluginHost{plugin})

	err := plugin.initMatrixClients()
	require.Error(t, err)
}

// TestInitMatrixClientsConcurrentRebuildMatchesFinalRegistry covers §5.1's "concurrent
// rebuild doesn't resurrect a stale snapshot" requirement. initMatrixClientsMu fully
// serializes each rebuild's read-compute-swap cycle, so many concurrent Add calls
// (each of which ends in refreshServersAndBroadcast -> initMatrixClients) must still
// leave the client cache exactly matching the final registry - never missing an entry
// that a later, still-in-flight rebuild would have added, and never holding one from a
// registry state that no longer exists.
func TestInitMatrixClientsConcurrentRebuildMatchesFinalRegistry(t *testing.T) {
	plugin := newTestPluginForServers(t)
	plugin.kvstore = newCASConflictKVStore() // real CAS semantics - required for concurrent Add to not lose registrations
	plugin.servers = servers.New(plugin.kvstore, pluginLogger{plugin}, pluginHost{plugin})
	api := plugin.API.(*plugintest.API)
	api.On("GetUserByUsername", "mattermost-bridge").Return(nil, &model.AppError{Message: "not found"}).Maybe()
	api.On("GetUsers", mock.Anything).Return([]*model.User{{Id: model.NewId()}}, nil).Maybe()
	api.On("RegisterPluginForSharedChannels", mock.Anything).Return("remote-"+model.NewId(), nil).Maybe()
	api.On("PublishPluginClusterEvent", mock.Anything, mock.Anything).Return(nil).Maybe()
	mockAnyLogCalls(api)

	const n = 20
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := addTestServer(plugin, fmt.Sprintf("https://server%d.example.com", i), "as", fmt.Sprintf("hs%d", i), "", "", fmt.Sprintf("server%d.example.com", i))
			assert.NoError(t, err)
		}(i)
	}
	wg.Wait()

	servers, err := plugin.servers.List()
	require.NoError(t, err)
	require.Len(t, servers, n)

	for _, s := range servers {
		assert.NotNil(t, plugin.getMatrixClient(s.ServerID), "server %s missing from client cache after concurrent Add calls - a rebuild resurrected a stale snapshot", s.ServerID)
	}

	plugin.matrixClientsLock.RLock()
	cacheSize := len(plugin.matrixClients)
	plugin.matrixClientsLock.RUnlock()
	assert.Equal(t, len(servers), cacheSize, "client cache must exactly match the final registry")
}

// TestServerDomainForIDUsesCachedSnapshot pins serverDomainForID to the serverConfigs
// snapshot. Its one caller, isGhostUser, runs one to three times per inbound Matrix
// event, so a full registry read per call would sit on the hottest inbound path there is.
func TestServerDomainForIDUsesCachedSnapshot(t *testing.T) {
	const ghostOnServer = "@_mattermost_abc123:matrix.example.com"

	plugin := setupPluginForTest()
	plugin.kvstore = NewMemoryKVStore()
	plugin.servers = servers.New(plugin.kvstore, pluginLogger{plugin}, pluginHost{plugin})
	serverID, _ := registerTestServer(t, plugin, "https://matrix.example.com", "matrix.example.com", nil)

	// Failing every read of the registry key is what makes this a real assertion: a KV
	// round trip left on this path would surface below as an error, not a domain.
	plugin.kvstore = &erroringKVStore{KVStore: plugin.kvstore, errOnGetKey: kvstore.KeyServersConfig}
	plugin.servers = servers.New(plugin.kvstore, pluginLogger{plugin}, pluginHost{plugin})

	domain, err := plugin.serverDomainForID(serverID)
	require.NoError(t, err)
	assert.Equal(t, "matrix.example.com", domain)

	isGhost, err := plugin.isGhostUser(serverID, ghostOnServer)
	require.NoError(t, err)
	assert.True(t, isGhost)

	// The direct-KV fallback still applies when the snapshot holds no entry for the
	// server, and a failure there must surface as an error - never as "not a ghost user",
	// which would let the bridge re-import its own ghost events (see isGhostUser).
	plugin.serverConfigs = nil

	_, err = plugin.serverDomainForID(serverID)
	require.Error(t, err)

	_, err = plugin.isGhostUser(serverID, ghostOnServer)
	require.Error(t, err)
}
