package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
	tcexec "github.com/testcontainers/testcontainers-go/exec"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/e2e/harness"
)

// putTransaction runs inside the Synapse container, so it reaches Mattermost the same way
// Synapse's own Application Service transactions do.
const putTransaction = `import sys, urllib.request
req = urllib.request.Request(sys.argv[1], data=b'{"events":[]}', method="PUT",
    headers={"Authorization": "Bearer " + sys.argv[2], "Content-Type": "application/json"})
print(urllib.request.urlopen(req, timeout=10).status)`

func TestHarnessBoot(t *testing.T) {
	env := harness.Shared(t)

	t.Run("PluginRunning", func(t *testing.T) {
		statuses, _, err := env.Admin.GetPluginStatuses(t.Context())
		require.NoError(t, err)
		var state int
		for _, status := range statuses {
			if status.PluginId == harness.PluginID {
				state = status.State
			}
		}
		require.Equal(t, model.PluginStateRunning, state)
	})

	t.Run("ServerRegistered", func(t *testing.T) {
		resp := harness.PluginRequest(t, env.Admin, http.MethodGet, "/api/v1/servers", nil)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var list struct {
			Servers []struct {
				ServerID   string `json:"server_id"`
				ServerName string `json:"server_name"`
				RemoteID   string `json:"remote_id"`
			} `json:"servers"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&list))
		require.Len(t, list.Servers, 1)
		require.Equal(t, env.ServerID, list.Servers[0].ServerID)
		require.Equal(t, harness.ServerName, list.Servers[0].ServerName)
		require.Equal(t, env.RemoteID, list.Servers[0].RemoteID)
	})

	t.Run("RateLimitingDisabled", func(t *testing.T) {
		config, _, err := env.Admin.GetConfig(t.Context())
		require.NoError(t, err)
		require.Equal(t, "disabled", config.PluginSettings.Plugins[harness.PluginID]["rate_limiting_mode"])
	})

	t.Run("RemoteClusterRegistered", func(t *testing.T) {
		remotes, _, err := env.Admin.GetRemoteClusters(t.Context(), 0, 100, model.RemoteClusterQueryFilter{OnlyPlugins: true})
		require.NoError(t, err)
		var ids []string
		for _, remote := range remotes {
			ids = append(ids, remote.RemoteId)
		}
		require.Contains(t, ids, env.RemoteID)
	})

	t.Run("SynapseReachesPlugin", func(t *testing.T) {
		url := "http://mattermost:8065/plugins/" + harness.PluginID + "/_matrix/app/v1/transactions/boot-" + model.NewId()
		code, output, err := env.Synapse.Container.Exec(t.Context(),
			[]string{"python3", "-c", putTransaction, url, harness.HSToken}, tcexec.Multiplexed())
		require.NoError(t, err)
		out, err := io.ReadAll(output)
		require.NoError(t, err)
		require.Equal(t, 0, code, "transaction PUT from Synapse failed: %s", out)
		require.Equal(t, "200", strings.TrimSpace(string(out)))
	})

	t.Run("SharedOnce", func(t *testing.T) {
		require.Same(t, env, harness.Shared(t))
	})
}
