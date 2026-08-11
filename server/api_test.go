package main

import (
	"encoding/json"
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

// newTestPluginForAPI returns a Plugin with an in-memory KV store, ready for
// api.go/MatrixAuthorizationRequired tests.
func newTestPluginForAPI(t *testing.T) *Plugin {
	t.Helper()
	plugin := setupPluginForTest()
	plugin.kvstore = NewMemoryKVStore()
	return plugin
}

// seedAuthTestServers registers a fixed set of servers for the auth middleware tests:
// serverA (enabled, hs_token "hs-a"), serverB (enabled, hs_token "hs-b"), serverEmpty
// (enabled, empty hs_token - must never match anything), and serverDisabled (disabled,
// hs_token "hs-disabled").
func seedAuthTestServers(t *testing.T, plugin *Plugin) {
	t.Helper()
	servers := []kvstore.ServerConfig{
		{ServerID: "serverA", ServerName: "a.example.com", HSToken: "hs-a", Enabled: true},
		{ServerID: "serverB", ServerName: "b.example.com", HSToken: "hs-b", Enabled: true},
		{ServerID: "serverEmpty", ServerName: "empty.example.com", HSToken: "", Enabled: true},
		{ServerID: "serverDisabled", ServerName: "disabled.example.com", HSToken: "hs-disabled", Enabled: false},
	}
	data, err := kvstore.MarshalServersConfig(servers)
	require.NoError(t, err)
	require.NoError(t, plugin.kvstore.Set(kvstore.KeyServersConfig, data))
}

// finalHandler records whether it ran and captures the resolved serverID from context.
func finalHandler(ran *bool, gotServerID *string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*ran = true
		if serverID, ok := serverIDFromContext(r.Context()); ok {
			*gotServerID = serverID
		}
		w.WriteHeader(http.StatusOK)
	})
}

// TestMatrixAuthorizationRequired covers §5.1's inbound-auth requirements: token
// matches the right server, an empty HSToken never matches, an unknown token 401s, a
// disabled server's correct token 503s, and the resolved serverID reaches the handler.
func TestMatrixAuthorizationRequired(t *testing.T) {
	t.Run("token matches the right server and reaches the handler", func(t *testing.T) {
		plugin := newTestPluginForAPI(t)
		seedAuthTestServers(t, plugin)

		var ran bool
		var gotServerID string
		handler := plugin.MatrixAuthorizationRequired(finalHandler(&ran, &gotServerID))

		req := httptest.NewRequest(http.MethodPut, "/transactions/txn1", nil)
		req.Header.Set("Authorization", "Bearer hs-b")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		assert.True(t, ran)
		assert.Equal(t, "serverB", gotServerID)
		assert.Equal(t, http.StatusOK, w.Result().StatusCode)
	})

	t.Run("an empty presented token never matches an empty HSToken entry", func(t *testing.T) {
		plugin := newTestPluginForAPI(t)
		seedAuthTestServers(t, plugin)

		var ran bool
		var gotServerID string
		handler := plugin.MatrixAuthorizationRequired(finalHandler(&ran, &gotServerID))

		req := httptest.NewRequest(http.MethodPut, "/transactions/txn1", nil)
		req.Header.Set("Authorization", "Bearer ")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		assert.False(t, ran)
		assert.Equal(t, http.StatusUnauthorized, w.Result().StatusCode)
	})

	t.Run("no Authorization header at all never matches an empty HSToken entry", func(t *testing.T) {
		plugin := newTestPluginForAPI(t)
		seedAuthTestServers(t, plugin)

		var ran bool
		var gotServerID string
		handler := plugin.MatrixAuthorizationRequired(finalHandler(&ran, &gotServerID))

		req := httptest.NewRequest(http.MethodPut, "/transactions/txn1", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		assert.False(t, ran)
		assert.Equal(t, http.StatusUnauthorized, w.Result().StatusCode)
	})

	t.Run("unknown token is rejected with 401", func(t *testing.T) {
		plugin := newTestPluginForAPI(t)
		seedAuthTestServers(t, plugin)

		var ran bool
		var gotServerID string
		handler := plugin.MatrixAuthorizationRequired(finalHandler(&ran, &gotServerID))

		req := httptest.NewRequest(http.MethodPut, "/transactions/txn1", nil)
		req.Header.Set("Authorization", "Bearer nonexistent-token")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		assert.False(t, ran)
		assert.Equal(t, http.StatusUnauthorized, w.Result().StatusCode)
	})

	t.Run("a disabled server's correct token is rejected with 503", func(t *testing.T) {
		plugin := newTestPluginForAPI(t)
		seedAuthTestServers(t, plugin)

		var ran bool
		var gotServerID string
		handler := plugin.MatrixAuthorizationRequired(finalHandler(&ran, &gotServerID))

		req := httptest.NewRequest(http.MethodPut, "/transactions/txn1", nil)
		req.Header.Set("Authorization", "Bearer hs-disabled")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		assert.False(t, ran)
		assert.Equal(t, http.StatusServiceUnavailable, w.Result().StatusCode)
	})

	t.Run("registry read failure returns 500", func(t *testing.T) {
		plugin := newTestPluginForAPI(t)
		plugin.kvstore = &erroringKVStore{KVStore: NewMemoryKVStore(), errOnGetKey: kvstore.KeyServersConfig}

		var ran bool
		var gotServerID string
		handler := plugin.MatrixAuthorizationRequired(finalHandler(&ran, &gotServerID))

		req := httptest.NewRequest(http.MethodPut, "/transactions/txn1", nil)
		req.Header.Set("Authorization", "Bearer hs-a")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		assert.False(t, ran)
		assert.Equal(t, http.StatusInternalServerError, w.Result().StatusCode)
	})
}

// TestMatrixAuthorizationRequiredCacheRefresh covers cache-refresh correctness for the
// per-node server-config cache MatrixAuthorizationRequired now reads instead of the KV
// store directly: disabling or removing a server must promptly stop its hs_token from
// authorizing further requests, at the same point (initMatrixClients rebuild) that
// already stops that server's outbound routing.
func TestMatrixAuthorizationRequiredCacheRefresh(t *testing.T) {
	newAuthRequest := func(bearer string) *http.Request {
		req := httptest.NewRequest(http.MethodPut, "/transactions/txn1", nil)
		req.Header.Set("Authorization", "Bearer "+bearer)
		return req
	}

	t.Run("disabling a server stops its token from authorizing further requests", func(t *testing.T) {
		plugin := newTestPluginForAddServer(t)
		serverID, err := plugin.AddServer("https://a.example.com", "as1", "hs-a", "", "", "a.example.com")
		require.NoError(t, err)

		handler := plugin.MatrixAuthorizationRequired(finalHandler(new(bool), new(string)))

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, newAuthRequest("hs-a"))
		require.Equal(t, http.StatusOK, w.Result().StatusCode, "token must work while the server is enabled")

		require.NoError(t, plugin.SetServerEnabled(serverID, false))

		w = httptest.NewRecorder()
		handler.ServeHTTP(w, newAuthRequest("hs-a"))
		assert.Equal(t, http.StatusServiceUnavailable, w.Result().StatusCode, "disabling must promptly stop the token from being accepted")
	})

	t.Run("removing a server stops its token from authorizing further requests", func(t *testing.T) {
		plugin := newTestPluginForAddServer(t)
		_, err := plugin.AddServer("https://a.example.com", "as1", "hs-a", "", "", "a.example.com")
		require.NoError(t, err)
		serverID2, err := plugin.AddServer("https://b.example.com", "as2", "hs-b", "", "", "b.example.com")
		require.NoError(t, err)

		handler := plugin.MatrixAuthorizationRequired(finalHandler(new(bool), new(string)))

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, newAuthRequest("hs-b"))
		require.Equal(t, http.StatusOK, w.Result().StatusCode)

		removed, err := plugin.RemoveServer(serverID2)
		require.NoError(t, err)
		require.True(t, removed)

		w = httptest.NewRecorder()
		handler.ServeHTTP(w, newAuthRequest("hs-b"))
		assert.Equal(t, http.StatusUnauthorized, w.Result().StatusCode, "removal must promptly stop the token from being accepted")

		// serverA's token must be unaffected by serverB's removal.
		w = httptest.NewRecorder()
		handler.ServeHTTP(w, newAuthRequest("hs-a"))
		assert.Equal(t, http.StatusOK, w.Result().StatusCode)
	})
}

// TestHandleServerAutocomplete covers the dynamic autocomplete endpoint. The admin gate
// is the important part: MattermostAuthorizationRequired only proves the caller is logged
// in, and server IDs are admin-only information.
func TestHandleServerAutocomplete(t *testing.T) {
	userID := "userid1userid1userid1userid"

	newPlugin := func(t *testing.T, admin bool, servers []kvstore.ServerConfig) *Plugin {
		t.Helper()
		api := &plugintest.API{}
		api.On("LogError", mock.Anything, mock.Anything, mock.Anything).Maybe()
		api.On("HasPermissionTo", userID, model.PermissionManageSystem).Return(admin)
		plugin := setupPluginForTestWithLogger(t, api)
		plugin.kvstore = NewMemoryKVStore()
		plugin.serverConfigs = make(map[string]kvstore.ServerConfig, len(servers))
		for _, s := range servers {
			plugin.serverConfigs[s.ServerID] = s
		}
		return plugin
	}

	serve := func(plugin *Plugin, userID string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/autocomplete/servers", nil)
		if userID != "" {
			req.Header.Set("Mattermost-User-ID", userID)
		}
		rec := httptest.NewRecorder()
		plugin.handleServerAutocomplete(rec, req)
		return rec
	}

	t.Run("non-admin is forbidden", func(t *testing.T) {
		plugin := newPlugin(t, false, []kvstore.ServerConfig{
			{ServerID: "serverA", ServerName: "a.example.com", ServerURL: "https://a.example.com", Enabled: true},
		})
		rec := serve(plugin, userID)
		assert.Equal(t, http.StatusForbidden, rec.Code)
		assert.NotContains(t, rec.Body.String(), "serverA")
	})

	t.Run("admin gets one item per server", func(t *testing.T) {
		plugin := newPlugin(t, true, []kvstore.ServerConfig{
			{ServerID: "serverA", ServerName: "a.example.com", ServerURL: "https://a.example.com", Enabled: true},
			{ServerID: "serverB", ServerName: "b.example.com", ServerURL: "https://b.example.com", Enabled: false},
		})
		rec := serve(plugin, userID)
		require.Equal(t, http.StatusOK, rec.Code)

		var items []model.AutocompleteListItem
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &items))
		require.Len(t, items, 2)

		byItem := map[string]model.AutocompleteListItem{}
		for _, it := range items {
			byItem[it.Item] = it
		}
		assert.Equal(t, "a.example.com", byItem["serverA"].Hint)
		assert.Contains(t, byItem["serverA"].HelpText, "enabled")
		assert.Contains(t, byItem["serverB"].HelpText, "disabled")
	})

	t.Run("no servers yields an empty array, not an error", func(t *testing.T) {
		plugin := newPlugin(t, true, nil)
		rec := serve(plugin, userID)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.JSONEq(t, "[]", rec.Body.String())
	})
}
