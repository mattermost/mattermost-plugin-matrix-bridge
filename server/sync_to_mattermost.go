package main

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/pkg/errors"
	"github.com/wiggin77/mattermost-plugin-matrix-bridge/server/matrix"
)

// syncMatrixMessageToMattermost handles syncing Matrix messages to Mattermost posts
func (p *Plugin) syncMatrixMessageToMattermost(event MatrixEvent, channelID string) error {
	p.API.LogDebug("Syncing Matrix message to Mattermost", "event_id", event.EventID, "sender", event.Sender, "channel_id", channelID)

	// Extract message content (prefer formatted_body if available)
	content := p.extractMatrixMessageContent(event)

	// Check if this is a message edit (has m.relates_to with rel_type: m.replace)
	if relatesTo, exists := event.Content["m.relates_to"].(map[string]interface{}); exists {
		if relType, exists := relatesTo["rel_type"].(string); exists && relType == "m.replace" {
			// This is an edit - handle separately (allow empty content for deletions)
			return p.handleMatrixMessageEdit(event, channelID)
		}
	}

	// For new messages (not edits), skip if content is empty
	if content == "" {
		p.API.LogDebug("Matrix message has no body content, skipping new message", "event_id", event.EventID, "content", event.Content)
		return nil // Empty new messages don't need to be synced
	}

	// Get or create Mattermost user for the Matrix sender
	mattermostUserID, err := p.getOrCreateMattermostUser(event.Sender, channelID)
	if err != nil {
		return errors.Wrap(err, "failed to get or create Mattermost user")
	}

	// Convert Matrix content to Mattermost format
	mattermostContent := p.convertMatrixToMattermost(content)

	// Check if this is a threaded message (reply)
	var rootID string
	if relatesTo, exists := event.Content["m.relates_to"].(map[string]interface{}); exists {
		if relType, exists := relatesTo["rel_type"].(string); exists && relType == "m.thread" {
			if parentEventID, exists := relatesTo["event_id"].(string); exists {
				// Find the Mattermost post ID for this Matrix event
				if mattermostPostID := p.getPostIDFromMatrixEvent(parentEventID, channelID); mattermostPostID != "" {
					rootID = mattermostPostID
				}
			}
		}
	}

	// Create Mattermost post
	post := &model.Post{
		UserId:    mattermostUserID,
		ChannelId: channelID,
		Message:   mattermostContent,
		CreateAt:  event.Timestamp,
		RootId:    rootID,
		RemoteId:  &p.remoteID, // Attribute to Matrix remote
		Props:     make(map[string]interface{}),
	}

	// Store Matrix event ID in post properties for reaction mapping and edit tracking
	config := p.getConfiguration()
	serverDomain := extractServerDomain(p.API, config.MatrixServerURL)
	propertyKey := "matrix_event_id_" + serverDomain
	post.Props[propertyKey] = event.EventID
	post.Props["from_matrix"] = true

	// Create the post in Mattermost
	createdPost, appErr := p.API.CreatePost(post)
	if appErr != nil {
		return errors.Wrap(appErr, "failed to create Mattermost post")
	}

	p.API.LogDebug("Successfully synced Matrix message to Mattermost", "matrix_event_id", event.EventID, "mattermost_post_id", createdPost.Id, "sender", event.Sender, "channel_id", channelID)
	return nil
}

// handleMatrixMessageEdit handles Matrix message edits by updating the corresponding Mattermost post
func (p *Plugin) handleMatrixMessageEdit(event MatrixEvent, channelID string) error {
	// Extract the new content from the edit event
	newContent := p.extractMatrixMessageContent(event)
	// Extract the original event ID being edited
	relatesTo, exists := event.Content["m.relates_to"].(map[string]interface{})
	if !exists {
		return errors.New("edit event missing m.relates_to")
	}

	originalEventID, exists := relatesTo["event_id"].(string)
	if !exists {
		return errors.New("edit event missing original event_id")
	}

	// Find the Mattermost post corresponding to the original Matrix event
	postID := p.getPostIDFromMatrixEvent(originalEventID, channelID)
	if postID == "" {
		p.API.LogWarn("Cannot find Mattermost post for Matrix edit", "original_event_id", originalEventID, "edit_event_id", event.EventID)
		return nil // Post not found, maybe it wasn't synced
	}

	// Get the existing post
	post, appErr := p.API.GetPost(postID)
	if appErr != nil {
		return errors.Wrap(appErr, "failed to get post for edit")
	}

	// Update the post content (allow empty content - user may have deleted all text)
	post.Message = p.convertMatrixToMattermost(newContent)
	post.EditAt = event.Timestamp

	// Update the post
	updatedPost, appErr := p.API.UpdatePost(post)
	if appErr != nil {
		return errors.Wrap(appErr, "failed to update post")
	}

	p.API.LogDebug("Successfully updated Mattermost post from Matrix edit", "original_matrix_event_id", originalEventID, "edit_matrix_event_id", event.EventID, "mattermost_post_id", updatedPost.Id)
	return nil
}

// syncMatrixReactionToMattermost handles syncing Matrix reactions to Mattermost
func (p *Plugin) syncMatrixReactionToMattermost(event MatrixEvent, channelID string) error {
	p.API.LogDebug("Syncing Matrix reaction to Mattermost", "event_id", event.EventID, "sender", event.Sender, "channel_id", channelID)

	// Extract reaction key (emoji)
	relatesTo, exists := event.Content["m.relates_to"].(map[string]interface{})
	if !exists {
		return errors.New("reaction event missing m.relates_to")
	}

	key, exists := relatesTo["key"].(string)
	if !exists {
		return errors.New("reaction event missing key")
	}

	targetEventID, exists := relatesTo["event_id"].(string)
	if !exists {
		return errors.New("reaction event missing target event_id")
	}

	// Find the Mattermost post corresponding to the target Matrix event
	postID := p.getPostIDFromMatrixEvent(targetEventID, channelID)
	if postID == "" {
		p.API.LogWarn("Cannot find Mattermost post for Matrix reaction", "target_event_id", targetEventID, "reaction_event_id", event.EventID)
		return nil // Post not found, maybe it wasn't synced
	}

	// Get or create Mattermost user for the reaction sender
	mattermostUserID, err := p.getOrCreateMattermostUser(event.Sender, channelID)
	if err != nil {
		return errors.Wrap(err, "failed to get or create Mattermost user for reaction")
	}

	// Convert Matrix emoji to Mattermost format
	emojiName := p.convertMatrixEmojiToMattermost(key)

	// Create Mattermost reaction
	reaction := &model.Reaction{
		UserId:    mattermostUserID,
		PostId:    postID,
		EmojiName: emojiName,
		CreateAt:  event.Timestamp,
		RemoteId:  &p.remoteID, // Attribute to Matrix remote
	}

	// Add the reaction
	_, appErr := p.API.AddReaction(reaction)
	if appErr != nil {
		return errors.Wrap(appErr, "failed to add reaction to Mattermost")
	}

	p.API.LogDebug("Successfully synced Matrix reaction to Mattermost", "matrix_event_id", event.EventID, "mattermost_post_id", postID, "emoji", emojiName, "sender", event.Sender)
	return nil
}

// syncMatrixRedactionToMattermost handles Matrix message deletions (redactions)
func (p *Plugin) syncMatrixRedactionToMattermost(event MatrixEvent, channelID string) error {
	p.API.LogDebug("Syncing Matrix redaction to Mattermost", "event_id", event.EventID, "sender", event.Sender, "channel_id", channelID)

	// Extract the redacted event ID
	redactsEventID, exists := event.Content["redacts"].(string)
	if !exists {
		return errors.New("redaction event missing redacts field")
	}

	// Find the Mattermost post corresponding to the redacted Matrix event
	postID := p.getPostIDFromMatrixEvent(redactsEventID, channelID)
	if postID == "" {
		p.API.LogWarn("Cannot find Mattermost post for Matrix redaction", "redacted_event_id", redactsEventID, "redaction_event_id", event.EventID)
		return nil // Post not found, maybe it wasn't synced
	}

	// Delete the post
	if appErr := p.API.DeletePost(postID); appErr != nil {
		return errors.Wrap(appErr, "failed to delete post")
	}

	p.API.LogDebug("Successfully deleted Mattermost post from Matrix redaction", "redacted_matrix_event_id", redactsEventID, "redaction_matrix_event_id", event.EventID, "mattermost_post_id", postID)
	return nil
}

// getOrCreateMattermostUser gets or creates a Mattermost user for a Matrix user
// If channelID is provided, ensures the user is added to the team associated with that channel
func (p *Plugin) getOrCreateMattermostUser(matrixUserID string, channelID string) (string, error) {
	// Check if we already have a mapping for this Matrix user
	userMapKey := "matrix_user_" + matrixUserID
	userIDBytes, err := p.kvstore.Get(userMapKey)
	if err == nil && len(userIDBytes) > 0 {
		mattermostUserID := string(userIDBytes)

		// Verify the user still exists
		if existingUser, appErr := p.API.GetUser(mattermostUserID); appErr == nil {
			// User exists, ensure they're in the team for this channel
			if channelID != "" {
				if err := p.addUserToChannelTeam(mattermostUserID, channelID); err != nil {
					p.API.LogWarn("Failed to add existing Matrix user to team", "error", err, "user_id", mattermostUserID, "channel_id", channelID, "matrix_user_id", matrixUserID)
				}
			}

			// Check if we need to update their profile
			context := &ProfileUpdateContext{
				EventID: "",
				Source:  "api",
			}
			p.updateMattermostUserProfile(existingUser, matrixUserID, context)
			return mattermostUserID, nil
		}

		// User no longer exists, remove the mapping
		if err := p.kvstore.Delete(userMapKey); err != nil {
			p.API.LogWarn("Failed to delete user mapping", "error", err, "key", userMapKey)
		}
	}

	// Extract username from Matrix user ID (@username:server.com -> username)
	username := p.extractUsernameFromMatrixUserID(matrixUserID)
	if username == "" {
		return "", errors.New("failed to extract username from Matrix user ID")
	}

	// Create a unique Mattermost username
	mattermostUsername := p.generateMattermostUsername(username)

	// Get real display name and avatar from Matrix profile
	var displayName string
	var firstName, lastName string
	var avatarData []byte

	if p.matrixClient != nil {
		profile, err := p.matrixClient.GetUserProfile(matrixUserID)
		if err != nil {
			p.API.LogWarn("Failed to get Matrix user profile", "error", err, "user_id", matrixUserID)
			displayName = username // Fallback to username
		} else {
			if profile.DisplayName != "" {
				displayName = profile.DisplayName
				// Try to parse first/last name from display name
				firstName, lastName = parseDisplayName(profile.DisplayName)
			} else {
				displayName = username // Fallback to username if no display name set
			}

			// Download avatar if available
			if profile.AvatarURL != "" {
				avatarData, err = p.downloadMatrixAvatar(profile.AvatarURL)
				if err != nil {
					p.API.LogWarn("Failed to download Matrix user avatar", "error", err, "user_id", matrixUserID, "avatar_url", profile.AvatarURL)
				}
			}
		}
	} else {
		displayName = username // Fallback if no Matrix client
	}

	// Create the user in Mattermost
	user := &model.User{
		Username:    mattermostUsername,
		Email:       fmt.Sprintf("%s@matrix.bridge", mattermostUsername), // Placeholder email
		Password:    model.NewId(),                                       // Generate random password (user won't use it for login)
		Nickname:    displayName,                                         // Use real Matrix display name
		FirstName:   firstName,                                           // Parsed from display name if possible
		LastName:    lastName,                                            // Parsed from display name if possible
		AuthData:    nil,
		AuthService: "",
		RemoteId:    &p.remoteID, // Attribute to Matrix remote
	}

	createdUser, appErr := p.API.CreateUser(user)
	if appErr != nil {
		return "", errors.Wrap(appErr, "failed to create Mattermost user")
	}

	// Set avatar if we downloaded one
	if len(avatarData) > 0 {
		appErr = p.API.SetProfileImage(createdUser.Id, avatarData)
		if appErr != nil {
			p.API.LogWarn("Failed to set Matrix user avatar", "error", appErr, "user_id", createdUser.Id, "matrix_user_id", matrixUserID)
		} else {
			p.API.LogDebug("Successfully set avatar for Matrix user", "user_id", createdUser.Id, "matrix_user_id", matrixUserID)
		}
	}

	// Add user to team if channelID is provided
	if channelID != "" {
		if err := p.addUserToChannelTeam(createdUser.Id, channelID); err != nil {
			p.API.LogWarn("Failed to add new Matrix user to team", "error", err, "user_id", createdUser.Id, "channel_id", channelID, "matrix_user_id", matrixUserID)
		}
	}

	// Store the mapping
	err = p.kvstore.Set(userMapKey, []byte(createdUser.Id))
	if err != nil {
		p.API.LogWarn("Failed to store Matrix user mapping", "error", err, "matrix_user_id", matrixUserID, "mattermost_user_id", createdUser.Id)
	}

	p.API.LogDebug("Created Mattermost user for Matrix user", "matrix_user_id", matrixUserID, "mattermost_user_id", createdUser.Id, "username", mattermostUsername)
	return createdUser.Id, nil
}

// addUserToChannelTeam adds a user to the team that owns the specified channel
func (p *Plugin) addUserToChannelTeam(userID, channelID string) error {
	// Get the channel to find its team ID
	channel, appErr := p.API.GetChannel(channelID)
	if appErr != nil {
		return errors.Wrap(appErr, "failed to get channel")
	}

	// Check if user is already a member of the team
	_, appErr = p.API.GetTeamMember(channel.TeamId, userID)
	if appErr == nil {
		// User is already a team member
		p.API.LogDebug("User already member of team", "user_id", userID, "team_id", channel.TeamId, "channel_id", channelID)
		return nil
	}

	// Add user to the team
	_, appErr = p.API.CreateTeamMember(channel.TeamId, userID)
	if appErr != nil {
		return errors.Wrap(appErr, "failed to add user to team")
	}

	p.API.LogDebug("Added Matrix user to team", "user_id", userID, "team_id", channel.TeamId, "channel_id", channelID)
	return nil
}

// extractUsernameFromMatrixUserID extracts username from Matrix user ID
func (p *Plugin) extractUsernameFromMatrixUserID(userID string) string {
	// Matrix user IDs are in format @username:server.com
	if !strings.HasPrefix(userID, "@") {
		return ""
	}

	parts := strings.Split(userID[1:], ":")
	if len(parts) == 0 {
		return ""
	}

	return parts[0]
}

// generateMattermostUsername creates a unique Mattermost username
func (p *Plugin) generateMattermostUsername(baseUsername string) string {
	// Sanitize username for Mattermost (following Shared Channels convention)
	sanitized := strings.ToLower(baseUsername)
	sanitized = regexp.MustCompile(`[^a-z0-9\-_]`).ReplaceAllString(sanitized, "_")

	// Follow Shared Channels convention: remote_name:username_sanitized
	username := "matrix:" + sanitized

	// Ensure uniqueness by checking if username exists
	counter := 1
	originalUsername := username

	for {
		if _, appErr := p.API.GetUserByUsername(username); appErr != nil {
			// Username doesn't exist, we can use it
			break
		}

		// Username exists, try with counter
		username = fmt.Sprintf("%s_%d", originalUsername, counter)
		counter++

		// Prevent infinite loop
		if counter > 1000 {
			// Fallback to using counter
			username = fmt.Sprintf("matrix:user_%d", counter)
			break
		}
	}

	return username
}

// getPostIDFromMatrixEvent finds the Mattermost post ID for a Matrix event ID
func (p *Plugin) getPostIDFromMatrixEvent(matrixEventID, channelID string) string {
	// Search through posts in the channel to find one with matching Matrix event ID
	config := p.getConfiguration()
	serverDomain := extractServerDomain(p.API, config.MatrixServerURL)
	propertyKey := "matrix_event_id_" + serverDomain

	// Get recent posts in the channel (Matrix events are usually recent)
	postList, appErr := p.API.GetPostsForChannel(channelID, 0, 100)
	if appErr != nil {
		p.API.LogWarn("Failed to get posts for channel", "error", appErr, "channel_id", channelID)
		return ""
	}

	// Search through posts for matching Matrix event ID
	for _, postID := range postList.Order {
		post := postList.Posts[postID]
		if post.Props != nil {
			if eventID, exists := post.Props[propertyKey].(string); exists && eventID == matrixEventID {
				return postID
			}
		}
	}

	return ""
}

// convertMatrixToMattermost converts Matrix message format to Mattermost
func (p *Plugin) convertMatrixToMattermost(content string) string {
	// Basic conversion - can be enhanced later
	// Matrix uses different markdown syntax in some cases

	// Convert Matrix mentions (@username:server.com) to Mattermost format if possible
	// For now, keep as-is since we'd need user mapping

	return content
}

// convertMatrixEmojiToMattermost converts Matrix emoji format to Mattermost
func (p *Plugin) convertMatrixEmojiToMattermost(matrixEmoji string) string {
	// Matrix reactions can be Unicode emoji or custom emoji

	// If it's already a simple name (like "thumbsup"), validate it exists in Mattermost
	if regexp.MustCompile(`^[a-z0-9_+-]+$`).MatchString(matrixEmoji) {
		// Check if this emoji name exists in our mapping
		if _, exists := emojiNameToIndex[matrixEmoji]; exists {
			return matrixEmoji
		}
		// Fall through to Unicode handling if name doesn't exist
	}

	// For Unicode emoji, try to find the corresponding Mattermost name
	// Search through our emoji mappings to find a match
	for emojiName, index := range emojiNameToIndex {
		if unicodeHex, exists := emojiIndexToUnicode[index]; exists {
			unicode := hexToUnicode(unicodeHex)
			if unicode == matrixEmoji {
				return emojiName
			}
		}
	}

	// Try to find similar matches (without variation selectors)
	stripVariation := func(s string) string {
		// Remove common variation selectors
		s = strings.ReplaceAll(s, "\uFE0F", "") // variation selector-16
		s = strings.ReplaceAll(s, "\uFE0E", "") // variation selector-15
		return s
	}

	strippedMatrix := stripVariation(matrixEmoji)
	if strippedMatrix != matrixEmoji {
		for emojiName, index := range emojiNameToIndex {
			if unicodeHex, exists := emojiIndexToUnicode[index]; exists {
				unicode := stripVariation(hexToUnicode(unicodeHex))
				if unicode == strippedMatrix {
					return emojiName
				}
			}
		}
	}

	// Fallback mapping for common emojis
	switch matrixEmoji {
	case "👍":
		return "+1"
	case "👎":
		return "-1"
	case "❤️", "❤":
		return "heart"
	case "😂":
		return "joy"
	case "😢":
		return "cry"
	case "😄":
		return "smile"
	case "😃":
		return "smiley"
	case "😊":
		return "blush"
	case "🔥":
		return "fire"
	case "🎉":
		return "tada"
	case "👏":
		return "clap"
	default:
		// If we can't find a match, use a safe fallback
		// Check if this is a complex emoji sequence that we should just skip
		if len([]rune(matrixEmoji)) > 3 {
			p.API.LogWarn("Unknown complex emoji from Matrix", "emoji", matrixEmoji)
			return "question" // Use question mark emoji as fallback
		}

		// For simple unknown emoji, try to create a safe name
		safeName := regexp.MustCompile(`[^a-z0-9]`).ReplaceAllString(strings.ToLower(matrixEmoji), "")
		if safeName == "" || len(safeName) < 2 {
			return "question" // Use question mark emoji as fallback
		}
		return safeName
	}
}

// downloadMatrixAvatar downloads an avatar image from a Matrix MXC URI
func (p *Plugin) downloadMatrixAvatar(avatarURL string) ([]byte, error) {
	if p.matrixClient == nil {
		return nil, errors.New("Matrix client not configured")
	}

	// Use the Matrix client's dedicated avatar download method
	return p.matrixClient.DownloadAvatar(avatarURL)
}

// ProfileUpdateContext provides context information for profile updates
type ProfileUpdateContext struct {
	EventID string // Matrix event ID if update is from an event
	Source  string // "api" for proactive checks, "event" for Matrix member events
}

// updateMattermostUserProfile updates an existing Mattermost user's profile from Matrix
// Can be called either proactively (fetching current profile) or reactively (from Matrix events)
func (p *Plugin) updateMattermostUserProfile(mattermostUser *model.User, matrixUserID string, context *ProfileUpdateContext, profileData ...*matrix.UserProfile) {
	if p.matrixClient == nil {
		return
	}

	var profile *matrix.UserProfile
	var err error

	// Determine profile data source
	if len(profileData) > 0 && profileData[0] != nil {
		// Use provided profile data (from Matrix event)
		profile = profileData[0]
	} else {
		// Fetch current Matrix profile (proactive check)
		profile, err = p.matrixClient.GetUserProfile(matrixUserID)
		if err != nil {
			p.API.LogWarn("Failed to get Matrix user profile for update", "error", err, "user_id", matrixUserID)
			return
		}
	}

	var needsUpdate bool
	updatedUser := *mattermostUser // Create a copy

	// Check display name updates
	if profile.DisplayName != "" {
		// Parse first/last name from display name
		firstName, lastName := parseDisplayName(profile.DisplayName)

		// Update nickname (display name)
		if updatedUser.Nickname != profile.DisplayName {
			if context.Source == "event" {
				p.API.LogDebug("Updating user display name from Matrix event", "user_id", mattermostUser.Id, "old_name", mattermostUser.Nickname, "new_name", profile.DisplayName, "matrix_user_id", matrixUserID, "event_id", context.EventID)
			} else {
				p.API.LogDebug("Updating display name from proactive check", "user_id", mattermostUser.Id, "old", mattermostUser.Nickname, "new", profile.DisplayName)
			}
			updatedUser.Nickname = profile.DisplayName
			needsUpdate = true
		}

		// Update first/last name if they changed
		if updatedUser.FirstName != firstName {
			updatedUser.FirstName = firstName
			needsUpdate = true
		}
		if updatedUser.LastName != lastName {
			updatedUser.LastName = lastName
			needsUpdate = true
		}
	}

	// Update user profile if needed
	if needsUpdate {
		if _, appErr := p.API.UpdateUser(&updatedUser); appErr != nil {
			if context.Source == "event" {
				p.API.LogError("Failed to update Mattermost user profile from Matrix event", "error", appErr, "user_id", mattermostUser.Id, "matrix_user_id", matrixUserID, "event_id", context.EventID)
			} else {
				p.API.LogWarn("Failed to update Mattermost user profile from proactive check", "error", appErr, "user_id", mattermostUser.Id, "matrix_user_id", matrixUserID)
			}
		} else {
			if context.Source == "event" {
				p.API.LogDebug("Successfully updated Mattermost user profile from Matrix event", "user_id", mattermostUser.Id, "matrix_user_id", matrixUserID, "display_name", profile.DisplayName, "event_id", context.EventID)
			} else {
				p.API.LogDebug("Updated Mattermost user profile from proactive check", "user_id", mattermostUser.Id, "matrix_user_id", matrixUserID, "display_name", profile.DisplayName)
			}
		}
	}

	// Check avatar updates
	if profile.AvatarURL != "" {
		p.updateMattermostUserAvatar(mattermostUser, matrixUserID, profile.AvatarURL, context)
	}
}

// updateMattermostUserAvatar updates a Mattermost user's profile image from Matrix
// Compares current and new image data to avoid unnecessary updates
func (p *Plugin) updateMattermostUserAvatar(mattermostUser *model.User, matrixUserID, matrixAvatarURL string, context *ProfileUpdateContext) {
	// Get current Mattermost profile image
	currentAvatarData, appErr := p.API.GetProfileImage(mattermostUser.Id)
	if appErr != nil {
		if context.Source == "event" {
			p.API.LogWarn("Failed to get current Mattermost profile image for comparison", "error", appErr, "user_id", mattermostUser.Id, "matrix_user_id", matrixUserID, "event_id", context.EventID)
		} else {
			p.API.LogDebug("Failed to get current Mattermost profile image for comparison", "error", appErr, "user_id", mattermostUser.Id, "matrix_user_id", matrixUserID)
		}
		// Continue with update even if we can't get current image
		currentAvatarData = nil
	}

	// Download Matrix avatar image
	newAvatarData, err := p.downloadMatrixAvatar(matrixAvatarURL)
	if err != nil {
		if context.Source == "event" {
			p.API.LogWarn("Failed to download Matrix avatar for comparison", "error", err, "user_id", mattermostUser.Id, "matrix_user_id", matrixUserID, "avatar_url", matrixAvatarURL, "event_id", context.EventID)
		} else {
			p.API.LogDebug("Failed to download Matrix avatar for comparison", "error", err, "user_id", mattermostUser.Id, "matrix_user_id", matrixUserID, "avatar_url", matrixAvatarURL)
		}
		return
	}

	// Compare image data to see if update is needed
	if currentAvatarData != nil && p.compareImageData(currentAvatarData, newAvatarData) {
		p.API.LogDebug("Matrix avatar unchanged, skipping update", "user_id", mattermostUser.Id, "matrix_user_id", matrixUserID, "source", context.Source)
		return
	}

	// Images are different, update the profile image
	appErr = p.API.SetProfileImage(mattermostUser.Id, newAvatarData)
	if appErr != nil {
		if context.Source == "event" {
			p.API.LogError("Failed to update Mattermost user avatar from Matrix event", "error", appErr, "user_id", mattermostUser.Id, "matrix_user_id", matrixUserID, "event_id", context.EventID)
		} else {
			p.API.LogWarn("Failed to update Mattermost user avatar from proactive check", "error", appErr, "user_id", mattermostUser.Id, "matrix_user_id", matrixUserID)
		}
		return
	}

	// Log successful avatar update
	if context.Source == "event" {
		p.API.LogDebug("Successfully updated Mattermost user avatar from Matrix event", "user_id", mattermostUser.Id, "matrix_user_id", matrixUserID, "avatar_url", matrixAvatarURL, "event_id", context.EventID, "size_bytes", len(newAvatarData))
	} else {
		p.API.LogDebug("Updated Mattermost user avatar from proactive check", "user_id", mattermostUser.Id, "matrix_user_id", matrixUserID, "avatar_url", matrixAvatarURL, "size_bytes", len(newAvatarData))
	}
}

// compareImageData compares two image byte arrays to determine if they're the same
// Uses a simple byte comparison for now, could be enhanced with more sophisticated comparison
func (p *Plugin) compareImageData(currentData, newData []byte) bool {
	// Quick size check first
	if len(currentData) != len(newData) {
		return false
	}

	// If both are empty, they're the same
	if len(currentData) == 0 {
		return true
	}

	// Compare byte by byte
	for i := range currentData {
		if currentData[i] != newData[i] {
			return false
		}
	}

	return true
}

// syncMatrixMemberEventToMattermost handles Matrix member events (joins, leaves, profile changes)
func (p *Plugin) syncMatrixMemberEventToMattermost(event MatrixEvent, channelID string) error {
	p.API.LogDebug("Processing Matrix member event", "event_id", event.EventID, "sender", event.Sender, "channel_id", channelID)

	// Skip events from our own ghost users to prevent loops
	if p.isGhostUser(event.Sender) {
		p.API.LogDebug("Ignoring member event from ghost user", "sender", event.Sender, "event_id", event.EventID)
		return nil
	}

	// Extract membership state and content
	membership, ok := event.Content["membership"].(string)
	if !ok {
		p.API.LogDebug("Member event missing membership field", "event_id", event.EventID, "sender", event.Sender)
		return nil
	}

	// We only care about profile changes for existing members
	// Profile changes happen when membership is "join" and there are profile fields
	if membership != "join" {
		p.API.LogDebug("Ignoring non-join member event", "event_id", event.EventID, "sender", event.Sender, "membership", membership)
		return nil
	}

	// Check if this is a profile change (has displayname or avatar_url in content)
	displayName, hasDisplayName := event.Content["displayname"].(string)
	avatarURL, hasAvatarURL := event.Content["avatar_url"].(string)

	if !hasDisplayName && !hasAvatarURL {
		p.API.LogDebug("Member event has no profile information", "event_id", event.EventID, "sender", event.Sender)
		return nil
	}

	p.API.LogDebug("Detected Matrix profile change", "event_id", event.EventID, "sender", event.Sender, "display_name", displayName, "avatar_url", avatarURL)

	// Check if we have a Mattermost user for this Matrix user
	userMapKey := "matrix_user_" + event.Sender
	userIDBytes, err := p.kvstore.Get(userMapKey)
	if err != nil || len(userIDBytes) == 0 {
		p.API.LogDebug("No Mattermost user found for Matrix user profile change", "matrix_user_id", event.Sender, "event_id", event.EventID)
		return nil // User doesn't exist in Mattermost yet, ignore profile change
	}

	mattermostUserID := string(userIDBytes)

	// Get the existing Mattermost user
	mattermostUser, appErr := p.API.GetUser(mattermostUserID)
	if appErr != nil {
		p.API.LogWarn("Failed to get Mattermost user for profile update", "error", appErr, "user_id", mattermostUserID, "matrix_user_id", event.Sender)
		return nil
	}

	p.API.LogDebug("Found Mattermost user for profile update", "user_id", mattermostUser.Id, "username", mattermostUser.Username, "matrix_user_id", event.Sender)

	// Create profile data from Matrix event
	eventProfile := &matrix.UserProfile{
		DisplayName: displayName,
		AvatarURL:   avatarURL,
	}

	// Update the user's profile using the unified method
	context := &ProfileUpdateContext{
		EventID: event.EventID,
		Source:  "event",
	}

	p.updateMattermostUserProfile(mattermostUser, event.Sender, context, eventProfile)

	return nil
}
