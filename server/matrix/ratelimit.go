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
	// RoomCreation limits for room creation operations
	RoomCreation TokenBucketConfig `json:"room_creation"`
	// Messages limits for message sending operations
	Messages TokenBucketConfig `json:"messages"`
	// Invites limits for room invitation operations
	Invites TokenBucketConfig `json:"invites"`
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
		// Room creation: Allow bursts but limit sustained creation
		// Based on Synapse defaults but more permissive for Application Services
		RoomCreation: TokenBucketConfig{
			Rate:      0.5, // 0.5 rooms per second sustained rate
			BurstSize: 5,   // Allow creating 5 rooms quickly
			Interval:  0,   // Use token bucket, not interval
		},
		// Messages: Based on Synapse rc_message defaults (0.2/sec, burst 10)
		Messages: TokenBucketConfig{
			Rate:      0.2, // 0.2 messages per second sustained
			BurstSize: 10,  // Allow 10 message burst
			Interval:  0,
		},
		// Invites: Based on Synapse rc_invites defaults
		Invites: TokenBucketConfig{
			Rate:      0.3, // 0.3 invites per second sustained
			BurstSize: 10,  // Allow 10 invite burst
			Interval:  0,
		},
	}
}

// TestRateLimitConfig returns more aggressive limits suitable for tests
func TestRateLimitConfig() RateLimitConfig {
	return RateLimitConfig{
		Enabled: true,
		// More aggressive limits for tests to prevent CI rate limiting
		RoomCreation: TokenBucketConfig{
			Rate:      0, // Disable token bucket
			BurstSize: 0,
			Interval:  2 * time.Second, // 2 second minimum interval between operations
		},
		Messages: TokenBucketConfig{
			Rate:      0.1, // Slower message rate for tests
			BurstSize: 5,
			Interval:  0,
		},
		Invites: TokenBucketConfig{
			Rate:      0.2,
			BurstSize: 5,
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
