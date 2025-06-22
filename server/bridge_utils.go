package main

import (
	"path/filepath"
	"strings"

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
		return "", errors.Wrap(err, "failed to get room mapping from store")
	}
	return string(roomID), nil
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

func (s *BridgeUtils) detectMimeType(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".pdf":
		return "application/pdf"
	case ".txt":
		return "text/plain"
	case ".mp4":
		return "video/mp4"
	case ".mp3":
		return "audio/mpeg"
	case ".wav":
		return "audio/wav"
	default:
		return "application/octet-stream"
	}
}

func (s *BridgeUtils) isGhostUser(matrixUserID string) bool {
	// Ghost users follow the pattern: @_mattermost_<user_id>:<server_domain>
	return strings.HasPrefix(matrixUserID, "@_mattermost_")
}
