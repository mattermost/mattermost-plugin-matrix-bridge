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

	err := tracker.Put(postID, updateAt)
	if err != nil {
		t.Fatalf("Unexpected error from Put: %v", err)
	}

	retrievedUpdateAt, exists := tracker.Get(postID)
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

	err := tracker.Put(postID, updateAt)
	if err != nil {
		t.Fatalf("Unexpected error from Put: %v", err)
	}

	tracker.Delete(postID)

	_, exists := tracker.Get(postID)
	if exists {
		t.Fatalf("Expected post ID to be deleted from tracker")
	}
}

func TestPostTracker_MaxEntries(t *testing.T) {
	tracker := NewPostTracker(DefaultPostTrackerMaxEntries)

	// Add exactly 10,000 entries (should not trigger capacity check)
	for i := 0; i < 10000; i++ {
		postID := fmt.Sprintf("test_post_%d", i)
		updateAt := time.Now().UnixMilli()
		err := tracker.Put(postID, updateAt)
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
	err := tracker.Put("should_fail", time.Now().UnixMilli())
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
	err := tracker.Put(oldPostID, oldUpdateAt)
	if err != nil {
		t.Fatalf("Unexpected error from Put: %v", err)
	}

	// Add a recent entry
	recentPostID := "recent_post"
	recentUpdateAt := time.Now().UnixMilli()
	err = tracker.Put(recentPostID, recentUpdateAt)
	if err != nil {
		t.Fatalf("Unexpected error from Put: %v", err)
	}

	// Trigger cleanup by doing many puts
	for i := 0; i < 100; i++ {
		postID := fmt.Sprintf("trigger_cleanup_%d", i)
		updateAt := time.Now().UnixMilli()
		err := tracker.Put(postID, updateAt)
		if err != nil {
			t.Fatalf("Unexpected error from Put during cleanup trigger: %v", err)
		}
	}

	// Old entry should be cleaned up
	_, oldExists := tracker.Get(oldPostID)
	if oldExists {
		t.Fatalf("Expected old entry to be cleaned up")
	}

	// Recent entry should still exist
	_, recentExists := tracker.Get(recentPostID)
	if !recentExists {
		t.Fatalf("Expected recent entry to still exist")
	}
}

func TestPostTracker_CustomMaxEntries(t *testing.T) {
	// Test with a smaller limit to verify configurability
	customLimit := 5
	tracker := NewPostTracker(customLimit)

	// Add entries up to the limit
	for i := 0; i < customLimit; i++ {
		postID := fmt.Sprintf("test_post_%d", i)
		updateAt := time.Now().UnixMilli()
		err := tracker.Put(postID, updateAt)
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
	err := tracker.Put("should_fail", time.Now().UnixMilli())
	if err == nil {
		t.Fatalf("Expected Put to fail when at custom capacity limit")
	}

	// Size should still be the custom limit
	sizeAfterFailure := tracker.Size()
	if sizeAfterFailure != customLimit {
		t.Fatalf("Expected tracker size to remain %d after failed Put, got %d", customLimit, sizeAfterFailure)
	}
}
