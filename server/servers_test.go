package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
)

func TestNormalizeServerEndpoint(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		want    string
		wantErr bool
	}{
		{name: "https default port", url: "https://example.com", want: "example.com:443"},
		{name: "http default port", url: "http://example.com", want: "example.com:80"},
		{name: "explicit port", url: "https://example.com:8448", want: "example.com:8448"},
		{name: "uppercase host is lowercased", url: "https://Example.COM", want: "example.com:443"},
		{name: "trailing slash ignored", url: "https://example.com/", want: "example.com:443"},
		{name: "missing scheme errors", url: "example.com", wantErr: true},
		{name: "empty URL errors", url: "", wantErr: true},
		{name: "missing host errors", url: "https://", wantErr: true},
		{name: "unsupported scheme errors", url: "ftp://example.com", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeServerEndpoint(tt.url)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestNormalizeServerEndpointDistinguishesPorts(t *testing.T) {
	endpoint8008, err := normalizeServerEndpoint("http://localhost:8008")
	require.NoError(t, err)
	endpoint8009, err := normalizeServerEndpoint("http://localhost:8009")
	require.NoError(t, err)
	assert.NotEqual(t, endpoint8008, endpoint8009, "distinct ports must produce distinct endpoints")
}

func TestEventDomainFromEndpoint(t *testing.T) {
	assert.Equal(t, "localhost_8008", eventDomainFromEndpoint("localhost:8008"))
	assert.NotEqual(t, eventDomainFromEndpoint("localhost:8008"), eventDomainFromEndpoint("localhost:8009"),
		"EventDomain must stay distinct for endpoints that only differ by port")
}

// newTestPluginForServers returns a Plugin with an in-memory KV store, ready for
// servers.go tests. The unreachable-server-name-probe URLs used throughout intentionally
// point at a closed local port so the HTTP client fails fast instead of timing out.
func newTestPluginForServers(t *testing.T) *Plugin {
	t.Helper()
	plugin := setupPluginForTest()
	plugin.kvstore = NewMemoryKVStore()
	plugin.configuration = &configuration{}
	mockAnyLogCalls(plugin.API.(*plugintest.API))
	return plugin
}

const unreachableURL = "http://127.0.0.1:1"

func TestResolveServerName(t *testing.T) {
	t.Run("configuredName short-circuits without any HTTP call", func(t *testing.T) {
		plugin := newTestPluginForServers(t)
		// NormalizeServerName strips scheme/port/trailing-slash but does not lowercase -
		// case is preserved verbatim, matching how the value would appear in Matrix IDs.
		name, err := plugin.resolveServerName(unreachableURL, "Configured.Example.COM")
		require.NoError(t, err)
		assert.Equal(t, "Configured.Example.COM", name)
	})

	t.Run("key server endpoint supplies the name when reachable", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"server_name": "discovered.example.com"})
		}))
		defer server.Close()

		plugin := newTestPluginForServers(t)
		name, err := plugin.resolveServerName(server.URL, "")
		require.NoError(t, err)
		assert.Equal(t, "discovered.example.com", name)
	})

	t.Run("404 falls through to hostname", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer server.Close()

		plugin := newTestPluginForServers(t)
		name, err := plugin.resolveServerName(server.URL, "")
		require.NoError(t, err)

		parsed, parseErr := url.Parse(server.URL)
		require.NoError(t, parseErr)
		assert.Equal(t, parsed.Hostname(), name)
	})

	t.Run("malformed response body falls through to hostname", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("not json"))
		}))
		defer server.Close()

		plugin := newTestPluginForServers(t)
		name, err := plugin.resolveServerName(server.URL, "")
		require.NoError(t, err)

		parsed, parseErr := url.Parse(server.URL)
		require.NoError(t, parseErr)
		assert.Equal(t, parsed.Hostname(), name)
	})

	t.Run("response missing server_name falls through to hostname", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"verify_keys": {}}`))
		}))
		defer server.Close()

		plugin := newTestPluginForServers(t)
		name, err := plugin.resolveServerName(server.URL, "")
		require.NoError(t, err)

		parsed, parseErr := url.Parse(server.URL)
		require.NoError(t, parseErr)
		assert.Equal(t, parsed.Hostname(), name)
	})

	t.Run("transport error falls through to hostname and never fails for a parseable URL", func(t *testing.T) {
		plugin := newTestPluginForServers(t)
		name, err := plugin.resolveServerName(unreachableURL, "")
		require.NoError(t, err)
		assert.Equal(t, "127.0.0.1", name)
	})
}

// newTestPluginForAddServer additionally mocks the best-effort registerServerForSharedChannels
// / refreshServersAndBroadcast side effects AddServer triggers, so tests can focus on
// registry semantics without those calls panicking on an unmocked expectation.
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

func TestAddServer(t *testing.T) {
	t.Run("rejects an endpoint already live in the registry", func(t *testing.T) {
		plugin := newTestPluginForAddServer(t)

		_, err := plugin.AddServer("https://a.example.com", "as1", "hs1", "", "", "first.example.com")
		require.NoError(t, err)

		_, err = plugin.AddServer("https://a.example.com", "as2", "hs2", "", "", "second.example.com")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "already registered")
	})

	t.Run("rejects a server name that duplicates an existing entry's", func(t *testing.T) {
		plugin := newTestPluginForAddServer(t)

		_, err := plugin.AddServer("https://a.example.com", "as1", "hs1", "", "", "shared.example.com")
		require.NoError(t, err)

		_, err = plugin.AddServer("https://b.example.com", "as2", "hs2", "", "", "shared.example.com")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "conflicts")
	})

	t.Run("mints a fresh ID when none is supplied", func(t *testing.T) {
		plugin := newTestPluginForAddServer(t)
		id, err := plugin.AddServer("https://a.example.com", "as1", "hs1", "", "", "a.example.com")
		require.NoError(t, err)
		assert.True(t, model.IsValidId(id))
	})

	t.Run("re-adopts a supplied server ID verbatim", func(t *testing.T) {
		plugin := newTestPluginForAddServer(t)
		priorID := model.NewId()

		id, err := plugin.AddServer("https://a.example.com", "as1", "hs1", "", priorID, "a.example.com")
		require.NoError(t, err)
		assert.Equal(t, priorID, id)
	})

	t.Run("rejects a malformed server ID", func(t *testing.T) {
		plugin := newTestPluginForAddServer(t)
		_, err := plugin.AddServer("https://a.example.com", "as1", "hs1", "", "not-a-valid-id", "a.example.com")
		require.Error(t, err)
	})

	t.Run("rejects a server ID colliding with a live entry", func(t *testing.T) {
		plugin := newTestPluginForAddServer(t)
		id1, err := plugin.AddServer("https://a.example.com", "as1", "hs1", "", "", "a.example.com")
		require.NoError(t, err)

		_, err = plugin.AddServer("https://b.example.com", "as2", "hs2", "", id1, "b.example.com")
		require.Error(t, err)
	})

	t.Run("registers a remote and refreshes the client caches", func(t *testing.T) {
		plugin := newTestPluginForAddServer(t)
		id, err := plugin.AddServer("https://a.example.com", "as1", "hs1", "", "", "a.example.com")
		require.NoError(t, err)

		servers, err := plugin.getServers()
		require.NoError(t, err)
		require.Len(t, servers, 1)
		assert.NotEmpty(t, servers[0].RemoteID, "AddServer must register a shared-channels remote")

		assert.NotNil(t, plugin.getMatrixClient(id), "refreshServersAndBroadcast must rebuild this node's client cache")
	})

	t.Run("EventDomain is derived from the endpoint and stays distinct across ports", func(t *testing.T) {
		plugin := newTestPluginForAddServer(t)
		id1, err := plugin.AddServer("http://localhost:8008", "as1", "hs1", "", "", "localhost8008.example.com")
		require.NoError(t, err)
		id2, err := plugin.AddServer("http://localhost:8009", "as2", "hs2", "", "", "localhost8009.example.com")
		require.NoError(t, err)

		servers, err := plugin.getServers()
		require.NoError(t, err)

		domains := map[string]string{}
		for _, s := range servers {
			domains[s.ServerID] = s.EventDomain
		}
		assert.NotEqual(t, domains[id1], domains[id2])
	})
}

func TestRemoveServer(t *testing.T) {
	t.Run("unknown ID returns false, not an error", func(t *testing.T) {
		plugin := newTestPluginForAddServer(t)
		removed, err := plugin.RemoveServer("nonexistent")
		require.NoError(t, err)
		assert.False(t, removed)
	})

	t.Run("refuses to remove the migrated entry (SiteURL empty) and leaves it registered", func(t *testing.T) {
		plugin := newTestPluginForAddServer(t)
		entry := kvstore.ServerConfig{ServerID: "legacy1", ServerName: "legacy.example.com", SiteURL: ""}
		data, err := kvstore.MarshalServersConfig([]kvstore.ServerConfig{entry})
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.KeyServersConfig, data))

		removed, err := plugin.RemoveServer("legacy1")
		require.Error(t, err)
		assert.False(t, removed)

		servers, err := plugin.getServers()
		require.NoError(t, err)
		require.Len(t, servers, 1, "the migrated entry must remain registered")
		assert.Equal(t, "legacy1", servers[0].ServerID)
	})

	t.Run("removes an entry but leaves its namespaced keys and channel mappings intact", func(t *testing.T) {
		plugin := newTestPluginForAddServer(t)
		id, err := plugin.AddServer("https://a.example.com", "as1", "hs1", "", "", "a.example.com")
		require.NoError(t, err)

		ghostKey := kvstore.BuildGhostUserKey(id, "mmuser1")
		require.NoError(t, plugin.kvstore.Set(ghostKey, []byte("@_mattermost_mmuser1:a.example.com")))

		channelID := model.NewId()
		mappingData, err := kvstore.BuildSingleChannelMapping(id, "!room:a.example.com")
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.BuildChannelMappingKey(channelID), mappingData))

		removed, err := plugin.RemoveServer(id)
		require.NoError(t, err)
		assert.True(t, removed)

		servers, err := plugin.getServers()
		require.NoError(t, err)
		assert.Empty(t, servers)

		val, err := plugin.kvstore.Get(ghostKey)
		require.NoError(t, err)
		assert.Equal(t, "@_mattermost_mmuser1:a.example.com", string(val), "namespaced keys must survive removal - this is what re-adoption depends on")

		mappingVal, err := plugin.kvstore.Get(kvstore.BuildChannelMappingKey(channelID))
		require.NoError(t, err)
		assert.NotEmpty(t, mappingVal, "channel mappings must survive removal")
	})

	t.Run("survives a failing unregister call", func(t *testing.T) {
		plugin := newTestPluginForServers(t)
		api := plugin.API.(*plugintest.API)
		api.On("GetUserByUsername", "mattermost-bridge").Return(nil, &model.AppError{Message: "not found"}).Maybe()
		api.On("GetUsers", mock.Anything).Return([]*model.User{{Id: model.NewId()}}, nil).Maybe()
		api.On("RegisterPluginForSharedChannels", mock.Anything).Return("remote-1", nil).Maybe()
		api.On("PublishPluginClusterEvent", mock.Anything, mock.Anything).Return(nil).Maybe()
		api.On("UnregisterPluginRemoteForSharedChannels", mock.Anything).Return(&model.AppError{Message: "boom"}).Maybe()
		mockAnyLogCalls(api)

		id, err := plugin.AddServer("https://a.example.com", "as1", "hs1", "", "", "a.example.com")
		require.NoError(t, err)

		removed, err := plugin.RemoveServer(id)
		require.NoError(t, err, "a failing unregister must not fail RemoveServer - the registry write already succeeded")
		assert.True(t, removed)
	})
}

// TestReAdoptionRestoresNamespacedRecords covers §5.1's re-adoption round trip: remove a
// server, then AddServer with the same ID must make its namespaced ghost-user key and
// channel mapping reachable again.
func TestReAdoptionRestoresNamespacedRecords(t *testing.T) {
	plugin := newTestPluginForAddServer(t)

	id, err := plugin.AddServer("https://a.example.com", "as1", "hs1", "", "", "a.example.com")
	require.NoError(t, err)

	ghostKey := kvstore.BuildGhostUserKey(id, "mmuser1")
	require.NoError(t, plugin.kvstore.Set(ghostKey, []byte("@_mattermost_mmuser1:a.example.com")))

	channelID := model.NewId()
	mappingData, err := kvstore.BuildSingleChannelMapping(id, "!room:a.example.com")
	require.NoError(t, err)
	mappingKey := kvstore.BuildChannelMappingKey(channelID)
	require.NoError(t, plugin.kvstore.Set(mappingKey, mappingData))

	removed, err := plugin.RemoveServer(id)
	require.NoError(t, err)
	require.True(t, removed)

	// Re-adopt at the same endpoint with the same ID.
	reAdoptedID, err := plugin.AddServer("https://a.example.com", "as1-new", "hs1-new", "", id, "a.example.com")
	require.NoError(t, err)
	assert.Equal(t, id, reAdoptedID)

	ghostVal, err := plugin.kvstore.Get(ghostKey)
	require.NoError(t, err)
	assert.Equal(t, "@_mattermost_mmuser1:a.example.com", string(ghostVal), "ghost-user key must resolve again after re-adoption")

	mappingVal, err := plugin.kvstore.Get(mappingKey)
	require.NoError(t, err)
	mappings, err := kvstore.ParseChannelServerMappings(mappingVal)
	require.NoError(t, err)
	assert.Equal(t, "!room:a.example.com", kvstore.RoomIDForServer(mappings, id), "channel mapping must resolve again after re-adoption")
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

		servers, err := plugin.getServers()
		require.NoError(t, err)
		require.Len(t, servers, 1)
		assert.Empty(t, servers[0].SiteURL, "the migrated entry must keep SiteURL empty")
	})
}
