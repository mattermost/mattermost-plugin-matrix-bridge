package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
)

// newAuthTestPlugin builds a plugin with an in-memory KV store, a global
// EnableSync flag, and the given server registry entries.
func newAuthTestPlugin(t *testing.T, enableSync bool, servers []kvstore.ServerConfig) *Plugin {
	t.Helper()
	plugin := &Plugin{}
	plugin.logger = &testLogger{t: t}
	plugin.configuration = &configuration{EnableSync: enableSync}
	plugin.kvstore = NewMemoryKVStore()
	data, err := json.Marshal(servers)
	require.NoError(t, err)
	require.NoError(t, plugin.kvstore.Set(kvstore.KeyServersConfig, data))
	return plugin
}

// serveAuth runs a request with the given bearer token through
// MatrixAuthorizationRequired, returning the status code and the serverID the
// inner handler observed in the request context (empty when the handler did not
// run).
func serveAuth(p *Plugin, bearer string) (status int, resolvedServerID string, handlerRan bool) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerRan = true
		resolvedServerID, _ = serverIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPut, "/_matrix/app/v1/transactions/txn1", nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	p.MatrixAuthorizationRequired(inner).ServeHTTP(rec, req)
	return rec.Code, resolvedServerID, handlerRan
}

func TestMatrixAuthorizationRequired(t *testing.T) {
	twoServers := []kvstore.ServerConfig{
		{ServerID: "serverAserverAserverAserv01", ServerURL: "https://a.example.com", HSToken: "hs_token_a", Enabled: true},
		{ServerID: "serverBserverBserverBserv02", ServerURL: "https://b.example.com", HSToken: "hs_token_b", Enabled: true},
	}

	t.Run("token A resolves server A", func(t *testing.T) {
		p := newAuthTestPlugin(t, true, twoServers)
		status, serverID, ran := serveAuth(p, "hs_token_a")
		assert.Equal(t, http.StatusOK, status)
		assert.True(t, ran)
		assert.Equal(t, "serverAserverAserverAserv01", serverID)
	})

	t.Run("token B resolves server B", func(t *testing.T) {
		p := newAuthTestPlugin(t, true, twoServers)
		status, serverID, ran := serveAuth(p, "hs_token_b")
		assert.Equal(t, http.StatusOK, status)
		assert.True(t, ran)
		assert.Equal(t, "serverBserverBserverBserv02", serverID)
	})

	t.Run("unknown token is unauthorized and handler does not run", func(t *testing.T) {
		p := newAuthTestPlugin(t, true, twoServers)
		status, _, ran := serveAuth(p, "not_a_real_token")
		assert.Equal(t, http.StatusUnauthorized, status)
		assert.False(t, ran)
	})

	t.Run("global sync disabled returns 503", func(t *testing.T) {
		p := newAuthTestPlugin(t, false, twoServers)
		status, _, ran := serveAuth(p, "hs_token_a")
		assert.Equal(t, http.StatusServiceUnavailable, status)
		assert.False(t, ran)
	})

	t.Run("matched but disabled server returns 503", func(t *testing.T) {
		servers := []kvstore.ServerConfig{
			{ServerID: "serverAserverAserverAserv01", ServerURL: "https://a.example.com", HSToken: "hs_token_a", Enabled: false},
		}
		p := newAuthTestPlugin(t, true, servers)
		status, _, ran := serveAuth(p, "hs_token_a")
		assert.Equal(t, http.StatusServiceUnavailable, status)
		assert.False(t, ran)
	})

	t.Run("empty presented token never matches an empty hs_token", func(t *testing.T) {
		servers := []kvstore.ServerConfig{
			{ServerID: "serverAserverAserverAserv01", ServerURL: "https://a.example.com", HSToken: "", Enabled: true},
		}
		p := newAuthTestPlugin(t, true, servers)
		// No Authorization header at all.
		status, _, ran := serveAuth(p, "")
		assert.Equal(t, http.StatusUnauthorized, status)
		assert.False(t, ran)
	})

	t.Run("no servers configured returns 503", func(t *testing.T) {
		p := newAuthTestPlugin(t, true, nil)
		status, _, ran := serveAuth(p, "hs_token_a")
		assert.Equal(t, http.StatusServiceUnavailable, status)
		assert.False(t, ran)
	})
}
