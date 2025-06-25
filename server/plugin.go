package main

import (
	"net/http"
	"sync"
	"time"

	"github.com/mattermost/logr/v2"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
	"github.com/mattermost/mattermost/server/public/pluginapi"
	"github.com/mattermost/mattermost/server/public/pluginapi/cluster"
	"github.com/pkg/errors"
	"github.com/wiggin77/mattermost-plugin-matrix-bridge/server/command"
	"github.com/wiggin77/mattermost-plugin-matrix-bridge/server/matrix"
	"github.com/wiggin77/mattermost-plugin-matrix-bridge/server/store/kvstore"
)

// Plugin implements the interface expected by the Mattermost server to communicate between the server and plugin processes.
type Plugin struct {
	plugin.MattermostPlugin

	// kvstore is the client used to read/write KV records for this plugin.
	kvstore kvstore.KVStore

	// client is the Mattermost server API client.
	client *pluginapi.Client

	// commandClient is the client used to register and execute slash commands.
	commandClient command.Command

	// matrixClient is the client used to communicate with Matrix servers.
	matrixClient *matrix.Client

	// postTracker tracks post creation timestamps to detect redundant edits
	postTracker *PostTracker

	// pendingFiles tracks uploaded files awaiting their posts
	pendingFiles *PendingFileTracker

	// remoteID is the identifier returned by RegisterPluginForSharedChannels
	remoteID string

	backgroundJob *cluster.Job

	// configurationLock synchronizes access to the configuration.
	configurationLock sync.RWMutex

	// configuration is the active plugin configuration. Consult getConfiguration and
	// setConfiguration for usage.
	configuration *configuration

	// Logr instance specifically for logging Matrix transactions.
	transactionLogger logr.Logger

	// maxProfileImageSize is the maximum size for profile images in bytes
	maxProfileImageSize int64

	// maxFileSize is the maximum size for file attachments in bytes
	maxFileSize int64

	// Bridge components for dependency injection architecture
	mattermostToMatrixBridge *MattermostToMatrixBridge
	matrixToMattermostBridge *MatrixToMattermostBridge
}

// OnActivate is invoked when the plugin is activated. If an error is returned, the plugin will be deactivated.
func (p *Plugin) OnActivate() error {
	var err error
	p.transactionLogger, err = CreateLogger()
	if err != nil {
		return errors.Wrap(err, "failed to create transaction logger")
	}

	p.client = pluginapi.NewClient(p.API, p.Driver)

	p.kvstore = kvstore.NewKVStore(p.client)

	p.postTracker = NewPostTracker(DefaultPostTrackerMaxEntries)
	p.pendingFiles = NewPendingFileTracker()

	p.initMatrixClient()

	// Initialize bridge components
	p.initBridges()

	p.commandClient = command.NewCommandHandler(p)

	if err := p.registerForSharedChannels(); err != nil {
		p.API.LogWarn("Failed to register for shared channels", "error", err)
	}

	job, err := cluster.Schedule(
		p.API,
		"BackgroundJob",
		cluster.MakeWaitForRoundedInterval(1*time.Hour),
		p.runJob,
	)
	if err != nil {
		return errors.Wrap(err, "failed to schedule background job")
	}

	p.backgroundJob = job

	return nil
}

// OnDeactivate is invoked when the plugin is deactivated.
func (p *Plugin) OnDeactivate() error {
	if p.backgroundJob != nil {
		if err := p.backgroundJob.Close(); err != nil {
			p.API.LogError("Failed to close background job", "err", err)
		}
	}
	return nil
}

// ExecuteCommand executes the commands that were registered in the NewCommandHandler function.
func (p *Plugin) ExecuteCommand(_ *plugin.Context, args *model.CommandArgs) (*model.CommandResponse, *model.AppError) {
	response, err := p.commandClient.Handle(args)
	if err != nil {
		return nil, model.NewAppError("ExecuteCommand", "plugin.command.execute_command.app_error", nil, err.Error(), http.StatusInternalServerError)
	}
	return response, nil
}

func (p *Plugin) initMatrixClient() {
	config := p.getConfiguration()
	p.matrixClient = matrix.NewClient(config.MatrixServerURL, config.MatrixASToken, p.remoteID, p.API)
}

func (p *Plugin) initBridges() {
	// Create shared utilities
	sharedUtils := NewBridgeUtils(BridgeUtilsConfig{
		Logger:              p.API,
		API:                 p.API,
		KVStore:             p.kvstore,
		MatrixClient:        p.matrixClient,
		RemoteID:            p.remoteID,
		MaxProfileImageSize: p.maxProfileImageSize,
		MaxFileSize:         p.maxFileSize,
		ConfigGetter:        p,
	})

	// Create bridge instances
	p.mattermostToMatrixBridge = NewMattermostToMatrixBridge(sharedUtils, p.pendingFiles, p.postTracker)
	p.matrixToMattermostBridge = NewMatrixToMattermostBridge(sharedUtils)
}

func (p *Plugin) registerForSharedChannels() error {
	// Get the bot user ID or use a system admin
	botUser, err := p.API.GetUserByUsername("mattermost-bridge")
	var creatorID string
	if err != nil {
		// Fallback to getting any system admin
		users, err2 := p.API.GetUsers(&model.UserGetOptions{
			Page:    0,
			PerPage: 1,
		})
		if err2 != nil || len(users) == 0 {
			return errors.New("failed to find a valid creator user")
		}
		creatorID = users[0].Id
	} else {
		creatorID = botUser.Id
	}

	opts := model.RegisterPluginOpts{
		Displayname:  "Matrix_Bridge",
		PluginID:     "com.mattermost.plugin-matrix-bridge",
		CreatorID:    creatorID,
		AutoShareDMs: false,
		AutoInvited:  true,
	}

	remoteID, appErr := p.API.RegisterPluginForSharedChannels(opts)
	if appErr != nil {
		return errors.Wrap(appErr, "failed to register plugin for shared channels")
	}

	// Store the remote ID for use in sync operations
	p.remoteID = remoteID

	p.API.LogInfo("Successfully registered plugin for shared channels", "remote_id", remoteID)
	return nil
}

// PluginAccessor interface implementation for command handlers

// GetMatrixClient returns the Matrix client instance
func (p *Plugin) GetMatrixClient() *matrix.Client {
	return p.matrixClient
}

// GetKVStore returns the KV store instance
func (p *Plugin) GetKVStore() kvstore.KVStore {
	return p.kvstore
}

// GetConfiguration returns the plugin configuration
func (p *Plugin) GetConfiguration() command.Configuration {
	return p.getConfiguration()
}

// CreateOrGetGhostUser gets an existing ghost user or creates a new one for a Mattermost user
func (p *Plugin) CreateOrGetGhostUser(mattermostUserID string) (string, error) {
	return p.mattermostToMatrixBridge.CreateOrGetGhostUser(mattermostUserID)
}

// GetPluginAPI returns the Mattermost plugin API
func (p *Plugin) GetPluginAPI() plugin.API {
	return p.API
}

// GetPluginAPIClient returns the pluginapi client
func (p *Plugin) GetPluginAPIClient() *pluginapi.Client {
	return p.client
}

// See https://developers.mattermost.com/extend/plugins/server/reference/
