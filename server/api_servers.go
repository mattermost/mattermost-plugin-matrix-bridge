package main

import (
	"encoding/json"
	"net/http"

	"github.com/gorilla/mux"
	"github.com/pkg/errors"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/servers"
	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
)

// registerServersRoutes hangs the System Console's server-registry REST API off
// router, which the caller (ServeHTTP) has already gated with SystemAdminRequired.
// One handler per row in the design doc's endpoint table (§3.5); each below notes
// which row it implements.
func (p *Plugin) registerServersRoutes(router *mux.Router) {
	router.HandleFunc("/servers", p.handleListServers).Methods(http.MethodGet)
	router.HandleFunc("/servers/health", p.handleServersHealth).Methods(http.MethodGet)
}

// apiErrorBody is the one error shape every handler in this file uses:
// {"message": "..."}. Registry error messages are already written for humans (they
// name the conflicting server_id, point at the recovery command), so the caller
// renders them verbatim rather than substituting its own wording.
type apiErrorBody struct {
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, apiErrorBody{Message: message})
}

// statusForServersError maps a servers/kvstore sentinel to the HTTP status §3.4
// assigns it. Anything unrecognized is a 500.
func statusForServersError(err error) int {
	switch {
	case errors.Is(err, servers.ErrNotRegistered):
		return http.StatusNotFound
	case errors.Is(err, servers.ErrEndpointTaken), errors.Is(err, servers.ErrNameTaken), errors.Is(err, servers.ErrIDTaken):
		return http.StatusConflict
	case errors.Is(err, servers.ErrMigratedImmutable):
		return http.StatusConflict
	case errors.Is(err, servers.ErrInvalidInput):
		return http.StatusBadRequest
	case errors.Is(err, kvstore.ErrChannelAlreadyMapped):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

// writeServersError maps err to its §3.4 status and writes the error body. A 500 is
// logged with the real error and returns a generic message; every other status
// returns the registry's own message verbatim, since it is already written for a
// human and never contains a token.
func (p *Plugin) writeServersError(w http.ResponseWriter, action string, err error) {
	status := statusForServersError(err)
	if status == http.StatusInternalServerError {
		p.logger.LogError("Matrix server management API error", "action", action, "error", err)
		writeJSONError(w, status, "internal server error")
		return
	}
	writeJSONError(w, status, err.Error())
}

// ServerView is the REST representation of a registered server. It never contains
// a token - has_as_token/has_hs_token let the edit form show "configured"
// placeholders without leaking values.
type ServerView struct {
	ServerID           string `json:"server_id"`
	ServerURL          string `json:"server_url"`
	ServerName         string `json:"server_name"`
	Endpoint           string `json:"endpoint"`
	EventDomain        string `json:"event_domain"`
	UsernamePrefix     string `json:"username_prefix"`
	Enabled            bool   `json:"enabled"`
	RemoteID           string `json:"remote_id"`
	IsMigrated         bool   `json:"is_migrated"`
	HasASToken         bool   `json:"has_as_token"`
	HasHSToken         bool   `json:"has_hs_token"`
	MappedChannelCount *int   `json:"mapped_channel_count"`
}

// newServerView builds a ServerView from a registry entry. mappedChannelCount is
// nil when the keyspace scan that produces it failed - the UI must render
// "unavailable", never 0, since 0 reads as "nothing is bridged" and invites an
// admin to remove a live server.
func newServerView(s kvstore.ServerConfig, mappedChannelCount *int) ServerView {
	return ServerView{
		ServerID:           s.ServerID,
		ServerURL:          s.ServerURL,
		ServerName:         s.ServerName,
		Endpoint:           s.Endpoint,
		EventDomain:        s.EventDomain,
		UsernamePrefix:     s.UsernamePrefix,
		Enabled:            s.Enabled,
		RemoteID:           s.RemoteID,
		IsMigrated:         s.SiteURL == "",
		HasASToken:         s.ASToken != "",
		HasHSToken:         s.HSToken != "",
		MappedChannelCount: mappedChannelCount,
	}
}

// listServersResponse is GET /servers's body.
type listServersResponse struct {
	Servers           []ServerView `json:"servers"`
	CountsUnavailable bool         `json:"counts_unavailable,omitempty"`
}

// handleListServers implements `GET /servers`. Reads a fresh KV snapshot via
// Servers().List - never the per-node cachedServerConfigs, which exists for the
// inbound hot path and would show an admin who just added a server a stale list.
func (p *Plugin) handleListServers(w http.ResponseWriter, _ *http.Request) {
	list, err := p.servers.List()
	if err != nil {
		p.writeServersError(w, "list servers", err)
		return
	}

	counts, countsErr := p.servers.CountMappedChannels()

	resp := listServersResponse{Servers: make([]ServerView, 0, len(list))}
	if countsErr != nil {
		resp.CountsUnavailable = true
	}
	for _, s := range list {
		var countPtr *int
		if countsErr == nil {
			count := counts[s.ServerID]
			countPtr = &count
		}
		resp.Servers = append(resp.Servers, newServerView(s, countPtr))
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleServersHealth implements `GET /servers/health`, split from the list
// endpoint because probing costs up to the health-probe deadline (~8s) and would
// otherwise make the table unusable on first paint. It probes only enabled servers
// with a live client, exactly as /matrix status does, and reports "timed out"
// rather than healthy for a probe that misses the deadline.
func (p *Plugin) handleServersHealth(w http.ResponseWriter, _ *http.Request) {
	list, err := p.servers.List()
	if err != nil {
		p.writeServersError(w, "servers health", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]map[string]string{"health": p.servers.ProbeHealth(list)})
}
