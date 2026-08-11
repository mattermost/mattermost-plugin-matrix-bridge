package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/servers"
	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
)

// TestSystemAdminRequired covers §5.1's admin-gate requirements for the whole
// server-management API surface, exercised through the real router (ServeHTTP) so
// the middleware chain - not just the middleware function in isolation - is what's
// under test.
func TestSystemAdminRequired(t *testing.T) {
	t.Run("non-admin gets 403 with no server data in the body", func(t *testing.T) {
		plugin := newTestPluginForAPI(t)
		api := plugin.API.(*plugintest.API)
		userID := model.NewId()
		api.On("HasPermissionTo", userID, model.PermissionManageSystem).Return(false)

		data, err := kvstore.MarshalServersConfig([]kvstore.ServerConfig{{ServerID: "secret-server-id", ServerName: "secret.example.com"}})
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.KeyServersConfig, data))

		req := httptest.NewRequest(http.MethodGet, "/api/v1/servers", nil)
		req.Header.Set("Mattermost-User-ID", userID)
		rec := httptest.NewRecorder()
		plugin.ServeHTTP(nil, rec, req)

		assert.Equal(t, http.StatusForbidden, rec.Code)
		assert.NotContains(t, rec.Body.String(), "secret-server-id")
	})

	t.Run("unauthenticated request gets 401", func(t *testing.T) {
		plugin := newTestPluginForAPI(t)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/servers", nil)
		rec := httptest.NewRecorder()
		plugin.ServeHTTP(nil, rec, req)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("admin passes", func(t *testing.T) {
		plugin := newTestPluginForAPI(t)
		api := plugin.API.(*plugintest.API)
		userID := model.NewId()
		api.On("HasPermissionTo", userID, model.PermissionManageSystem).Return(true)

		req := httptest.NewRequest(http.MethodGet, "/api/v1/servers", nil)
		req.Header.Set("Mattermost-User-ID", userID)
		rec := httptest.NewRecorder()
		plugin.ServeHTTP(nil, rec, req)
		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("autocomplete route enforces the same gate", func(t *testing.T) {
		plugin := newTestPluginForAPI(t)
		api := plugin.API.(*plugintest.API)
		userID := model.NewId()
		api.On("HasPermissionTo", userID, model.PermissionManageSystem).Return(false)

		req := httptest.NewRequest(http.MethodGet, "/api/v1/autocomplete/servers", nil)
		req.Header.Set("Mattermost-User-ID", userID)
		rec := httptest.NewRecorder()
		plugin.ServeHTTP(nil, rec, req)
		assert.Equal(t, http.StatusForbidden, rec.Code)
	})
}

// listKeysErrorKVStore fails ListKeys (and so any keyspace scan built on it, e.g.
// Servers().CountMappedChannels) while leaving Get/Set untouched, so GET /servers
// can be tested with a working registry read but a failing count.
type listKeysErrorKVStore struct{ kvstore.KVStore }

func (e *listKeysErrorKVStore) ListKeys(int, int) ([]string, error) {
	return nil, errors.New("simulated keyspace scan failure")
}

func TestHandleListServers(t *testing.T) {
	t.Run("zero servers yields an empty array, not an error", func(t *testing.T) {
		plugin := newTestPluginForAPI(t)
		req := httptest.NewRequest(http.MethodGet, "/servers", nil)
		rec := httptest.NewRecorder()
		plugin.handleListServers(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		assert.JSONEq(t, `{"servers":[]}`, rec.Body.String())
	})

	t.Run("tokens are absent from the body; has_as_token/has_hs_token reflect the stored values", func(t *testing.T) {
		plugin := newTestPluginForAPI(t)
		entry := kvstore.ServerConfig{
			ServerID: "s1", ServerName: "s1.example.com", ServerURL: "https://s1.example.com",
			ASToken: "secret-as", HSToken: "secret-hs", Enabled: true, SiteURL: "https://s1.example.com",
		}
		data, err := kvstore.MarshalServersConfig([]kvstore.ServerConfig{entry})
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.KeyServersConfig, data))

		req := httptest.NewRequest(http.MethodGet, "/servers", nil)
		rec := httptest.NewRecorder()
		plugin.handleListServers(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		assert.NotContains(t, rec.Body.String(), "secret-as")
		assert.NotContains(t, rec.Body.String(), "secret-hs")

		var body listServersResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		require.Len(t, body.Servers, 1)
		assert.True(t, body.Servers[0].HasASToken)
		assert.True(t, body.Servers[0].HasHSToken)
		assert.False(t, body.Servers[0].IsMigrated)
		require.NotNil(t, body.Servers[0].MappedChannelCount)
		assert.Equal(t, 0, *body.Servers[0].MappedChannelCount)
	})

	t.Run("is_migrated is true exactly for SiteURL empty", func(t *testing.T) {
		plugin := newTestPluginForAPI(t)
		entry := kvstore.ServerConfig{ServerID: "legacy1", ServerName: "legacy.example.com", SiteURL: ""}
		data, err := kvstore.MarshalServersConfig([]kvstore.ServerConfig{entry})
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.KeyServersConfig, data))

		req := httptest.NewRequest(http.MethodGet, "/servers", nil)
		rec := httptest.NewRecorder()
		plugin.handleListServers(rec, req)

		var body listServersResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		require.Len(t, body.Servers, 1)
		assert.True(t, body.Servers[0].IsMigrated)
	})

	t.Run("a failing keyspace scan yields mapped_channel_count null and counts_unavailable true, not zeros or a 500", func(t *testing.T) {
		plugin := newTestPluginForAPI(t)
		entry := kvstore.ServerConfig{ServerID: "s1", ServerName: "s1.example.com"}
		data, err := kvstore.MarshalServersConfig([]kvstore.ServerConfig{entry})
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.KeyServersConfig, data))

		plugin.kvstore = &listKeysErrorKVStore{KVStore: plugin.kvstore}
		plugin.servers = servers.New(plugin.kvstore, pluginLogger{plugin}, pluginHost{plugin})

		req := httptest.NewRequest(http.MethodGet, "/servers", nil)
		rec := httptest.NewRecorder()
		plugin.handleListServers(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		var body listServersResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.True(t, body.CountsUnavailable)
		require.Len(t, body.Servers, 1)
		assert.Nil(t, body.Servers[0].MappedChannelCount)
	})
}

func TestHandleServersHealth(t *testing.T) {
	t.Run("disabled server reports disabled without probing", func(t *testing.T) {
		plugin := newTestPluginForAPI(t)
		entry := kvstore.ServerConfig{ServerID: "s1", ServerName: "s1.example.com", Enabled: false}
		data, err := kvstore.MarshalServersConfig([]kvstore.ServerConfig{entry})
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.KeyServersConfig, data))

		req := httptest.NewRequest(http.MethodGet, "/servers/health", nil)
		rec := httptest.NewRecorder()
		plugin.handleServersHealth(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		var body struct {
			Health map[string]string `json:"health"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Equal(t, "disabled", body.Health["s1"])
	})

	t.Run("no client reports unavailable, never healthy", func(t *testing.T) {
		plugin := newTestPluginForAPI(t)
		entry := kvstore.ServerConfig{ServerID: "s1", ServerName: "s1.example.com", Enabled: true}
		data, err := kvstore.MarshalServersConfig([]kvstore.ServerConfig{entry})
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.KeyServersConfig, data))

		req := httptest.NewRequest(http.MethodGet, "/servers/health", nil)
		rec := httptest.NewRecorder()
		plugin.handleServersHealth(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		var body struct {
			Health map[string]string `json:"health"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Equal(t, "unavailable", body.Health["s1"])
	})
}
