package main

import (
	"sync"
	"time"

	"github.com/pkg/errors"
)

const (
	// DefaultPostTrackerMaxEntries is the default maximum number of entries in PostTracker
	DefaultPostTrackerMaxEntries = 10000
)

// PostTracker tracks post creation timestamps in memory to detect redundant edits
type PostTracker struct {
	mutex       sync.RWMutex
	entries     map[string]int64 // postID -> UpdateAt timestamp
	putCounter  int              // Counter for triggering cleanup
	cleanupFreq int              // Cleanup every N puts
	maxEntries  int              // Maximum number of entries allowed
}

// NewPostTracker creates a new PostTracker instance with the specified maximum entries
func NewPostTracker(maxEntries int) *PostTracker {
	return &PostTracker{
		entries:     make(map[string]int64),
		cleanupFreq: 100, // Cleanup every 100 puts
		maxEntries:  maxEntries,
	}
}

// Put stores a post ID with its UpdateAt timestamp
// Returns an error if the tracker is at capacity and cannot accept new entries
func (pt *PostTracker) Put(postID string, updateAt int64) error {
	pt.mutex.Lock()
	defer pt.mutex.Unlock()

	// Check if we're at capacity before adding
	if len(pt.entries) >= pt.maxEntries {
		// Try to clean up old entries first
		pt.cleanupOldEntries()

		// If still at capacity after cleanup, reject the new entry
		if len(pt.entries) >= pt.maxEntries {
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

// PendingFile represents a file that has been uploaded to Matrix but not yet attached to a post
type PendingFile struct {
	FileID     string
	Filename   string
	MxcURI     string
	MimeType   string
	Size       int64
	UploadedAt int64
}

// PendingFileTracker tracks files uploaded to Matrix that are awaiting their posts
type PendingFileTracker struct {
	mutex       sync.RWMutex
	filesByPost map[string][]*PendingFile // postID -> list of pending files
	addCounter  int                       // Counter for triggering cleanup
	cleanupFreq int                       // Cleanup every N adds
}

// NewPendingFileTracker creates a new PendingFileTracker instance
func NewPendingFileTracker() *PendingFileTracker {
	return &PendingFileTracker{
		filesByPost: make(map[string][]*PendingFile),
		cleanupFreq: 50, // Cleanup every 50 adds
	}
}

// AddFile adds a pending file for a specific post
func (pft *PendingFileTracker) AddFile(postID string, file *PendingFile) {
	pft.mutex.Lock()
	defer pft.mutex.Unlock()

	file.UploadedAt = time.Now().UnixMilli()
	pft.filesByPost[postID] = append(pft.filesByPost[postID], file)
	pft.addCounter++

	// Periodic cleanup based on add frequency
	if pft.addCounter%pft.cleanupFreq == 0 {
		pft.cleanupOldFiles()
	}
}

// GetFiles retrieves and removes all pending files for a post
func (pft *PendingFileTracker) GetFiles(postID string) []*PendingFile {
	pft.mutex.Lock()
	defer pft.mutex.Unlock()

	files := pft.filesByPost[postID]
	delete(pft.filesByPost, postID)
	return files
}

// RemoveFile removes a specific file from pending files for a post
func (pft *PendingFileTracker) RemoveFile(postID, fileID string) bool {
	pft.mutex.Lock()
	defer pft.mutex.Unlock()

	files := pft.filesByPost[postID]
	for i, file := range files {
		if file.FileID == fileID {
			// Remove this file from the slice
			pft.filesByPost[postID] = append(files[:i], files[i+1:]...)

			// If no files left for this post, remove the post entry
			if len(pft.filesByPost[postID]) == 0 {
				delete(pft.filesByPost, postID)
			}
			return true
		}
	}
	return false
}

// CleanupOldFiles removes files older than 30 minutes (in case posts never arrive)
func (pft *PendingFileTracker) CleanupOldFiles() int {
	pft.mutex.Lock()
	defer pft.mutex.Unlock()

	return pft.cleanupOldFiles()
}

// cleanupOldFiles removes files older than 30 minutes
// Must be called with mutex already locked
func (pft *PendingFileTracker) cleanupOldFiles() int {
	cutoffTime := time.Now().Add(-30 * time.Minute).UnixMilli()
	removedCount := 0

	for postID, files := range pft.filesByPost {
		var keepFiles []*PendingFile
		for _, file := range files {
			if file.UploadedAt >= cutoffTime {
				keepFiles = append(keepFiles, file)
			} else {
				removedCount++
			}
		}

		if len(keepFiles) == 0 {
			delete(pft.filesByPost, postID)
		} else {
			pft.filesByPost[postID] = keepFiles
		}
	}

	// Reset counter after cleanup
	pft.addCounter = 0

	return removedCount
}

// Size returns the total number of pending files (for debugging/monitoring)
func (pft *PendingFileTracker) Size() int {
	pft.mutex.RLock()
	defer pft.mutex.RUnlock()

	total := 0
	for _, files := range pft.filesByPost {
		total += len(files)
	}
	return total
}
