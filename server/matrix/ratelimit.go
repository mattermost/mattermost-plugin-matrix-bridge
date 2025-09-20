// Package matrix provides Matrix client functionality for the Mattermost bridge.
package matrix

import (
	"context"
	"sync"
	"time"

	"github.com/pkg/errors"
)

// RateLimitingMode represents the different rate limiting strategies available to users
type RateLimitingMode string

// Rate limiting mode constants define different throttling strategies
const (
	RateLimitDisabled     RateLimitingMode = "disabled"     // No rate limiting (maximum performance, risk of 429 errors)
	RateLimitTesting      RateLimitingMode = "testing"      // Fast rate limiting for unit tests (2 second intervals)
	RateLimitRelaxed      RateLimitingMode = "relaxed"      // Light throttling (fast, suitable for dedicated Matrix servers)
	RateLimitAutomatic    RateLimitingMode = "automatic"    // Balanced throttling (good performance with safety) - DEFAULT
	RateLimitConservative RateLimitingMode = "conservative" // Heavy throttling (safest, matches Synapse defaults exactly)
	RateLimitRestricted   RateLimitingMode = "restricted"   // Maximum throttling (slowest, for shared/limited Matrix servers)
)

// DisabledWaitTime is returned when rate limiting is effectively disabled (rate <= 0)
// This long duration prevents infinite waiting while clearly indicating disabled state
const DisabledWaitTime = time.Hour

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

// isDisabled checks if rate limiting is disabled
func (tb *TokenBucket) isDisabled() bool {
	return tb.rate == 0 && tb.burstSize == 0 && tb.interval == 0
}

// Allow checks if an operation is allowed and consumes a token if so
func (tb *TokenBucket) Allow() bool {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	if tb.isDisabled() {
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

	if tb.isDisabled() {
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
		return DisabledWaitTime // Effectively disabled
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

// TestRateLimitConfig returns fast limits suitable for unit tests with permissive Synapse configuration
func TestRateLimitConfig() RateLimitConfig {
	// Use testing mode - optimized for real Matrix server integration tests
	return GetRateLimitConfigByMode(RateLimitTesting)
}

// Unit test rate limiting constants - shared between config and tests
const (
	UnitTestRoomCreationRate      = 0.075                   // 0.075 rooms per second (13.3 second intervals) - 25% slower
	UnitTestRoomCreationBurstSize = 3                       // Allow creating 3 rooms quickly - 25% smaller
	UnitTestRoomCreationInterval  = 6700 * time.Millisecond // 6.7 second intervals for testing mode
	UnitTestMessageRate           = 0.3                     // 0.3 messages per second - 25% slower
	UnitTestMessageBurstSize      = 15                      // Allow 15 message burst - 25% smaller
	UnitTestInviteRate            = 0.45                    // 0.45 invites per second - 25% slower
	UnitTestInviteBurstSize       = 15                      // Allow 15 invite burst - 25% smaller
	UnitTestRegistrationRate      = 0.255                   // 0.255 registrations per second - 25% slower
	UnitTestRegistrationBurstSize = 4                       // Allow 4 registration burst - 25% smaller
	UnitTestJoinRate              = 0.3                     // 0.3 joins per second - 25% slower
	UnitTestJoinBurstSize         = 7                       // Allow 7 join burst - 25% smaller
)

// UnitTestRateLimitConfig returns predictable rate limits for unit tests that don't connect to real servers
func UnitTestRateLimitConfig() RateLimitConfig {
	// Uses constants so that tests and config stay in sync when values are tweaked
	return RateLimitConfig{
		Enabled: true,
		RoomCreation: TokenBucketConfig{
			Rate:      UnitTestRoomCreationRate,
			BurstSize: UnitTestRoomCreationBurstSize,
			Interval:  0, // Use token bucket, not interval
		},
		Messages: TokenBucketConfig{
			Rate:      UnitTestMessageRate,
			BurstSize: UnitTestMessageBurstSize,
			Interval:  0,
		},
		Invites: TokenBucketConfig{
			Rate:      UnitTestInviteRate,
			BurstSize: UnitTestInviteBurstSize,
			Interval:  0,
		},
		Registration: TokenBucketConfig{
			Rate:      UnitTestRegistrationRate,
			BurstSize: UnitTestRegistrationBurstSize,
			Interval:  0,
		},
		Joins: TokenBucketConfig{
			Rate:      UnitTestJoinRate,
			BurstSize: UnitTestJoinBurstSize,
			Interval:  0,
		},
	}
}

// LoadTestRateLimitConfig returns rate limits optimized for load testing (high throttling)
func LoadTestRateLimitConfig() RateLimitConfig {
	// Designed to produce measurable throttling under load for TestClient_MessageSpamLoad
	return RateLimitConfig{
		Enabled: true,
		Messages: TokenBucketConfig{
			Rate:      5.0, // 5 messages per second - designed to throttle spam
			BurstSize: 10,  // Allow 10 message burst
			Interval:  0,
		},
		// Other operations use conservative rates
		RoomCreation: TokenBucketConfig{Rate: 0.1, BurstSize: 2, Interval: 0},
		Invites:      TokenBucketConfig{Rate: 1.0, BurstSize: 5, Interval: 0},
		Registration: TokenBucketConfig{Rate: 1.0, BurstSize: 3, Interval: 0},
		Joins:        TokenBucketConfig{Rate: 1.0, BurstSize: 5, Interval: 0},
	}
}

// GetRateLimitConfigByMode returns a rate limit configuration based on the specified mode
func GetRateLimitConfigByMode(mode RateLimitingMode) RateLimitConfig {
	switch mode {
	case RateLimitDisabled:
		return RateLimitConfig{
			Enabled: false, // Disable all rate limiting
		}

	case RateLimitTesting:
		// More conservative rate limiting for integration tests - uses reduced constants
		// Use interval-based limiting for room creation, token bucket for other operations
		return RateLimitConfig{
			Enabled: true,
			RoomCreation: TokenBucketConfig{
				Rate:      0,                            // Disable token bucket
				BurstSize: 0,                            // Disable burst
				Interval:  UnitTestRoomCreationInterval, // Use constant for easy tweaking
			},
			Messages: TokenBucketConfig{
				Rate:      UnitTestMessageRate * 33.3, // Scale up for integration tests: 0.3 * 33.3 ≈ 10.0
				BurstSize: UnitTestMessageBurstSize,   // 15 message burst
				Interval:  0,
			},
			Invites: TokenBucketConfig{
				Rate:      UnitTestInviteRate * 22.2, // Scale up for integration tests: 0.45 * 22.2 ≈ 10.0
				BurstSize: UnitTestInviteBurstSize,   // 15 invite burst
				Interval:  0,
			},
			Registration: TokenBucketConfig{
				Rate:      UnitTestRegistrationRate * 19.6, // Scale up for integration tests: 0.255 * 19.6 ≈ 5.0
				BurstSize: UnitTestRegistrationBurstSize,   // 4 registration burst
				Interval:  0,
			},
			Joins: TokenBucketConfig{
				Rate:      UnitTestJoinRate * 16.7, // Scale up for integration tests: 0.3 * 16.7 ≈ 5.0
				BurstSize: UnitTestJoinBurstSize,   // 7 join burst
				Interval:  0,
			},
		}

	case RateLimitRelaxed:
		// 5x faster than Synapse defaults - for dedicated Matrix servers
		return RateLimitConfig{
			Enabled: true,
			RoomCreation: TokenBucketConfig{
				Rate:      0.25, // 0.25 rooms per second (4 second intervals)
				BurstSize: 3,    // Allow creating 3 rooms quickly
				Interval:  0,
			},
			Messages: TokenBucketConfig{
				Rate:      1.0, // 1 message per second
				BurstSize: 15,  // Allow 15 message burst
				Interval:  0,
			},
			Invites: TokenBucketConfig{
				Rate:      1.5, // 1.5 invites per second
				BurstSize: 15,  // Allow 15 invite burst
				Interval:  0,
			},
			Registration: TokenBucketConfig{
				Rate:      0.85, // 0.85 registrations per second
				BurstSize: 5,    // Allow 5 registration burst
				Interval:  0,
			},
			Joins: TokenBucketConfig{
				Rate:      1.0, // 1 join per second
				BurstSize: 8,   // Allow 8 join burst
				Interval:  0,
			},
		}

	case RateLimitAutomatic:
		// 2x faster than Synapse defaults - balanced performance with safety
		return RateLimitConfig{
			Enabled: true,
			RoomCreation: TokenBucketConfig{
				Rate:      0.1, // 0.1 rooms per second (10 second intervals)
				BurstSize: 3,   // Allow creating 3 rooms quickly
				Interval:  0,
			},
			Messages: TokenBucketConfig{
				Rate:      0.4, // 0.4 messages per second
				BurstSize: 12,  // Allow 12 message burst
				Interval:  0,
			},
			Invites: TokenBucketConfig{
				Rate:      0.6, // 0.6 invites per second
				BurstSize: 12,  // Allow 12 invite burst
				Interval:  0,
			},
			Registration: TokenBucketConfig{
				Rate:      0.34, // 0.34 registrations per second
				BurstSize: 4,    // Allow 4 registration burst
				Interval:  0,
			},
			Joins: TokenBucketConfig{
				Rate:      0.4, // 0.4 joins per second
				BurstSize: 7,   // Allow 7 join burst
				Interval:  0,
			},
		}

	case RateLimitConservative:
		// Match Synapse defaults exactly - maximum safety
		return DefaultRateLimitConfig()

	case RateLimitRestricted:
		// 2x slower than Synapse defaults - for shared/limited Matrix servers
		return RateLimitConfig{
			Enabled: true,
			RoomCreation: TokenBucketConfig{
				Rate:      0.025, // 0.025 rooms per second (40 second intervals)
				BurstSize: 1,     // Allow creating 1 room only
				Interval:  0,
			},
			Messages: TokenBucketConfig{
				Rate:      0.1, // 0.1 messages per second
				BurstSize: 5,   // Allow 5 message burst
				Interval:  0,
			},
			Invites: TokenBucketConfig{
				Rate:      0.15, // 0.15 invites per second
				BurstSize: 5,    // Allow 5 invite burst
				Interval:  0,
			},
			Registration: TokenBucketConfig{
				Rate:      0.085, // 0.085 registrations per second
				BurstSize: 2,     // Allow 2 registration burst
				Interval:  0,
			},
			Joins: TokenBucketConfig{
				Rate:      0.1, // 0.1 joins per second
				BurstSize: 3,   // Allow 3 join burst
				Interval:  0,
			},
		}

	default:
		// Default to automatic mode for unknown values
		return GetRateLimitConfigByMode(RateLimitAutomatic)
	}
}

// ValidateRateLimitingMode validates a rate limiting mode and returns true if valid
func ValidateRateLimitingMode(mode RateLimitingMode) bool {
	validModes := []RateLimitingMode{
		RateLimitDisabled,
		RateLimitTesting,
		RateLimitRelaxed,
		RateLimitAutomatic,
		RateLimitConservative,
		RateLimitRestricted,
	}

	for _, validMode := range validModes {
		if mode == validMode {
			return true
		}
	}
	return false
}

// ParseRateLimitingMode parses a string to RateLimitingMode with automatic default fallback
func ParseRateLimitingMode(modeStr string) RateLimitingMode {
	mode := RateLimitingMode(modeStr)

	// Return default for empty or invalid modes
	if modeStr == "" || !ValidateRateLimitingMode(mode) {
		return RateLimitAutomatic
	}

	return mode
}

// IsRateLimitError checks if an error is a Matrix 429 rate limit error
func IsRateLimitError(err error) bool {
	var matrixErr *Error
	if errors.As(err, &matrixErr) {
		return matrixErr.StatusCode == 429 || matrixErr.ErrCode == "M_LIMIT_EXCEEDED"
	}
	return false
}
