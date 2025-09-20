package main

import (
	"testing"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/matrix"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

func TestExtractMatrixMessageContent(t *testing.T) {
	// Create test BridgeUtils instance
	api := &plugintest.API{}
	api.On("LogDebug", mock.Anything, mock.Anything).Maybe()
	api.On("LogInfo", mock.Anything, mock.Anything).Maybe()
	api.On("LogWarn", mock.Anything, mock.Anything).Maybe()
	api.On("LogError", mock.Anything, mock.Anything).Maybe()

	logger := &testLogger{t: t}
	kvstore := NewMemoryKVStore()
	matrixClient := matrix.NewClientWithLoggerAndRateLimit("https://test.example.com", "test_token", "test_remote", matrix.NewTestLogger(t), matrix.UnitTestRateLimitConfig())

	config := BridgeUtilsConfig{
		Logger:       logger,
		API:          api,
		KVStore:      kvstore,
		MatrixClient: matrixClient,
		RemoteID:     "test-remote",
	}

	bridgeUtils := NewBridgeUtils(config)

	tests := []struct {
		name     string
		event    MatrixEvent
		expected string
	}{
		{
			name: "message with only body",
			event: MatrixEvent{
				Content: map[string]any{
					"body": "Hello world",
				},
			},
			expected: "Hello world",
		},
		{
			name: "message with body and identical formatted_body",
			event: MatrixEvent{
				Content: map[string]any{
					"body":           "Hello world",
					"formatted_body": "Hello world",
				},
			},
			expected: "Hello world",
		},
		{
			name: "message with body and different formatted_body (HTML)",
			event: MatrixEvent{
				Content: map[string]any{
					"body":           "Hello world",
					"formatted_body": "Hello <strong>world</strong>",
					"format":         "org.matrix.custom.html",
				},
			},
			expected: "Hello **world**",
		},
		{
			name: "message with body and different formatted_body (no format field)",
			event: MatrixEvent{
				Content: map[string]any{
					"body":           "Hello world",
					"formatted_body": "Hello <em>world</em>",
				},
			},
			expected: "Hello *world*",
		},
		{
			name: "message with only formatted_body",
			event: MatrixEvent{
				Content: map[string]any{
					"formatted_body": "Hello <strong>world</strong>",
					"format":         "org.matrix.custom.html",
				},
			},
			expected: "Hello **world**",
		},
		{
			name: "message with empty content",
			event: MatrixEvent{
				Content: map[string]any{},
			},
			expected: "",
		},
		{
			name: "message with nil content",
			event: MatrixEvent{
				Content: nil,
			},
			expected: "",
		},
		{
			name: "message with non-string body",
			event: MatrixEvent{
				Content: map[string]any{
					"body": 123,
				},
			},
			expected: "",
		},
		{
			name: "message with non-string formatted_body",
			event: MatrixEvent{
				Content: map[string]any{
					"body":           "Hello world",
					"formatted_body": 123,
				},
			},
			expected: "Hello world",
		},
		{
			name: "message with empty body and formatted_body",
			event: MatrixEvent{
				Content: map[string]any{
					"body":           "",
					"formatted_body": "",
				},
			},
			expected: "",
		},
		{
			name: "HTML content without format field",
			event: MatrixEvent{
				Content: map[string]any{
					"body":           "Plain text",
					"formatted_body": "HTML with &lt;tags&gt; and entities &amp; stuff",
				},
			},
			expected: "HTML with <tags> and entities & stuff",
		},
		{
			name: "complex HTML formatting",
			event: MatrixEvent{
				Content: map[string]any{
					"body":           "Complex message",
					"formatted_body": "<p>This is <strong>bold</strong> and <em>italic</em> text with a <a href=\"https://example.com\">link</a></p>",
					"format":         "org.matrix.custom.html",
				},
			},
			expected: "This is **bold** and *italic* text with a [link](https://example.com)",
		},
		{
			name: "edit event with plain text",
			event: MatrixEvent{
				Content: map[string]any{
					"body":    " * Edited message", // fallback body has /* prefix
					"msgtype": "m.text",
					"m.new_content": map[string]any{
						"msgtype": "m.text",
						"body":    "Edited message", // actual content without /*
					},
					"m.relates_to": map[string]any{
						"rel_type": "m.replace",
						"event_id": "$original_event_id:matrix.org",
					},
				},
			},
			expected: "Edited message",
		},
		{
			name: "edit event with HTML formatting",
			event: MatrixEvent{
				Content: map[string]any{
					"body":    " * Edited message", // fallback body
					"msgtype": "m.text",
					"m.new_content": map[string]any{
						"msgtype":        "m.text",
						"body":           "Edited message",
						"formatted_body": "<p>Edited <strong>message</strong> with <em>formatting</em></p>",
						"format":         "org.matrix.custom.html",
					},
					"m.relates_to": map[string]any{
						"rel_type": "m.replace",
						"event_id": "$original_event_id:matrix.org",
					},
				},
			},
			expected: "Edited **message** with *formatting*",
		},
		{
			name: "edit event without m.new_content (malformed)",
			event: MatrixEvent{
				Content: map[string]any{
					"body":    " * Edited message",
					"msgtype": "m.text",
					"m.relates_to": map[string]any{
						"rel_type": "m.replace",
						"event_id": "$original_event_id:matrix.org",
					},
				},
			},
			expected: " * Edited message", // Falls back to top-level body
		},
		{
			name: "edit event with empty m.new_content",
			event: MatrixEvent{
				Content: map[string]any{
					"body":          " * ",
					"msgtype":       "m.text",
					"m.new_content": map[string]any{},
					"m.relates_to": map[string]any{
						"rel_type": "m.replace",
						"event_id": "$original_event_id:matrix.org",
					},
				},
			},
			expected: "", // Empty content from m.new_content
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := bridgeUtils.extractMatrixMessageContent(tt.event)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestIsHTMLContent(t *testing.T) {
	// Create test BridgeUtils instance
	api := &plugintest.API{}
	api.On("LogDebug", mock.Anything, mock.Anything).Maybe()

	logger := &testLogger{t: t}
	kvstore := NewMemoryKVStore()
	matrixClient := matrix.NewClientWithLoggerAndRateLimit("https://test.example.com", "test_token", "test_remote", matrix.NewTestLogger(t), matrix.UnitTestRateLimitConfig())

	config := BridgeUtilsConfig{
		Logger:       logger,
		API:          api,
		KVStore:      kvstore,
		MatrixClient: matrixClient,
		RemoteID:     "test-remote",
	}

	bridgeUtils := NewBridgeUtils(config)

	tests := []struct {
		name     string
		content  string
		event    MatrixEvent
		expected bool
	}{
		{
			name:     "explicit HTML format",
			content:  "Hello world",
			event:    MatrixEvent{Content: map[string]any{"format": "org.matrix.custom.html"}},
			expected: true,
		},
		{
			name:     "non-HTML format",
			content:  "Hello world",
			event:    MatrixEvent{Content: map[string]any{"format": "plain"}},
			expected: false,
		},
		{
			name:     "no format field with HTML tags",
			content:  "Hello <strong>world</strong>",
			event:    MatrixEvent{Content: map[string]any{}},
			expected: true,
		},
		{
			name:     "no format field with HTML entities",
			content:  "Hello &amp; world",
			event:    MatrixEvent{Content: map[string]any{}},
			expected: true,
		},
		{
			name:     "no format field with plain text",
			content:  "Hello world",
			event:    MatrixEvent{Content: map[string]any{}},
			expected: false,
		},
		{
			name:     "empty content",
			content:  "",
			event:    MatrixEvent{Content: map[string]any{}},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := bridgeUtils.isHTMLContent(tt.content, tt.event)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestIsHTML(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		expected bool
	}{
		{
			name:     "HTML with tags",
			content:  "Hello <strong>world</strong>",
			expected: true,
		},
		{
			name:     "HTML with self-closing tags",
			content:  "Line break<br/>here",
			expected: true,
		},
		{
			name:     "HTML with entities",
			content:  "Hello &amp; world",
			expected: true,
		},
		{
			name:     "HTML with numeric entities",
			content:  "Hello &#39; world",
			expected: true,
		},
		{
			name:     "plain text",
			content:  "Hello world",
			expected: false,
		},
		{
			name:     "text with angle brackets but not HTML",
			content:  "2 < 3 and 5 > 4",
			expected: false,
		},
		{
			name:     "text with ampersand but not HTML entity",
			content:  "Tom & Jerry",
			expected: false,
		},
		{
			name:     "empty string",
			content:  "",
			expected: false,
		},
		{
			name:     "complex HTML",
			content:  "<div class=\"test\">Hello &quot;world&quot;</div>",
			expected: true,
		},
		{
			name:     "invalid HTML with special characters in attributes",
			content:  "<div weird!@#$%^&*()stuff>",
			expected: false,
		},
		{
			name:     "invalid HTML with numeric attribute name",
			content:  "<tag 123invalid=value>",
			expected: false,
		},
		{
			name:     "valid data attribute",
			content:  "<span data-test=\"value\">",
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isHTML(tt.content)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestExtractMattermostMetadata(t *testing.T) {
	// Create test BridgeUtils instance
	api := &plugintest.API{}
	logger := &testLogger{t: t}
	kvstore := NewMemoryKVStore()
	matrixClient := matrix.NewClientWithLoggerAndRateLimit("https://test.example.com", "test_token", "test_remote", matrix.NewTestLogger(t), matrix.UnitTestRateLimitConfig())

	config := BridgeUtilsConfig{
		Logger:       logger,
		API:          api,
		KVStore:      kvstore,
		MatrixClient: matrixClient,
		RemoteID:     "test-remote",
	}

	bridgeUtils := NewBridgeUtils(config)

	tests := []struct {
		name             string
		event            MatrixEvent
		expectedPostID   string
		expectedRemoteID string
	}{
		{
			name: "event with both metadata fields",
			event: MatrixEvent{
				Content: map[string]any{
					"mattermost_post_id":   "post123",
					"mattermost_remote_id": "remote456",
				},
			},
			expectedPostID:   "post123",
			expectedRemoteID: "remote456",
		},
		{
			name: "event with only post ID",
			event: MatrixEvent{
				Content: map[string]any{
					"mattermost_post_id": "post123",
				},
			},
			expectedPostID:   "post123",
			expectedRemoteID: "",
		},
		{
			name: "event with only remote ID",
			event: MatrixEvent{
				Content: map[string]any{
					"mattermost_remote_id": "remote456",
				},
			},
			expectedPostID:   "",
			expectedRemoteID: "remote456",
		},
		{
			name: "event with no metadata",
			event: MatrixEvent{
				Content: map[string]any{
					"body": "Hello world",
				},
			},
			expectedPostID:   "",
			expectedRemoteID: "",
		},
		{
			name: "event with nil content",
			event: MatrixEvent{
				Content: nil,
			},
			expectedPostID:   "",
			expectedRemoteID: "",
		},
		{
			name: "event with non-string metadata",
			event: MatrixEvent{
				Content: map[string]any{
					"mattermost_post_id":   123,
					"mattermost_remote_id": 456,
				},
			},
			expectedPostID:   "",
			expectedRemoteID: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			postID, remoteID := bridgeUtils.extractMattermostMetadata(tt.event)
			assert.Equal(t, tt.expectedPostID, postID)
			assert.Equal(t, tt.expectedRemoteID, remoteID)
		})
	}
}

func TestIsGhostUser(t *testing.T) {
	// Create test BridgeUtils instance
	api := &plugintest.API{}
	logger := &testLogger{t: t}
	kvstore := NewMemoryKVStore()
	matrixClient := matrix.NewClientWithLoggerAndRateLimit("https://test.example.com", "test_token", "test_remote", matrix.NewTestLogger(t), matrix.UnitTestRateLimitConfig())

	config := BridgeUtilsConfig{
		Logger:       logger,
		API:          api,
		KVStore:      kvstore,
		MatrixClient: matrixClient,
		RemoteID:     "test-remote",
	}

	bridgeUtils := NewBridgeUtils(config)

	tests := []struct {
		name         string
		matrixUserID string
		expected     bool
	}{
		{
			name:         "valid ghost user",
			matrixUserID: "@_mattermost_user123:example.com",
			expected:     true,
		},
		{
			name:         "regular user",
			matrixUserID: "@alice:example.com",
			expected:     false,
		},
		{
			name:         "user with mattermost in name but not ghost",
			matrixUserID: "@mattermost_fan:example.com",
			expected:     false,
		},
		{
			name:         "empty string",
			matrixUserID: "",
			expected:     false,
		},
		{
			name:         "partial ghost user prefix without trailing underscore",
			matrixUserID: "@_mattermost",
			expected:     false, // Missing trailing underscore
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := bridgeUtils.isGhostUser(tt.matrixUserID)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestExtractMentionedUsers(t *testing.T) {
	// Create test BridgeUtils instance
	api := &plugintest.API{}
	logger := &testLogger{t: t}
	kvstore := NewMemoryKVStore()
	matrixClient := matrix.NewClientWithLoggerAndRateLimit("https://test.example.com", "test_token", "test_remote", matrix.NewTestLogger(t), matrix.UnitTestRateLimitConfig())

	config := BridgeUtilsConfig{
		Logger:       logger,
		API:          api,
		KVStore:      kvstore,
		MatrixClient: matrixClient,
		RemoteID:     "test-remote",
	}

	bridgeUtils := NewBridgeUtils(config)

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
			result := bridgeUtils.extractMentionedUsers(tt.event)

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
	// Create test BridgeUtils instance
	api := &plugintest.API{}
	logger := &testLogger{t: t}
	kvstore := NewMemoryKVStore()
	matrixClient := matrix.NewClientWithLoggerAndRateLimit("https://test.example.com", "test_token", "test_remote", matrix.NewTestLogger(t), matrix.UnitTestRateLimitConfig())

	config := BridgeUtilsConfig{
		Logger:       logger,
		API:          api,
		KVStore:      kvstore,
		MatrixClient: matrixClient,
		RemoteID:     "test-remote",
	}

	bridgeUtils := NewBridgeUtils(config)

	tests := []struct {
		name               string
		htmlContent        string
		matrixUserID       string
		mattermostUsername string
		expected           string
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
			result := bridgeUtils.replaceMatrixMentionHTML(tt.htmlContent, tt.matrixUserID, tt.mattermostUsername)

			if result != tt.expected {
				t.Errorf("replaceMatrixMentionHTML() = %q, want %q", result, tt.expected)
			}
		})
	}
}
