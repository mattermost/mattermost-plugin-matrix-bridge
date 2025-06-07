package main

import (
	"net/url"
	"strings"
	
	"github.com/pkg/errors"
)

// getOrCreateGhostUser retrieves or creates a Matrix ghost user for a Mattermost user
func (p *Plugin) getOrCreateGhostUser(mattermostUserID, mattermostUsername, displayName string, avatarData []byte, avatarContentType string) (string, error) {
	// Check if we already have this ghost user cached
	ghostUserKey := "ghost_user_" + mattermostUserID
	ghostUserIDBytes, err := p.kvstore.Get(ghostUserKey)
	if err == nil && len(ghostUserIDBytes) > 0 {
		// Ghost user already exists
		return string(ghostUserIDBytes), nil
	}

	// Create new ghost user with display name and avatar
	ghostUser, err := p.matrixClient.CreateGhostUser(mattermostUserID, mattermostUsername, displayName, avatarData, avatarContentType)
	if err != nil {
		// Check if this is a display name error (user was created but display name failed)
		if ghostUser != nil && ghostUser.UserID != "" {
			p.API.LogWarn("Ghost user created but display name setting failed", "error", err, "ghost_user_id", ghostUser.UserID, "display_name", displayName)
			// Continue with caching - user creation was successful
		} else {
			return "", errors.Wrap(err, "failed to create ghost user")
		}
	}

	// Cache the ghost user ID
	err = p.kvstore.Set(ghostUserKey, []byte(ghostUser.UserID))
	if err != nil {
		p.API.LogWarn("Failed to cache ghost user ID", "error", err, "ghost_user_id", ghostUser.UserID)
		// Continue anyway, the ghost user was created successfully
	}

	if displayName != "" {
		p.API.LogInfo("Created new ghost user with display name", "mattermost_user_id", mattermostUserID, "ghost_user_id", ghostUser.UserID, "display_name", displayName)
	} else {
		p.API.LogInfo("Created new ghost user", "mattermost_user_id", mattermostUserID, "ghost_user_id", ghostUser.UserID)
	}
	return ghostUser.UserID, nil
}

// ensureGhostUserInRoom ensures that a ghost user is joined to a specific Matrix room
func (p *Plugin) ensureGhostUserInRoom(ghostUserID, matrixRoomID, mattermostUserID string) error {
	// Check if we've already confirmed this ghost user is in this room
	roomMembershipKey := "ghost_room_" + mattermostUserID + "_" + matrixRoomID
	membershipBytes, err := p.kvstore.Get(roomMembershipKey)
	if err == nil && len(membershipBytes) > 0 && string(membershipBytes) == "joined" {
		// Ghost user is already confirmed to be in this room
		return nil
	}

	// Attempt to join the ghost user to the room
	err = p.matrixClient.JoinRoomAsUser(matrixRoomID, ghostUserID)
	if err != nil {
		p.API.LogWarn("Failed to join ghost user to room", "error", err, "ghost_user_id", ghostUserID, "room_id", matrixRoomID)
		return errors.Wrap(err, "failed to join ghost user to room")
	}

	// Cache the successful join
	err = p.kvstore.Set(roomMembershipKey, []byte("joined"))
	if err != nil {
		p.API.LogWarn("Failed to cache ghost user room membership", "error", err, "ghost_user_id", ghostUserID, "room_id", matrixRoomID)
		// Continue anyway, the join was successful
	}

	p.API.LogDebug("Ghost user joined room successfully", "ghost_user_id", ghostUserID, "room_id", matrixRoomID)
	return nil
}

// getMatrixRoomID retrieves the Matrix room identifier for a given Mattermost channel
func (p *Plugin) getMatrixRoomID(channelID string) (string, error) {
	roomID, err := p.kvstore.Get("channel_mapping_" + channelID)
	if err != nil {
		return "", errors.Wrap(err, "failed to get room mapping from store")
	}
	return string(roomID), nil
}

// extractServerDomain extracts the hostname from a Matrix server URL
func (p *Plugin) extractServerDomain(serverURL string) string {
	if serverURL == "" {
		return "unknown"
	}

	parsedURL, err := url.Parse(serverURL)
	if err != nil {
		p.API.LogWarn("Failed to parse Matrix server URL", "url", serverURL, "error", err)
		return "unknown"
	}

	hostname := parsedURL.Hostname()
	if hostname == "" {
		p.API.LogWarn("Could not extract hostname from Matrix server URL", "url", serverURL)
		return "unknown"
	}

	// Replace dots and colons to make it safe for use in property keys
	return strings.ReplaceAll(strings.ReplaceAll(hostname, ".", "_"), ":", "_")
}

// convertEmojiForMatrix converts Mattermost emoji names to Matrix reaction format
func (p *Plugin) convertEmojiForMatrix(emojiName string) string {
	// Map common Mattermost emoji names to Unicode equivalents for Matrix
	emojiMap := map[string]string{
		"+1":        "👍",
		"-1":        "👎", 
		"heart":     "❤️",
		"smile":     "😄",
		"laughing":  "😆",
		"confused":  "😕",
		"frowning":  "😦",
		"heart_eyes": "😍",
		"rage":      "😡",
		"slightly_smiling_face": "🙂",
		"white_check_mark": "✅",
		"x":         "❌",
		"heavy_check_mark": "✔️",
		"fire":      "🔥",
		"clap":      "👏",
		"eyes":      "👀",
		"thinking_face": "🤔",
		"tada":      "🎉",
		"rocket":    "🚀",
	}

	// Check if we have a mapping for this emoji
	if unicode, exists := emojiMap[emojiName]; exists {
		return unicode
	}

	// For custom emojis or unmapped emojis, return the name as-is
	// Matrix clients can handle custom emoji names
	return ":" + emojiName + ":"
}

