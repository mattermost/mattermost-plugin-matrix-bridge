package matrix

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"time"

	"github.com/google/uuid"
	"github.com/pkg/errors"
)

type Client struct {
	serverURL   string
	accessToken string
	userID      string
	httpClient  *http.Client
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

func NewClient(serverURL, accessToken, userID string) *Client {
	return &Client{
		serverURL:   serverURL,
		accessToken: accessToken,
		userID:      userID,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (c *Client) SendMessage(roomID, message string) (*SendEventResponse, error) {
	if c.serverURL == "" || c.accessToken == "" {
		return nil, errors.New("matrix client not configured")
	}

	content := MessageContent{
		MsgType: "m.text",
		Body:    message,
	}

	return c.sendEvent(roomID, "m.room.message", content)
}

func (c *Client) SendFormattedMessage(roomID, textBody, htmlBody string) (*SendEventResponse, error) {
	if c.serverURL == "" || c.accessToken == "" {
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
	req.Header.Set("Authorization", "Bearer "+c.accessToken)

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
	if c.serverURL == "" || c.accessToken == "" {
		return errors.New("matrix client not configured")
	}

	url := c.serverURL + "/_matrix/client/v3/account/whoami"
	
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return errors.Wrap(err, "failed to create request")
	}

	req.Header.Set("Authorization", "Bearer "+c.accessToken)

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