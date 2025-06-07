package main

import (
	"net/http"
	"sync"
	"time"

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

	backgroundJob *cluster.Job

	// configurationLock synchronizes access to the configuration.
	configurationLock sync.RWMutex

	// configuration is the active plugin configuration. Consult getConfiguration and
	// setConfiguration for usage.
	configuration *configuration
}

// OnActivate is invoked when the plugin is activated. If an error is returned, the plugin will be deactivated.
func (p *Plugin) OnActivate() error {
	p.client = pluginapi.NewClient(p.API, p.Driver)

	p.kvstore = kvstore.NewKVStore(p.client)

	p.initMatrixClient()

	p.commandClient = command.NewCommandHandler(p.client, p.kvstore, p.matrixClient)

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

// This will execute the commands that were registered in the NewCommandHandler function.
func (p *Plugin) ExecuteCommand(c *plugin.Context, args *model.CommandArgs) (*model.CommandResponse, *model.AppError) {
	response, err := p.commandClient.Handle(args)
	if err != nil {
		return nil, model.NewAppError("ExecuteCommand", "plugin.command.execute_command.app_error", nil, err.Error(), http.StatusInternalServerError)
	}
	return response, nil
}

func (p *Plugin) initMatrixClient() {
	config := p.getConfiguration()
	p.matrixClient = matrix.NewClientWithAppService(config.MatrixServerURL, config.MatrixAccessToken, config.MatrixUserID, config.MatrixASToken)
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

	p.API.LogInfo("Successfully registered plugin for shared channels", "remote_id", remoteID)
	return nil
}

func (p *Plugin) OnSharedChannelsSyncMsg(msg *model.SyncMsg, rc *model.RemoteCluster) (model.SyncResponse, error) {
	config := p.getConfiguration()
	if !config.EnableSync {
		return model.SyncResponse{}, nil
	}

	if p.matrixClient == nil {
		p.API.LogError("Matrix client not initialized")
		return model.SyncResponse{}, errors.New("matrix client not initialized")
	}

	for _, post := range msg.Posts {
		if err := p.syncPostToMatrix(post, msg.ChannelId); err != nil {
			p.API.LogError("Failed to sync post to Matrix", "error", err, "post_id", post.Id)
			continue
		}
	}

	return model.SyncResponse{}, nil
}

func (p *Plugin) OnSharedChannelsPing(rc *model.RemoteCluster) bool {
	config := p.getConfiguration()
	
	// If sync is disabled, we're still "healthy" but not actively processing
	if !config.EnableSync {
		p.API.LogDebug("Ping received but sync is disabled")
		return true
	}

	// If Matrix client is not configured, we're not healthy
	if p.matrixClient == nil {
		p.API.LogWarn("Ping failed - Matrix client not initialized")
		return false
	}

	// Test Matrix connection health
	if config.MatrixServerURL != "" && config.MatrixAccessToken != "" {
		if err := p.matrixClient.TestConnection(); err != nil {
			p.API.LogWarn("Ping failed - Matrix connection test failed", "error", err)
			return false
		}
	} else {
		p.API.LogWarn("Ping failed - Matrix configuration incomplete")
		return false
	}

	p.API.LogDebug("Ping successful - Matrix bridge is healthy")
	return true
}

func (p *Plugin) OnSharedChannelsAttachmentSyncMsg(fi *model.FileInfo, post *model.Post, rc *model.RemoteCluster) error {
	config := p.getConfiguration()
	if !config.EnableSync {
		return nil
	}

	if p.matrixClient == nil {
		return errors.New("matrix client not initialized")
	}

	p.API.LogDebug("Received attachment sync", "file_id", fi.Id, "post_id", post.Id, "filename", fi.Name)
	
	// For now, we'll log the attachment but not sync it to Matrix
	// TODO: Implement Matrix file upload for attachments
	p.API.LogInfo("Attachment sync not yet implemented", "filename", fi.Name, "size", fi.Size)
	
	return nil
}

func (p *Plugin) OnSharedChannelsProfileImageSyncMsg(user *model.User, rc *model.RemoteCluster) error {
	config := p.getConfiguration()
	if !config.EnableSync {
		return nil
	}

	p.API.LogDebug("Received profile image sync", "user_id", user.Id, "username", user.Username)
	
	// For now, we'll log the profile image sync but not implement it
	// TODO: Implement Matrix avatar sync for user profile images
	p.API.LogInfo("Profile image sync not yet implemented", "username", user.Username)
	
	return nil
}

func (p *Plugin) syncPostToMatrix(post *model.Post, channelID string) error {
	matrixRoomIdentifier, err := p.getMatrixRoomID(channelID)
	if err != nil {
		return errors.Wrap(err, "failed to get Matrix room identifier")
	}

	if matrixRoomIdentifier == "" {
		p.API.LogWarn("No Matrix room mapped for channel", "channel_id", channelID)
		return nil
	}

	// Resolve room alias to room ID if needed
	matrixRoomID, err := p.matrixClient.ResolveRoomAlias(matrixRoomIdentifier)
	if err != nil {
		return errors.Wrap(err, "failed to resolve Matrix room identifier")
	}

	user, appErr := p.API.GetUser(post.UserId)
	if appErr != nil {
		return errors.Wrap(appErr, "failed to get user")
	}

	// Send message as ghost user
	err = p.syncPostAsGhostUser(post, matrixRoomID, user)
	if err != nil {
		return err
	}

	p.API.LogDebug("Successfully synced post to Matrix", "post_id", post.Id, "matrix_room_id", matrixRoomID, "matrix_room_identifier", matrixRoomIdentifier)
	return nil
}

func (p *Plugin) syncPostAsGhostUser(post *model.Post, matrixRoomID string, user *model.User) error {
	// Get or create ghost user
	ghostUserID, err := p.getOrCreateGhostUser(user.Id, user.Username)
	if err != nil {
		return errors.Wrap(err, "failed to get or create ghost user")
	}

	// Send message as ghost user (no display name prefix needed since it appears from the user directly)
	_, err = p.matrixClient.SendMessageAsGhost(matrixRoomID, post.Message, ghostUserID)
	if err != nil {
		return errors.Wrap(err, "failed to send message as ghost user")
	}

	p.API.LogDebug("Successfully synced post as ghost user", "post_id", post.Id, "ghost_user_id", ghostUserID)
	return nil
}

func (p *Plugin) getOrCreateGhostUser(mattermostUserID, mattermostUsername string) (string, error) {
	// Check if we already have this ghost user cached
	ghostUserKey := "ghost_user_" + mattermostUserID
	ghostUserIDBytes, err := p.kvstore.Get(ghostUserKey)
	if err == nil && len(ghostUserIDBytes) > 0 {
		// Ghost user already exists
		return string(ghostUserIDBytes), nil
	}

	// Create new ghost user
	ghostUser, err := p.matrixClient.CreateGhostUser(mattermostUserID, mattermostUsername)
	if err != nil {
		return "", errors.Wrap(err, "failed to create ghost user")
	}

	// Cache the ghost user ID
	err = p.kvstore.Set(ghostUserKey, []byte(ghostUser.UserID))
	if err != nil {
		p.API.LogWarn("Failed to cache ghost user ID", "error", err, "ghost_user_id", ghostUser.UserID)
		// Continue anyway, the ghost user was created successfully
	}

	p.API.LogInfo("Created new ghost user", "mattermost_user_id", mattermostUserID, "ghost_user_id", ghostUser.UserID)
	return ghostUser.UserID, nil
}

func (p *Plugin) getMatrixRoomID(channelID string) (string, error) {
	roomID, err := p.kvstore.Get("channel_mapping_" + channelID)
	if err != nil {
		return "", errors.Wrap(err, "failed to get room mapping from store")
	}
	return string(roomID), nil
}

func (p *Plugin) setMatrixRoomMapping(channelID, roomID string) error {
	err := p.kvstore.Set("channel_mapping_"+channelID, []byte(roomID))
	if err != nil {
		return errors.Wrap(err, "failed to set room mapping in store")
	}
	return nil
}

// See https://developers.mattermost.com/extend/plugins/server/reference/
