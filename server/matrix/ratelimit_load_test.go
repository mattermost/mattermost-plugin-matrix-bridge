package matrix

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestTokenBucket_HighTrafficLoad tests that TokenBucket can handle high traffic
// and properly throttles requests under extreme load
func TestTokenBucket_HighTrafficLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping load test in short mode")
	}

	config := TokenBucketConfig{
		Rate:      0, // Disable token bucket
		BurstSize: 0,
		Interval:  10 * time.Millisecond, // 10ms interval = max 100 requests/second
	}

	tb := NewTokenBucket(config)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	const numWorkers = 50
	const requestsPerWorker = 20
	const expectedMaxThroughput = 100 // requests per second

	var (
		totalRequests   int64
		successRequests int64
		wg              sync.WaitGroup
	)

	startTime := time.Now()

	// Launch workers that hammer the rate limiter
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for j := 0; j < requestsPerWorker; j++ {
				atomic.AddInt64(&totalRequests, 1)

				// Try to get permission
				if err := tb.Wait(ctx); err == nil {
					atomic.AddInt64(&successRequests, 1)
				} else if err == context.DeadlineExceeded {
					// Expected when load exceeds capacity
					break
				} else {
					t.Errorf("Worker %d: Unexpected error: %v", workerID, err)
					break
				}
			}
		}(i)
	}

	wg.Wait()
	elapsed := time.Since(startTime)

	totalReqs := atomic.LoadInt64(&totalRequests)
	successReqs := atomic.LoadInt64(&successRequests)
	actualThroughput := float64(successReqs) / elapsed.Seconds()

	t.Logf("Load test results:")
	t.Logf("  Total requests attempted: %d", totalReqs)
	t.Logf("  Successful requests: %d", successReqs)
	t.Logf("  Elapsed time: %v", elapsed)
	t.Logf("  Actual throughput: %.1f requests/second", actualThroughput)
	t.Logf("  Expected max throughput: %d requests/second", expectedMaxThroughput)

	// Verify that rate limiting worked
	assert.Greater(t, totalReqs, successReqs, "Should have throttled some requests under high load")
	assert.LessOrEqual(t, actualThroughput, float64(expectedMaxThroughput)*1.1, "Should not exceed expected throughput by more than 10%%")
	assert.GreaterOrEqual(t, actualThroughput, float64(expectedMaxThroughput)*0.8, "Should achieve at least 80%% of expected throughput")
}

// TestClient_MessageSpamLoad tests that message rate limiting prevents overwhelming Matrix server
func TestClient_MessageSpamLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping load test in short mode")
	}

	// Use load test configuration optimized for measurable throttling
	config := LoadTestRateLimitConfig()

	logger := NewTestLogger(t)
	client := NewClientWithLoggerAndRateLimit("http://localhost:1", "test_token", "test_remote", logger, config)

	req := MessageRequest{
		RoomID:      "!spam-test:example.invalid",
		GhostUserID: "@spammer:example.invalid",
		Message:     "Spam message content",
	}

	const numSpammers = 20
	const messagesPerSpammer = 10
	const testDuration = 3 * time.Second
	const expectedMaxRate = 5.0 // messages per second

	var (
		totalAttempts int64
		rateLimited   int64
		networkErrors int64
		wg            sync.WaitGroup
	)

	startTime := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), testDuration)
	defer cancel()

	// Launch spammer goroutines
	for i := 0; i < numSpammers; i++ {
		wg.Add(1)
		go func(_ int) {
			defer wg.Done()
			for j := 0; j < messagesPerSpammer && ctx.Err() == nil; j++ {
				atomic.AddInt64(&totalAttempts, 1)

				start := time.Now()
				_, err := client.SendMessage(req)
				elapsed := time.Since(start)

				if err != nil {
					if elapsed > 100*time.Millisecond {
						// Likely rate limited (took significant time)
						atomic.AddInt64(&rateLimited, 1)
					} else {
						// Quick failure likely network error
						atomic.AddInt64(&networkErrors, 1)
					}
				}

				// Small delay to prevent busy spinning
				time.Sleep(time.Millisecond)
			}
		}(i)
	}

	wg.Wait()
	elapsed := time.Since(startTime)

	attempts := atomic.LoadInt64(&totalAttempts)
	throttled := atomic.LoadInt64(&rateLimited)
	netErrs := atomic.LoadInt64(&networkErrors)

	// Calculate effective rate if no rate limiting occurred
	potentialRate := float64(attempts) / elapsed.Seconds()
	throttlePercentage := float64(throttled) / float64(attempts) * 100

	t.Logf("Message spam load test results:")
	t.Logf("  Total message attempts: %d", attempts)
	t.Logf("  Rate limited: %d (%.1f%%)", throttled, throttlePercentage)
	t.Logf("  Network errors: %d", netErrs)
	t.Logf("  Test duration: %v", elapsed)
	t.Logf("  Potential rate without limiting: %.1f messages/second", potentialRate)
	t.Logf("  Expected max rate: %.1f messages/second", expectedMaxRate)

	// Verify that rate limiting is working effectively
	assert.Greater(t, attempts, int64(0), "Should have attempted some messages")
	assert.Greater(t, throttled, int64(0), "Should have rate limited some messages under high load")
	assert.Greater(t, throttlePercentage, 50.0, "Should have throttled significant percentage of spam attempts")

	// Verify we're not allowing way more than the configured rate
	if potentialRate > expectedMaxRate*2 {
		t.Errorf("Rate limiting may be ineffective: potential rate %.1f exceeds expected max rate %.1f by more than 2x",
			potentialRate, expectedMaxRate)
	}
}

// TestClient_RoomCreationSpamLoad tests room creation rate limiting under heavy load
func TestClient_RoomCreationSpamLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping load test in short mode")
	}

	// Use load test configuration optimized for measurable throttling
	config := LoadTestRateLimitConfig()

	logger := NewTestLogger(t)
	client := NewClientWithLoggerAndRateLimit("http://localhost:1", "test_token", "test_remote", logger, config)

	const numCreators = 10
	const roomsPerCreator = 5
	const testDuration = 4 * time.Second

	var (
		totalAttempts int64
		rateLimited   int64
		networkErrors int64
		wg            sync.WaitGroup
	)

	startTime := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), testDuration)
	defer cancel()

	// Launch room creator goroutines
	for i := 0; i < numCreators; i++ {
		wg.Add(1)
		go func(_ int) {
			defer wg.Done()
			for j := 0; j < roomsPerCreator && ctx.Err() == nil; j++ {
				atomic.AddInt64(&totalAttempts, 1)

				roomName := func() string { return "Spam Room" }() // Use closure to avoid name conflicts

				start := time.Now()
				_, err := client.CreateRoom(roomName, "", "test.example.invalid", true, "")
				elapsed := time.Since(start)

				if err != nil {
					if elapsed > 200*time.Millisecond {
						// Likely rate limited
						atomic.AddInt64(&rateLimited, 1)
					} else {
						// Quick failure likely network error
						atomic.AddInt64(&networkErrors, 1)
					}
				}

				// Small delay
				time.Sleep(10 * time.Millisecond)
			}
		}(i)
	}

	wg.Wait()
	elapsed := time.Since(startTime)

	attempts := atomic.LoadInt64(&totalAttempts)
	throttled := atomic.LoadInt64(&rateLimited)
	netErrs := atomic.LoadInt64(&networkErrors)

	potentialRate := float64(attempts) / elapsed.Seconds()
	throttlePercentage := float64(throttled) / float64(attempts) * 100
	expectedMaxRate := 2.0 // 1 room per 500ms = 2 rooms per second

	t.Logf("Room creation spam load test results:")
	t.Logf("  Total room creation attempts: %d", attempts)
	t.Logf("  Rate limited: %d (%.1f%%)", throttled, throttlePercentage)
	t.Logf("  Network errors: %d", netErrs)
	t.Logf("  Test duration: %v", elapsed)
	t.Logf("  Potential rate without limiting: %.1f rooms/second", potentialRate)
	t.Logf("  Expected max rate: %.1f rooms/second", expectedMaxRate)

	// Verify room creation rate limiting is working
	assert.Greater(t, attempts, int64(0), "Should have attempted some room creations")
	assert.Greater(t, throttled, int64(0), "Should have rate limited some room creation attempts")
	assert.Greater(t, throttlePercentage, 30.0, "Should have throttled significant percentage of room creation spam")
}

// TestClient_MixedOperationLoad tests rate limiting with mixed operations under load
func TestClient_MixedOperationLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping load test in short mode")
	}

	// Use load test configuration optimized for measurable throttling
	config := LoadTestRateLimitConfig()

	logger := NewTestLogger(t)
	client := NewClientWithLoggerAndRateLimit("http://localhost:1", "test_token", "test_remote", logger, config)

	const numWorkers = 15
	const operationsPerWorker = 10
	const testDuration = 5 * time.Second

	var (
		messageAttempts  int64
		messageThrottled int64
		roomAttempts     int64
		roomThrottled    int64
		wg               sync.WaitGroup
	)

	startTime := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), testDuration)
	defer cancel()

	// Launch mixed operation workers
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func(_ int) {
			defer wg.Done()

			for j := 0; j < operationsPerWorker && ctx.Err() == nil; j++ {
				// Alternate between messages and room creation
				if j%3 == 0 {
					// Room creation (less frequent)
					atomic.AddInt64(&roomAttempts, 1)
					start := time.Now()
					_, err := client.CreateRoom("Mixed Load Room", "", "test.example.invalid", true, "")
					elapsed := time.Since(start)

					if err != nil && elapsed > 200*time.Millisecond {
						atomic.AddInt64(&roomThrottled, 1)
					}
				} else {
					// Message sending (more frequent)
					atomic.AddInt64(&messageAttempts, 1)
					start := time.Now()
					_, err := client.SendMessage(MessageRequest{
						RoomID:      "!mixed-load:example.invalid",
						GhostUserID: "@mixed-user:example.invalid",
						Message:     "Mixed load test message",
					})
					elapsed := time.Since(start)

					if err != nil && elapsed > 100*time.Millisecond {
						atomic.AddInt64(&messageThrottled, 1)
					}
				}

				// Brief pause
				time.Sleep(20 * time.Millisecond)
			}
		}(i)
	}

	wg.Wait()
	elapsed := time.Since(startTime)

	msgAttempts := atomic.LoadInt64(&messageAttempts)
	msgThrottled := atomic.LoadInt64(&messageThrottled)
	rmAttempts := atomic.LoadInt64(&roomAttempts)
	rmThrottled := atomic.LoadInt64(&roomThrottled)

	msgThrottleRate := float64(msgThrottled) / float64(msgAttempts) * 100
	roomThrottleRate := float64(rmThrottled) / float64(rmAttempts) * 100

	t.Logf("Mixed operation load test results:")
	t.Logf("  Test duration: %v", elapsed)
	t.Logf("  Message attempts: %d, throttled: %d (%.1f%%)", msgAttempts, msgThrottled, msgThrottleRate)
	t.Logf("  Room creation attempts: %d, throttled: %d (%.1f%%)", rmAttempts, rmThrottled, roomThrottleRate)

	// Verify that rate limiting is working for mixed operations
	assert.Greater(t, msgAttempts, int64(0), "Should have attempted messages")
	assert.Greater(t, rmAttempts, int64(0), "Should have attempted room creations")

	// Room creation should be more aggressively throttled due to stricter limits
	if rmAttempts > 5 {
		assert.Greater(t, roomThrottleRate, msgThrottleRate, "Room creation should be throttled more aggressively than messages")
	}

	// Both should show some throttling under mixed high load
	if msgAttempts > 20 {
		assert.Greater(t, msgThrottleRate, 10.0, "Should throttle some messages under high mixed load")
	}
}

// TestTokenBucket_StressTest tests TokenBucket under extreme concurrent stress
func TestTokenBucket_StressTest(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping stress test in short mode")
	}

	config := TokenBucketConfig{
		Rate:      100.0, // 100 operations per second
		BurstSize: 50,    // Allow burst of 50
		Interval:  0,
	}

	tb := NewTokenBucket(config)

	const numGoroutines = 100
	const operationsPerGoroutine = 100
	const testDuration = 3 * time.Second

	var (
		totalOperations   int64
		allowedOperations int64
		timeoutOperations int64
		wg                sync.WaitGroup
	)

	ctx, cancel := context.WithTimeout(context.Background(), testDuration)
	defer cancel()

	startTime := time.Now()

	// Launch stress test goroutines
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(_ int) {
			defer wg.Done()

			for j := 0; j < operationsPerGoroutine; j++ {
				if ctx.Err() != nil {
					break
				}

				atomic.AddInt64(&totalOperations, 1)

				// Use a short timeout to avoid blocking too long
				opCtx, opCancel := context.WithTimeout(ctx, 100*time.Millisecond)
				err := tb.Wait(opCtx)
				opCancel()

				switch err {
				case nil:
					atomic.AddInt64(&allowedOperations, 1)
				case context.DeadlineExceeded:
					atomic.AddInt64(&timeoutOperations, 1)
				}

				// Brief pause to avoid busy spinning
				time.Sleep(time.Microsecond * 100)
			}
		}(i)
	}

	wg.Wait()
	elapsed := time.Since(startTime)

	total := atomic.LoadInt64(&totalOperations)
	allowed := atomic.LoadInt64(&allowedOperations)
	timeouts := atomic.LoadInt64(&timeoutOperations)

	allowedRate := float64(allowed) / elapsed.Seconds()
	timeoutPercentage := float64(timeouts) / float64(total) * 100

	t.Logf("TokenBucket stress test results:")
	t.Logf("  Test duration: %v", elapsed)
	t.Logf("  Total operations attempted: %d", total)
	t.Logf("  Operations allowed: %d", allowed)
	t.Logf("  Operations timed out: %d (%.1f%%)", timeouts, timeoutPercentage)
	t.Logf("  Effective allowed rate: %.1f operations/second", allowedRate)
	t.Logf("  Expected max rate: 100 operations/second")

	// Verify stress test results
	assert.Greater(t, total, int64(1000), "Should have attempted many operations")
	assert.Greater(t, allowed, int64(100), "Should have allowed reasonable number of operations")
	assert.Greater(t, timeoutPercentage, 50.0, "Should have timed out majority of operations under extreme stress")
	assert.LessOrEqual(t, allowedRate, 120.0, "Should not exceed configured rate by more than 20%% even under stress")
	assert.GreaterOrEqual(t, allowedRate, 80.0, "Should achieve at least 80%% of configured rate under stress")
}

// TestClient_RateLimitingEffectiveness_Integration tests that rate limiting
// actually prevents the issues it's designed to solve
func TestClient_RateLimitingEffectiveness_Integration(t *testing.T) {
	// This test verifies that our rate limiting would prevent the original
	// 429 M_LIMIT_EXCEEDED errors we were seeing in CI

	// Use standard test config which provides fast but consistent throttling validation
	config := TestRateLimitConfig()
	logger := NewTestLogger(t)
	client := NewClientWithLoggerAndRateLimit("http://localhost:1", "test_token", "test_remote", logger, config)

	// Calculate expected timing thresholds from actual config
	var expectedThrottleDelay time.Duration
	if config.Messages.Interval > 0 {
		// Interval-based rate limiting
		expectedThrottleDelay = config.Messages.Interval
	} else if config.Messages.Rate > 0 {
		// Token bucket rate limiting: 1/rate = seconds per token
		expectedThrottleDelay = time.Duration(float64(time.Second) / config.Messages.Rate)
	}

	// Use 80% of expected delay as threshold to account for timing variations
	throttleThreshold := time.Duration(float64(expectedThrottleDelay) * 0.8)

	// Simulate the rapid operations that were causing failures
	const rapidOperations = 20

	// Test rapid message sending (what was failing)
	messageReq := MessageRequest{
		RoomID:      "!integration-test:example.invalid",
		GhostUserID: "@integration-test:example.invalid",
		Message:     "Integration test message",
	}

	var messageDurations []time.Duration
	for i := 0; i < rapidOperations; i++ {
		start := time.Now()
		_, err := client.SendMessage(messageReq)
		duration := time.Since(start)
		messageDurations = append(messageDurations, duration)

		// We expect network errors, but importantly, no rate limiting should cause long delays
		// beyond the first few burst messages
		assert.Error(t, err, "Expected network error")
	}

	// Analyze the timing pattern
	immediateCount := 0
	throttledCount := 0

	for i, duration := range messageDurations {
		if duration < 50*time.Millisecond {
			immediateCount++
		} else if duration > throttleThreshold {
			throttledCount++
		}

		t.Logf("Message %d: %v", i+1, duration)
	}

	t.Logf("Integration test results:")
	t.Logf("  Rate limit config: %v messages/sec, burst: %d", config.Messages.Rate, config.Messages.BurstSize)
	t.Logf("  Expected throttle delay: %v, threshold: %v", expectedThrottleDelay, throttleThreshold)
	t.Logf("  Immediate messages (burst): %d", immediateCount)
	t.Logf("  Throttled messages: %d", throttledCount)

	// Verify that rate limiting is working as designed
	assert.Greater(t, immediateCount, 0, "Should allow some immediate messages (burst)")
	// With current test config (1.0 msgs/sec, burst 15), we expect most messages to be immediate
	// and only messages beyond the burst to be throttled
	expectedThrottled := max(0, rapidOperations-config.Messages.BurstSize)
	assert.GreaterOrEqual(t, throttledCount, expectedThrottled, "Should throttle messages beyond burst capacity")

	// Most importantly: if we were actually hitting a Matrix server, these delays
	// should prevent 429 M_LIMIT_EXCEEDED errors because we're self-limiting
	// to be more restrictive than typical Matrix server limits
}
