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

// statusProbeDeadline bounds how long ProbeHealth waits for all server health probes
// together. A var so tests can shorten it.
var statusProbeDeadline = 8 * time.Second

// Host is everything the service needs from the plugin runtime: per-node Matrix
// clients, shared-channels remote lifecycle, cache invalidation, and the
// site/plugin identity used to build the registration URL. Package main implements
// it with a thin adapter (servers_host.go); tests use a fake.
type Host interface {
	MatrixClient(serverID string) *matrix.Client
	RegisterRemoteForSiteURL(siteURL string) (remoteID string, err error)
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
	Key    string `json:"key"` // "registry" | "client" | "connection" | "appservice"
	Label  string `json:"label"`
	Status string `json:"status"` // "ok" | "fail" | "skip"
	Detail string `json:"detail"` // error text or supporting detail; may be empty
}

// Diagnostics is the structured result of Diagnose.
type Diagnostics struct {
	ServerID   string             `json:"server_id"`
	Checks     []Check            `json:"checks"`
	ServerInfo *matrix.ServerInfo `json:"server_info"` // nil when unavailable
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
//  1. configuredName, if non-empty (the --server-name override).
//  2. GET <serverURL>/_matrix/key/v2/server's server_name field (best-effort - any
//     failure falls through rather than erroring).
//  3. Hostname(serverURL), which always succeeds for a parseable URL, so this
//     function never fails to produce a non-empty name for a parseable URL.
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
	siteURL := "https://" + endpoint

	checkConflicts := func(current []kvstore.ServerConfig) error {
		for _, existing := range current {
			if existing.Endpoint == endpoint {
				return wrapf(ErrEndpointTaken, "a server is already registered at this endpoint (server_id: %s); use `/matrix server remove %s` first", existing.ServerID, existing.ServerID)
			}
			if existing.ServerName == resolvedServerName {
				return wrapf(ErrNameTaken, "server name %q conflicts with existing server %s; two servers cannot share a Matrix ID domain", resolvedServerName, existing.ServerID)
			}
			if req.ServerID != "" && existing.ServerID == req.ServerID {
				return wrapf(ErrIDTaken, "server ID %s is already registered", req.ServerID)
			}
			if req.HSToken != "" && existing.HSToken == req.HSToken {
				return wrapf(ErrHSTokenTaken, "hs_token conflicts with existing server %s; hs_token must be unique across registered servers", existing.ServerID)
			}
		}
		return nil
	}

	// Conflicts are checked here as well as inside the mutate callback below.
	// RegisterRemoteForSiteURL is idempotent per SiteURL, so re-adding an endpoint that is
	// already registered hands back the *existing* server's RemoteID; without this
	// pre-check the rollback would unregister that live remote, taking down the existing
	// server's channel invitations and sync cursors.
	//
	// This narrows the window rather than closing it: two concurrent adds for the same
	// endpoint can both clear the pre-check, both receive the same idempotent RemoteID,
	// and the CAS loser's rollback then unregisters the winner's live remote. Closing
	// that needs the registry entry reserved by CAS before the remote is created.
	existing, err := s.List()
	if err != nil {
		return kvstore.ServerConfig{}, err
	}
	if err := checkConflicts(existing); err != nil {
		return kvstore.ServerConfig{}, err
	}

	// Register the shared-channels remote BEFORE the registry write, so its ID can go
	// into the entry itself. Registering afterwards would persist a server with an empty
	// RemoteID whenever the call failed - a registered server that silently syncs
	// nothing until the next activation happens to retry it.
	remoteID, err := s.host.RegisterRemoteForSiteURL(siteURL)
	if err != nil {
		return kvstore.ServerConfig{}, errors.Wrap(err, "failed to register server for shared channels")
	}

	var created kvstore.ServerConfig
	err = s.mutate(func(current []kvstore.ServerConfig) ([]kvstore.ServerConfig, error) {
		// The authoritative check: only this callback sees the slice the write lands on.
		if err := checkConflicts(current); err != nil {
			return nil, err
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
			SiteURL:        siteURL,
			RemoteID:       remoteID,
		}

		result := make([]kvstore.ServerConfig, len(current), len(current)+1)
		copy(result, current)
		return append(result, created), nil
	})
	if err != nil {
		// The remote was created but belongs to no entry, so it must not be left
		// registered. Reaching here means a concurrent writer won the race after the
		// pre-check above passed, or the CAS itself failed.
		if unregErr := s.host.UnregisterRemote(remoteID); unregErr != nil {
			s.logger.LogWarn("Failed to unregister shared-channels remote after a rejected add", "remote_id", remoteID, "error", unregErr)
		}
		return kvstore.ServerConfig{}, err
	}

	if req.ServerID != "" {
		s.warnIfEventDomainMismatch(req.ServerID, eventDomain)
	}

	if err := s.host.RefreshAndBroadcast("server_added"); err != nil {
		s.logger.LogWarn("Failed to refresh Matrix clients after adding server", "server_id", created.ServerID, "error", err)
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
	keys, err := kvstore.ListAllKeysWithPrefix(s.kv, prefix, kvstore.DefaultListKeysBatchSize)
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
// Reports "not registered" from inside the mutator rather than from a captured flag:
// SetAtomicWithRetries may run the callback several times, so a flag set by a losing
// attempt would decide the outcome of the winning one. Returning the error also means a
// missing server writes nothing at all, instead of persisting the unchanged slice.
func (s *Service) SetEnabled(serverID string, enabled bool) error {
	err := s.mutate(func(current []kvstore.ServerConfig) ([]kvstore.ServerConfig, error) {
		idx := -1
		for i := range current {
			if current[i].ServerID == serverID {
				idx = i
				break
			}
		}
		if idx == -1 {
			return nil, wrapf(ErrNotRegistered, "server %s is not registered", serverID)
		}

		updated := make([]kvstore.ServerConfig, len(current))
		copy(updated, current)
		updated[idx].Enabled = enabled
		return updated, nil
	})
	if err != nil {
		return err
	}

	return s.host.RefreshAndBroadcast("server_enabled_changed")
}

// Update carries a partial update. A nil field means "leave alone" - an absent
// field and an empty string are NOT the same thing, which is what lets the edit
// form send only what the admin actually changed. ServerID, EventDomain, SiteURL
// and RemoteID are never editable through Update - see the field-by-field notes on
// each setter below.
type Update struct {
	ServerURL      *string
	ASToken        *string
	HSToken        *string
	UsernamePrefix *string // "" -> DefaultUsernamePrefix, matching Add
	ServerName     *string // "" -> re-discovered, matching Add
}

// Update applies a partial edit to serverID and returns the updated entry plus any
// human-readable warnings for changes that succeeded but have consequences the
// admin must know about. Validation and name resolution happen before the CAS
// mutator runs (ResolveServerName performs a network probe, and the callback must
// stay pure); uniqueness checks run inside it, against the live slice and
// excluding the entry being edited, so two concurrent edits cannot both win.
//
// EventDomain, SiteURL and RemoteID are never touched, even when ServerURL
// changes: recomputing EventDomain would orphan the matrix_event_id_<domain>
// property on every already-synced post, and re-deriving SiteURL would make the
// platform hand back a different shared-channels remote than the one already
// registered. Update does not re-register or unregister that remote - SiteURL is
// unchanged, so there is nothing to re-key.
func (s *Service) Update(serverID string, u Update) (kvstore.ServerConfig, []string, error) {
	current, err := s.Get(serverID)
	if err != nil {
		return kvstore.ServerConfig{}, nil, err
	}

	newServerURL := current.ServerURL
	newEndpoint := current.Endpoint
	endpointChanged := false
	if u.ServerURL != nil {
		endpoint, normErr := NormalizeEndpoint(*u.ServerURL)
		if normErr != nil {
			return kvstore.ServerConfig{}, nil, wrapf(ErrInvalidInput, "invalid server URL: %v", normErr)
		}
		newServerURL = *u.ServerURL
		if endpoint != current.Endpoint {
			newEndpoint = endpoint
			endpointChanged = true
		}
	}

	if u.ASToken != nil && *u.ASToken == "" {
		return kvstore.ServerConfig{}, nil, wrapf(ErrInvalidInput, "as_token cannot be empty")
	}
	if u.HSToken != nil && *u.HSToken == "" {
		return kvstore.ServerConfig{}, nil, wrapf(ErrInvalidInput, "hs_token cannot be empty")
	}
	hsTokenChanged := u.HSToken != nil && *u.HSToken != current.HSToken

	newUsernamePrefix := current.UsernamePrefix
	usernamePrefixChanged := false
	if u.UsernamePrefix != nil {
		resolved := *u.UsernamePrefix
		if resolved == "" {
			resolved = DefaultUsernamePrefix
		}
		if resolved != current.UsernamePrefix {
			newUsernamePrefix = resolved
			usernamePrefixChanged = true
		}
	}

	// Resolved through ResolveServerName exactly like Add, against the (possibly
	// just-updated) URL, so normalization matches add-time behaviour.
	newServerName := current.ServerName
	serverNameChanged := false
	if u.ServerName != nil {
		resolved, resolveErr := s.ResolveServerName(newServerURL, *u.ServerName)
		if resolveErr != nil {
			return kvstore.ServerConfig{}, nil, errors.Wrap(resolveErr, "failed to resolve server name")
		}
		if resolved != current.ServerName {
			newServerName = resolved
			serverNameChanged = true
		}
	}

	var updated kvstore.ServerConfig
	err = s.mutate(func(list []kvstore.ServerConfig) ([]kvstore.ServerConfig, error) {
		idx := -1
		for i, entry := range list {
			if entry.ServerID == serverID {
				idx = i
				continue
			}
			if endpointChanged && entry.Endpoint == newEndpoint {
				return nil, wrapf(ErrEndpointTaken, "a server is already registered at this endpoint (server_id: %s); use `/matrix server remove %s` first", entry.ServerID, entry.ServerID)
			}
			if serverNameChanged && entry.ServerName == newServerName {
				return nil, wrapf(ErrNameTaken, "server name %q conflicts with existing server %s; two servers cannot share a Matrix ID domain", newServerName, entry.ServerID)
			}
			if hsTokenChanged && entry.HSToken == *u.HSToken {
				return nil, wrapf(ErrHSTokenTaken, "hs_token conflicts with existing server %s; hs_token must be unique across registered servers", entry.ServerID)
			}
		}
		if idx == -1 {
			// Concurrently removed between the Get above and this callback.
			return nil, wrapf(ErrNotRegistered, "server %s is not registered", serverID)
		}

		result := make([]kvstore.ServerConfig, len(list))
		copy(result, list)
		entry := result[idx]
		entry.ServerURL = newServerURL
		entry.Endpoint = newEndpoint
		entry.ServerName = newServerName
		entry.UsernamePrefix = newUsernamePrefix
		if u.ASToken != nil {
			entry.ASToken = *u.ASToken
		}
		if u.HSToken != nil {
			entry.HSToken = *u.HSToken
		}
		result[idx] = entry
		updated = entry
		return result, nil
	})
	if err != nil {
		return kvstore.ServerConfig{}, nil, err
	}

	var warnings []string
	if endpointChanged {
		warnings = append(warnings, "The server URL changed. Its event domain and shared-channels remote key stay at their original values by design, so editing or deleting posts synced before this change keeps working and the server's existing remote identity is unaffected.")
	}
	if usernamePrefixChanged {
		warnings = append(warnings, "The username prefix only applies to new Mattermost users created for Matrix-originated users from now on; existing users keep their current usernames.")
	}
	if serverNameChanged {
		warnings = append(warnings, fmt.Sprintf("Changing the server name to %q means every existing ghost user for this server is no longer recognized as one; inbound events from them will be treated as regular Matrix users' events until they are recreated.", newServerName))
	}

	if err := s.host.RefreshAndBroadcast("server_updated"); err != nil {
		s.logger.LogWarn("Failed to refresh Matrix clients after updating server", "server_id", serverID, "error", err)
	}

	return updated, warnings, nil
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

// Mappings returns every channel currently bridged to serverID: a single
// ListAllKeysWithPrefix scan, ParseChannelServerMappings on each value, keeping
// entries whose ServerID matches serverID. Never indexes [0] - a channel's mapping
// value is a list.
func (s *Service) Mappings(serverID string) ([]ChannelMapping, error) {
	keys, err := kvstore.ListAllKeysWithPrefix(s.kv, kvstore.KeyPrefixChannelMapping, kvstore.DefaultListKeysBatchSize)
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
