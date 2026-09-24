package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/e2e/harness"
	matrixtest "github.com/mattermost/mattermost-plugin-matrix-bridge/testcontainers/matrix"
)

// requestTimeout bounds each helper request. Helpers use their own context rather than
// t.Context(), which Go cancels before cleanups run.
const requestTimeout = time.Minute

const (
	// outageWindow is how long a negative sync check waits. Keep it short: a longer outage pushes
	// Synapse's next retry, and so redelivery, further out.
	outageWindow = 5 * time.Second
	// synapseRecoveryTimeout bounds inbound delivery after an outage. Synapse retries failed
	// Application Service transactions with exponential backoff (2 s, 4 s, 8 s, ...), so the
	// first delivery after recovery waits for the next retry.
	synapseRecoveryTimeout = 2 * time.Minute
)

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

// mappingView is one row of the plugin's per-server mappings list.
type mappingView struct {
	ChannelID   string `json:"channel_id"`
	ChannelName string `json:"channel_name"`
	TeamName    string `json:"team_name"`
	RoomID      string `json:"room_id"`
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

func serverMappings(t *testing.T, client *model.Client4, serverID string) []mappingView {
	t.Helper()
	const perPage = 200
	var all []mappingView
	for page := 0; ; page++ {
		var resp struct {
			Mappings []mappingView `json:"mappings"`
		}
		path := fmt.Sprintf("%s/%s/mappings?page=%d&per_page=%d", serversPath, serverID, page, perPage)
		decodeJSON(t, requireStatus(t, client, http.MethodGet, path, nil, http.StatusOK), &resp)
		all = append(all, resp.Mappings...)
		if len(resp.Mappings) < perPage {
			return all
		}
	}
}

func findMapping(mappings []mappingView, channelID string) (mappingView, bool) {
	i := slices.IndexFunc(mappings, func(m mappingView) bool { return m.ChannelID == channelID })
	if i < 0 {
		return mappingView{}, false
	}
	return mappings[i], true
}

// preserveRegistry restores the shared server on cleanup, even if the test stops midway: it
// re-adopts the server if it was removed, re-enables it, and puts back its tokens and username
// prefix, then requires the registry to equal its state when this was called.
func preserveRegistry(t *testing.T, env *harness.Env) {
	t.Helper()
	isShared := func(s serverView) bool { return s.ServerID == env.ServerID }
	snapshot := listServers(t, env.Admin)
	i := slices.IndexFunc(snapshot, isShared)
	require.GreaterOrEqual(t, i, 0, "shared server %s is not registered", env.ServerID)
	original := snapshot[i]

	t.Cleanup(func() {
		if !slices.ContainsFunc(listServers(t, env.Admin), isShared) {
			requireStatus(t, env.Admin, http.MethodPost, serversPath, map[string]string{
				"server_url": env.Synapse.InternalURL,
				"as_token":   harness.ASToken,
				"hs_token":   harness.HSToken,
				"server_id":  env.ServerID,
			}, http.StatusCreated)
		}
		serverPath := serversPath + "/" + env.ServerID
		requireStatus(t, env.Admin, http.MethodPut, serverPath+"/enabled", map[string]bool{"enabled": true}, http.StatusOK)
		requireStatus(t, env.Admin, http.MethodPatch, serverPath, map[string]string{
			"as_token":        harness.ASToken,
			"hs_token":        harness.HSToken,
			"username_prefix": original.UsernamePrefix,
		}, http.StatusOK)
		require.Equal(t, snapshot, listServers(t, env.Admin), "shared server registry was not restored")
	})
}

func sendFromMatrix(t *testing.T, env *harness.Env, b *harness.BridgedChannel, label string) string {
	t.Helper()
	body := "e2e " + label + " from Matrix " + model.NewId()
	env.Synapse.SendEventAsUser(t, b.MatrixUser, b.RoomID, "m.room.message", map[string]any{
		"msgtype": "m.text",
		"body":    body,
	})
	return body
}

func postFromMattermost(t *testing.T, b *harness.BridgedChannel, label string) *model.Post {
	t.Helper()
	post, _, err := b.MattermostClient.CreatePost(t.Context(), &model.Post{
		ChannelId: b.Channel.Id,
		Message:   "e2e " + label + " from Mattermost " + model.NewId(),
	})
	require.NoError(t, err, "create post in channel %s", b.Channel.Id)
	return post
}

func waitInbound(t *testing.T, b *harness.BridgedChannel, body string, timeout time.Duration) *model.Post {
	t.Helper()
	return harness.WaitForPostWithin(t, b.MattermostClient, b.Channel.Id, func(p *model.Post) bool {
		return p.Message == body
	}, timeout)
}

// waitOutbound waits for post in the room and requires it to come from the author's ghost and
// carry the post's ID.
func waitOutbound(t *testing.T, env *harness.Env, b *harness.BridgedChannel, post *model.Post) {
	t.Helper()
	event := env.Synapse.WaitForRoomEvent(t, b.RoomID, func(e matrixtest.Event) bool {
		return e.Type == "m.room.message" && e.Content["body"] == post.Message
	})
	require.Equal(t, harness.GhostUserID(post.UserId), event.Sender)
	require.Equal(t, post.Id, event.Content["mattermost_post_id"])
}

// requireSyncBothWays requires a new post to reach the room as its author's ghost and a new
// Matrix message to reach the channel within inboundTimeout.
func requireSyncBothWays(t *testing.T, env *harness.Env, b *harness.BridgedChannel, inboundTimeout time.Duration) {
	t.Helper()
	waitOutbound(t, env, b, postFromMattermost(t, b, "sync"))
	waitInbound(t, b, sendFromMatrix(t, env, b, "sync"), inboundTimeout)
}

// requireNoSyncEitherWay sends a Matrix message, then a post, and requires that neither is
// bridged within outageWindow. It returns the Matrix message body, which Synapse redelivers once
// the plugin accepts transactions again.
func requireNoSyncEitherWay(t *testing.T, env *harness.Env, b *harness.BridgedChannel) string {
	t.Helper()
	// Matrix first, so Synapse's delivery attempt is rejected well inside the outage instead of
	// racing the re-enable, and the message only arrives through a retry.
	inbound := sendFromMatrix(t, env, b, "outage")
	outbound := postFromMattermost(t, b, "outage").Message

	harness.RequireNoPost(t, b.MattermostClient, b.Channel.Id, func(p *model.Post) bool {
		return p.Message == inbound
	}, outageWindow)
	// Room events stay in the timeline, so one check after the inbound wait covers the whole
	// outageWindow.
	env.Synapse.RequireNoRoomEvent(t, b.RoomID, func(e matrixtest.Event) bool {
		return e.Content["body"] == outbound
	}, matrixtest.PollInterval)
	return inbound
}

func TestServerManagementEnableDisable(t *testing.T) {
	env := harness.Shared(t)
	preserveRegistry(t, env)
	b := harness.NewBridgedChannel(t)
	serverPath := serversPath + "/" + env.ServerID

	for _, tc := range []struct {
		name       string
		setEnabled func(t *testing.T, enabled bool)
	}{
		{"REST", func(t *testing.T, enabled bool) {
			var resp serverResponse
			decodeJSON(t, requireStatus(t, env.Admin, http.MethodPut, serverPath+"/enabled", map[string]bool{"enabled": enabled}, http.StatusOK), &resp)
			require.Equal(t, enabled, resp.Server.Enabled)
		}},
		{"Slash", func(t *testing.T, enabled bool) {
			verb, state := "disable", "disabled"
			if enabled {
				verb, state = "enable", "enabled"
			}
			reply := runCommand(t, env.Admin, b.Channel.Id, "/matrix server "+verb+" "+env.ServerID)
			require.Equal(t, "✅ Server `"+env.ServerID+"` is now **"+state+"**.", reply)
			list := listServers(t, env.Admin)
			require.Len(t, list, 1)
			require.Equal(t, enabled, list[0].Enabled)
		}},
	} {
		// A failed case leaves Synapse backing off, which would fail the next case too.
		require.True(t, t.Run(tc.name, func(t *testing.T) {
			tc.setEnabled(t, false)
			require.Equal(t, http.StatusServiceUnavailable, transactionStatus(t, env, harness.HSToken))
			missed := requireNoSyncEitherWay(t, env, b)

			tc.setEnabled(t, true)
			enabled := time.Now()
			require.Equal(t, http.StatusOK, transactionStatus(t, env, harness.HSToken))
			waitInbound(t, b, missed, synapseRecoveryTimeout)
			t.Logf("Synapse redelivered the missed message %s after re-enabling", time.Since(enabled).Round(time.Millisecond))

			requireSyncBothWays(t, env, b, matrixtest.DefaultWaitTimeout)
		}))
	}
}

func TestServerManagementRemoveAndReadopt(t *testing.T) {
	env := harness.Shared(t)
	preserveRegistry(t, env)
	b := harness.NewBridgedChannel(t)
	mappingsPath := serversPath + "/" + env.ServerID + "/mappings"
	var missed string
	var readded time.Time

	// Each step depends on the one before, so the test stops at the first failing step.
	require.True(t, t.Run("Remove", func(t *testing.T) {
		var removed struct {
			ServerID        string `json:"server_id"`
			RecoveryCommand string `json:"recovery_command"`
		}
		decodeJSON(t, requireStatus(t, env.Admin, http.MethodDelete, serversPath+"/"+env.ServerID, nil, http.StatusOK), &removed)
		require.Equal(t, env.ServerID, removed.ServerID)
		require.True(t, strings.HasSuffix(removed.RecoveryCommand, "--server-id "+env.ServerID), "recovery command: %s", removed.RecoveryCommand)

		require.Empty(t, listServers(t, env.Admin))
		requireStatus(t, env.Admin, http.MethodGet, mappingsPath, nil, http.StatusNotFound)
		require.Equal(t, http.StatusUnauthorized, transactionStatus(t, env, harness.HSToken))
		requireShared(t, env, b.Channel.Id, false)

		missed = requireNoSyncEitherWay(t, env, b)
	}))

	require.True(t, t.Run("Readd", func(t *testing.T) {
		reply := runCommand(t, env.Admin, b.Channel.Id,
			"/matrix server add "+env.Synapse.InternalURL+" "+harness.ASToken+" "+harness.HSToken+" --server-id "+env.ServerID)
		readded = time.Now()
		requireNoTokens(t, reply, harness.ASToken, harness.HSToken)
		require.Contains(t, reply, "✅ **Matrix server added**")
		require.Contains(t, reply, env.ServerID)

		list := listServers(t, env.Admin)
		require.Len(t, list, 1)
		require.Equal(t, env.ServerID, list[0].ServerID)
		require.Equal(t, env.RemoteID, list[0].RemoteID, "re-adoption should restore the same remote")
		require.True(t, list[0].Enabled)

		mapping, found := findMapping(serverMappings(t, env.Admin, env.ServerID), b.Channel.Id)
		require.True(t, found, "channel %s is not listed in the re-adopted server's mappings", b.Channel.Id)
		require.Equal(t, b.RoomID, mapping.RoomID)
		require.Equal(t, http.StatusOK, transactionStatus(t, env, harness.HSToken))
	}))

	require.True(t, t.Run("InboundResumes", func(t *testing.T) {
		waitInbound(t, b, missed, synapseRecoveryTimeout)
		t.Logf("Synapse redelivered the missed message %s after re-adding", time.Since(readded).Round(time.Millisecond))
		waitInbound(t, b, sendFromMatrix(t, env, b, "readopted"), matrixtest.DefaultWaitTimeout)
	}))

	require.True(t, t.Run("OutboundStaysOff", func(t *testing.T) {
		// Removing the remote makes Mattermost drop the channel's invite to it, and re-adding the
		// server doesn't restore it.
		requireShared(t, env, b.Channel.Id, false)
		post := postFromMattermost(t, b, "readopted")
		env.Synapse.RequireNoRoomEvent(t, b.RoomID, func(e matrixtest.Event) bool {
			return e.Content["body"] == post.Message
		}, outageWindow)
	}))

	t.Run("RemapRestoresOutbound", func(t *testing.T) {
		ghost := harness.GhostUserID(b.MattermostUser.Id)
		require.True(t, slices.ContainsFunc(env.Synapse.GetRoomMembers(t, b.RoomID), func(m *matrixtest.RoomMember) bool {
			return m.UserID == ghost && m.Membership == "join"
		}), "ghost %s is no longer joined to room %s", ghost, b.RoomID)

		reply := runCommand(t, env.Admin, b.Channel.Id, "/matrix map "+b.RoomAlias)
		require.Contains(t, reply, "Mapping Saved")
		require.Contains(t, reply, "Channel sharing enabled")
		requireShared(t, env, b.Channel.Id, true)

		waitOutbound(t, env, b, postFromMattermost(t, b, "remapped"))
	})
}

// requireShared waits until the channel's share with the env's remote matches want.
func requireShared(t *testing.T, env *harness.Env, channelID string, want bool) {
	t.Helper()
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		ctx, cancel := helperContext()
		defer cancel()
		shared, err := harness.SharedWithRemote(ctx, env, channelID)
		if assert.NoError(c, err) {
			assert.Equal(c, want, shared, "channel %s shared with remote %s", channelID, env.RemoteID)
		}
	}, matrixtest.DefaultWaitTimeout, matrixtest.PollInterval)
}
