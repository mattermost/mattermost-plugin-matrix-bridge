package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
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

// Dedicated starts an environment for this test alone and terminates it on cleanup, logging the
// plugin and Synapse log tails first if the test failed. It skips the test under -short.
func Dedicated(t *testing.T, opts ...Option) *Env {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping e2e test in -short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	started := time.Now()
	env, err := Start(ctx, opts...)
	if err != nil {
		t.Fatalf("start dedicated e2e environment: %v", err)
	}
	t.Logf("dedicated e2e environment started in %s", time.Since(started).Round(time.Second))

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if t.Failed() {
			t.Logf("e2e diagnostics:\n%s", env.diagnostics(ctx, true))
		}
		if err := env.Terminate(ctx); err != nil {
			t.Errorf("terminate dedicated e2e environment: %v", err)
		}
	})
	return env
}

// CheckSharedRegistry reports drift in the shared environment's server registry: the server list
// must equal the one Start registered, every connection check must pass (catching a wrong
// as_token), and Synapse's hs_token must still be accepted. It returns nil if Shared never
// started an environment.
func CheckSharedRegistry(ctx context.Context) error {
	env := sharedEnv
	if env == nil {
		return nil
	}

	var list struct {
		Servers []map[string]any `json:"servers"`
	}
	if err := getJSON(ctx, env, http.MethodGet, "/api/v1/servers", &list); err != nil {
		return err
	}
	if want := []map[string]any{env.registered}; !reflect.DeepEqual(list.Servers, want) {
		return fmt.Errorf("server registry drifted: got %v, want %v", list.Servers, want)
	}

	var diagnostics Diagnostics
	if err := getJSON(ctx, env, http.MethodPost, "/api/v1/servers/"+env.ServerID+"/test", &diagnostics); err != nil {
		return err
	}
	var errs []error
	for _, check := range diagnostics.Checks {
		if check.Status != "ok" {
			errs = append(errs, fmt.Errorf("server check %s is %s: %s", check.Key, check.Status, check.Detail))
		}
	}

	status, err := TransactionStatus(ctx, env, HSToken)
	if err != nil {
		errs = append(errs, fmt.Errorf("webhook transaction: %w", err))
	} else if status != http.StatusOK {
		errs = append(errs, fmt.Errorf("webhook transaction with Synapse's hs_token returned %d, want 200", status))
	}
	return errors.Join(errs...)
}

func getJSON(ctx context.Context, env *Env, method, path string, target any) error {
	resp, err := PluginRequestContext(ctx, env.Admin, method, path, nil)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%s %s: read body: %w", method, path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("%s %s: decode: %w", method, path, err)
	}
	return nil
}

// TerminateShared stops the shared environment if Shared started one.
func TerminateShared(ctx context.Context) error {
	if sharedEnv == nil {
		return nil
	}
	return sharedEnv.Terminate(ctx)
}
