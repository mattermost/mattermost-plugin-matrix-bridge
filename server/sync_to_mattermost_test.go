package main

import (
	"testing"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
	"github.com/mattermost/mattermost/server/public/pluginapi"
	"github.com/stretchr/testify/assert"
)

// setupGetPostIDTest creates a test environment for getPostIDFromMatrixEvent tests
func setupGetPostIDTest(t *testing.T) (*MatrixToMattermostBridge, kvstore.KVStore) {
	// Setup plugin with in-memory KV store to avoid mock complexity
	plugin := setupPluginForTest()
	plugin.client = pluginapi.NewClient(plugin.API, nil)
	plugin.kvstore = NewMemoryKVStore()
	plugin.matrixClient = createMatrixClientWithTestLogger(t, "", "", "")
	plugin.initBridges()

	return plugin.matrixToMattermostBridge, plugin.kvstore
}

// TestGetPostIDFromMatrixEvent_KVStorePath tests the optimized KV store path
func TestGetPostIDFromMatrixEvent_KVStorePath(t *testing.T) {
	bridge, store := setupGetPostIDTest(t)

	// Test cases for KV store path
	testCases := []struct {
		name           string
		eventID        string
		storedPostID   string
		channelID      string
		expectedResult string
		shouldStore    bool
		description    string
	}{
		{
			name:           "KV store hit returns post ID",
			eventID:        "$matrix_event_123",
			storedPostID:   "post_456",
			channelID:      "channel_123",
			expectedResult: "post_456",
			shouldStore:    true,
			description:    "Should return post ID from KV store without Matrix API call",
		},
		{
			name:           "KV store miss falls back to Matrix API",
			eventID:        "$mattermost_event_456",
			storedPostID:   "",
			channelID:      "channel_123",
			expectedResult: "",
			shouldStore:    false,
			description:    "Should fall back to Matrix API when no KV store mapping",
		},
		{
			name:           "Empty post ID value falls back",
			eventID:        "$matrix_event_empty",
			storedPostID:   "",
			channelID:      "channel_123",
			expectedResult: "",
			shouldStore:    true,
			description:    "Should fall back to Matrix API when KV store has empty value",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Setup: Store mapping if needed
			if tc.shouldStore {
				mappingKey := kvstore.BuildMatrixEventPostKey(tc.eventID)
				err := store.Set(mappingKey, []byte(tc.storedPostID))
				assert.NoError(t, err)
			}

			// When: Call getPostIDFromMatrixEvent
			result := bridge.getPostIDFromMatrixEvent(tc.eventID, tc.channelID)

			// Then: Verify result
			assert.Equal(t, tc.expectedResult, result, tc.description)
		})
	}
}

// TestGetPostIDFromMatrixEvent_MixedEventTypes tests both paths work together
func TestGetPostIDFromMatrixEvent_MixedEventTypes(t *testing.T) {
	bridge, store := setupGetPostIDTest(t)

	// Test: Matrix-originated event (should use KV store)
	matrixEventID := "$matrix_originated_event"
	matrixPostID := "post_matrix_123"
	mappingKey := kvstore.BuildMatrixEventPostKey(matrixEventID)

	err := store.Set(mappingKey, []byte(matrixPostID))
	assert.NoError(t, err)

	result1 := bridge.getPostIDFromMatrixEvent(matrixEventID, "channel_123")
	assert.Equal(t, matrixPostID, result1, "Matrix-originated event should use KV store")

	// Test: Mattermost-originated event (should use Matrix API fallback)
	mattermostEventID := "$mattermost_originated_event"

	// Verify no KV mapping exists
	mappingKey2 := kvstore.BuildMatrixEventPostKey(mattermostEventID)
	_, err = store.Get(mappingKey2)
	assert.Error(t, err, "Should not have KV mapping for Mattermost event")

	result2 := bridge.getPostIDFromMatrixEvent(mattermostEventID, "channel_123")
	assert.Equal(t, "", result2, "Mattermost-originated event should fall back to Matrix API")
}

// TestGetPostIDFromMatrixEvent_KVStoreUpdates tests KV store updates
func TestGetPostIDFromMatrixEvent_KVStoreUpdates(t *testing.T) {
	bridge, store := setupGetPostIDTest(t)

	eventID := "$matrix_event_update"
	channelID := "channel_123"
	mappingKey := kvstore.BuildMatrixEventPostKey(eventID)

	// Initially no mapping
	result1 := bridge.getPostIDFromMatrixEvent(eventID, channelID)
	assert.Equal(t, "", result1, "Should be empty when no mapping exists")

	// Add mapping
	postID := "post_new_123"
	err := store.Set(mappingKey, []byte(postID))
	assert.NoError(t, err)

	// Now should find it
	result2 := bridge.getPostIDFromMatrixEvent(eventID, channelID)
	assert.Equal(t, postID, result2, "Should return post ID from KV store")

	// Update mapping
	newPostID := "post_updated_456"
	err = store.Set(mappingKey, []byte(newPostID))
	assert.NoError(t, err)

	// Should return updated value
	result3 := bridge.getPostIDFromMatrixEvent(eventID, channelID)
	assert.Equal(t, newPostID, result3, "Should return updated post ID")
}

// TestGetPostIDFromMatrixEvent_EdgeCases tests edge cases
func TestGetPostIDFromMatrixEvent_EdgeCases(t *testing.T) {
	bridge, store := setupGetPostIDTest(t)

	// Test empty event ID
	result1 := bridge.getPostIDFromMatrixEvent("", "channel_123")
	assert.Equal(t, "", result1, "Empty event ID should return empty")

	// Test empty channel ID with KV store mapping
	eventID := "$event_with_empty_channel"
	expectedPostID := "post_123"

	mappingKey := kvstore.BuildMatrixEventPostKey(eventID)
	err := store.Set(mappingKey, []byte(expectedPostID))
	assert.NoError(t, err)

	result2 := bridge.getPostIDFromMatrixEvent(eventID, "")
	assert.Equal(t, expectedPostID, result2, "Empty channel ID should still work with KV store")
}

// TestGetPostIDFromMatrixEvent_MatrixAPIFallback tests the Matrix API fallback path
func TestGetPostIDFromMatrixEvent_MatrixAPIFallback(t *testing.T) {
	bridge, store := setupGetPostIDTest(t)

	// Test with no KV store mapping (should fall back to Matrix API)
	eventID := "$mattermost_event_123"
	channelID := "channel_123"

	// Verify no KV mapping exists
	mappingKey := kvstore.BuildMatrixEventPostKey(eventID)
	_, err := store.Get(mappingKey)
	assert.Error(t, err, "Should not have KV store mapping")

	// Call function - should fall back to Matrix API and return empty (since we don't have a real Matrix server)
	result := bridge.getPostIDFromMatrixEvent(eventID, channelID)
	assert.Equal(t, "", result, "Should return empty when Matrix API fallback fails")
}
