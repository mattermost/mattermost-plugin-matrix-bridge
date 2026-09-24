// Package harness starts a real Mattermost + Synapse environment for the e2e suite.
package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"

	matrixtest "github.com/mattermost/mattermost-plugin-matrix-bridge/testcontainers/matrix"
)

//nolint:gosec // test fixture credentials, not real secrets
const (
	HSToken       = "e2e_hs_token"
	asToken       = "e2e_as_token"
	adminUsername = "admin"
	adminPassword = "e2e-admin-password"
)

const (
	PluginID = "com.mattermost.plugin-matrix-bridge"
	// ServerName is Synapse's server_name, which the plugin discovers when registering it.
	ServerName = "e2e.matrix.local"

	teamName              = "test"
	mattermostInternalURL = "http://mattermost:8065"
	synapseAlias          = "synapse"

	mattermostLogLines = 100
	synapseLogLines    = 50
)

// Env is a running environment: Postgres, Mattermost with the plugin, and Synapse on one network.
type Env struct {
	Network    *testcontainers.DockerNetwork
	Synapse    *matrixtest.Container
	Postgres   *testcontainers.DockerContainer
	Mattermost *testcontainers.DockerContainer
	Admin      *model.Client4
	Team       *model.Team
	ServerID   string
	ServerName string
	RemoteID   string
}

// Start boots the environment and registers Synapse through the plugin's REST API. It needs the
// plugin bundle path in E2E_PLUGIN_BUNDLE. On failure it tears down whatever already started and
// returns an error that includes the container log tails.
func Start(ctx context.Context) (*Env, error) {
	bundle, err := bundlePath()
	if err != nil {
		return nil, err
	}

	env := &Env{ServerName: ServerName}
	if err := env.start(ctx, bundle); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		// Startup usually fails before the plugin logs anything, so keep every Mattermost line.
		if diagnostics := env.diagnostics(cleanupCtx, false); diagnostics != "" {
			err = fmt.Errorf("%w\n%s", err, diagnostics)
		}
		if termErr := env.Terminate(cleanupCtx); termErr != nil {
			err = errors.Join(err, fmt.Errorf("terminate partial environment: %w", termErr))
		}
		return nil, err
	}
	return env, nil
}

func (e *Env) start(ctx context.Context, bundle string) error {
	var err error
	if e.Network, err = network.New(ctx); err != nil {
		return fmt.Errorf("create docker network: %w", err)
	}

	e.Synapse, err = matrixtest.Start(ctx, matrixtest.MatrixTestConfig{
		ServerName:    ServerName,
		ASToken:       asToken,
		HSToken:       HSToken,
		Network:       e.Network,
		NetworkAlias:  synapseAlias,
		AppServiceURL: mattermostInternalURL + "/plugins/" + PluginID,
	}, nil)
	if err != nil {
		return fmt.Errorf("start Synapse: %w", err)
	}

	// On failure these return the created container (if any), so Terminate still stops it.
	if e.Postgres, err = startPostgres(ctx, e.Network); err != nil {
		return fmt.Errorf("start Postgres: %w", err)
	}
	if e.Mattermost, err = startMattermost(ctx, e.Network); err != nil {
		return fmt.Errorf("start Mattermost: %w", err)
	}

	if err := e.setupMattermost(ctx, bundle); err != nil {
		return err
	}
	if err := e.waitForPluginRunning(ctx); err != nil {
		return err
	}
	if err := e.registerServer(ctx); err != nil {
		return err
	}
	return e.waitForRemoteOnline(ctx)
}

// Terminate stops Mattermost, Postgres, then Synapse, then removes the network.
// It is safe on a partially started Env.
func (e *Env) Terminate(ctx context.Context) error {
	var errs []error
	if e.Mattermost != nil {
		errs = append(errs, e.Mattermost.Terminate(ctx))
	}
	if e.Postgres != nil {
		errs = append(errs, e.Postgres.Terminate(ctx))
	}
	if e.Synapse != nil {
		errs = append(errs, e.Synapse.Terminate(ctx))
	}
	if e.Network != nil {
		errs = append(errs, e.Network.Remove(ctx))
	}
	return errors.Join(errs...)
}

// diagnostics returns the tail of the Mattermost log (only plugin lines when pluginOnly) and the
// tail of the Synapse log. Containers that were never created are skipped.
func (e *Env) diagnostics(ctx context.Context, pluginOnly bool) string {
	var b strings.Builder
	if e.Mattermost != nil {
		filter, label := "", "all lines"
		if pluginOnly {
			filter, label = PluginID, "plugin lines"
		}
		logs, err := containerLogs(ctx, e.Mattermost)
		if err != nil {
			logs = "(unavailable: " + err.Error() + ")"
		} else {
			logs = tail(logs, mattermostLogLines, filter)
		}
		fmt.Fprintf(&b, "--- Mattermost log (%s, last %d) ---\n%s\n", label, mattermostLogLines, logs)
	}
	if e.Synapse != nil {
		logs, err := e.Synapse.Logs(ctx, synapseLogLines)
		if err != nil {
			logs = "(unavailable: " + err.Error() + ")"
		}
		fmt.Fprintf(&b, "--- Synapse log (last %d) ---\n%s\n", synapseLogLines, logs)
	}
	return b.String()
}

func bundlePath() (string, error) {
	bundle := os.Getenv("E2E_PLUGIN_BUNDLE")
	if bundle == "" {
		return "", errors.New("E2E_PLUGIN_BUNDLE is not set; run the e2e tests with `make e2e`")
	}
	if _, err := os.Stat(bundle); err != nil {
		return "", fmt.Errorf("E2E_PLUGIN_BUNDLE %q is not readable (%w); run `make e2e` to build it", bundle, err)
	}
	return bundle, nil
}

func (e *Env) waitForPluginRunning(ctx context.Context) error {
	lastState := "no status yet"
	err := poll(ctx, 2*time.Minute, func(ctx context.Context) (bool, error) {
		statuses, _, err := e.Admin.GetPluginStatuses(ctx)
		if err != nil {
			lastState = "status request failed: " + err.Error()
			return false, nil
		}
		for _, status := range statuses {
			if status.PluginId == PluginID {
				lastState = fmt.Sprintf("state %d", status.State)
				return status.State == model.PluginStateRunning, nil
			}
		}
		lastState = "not installed"
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("plugin %s never reached running (%s): %w", PluginID, lastState, err)
	}
	return nil
}

func (e *Env) registerServer(ctx context.Context) error {
	body := map[string]string{
		"server_url": e.Synapse.InternalURL,
		"as_token":   asToken,
		"hs_token":   HSToken,
	}

	var status int
	var respBody []byte
	err := poll(ctx, 30*time.Second, func(ctx context.Context) (bool, error) {
		resp, err := pluginRequest(ctx, e.Admin, http.MethodPost, "/api/v1/servers", body)
		if err != nil {
			return false, err
		}
		defer func() { _ = resp.Body.Close() }()
		status = resp.StatusCode
		respBody, err = io.ReadAll(resp.Body)
		// Mattermost 404s /plugins/<id> until the plugin is active.
		return status != http.StatusNotFound, err
	})
	if err != nil {
		return fmt.Errorf("register Matrix server: %w", err)
	}
	if status != http.StatusCreated {
		hint := ""
		if status == http.StatusInternalServerError {
			hint = " (if the plugin log says it failed to register the server for shared channels, Connected Workspaces may need a license)"
		}
		return fmt.Errorf("register Matrix server: status %d: %s%s", status, strings.TrimSpace(string(respBody)), hint)
	}

	var created struct {
		Server struct {
			ServerID   string `json:"server_id"`
			ServerName string `json:"server_name"`
			RemoteID   string `json:"remote_id"`
		} `json:"server"`
	}
	if err := json.Unmarshal(respBody, &created); err != nil {
		return fmt.Errorf("decode register response: %w", err)
	}
	if created.Server.ServerName != ServerName {
		return fmt.Errorf("discovered server_name %q, want %q", created.Server.ServerName, ServerName)
	}
	if created.Server.RemoteID == "" {
		return errors.New("registered server has no remote_id")
	}
	e.ServerID = created.Server.ServerID
	e.RemoteID = created.Server.RemoteID
	return nil
}

// waitForRemoteOnline waits for Mattermost's first successful ping of the plugin remote. Until
// then Mattermost holds channel invites to it, so newly bridged channels would not sync.
func (e *Env) waitForRemoteOnline(ctx context.Context) error {
	lastState := "no response yet"
	err := poll(ctx, 2*time.Minute, func(ctx context.Context) (bool, error) {
		remote, _, err := e.Admin.GetRemoteCluster(ctx, e.RemoteID)
		if err != nil {
			lastState = "request failed: " + err.Error()
			return false, nil
		}
		lastState = fmt.Sprintf("last ping at %d", remote.LastPingAt)
		return remote.IsOnline(), nil
	})
	if err != nil {
		return fmt.Errorf("plugin remote %s never came online (%s): %w", e.RemoteID, lastState, err)
	}
	return nil
}

// poll calls check every PollInterval until it reports done or fails, or until timeout or ctx
// ends. check gets the bounded context so its requests stop with the poll.
func poll(ctx context.Context, timeout time.Duration, check func(ctx context.Context) (bool, error)) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(matrixtest.PollInterval)
	defer ticker.Stop()
	for {
		done, err := check(ctx)
		if err != nil || done {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("gave up after %s: %w", timeout, ctx.Err())
		case <-ticker.C:
		}
	}
}

func containerLogs(ctx context.Context, container testcontainers.Container) (string, error) {
	reader, err := container.Logs(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = reader.Close() }()
	data, err := io.ReadAll(reader)
	return string(data), err
}

// tail returns the last n lines of logs that contain filter.
func tail(logs string, n int, filter string) string {
	var lines []string
	for line := range strings.SplitSeq(logs, "\n") {
		if strings.Contains(line, filter) {
			lines = append(lines, line)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
