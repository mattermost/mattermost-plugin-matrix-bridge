package harness

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	defaultMattermostImage = "mattermost/mattermost-enterprise-edition:11.8.5"
	postgresImage          = "docker.io/postgres:15.2-alpine"
	mattermostPort         = "8065/tcp"
)

func startPostgres(ctx context.Context, nw *testcontainers.DockerNetwork) (*testcontainers.DockerContainer, error) {
	return testcontainers.Run(ctx, postgresImage,
		testcontainers.WithEnv(map[string]string{
			"POSTGRES_DB":       "mattermost",
			"POSTGRES_USER":     "mmuser",
			"POSTGRES_PASSWORD": "mmpassword",
		}),
		network.WithNetwork([]string{"db"}, nw),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(time.Minute)),
	)
}

// startMattermost starts an unlicensed Enterprise container (Entry mode). The image tags are
// amd64-only, so on Apple Silicon it runs emulated and needs a long startup deadline.
func startMattermost(ctx context.Context, nw *testcontainers.DockerNetwork) (*testcontainers.DockerContainer, error) {
	image := defaultMattermostImage
	if override := os.Getenv("MM_E2E_IMAGE"); override != "" {
		image = override
	}

	return testcontainers.Run(ctx, image,
		testcontainers.WithImagePlatform("linux/amd64"),
		testcontainers.WithEnv(map[string]string{
			"MM_SQLSETTINGS_DRIVERNAME": "postgres",
			"MM_SQLSETTINGS_DATASOURCE": "postgres://mmuser:mmpassword@db:5432/mattermost?sslmode=disable",
			// Must match Synapse's AppServiceURL: the plugin renders its registration YAML from SiteURL.
			"MM_SERVICESETTINGS_SITEURL":                                SiteURL,
			"MM_SERVICEENVIRONMENT":                                     model.ServiceEnvironmentTest,
			"MM_PLUGINSETTINGS_ENABLEUPLOADS":                           "true",
			"MM_PLUGINSETTINGS_AUTOMATICPREPACKAGEDPLUGINS":             "false",
			"MM_LOGSETTINGS_CONSOLELEVEL":                               "DEBUG",
			"MM_CONNECTEDWORKSPACESSETTINGS_ENABLESHAREDCHANNELS":       "true",
			"MM_CONNECTEDWORKSPACESSETTINGS_ENABLEREMOTECLUSTERSERVICE": "true",
			// Every test shares one team, and each run adds users plus ghosts.
			"MM_TEAMSETTINGS_MAXUSERSPERTEAM": "10000",
		}),
		testcontainers.WithExposedPorts(mattermostPort),
		network.WithNetwork([]string{"mattermost"}, nw),
		testcontainers.WithWaitStrategy(wait.ForLog("Server is listening on").WithStartupTimeout(3*time.Minute)),
	)
}

// setupMattermost creates the admin (the first user becomes System Admin) and the team, then
// installs the plugin with rate limiting disabled before enabling it.
func (e *Env) setupMattermost(ctx context.Context, bundle string) error {
	url, err := e.Mattermost.PortEndpoint(ctx, mattermostPort, "http")
	if err != nil {
		return fmt.Errorf("get Mattermost URL: %w", err)
	}
	e.Admin = model.NewAPIv4Client(url)
	admin := &model.User{Email: "admin@example.com", Username: AdminUsername, Password: AdminPassword}
	if _, _, err := e.Admin.CreateUser(ctx, admin); err != nil {
		return fmt.Errorf("create admin: %w", err)
	}
	if _, _, err := e.Admin.Login(ctx, AdminUsername, AdminPassword); err != nil {
		return fmt.Errorf("log in as admin: %w", err)
	}

	if e.Team, _, err = e.Admin.CreateTeam(ctx, &model.Team{Name: teamName, DisplayName: "Test", Type: model.TeamOpen}); err != nil {
		return fmt.Errorf("create team: %w", err)
	}

	file, err := os.Open(bundle) //nolint:gosec // the bundle path is chosen by whoever runs the tests
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	if _, _, err := e.Admin.UploadPlugin(ctx, file); err != nil {
		return fmt.Errorf("upload plugin bundle: %w", err)
	}
	patch := &model.Config{PluginSettings: model.PluginSettings{
		Plugins: map[string]map[string]any{PluginID: {"rate_limiting_mode": "disabled"}},
	}}
	if _, _, err := e.Admin.PatchConfig(ctx, patch); err != nil {
		return fmt.Errorf("configure plugin: %w", err)
	}
	if _, err := e.Admin.EnablePlugin(ctx, PluginID); err != nil {
		return fmt.Errorf("enable plugin: %w", err)
	}
	return nil
}
