package e2e

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/e2e/harness"
	matrixtest "github.com/mattermost/mattermost-plugin-matrix-bridge/testcontainers/matrix"
)

func TestMain(m *testing.M) {
	code := m.Run()

	// Registry-mutating tests must restore the shared server in cleanups; a missed restore fails
	// the package run.
	checkCtx, checkCancel := context.WithTimeout(context.Background(), time.Minute)
	if err := harness.CheckSharedRegistry(checkCtx); err != nil {
		fmt.Fprintf(os.Stderr, "shared e2e registry check failed: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	checkCancel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	if err := harness.TerminateShared(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "failed to terminate e2e environment: %v\n", err)
	}
	cancel()
	matrixtest.CleanupAllContainers()

	os.Exit(code)
}
