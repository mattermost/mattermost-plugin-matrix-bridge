package harness

import (
	"context"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	matrixtest "github.com/mattermost/mattermost-plugin-matrix-bridge/testcontainers/matrix"
)

const recentPosts = 100

// WaitForPost polls the channel's most recent posts, as seen by client, until one matches and
// returns it.
func WaitForPost(t *testing.T, client *model.Client4, channelID string, match func(*model.Post) bool) *model.Post {
	t.Helper()
	return WaitForPostWithin(t, client, channelID, match, matrixtest.DefaultWaitTimeout)
}

// WaitForPostWithin is WaitForPost with a caller-chosen timeout.
func WaitForPostWithin(t *testing.T, client *model.Client4, channelID string, match func(*model.Post) bool, timeout time.Duration) *model.Post {
	t.Helper()
	var found *model.Post
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		post, err := findPost(t.Context(), client, channelID, match)
		if !assert.NoError(c, err) {
			return
		}
		if post == nil {
			c.Errorf("no matching post yet in channel %s", channelID)
			return
		}
		found = post
	}, timeout, matrixtest.PollInterval)
	return found
}

// RequireNoPost fails if a matching post appears in the channel during window or in a final
// check after it; only the final check's fetch errors fail the test.
func RequireNoPost(t *testing.T, client *model.Client4, channelID string, match func(*model.Post) bool, window time.Duration) {
	t.Helper()
	require.Never(t, func() bool {
		post, _ := findPost(t.Context(), client, channelID, match)
		return post != nil
	}, window, matrixtest.PollInterval, "unexpected matching post in channel %s", channelID)

	post, err := findPost(t.Context(), client, channelID, match)
	require.NoError(t, err, "fetch posts for channel %s", channelID)
	require.Nil(t, post, "unexpected matching post in channel %s", channelID)
}

func findPost(ctx context.Context, client *model.Client4, channelID string, match func(*model.Post) bool) (*model.Post, error) {
	list, _, err := client.GetPostsForChannel(ctx, channelID, 0, recentPosts, "", false, false)
	if err != nil {
		return nil, err
	}
	for _, id := range list.Order {
		if post := list.Posts[id]; match(post) {
			return post, nil
		}
	}
	return nil, nil
}
