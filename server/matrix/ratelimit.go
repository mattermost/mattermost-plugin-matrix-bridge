// Package matrix provides Matrix client functionality for the Mattermost bridge.
package matrix

import (
	"context"
	"sync"
	"time"

	"github.com/pkg/errors"
)

// RateLimitConfig defines rate limiting configuration for Matrix operations
type RateLimitConfig struct {
	// RoomCreation limits for room creation operations (rc_room_creation)
	RoomCreation TokenBucketConfig `json:"room_creation"`
	// Messages limits for message sending operations (rc_message)
	Messages TokenBucketConfig `json:"messages"`
	// Invites limits for room invitation operations (rc_invites)
	Invites TokenBucketConfig `json:"invites"`
	// Registration limits for user registration operations (rc_registration)
	Registration TokenBucketConfig `json:"registration"`
	// Joins limits for room join operations (rc_joins)
	Joins TokenBucketConfig `json:"joins"`
	// Enabled controls whether rate limiting is active
	Enabled bool `json:"enabled"`
}

// TokenBucketConfig defines token bucket algorithm parameters
type TokenBucketConfig struct {
	// Rate is tokens per second to add to bucket
	Rate float64 `json:"rate"`
	// BurstSize is maximum tokens the bucket can hold
	BurstSize int `json:"burst_size"`
	// Interval is minimum time between operations (alternative to rate-based limiting)
	Interval time.Duration `json:"interval,omitempty"`
}

// TokenBucket implements a token bucket rate limiter
type TokenBucket struct {
	mu         sync.Mutex
	rate       float64       // tokens per second
	burstSize  int           // maximum tokens
	tokens     float64       // current tokens
	lastRefill time.Time     // last refill time
	interval   time.Duration // minimum interval between operations
	lastOp     time.Time     // last operation time (for interval-based limiting)
}

// NewTokenBucket creates a new token bucket with the given configuration
func NewTokenBucket(config TokenBucketConfig) *TokenBucket {
	tb := &TokenBucket{
		rate:       config.Rate,
		burstSize:  config.BurstSize,
		tokens:     float64(config.BurstSize), // Start with full bucket
		lastRefill: time.Now(),
		interval:   config.Interval,
		lastOp:     time.Time{},
	}
	return tb
}

// Allow checks if an operation is allowed and consumes a token if so
func (tb *TokenBucket) Allow() bool {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	// Check for disabled case: rate=0, burstSize=0, interval=0
	if tb.rate == 0 && tb.burstSize == 0 && tb.interval == 0 {
		return true // Disabled rate limiting allows all operations
	}

	now := time.Now()

	// If using interval-based limiting, check minimum interval
	if tb.interval > 0 {
		if !tb.lastOp.IsZero() && now.Sub(tb.lastOp) < tb.interval {
			return false
		}
		tb.lastOp = now
		return true
	}

	// Token bucket algorithm
	// Add tokens based on time elapsed
	elapsed := now.Sub(tb.lastRefill)
	tb.tokens += elapsed.Seconds() * tb.rate
	if tb.tokens > float64(tb.burstSize) {
		tb.tokens = float64(tb.burstSize)
	}
	tb.lastRefill = now

	// Check if we have enough tokens
	if tb.tokens >= 1.0 {
		tb.tokens--
		return true
	}

	return false
}

// Wait blocks until an operation is allowed, then consumes a token
func (tb *TokenBucket) Wait(ctx context.Context) error {
	for {
		if tb.Allow() {
			return nil
		}

		// Calculate wait time
		waitTime := tb.getWaitTime()
		if waitTime <= 0 {
			continue
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(waitTime):
			// Continue loop to try again
		}
	}
}

// getWaitTime calculates how long to wait before next operation is allowed
func (tb *TokenBucket) getWaitTime() time.Duration {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	// Check for disabled case: rate=0, burstSize=0, interval=0
	if tb.rate == 0 && tb.burstSize == 0 && tb.interval == 0 {
		return 0 // No waiting needed when disabled
	}

	now := time.Now()

	// For interval-based limiting
	if tb.interval > 0 {
		if tb.lastOp.IsZero() {
			return 0
		}
		elapsed := now.Sub(tb.lastOp)
		if elapsed >= tb.interval {
			return 0
		}
		return tb.interval - elapsed
	}

	// For token bucket
	if tb.tokens >= 1.0 {
		return 0
	}

	// Time to get 1 token
	tokensNeeded := 1.0 - tb.tokens
	if tb.rate <= 0 {
		return time.Hour // Effectively disabled
	}
	return time.Duration(tokensNeeded / tb.rate * float64(time.Second))
}

// DefaultRateLimitConfig returns sensible defaults for production use
func DefaultRateLimitConfig() RateLimitConfig {
	return RateLimitConfig{
		Enabled: true,
		// Room creation: Match Synapse rc_room_creation defaults
		RoomCreation: TokenBucketConfig{
			Rate:      0.05, // 0.05 rooms per second (20 second intervals)
			BurstSize: 2,    // Allow creating 2 rooms quickly
			Interval:  0,    // Use token bucket, not interval
		},
		// Messages: Match Synapse rc_message defaults exactly
		Messages: TokenBucketConfig{
			Rate:      0.2, // 0.2 messages per second sustained
			BurstSize: 10,  // Allow 10 message burst
			Interval:  0,
		},
		// Invites: Match Synapse rc_invites per_room defaults
		Invites: TokenBucketConfig{
			Rate:      0.3, // 0.3 invites per second per room
			BurstSize: 10,  // Allow 10 invite burst
			Interval:  0,
		},
		// Registration: Match Synapse rc_registration defaults
		Registration: TokenBucketConfig{
			Rate:      0.17, // 0.17 registrations per second
			BurstSize: 3,    // Allow 3 registration burst
			Interval:  0,    // Use token bucket, not interval
		},
		// Joins: Match Synapse rc_joins local defaults
		Joins: TokenBucketConfig{
			Rate:      0.2, // 0.2 joins per second (local rate)
			BurstSize: 5,   // Allow 5 join burst
			Interval:  0,
		},
	}
}

// TestRateLimitConfig returns fast limits suitable for tests while maintaining throttling behavior
func TestRateLimitConfig() RateLimitConfig {
	return RateLimitConfig{
		Enabled: true,
		// 10x faster than production but still tests throttling behavior
		RoomCreation: TokenBucketConfig{
			Rate:      0.5, // 0.5 rooms per second (2 second intervals) - 10x faster than 0.05/sec
			BurstSize: 2,   // Same burst size for testing
			Interval:  0,
		},
		Messages: TokenBucketConfig{
			Rate:      2.0, // 2 messages per second (500ms intervals) - 10x faster than 0.2/sec
			BurstSize: 10,  // Same burst size for testing
			Interval:  0,
		},
		Invites: TokenBucketConfig{
			Rate:      3.0, // 3 invites per second - 10x faster than 0.3/sec
			BurstSize: 10,  // Same burst size for testing
			Interval:  0,
		},
		Registration: TokenBucketConfig{
			Rate:      1.7, // 1.7 registrations per second - 10x faster than 0.17/sec
			BurstSize: 3,   // Same burst size for testing
			Interval:  0,
		},
		Joins: TokenBucketConfig{
			Rate:      2.0, // 2 joins per second - 10x faster than 0.2/sec
			BurstSize: 5,   // Same burst size for testing
			Interval:  0,
		},
	}
}

// IsRateLimitError checks if an error is a Matrix 429 rate limit error
func IsRateLimitError(err error) bool {
	var matrixErr *Error
	if errors.As(err, &matrixErr) {
		return matrixErr.StatusCode == 429 || matrixErr.ErrCode == "M_LIMIT_EXCEEDED"
	}
	return false
}
