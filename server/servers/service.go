// Package servers owns the Matrix homeserver registry: the KV-backed list of
// registered servers, its mutations, and the derived views (health, diagnostics,
// registration YAML, mapped-channel counts) that both the /matrix server slash
// commands and the System Console REST API need. It is a leaf package - it imports
// only kvstore and matrix, never main or command - so both of those packages can
// import it without creating a cycle.
package servers

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/pkg/errors"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/matrix"
	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
)

// DefaultUsernamePrefix is the default username prefix for Matrix-originated users,
// used whenever a caller of Add or Update leaves UsernamePrefix empty.
const DefaultUsernamePrefix = "matrix"

// listAllKeysBatchSize is the raw-keyspace page size used when scanning for
// namespaced records. A var (not const) so tests can shrink it to exercise
// multi-page paging without needing thousands of fixture keys.
var listAllKeysBatchSize = 1000

// statusProbeDeadline bounds how long ProbeHealth waits for all server health probes
// together. A var so tests can shorten it.
var statusProbeDeadline = 8 * time.Second

// Host is everything the service needs from the plugin runtime: per-node Matrix
// clients, shared-channels remote lifecycle, cache invalidation, and the
// site/plugin identity used to build the registration URL. Package main implements
// it with a thin adapter (servers_host.go); tests use a fake.
type Host interface {
	MatrixClient(serverID string) *matrix.Client
	RegisterRemote(serverID string) error
	UnregisterRemote(remoteID string) error
	RefreshAndBroadcast(reason string) error
	SiteURL() string
	PluginID() string
}

// Logger is declared here rather than imported so this package depends on nothing
// above it; main's Logger satisfies it structurally.
type Logger interface {
	LogDebug(message string, keyValuePairs ...any)
	LogInfo(message string, keyValuePairs ...any)
	LogWarn(message string, keyValuePairs ...any)
	LogError(message string, keyValuePairs ...any)
}

// Service owns the server registry.
type Service struct {
	kv     kvstore.KVStore
	logger Logger
	host   Host
}

// New creates a Service backed by kv, logging through logger, and reaching the
// plugin runtime through host.
func New(kv kvstore.KVStore, logger Logger, host Host) *Service {
	return &Service{kv: kv, logger: logger, host: host}
}

// AddRequest carries the input to Add.
type AddRequest struct {
	ServerURL      string
	ASToken        string
	HSToken        string
	UsernamePrefix string // "" -> DefaultUsernamePrefix
	ServerID       string // "" -> model.NewId(); non-empty re-adopts a previously removed server
	ServerName     string // "" -> discovered
}

// ChannelMapping is one channel's entry in a server's bridged-channel list.
type ChannelMapping struct {
	ChannelID string
	RoomID    string
}

// Check is one diagnostic step in Diagnostics.Checks.
type Check struct {
	Key    string // "registry" | "client" | "connection" | "appservice"
	Label  string
	Status string // "ok" | "fail" | "skip"
	Detail string // error text or supporting detail; may be empty
}

// Diagnostics is the structured result of Diagnose.
type Diagnostics struct {
	ServerID   string
	Checks     []Check
	ServerInfo *matrix.ServerInfo // nil when unavailable
}

// wrapf wraps sentinel with a formatted message, preserving the message text (so
// command output does not change) while keeping errors.Is(result, sentinel) true
// even after the error travels out of a CAS callback through
// kvstore.SetAtomicWithRetries.
func wrapf(sentinel error, format string, args ...any) error {
	return errors.Wrap(sentinel, fmt.Sprintf(format, args...))
}

// mutate atomically reads-modifies-writes the server registry via compare-and-set.
// mutator must be a pure function of the slice it is given: SetAtomicWithRetries may
// invoke it more than once if a concurrent writer wins the race, so it must not
// perform network or plugin-API calls.
func (s *Service) mutate(mutator func([]kvstore.ServerConfig) ([]kvstore.ServerConfig, error)) error {
	return s.kv.SetAtomicWithRetries(kvstore.KeyServersConfig, func(oldValue []byte) ([]byte, error) {
		current, err := kvstore.ParseServersConfig(oldValue)
		if err != nil {
			return nil, err
		}

		updated, err := mutator(current)
		if err != nil {
			return nil, err
		}

		return kvstore.MarshalServersConfig(updated)
	})
}

// List returns every registered server. A missing registry key is not an error -
// it returns (nil, nil), meaning "no servers registered yet".
func (s *Service) List() ([]kvstore.ServerConfig, error) {
	data, err := s.kv.Get(kvstore.KeyServersConfig)
	if err != nil {
		return nil, errors.Wrap(err, "failed to read servers config")
	}
	return kvstore.ParseServersConfig(data)
}

// Get returns the registry entry for serverID, or ErrNotRegistered if it is not
// registered. Reads a fresh snapshot of the registry on every call.
func (s *Service) Get(serverID string) (kvstore.ServerConfig, error) {
	servers, err := s.List()
	if err != nil {
		return kvstore.ServerConfig{}, err
	}
	for _, server := range servers {
		if server.ServerID == serverID {
			return server, nil
		}
	}
	return kvstore.ServerConfig{}, wrapf(ErrNotRegistered, "server %s is not registered", serverID)
}

// Domain returns the ServerName (Matrix ID domain) for a registered server.
func (s *Service) Domain(serverID string) (string, error) {
	server, err := s.Get(serverID)
	if err != nil {
		return "", err
	}
	return server.ServerName, nil
}

// ResolveIdentifier resolves a user-supplied server identifier. It matches, in
// order, by server_id, then by ServerName, then by URL host. An empty arg is only
// valid when exactly one server is registered.
func (s *Service) ResolveIdentifier(arg string) (string, error) {
	all, err := s.List()
	if err != nil {
		return "", errors.Wrap(err, "failed to list Matrix servers")
	}

	if arg == "" {
		switch len(all) {
		case 0:
			return "", errors.New("no Matrix servers are registered; use `/matrix server add` first")
		case 1:
			return all[0].ServerID, nil
		default:
			return "", errors.New("multiple Matrix servers are registered; specify a server_id (see `/matrix server list`)")
		}
	}

	for _, server := range all {
		if server.ServerID == arg {
			return server.ServerID, nil
		}
	}
	for _, server := range all {
		if server.ServerName == arg {
			return server.ServerID, nil
		}
	}
	for _, server := range all {
		if host, err := matrix.ExtractServerDomain(server.ServerURL); err == nil && host != "" && host == arg {
			return server.ServerID, nil
		}
	}

	return "", errors.Errorf("no registered Matrix server matches %q", arg)
}

// NormalizeEndpoint parses rawURL and returns its "host:port" form, lowercased,
// with the default port filled in from the scheme (http->80, https->443). This is
// the registry's uniqueness key: two entries may never share an endpoint.
func NormalizeEndpoint(rawURL string) (string, error) {
	if strings.TrimSpace(rawURL) == "" {
		return "", errors.New("server URL is required")
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", errors.Wrap(err, "failed to parse server URL")
	}

	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return "", errors.New("server URL must include a host")
	}

	port := parsed.Port()
	switch strings.ToLower(parsed.Scheme) {
	case "https":
		if port == "" {
			port = "443"
		}
	case "http":
		if port == "" {
			port = "80"
		}
	default:
		return "", errors.Errorf("server URL must use http:// or https:// (got scheme %q)", parsed.Scheme)
	}

	return host + ":" + port, nil
}

// eventDomainFromEndpoint derives the sanitized property-key suffix for a new
// server from its normalized endpoint (host:port), replacing '.' and ':' with '_'.
func eventDomainFromEndpoint(endpoint string) string {
	return strings.NewReplacer(".", "_", ":", "_").Replace(endpoint)
}

// ResolveServerName resolves the domain that will appear in this homeserver's
// Matrix IDs. Resolution order, first success wins:
//  1. configuredName, if non-empty (manual override, or the legacy
//     matrix_server_name when called from the v3 migration).
//  2. GET <serverURL>/_matrix/key/v2/server's server_name field (best-effort - any
//     failure falls through rather than erroring).
//  3. Hostname(serverURL), which always succeeds for a parseable URL, so this
//     function never fails to produce a non-empty name for a parseable URL.
//
// The same function is used by Add and the v3 migration so ghost recognition stays
// consistent between the two callers.
func (s *Service) ResolveServerName(serverURL, configuredName string) (string, error) {
	if configuredName != "" {
		normalized, err := matrix.NormalizeServerName(configuredName)
		if err != nil {
			return "", errors.Wrap(err, "invalid configured server name")
		}
		return normalized, nil
	}

	// Best-effort federation probe. Unauthenticated, so no AS token is needed (and
	// none may exist yet for a server that isn't registered).
	probe := matrix.NewClientWithLoggerAndRateLimit(serverURL, "", "", "", s.logger, matrix.RateLimitConfig{})
	if discovered, err := probe.GetServerNameFromKeyEndpoint(); err == nil && discovered != "" {
		if normalized, normErr := matrix.NormalizeServerName(discovered); normErr == nil && normalized != "" {
			return normalized, nil
		}
	}

	hostname, err := matrix.ExtractServerDomain(serverURL)
	if err != nil {
		return "", errors.Wrap(err, "failed to determine server name from URL")
	}

	s.logger.LogWarn("Could not discover Matrix server name via /_matrix/key/v2/server; falling back to URL hostname",
		"server_url", serverURL, "hostname", hostname)

	normalized, err := matrix.NormalizeServerName(hostname)
	if err != nil {
		return "", errors.Wrap(err, "failed to normalize hostname as server name")
	}
	return normalized, nil
}

// Add registers a new Matrix homeserver, or re-adopts a previously removed one when
// req.ServerID is non-empty (see docs on Remove for the recovery mechanism).
func (s *Service) Add(req AddRequest) (kvstore.ServerConfig, error) {
	endpoint, err := NormalizeEndpoint(req.ServerURL)
	if err != nil {
		return kvstore.ServerConfig{}, wrapf(ErrInvalidInput, "invalid server URL: %v", err)
	}

	if req.ServerID != "" && !model.IsValidId(req.ServerID) {
		return kvstore.ServerConfig{}, wrapf(ErrInvalidInput, "%q is not a valid server ID", req.ServerID)
	}

	resolvedServerName, err := s.ResolveServerName(req.ServerURL, req.ServerName)
	if err != nil {
		return kvstore.ServerConfig{}, errors.Wrap(err, "failed to resolve server name")
	}

	usernamePrefix := req.UsernamePrefix
	if usernamePrefix == "" {
		usernamePrefix = DefaultUsernamePrefix
	}

	eventDomain := eventDomainFromEndpoint(endpoint)

	var created kvstore.ServerConfig
	err = s.mutate(func(current []kvstore.ServerConfig) ([]kvstore.ServerConfig, error) {
		for _, existing := range current {
			if existing.Endpoint == endpoint {
				return nil, wrapf(ErrEndpointTaken, "a server is already registered at this endpoint (server_id: %s); use `/matrix server remove %s` first", existing.ServerID, existing.ServerID)
			}
			if existing.ServerName == resolvedServerName {
				return nil, wrapf(ErrNameTaken, "server name %q conflicts with existing server %s; two servers cannot share a Matrix ID domain", resolvedServerName, existing.ServerID)
			}
			if req.ServerID != "" && existing.ServerID == req.ServerID {
				return nil, wrapf(ErrIDTaken, "server ID %s is already registered", req.ServerID)
			}
		}

		id := req.ServerID
		if id == "" {
			id = model.NewId()
		}

		created = kvstore.ServerConfig{
			ServerID:       id,
			ServerURL:      req.ServerURL,
			Endpoint:       endpoint,
			ServerName:     resolvedServerName,
			EventDomain:    eventDomain,
			ASToken:        req.ASToken,
			HSToken:        req.HSToken,
			UsernamePrefix: usernamePrefix,
			Enabled:        true,
			SiteURL:        "https://" + endpoint,
		}

		result := make([]kvstore.ServerConfig, len(current), len(current)+1)
		copy(result, current)
		return append(result, created), nil
	})
	if err != nil {
		return kvstore.ServerConfig{}, err
	}

	if req.ServerID != "" {
		s.warnIfEventDomainMismatch(req.ServerID, eventDomain)
	}

	// Register the shared-channels remote immediately so the new server is usable
	// without waiting for a restart. Failure is non-fatal - the next activation
	// retries.
	if err := s.host.RegisterRemote(created.ServerID); err != nil {
		s.logger.LogWarn("Failed to register new server for shared channels; will retry on next activation", "server_id", created.ServerID, "error", err)
	}

	if err := s.host.RefreshAndBroadcast("server_added"); err != nil {
		s.logger.LogWarn("Failed to refresh Matrix clients after adding server", "server_id", created.ServerID, "error", err)
	}

	// Re-read so the returned view reflects RegisterRemote's RemoteID write, if any.
	if refreshed, err := s.Get(created.ServerID); err == nil {
		created = refreshed
	}

	return created, nil
}

// warnIfEventDomainMismatch logs a warning when a (re-)adopted serverID has
// surviving matrix_event_post_ records from a prior registration. There is no
// stored record of what EventDomain those records were created under (removal
// keeps no tombstone), so this cannot verify a mismatch precisely - it flags the
// one case the operator has no other way to discover: re-adding the same ID at a
// different endpoint silently orphans every pre-existing post's Matrix event ID
// property.
func (s *Service) warnIfEventDomainMismatch(serverID, newEventDomain string) {
	prefix := kvstore.KeyPrefixMatrixEventPost + serverID + "_"
	keys, err := kvstore.ListAllKeysWithPrefix(s.kv, prefix, listAllKeysBatchSize)
	if err != nil {
		s.logger.LogWarn("Failed to check for surviving event-post records during server re-adoption", "server_id", serverID, "error", err)
		return
	}
	if len(keys) > 0 {
		s.logger.LogWarn("Re-adopted server ID has surviving synced-post records from a prior registration; verify this server's EventDomain matches what was used before re-adoption, or edits/deletes of pre-existing posts will silently stop working",
			"server_id", serverID, "event_domain", newEventDomain, "surviving_record_count", len(keys))
	}
}

// Remove deletes serverID's registry entry and unregisters its shared-channels
// remote. It deletes no other KV records: every namespaced key stays exactly where
// it was, addressed by a ServerID the caller should print as the recovery key for
// re-adoption via Add.
//
// Refuses to remove an entry whose SiteURL is empty (ErrMigratedImmutable) - that
// is the migrated legacy server, which resolves to the pre-upgrade
// plugin_<PluginID> remote and cannot be re-created with the same identity.
// Disabling it is the supported way to take it out of service.
func (s *Service) Remove(serverID string) (bool, error) {
	var removed *kvstore.ServerConfig

	err := s.mutate(func(current []kvstore.ServerConfig) ([]kvstore.ServerConfig, error) {
		idx := -1
		for i, server := range current {
			if server.ServerID == serverID {
				idx = i
				break
			}
		}
		if idx == -1 {
			removed = nil
			return current, nil
		}

		if current[idx].SiteURL == "" {
			return nil, wrapf(ErrMigratedImmutable, "this server was migrated from the legacy single-server configuration and cannot be removed; use `/matrix server disable` to take it out of service")
		}

		entry := current[idx]
		removed = &entry

		result := make([]kvstore.ServerConfig, 0, len(current)-1)
		result = append(result, current[:idx]...)
		result = append(result, current[idx+1:]...)
		return result, nil
	})
	if err != nil {
		return false, err
	}

	if removed == nil {
		return false, nil
	}

	if removed.RemoteID != "" {
		if err := s.host.UnregisterRemote(removed.RemoteID); err != nil {
			s.logger.LogWarn("Failed to unregister shared-channels remote for removed server; registry write already succeeded",
				"server_id", serverID, "remote_id", removed.RemoteID, "error", err)
		}
	}

	if err := s.host.RefreshAndBroadcast("server_removed"); err != nil {
		s.logger.LogWarn("Failed to refresh Matrix clients after removing server", "server_id", serverID, "error", err)
	}

	return true, nil
}

// SetEnabled flips a server's Enabled flag and refreshes every node's caches. This
// is a pure flag flip - no re-registration, no re-invites, no cursor reset. The
// shared-channels remote stays registered and the channel invitations stay in
// place; routing alone consults Enabled.
func (s *Service) SetEnabled(serverID string, enabled bool) error {
	found := false
	err := s.mutate(func(current []kvstore.ServerConfig) ([]kvstore.ServerConfig, error) {
		updated := make([]kvstore.ServerConfig, len(current))
		copy(updated, current)
		for i := range updated {
			if updated[i].ServerID == serverID {
				updated[i].Enabled = enabled
				found = true
			}
		}
		return updated, nil
	})
	if err != nil {
		return err
	}
	if !found {
		return wrapf(ErrNotRegistered, "server %s is not registered", serverID)
	}

	return s.host.RefreshAndBroadcast("server_enabled_changed")
}

// Seed idempotently inserts entry if no existing entry shares its Endpoint,
// returning the ID that ends up registered at that endpoint (entry.ServerID on a
// fresh insert, or the existing entry's ID if it was already materialized).
// Migration-only: unlike Add, it performs no name resolution, no SiteURL
// derivation, and registers no remote - the caller (main's
// materializeServerFromLegacyConfig) composes entry itself, deliberately keeping a
// legacy SiteURL of "" and a master-derived EventDomain.
func (s *Service) Seed(entry kvstore.ServerConfig) (string, error) {
	var resultID string
	err := s.mutate(func(current []kvstore.ServerConfig) ([]kvstore.ServerConfig, error) {
		for _, existing := range current {
			if existing.Endpoint == entry.Endpoint {
				resultID = existing.ServerID
				return current, nil
			}
		}
		resultID = entry.ServerID
		result := make([]kvstore.ServerConfig, len(current), len(current)+1)
		copy(result, current)
		return append(result, entry), nil
	})
	if err != nil {
		return "", err
	}
	return resultID, nil
}

// SetRemoteID persists remoteID as serverID's shared-channels remote. Used by
// main's registerServerForSharedChannels, which discovers remoteID via a platform
// RegisterPluginForSharedChannels call this package must not make itself (that is
// Host's job) but must still be able to persist. No-ops (not an error) if serverID
// was concurrently removed.
func (s *Service) SetRemoteID(serverID, remoteID string) error {
	return s.mutate(func(current []kvstore.ServerConfig) ([]kvstore.ServerConfig, error) {
		updated := make([]kvstore.ServerConfig, len(current))
		copy(updated, current)
		for i := range updated {
			if updated[i].ServerID == serverID {
				updated[i].RemoteID = remoteID
				return updated, nil
			}
		}
		// Server was concurrently removed; nothing to persist.
		return current, nil
	})
}

// MergeRemoteIDs persists a batch of discovered remote IDs (serverID -> remoteID)
// in a single registry write, keeping every entry not present in the map
// unchanged. Used by main's bulk registerForSharedChannels, whose network calls
// must happen outside any single CAS callback.
func (s *Service) MergeRemoteIDs(remoteIDs map[string]string) error {
	if len(remoteIDs) == 0 {
		return nil
	}
	return s.mutate(func(current []kvstore.ServerConfig) ([]kvstore.ServerConfig, error) {
		updated := make([]kvstore.ServerConfig, len(current))
		copy(updated, current)
		for i := range updated {
			if remoteID, ok := remoteIDs[updated[i].ServerID]; ok {
				updated[i].RemoteID = remoteID
			}
		}
		return updated, nil
	})
}

// CountMappedChannels does a single keyspace scan shared by /matrix list, /matrix
// status, /matrix server list, /matrix server status and GET /servers.
func (s *Service) CountMappedChannels() (map[string]int, error) {
	keys, err := kvstore.ListAllKeysWithPrefix(s.kv, kvstore.KeyPrefixChannelMapping, listAllKeysBatchSize)
	if err != nil {
		return nil, err
	}

	counts := make(map[string]int)
	for _, key := range keys {
		data, err := s.kv.Get(key)
		if err != nil {
			continue
		}
		mappings, err := kvstore.ParseChannelServerMappings(data)
		if err != nil {
			continue
		}
		for _, m := range mappings {
			counts[m.ServerID]++
		}
	}
	return counts, nil
}

// Mappings returns every channel currently bridged to serverID: a single
// ListAllKeysWithPrefix scan, ParseChannelServerMappings on each value, keeping
// entries whose ServerID matches serverID. Never indexes [0] - a channel's mapping
// value is a list.
func (s *Service) Mappings(serverID string) ([]ChannelMapping, error) {
	keys, err := kvstore.ListAllKeysWithPrefix(s.kv, kvstore.KeyPrefixChannelMapping, listAllKeysBatchSize)
	if err != nil {
		return nil, err
	}

	var result []ChannelMapping
	for _, key := range keys {
		data, err := s.kv.Get(key)
		if err != nil {
			continue
		}
		mappings, err := kvstore.ParseChannelServerMappings(data)
		if err != nil {
			continue
		}
		channelID := strings.TrimPrefix(key, kvstore.KeyPrefixChannelMapping)
		for _, m := range mappings {
			if m.ServerID == serverID {
				result = append(result, ChannelMapping{ChannelID: channelID, RoomID: m.RoomID})
			}
		}
	}
	return result, nil
}

// ProbeHealth concurrently health-checks every server in list under a single
// deadline, so N servers cost roughly one probe's worth of wall-clock time rather
// than N. Servers whose probe misses the deadline render as "timed out", never as
// healthy.
func (s *Service) ProbeHealth(list []kvstore.ServerConfig) map[string]string {
	results := make(map[string]string, len(list))
	var mu sync.Mutex
	var wg sync.WaitGroup

	ctx, cancel := context.WithTimeout(context.Background(), statusProbeDeadline)
	defer cancel()

	setResult := func(serverID, status string) {
		mu.Lock()
		results[serverID] = status
		mu.Unlock()
	}

	for _, server := range list {
		if !server.Enabled {
			setResult(server.ServerID, "disabled")
			continue
		}

		matrixClient := s.host.MatrixClient(server.ServerID)
		if matrixClient == nil {
			setResult(server.ServerID, "unavailable")
			continue
		}

		wg.Add(1)
		go func(serverID string, client *matrix.Client) {
			defer wg.Done()
			done := make(chan error, 1)
			go func() { done <- client.TestConnection() }()

			select {
			case err := <-done:
				if err != nil {
					setResult(serverID, "unhealthy")
				} else {
					setResult(serverID, "healthy")
				}
			case <-ctx.Done():
				setResult(serverID, "timed out")
			}
		}(server.ServerID, matrixClient)
	}

	wg.Wait()
	return results
}

// Diagnose runs testServerConnection's checks as structured data: registry, client,
// connection, appservice, in that order. An unregistered server yields only a
// failed registry check. A nil client yields ok/fail for client, then skip for
// connection and appservice. A skipped check never renders as a pass.
func (s *Service) Diagnose(serverID string) Diagnostics {
	diag := Diagnostics{ServerID: serverID}

	server, err := s.Get(serverID)
	if err != nil {
		diag.Checks = append(diag.Checks, Check{Key: "registry", Label: "Server registry", Status: "fail", Detail: err.Error()})
		return diag
	}
	diag.Checks = append(diag.Checks, Check{Key: "registry", Label: "Server URL", Status: "ok", Detail: server.ServerURL})

	matrixClient := s.host.MatrixClient(serverID)
	if matrixClient == nil {
		diag.Checks = append(diag.Checks,
			Check{Key: "client", Label: "Matrix Client", Status: "fail", Detail: "not initialized"},
			Check{Key: "connection", Label: "Connection", Status: "skip"},
			Check{Key: "appservice", Label: "Application Service", Status: "skip"},
		)
		return diag
	}
	diag.Checks = append(diag.Checks, Check{Key: "client", Label: "Matrix Client", Status: "ok", Detail: "initialized"})

	if err := matrixClient.TestConnection(); err != nil {
		diag.Checks = append(diag.Checks,
			Check{Key: "connection", Label: "Connection", Status: "fail", Detail: err.Error()},
			Check{Key: "appservice", Label: "Application Service", Status: "skip"},
		)
		return diag
	}
	diag.Checks = append(diag.Checks, Check{Key: "connection", Label: "Connection", Status: "ok", Detail: "successfully connected to Matrix server"})

	if serverInfo, infoErr := matrixClient.GetServerInfo(); infoErr == nil && serverInfo != nil {
		diag.ServerInfo = serverInfo
	}

	if err := matrixClient.TestApplicationServicePermissions(); err != nil {
		diag.Checks = append(diag.Checks, Check{Key: "appservice", Label: "Application Service", Status: "fail", Detail: err.Error()})
	} else {
		diag.Checks = append(diag.Checks, Check{Key: "appservice", Label: "Application Service", Status: "ok", Detail: "permissions verified (can query namespace)"})
	}

	return diag
}

// RegistrationYAML renders the Application Service registration YAML for serverID.
// This is the ONLY place that renders it - a second copy of this string is a second
// chance to get the url: line wrong, which silently kills inbound sync in one
// direction only while outbound keeps working.
func (s *Service) RegistrationYAML(serverID string) (filename, content string, err error) {
	server, err := s.Get(serverID)
	if err != nil {
		return "", "", err
	}

	// The registration url is the plugin's base path ONLY. The homeserver appends
	// the appservice path itself ("/_matrix/app/v1/transactions/{txnId}"), so
	// including "/_matrix/app/v1" here produces a doubled path that matches no
	// route and silently breaks all inbound traffic for that server.
	webhookURL := strings.TrimSuffix(s.host.SiteURL(), "/") + "/plugins/" + s.host.PluginID()

	content = fmt.Sprintf(`id: mattermost-bridge-%s
url: %s
as_token: %s
hs_token: %s
sender_localpart: _mattermost_bot
rate_limited: false
namespaces:
  users:
    - exclusive: true
      regex: '@_mattermost_.*'
  aliases:
    - exclusive: false
      regex: '#mattermost-bridge-.*'
  rooms: []
`, server.ServerID, webhookURL, server.ASToken, server.HSToken)

	filename = fmt.Sprintf("mattermost-bridge-%s.yaml", server.ServerID)
	return filename, content, nil
}
