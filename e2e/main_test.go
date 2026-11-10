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

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	if err := harness.TerminateShared(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "failed to terminate e2e environment: %v\n", err)
	}
	cancel()
	matrixtest.CleanupAllContainers()

	os.Exit(code)
}
