package e2e

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/e2e/harness"
)

// requestTimeout bounds each helper request. Helpers use their own context rather than
// t.Context(), which Go cancels before cleanups run.
const requestTimeout = time.Minute

const serversPath = "/api/v1/servers"

// serverView is the plugin's REST shape for a registered server.
type serverView struct {
	ServerID       string `json:"server_id"`
	ServerURL      string `json:"server_url"`
	ServerName     string `json:"server_name"`
	Endpoint       string `json:"endpoint"`
	EventDomain    string `json:"event_domain"`
	UsernamePrefix string `json:"username_prefix"`
	Enabled        bool   `json:"enabled"`
	RemoteID       string `json:"remote_id"`
	HasASToken     bool   `json:"has_as_token"`
	HasHSToken     bool   `json:"has_hs_token"`
}

type serverResponse struct {
	Server   serverView `json:"server"`
	Warnings []string   `json:"warnings"`
}

func helperContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), requestTimeout)
}

// requireStatus sends a plugin request as client's user, requires the status, and returns the
// body. The body is printed on a mismatch, so don't use it where a successful response carries
// tokens.
func requireStatus(t *testing.T, client *model.Client4, method, path string, body any, want int) []byte {
	t.Helper()
	ctx, cancel := helperContext()
	defer cancel()
	resp, err := harness.PluginRequestContext(ctx, client, method, path, body)
	require.NoError(t, err, "%s %s", method, path)
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "read %s %s response", method, path)
	require.Equal(t, want, resp.StatusCode, "%s %s: %s", method, path, data)
	return data
}

func decodeJSON(t *testing.T, data []byte, target any) {
	t.Helper()
	require.NoError(t, json.Unmarshal(data, target), "decode %s", data)
}

func listServers(t *testing.T, client *model.Client4) []serverView {
	t.Helper()
	var list struct {
		Servers []serverView `json:"servers"`
	}
	decodeJSON(t, requireStatus(t, client, http.MethodGet, serversPath, nil, http.StatusOK), &list)
	return list.Servers
}

func diagnose(t *testing.T, client *model.Client4, serverID string) harness.Diagnostics {
	t.Helper()
	var diag harness.Diagnostics
	decodeJSON(t, requireStatus(t, client, http.MethodPost, serversPath+"/"+serverID+"/test", nil, http.StatusOK), &diag)
	return diag
}

func requireAllChecksOK(t *testing.T, diag harness.Diagnostics) {
	t.Helper()
	require.NotEmpty(t, diag.Checks)
	for _, check := range diag.Checks {
		require.Equal(t, "ok", check.Status, "check %s: %s", check.Key, check.Detail)
	}
}

// transactionStatus returns the webhook's status for an empty transaction sent with token.
func transactionStatus(t *testing.T, env *harness.Env, token string) int {
	t.Helper()
	ctx, cancel := helperContext()
	defer cancel()
	status, err := harness.TransactionStatus(ctx, env, token)
	require.NoError(t, err, "PUT webhook transaction")
	return status
}

// runCommand runs a slash command and returns its reply text. Failure messages name only the
// command's first words, because some commands carry tokens.
func runCommand(t *testing.T, client *model.Client4, channelID, command string) string {
	t.Helper()
	ctx, cancel := helperContext()
	defer cancel()
	resp, err := harness.ExecuteCommandContext(ctx, client, channelID, command)
	fields := strings.Fields(command)
	require.NoError(t, err, "execute %q in channel %s", strings.Join(fields[:min(3, len(fields))], " "), channelID)
	return resp.Text
}

// requireNoTokens fails if data contains a token value or a token key, without printing either.
func requireNoTokens(t *testing.T, data string, tokens ...string) {
	t.Helper()
	for i, token := range tokens {
		require.False(t, strings.Contains(data, token), "response contains token #%d", i+1)
	}
	for _, key := range []string{`"as_token"`, `"hs_token"`} {
		require.False(t, strings.Contains(data, key), "response contains a %s key", key)
	}
}

func TestServerManagementAddServer(t *testing.T) {
	env := harness.Dedicated(t, harness.WithoutServer())
	addBody := map[string]string{
		"server_url": env.Synapse.InternalURL,
		"as_token":   harness.ASToken,
		"hs_token":   harness.HSToken,
	}
	addCommand := "/matrix server add " + env.Synapse.InternalURL + " " + harness.ASToken + " " + harness.HSToken

	id := model.NewId()
	channel, _, err := env.Admin.CreateChannel(t.Context(), &model.Channel{
		TeamId:      env.Team.Id,
		Name:        "servers-" + id,
		DisplayName: "Servers " + id,
		Type:        model.ChannelTypeOpen,
	})
	require.NoError(t, err)

	var added serverView

	t.Run("Empty", func(t *testing.T) {
		require.Empty(t, listServers(t, env.Admin))
		require.Contains(t, runCommand(t, env.Admin, channel.Id, "/matrix server list"), "No Matrix servers are registered")
		require.Equal(t, http.StatusUnauthorized, transactionStatus(t, env, harness.HSToken))
	})

	t.Run("REST", func(t *testing.T) {
		data := requireStatus(t, env.Admin, http.MethodPost, serversPath, addBody, http.StatusCreated)
		requireNoTokens(t, string(data), harness.ASToken, harness.HSToken)

		var created serverResponse
		decodeJSON(t, data, &created)
		added = created.Server
		require.Equal(t, harness.ServerName, added.ServerName, "server_name should be discovered from Synapse")
		require.NotEmpty(t, added.ServerID)
		require.NotEmpty(t, added.RemoteID)
		require.True(t, added.HasASToken)
		require.True(t, added.HasHSToken)
		require.True(t, added.Enabled)
		require.Equal(t, "matrix", added.UsernamePrefix)
		require.Empty(t, created.Warnings)

		require.Equal(t, http.StatusOK, transactionStatus(t, env, harness.HSToken))
		requireAllChecksOK(t, diagnose(t, env.Admin, added.ServerID))
		remote, _, err := env.Admin.GetRemoteCluster(t.Context(), added.RemoteID)
		require.NoError(t, err)
		require.Equal(t, harness.PluginID, remote.PluginID)
	})

	t.Run("Duplicate", func(t *testing.T) {
		require.NotEmpty(t, added.ServerID, "needs the REST subtest")

		data := requireStatus(t, env.Admin, http.MethodPost, serversPath, addBody, http.StatusConflict)
		var apiErr struct {
			Message string `json:"message"`
		}
		decodeJSON(t, data, &apiErr)
		require.Contains(t, apiErr.Message, "already registered at this endpoint")

		reply := runCommand(t, env.Admin, channel.Id, addCommand)
		require.True(t, strings.HasPrefix(reply, "❌ Failed to add server"), "reply: %s", reply)
		require.Contains(t, reply, "already registered")

		require.Len(t, listServers(t, env.Admin), 1)
	})

	t.Run("Slash", func(t *testing.T) {
		require.NotEmpty(t, added.ServerID, "needs the REST subtest")
		requireStatus(t, env.Admin, http.MethodDelete, serversPath+"/"+added.ServerID, nil, http.StatusOK)

		reply := runCommand(t, env.Admin, channel.Id, addCommand)
		requireNoTokens(t, reply, harness.ASToken, harness.HSToken)
		require.Contains(t, reply, "✅ **Matrix server added**")
		require.Contains(t, reply, harness.ServerName)

		list := listServers(t, env.Admin)
		require.Len(t, list, 1)
		require.NotEqual(t, added.ServerID, list[0].ServerID, "a plain re-add should get a new server_id")
		require.Contains(t, reply, list[0].ServerID)
		require.Equal(t, harness.ServerName, list[0].ServerName)
		require.Equal(t, added.RemoteID, list[0].RemoteID, "re-adding the same endpoint should restore its remote")
		require.Equal(t, http.StatusOK, transactionStatus(t, env, harness.HSToken))
		added = list[0]
	})

	t.Run("ServerNameOverride", func(t *testing.T) {
		require.NotEmpty(t, added.ServerID, "needs the REST subtest")
		requireStatus(t, env.Admin, http.MethodDelete, serversPath+"/"+added.ServerID, nil, http.StatusOK)

		body := maps.Clone(addBody)
		body["server_name"] = "override.e2e.local"
		var created serverResponse
		decodeJSON(t, requireStatus(t, env.Admin, http.MethodPost, serversPath, body, http.StatusCreated), &created)
		require.Equal(t, "override.e2e.local", created.Server.ServerName)

		requireStatus(t, env.Admin, http.MethodDelete, serversPath+"/"+created.Server.ServerID, nil, http.StatusOK)
		require.Empty(t, listServers(t, env.Admin))
	})
}
