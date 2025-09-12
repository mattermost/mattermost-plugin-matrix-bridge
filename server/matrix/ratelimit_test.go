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
	config := TestRateLimitConfig()

	assert.True(t, config.Enabled, "Test config should be enabled")
	// Test config uses fast rates (10x faster than production) for efficient testing
	assert.Equal(t, 0.5, config.RoomCreation.Rate, "Test config should have 0.5 rooms per second")
	assert.Equal(t, 2, config.RoomCreation.BurstSize, "Test config should have burst of 2 rooms")
	assert.Equal(t, time.Duration(0), config.RoomCreation.Interval, "Test config should use token bucket for room creation")
	assert.Equal(t, 2.0, config.Messages.Rate, "Test config should allow 2 messages per second")
	assert.Equal(t, 10, config.Messages.BurstSize, "Test config should have burst of 10 messages")
	assert.Equal(t, 3.0, config.Invites.Rate, "Test config should allow 3 invites per second")
	assert.Equal(t, 10, config.Invites.BurstSize, "Test config should have burst of 10 invites")
	assert.Equal(t, 1.7, config.Registration.Rate, "Test config should allow 1.7 registrations per second")
	assert.Equal(t, 3, config.Registration.BurstSize, "Test config should have burst of 3 registrations")
	assert.Equal(t, time.Duration(0), config.Registration.Interval, "Test config should use token bucket for registration")
	assert.Equal(t, 2.0, config.Joins.Rate, "Test config should allow 2 joins per second")
	assert.Equal(t, 5, config.Joins.BurstSize, "Test config should have burst of 5 joins")
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
