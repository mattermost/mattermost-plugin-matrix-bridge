package harness

import (
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"

	matrixtest "github.com/mattermost/mattermost-plugin-matrix-bridge/testcontainers/matrix"
)

// NewMattermostUser creates a team member with a unique username and returns it with a client
// logged in as that user.
func NewMattermostUser(t *testing.T) (*model.User, *model.Client4) {
	t.Helper()
	env := Shared(t)
	username := "user" + model.NewId()
	password := model.NewId()

	user, _, err := env.Admin.CreateUser(t.Context(), &model.User{
		Username: username,
		Email:    username + "@example.com",
		Password: password,
	})
	require.NoError(t, err, "create Mattermost user %s", username)
	_, _, err = env.Admin.AddTeamMember(t.Context(), env.Team.Id, user.Id)
	require.NoError(t, err, "add Mattermost user %s to team %s", username, env.Team.Id)

	client := model.NewAPIv4Client(env.Admin.URL)
	_, _, err = client.Login(t.Context(), username, password)
	require.NoError(t, err, "log in as Mattermost user %s", username)
	return user, client
}

// NewMatrixUser registers a Synapse user with a unique localpart and its own access token.
func NewMatrixUser(t *testing.T) *matrixtest.User {
	t.Helper()
	return Shared(t).Synapse.CreateUser(t, "user"+model.NewId(), model.NewId())
}
