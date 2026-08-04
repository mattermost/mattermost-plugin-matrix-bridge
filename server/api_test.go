package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
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
