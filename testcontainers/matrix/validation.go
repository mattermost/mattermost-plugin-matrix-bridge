package matrix

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// MatrixEventValidation provides helpers for validating Matrix events in tests
//
//nolint:revive // MatrixEventValidation is intentionally named to be descriptive in test context
type MatrixEventValidation struct {
	t            *testing.T
	serverDomain string
	remoteID     string
}

// NewMatrixEventValidation creates a new validation helper
func NewMatrixEventValidation(t *testing.T, serverDomain, remoteID string) *MatrixEventValidation {
	return &MatrixEventValidation{
		t:            t,
		serverDomain: serverDomain,
		remoteID:     remoteID,
	}
}

// ValidateMessageEvent validates a basic Matrix message event structure
func (v *MatrixEventValidation) ValidateMessageEvent(event map[string]any, originalPost any) {
	// Verify basic event structure
	assert.Equal(v.t, "m.room.message", event["type"], "Event should be a message")

	content, ok := event["content"].(map[string]any)
	require.True(v.t, ok, "Event should have content")

	// Verify message content
	assert.Equal(v.t, "m.text", content["msgtype"], "Should be text message")
	assert.NotEmpty(v.t, content["body"], "Should have message body")

	// Verify Mattermost metadata if provided
	if originalPost != nil {
		if post, ok := originalPost.(interface{ GetId() string }); ok {
			assert.Equal(v.t, post.GetId(), content["mattermost_post_id"], "Should have correct post ID")
		}
	}

	if v.remoteID != "" {
		assert.Equal(v.t, v.remoteID, content["mattermost_remote_id"], "Should have correct remote ID")
	}
}

// ValidateMessageWithMentions validates a Matrix message with mentions
func (v *MatrixEventValidation) ValidateMessageWithMentions(event map[string]any, originalPost any, expectedMentionCount int) {
	// First validate basic message structure
	v.ValidateMessageEvent(event, originalPost)

	content := event["content"].(map[string]any)

	// Verify mention structure exists
	mentions, hasMentions := content["m.mentions"].(map[string]any)
	require.True(v.t, hasMentions, "Message with mentions should have m.mentions field")

	userIDs, hasUserIDs := mentions["user_ids"].([]any)
	require.True(v.t, hasUserIDs, "Mentions should have user_ids array")
	assert.Len(v.t, userIDs, expectedMentionCount, "Should have expected number of mentions")

	// Verify HTML formatting is present for mentions
	_, hasHTML := content["formatted_body"]
	assert.True(v.t, hasHTML, "Message with mentions should have HTML formatted body")
	assert.Equal(v.t, "org.matrix.custom.html", content["format"], "Should specify HTML format")
}

// ValidateThreadedMessage validates a threaded Matrix message
func (v *MatrixEventValidation) ValidateThreadedMessage(event map[string]any, originalPost any, parentEventID string) {
	v.ValidateMessageEvent(event, originalPost)

	content := event["content"].(map[string]any)

	// Verify threading relation
	relatesTo, hasRelation := content["m.relates_to"].(map[string]any)
	require.True(v.t, hasRelation, "Threaded message should have m.relates_to")

	assert.Equal(v.t, "m.thread", relatesTo["rel_type"], "Should use thread relation type")
	assert.Equal(v.t, parentEventID, relatesTo["event_id"], "Should reference parent event")
}

// ValidateFileMessage validates a Matrix file message event
func (v *MatrixEventValidation) ValidateFileMessage(event map[string]any, expectedFilename string, expectedMimeType string) {
	// Verify basic event structure
	assert.Equal(v.t, "m.room.message", event["type"], "Event should be a message")

	content, ok := event["content"].(map[string]any)
	require.True(v.t, ok, "Event should have content")

	// Verify file message type based on MIME type
	msgType := content["msgtype"].(string)
	//nolint:staticcheck // Switch statement is more readable than tagged switch for this use case
	switch {
	case expectedMimeType == "image/png" || expectedMimeType == "image/jpeg":
		assert.Equal(v.t, "m.image", msgType, "Should be image message")
	case expectedMimeType == "video/mp4":
		assert.Equal(v.t, "m.video", msgType, "Should be video message")
	case expectedMimeType == "audio/mp3":
		assert.Equal(v.t, "m.audio", msgType, "Should be audio message")
	default:
		assert.Equal(v.t, "m.file", msgType, "Should be file message")
	}

	// Verify file metadata
	assert.Equal(v.t, expectedFilename, content["body"], "Should have correct filename")

	fileInfo, hasFileInfo := content["info"].(map[string]any)
	require.True(v.t, hasFileInfo, "File message should have info")
	assert.Equal(v.t, expectedMimeType, fileInfo["mimetype"], "Should have correct MIME type")
}

// ValidateReactionEvent validates a Matrix reaction event
func (v *MatrixEventValidation) ValidateReactionEvent(event map[string]any, targetEventID string, expectedEmoji string) {
	// Verify basic event structure
	assert.Equal(v.t, "m.reaction", event["type"], "Event should be a reaction")

	content, ok := event["content"].(map[string]any)
	require.True(v.t, ok, "Event should have content")

	// Verify reaction relation
	relatesTo, hasRelation := content["m.relates_to"].(map[string]any)
	require.True(v.t, hasRelation, "Reaction should have m.relates_to")

	assert.Equal(v.t, "m.annotation", relatesTo["rel_type"], "Should use annotation relation type")
	assert.Equal(v.t, targetEventID, relatesTo["event_id"], "Should reference target event")
	assert.Equal(v.t, expectedEmoji, relatesTo["key"], "Should have correct emoji")
}

// ValidateEditEvent validates a Matrix message edit event
func (v *MatrixEventValidation) ValidateEditEvent(event map[string]any, originalEventID string, expectedNewContent string) {
	// Verify basic event structure
	assert.Equal(v.t, "m.room.message", event["type"], "Event should be a message")

	content, ok := event["content"].(map[string]any)
	require.True(v.t, ok, "Event should have content")

	// Verify edit relation
	relatesTo, hasRelation := content["m.relates_to"].(map[string]any)
	require.True(v.t, hasRelation, "Edit should have m.relates_to")

	assert.Equal(v.t, "m.replace", relatesTo["rel_type"], "Should use replace relation type")
	assert.Equal(v.t, originalEventID, relatesTo["event_id"], "Should reference original event")

	// Verify new content
	newContent, hasNewContent := content["m.new_content"].(map[string]any)
	require.True(v.t, hasNewContent, "Edit should have m.new_content")
	assert.Equal(v.t, expectedNewContent, newContent["body"], "Should have correct new content")
}
