package matrix

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTokenBucket_Allow_IntervalBased(t *testing.T) {
	// Test interval-based rate limiting (like room creation: 2 second intervals)
	config := TokenBucketConfig{
		Rate:      0, // Disable token bucket
		BurstSize: 0,
		Interval:  100 * time.Millisecond, // 100ms minimum interval
	}

	tb := NewTokenBucket(config)

	// First call should succeed
	assert.True(t, tb.Allow(), "First call should be allowed")

	// Immediate second call should fail (within interval)
	assert.False(t, tb.Allow(), "Second immediate call should be blocked")

	// Wait for interval to pass
	time.Sleep(120 * time.Millisecond)

	// Third call should succeed after interval
	assert.True(t, tb.Allow(), "Call after interval should be allowed")
}

func TestTokenBucket_Allow_TokenBased(t *testing.T) {
	// Test token bucket rate limiting (like messages: 1 per second, burst 10)
	config := TokenBucketConfig{
		Rate:      1.0, // 1 token per second
		BurstSize: 3,   // Allow burst of 3
		Interval:  0,   // No interval-based limiting
	}

	tb := NewTokenBucket(config)

	// Should allow burst size number of calls immediately
	for i := 0; i < 3; i++ {
		assert.True(t, tb.Allow(), "Burst call %d should be allowed", i+1)
	}

	// Fourth call should fail (burst exhausted)
	assert.False(t, tb.Allow(), "Call after burst should be blocked")

	// Wait for one token to regenerate
	time.Sleep(1200 * time.Millisecond)

	// Should allow one more call
	assert.True(t, tb.Allow(), "Call after token regeneration should be allowed")
}

func TestTokenBucket_Wait_IntervalBased(t *testing.T) {
	config := TokenBucketConfig{
		Rate:      0,
		BurstSize: 0,
		Interval:  50 * time.Millisecond,
	}

	tb := NewTokenBucket(config)
	ctx := context.Background()

	// First call should succeed immediately
	start := time.Now()
	err := tb.Wait(ctx)
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.Less(t, elapsed, 10*time.Millisecond, "First call should be immediate")

	// Second call should wait for interval
	start = time.Now()
	err = tb.Wait(ctx)
	elapsed = time.Since(start)

	require.NoError(t, err)
	assert.GreaterOrEqual(t, elapsed, 45*time.Millisecond, "Second call should wait ~50ms")
	assert.Less(t, elapsed, 100*time.Millisecond, "Wait should not be excessive")
}

func TestTokenBucket_Wait_TokenBased(t *testing.T) {
	config := TokenBucketConfig{
		Rate:      10.0, // 10 tokens per second = 100ms per token
		BurstSize: 2,    // Allow burst of 2
		Interval:  0,
	}

	tb := NewTokenBucket(config)
	ctx := context.Background()

	// First two calls should succeed immediately (burst)
	for i := 0; i < 2; i++ {
		start := time.Now()
		err := tb.Wait(ctx)
		elapsed := time.Since(start)

		require.NoError(t, err)
		assert.Less(t, elapsed, 10*time.Millisecond, "Burst call %d should be immediate", i+1)
	}

	// Third call should wait for token regeneration
	start := time.Now()
	err := tb.Wait(ctx)
	elapsed := time.Since(start)

	require.NoError(t, err)
	assert.GreaterOrEqual(t, elapsed, 90*time.Millisecond, "Third call should wait ~100ms")
	assert.Less(t, elapsed, 150*time.Millisecond, "Wait should not be excessive")
}

func TestTokenBucket_Wait_Timeout(t *testing.T) {
	config := TokenBucketConfig{
		Rate:      0,
		BurstSize: 0,
		Interval:  1 * time.Second, // Long interval
	}

	tb := NewTokenBucket(config)

	// First call to consume the initial allowance
	require.True(t, tb.Allow())

	// Second call should timeout
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := tb.Wait(ctx)
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.Equal(t, context.DeadlineExceeded, err)
	assert.GreaterOrEqual(t, elapsed, 90*time.Millisecond, "Should wait for timeout")
	assert.Less(t, elapsed, 150*time.Millisecond, "Should not wait too long")
}

func TestTokenBucket_ConcurrentAccess(t *testing.T) {
	config := TokenBucketConfig{
		Rate:      0,
		BurstSize: 0,
		Interval:  20 * time.Millisecond,
	}

	tb := NewTokenBucket(config)
	ctx := context.Background()

	const numGoroutines = 10
	const callsPerGoroutine = 5

	var wg sync.WaitGroup
	results := make(chan time.Duration, numGoroutines*callsPerGoroutine)

	// Launch multiple goroutines that all try to call Wait()
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < callsPerGoroutine; j++ {
				start := time.Now()
				err := tb.Wait(ctx)
				elapsed := time.Since(start)
				require.NoError(t, err)
				results <- elapsed
			}
		}()
	}

	wg.Wait()
	close(results)

	// Collect all results
	var durations []time.Duration
	for duration := range results {
		durations = append(durations, duration)
	}

	// Should have all calls completed
	assert.Len(t, durations, numGoroutines*callsPerGoroutine)

	// First call should be immediate, subsequent calls should have increasing delays
	// due to the interval-based rate limiting under concurrent access
	immediateCount := 0
	delayedCount := 0

	for _, duration := range durations {
		if duration < 10*time.Millisecond {
			immediateCount++
		} else {
			delayedCount++
		}
	}

	// Only one call should be immediate (the first one)
	assert.Equal(t, 1, immediateCount, "Only first call should be immediate")
	assert.Equal(t, numGoroutines*callsPerGoroutine-1, delayedCount, "All other calls should be delayed")
}

func TestTokenBucket_getWaitTime_IntervalBased(t *testing.T) {
	config := TokenBucketConfig{
		Rate:      0,
		BurstSize: 0,
		Interval:  100 * time.Millisecond,
	}

	tb := NewTokenBucket(config)

	// Before any operations, wait time should be 0
	waitTime := tb.getWaitTime()
	assert.Equal(t, time.Duration(0), waitTime, "Initial wait time should be 0")

	// After first operation, immediate second operation should need to wait
	require.True(t, tb.Allow())
	waitTime = tb.getWaitTime()
	assert.GreaterOrEqual(t, waitTime, 90*time.Millisecond, "Should need to wait ~100ms")
	assert.LessOrEqual(t, waitTime, 100*time.Millisecond, "Wait time should not exceed interval")

	// After waiting, should not need to wait again
	time.Sleep(waitTime + 10*time.Millisecond)
	waitTime = tb.getWaitTime()
	assert.Equal(t, time.Duration(0), waitTime, "After waiting, should not need to wait again")
}

func TestTokenBucket_getWaitTime_TokenBased(t *testing.T) {
	config := TokenBucketConfig{
		Rate:      10.0, // 10 tokens per second = 100ms per token
		BurstSize: 2,
		Interval:  0,
	}

	tb := NewTokenBucket(config)

	// With burst available, wait time should be 0
	waitTime := tb.getWaitTime()
	assert.Equal(t, time.Duration(0), waitTime, "With tokens available, wait time should be 0")

	// Exhaust burst
	require.True(t, tb.Allow())
	require.True(t, tb.Allow())

	// Now should need to wait for token regeneration
	waitTime = tb.getWaitTime()
	assert.GreaterOrEqual(t, waitTime, 90*time.Millisecond, "Should need to wait ~100ms for token")
	assert.LessOrEqual(t, waitTime, 110*time.Millisecond, "Wait time should be reasonable")
}

func TestDefaultRateLimitConfig(t *testing.T) {
	config := DefaultRateLimitConfig()

	assert.True(t, config.Enabled, "Default config should be enabled")
	assert.Greater(t, config.RoomCreation.Rate, 0.0, "Room creation should have positive rate")
	assert.Greater(t, config.Messages.Rate, 0.0, "Messages should have positive rate")
	assert.Greater(t, config.Messages.BurstSize, 0, "Messages should have burst size")
	assert.Greater(t, config.Invites.Rate, 0.0, "Invites should have positive rate")
}

func TestTestRateLimitConfig(t *testing.T) {
	config := UnitTestRateLimitConfig()

	assert.True(t, config.Enabled, "Unit test config should be enabled")
	// Unit test config uses shared constants - tests automatically stay in sync when constants change
	assert.Equal(t, UnitTestRoomCreationRate, config.RoomCreation.Rate, "Unit test config should use UnitTestRoomCreationRate constant")
	assert.Equal(t, UnitTestRoomCreationBurstSize, config.RoomCreation.BurstSize, "Unit test config should use UnitTestRoomCreationBurstSize constant")
	assert.Equal(t, time.Duration(0), config.RoomCreation.Interval, "Test config should use token bucket for room creation")
	assert.Equal(t, UnitTestMessageRate, config.Messages.Rate, "Unit test config should use UnitTestMessageRate constant")
	assert.Equal(t, UnitTestMessageBurstSize, config.Messages.BurstSize, "Unit test config should use UnitTestMessageBurstSize constant")
	assert.Equal(t, UnitTestInviteRate, config.Invites.Rate, "Unit test config should use UnitTestInviteRate constant")
	assert.Equal(t, UnitTestInviteBurstSize, config.Invites.BurstSize, "Unit test config should use UnitTestInviteBurstSize constant")
	assert.Equal(t, UnitTestRegistrationRate, config.Registration.Rate, "Unit test config should use UnitTestRegistrationRate constant")
	assert.Equal(t, UnitTestRegistrationBurstSize, config.Registration.BurstSize, "Unit test config should use UnitTestRegistrationBurstSize constant")
	assert.Equal(t, time.Duration(0), config.Registration.Interval, "Test config should use token bucket for registration")
	assert.Equal(t, UnitTestJoinRate, config.Joins.Rate, "Unit test config should use UnitTestJoinRate constant")
	assert.Equal(t, UnitTestJoinBurstSize, config.Joins.BurstSize, "Unit test config should use UnitTestJoinBurstSize constant")
}

func TestGetRateLimitConfigByMode(t *testing.T) {
	tests := []struct {
		name     string
		mode     RateLimitingMode
		validate func(t *testing.T, config RateLimitConfig)
	}{
		{
			name: "Disabled mode",
			mode: RateLimitDisabled,
			validate: func(t *testing.T, config RateLimitConfig) {
				assert.False(t, config.Enabled, "Disabled mode should have rate limiting disabled")
			},
		},
		{
			name: "Relaxed mode",
			mode: RateLimitRelaxed,
			validate: func(t *testing.T, config RateLimitConfig) {
				assert.True(t, config.Enabled, "Relaxed mode should be enabled")
				// Should be 5x faster than Synapse defaults
				assert.Equal(t, 0.25, config.RoomCreation.Rate, "Relaxed mode should have 0.25 rooms/sec")
				assert.Equal(t, 1.0, config.Messages.Rate, "Relaxed mode should have 1.0 messages/sec")
				assert.Equal(t, 1.5, config.Invites.Rate, "Relaxed mode should have 1.5 invites/sec")
				assert.Equal(t, 0.85, config.Registration.Rate, "Relaxed mode should have 0.85 registrations/sec")
				assert.Equal(t, 1.0, config.Joins.Rate, "Relaxed mode should have 1.0 joins/sec")
			},
		},
		{
			name: "Automatic mode (default)",
			mode: RateLimitAutomatic,
			validate: func(t *testing.T, config RateLimitConfig) {
				assert.True(t, config.Enabled, "Automatic mode should be enabled")
				// Should be 2x faster than Synapse defaults
				assert.Equal(t, 0.1, config.RoomCreation.Rate, "Automatic mode should have 0.1 rooms/sec")
				assert.Equal(t, 0.4, config.Messages.Rate, "Automatic mode should have 0.4 messages/sec")
				assert.Equal(t, 0.6, config.Invites.Rate, "Automatic mode should have 0.6 invites/sec")
				assert.Equal(t, 0.34, config.Registration.Rate, "Automatic mode should have 0.34 registrations/sec")
				assert.Equal(t, 0.4, config.Joins.Rate, "Automatic mode should have 0.4 joins/sec")
			},
		},
		{
			name: "Conservative mode",
			mode: RateLimitConservative,
			validate: func(t *testing.T, config RateLimitConfig) {
				assert.True(t, config.Enabled, "Conservative mode should be enabled")
				// Should match Synapse defaults exactly
				defaultConfig := DefaultRateLimitConfig()
				assert.Equal(t, defaultConfig.RoomCreation.Rate, config.RoomCreation.Rate, "Conservative should match Synapse room creation rate")
				assert.Equal(t, defaultConfig.Messages.Rate, config.Messages.Rate, "Conservative should match Synapse message rate")
				assert.Equal(t, defaultConfig.Invites.Rate, config.Invites.Rate, "Conservative should match Synapse invite rate")
				assert.Equal(t, defaultConfig.Registration.Rate, config.Registration.Rate, "Conservative should match Synapse registration rate")
				assert.Equal(t, defaultConfig.Joins.Rate, config.Joins.Rate, "Conservative should match Synapse join rate")
			},
		},
		{
			name: "Restricted mode",
			mode: RateLimitRestricted,
			validate: func(t *testing.T, config RateLimitConfig) {
				assert.True(t, config.Enabled, "Restricted mode should be enabled")
				// Should be 2x slower than Synapse defaults
				assert.Equal(t, 0.025, config.RoomCreation.Rate, "Restricted mode should have 0.025 rooms/sec")
				assert.Equal(t, 0.1, config.Messages.Rate, "Restricted mode should have 0.1 messages/sec")
				assert.Equal(t, 0.15, config.Invites.Rate, "Restricted mode should have 0.15 invites/sec")
				assert.Equal(t, 0.085, config.Registration.Rate, "Restricted mode should have 0.085 registrations/sec")
				assert.Equal(t, 0.1, config.Joins.Rate, "Restricted mode should have 0.1 joins/sec")
			},
		},
		{
			name: "Unknown mode defaults to automatic",
			mode: RateLimitingMode("unknown"),
			validate: func(t *testing.T, config RateLimitConfig) {
				automaticConfig := GetRateLimitConfigByMode(RateLimitAutomatic)
				assert.Equal(t, automaticConfig, config, "Unknown mode should default to automatic")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := GetRateLimitConfigByMode(tt.mode)
			tt.validate(t, config)
		})
	}
}

func TestRateLimitingModePerformanceOrder(t *testing.T) {
	// Test that modes are ordered from fastest to slowest for messages
	configs := map[string]RateLimitConfig{
		"disabled":     GetRateLimitConfigByMode(RateLimitDisabled),
		"relaxed":      GetRateLimitConfigByMode(RateLimitRelaxed),
		"automatic":    GetRateLimitConfigByMode(RateLimitAutomatic),
		"conservative": GetRateLimitConfigByMode(RateLimitConservative),
		"restricted":   GetRateLimitConfigByMode(RateLimitRestricted),
	}

	// Disabled should have no rate (effectively infinite)
	assert.False(t, configs["disabled"].Enabled, "Disabled should not be enabled")

	// For enabled modes, verify rate ordering (higher rate = faster)
	assert.Greater(t, configs["relaxed"].Messages.Rate, configs["automatic"].Messages.Rate,
		"Relaxed should be faster than automatic")
	assert.Greater(t, configs["automatic"].Messages.Rate, configs["conservative"].Messages.Rate,
		"Automatic should be faster than conservative")
	assert.Greater(t, configs["conservative"].Messages.Rate, configs["restricted"].Messages.Rate,
		"Conservative should be faster than restricted")

	// Same ordering should apply to room creation
	assert.Greater(t, configs["relaxed"].RoomCreation.Rate, configs["automatic"].RoomCreation.Rate,
		"Relaxed room creation should be faster than automatic")
	assert.Greater(t, configs["automatic"].RoomCreation.Rate, configs["conservative"].RoomCreation.Rate,
		"Automatic room creation should be faster than conservative")
	assert.Greater(t, configs["conservative"].RoomCreation.Rate, configs["restricted"].RoomCreation.Rate,
		"Conservative room creation should be faster than restricted")
}

func TestValidateRateLimitingMode(t *testing.T) {
	tests := []struct {
		name     string
		mode     RateLimitingMode
		expected bool
	}{
		{
			name:     "Valid disabled mode",
			mode:     RateLimitDisabled,
			expected: true,
		},
		{
			name:     "Valid relaxed mode",
			mode:     RateLimitRelaxed,
			expected: true,
		},
		{
			name:     "Valid automatic mode",
			mode:     RateLimitAutomatic,
			expected: true,
		},
		{
			name:     "Valid conservative mode",
			mode:     RateLimitConservative,
			expected: true,
		},
		{
			name:     "Valid restricted mode",
			mode:     RateLimitRestricted,
			expected: true,
		},
		{
			name:     "Invalid empty mode",
			mode:     RateLimitingMode(""),
			expected: false,
		},
		{
			name:     "Invalid unknown mode",
			mode:     RateLimitingMode("unknown"),
			expected: false,
		},
		{
			name:     "Invalid random string",
			mode:     RateLimitingMode("totally_invalid"),
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ValidateRateLimitingMode(tt.mode)
			assert.Equal(t, tt.expected, result, "ValidateRateLimitingMode() mismatch for mode: %v", tt.mode)
		})
	}
}

func TestParseRateLimitingMode(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected RateLimitingMode
	}{
		{
			name:     "Valid disabled mode",
			input:    "disabled",
			expected: RateLimitDisabled,
		},
		{
			name:     "Valid relaxed mode",
			input:    "relaxed",
			expected: RateLimitRelaxed,
		},
		{
			name:     "Valid automatic mode",
			input:    "automatic",
			expected: RateLimitAutomatic,
		},
		{
			name:     "Valid conservative mode",
			input:    "conservative",
			expected: RateLimitConservative,
		},
		{
			name:     "Valid restricted mode",
			input:    "restricted",
			expected: RateLimitRestricted,
		},
		{
			name:     "Empty string defaults to automatic",
			input:    "",
			expected: RateLimitAutomatic,
		},
		{
			name:     "Invalid string defaults to automatic",
			input:    "unknown",
			expected: RateLimitAutomatic,
		},
		{
			name:     "Random string defaults to automatic",
			input:    "totally_invalid",
			expected: RateLimitAutomatic,
		},
		{
			name:     "Case sensitive - uppercase fails",
			input:    "AUTOMATIC",
			expected: RateLimitAutomatic,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ParseRateLimitingMode(tt.input)
			assert.Equal(t, tt.expected, result, "ParseRateLimitingMode() mismatch for input: %v", tt.input)
		})
	}
}

func TestIsRateLimitError(t *testing.T) {
	// This test already exists in ratelimit_test.go, but let's make sure it's comprehensive
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name: "Matrix 429 error with M_LIMIT_EXCEEDED",
			err: &Error{
				StatusCode: 429,
				ErrCode:    "M_LIMIT_EXCEEDED",
				ErrMsg:     "Too Many Requests",
			},
			expected: true,
		},
		{
			name: "Matrix error with M_LIMIT_EXCEEDED code only",
			err: &Error{
				StatusCode: 500,
				ErrCode:    "M_LIMIT_EXCEEDED",
				ErrMsg:     "Rate limit exceeded",
			},
			expected: true,
		},
		{
			name: "Matrix 429 error without M_LIMIT_EXCEEDED",
			err: &Error{
				StatusCode: 429,
				ErrCode:    "UNKNOWN",
				ErrMsg:     "Too Many Requests",
			},
			expected: true,
		},
		{
			name: "Non-rate limit Matrix error",
			err: &Error{
				StatusCode: 400,
				ErrCode:    "M_INVALID_PARAM",
				ErrMsg:     "Bad request",
			},
			expected: false,
		},
		{
			name:     "Wrapped rate limit error",
			err:      &Error{StatusCode: 429, ErrCode: "M_LIMIT_EXCEEDED"},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsRateLimitError(tt.err)
			assert.Equal(t, tt.expected, result, "IsRateLimitError() mismatch for error: %v", tt.err)
		})
	}
}
