package command

import (
	"sort"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/mattermost/mattermost/server/public/pluginapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/matrix"
	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/servers"
	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
)

// fakeHost is a minimal servers.Host for command package tests: every method is a
// configurable no-op except MatrixClient, which serves whatever a test wired up.
type fakeHost struct {
	matrixClients map[string]*matrix.Client
	siteURL       string
	pluginID      string

	registerErr   error
	unregisterErr error
	refreshErr    error
}

func (h *fakeHost) MatrixClient(serverID string) *matrix.Client { return h.matrixClients[serverID] }
func (h *fakeHost) UnregisterRemote(_ string) error             { return h.unregisterErr }
func (h *fakeHost) RefreshAndBroadcast(_ string) error          { return h.refreshErr }

func (h *fakeHost) RegisterRemoteForSiteURL(siteURL string) (string, error) {
	if h.registerErr != nil {
		return "", h.registerErr
	}
	return "remote-for-" + siteURL, nil
}
func (h *fakeHost) SiteURL() string  { return h.siteURL }
func (h *fakeHost) PluginID() string { return h.pluginID }

// mockPlugin implements PluginAccessor for testing. The server registry itself is a
// real *servers.Service backed by an in-memory KV store and fakeHost - only the
// non-registry PluginAccessor methods (Matrix client access, channel mapping,
// migrations) are hand-faked here.
type mockPlugin struct {
	client    *pluginapi.Client
	kvstore   kvstore.KVStore
	pluginAPI plugin.API

	serverSvc *servers.Service
	host      *fakeHost

	mapErr   error
	unmapErr error

	migrationErr    error
	migrationResult *MigrationResult
}

func (m *mockPlugin) GetKVStore() kvstore.KVStore { return m.kvstore }

func (m *mockPlugin) Servers() *servers.Service { return m.serverSvc }

func (m *mockPlugin) GetMatrixClientForServer(_ string) *matrix.Client { return nil }
func (m *mockPlugin) GetRemoteIDForServer(serverID string) string      { return "remote-" + serverID }

func (m *mockPlugin) CreateOrGetGhostUserForServer(serverID, mattermostUserID string) (string, error) {
	return "@_mattermost_" + mattermostUserID + ":" + serverID, nil
}

func (m *mockPlugin) GetMatrixUserIDFromMattermostUserForServer(serverID, mattermostUserID string) (string, error) {
	return "@" + mattermostUserID + ":" + serverID, nil
}

func (m *mockPlugin) MapChannelToServer(_, _, _ string) error  { return m.mapErr }
func (m *mockPlugin) UnmapChannelFromServer(_, _ string) error { return m.unmapErr }

func (m *mockPlugin) GetPluginAPI() plugin.API              { return m.pluginAPI }
func (m *mockPlugin) GetPluginAPIClient() *pluginapi.Client { return m.client }
func (m *mockPlugin) GetPluginID() string                   { return "com.mattermost.plugin-matrix-bridge" }

func (m *mockPlugin) RunKVStoreMigrations() error { return m.migrationErr }
func (m *mockPlugin) RunKVStoreMigrationsWithResults() (*MigrationResult, error) {
	if m.migrationErr != nil {
		return nil, m.migrationErr
	}
	if m.migrationResult != nil {
		return m.migrationResult, nil
	}
	return &MigrationResult{}, nil
}

// memoryKVStore is a minimal in-memory kvstore.KVStore for command tests that touch
// keyspace scans (e.g. executeListMappingsCommand).
type memoryKVStore struct {
	data map[string][]byte

	// getErr/setErr inject a failure for one specific key, so tests can exercise
	// KV failure paths without failing every unrelated read or write.
	getErr  map[string]error
	setErr  map[string]error
	setKeys []string

	// onGet, when set, runs just after a Get has been served, so a test can mutate the
	// store between two reads the handler under test makes back to back - the first read
	// still sees the seeded value.
	onGet func(key string)
}

func newMemoryKVStore() *memoryKVStore {
	return &memoryKVStore{
		data:   make(map[string][]byte),
		getErr: make(map[string]error),
		setErr: make(map[string]error),
	}
}

func (m *memoryKVStore) GetTemplateData(_ string) (string, error) { return "", nil }

func (m *memoryKVStore) Get(key string) ([]byte, error) {
	if err := m.getErr[key]; err != nil {
		return nil, err
	}
	value := m.data[key]
	if m.onGet != nil {
		m.onGet(key)
	}
	return value, nil
}

func (m *memoryKVStore) Set(key string, value []byte) error {
	m.setKeys = append(m.setKeys, key)
	if err := m.setErr[key]; err != nil {
		return err
	}
	m.data[key] = value
	return nil
}

func (m *memoryKVStore) Delete(key string) error {
	delete(m.data, key)
	return nil
}

func (m *memoryKVStore) ListKeys(page, perPage int) ([]string, error) {
	return m.ListKeysWithPrefix(page, perPage, "")
}

func (m *memoryKVStore) ListKeysWithPrefix(page, perPage int, prefix string) ([]string, error) {
	var keys []string
	for k := range m.data {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	// map iteration order is random; pagination below slices a fixed [start:end] window,
	// which needs a stable order or the same page could return different keys across calls.
	sort.Strings(keys)
	start := page * perPage
	if start >= len(keys) {
		return []string{}, nil
	}
	end := min(start+perPage, len(keys))
	return keys[start:end], nil
}

func (m *memoryKVStore) SetAtomicWithRetries(key string, valueFunc func(oldValue []byte) ([]byte, error)) error {
	newValue, err := valueFunc(m.data[key])
	if err != nil {
		return err
	}
	return m.Set(key, newValue)
}

// testLogger is a servers.Logger that discards everything - command tests assert on
// command output, not on what the registry logged internally.
type testLogger struct{}

func (testLogger) LogDebug(string, ...any) {}
func (testLogger) LogInfo(string, ...any)  {}
func (testLogger) LogWarn(string, ...any)  {}
func (testLogger) LogError(string, ...any) {}

// newTestHandler builds a Handler with a fresh mockPlugin and mock Mattermost API,
// without going through NewCommandHandler (which registers a real slash command).
// seedServers, if any, are written directly to the KV store - bypassing
// Servers().Add's name-resolution network probe and server_id format validation, so
// fixtures can use readable IDs like "serverA".
func newTestHandler(t *testing.T, seedServers ...kvstore.ServerConfig) (*Handler, *mockPlugin, *plugintest.API) {
	t.Helper()

	api := &plugintest.API{}
	client := pluginapi.NewClient(api, nil)
	store := newMemoryKVStore()

	if len(seedServers) > 0 {
		data, err := kvstore.MarshalServersConfig(seedServers)
		require.NoError(t, err)
		require.NoError(t, store.Set(kvstore.KeyServersConfig, data))
	}

	host := &fakeHost{matrixClients: map[string]*matrix.Client{}, pluginID: "com.mattermost.plugin-matrix-bridge"}
	mp := &mockPlugin{
		client:    client,
		pluginAPI: api,
		kvstore:   store,
		serverSvc: servers.New(store, testLogger{}, host),
		host:      host,
	}

	return &Handler{
		plugin:    mp,
		client:    client,
		kvstore:   store,
		pluginAPI: api,
	}, mp, api
}

// Server identifier resolution (server_id / ServerName / URL host matching) now
// lives in servers.Service.ResolveIdentifier - see server/servers/service_test.go.
// TestExecuteServerMapDispatch and TestExecuteServerTest below still exercise it
// end-to-end through the command layer.

func TestStripFlags(t *testing.T) {
	t.Run("no flags present", func(t *testing.T) {
		positional, flags, err := stripFlags([]string{"a", "b", "c"}, "server-id", "server-name")
		require.NoError(t, err)
		assert.Equal(t, []string{"a", "b", "c"}, positional)
		assert.Empty(t, flags)
	})

	t.Run("space-separated flag value", func(t *testing.T) {
		positional, flags, err := stripFlags([]string{"url", "as", "hs", "--server-id", "abc123"}, "server-id", "server-name")
		require.NoError(t, err)
		assert.Equal(t, []string{"url", "as", "hs"}, positional)
		assert.Equal(t, "abc123", flags["server-id"])
	})

	t.Run("equals-separated flag value", func(t *testing.T) {
		positional, flags, err := stripFlags([]string{"url", "as", "hs", "--server-id=abc123"}, "server-id", "server-name")
		require.NoError(t, err)
		assert.Equal(t, []string{"url", "as", "hs"}, positional)
		assert.Equal(t, "abc123", flags["server-id"])
	})

	t.Run("flag before positional username_prefix does not shift it", func(t *testing.T) {
		positional, flags, err := stripFlags([]string{"url", "as", "hs", "--server-id", "abc123", "myprefix"}, "server-id", "server-name")
		require.NoError(t, err)
		assert.Equal(t, []string{"url", "as", "hs", "myprefix"}, positional)
		assert.Equal(t, "abc123", flags["server-id"])
	})

	t.Run("flag in the middle does not shift trailing positional args", func(t *testing.T) {
		positional, flags, err := stripFlags([]string{"url", "--server-name=my.name", "as", "hs", "myprefix"}, "server-id", "server-name")
		require.NoError(t, err)
		assert.Equal(t, []string{"url", "as", "hs", "myprefix"}, positional)
		assert.Equal(t, "my.name", flags["server-name"])
	})

	t.Run("both flags present in either form", func(t *testing.T) {
		positional, flags, err := stripFlags([]string{"url", "as", "hs", "--server-id=abc123", "--server-name", "my.name"}, "server-id", "server-name")
		require.NoError(t, err)
		assert.Equal(t, []string{"url", "as", "hs"}, positional)
		assert.Equal(t, "abc123", flags["server-id"])
		assert.Equal(t, "my.name", flags["server-name"])
	})

	t.Run("unknown flag errors instead of becoming positional", func(t *testing.T) {
		_, _, err := stripFlags([]string{"url", "as", "hs", "--bogus", "value"}, "server-id", "server-name")
		require.Error(t, err)
	})

	t.Run("missing value for flag errors", func(t *testing.T) {
		_, _, err := stripFlags([]string{"url", "as", "hs", "--server-id"}, "server-id", "server-name")
		require.Error(t, err)
	})
}

func TestExecuteServerGroupAdminGate(t *testing.T) {
	h, _, api := newTestHandler(t)

	userID := model.NewId()
	api.On("HasPermissionTo", userID, model.PermissionManageSystem).Return(false)

	resp := h.executeServerGroup(&model.CommandArgs{UserId: userID}, []string{"list"})
	assert.Contains(t, resp.Text, "System Admin")
}

func TestExecuteServerGroupAddRemoveList(t *testing.T) {
	userID := model.NewId()
	args := &model.CommandArgs{UserId: userID}

	t.Run("add requires at least 3 positional args", func(t *testing.T) {
		h, _, api := newTestHandler(t)
		api.On("HasPermissionTo", userID, model.PermissionManageSystem).Return(true)
		resp := h.executeServerGroup(args, []string{"add", "https://matrix.example.com"})
		assert.Contains(t, resp.Text, "Usage")
	})

	t.Run("add happy path registers a server", func(t *testing.T) {
		h, mp, api := newTestHandler(t)
		api.On("HasPermissionTo", userID, model.PermissionManageSystem).Return(true)
		resp := h.executeServerGroup(args, []string{"add", "https://matrix.example.com", "as-token", "hs-token"})
		assert.Contains(t, resp.Text, "added")
		list, err := mp.serverSvc.List()
		require.NoError(t, err)
		assert.Len(t, list, 1)
	})

	t.Run("add failure surfaces the error", func(t *testing.T) {
		h2, _, api2 := newTestHandler(t)
		api2.On("HasPermissionTo", userID, model.PermissionManageSystem).Return(true)
		// An invalid server_id forces Servers().Add to fail its own validation,
		// without needing to fake a registry-write failure.
		resp := h2.executeServerGroup(args, []string{"add", "https://matrix.example.com", "as-token", "hs-token", "--server-id", "not-a-valid-id"})
		assert.Contains(t, resp.Text, "Failed to add server")
	})

	t.Run("list shows the registered server", func(t *testing.T) {
		seeded := kvstore.ServerConfig{ServerID: "server-list-test", ServerName: "list.example.com", ServerURL: "https://list.example.com", Enabled: true}
		h, _, api := newTestHandler(t, seeded)
		api.On("HasPermissionTo", userID, model.PermissionManageSystem).Return(true)
		resp := h.executeServerGroup(args, []string{"list"})
		assert.Contains(t, resp.Text, seeded.ServerID)
	})

	t.Run("remove requires a server_id", func(t *testing.T) {
		h, _, api := newTestHandler(t)
		api.On("HasPermissionTo", userID, model.PermissionManageSystem).Return(true)
		resp := h.executeServerGroup(args, []string{"remove"})
		assert.Contains(t, resp.Text, "Usage")
	})

	t.Run("remove of an unknown identifier reports no match", func(t *testing.T) {
		h3, _, api3 := newTestHandler(t)
		api3.On("HasPermissionTo", userID, model.PermissionManageSystem).Return(true)
		resp := h3.executeServerGroup(args, []string{"remove", "nonexistent"})
		assert.Contains(t, resp.Text, "no registered Matrix server matches")
	})

	t.Run("remove reports not found when the server vanishes after resolution", func(t *testing.T) {
		seeded := kvstore.ServerConfig{ServerID: "server-gone-test", ServerName: "gone.example.com", ServerURL: "https://gone.example.com", Enabled: true, SiteURL: "https://gone.example.com"}
		h3, mp3, api3 := newTestHandler(t, seeded)
		api3.On("HasPermissionTo", userID, model.PermissionManageSystem).Return(true)

		// Resolving the name and removing the entry are two separate registry reads, so
		// a concurrent removal in between is reachable in production. Empty the registry
		// once the first read - the one ResolveIdentifier does - has been served, so
		// Remove's own read finds nothing and reports not-found instead of claiming a
		// removal.
		store := mp3.kvstore.(*memoryKVStore)
		store.onGet = func(key string) {
			if key == kvstore.KeyServersConfig {
				store.onGet = nil
				delete(store.data, key)
			}
		}

		resp := h3.executeServerGroup(args, []string{"remove", "gone.example.com"})
		assert.Contains(t, resp.Text, "No server found")
	})

	t.Run("remove happy path prints the recovery key", func(t *testing.T) {
		seeded := kvstore.ServerConfig{ServerID: "server-remove-test", ServerName: "remove.example.com", ServerURL: "https://remove.example.com", Enabled: true, SiteURL: "https://remove.example.com"}
		h, _, api := newTestHandler(t, seeded)
		api.On("HasPermissionTo", userID, model.PermissionManageSystem).Return(true)
		serverID := seeded.ServerID
		resp := h.executeServerGroup(args, []string{"remove", serverID})
		assert.Contains(t, resp.Text, serverID)
		assert.Contains(t, resp.Text, "--server-id")
	})
}

func TestExecuteServerGroupEnableDisable(t *testing.T) {
	serverA := kvstore.ServerConfig{ServerID: "serverA", ServerName: "a.example.com", Enabled: false}
	h, _, api := newTestHandler(t, serverA)

	userID := model.NewId()
	api.On("HasPermissionTo", userID, model.PermissionManageSystem).Return(true)
	args := &model.CommandArgs{UserId: userID}

	resp := h.executeServerGroup(args, []string{"enable", "serverA"})
	assert.Contains(t, resp.Text, "enabled")

	resp = h.executeServerGroup(args, []string{"disable", "serverA"})
	assert.Contains(t, resp.Text, "disabled")

	resp = h.executeServerGroup(args, []string{"enable"})
	assert.Contains(t, resp.Text, "Usage")
}

// The subcommands taking a required server identifier must accept every form
// Servers().ResolveIdentifier advertises - the server ID, the server name, and the URL
// host - not just the canonical 26-character ID the registry itself keys entries by.
func TestExecuteServerGroupResolvesServerIdentifierForms(t *testing.T) {
	// Name and URL host deliberately differ from the ID and from each other, so a test
	// passing for one form cannot be passing by accident for another.
	const (
		serverID   = "serveridentifier1"
		serverName = "friendly-name"
		urlHost    = "host.example.com"
	)
	seeded := func() kvstore.ServerConfig {
		return kvstore.ServerConfig{
			ServerID:   serverID,
			ServerName: serverName,
			ServerURL:  "https://" + urlHost,
			Endpoint:   urlHost + ":443",
			SiteURL:    "https://" + urlHost + ":443",
			Enabled:    false,
		}
	}

	// listServers reads back through the same service the handler mutates, so these
	// assertions check what was actually persisted rather than a mock's bookkeeping.
	listServers := func(t *testing.T, mp *mockPlugin) []kvstore.ServerConfig {
		t.Helper()
		list, err := mp.serverSvc.List()
		require.NoError(t, err)
		return list
	}

	userID := model.NewId()
	args := &model.CommandArgs{UserId: userID}

	identifiers := []struct {
		name string
		arg  string
	}{
		{"server ID", serverID},
		{"server name", serverName},
		{"URL host", urlHost},
	}

	for _, id := range identifiers {
		t.Run("remove by "+id.name, func(t *testing.T) {
			h, mp, api := newTestHandler(t, seeded())
			api.On("HasPermissionTo", userID, model.PermissionManageSystem).Return(true)

			resp := h.executeServerGroup(args, []string{"remove", id.arg})
			assert.Contains(t, resp.Text, "Server removed")
			assert.Empty(t, listServers(t, mp))
		})

		t.Run("enable by "+id.name, func(t *testing.T) {
			h, mp, api := newTestHandler(t, seeded())
			api.On("HasPermissionTo", userID, model.PermissionManageSystem).Return(true)

			resp := h.executeServerGroup(args, []string{"enable", id.arg})
			assert.Contains(t, resp.Text, "enabled")
			list := listServers(t, mp)
			require.Len(t, list, 1)
			assert.True(t, list[0].Enabled)
		})

		t.Run("disable by "+id.name, func(t *testing.T) {
			server := seeded()
			server.Enabled = true
			h, mp, api := newTestHandler(t, server)
			api.On("HasPermissionTo", userID, model.PermissionManageSystem).Return(true)

			resp := h.executeServerGroup(args, []string{"disable", id.arg})
			assert.Contains(t, resp.Text, "disabled")
			list := listServers(t, mp)
			require.Len(t, list, 1)
			assert.False(t, list[0].Enabled)
		})
	}

	for _, sub := range []string{"remove", "enable", "disable"} {
		t.Run(sub+" of an unknown identifier errors", func(t *testing.T) {
			h, mp, api := newTestHandler(t, seeded())
			api.On("HasPermissionTo", userID, model.PermissionManageSystem).Return(true)

			resp := h.executeServerGroup(args, []string{sub, "nonexistent"})
			assert.Contains(t, resp.Text, "no registered Matrix server matches")
			list := listServers(t, mp)
			require.Len(t, list, 1)
			assert.False(t, list[0].Enabled)
		})
	}
}

func TestExecuteServerGroupUnknownSubcommand(t *testing.T) {
	h, _, api := newTestHandler(t)
	userID := model.NewId()
	api.On("HasPermissionTo", userID, model.PermissionManageSystem).Return(true)

	resp := h.executeServerGroup(&model.CommandArgs{UserId: userID}, []string{"bogus"})
	assert.Contains(t, resp.Text, "Usage")
}

// TestExecuteMigrateCommand covers the guards that run before the version marker is
// reset. Each must fail closed: resetting kv_store_version to "0" and then not
// completing a migration leaves the marker at 0, and the next activation re-runs
// v1/v2/v3 unattended against an already-namespaced store.
func TestExecuteMigrateCommand(t *testing.T) {
	serverA := kvstore.ServerConfig{ServerID: "serverA"}
	serverB := kvstore.ServerConfig{ServerID: "serverB"}

	t.Run("refuses with multiple servers", func(t *testing.T) {
		h, _, _ := newTestHandler(t, serverA, serverB)
		store := h.kvstore.(*memoryKVStore)
		require.NoError(t, store.Set(kvstore.KeyStoreVersion, []byte("3")))
		store.setKeys = nil

		resp := h.executeMigrateCommand(&model.CommandArgs{})

		assert.Contains(t, resp.Text, "refuses to run")
		assert.Empty(t, store.setKeys, "must not write the version marker after refusing")
		assert.Equal(t, []byte("3"), store.data[kvstore.KeyStoreVersion])
	})

	t.Run("fails closed when the server list cannot be read", func(t *testing.T) {
		h, _, _ := newTestHandler(t)
		store := h.kvstore.(*memoryKVStore)
		require.NoError(t, store.Set(kvstore.KeyStoreVersion, []byte("3")))
		store.getErr[kvstore.KeyServersConfig] = assert.AnError
		store.setKeys = nil

		resp := h.executeMigrateCommand(&model.CommandArgs{})

		assert.Contains(t, resp.Text, "Failed to load Matrix servers")
		assert.Empty(t, store.setKeys, "a KV failure must not skip the multi-server check and reset the marker")
		assert.Equal(t, []byte("3"), store.data[kvstore.KeyStoreVersion])
	})

	t.Run("fails closed when the version marker cannot be read", func(t *testing.T) {
		h, _, _ := newTestHandler(t, serverA)
		store := h.kvstore.(*memoryKVStore)
		require.NoError(t, store.Set(kvstore.KeyStoreVersion, []byte("3")))
		store.setKeys = nil
		store.getErr[kvstore.KeyStoreVersion] = assert.AnError

		resp := h.executeMigrateCommand(&model.CommandArgs{})

		assert.Contains(t, resp.Text, "Failed to read migration version")
		assert.Empty(t, store.setKeys, "must not reset a marker it could not read")
	})

	t.Run("reports migration failure after the reset", func(t *testing.T) {
		h, mp, _ := newTestHandler(t, serverA)
		mp.migrationErr = assert.AnError
		store := h.kvstore.(*memoryKVStore)
		require.NoError(t, store.Set(kvstore.KeyStoreVersion, []byte("3")))

		resp := h.executeMigrateCommand(&model.CommandArgs{})

		assert.Contains(t, resp.Text, "Migration failed")
		assert.Equal(t, []byte("0"), store.data[kvstore.KeyStoreVersion])
	})

	t.Run("runs and reports the previous version on success", func(t *testing.T) {
		h, mp, _ := newTestHandler(t, serverA)
		mp.migrationResult = &MigrationResult{UserMappingsCreated: 2}
		store := h.kvstore.(*memoryKVStore)
		require.NoError(t, store.Set(kvstore.KeyStoreVersion, []byte("2")))

		resp := h.executeMigrateCommand(&model.CommandArgs{})

		assert.Contains(t, resp.Text, "Migration completed successfully")
		assert.Contains(t, resp.Text, "Reset version: 2")
		assert.Contains(t, resp.Text, "User reverse mappings created/updated: 2")
	})
}

// TestExecuteMatrixCommandAdminGate exercises the dispatcher-level admin gate in
// executeMatrixCommand: every subcommand - including status and an unrecognized one -
// must be rejected for a non-admin caller before any handler logic runs.
func TestExecuteMatrixCommandAdminGate(t *testing.T) {
	tests := []struct {
		name    string
		command string
	}{
		{"test", "/matrix test"},
		{"create", "/matrix create"},
		{"map", "/matrix map #room:server.com"},
		{"unmap", "/matrix unmap"},
		{"list", "/matrix list"},
		{"status", "/matrix status"},
		{"migrate", "/matrix migrate"},
		{"server", "/matrix server list"},
		{"unknown subcommand", "/matrix bogus"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, _, api := newTestHandler(t)
			userID := model.NewId()
			api.On("HasPermissionTo", userID, model.PermissionManageSystem).Return(false)

			resp := h.executeMatrixCommand(&model.CommandArgs{UserId: userID, Command: tt.command})
			assert.Contains(t, resp.Text, "System Admin")
			// Exactly one permission check, and nothing beyond it: if the guard didn't
			// short-circuit before the leaf handler, that handler would go on to call
			// other plugintest.API methods, which panic here because they have no
			// stubbed expectations.
			api.AssertNumberOfCalls(t, "HasPermissionTo", 1)
		})
	}
}

// TestExecuteMatrixCommandStatusReachableByAdmin confirms that gating status did not
// break it: an admin still clears the guard and gets the real status report back.
func TestExecuteMatrixCommandStatusReachableByAdmin(t *testing.T) {
	serverA := kvstore.ServerConfig{ServerID: "serverA", ServerName: "a.example.com", ServerURL: "https://a.example.com", Enabled: true}
	h, _, api := newTestHandler(t, serverA)
	userID := model.NewId()
	api.On("HasPermissionTo", userID, model.PermissionManageSystem).Return(true)

	resp := h.executeMatrixCommand(&model.CommandArgs{UserId: userID, Command: "/matrix status"})
	assert.NotContains(t, resp.Text, "System Admin to use")
	assert.Contains(t, resp.Text, "Matrix Bridge Status")
}

// TestExecuteMatrixCommandAdminPassesGate confirms an admin caller clears the gate and
// reaches the real subcommand handler (as opposed to getting the admin-required
// response).
func TestExecuteMatrixCommandAdminPassesGate(t *testing.T) {
	h, _, api := newTestHandler(t)
	userID := model.NewId()
	api.On("HasPermissionTo", userID, model.PermissionManageSystem).Return(true)

	resp := h.executeMatrixCommand(&model.CommandArgs{UserId: userID, Command: "/matrix list"})
	assert.NotContains(t, resp.Text, "System Admin")
	assert.Contains(t, resp.Text, "No channel mappings found")
}

func TestSanitizeShareName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"simple name", "General", "general"},
		{"name with spaces", "My Cool Channel", "my-cool-channel"},
		{"name with special chars", "Test!@#Channel", "testchannel"},
		{"empty after sanitization", "!@#$%", "matrixbridge"},
		{"leading/trailing hyphens removed", "-test-", "test"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := sanitizeShareName(tt.input)
			assert.Equal(t, tt.expected, result)
			// A ShareName must start and end with alphanumeric.
			assert.False(t, strings.HasPrefix(result, "-") || strings.HasPrefix(result, "_"))
			assert.False(t, strings.HasSuffix(result, "-") || strings.HasSuffix(result, "_"))
		})
	}
}

// TestExecuteServerRegistrationURL pins the registration `url` to the plugin's base path.
// The homeserver appends "/_matrix/app/v1/transactions/{txnId}" itself, so a url that
// already carries "/_matrix/app/v1" yields a doubled path that matches no route in
// server/api.go and silently breaks every inbound transaction for that server.
func TestExecuteServerRegistrationURL(t *testing.T) {
	server := kvstore.ServerConfig{
		ServerID:   "serverAserverAserverAserv1",
		ServerName: "a.example.com",
		ServerURL:  "https://a.example.com",
		ASToken:    "as_token_value",
		HSToken:    "hs_token_value",
	}

	for _, tc := range []struct {
		name    string
		siteURL string
	}{
		{"plain site url", "https://mm.example.com"},
		{"site url with trailing slash", "https://mm.example.com/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, mp, _ := newTestHandler(t, server)
			mp.host.siteURL = tc.siteURL

			resp := h.executeServerRegistrationCommand(server.ServerID)

			assert.Contains(t, resp.Text, "url: https://mm.example.com/plugins/com.mattermost.plugin-matrix-bridge\n")
			assert.NotContains(t, resp.Text, "_matrix/app/v1")
			assert.NotContains(t, resp.Text, "//plugins/")
			assert.Contains(t, resp.Text, "as_token: as_token_value")
			assert.Contains(t, resp.Text, "hs_token: hs_token_value")
		})
	}
}

// TestExecuteServerTest covers /matrix server test, which is the only way to reach the
// Application Service permission diagnostic once two or more servers are registered:
// /matrix test resolves the sole server and refuses when there are several.
func TestExecuteServerTest(t *testing.T) {
	userID := model.NewId()
	args := &model.CommandArgs{UserId: userID}

	serverA := kvstore.ServerConfig{ServerID: "serverAserverAserverAserv1", ServerName: "a.example.com", ServerURL: "https://a.example.com", Enabled: true}
	serverB := kvstore.ServerConfig{ServerID: "serverBserverBserverBserv2", ServerName: "b.example.com", ServerURL: "https://b.example.com", Enabled: true}

	t.Run("targets a named server when several are registered", func(t *testing.T) {
		h, _, api := newTestHandler(t, serverA, serverB)
		api.On("HasPermissionTo", userID, model.PermissionManageSystem).Return(true)

		resp := h.executeServerGroup(args, []string{"test", "b.example.com"})

		// mockPlugin has no Matrix client, so the run stops at that check - which still
		// proves the right server was resolved and reached testServerConnection.
		assert.Contains(t, resp.Text, serverB.ServerURL)
		assert.NotContains(t, resp.Text, serverA.ServerURL)
		assert.Contains(t, resp.Text, "Matrix Client")
	})

	t.Run("bare arg resolves the only server", func(t *testing.T) {
		h, _, api := newTestHandler(t, serverA)
		api.On("HasPermissionTo", userID, model.PermissionManageSystem).Return(true)

		resp := h.executeServerGroup(args, []string{"test"})
		assert.Contains(t, resp.Text, serverA.ServerURL)
	})

	t.Run("bare arg is ambiguous with several servers", func(t *testing.T) {
		h, _, api := newTestHandler(t, serverA, serverB)
		api.On("HasPermissionTo", userID, model.PermissionManageSystem).Return(true)

		resp := h.executeServerGroup(args, []string{"test"})
		assert.Contains(t, resp.Text, "multiple Matrix servers")
	})

	t.Run("unknown server errors", func(t *testing.T) {
		h, _, api := newTestHandler(t, serverA, serverB)
		api.On("HasPermissionTo", userID, model.PermissionManageSystem).Return(true)

		resp := h.executeServerGroup(args, []string{"test", "nope.example.com"})
		assert.Contains(t, resp.Text, "no registered Matrix server matches")
	})
}

// TestOptionalServerIDArgRejectsExtraPositionals covers the four /matrix server
// subcommands that take an optional server identifier: a stray extra word must be a
// usage error rather than silently retargeting the command at the first argument.
func TestOptionalServerIDArgRejectsExtraPositionals(t *testing.T) {
	userID := model.NewId()
	args := &model.CommandArgs{UserId: userID}
	serverA := kvstore.ServerConfig{ServerID: "serverAserverAserverAserv1", ServerName: "a.example.com", ServerURL: "https://a.example.com", Enabled: true}

	for _, sub := range []string{"unmap", "registration", "status", "test"} {
		t.Run(sub+" rejects two positionals", func(t *testing.T) {
			h, _, api := newTestHandler(t, serverA)
			api.On("HasPermissionTo", userID, model.PermissionManageSystem).Return(true)

			resp := h.executeServerGroup(args, []string{sub, "a.example.com", "stray"})
			assert.Contains(t, resp.Text, "Usage: /matrix server "+sub+" [server_id]")
		})
	}

	t.Run("zero and one positional still work", func(t *testing.T) {
		got, errResp := optionalServerIDArg(nil, "usage")
		assert.Nil(t, errResp)
		assert.Empty(t, got)

		got, errResp = optionalServerIDArg([]string{"serverA"}, "usage")
		assert.Nil(t, errResp)
		assert.Equal(t, "serverA", got)
	})
}

func TestServerAutocompleteURL(t *testing.T) {
	assert.Equal(t,
		"/plugins/com.mattermost.plugin-matrix-bridge/api/v1/autocomplete/servers",
		ServerAutocompleteURL("com.mattermost.plugin-matrix-bridge"))
}

// TestExecuteServerMapDispatch covers /matrix server map's positional grammar:
// [server_id] <room_alias|room_id>, strict at one or two arguments.
func TestExecuteServerMapDispatch(t *testing.T) {
	userID := model.NewId()
	args := &model.CommandArgs{UserId: userID, ChannelId: model.NewId()}
	serverA := kvstore.ServerConfig{ServerID: "serverAserverAserverAserv1", ServerName: "a.example.com", ServerURL: "https://a.example.com", Enabled: true}
	serverB := kvstore.ServerConfig{ServerID: "serverBserverBserverBserv2", ServerName: "b.example.com", ServerURL: "https://b.example.com", Enabled: true}

	t.Run("server id then room resolves the named server", func(t *testing.T) {
		h, _, api := newTestHandler(t, serverA, serverB)
		api.On("HasPermissionTo", userID, model.PermissionManageSystem).Return(true)

		// mockPlugin has no Matrix client, so mapChannelCore stops at that check - which
		// still proves the arguments resolved to a registered server.
		resp := h.executeServerGroup(args, []string{"map", serverB.ServerID, "#room:b.example.com"})
		assert.Equal(t, matrixClientNotConfigured, resp.Text)
	})

	t.Run("room only resolves the sole server", func(t *testing.T) {
		h, _, api := newTestHandler(t, serverA)
		api.On("HasPermissionTo", userID, model.PermissionManageSystem).Return(true)

		resp := h.executeServerGroup(args, []string{"map", "#room:a.example.com"})
		assert.Equal(t, matrixClientNotConfigured, resp.Text)
	})

	t.Run("room only is ambiguous with several servers", func(t *testing.T) {
		h, _, api := newTestHandler(t, serverA, serverB)
		api.On("HasPermissionTo", userID, model.PermissionManageSystem).Return(true)

		resp := h.executeServerGroup(args, []string{"map", "#room:a.example.com"})
		assert.Contains(t, resp.Text, "multiple Matrix servers are registered")
	})

	for _, tc := range []struct {
		name string
		rest []string
	}{
		{"no arguments", []string{}},
		{"three arguments", []string{serverA.ServerID, "#room:a.example.com", "stray"}},
	} {
		t.Run(tc.name+" is a usage error", func(t *testing.T) {
			h, _, api := newTestHandler(t, serverA)
			api.On("HasPermissionTo", userID, model.PermissionManageSystem).Return(true)

			resp := h.executeServerGroup(args, append([]string{"map"}, tc.rest...))
			assert.Contains(t, resp.Text, "Usage: /matrix server map [server_id] <room_alias|room_id>")
		})
	}
}
