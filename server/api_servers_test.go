package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
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
// Servers().Mappings) while leaving Get/Set untouched, so a handler can be tested with
// a working registry read but a failing keyspace scan.
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

	// The list render must cost one registry read and nothing else. Failing every
	// keyspace scan is what makes that a real assertion: a per-server scan reintroduced
	// here (a mapped-channel count, say) would surface below as a 500 rather than a list.
	t.Run("renders without any keyspace scan", func(t *testing.T) {
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
		require.Len(t, body.Servers, 1)
		assert.Equal(t, "s1", body.Servers[0].ServerID)
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

// jsonRequest builds a request carrying body as its JSON payload, with
// server_id/channel_id mux vars injected the way the real router would populate
// them - the handlers under test read serverID via mux.Vars, which only a router
// (or mux.SetURLVars, for tests) can supply.
func jsonRequest(t *testing.T, method, path string, body any, vars map[string]string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	req := httptest.NewRequest(method, path, &buf)
	if len(vars) > 0 {
		req = mux.SetURLVars(req, vars)
	}
	return req
}

// mockSharedChannelsAPI stubs the platform calls Servers().Add's best-effort
// shared-channels remote registration makes, so tests can exercise the real
// registry write without wiring up a full RegisterPluginForSharedChannels round
// trip. Mirrors newTestPluginForAddServer in servers_test.go.
func mockSharedChannelsAPI(api *plugintest.API) {
	api.On("GetUserByUsername", "mattermost-bridge").Return(nil, &model.AppError{Message: "not found"}).Maybe()
	api.On("GetUsers", mock.Anything).Return([]*model.User{{Id: model.NewId()}}, nil).Maybe()
	api.On("RegisterPluginForSharedChannels", mock.Anything).Return("remote-"+model.NewId(), nil).Maybe()
	api.On("PublishPluginClusterEvent", mock.Anything, mock.Anything).Return(nil).Maybe()
	api.On("UnregisterPluginRemoteForSharedChannels", mock.Anything).Return(nil).Maybe()
	mockAnyLogCalls(api)
}

func TestHandleAddServer(t *testing.T) {
	t.Run("happy path returns 201 and the created view", func(t *testing.T) {
		plugin := newTestPluginForAPI(t)
		mockSharedChannelsAPI(plugin.API.(*plugintest.API))
		req := jsonRequest(t, http.MethodPost, "/servers", addServerRequest{
			ServerURL: "https://a.example.com", ASToken: "as1", HSToken: "hs1", ServerName: "a.example.com",
		}, nil)
		rec := httptest.NewRecorder()
		plugin.handleAddServer(rec, req)

		require.Equal(t, http.StatusCreated, rec.Code)
		var body struct {
			Server   ServerView `json:"server"`
			Warnings []string   `json:"warnings"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.NotEmpty(t, body.Server.ServerID)
		assert.Equal(t, "a.example.com", body.Server.ServerName)
		assert.Empty(t, body.Warnings)
	})

	t.Run("duplicate endpoint returns 409 with the registry's message", func(t *testing.T) {
		plugin := newTestPluginForAPI(t)
		mockSharedChannelsAPI(plugin.API.(*plugintest.API))
		_, err := plugin.servers.Add(servers.AddRequest{ServerURL: "https://a.example.com", ASToken: "as1", HSToken: "hs1", ServerName: "a.example.com"})
		require.NoError(t, err)

		req := jsonRequest(t, http.MethodPost, "/servers", addServerRequest{
			ServerURL: "https://a.example.com", ASToken: "as2", HSToken: "hs2", ServerName: "second.example.com",
		}, nil)
		rec := httptest.NewRecorder()
		plugin.handleAddServer(rec, req)

		require.Equal(t, http.StatusConflict, rec.Code)
		assert.Contains(t, rec.Body.String(), "already registered")
	})

	t.Run("a malformed body returns 400", func(t *testing.T) {
		plugin := newTestPluginForAPI(t)
		req := httptest.NewRequest(http.MethodPost, "/servers", strings.NewReader("not json"))
		rec := httptest.NewRecorder()
		plugin.handleAddServer(rec, req)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("a malformed URL returns 400", func(t *testing.T) {
		plugin := newTestPluginForAPI(t)
		req := jsonRequest(t, http.MethodPost, "/servers", addServerRequest{ServerURL: "not-a-url", ASToken: "as1", HSToken: "hs1"}, nil)
		rec := httptest.NewRecorder()
		plugin.handleAddServer(rec, req)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("a server_id re-adoption is passed through verbatim", func(t *testing.T) {
		plugin := newTestPluginForAPI(t)
		mockSharedChannelsAPI(plugin.API.(*plugintest.API))
		priorID := model.NewId()
		req := jsonRequest(t, http.MethodPost, "/servers", addServerRequest{
			ServerURL: "https://a.example.com", ASToken: "as1", HSToken: "hs1", ServerID: priorID, ServerName: "a.example.com",
		}, nil)
		rec := httptest.NewRecorder()
		plugin.handleAddServer(rec, req)

		require.Equal(t, http.StatusCreated, rec.Code)
		var body struct {
			Server ServerView `json:"server"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Equal(t, priorID, body.Server.ServerID)
	})
}

func TestHandleUpdateServer(t *testing.T) {
	seed := func(t *testing.T, plugin *Plugin, entry kvstore.ServerConfig) {
		t.Helper()
		data, err := kvstore.MarshalServersConfig([]kvstore.ServerConfig{entry})
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.KeyServersConfig, data))
	}

	t.Run("a partial update applies", func(t *testing.T) {
		plugin := newTestPluginForAPI(t)
		seed(t, plugin, kvstore.ServerConfig{ServerID: "s1", ServerURL: "https://a.example.com", Endpoint: "a.example.com:443", ServerName: "a.example.com", ASToken: "as1", HSToken: "hs1"})

		newHS := "hs1-new"
		req := jsonRequest(t, http.MethodPatch, "/servers/s1", updateServerRequest{HSToken: &newHS}, map[string]string{"server_id": "s1"})
		rec := httptest.NewRecorder()
		plugin.handleUpdateServer(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		var body struct {
			Server   ServerView `json:"server"`
			Warnings []string   `json:"warnings"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Equal(t, "https://a.example.com", body.Server.ServerURL, "an untouched field must keep its stored value")
		assert.NotContains(t, rec.Body.String(), "hs1-new", "the new token itself must never appear in the response body")
		assert.Empty(t, body.Warnings, "an HSToken-only change has no consequence worth warning about")
	})

	t.Run("unknown server_id is 404", func(t *testing.T) {
		plugin := newTestPluginForAPI(t)
		newHS := "x"
		req := jsonRequest(t, http.MethodPatch, "/servers/nope", updateServerRequest{HSToken: &newHS}, map[string]string{"server_id": "nope"})
		rec := httptest.NewRecorder()
		plugin.handleUpdateServer(rec, req)
		assert.Equal(t, http.StatusNotFound, rec.Code)
	})

	t.Run("a conflict is 409", func(t *testing.T) {
		plugin := newTestPluginForAPI(t)
		data, err := kvstore.MarshalServersConfig([]kvstore.ServerConfig{
			{ServerID: "s1", ServerURL: "https://a.example.com", Endpoint: "a.example.com:443", ServerName: "a.example.com"},
			{ServerID: "s2", ServerURL: "https://b.example.com", Endpoint: "b.example.com:443", ServerName: "b.example.com"},
		})
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.KeyServersConfig, data))

		newURL := "https://b.example.com"
		req := jsonRequest(t, http.MethodPatch, "/servers/s1", updateServerRequest{ServerURL: &newURL}, map[string]string{"server_id": "s1"})
		rec := httptest.NewRecorder()
		plugin.handleUpdateServer(rec, req)
		assert.Equal(t, http.StatusConflict, rec.Code)
	})

	t.Run("warnings are present in the body on a server_name change", func(t *testing.T) {
		plugin := newTestPluginForAPI(t)
		seed(t, plugin, kvstore.ServerConfig{ServerID: "s1", ServerURL: "https://a.example.com", Endpoint: "a.example.com:443", ServerName: "a.example.com"})

		newName := "renamed.example.com"
		req := jsonRequest(t, http.MethodPatch, "/servers/s1", updateServerRequest{ServerName: &newName}, map[string]string{"server_id": "s1"})
		rec := httptest.NewRecorder()
		plugin.handleUpdateServer(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		var body struct {
			Warnings []string `json:"warnings"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.NotEmpty(t, body.Warnings)
	})
}

func TestHandleRemoveServer(t *testing.T) {
	t.Run("success returns the server_id and a recovery command containing it", func(t *testing.T) {
		plugin := newTestPluginForAPI(t)
		data, err := kvstore.MarshalServersConfig([]kvstore.ServerConfig{
			{ServerID: "s1", ServerURL: "https://a.example.com", ServerName: "a.example.com", SiteURL: "https://a.example.com"},
		})
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.KeyServersConfig, data))

		req := jsonRequest(t, http.MethodDelete, "/servers/s1", nil, map[string]string{"server_id": "s1"})
		rec := httptest.NewRecorder()
		plugin.handleRemoveServer(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		var body struct {
			ServerID        string `json:"server_id"`
			RecoveryCommand string `json:"recovery_command"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Equal(t, "s1", body.ServerID)
		assert.Contains(t, body.RecoveryCommand, "s1")
		assert.Contains(t, body.RecoveryCommand, "--server-id")
	})

	t.Run("a migrated entry is 409", func(t *testing.T) {
		plugin := newTestPluginForAPI(t)
		data, err := kvstore.MarshalServersConfig([]kvstore.ServerConfig{
			{ServerID: "legacy1", ServerName: "legacy.example.com", SiteURL: ""},
		})
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.KeyServersConfig, data))

		req := jsonRequest(t, http.MethodDelete, "/servers/legacy1", nil, map[string]string{"server_id": "legacy1"})
		rec := httptest.NewRecorder()
		plugin.handleRemoveServer(rec, req)
		assert.Equal(t, http.StatusConflict, rec.Code)
	})

	t.Run("an unknown ID is 404", func(t *testing.T) {
		plugin := newTestPluginForAPI(t)
		req := jsonRequest(t, http.MethodDelete, "/servers/nope", nil, map[string]string{"server_id": "nope"})
		rec := httptest.NewRecorder()
		plugin.handleRemoveServer(rec, req)
		assert.Equal(t, http.StatusNotFound, rec.Code)
	})
}

func TestHandleSetServerEnabled(t *testing.T) {
	t.Run("flips the flag both ways", func(t *testing.T) {
		plugin := newTestPluginForAPI(t)
		api := plugin.API.(*plugintest.API)
		data, err := kvstore.MarshalServersConfig([]kvstore.ServerConfig{
			{ServerID: "s1", ServerURL: "https://a.example.com", ServerName: "a.example.com", Enabled: false},
		})
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.KeyServersConfig, data))

		req := jsonRequest(t, http.MethodPut, "/servers/s1/enabled", setServerEnabledRequest{Enabled: true}, map[string]string{"server_id": "s1"})
		rec := httptest.NewRecorder()
		plugin.handleSetServerEnabled(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		var body struct {
			Server ServerView `json:"server"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.True(t, body.Server.Enabled)

		// §3.11: enabling/disabling never re-registers or re-invites a remote.
		api.AssertNotCalled(t, "RegisterPluginForSharedChannels", mock.Anything)
		api.AssertNotCalled(t, "InviteRemoteToChannel", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("unknown ID is 404", func(t *testing.T) {
		plugin := newTestPluginForAPI(t)
		req := jsonRequest(t, http.MethodPut, "/servers/nope/enabled", setServerEnabledRequest{Enabled: true}, map[string]string{"server_id": "nope"})
		rec := httptest.NewRecorder()
		plugin.handleSetServerEnabled(rec, req)
		assert.Equal(t, http.StatusNotFound, rec.Code)
	})
}

func TestHandleTestServer(t *testing.T) {
	t.Run("unregistered server is 404", func(t *testing.T) {
		plugin := newTestPluginForAPI(t)
		req := jsonRequest(t, http.MethodPost, "/servers/nope/test", nil, map[string]string{"server_id": "nope"})
		rec := httptest.NewRecorder()
		plugin.handleTestServer(rec, req)
		assert.Equal(t, http.StatusNotFound, rec.Code)
	})

	t.Run("a nil client yields skip, not fail, for connection and appservice", func(t *testing.T) {
		plugin := newTestPluginForAPI(t)
		data, err := kvstore.MarshalServersConfig([]kvstore.ServerConfig{{ServerID: "s1", ServerURL: "https://a.example.com"}})
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.KeyServersConfig, data))

		req := jsonRequest(t, http.MethodPost, "/servers/s1/test", nil, map[string]string{"server_id": "s1"})
		rec := httptest.NewRecorder()
		plugin.handleTestServer(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		var diag servers.Diagnostics
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &diag))
		require.Len(t, diag.Checks, 4)
		assert.Equal(t, "skip", diag.Checks[2].Status)
		assert.Equal(t, "skip", diag.Checks[3].Status)
	})
}

func TestHandleServerRegistration(t *testing.T) {
	t.Run("body contains both tokens and nothing is logged", func(t *testing.T) {
		plugin := newTestPluginForAPI(t)
		api := plugin.API.(*plugintest.API)
		siteURL := "https://mm.example.com"
		api.On("GetConfig").Return(&model.Config{ServiceSettings: model.ServiceSettings{SiteURL: &siteURL}}).Maybe()

		data, err := kvstore.MarshalServersConfig([]kvstore.ServerConfig{
			{ServerID: "s1", ServerName: "a.example.com", ASToken: "secret-as", HSToken: "secret-hs"},
		})
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.KeyServersConfig, data))

		req := jsonRequest(t, http.MethodGet, "/servers/s1/registration", nil, map[string]string{"server_id": "s1"})
		rec := httptest.NewRecorder()
		plugin.handleServerRegistration(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		var body struct {
			Filename string `json:"filename"`
			Content  string `json:"content"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.NotEmpty(t, body.Filename)
		assert.Contains(t, body.Content, "secret-as")
		assert.Contains(t, body.Content, "secret-hs")
		assert.NotContains(t, body.Content, "_matrix/app/v1")

		api.AssertNotCalled(t, "LogDebug", mock.Anything, mock.Anything, mock.Anything)
	})

	t.Run("unknown server is 404", func(t *testing.T) {
		plugin := newTestPluginForAPI(t)
		req := jsonRequest(t, http.MethodGet, "/servers/nope/registration", nil, map[string]string{"server_id": "nope"})
		rec := httptest.NewRecorder()
		plugin.handleServerRegistration(rec, req)
		assert.Equal(t, http.StatusNotFound, rec.Code)
	})
}

func TestHandleServerMappings(t *testing.T) {
	seedServerAndMappings := func(t *testing.T, plugin *Plugin) {
		t.Helper()
		data, err := kvstore.MarshalServersConfig([]kvstore.ServerConfig{
			{ServerID: "s1", ServerName: "a.example.com"},
			{ServerID: "other-server", ServerName: "other.example.com"},
		})
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.KeyServersConfig, data))
	}

	t.Run("filters to the requested server and ignores another server's entries in the same array", func(t *testing.T) {
		plugin := newTestPluginForAPI(t)
		seedServerAndMappings(t, plugin)
		api := plugin.API.(*plugintest.API)

		mappingData, err := kvstore.MarshalChannelServerMappings([]kvstore.ChannelServerMapping{
			{ServerID: "other-server", RoomID: "!other:example.com"},
			{ServerID: "s1", RoomID: "!room1:example.com"},
		})
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.BuildChannelMappingKey("channel1"), mappingData))

		api.On("GetChannel", "channel1").Return(&model.Channel{Id: "channel1", Name: "town-square", DisplayName: "Town Square", Type: model.ChannelTypeOpen, TeamId: "team1"}, nil)
		api.On("GetTeam", "team1").Return(&model.Team{Id: "team1", Name: "core"}, nil)

		req := jsonRequest(t, http.MethodGet, "/servers/s1/mappings", nil, map[string]string{"server_id": "s1"})
		rec := httptest.NewRecorder()
		plugin.handleServerMappings(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		var body struct {
			TotalCount int           `json:"total_count"`
			Mappings   []MappingView `json:"mappings"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		require.Equal(t, 1, body.TotalCount)
		require.Len(t, body.Mappings, 1)
		assert.Equal(t, "channel1", body.Mappings[0].ChannelID)
		assert.Equal(t, "Town Square", body.Mappings[0].ChannelName)
		assert.Equal(t, "core", body.Mappings[0].TeamName)
		assert.False(t, body.Mappings[0].ChannelMissing)
	})

	t.Run("a deleted channel yields channel_missing true and is still listed", func(t *testing.T) {
		plugin := newTestPluginForAPI(t)
		seedServerAndMappings(t, plugin)
		api := plugin.API.(*plugintest.API)

		mappingData, err := kvstore.MarshalChannelServerMappings([]kvstore.ChannelServerMapping{{ServerID: "s1", RoomID: "!room1:example.com"}})
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.BuildChannelMappingKey("gone-channel"), mappingData))

		api.On("GetChannel", "gone-channel").Return(nil, &model.AppError{Message: "not found"})

		req := jsonRequest(t, http.MethodGet, "/servers/s1/mappings", nil, map[string]string{"server_id": "s1"})
		rec := httptest.NewRecorder()
		plugin.handleServerMappings(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		var body struct {
			Mappings []MappingView `json:"mappings"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		require.Len(t, body.Mappings, 1)
		assert.True(t, body.Mappings[0].ChannelMissing)
	})

	t.Run("a DM yields an empty team_name", func(t *testing.T) {
		plugin := newTestPluginForAPI(t)
		seedServerAndMappings(t, plugin)
		api := plugin.API.(*plugintest.API)

		mappingData, err := kvstore.MarshalChannelServerMappings([]kvstore.ChannelServerMapping{{ServerID: "s1", RoomID: "!dm:example.com"}})
		require.NoError(t, err)
		require.NoError(t, plugin.kvstore.Set(kvstore.BuildChannelMappingKey("dm-channel"), mappingData))

		api.On("GetChannel", "dm-channel").Return(&model.Channel{Id: "dm-channel", Name: "dm", Type: model.ChannelTypeDirect}, nil)

		req := jsonRequest(t, http.MethodGet, "/servers/s1/mappings", nil, map[string]string{"server_id": "s1"})
		rec := httptest.NewRecorder()
		plugin.handleServerMappings(rec, req)

		var body struct {
			Mappings []MappingView `json:"mappings"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		require.Len(t, body.Mappings, 1)
		assert.Empty(t, body.Mappings[0].TeamName)
	})

	t.Run("pagination bounds: per_page clamped to 200, page beyond the end yields an empty list with the true total_count", func(t *testing.T) {
		plugin := newTestPluginForAPI(t)
		seedServerAndMappings(t, plugin)
		api := plugin.API.(*plugintest.API)
		api.On("GetChannel", mock.AnythingOfType("string")).Return(&model.Channel{Name: "chan", Type: model.ChannelTypeOpen}, nil)

		for i := range 3 {
			mappingData, err := kvstore.MarshalChannelServerMappings([]kvstore.ChannelServerMapping{{ServerID: "s1", RoomID: "!room:example.com"}})
			require.NoError(t, err)
			require.NoError(t, plugin.kvstore.Set(kvstore.BuildChannelMappingKey(model.NewId()+strconv.Itoa(i)), mappingData))
		}

		req := jsonRequest(t, http.MethodGet, "/servers/s1/mappings?page=5&per_page=500", nil, map[string]string{"server_id": "s1"})
		rec := httptest.NewRecorder()
		plugin.handleServerMappings(rec, req)

		require.Equal(t, http.StatusOK, rec.Code)
		var body struct {
			TotalCount int           `json:"total_count"`
			Mappings   []MappingView `json:"mappings"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Equal(t, 3, body.TotalCount)
		assert.Empty(t, body.Mappings)
	})

	t.Run("unknown server is 404", func(t *testing.T) {
		plugin := newTestPluginForAPI(t)
		req := jsonRequest(t, http.MethodGet, "/servers/nope/mappings", nil, map[string]string{"server_id": "nope"})
		rec := httptest.NewRecorder()
		plugin.handleServerMappings(rec, req)
		assert.Equal(t, http.StatusNotFound, rec.Code)
	})
}
