package main

import (
	"testing"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/matrix"
	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
	"github.com/mattermost/mattermost/server/public/pluginapi"
	"github.com/stretchr/testify/assert"
)

func TestCompareTextContent(t *testing.T) {
	plugin := setupPluginForTest()
	// Set up required fields for bridge initialization
	plugin.maxProfileImageSize = DefaultMaxProfileImageSize
	plugin.maxFileSize = DefaultMaxFileSize
	plugin.postTracker = NewPostTracker(DefaultPostTrackerMaxEntries)
	plugin.pendingFiles = NewPendingFileTracker()
	plugin.client = pluginapi.NewClient(plugin.API, nil)
	plugin.kvstore = kvstore.NewKVStore(plugin.client)
	plugin.matrixClient = createMatrixClientWithTestLogger(t, "", "", "")
	// Initialize bridges for testing
	plugin.initBridges()

	tests := []struct {
		name           string
		currentEvent   map[string]any
		newPlainText   string
		newHTMLContent string
		expected       bool
		description    string
	}{
		{
			name: "identical text content, no HTML",
			currentEvent: map[string]any{
				"content": map[string]any{
					"msgtype": "m.text",
					"body":    "Hello world",
				},
			},
			newPlainText:   "Hello world",
			newHTMLContent: "",
			expected:       true,
			description:    "Should return true when text content is identical",
		},
		{
			name: "different text content",
			currentEvent: map[string]any{
				"content": map[string]any{
					"msgtype": "m.text",
					"body":    "Hello world",
				},
			},
			newPlainText:   "Hello universe",
			newHTMLContent: "",
			expected:       false,
			description:    "Should return false when text content differs",
		},
		{
			name: "identical text and HTML content",
			currentEvent: map[string]any{
				"content": map[string]any{
					"msgtype":        "m.text",
					"body":           "Hello world",
					"format":         "org.matrix.custom.html",
					"formatted_body": "<b>Hello world</b>",
				},
			},
			newPlainText:   "Hello world",
			newHTMLContent: "<b>Hello world</b>",
			expected:       true,
			description:    "Should return true when both text and HTML content are identical",
		},
		{
			name: "different HTML content",
			currentEvent: map[string]any{
				"content": map[string]any{
					"msgtype":        "m.text",
					"body":           "Hello world",
					"format":         "org.matrix.custom.html",
					"formatted_body": "<b>Hello world</b>",
				},
			},
			newPlainText:   "Hello world",
			newHTMLContent: "<i>Hello world</i>",
			expected:       false,
			description:    "Should return false when HTML content differs",
		},
		{
			name: "new HTML content, current has none",
			currentEvent: map[string]any{
				"content": map[string]any{
					"msgtype": "m.text",
					"body":    "Hello world",
				},
			},
			newPlainText:   "Hello world",
			newHTMLContent: "<b>Hello world</b>",
			expected:       false,
			description:    "Should return false when new content has HTML but current doesn't",
		},
		{
			name: "no content field in current event",
			currentEvent: map[string]any{
				"type": "m.room.message",
			},
			newPlainText:   "Hello world",
			newHTMLContent: "",
			expected:       false,
			description:    "Should return false when current event has no content field",
		},
		{
			name: "missing body in current event",
			currentEvent: map[string]any{
				"content": map[string]any{
					"msgtype": "m.text",
				},
			},
			newPlainText:   "Hello world",
			newHTMLContent: "",
			expected:       false,
			description:    "Should return false when current event has no body field",
		},
		{
			name: "current has HTML, new has none",
			currentEvent: map[string]any{
				"content": map[string]any{
					"msgtype":        "m.text",
					"body":           "Hello world",
					"format":         "org.matrix.custom.html",
					"formatted_body": "<b>Hello world</b>",
				},
			},
			newPlainText:   "Hello world",
			newHTMLContent: "",
			expected:       false,
			description:    "Should return false when current has HTML but new doesn't",
		},
		{
			name: "both have empty HTML",
			currentEvent: map[string]any{
				"content": map[string]any{
					"msgtype":        "m.text",
					"body":           "Hello world",
					"formatted_body": "",
				},
			},
			newPlainText:   "Hello world",
			newHTMLContent: "",
			expected:       true,
			description:    "Should return true when both have empty HTML content",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := plugin.mattermostToMatrixBridge.compareTextContent(tt.currentEvent, tt.newPlainText, tt.newHTMLContent, []matrix.FileAttachment{})
			assert.Equal(t, tt.expected, result, tt.description)
		})
	}
}

func TestCompareTextContentFileOnly(t *testing.T) {
	plugin := setupPluginForTest()
	// Set up required fields for bridge initialization
	plugin.maxProfileImageSize = DefaultMaxProfileImageSize
	plugin.maxFileSize = DefaultMaxFileSize
	plugin.postTracker = NewPostTracker(DefaultPostTrackerMaxEntries)
	plugin.pendingFiles = NewPendingFileTracker()
	// Initialize bridges for testing
	plugin.initBridges()

	tests := []struct {
		name           string
		currentEvent   map[string]any
		newPlainText   string
		newHTMLContent string
		newFiles       []matrix.FileAttachment
		expected       bool
		description    string
	}{
		{
			name: "file-only post: Matrix body matches filename",
			currentEvent: map[string]any{
				"content": map[string]any{
					"msgtype": "m.text",
					"body":    "Eye_of_Beholder.webp",
				},
			},
			newPlainText:   "",
			newHTMLContent: "",
			newFiles: []matrix.FileAttachment{
				{
					Filename: "Eye_of_Beholder.webp",
					MxcURI:   "mxc://example.com/abc123",
					MimeType: "image/webp",
					Size:     12345,
				},
			},
			expected:    true,
			description: "Should return true when Matrix body matches filename for file-only post",
		},
		{
			name: "file-only post: Matrix body doesn't match filename",
			currentEvent: map[string]any{
				"content": map[string]any{
					"msgtype": "m.text",
					"body":    "different.jpg",
				},
			},
			newPlainText:   "",
			newHTMLContent: "",
			newFiles: []matrix.FileAttachment{
				{
					Filename: "Eye_of_Beholder.webp",
					MxcURI:   "mxc://example.com/abc123",
					MimeType: "image/webp",
					Size:     12345,
				},
			},
			expected:    false,
			description: "Should return false when Matrix body doesn't match filename",
		},
		{
			name: "file-only post: multiple files, matches one",
			currentEvent: map[string]any{
				"content": map[string]any{
					"msgtype": "m.text",
					"body":    "second.pdf",
				},
			},
			newPlainText:   "",
			newHTMLContent: "",
			newFiles: []matrix.FileAttachment{
				{
					Filename: "first.jpg",
					MxcURI:   "mxc://example.com/abc123",
					MimeType: "image/jpeg",
					Size:     12345,
				},
				{
					Filename: "second.pdf",
					MxcURI:   "mxc://example.com/def456",
					MimeType: "application/pdf",
					Size:     67890,
				},
			},
			expected:    true,
			description: "Should return true when Matrix body matches any filename",
		},
		{
			name: "not file-only: has text content",
			currentEvent: map[string]any{
				"content": map[string]any{
					"msgtype": "m.text",
					"body":    "some text content",
				},
			},
			newPlainText:   "",
			newHTMLContent: "",
			newFiles: []matrix.FileAttachment{
				{
					Filename: "Eye_of_Beholder.webp",
					MxcURI:   "mxc://example.com/abc123",
					MimeType: "image/webp",
					Size:     12345,
				},
			},
			expected:    false,
			description: "Should return false when not a file-only post (has different text)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := plugin.mattermostToMatrixBridge.compareTextContent(tt.currentEvent, tt.newPlainText, tt.newHTMLContent, tt.newFiles)
			assert.Equal(t, tt.expected, result, tt.description)
		})
	}
}

func TestCompareFileAttachments(t *testing.T) {
	tests := []struct {
		name         string
		currentFiles []matrix.FileAttachment
		newFiles     []matrix.FileAttachment
		expected     bool
		description  string
	}{
		{
			name:         "both empty",
			currentFiles: []matrix.FileAttachment{},
			newFiles:     []matrix.FileAttachment{},
			expected:     true,
			description:  "Should return true when both have no files",
		},
		{
			name: "identical single file",
			currentFiles: []matrix.FileAttachment{
				{
					Filename: "test.jpg",
					MxcURI:   "mxc://example.com/abcd1234",
					MimeType: "image/jpeg",
					Size:     12345,
				},
			},
			newFiles: []matrix.FileAttachment{
				{
					Filename: "test.jpg",
					MxcURI:   "mxc://example.com/abcd1234",
					MimeType: "image/jpeg",
					Size:     12345,
				},
			},
			expected:    true,
			description: "Should return true when files are identical",
		},
		{
			name: "different filename",
			currentFiles: []matrix.FileAttachment{
				{
					Filename: "test.jpg",
					MxcURI:   "mxc://example.com/abcd1234",
					MimeType: "image/jpeg",
					Size:     12345,
				},
			},
			newFiles: []matrix.FileAttachment{
				{
					Filename: "different.jpg",
					MxcURI:   "mxc://example.com/abcd1234",
					MimeType: "image/jpeg",
					Size:     12345,
				},
			},
			expected:    false,
			description: "Should return false when filename differs",
		},
		{
			name: "different MXC URI",
			currentFiles: []matrix.FileAttachment{
				{
					Filename: "test.jpg",
					MxcURI:   "mxc://example.com/abcd1234",
					MimeType: "image/jpeg",
					Size:     12345,
				},
			},
			newFiles: []matrix.FileAttachment{
				{
					Filename: "test.jpg",
					MxcURI:   "mxc://example.com/different",
					MimeType: "image/jpeg",
					Size:     12345,
				},
			},
			expected:    false,
			description: "Should return false when MXC URI differs",
		},
		{
			name: "different MIME type",
			currentFiles: []matrix.FileAttachment{
				{
					Filename: "test.jpg",
					MxcURI:   "mxc://example.com/abcd1234",
					MimeType: "image/jpeg",
					Size:     12345,
				},
			},
			newFiles: []matrix.FileAttachment{
				{
					Filename: "test.jpg",
					MxcURI:   "mxc://example.com/abcd1234",
					MimeType: "image/png",
					Size:     12345,
				},
			},
			expected:    false,
			description: "Should return false when MIME type differs",
		},
		{
			name: "different size",
			currentFiles: []matrix.FileAttachment{
				{
					Filename: "test.jpg",
					MxcURI:   "mxc://example.com/abcd1234",
					MimeType: "image/jpeg",
					Size:     12345,
				},
			},
			newFiles: []matrix.FileAttachment{
				{
					Filename: "test.jpg",
					MxcURI:   "mxc://example.com/abcd1234",
					MimeType: "image/jpeg",
					Size:     54321,
				},
			},
			expected:    false,
			description: "Should return false when size differs",
		},
		{
			name: "different count - more current files",
			currentFiles: []matrix.FileAttachment{
				{
					Filename: "test1.jpg",
					MxcURI:   "mxc://example.com/abcd1234",
					MimeType: "image/jpeg",
					Size:     12345,
				},
				{
					Filename: "test2.jpg",
					MxcURI:   "mxc://example.com/efgh5678",
					MimeType: "image/jpeg",
					Size:     67890,
				},
			},
			newFiles: []matrix.FileAttachment{
				{
					Filename: "test1.jpg",
					MxcURI:   "mxc://example.com/abcd1234",
					MimeType: "image/jpeg",
					Size:     12345,
				},
			},
			expected:    false,
			description: "Should return false when current has more files",
		},
		{
			name: "different count - more new files",
			currentFiles: []matrix.FileAttachment{
				{
					Filename: "test1.jpg",
					MxcURI:   "mxc://example.com/abcd1234",
					MimeType: "image/jpeg",
					Size:     12345,
				},
			},
			newFiles: []matrix.FileAttachment{
				{
					Filename: "test1.jpg",
					MxcURI:   "mxc://example.com/abcd1234",
					MimeType: "image/jpeg",
					Size:     12345,
				},
				{
					Filename: "test2.jpg",
					MxcURI:   "mxc://example.com/efgh5678",
					MimeType: "image/jpeg",
					Size:     67890,
				},
			},
			expected:    false,
			description: "Should return false when new has more files",
		},
		{
			name: "identical multiple files",
			currentFiles: []matrix.FileAttachment{
				{
					Filename: "test1.jpg",
					MxcURI:   "mxc://example.com/abcd1234",
					MimeType: "image/jpeg",
					Size:     12345,
				},
				{
					Filename: "test2.pdf",
					MxcURI:   "mxc://example.com/efgh5678",
					MimeType: "application/pdf",
					Size:     67890,
				},
			},
			newFiles: []matrix.FileAttachment{
				{
					Filename: "test1.jpg",
					MxcURI:   "mxc://example.com/abcd1234",
					MimeType: "image/jpeg",
					Size:     12345,
				},
				{
					Filename: "test2.pdf",
					MxcURI:   "mxc://example.com/efgh5678",
					MimeType: "application/pdf",
					Size:     67890,
				},
			},
			expected:    true,
			description: "Should return true when multiple files are identical",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := compareFileAttachmentArrays(tt.currentFiles, tt.newFiles)
			assert.Equal(t, tt.expected, result, tt.description)
		})
	}
}
