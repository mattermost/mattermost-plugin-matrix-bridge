// Package matrix provides Matrix client functionality for the Mattermost bridge.
package matrix

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/mattermost/mattermost/server/public/plugin"
	"github.com/pkg/errors"
)

// Error represents a Matrix API error response
type Error struct {
	ErrCode    string `json:"errcode"`
	ErrMsg     string `json:"error"`
	StatusCode int    `json:"-"`
}

// Error implements the error interface
func (e *Error) Error() string {
	return fmt.Sprintf("matrix API error: %d %s - %s", e.StatusCode, e.ErrCode, e.ErrMsg)
}

// IsAlreadyJoined checks if the error indicates the user is already in the room
func (e *Error) IsAlreadyJoined() bool {
	return e.ErrCode == "M_BAD_STATE" ||
		strings.Contains(strings.ToLower(e.ErrMsg), "already joined") ||
		strings.Contains(strings.ToLower(e.ErrMsg), "already in the room") ||
		strings.Contains(e.ErrCode, "ALREADY_JOINED")
}

// parseMatrixError attempts to parse a Matrix error from response body
func parseMatrixError(statusCode int, body []byte) *Error {
	var mErr Error
	mErr.StatusCode = statusCode

	if err := json.Unmarshal(body, &mErr); err != nil {
		// Fallback for non-JSON responses
		mErr.ErrCode = "UNKNOWN"
		mErr.ErrMsg = string(body)
	}

	return &mErr
}

// isForbiddenJoinError checks if an error indicates a forbidden join attempt
func isForbiddenJoinError(err error) bool {
	var matrixErr *Error
	if errors.As(err, &matrixErr) {
		return matrixErr.StatusCode == http.StatusForbidden ||
			matrixErr.ErrCode == "M_FORBIDDEN" ||
			strings.Contains(strings.ToLower(matrixErr.ErrMsg), "not invited")
	}

	// Fallback for legacy error types that aren't structured
	return strings.Contains(err.Error(), "403") ||
		strings.Contains(err.Error(), "M_FORBIDDEN") ||
		strings.Contains(err.Error(), "not invited")
}

// Path traversal validation functions

// ValidatePathComponent checks for path traversal sequences in URL path components
func ValidatePathComponent(component string) error {
	if strings.Contains(component, "..") {
		return errors.Errorf("path traversal detected in component: %s", component)
	}
	return nil
}

// BuildSecureURL constructs URL paths with proper escaping and validation
func BuildSecureURL(baseURL string, pathComponents ...string) (string, error) {
	var urlParts []string

	for _, component := range pathComponents {
		if err := ValidatePathComponent(component); err != nil {
			return "", err
		}
		urlParts = append(urlParts, url.PathEscape(component))
	}

	return baseURL + strings.Join(urlParts, "/"), nil
}

// ValidateMXCComponents validates MXC URI components for path traversal attacks
func ValidateMXCComponents(serverName, mediaID string) error {
	if err := ValidatePathComponent(serverName); err != nil {
		return errors.Wrap(err, "invalid server name in MXC URI")
	}
	if err := ValidatePathComponent(mediaID); err != nil {
		return errors.Wrap(err, "invalid media ID in MXC URI")
	}
	return nil
}

// Logger interface for matrix client logging
type Logger interface {
	LogDebug(message string, keyValuePairs ...any)
	LogInfo(message string, keyValuePairs ...any)
	LogWarn(message string, keyValuePairs ...any)
	LogError(message string, keyValuePairs ...any)
}

// APILogger implements Logger interface using plugin.API
type APILogger struct {
	api plugin.API
}

// NewAPILogger creates a new APILogger
func NewAPILogger(api plugin.API) Logger {
	return &APILogger{api: api}
}

// LogDebug logs a debug message using the plugin API
func (l *APILogger) LogDebug(message string, keyValuePairs ...any) {
	l.api.LogDebug(message, keyValuePairs...)
}

// LogInfo logs an info message using the plugin API
func (l *APILogger) LogInfo(message string, keyValuePairs ...any) {
	l.api.LogInfo(message, keyValuePairs...)
}

// LogWarn logs a warning message using the plugin API
func (l *APILogger) LogWarn(message string, keyValuePairs ...any) {
	l.api.LogWarn(message, keyValuePairs...)
}

// LogError logs an error message using the plugin API
func (l *APILogger) LogError(message string, keyValuePairs ...any) {
	l.api.LogError(message, keyValuePairs...)
}

// TestLogger implements Logger interface for testing
type TestLogger interface {
	Logf(format string, args ...any)
}

// testLogger implements Logger interface using a TestLogger (like testing.T)
type testLogger struct {
	t TestLogger
}

// NewTestLogger creates a new test logger that logs to a TestLogger (like testing.T)
func NewTestLogger(t TestLogger) Logger {
	return &testLogger{t: t}
}

func (l *testLogger) LogDebug(message string, keyValuePairs ...any) {
	if l.t != nil {
		l.t.Logf("[DEBUG] %s %v", message, keyValuePairs)
	}
}

func (l *testLogger) LogInfo(message string, keyValuePairs ...any) {
	if l.t != nil {
		l.t.Logf("[INFO] %s %v", message, keyValuePairs)
	}
}

func (l *testLogger) LogWarn(message string, keyValuePairs ...any) {
	if l.t != nil {
		l.t.Logf("[WARN] %s %v", message, keyValuePairs)
	}
}

func (l *testLogger) LogError(message string, keyValuePairs ...any) {
	if l.t != nil {
		l.t.Logf("[ERROR] %s %v", message, keyValuePairs)
	}
}

// Client represents a Matrix HTTP client for communicating with Matrix servers.
type Client struct {
	serverURL    string
	asToken      string // Application Service token for all operations
	remoteID     string // Plugin remote ID for metadata
	httpClient   *http.Client
	logger       Logger
	serverDomain string // explicit server domain for testing

	// Rate limiting
	rateLimitConfig     RateLimitConfig
	roomCreationLimiter *TokenBucket
	messageLimiter      *TokenBucket
	inviteLimiter       *TokenBucket
	registrationLimiter *TokenBucket
	joinLimiter         *TokenBucket
}

// waitForRateLimit applies rate limiting for the specified operation
func (c *Client) waitForRateLimit(limiter *TokenBucket, operation string) error {
	if !c.rateLimitConfig.Enabled {
		c.logger.LogDebug("Rate limiting disabled, proceeding without throttling", "operation", operation)
		return nil
	}

	if limiter == nil {
		c.logger.LogWarn("Rate limiter not initialized for operation", "operation", operation)
		return nil
	}

	c.logger.LogDebug("Applying rate limiting", "operation", operation)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	startTime := time.Now()
	if err := limiter.Wait(ctx); err != nil {
		c.logger.LogWarn("Rate limiting failed", "operation", operation, "error", err, "waited_duration", time.Since(startTime))
		return errors.Wrap(err, operation+" rate limited")
	}

	waitDuration := time.Since(startTime)
	if waitDuration > 100*time.Millisecond {
		c.logger.LogInfo("Rate limiting applied", "operation", operation, "waited_duration", waitDuration)
	} else {
		c.logger.LogDebug("Rate limiting completed (no wait required)", "operation", operation, "waited_duration", waitDuration)
	}

	return nil
}

// MessageContent represents the content structure for Matrix messages.
type MessageContent struct {
	MsgType       string `json:"msgtype"`
	Body          string `json:"body"`
	Format        string `json:"format,omitempty"`
	FormattedBody string `json:"formatted_body,omitempty"`
}

// FileAttachment represents a file attachment for Matrix messages.
type FileAttachment struct {
	Filename string `json:"filename"`
	MxcURI   string `json:"mxc_uri"`
	MimeType string `json:"mimetype"`
	Size     int64  `json:"size"`
}

// MessageRequest represents a request to send a message as a ghost user with all optional parameters.
type MessageRequest struct {
	RoomID         string           `json:"room_id"`           // Required: Matrix room ID
	GhostUserID    string           `json:"ghost_user_id"`     // Required: Ghost user ID to send as
	Message        string           `json:"message"`           // Optional: Plain text message content
	HTMLMessage    string           `json:"html_message"`      // Optional: HTML formatted message content
	ThreadEventID  string           `json:"thread_event_id"`   // Optional: Event ID to thread/reply to
	PostID         string           `json:"post_id"`           // Optional: Mattermost post ID metadata
	Files          []FileAttachment `json:"files"`             // Optional: File attachments
	ReplyToEventID string           `json:"reply_to_event_id"` // Optional: Event ID to reply to (for files)
	Mentions       map[string]any   `json:"mentions"`          // Optional: Matrix mentions data (m.mentions field)
}

// SendEventResponse represents the response from Matrix when sending events.
type SendEventResponse struct {
	EventID string `json:"event_id"`
}

// NewClientWithRateLimit creates a new Matrix client with custom rate limiting.
func NewClientWithRateLimit(serverURL, asToken, remoteID string, api plugin.API, rateLimitConfig RateLimitConfig) *Client {
	return NewClientWithLoggerAndRateLimit(serverURL, asToken, remoteID, NewAPILogger(api), rateLimitConfig)
}

// NewClientWithLoggerAndRateLimit creates a new Matrix client with custom logger and rate limiting.
func NewClientWithLoggerAndRateLimit(serverURL, asToken, remoteID string, logger Logger, rateLimitConfig RateLimitConfig) *Client {
	client := &Client{
		serverURL: serverURL,
		asToken:   asToken,
		remoteID:  remoteID,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		logger:          logger,
		rateLimitConfig: rateLimitConfig,
	}

	// Initialize rate limiters if enabled
	if rateLimitConfig.Enabled {
		logger.LogInfo("Initializing Matrix client with rate limiting enabled",
			"room_creation_rate", rateLimitConfig.RoomCreation.Rate,
			"room_creation_burst", rateLimitConfig.RoomCreation.BurstSize,
			"message_rate", rateLimitConfig.Messages.Rate,
			"message_burst", rateLimitConfig.Messages.BurstSize)

		client.roomCreationLimiter = NewTokenBucket(rateLimitConfig.RoomCreation)
		client.messageLimiter = NewTokenBucket(rateLimitConfig.Messages)
		client.inviteLimiter = NewTokenBucket(rateLimitConfig.Invites)
		client.registrationLimiter = NewTokenBucket(rateLimitConfig.Registration)
		client.joinLimiter = NewTokenBucket(rateLimitConfig.Joins)
	} else {
		logger.LogWarn("Matrix client initialized with rate limiting DISABLED - may encounter 429 errors")
	}

	return client
}

// SetServerDomain sets an explicit server domain (used for testing)
func (c *Client) SetServerDomain(domain string) {
	c.serverDomain = domain
}

// SetServerURL updates the server URL for the client
func (c *Client) SetServerURL(serverURL string) {
	c.serverURL = serverURL
}

// SendReactionAsGhost sends a reaction to a message as a ghost user
func (c *Client) SendReactionAsGhost(roomID, eventID, emoji, ghostUserID string) (*SendEventResponse, error) {
	if c.asToken == "" {
		return nil, errors.New("application service token not configured")
	}

	// Apply rate limiting for reaction sending
	if err := c.waitForRateLimit(c.messageLimiter, "Reaction sending"); err != nil {
		return nil, err
	}

	// Matrix reaction content structure
	content := map[string]any{
		"m.relates_to": map[string]any{
			"rel_type": "m.annotation",
			"event_id": eventID,
			"key":      emoji,
		},
	}

	return c.sendEventAsUser(roomID, "m.reaction", content, ghostUserID)
}

// RedactEventAsGhost redacts (removes) an event as a ghost user
func (c *Client) RedactEventAsGhost(roomID, eventID, ghostUserID string) (*SendEventResponse, error) {
	if c.asToken == "" {
		return nil, errors.New("application service token not configured")
	}

	// Apply rate limiting for redaction operations
	if err := c.waitForRateLimit(c.messageLimiter, "Event redaction"); err != nil {
		return nil, err
	}

	// Empty content for redaction
	content := map[string]any{}

	txnID := uuid.New().String()
	endpoint, err := BuildSecureURL("/_matrix/client/v3/rooms/", roomID, "redact", eventID, txnID)
	if err != nil {
		return nil, errors.Wrap(err, "invalid room or event ID")
	}
	reqURL := c.serverURL + endpoint

	// Add user_id query parameter for impersonation
	if ghostUserID != "" {
		reqURL += "?user_id=" + url.QueryEscape(ghostUserID)
	}

	jsonData, err := json.Marshal(content)
	if err != nil {
		return nil, errors.Wrap(err, "failed to marshal redaction content")
	}

	req, err := http.NewRequest("PUT", reqURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, errors.Wrap(err, "failed to create redaction request")
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.asToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, errors.Wrap(err, "failed to send redaction request")
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, errors.Wrap(err, "failed to read redaction response body")
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("matrix redaction API error: %d %s", resp.StatusCode, string(body))
	}

	var response SendEventResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal redaction response")
	}

	return &response, nil
}

// GetEvent retrieves a single Matrix event by ID
func (c *Client) GetEvent(roomID, eventID string) (map[string]any, error) {
	if c.asToken == "" {
		return nil, errors.New("application service token not configured")
	}

	endpoint, err := BuildSecureURL("/_matrix/client/v3/rooms/", roomID, "event", eventID)
	if err != nil {
		return nil, errors.Wrap(err, "invalid room or event ID")
	}
	reqURL := c.serverURL + endpoint

	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create get event request")
	}

	req.Header.Set("Authorization", "Bearer "+c.asToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, errors.Wrap(err, "failed to send get event request")
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, errors.Wrap(err, "failed to read get event response")
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to get event: %d %s", resp.StatusCode, string(body))
	}

	var event map[string]any
	if err := json.Unmarshal(body, &event); err != nil {
		return nil, errors.Wrap(err, "failed to parse get event response")
	}

	return event, nil
}

// GetEventRelationsAsUser retrieves events related to a specific event (like reactions) as a specific user
func (c *Client) GetEventRelationsAsUser(roomID, eventID, userID string) ([]map[string]any, error) {
	if c.serverURL == "" || c.asToken == "" {
		return nil, errors.New("matrix client not configured")
	}

	requestURL := c.serverURL + "/_matrix/client/v1/rooms/" + url.PathEscape(roomID) + "/relations/" + url.PathEscape(eventID)

	// Add user_id query parameter for impersonation
	if userID != "" {
		requestURL += "?user_id=" + url.QueryEscape(userID)
	}

	req, err := http.NewRequest("GET", requestURL, nil)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create relations request")
	}

	req.Header.Set("Authorization", "Bearer "+c.asToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, errors.Wrap(err, "failed to send relations request")
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, errors.Wrap(err, "failed to read relations response")
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to get event relations: %d %s", resp.StatusCode, string(body))
	}

	var response struct {
		Chunk []map[string]any `json:"chunk"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal relations response")
	}

	return response.Chunk, nil
}

// TestConnection verifies that the Matrix client can connect to the server.
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
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("matrix connection test failed: %d %s", resp.StatusCode, string(body))
	}

	return nil
}

// JoinRoom joins a Matrix room using either a room ID or room alias.
func (c *Client) JoinRoom(roomIdentifier string) error {
	if c.serverURL == "" || c.asToken == "" {
		return errors.New("matrix client not configured")
	}

	// Apply rate limiting for room join operations (rc_joins)
	if err := c.waitForRateLimit(c.joinLimiter, "Room join"); err != nil {
		return err
	}

	// Use the unified join endpoint that supports both room IDs and aliases
	encodedIdentifier := url.PathEscape(roomIdentifier)
	requestURL := c.serverURL + "/_matrix/client/v3/join/" + encodedIdentifier

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
	defer func() { _ = resp.Body.Close() }()

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

	// Apply rate limiting for room join operations (rc_joins)
	if err := c.waitForRateLimit(c.joinLimiter, "Room join"); err != nil {
		return err
	}

	// Use the unified join endpoint that supports both room IDs and aliases
	encodedIdentifier := url.PathEscape(roomIdentifier)
	requestURL := c.serverURL + "/_matrix/client/v3/join/" + encodedIdentifier

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
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return errors.Wrap(err, "failed to read join response")
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to join room as user: %d %s", resp.StatusCode, string(body))
	}

	return nil
}

// InviteUserToRoom invites a user to a Matrix room
func (c *Client) InviteUserToRoom(roomID, userID string) error {
	if c.serverURL == "" || c.asToken == "" {
		return errors.New("matrix client not configured")
	}

	// Apply rate limiting for room invite operations
	if err := c.waitForRateLimit(c.inviteLimiter, "Room invite"); err != nil {
		return err
	}

	c.logger.LogDebug("Inviting user to Matrix room", "room_id", roomID, "user_id", userID)

	// Prepare invite request body
	inviteData := map[string]string{
		"user_id": userID,
	}

	jsonData, err := json.Marshal(inviteData)
	if err != nil {
		return errors.Wrap(err, "failed to marshal invite data")
	}

	// Use room invite endpoint
	encodedRoomID := url.PathEscape(roomID)
	requestURL := c.serverURL + "/_matrix/client/v3/rooms/" + encodedRoomID + "/invite"

	req, err := http.NewRequest("POST", requestURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return errors.Wrap(err, "failed to create invite request")
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.asToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return errors.Wrap(err, "failed to send invite request")
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return errors.Wrap(err, "failed to read invite response")
	}

	if resp.StatusCode != http.StatusOK {
		// Parse Matrix error response for better error handling
		matrixErr := parseMatrixError(resp.StatusCode, body)

		// Check for common "already joined" error cases to make this idempotent
		if matrixErr.IsAlreadyJoined() {
			c.logger.LogDebug("User already joined or invited to Matrix room, skipping", "room_id", roomID, "user_id", userID, "matrix_error", matrixErr.ErrCode)
			return nil // Not an error - user is already in the room
		}

		c.logger.LogWarn("Failed to invite user to Matrix room", "status_code", resp.StatusCode, "matrix_error", matrixErr.ErrCode, "error_message", matrixErr.ErrMsg, "room_id", roomID, "user_id", userID)
		return matrixErr
	}

	c.logger.LogDebug("Successfully invited user to Matrix room", "room_id", roomID, "user_id", userID)
	return nil
}

// GetRoomJoinRule fetches the join rule for a Matrix room
func (c *Client) GetRoomJoinRule(roomID string) (string, error) {
	if c.serverURL == "" || c.asToken == "" {
		return "", errors.New("matrix client not configured")
	}

	// Use the room state endpoint to get join rules
	encodedRoomID := url.PathEscape(roomID)
	requestURL := c.serverURL + "/_matrix/client/v3/rooms/" + encodedRoomID + "/state/m.room.join_rules/"

	req, err := http.NewRequest("GET", requestURL, nil)
	if err != nil {
		return "", errors.Wrap(err, "failed to create join rules request")
	}

	req.Header.Set("Authorization", "Bearer "+c.asToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", errors.Wrap(err, "failed to send join rules request")
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", errors.Wrap(err, "failed to read join rules response")
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to get join rules: %d %s", resp.StatusCode, string(body))
	}

	var joinRuleEvent struct {
		JoinRule string `json:"join_rule"`
	}
	if err := json.Unmarshal(body, &joinRuleEvent); err != nil {
		return "", errors.Wrap(err, "failed to unmarshal join rules response")
	}

	return joinRuleEvent.JoinRule, nil
}

// InviteAndJoinGhostUser invites a ghost user to a room (via application service) and then joins them
// This checks the room's join rules first to determine if invitation is required
func (c *Client) InviteAndJoinGhostUser(roomIdentifier, ghostUserID string) error {
	if c.serverURL == "" || c.asToken == "" {
		return errors.New("matrix client not configured")
	}

	c.logger.LogDebug("Attempting to join ghost user to Matrix room", "room_identifier", roomIdentifier, "ghost_user_id", ghostUserID)

	// Always resolve room alias to room ID for consistency
	roomID, err := c.ResolveRoomAlias(roomIdentifier)
	if err != nil {
		return errors.Wrap(err, "failed to resolve room identifier")
	}

	c.logger.LogDebug("Resolved room identifier to room ID", "room_identifier", roomIdentifier, "room_id", roomID, "ghost_user_id", ghostUserID)

	// Check the room's join rules first
	joinRule, err := c.GetRoomJoinRule(roomID)
	if err != nil {
		c.logger.LogWarn("Failed to get room join rules, falling back to try-join approach", "room_id", roomID, "error", err)
		// Fall back to the old approach if we can't determine join rules
		return c.joinGhostUserWithFallback(roomID, ghostUserID)
	}

	c.logger.LogDebug("Determined room join rule", "room_id", roomID, "join_rule", joinRule)

	// Handle based on join rule
	switch joinRule {
	case "public":
		// Public room - can join directly
		err := c.JoinRoomAsUser(roomID, ghostUserID)
		if err != nil {
			return errors.Wrap(err, "failed to join ghost user to public room")
		}
		c.logger.LogDebug("Successfully joined ghost user to public room", "room_id", roomID, "ghost_user_id", ghostUserID)
		return nil

	case "invite":
		// Private/invite-only room - need to invite first
		c.logger.LogDebug("Room requires invitation, inviting ghost user first", "room_id", roomID, "ghost_user_id", ghostUserID)

		// Invite the ghost user to the room using application service bot
		inviteErr := c.InviteUserToRoom(roomID, ghostUserID)
		if inviteErr != nil {
			return errors.Wrap(inviteErr, "failed to invite ghost user to private room")
		}

		// Now join after invitation
		joinErr := c.JoinRoomAsUser(roomID, ghostUserID)
		if joinErr != nil {
			return errors.Wrap(joinErr, "failed to join ghost user to room after invitation")
		}

		c.logger.LogDebug("Successfully invited and joined ghost user to private room", "room_id", roomID, "ghost_user_id", ghostUserID)
		return nil

	default:
		// Unknown or unsupported join rule (e.g., "knock", "restricted")
		c.logger.LogWarn("Unknown join rule, falling back to try-join approach", "room_id", roomID, "join_rule", joinRule)
		return c.joinGhostUserWithFallback(roomID, ghostUserID)
	}
}

// joinGhostUserWithFallback is the fallback method that tries join first, then invite+join
func (c *Client) joinGhostUserWithFallback(roomID, ghostUserID string) error {
	// First try to join directly
	err := c.JoinRoomAsUser(roomID, ghostUserID)
	if err == nil {
		c.logger.LogDebug("Successfully joined ghost user to room directly", "room_id", roomID, "ghost_user_id", ghostUserID)
		return nil
	}

	// If join failed with a forbidden error, try invitation first
	if isForbiddenJoinError(err) {
		var matrixErr *Error
		if errors.As(err, &matrixErr) {
			c.logger.LogDebug("Direct join failed with forbidden error, attempting invitation first", "room_id", roomID, "ghost_user_id", ghostUserID, "matrix_error", matrixErr.ErrCode)
		} else {
			// Fallback for legacy error types that aren't structured
			c.logger.LogDebug("Direct join failed with forbidden error (legacy error), attempting invitation first", "room_id", roomID, "ghost_user_id", ghostUserID, "error", err.Error())
		}
	} else {
		// For other errors, return the original join error
		return errors.Wrap(err, "failed to join ghost user to room")
	}

	// Common invitation and join logic for both structured and legacy errors
	// Invite the ghost user to the room using application service bot
	inviteErr := c.InviteUserToRoom(roomID, ghostUserID)
	if inviteErr != nil {
		return errors.Wrap(inviteErr, "failed to invite ghost user to private room")
	}

	// Now try to join again after invitation
	joinErr := c.JoinRoomAsUser(roomID, ghostUserID)
	if joinErr != nil {
		return errors.Wrap(joinErr, "failed to join ghost user to room after invitation")
	}

	c.logger.LogDebug("Successfully invited and joined ghost user to private room via fallback", "room_id", roomID, "ghost_user_id", ghostUserID)
	return nil
}

// CreateRoom creates a new Matrix room with the specified name, topic, and settings.
// Returns the room ID or alias on success.
func (c *Client) CreateRoom(name, topic, serverDomain string, publish bool, mattermostChannelID string) (string, error) {
	if c.serverURL == "" || c.asToken == "" {
		return "", errors.New("matrix client not configured")
	}

	// Apply rate limiting for room creation
	if err := c.waitForRateLimit(c.roomCreationLimiter, "Room creation"); err != nil {
		return "", err
	}

	c.logger.LogDebug("Creating Matrix room", "name", name, "topic", topic, "server_domain", serverDomain)

	// Create room alias using reserved Application Service namespace
	alias := strings.ToLower(strings.ReplaceAll(name, " ", "-"))
	alias = strings.ReplaceAll(alias, "_", "-")
	// Use _mattermost_ prefix for namespace reservation
	roomAlias := ""
	if serverDomain != "" {
		roomAlias = "#_mattermost_" + alias + ":" + serverDomain
	}

	// Set room visibility and rules based on publish parameter
	var preset, visibility, joinRule, historyVisibility, guestAccess string
	if publish {
		preset = "public_chat"
		visibility = "public"
		joinRule = "public"
		historyVisibility = "world_readable"
		guestAccess = "can_join"
	} else {
		preset = "private_chat"
		visibility = "private"
		joinRule = "invite"
		historyVisibility = "invited"
		guestAccess = "forbidden"
	}

	roomData := map[string]any{
		"name":         name,
		"topic":        topic,
		"preset":       preset,
		"visibility":   visibility,
		"is_direct":    false, // Explicitly mark as not a direct message room
		"room_version": "10",  // Explicitly set room version and directory visibility
		"initial_state": []map[string]any{
			{
				"type":      "m.room.guest_access",
				"state_key": "",
				"content": map[string]any{
					"guest_access": guestAccess,
				},
			},
			{
				"type":      "m.room.history_visibility",
				"state_key": "",
				"content": map[string]any{
					"history_visibility": historyVisibility,
				},
			},
			{
				"type":      "m.room.join_rules",
				"state_key": "",
				"content": map[string]any{
					"join_rule": joinRule,
				},
			},
			{
				"type":      "com.mattermost.bridge.channel",
				"state_key": "",
				"content": map[string]any{
					"mattermost_channel_id": mattermostChannelID,
					"created_at":            time.Now().Unix(),
				},
			},
		},
		"creation_content": map[string]any{
			"m.federate": true,
		},
	}

	// Add room alias if we have a server domain
	if roomAlias != "" {
		roomData["room_alias_name"] = "_mattermost_" + alias // Include namespace prefix in local part
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
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", errors.Wrap(err, "failed to read room creation response")
	}

	if resp.StatusCode != http.StatusOK {
		matrixErr := parseMatrixError(resp.StatusCode, body)

		// Handle 429 Too Many Requests with specific logging
		if IsRateLimitError(matrixErr) {
			c.logger.LogWarn("Matrix room creation rate limited, server rejected request",
				"status_code", resp.StatusCode,
				"response", string(body),
				"room_name", name,
				"error_code", matrixErr.ErrCode)
			return "", matrixErr
		}

		c.logger.LogError("Matrix room creation failed", "status_code", resp.StatusCode, "response", string(body), "room_name", name)
		return "", matrixErr
	}

	var response struct {
		RoomID string `json:"room_id"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return "", errors.Wrap(err, "failed to unmarshal room creation response")
	}

	// Join the application service bot to the room immediately after creation
	// This is required for the bot to query room state, invite users, etc.
	if err := c.JoinRoom(response.RoomID); err != nil {
		c.logger.LogWarn("Failed to join application service bot to created room", "room_id", response.RoomID, "error", err)
		// Continue - the room was created successfully, but bot operations may fail
	} else {
		c.logger.LogDebug("Application service bot joined room", "room_id", response.RoomID)
	}

	// Publish to directory based on the publish parameter
	if publish {
		c.logger.LogDebug("Publishing room to public directory", "room_id", response.RoomID)
		if err := c.PublishRoomToDirectory(response.RoomID, true); err != nil {
			// Log warning but don't fail room creation - the room was created successfully
			c.logger.LogWarn("Failed to publish room to public directory", "room_id", response.RoomID, "error", err)
			c.logger.LogDebug("Room created but not published to directory", "room_id", response.RoomID, "room_alias", roomAlias)
		} else {
			c.logger.LogDebug("Room created and published to directory", "room_id", response.RoomID, "room_alias", roomAlias)
		}
	} else {
		c.logger.LogDebug("Room created (not published to directory)", "room_id", response.RoomID, "room_alias", roomAlias)
	}

	// Log successful room creation
	returnValue := response.RoomID
	if roomAlias != "" {
		returnValue = roomAlias
	}
	c.logger.LogInfo("Matrix room created successfully", "room_id", response.RoomID, "room_alias", roomAlias, "return_value", returnValue)

	// Add bridge alias for Matrix Application Service filtering
	if roomAlias != "" {
		// Create bridge alias with mattermost-bridge- prefix
		bridgeAlias := "#mattermost-bridge-" + alias + ":" + serverDomain
		err = c.AddRoomAlias(response.RoomID, bridgeAlias)
		if err != nil {
			c.logger.LogWarn("Failed to add bridge filtering alias", "error", err, "bridge_alias", bridgeAlias, "room_id", response.RoomID)
			// Continue - user alias still works, bridge filtering just won't work for this room
		} else {
			c.logger.LogDebug("Successfully added bridge filtering alias", "room_id", response.RoomID, "bridge_alias", bridgeAlias, "user_alias", roomAlias)
		}
	}

	// Return the room alias if we created one, otherwise return the room ID
	if roomAlias != "" {
		return roomAlias, nil
	}
	return response.RoomID, nil
}

// CreateDirectRoom creates a Matrix DM room and invites the specified ghost users
func (c *Client) CreateDirectRoom(ghostUserIDs []string, roomName string) (string, error) {
	if c.serverURL == "" || c.asToken == "" {
		return "", errors.New("matrix client not configured")
	}

	if len(ghostUserIDs) < 2 {
		return "", errors.New("direct room requires at least 2 users")
	}

	// Apply rate limiting for room creation operations
	if err := c.waitForRateLimit(c.roomCreationLimiter, "Direct room creation"); err != nil {
		return "", err
	}

	c.logger.LogDebug("Creating Matrix DM room", "users", ghostUserIDs)

	roomData := map[string]any{
		"preset":    "private_chat",
		"is_direct": true,
		"invite":    ghostUserIDs,
		"creation_content": map[string]any{
			"m.federate": true,
		},
		"initial_state": []map[string]any{
			{
				"type":      "m.room.history_visibility",
				"state_key": "",
				"content": map[string]any{
					"history_visibility": "shared",
				},
			},
		},
	}

	// Set room name for better identification
	if roomName != "" {
		roomData["name"] = roomName
	}

	// For group DMs (more than 2 users), adjust settings
	if len(ghostUserIDs) > 2 {
		if roomName == "" {
			roomData["name"] = "Group Chat"
		}
		roomData["preset"] = "private_chat"
		// Group DMs are not considered "direct" in Matrix spec
		roomData["is_direct"] = false
	}

	jsonData, err := json.Marshal(roomData)
	if err != nil {
		return "", errors.Wrap(err, "failed to marshal DM room creation data")
	}

	url := c.serverURL + "/_matrix/client/v3/createRoom"

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return "", errors.Wrap(err, "failed to create DM room creation request")
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.asToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", errors.Wrap(err, "failed to send DM room creation request")
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", errors.Wrap(err, "failed to read DM room creation response")
	}

	if resp.StatusCode != http.StatusOK {
		c.logger.LogError("Matrix DM room creation failed", "status_code", resp.StatusCode, "response", string(body), "users", ghostUserIDs)
		return "", fmt.Errorf("failed to create DM room: %d %s", resp.StatusCode, string(body))
	}

	var response struct {
		RoomID string `json:"room_id"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return "", errors.Wrap(err, "failed to unmarshal DM room creation response")
	}

	c.logger.LogInfo("Matrix DM room created successfully", "room_id", response.RoomID, "users", ghostUserIDs)
	return response.RoomID, nil
}

// extractServerDomain extracts the hostname from the Matrix server URL
func (c *Client) extractServerDomain() (string, error) {
	// Use explicit server domain if set (for testing)
	if c.serverDomain != "" {
		return c.serverDomain, nil
	}

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

// AddRoomAlias adds an additional alias to an existing Matrix room
func (c *Client) AddRoomAlias(roomID, alias string) error {
	if c.serverURL == "" || c.asToken == "" {
		return errors.New("matrix client not configured")
	}

	c.logger.LogDebug("Adding room alias", "room_id", roomID, "alias", alias)

	requestBody := map[string]string{
		"room_id": roomID,
	}

	jsonData, err := json.Marshal(requestBody)
	if err != nil {
		return errors.Wrap(err, "failed to marshal alias request")
	}

	// URL encode the alias to handle special characters
	encodedAlias := url.PathEscape(alias)
	requestURL := c.serverURL + "/_matrix/client/v3/directory/room/" + encodedAlias

	req, err := http.NewRequest("PUT", requestURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return errors.Wrap(err, "failed to create alias request")
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.asToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return errors.Wrap(err, "failed to send alias request")
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return errors.Wrap(err, "failed to read alias response")
	}

	if resp.StatusCode != http.StatusOK {
		c.logger.LogError("Failed to add room alias", "status_code", resp.StatusCode, "response", string(body), "alias", alias, "room_id", roomID)
		return fmt.Errorf("failed to add room alias: %d %s", resp.StatusCode, string(body))
	}

	c.logger.LogDebug("Successfully added room alias", "room_id", roomID, "alias", alias)
	return nil
}

// Ghost user management functions

// GhostUser represents a Matrix user created by the application service to represent a Mattermost user.
type GhostUser struct {
	UserID   string `json:"user_id"`
	Username string `json:"username"`
}

// CreateGhostUser creates a ghost user for a Mattermost user with display name and avatar data
func (c *Client) CreateGhostUser(mattermostUserID, displayName string, avatarData []byte, avatarContentType string) (*GhostUser, error) {
	if c.asToken == "" {
		return nil, errors.New("application service token not configured")
	}

	// Apply rate limiting for user creation operations
	if err := c.waitForRateLimit(c.inviteLimiter, "Ghost user creation"); err != nil {
		return nil, err
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
	regData := map[string]any{
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
	defer func() { _ = resp.Body.Close() }()

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
		if err := json.Unmarshal(body, &errorResp); err != nil || errorResp.Errcode != "M_USER_IN_USE" {
			return nil, fmt.Errorf("failed to create ghost user: %d %s", resp.StatusCode, string(body))
		}
		// User already exists, that's fine - continue with profile setup
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

	// Apply rate limiting for profile operations
	if err := c.waitForRateLimit(c.inviteLimiter, "Display name setting"); err != nil {
		return err
	}

	// Content for the display name event
	content := map[string]any{
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
	defer func() { _ = resp.Body.Close() }()

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

	// Apply rate limiting for profile operations
	if err := c.waitForRateLimit(c.inviteLimiter, "Avatar URL setting"); err != nil {
		return err
	}

	// Content for the avatar URL event
	content := map[string]any{
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
	defer func() { _ = resp.Body.Close() }()

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

	// Apply rate limiting for media upload operations
	if err := c.waitForRateLimit(c.messageLimiter, "Media upload"); err != nil {
		return "", err
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
	defer func() { _ = resp.Body.Close() }()

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

// EditMessageAsGhost edits an existing message as a ghost user, with optional HTML formatting
func (c *Client) EditMessageAsGhost(roomID, eventID, newMessage, htmlMessage, ghostUserID string) (*SendEventResponse, error) {
	if c.asToken == "" {
		return nil, errors.New("application service token not configured")
	}

	// Apply rate limiting for message edit operations
	if err := c.waitForRateLimit(c.messageLimiter, "Message edit"); err != nil {
		return nil, err
	}

	// Matrix edit event content structure
	newContent := map[string]any{
		"msgtype": "m.text",
		"body":    newMessage,
	}

	// Add HTML formatting to new content if provided
	if htmlMessage != "" {
		newContent["format"] = "org.matrix.custom.html"
		newContent["formatted_body"] = htmlMessage
	}

	content := map[string]any{
		"msgtype":       "m.text",
		"body":          " * " + newMessage, // Fallback for clients that don't support edits
		"m.new_content": newContent,
		"m.relates_to": map[string]any{
			"rel_type": "m.replace",
			"event_id": eventID,
		},
	}

	return c.sendEventAsUser(roomID, "m.room.message", content, ghostUserID)
}

// SendMessage sends a message as a ghost user with all optional parameters consolidated into a single request.
func (c *Client) SendMessage(req MessageRequest) (*SendEventResponse, error) {
	if c.asToken == "" {
		return nil, errors.New("application service token not configured")
	}

	// Apply rate limiting for message sending
	if err := c.waitForRateLimit(c.messageLimiter, "Message sending"); err != nil {
		return nil, err
	}

	// Validate required fields
	if req.RoomID == "" {
		return nil, errors.New("room_id is required")
	}
	if req.GhostUserID == "" {
		return nil, errors.New("ghost_user_id is required")
	}

	c.logger.LogDebug("Sending message as ghost user", "room_id", req.RoomID, "ghost_user_id", req.GhostUserID, "file_count", len(req.Files), "has_text", req.Message != "" || req.HTMLMessage != "")

	// Simplified logic: send text (if any) and files (if any) as separate top-level messages
	// All messages from one Mattermost post will be linked via m.relates_to

	if req.Message == "" && req.HTMLMessage == "" && len(req.Files) == 0 {
		return nil, errors.New("no message content or files to send")
	}

	return c.sendMattermostPost(req)
}

// sendMattermostPost sends all content from a Mattermost post as separate Matrix messages
// Text (if any) and each file become separate top-level messages, linked via m.relates_to
func (c *Client) sendMattermostPost(req MessageRequest) (*SendEventResponse, error) {
	var primaryResponse *SendEventResponse
	var rootEventID string

	// Send text message first if present
	if req.Message != "" || req.HTMLMessage != "" {
		textResponse, err := c.sendTextMessage(req, "")
		if err != nil {
			return nil, errors.Wrap(err, "failed to send text message")
		}
		primaryResponse = textResponse
		rootEventID = textResponse.EventID
		c.logger.LogDebug("Sent text message", "event_id", rootEventID)
	}

	// Send each file as separate top-level message
	for _, file := range req.Files {
		fileResponse, err := c.sendFileMessage(req, file, rootEventID)
		if err != nil {
			// Log error but continue with other files
			c.logger.LogWarn("Failed to send file message", "filename", file.Filename, "error", err)
			continue
		}

		// If this is the first message (no text), use it as primary and root
		if primaryResponse == nil {
			primaryResponse = fileResponse
			rootEventID = fileResponse.EventID
			c.logger.LogDebug("Sent first file message as root", "event_id", rootEventID, "filename", file.Filename)
		} else {
			c.logger.LogDebug("Sent file message linked to root", "event_id", fileResponse.EventID, "filename", file.Filename, "root_event_id", rootEventID)
		}
	}

	if primaryResponse == nil {
		return nil, errors.New("failed to send any content")
	}

	c.logger.LogDebug("Successfully sent Mattermost post", "primary_event_id", primaryResponse.EventID, "text_present", req.Message != "" || req.HTMLMessage != "", "file_count", len(req.Files))
	return primaryResponse, nil
}

// sendTextMessage sends a text message with optional relation to root event
func (c *Client) sendTextMessage(req MessageRequest, rootEventID string) (*SendEventResponse, error) {
	content := make(map[string]any)

	// Text message content
	content["msgtype"] = "m.text"
	content["body"] = req.Message

	// Add HTML formatting if provided
	if req.HTMLMessage != "" {
		content["format"] = "org.matrix.custom.html"
		content["formatted_body"] = req.HTMLMessage
	}

	// Add mentions if provided
	if req.Mentions != nil {
		content["m.mentions"] = req.Mentions
	}

	// Add threading if provided (takes priority over post grouping)
	if req.ThreadEventID != "" {
		content["m.relates_to"] = map[string]any{
			"rel_type": "m.thread",
			"event_id": req.ThreadEventID,
		}
	} else if rootEventID != "" {
		// Add relation to root event if no threading (for post grouping)
		content["m.relates_to"] = map[string]any{
			"rel_type": "m.mattermost.post",
			"event_id": rootEventID,
		}
	}

	// Add Mattermost metadata - ALWAYS include for deletion tracking
	if req.PostID != "" {
		content["mattermost_post_id"] = req.PostID
	}
	if c.remoteID != "" {
		content["mattermost_remote_id"] = c.remoteID
	}

	return c.sendEventAsUser(req.RoomID, "m.room.message", content, req.GhostUserID)
}

// sendFileMessage sends a file message with optional relation to root event
func (c *Client) sendFileMessage(req MessageRequest, file FileAttachment, rootEventID string) (*SendEventResponse, error) {
	content := make(map[string]any)

	// Determine message type based on MIME type
	switch {
	case strings.HasPrefix(file.MimeType, "image/"):
		content["msgtype"] = "m.image"
	case strings.HasPrefix(file.MimeType, "video/"):
		content["msgtype"] = "m.video"
	case strings.HasPrefix(file.MimeType, "audio/"):
		content["msgtype"] = "m.audio"
	default:
		content["msgtype"] = "m.file"
	}

	// File content
	content["body"] = file.Filename
	content["url"] = file.MxcURI
	content["info"] = map[string]any{
		"size":     file.Size,
		"mimetype": file.MimeType,
	}

	// Add threading if provided (takes priority over post grouping)
	if req.ThreadEventID != "" {
		content["m.relates_to"] = map[string]any{
			"rel_type": "m.thread",
			"event_id": req.ThreadEventID,
		}
	} else if rootEventID != "" {
		// Add relation to root event if no threading (for post grouping)
		content["m.relates_to"] = map[string]any{
			"rel_type": "m.mattermost.post",
			"event_id": rootEventID,
		}
	}

	// Add Mattermost metadata - ALWAYS include for deletion tracking
	if req.PostID != "" {
		content["mattermost_post_id"] = req.PostID
	}
	if c.remoteID != "" {
		content["mattermost_remote_id"] = c.remoteID
	}

	return c.sendEventAsUser(req.RoomID, "m.room.message", content, req.GhostUserID)
}

// sendEventAsUser sends an event as a specific user (using application service impersonation)
func (c *Client) sendEventAsUser(roomID, eventType string, content any, userID string) (*SendEventResponse, error) {
	txnID := uuid.New().String()
	endpoint, err := BuildSecureURL("/_matrix/client/v3/rooms/", roomID, "send", eventType, txnID)
	if err != nil {
		return nil, errors.Wrap(err, "invalid room ID or event type")
	}
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
	defer func() { _ = resp.Body.Close() }()

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

// ResolveRoomAlias resolves a Matrix room alias to its room ID.
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
	defer func() { _ = resp.Body.Close() }()

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

// UserProfile represents a Matrix user's profile information
type UserProfile struct {
	DisplayName string `json:"displayname,omitempty"`
	AvatarURL   string `json:"avatar_url,omitempty"`
}

// GetUserProfile retrieves the profile information for a Matrix user
func (c *Client) GetUserProfile(userID string) (*UserProfile, error) {
	if c.serverURL == "" || c.asToken == "" {
		return nil, errors.New("matrix client not configured")
	}

	// URL encode the user ID to handle special characters
	encodedUserID := url.PathEscape(userID)
	requestURL := c.serverURL + "/_matrix/client/v3/profile/" + encodedUserID

	req, err := http.NewRequest("GET", requestURL, nil)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create profile request")
	}

	req.Header.Set("Authorization", "Bearer "+c.asToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, errors.Wrap(err, "failed to send profile request")
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, errors.Wrap(err, "failed to read profile response")
	}

	if resp.StatusCode != http.StatusOK {
		c.logger.LogWarn("Failed to get Matrix user profile", "status_code", resp.StatusCode, "response", string(body), "user_id", userID)
		// Return empty profile rather than error - user might not have set a display name
		return &UserProfile{}, nil
	}

	var profile UserProfile
	if err := json.Unmarshal(body, &profile); err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal profile response")
	}

	c.logger.LogDebug("Successfully retrieved Matrix user profile", "user_id", userID, "display_name", profile.DisplayName)
	return &profile, nil
}

// DownloadFile downloads file data from a Matrix MXC URI with configurable size limit
func (c *Client) DownloadFile(mxcURI string, maxSize int64, contentTypePrefix string) ([]byte, error) {
	if mxcURI == "" {
		return nil, errors.New("MXC URI is empty")
	}

	// Matrix file URIs are in the format mxc://server/media_id
	if !strings.HasPrefix(mxcURI, "mxc://") {
		return nil, errors.New("invalid Matrix MXC URI format")
	}

	// Extract server and media ID from mxc://server/media_id
	mxcParts := strings.TrimPrefix(mxcURI, "mxc://")
	parts := strings.SplitN(mxcParts, "/", 2)
	if len(parts) != 2 {
		return nil, errors.New("invalid Matrix MXC URI format")
	}

	serverName := parts[0]
	mediaID := parts[1]

	// Validate MXC URI components for path traversal attacks
	if err := ValidateMXCComponents(serverName, mediaID); err != nil {
		return nil, errors.Wrap(err, "invalid MXC URI components")
	}

	// Build secure media download URLs with proper escaping
	var downloadURLs []string
	var validationErrors []error

	// NEW: Try client API endpoints first (newer Synapse versions use these)
	url1, err := BuildSecureURL(c.serverURL+"/_matrix/client/v1/media/download/", serverName, mediaID)
	if err == nil {
		downloadURLs = append(downloadURLs, url1)
	} else {
		validationErrors = append(validationErrors, errors.Wrap(err, "client v1 with server"))
	}

	url2, err := BuildSecureURL(c.serverURL+"/_matrix/client/v1/media/download/", mediaID)
	if err == nil {
		downloadURLs = append(downloadURLs, url2)
	} else {
		validationErrors = append(validationErrors, errors.Wrap(err, "client v1 media-only"))
	}

	// Standard Matrix media repository API v3
	url3, err := BuildSecureURL(c.serverURL+"/_matrix/media/v3/download/", serverName, mediaID)
	if err == nil {
		downloadURLs = append(downloadURLs, url3)
	} else {
		validationErrors = append(validationErrors, errors.Wrap(err, "media v3 with server"))
	}

	// Fallback to v1 API (some older servers)
	url4, err := BuildSecureURL(c.serverURL+"/_matrix/media/v1/download/", serverName, mediaID)
	if err == nil {
		downloadURLs = append(downloadURLs, url4)
	} else {
		validationErrors = append(validationErrors, errors.Wrap(err, "media v1 with server"))
	}

	// Alternative endpoint without server name (some configurations)
	url5, err := BuildSecureURL(c.serverURL+"/_matrix/media/v3/download/", mediaID)
	if err == nil {
		downloadURLs = append(downloadURLs, url5)
	} else {
		validationErrors = append(validationErrors, errors.Wrap(err, "media v3 media-only"))
	}

	url6, err := BuildSecureURL(c.serverURL+"/_matrix/media/v1/download/", mediaID)
	if err == nil {
		downloadURLs = append(downloadURLs, url6)
	} else {
		validationErrors = append(validationErrors, errors.Wrap(err, "media v1 media-only"))
	}

	if len(downloadURLs) == 0 {
		// Collect all validation error messages for debugging
		var errorMsgs []string
		for _, validationErr := range validationErrors {
			errorMsgs = append(errorMsgs, validationErr.Error())
		}
		return nil, errors.Errorf("failed to construct any valid download URLs for MXC URI %s - validation errors: %s",
			mxcURI, strings.Join(errorMsgs, "; "))
	}

	var lastErr error
	for i, downloadURL := range downloadURLs {
		c.logger.LogDebug("Attempting to download Matrix file", "url", downloadURL, "attempt", i+1, "mxc_uri", mxcURI)

		req, err := http.NewRequest("GET", downloadURL, nil)
		if err != nil {
			c.logger.LogWarn("Failed to create file download request", "error", err, "url", downloadURL)
			lastErr = err
			continue
		}

		// Add authorization header for authenticated download
		req.Header.Set("Authorization", "Bearer "+c.asToken)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			c.logger.LogWarn("Failed to download file from URL", "error", err, "url", downloadURL)
			lastErr = err
			continue
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			c.logger.LogWarn("Matrix media endpoint returned error", "url", downloadURL, "status", resp.StatusCode)
			lastErr = fmt.Errorf("HTTP %d from %s", resp.StatusCode, downloadURL)
			continue
		}

		// Check content type if specified
		contentType := resp.Header.Get("Content-Type")
		if contentTypePrefix != "" && !strings.HasPrefix(contentType, contentTypePrefix) {
			c.logger.LogWarn("Invalid content type", "content_type", contentType, "expected_prefix", contentTypePrefix, "url", downloadURL)
			lastErr = fmt.Errorf("invalid content type: %s (expected prefix: %s)", contentType, contentTypePrefix)
			continue
		}

		// Read the file data
		fileData, err := io.ReadAll(resp.Body)
		if err != nil {
			c.logger.LogWarn("Failed to read file data", "error", err, "url", downloadURL)
			lastErr = err
			continue
		}

		// Check size limit
		if maxSize > 0 && int64(len(fileData)) > maxSize {
			c.logger.LogWarn("File too large", "size", len(fileData), "max", maxSize, "url", downloadURL)
			lastErr = fmt.Errorf("file too large: %d bytes (max %d)", len(fileData), maxSize)
			continue
		}

		c.logger.LogDebug("Successfully downloaded Matrix file", "url", downloadURL, "size", len(fileData), "content_type", contentType, "mxc_uri", mxcURI)
		return fileData, nil
	}

	// If we get here, all attempts failed
	return nil, errors.Wrapf(lastErr, "failed to download file from any endpoint for MXC URI: %s", mxcURI)
}

// PublishRoomToDirectory explicitly publishes a room to the public directory
func (c *Client) PublishRoomToDirectory(roomID string, publish bool) error {
	if c.asToken == "" {
		return errors.New("application service token not configured")
	}

	requestURL := c.serverURL + "/_matrix/client/v3/directory/list/room/" + url.PathEscape(roomID)

	content := map[string]any{
		"visibility": "public",
	}
	if !publish {
		content["visibility"] = "private"
	}

	jsonData, err := json.Marshal(content)
	if err != nil {
		return errors.Wrap(err, "failed to marshal directory visibility content")
	}

	req, err := http.NewRequest("PUT", requestURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return errors.Wrap(err, "failed to create directory visibility request")
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.asToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return errors.Wrap(err, "failed to send directory visibility request")
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return errors.Wrap(err, "failed to read directory visibility response")
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to set directory visibility: %d %s", resp.StatusCode, string(body))
	}

	return nil
}

// AddFileMetadataToMessage adds custom metadata to a message containing file attachment event IDs
func (c *Client) AddFileMetadataToMessage(roomID, messageEventID string, fileEventIDs []string, ghostUserID string) error {
	if c.asToken == "" {
		return errors.New("application service token not configured")
	}

	// Create a custom event to store the file attachment metadata
	// We'll use a custom event type that won't be displayed by Matrix clients
	content := map[string]any{
		"file_attachments":   fileEventIDs,
		"relates_to_message": messageEventID,
		// Add proper Matrix relation so it's returned by the relations API
		"m.relates_to": map[string]any{
			"rel_type": "m.mattermost.file_metadata",
			"event_id": messageEventID,
		},
	}

	// Send as a custom event type that Matrix clients will ignore
	err := c.sendCustomEventAsUser(roomID, "m.mattermost.file_metadata", content, ghostUserID)
	if err != nil {
		return errors.Wrapf(err, "failed to send file metadata event for message %s with %d files", messageEventID, len(fileEventIDs))
	}

	// Log successful metadata creation
	c.logger.LogDebug("Successfully created file metadata event", "message_event_id", messageEventID, "file_count", len(fileEventIDs), "file_event_ids", fileEventIDs)
	return nil
}

// sendCustomEventAsUser sends a custom event type as a specific user
func (c *Client) sendCustomEventAsUser(roomID, eventType string, content any, userID string) error {
	txnID := uuid.New().String()
	endpoint, err := BuildSecureURL("/_matrix/client/v3/rooms/", roomID, "send", eventType, txnID)
	if err != nil {
		return errors.Wrap(err, "invalid room ID or event type")
	}
	reqURL := c.serverURL + endpoint

	// Add user_id query parameter for impersonation
	if userID != "" {
		reqURL += "?user_id=" + url.QueryEscape(userID)
	}

	jsonData, err := json.Marshal(content)
	if err != nil {
		return errors.Wrap(err, "failed to marshal event content")
	}

	req, err := http.NewRequest("PUT", reqURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return errors.Wrap(err, "failed to create request")
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.asToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return errors.Wrap(err, "failed to send request")
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return errors.Wrap(err, "failed to read response body")
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("matrix API error: %d %s", resp.StatusCode, string(body))
	}

	return nil
}

// ServerVersionResponse represents the response from the Matrix server version endpoint
type ServerVersionResponse struct {
	Server map[string]any `json:"server,omitempty"`
}

// ServerInfo contains server name and version information
type ServerInfo struct {
	Name    string
	Version string
}

// GetServerVersion retrieves the Matrix server version information
func (c *Client) GetServerVersion() (string, error) {
	if c.serverURL == "" {
		return "", errors.New("matrix server URL not configured")
	}

	requestURL := c.serverURL + "/_matrix/federation/v1/version"

	req, err := http.NewRequest("GET", requestURL, nil)
	if err != nil {
		return "", errors.Wrap(err, "failed to create version request")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", errors.Wrap(err, "failed to send version request")
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", errors.Wrap(err, "failed to read version response")
	}

	if resp.StatusCode != http.StatusOK {
		// Fall back to trying the client API endpoint if federation API is not available
		return c.getServerVersionFromClient()
	}

	var versionResp ServerVersionResponse
	if err := json.Unmarshal(body, &versionResp); err != nil {
		return "", errors.Wrap(err, "failed to unmarshal version response")
	}

	// Extract version information from the server field
	if versionResp.Server != nil {
		if version, exists := versionResp.Server["version"].(string); exists && version != "" {
			return version, nil
		}
		if name, exists := versionResp.Server["name"].(string); exists && name != "" {
			return name, nil
		}
	}

	return "Unknown", nil
}

// getServerVersionFromClient tries to get version info from client API endpoints
func (c *Client) getServerVersionFromClient() (string, error) {
	// Try the client versions endpoint
	requestURL := c.serverURL + "/_matrix/client/versions"

	req, err := http.NewRequest("GET", requestURL, nil)
	if err != nil {
		return "", errors.Wrap(err, "failed to create client versions request")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", errors.Wrap(err, "failed to send client versions request")
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusOK {
		return "Matrix Server (version info not available)", nil
	}

	return "", errors.New("unable to determine server version")
}

// TestApplicationServicePermissions tests AS permissions without making invasive changes
func (c *Client) TestApplicationServicePermissions() error {
	if c.asToken == "" {
		return errors.New("application service token not configured")
	}

	// Test 1: Try to query a user that should be in our namespace but doesn't exist
	// This tests if we have permission to query users in our namespace
	testUserID := "@_mattermost_nonexistent_test_user:" + c.extractServerDomainForTest()

	requestURL := c.serverURL + "/_matrix/client/v3/profile/" + url.PathEscape(testUserID)

	req, err := http.NewRequest("GET", requestURL, nil)
	if err != nil {
		return errors.Wrap(err, "failed to create AS permission test request")
	}

	req.Header.Set("Authorization", "Bearer "+c.asToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return errors.Wrap(err, "failed to send AS permission test request")
	}
	defer func() { _ = resp.Body.Close() }()

	// We expect either:
	// - 404: User doesn't exist (good, we have permission to query our namespace)
	// - 403: Forbidden (bad, AS not properly configured)
	// - 401: Unauthorized (bad, token invalid)

	if resp.StatusCode == http.StatusNotFound {
		// This is expected - user doesn't exist but we have permission to query
		return nil
	}

	if resp.StatusCode == http.StatusForbidden {
		return errors.New("application service lacks permission to query users in its namespace")
	}

	if resp.StatusCode == http.StatusUnauthorized {
		return errors.New("application service token is invalid or not recognized")
	}

	// Any other status code is also acceptable (user might exist, etc.)
	return nil
}

// extractServerDomainForTest extracts domain for testing purposes
func (c *Client) extractServerDomainForTest() string {
	if c.serverURL == "" {
		return "example.com"
	}

	// Use the existing extractServerDomain method
	domain, err := c.extractServerDomain()
	if err != nil {
		return "example.com"
	}

	return domain
}

// GetServerInfo retrieves both server name and version information
func (c *Client) GetServerInfo() (*ServerInfo, error) {
	if c.serverURL == "" {
		return nil, errors.New("matrix server URL not configured")
	}

	requestURL := c.serverURL + "/_matrix/federation/v1/version"

	req, err := http.NewRequest("GET", requestURL, nil)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create version request")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, errors.Wrap(err, "failed to send version request")
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, errors.Wrap(err, "failed to read version response")
	}

	if resp.StatusCode != http.StatusOK {
		// Fall back to basic info if federation API is not available
		return &ServerInfo{
			Name:    "Matrix Server",
			Version: "Unknown",
		}, nil
	}

	var versionResp ServerVersionResponse
	if err := json.Unmarshal(body, &versionResp); err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal version response")
	}

	serverInfo := &ServerInfo{
		Name:    "Matrix Server",
		Version: "Unknown",
	}

	// Extract server information from the response
	if versionResp.Server != nil {
		if name, exists := versionResp.Server["name"].(string); exists && name != "" {
			serverInfo.Name = name
		}
		if version, exists := versionResp.Server["version"].(string); exists && version != "" {
			serverInfo.Version = version
		}
	}

	return serverInfo, nil
}

// RegisterUser creates a regular Matrix user (not a ghost user) with rate limiting
func (c *Client) RegisterUser(username, password string) (*RegisterResponse, error) {
	// Apply rate limiting for user registration operations (rc_registration)
	if err := c.waitForRateLimit(c.registrationLimiter, "User registration"); err != nil {
		return nil, err
	}

	// First, try to get registration flows
	_, err := c.makeMatrixRequest("POST", "/_matrix/client/v3/register", map[string]any{}, "")
	if err != nil {
		// If we get an error, it might contain flow information - this is expected
		c.logger.LogDebug("Registration flow error (expected)", "error", err)
	}

	// Try registration with dummy auth
	userData := map[string]any{
		"username": username,
		"password": password,
		"auth": map[string]any{
			"type": "m.login.dummy",
		},
	}

	result, err := c.makeMatrixRequest("POST", "/_matrix/client/v3/register", userData, "")
	if err != nil {
		return nil, errors.Wrap(err, "failed to register user")
	}

	var response RegisterResponse
	responseBytes, err := json.Marshal(result)
	if err != nil {
		return nil, errors.Wrap(err, "failed to marshal registration response")
	}

	err = json.Unmarshal(responseBytes, &response)
	if err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal registration response")
	}

	return &response, nil
}

// RegisterResponse represents the response from Matrix user registration
type RegisterResponse struct {
	UserID      string `json:"user_id"`
	AccessToken string `json:"access_token"`
	HomeServer  string `json:"home_server"`
	DeviceID    string `json:"device_id"`
}

// makeMatrixRequest makes an HTTP request to Matrix without Application Service authentication
func (c *Client) makeMatrixRequest(method, endpoint string, data any, token string) (any, error) {
	var body io.Reader

	if data != nil {
		jsonData, err := json.Marshal(data)
		if err != nil {
			return nil, err
		}
		body = strings.NewReader(string(jsonData))
	}

	requestURL := c.serverURL + endpoint
	req, err := http.NewRequest(method, requestURL, body)
	if err != nil {
		return nil, err
	}

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if data != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("matrix API error: %d %s", resp.StatusCode, string(responseBody))
	}

	if len(responseBody) == 0 {
		return map[string]any{}, nil
	}

	var result any
	if err := json.Unmarshal(responseBody, &result); err != nil {
		return nil, err
	}

	return result, nil
}

// GetMattermostChannelID retrieves the Mattermost channel ID from the Matrix room's custom state.
// Returns the channel ID if found, or empty string if not set.
func (c *Client) GetMattermostChannelID(roomID string) (string, error) {
	if c.serverURL == "" || c.asToken == "" {
		return "", errors.New("matrix client not configured")
	}

	url := c.serverURL + "/_matrix/client/v3/rooms/" + url.PathEscape(roomID) + "/state/com.mattermost.bridge.channel/"

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", errors.Wrap(err, "failed to create room state request")
	}

	req.Header.Set("Authorization", "Bearer "+c.asToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", errors.Wrap(err, "failed to send room state request")
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		// State event doesn't exist - this is normal for non-Mattermost rooms
		return "", nil
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("failed to get room state: %d %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", errors.Wrap(err, "failed to read room state response")
	}

	var stateContent struct {
		MattermostChannelID string `json:"mattermost_channel_id"`
		CreatedAt           int64  `json:"created_at"`
	}
	if err := json.Unmarshal(body, &stateContent); err != nil {
		return "", errors.Wrap(err, "failed to unmarshal room state response")
	}

	return stateContent.MattermostChannelID, nil
}

// RemoveMattermostChannelID removes the Mattermost channel ID from the Matrix room's custom state.
// This prevents the room from being found during fallback lookups after unmapping.
func (c *Client) RemoveMattermostChannelID(roomID string) error {
	if c.serverURL == "" || c.asToken == "" {
		return errors.New("matrix client not configured")
	}

	url := c.serverURL + "/_matrix/client/v3/rooms/" + url.PathEscape(roomID) + "/state/com.mattermost.bridge.channel/"

	// Send an empty object to clear the state
	body := strings.NewReader("{}")
	req, err := http.NewRequest("PUT", url, body)
	if err != nil {
		return errors.Wrap(err, "failed to create room state removal request")
	}

	req.Header.Set("Authorization", "Bearer "+c.asToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return errors.Wrap(err, "failed to send room state removal request")
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return errors.Errorf("failed to remove room state: HTTP %d", resp.StatusCode)
	}

	return nil
}
