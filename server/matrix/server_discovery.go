package matrix

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pkg/errors"
)

const (
	// maxWellKnownResponseSize is the maximum size in bytes for .well-known/matrix/server responses
	// This prevents memory exhaustion from excessively large responses
	maxWellKnownResponseSize = 10 * 1024 // 10KB
)

// WellKnownResponse represents the response from /.well-known/matrix/server
type WellKnownResponse struct {
	Server string `json:"m.server"`
}

// ServerDiscovery handles Matrix server name discovery
type ServerDiscovery struct {
	logger     Logger
	httpClient *http.Client
}

// NewServerDiscovery creates a new ServerDiscovery instance
func NewServerDiscovery(logger Logger) *ServerDiscovery {
	return &ServerDiscovery{
		logger: logger,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// DiscoverServerName discovers the Matrix server name (domain for Matrix IDs) using the following chain:
// 1. Use configuredServerName if provided (manual configuration)
// 2. Try .well-known discovery on the serverURL hostname
// 3. Fall back to extracting hostname from serverURL
//
// Returns the server name to use in Matrix IDs (e.g., "example.com")
func (sd *ServerDiscovery) DiscoverServerName(serverURL, configuredServerName string) (string, error) {
	// 1. If configured, use that
	if configuredServerName != "" {
		sd.logger.LogDebug("Using configured server name", "server_name", configuredServerName)
		return configuredServerName, nil
	}

	// 2. Parse the server URL to get hostname
	parsedURL, err := url.Parse(serverURL)
	if err != nil {
		return "", errors.Wrap(err, "failed to parse server URL")
	}

	hostname := parsedURL.Hostname()
	if hostname == "" {
		return "", errors.New("could not extract hostname from server URL")
	}

	// 3. Try .well-known discovery
	wellKnownServerName, err := sd.tryWellKnownDiscovery(hostname)
	if err == nil && wellKnownServerName != "" {
		sd.logger.LogDebug("Discovered server name via .well-known", "hostname", hostname, "server_name", wellKnownServerName)
		return wellKnownServerName, nil
	}

	// Log discovery failure but continue with fallback
	if err != nil {
		sd.logger.LogWarn("Failed to discover server name via .well-known, using hostname fallback", "hostname", hostname, "error", err.Error())
	}

	// 4. Fall back to using hostname as server name
	sd.logger.LogDebug("Using hostname as server name (fallback)", "server_name", hostname)
	return hostname, nil
}

// tryWellKnownDiscovery attempts to discover the Matrix server name via .well-known
// Returns the server name if discovery succeeds, empty string and error otherwise
func (sd *ServerDiscovery) tryWellKnownDiscovery(hostname string) (string, error) {
	// Construct .well-known URL
	wellKnownURL := (&url.URL{
		Scheme: "https",
		Host:   hostname,
		Path:   "/.well-known/matrix/server",
	}).String()

	sd.logger.LogDebug("Attempting .well-known server discovery", "url", wellKnownURL)

	// Make HTTP request
	resp, err := sd.httpClient.Get(wellKnownURL)
	if err != nil {
		return "", errors.Wrap(err, "failed to fetch .well-known")
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	// Check status code
	if resp.StatusCode != http.StatusOK {
		return "", errors.Errorf(".well-known returned status %d", resp.StatusCode)
	}

	// Read and limit response body
	limitedBody := io.LimitReader(resp.Body, maxWellKnownResponseSize)
	body, err := io.ReadAll(limitedBody)
	if err != nil {
		return "", errors.Wrap(err, "failed to read .well-known response")
	}

	// Parse JSON response
	var wellKnown WellKnownResponse
	if err := json.Unmarshal(body, &wellKnown); err != nil {
		return "", errors.Wrap(err, "failed to parse .well-known JSON")
	}

	// Validate response
	if wellKnown.Server == "" {
		return "", errors.New(".well-known response missing m.server field")
	}

	// The .well-known response contains the actual homeserver location
	// But the server name for Matrix IDs is the hostname we queried
	// Example: querying example.com/.well-known returns {"m.server": "matrix.example.com:443"}
	// Server name for IDs is: example.com
	// Actual homeserver API is at: matrix.example.com
	return hostname, nil
}

// ExtractServerDomain extracts the hostname from a fully-qualified server URL
// (e.g., "https://matrix.example.com:8008/path" -> "matrix.example.com").
// This expects a proper URL with a scheme and is used as a fallback when no
// manual configuration or .well-known discovery is available.
func ExtractServerDomain(serverURL string) (string, error) {
	if serverURL == "" {
		return "", errors.New("server URL not configured")
	}

	parsedURL, err := url.Parse(serverURL)
	if err != nil {
		return "", errors.Wrap(err, "failed to parse server URL")
	}

	hostname := parsedURL.Hostname()
	if hostname == "" {
		return "", errors.New("could not extract hostname from server URL")
	}

	return hostname, nil
}

// NormalizeServerName sanitizes a user-provided server name for use in Matrix IDs.
// Unlike ExtractServerDomain which expects a full URL with scheme, this handles
// bare server names that may have been entered with accidental protocol prefixes,
// trailing slashes, or port numbers (e.g., "https://example.com:8008/" -> "example.com").
func NormalizeServerName(serverName string) (string, error) {
	serverName = strings.TrimPrefix(serverName, "https://")
	serverName = strings.TrimPrefix(serverName, "http://")
	serverName = strings.TrimSuffix(serverName, "/")

	// Remove port if present (Matrix IDs don't include ports)
	if host, _, err := net.SplitHostPort(serverName); err == nil {
		serverName = host
	}

	if serverName == "" {
		return "", errors.New("server name is empty after normalization")
	}

	return serverName, nil
}
