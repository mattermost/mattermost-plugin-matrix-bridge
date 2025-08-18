package matrix

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// EventValidation provides helpers for validating Matrix events in tests
//
//nolint:revive // EventValidation is intentionally named to be descriptive in test context
type EventValidation struct {
	t            *testing.T
	serverDomain string
	remoteID     string
}

// NewEventValidation creates a new validation helper
func NewEventValidation(t *testing.T, serverDomain, remoteID string) *EventValidation {
	return &EventValidation{
		t:            t,
		serverDomain: serverDomain,
		remoteID:     remoteID,
	}
}

// ValidateMessageEvent validates a basic Matrix message event structure
func (v *EventValidation) ValidateMessageEvent(event Event, originalPost any) {
	// Verify basic event structure
	assert.Equal(v.t, "m.room.message", event.Type, "Event should be a message")
	assert.NotNil(v.t, event.Content, "Event should have content")

	// Verify message content
	assert.Equal(v.t, "m.text", event.Content["msgtype"], "Should be text message")
	assert.NotEmpty(v.t, event.Content["body"], "Should have message body")

	// Verify Mattermost metadata if provided
	if originalPost != nil {
		if post, ok := originalPost.(interface{ GetId() string }); ok {
			assert.Equal(v.t, post.GetId(), event.Content["mattermost_post_id"], "Should have correct post ID")
		}
	}

	if v.remoteID != "" {
		assert.Equal(v.t, v.remoteID, event.Content["mattermost_remote_id"], "Should have correct remote ID")
	}
}

// ValidateMessageWithMentions validates a Matrix message with mentions
func (v *EventValidation) ValidateMessageWithMentions(event Event, originalPost any, expectedMentionCount int) {
	// First validate basic message structure
	v.ValidateMessageEvent(event, originalPost)

	// Verify mention structure exists
	mentions, hasMentions := event.Content["m.mentions"].(map[string]any)
	require.True(v.t, hasMentions, "Message with mentions should have m.mentions field")

	userIDs, hasUserIDs := mentions["user_ids"].([]any)
	require.True(v.t, hasUserIDs, "Mentions should have user_ids array")
	assert.Len(v.t, userIDs, expectedMentionCount, "Should have expected number of mentions")

	// Verify HTML formatting is present for mentions
	_, hasHTML := event.Content["formatted_body"]
	assert.True(v.t, hasHTML, "Message with mentions should have HTML formatted body")
	assert.Equal(v.t, "org.matrix.custom.html", event.Content["format"], "Should specify HTML format")
}

// ValidateThreadedMessage validates a threaded Matrix message
func (v *EventValidation) ValidateThreadedMessage(event Event, originalPost any, parentEventID string) {
	v.ValidateMessageEvent(event, originalPost)

	// Verify threading relation
	relatesTo, hasRelation := event.Content["m.relates_to"].(map[string]any)
	require.True(v.t, hasRelation, "Threaded message should have m.relates_to")

	assert.Equal(v.t, "m.thread", relatesTo["rel_type"], "Should use thread relation type")
	assert.Equal(v.t, parentEventID, relatesTo["event_id"], "Should reference parent event")
}

// ValidateFileMessage validates a Matrix file message event
func (v *EventValidation) ValidateFileMessage(event Event, expectedFilename string, expectedMimeType string) {
	// Verify basic event structure
	assert.Equal(v.t, "m.room.message", event.Type, "Event should be a message")
	assert.NotNil(v.t, event.Content, "Event should have content")

	// Verify file message type based on MIME type
	msgType := event.Content["msgtype"].(string)
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
	assert.Equal(v.t, expectedFilename, event.Content["body"], "Should have correct filename")

	fileInfo, hasFileInfo := event.Content["info"].(map[string]any)
	require.True(v.t, hasFileInfo, "File message should have info")
	assert.Equal(v.t, expectedMimeType, fileInfo["mimetype"], "Should have correct MIME type")
}

// ValidateReactionEvent validates a Matrix reaction event
func (v *EventValidation) ValidateReactionEvent(event Event, targetEventID string, expectedEmoji string) {
	// Verify basic event structure
	assert.Equal(v.t, "m.reaction", event.Type, "Event should be a reaction")
	assert.NotNil(v.t, event.Content, "Event should have content")

	// Verify reaction relation
	relatesTo, hasRelation := event.Content["m.relates_to"].(map[string]any)
	require.True(v.t, hasRelation, "Reaction should have m.relates_to")

	assert.Equal(v.t, "m.annotation", relatesTo["rel_type"], "Should use annotation relation type")
	assert.Equal(v.t, targetEventID, relatesTo["event_id"], "Should reference target event")
	assert.Equal(v.t, expectedEmoji, relatesTo["key"], "Should have correct emoji")
}

// ValidateEditEvent validates a Matrix message edit event
func (v *EventValidation) ValidateEditEvent(event Event, originalEventID string, expectedNewContent string) {
	// Verify basic event structure
	assert.Equal(v.t, "m.room.message", event.Type, "Event should be a message")
	assert.NotNil(v.t, event.Content, "Event should have content")

	// Verify edit relation
	relatesTo, hasRelation := event.Content["m.relates_to"].(map[string]any)
	require.True(v.t, hasRelation, "Edit should have m.relates_to")

	assert.Equal(v.t, "m.replace", relatesTo["rel_type"], "Should use replace relation type")
	assert.Equal(v.t, originalEventID, relatesTo["event_id"], "Should reference original event")

	// Verify new content
	newContent, hasNewContent := event.Content["m.new_content"].(map[string]any)
	require.True(v.t, hasNewContent, "Edit should have m.new_content")
	assert.Equal(v.t, expectedNewContent, newContent["body"], "Should have correct new content")
}
