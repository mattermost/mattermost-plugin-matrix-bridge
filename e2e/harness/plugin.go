package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

// PluginRequest sends an authenticated request to /plugins/<PluginID><path> as the client's
// user. The response body is closed on test cleanup.
func PluginRequest(t *testing.T, client *model.Client4, method, path string, body any) *http.Response {
	t.Helper()
	resp, err := PluginRequestContext(t.Context(), client, method, path, body)
	require.NoError(t, err, "%s %s", method, path)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// PluginRequestContext is PluginRequest with a caller-chosen context, for use in cleanups; the
// caller closes the response body. It is built by hand because Client4.DoAPIRequestWithHeaders
// always prefixes /api/v4.
func PluginRequestContext(ctx context.Context, client *model.Client4, method, path string, body any) (*http.Response, error) {
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

// Diagnostics is the plugin's POST /servers/{id}/test response.
type Diagnostics struct {
	Checks []struct {
		Key    string `json:"key"`
		Status string `json:"status"`
		Detail string `json:"detail"`
	} `json:"checks"`
	ServerInfo map[string]any `json:"server_info"`
}

// TransactionStatus PUTs an empty Application Service transaction to the plugin's webhook,
// authenticated with token as Synapse would send it, and returns the response status.
func TransactionStatus(ctx context.Context, env *Env, token string) (int, error) {
	url := env.Admin.URL + "/plugins/" + PluginID + "/_matrix/app/v1/transactions/e2e-" + model.NewId()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, strings.NewReader(`{"events":[]}`))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.Admin.HTTPClient.Do(req)
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}
