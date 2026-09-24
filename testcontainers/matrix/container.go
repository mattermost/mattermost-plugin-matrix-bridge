package matrix

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/matrix"
)

// Global container registry for cleanup
var (
	activeContainers = make(map[*Container]bool)
	containerMutex   sync.RWMutex
)

const synapsePort = "18008"

// DefaultWaitTimeout and PollInterval are the shared defaults for polling waits.
const (
	DefaultWaitTimeout = 30 * time.Second
	PollInterval       = 250 * time.Millisecond
)

// Container wraps a testcontainer running Synapse
type Container struct {
	Container    testcontainers.Container
	ServerURL    string
	ServerDomain string
	ASToken      string
	HSToken      string
	Client       *matrix.Client
	// InternalURL is Synapse's URL inside MatrixTestConfig.Network; empty when not networked.
	InternalURL string

	tokensMu   sync.Mutex
	userTokens map[string]string
}

// StartMatrixContainer starts a Synapse container for testing
func StartMatrixContainer(t *testing.T, config MatrixTestConfig) *Container {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	mc, err := Start(ctx, config, t)
	require.NoError(t, err)
	return mc
}

// Start starts a Synapse container without needing a *testing.T; stop it with Terminate.
// A nil logger discards logs.
func Start(ctx context.Context, config MatrixTestConfig, logger matrix.TestLogger) (*Container, error) {
	logf := func(format string, args ...any) {
		if logger != nil {
			logger.Logf(format, args...)
		}
	}

	if config.AppServiceURL == "" {
		config.AppServiceURL = defaultAppServiceURL
	}
	if config.Network != nil && config.NetworkAlias == "" {
		config.NetworkAlias = "synapse"
	}

	// Create Synapse configuration
	synapseConfig := generateSynapseConfig(config)
	appServiceConfig := generateAppServiceConfig(config)

	req := testcontainers.ContainerRequest{
		Image: "matrixdotorg/synapse:v1.119.0",
		Env: map[string]string{
			"SYNAPSE_SERVER_NAME":  config.ServerName,
			"SYNAPSE_REPORT_STATS": "no",
			"SYNAPSE_NO_TLS":       "true",
		},
		ExposedPorts: []string{synapsePort + "/tcp"}, // Expose port for dynamic assignment
		Files: []testcontainers.ContainerFile{
			{
				HostFilePath:      "",
				ContainerFilePath: "/data/homeserver.yaml",
				FileMode:          0o644,
				Reader:            strings.NewReader(synapseConfig),
			},
			{
				HostFilePath:      "",
				ContainerFilePath: "/data/appservice.yaml",
				FileMode:          0o644,
				Reader:            strings.NewReader(appServiceConfig),
			},
			{
				HostFilePath:      "",
				ContainerFilePath: "/data/log.config",
				FileMode:          0o644,
				Reader:            strings.NewReader(generateLogConfig()),
			},
		},
		Entrypoint: []string{
			"sh", "-c",
			"python -m synapse.app.homeserver --config-path=/data/homeserver.yaml --generate-keys && python -m synapse.app.homeserver --config-path=/data/homeserver.yaml",
		},
		// Wait for Synapse to start (HTTP readiness checked after port mapping)
		WaitingFor: wait.ForLog("SynapseSite starting on " + synapsePort).WithStartupTimeout(60 * time.Second),
	}
	if config.Network != nil {
		req.Networks = []string{config.Network.Name}
		req.NetworkAliases = map[string][]string{config.Network.Name: {config.NetworkAlias}}
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		terminateQuietly(container)
		return nil, fmt.Errorf("failed to start Synapse container: %w", err)
	}

	// Get the dynamically assigned host port
	hostPort, err := container.MappedPort(ctx, synapsePort)
	if err != nil {
		terminateQuietly(container)
		return nil, fmt.Errorf("failed to get Synapse mapped port: %w", err)
	}

	serverURL := fmt.Sprintf("http://localhost:%s", hostPort.Port())
	logf("Using dynamically assigned port: %s", serverURL)

	mc := &Container{
		Container:    container,
		ServerURL:    serverURL,
		ServerDomain: config.ServerName,
		ASToken:      config.ASToken,
		HSToken:      config.HSToken,
		userTokens:   make(map[string]string),
	}
	if config.Network != nil {
		mc.InternalURL = fmt.Sprintf("http://%s:%s", config.NetworkAlias, synapsePort)
	}

	// Wait for Matrix to be fully ready
	if err := mc.waitForMatrixReady(ctx, logf); err != nil {
		terminateQuietly(container)
		return nil, err
	}

	// Create Matrix client with rate limiting for test operations
	mc.Client = matrix.NewClientWithLoggerAndRateLimit(
		serverURL,
		config.ASToken,
		"test-remote-id",
		"", // No configured server name - will be set via SetServerDomain for testing
		matrix.NewTestLogger(logger),
		matrix.TestRateLimitConfig(),
	)
	mc.Client.SetServerDomain(config.ServerName)

	// Register container for cleanup tracking
	containerMutex.Lock()
	activeContainers[mc] = true
	containerMutex.Unlock()

	return mc, nil
}

// Cleanup terminates the Matrix container
func (mc *Container) Cleanup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := mc.Terminate(ctx); err != nil {
		t.Logf("Warning: Failed to terminate Matrix container: %v", err)
		// Don't fail the test on cleanup errors, just log them
	}
}

// Terminate stops the container and removes it from CleanupAllContainers tracking.
func (mc *Container) Terminate(ctx context.Context) error {
	if mc.Container == nil {
		return nil
	}

	containerMutex.Lock()
	delete(activeContainers, mc)
	containerMutex.Unlock()

	return mc.Container.Terminate(ctx)
}

func terminateQuietly(container testcontainers.Container) {
	if container == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = container.Terminate(ctx)
}

// CleanupAllContainers forcibly cleans up any remaining active containers
// This should be called as a safety measure, e.g., in TestMain
func CleanupAllContainers() {
	containerMutex.Lock()
	defer containerMutex.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for mc := range activeContainers {
		if mc.Container != nil {
			_ = mc.Container.Terminate(ctx) // Ignore errors in emergency cleanup
		}
	}

	// Clear the map
	activeContainers = make(map[*Container]bool)
}

// waitForMatrixReady waits for Matrix server to be fully operational
func (mc *Container) waitForMatrixReady(ctx context.Context, logf func(format string, args ...any)) error {
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	// Give extra time for server to fully start after log message
	time.Sleep(3 * time.Second)

	attempts := 0
	var lastErr error
	for {
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("matrix server HTTP endpoint did not become ready within timeout at %s: %w", mc.ServerURL, lastErr)
			}
			return fmt.Errorf("matrix server HTTP endpoint did not become ready within timeout at %s", mc.ServerURL)
		default:
			ready, err := mc.isMatrixReady()
			if err != nil {
				lastErr = err
				attempts++
				// Log every 10 attempts to avoid spam
				if attempts%10 == 1 {
					logf("Waiting for Matrix server at %s (attempt %d): %v", mc.ServerURL, attempts, err)
				}
			}
			if ready {
				logf("Matrix server is ready and responding at %s after %d attempts", mc.ServerURL, attempts)
				return nil
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
}

// isMatrixReady checks if Matrix server is responding properly
func (mc *Container) isMatrixReady() (bool, error) {
	client := &http.Client{
		Timeout: 5 * time.Second,
	}
	resp, err := client.Get(mc.ServerURL + "/_matrix/client/versions")
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}
	return true, nil
}

// CreateRoom creates a test room and returns its room ID
// Uses the Matrix client with proper rate limiting to avoid 429 errors
func (mc *Container) CreateRoom(t *testing.T, roomName string) string {
	roomIdentifier, err := mc.Client.CreateRoom(roomName, "", mc.ServerDomain, true, "")
	require.NoError(t, err)

	// The client returns either a room alias or room ID
	// For test purposes, we need to get the actual room ID
	if strings.HasPrefix(roomIdentifier, "#") {
		// It's an alias, resolve it to room ID
		roomID, err := mc.Client.ResolveRoomAlias(roomIdentifier)
		require.NoError(t, err)
		return roomID
	}

	// It's already a room ID
	return roomIdentifier
}

// JoinRoom joins a room as the application service
func (mc *Container) JoinRoom(t *testing.T, roomID string) {
	_, err := mc.makeMatrixRequest(mc.ASToken, "POST", fmt.Sprintf("/_matrix/client/v3/join/%s", roomID), map[string]any{})
	require.NoError(t, err)
}

// GetRoomEvents retrieves events from a Matrix room
func (mc *Container) GetRoomEvents(t *testing.T, roomID string) []Event {
	events, err := mc.fetchRoomEvents(roomID)
	require.NoError(t, err)

	// Debug: Log the events we receive
	t.Logf("GetRoomEvents: Found %d events in room %s", len(events), roomID)
	for i, event := range events {
		t.Logf("Event %d: type=%s, event_id=%s, sender=%s, has_mattermost_post_id=%v",
			i, event.Type, event.EventID, event.Sender,
			event.Content != nil && event.Content["mattermost_post_id"] != nil)
	}

	return events
}

// GetEvent retrieves a specific event by ID
func (mc *Container) GetEvent(t *testing.T, roomID, eventID string) Event {
	result, err := mc.makeMatrixRequest(mc.ASToken, "GET", fmt.Sprintf("/_matrix/client/v3/rooms/%s/event/%s", roomID, eventID), nil)
	require.NoError(t, err)

	var event Event
	responseBytes, err := json.Marshal(result)
	require.NoError(t, err)
	err = json.Unmarshal(responseBytes, &event)
	require.NoError(t, err)

	return event
}

// GetRoomState retrieves the current state of a room
func (mc *Container) GetRoomState(t *testing.T, roomID string) []Event {
	result, err := mc.makeMatrixRequest(mc.ASToken, "GET", fmt.Sprintf("/_matrix/client/v3/rooms/%s/state", roomID), nil)
	require.NoError(t, err)

	var stateEvents []Event
	responseBytes, err := json.Marshal(result)
	require.NoError(t, err)
	err = json.Unmarshal(responseBytes, &stateEvents)
	require.NoError(t, err)

	return stateEvents
}

// GetRoomName retrieves the name of a room from its state
func (mc *Container) GetRoomName(t *testing.T, roomID string) string {
	state := mc.GetRoomState(t, roomID)

	// Look for m.room.name event
	for _, event := range state {
		if event.Type == "m.room.name" {
			if name, exists := event.Content["name"].(string); exists {
				return name
			}
		}
	}

	return "" // No name found
}

// SendMessage sends a message to a room (for testing purposes)
func (mc *Container) SendMessage(t *testing.T, roomID, message string) string {
	content := map[string]any{
		"msgtype": "m.text",
		"body":    message,
	}

	result, err := mc.makeMatrixRequest(mc.ASToken, "PUT", fmt.Sprintf("/_matrix/client/v3/rooms/%s/send/m.room.message/%d", roomID, time.Now().UnixNano()), content)
	require.NoError(t, err)

	var response SendEventResponse
	responseBytes, err := json.Marshal(result)
	require.NoError(t, err)
	err = json.Unmarshal(responseBytes, &response)
	require.NoError(t, err)

	return response.EventID
}

// Matrix API Response Structs

// CreateRoomResponse represents the response from the /createRoom endpoint
type CreateRoomResponse struct {
	RoomID string `json:"room_id"`
}

// RegisterResponse represents the response from the /register endpoint
type RegisterResponse struct {
	UserID      string `json:"user_id"`
	AccessToken string `json:"access_token,omitempty"`
	DeviceID    string `json:"device_id,omitempty"`
}

// SendEventResponse represents the response from sending an event
type SendEventResponse struct {
	EventID string `json:"event_id"`
}

// RoomMembersResponse represents the response from /rooms/{roomId}/members
type RoomMembersResponse struct {
	Chunk []Event `json:"chunk"`
}

// RoomMessagesResponse represents the response from /rooms/{roomId}/messages
type RoomMessagesResponse struct {
	Chunk []Event `json:"chunk"`
	Start string  `json:"start,omitempty"`
	End   string  `json:"end,omitempty"`
}

// Event represents a generic Matrix event
type Event struct {
	Type         string         `json:"type"`
	EventID      string         `json:"event_id,omitempty"`
	Sender       string         `json:"sender,omitempty"`
	StateKey     *string        `json:"state_key,omitempty"`
	Content      map[string]any `json:"content"`
	Timestamp    int64          `json:"origin_server_ts,omitempty"`
	RoomID       string         `json:"room_id,omitempty"`
	RelatedEvent map[string]any `json:"m.relates_to,omitempty"`
}

// User represents a test user with credentials
type User struct {
	UserID      string
	Username    string
	Password    string
	AccessToken string
}

// CreateUser creates a test user and returns user information
// Uses the rate-limited Matrix client to avoid 429 errors
func (mc *Container) CreateUser(t *testing.T, username, password string) *User {
	// Use the rate-limited Matrix client instead of direct HTTP calls
	// This prevents 429 M_LIMIT_EXCEEDED errors during rapid user creation
	response, err := mc.Client.RegisterUser(username, password)
	require.NoError(t, err)

	mc.tokensMu.Lock()
	mc.userTokens[response.UserID] = response.AccessToken
	mc.tokensMu.Unlock()

	return &User{
		UserID:      response.UserID,
		Username:    username,
		Password:    password,
		AccessToken: response.AccessToken,
	}
}

// RoomMember represents a member of a Matrix room
type RoomMember struct {
	UserID     string
	Membership string
}

// GetRoomMembers retrieves the members of a room
func (mc *Container) GetRoomMembers(t *testing.T, roomID string) []*RoomMember {
	result, err := mc.makeMatrixRequest(mc.ASToken, "GET", fmt.Sprintf("/_matrix/client/v3/rooms/%s/members", roomID), nil)
	require.NoError(t, err)

	var response RoomMembersResponse
	responseBytes, err := json.Marshal(result)
	require.NoError(t, err)
	err = json.Unmarshal(responseBytes, &response)
	require.NoError(t, err)

	members := make([]*RoomMember, 0, len(response.Chunk))
	for _, event := range response.Chunk {
		if event.Type == "m.room.member" && event.StateKey != nil {
			userID := *event.StateKey
			if membership, exists := event.Content["membership"].(string); exists {
				members = append(members, &RoomMember{
					UserID:     userID,
					Membership: membership,
				})
			}
		}
	}

	return members
}

// RoomInfo contains information about a Matrix room
type RoomInfo struct {
	Name        string
	Topic       string
	JoinRule    string
	GuestAccess bool
}

// GetRoomInfo retrieves comprehensive information about a room
func (mc *Container) GetRoomInfo(t *testing.T, roomID string) *RoomInfo {
	state := mc.GetRoomState(t, roomID)

	info := &RoomInfo{}

	for _, event := range state {
		eventType := event.Type
		content := event.Content

		switch eventType {
		case "m.room.name":
			if name, exists := content["name"].(string); exists {
				info.Name = name
			}
		case "m.room.topic":
			if topic, exists := content["topic"].(string); exists {
				info.Topic = topic
			}
		case "m.room.join_rules":
			if joinRule, exists := content["join_rule"].(string); exists {
				info.JoinRule = joinRule
			}
		case "m.room.guest_access":
			if guestAccess, exists := content["guest_access"].(string); exists {
				info.GuestAccess = guestAccess == "can_join"
			}
		}
	}

	return info
}

// GetApplicationServiceBotUserID returns the application service bot user ID
func (mc *Container) GetApplicationServiceBotUserID() string {
	return fmt.Sprintf("@_mattermost_bot:%s", mc.ServerDomain)
}

// JoinRoomAsUser joins a room as a user created with CreateUser, using that user's own token.
// Unknown users fall back to the AS token, which joins as the AS bot.
func (mc *Container) JoinRoomAsUser(_ *testing.T, userID, roomID string) error {
	token := mc.userToken(userID)
	if token == "" {
		token = mc.ASToken
	}
	_, err := mc.makeMatrixRequest(token, "POST", "/_matrix/client/v3/join/"+url.PathEscape(roomID), map[string]any{})
	return err
}

// DoAsUser makes a Matrix client API request authenticated with the user's own access token.
func (mc *Container) DoAsUser(user *User, method, endpoint string, body any) (any, error) {
	return mc.makeMatrixRequest(user.AccessToken, method, endpoint, body)
}

// SendEventAsUser sends a room event as the user and returns its event ID.
func (mc *Container) SendEventAsUser(t *testing.T, user *User, roomID, eventType string, content map[string]any) string {
	endpoint := fmt.Sprintf("/_matrix/client/v3/rooms/%s/send/%s/%d",
		url.PathEscape(roomID), url.PathEscape(eventType), time.Now().UnixNano())
	result, err := mc.DoAsUser(user, "PUT", endpoint, content)
	require.NoError(t, err, "send %s as %s to room %s", eventType, user.UserID, roomID)

	var response SendEventResponse
	require.NoError(t, decode(result, &response))
	return response.EventID
}

// CreateRoomAsUser creates a public room owned by the user and returns its ID and alias.
// The alias localpart must not start with _mattermost_ or mattermost-bridge-: the AS owns those.
func (mc *Container) CreateRoomAsUser(t *testing.T, user *User, name, aliasLocalpart string) (roomID, alias string) {
	result, err := mc.DoAsUser(user, "POST", "/_matrix/client/v3/createRoom", map[string]any{
		"name":            name,
		"preset":          "public_chat",
		"room_alias_name": aliasLocalpart,
	})
	require.NoError(t, err, "create room %q as %s", aliasLocalpart, user.UserID)

	var response CreateRoomResponse
	require.NoError(t, decode(result, &response))
	return response.RoomID, fmt.Sprintf("#%s:%s", aliasLocalpart, mc.ServerDomain)
}

// WaitForRoomEvent polls the room's 100 most recent events until one matches and returns it.
// The AS bot must be able to read the room, which is true once it has joined (as in bridged rooms).
func (mc *Container) WaitForRoomEvent(t *testing.T, roomID string, match func(Event) bool) Event {
	t.Helper()
	var found Event
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		events, err := mc.fetchRoomEvents(roomID)
		if !assert.NoError(c, err) {
			return
		}
		if i := slices.IndexFunc(events, match); i >= 0 {
			found = events[i]
			return
		}
		c.Errorf("no matching event yet in room %s (%d events checked)", roomID, len(events))
	}, DefaultWaitTimeout, PollInterval)
	return found
}

// RequireNoRoomEvent fails if a matching event appears in the room during window or in a final
// check after it; only the final check's fetch errors fail the test. It has the same
// readability precondition as WaitForRoomEvent.
func (mc *Container) RequireNoRoomEvent(t *testing.T, roomID string, match func(Event) bool, window time.Duration) {
	t.Helper()
	matches := func() (bool, error) {
		events, err := mc.fetchRoomEvents(roomID)
		if err != nil {
			return false, err
		}
		return slices.ContainsFunc(events, match), nil
	}

	require.Never(t, func() bool {
		found, _ := matches()
		return found
	}, window, PollInterval, "unexpected matching event in room %s", roomID)

	found, err := matches()
	require.NoError(t, err, "fetch events for room %s", roomID)
	require.False(t, found, "unexpected matching event in room %s", roomID)
}

// Logs returns the last n lines of the Synapse container log, or all of it when n <= 0.
func (mc *Container) Logs(ctx context.Context, n int) (string, error) {
	reader, err := mc.Container.Logs(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = reader.Close() }()

	data, err := io.ReadAll(reader)
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n"), nil
}

func (mc *Container) fetchRoomEvents(roomID string) ([]Event, error) {
	result, err := mc.makeMatrixRequest(mc.ASToken, "GET",
		fmt.Sprintf("/_matrix/client/v3/rooms/%s/messages?dir=b&limit=100", url.PathEscape(roomID)), nil)
	if err != nil {
		return nil, err
	}
	var response RoomMessagesResponse
	if err := decode(result, &response); err != nil {
		return nil, err
	}
	return response.Chunk, nil
}

func (mc *Container) userToken(userID string) string {
	mc.tokensMu.Lock()
	defer mc.tokensMu.Unlock()
	return mc.userTokens[userID]
}

// decode converts a generic JSON result from makeMatrixRequest into target.
func decode(result, target any) error {
	responseBytes, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return json.Unmarshal(responseBytes, target)
}

// makeMatrixRequest makes a request to the Matrix server authenticated with token
func (mc *Container) makeMatrixRequest(token, method, endpoint string, data any) (any, error) {
	var body io.Reader

	if data != nil {
		jsonData, err := json.Marshal(data)
		if err != nil {
			return nil, err
		}
		body = strings.NewReader(string(jsonData))
	}

	req, err := http.NewRequest(method, mc.ServerURL+endpoint, body)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+token)
	if data != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	client := &http.Client{
		Timeout: 10 * time.Second,
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode >= 400 {
		//nolint:staticcheck // Error message capitalization is intentional for Matrix API errors
		return nil, fmt.Errorf("matrix API error: %d %s", resp.StatusCode, string(responseBody))
	}

	if len(responseBody) == 0 {
		return nil, nil
	}

	var result any
	err = json.Unmarshal(responseBody, &result)
	if err != nil {
		return nil, err
	}

	return result, nil
}

// generateSynapseConfig generates a basic Synapse configuration
func generateSynapseConfig(config MatrixTestConfig) string {
	return fmt.Sprintf(`
server_name: "%s"
pid_file: /tmp/homeserver.pid
web_client_location: https://app.element.io/

listeners:
  - port: 18008
    tls: false
    type: http
    x_forwarded: true
    bind_addresses: ['0.0.0.0']
    resources:
      - names: [client, federation]
        compress: false

database:
  name: sqlite3
  args:
    database: ":memory:"

log_config: "/data/log.config"

media_store_path: /tmp/media_store
registration_shared_secret: "test_secret_12345"
report_stats: false
macaroon_secret_key: "test_macaroon_12345"
form_secret: "test_form_12345"

signing_key_path: "/tmp/signing.key"

trusted_key_servers: []

app_service_config_files:
  - /data/appservice.yaml

# Disable user directory for simpler testing
user_directory:
  enabled: false

# Disable encryption for simpler testing
encryption_enabled_by_default_for_room_type: off

# Allow public rooms and directory publishing
allow_public_rooms_over_federation: true
allow_public_rooms_without_auth: true

# Enable registration for testing
enable_registration: true
enable_registration_without_verification: true

# Disable rate limiting for tests (matches production config)
rc_message:
  per_second: 1000
  burst_count: 1000

rc_registration:
  per_second: 1000
  burst_count: 1000

rc_login:
  address:
    per_second: 1000
    burst_count: 1000
  account:
    per_second: 1000
    burst_count: 1000
  failed_attempts:
    per_second: 1000
    burst_count: 1000
`, config.ServerName)
}

// generateAppServiceConfig generates application service configuration
func generateAppServiceConfig(config MatrixTestConfig) string {
	return fmt.Sprintf(`
id: mattermost-bridge
url: %s
as_token: "%s"
hs_token: "%s"
sender_localpart: _mattermost_bot

namespaces:
  users:
    - exclusive: true
      regex: "@_mattermost_.*:%s"
  aliases:
    - exclusive: true  
      regex: "#_mattermost_.*:%s"
    - exclusive: true
      regex: "#mattermost-bridge-.*:%s"
  rooms: []

protocols: []
`, config.AppServiceURL, config.ASToken, config.HSToken, config.ServerName, config.ServerName, config.ServerName)
}

// generateLogConfig generates a simple logging configuration for Synapse
func generateLogConfig() string {
	return `version: 1
formatters:
  precise:
    format: '%(asctime)s - %(name)s - %(lineno)d - %(levelname)s - %(request)s - %(message)s'
handlers:
  console:
    class: logging.StreamHandler
    formatter: precise
    stream: ext://sys.stdout
root:
  level: INFO
  handlers: [console]
`
}
