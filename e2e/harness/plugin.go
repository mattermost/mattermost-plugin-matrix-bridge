package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

// PluginRequest sends an authenticated request to /plugins/<PluginID><path> as the client's
// user. The response body is closed on test cleanup.
func PluginRequest(t *testing.T, client *model.Client4, method, path string, body any) *http.Response {
	t.Helper()
	resp, err := pluginRequest(t.Context(), client, method, path, body)
	require.NoError(t, err, "%s %s", method, path)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// pluginRequest is built by hand because Client4.DoAPIRequestWithHeaders always prefixes /api/v4.
func pluginRequest(ctx context.Context, client *model.Client4, method, path string, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, client.URL+"/plugins/"+PluginID+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set(model.HeaderAuth, model.HeaderBearer+" "+client.AuthToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return client.HTTPClient.Do(req)
}
