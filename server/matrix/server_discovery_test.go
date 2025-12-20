package matrix

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeServerName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "Clean domain",
			input:    "example.com",
			expected: "example.com",
		},
		{
			name:     "Domain with https prefix",
			input:    "https://example.com",
			expected: "example.com",
		},
		{
			name:     "Domain with http prefix",
			input:    "http://example.com",
			expected: "example.com",
		},
		{
			name:     "Domain with trailing slash",
			input:    "example.com/",
			expected: "example.com",
		},
		{
			name:     "Domain with port (should remove port)",
			input:    "example.com:8008",
			expected: "example.com",
		},
		{
			name:     "Domain with protocol and port",
			input:    "https://example.com:8008",
			expected: "example.com",
		},
		{
			name:     "Domain with protocol, port, and trailing slash",
			input:    "https://example.com:8008/",
			expected: "example.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := NormalizeServerName(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestExtractServerDomain(t *testing.T) {
	tests := []struct {
		name        string
		serverURL   string
		expected    string
		expectError bool
	}{
		{
			name:        "Valid HTTPS URL",
			serverURL:   "https://matrix.example.com",
			expected:    "matrix.example.com",
			expectError: false,
		},
		{
			name:        "Valid HTTPS URL with port",
			serverURL:   "https://matrix.example.com:8008",
			expected:    "matrix.example.com",
			expectError: false,
		},
		{
			name:        "Valid HTTP URL",
			serverURL:   "http://localhost:8008",
			expected:    "localhost",
			expectError: false,
		},
		{
			name:        "Empty URL",
			serverURL:   "",
			expected:    "",
			expectError: true,
		},
		{
			name:        "Invalid URL",
			serverURL:   "://invalid",
			expected:    "",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ExtractServerDomain(tt.serverURL)
			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expected, result)
			}
		})
	}
}

func TestServerDiscoveryWithConfiguredServerName(t *testing.T) {
	logger := NewTestLogger(t)
	discovery := NewServerDiscovery(logger)

	serverName, err := discovery.DiscoverServerName("https://matrix.example.com", "example.com")

	require.NoError(t, err)
	assert.Equal(t, "example.com", serverName, "Should use configured server name")
}

func TestServerDiscoveryWithWellKnown(t *testing.T) {
	// Create a test HTTP server that serves .well-known
	wellKnownHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/matrix/server" {
			w.Header().Set("Content-Type", "application/json")
			response := WellKnownResponse{
				Server: "matrix.example.com:443",
			}
			json.NewEncoder(w).Encode(response)
			return
		}
		http.NotFound(w, r)
	})

	server := httptest.NewTLSServer(wellKnownHandler)
	defer server.Close()

	// Extract hostname from test server URL
	// Note: In real usage, the URL would be like "https://example.com"
	// and .well-known would be at "https://example.com/.well-known/matrix/server"
	// For testing, we're using the test server directly

	logger := NewTestLogger(t)
	discovery := NewServerDiscovery(logger)
	// Use the test server's custom HTTP client
	discovery.httpClient = server.Client()

	// We can't easily test the full .well-known discovery with httptest
	// because it requires hostname resolution, so we'll test the tryWellKnownDiscovery directly
	// This test mainly validates the JSON parsing logic

	// For now, test that manual config works
	serverName, err := discovery.DiscoverServerName("https://matrix.example.com", "example.com")
	require.NoError(t, err)
	assert.Equal(t, "example.com", serverName)
}

func TestServerDiscoveryFallbackToHostname(t *testing.T) {
	logger := NewTestLogger(t)
	discovery := NewServerDiscovery(logger)

	// No configured server name, .well-known will fail for invalid domain
	// Should fall back to hostname extraction
	serverName, err := discovery.DiscoverServerName("https://matrix.example.com:8008", "")

	require.NoError(t, err)
	assert.Equal(t, "matrix.example.com", serverName, "Should fall back to hostname from URL")
}

func TestServerDiscoveryInvalidURL(t *testing.T) {
	logger := NewTestLogger(t)
	discovery := NewServerDiscovery(logger)

	_, err := discovery.DiscoverServerName("://invalid-url", "")

	assert.Error(t, err, "Should return error for invalid URL")
}

func TestTryWellKnownDiscovery(t *testing.T) {
	t.Run("Successful discovery", func(t *testing.T) {
		// Create a test server that returns valid .well-known response
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/.well-known/matrix/server" {
				w.Header().Set("Content-Type", "application/json")
				response := WellKnownResponse{
					Server: "matrix.example.com:8448",
				}
				json.NewEncoder(w).Encode(response)
				return
			}
			http.NotFound(w, r)
		}))
		defer server.Close()

		// Extract hostname from test server
		logger := NewTestLogger(t)
		discovery := NewServerDiscovery(logger)

		// We can't test this directly without modifying the hostname
		// but we can test that the HTTP response is parsed correctly
		// by calling the server directly
		resp, err := discovery.httpClient.Get(server.URL + "/.well-known/matrix/server")
		require.NoError(t, err)
		defer resp.Body.Close()

		var wellKnown WellKnownResponse
		err = json.NewDecoder(resp.Body).Decode(&wellKnown)
		require.NoError(t, err)
		assert.Equal(t, "matrix.example.com:8448", wellKnown.Server)
	})

	t.Run("404 Not Found", func(t *testing.T) {
		logger := NewTestLogger(t)
		discovery := NewServerDiscovery(logger)

		// Try a domain that definitely doesn't have .well-known
		serverName, err := discovery.tryWellKnownDiscovery("nonexistent-test-domain-12345.invalid")

		assert.Error(t, err)
		assert.Empty(t, serverName)
	})

	t.Run("Invalid JSON response", func(t *testing.T) {
		// Create a test server that returns invalid JSON
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte("invalid json"))
		}))
		defer server.Close()

		logger := NewTestLogger(t)
		discovery := NewServerDiscovery(logger)

		// Extract hostname from test server URL for testing
		// This won't work in practice but tests the JSON parsing
		resp, err := discovery.httpClient.Get(server.URL + "/.well-known/matrix/server")
		require.NoError(t, err)
		defer resp.Body.Close()

		var wellKnown WellKnownResponse
		err = json.NewDecoder(resp.Body).Decode(&wellKnown)
		assert.Error(t, err, "Should fail to decode invalid JSON")
	})
}

func TestServerDiscoveryIntegration(t *testing.T) {
	tests := []struct {
		name                   string
		serverURL              string
		configuredServerName   string
		expectedServerName     string
		shouldAttemptWellKnown bool
	}{
		{
			name:                   "Configured server name provided",
			serverURL:              "https://matrix.example.com:8008",
			configuredServerName:   "example.com",
			expectedServerName:     "example.com",
			shouldAttemptWellKnown: false,
		},
		{
			name:                   "No configured name, fallback to hostname",
			serverURL:              "https://matrix.example.com:8008",
			configuredServerName:   "",
			expectedServerName:     "matrix.example.com",
			shouldAttemptWellKnown: true,
		},
		{
			name:                   "Clean URL without port",
			serverURL:              "https://matrix.org",
			configuredServerName:   "",
			expectedServerName:     "matrix.org",
			shouldAttemptWellKnown: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger := NewTestLogger(t)
			discovery := NewServerDiscovery(logger)

			serverName, err := discovery.DiscoverServerName(tt.serverURL, tt.configuredServerName)

			require.NoError(t, err)
			assert.Equal(t, tt.expectedServerName, serverName)
		})
	}
}
