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
	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
)

// mockPlugin implements PluginAccessor for testing, backed by an in-memory server list
// instead of a real KV store or Matrix client.
type mockPlugin struct {
	client    *pluginapi.Client
	kvstore   kvstore.KVStore
	pluginAPI plugin.API

	servers []kvstore.ServerConfig

	addServerErr error
	addServerID  string

	removeServerOK  bool
	removeServerErr error

	setEnabledErr error

	mapErr   error
	unmapErr error

	migrationErr    error
	migrationResult *MigrationResult
}

func (m *mockPlugin) GetKVStore() kvstore.KVStore { return m.kvstore }

func (m *mockPlugin) GetManagedServers() ([]kvstore.ServerConfig, error) {
	return m.servers, nil
}

func (m *mockPlugin) AddServer(serverURL, _, _, _, serverID, _ string) (string, error) {
	if m.addServerErr != nil {
		return "", m.addServerErr
	}
	id := m.addServerID
	if id == "" {
		id = serverID
	}
	if id == "" {
		id = model.NewId()
	}
	m.servers = append(m.servers, kvstore.ServerConfig{
		ServerID:   id,
		ServerURL:  serverURL,
		ServerName: serverURL,
		Enabled:    true,
	})
	return id, nil
}

func (m *mockPlugin) RemoveServer(serverID string) (bool, error) {
	if m.removeServerErr != nil {
		return false, m.removeServerErr
	}
	if !m.removeServerOK {
		return false, nil
	}
	for i, s := range m.servers {
		if s.ServerID == serverID {
			m.servers = append(m.servers[:i], m.servers[i+1:]...)
			break
		}
	}
	return true, nil
}

func (m *mockPlugin) SetServerEnabled(serverID string, enabled bool) error {
	if m.setEnabledErr != nil {
		return m.setEnabledErr
	}
	for i := range m.servers {
		if m.servers[i].ServerID == serverID {
			m.servers[i].Enabled = enabled
			return nil
		}
	}
	return assert.AnError
}

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
// keyspace scans (e.g. countMappedChannelsPerServer).
type memoryKVStore struct {
	data map[string][]byte
}

func newMemoryKVStore() kvstore.KVStore {
	return &memoryKVStore{data: make(map[string][]byte)}
}

func (m *memoryKVStore) GetTemplateData(_ string) (string, error) { return "", nil }

func (m *memoryKVStore) Get(key string) ([]byte, error) {
	return m.data[key], nil
}

func (m *memoryKVStore) Set(key string, value []byte) error {
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

// newTestHandler builds a Handler with a fresh mockPlugin and mock Mattermost API,
// without going through NewCommandHandler (which registers a real slash command).
func newTestHandler(t *testing.T, servers ...kvstore.ServerConfig) (*Handler, *mockPlugin, *plugintest.API) {
	t.Helper()

	api := &plugintest.API{}
	client := pluginapi.NewClient(api, nil)
	store := newMemoryKVStore()

	mp := &mockPlugin{
		client:    client,
		pluginAPI: api,
		kvstore:   store,
		servers:   servers,
	}

	return &Handler{
		plugin:    mp,
		client:    client,
		kvstore:   store,
		pluginAPI: api,
	}, mp, api
}

func TestResolveServerIDArg(t *testing.T) {
	serverA := kvstore.ServerConfig{ServerID: "serverA", ServerName: "a.example.com", ServerURL: "https://a.example.com"}
	serverB := kvstore.ServerConfig{ServerID: "serverB", ServerName: "b.example.com", ServerURL: "https://b.example.com"}

	t.Run("empty arg with zero servers errors", func(t *testing.T) {
		h, _, _ := newTestHandler(t)
		_, err := h.resolveServerIDArg("")
		require.Error(t, err)
	})

	t.Run("empty arg with exactly one server resolves it", func(t *testing.T) {
		h, _, _ := newTestHandler(t, serverA)
		id, err := h.resolveServerIDArg("")
		require.NoError(t, err)
		assert.Equal(t, "serverA", id)
	})

	t.Run("empty arg with multiple servers is ambiguous", func(t *testing.T) {
		h, _, _ := newTestHandler(t, serverA, serverB)
		_, err := h.resolveServerIDArg("")
		require.Error(t, err)
	})

	t.Run("matches by server_id", func(t *testing.T) {
		h, _, _ := newTestHandler(t, serverA, serverB)
		id, err := h.resolveServerIDArg("serverB")
		require.NoError(t, err)
		assert.Equal(t, "serverB", id)
	})

	t.Run("matches by server name", func(t *testing.T) {
		h, _, _ := newTestHandler(t, serverA, serverB)
		id, err := h.resolveServerIDArg("a.example.com")
		require.NoError(t, err)
		assert.Equal(t, "serverA", id)
	})

	t.Run("matches by URL host", func(t *testing.T) {
		serverC := kvstore.ServerConfig{ServerID: "serverC", ServerName: "different-name.example.org", ServerURL: "https://c.example.com"}
		h, _, _ := newTestHandler(t, serverC)
		id, err := h.resolveServerIDArg("c.example.com")
		require.NoError(t, err)
		assert.Equal(t, "serverC", id)
	})

	t.Run("no match errors", func(t *testing.T) {
		h, _, _ := newTestHandler(t, serverA)
		_, err := h.resolveServerIDArg("nonexistent")
		require.Error(t, err)
	})
}

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
	h, mp, api := newTestHandler(t)

	userID := model.NewId()
	api.On("HasPermissionTo", userID, model.PermissionManageSystem).Return(true)

	args := &model.CommandArgs{UserId: userID}

	t.Run("add requires at least 3 positional args", func(t *testing.T) {
		resp := h.executeServerGroup(args, []string{"add", "https://matrix.example.com"})
		assert.Contains(t, resp.Text, "Usage")
	})

	t.Run("add happy path registers a server", func(t *testing.T) {
		resp := h.executeServerGroup(args, []string{"add", "https://matrix.example.com", "as-token", "hs-token"})
		assert.Contains(t, resp.Text, "added")
		assert.Len(t, mp.servers, 1)
	})

	t.Run("add failure surfaces the error", func(t *testing.T) {
		h2, mp2, api2 := newTestHandler(t)
		api2.On("HasPermissionTo", userID, model.PermissionManageSystem).Return(true)
		mp2.addServerErr = assert.AnError
		resp := h2.executeServerGroup(args, []string{"add", "https://matrix.example.com", "as-token", "hs-token"})
		assert.Contains(t, resp.Text, "Failed to add server")
	})

	t.Run("list shows the registered server", func(t *testing.T) {
		resp := h.executeServerGroup(args, []string{"list"})
		assert.Contains(t, resp.Text, mp.servers[0].ServerID)
	})

	t.Run("remove requires a server_id", func(t *testing.T) {
		resp := h.executeServerGroup(args, []string{"remove"})
		assert.Contains(t, resp.Text, "Usage")
	})

	t.Run("remove of unknown server reports not found", func(t *testing.T) {
		h3, mp3, api3 := newTestHandler(t)
		api3.On("HasPermissionTo", userID, model.PermissionManageSystem).Return(true)
		mp3.removeServerOK = false
		resp := h3.executeServerGroup(args, []string{"remove", "nonexistent"})
		assert.Contains(t, resp.Text, "No server found")
	})

	t.Run("remove happy path prints the recovery key", func(t *testing.T) {
		serverID := mp.servers[0].ServerID
		mp.removeServerOK = true
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

func TestExecuteServerGroupUnknownSubcommand(t *testing.T) {
	h, _, api := newTestHandler(t)
	userID := model.NewId()
	api.On("HasPermissionTo", userID, model.PermissionManageSystem).Return(true)

	resp := h.executeServerGroup(&model.CommandArgs{UserId: userID}, []string{"bogus"})
	assert.Contains(t, resp.Text, "Usage")
}

func TestExecuteMigrateCommandRefusesWithMultipleServers(t *testing.T) {
	serverA := kvstore.ServerConfig{ServerID: "serverA"}
	serverB := kvstore.ServerConfig{ServerID: "serverB"}
	h, _, _ := newTestHandler(t, serverA, serverB)

	resp := h.executeMigrateCommand(&model.CommandArgs{})
	assert.Contains(t, resp.Text, "refuses to run")
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
