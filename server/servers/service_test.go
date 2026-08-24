package servers

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/matrix"
	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
)

// --- test doubles ---

// testLogger discards everything; tests assert on returned data, not log lines.
type testLogger struct{}

func (testLogger) LogDebug(string, ...any) {}
func (testLogger) LogInfo(string, ...any)  {}
func (testLogger) LogWarn(string, ...any)  {}
func (testLogger) LogError(string, ...any) {}

// memoryKVStore is a minimal in-memory kvstore.KVStore. This package cannot import
// package main's MemoryKVStore (main cannot be imported by anything), so it keeps a
// small copy of its own.
type memoryKVStore struct {
	mu   sync.Mutex
	data map[string][]byte
}

func newMemoryKVStore() *memoryKVStore {
	return &memoryKVStore{data: make(map[string][]byte)}
}

func (m *memoryKVStore) GetTemplateData(string) (string, error) { return "", nil }

func (m *memoryKVStore) Get(key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if v, ok := m.data[key]; ok {
		out := make([]byte, len(v))
		copy(out, v)
		return out, nil
	}
	return nil, nil
}

func (m *memoryKVStore) Set(key string, value []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]byte, len(value))
	copy(out, value)
	m.data[key] = out
	return nil
}

func (m *memoryKVStore) Delete(key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, key)
	return nil
}

func (m *memoryKVStore) ListKeys(page, perPage int) ([]string, error) {
	return m.ListKeysWithPrefix(page, perPage, "")
}

func (m *memoryKVStore) ListKeysWithPrefix(page, perPage int, prefix string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var keys []string
	for k := range m.data {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	start := page * perPage
	if start >= len(keys) {
		return []string{}, nil
	}
	end := min(start+perPage, len(keys))
	return keys[start:end], nil
}

func (m *memoryKVStore) SetAtomicWithRetries(key string, valueFunc func([]byte) ([]byte, error)) error {
	old, err := m.Get(key)
	if err != nil {
		return err
	}
	newValue, err := valueFunc(old)
	if err != nil {
		return err
	}
	return m.Set(key, newValue)
}

// casConflictKVStore gives SetAtomicWithRetries real compare-and-set retry
// semantics via a per-key version counter, so tests can force a real lost race
// (not just a single-shot read-compute-write) by installing onFirstRead.
type casConflictKVStore struct {
	*memoryKVStore

	mu          sync.Mutex
	versions    map[string]int
	reads       map[string]int
	onFirstRead func(store kvstore.KVStore)
}

func newCASConflictKVStore() *casConflictKVStore {
	return &casConflictKVStore{
		memoryKVStore: newMemoryKVStore(),
		versions:      make(map[string]int),
		reads:         make(map[string]int),
	}
}

func (c *casConflictKVStore) SetAtomicWithRetries(key string, valueFunc func([]byte) ([]byte, error)) error {
	for {
		c.mu.Lock()
		old, err := c.Get(key)
		if err != nil {
			c.mu.Unlock()
			return err
		}
		oldVersion := c.versions[key]
		c.reads[key]++
		if c.reads[key] == 1 && c.onFirstRead != nil {
			c.onFirstRead(c.memoryKVStore)
			c.versions[key]++
		}
		c.mu.Unlock()

		newValue, err := valueFunc(old)
		if err != nil {
			return err
		}

		c.mu.Lock()
		if c.versions[key] != oldVersion {
			c.mu.Unlock()
			continue
		}
		if err := c.Set(key, newValue); err != nil {
			c.mu.Unlock()
			return err
		}
		c.versions[key]++
		c.mu.Unlock()
		return nil
	}
}

// writeCountingKVStore counts the writes that actually reach the backing store, so
// tests can assert that an operation which fails validation costs no registry write at
// all - a mutator that returns the unchanged slice instead of an error still persists
// it, and only a write count can tell the two apart.
type writeCountingKVStore struct {
	*memoryKVStore
	writes int
}

func (w *writeCountingKVStore) Set(key string, value []byte) error {
	w.writes++
	return w.memoryKVStore.Set(key, value)
}

// SetAtomicWithRetries reimplements the embedded store's single-shot
// read-compute-write rather than delegating to it: the promoted method would write
// through the inner store's own Set, bypassing this wrapper's counter and leaving
// every atomic write uncounted.
func (w *writeCountingKVStore) SetAtomicWithRetries(key string, valueFunc func([]byte) ([]byte, error)) error {
	old, err := w.Get(key)
	if err != nil {
		return err
	}
	newValue, err := valueFunc(old)
	if err != nil {
		return err
	}
	return w.Set(key, newValue)
}

// erroringKVStore fails Get for one specific key.
type erroringKVStore struct {
	kvstore.KVStore
	errOnGetKey string
}

func (e *erroringKVStore) Get(key string) ([]byte, error) {
	if key == e.errOnGetKey {
		return nil, errors.New("simulated KV store read failure")
	}
	return e.KVStore.Get(key)
}

func (e *erroringKVStore) SetAtomicWithRetries(key string, valueFunc func([]byte) ([]byte, error)) error {
	if _, err := e.Get(key); err != nil {
		return err
	}
	return e.KVStore.SetAtomicWithRetries(key, valueFunc)
}

// fakeHost is a servers.Host test double that records every call, so tests can
// assert Host is honoured (called in the right order, with the right args) rather
// than bypassed.
type fakeHost struct {
	mu sync.Mutex

	matrixClients map[string]*matrix.Client
	siteURL       string
	pluginID      string

	// remoteID, when set, is what RegisterRemoteForSiteURL hands back, for tests that
	// need to assert on a specific remote ID rather than the derived default.
	remoteID string

	registerErr   error
	unregisterErr error
	refreshErr    error

	calls []string
}

func (h *fakeHost) record(call string) {
	h.mu.Lock()
	h.calls = append(h.calls, call)
	h.mu.Unlock()
}

func (h *fakeHost) Calls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.calls))
	copy(out, h.calls)
	return out
}

func (h *fakeHost) MatrixClient(serverID string) *matrix.Client {
	h.record("MatrixClient:" + serverID)
	if h.matrixClients == nil {
		return nil
	}
	return h.matrixClients[serverID]
}

func (h *fakeHost) RegisterRemoteForSiteURL(siteURL string) (string, error) {
	h.record("RegisterRemoteForSiteURL:" + siteURL)
	if h.registerErr != nil {
		return "", h.registerErr
	}
	if h.remoteID != "" {
		return h.remoteID, nil
	}
	return "remote-for-" + siteURL, nil
}

func (h *fakeHost) UnregisterRemote(remoteID string) error {
	h.record("UnregisterRemote:" + remoteID)
	return h.unregisterErr
}

func (h *fakeHost) RefreshAndBroadcast(reason string) error {
	h.record("RefreshAndBroadcast:" + reason)
	return h.refreshErr
}

func (h *fakeHost) SiteURL() string  { return h.siteURL }
func (h *fakeHost) PluginID() string { return h.pluginID }

// panicHost panics on every call - used to prove the service never reaches the
// platform directly for the read-only methods.
type panicHost struct{}

func (panicHost) MatrixClient(string) *matrix.Client { panic("MatrixClient called") }
func (panicHost) RegisterRemoteForSiteURL(string) (string, error) {
	panic("RegisterRemoteForSiteURL called")
}
func (panicHost) UnregisterRemote(string) error    { panic("UnregisterRemote called") }
func (panicHost) RefreshAndBroadcast(string) error { panic("RefreshAndBroadcast called") }
func (panicHost) SiteURL() string                  { panic("SiteURL called") }
func (panicHost) PluginID() string                 { panic("PluginID called") }

func newTestService(t *testing.T) (*Service, *fakeHost, *memoryKVStore) {
	t.Helper()
	kv := newMemoryKVStore()
	host := &fakeHost{matrixClients: map[string]*matrix.Client{}, pluginID: "com.mattermost.plugin-matrix-bridge"}
	return New(kv, testLogger{}, host), host, kv
}

const unreachableURL = "http://127.0.0.1:1"

// --- NormalizeEndpoint ---

func TestNormalizeEndpoint(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		want    string
		wantErr bool
	}{
		{name: "https default port", url: "https://example.com", want: "example.com:443"},
		{name: "http default port", url: "http://example.com", want: "example.com:80"},
		{name: "explicit port", url: "https://example.com:8448", want: "example.com:8448"},
		{name: "uppercase host is lowercased", url: "https://Example.COM", want: "example.com:443"},
		{name: "trailing slash ignored", url: "https://example.com/", want: "example.com:443"},
		{name: "missing scheme errors", url: "example.com", wantErr: true},
		{name: "empty URL errors", url: "", wantErr: true},
		{name: "missing host errors", url: "https://", wantErr: true},
		{name: "unsupported scheme errors", url: "ftp://example.com", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeEndpoint(tt.url)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestEventDomainFromEndpoint(t *testing.T) {
	assert.Equal(t, "localhost_8008", eventDomainFromEndpoint("localhost:8008"))
	assert.NotEqual(t, eventDomainFromEndpoint("localhost:8008"), eventDomainFromEndpoint("localhost:8009"))
}

// --- ResolveServerName ---

func TestResolveServerName(t *testing.T) {
	svc, _, _ := newTestService(t)

	t.Run("configuredName short-circuits without any HTTP call", func(t *testing.T) {
		name, err := svc.ResolveServerName(unreachableURL, "Configured.Example.COM")
		require.NoError(t, err)
		assert.Equal(t, "Configured.Example.COM", name)
	})

	t.Run("key server endpoint supplies the name when reachable", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"server_name": "discovered.example.com"})
		}))
		defer server.Close()

		name, err := svc.ResolveServerName(server.URL, "")
		require.NoError(t, err)
		assert.Equal(t, "discovered.example.com", name)
	})

	t.Run("404 falls through to hostname", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer server.Close()

		name, err := svc.ResolveServerName(server.URL, "")
		require.NoError(t, err)
		parsed, parseErr := url.Parse(server.URL)
		require.NoError(t, parseErr)
		assert.Equal(t, parsed.Hostname(), name)
	})

	t.Run("transport error falls through to hostname and never fails for a parseable URL", func(t *testing.T) {
		name, err := svc.ResolveServerName(unreachableURL, "")
		require.NoError(t, err)
		assert.Equal(t, "127.0.0.1", name)
	})
}

// --- Add ---

func TestAdd(t *testing.T) {
	t.Run("rejects an endpoint already live in the registry", func(t *testing.T) {
		svc, _, _ := newTestService(t)
		_, err := svc.Add(AddRequest{ServerURL: "https://a.example.com", ASToken: "as1", HSToken: "hs1", ServerName: "first.example.com"})
		require.NoError(t, err)

		_, err = svc.Add(AddRequest{ServerURL: "https://a.example.com", ASToken: "as2", HSToken: "hs2", ServerName: "second.example.com"})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrEndpointTaken)
		assert.Contains(t, err.Error(), "already registered")
	})

	t.Run("rejects a server name that duplicates an existing entry's", func(t *testing.T) {
		svc, _, _ := newTestService(t)
		_, err := svc.Add(AddRequest{ServerURL: "https://a.example.com", ASToken: "as1", HSToken: "hs1", ServerName: "shared.example.com"})
		require.NoError(t, err)

		_, err = svc.Add(AddRequest{ServerURL: "https://b.example.com", ASToken: "as2", HSToken: "hs2", ServerName: "shared.example.com"})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrNameTaken)
		assert.Contains(t, err.Error(), "conflicts")
	})

	t.Run("mints a fresh ID when none is supplied", func(t *testing.T) {
		svc, _, _ := newTestService(t)
		created, err := svc.Add(AddRequest{ServerURL: "https://a.example.com", ASToken: "as1", HSToken: "hs1", ServerName: "a.example.com"})
		require.NoError(t, err)
		assert.True(t, model.IsValidId(created.ServerID))
	})

	t.Run("re-adopts a supplied server ID verbatim", func(t *testing.T) {
		svc, _, _ := newTestService(t)
		priorID := model.NewId()
		created, err := svc.Add(AddRequest{ServerURL: "https://a.example.com", ASToken: "as1", HSToken: "hs1", ServerID: priorID, ServerName: "a.example.com"})
		require.NoError(t, err)
		assert.Equal(t, priorID, created.ServerID)
	})

	t.Run("rejects a malformed server ID", func(t *testing.T) {
		svc, _, _ := newTestService(t)
		_, err := svc.Add(AddRequest{ServerURL: "https://a.example.com", ASToken: "as1", HSToken: "hs1", ServerID: "not-a-valid-id", ServerName: "a.example.com"})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidInput)
	})

	t.Run("rejects a server ID colliding with a live entry", func(t *testing.T) {
		svc, _, _ := newTestService(t)
		created, err := svc.Add(AddRequest{ServerURL: "https://a.example.com", ASToken: "as1", HSToken: "hs1", ServerName: "a.example.com"})
		require.NoError(t, err)

		_, err = svc.Add(AddRequest{ServerURL: "https://b.example.com", ASToken: "as2", HSToken: "hs2", ServerID: created.ServerID, ServerName: "b.example.com"})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrIDTaken)
	})

	t.Run("rejects an invalid URL", func(t *testing.T) {
		svc, _, _ := newTestService(t)
		_, err := svc.Add(AddRequest{ServerURL: "not-a-url", ASToken: "as1", HSToken: "hs1"})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidInput)
	})

	t.Run("rejects a duplicate non-empty hs_token and leaves the registry unchanged", func(t *testing.T) {
		svc, _, _ := newTestService(t)
		_, err := svc.Add(AddRequest{ServerURL: "https://a.example.com", ASToken: "as1", HSToken: "shared-hs-token", ServerName: "a.example.com"})
		require.NoError(t, err)

		_, err = svc.Add(AddRequest{ServerURL: "https://b.example.com", ASToken: "as2", HSToken: "shared-hs-token", ServerName: "b.example.com"})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrHSTokenTaken)

		list, err := svc.List()
		require.NoError(t, err)
		require.Len(t, list, 1, "the rejected registration must not be persisted")
	})

	t.Run("an empty hs_token is not treated as a duplicate", func(t *testing.T) {
		svc, _, _ := newTestService(t)
		_, err := svc.Add(AddRequest{ServerURL: "https://a.example.com", ASToken: "as1", ServerName: "a.example.com"})
		require.NoError(t, err)

		_, err = svc.Add(AddRequest{ServerURL: "https://b.example.com", ASToken: "as2", ServerName: "b.example.com"})
		require.NoError(t, err, "two servers with no hs_token yet must both be registrable")
	})

	t.Run("Host is honoured in order: RegisterRemoteForSiteURL then RefreshAndBroadcast", func(t *testing.T) {
		svc, host, _ := newTestService(t)
		created, err := svc.Add(AddRequest{ServerURL: "https://a.example.com", ASToken: "as1", HSToken: "hs1", ServerName: "a.example.com"})
		require.NoError(t, err)

		calls := host.Calls()
		require.Len(t, calls, 2)
		assert.Equal(t, "RegisterRemoteForSiteURL:"+created.SiteURL, calls[0])
		assert.Equal(t, "RefreshAndBroadcast:server_added", calls[1])
		assert.Equal(t, "remote-for-"+created.SiteURL, created.RemoteID, "the entry must carry the remote it was registered with")
	})

	t.Run("fails without ever writing to the registry when remote registration fails", func(t *testing.T) {
		svc, host, kv := newTestService(t)
		host.registerErr = errors.New("boom")

		_, err := svc.Add(AddRequest{ServerURL: "https://a.example.com", ASToken: "as1", HSToken: "hs1", ServerName: "a.example.com"})
		require.Error(t, err, "a server whose remote could not be created must not be registered")
		assert.Contains(t, err.Error(), "failed to register server for shared channels")

		raw, err := kv.Get(kvstore.KeyServersConfig)
		require.NoError(t, err)
		assert.Empty(t, raw, "the registry key must not have been written at all")
		assert.NotContains(t, strings.Join(host.Calls(), ","), "RefreshAndBroadcast")
	})

	t.Run("unregisters the orphan remote when the registry write is rejected", func(t *testing.T) {
		svc, host, _ := newTestService(t)
		_, err := svc.Add(AddRequest{ServerURL: "https://a.example.com", ASToken: "as1", HSToken: "hs1", ServerName: "a.example.com"})
		require.NoError(t, err)

		// Registration now happens before the conflict check, so this add gets as far as
		// obtaining a remote and is only then rejected - that remote belongs to no entry
		// and must not be left registered.
		host.remoteID = "remote-orphan"
		_, err = svc.Add(AddRequest{ServerURL: "https://a.example.com", ASToken: "as2", HSToken: "hs2", ServerName: "second.example.com"})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrEndpointTaken)
		assert.Contains(t, host.Calls(), "UnregisterRemote:remote-orphan")

		list, err := svc.List()
		require.NoError(t, err)
		require.Len(t, list, 1, "the rejected add must not add a second entry")
	})

	t.Run("a RefreshAndBroadcast error is non-fatal", func(t *testing.T) {
		svc, host, _ := newTestService(t)
		host.refreshErr = errors.New("boom")
		_, err := svc.Add(AddRequest{ServerURL: "https://a.example.com", ASToken: "as1", HSToken: "hs1", ServerName: "a.example.com"})
		require.NoError(t, err, "a cache-refresh failure must not fail a registry write that already landed")
	})

	t.Run("EventDomain is derived from the endpoint and stays distinct across ports", func(t *testing.T) {
		svc, _, _ := newTestService(t)
		created1, err := svc.Add(AddRequest{ServerURL: "http://localhost:8008", ASToken: "as1", HSToken: "hs1", ServerName: "localhost8008.example.com"})
		require.NoError(t, err)
		created2, err := svc.Add(AddRequest{ServerURL: "http://localhost:8009", ASToken: "as2", HSToken: "hs2", ServerName: "localhost8009.example.com"})
		require.NoError(t, err)
		assert.NotEqual(t, created1.EventDomain, created2.EventDomain)
	})
}

// --- Remove ---

func TestRemove(t *testing.T) {
	t.Run("unknown ID returns false, not an error", func(t *testing.T) {
		svc, _, _ := newTestService(t)
		removed, err := svc.Remove("nonexistent")
		require.NoError(t, err)
		assert.False(t, removed)
	})

	t.Run("refuses to remove the migrated entry (SiteURL empty) and leaves it registered", func(t *testing.T) {
		svc, _, kv := newTestService(t)
		entry := kvstore.ServerConfig{ServerID: "legacy1", ServerName: "legacy.example.com", SiteURL: ""}
		data, err := kvstore.MarshalServersConfig([]kvstore.ServerConfig{entry})
		require.NoError(t, err)
		require.NoError(t, kv.Set(kvstore.KeyServersConfig, data))

		removed, err := svc.Remove("legacy1")
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrMigratedImmutable)
		assert.False(t, removed)

		list, err := svc.List()
		require.NoError(t, err)
		require.Len(t, list, 1)
	})

	t.Run("removes an entry but leaves its namespaced keys intact", func(t *testing.T) {
		svc, _, kv := newTestService(t)
		created, err := svc.Add(AddRequest{ServerURL: "https://a.example.com", ASToken: "as1", HSToken: "hs1", ServerName: "a.example.com"})
		require.NoError(t, err)

		ghostKey := kvstore.BuildGhostUserKey(created.ServerID, "mmuser1")
		require.NoError(t, kv.Set(ghostKey, []byte("@_mattermost_mmuser1:a.example.com")))

		removed, err := svc.Remove(created.ServerID)
		require.NoError(t, err)
		assert.True(t, removed)

		list, err := svc.List()
		require.NoError(t, err)
		assert.Empty(t, list)

		val, err := kv.Get(ghostKey)
		require.NoError(t, err)
		assert.Equal(t, "@_mattermost_mmuser1:a.example.com", string(val))
	})

	t.Run("Host is honoured in order: UnregisterRemote then RefreshAndBroadcast", func(t *testing.T) {
		svc, host, kv := newTestService(t)
		entry := kvstore.ServerConfig{ServerID: "s1", ServerName: "s1.example.com", SiteURL: "https://s1.example.com", RemoteID: "remote-1"}
		data, err := kvstore.MarshalServersConfig([]kvstore.ServerConfig{entry})
		require.NoError(t, err)
		require.NoError(t, kv.Set(kvstore.KeyServersConfig, data))

		removed, err := svc.Remove("s1")
		require.NoError(t, err)
		require.True(t, removed)

		calls := host.Calls()
		require.Len(t, calls, 2)
		assert.Equal(t, "UnregisterRemote:remote-1", calls[0])
		assert.Equal(t, "RefreshAndBroadcast:server_removed", calls[1])
	})

	t.Run("a failing unregister call is non-fatal", func(t *testing.T) {
		svc, host, kv := newTestService(t)
		host.unregisterErr = errors.New("boom")
		entry := kvstore.ServerConfig{ServerID: "s1", ServerName: "s1.example.com", SiteURL: "https://s1.example.com", RemoteID: "remote-1"}
		data, err := kvstore.MarshalServersConfig([]kvstore.ServerConfig{entry})
		require.NoError(t, err)
		require.NoError(t, kv.Set(kvstore.KeyServersConfig, data))

		removed, err := svc.Remove("s1")
		require.NoError(t, err, "a failing unregister must not fail Remove - the registry write already succeeded")
		assert.True(t, removed)
	})
}

// --- SetEnabled ---

func TestSetEnabled(t *testing.T) {
	t.Run("flips the flag both ways", func(t *testing.T) {
		svc, host, kv := newTestService(t)
		entry := kvstore.ServerConfig{ServerID: "s1", ServerName: "s1.example.com", Enabled: false}
		data, err := kvstore.MarshalServersConfig([]kvstore.ServerConfig{entry})
		require.NoError(t, err)
		require.NoError(t, kv.Set(kvstore.KeyServersConfig, data))

		require.NoError(t, svc.SetEnabled("s1", true))
		got, err := svc.Get("s1")
		require.NoError(t, err)
		assert.True(t, got.Enabled)

		require.NoError(t, svc.SetEnabled("s1", false))
		got, err = svc.Get("s1")
		require.NoError(t, err)
		assert.False(t, got.Enabled)

		// SetEnabled must call ONLY RefreshAndBroadcast - never register/unregister a
		// remote. Disabling is a pure flag flip.
		for _, call := range host.Calls() {
			assert.NotContains(t, call, "RegisterRemote")
			assert.NotContains(t, call, "UnregisterRemote")
		}
	})

	t.Run("unknown ID errors", func(t *testing.T) {
		svc, _, _ := newTestService(t)
		err := svc.SetEnabled("nonexistent", true)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrNotRegistered)
	})

	// The mutate callback may run several times and only the invocation that wins the
	// CAS decides the outcome, so "did I find the server?" must come out of that
	// invocation. A captured flag set on an earlier, discarded invocation would report
	// success for a server that no longer exists by the time the write lands.
	t.Run("not-found is decided by the winning attempt, not an earlier one", func(t *testing.T) {
		host := &fakeHost{}
		store := newCASConflictKVStore()
		svc := New(store, testLogger{}, host)

		seed := []kvstore.ServerConfig{
			{ServerID: "serverA", ServerName: "a.example.com", Enabled: true},
			{ServerID: "serverB", ServerName: "b.example.com", Enabled: true},
		}
		data, err := kvstore.MarshalServersConfig(seed)
		require.NoError(t, err)
		require.NoError(t, store.Set(kvstore.KeyServersConfig, data))

		// A concurrent Remove deletes serverA between this call's read and write, so the
		// first callback invocation sees it and the retried one does not.
		store.onFirstRead = func(kv kvstore.KVStore) {
			remaining, marshalErr := kvstore.MarshalServersConfig(seed[1:])
			require.NoError(t, marshalErr)
			require.NoError(t, kv.Set(kvstore.KeyServersConfig, remaining))
		}

		err = svc.SetEnabled("serverA", false)
		require.Error(t, err, "a server removed before the winning retry must not be reported as successfully updated")
		assert.ErrorIs(t, err, ErrNotRegistered)
		assert.Empty(t, host.Calls(), "a failed update must not broadcast a cache refresh")

		final, err := svc.List()
		require.NoError(t, err)
		require.Len(t, final, 1, "the concurrent removal must stand")
		assert.Equal(t, "serverB", final[0].ServerID)
		assert.True(t, final[0].Enabled, "the untouched server must keep its flag")
	})

	// The other half of the same design: signalling not-found as a callback error aborts
	// the atomic update, so a typo'd server ID never rewrites the whole registry on its
	// way to returning an error.
	t.Run("an unknown ID writes nothing", func(t *testing.T) {
		store := &writeCountingKVStore{memoryKVStore: newMemoryKVStore()}
		svc := New(store, testLogger{}, &fakeHost{})

		data, err := kvstore.MarshalServersConfig([]kvstore.ServerConfig{
			{ServerID: "serverA", ServerName: "a.example.com", Enabled: true},
		})
		require.NoError(t, err)
		require.NoError(t, store.Set(kvstore.KeyServersConfig, data))
		store.writes = 0

		err = svc.SetEnabled("nosuchserver", true)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrNotRegistered)
		assert.Zero(t, store.writes, "an unknown server ID must not trigger a registry write")

		final, err := svc.List()
		require.NoError(t, err)
		require.Len(t, final, 1)
		assert.True(t, final[0].Enabled)
	})
}

// --- Update ---

func seedOneServer(t *testing.T, kv *memoryKVStore, entry kvstore.ServerConfig) {
	t.Helper()
	data, err := kvstore.MarshalServersConfig([]kvstore.ServerConfig{entry})
	require.NoError(t, err)
	require.NoError(t, kv.Set(kvstore.KeyServersConfig, data))
}

func TestUpdate(t *testing.T) {
	baseEntry := func() kvstore.ServerConfig {
		return kvstore.ServerConfig{
			ServerID:       "s1",
			ServerURL:      "https://a.example.com",
			Endpoint:       "a.example.com:443",
			ServerName:     "a.example.com",
			EventDomain:    "a_example_com_443",
			ASToken:        "as1",
			HSToken:        "hs1",
			UsernamePrefix: "matrix",
			Enabled:        true,
			RemoteID:       "remote-1",
			SiteURL:        "https://a.example.com:443",
		}
	}

	t.Run("a nil field leaves the stored value untouched", func(t *testing.T) {
		svc, _, kv := newTestService(t)
		seedOneServer(t, kv, baseEntry())

		updated, warnings, err := svc.Update("s1", Update{ASToken: new("as1-new")})
		require.NoError(t, err)
		assert.Empty(t, warnings)
		assert.Equal(t, "as1-new", updated.ASToken)
		assert.Equal(t, "hs1", updated.HSToken)
		assert.Equal(t, "https://a.example.com", updated.ServerURL)
		assert.Equal(t, "a.example.com", updated.ServerName)
		assert.Equal(t, "matrix", updated.UsernamePrefix)
	})

	t.Run("EventDomain, SiteURL, RemoteID and ServerID are unchanged after a ServerURL change", func(t *testing.T) {
		svc, _, kv := newTestService(t)
		seedOneServer(t, kv, baseEntry())

		updated, warnings, err := svc.Update("s1", Update{ServerURL: new("https://a.example.com:8448")})
		require.NoError(t, err)
		require.Len(t, warnings, 1, "an endpoint change must warn that EventDomain/remote key are unaffected")
		assert.Equal(t, "s1", updated.ServerID)
		assert.Equal(t, "a_example_com_443", updated.EventDomain, "EventDomain must never be recomputed")
		assert.Equal(t, "https://a.example.com:443", updated.SiteURL, "SiteURL is the remote's identity key and must never be re-keyed")
		assert.Equal(t, "remote-1", updated.RemoteID)
		assert.Equal(t, "a.example.com:8448", updated.Endpoint, "Endpoint must be re-derived from the new URL")

		// Assert the stored bytes directly - this is the silent-orphaning regression.
		stored, err := svc.Get("s1")
		require.NoError(t, err)
		assert.Equal(t, "a_example_com_443", stored.EventDomain)
		assert.Equal(t, "https://a.example.com:443", stored.SiteURL)
	})

	t.Run("an endpoint colliding with another entry is rejected", func(t *testing.T) {
		svc, _, kv := newTestService(t)
		require.NoError(t, kv.Set(kvstore.KeyServersConfig, mustMarshal(t, []kvstore.ServerConfig{
			baseEntry(),
			{ServerID: "s2", ServerURL: "https://b.example.com", Endpoint: "b.example.com:443", ServerName: "b.example.com"},
		})))

		_, _, err := svc.Update("s1", Update{ServerURL: new("https://b.example.com")})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrEndpointTaken)
	})

	t.Run("re-submitting the entry's own endpoint succeeds", func(t *testing.T) {
		svc, _, kv := newTestService(t)
		seedOneServer(t, kv, baseEntry())

		updated, warnings, err := svc.Update("s1", Update{ServerURL: new("https://a.example.com")})
		require.NoError(t, err)
		assert.Empty(t, warnings, "no genuine endpoint change means no warning")
		assert.Equal(t, "a.example.com:443", updated.Endpoint)
	})

	t.Run("empty ASToken is rejected", func(t *testing.T) {
		svc, _, kv := newTestService(t)
		seedOneServer(t, kv, baseEntry())
		_, _, err := svc.Update("s1", Update{ASToken: new("")})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidInput)
	})

	t.Run("empty HSToken is rejected", func(t *testing.T) {
		svc, _, kv := newTestService(t)
		seedOneServer(t, kv, baseEntry())
		_, _, err := svc.Update("s1", Update{HSToken: new("")})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrInvalidInput)
	})

	t.Run("an hs_token colliding with another entry is rejected", func(t *testing.T) {
		svc, _, kv := newTestService(t)
		require.NoError(t, kv.Set(kvstore.KeyServersConfig, mustMarshal(t, []kvstore.ServerConfig{
			baseEntry(),
			{ServerID: "s2", ServerURL: "https://b.example.com", Endpoint: "b.example.com:443", ServerName: "b.example.com", HSToken: "hs2"},
		})))

		_, _, err := svc.Update("s1", Update{HSToken: new("hs2")})
		require.Error(t, err, "the edit path must enforce hs_token uniqueness exactly as Add does")
		assert.ErrorIs(t, err, ErrHSTokenTaken)
	})

	t.Run("re-submitting the entry's own hs_token succeeds", func(t *testing.T) {
		svc, _, kv := newTestService(t)
		require.NoError(t, kv.Set(kvstore.KeyServersConfig, mustMarshal(t, []kvstore.ServerConfig{
			baseEntry(),
			{ServerID: "s2", ServerURL: "https://b.example.com", Endpoint: "b.example.com:443", ServerName: "b.example.com", HSToken: "hs2"},
		})))

		updated, _, err := svc.Update("s1", Update{HSToken: new("hs1")})
		require.NoError(t, err)
		assert.Equal(t, "hs1", updated.HSToken)
	})

	t.Run("empty UsernamePrefix resets to the default", func(t *testing.T) {
		svc, _, kv := newTestService(t)
		entry := baseEntry()
		entry.UsernamePrefix = "custom"
		seedOneServer(t, kv, entry)
		updated, warnings, err := svc.Update("s1", Update{UsernamePrefix: new("")})
		require.NoError(t, err)
		assert.Equal(t, DefaultUsernamePrefix, updated.UsernamePrefix)
		assert.Len(t, warnings, 1)
	})

	t.Run("a ServerName colliding with another entry is rejected", func(t *testing.T) {
		svc, _, kv := newTestService(t)
		require.NoError(t, kv.Set(kvstore.KeyServersConfig, mustMarshal(t, []kvstore.ServerConfig{
			baseEntry(),
			{ServerID: "s2", ServerURL: "https://b.example.com", Endpoint: "b.example.com:443", ServerName: "b.example.com"},
		})))

		_, _, err := svc.Update("s1", Update{ServerName: new("b.example.com")})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrNameTaken)
	})

	t.Run("a successful ServerName change returns a warning", func(t *testing.T) {
		svc, _, kv := newTestService(t)
		seedOneServer(t, kv, baseEntry())

		updated, warnings, err := svc.Update("s1", Update{ServerName: new("renamed.example.com")})
		require.NoError(t, err)
		assert.Equal(t, "renamed.example.com", updated.ServerName)
		require.Len(t, warnings, 1)
		assert.Contains(t, warnings[0], "ghost")
	})

	t.Run("a migrated entry (SiteURL empty) is editable", func(t *testing.T) {
		svc, _, kv := newTestService(t)
		entry := baseEntry()
		entry.SiteURL = ""
		seedOneServer(t, kv, entry)

		updated, _, err := svc.Update("s1", Update{ASToken: new("as-new")})
		require.NoError(t, err)
		assert.Equal(t, "as-new", updated.ASToken)
		assert.Empty(t, updated.SiteURL, "SiteURL must stay empty - Update never sets it")
	})

	t.Run("unknown server_id is not registered", func(t *testing.T) {
		svc, _, _ := newTestService(t)
		_, _, err := svc.Update("nonexistent", Update{ASToken: new("x")})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrNotRegistered)
	})

	t.Run("Update calls only RefreshAndBroadcast - never register or unregister a remote", func(t *testing.T) {
		svc, host, kv := newTestService(t)
		seedOneServer(t, kv, baseEntry())

		_, _, err := svc.Update("s1", Update{ASToken: new("as-new")})
		require.NoError(t, err)

		calls := host.Calls()
		require.Len(t, calls, 1)
		assert.Equal(t, "RefreshAndBroadcast:server_updated", calls[0])
	})

	t.Run("concurrent edits produce one winner", func(t *testing.T) {
		store := newCASConflictKVStore()
		host := &fakeHost{matrixClients: map[string]*matrix.Client{}}
		svc := New(store, testLogger{}, host)
		require.NoError(t, store.Set(kvstore.KeyServersConfig, mustMarshal(t, []kvstore.ServerConfig{baseEntry()})))

		store.onFirstRead = func(kv kvstore.KVStore) {
			current, getErr := kv.Get(kvstore.KeyServersConfig)
			require.NoError(t, getErr)
			list, parseErr := kvstore.ParseServersConfig(current)
			require.NoError(t, parseErr)
			for i := range list {
				if list[i].ServerID == "s1" {
					list[i].HSToken = "hs-from-concurrent-writer"
				}
			}
			updatedBytes, marshalErr := kvstore.MarshalServersConfig(list)
			require.NoError(t, marshalErr)
			require.NoError(t, kv.Set(kvstore.KeyServersConfig, updatedBytes))
		}

		_, _, err := svc.Update("s1", Update{ASToken: new("as-from-this-call")})
		require.NoError(t, err)

		final, err := svc.Get("s1")
		require.NoError(t, err)
		assert.Equal(t, "as-from-this-call", final.ASToken, "this call's own write must still land")
		assert.Equal(t, "hs-from-concurrent-writer", final.HSToken, "the concurrent writer's change must survive, not be clobbered by a stale retry")
	})
}

func mustMarshal(t *testing.T, servers []kvstore.ServerConfig) []byte {
	t.Helper()
	data, err := kvstore.MarshalServersConfig(servers)
	require.NoError(t, err)
	return data
}

// --- Seed ---

func TestSeed(t *testing.T) {
	t.Run("inserts a fresh entry verbatim - no name resolution, no remote registration", func(t *testing.T) {
		svc, host, _ := newTestService(t)
		entry := kvstore.ServerConfig{
			ServerID:    "legacy1",
			ServerURL:   "https://legacy.example.com",
			Endpoint:    "legacy.example.com:443",
			ServerName:  "legacy.example.com",
			EventDomain: "caller_supplied_domain",
			SiteURL:     "",
		}
		id, err := svc.Seed(entry)
		require.NoError(t, err)
		assert.Equal(t, "legacy1", id)

		stored, err := svc.Get("legacy1")
		require.NoError(t, err)
		assert.Equal(t, "caller_supplied_domain", stored.EventDomain)
		assert.Empty(t, stored.SiteURL)
		assert.Empty(t, host.Calls(), "Seed must not touch Host at all")
	})

	t.Run("idempotent by endpoint", func(t *testing.T) {
		svc, _, _ := newTestService(t)
		entry := kvstore.ServerConfig{ServerID: "legacy1", Endpoint: "legacy.example.com:443"}
		id1, err := svc.Seed(entry)
		require.NoError(t, err)

		entry2 := kvstore.ServerConfig{ServerID: "legacy2", Endpoint: "legacy.example.com:443"}
		id2, err := svc.Seed(entry2)
		require.NoError(t, err)
		assert.Equal(t, id1, id2, "a second Seed at the same endpoint must return the already-materialized ID")

		list, err := svc.List()
		require.NoError(t, err)
		assert.Len(t, list, 1)
	})
}

// --- Typed errors travel through SetAtomicWithRetries ---

func TestTypedErrorsSurviveCASRoundTrip(t *testing.T) {
	svc, _, _ := newTestService(t)
	_, err := svc.Add(AddRequest{ServerURL: "https://a.example.com", ASToken: "as1", HSToken: "hs1", ServerName: "a.example.com"})
	require.NoError(t, err)

	_, err = svc.Add(AddRequest{ServerURL: "https://a.example.com", ASToken: "as2", HSToken: "hs2", ServerName: "b.example.com"})
	require.Error(t, err)
	assert.Truef(t, errors.Is(err, ErrEndpointTaken), "errors.Is must still match ErrEndpointTaken after traveling through SetAtomicWithRetries: %v", err)
}

// TestMutateRetriesOnRealConflict covers the CAS-retry-under-real-conflict
// requirement: a genuine lost race (not just a single-shot read-compute-write) must
// force the mutator to retry against a fresh read, and both writes must land.
func TestMutateRetriesOnRealConflict(t *testing.T) {
	store := newCASConflictKVStore()
	host := &fakeHost{matrixClients: map[string]*matrix.Client{}}
	svc := New(store, testLogger{}, host)

	seed := []kvstore.ServerConfig{
		{ServerID: "serverA", ServerName: "a.example.com", Enabled: true},
		{ServerID: "serverB", ServerName: "b.example.com", Enabled: true},
	}
	data, err := kvstore.MarshalServersConfig(seed)
	require.NoError(t, err)
	require.NoError(t, store.Set(kvstore.KeyServersConfig, data))

	store.onFirstRead = func(kv kvstore.KVStore) {
		current, getErr := kv.Get(kvstore.KeyServersConfig)
		require.NoError(t, getErr)
		servers, parseErr := kvstore.ParseServersConfig(current)
		require.NoError(t, parseErr)
		for i := range servers {
			if servers[i].ServerID == "serverB" {
				servers[i].Enabled = false
			}
		}
		updated, marshalErr := kvstore.MarshalServersConfig(servers)
		require.NoError(t, marshalErr)
		require.NoError(t, kv.Set(kvstore.KeyServersConfig, updated))
	}

	var mutatorCalls int
	err = svc.mutate(func(current []kvstore.ServerConfig) ([]kvstore.ServerConfig, error) {
		mutatorCalls++
		updated := make([]kvstore.ServerConfig, len(current))
		copy(updated, current)
		for i := range updated {
			if updated[i].ServerID == "serverA" {
				updated[i].UsernamePrefix = "renamed"
			}
		}
		return updated, nil
	})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, mutatorCalls, 2, "a real conflict must force a retry against a fresh read")

	final, err := svc.List()
	require.NoError(t, err)
	byID := map[string]kvstore.ServerConfig{}
	for _, s := range final {
		byID[s.ServerID] = s
	}
	assert.Equal(t, "renamed", byID["serverA"].UsernamePrefix)
	assert.False(t, byID["serverB"].Enabled, "the concurrent writer's change must survive")
}

// --- The service never reaches the platform directly ---

func TestServiceNeverReachesHostForReads(t *testing.T) {
	svc := New(newMemoryKVStore(), testLogger{}, panicHost{})

	_, err := svc.List()
	require.NoError(t, err)
	_, err = svc.Get("nope")
	require.Error(t, err) // ErrNotRegistered, not a panic
	_, err = svc.Mappings("nope")
	require.NoError(t, err)
	_, err = svc.ResolveIdentifier("nope")
	require.Error(t, err)
}

// TestListSurfacesRegistryReadFailure covers initMatrixClients' error-handling
// requirement one layer down: a registry read failure must surface as an error
// rather than silently returning an empty (or stale) list.
func TestListSurfacesRegistryReadFailure(t *testing.T) {
	kv := &erroringKVStore{KVStore: newMemoryKVStore(), errOnGetKey: kvstore.KeyServersConfig}
	svc := New(kv, testLogger{}, &fakeHost{})

	_, err := svc.List()
	require.Error(t, err)
}

// --- ResolveIdentifier ---

func TestResolveIdentifier(t *testing.T) {
	serverA := kvstore.ServerConfig{ServerID: "serverA", ServerName: "a.example.com", ServerURL: "https://a.example.com"}
	serverB := kvstore.ServerConfig{ServerID: "serverB", ServerName: "b.example.com", ServerURL: "https://b.example.com"}

	seed := func(t *testing.T, entries ...kvstore.ServerConfig) *Service {
		svc, _, kv := newTestService(t)
		data, err := kvstore.MarshalServersConfig(entries)
		require.NoError(t, err)
		require.NoError(t, kv.Set(kvstore.KeyServersConfig, data))
		return svc
	}

	t.Run("empty arg with zero servers errors", func(t *testing.T) {
		svc, _, _ := newTestService(t)
		_, err := svc.ResolveIdentifier("")
		require.Error(t, err)
	})

	t.Run("empty arg with exactly one server resolves it", func(t *testing.T) {
		svc := seed(t, serverA)
		id, err := svc.ResolveIdentifier("")
		require.NoError(t, err)
		assert.Equal(t, "serverA", id)
	})

	t.Run("empty arg with multiple servers is ambiguous", func(t *testing.T) {
		svc := seed(t, serverA, serverB)
		_, err := svc.ResolveIdentifier("")
		require.Error(t, err)
	})

	t.Run("matches by server_id", func(t *testing.T) {
		svc := seed(t, serverA, serverB)
		id, err := svc.ResolveIdentifier("serverB")
		require.NoError(t, err)
		assert.Equal(t, "serverB", id)
	})

	t.Run("matches by server name", func(t *testing.T) {
		svc := seed(t, serverA, serverB)
		id, err := svc.ResolveIdentifier("a.example.com")
		require.NoError(t, err)
		assert.Equal(t, "serverA", id)
	})

	t.Run("matches by URL host", func(t *testing.T) {
		serverC := kvstore.ServerConfig{ServerID: "serverC", ServerName: "different-name.example.org", ServerURL: "https://c.example.com"}
		svc := seed(t, serverC)
		id, err := svc.ResolveIdentifier("c.example.com")
		require.NoError(t, err)
		assert.Equal(t, "serverC", id)
	})

	t.Run("no match errors", func(t *testing.T) {
		svc := seed(t, serverA)
		_, err := svc.ResolveIdentifier("nonexistent")
		require.Error(t, err)
	})
}

// --- RegistrationYAML ---

func TestRegistrationYAML(t *testing.T) {
	for _, tc := range []struct {
		name    string
		siteURL string
	}{
		{"plain site url", "https://mm.example.com"},
		{"site url with trailing slash", "https://mm.example.com/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, host, kv := newTestService(t)
			host.siteURL = tc.siteURL
			host.pluginID = "com.mattermost.plugin-matrix-bridge"

			entry := kvstore.ServerConfig{ServerID: "server1", ServerName: "a.example.com", ASToken: "as_token_value", HSToken: "hs_token_value"}
			data, err := kvstore.MarshalServersConfig([]kvstore.ServerConfig{entry})
			require.NoError(t, err)
			require.NoError(t, kv.Set(kvstore.KeyServersConfig, data))

			filename, content, err := svc.RegistrationYAML("server1")
			require.NoError(t, err)
			assert.NotEmpty(t, filename)
			assert.Contains(t, content, "url: https://mm.example.com/plugins/com.mattermost.plugin-matrix-bridge\n")
			assert.NotContains(t, content, "_matrix/app/v1")
			assert.NotContains(t, content, "//plugins/")
			assert.Contains(t, content, "as_token: as_token_value")
			assert.Contains(t, content, "hs_token: hs_token_value")
		})
	}

	t.Run("unknown server 404s", func(t *testing.T) {
		svc, _, _ := newTestService(t)
		_, _, err := svc.RegistrationYAML("nope")
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrNotRegistered)
	})
}

// --- Mappings ---

func TestMappings(t *testing.T) {
	svc, _, kv := newTestService(t)

	// Two-entry array so the ServerID filter is genuinely exercised - never index [0].
	mappingData, err := kvstore.MarshalChannelServerMappings([]kvstore.ChannelServerMapping{
		{ServerID: "other-server", RoomID: "!other:example.com"},
		{ServerID: "server1", RoomID: "!room1:example.com"},
	})
	require.NoError(t, err)
	require.NoError(t, kv.Set(kvstore.BuildChannelMappingKey("channel1"), mappingData))

	otherData, err := kvstore.MarshalChannelServerMappings([]kvstore.ChannelServerMapping{{ServerID: "other-server", RoomID: "!x:example.com"}})
	require.NoError(t, err)
	require.NoError(t, kv.Set(kvstore.BuildChannelMappingKey("channel2"), otherData))

	mappings, err := svc.Mappings("server1")
	require.NoError(t, err)
	require.Len(t, mappings, 1)
	assert.Equal(t, "channel1", mappings[0].ChannelID)
	assert.Equal(t, "!room1:example.com", mappings[0].RoomID)
}

// --- Diagnose ---

func TestDiagnose(t *testing.T) {
	t.Run("unregistered server yields only a failed registry check", func(t *testing.T) {
		svc, _, _ := newTestService(t)
		diag := svc.Diagnose("nope")
		require.Len(t, diag.Checks, 1)
		assert.Equal(t, "registry", diag.Checks[0].Key)
		assert.Equal(t, "fail", diag.Checks[0].Status)
	})

	t.Run("nil client yields fail then skip for connection and appservice", func(t *testing.T) {
		svc, _, kv := newTestService(t)
		entry := kvstore.ServerConfig{ServerID: "server1", ServerURL: "https://a.example.com"}
		data, err := kvstore.MarshalServersConfig([]kvstore.ServerConfig{entry})
		require.NoError(t, err)
		require.NoError(t, kv.Set(kvstore.KeyServersConfig, data))

		diag := svc.Diagnose("server1")
		require.Len(t, diag.Checks, 4)
		assert.Equal(t, "ok", diag.Checks[0].Status)
		assert.Equal(t, "fail", diag.Checks[1].Status)
		assert.Equal(t, "skip", diag.Checks[2].Status)
		assert.Equal(t, "skip", diag.Checks[3].Status)
	})

	t.Run("full success runs every check", func(t *testing.T) {
		matrixServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "/account/whoami"):
				w.WriteHeader(http.StatusOK)
			case strings.Contains(r.URL.Path, "/profile/"):
				w.WriteHeader(http.StatusNotFound) // AS has permission to query its namespace
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer matrixServer.Close()

		svc, host, kv := newTestService(t)
		entry := kvstore.ServerConfig{ServerID: "server1", ServerURL: matrixServer.URL}
		data, err := kvstore.MarshalServersConfig([]kvstore.ServerConfig{entry})
		require.NoError(t, err)
		require.NoError(t, kv.Set(kvstore.KeyServersConfig, data))

		client := matrix.NewClientWithLoggerAndRateLimit(matrixServer.URL, "as-token", "", "", testLogger{}, matrix.RateLimitConfig{})
		host.matrixClients["server1"] = client

		diag := svc.Diagnose("server1")
		require.Len(t, diag.Checks, 4)
		for _, check := range diag.Checks {
			assert.Equal(t, "ok", check.Status, "check %s must be ok", check.Key)
		}
	})
}

// --- ProbeHealth ---

func TestProbeHealth(t *testing.T) {
	svc, host, _ := newTestService(t)

	matrixServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer matrixServer.Close()

	healthyClient := matrix.NewClientWithLoggerAndRateLimit(matrixServer.URL, "as-token", "", "", testLogger{}, matrix.RateLimitConfig{})
	host.matrixClients["healthy"] = healthyClient

	list := []kvstore.ServerConfig{
		{ServerID: "disabled", Enabled: false},
		{ServerID: "unavailable", Enabled: true},
		{ServerID: "healthy", Enabled: true},
	}

	results := svc.ProbeHealth(list)
	assert.Equal(t, "disabled", results["disabled"])
	assert.Equal(t, "unavailable", results["unavailable"])
	assert.Equal(t, "healthy", results["healthy"])
}

func TestProbeHealthTimesOut(t *testing.T) {
	old := statusProbeDeadline
	statusProbeDeadline = 1
	defer func() { statusProbeDeadline = old }()

	block := make(chan struct{})
	matrixServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-block
		w.WriteHeader(http.StatusOK)
	}))
	defer matrixServer.Close()
	// Registered after Close, so it runs first (defers are LIFO): Close() blocks
	// until in-flight handlers finish, and the abandoned probe request from
	// ProbeHealth below is still blocked in the handler at that point. Closing
	// this first unblocks it so Close() doesn't wait forever.
	defer close(block)

	svc, host, _ := newTestService(t)
	client := matrix.NewClientWithLoggerAndRateLimit(matrixServer.URL, "as-token", "", "", testLogger{}, matrix.RateLimitConfig{})
	host.matrixClients["slow"] = client

	results := svc.ProbeHealth([]kvstore.ServerConfig{{ServerID: "slow", Enabled: true}})
	assert.Equal(t, "timed out", results["slow"])
}
