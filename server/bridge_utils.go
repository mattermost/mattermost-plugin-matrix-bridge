package main

import (
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
	"github.com/pkg/errors"
	"github.com/wiggin77/mattermost-plugin-matrix-bridge/server/matrix"
	"github.com/wiggin77/mattermost-plugin-matrix-bridge/server/store/kvstore"
)

// ConfigurationGetter interface for getting plugin configuration
type ConfigurationGetter interface {
	getConfiguration() *configuration
}

// BridgeUtilsConfig contains all dependencies needed for BridgeUtils
type BridgeUtilsConfig struct {
	Logger              Logger
	API                 plugin.API
	KVStore             kvstore.KVStore
	MatrixClient        *matrix.Client
	RemoteID            string
	MaxProfileImageSize int64
	MaxFileSize         int64
	ConfigGetter        ConfigurationGetter
}

// BridgeUtils contains common utilities used by both bridge types
type BridgeUtils struct {
	logger              Logger
	API                 plugin.API
	kvstore             kvstore.KVStore
	matrixClient        *matrix.Client
	remoteID            string
	maxProfileImageSize int64
	maxFileSize         int64
	configGetter        ConfigurationGetter
}

// NewBridgeUtils creates a new BridgeUtils instance
func NewBridgeUtils(config BridgeUtilsConfig) *BridgeUtils {
	return &BridgeUtils{
		logger:              config.Logger,
		API:                 config.API,
		kvstore:             config.KVStore,
		matrixClient:        config.MatrixClient,
		remoteID:            config.RemoteID,
		maxProfileImageSize: config.MaxProfileImageSize,
		maxFileSize:         config.MaxFileSize,
		configGetter:        config.ConfigGetter,
	}
}

// Shared utility methods that both bridge types need

func (s *BridgeUtils) getMatrixRoomID(channelID string) (string, error) {
	roomID, err := s.kvstore.Get("channel_mapping_" + channelID)
	if err != nil {
		// KV store error (typically key not found) - unmapped channels are expected
		return "", nil
	}
	return string(roomID), nil
}

func (s *BridgeUtils) setChannelRoomMapping(channelID, matrixRoomIdentifier string) error {
	// Always resolve to room ID for consistent forward mapping storage
	var roomID string
	var err error

	if strings.HasPrefix(matrixRoomIdentifier, "#") {
		// Resolve alias to room ID
		roomID, err = s.matrixClient.ResolveRoomAlias(matrixRoomIdentifier)
		if err != nil {
			s.logger.LogWarn("Failed to resolve room alias during mapping creation", "room_alias", matrixRoomIdentifier, "error", err)
			// Fallback: store the alias (better than failing completely)
			roomID = matrixRoomIdentifier
		}
	} else {
		// Already a room ID
		roomID = matrixRoomIdentifier
	}

	// Store forward mapping: channel_mapping_<channelID> -> room_id (always room ID)
	err = s.kvstore.Set("channel_mapping_"+channelID, []byte(roomID))
	if err != nil {
		return errors.Wrap(err, "failed to store channel room mapping")
	}

	// Store reverse mapping for the room ID
	err = s.kvstore.Set("room_mapping_"+roomID, []byte(channelID))
	if err != nil {
		return errors.Wrap(err, "failed to store reverse room mapping")
	}

	// If we started with an alias, also create reverse mapping for the alias
	// This allows lookups by both alias and room ID
	if strings.HasPrefix(matrixRoomIdentifier, "#") && roomID != matrixRoomIdentifier {
		err = s.kvstore.Set("room_mapping_"+matrixRoomIdentifier, []byte(channelID))
		if err != nil {
			s.logger.LogWarn("Failed to create alias reverse mapping", "channel_id", channelID, "room_alias", matrixRoomIdentifier, "error", err)
		} else {
			s.logger.LogDebug("Created reverse mappings for alias", "channel_id", channelID, "room_alias", matrixRoomIdentifier, "room_id", roomID)
		}
	}

	return nil
}

func (s *BridgeUtils) getConfiguration() *configuration {
	return s.configGetter.getConfiguration()
}

func (s *BridgeUtils) extractMattermostMetadata(event MatrixEvent) (postID string, remoteID string) {
	if event.Content != nil {
		if id, ok := event.Content["mattermost_post_id"].(string); ok {
			postID = id
		}
		if id, ok := event.Content["mattermost_remote_id"].(string); ok {
			remoteID = id
		}
	}
	return postID, remoteID
}

func (s *BridgeUtils) extractMatrixMessageContent(event MatrixEvent) string {
	if event.Content == nil {
		return ""
	}

	// Prefer formatted_body if available and different from body
	if formattedBody, ok := event.Content["formatted_body"].(string); ok {
		if body, hasBody := event.Content["body"].(string); hasBody {
			// Only use formatted_body if it's different from body (indicating actual formatting)
			if formattedBody != body {
				return formattedBody
			}
		}
	}

	// Fall back to plain body
	if body, ok := event.Content["body"].(string); ok {
		return body
	}

	return ""
}

func (s *BridgeUtils) downloadMatrixFile(mxcURL string) ([]byte, error) {
	data, err := s.matrixClient.DownloadFile(mxcURL, s.maxFileSize, "")
	if err != nil {
		return nil, errors.Wrap(err, "failed to download Matrix media")
	}
	return data, nil
}

func (s *BridgeUtils) isGhostUser(matrixUserID string) bool {
	// Ghost users follow the pattern: @_mattermost_<user_id>:<server_domain>
	return strings.HasPrefix(matrixUserID, "@_mattermost_")
}

// DM channel detection and handling utilities

func (s *BridgeUtils) isDirectChannel(channelID string) (bool, []string, error) {
	channel, appErr := s.API.GetChannel(channelID)
	if appErr != nil {
		return false, nil, errors.Wrap(appErr, "failed to get channel")
	}

	if channel.Type == model.ChannelTypeDirect {
		// Get the two users in the DM
		members, appErr := s.API.GetChannelMembers(channelID, 0, 10)
		if appErr != nil {
			return false, nil, errors.Wrap(appErr, "failed to get channel members")
		}

		userIDs := make([]string, len(members))
		for i, member := range members {
			userIDs[i] = member.UserId
		}
		return true, userIDs, nil
	}

	if channel.Type == model.ChannelTypeGroup {
		// Handle group DMs - get all members
		members, appErr := s.API.GetChannelMembers(channelID, 0, 100) // Larger limit for group DMs
		if appErr != nil {
			return false, nil, errors.Wrap(appErr, "failed to get group channel members")
		}

		userIDs := make([]string, len(members))
		for i, member := range members {
			userIDs[i] = member.UserId
		}
		return true, userIDs, nil
	}

	return false, nil, nil
}
