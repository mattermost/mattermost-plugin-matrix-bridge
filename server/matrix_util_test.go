package main

import (
	"strings"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/mocks"
	"github.com/stretchr/testify/require"
)

const (
	sampleMultiLineHTML = `<h3>Suspected Lateral Movement – Relay Switching Behavior Observed</h3>  
<p>We've observed endpoint switching consistent with lateral movement from SATCOM ingress. Traces suggest adversary is exploring multiple exit points across coalition relay nodes.</p>  
<ul>
<li>Origin trace tied to COMSAT-4</li>
<li>Switches across 3 endpoint IPs within 90 seconds</li>
<li>Echoes techniques seen in the 2023 OPFOR sim</li>
<li>Request trace overlays from JMOD and ASD</li>
</ul>`

	sampleMultiLineMarkdown = `### Suspected Lateral Movement – Relay Switching Behavior Observed

We've observed endpoint switching consistent with lateral movement from SATCOM ingress. Traces suggest adversary is exploring multiple exit points across coalition relay nodes.

- Origin trace tied to COMSAT-4
- Switches across 3 endpoint IPs within 90 seconds
- Echoes techniques seen in the 2023 OPFOR sim
- Request trace overlays from JMOD and ASD
`
)

func TestParseDisplayName(t *testing.T) {
	tests := []struct {
		name              string
		displayName       string
		expectedFirstName string
		expectedLastName  string
	}{
		{
			name:              "empty string",
			displayName:       "",
			expectedFirstName: "",
			expectedLastName:  "",
		},
		{
			name:              "whitespace only",
			displayName:       "   \t\n   ",
			expectedFirstName: "",
			expectedLastName:  "",
		},
		{
			name:              "single name",
			displayName:       "John",
			expectedFirstName: "John",
			expectedLastName:  "",
		},
		{
			name:              "single name with whitespace",
			displayName:       "  Alice  ",
			expectedFirstName: "Alice",
			expectedLastName:  "",
		},
		{
			name:              "first and last name",
			displayName:       "John Doe",
			expectedFirstName: "John",
			expectedLastName:  "Doe",
		},
		{
			name:              "first and last name with extra whitespace",
			displayName:       "  John   Doe  ",
			expectedFirstName: "John",
			expectedLastName:  "Doe",
		},
		{
			name:              "three names",
			displayName:       "John Michael Doe",
			expectedFirstName: "John",
			expectedLastName:  "Michael Doe",
		},
		{
			name:              "multiple names",
			displayName:       "Mary Jane Watson Parker",
			expectedFirstName: "Mary",
			expectedLastName:  "Jane Watson Parker",
		},
		{
			name:              "name with prefix/suffix",
			displayName:       "Dr. John von Neumann Jr.",
			expectedFirstName: "Dr.",
			expectedLastName:  "John von Neumann Jr.",
		},
		{
			name:              "unicode characters",
			displayName:       "José María García",
			expectedFirstName: "José",
			expectedLastName:  "María García",
		},
		{
			name:              "single unicode name",
			displayName:       "李小明",
			expectedFirstName: "李小明",
			expectedLastName:  "",
		},
		{
			name:              "names with hyphen",
			displayName:       "Mary-Jane Watson",
			expectedFirstName: "Mary-Jane",
			expectedLastName:  "Watson",
		},
		{
			name:              "names with apostrophe",
			displayName:       "John O'Connor",
			expectedFirstName: "John",
			expectedLastName:  "O'Connor",
		},
		{
			name:              "single character names",
			displayName:       "A B",
			expectedFirstName: "A",
			expectedLastName:  "B",
		},
		{
			name:              "very long name",
			displayName:       "Jean-Baptiste Grenouille de la Montagne",
			expectedFirstName: "Jean-Baptiste",
			expectedLastName:  "Grenouille de la Montagne",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			firstName, lastName := parseDisplayName(tt.displayName)

			if firstName != tt.expectedFirstName {
				t.Errorf("parseDisplayName(%q) firstName = %q, want %q",
					tt.displayName, firstName, tt.expectedFirstName)
			}

			if lastName != tt.expectedLastName {
				t.Errorf("parseDisplayName(%q) lastName = %q, want %q",
					tt.displayName, lastName, tt.expectedLastName)
			}
		})
	}
}

func TestParseDisplayNameEdgeCases(t *testing.T) {
	// Test with tabs and multiple spaces
	firstName, lastName := parseDisplayName("John\t\t   Doe")
	if firstName != "John" || lastName != "Doe" {
		t.Errorf("parseDisplayName with tabs failed: got (%q, %q), want (\"John\", \"Doe\")", firstName, lastName)
	}

	// Test with newlines
	firstName, lastName = parseDisplayName("John\nDoe")
	if firstName != "John" || lastName != "Doe" {
		t.Errorf("parseDisplayName with newlines failed: got (%q, %q), want (\"John\", \"Doe\")", firstName, lastName)
	}

	// Test with mixed whitespace
	firstName, lastName = parseDisplayName(" \t John \n  Michael \r  Doe \t ")
	if firstName != "John" || lastName != "Michael Doe" {
		t.Errorf("parseDisplayName with mixed whitespace failed: got (%q, %q), want (\"John\", \"Michael Doe\")", firstName, lastName)
	}
}

// mockLogger implements the Logger interface for testing
type mockLogger struct {
	logs []string
}

func (m *mockLogger) LogWarn(message string, _ ...any) {
	m.logs = append(m.logs, message)
}

func (m *mockLogger) LogDebug(message string, _ ...any) {
	m.logs = append(m.logs, message)
}

func (m *mockLogger) LogInfo(message string, _ ...any) {
	m.logs = append(m.logs, message)
}

func (m *mockLogger) LogError(message string, _ ...any) {
	m.logs = append(m.logs, message)
}

func TestExtractServerDomain(t *testing.T) {
	tests := []struct {
		name      string
		serverURL string
		expected  string
		shouldLog bool
	}{
		{
			name:      "empty URL",
			serverURL: "",
			expected:  "unknown",
			shouldLog: false,
		},
		{
			name:      "valid HTTPS URL",
			serverURL: "https://matrix.example.com",
			expected:  "matrix_example_com",
			shouldLog: false,
		},
		{
			name:      "valid HTTP URL",
			serverURL: "http://localhost:8008",
			expected:  "localhost",
			shouldLog: false,
		},
		{
			name:      "URL with port",
			serverURL: "https://matrix.example.com:8448",
			expected:  "matrix_example_com",
			shouldLog: false,
		},
		{
			name:      "URL with path",
			serverURL: "https://example.com/_matrix",
			expected:  "example_com",
			shouldLog: false,
		},
		{
			name:      "invalid URL",
			serverURL: "not-a-url",
			expected:  "unknown",
			shouldLog: true,
		},
		{
			name:      "URL with special chars",
			serverURL: "https://sub.domain.example.com",
			expected:  "sub_domain_example_com",
			shouldLog: false,
		},
		{
			name:      "IPv6 URL",
			serverURL: "https://[::1]:8008",
			expected:  "__1", // Colons are replaced with underscores
			shouldLog: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockLogger := &mockLogger{}
			result := extractServerDomain(mockLogger, tt.serverURL)

			if result != tt.expected {
				t.Errorf("extractServerDomain(%q) = %q, want %q", tt.serverURL, result, tt.expected)
			}

			if tt.shouldLog && len(mockLogger.logs) == 0 {
				t.Errorf("extractServerDomain(%q) should have logged a warning", tt.serverURL)
			}

			if !tt.shouldLog && len(mockLogger.logs) > 0 {
				t.Errorf("extractServerDomain(%q) should not have logged warnings, but got: %v", tt.serverURL, mockLogger.logs)
			}
		})
	}
}

func TestCleanupMarkdown(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "no cleanup needed",
			input:    "Hello world",
			expected: "Hello world",
		},
		{
			name:     "remove excessive newlines",
			input:    "Hello\n\n\n\nworld",
			expected: "Hello\n\nworld",
		},
		{
			name:     "trim whitespace",
			input:    "  Hello world  ",
			expected: "Hello world",
		},
		{
			name:     "trim and clean newlines",
			input:    "  \n\nHello\n\n\n\nworld\n\n  ",
			expected: "Hello\n\nworld",
		},
		{
			name:     "preserve double newlines",
			input:    "Hello\n\nworld",
			expected: "Hello\n\nworld",
		},
		{
			name:     "reduce many newlines to double",
			input:    "Hello\n\n\n\n\n\nworld",
			expected: "Hello\n\nworld",
		},
		{
			name:     "handle tabs and spaces",
			input:    "\t  Hello world  \t",
			expected: "Hello world",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := cleanupMarkdown(tt.input)
			if result != tt.expected {
				t.Errorf("cleanupMarkdown(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestConvertHTMLToMarkdown(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "plain text",
			input:    "Hello world",
			expected: "Hello world",
		},
		{
			name:     "bold text",
			input:    "<strong>bold</strong>",
			expected: "**bold**",
		},
		{
			name:     "italic text",
			input:    "<em>italic</em>",
			expected: "*italic*",
		},
		{
			name:     "mixed formatting",
			input:    "<strong>bold</strong> and <em>italic</em>",
			expected: "**bold** and *italic*",
		},
		{
			name:     "code block",
			input:    "<pre><code>console.log('test');</code></pre>",
			expected: "```\nconsole.log('test');\n```",
		},
		{
			name:     "inline code",
			input:    "Use <code>console.log</code> for debugging",
			expected: "Use `console.log` for debugging",
		},
		{
			name:     "links",
			input:    "<a href=\"https://example.com\">Example</a>",
			expected: "[Example](https://example.com)",
		},
		{
			name:     "headers",
			input:    "<h1>Title</h1><h2>Subtitle</h2>",
			expected: "# Title\n\n## Subtitle",
		},
		{
			name:     "unordered list",
			input:    "<ul><li>Item 1</li><li>Item 2</li></ul>",
			expected: "- Item 1\n- Item 2",
		},
		{
			name:     "paragraphs",
			input:    "<p>First paragraph</p><p>Second paragraph</p>",
			expected: "First paragraph\n\nSecond paragraph",
		},
		{
			name:     "line breaks",
			input:    "Line 1<br>Line 2<br/>Line 3",
			expected: "Line 1\n\nLine 2\n\nLine 3", // HTML-to-markdown adds double newlines for br tags
		},
		{
			name:     "multi-line formatted",
			input:    sampleMultiLineHTML,
			expected: sampleMultiLineMarkdown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockLogger := &mockLogger{}
			result := convertHTMLToMarkdown(mockLogger, tt.input)

			// Normalize whitespace for comparison
			result = strings.TrimSpace(result)
			expected := strings.TrimSpace(tt.expected)

			require.Equal(t, expected, result)
		})
	}
}

func TestConvertHTMLToMarkdownError(t *testing.T) {
	// Test error handling - the html-to-markdown library is quite resilient,
	// so we just test that it doesn't panic and produces some output
	mockLogger := &mockLogger{}
	input := "<invalid><unclosed>tags"
	result := convertHTMLToMarkdown(mockLogger, input)

	// Should produce some output (library handles malformed HTML gracefully)
	if result == "" {
		t.Errorf("convertHTMLToMarkdown should produce some output, got empty string")
	}

	// Library handles malformed HTML without errors, so no warning expected
	if len(mockLogger.logs) > 0 {
		t.Errorf("convertHTMLToMarkdown should not log warnings for this input, but got: %v", mockLogger.logs)
	}
}

func TestExtractMattermostUserIDFromGhost(t *testing.T) {
	tests := []struct {
		name        string
		ghostUserID string
		expected    string
		setup       func(*mocks.MockAPI)
	}{
		{
			name:        "valid ghost user ID",
			ghostUserID: "@_mattermost_yeqo3irkujdstfmbnkx46bbhuw:synapse-mydomain.com",
			expected:    "yeqo3irkujdstfmbnkx46bbhuw",
			setup: func(_ *mocks.MockAPI) {
			},
		},
		{
			name:        "another valid ghost user ID",
			ghostUserID: "@_mattermost_user123:matrix.example.com",
			expected:    "user123",
			setup: func(_ *mocks.MockAPI) {
			},
		},
		{
			name:        "not a ghost user - regular Matrix user",
			ghostUserID: "@alice:example.com",
			expected:    "",
			setup: func(_ *mocks.MockAPI) {
				// No expectations needed
			},
		},
		{
			name:        "not a ghost user - wrong prefix",
			ghostUserID: "@wrong_prefix_user123:example.com",
			expected:    "",
			setup: func(_ *mocks.MockAPI) {
				// No expectations needed
			},
		},
		{
			name:        "malformed ghost user - no colon",
			ghostUserID: "@_mattermost_user123",
			expected:    "",
			setup: func(_ *mocks.MockAPI) {
				// No expectations needed
			},
		},
		{
			name:        "malformed ghost user - empty user ID",
			ghostUserID: "@_mattermost_:example.com",
			expected:    "",
			setup: func(_ *mocks.MockAPI) {
				// No expectations needed
			},
		},
		{
			name:        "ghost user with complex server domain",
			ghostUserID: "@_mattermost_abc123:matrix.subdomain.example.com:8448",
			expected:    "abc123",
			setup: func(_ *mocks.MockAPI) {
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			mockAPI := mocks.NewMockAPI(ctrl)

			tt.setup(mockAPI)

			plugin := setupPluginForTestWithLogger(t, mockAPI)

			result := plugin.extractMattermostUserIDFromGhost(tt.ghostUserID)

			if result != tt.expected {
				t.Errorf("extractMattermostUserIDFromGhost() = %q, want %q", result, tt.expected)
			}
		})
	}
}
