package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"

	"github.com/gorilla/mux"
	"github.com/pkg/errors"

	"github.com/mattermost/mattermost/server/public/model"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/servers"
	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
)

// registerServersRoutes hangs the System Console's server-registry REST API off
// router, which the caller (ServeHTTP) has already gated with SystemAdminRequired.
// One handler per row in the design doc's endpoint table (§3.5); each below notes
// which row it implements.
func (p *Plugin) registerServersRoutes(router *mux.Router) {
	router.HandleFunc("/servers", p.handleListServers).Methods(http.MethodGet)
	router.HandleFunc("/servers", p.handleAddServer).Methods(http.MethodPost)
	router.HandleFunc("/servers/health", p.handleServersHealth).Methods(http.MethodGet)
	router.HandleFunc("/servers/{server_id}", p.handleUpdateServer).Methods(http.MethodPatch)
	router.HandleFunc("/servers/{server_id}", p.handleRemoveServer).Methods(http.MethodDelete)
	router.HandleFunc("/servers/{server_id}/enabled", p.handleSetServerEnabled).Methods(http.MethodPut)
	router.HandleFunc("/servers/{server_id}/test", p.handleTestServer).Methods(http.MethodPost)
	router.HandleFunc("/servers/{server_id}/registration", p.handleServerRegistration).Methods(http.MethodGet)
	router.HandleFunc("/servers/{server_id}/mappings", p.handleServerMappings).Methods(http.MethodGet)
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

// serverViewWithCount builds a ServerView carrying s's current mapped-channel
// count, for the mutating handlers' response bodies. A fresh scan on every
// mutation is an acceptable cost here - unlike GET /servers, this never runs on a
// list render or any other hot/repeated path.
func (p *Plugin) serverViewWithCount(s kvstore.ServerConfig) ServerView {
	counts, err := p.servers.CountMappedChannels()
	if err != nil {
		return newServerView(s, nil)
	}
	count := counts[s.ServerID]
	return newServerView(s, &count)
}

// actingUserID reads the caller's Mattermost user ID for the structured log line
// every mutating handler below writes, naming the action, the server_id and the
// acting user - and never a token value.
func actingUserID(r *http.Request) string {
	return r.Header.Get("Mattermost-User-ID")
}

// addServerRequest is POST /servers's body.
type addServerRequest struct {
	ServerURL      string `json:"server_url"`
	ASToken        string `json:"as_token"`
	HSToken        string `json:"hs_token"`
	UsernamePrefix string `json:"username_prefix,omitempty"`
	ServerID       string `json:"server_id,omitempty"`   // re-adopt a previously removed server
	ServerName     string `json:"server_name,omitempty"` // override discovery
}

// handleAddServer implements `POST /servers`.
func (p *Plugin) handleAddServer(w http.ResponseWriter, r *http.Request) {
	var req addServerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "malformed request body: "+err.Error())
		return
	}

	created, err := p.servers.Add(servers.AddRequest{
		ServerURL:      req.ServerURL,
		ASToken:        req.ASToken,
		HSToken:        req.HSToken,
		UsernamePrefix: req.UsernamePrefix,
		ServerID:       req.ServerID,
		ServerName:     req.ServerName,
	})
	if err != nil {
		p.writeServersError(w, "add server", err)
		return
	}

	p.logger.LogInfo("Matrix server added via System Console", "server_id", created.ServerID, "user_id", actingUserID(r))

	// A just-created server cannot yet have any channel mapped to it - no scan
	// needed to know the count is 0.
	zero := 0
	writeJSON(w, http.StatusCreated, map[string]any{
		"server":   newServerView(created, &zero),
		"warnings": []string{},
	})
}

// updateServerRequest is PATCH /servers/{server_id}'s body. A nil field is
// "unset" in JSON terms (the key absent, or present as null) - a blank token
// input must be omitted by the client rather than sent as "", which the registry
// rejects (see servers.Update's ASToken/HSToken notes).
type updateServerRequest struct {
	ServerURL      *string `json:"server_url,omitempty"`
	ASToken        *string `json:"as_token,omitempty"`
	HSToken        *string `json:"hs_token,omitempty"`
	UsernamePrefix *string `json:"username_prefix,omitempty"`
	ServerName     *string `json:"server_name,omitempty"`
}

// handleUpdateServer implements `PATCH /servers/{server_id}`.
func (p *Plugin) handleUpdateServer(w http.ResponseWriter, r *http.Request) {
	serverID := mux.Vars(r)["server_id"]

	var req updateServerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "malformed request body: "+err.Error())
		return
	}

	updated, warnings, err := p.servers.Update(serverID, servers.Update{
		ServerURL:      req.ServerURL,
		ASToken:        req.ASToken,
		HSToken:        req.HSToken,
		UsernamePrefix: req.UsernamePrefix,
		ServerName:     req.ServerName,
	})
	if err != nil {
		p.writeServersError(w, "update server", err)
		return
	}

	p.logger.LogInfo("Matrix server updated via System Console", "server_id", serverID, "user_id", actingUserID(r))

	if warnings == nil {
		warnings = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"server":   p.serverViewWithCount(updated),
		"warnings": warnings,
	})
}

// handleRemoveServer implements `DELETE /servers/{server_id}`. Service.Remove
// keeps every namespaced KV record - the recovery_command is the cheap path back,
// so it always names this exact server_id.
func (p *Plugin) handleRemoveServer(w http.ResponseWriter, r *http.Request) {
	serverID := mux.Vars(r)["server_id"]

	removed, err := p.servers.Remove(serverID)
	if err != nil {
		p.writeServersError(w, "remove server", err)
		return
	}
	if !removed {
		writeJSONError(w, http.StatusNotFound, fmt.Sprintf("no server found with ID %q", serverID))
		return
	}

	p.logger.LogInfo("Matrix server removed via System Console", "server_id", serverID, "user_id", actingUserID(r))

	writeJSON(w, http.StatusOK, map[string]any{
		"server_id":        serverID,
		"recovery_command": fmt.Sprintf("/matrix server add <server_url> <as_token> <hs_token> --server-id %s", serverID),
	})
}

// setServerEnabledRequest is PUT /servers/{server_id}/enabled's body.
type setServerEnabledRequest struct {
	Enabled bool `json:"enabled"`
}

// handleSetServerEnabled implements `PUT /servers/{server_id}/enabled`. Applies
// immediately, like every other mutation here - Service.SetEnabled is a pure flag
// flip that never re-registers or re-invites (backend §3.11).
func (p *Plugin) handleSetServerEnabled(w http.ResponseWriter, r *http.Request) {
	serverID := mux.Vars(r)["server_id"]

	var req setServerEnabledRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "malformed request body: "+err.Error())
		return
	}

	if err := p.servers.SetEnabled(serverID, req.Enabled); err != nil {
		p.writeServersError(w, "set server enabled", err)
		return
	}

	p.logger.LogInfo("Matrix server enabled state changed via System Console", "server_id", serverID, "enabled", req.Enabled, "user_id", actingUserID(r))

	updated, err := p.servers.Get(serverID)
	if err != nil {
		p.writeServersError(w, "set server enabled", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"server": p.serverViewWithCount(updated)})
}

// handleTestServer implements `POST /servers/{server_id}/test`. A POST, not a
// GET: it performs real network calls including the Application Service
// permission probe, and must not be cached by any intermediary. An unregistered
// server's single failed registry check is surfaced as a 404, matching every
// other endpoint's not-registered semantics, rather than as a 200 carrying a
// failed check.
func (p *Plugin) handleTestServer(w http.ResponseWriter, r *http.Request) {
	serverID := mux.Vars(r)["server_id"]

	diag := p.servers.Diagnose(serverID)
	if len(diag.Checks) == 1 && diag.Checks[0].Key == "registry" && diag.Checks[0].Status == "fail" {
		writeJSONError(w, http.StatusNotFound, diag.Checks[0].Detail)
		return
	}

	writeJSON(w, http.StatusOK, diag)
}

// handleServerRegistration implements `GET /servers/{server_id}/registration`.
// The response carries both tokens - by design, that is the point - so nothing
// from this handler is ever logged, not even at debug level.
func (p *Plugin) handleServerRegistration(w http.ResponseWriter, r *http.Request) {
	serverID := mux.Vars(r)["server_id"]

	filename, content, err := p.servers.RegistrationYAML(serverID)
	if err != nil {
		p.writeServersError(w, "get server registration", err)
		return
	}

	// Must be set before writeJSON calls WriteHeader - the response carries both
	// AS/HS tokens and must never be cached by a browser or intermediary.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"filename": filename, "content": content})
}

// MappingView is one row of GET /servers/{server_id}/mappings.
type MappingView struct {
	ChannelID      string `json:"channel_id"`
	ChannelName    string `json:"channel_name"`
	TeamName       string `json:"team_name"` // "" for a DM/GM - the UI labels that "Direct message"
	RoomID         string `json:"room_id"`
	ChannelMissing bool   `json:"channel_missing"`
}

const (
	defaultMappingsPerPage = 50
	maxMappingsPerPage     = 200
)

// paginationParams reads page/per_page query parameters, defaulting page to 0 and
// per_page to defaultMappingsPerPage, clamped to [1, maxMappingsPerPage].
func paginationParams(r *http.Request) (page, perPage int) {
	page = 0
	perPage = defaultMappingsPerPage

	if v := r.URL.Query().Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			page = n
		}
	}
	if v := r.URL.Query().Get("per_page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			perPage = n
		}
	}
	if perPage > maxMappingsPerPage {
		perPage = maxMappingsPerPage
	}

	return page, perPage
}

// handleServerMappings implements `GET /servers/{server_id}/mappings`. The KV
// half (which channels are mapped to serverID) is Servers().Mappings; this
// handler decorates each with channel/team display names through the plugin API
// (memoizing team lookups, since many channels typically share a team), sorts by
// team then channel name, and paginates in memory - deliberately not pushed into
// the servers package, which must stay platform-free.
//
// This is a full-keyspace scan (via Servers().Mappings) on every call - the
// webapp must only call this when an admin opens a server's mappings panel,
// never as part of the server list render.
func (p *Plugin) handleServerMappings(w http.ResponseWriter, r *http.Request) {
	serverID := mux.Vars(r)["server_id"]

	if _, err := p.servers.Get(serverID); err != nil {
		p.writeServersError(w, "get server mappings", err)
		return
	}

	mappings, err := p.servers.Mappings(serverID)
	if err != nil {
		p.writeServersError(w, "get server mappings", err)
		return
	}

	teamNames := make(map[string]string)
	views := make([]MappingView, 0, len(mappings))
	for _, m := range mappings {
		view := MappingView{ChannelID: m.ChannelID, RoomID: m.RoomID}

		channel, appErr := p.API.GetChannel(m.ChannelID)
		if appErr != nil {
			view.ChannelMissing = true
			views = append(views, view)
			continue
		}

		view.ChannelName = channel.DisplayName
		if view.ChannelName == "" {
			view.ChannelName = channel.Name
		}

		if channel.Type != model.ChannelTypeDirect && channel.Type != model.ChannelTypeGroup && channel.TeamId != "" {
			if name, ok := teamNames[channel.TeamId]; ok {
				view.TeamName = name
			} else if team, teamErr := p.API.GetTeam(channel.TeamId); teamErr == nil && team != nil {
				teamNames[channel.TeamId] = team.Name
				view.TeamName = team.Name
			}
		}

		views = append(views, view)
	}

	sort.Slice(views, func(i, j int) bool {
		if views[i].TeamName != views[j].TeamName {
			return views[i].TeamName < views[j].TeamName
		}
		return views[i].ChannelName < views[j].ChannelName
	})

	totalCount := len(views)
	page, perPage := paginationParams(r)
	start := min(page*perPage, totalCount)
	end := min(start+perPage, totalCount)

	writeJSON(w, http.StatusOK, map[string]any{
		"total_count": totalCount,
		"mappings":    views[start:end],
	})
}
