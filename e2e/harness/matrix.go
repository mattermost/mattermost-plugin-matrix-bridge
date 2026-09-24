package harness

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"

	matrixtest "github.com/mattermost/mattermost-plugin-matrix-bridge/testcontainers/matrix"
)

// UploadMediaAsUser uploads data to Synapse's media repository with the user's own token and
// returns its mxc:// URI.
func UploadMediaAsUser(t *testing.T, user *matrixtest.User, filename, contentType string, data []byte) string {
	t.Helper()
	endpoint := Shared(t).Synapse.ServerURL + "/_matrix/media/v3/upload?filename=" + url.QueryEscape(filename)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint, bytes.NewReader(data))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+user.AccessToken)
	req.Header.Set("Content-Type", contentType)

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	require.NoError(t, err, "upload %s as %s", filename, user.UserID)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "read upload response for %s", filename)
	require.Equal(t, http.StatusOK, resp.StatusCode, "upload %s as %s: %s", filename, user.UserID, body)

	var uploaded struct {
		ContentURI string `json:"content_uri"`
	}
	require.NoError(t, json.Unmarshal(body, &uploaded), "decode upload response for %s: %s", filename, body)
	require.NotEmpty(t, uploaded.ContentURI, "upload response for %s has no content_uri: %s", filename, body)
	return uploaded.ContentURI
}

// RedactAsUser redacts an event with the user's own token and returns the redaction event ID.
func RedactAsUser(t *testing.T, user *matrixtest.User, roomID, eventID string) string {
	t.Helper()
	path := "/_matrix/client/v3/rooms/" + url.PathEscape(roomID) + "/redact/" + url.PathEscape(eventID) + "/" + model.NewId()
	result, err := Shared(t).Synapse.DoAsUser(user, http.MethodPut, path, map[string]any{})
	require.NoError(t, err, "redact event %s in room %s as %s", eventID, roomID, user.UserID)

	response, _ := result.(map[string]any)
	redactionID, _ := response["event_id"].(string)
	require.NotEmpty(t, redactionID, "redaction of event %s in room %s returned no event_id: %v", eventID, roomID, result)
	return redactionID
}
