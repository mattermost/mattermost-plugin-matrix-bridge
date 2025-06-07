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

// Ghost user management functions

type GhostUser struct {
	UserID   string `json:"user_id"`
	Username string `json:"username"`
}

// CreateGhostUser creates a ghost user for a Mattermost user
func (c *Client) CreateGhostUser(mattermostUserID, mattermostUsername string) (*GhostUser, error) {
	if c.asToken == "" {
		return nil, errors.New("application service token not configured")
	}

	// Generate ghost user ID following the namespace pattern
	ghostUsername := fmt.Sprintf("_mattermost_%s", mattermostUserID)
	ghostUserID := fmt.Sprintf("@%s:synapse-wiggin77.ngrok.io", ghostUsername)

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

	return &GhostUser{
		UserID:   ghostUserID,
		Username: ghostUsername,
	}, nil
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