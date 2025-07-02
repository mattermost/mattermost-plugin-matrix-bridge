package main

import (
	"strings"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/mocks"
	"github.com/mattermost/mattermost/server/public/model"
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockLogger := &mockLogger{}
			result := convertHTMLToMarkdown(mockLogger, tt.input)

			// Normalize whitespace for comparison
			result = strings.TrimSpace(result)
			expected := strings.TrimSpace(tt.expected)

			if result != expected {
				t.Errorf("convertHTMLToMarkdown(%q) = %q, want %q", tt.input, result, expected)
			}
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

func TestExtractMentionedUsers(t *testing.T) {
	tests := []struct {
		name     string
		event    MatrixEvent
		expected []string
	}{
		{
			name: "no mentions field",
			event: MatrixEvent{
				EventID: "event1",
				Content: map[string]any{},
			},
			expected: nil,
		},
		{
			name: "empty mentions",
			event: MatrixEvent{
				EventID: "event2",
				Content: map[string]any{
					"m.mentions": map[string]any{},
				},
			},
			expected: nil,
		},
		{
			name: "single mention",
			event: MatrixEvent{
				EventID: "event3",
				Content: map[string]any{
					"m.mentions": map[string]any{
						"user_ids": []any{"@alice:example.com"},
					},
				},
			},
			expected: []string{"@alice:example.com"},
		},
		{
			name: "multiple mentions",
			event: MatrixEvent{
				EventID: "event4",
				Content: map[string]any{
					"m.mentions": map[string]any{
						"user_ids": []any{"@alice:example.com", "@bob:example.com"},
					},
				},
			},
			expected: []string{"@alice:example.com", "@bob:example.com"},
		},
		{
			name: "invalid mentions format",
			event: MatrixEvent{
				EventID: "event5",
				Content: map[string]any{
					"m.mentions": "invalid",
				},
			},
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a minimal plugin instance for testing
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			mockAPI := mocks.NewMockAPI(ctrl)

			plugin := setupPluginForTestWithLogger(t, mockAPI)

			result := plugin.extractMentionedUsers(tt.event)

			if len(result) != len(tt.expected) {
				t.Errorf("extractMentionedUsers() returned %d users, want %d", len(result), len(tt.expected))
				return
			}

			for i, expected := range tt.expected {
				if result[i] != expected {
					t.Errorf("extractMentionedUsers()[%d] = %q, want %q", i, result[i], expected)
				}
			}
		})
	}
}

func TestReplaceMatrixMentionHTML(t *testing.T) {
	tests := []struct {
		name               string
		htmlContent        string
		matrixUserID       string
		mattermostUsername string
		expected           string
		setup              func(*mocks.MockAPI)
	}{
		{
			name:               "simple mention",
			htmlContent:        `Hello <a href="https://matrix.to/#/@alice:example.com">Alice</a>!`,
			matrixUserID:       "@alice:example.com",
			mattermostUsername: "alice",
			expected:           "Hello @alice!",
		},
		{
			name:               "mention with display name",
			htmlContent:        `Hey <a href="https://matrix.to/#/@bob:server.org">Bob Smith</a>, how are you?`,
			matrixUserID:       "@bob:server.org",
			mattermostUsername: "bobsmith",
			expected:           "Hey @bobsmith, how are you?",
		},
		{
			name:               "multiple mentions of same user",
			htmlContent:        `<a href="https://matrix.to/#/@alice:example.com">Alice</a> and <a href="https://matrix.to/#/@alice:example.com">Alice again</a>`,
			matrixUserID:       "@alice:example.com",
			mattermostUsername: "alice",
			expected:           "@alice and @alice",
		},
		{
			name:               "no mention links",
			htmlContent:        "Just plain text",
			matrixUserID:       "@alice:example.com",
			mattermostUsername: "alice",
			expected:           "Just plain text",
		},
		{
			name:               "mention with different formatting",
			htmlContent:        `<a href='https://matrix.to/#/@user:example.com' class="mention">User Name</a>`,
			matrixUserID:       "@user:example.com",
			mattermostUsername: "username",
			expected:           "@username",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a minimal plugin instance for testing
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			mockAPI := mocks.NewMockAPI(ctrl)

			plugin := setupPluginForTestWithLogger(t, mockAPI)

			result := plugin.replaceMatrixMentionHTML(tt.htmlContent, tt.matrixUserID, tt.mattermostUsername)

			if result != tt.expected {
				t.Errorf("replaceMatrixMentionHTML() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestGetMattermostUsernameFromMatrix(t *testing.T) {
	tests := []struct {
		name         string
		matrixUserID string
		expected     string
		setup        func(*mocks.MockAPI, *mocks.MockKVStore)
	}{
		{
			name:         "user not found in kvstore",
			matrixUserID: "@alice:example.com",
			expected:     "",
			setup: func(_ *mocks.MockAPI, mockKV *mocks.MockKVStore) {
				mockKV.EXPECT().Get("matrix_user_@alice:example.com").Return(nil, &model.AppError{Message: "Not found"})
			},
		},
		{
			name:         "user found but Mattermost user doesn't exist",
			matrixUserID: "@bob:example.com",
			expected:     "",
			setup: func(mockAPI *mocks.MockAPI, mockKV *mocks.MockKVStore) {
				mockKV.EXPECT().Get("matrix_user_@bob:example.com").Return([]byte("user123"), nil)
				mockAPI.EXPECT().GetUser("user123").Return(nil, &model.AppError{Message: "User not found"})
			},
		},
		{
			name:         "user found successfully",
			matrixUserID: "@charlie:example.com",
			expected:     "charlie_mm",
			setup: func(mockAPI *mocks.MockAPI, mockKV *mocks.MockKVStore) {
				mockKV.EXPECT().Get("matrix_user_@charlie:example.com").Return([]byte("user456"), nil)
				user := &model.User{
					Id:       "user456",
					Username: "charlie_mm",
				}
				mockAPI.EXPECT().GetUser("user456").Return(user, nil)
			},
		},
		{
			name:         "ghost user found successfully",
			matrixUserID: "@_mattermost_yeqo3irkujdstfmbnkx46bbhuw:synapse-wiggin77.ngrok.io",
			expected:     "doug_lauder",
			setup: func(mockAPI *mocks.MockAPI, _ *mocks.MockKVStore) {
				user := &model.User{
					Id:       "yeqo3irkujdstfmbnkx46bbhuw",
					Username: "doug_lauder",
				}
				mockAPI.EXPECT().GetUser("yeqo3irkujdstfmbnkx46bbhuw").Return(user, nil)
			},
		},
		{
			name:         "ghost user but Mattermost user doesn't exist",
			matrixUserID: "@_mattermost_nonexistentuser:example.com",
			expected:     "",
			setup: func(mockAPI *mocks.MockAPI, _ *mocks.MockKVStore) {
				mockAPI.EXPECT().GetUser("nonexistentuser").Return(nil, &model.AppError{Message: "User not found"})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create plugin instance with mocks
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			mockAPI := mocks.NewMockAPI(ctrl)
			mockKV := mocks.NewMockKVStore(ctrl)

			// Set up test-specific expectations
			tt.setup(mockAPI, mockKV)

			plugin := setupPluginForTestWithKVStore(t, mockAPI, mockKV)

			result := plugin.getMattermostUsernameFromMatrix(tt.matrixUserID)

			if result != tt.expected {
				t.Errorf("getMattermostUsernameFromMatrix() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestProcessMatrixMentions(t *testing.T) {
	tests := []struct {
		name        string
		htmlContent string
		event       MatrixEvent
		expected    string
		setup       func(*mocks.MockAPI, *mocks.MockKVStore)
	}{
		{
			name:        "no mentions in event",
			htmlContent: `Hello <a href="https://matrix.to/#/@alice:example.com">Alice</a>!`,
			event: MatrixEvent{
				EventID: "event1",
				Content: map[string]any{},
			},
			expected: `Hello <a href="https://matrix.to/#/@alice:example.com">Alice</a>!`,
			setup: func(_ *mocks.MockAPI, _ *mocks.MockKVStore) {
				// No expectations needed
			},
		},
		{
			name:        "single mention found and replaced",
			htmlContent: `Hello <a href="https://matrix.to/#/@alice:example.com">Alice</a>!`,
			event: MatrixEvent{
				EventID: "event2",
				Content: map[string]any{
					"m.mentions": map[string]any{
						"user_ids": []any{"@alice:example.com"},
					},
				},
			},
			expected: "Hello @alice_mm!",
			setup: func(mockAPI *mocks.MockAPI, mockKV *mocks.MockKVStore) {
				mockKV.EXPECT().Get("matrix_user_@alice:example.com").Return([]byte("user123"), nil)
				user := &model.User{
					Id:       "user123",
					Username: "alice_mm",
				}
				mockAPI.EXPECT().GetUser("user123").Return(user, nil)
			},
		},
		{
			name:        "multiple mentions processed",
			htmlContent: `<a href="https://matrix.to/#/@alice:example.com">Alice</a> and <a href="https://matrix.to/#/@bob:example.com">Bob</a>`,
			event: MatrixEvent{
				EventID: "event3",
				Content: map[string]any{
					"m.mentions": map[string]any{
						"user_ids": []any{"@alice:example.com", "@bob:example.com"},
					},
				},
			},
			expected: "@alice_mm and @bob_mm",
			setup: func(mockAPI *mocks.MockAPI, mockKV *mocks.MockKVStore) {
				// Setup for Alice
				mockKV.EXPECT().Get("matrix_user_@alice:example.com").Return([]byte("user123"), nil)
				aliceUser := &model.User{
					Id:       "user123",
					Username: "alice_mm",
				}
				mockAPI.EXPECT().GetUser("user123").Return(aliceUser, nil)

				// Setup for Bob
				mockKV.EXPECT().Get("matrix_user_@bob:example.com").Return([]byte("user456"), nil)
				bobUser := &model.User{
					Id:       "user456",
					Username: "bob_mm",
				}
				mockAPI.EXPECT().GetUser("user456").Return(bobUser, nil)
			},
		},
		{
			name:        "mention with no Mattermost mapping",
			htmlContent: `Hello <a href="https://matrix.to/#/@unknown:example.com">Unknown</a>!`,
			event: MatrixEvent{
				EventID: "event4",
				Content: map[string]any{
					"m.mentions": map[string]any{
						"user_ids": []any{"@unknown:example.com"},
					},
				},
			},
			expected: `Hello <a href="https://matrix.to/#/@unknown:example.com">Unknown</a>!`,
			setup: func(_ *mocks.MockAPI, mockKV *mocks.MockKVStore) {
				mockKV.EXPECT().Get("matrix_user_@unknown:example.com").Return(nil, &model.AppError{Message: "Not found"})
			},
		},
		{
			name:        "mixed content with mentions and regular links",
			htmlContent: `Check out <a href="https://example.com">this link</a> and <a href="https://matrix.to/#/@alice:example.com">Alice</a>`,
			event: MatrixEvent{
				EventID: "event5",
				Content: map[string]any{
					"m.mentions": map[string]any{
						"user_ids": []any{"@alice:example.com"},
					},
				},
			},
			expected: `Check out <a href="https://example.com">this link</a> and @alice_mm`,
			setup: func(mockAPI *mocks.MockAPI, mockKV *mocks.MockKVStore) {
				mockKV.EXPECT().Get("matrix_user_@alice:example.com").Return([]byte("user123"), nil)
				user := &model.User{
					Id:       "user123",
					Username: "alice_mm",
				}
				mockAPI.EXPECT().GetUser("user123").Return(user, nil)
			},
		},
		{
			name:        "ghost user mention from real example",
			htmlContent: `This is a test mention for <a href="https://matrix.to/#/@_mattermost_yeqo3irkujdstfmbnkx46bbhuw:synapse-wiggin77.ngrok.io">Doug Lauder</a> hope it works`,
			event: MatrixEvent{
				EventID: "event6",
				Content: map[string]any{
					"m.mentions": map[string]any{
						"user_ids": []any{"@_mattermost_yeqo3irkujdstfmbnkx46bbhuw:synapse-wiggin77.ngrok.io"},
					},
				},
			},
			expected: `This is a test mention for @doug_lauder hope it works`,
			setup: func(mockAPI *mocks.MockAPI, _ *mocks.MockKVStore) {
				user := &model.User{
					Id:       "yeqo3irkujdstfmbnkx46bbhuw",
					Username: "doug_lauder",
				}
				mockAPI.EXPECT().GetUser("yeqo3irkujdstfmbnkx46bbhuw").Return(user, nil)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			mockAPI := mocks.NewMockAPI(ctrl)
			mockKV := mocks.NewMockKVStore(ctrl)

			tt.setup(mockAPI, mockKV)

			plugin := setupPluginForTestWithKVStore(t, mockAPI, mockKV)

			result := plugin.processMatrixMentions(tt.htmlContent, tt.event)

			if result != tt.expected {
				t.Errorf("processMatrixMentions() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestConvertHTMLToMarkdownWithMentions(t *testing.T) {
	tests := []struct {
		name        string
		htmlContent string
		event       MatrixEvent
		expected    string
		setup       func(*mocks.MockAPI, *mocks.MockKVStore)
	}{
		{
			name:        "plain HTML without mentions",
			htmlContent: "<strong>Hello</strong> <em>world</em>",
			event: MatrixEvent{
				EventID: "event1",
				Content: map[string]any{},
			},
			expected: "**Hello** *world*",
			setup: func(_ *mocks.MockAPI, _ *mocks.MockKVStore) {
				// No expectations needed
			},
		},
		{
			name:        "HTML with mentions converted to markdown",
			htmlContent: `<p>Hello <a href="https://matrix.to/#/@alice:example.com">Alice</a>, how are you?</p>`,
			event: MatrixEvent{
				EventID: "event2",
				Content: map[string]any{
					"m.mentions": map[string]any{
						"user_ids": []any{"@alice:example.com"},
					},
				},
			},
			expected: "Hello @alice\\_mm, how are you?",
			setup: func(mockAPI *mocks.MockAPI, mockKV *mocks.MockKVStore) {
				mockKV.EXPECT().Get("matrix_user_@alice:example.com").Return([]byte("user123"), nil)
				user := &model.User{
					Id:       "user123",
					Username: "alice_mm",
				}
				mockAPI.EXPECT().GetUser("user123").Return(user, nil)
			},
		},
		{
			name:        "complex HTML with mentions and formatting",
			htmlContent: `<p><strong>Important:</strong> <a href="https://matrix.to/#/@alice:example.com">Alice</a> needs to see this <em>urgent</em> message!</p>`,
			event: MatrixEvent{
				EventID: "event3",
				Content: map[string]any{
					"m.mentions": map[string]any{
						"user_ids": []any{"@alice:example.com"},
					},
				},
			},
			expected: "**Important:** @alice\\_mm needs to see this *urgent* message!",
			setup: func(mockAPI *mocks.MockAPI, mockKV *mocks.MockKVStore) {
				mockKV.EXPECT().Get("matrix_user_@alice:example.com").Return([]byte("user123"), nil)
				user := &model.User{
					Id:       "user123",
					Username: "alice_mm",
				}
				mockAPI.EXPECT().GetUser("user123").Return(user, nil)
			},
		},
		{
			name:        "mentions with no Mattermost mapping remain as HTML",
			htmlContent: `<p>Hello <a href="https://matrix.to/#/@unknown:example.com">Unknown User</a></p>`,
			event: MatrixEvent{
				EventID: "event4",
				Content: map[string]any{
					"m.mentions": map[string]any{
						"user_ids": []any{"@unknown:example.com"},
					},
				},
			},
			expected: "Hello [Unknown User](https://matrix.to/#/@unknown:example.com)",
			setup: func(_ *mocks.MockAPI, mockKV *mocks.MockKVStore) {
				mockKV.EXPECT().Get("matrix_user_@unknown:example.com").Return(nil, &model.AppError{Message: "Not found"})
			},
		},
		{
			name:        "multiple mentions with mixed formatting",
			htmlContent: `<ul><li><a href="https://matrix.to/#/@alice:example.com">Alice</a> - <strong>Team Lead</strong></li><li><a href="https://matrix.to/#/@bob:example.com">Bob</a> - <em>Developer</em></li></ul>`,
			event: MatrixEvent{
				EventID: "event5",
				Content: map[string]any{
					"m.mentions": map[string]any{
						"user_ids": []any{"@alice:example.com", "@bob:example.com"},
					},
				},
			},
			expected: "- @alice\\_mm - **Team Lead**\n- @bob\\_mm - *Developer*",
			setup: func(mockAPI *mocks.MockAPI, mockKV *mocks.MockKVStore) {
				// Setup for Alice
				mockKV.EXPECT().Get("matrix_user_@alice:example.com").Return([]byte("user123"), nil)
				aliceUser := &model.User{
					Id:       "user123",
					Username: "alice_mm",
				}
				mockAPI.EXPECT().GetUser("user123").Return(aliceUser, nil)

				// Setup for Bob
				mockKV.EXPECT().Get("matrix_user_@bob:example.com").Return([]byte("user456"), nil)
				bobUser := &model.User{
					Id:       "user456",
					Username: "bob_mm",
				}
				mockAPI.EXPECT().GetUser("user456").Return(bobUser, nil)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			mockAPI := mocks.NewMockAPI(ctrl)
			mockKV := mocks.NewMockKVStore(ctrl)

			tt.setup(mockAPI, mockKV)

			plugin := setupPluginForTestWithKVStore(t, mockAPI, mockKV)

			result := plugin.convertHTMLToMarkdownWithMentions(tt.htmlContent, tt.event)

			// Normalize whitespace for comparison
			result = strings.TrimSpace(result)
			expected := strings.TrimSpace(tt.expected)

			if result != expected {
				t.Errorf("convertHTMLToMarkdownWithMentions() = %q, want %q", result, expected)
			}
		})
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
			ghostUserID: "@_mattermost_yeqo3irkujdstfmbnkx46bbhuw:synapse-wiggin77.ngrok.io",
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
