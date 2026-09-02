// Package main implements the Mattermost Matrix Bridge plugin server component.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/gorilla/mux"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
)

// contextKey is a private type for context keys defined by this package, to avoid
// collisions with keys defined in other packages.
type contextKey string

// contextKeyServerID is the context key under which MatrixAuthorizationRequired stores
// the resolved serverID for downstream handlers.
const contextKeyServerID contextKey = "matrix_server_id"

// serverIDFromContext retrieves the serverID resolved by MatrixAuthorizationRequired.
func serverIDFromContext(ctx context.Context) (string, bool) {
	serverID, ok := ctx.Value(contextKeyServerID).(string)
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

	// System Admin only: the slash-command autocomplete list and the full server
	// registry management surface used by the System Console. Both need
	// PermissionManageSystem on top of just being logged in - server IDs, URLs and
	// token presence are admin-only information.
	adminRouter := apiRouter.NewRoute().Subrouter()
	adminRouter.Use(p.SystemAdminRequired)
	adminRouter.HandleFunc(autocompleteServersPath, p.handleServerAutocomplete).Methods(http.MethodGet)
	p.registerServersRoutes(adminRouter)

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

// SystemAdminRequired is a middleware requiring model.PermissionManageSystem, the
// same gate the /matrix slash commands themselves use. Layered on top of
// MattermostAuthorizationRequired, so a request reaching this always already has a
// Mattermost-User-ID. Never leaks server data in a 403 body.
func (p *Plugin) SystemAdminRequired(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID := r.Header.Get("Mattermost-User-ID")
		if !p.API.HasPermissionTo(userID, model.PermissionManageSystem) {
			writeJSONError(w, http.StatusForbidden, "you must be a System Admin to use this endpoint")
			return
		}

		next.ServeHTTP(w, r)
	})
}

// MatrixAuthorizationRequired is a middleware that requires a bearer token matching one
// registered server's hs_token. It compares against EVERY server without an early
// return (so the comparison cost doesn't leak which, if any, server matched via
// timing), using subtle.ConstantTimeCompare. Entries with an empty HSToken are skipped
// so an empty presented token can never match one. The matched server must also be
// enabled. On success, the resolved serverID is injected into the request context for
// downstream handlers.
//
// Reads the per-node server-config cache maintained by initMatrixClients rather than the
// KV store directly, so this genuinely hot path (every inbound Matrix webhook) does not
// pay for a KV read plus a full-registry unmarshal on every request. Disabling or
// removing a server takes effect as soon as that cache is rebuilt - the same point at
// which it already takes effect for outbound routing - so this does not change
// cache-invalidation timing. Falls back to a direct KV read only when the cache has
// never been built yet (e.g. OnConfigurationChange firing before OnActivate has run),
// matching getServers' existing error handling for that case.
func (p *Plugin) MatrixAuthorizationRequired(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		presented := []byte(authHeader)

		servers, ok := p.cachedServerConfigs()
		if !ok {
			var err error
			servers, err = p.servers.List()
			if err != nil {
				p.logger.LogError("Failed to read servers config for Matrix webhook authorization", "error", err)
				http.Error(w, "Internal error", http.StatusInternalServerError)
				return
			}
		}

		var matched *serverAuthMatch
		for _, s := range servers {
			if s.HSToken == "" {
				continue
			}

			expected := []byte("Bearer " + s.HSToken)
			if subtle.ConstantTimeCompare(presented, expected) == 1 {
				m := serverAuthMatch{serverID: s.ServerID, enabled: s.Enabled}
				matched = &m
				// Deliberately no early return: every entry is compared, so the
				// response timing does not reveal which token (if any) matched.
			}
		}

		if matched == nil {
			p.logger.LogWarn("Matrix webhook authentication failed - no server's hs_token matched")
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		if !matched.enabled {
			p.logger.LogDebug("Matrix webhook received for a disabled server", "server_id", matched.serverID)
			http.Error(w, "Server disabled", http.StatusServiceUnavailable)
			return
		}

		ctx := context.WithValue(r.Context(), contextKeyServerID, matched.serverID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// serverAuthMatch holds the outcome of matching a presented hs_token against the
// registry, deferred until after the full constant-time scan completes.
type serverAuthMatch struct {
	serverID string
	enabled  bool
}

// autocompleteServersPath is the apiRouter-relative path serving the slash-command
// autocomplete list of registered Matrix servers. The command package builds its
// FetchURL from this (see command.ServerAutocompleteURL), so the route and the URL
// advertised to the webapp cannot drift apart.
const autocompleteServersPath = "/autocomplete/servers"

// handleServerAutocomplete serves the dynamic autocomplete list for arguments that take a
// server_id, so admins can pick a server instead of copying an opaque 26-character ID.
// Registered on adminRouter (SystemAdminRequired) - the permission gate lives there,
// in exactly one place, rather than duplicated in this handler.
//
// An empty list is returned (rather than an error) whenever there is nothing to suggest,
// including when the client cache has not been built yet. Autocomplete then shows no
// suggestions, which degrades better than surfacing an error while someone is typing.
func (p *Plugin) handleServerAutocomplete(w http.ResponseWriter, _ *http.Request) {
	servers, _ := p.cachedServerConfigs()

	items := make([]model.AutocompleteListItem, 0, len(servers))
	for _, s := range servers {
		state := "disabled"
		if s.Enabled {
			state = "enabled"
		}
		items = append(items, model.AutocompleteListItem{
			Item:     s.ServerID,
			Hint:     s.ServerName,
			HelpText: fmt.Sprintf("%s (%s)", s.ServerURL, state),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(items); err != nil {
		p.logger.LogError("Failed to encode server autocomplete list", "error", err)
	}
}

// HelloWorld handles GET requests to /hello endpoint.
func (p *Plugin) HelloWorld(w http.ResponseWriter, _ *http.Request) {
	if _, err := w.Write([]byte("Hello, world!")); err != nil {
		p.logger.LogError("Failed to write response", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
