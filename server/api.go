// Package main implements the Mattermost Matrix Bridge plugin server component.
package main

import (
	"context"
	"crypto/subtle"
	"net/http"

	"github.com/gorilla/mux"
	"github.com/mattermost/mattermost/server/public/plugin"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
)

// contextKey is an unexported type for keys stored in a request context, avoiding
// collisions with keys defined in other packages.
type contextKey string

// matrixServerIDContextKey carries the serverID resolved by the Matrix auth
// middleware (from the presented hs_token) to the downstream webhook handler.
const matrixServerIDContextKey contextKey = "matrix_server_id"

// serverIDFromContext returns the serverID resolved by MatrixAuthorizationRequired
// for the current request. The bool is false if no serverID was injected.
func serverIDFromContext(ctx context.Context) (string, bool) {
	serverID, ok := ctx.Value(matrixServerIDContextKey).(string)
	return serverID, ok
}

// ServeHTTP demonstrates a plugin that handles HTTP requests by greeting the world.
// The root URL is currently <siteUrl>/plugins/com.mattermost.plugin-starter-template/api/v1/. Replace com.mattermost.plugin-starter-template with the plugin ID.
func (p *Plugin) ServeHTTP(_ *plugin.Context, w http.ResponseWriter, r *http.Request) {
	router := mux.NewRouter()

	// Matrix Application Service webhook endpoint with Matrix authentication
	matrixRouter := router.PathPrefix("/_matrix/app/v1").Subrouter()
	matrixRouter.Use(p.MatrixAuthorizationRequired)
	matrixRouter.HandleFunc("/transactions/{txnId}", p.handleMatrixTransaction).Methods(http.MethodPut)

	// Authenticated Mattermost API routes
	apiRouter := router.PathPrefix("/api/v1").Subrouter()
	apiRouter.Use(p.MattermostAuthorizationRequired)
	apiRouter.HandleFunc("/hello", p.HelloWorld).Methods(http.MethodGet)

	router.ServeHTTP(w, r)
}

// MattermostAuthorizationRequired is a middleware that requires users to be logged in.
func (p *Plugin) MattermostAuthorizationRequired(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID := r.Header.Get("Mattermost-User-ID")
		if userID == "" {
			http.Error(w, "Not authorized", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// MatrixAuthorizationRequired is a middleware that requires valid Matrix hs_token
// authentication. Each managed homeserver has its own hs_token, so the presented
// bearer token is matched against every server's HSToken to resolve which server
// the transaction originated from. The resolved serverID is injected into the
// request context for the downstream handler.
func (p *Plugin) MatrixAuthorizationRequired(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Master switch: reject all inbound traffic when sync is globally disabled.
		if !p.getConfiguration().EnableSync {
			p.logger.LogDebug("Matrix webhook received but sync is disabled")
			http.Error(w, "Sync disabled", http.StatusServiceUnavailable)
			return
		}

		servers, err := p.getServers()
		if err != nil {
			p.logger.LogError("Matrix webhook received but server registry could not be read", "error", err)
			http.Error(w, "Matrix not configured", http.StatusServiceUnavailable)
			return
		}
		if len(servers) == 0 {
			p.logger.LogWarn("Matrix webhook received but no Matrix server is configured")
			http.Error(w, "Matrix not configured", http.StatusServiceUnavailable)
			return
		}

		// Match the presented token against every server's hs_token. Scan the full
		// list (no early return) so the work is independent of which server matches;
		// the registry holds only a handful of servers so this stays bounded.
		authHeader := r.Header.Get("Authorization")
		matched := kvstore.ServerConfig{}
		found := false
		for _, server := range servers {
			if server.HSToken == "" {
				// A server without an hs_token can never authenticate a webhook;
				// skip it so an empty presented token never matches an empty config.
				continue
			}
			expectedToken := "Bearer " + server.HSToken
			if subtle.ConstantTimeCompare([]byte(authHeader), []byte(expectedToken)) == 1 {
				matched = server
				found = true
			}
		}

		if !found {
			p.logger.LogWarn("Matrix webhook authentication failed - bearer token did not match any configured server")
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		// Per-server switch: a matched but disabled server must not accept traffic.
		if !matched.Enabled {
			p.logger.LogDebug("Matrix webhook received for a disabled server", "server_id", matched.ServerID)
			http.Error(w, "Sync disabled", http.StatusServiceUnavailable)
			return
		}

		ctx := context.WithValue(r.Context(), matrixServerIDContextKey, matched.ServerID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// HelloWorld handles GET requests to /hello endpoint.
func (p *Plugin) HelloWorld(w http.ResponseWriter, _ *http.Request) {
	if _, err := w.Write([]byte("Hello, world!")); err != nil {
		p.logger.LogError("Failed to write response", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
