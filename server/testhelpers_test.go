package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/matrix"
	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
	matrixtest "github.com/mattermost/mattermost-plugin-matrix-bridge/testcontainers/matrix"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/mock"
)

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
	return matrix.NewClientWithLoggerAndRateLimit(serverURL, asToken, remoteID, testLogger, matrix.TestRateLimitConfig())
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

	plugin := &Plugin{remoteID: "test-remote-id"}
	plugin.SetAPI(api)

	// Initialize kvstore with in-memory implementation for testing
	plugin.kvstore = NewMemoryKVStore()

	// Initialize required plugin components
	plugin.pendingFiles = NewPendingFileTracker()
	plugin.postTracker = NewPostTracker(DefaultPostTrackerMaxEntries)

	// Reuse the container's Matrix client to share rate limiting state
	// This prevents rate limit conflicts between container setup and plugin operations
	plugin.matrixClient = matrixContainer.Client

	config := &configuration{
		MatrixServerURL: matrixContainer.ServerURL,
		MatrixASToken:   matrixContainer.ASToken,
		MatrixHSToken:   matrixContainer.HSToken,
	}
	plugin.configuration = config

	// Set up basic mocks
	setupBasicMocks(api, testUserID)

	// Set up test data in KV store
	setupTestKVData(plugin.kvstore, testChannelID, testRoomID)

	// Initialize the logger with test implementation
	plugin.logger = &testLogger{t: t}

	// Initialize bridges for testing
	plugin.initBridges()

	return &TestSetup{
		Plugin:      plugin,
		ChannelID:   testChannelID,
		UserID:      testUserID,
		RoomID:      testRoomID,
		GhostUserID: testGhostUserID,
		API:         api,
	}
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
func setupTestKVData(kvstore kvstore.KVStore, testChannelID, testRoomID string) {
	// Set up channel mapping
	_ = kvstore.Set("channel_mapping_"+testChannelID, []byte(testRoomID))

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

// Get retrieves a value from the KV store by key.
func (m *MemoryKVStore) Get(key string) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if data, exists := m.data[key]; exists {
		// Return a copy to prevent external modification
		result := make([]byte, len(data))
		copy(result, data)
		return result, nil
	}
	return nil, errors.New("key not found")
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

	end := start + perPage
	if end > len(keys) {
		end = len(keys)
	}

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

	end := start + perPage
	if end > len(keys) {
		end = len(keys)
	}

	return keys[start:end], nil
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

	// Test Get non-existent key
	_, err = store.Get("non-existent")
	if err == nil {
		t.Error("Expected error for non-existent key")
	}

	// Test Delete
	err = store.Delete("test-key")
	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}

	_, err = store.Get("test-key")
	if err == nil {
		t.Error("Expected error for deleted key")
	}
}

// generateUniqueRoomName creates a unique room name to avoid alias conflicts
func generateUniqueRoomName(baseName string) string {
	return fmt.Sprintf("%s %s", baseName, model.NewId()[:8])
}

// TestMain provides global test setup and cleanup
func TestMain(m *testing.M) {
	// Run tests
	code := m.Run()

	// Ensure all Matrix containers are cleaned up
	matrixtest.CleanupAllContainers()

	os.Exit(code)
}
