package harness

import (
	"context"
	"net/url"
	"slices"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	matrixtest "github.com/mattermost/mattermost-plugin-matrix-bridge/testcontainers/matrix"
)

// BridgedChannel is a Mattermost channel mapped to its own Matrix room through /matrix map.
// MattermostUser is a channel member and MatrixUser created the room.
type BridgedChannel struct {
	Channel          *model.Channel
	RoomID           string
	RoomAlias        string
	MatrixUser       *matrixtest.User
	MattermostUser   *model.User
	MattermostClient *model.Client4
}

// GhostUserID returns the Matrix user that the bridge posts as for a Mattermost user.
func GhostUserID(mmUserID string) string {
	return "@_mattermost_" + mmUserID + ":" + ServerName
}

// ExecuteCommand runs a slash command in the channel as the client's user.
func ExecuteCommand(t *testing.T, client *model.Client4, channelID, command string) *model.CommandResponse {
	t.Helper()
	resp, err := ExecuteCommandContext(t.Context(), client, channelID, command)
	require.NoError(t, err, "execute %q in channel %s", command, channelID)
	return resp
}

// ExecuteCommandContext is ExecuteCommand with a caller-chosen context, for use in cleanups.
func ExecuteCommandContext(ctx context.Context, client *model.Client4, channelID, command string) (*model.CommandResponse, error) {
	resp, _, err := client.ExecuteCommand(ctx, channelID, command)
	return resp, err
}

// NewBridgedChannel creates a channel, a Matrix room, and a user on each side, then maps them
// with /matrix map as the admin. It returns once the bot has joined the room and the channel is
// shared with the server's remote.
func NewBridgedChannel(t *testing.T) *BridgedChannel {
	t.Helper()
	env := Shared(t)
	id := model.NewId()

	channel, _, err := env.Admin.CreateChannel(t.Context(), &model.Channel{
		TeamId:      env.Team.Id,
		Name:        "bridged-" + id,
		DisplayName: "Bridged " + id,
		Type:        model.ChannelTypeOpen,
	})
	require.NoError(t, err, "create channel bridged-%s", id)

	mmUser, mmClient := NewMattermostUser(t)
	_, _, err = env.Admin.AddChannelMember(t.Context(), channel.Id, mmUser.Id)
	require.NoError(t, err, "add user %s to channel %s", mmUser.Id, channel.Id)

	matrixUser := NewMatrixUser(t)
	roomID, alias := env.Synapse.CreateRoomAsUser(t, matrixUser, "Bridged "+id, "bridged-"+id)

	command := "/matrix map " + alias
	resp := ExecuteCommand(t, env.Admin, channel.Id, command)

	bot := env.Synapse.GetApplicationServiceBotUserID()
	membershipPath := "/_matrix/client/v3/rooms/" + url.PathEscape(roomID) + "/state/m.room.member/" + url.PathEscape(bot)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		result, err := env.Synapse.DoAsUser(matrixUser, "GET", membershipPath, nil)
		if !assert.NoError(c, err) {
			return
		}
		state, _ := result.(map[string]any)
		membership, _ := state["membership"].(string)
		assert.Equal(c, "join", membership)
	}, matrixtest.DefaultWaitTimeout, matrixtest.PollInterval,
		"bot %s never joined room %s for channel %s after %q; command response: %s", bot, roomID, channel.Id, command, resp.Text)

	require.EventuallyWithT(t, func(c *assert.CollectT) {
		shared, err := SharedWithRemote(t.Context(), env, channel.Id)
		if assert.NoError(c, err) {
			assert.True(c, shared, "channel %s not confirmed as shared yet", channel.Id)
		}
	}, matrixtest.DefaultWaitTimeout, matrixtest.PollInterval,
		"channel %s was never shared with remote %s after %q; command response: %s", channel.Id, env.RemoteID, command, resp.Text)

	return &BridgedChannel{
		Channel:          channel,
		RoomID:           roomID,
		RoomAlias:        alias,
		MatrixUser:       matrixUser,
		MattermostUser:   mmUser,
		MattermostClient: mmClient,
	}
}

// SharedWithRemote reports whether the env's remote has a confirmed invite to the channel. The
// endpoint ignores a channel filter, so it pages through every channel shared with the remote.
func SharedWithRemote(ctx context.Context, env *Env, channelID string) (bool, error) {
	const perPage = 200
	for page := 0; ; page++ {
		remotes, _, err := env.Admin.GetSharedChannelRemotesByRemoteCluster(ctx, env.RemoteID,
			model.SharedChannelRemoteFilterOpts{}, page, perPage)
		if err != nil {
			return false, err
		}
		if slices.ContainsFunc(remotes, func(r *model.SharedChannelRemote) bool { return r.ChannelId == channelID }) {
			return true, nil
		}
		if len(remotes) < perPage {
			return false, nil
		}
	}
}
