package matrix

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// MatrixContainer wraps a testcontainer running Synapse
//
//nolint:revive // MatrixContainer is intentionally named to be descriptive in test context
type MatrixContainer struct {
	Container    testcontainers.Container
	ServerURL    string
	ServerDomain string
	ASToken      string
	HSToken      string
}

// StartMatrixContainer starts a Synapse container for testing
func StartMatrixContainer(t *testing.T, config MatrixTestConfig) *MatrixContainer {
	ctx := context.Background()

	// Create Synapse configuration
	synapseConfig := generateSynapseConfig(config)
	appServiceConfig := generateAppServiceConfig(config)

	// Create container with Synapse using host networking to bypass VPN issues
	req := testcontainers.ContainerRequest{
		Image:        "matrixdotorg/synapse:latest",
		NetworkMode:  "host", // Use host networking to bypass VPN conflicts
		Env: map[string]string{
			"SYNAPSE_SERVER_NAME":  config.ServerName,
			"SYNAPSE_REPORT_STATS": "no",
			"SYNAPSE_NO_TLS":       "true",
		},
		Files: []testcontainers.ContainerFile{
			{
				HostFilePath:      "",
				ContainerFilePath: "/data/homeserver.yaml",
				FileMode:          0644,
				Reader:            strings.NewReader(synapseConfig),
			},
			{
				HostFilePath:      "",
				ContainerFilePath: "/data/appservice.yaml",
				FileMode:          0644,
				Reader:            strings.NewReader(appServiceConfig),
			},
			{
				HostFilePath:      "",
				ContainerFilePath: "/data/log.config",
				FileMode:          0644,
				Reader:            strings.NewReader(generateLogConfig()),
			},
		},
		Entrypoint: []string{
			"sh", "-c",
			"python -m synapse.app.homeserver --config-path=/data/homeserver.yaml --generate-keys && python -m synapse.app.homeserver --config-path=/data/homeserver.yaml",
		},
		WaitingFor: wait.ForLog("SynapseSite starting on 18008").WithStartupTimeout(60*time.Second),
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err)

	// With host networking, use a different port to avoid conflicts
	serverURL := "http://localhost:18008"
	t.Logf("Using host networking: %s", serverURL)

	mc := &MatrixContainer{
		Container:    container,
		ServerURL:    serverURL,
		ServerDomain: config.ServerName,
		ASToken:      config.ASToken,
		HSToken:      config.HSToken,
	}

	// Wait for Matrix to be fully ready
	mc.waitForMatrixReady(t)

	return mc
}

// Cleanup terminates the Matrix container
func (mc *MatrixContainer) Cleanup(t *testing.T) {
	ctx := context.Background()
	err := mc.Container.Terminate(ctx)
	require.NoError(t, err)
}

// waitForMatrixReady waits for Matrix server to be fully operational
func (mc *MatrixContainer) waitForMatrixReady(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Give extra time for server to fully start after log message
	time.Sleep(2 * time.Second)

	for {
		select {
		case <-ctx.Done():
			t.Logf("Matrix server connectivity check timed out. VPN environments may interfere with container networking.")
			t.Logf("Proceeding anyway since container is running and log shows server started.")
			return // Don't fail - proceed with tests
		default:
			if mc.isMatrixReady() {
				t.Logf("Matrix server is ready and responding")
				return
			}
			time.Sleep(1 * time.Second)
		}
	}
}

// isMatrixReady checks if Matrix server is responding properly
func (mc *MatrixContainer) isMatrixReady() bool {
	client := &http.Client{
		Timeout: 5 * time.Second,
	}
	resp, err := client.Get(mc.ServerURL + "/_matrix/client/versions")
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()

	return resp.StatusCode == http.StatusOK
}

// CreateRoom creates a test room and returns its room ID
func (mc *MatrixContainer) CreateRoom(t *testing.T, roomName string) string {
	roomData := map[string]any{
		"name":       roomName,
		"preset":     "public_chat",
		"visibility": "public",
	}

	roomID, err := mc.makeMatrixRequest("POST", "/_matrix/client/v3/createRoom", roomData)
	require.NoError(t, err)

	response := roomID.(map[string]any)
	return response["room_id"].(string)
}

// JoinRoom joins a room as the application service
func (mc *MatrixContainer) JoinRoom(t *testing.T, roomID string) {
	_, err := mc.makeMatrixRequest("POST", fmt.Sprintf("/_matrix/client/v3/join/%s", roomID), map[string]any{})
	require.NoError(t, err)
}

// GetRoomEvents retrieves events from a Matrix room
func (mc *MatrixContainer) GetRoomEvents(t *testing.T, roomID string) []map[string]any {
	result, err := mc.makeMatrixRequest("GET", fmt.Sprintf("/_matrix/client/v3/rooms/%s/messages", roomID), nil)
	require.NoError(t, err)

	response := result.(map[string]any)
	chunk := response["chunk"].([]any)

	events := make([]map[string]any, len(chunk))
	for i, event := range chunk {
		events[i] = event.(map[string]any)
	}

	return events
}

// GetEvent retrieves a specific event by ID
func (mc *MatrixContainer) GetEvent(t *testing.T, roomID, eventID string) map[string]any {
	result, err := mc.makeMatrixRequest("GET", fmt.Sprintf("/_matrix/client/v3/rooms/%s/event/%s", roomID, eventID), nil)
	require.NoError(t, err)

	return result.(map[string]any)
}

// SendMessage sends a message to a room (for testing purposes)
func (mc *MatrixContainer) SendMessage(t *testing.T, roomID, message string) string {
	content := map[string]any{
		"msgtype": "m.text",
		"body":    message,
	}

	result, err := mc.makeMatrixRequest("PUT", fmt.Sprintf("/_matrix/client/v3/rooms/%s/send/m.room.message/%d", roomID, time.Now().UnixNano()), content)
	require.NoError(t, err)

	response := result.(map[string]any)
	return response["event_id"].(string)
}

// CreateUser creates a test user
func (mc *MatrixContainer) CreateUser(t *testing.T, username, password string) string {
	// First, try to get registration flows
	_, err := mc.makeMatrixRequestNoAuth("POST", "/_matrix/client/v3/register", map[string]any{})
	if err != nil {
		// If we get an error, it might contain flow information
		t.Logf("Registration flow error (expected): %v", err)
	}

	// Try registration with dummy auth
	userData := map[string]any{
		"username": username,
		"password": password,
		"auth": map[string]any{
			"type": "m.login.dummy",
		},
	}

	result, err := mc.makeMatrixRequestNoAuth("POST", "/_matrix/client/v3/register", userData)
	require.NoError(t, err)

	response := result.(map[string]any)
	return response["user_id"].(string)
}

// makeMatrixRequest makes an authenticated request to the Matrix server
func (mc *MatrixContainer) makeMatrixRequest(method, endpoint string, data any) (any, error) {
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

	req.Header.Set("Authorization", "Bearer "+mc.ASToken)
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
		return nil, fmt.Errorf("Matrix API error: %d %s", resp.StatusCode, string(responseBody))
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

// makeMatrixRequestNoAuth makes an unauthenticated request to the Matrix server
func (mc *MatrixContainer) makeMatrixRequestNoAuth(method, endpoint string, data any) (any, error) {
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
		return nil, fmt.Errorf("Matrix API error: %d %s", resp.StatusCode, string(responseBody))
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

# Allow public rooms
allow_public_rooms_over_federation: true
allow_public_rooms_without_auth: true

# Disable rate limiting for tests
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

# Enable registration for testing
enable_registration: true
enable_registration_without_verification: true
`, config.ServerName)
}

// generateAppServiceConfig generates application service configuration
func generateAppServiceConfig(config MatrixTestConfig) string {
	return fmt.Sprintf(`
id: mattermost-bridge
url: http://localhost:8080
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
  rooms: []

protocols: []
`, config.ASToken, config.HSToken, config.ServerName, config.ServerName)
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
