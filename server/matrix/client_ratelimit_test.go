package matrix

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClient_SendMessage_RateLimiting(t *testing.T) {
	// Create a client with aggressive rate limiting for messages
	config := RateLimitConfig{
		Enabled: true,
		Messages: TokenBucketConfig{
			Rate:      0, // Disable token bucket
			BurstSize: 0,
			Interval:  100 * time.Millisecond, // 100ms minimum interval between messages
		},
		RoomCreation: TokenBucketConfig{Rate: 10, BurstSize: 5}, // Permissive room creation
		Invites:      TokenBucketConfig{Rate: 10, BurstSize: 5}, // Permissive invites
	}

	logger := NewTestLogger(t)
	client := NewClientWithLoggerAndRateLimit("http://localhost:1", "test_token", "test_remote", logger, config)

	req := MessageRequest{
		RoomID:      "!test:example.invalid",
		GhostUserID: "@test:example.invalid",
		Message:     "Test message",
	}

	// First message should succeed quickly
	start := time.Now()
	_, err := client.SendMessage(req)
	elapsed := time.Since(start)

	// We expect this to fail with a network error since we're not actually connecting
	// But we want to verify the rate limiting timing, not the network call
	assert.Error(t, err, "Expected network error")
	assert.Less(t, elapsed, 50*time.Millisecond, "First message should not be rate limited")

	// Second message should be rate limited
	start = time.Now()
	_, err = client.SendMessage(req)
	elapsed = time.Since(start)

	assert.Error(t, err, "Expected network error")
	assert.GreaterOrEqual(t, elapsed, 90*time.Millisecond, "Second message should be rate limited")
	assert.Less(t, elapsed, 150*time.Millisecond, "Rate limiting should not be excessive")
}

func TestClient_CreateRoom_RateLimiting(t *testing.T) {
	// Create a client with aggressive rate limiting for room creation
	config := RateLimitConfig{
		Enabled: true,
		RoomCreation: TokenBucketConfig{
			Rate:      0, // Disable token bucket
			BurstSize: 0,
			Interval:  100 * time.Millisecond, // 100ms minimum interval between room creation
		},
		Messages: TokenBucketConfig{Rate: 10, BurstSize: 5}, // Permissive messages
		Invites:  TokenBucketConfig{Rate: 10, BurstSize: 5}, // Permissive invites
	}

	logger := NewTestLogger(t)
	client := NewClientWithLoggerAndRateLimit("http://localhost:1", "test_token", "test_remote", logger, config)

	// First room creation should succeed quickly
	start := time.Now()
	_, err := client.CreateRoom("Test Room 1", "", "test.example.invalid", true, "")
	elapsed := time.Since(start)

	// We expect this to fail with a network error since we're not actually connecting
	assert.Error(t, err, "Expected network error")
	assert.Less(t, elapsed, 50*time.Millisecond, "First room creation should not be rate limited")

	// Second room creation should be rate limited
	start = time.Now()
	_, err = client.CreateRoom("Test Room 2", "", "test.example.invalid", true, "")
	elapsed = time.Since(start)

	assert.Error(t, err, "Expected network error")
	assert.GreaterOrEqual(t, elapsed, 90*time.Millisecond, "Second room creation should be rate limited")
	assert.Less(t, elapsed, 150*time.Millisecond, "Rate limiting should not be excessive")
}

func TestClient_ConcurrentMessageSending_RateLimiting(t *testing.T) {
	// Test that concurrent message sending is properly throttled
	config := RateLimitConfig{
		Enabled: true,
		Messages: TokenBucketConfig{
			Rate:      0, // Disable token bucket
			BurstSize: 0,
			Interval:  50 * time.Millisecond, // 50ms minimum interval
		},
		RoomCreation: TokenBucketConfig{Rate: 10, BurstSize: 5}, // Permissive
		Invites:      TokenBucketConfig{Rate: 10, BurstSize: 5}, // Permissive
	}

	logger := NewTestLogger(t)
	client := NewClientWithLoggerAndRateLimit("http://localhost:1", "test_token", "test_remote", logger, config)

	req := MessageRequest{
		RoomID:      "!test:example.invalid",
		GhostUserID: "@test:example.invalid",
		Message:     "Concurrent test message",
	}

	const numMessages = 5
	var wg sync.WaitGroup
	results := make(chan time.Duration, numMessages)

	// Send multiple messages concurrently
	for i := 0; i < numMessages; i++ {
		wg.Add(1)
		go func(_ int) {
			defer wg.Done()
			start := time.Now()
			_, err := client.SendMessage(req)
			elapsed := time.Since(start)
			// We expect network errors, but we care about timing
			assert.Error(t, err)
			results <- elapsed
		}(i)
	}

	wg.Wait()
	close(results)

	// Collect all durations
	var durations []time.Duration
	for duration := range results {
		durations = append(durations, duration)
	}

	require.Len(t, durations, numMessages)

	// First message should be fast, subsequent should be increasingly delayed
	immediateCount := 0
	for _, duration := range durations {
		if duration < 30*time.Millisecond {
			immediateCount++
		}
	}

	// Only one message should be immediate
	assert.Equal(t, 1, immediateCount, "Only first message should be immediate under concurrent load")
}

func TestClient_RateLimiting_Disabled(t *testing.T) {
	// Test that when rate limiting is disabled, operations are immediate
	config := GetRateLimitConfigByMode(RateLimitDisabled)

	logger := NewTestLogger(t)
	client := NewClientWithLoggerAndRateLimit("http://localhost:1", "test_token", "test_remote", logger, config)

	req := MessageRequest{
		RoomID:      "!test:example.invalid",
		GhostUserID: "@test:example.invalid",
		Message:     "Test message",
	}

	// Multiple rapid messages should all be immediate when rate limiting is disabled
	for i := 0; i < 3; i++ {
		start := time.Now()
		_, err := client.SendMessage(req)
		elapsed := time.Since(start)

		assert.Error(t, err, "Expected network error")
		assert.Less(t, elapsed, 20*time.Millisecond, "Message %d should be immediate when rate limiting disabled", i+1)
	}
}

func TestClient_RateLimiting_ContextTimeout(t *testing.T) {
	// Test that rate limiting respects context timeouts
	config := RateLimitConfig{
		Enabled: true,
		Messages: TokenBucketConfig{
			Rate:      0,
			BurstSize: 0,
			Interval:  500 * time.Millisecond, // Long interval
		},
		RoomCreation: TokenBucketConfig{Rate: 10, BurstSize: 5},
		Invites:      TokenBucketConfig{Rate: 10, BurstSize: 5},
	}

	logger := NewTestLogger(t)
	client := NewClientWithLoggerAndRateLimit("http://localhost:1", "test_token", "test_remote", logger, config)

	req := MessageRequest{
		RoomID:      "!test:example.invalid",
		GhostUserID: "@test:example.invalid",
		Message:     "Test message",
	}

	// First message to consume initial allowance
	_, err := client.SendMessage(req)
	assert.Error(t, err, "Expected network error")

	// Create a client with timeout that's shorter than rate limit interval
	// Note: We can't easily test this with the current SendMessage implementation
	// because it creates its own context. This test documents the intended behavior.

	// For now, just verify that the rate limiter would timeout
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	if client.messageLimiter != nil {
		start := time.Now()
		err := client.messageLimiter.Wait(ctx)
		elapsed := time.Since(start)

		require.Error(t, err)
		assert.Equal(t, context.DeadlineExceeded, err)
		assert.GreaterOrEqual(t, elapsed, 90*time.Millisecond)
	}
}

func TestClient_TokenBucketBurstBehavior(t *testing.T) {
	// Test token bucket burst behavior with messages
	config := RateLimitConfig{
		Enabled: true,
		Messages: TokenBucketConfig{
			Rate:      2.0, // 2 tokens per second = 500ms per token
			BurstSize: 3,   // Allow burst of 3 messages
			Interval:  0,   // No interval-based limiting
		},
		RoomCreation: TokenBucketConfig{Rate: 10, BurstSize: 5},
		Invites:      TokenBucketConfig{Rate: 10, BurstSize: 5},
	}

	logger := NewTestLogger(t)
	client := NewClientWithLoggerAndRateLimit("http://localhost:1", "test_token", "test_remote", logger, config)

	req := MessageRequest{
		RoomID:      "!test:example.invalid",
		GhostUserID: "@test:example.invalid",
		Message:     "Burst test message",
	}

	// First 3 messages should be immediate (burst)
	for i := 0; i < 3; i++ {
		start := time.Now()
		_, err := client.SendMessage(req)
		elapsed := time.Since(start)

		assert.Error(t, err, "Expected network error")
		assert.Less(t, elapsed, 30*time.Millisecond, "Burst message %d should be immediate", i+1)
	}

	// 4th message should be rate limited (burst exhausted)
	start := time.Now()
	_, err := client.SendMessage(req)
	elapsed := time.Since(start)

	assert.Error(t, err, "Expected network error")
	assert.GreaterOrEqual(t, elapsed, 450*time.Millisecond, "Message after burst should wait ~500ms")
	assert.Less(t, elapsed, 600*time.Millisecond, "Rate limiting should not be excessive")
}

func TestClient_MixedOperations_IndependentRateLimiting(t *testing.T) {
	// Test that different operations have independent rate limiting
	config := RateLimitConfig{
		Enabled: true,
		Messages: TokenBucketConfig{
			Rate:      0,
			BurstSize: 0,
			Interval:  100 * time.Millisecond,
		},
		RoomCreation: TokenBucketConfig{
			Rate:      0,
			BurstSize: 0,
			Interval:  200 * time.Millisecond, // Different interval
		},
		Invites: TokenBucketConfig{Rate: 10, BurstSize: 5}, // Permissive
	}

	logger := NewTestLogger(t)
	client := NewClientWithLoggerAndRateLimit("http://localhost:1", "test_token", "test_remote", logger, config)

	// Send message (consumes message rate limit)
	start := time.Now()
	_, err := client.SendMessage(MessageRequest{
		RoomID:      "!test:example.invalid",
		GhostUserID: "@test:example.invalid",
		Message:     "Test message",
	})
	elapsed := time.Since(start)
	assert.Error(t, err)
	assert.Less(t, elapsed, 30*time.Millisecond, "First message should be immediate")

	// Create room immediately after (should not be affected by message rate limit)
	start = time.Now()
	_, err = client.CreateRoom("Test Room", "", "test.example.invalid", true, "")
	elapsed = time.Since(start)
	assert.Error(t, err)
	assert.Less(t, elapsed, 30*time.Millisecond, "Room creation should not be affected by message rate limit")

	// Second message should be rate limited
	start = time.Now()
	_, err = client.SendMessage(MessageRequest{
		RoomID:      "!test:example.invalid",
		GhostUserID: "@test:example.invalid",
		Message:     "Second message",
	})
	elapsed = time.Since(start)
	assert.Error(t, err)
	assert.GreaterOrEqual(t, elapsed, 70*time.Millisecond, "Second message should be rate limited")

	// Second room creation should be rate limited (but differently than messages)
	start = time.Now()
	_, err = client.CreateRoom("Test Room 2", "", "test.example.invalid", true, "")
	elapsed = time.Since(start)
	assert.Error(t, err)
	assert.GreaterOrEqual(t, elapsed, 80*time.Millisecond, "Second room creation should be rate limited with 200ms interval")
}

func TestClient_RateLimitError_Detection(t *testing.T) {
	// Test that client properly detects when it should have prevented rate limit errors
	// This is more of a documentation test for the intended behavior

	config := UnitTestRateLimitConfig() // Use unit test config with predictable values
	logger := NewTestLogger(t)
	client := NewClientWithLoggerAndRateLimit("http://localhost:1", "test_token", "test_remote", logger, config)

	// Verify that rate limiters are properly initialized
	assert.NotNil(t, client.messageLimiter, "Message limiter should be initialized")
	assert.NotNil(t, client.roomCreationLimiter, "Room creation limiter should be initialized")
	assert.NotNil(t, client.inviteLimiter, "Invite limiter should be initialized")
	assert.NotNil(t, client.registrationLimiter, "Registration limiter should be initialized")
	assert.NotNil(t, client.joinLimiter, "Join limiter should be initialized")

	// Test that rate limit detection works
	rateLimitErr := &Error{
		StatusCode: 429,
		ErrCode:    "M_LIMIT_EXCEEDED",
		ErrMsg:     "Too Many Requests",
	}

	assert.True(t, IsRateLimitError(rateLimitErr), "Should detect rate limit error")

	// Test that the client's rate limiting should prevent such errors
	// In a real scenario, if we get a rate limit error, it means our client-side
	// rate limiting was insufficient
}

func TestNewTokenBucket_ConfigValidation(t *testing.T) {
	tests := []struct {
		name   string
		config TokenBucketConfig
		desc   string
	}{
		{
			name:   "interval_based",
			config: TokenBucketConfig{Rate: 0, BurstSize: 0, Interval: 100 * time.Millisecond},
			desc:   "Interval-based rate limiting should work",
		},
		{
			name:   "token_based",
			config: TokenBucketConfig{Rate: 1.0, BurstSize: 5, Interval: 0},
			desc:   "Token-based rate limiting should work",
		},
		{
			name:   "disabled",
			config: TokenBucketConfig{Rate: 0, BurstSize: 0, Interval: 0},
			desc:   "Disabled rate limiting should allow all operations",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tb := NewTokenBucket(tt.config)
			assert.NotNil(t, tb, tt.desc)

			if tt.config.Interval == 0 && tt.config.Rate == 0 {
				// Disabled case - should always allow
				for i := 0; i < 10; i++ {
					assert.True(t, tb.Allow(), "Disabled rate limiting should always allow")
				}
			} else {
				// Should allow at least first operation
				assert.True(t, tb.Allow(), "Should allow at least first operation")
			}
		})
	}
}

// Benchmark tests to ensure rate limiting doesn't add excessive overhead
func BenchmarkTokenBucket_Allow_Interval(b *testing.B) {
	config := TokenBucketConfig{
		Rate:      0,
		BurstSize: 0,
		Interval:  time.Nanosecond, // Very short interval for benchmarking
	}
	tb := NewTokenBucket(config)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tb.Allow()
	}
}

func BenchmarkTokenBucket_Allow_Token(b *testing.B) {
	config := TokenBucketConfig{
		Rate:      1000000.0, // Very high rate for benchmarking
		BurstSize: 1000000,
		Interval:  0,
	}
	tb := NewTokenBucket(config)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tb.Allow()
	}
}

func BenchmarkClient_SendMessage_WithRateLimit(b *testing.B) {
	// Use disabled rate limiting for pure performance benchmarking
	config := GetRateLimitConfigByMode(RateLimitDisabled)

	logger := NewTestLogger(b)
	client := NewClientWithLoggerAndRateLimit("http://localhost:1", "test_token", "test_remote", logger, config)

	req := MessageRequest{
		RoomID:      "!test:example.invalid",
		GhostUserID: "@test:example.invalid",
		Message:     "Benchmark message",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// We expect this to fail with network error, but we're measuring the overhead
		// of rate limiting checks, not actual network calls
		_, _ = client.SendMessage(req)
	}
}
