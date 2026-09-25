// Command e2e-env starts the e2e environment (Postgres, Mattermost with the plugin, Synapse), prints
// its connection details as JSON, and keeps it running until SIGTERM or SIGINT. The Playwright
// suite in e2e/playwright uses it so the environment has a single implementation.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/e2e/harness"
	matrixtest "github.com/mattermost/mattermost-plugin-matrix-bridge/testcontainers/matrix"
)

type connection struct {
	MattermostURL      string `json:"mattermost_url"`
	SiteURL            string `json:"site_url"`
	AdminUsername      string `json:"admin_username"`
	AdminPassword      string `json:"admin_password"`
	TeamName           string `json:"team_name"`
	PluginID           string `json:"plugin_id"`
	SynapseInternalURL string `json:"synapse_internal_url"`
	SynapseServerName  string `json:"synapse_server_name"`
	ASToken            string `json:"as_token"`
	HSToken            string `json:"hs_token"`
	ServerID           string `json:"server_id"`
	RemoteID           string `json:"remote_id"`
}

func main() {
	os.Exit(run())
}

func run() int {
	noServer := flag.Bool("no-server", false, "start without registering Synapse with the plugin")
	out := flag.String("out", "", "also write the connection JSON to this file, removed on shutdown")
	flag.Parse()
	log.SetPrefix("e2e-env: ")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	var opts []harness.Option
	if *noServer {
		opts = append(opts, harness.WithoutServer())
	}

	log.Print("starting environment")
	started := time.Now()
	startCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	env, err := harness.Start(startCtx, opts...)
	cancel()
	if err != nil {
		log.Printf("start environment: %v", err)
		return 1
	}
	log.Printf("environment started in %s", time.Since(started).Round(time.Second))

	code := serve(ctx, env, *out)

	log.Print("stopping environment")
	stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := env.Terminate(stopCtx); err != nil {
		log.Printf("terminate environment: %v", err)
		code = 1
	}
	if *out != "" {
		if err := os.Remove(*out); err != nil && !os.IsNotExist(err) {
			log.Printf("remove %s: %v", *out, err)
		}
	}
	matrixtest.CleanupAllContainers()
	return code
}

// serve publishes the connection details and blocks until ctx ends. It returns 1 if the details
// couldn't be published; the caller tears the environment down either way.
func serve(ctx context.Context, env *harness.Env, out string) int {
	// A struct of strings always encodes.
	data, _ := json.MarshalIndent(connection{
		MattermostURL:      env.Admin.URL,
		SiteURL:            harness.SiteURL,
		AdminUsername:      harness.AdminUsername,
		AdminPassword:      harness.AdminPassword,
		TeamName:           env.Team.Name,
		PluginID:           harness.PluginID,
		SynapseInternalURL: env.Synapse.InternalURL,
		SynapseServerName:  harness.ServerName,
		ASToken:            harness.ASToken,
		HSToken:            harness.HSToken,
		ServerID:           env.ServerID,
		RemoteID:           env.RemoteID,
	}, "", "  ")
	data = append(data, '\n')

	if out != "" {
		if err := writeAtomic(out, data); err != nil {
			log.Printf("write %s: %v", out, err)
			return 1
		}
	}
	if _, err := os.Stdout.Write(data); err != nil {
		log.Printf("write stdout: %v", err)
		return 1
	}

	log.Print("ready; send SIGTERM or press Ctrl-C to stop")
	<-ctx.Done()
	return 0
}

// writeAtomic writes data to a temporary file next to path and renames it, so a reader never sees
// a partial file.
func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".e2e-env-*.json")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
