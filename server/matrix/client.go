package matrix

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pkg/errors"
)

type Client struct {
	serverURL  string
	asToken    string // Application Service token for all operations
	httpClient *http.Client
}

type MessageContent struct {
	MsgType string `json:"msgtype"`
	Body    string `json:"body"`
	Format  string `json:"format,omitempty"`
	FormattedBody string `json:"formatted_body,omitempty"`
}

type SendEventResponse struct {
	EventID string `json:"event_id"`
}

func NewClient(serverURL, asToken string) *Client {
	return &Client{
		serverURL: serverURL,
		asToken:   asToken,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (c *Client) SendMessage(roomID, message string) (*SendEventResponse, error) {
	if c.serverURL == "" || c.asToken == "" {
		return nil, errors.New("matrix client not configured")
	}

	content := MessageContent{
		MsgType: "m.text",
		Body:    message,
	}

	return c.sendEvent(roomID, "m.room.message", content)
}

func (c *Client) SendFormattedMessage(roomID, textBody, htmlBody string) (*SendEventResponse, error) {
	if c.serverURL == "" || c.asToken == "" {
		return nil, errors.New("matrix client not configured")
	}

	content := MessageContent{
		MsgType:       "m.text",
		Body:          textBody,
		Format:        "org.matrix.custom.html",
		FormattedBody: htmlBody,
	}

	return c.sendEvent(roomID, "m.room.message", content)
}

func (c *Client) sendEvent(roomID, eventType string, content interface{}) (*SendEventResponse, error) {
	txnID := uuid.New().String()
	endpoint := path.Join("/_matrix/client/v3/rooms", roomID, "send", eventType, txnID)
	url := c.serverURL + endpoint

	jsonData, err := json.Marshal(content)
	if err != nil {
		return nil, errors.Wrap(err, "failed to marshal event content")
	}

	req, err := http.NewRequest("PUT", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, errors.Wrap(err, "failed to create request")
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.asToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, errors.Wrap(err, "failed to send request")
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, errors.Wrap(err, "failed to read response body")
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("matrix API error: %d %s", resp.StatusCode, string(body))
	}

	var response SendEventResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal response")
	}

	return &response, nil
}

// SendReactionAsGhost sends a reaction to a message as a ghost user
func (c *Client) SendReactionAsGhost(roomID, eventID, emoji, ghostUserID string) (*SendEventResponse, error) {
	if c.asToken == "" {
		return nil, errors.New("application service token not configured")
	}

	// Matrix reaction content structure
	content := map[string]interface{}{
		"m.relates_to": map[string]interface{}{
			"rel_type": "m.annotation",
			"event_id": eventID,
			"key":      emoji,
		},
	}

	return c.sendEventAsUser(roomID, "m.reaction", content, ghostUserID)
}

func (c *Client) TestConnection() error {
	if c.serverURL == "" || c.asToken == "" {
		return errors.New("matrix client not configured")
	}

	url := c.serverURL + "/_matrix/client/v3/account/whoami"
	
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return errors.Wrap(err, "failed to create request")
	}

	req.Header.Set("Authorization", "Bearer "+c.asToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return errors.Wrap(err, "failed to send request")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("matrix connection test failed: %d %s", resp.StatusCode, string(body))
	}

	return nil
}

func (c *Client) JoinRoom(roomIdentifier string) error {
	if c.serverURL == "" || c.asToken == "" {
		return errors.New("matrix client not configured")
	}

	var requestURL string
	if strings.HasPrefix(roomIdentifier, "#") {
		// For room aliases, use the /join endpoint with URL-encoded alias
		encodedAlias := url.PathEscape(roomIdentifier)
		requestURL = c.serverURL + "/_matrix/client/v3/join/" + encodedAlias
	} else {
		// For room IDs, use the original endpoint
		requestURL = c.serverURL + "/_matrix/client/v3/rooms/" + roomIdentifier + "/join"
	}

	req, err := http.NewRequest("POST", requestURL, bytes.NewBuffer([]byte("{}")))
	if err != nil {
		return errors.Wrap(err, "failed to create join request")
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.asToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return errors.Wrap(err, "failed to send join request")
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return errors.Wrap(err, "failed to read join response")
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to join room: %d %s", resp.StatusCode, string(body))
	}

	return nil
}

// JoinRoomAsUser joins a room as a specific user (using application service impersonation)
func (c *Client) JoinRoomAsUser(roomIdentifier, userID string) error {
	if c.serverURL == "" || c.asToken == "" {
		return errors.New("matrix client not configured")
	}

	var requestURL string
	if strings.HasPrefix(roomIdentifier, "#") {
		// For room aliases, use the /join endpoint with URL-encoded alias
		encodedAlias := url.PathEscape(roomIdentifier)
		requestURL = c.serverURL + "/_matrix/client/v3/join/" + encodedAlias
	} else {
		// For room IDs, use the original endpoint
		requestURL = c.serverURL + "/_matrix/client/v3/rooms/" + roomIdentifier + "/join"
	}

	// Add user_id query parameter for impersonation
	if userID != "" {
		requestURL += "?user_id=" + url.QueryEscape(userID)
	}

	req, err := http.NewRequest("POST", requestURL, bytes.NewBuffer([]byte("{}")))
	if err != nil {
		return errors.Wrap(err, "failed to create join request")
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.asToken) // Use AS token for impersonation

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return errors.Wrap(err, "failed to send join request")
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return errors.Wrap(err, "failed to read join response")
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to join room as user: %d %s", resp.StatusCode, string(body))
	}

	return nil
}

func (c *Client) CreateRoom(name, topic, serverDomain string) (string, error) {
	if c.serverURL == "" || c.asToken == "" {
		return "", errors.New("matrix client not configured")
	}

	// Create room alias from name
	alias := strings.ToLower(strings.ReplaceAll(name, " ", "-"))
	alias = strings.ReplaceAll(alias, "_", "-")
	roomAlias := ""
	if serverDomain != "" {
		roomAlias = "#" + alias + ":" + serverDomain
	}

	roomData := map[string]interface{}{
		"name":       name,
		"topic":      topic,
		"preset":     "public_chat",
		"visibility": "public",
	}

	// Add room alias if we have a server domain
	if roomAlias != "" {
		roomData["room_alias_name"] = alias // Just the local part for room_alias_name
	}

	jsonData, err := json.Marshal(roomData)
	if err != nil {
		return "", errors.Wrap(err, "failed to marshal room creation data")
	}

	url := c.serverURL + "/_matrix/client/v3/createRoom"

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return "", errors.Wrap(err, "failed to create room creation request")
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.asToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", errors.Wrap(err, "failed to send room creation request")
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", errors.Wrap(err, "failed to read room creation response")
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to create room: %d %s", resp.StatusCode, string(body))
	}

	var response struct {
		RoomID string `json:"room_id"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return "", errors.Wrap(err, "failed to unmarshal room creation response")
	}

	// Return the room alias if we created one, otherwise return the room ID
	if roomAlias != "" {
		return roomAlias, nil
	}
	return response.RoomID, nil
}

// extractServerDomain extracts the hostname from the Matrix server URL
func (c *Client) extractServerDomain() (string, error) {
	if c.serverURL == "" {
		return "", errors.New("server URL not configured")
	}

	parsedURL, err := url.Parse(c.serverURL)
	if err != nil {
		return "", errors.Wrap(err, "failed to parse server URL")
	}

	hostname := parsedURL.Hostname()
	if hostname == "" {
		return "", errors.New("could not extract hostname from server URL")
	}

	return hostname, nil
}

// Ghost user management functions

type GhostUser struct {
	UserID   string `json:"user_id"`
	Username string `json:"username"`
}

// CreateGhostUser creates a ghost user for a Mattermost user with display name and avatar data
func (c *Client) CreateGhostUser(mattermostUserID, mattermostUsername, displayName string, avatarData []byte, avatarContentType string) (*GhostUser, error) {
	if c.asToken == "" {
		return nil, errors.New("application service token not configured")
	}

	// Extract server domain from serverURL
	serverDomain, err := c.extractServerDomain()
	if err != nil {
		return nil, errors.Wrap(err, "failed to extract server domain")
	}

	// Generate ghost user ID following the namespace pattern
	ghostUsername := fmt.Sprintf("_mattermost_%s", mattermostUserID)
	ghostUserID := fmt.Sprintf("@%s:%s", ghostUsername, serverDomain)

	// Registration data
	regData := map[string]interface{}{
		"type":     "m.login.application_service",
		"username": ghostUsername,
	}

	jsonData, err := json.Marshal(regData)
	if err != nil {
		return nil, errors.Wrap(err, "failed to marshal registration data")
	}

	url := c.serverURL + "/_matrix/client/v3/register"

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, errors.Wrap(err, "failed to create registration request")
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.asToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, errors.Wrap(err, "failed to send registration request")
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, errors.Wrap(err, "failed to read registration response")
	}

	// 200 = created, 400 with M_USER_IN_USE = already exists (both are fine)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusBadRequest {
		return nil, fmt.Errorf("failed to create ghost user: %d %s", resp.StatusCode, string(body))
	}

	// Check if it's a "user already exists" error
	if resp.StatusCode == http.StatusBadRequest {
		var errorResp struct {
			Errcode string `json:"errcode"`
		}
		if err := json.Unmarshal(body, &errorResp); err == nil && errorResp.Errcode == "M_USER_IN_USE" {
			// User already exists, that's fine
		} else {
			return nil, fmt.Errorf("failed to create ghost user: %d %s", resp.StatusCode, string(body))
		}
	}

	// Set display name for the ghost user if provided
	if displayName != "" {
		err = c.SetDisplayName(ghostUserID, displayName)
		if err != nil {
			// Don't fail user creation if display name setting fails
			// Return the error info in a way the caller can log it
			return &GhostUser{
				UserID:   ghostUserID,
				Username: ghostUsername,
			}, errors.Wrap(err, "ghost user created but failed to set display name")
		}
	}

	// Upload and set avatar for the ghost user if provided
	if len(avatarData) > 0 {
		// Upload avatar to Matrix
		mxcURI, err := c.UploadAvatarFromData(avatarData, avatarContentType)
		if err != nil {
			// Don't fail user creation if avatar upload fails
			return &GhostUser{
				UserID:   ghostUserID,
				Username: ghostUsername,
			}, errors.Wrap(err, "ghost user created but failed to upload avatar")
		}
		
		// Set the uploaded avatar
		err = c.SetAvatarURL(ghostUserID, mxcURI)
		if err != nil {
			// Don't fail user creation if avatar setting fails
			return &GhostUser{
				UserID:   ghostUserID,
				Username: ghostUsername,
			}, errors.Wrap(err, "ghost user created but failed to set avatar")
		}
	}

	return &GhostUser{
		UserID:   ghostUserID,
		Username: ghostUsername,
	}, nil
}

// SetDisplayName sets the display name for a user (using application service impersonation)
func (c *Client) SetDisplayName(userID, displayName string) error {
	if c.asToken == "" {
		return errors.New("application service token not configured")
	}

	// Content for the display name event
	content := map[string]interface{}{
		"displayname": displayName,
	}

	jsonData, err := json.Marshal(content)
	if err != nil {
		return errors.Wrap(err, "failed to marshal display name content")
	}

	// Use the profile API endpoint with user impersonation
	requestURL := c.serverURL + "/_matrix/client/v3/profile/" + url.PathEscape(userID) + "/displayname"
	if userID != "" {
		requestURL += "?user_id=" + url.QueryEscape(userID)
	}

	req, err := http.NewRequest("PUT", requestURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return errors.Wrap(err, "failed to create display name request")
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.asToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return errors.Wrap(err, "failed to send display name request")
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return errors.Wrap(err, "failed to read display name response")
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to set display name: %d %s", resp.StatusCode, string(body))
	}

	return nil
}

// SetAvatarURL sets the avatar URL for a user (using application service impersonation)
func (c *Client) SetAvatarURL(userID, avatarURL string) error {
	if c.asToken == "" {
		return errors.New("application service token not configured")
	}

	// Content for the avatar URL event
	content := map[string]interface{}{
		"avatar_url": avatarURL,
	}

	jsonData, err := json.Marshal(content)
	if err != nil {
		return errors.Wrap(err, "failed to marshal avatar URL content")
	}

	// Use the profile API endpoint with user impersonation
	requestURL := c.serverURL + "/_matrix/client/v3/profile/" + url.PathEscape(userID) + "/avatar_url"
	if userID != "" {
		requestURL += "?user_id=" + url.QueryEscape(userID)
	}

	req, err := http.NewRequest("PUT", requestURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return errors.Wrap(err, "failed to create avatar URL request")
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.asToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return errors.Wrap(err, "failed to send avatar URL request")
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return errors.Wrap(err, "failed to read avatar URL response")
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to set avatar URL: %d %s", resp.StatusCode, string(body))
	}

	return nil
}

// UploadMedia uploads media content to the Matrix server and returns the mxc:// URI
func (c *Client) UploadMedia(data []byte, filename, contentType string) (string, error) {
	if c.asToken == "" {
		return "", errors.New("application service token not configured")
	}

	// Use the media upload endpoint
	requestURL := c.serverURL + "/_matrix/media/v3/upload"
	if filename != "" {
		requestURL += "?filename=" + url.QueryEscape(filename)
	}

	req, err := http.NewRequest("POST", requestURL, bytes.NewBuffer(data))
	if err != nil {
		return "", errors.Wrap(err, "failed to create media upload request")
	}

	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+c.asToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", errors.Wrap(err, "failed to send media upload request")
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", errors.Wrap(err, "failed to read media upload response")
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to upload media: %d %s", resp.StatusCode, string(body))
	}

	var response struct {
		ContentURI string `json:"content_uri"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return "", errors.Wrap(err, "failed to unmarshal media upload response")
	}

	return response.ContentURI, nil
}

// UploadAvatarFromData uploads avatar image data to Matrix and returns mxc:// URI
func (c *Client) UploadAvatarFromData(imageData []byte, contentType string) (string, error) {
	if len(imageData) == 0 {
		return "", errors.New("image data is empty")
	}

	// Determine content type if not provided
	if contentType == "" {
		contentType = "application/octet-stream" // fallback
	}

	// Generate filename based on content type
	var filename string
	switch contentType {
	case "image/jpeg":
		filename = "avatar.jpg"
	case "image/png":
		filename = "avatar.png"
	case "image/gif":
		filename = "avatar.gif"
	case "image/webp":
		filename = "avatar.webp"
	default:
		filename = "avatar"
	}

	// Upload to Matrix
	mxcURI, err := c.UploadMedia(imageData, filename, contentType)
	if err != nil {
		return "", errors.Wrap(err, "failed to upload avatar to Matrix")
	}

	return mxcURI, nil
}

// UpdateGhostUserAvatar uploads new avatar data and updates the ghost user's avatar
func (c *Client) UpdateGhostUserAvatar(userID string, avatarData []byte, avatarContentType string) error {
	if len(avatarData) == 0 {
		return errors.New("avatar data is empty")
	}

	// Upload avatar to Matrix
	mxcURI, err := c.UploadAvatarFromData(avatarData, avatarContentType)
	if err != nil {
		return errors.Wrap(err, "failed to upload avatar")
	}
	
	// Set the uploaded avatar
	err = c.SetAvatarURL(userID, mxcURI)
	if err != nil {
		return errors.Wrap(err, "failed to set avatar URL")
	}

	return nil
}

// SendMessageAsGhost sends a message as a ghost user
func (c *Client) SendMessageAsGhost(roomID, message, ghostUserID string) (*SendEventResponse, error) {
	if c.asToken == "" {
		return nil, errors.New("application service token not configured")
	}

	content := MessageContent{
		MsgType: "m.text",
		Body:    message,
	}

	return c.sendEventAsUser(roomID, "m.room.message", content, ghostUserID)
}

// SendFormattedMessageAsGhost sends a formatted message as a ghost user
func (c *Client) SendFormattedMessageAsGhost(roomID, textBody, htmlBody, ghostUserID string) (*SendEventResponse, error) {
	if c.asToken == "" {
		return nil, errors.New("application service token not configured")
	}

	content := MessageContent{
		MsgType:       "m.text",
		Body:          textBody,
		Format:        "org.matrix.custom.html",
		FormattedBody: htmlBody,
	}

	return c.sendEventAsUser(roomID, "m.room.message", content, ghostUserID)
}

// EditMessageAsGhost edits an existing message as a ghost user
func (c *Client) EditMessageAsGhost(roomID, eventID, newMessage, ghostUserID string) (*SendEventResponse, error) {
	if c.asToken == "" {
		return nil, errors.New("application service token not configured")
	}

	// Matrix edit event content structure
	content := map[string]interface{}{
		"msgtype": "m.text",
		"body":    " * " + newMessage, // Fallback for clients that don't support edits
		"m.new_content": map[string]interface{}{
			"msgtype": "m.text",
			"body":    newMessage,
		},
		"m.relates_to": map[string]interface{}{
			"rel_type": "m.replace",
			"event_id": eventID,
		},
	}

	return c.sendEventAsUser(roomID, "m.room.message", content, ghostUserID)
}


// sendEventAsUser sends an event as a specific user (using application service impersonation)
func (c *Client) sendEventAsUser(roomID, eventType string, content interface{}, userID string) (*SendEventResponse, error) {
	txnID := uuid.New().String()
	endpoint := path.Join("/_matrix/client/v3/rooms", roomID, "send", eventType, txnID)
	reqURL := c.serverURL + endpoint

	// Add user_id query parameter for impersonation
	if userID != "" {
		reqURL += "?user_id=" + url.QueryEscape(userID)
	}

	jsonData, err := json.Marshal(content)
	if err != nil {
		return nil, errors.Wrap(err, "failed to marshal event content")
	}

	req, err := http.NewRequest("PUT", reqURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, errors.Wrap(err, "failed to create request")
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.asToken) // Use AS token for impersonation

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, errors.Wrap(err, "failed to send request")
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, errors.Wrap(err, "failed to read response body")
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("matrix API error: %d %s", resp.StatusCode, string(body))
	}

	var response SendEventResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal response")
	}

	return &response, nil
}

func (c *Client) ResolveRoomAlias(roomAlias string) (string, error) {
	if c.serverURL == "" || c.asToken == "" {
		return "", errors.New("matrix client not configured")
	}

	if !strings.HasPrefix(roomAlias, "#") {
		// If it's already a room ID, return as-is
		return roomAlias, nil
	}

	// URL encode the room alias
	encodedAlias := url.QueryEscape(roomAlias)
	requestURL := c.serverURL + "/_matrix/client/v3/directory/room/" + encodedAlias

	req, err := http.NewRequest("GET", requestURL, nil)
	if err != nil {
		return "", errors.Wrap(err, "failed to create alias resolution request")
	}

	req.Header.Set("Authorization", "Bearer "+c.asToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", errors.Wrap(err, "failed to send alias resolution request")
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", errors.Wrap(err, "failed to read alias resolution response")
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to resolve room alias: %d %s", resp.StatusCode, string(body))
	}

	var response struct {
		RoomID string `json:"room_id"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return "", errors.Wrap(err, "failed to unmarshal alias resolution response")
	}

	return response.RoomID, nil
}