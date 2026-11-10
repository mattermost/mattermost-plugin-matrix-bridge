package harness

import (
	"context"
	"sync"
	"testing"
	"time"
)

var (
	sharedOnce sync.Once
	sharedEnv  *Env
	sharedErr  error

	// diagnosedTests keeps the failure-diagnostics cleanup to one per test when Shared is called
	// repeatedly on the same test.
	diagnosedTests sync.Map
)

// Shared returns the package-wide environment, starting it on first use. It skips the test
// under -short, and logs the plugin and Synapse log tails if the test fails.
func Shared(t *testing.T) *Env {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping e2e test in -short mode")
	}

	sharedOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		started := time.Now()
		sharedEnv, sharedErr = Start(ctx)
		if sharedErr == nil {
			t.Logf("e2e environment started in %s", time.Since(started).Round(time.Second))
		}
	})
	if sharedErr != nil {
		t.Fatalf("start e2e environment: %v", sharedErr)
	}

	env := sharedEnv
	if _, registered := diagnosedTests.LoadOrStore(t, true); !registered {
		t.Cleanup(func() {
			diagnosedTests.Delete(t)
			if t.Failed() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				t.Logf("e2e diagnostics:\n%s", env.diagnostics(ctx, true))
			}
		})
	}
	return env
}

// TerminateShared stops the shared environment if Shared started one.
func TerminateShared(ctx context.Context) error {
	if sharedEnv == nil {
		return nil
	}
	return sharedEnv.Terminate(ctx)
}
