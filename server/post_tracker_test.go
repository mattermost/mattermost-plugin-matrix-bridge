package main

import (
	"fmt"
	"testing"
	"time"
)

func TestPostTracker_PutAndGet(t *testing.T) {
	tracker := NewPostTracker(DefaultPostTrackerMaxEntries)

	// Test Put and Get
	postID := "test_post_123"
	updateAt := time.Now().UnixMilli()

	err := tracker.Put("srv1", postID, updateAt)
	if err != nil {
		t.Fatalf("Unexpected error from Put: %v", err)
	}

	retrievedUpdateAt, exists := tracker.Get("srv1", postID)
	if !exists {
		t.Fatalf("Expected post ID to exist in tracker")
	}

	if retrievedUpdateAt != updateAt {
		t.Fatalf("Expected updateAt %d, got %d", updateAt, retrievedUpdateAt)
	}
}

func TestPostTracker_Delete(t *testing.T) {
	tracker := NewPostTracker(DefaultPostTrackerMaxEntries)

	postID := "test_post_456"
	updateAt := time.Now().UnixMilli()

	err := tracker.Put("srv1", postID, updateAt)
	if err != nil {
		t.Fatalf("Unexpected error from Put: %v", err)
	}

	tracker.Delete("srv1", postID)

	_, exists := tracker.Get("srv1", postID)
	if exists {
		t.Fatalf("Expected post ID to be deleted from tracker")
	}
}

func TestPostTracker_MaxEntries(t *testing.T) {
	tracker := NewPostTracker(DefaultPostTrackerMaxEntries)

	// Add exactly 10,000 entries (should not trigger capacity check)
	for i := range 10000 {
		postID := fmt.Sprintf("test_post_%d", i)
		updateAt := time.Now().UnixMilli()
		err := tracker.Put("srv1", postID, updateAt)
		if err != nil {
			t.Fatalf("Unexpected error adding entry %d: %v", i, err)
		}
	}

	// Should have exactly 10,000 entries
	size := tracker.Size()
	if size != 10000 {
		t.Fatalf("Expected tracker size to be 10000, got %d", size)
	}

	// Try to add one more entry - should fail since all entries are recent
	err := tracker.Put("srv1", "should_fail", time.Now().UnixMilli())
	if err == nil {
		t.Fatalf("Expected Put to fail when at capacity with recent entries")
	}

	// Size should still be 10,000
	sizeAfterFailure := tracker.Size()
	if sizeAfterFailure != 10000 {
		t.Fatalf("Expected tracker size to remain 10000 after failed Put, got %d", sizeAfterFailure)
	}
}

func TestPostTracker_CleanupOldEntries(t *testing.T) {
	tracker := NewPostTracker(DefaultPostTrackerMaxEntries)

	// Add an old entry (older than 1 hour)
	oldPostID := "old_post"
	oldUpdateAt := time.Now().Add(-2 * time.Hour).UnixMilli()
	err := tracker.Put("srv1", oldPostID, oldUpdateAt)
	if err != nil {
		t.Fatalf("Unexpected error from Put: %v", err)
	}

	// Add a recent entry
	recentPostID := "recent_post"
	recentUpdateAt := time.Now().UnixMilli()
	err = tracker.Put("srv1", recentPostID, recentUpdateAt)
	if err != nil {
		t.Fatalf("Unexpected error from Put: %v", err)
	}

	// Trigger cleanup by doing many puts
	for i := range 100 {
		postID := fmt.Sprintf("trigger_cleanup_%d", i)
		updateAt := time.Now().UnixMilli()
		err := tracker.Put("srv1", postID, updateAt)
		if err != nil {
			t.Fatalf("Unexpected error from Put during cleanup trigger: %v", err)
		}
	}

	// Old entry should be cleaned up
	_, oldExists := tracker.Get("srv1", oldPostID)
	if oldExists {
		t.Fatalf("Expected old entry to be cleaned up")
	}

	// Recent entry should still exist
	_, recentExists := tracker.Get("srv1", recentPostID)
	if !recentExists {
		t.Fatalf("Expected recent entry to still exist")
	}
}

func TestPostTracker_CustomMaxEntries(t *testing.T) {
	// Test with a smaller limit to verify configurability
	customLimit := 5
	tracker := NewPostTracker(customLimit)

	// Add entries up to the limit
	for i := range customLimit {
		postID := fmt.Sprintf("test_post_%d", i)
		updateAt := time.Now().UnixMilli()
		err := tracker.Put("srv1", postID, updateAt)
		if err != nil {
			t.Fatalf("Unexpected error adding entry %d: %v", i, err)
		}
	}

	// Should have exactly the custom limit of entries
	size := tracker.Size()
	if size != customLimit {
		t.Fatalf("Expected tracker size to be %d, got %d", customLimit, size)
	}

	// Try to add one more entry - should fail
	err := tracker.Put("srv1", "should_fail", time.Now().UnixMilli())
	if err == nil {
		t.Fatalf("Expected Put to fail when at custom capacity limit")
	}

	// Size should still be the custom limit
	sizeAfterFailure := tracker.Size()
	if sizeAfterFailure != customLimit {
		t.Fatalf("Expected tracker size to remain %d after failed Put, got %d", customLimit, sizeAfterFailure)
	}
}

// TestPostTracker_IsolatesIdenticalPostIDsAcrossServers verifies that a post shared to
// two Matrix servers keeps independent tracking state for each - the same Mattermost
// post ID under two serverIDs must not collide.
func TestPostTracker_IsolatesIdenticalPostIDsAcrossServers(t *testing.T) {
	tracker := NewPostTracker(DefaultPostTrackerMaxEntries)

	postID := "shared-post"
	updateAtA := time.Now().UnixMilli()
	updateAtB := updateAtA + 1000

	if err := tracker.Put("server-a", postID, updateAtA); err != nil {
		t.Fatalf("Unexpected error from Put for server-a: %v", err)
	}
	if err := tracker.Put("server-b", postID, updateAtB); err != nil {
		t.Fatalf("Unexpected error from Put for server-b: %v", err)
	}

	gotA, existsA := tracker.Get("server-a", postID)
	if !existsA || gotA != updateAtA {
		t.Fatalf("Expected server-a's entry to be %d, got %d (exists=%v)", updateAtA, gotA, existsA)
	}

	gotB, existsB := tracker.Get("server-b", postID)
	if !existsB || gotB != updateAtB {
		t.Fatalf("Expected server-b's entry to be %d, got %d (exists=%v)", updateAtB, gotB, existsB)
	}

	tracker.Delete("server-a", postID)

	if _, exists := tracker.Get("server-a", postID); exists {
		t.Fatalf("Expected server-a's entry to be deleted")
	}
	if _, exists := tracker.Get("server-b", postID); !exists {
		t.Fatalf("Deleting server-a's entry must not affect server-b's entry")
	}
}

// TestPendingFileTracker_IsolatesIdenticalPostIDsAcrossServers verifies the same
// isolation for pending file attachments.
func TestPendingFileTracker_IsolatesIdenticalPostIDsAcrossServers(t *testing.T) {
	tracker := NewPendingFileTracker()

	postID := "shared-post"
	fileA := &PendingFile{FileID: "file-a", Filename: "a.png", MxcURI: "mxc://server-a/a"}
	fileB := &PendingFile{FileID: "file-b", Filename: "b.png", MxcURI: "mxc://server-b/b"}

	tracker.AddFile("server-a", postID, fileA)
	tracker.AddFile("server-b", postID, fileB)

	filesA := tracker.GetFiles("server-a", postID)
	if len(filesA) != 1 || filesA[0].MxcURI != "mxc://server-a/a" {
		t.Fatalf("Expected server-a's files to contain only its own file, got %+v", filesA)
	}

	// GetFiles removes what it returns; a second read of server-a must now be empty.
	filesAAgain := tracker.GetFiles("server-a", postID)
	if len(filesAAgain) != 0 {
		t.Fatalf("Expected server-a's files to have been removed by the prior GetFiles call, got %+v", filesAAgain)
	}

	// ...and server-b's files must be unaffected by server-a's removal.
	filesB := tracker.GetFiles("server-b", postID)
	if len(filesB) != 1 || filesB[0].MxcURI != "mxc://server-b/b" {
		t.Fatalf("Expected server-b's files to contain only its own file, got %+v", filesB)
	}
}
