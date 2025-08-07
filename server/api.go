// Package main implements the Mattermost Matrix Bridge plugin server component.
package main

import (
	"crypto/subtle"
	"net/http"

	"github.com/gorilla/mux"
	"github.com/mattermost/mattermost/server/public/plugin"
)

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

// MatrixAuthorizationRequired is a middleware that requires valid Matrix hs_token authentication.
func (p *Plugin) MatrixAuthorizationRequired(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		config := p.getConfiguration()

		// Check if sync is enabled
		if !config.EnableSync {
			p.logger.LogDebug("Matrix webhook received but sync is disabled")
			http.Error(w, "Sync disabled", http.StatusServiceUnavailable)
			return
		}

		// Verify hs_token in Authorization header
		authHeader := r.Header.Get("Authorization")
		expectedToken := "Bearer " + config.MatrixHSToken

		if config.MatrixHSToken == "" {
			p.logger.LogWarn("Matrix webhook received but hs_token not configured")
			http.Error(w, "Matrix not configured", http.StatusServiceUnavailable)
			return
		}

		if subtle.ConstantTimeCompare([]byte(authHeader), []byte(expectedToken)) != 1 {
			p.logger.LogWarn("Matrix webhook authentication failed - bearer token mismatch")
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// HelloWorld handles GET requests to /hello endpoint.
func (p *Plugin) HelloWorld(w http.ResponseWriter, _ *http.Request) {
	if _, err := w.Write([]byte("Hello, world!")); err != nil {
		p.logger.LogError("Failed to write response", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
