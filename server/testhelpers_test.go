package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/matrix"
	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/servers"
	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
	matrixtest "github.com/mattermost/mattermost-plugin-matrix-bridge/testcontainers/matrix"
)

// addTestServer wraps plugin.servers.Add with AddServer's old positional signature,
// so the many call sites written against it don't all need to spell out an
// AddRequest literal.
func addTestServer(plugin *Plugin, serverURL, asToken, hsToken, usernamePrefix, serverID, serverNameOverride string) (string, error) {
	created, err := plugin.servers.Add(servers.AddRequest{
		ServerURL:      serverURL,
		ASToken:        asToken,
		HSToken:        hsToken,
		UsernamePrefix: usernamePrefix,
		ServerID:       serverID,
		ServerName:     serverNameOverride,
	})
	return created.ServerID, err
}

// testLogger implements Logger interface for testing
type testLogger struct {
	t *testing.T
}

func (l *testLogger) LogDebug(message string, keyValuePairs ...any) {
	if l.t != nil {
		l.t.Logf("[DEBUG] %s %v", message, keyValuePairs)
	}
}

func (l *testLogger) LogInfo(message string, keyValuePairs ...any) {
	if l.t != nil {
		l.t.Logf("[INFO] %s %v", message, keyValuePairs)
	}
}

func (l *testLogger) LogWarn(message string, keyValuePairs ...any) {
	if l.t != nil {
		l.t.Logf("[WARN] %s %v", message, keyValuePairs)
	}
}

func (l *testLogger) LogError(message string, keyValuePairs ...any) {
	if l.t != nil {
		l.t.Logf("[ERROR] %s %v", message, keyValuePairs)
	}
}

// TestSetup contains common test setup data for integration tests
type TestSetup struct {
	Plugin      *Plugin
	ServerID    string
	RemoteID    string
	ChannelID   string
	UserID      string
	RoomID      string
	GhostUserID string
	API         *plugintest.API
}

// setupPluginForTest creates a basic plugin instance with mock API for unit tests
func setupPluginForTest() *Plugin {
	api := &plugintest.API{}

	// Allow any logging calls since we're not testing logging behavior
	api.On("LogDebug", mock.Anything, mock.Anything).Maybe()
	api.On("LogDebug", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()
	api.On("LogDebug", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()

	// Migration code paths call LoadPluginConfiguration to check for legacy flat
	// configuration; default to "none configured" (a fresh install) unless a test
	// overrides this expectation.
	api.On("LoadPluginConfiguration", mock.Anything).Return(nil).Maybe()

	plugin := &Plugin{}
	plugin.SetAPI(api)
	plugin.logger = &testLogger{}
	return plugin
}

// setupPluginForTestWithLogger creates a plugin instance with test logger that logs to testing.T
func setupPluginForTestWithLogger(t *testing.T, api plugin.API) *Plugin {
	plugin := &Plugin{}
	plugin.API = api
	plugin.logger = &testLogger{t: t}
	return plugin
}

// createMatrixClientWithTestLogger creates a matrix client with test logger and rate limiting for testing
func createMatrixClientWithTestLogger(t *testing.T, serverURL, asToken, remoteID string) *matrix.Client {
	testLogger := matrix.NewTestLogger(t)
	return matrix.NewClientWithLoggerAndRateLimit(serverURL, asToken, remoteID, "", testLogger, matrix.TestRateLimitConfig())
}

// TestMatrixClientTestLogger verifies that matrix client uses test logger correctly
func TestMatrixClientTestLogger(t *testing.T) {
	// Create a matrix client with test logger
	client := createMatrixClientWithTestLogger(t, "https://test.example.com", "test_token", "test_remote")

	// This would trigger logging if the matrix client were to log something
	// Since we can't easily test actual HTTP calls without a server, this test mainly
	// verifies that the client is created correctly with a test logger
	if client == nil {
		t.Error("Matrix client should not be nil")
	}

	// Log success - this confirms the test logger interface is working
	t.Log("Matrix client created successfully with test logger")
}

// setupTestPlugin creates a test plugin instance with Matrix container for integration tests
func setupTestPlugin(t *testing.T, matrixContainer *matrixtest.Container) *TestSetup {
	api := &plugintest.API{}

	testChannelID := model.NewId()
	testUserID := model.NewId()
	testRoomID := matrixContainer.CreateRoom(t, "Test Room")
	testGhostUserID := "@_mattermost_" + testUserID + ":" + matrixContainer.ServerDomain

	serverID := model.NewId()
	remoteID := "test-remote-id"

	plugin := &Plugin{}
	plugin.SetAPI(api)

	// Initialize kvstore with in-memory implementation for testing
	plugin.kvstore = NewMemoryKVStore()
	plugin.servers = servers.New(plugin.kvstore, pluginLogger{plugin}, pluginHost{plugin})

	// Initialize required plugin components
	plugin.pendingFiles = NewPendingFileTracker()
	plugin.postTracker = NewPostTracker(DefaultPostTrackerMaxEntries)
	plugin.configuration = &configuration{}
	plugin.logger = &testLogger{t: t}

	// Register a single server backed by the container, and reuse the container's
	// Matrix client to share rate limiting state - this prevents rate limit conflicts
	// between container setup and plugin operations.
	serverConfig := kvstore.ServerConfig{
		ServerID:    serverID,
		ServerURL:   matrixContainer.ServerURL,
		Endpoint:    matrixContainer.ServerURL,
		ServerName:  matrixContainer.ServerDomain,
		EventDomain: sanitizeForEventDomain(matrixContainer.ServerDomain),
		ASToken:     matrixContainer.ASToken,
		HSToken:     matrixContainer.HSToken,
		Enabled:     true,
		RemoteID:    remoteID,
		SiteURL:     "https://" + matrixContainer.ServerDomain,
	}
	serversData, err := kvstore.MarshalServersConfig([]kvstore.ServerConfig{serverConfig})
	if err != nil {
		t.Fatalf("failed to marshal test server config: %v", err)
	}
	if err := plugin.kvstore.Set(kvstore.KeyServersConfig, serversData); err != nil {
		t.Fatalf("failed to seed test server config: %v", err)
	}

	plugin.matrixClients = map[string]*matrix.Client{serverID: matrixContainer.Client}
	plugin.remoteToServerID = map[string]string{remoteID: serverID}
	plugin.ownRemoteIDs = map[string]struct{}{remoteID: {}}
	// Populate the snapshot cache too, exactly as initMatrixClients would: bridge helpers
	// read this rather than KV (see BridgeUtils.serverConfig), so leaving it empty would
	// send every one of them down the direct-KV fallback path instead of the cached path
	// production actually takes.
	plugin.serverConfigs = map[string]kvstore.ServerConfig{serverID: serverConfig}

	// Set up basic mocks
	setupBasicMocks(api, testUserID)

	// Set up test data in KV store
	setupTestKVData(t, plugin.kvstore, serverID, testChannelID, testRoomID)

	return &TestSetup{
		Plugin:      plugin,
		ServerID:    serverID,
		RemoteID:    remoteID,
		ChannelID:   testChannelID,
		UserID:      testUserID,
		RoomID:      testRoomID,
		GhostUserID: testGhostUserID,
		API:         api,
	}
}

// sanitizeForEventDomain mirrors eventDomainFromEndpoint's sanitization, for test setup.
func sanitizeForEventDomain(s string) string {
	return strings.NewReplacer(".", "_", ":", "_").Replace(s)
}

// registerTestServer seeds a single server registry entry backed by matrixClient into
// plugin's kvstore and per-node caches, returning the minted serverID and remoteID. Most
// unit tests that need "a" Matrix server, without caring about the specifics of
// multi-server routing, should use this instead of hand-building a ServerConfig.
func registerTestServer(t *testing.T, plugin *Plugin, serverURL, serverName string, matrixClient *matrix.Client) (serverID, remoteID string) {
	t.Helper()

	serverID = model.NewId()
	remoteID = "test-remote-" + serverID[:8]

	serverConfig := kvstore.ServerConfig{
		ServerID:    serverID,
		ServerURL:   serverURL,
		Endpoint:    serverURL,
		ServerName:  serverName,
		EventDomain: sanitizeForEventDomain(serverName),
		Enabled:     true,
		RemoteID:    remoteID,
		SiteURL:     "https://" + serverName,
	}

	existing, err := plugin.servers.List()
	if err != nil {
		t.Fatalf("failed to read existing test servers: %v", err)
	}

	data, err := kvstore.MarshalServersConfig(append(existing, serverConfig))
	if err != nil {
		t.Fatalf("failed to marshal test server config: %v", err)
	}
	if err := plugin.kvstore.Set(kvstore.KeyServersConfig, data); err != nil {
		t.Fatalf("failed to seed test server config: %v", err)
	}

	if plugin.matrixClients == nil {
		plugin.matrixClients = map[string]*matrix.Client{}
	}
	if plugin.remoteToServerID == nil {
		plugin.remoteToServerID = map[string]string{}
	}
	if plugin.ownRemoteIDs == nil {
		plugin.ownRemoteIDs = map[string]struct{}{}
	}
	if plugin.serverConfigs == nil {
		plugin.serverConfigs = map[string]kvstore.ServerConfig{}
	}
	// The caller typically constructs matrixClient before this remoteID is minted (it
	// doesn't exist yet). Stamp it now so posts/users the client creates attribute to
	// the right remote, matching what initMatrixClients does in production.
	if matrixClient != nil {
		matrixClient.SetRemoteID(remoteID)
	}
	plugin.matrixClients[serverID] = matrixClient
	plugin.remoteToServerID[remoteID] = serverID
	plugin.ownRemoteIDs[remoteID] = struct{}{}
	plugin.serverConfigs[serverID] = serverConfig

	return serverID, remoteID
}

// setTestServerUsernamePrefix overwrites serverID's UsernamePrefix in both the registry
// and the serverConfigs cache, for tests that verify per-server username prefix
// configurability.
func setTestServerUsernamePrefix(t *testing.T, plugin *Plugin, serverID, prefix string) {
	t.Helper()

	servers, err := plugin.servers.List()
	require.NoError(t, err)

	found := false
	for i := range servers {
		if servers[i].ServerID == serverID {
			servers[i].UsernamePrefix = prefix
			found = true
		}
	}
	require.True(t, found, "server %s must be registered before setting its username prefix", serverID)

	data, err := kvstore.MarshalServersConfig(servers)
	require.NoError(t, err)
	require.NoError(t, plugin.kvstore.Set(kvstore.KeyServersConfig, data))

	syncTestServerConfigsCache(plugin, servers)
}

// syncTestServerConfigsCache copies servers into plugin.serverConfigs, keyed by
// ServerID - exactly what initMatrixClients would have produced from this same slice.
// Test helpers that write a modified registry directly to KV, bypassing
// SetServerEnabled/RemoveServer/AddServer (usually to avoid rebuilding matrixClients from
// scratch and clobbering a fake matrix.Client registerTestServer already wired up), must
// call this afterward so serverConfigForRouting/cachedServerConfigs - which now read this
// cache instead of KV - see the change too.
func syncTestServerConfigsCache(plugin *Plugin, servers []kvstore.ServerConfig) {
	// Replace rather than merge: initMatrixClients builds a fresh map every rebuild, so
	// merging would leave a removed server cached and no longer match what it produces.
	configs := make(map[string]kvstore.ServerConfig, len(servers))
	for _, s := range servers {
		configs[s.ServerID] = s
	}
	plugin.serverConfigs = configs
}

// setTestServerEnabled overwrites serverID's Enabled flag in both the registry and the
// serverConfigs cache that serverConfigForRouting reads on hot paths. Deliberately does
// not go through SetServerEnabled/refreshServersAndBroadcast/initMatrixClients - that
// would rebuild matrixClients from each entry's ASToken/ServerURL fields and silently
// replace any fake matrix.Client a test wired up via registerTestServer with a
// non-functional one.
func setTestServerEnabled(t *testing.T, plugin *Plugin, serverID string, enabled bool) {
	t.Helper()

	servers, err := plugin.servers.List()
	require.NoError(t, err)

	found := false
	for i := range servers {
		if servers[i].ServerID == serverID {
			servers[i].Enabled = enabled
			found = true
		}
	}
	require.True(t, found, "server %s must be registered before setting its enabled flag", serverID)

	data, err := kvstore.MarshalServersConfig(servers)
	require.NoError(t, err)
	require.NoError(t, plugin.kvstore.Set(kvstore.KeyServersConfig, data))

	syncTestServerConfigsCache(plugin, servers)
}

// testBridges builds the on-demand Mattermost->Matrix and Matrix->Mattermost bridges
// for serverID, failing the test immediately if either can't be built (e.g. serverID
// wasn't registered via registerTestServer/setupTestPlugin first).
func (p *Plugin) testBridges(t *testing.T, serverID string) (*MattermostToMatrixBridge, *MatrixToMattermostBridge) {
	t.Helper()

	m2mx, err := p.newMattermostToMatrixBridge(serverID)
	require.NoError(t, err)

	mx2m, err := p.newMatrixToMattermostBridge(serverID)
	require.NoError(t, err)

	return m2mx, mx2m
}

// singleServerTestSetup holds what setupSingleServerTest/setupSingleServerIntegrationTest
// build for a single-server, container-backed test: the Plugin, the one server's IDs,
// and its two on-demand bridges. Validator is only populated by
// setupSingleServerIntegrationTest - callers that don't need it (e.g.
// dm_room_creation_test.go, which validates via the container's own room/message
// inspection rather than matrixtest.EventValidation) get it nil from setupSingleServerTest.
type singleServerTestSetup struct {
	Plugin    *Plugin
	ServerID  string
	RemoteID  string
	M2Mx      *MattermostToMatrixBridge
	Mx2M      *MatrixToMattermostBridge
	Validator *matrixtest.EventValidation
}

// setupSingleServerTest builds a fresh Plugin (kvstore/trackers/configuration/logger)
// registered with matrixContainer as its one server, plus both of that server's on-demand
// bridges. The logger is set before registerTestServer runs - registerTestServer builds a
// real matrix.Client, which logs at construction time - so this ordering constraint is
// enforced structurally here rather than left to every caller to remember.
//
// customize, if non-nil, runs after the server is registered and before the bridges are
// built, for a suite's own per-server tweaks (e.g. setTestServerUsernamePrefix). It must
// not rebuild the server's Matrix client (e.g. via SetServerEnabled), or it would silently
// replace the fake client registerTestServer just wired up.
func setupSingleServerTest(t *testing.T, api plugin.API, matrixContainer *matrixtest.Container, customize func(plugin *Plugin, serverID string)) *singleServerTestSetup {
	t.Helper()

	p := &Plugin{}
	p.SetAPI(api)
	p.kvstore = NewMemoryKVStore()
	p.servers = servers.New(p.kvstore, pluginLogger{p}, pluginHost{p})
	p.pendingFiles = NewPendingFileTracker()
	p.postTracker = NewPostTracker(DefaultPostTrackerMaxEntries)
	p.configuration = &configuration{}
	// Must exist before registerTestServer, below: it builds a real matrix.Client, which
	// logs at construction time.
	p.logger = &testLogger{t: t}

	matrixClient := createMatrixClientWithTestLogger(t, matrixContainer.ServerURL, matrixContainer.ASToken, "")
	matrixClient.SetServerDomain(matrixContainer.ServerDomain)
	serverID, remoteID := registerTestServer(t, p, matrixContainer.ServerURL, matrixContainer.ServerDomain, matrixClient)

	if customize != nil {
		customize(p, serverID)
	}

	m2mx, mx2m := p.testBridges(t, serverID)

	return &singleServerTestSetup{
		Plugin:   p,
		ServerID: serverID,
		RemoteID: remoteID,
		M2Mx:     m2mx,
		Mx2M:     mx2m,
	}
}

// setupSingleServerIntegrationTest wraps setupSingleServerTest with the
// testChannelID->testRoomID KV seeding and event validator every container-backed
// single-server suite in this package also needs, beyond the bare plugin+server+bridges
// setupSingleServerTest provides. See setupSingleServerTest for the customize parameter.
func setupSingleServerIntegrationTest(t *testing.T, api plugin.API, matrixContainer *matrixtest.Container, testChannelID, testRoomID string, customize func(plugin *Plugin, serverID string)) *singleServerTestSetup {
	t.Helper()

	setup := setupSingleServerTest(t, api, matrixContainer, customize)

	setupTestKVData(t, setup.Plugin.kvstore, setup.ServerID, testChannelID, testRoomID)

	setup.Validator = matrixtest.NewEventValidation(t, matrixContainer.ServerDomain, setup.RemoteID)

	return setup
}

// setupBasicMocks sets up common API mocks for integration tests
func setupBasicMocks(api *plugintest.API, testUserID string) {
	// Basic user mock
	testUser := &model.User{
		Id:       testUserID,
		Username: "testuser",
		Email:    "test@example.com",
		Nickname: "Test User",
	}
	api.On("GetUser", testUserID).Return(testUser, nil)
	api.On("GetUser", mock.AnythingOfType("string")).Return(&model.User{Id: "default", Username: "default"}, nil)

	// Mock profile image for ghost user creation
	api.On("GetProfileImage", testUserID).Return([]byte("fake-image-data"), nil)

	// Post update mock - return the updated post with current timestamp
	api.On("UpdatePost", mock.AnythingOfType("*model.Post")).Return(func(post *model.Post) *model.Post {
		// Simulate what Mattermost does - update the UpdateAt timestamp
		updatedPost := post.Clone() // Copy the post
		updatedPost.UpdateAt = time.Now().UnixMilli()
		return updatedPost
	}, nil)

	// Logging mocks - handle variable argument types
	api.On("LogDebug", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
	api.On("LogInfo", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
	api.On("LogWarn", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return()
}

// setupTestKVData sets up initial test data in the KV store
func setupTestKVData(t *testing.T, kv kvstore.KVStore, serverID, testChannelID, testRoomID string) {
	t.Helper()

	// Set up channel mapping
	data, err := kvstore.BuildSingleChannelMapping(serverID, testRoomID)
	require.NoError(t, err)
	require.NoError(t, kv.Set(kvstore.BuildChannelMappingKey(testChannelID), data))

	// Ghost users and ghost rooms are intentionally not set up here
	// to trigger creation during tests, which validates the creation logic
}

// setupMentionMocks sets up mocks for testing user mentions
func setupMentionMocks(api *plugintest.API, userID, username string) {
	user := &model.User{Id: userID, Username: username, Email: username + "@example.com"}
	api.On("GetUserByUsername", username).Return(user, nil)
	// Mock profile image for ghost user creation
	api.On("GetProfileImage", userID).Return([]byte("fake-image-data"), nil)
}

// clearMockExpectations clears all previous mock expectations for reuse in subtests
func clearMockExpectations(api *plugintest.API) {
	api.ExpectedCalls = nil
}

// Helper function to compare file attachment arrays (moved from sync_to_matrix_test.go)
func compareFileAttachmentArrays(currentFiles, newFiles []matrix.FileAttachment) bool {
	if len(currentFiles) != len(newFiles) {
		return false
	}

	for i, newFile := range newFiles {
		if i >= len(currentFiles) {
			return false
		}

		currentFile := currentFiles[i]
		if currentFile.Filename != newFile.Filename ||
			currentFile.MxcURI != newFile.MxcURI ||
			currentFile.MimeType != newFile.MimeType ||
			currentFile.Size != newFile.Size {
			return false
		}
	}

	return true
}

// MemoryKVStore provides an in-memory implementation of the KVStore interface for testing.
type MemoryKVStore struct {
	data map[string][]byte
	mu   sync.RWMutex
}

// NewMemoryKVStore creates a new in-memory KV store for testing.
func NewMemoryKVStore() kvstore.KVStore {
	return &MemoryKVStore{
		data: make(map[string][]byte),
	}
}

// GetTemplateData retrieves template data for a specific user from the KV store.
func (m *MemoryKVStore) GetTemplateData(userID string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	key := "template_key-" + userID
	if data, exists := m.data[key]; exists {
		return string(data), nil
	}
	return "", errors.New("key not found")
}

// Get retrieves a value from the KV store by key. Matching production semantics
// (pluginapi.KVService.Get), a missing key returns (nil, nil), not an error - callers
// throughout the plugin (e.g. Plugin.getServers) depend on that distinction.
func (m *MemoryKVStore) Get(key string) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if data, exists := m.data[key]; exists {
		// Return a copy to prevent external modification
		result := make([]byte, len(data))
		copy(result, data)
		return result, nil
	}
	return nil, nil
}

// Set stores a key-value pair in the KV store.
func (m *MemoryKVStore) Set(key string, value []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Store a copy to prevent external modification
	data := make([]byte, len(value))
	copy(data, value)
	m.data[key] = data
	return nil
}

// Delete removes a key-value pair from the KV store.
func (m *MemoryKVStore) Delete(key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.data, key)
	return nil
}

// ListKeys retrieves a paginated list of keys from the KV store.
func (m *MemoryKVStore) ListKeys(page, perPage int) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// Collect all keys
	keys := make([]string, 0, len(m.data))
	for key := range m.data {
		keys = append(keys, key)
	}

	// Sort keys for consistent ordering
	sort.Strings(keys)

	// Apply pagination
	start := page * perPage
	if start >= len(keys) {
		return []string{}, nil
	}

	end := min(start+perPage, len(keys))

	return keys[start:end], nil
}

// ListKeysWithPrefix retrieves a paginated list of keys with a specific prefix from the KV store.
func (m *MemoryKVStore) ListKeysWithPrefix(page, perPage int, prefix string) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// Collect keys with the specified prefix
	keys := make([]string, 0, len(m.data))
	for key := range m.data {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}

	// Sort keys for consistent ordering
	sort.Strings(keys)

	// Apply pagination
	start := page * perPage
	if start >= len(keys) {
		return []string{}, nil
	}

	end := min(start+perPage, len(keys))

	return keys[start:end], nil
}

// SetAtomicWithRetries sets key atomically. The in-memory store has no concurrent
// writers to race against in tests, so this simply reads, computes, and writes once.
func (m *MemoryKVStore) SetAtomicWithRetries(key string, valueFunc func(oldValue []byte) (newValue []byte, err error)) error {
	oldValue, err := m.Get(key)
	if err != nil {
		return err
	}

	newValue, err := valueFunc(oldValue)
	if err != nil {
		return err
	}

	return m.Set(key, newValue)
}

// Clear removes all data from the store (useful for test cleanup).
func (m *MemoryKVStore) Clear() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.data = make(map[string][]byte)
}

// Size returns the number of key-value pairs in the store.
func (m *MemoryKVStore) Size() int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return len(m.data)
}

// casConflictKVStore wraps a MemoryKVStore and gives SetAtomicWithRetries real
// compare-and-set retry semantics: it tracks a version per key and re-invokes valueFunc
// against a fresh read whenever the version changes between this call's read and write.
// MemoryKVStore's own SetAtomicWithRetries is a single-shot read-compute-write with no
// such check, so it cannot model a real conflict; this type exists so tests can install
// onFirstRead to simulate a concurrent writer winning the race for one call, forcing a
// real retry - the regression mutateServers's CAS design exists to survive.
type casConflictKVStore struct {
	kvstore.KVStore

	mu       sync.Mutex
	versions map[string]int
	reads    map[string]int

	// onFirstRead, if set, is invoked exactly once per key, after that key's first read
	// in SetAtomicWithRetries but before this call's write - simulating a concurrent
	// writer's real Set landing in between. Receives the underlying store so it can read
	// and write like an independent caller would.
	onFirstRead func(store kvstore.KVStore)
}

func newCASConflictKVStore() *casConflictKVStore {
	return &casConflictKVStore{
		KVStore:  NewMemoryKVStore(),
		versions: make(map[string]int),
		reads:    make(map[string]int),
	}
}

func (c *casConflictKVStore) SetAtomicWithRetries(key string, valueFunc func(oldValue []byte) (newValue []byte, err error)) error {
	for {
		// The read, the (at most once) injected concurrent write, and the version it
		// establishes must all happen under one lock hold - otherwise a third caller
		// could observe the bumped version before the injected write actually lands and
		// wrongly conclude there is no conflict, the same lost-update bug this type
		// exists to let tests exercise deliberately, not suffer from itself.
		c.mu.Lock()
		oldValue, err := c.Get(key)
		if err != nil {
			c.mu.Unlock()
			return err
		}
		oldVersion := c.versions[key]
		c.reads[key]++
		if c.reads[key] == 1 && c.onFirstRead != nil {
			c.onFirstRead(c.KVStore)
			c.versions[key]++
		}
		c.mu.Unlock()

		newValue, err := valueFunc(oldValue)
		if err != nil {
			return err
		}

		c.mu.Lock()
		if c.versions[key] != oldVersion {
			// Lost the race: someone else's write landed since our read. Retry with a
			// fresh read, exactly like the real KV store's SetAtomicWithRetries would.
			c.mu.Unlock()
			continue
		}
		// The version bump must happen together with the write, under the same lock
		// hold, so no concurrent caller can pass its own version check against a bumped
		// version whose corresponding write hasn't landed yet.
		if err := c.Set(key, newValue); err != nil {
			c.mu.Unlock()
			return err
		}
		c.versions[key]++
		c.mu.Unlock()
		return nil
	}
}

// erroringKVStore wraps a KVStore and fails Get for one specific key, for tests that
// need to simulate a registry-read failure (e.g. a corrupt or unreachable backing store)
// without a full hand-written fake.
type erroringKVStore struct {
	kvstore.KVStore
	errOnGetKey string
	getErr      error
}

func (e *erroringKVStore) Get(key string) ([]byte, error) {
	if key == e.errOnGetKey {
		if e.getErr != nil {
			return nil, e.getErr
		}
		return nil, errors.New("simulated KV store read failure")
	}
	return e.KVStore.Get(key)
}

// SetAtomicWithRetries overrides the promoted embedded implementation so errOnGetKey
// also fires for mutateServers-style CAS operations: those read the current value via
// the store's own internal Get, never through this wrapper's Get override, so without
// this override a configured failure on the registry key would be silently bypassed by
// any code path that mutates it instead of just reading it.
func (e *erroringKVStore) SetAtomicWithRetries(key string, valueFunc func(oldValue []byte) ([]byte, error)) error {
	if _, err := e.Get(key); err != nil {
		return err
	}
	return e.KVStore.SetAtomicWithRetries(key, valueFunc)
}

// TestMemoryKVStore tests the in-memory KV store implementation
func TestMemoryKVStore(t *testing.T) {
	store := NewMemoryKVStore()

	// Test Set and Get
	err := store.Set("test-key", []byte("test-value"))
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}

	value, err := store.Get("test-key")
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	if string(value) != "test-value" {
		t.Errorf("Expected 'test-value', got '%s'", string(value))
	}

	// Test Get non-existent key - matches production semantics: no error, nil data.
	missing, err := store.Get("non-existent")
	if err != nil {
		t.Errorf("Expected no error for non-existent key, got %v", err)
	}
	if missing != nil {
		t.Errorf("Expected nil data for non-existent key, got %v", missing)
	}

	// Test Delete
	err = store.Delete("test-key")
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}

	deleted, err := store.Get("test-key")
	if err != nil {
		t.Errorf("Expected no error for deleted key, got %v", err)
	}
	if deleted != nil {
		t.Errorf("Expected nil data for deleted key, got %v", deleted)
	}
}

// generateUniqueRoomName creates a unique room name to avoid alias conflicts
func generateUniqueRoomName(baseName string) string {
	return fmt.Sprintf("%s %s", baseName, model.NewId()[:8])
}

// skipIfShort skips container-backed integration tests when running with
// `go test -short`. CI runs `make test` without `-short`, so these suites
// still execute there; we deliberately do NOT auto-detect Docker availability,
// since that would let CI silently go green if Docker itself broke.
func skipIfShort(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping container-backed integration test in -short mode")
	}
}

// TestMain provides global test setup and cleanup
func TestMain(m *testing.M) {
	// Run tests
	code := m.Run()

	// Ensure all Matrix containers are cleaned up
	matrixtest.CleanupAllContainers()

	os.Exit(code)
}
