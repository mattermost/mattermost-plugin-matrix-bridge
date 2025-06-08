package main

import (
	"sync"
	"time"
	
	"github.com/pkg/errors"
)

// PostTracker tracks post creation timestamps in memory to detect redundant edits
type PostTracker struct {
	mutex       sync.RWMutex
	entries     map[string]int64 // postID -> UpdateAt timestamp
	putCounter  int              // Counter for triggering cleanup
	cleanupFreq int              // Cleanup every N puts
}

// NewPostTracker creates a new PostTracker instance
func NewPostTracker() *PostTracker {
	return &PostTracker{
		entries:     make(map[string]int64),
		cleanupFreq: 100, // Cleanup every 100 puts
	}
}

// Put stores a post ID with its UpdateAt timestamp
// Returns an error if the tracker is at capacity and cannot accept new entries
func (pt *PostTracker) Put(postID string, updateAt int64) error {
	pt.mutex.Lock()
	defer pt.mutex.Unlock()
	
	// Check if we're at capacity before adding
	if len(pt.entries) >= 10000 {
		// Try to clean up old entries first
		pt.cleanupOldEntries()
		
		// If still at capacity after cleanup, reject the new entry
		if len(pt.entries) >= 10000 {
			return errors.New("post tracker at capacity - cannot add new entry")
		}
	}
	
	// Add the entry
	pt.entries[postID] = updateAt
	pt.putCounter++
	
	// Periodic cleanup based on put frequency
	if pt.putCounter%pt.cleanupFreq == 0 {
		pt.cleanupOldEntries()
	}
	
	return nil
}

// Get retrieves the UpdateAt timestamp for a post ID
func (pt *PostTracker) Get(postID string) (int64, bool) {
	pt.mutex.RLock()
	defer pt.mutex.RUnlock()
	
	updateAt, exists := pt.entries[postID]
	return updateAt, exists
}

// Delete removes a post ID from tracking
func (pt *PostTracker) Delete(postID string) {
	pt.mutex.Lock()
	defer pt.mutex.Unlock()
	
	delete(pt.entries, postID)
}

// cleanupOldEntries removes entries older than 1 hour
// Must be called with mutex already locked
func (pt *PostTracker) cleanupOldEntries() {
	cutoffTime := time.Now().Add(-1 * time.Hour).UnixMilli()
	
	for postID, updateAt := range pt.entries {
		if updateAt < cutoffTime {
			delete(pt.entries, postID)
		}
	}
	
	// Reset counter after cleanup
	pt.putCounter = 0
}

// Size returns the current number of tracked entries (for debugging/monitoring)
func (pt *PostTracker) Size() int {
	pt.mutex.RLock()
	defer pt.mutex.RUnlock()
	
	return len(pt.entries)
}