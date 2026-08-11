package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/servers"
)

func TestHandleMatrixMemberDM_EarlyExits(t *testing.T) {
	stateKey := "@alice:matrix.example.com"

	tests := []struct {
		name  string
		event MatrixEvent
	}{
		{
			name: "nil content returns empty channel ID",
			event: MatrixEvent{
				RoomID:   "!room:matrix.example.com",
				Sender:   "@bob:matrix.example.com",
				StateKey: &stateKey,
				Content:  nil,
			},
		},
		{
			name: "leave membership returns empty channel ID",
			event: MatrixEvent{
				RoomID:   "!room:matrix.example.com",
				Sender:   "@bob:matrix.example.com",
				StateKey: &stateKey,
				Content:  map[string]any{"membership": "leave"},
			},
		},
		{
			name: "ban membership returns empty channel ID",
			event: MatrixEvent{
				RoomID:   "!room:matrix.example.com",
				Sender:   "@bob:matrix.example.com",
				StateKey: &stateKey,
				Content:  map[string]any{"membership": "ban"},
			},
		},
		{
			name: "missing membership field returns empty channel ID",
			event: MatrixEvent{
				RoomID:   "!room:matrix.example.com",
				Sender:   "@bob:matrix.example.com",
				StateKey: &stateKey,
				Content:  map[string]any{},
			},
		},
		{
			name: "nil state_key returns empty channel ID",
			event: MatrixEvent{
				RoomID:   "!room:matrix.example.com",
				Sender:   "@bob:matrix.example.com",
				StateKey: nil,
				Content:  map[string]any{"membership": "join"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plugin := &Plugin{}
			plugin.logger = &testLogger{t: t}
			plugin.kvstore = NewMemoryKVStore()
			plugin.servers = servers.New(plugin.kvstore, pluginLogger{plugin}, pluginHost{plugin})
			serverID, _ := registerTestServer(t, plugin, "https://matrix.example.com", "matrix.example.com", nil)

			channelID, err := plugin.handleMatrixMemberDM(serverID, tt.event)

			require.NoError(t, err)
			assert.Equal(t, "", channelID)
		})
	}
}

func TestHandleMatrixMemberDM_SwitchRouting(t *testing.T) {
	const matrixServerURL = "https://matrix.example.com"
	const serverDomain = "matrix.example.com"

	ghostUserID := "@_mattermost_userid123:" + serverDomain
	regularUserID := "@alice:" + serverDomain

	newPlugin := func(t *testing.T) (*Plugin, string) {
		t.Helper()
		plugin := &Plugin{}
		plugin.logger = &testLogger{t: t}
		plugin.kvstore = NewMemoryKVStore()
		plugin.servers = servers.New(plugin.kvstore, pluginLogger{plugin}, pluginHost{plugin})
		serverID, _ := registerTestServer(t, plugin, matrixServerURL, serverDomain, nil)
		return plugin, serverID
	}

	t.Run("neither user is ghost returns empty channel ID", func(t *testing.T) {
		plugin, serverID := newPlugin(t)

		sk := regularUserID
		event := MatrixEvent{
			RoomID:   "!room:" + serverDomain,
			Sender:   "@bob:" + serverDomain,
			StateKey: &sk,
			Content:  map[string]any{"membership": "join"},
		}

		channelID, err := plugin.handleMatrixMemberDM(serverID, event)

		require.NoError(t, err)
		assert.Equal(t, "", channelID)
	})

	t.Run("ghost user as target reaches createDMChannelForGhostUser", func(t *testing.T) {
		plugin, serverID := newPlugin(t)

		sk := ghostUserID // ghost is target (state_key)
		event := MatrixEvent{
			RoomID:   "!room:" + serverDomain,
			Sender:   regularUserID,
			StateKey: &sk,
			Content:  map[string]any{"membership": "join"},
		}

		// Ghost not registered in kvstore → createDMChannelForGhostUser returns "", nil silently
		channelID, err := plugin.handleMatrixMemberDM(serverID, event)

		require.NoError(t, err)
		assert.Equal(t, "", channelID)
	})

	t.Run("ghost user as actor reaches createDMChannelForGhostUser", func(t *testing.T) {
		plugin, serverID := newPlugin(t)

		sk := regularUserID
		event := MatrixEvent{
			RoomID:   "!room:" + serverDomain,
			Sender:   ghostUserID, // ghost is actor (sender)
			StateKey: &sk,
			Content:  map[string]any{"membership": "join"},
		}

		// Ghost not registered in kvstore → createDMChannelForGhostUser returns "", nil silently
		channelID, err := plugin.handleMatrixMemberDM(serverID, event)

		require.NoError(t, err)
		assert.Equal(t, "", channelID)
	})

	t.Run("invite membership is also handled", func(t *testing.T) {
		plugin, serverID := newPlugin(t)

		sk := regularUserID
		event := MatrixEvent{
			RoomID:   "!room:" + serverDomain,
			Sender:   "@bob:" + serverDomain,
			StateKey: &sk,
			Content:  map[string]any{"membership": "invite"},
		}

		// Neither user is ghost → default case → returns "", nil
		channelID, err := plugin.handleMatrixMemberDM(serverID, event)

		require.NoError(t, err)
		assert.Equal(t, "", channelID)
	})
}

// newTestPluginForTransaction returns a Plugin with an in-memory KV store and a real
// (in-memory, unconfigured) transaction logger, ready for handleMatrixTransaction tests.
func newTestPluginForTransaction(t *testing.T) *Plugin {
	t.Helper()
	plugin := setupPluginForTest()
	plugin.kvstore = NewMemoryKVStore()
	plugin.servers = servers.New(plugin.kvstore, pluginLogger{plugin}, pluginHost{plugin})

	logger, err := CreateTransactionLogger()
	require.NoError(t, err)
	t.Cleanup(func() { _ = logger.Logr().Shutdown() })
	plugin.transactionLogger = logger

	api := plugin.API.(*plugintest.API)
	api.On("GetConfig").Return(&model.Config{}).Maybe()
	mockAnyLogCalls(api)

	return plugin
}

// transactionRequest builds a PUT /transactions/{txnId} request with serverID already
// resolved in context, as MatrixAuthorizationRequired would have left it, and txnId
// available via mux.Vars as the router would have parsed it.
func transactionRequest(serverID, txnID, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPut, "/_matrix/app/v1/transactions/"+txnID, strings.NewReader(body))
	req = mux.SetURLVars(req, map[string]string{"txnId": txnID})
	ctx := context.WithValue(req.Context(), contextKeyServerID, serverID)
	return req.WithContext(ctx)
}

// TestHandleMatrixTransaction covers §5.1's inbound-auth edge cases around the
// processed-transaction dedupe map: a missing client must not mark the transaction
// processed (so the homeserver's retry survives), a genuine duplicate on one server must
// be deduped, and an identical txnId presented by two different servers must be
// processed for both - the dedupe key includes serverID for exactly this reason.
func TestHandleMatrixTransaction(t *testing.T) {
	t.Run("missing client responds 503 with Retry-After and does not mark the txn processed", func(t *testing.T) {
		plugin := newTestPluginForTransaction(t)
		serverID, _ := registerTestServer(t, plugin, "https://a.example.com", "a.example.com", nil) // no client

		txnID := "txn-" + model.NewId()
		w := httptest.NewRecorder()
		plugin.handleMatrixTransaction(w, transactionRequest(serverID, txnID, `{"events":[]}`))

		resp := w.Result()
		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
		assert.Equal(t, "5", resp.Header.Get("Retry-After"))

		transactionsMutex.RLock()
		_, exists := processedTransactions[transactionKey{serverID: serverID, txnID: txnID}]
		transactionsMutex.RUnlock()
		assert.False(t, exists, "a transaction rejected for a missing client must not be recorded as processed")

		// A retry against an up-to-date node (client now available) must still be
		// processed - proving the 503 path really did skip the dedupe write above.
		client := createMatrixClientWithTestLogger(t, "https://a.example.com", "as", "")
		plugin.matrixClientsLock.Lock()
		plugin.matrixClients[serverID] = client
		plugin.matrixClientsLock.Unlock()

		w2 := httptest.NewRecorder()
		plugin.handleMatrixTransaction(w2, transactionRequest(serverID, txnID, `{"events":[]}`))
		assert.Equal(t, http.StatusOK, w2.Result().StatusCode)
	})

	t.Run("duplicate txnId from the same server is deduped", func(t *testing.T) {
		plugin := newTestPluginForTransaction(t)
		client := createMatrixClientWithTestLogger(t, "https://a.example.com", "as", "")
		serverID, _ := registerTestServer(t, plugin, "https://a.example.com", "a.example.com", client)

		txnID := "txn-" + model.NewId()
		w1 := httptest.NewRecorder()
		plugin.handleMatrixTransaction(w1, transactionRequest(serverID, txnID, `{"events":[]}`))
		require.Equal(t, http.StatusOK, w1.Result().StatusCode)

		// An intentionally malformed body on the retry: if dedup didn't short-circuit
		// before JSON parsing, this would 400 instead of 200.
		w2 := httptest.NewRecorder()
		plugin.handleMatrixTransaction(w2, transactionRequest(serverID, txnID, `not valid json`))
		assert.Equal(t, http.StatusOK, w2.Result().StatusCode, "a deduped transaction must short-circuit before its body is even parsed")
	})

	t.Run("identical txnId from two different servers is processed for both, not deduped", func(t *testing.T) {
		plugin := newTestPluginForTransaction(t)
		clientA := createMatrixClientWithTestLogger(t, "https://a.example.com", "as", "")
		clientB := createMatrixClientWithTestLogger(t, "https://b.example.com", "as", "")
		serverIDA, _ := registerTestServer(t, plugin, "https://a.example.com", "a.example.com", clientA)
		serverIDB, _ := registerTestServer(t, plugin, "https://b.example.com", "b.example.com", clientB)

		txnID := "shared-txn-" + model.NewId()

		wA := httptest.NewRecorder()
		plugin.handleMatrixTransaction(wA, transactionRequest(serverIDA, txnID, `{"events":[]}`))
		require.Equal(t, http.StatusOK, wA.Result().StatusCode)

		// Same txnID, different server, and an intentionally malformed body: if this
		// were wrongly deduped against server A's transaction, it would still 200
		// without parsing the body. It must instead 400, proving it was processed fresh.
		wB := httptest.NewRecorder()
		plugin.handleMatrixTransaction(wB, transactionRequest(serverIDB, txnID, `not valid json`))
		assert.Equal(t, http.StatusBadRequest, wB.Result().StatusCode, "server B's transaction must be processed independently of server A's identical txnId")

		transactionsMutex.RLock()
		_, existsA := processedTransactions[transactionKey{serverID: serverIDA, txnID: txnID}]
		_, existsB := processedTransactions[transactionKey{serverID: serverIDB, txnID: txnID}]
		transactionsMutex.RUnlock()
		assert.True(t, existsA, "server A's transaction must be recorded as processed")
		assert.False(t, existsB, "server B's malformed retry must not be recorded as processed (it 400'd before reaching that point)")
	})
}
