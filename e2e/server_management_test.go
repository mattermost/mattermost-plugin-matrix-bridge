package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

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

// checkStatuses lists the diagnostics as "key=status" in the order the plugin ran them.
func checkStatuses(diag harness.Diagnostics) []string {
	statuses := make([]string, 0, len(diag.Checks))
	for _, check := range diag.Checks {
		statuses = append(statuses, check.Key+"="+check.Status)
	}
	return statuses
}

func requireAllChecksOK(t *testing.T, diag harness.Diagnostics) {
	t.Helper()
	require.Equal(t, []string{"registry=ok", "client=ok", "connection=ok", "appservice=ok"}, checkStatuses(diag), "%+v", diag.Checks)
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

	channel, _, _ := newChannel(t, env, "Servers "+model.NewId(), 0)

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

func serverHealth(t *testing.T, client *model.Client4) map[string]string {
	t.Helper()
	var resp struct {
		Health map[string]string `json:"health"`
	}
	decodeJSON(t, requireStatus(t, client, http.MethodGet, serversPath+"/health", nil, http.StatusOK), &resp)
	return resp.Health
}

func TestServerManagementConnectionTest(t *testing.T) {
	env := harness.Shared(t)
	serverPath := serversPath + "/" + env.ServerID
	testCommands := []string{"/matrix test", "/matrix server test", "/matrix server test " + env.ServerID}
	successLines := []string{
		"✅ **Server URL:** " + env.Synapse.InternalURL,
		"✅ **Matrix Client:** Initialized",
		"✅ **Connection:** Successfully connected",
		"✅ **Application Service:** Permissions verified",
	}
	channel := harness.NewBridgedChannel(t).Channel

	requireHealthy := func(t *testing.T) {
		t.Helper()
		diag := diagnose(t, env.Admin, env.ServerID)
		requireAllChecksOK(t, diag)
		require.NotNil(t, diag.ServerInfo)
		require.Equal(t, "healthy", serverHealth(t, env.Admin)[env.ServerID])
	}

	t.Run("Healthy", func(t *testing.T) {
		requireHealthy(t)
		for _, command := range testCommands {
			reply := runCommand(t, env.Admin, channel.Id, command)
			for _, line := range successLines {
				require.Contains(t, reply, line, "%q reply", command)
			}
			require.NotContains(t, reply, "❌", "%q reply", command)
		}
	})

	t.Run("WrongASToken", func(t *testing.T) {
		preserveRegistry(t, env)
		wrong := "e2e-wrong-" + model.NewId()
		data := requireStatus(t, env.Admin, http.MethodPatch, serverPath, map[string]string{"as_token": wrong}, http.StatusOK)
		requireNoTokens(t, string(data), wrong, harness.ASToken, harness.HSToken)

		diag := diagnose(t, env.Admin, env.ServerID)
		require.Equal(t, []string{"registry=ok", "client=ok", "connection=fail", "appservice=skip"}, checkStatuses(diag), "%+v", diag.Checks)
		require.Contains(t, diag.Checks[2].Detail, "401")
		require.Equal(t, "unhealthy", serverHealth(t, env.Admin)[env.ServerID])

		for _, command := range testCommands {
			reply := runCommand(t, env.Admin, channel.Id, command)
			require.Contains(t, reply, "❌ **Connection:**", "%q reply", command)
			require.NotContains(t, reply, "✅ **Application Service:**", "%q reply", command)
		}

		requireStatus(t, env.Admin, http.MethodPatch, serverPath, map[string]string{"as_token": harness.ASToken}, http.StatusOK)
		requireHealthy(t)
	})

	t.Run("SyncAfterRestore", func(t *testing.T) {
		requireSyncBothWays(t, env, harness.NewBridgedChannel(t), matrixtest.DefaultWaitTimeout)
	})
}

// registration is the Application Service registration file the plugin renders for Synapse.
type registration struct {
	ID              string `yaml:"id"`
	URL             string `yaml:"url"`
	ASToken         string `yaml:"as_token"`
	HSToken         string `yaml:"hs_token"`
	SenderLocalpart string `yaml:"sender_localpart"`
	RateLimited     *bool  `yaml:"rate_limited"`
	Namespaces      struct {
		Users   []namespace `yaml:"users"`
		Aliases []namespace `yaml:"aliases"`
	} `yaml:"namespaces"`
}

type namespace struct {
	Exclusive bool
	Regex     string
}

// TestServerManagementRegistrationYAML compares the tokens and the whole YAML with require.True,
// so a failure never prints either.
func TestServerManagementRegistrationYAML(t *testing.T) {
	env := harness.Shared(t)
	b := harness.NewBridgedChannel(t)
	var content string

	require.True(t, t.Run("REST", func(t *testing.T) {
		ctx, cancel := helperContext()
		defer cancel()
		resp, err := harness.PluginRequestContext(ctx, env.Admin, http.MethodGet, serversPath+"/"+env.ServerID+"/registration", nil)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, "no-store", resp.Header.Get("Cache-Control"))

		var file struct {
			Filename string `json:"filename"`
			Content  string `json:"content"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&file), "decode registration response")
		require.Equal(t, "mattermost-bridge-"+env.ServerID+".yaml", file.Filename)
		content = file.Content

		var reg registration
		require.NoError(t, yaml.Unmarshal([]byte(content), &reg), "parse registration YAML")

		config, _, err := env.Admin.GetConfig(t.Context())
		require.NoError(t, err)
		// Synapse appends /_matrix/app/v1/... itself, so the URL is the plugin's base path.
		require.Equal(t, *config.ServiceSettings.SiteURL+"/plugins/"+harness.PluginID, reg.URL)
		require.True(t, reg.ASToken == harness.ASToken, "as_token does not match the registered token")
		require.True(t, reg.HSToken == harness.HSToken, "hs_token does not match the registered token")
		require.Equal(t, "mattermost-bridge-"+env.ServerID, reg.ID)
		require.Equal(t, "_mattermost_bot", reg.SenderLocalpart)
		require.NotNil(t, reg.RateLimited, "rate_limited is not set")
		require.False(t, *reg.RateLimited)

		require.Len(t, reg.Namespaces.Users, 1)
		require.True(t, reg.Namespaces.Users[0].Exclusive)
		users := regexp.MustCompile("^(?:" + reg.Namespaces.Users[0].Regex + ")$")
		require.True(t, users.MatchString(harness.GhostUserID(model.NewId())))
		require.False(t, users.MatchString("@someone:"+harness.ServerName))

		require.Len(t, reg.Namespaces.Aliases, 1)
		aliases := regexp.MustCompile("^(?:" + reg.Namespaces.Aliases[0].Regex + ")$")
		require.True(t, aliases.MatchString("#mattermost-bridge-x:"+harness.ServerName))
	}))

	// The harness gives Synapse SiteURL + "/plugins/" + PluginID as the Application Service URL,
	// so an inbound message proves the rendered URL is the one Synapse delivers to.
	t.Run("InboundThroughRenderedURL", func(t *testing.T) {
		waitInbound(t, b, sendFromMatrix(t, env, b, "registration"), matrixtest.DefaultWaitTimeout)
	})

	t.Run("Slash", func(t *testing.T) {
		for _, command := range []string{"/matrix server registration", "/matrix server registration " + env.ServerID} {
			reply := runCommand(t, env.Admin, b.Channel.Id, command)
			_, rest, found := strings.Cut(reply, "```yaml\n")
			require.True(t, found, "%q reply has no yaml code block", command)
			yamlBlock, _, found := strings.Cut(rest, "```")
			require.True(t, found, "%q reply has an unterminated yaml code block", command)
			require.True(t, yamlBlock == content, "%q YAML differs from the REST registration", command)
		}
	})
}

// newChannel creates an open channel in the env's team, with the admin as its creator and the
// given number of new Mattermost members. It returns the channel and the members' users and
// clients.
func newChannel(t *testing.T, env *harness.Env, name string, members int) (*model.Channel, []*model.User, []*model.Client4) {
	t.Helper()
	channel, _, err := env.Admin.CreateChannel(t.Context(), &model.Channel{
		TeamId:      env.Team.Id,
		Name:        strings.ToLower(strings.ReplaceAll(name, " ", "-")),
		DisplayName: name,
		Type:        model.ChannelTypeOpen,
	})
	require.NoError(t, err, "create channel %s", name)

	users := make([]*model.User, 0, members)
	clients := make([]*model.Client4, 0, members)
	for range members {
		user, client := harness.NewMattermostUser(t)
		_, _, err := env.Admin.AddChannelMember(t.Context(), channel.Id, user.Id)
		require.NoError(t, err, "add user %s to channel %s", user.Id, channel.Id)
		users = append(users, user)
		clients = append(clients, client)
	}
	return channel, users, clients
}

// requireJoined waits until every user in userIDs has joined the room, as seen by viewer.
func requireJoined(t *testing.T, env *harness.Env, viewer *matrixtest.User, roomID string, userIDs ...string) {
	t.Helper()
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		result, err := env.Synapse.DoAsUser(viewer, http.MethodGet, "/_matrix/client/v3/rooms/"+url.PathEscape(roomID)+"/joined_members", nil)
		if !assert.NoError(c, err) {
			return
		}
		body, _ := result.(map[string]any)
		joined, _ := body["joined"].(map[string]any)
		for _, userID := range userIDs {
			assert.Contains(c, joined, userID, "user has not joined room %s", roomID)
		}
	}, matrixtest.DefaultWaitTimeout, matrixtest.PollInterval)
}

func resolveAlias(t *testing.T, env *harness.Env, alias string) string {
	t.Helper()
	roomID, err := env.Synapse.Client.ResolveRoomAlias(alias)
	require.NoError(t, err, "resolve alias %s", alias)
	return roomID
}

func requireMapped(t *testing.T, env *harness.Env, channel *model.Channel, roomID string) {
	t.Helper()
	mapping, found := findMapping(serverMappings(t, env.Admin, env.ServerID), channel.Id)
	require.True(t, found, "channel %s is not in the server's mappings", channel.Id)
	require.Equal(t, roomID, mapping.RoomID)
	require.Equal(t, channel.DisplayName, mapping.ChannelName)
	require.Equal(t, env.Team.Name, mapping.TeamName)
}

func requireNotMapped(t *testing.T, env *harness.Env, channelID string) {
	t.Helper()
	_, found := findMapping(serverMappings(t, env.Admin, env.ServerID), channelID)
	require.False(t, found, "channel %s is still in the server's mappings", channelID)
}

func TestServerManagementMapChannel(t *testing.T) {
	env := harness.Shared(t)
	bot := env.Synapse.GetApplicationServiceBotUserID()

	for _, tc := range []struct {
		name     string
		byRoomID bool
	}{
		{"ByAlias", false},
		{"ByRoomID", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := model.NewId()
			channel, members, clients := newChannel(t, env, "Map "+tc.name+" "+id, 2)
			matrixUser := harness.NewMatrixUser(t)
			localpart := "map-" + id
			roomID, alias := env.Synapse.CreateRoomAsUser(t, matrixUser, "Map "+id, localpart)
			// The plugin names its bridge alias after the alias localpart, or for a room ID, after
			// the channel display name lowercased with spaces and "_" turned into "-". For these
			// underscore-free names that is channel.Name.
			identifier, bridgeName := alias, localpart
			if tc.byRoomID {
				identifier, bridgeName = roomID, channel.Name
			}

			reply := runCommand(t, env.Admin, channel.Id, "/matrix map "+identifier)
			require.Contains(t, reply, "Mapping Saved")
			require.Contains(t, reply, "Channel sharing enabled")
			require.NotContains(t, reply, "Could not auto-join")

			requireJoined(t, env, matrixUser, roomID, bot, harness.GhostUserID(channel.CreatorId),
				harness.GhostUserID(members[0].Id), harness.GhostUserID(members[1].Id))
			require.Equal(t, roomID, resolveAlias(t, env, "#mattermost-bridge-"+bridgeName+":"+harness.ServerName))
			requireMapped(t, env, channel, roomID)
			requireShared(t, env, channel.Id, true)

			requireSyncBothWays(t, env, &harness.BridgedChannel{
				Channel:          channel,
				RoomID:           roomID,
				MatrixUser:       matrixUser,
				MattermostUser:   members[0],
				MattermostClient: clients[0],
			}, matrixtest.DefaultWaitTimeout)
		})
	}

	t.Run("InvalidIdentifier", func(t *testing.T) {
		channel, _, _ := newChannel(t, env, "Map Invalid "+model.NewId(), 0)
		require.Contains(t, runCommand(t, env.Admin, channel.Id, "/matrix map not-a-room"), "Invalid room identifier format")
		requireNotMapped(t, env, channel.Id)
	})

	t.Run("NonexistentRoom", func(t *testing.T) {
		t.Skip("bug: /matrix map to a nonexistent room saves the mapping")
		channel, _, _ := newChannel(t, env, "Map Missing "+model.NewId(), 0)
		reply := runCommand(t, env.Admin, channel.Id, "/matrix map #missing-"+model.NewId()+":"+harness.ServerName)
		require.NotContains(t, reply, "Mapping Saved")
		requireNotMapped(t, env, channel.Id)
	})
}

// requireUnmapped runs /matrix unmap in the bridged channel and checks the mapping, the share,
// the room's bridge state, and sync in both directions are gone.
func requireUnmapped(t *testing.T, env *harness.Env, b *harness.BridgedChannel) {
	t.Helper()
	require.Contains(t, runCommand(t, env.Admin, b.Channel.Id, "/matrix unmap"), "✅ **Mapping Removed**")
	requireNotMapped(t, env, b.Channel.Id)
	requireShared(t, env, b.Channel.Id, false)

	// User-created rooms start without this state, and a fix for the user-owned-room bug may skip
	// clearing it, so it only has to be empty when present.
	state := env.Synapse.GetRoomState(t, b.RoomID)
	if i := slices.IndexFunc(state, func(e matrixtest.Event) bool { return e.Type == "com.mattermost.bridge.channel" }); i >= 0 {
		require.Empty(t, state[i].Content, "room %s still names its Mattermost channel", b.RoomID)
	}

	// The plugin answers 200 for events from unmapped rooms, so Synapse drops the message instead
	// of backing off, and it is never redelivered.
	requireNoSyncEitherWay(t, env, b)
}

func TestServerManagementUnmapChannel(t *testing.T) {
	env := harness.Shared(t)

	t.Run("CreatedRoom", func(t *testing.T) {
		id := model.NewId()
		channel, members, clients := newChannel(t, env, "Unmap "+id, 1)
		reply := runCommand(t, env.Admin, channel.Id, "/matrix create e2e-unmap-"+id+" publish=true")
		require.Contains(t, reply, "Room Created & Mapped")
		roomID := resolveAlias(t, env, "#mattermost-bridge-e2e-unmap-"+id+":"+harness.ServerName)
		requireShared(t, env, channel.Id, true)

		matrixUser := harness.NewMatrixUser(t)
		require.NoError(t, env.Synapse.JoinRoomAsUser(t, matrixUser.UserID, roomID))
		b := &harness.BridgedChannel{
			Channel:          channel,
			RoomID:           roomID,
			MatrixUser:       matrixUser,
			MattermostUser:   members[0],
			MattermostClient: clients[0],
		}
		requireSyncBothWays(t, env, b, matrixtest.DefaultWaitTimeout)

		requireUnmapped(t, env, b)
	})

	// The bot joins a user-created room at power level 0, so Synapse forbids its write of the
	// empty com.mattermost.bridge.channel state and unmap aborts.
	t.Run("UserOwnedRoom", func(t *testing.T) {
		t.Skip("bug: /matrix unmap fails on user-created rooms (bot lacks power to clear room state)")
		requireUnmapped(t, env, harness.NewBridgedChannel(t))
	})
}

func TestServerManagementCreateRoom(t *testing.T) {
	env := harness.Shared(t)
	id := model.NewId()
	name := "e2e-create-" + id
	channel, members, clients := newChannel(t, env, "Create "+id, 1)

	require.Contains(t, runCommand(t, env.Admin, channel.Id, "/matrix create "+name+" publish=true"), "Room Created & Mapped")

	roomID := resolveAlias(t, env, "#_mattermost_"+name+":"+harness.ServerName)
	require.Equal(t, roomID, resolveAlias(t, env, "#mattermost-bridge-"+name+":"+harness.ServerName))
	require.Equal(t, name, env.Synapse.GetRoomName(t, roomID))

	matrixUser := harness.NewMatrixUser(t)
	require.NoError(t, env.Synapse.JoinRoomAsUser(t, matrixUser.UserID, roomID))
	requireJoined(t, env, matrixUser, roomID, harness.GhostUserID(channel.CreatorId), harness.GhostUserID(members[0].Id))
	requireMapped(t, env, channel, roomID)
	requireShared(t, env, channel.Id, true)

	requireSyncBothWays(t, env, &harness.BridgedChannel{
		Channel:          channel,
		RoomID:           roomID,
		MatrixUser:       matrixUser,
		MattermostUser:   members[0],
		MattermostClient: clients[0],
	}, matrixtest.DefaultWaitTimeout)
}
