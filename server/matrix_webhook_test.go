package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/mattermost/mattermost/server/public/pluginapi"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/servers"
	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
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

	t.Run("a per-event processing failure responds 503, does not mark the txn processed, and a retry succeeds", func(t *testing.T) {
		plugin := newTestPluginForTransaction(t)
		client := createMatrixClientWithTestLogger(t, "https://a.example.com", "as", "")
		serverID, _ := registerTestServer(t, plugin, "https://a.example.com", "a.example.com", client)

		roomID := "!room:a.example.com"
		roomMappingKey := kvstore.BuildRoomMappingKey(serverID, roomID)

		// Force the room-mapping lookup inside processMatrixEvent to fail, simulating a
		// transient KV read error rather than "no mapping found".
		realStore := plugin.kvstore
		plugin.kvstore = &erroringKVStore{KVStore: realStore, errOnGetKey: roomMappingKey}

		body := `{"events":[{"type":"m.room.message","event_id":"$evt1","sender":"@alice:a.example.com","room_id":"` + roomID + `"}]}`

		txnID := "txn-" + model.NewId()
		w := httptest.NewRecorder()
		plugin.handleMatrixTransaction(w, transactionRequest(serverID, txnID, body))

		resp := w.Result()
		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, "a per-event processing error must surface as a transaction-level failure")
		assert.Equal(t, "5", resp.Header.Get("Retry-After"))

		transactionsMutex.RLock()
		_, exists := processedTransactions[transactionKey{serverID: serverID, txnID: txnID}]
		transactionsMutex.RUnlock()
		assert.False(t, exists, "a transaction with a failed event must not be recorded as processed")

		// Restore the dependency and resubmit the same transaction: it must now succeed,
		// proving the failed attempt didn't get wrongly deduped.
		plugin.kvstore = realStore

		w2 := httptest.NewRecorder()
		plugin.handleMatrixTransaction(w2, transactionRequest(serverID, txnID, body))
		assert.Equal(t, http.StatusOK, w2.Result().StatusCode, "the retried transaction must be processed, not dropped as a duplicate")
	})
}

// TestIsPermanentEventError pins the classifier that decides between acknowledging an
// event and asking the homeserver to redeliver it. The sentinel travels up through
// several errors.Wrap layers before handleMatrixTransaction sees it, so the wrap chain
// is part of what is under test here.
func TestIsPermanentEventError(t *testing.T) {
	t.Run("recognizes the sentinel through the wrap chain it actually arrives in", func(t *testing.T) {
		// createDMChannelForGhostUser -> handleMatrixInitiatedDM -> processMatrixEvent.
		err := errors.Wrap(ErrChannelAlreadyMapped, "failed to store channel room mapping")
		err = errors.Wrap(err, "failed to handle Matrix-initiated DM")
		err = errors.Wrap(err, "failed to process Matrix event")

		assert.True(t, isPermanentEventError(err))
	})

	t.Run("treats unrecognized errors as transient", func(t *testing.T) {
		assert.False(t, isPermanentEventError(errors.New("kv store unavailable")))
		assert.False(t, isPermanentEventError(errors.Wrap(errors.New("boom"), "failed to read room mapping")))
		assert.False(t, isPermanentEventError(nil))
	})
}

// TestHandleMatrixTransactionPermanentFailure covers the AS-spec hazard that a 503 for a
// permanently-failing event blocks a homeserver forever: transactions are delivered in
// order, so the homeserver retries the same txnId indefinitely and every later event
// queues behind it. A permanent failure must be logged and acknowledged instead.
func TestHandleMatrixTransactionPermanentFailure(t *testing.T) {
	const serverDomain = "a.example.com"

	plugin := newTestPluginForTransaction(t)
	client := createMatrixClientWithTestLogger(t, "https://"+serverDomain, "as", "")
	serverID, _ := registerTestServer(t, plugin, "https://"+serverDomain, serverDomain, client)
	plugin.client = pluginapi.NewClient(plugin.API, nil)

	api := plugin.API.(*plugintest.API)

	// A ghost user this bridge created, whose Matrix DM will target a Mattermost channel
	// that is already mapped to a different live server.
	mattermostUserID := model.NewId()
	ghostUserID := "@_mattermost_" + mattermostUserID + ":" + serverDomain
	matrixUserID := "@alice:" + serverDomain
	matrixUserMattermostID := model.NewId()
	dmChannelID := model.NewId()

	require.NoError(t, plugin.kvstore.Set(kvstore.BuildGhostUserKey(serverID, mattermostUserID), []byte(ghostUserID)))
	require.NoError(t, plugin.kvstore.Set(kvstore.BuildMatrixUserKey(serverID, matrixUserID), []byte(matrixUserMattermostID)))

	// The DM channel already belongs to another live server, so the mapping write at the
	// end of createDMChannelForGhostUser returns ErrChannelAlreadyMapped - permanently.
	otherServerID, _ := registerTestServer(t, plugin, "https://b.example.com", "b.example.com", client)
	_, err := plugin.SetChannelMapping(dmChannelID, kvstore.ChannelServerMapping{ServerID: otherServerID, RoomID: "!other:b.example.com"})
	require.NoError(t, err)

	api.On("GetUser", mattermostUserID).Return(&model.User{Id: mattermostUserID, Username: "owner"}, nil).Maybe()
	api.On("GetUser", matrixUserMattermostID).Return(&model.User{Id: matrixUserMattermostID, Username: "alice"}, nil).Maybe()
	api.On("GetDirectChannel", mattermostUserID, matrixUserMattermostID).Return(&model.Channel{Id: dmChannelID}, nil).Maybe()

	// A second, unrelated room that IS mapped, so the transaction carries a later event
	// that must still be delivered once the permanent failure is skipped.
	mappedRoomID := "!mapped:" + serverDomain
	mappedChannelID := model.NewId()
	require.NoError(t, plugin.kvstore.Set(kvstore.BuildRoomMappingKey(serverID, mappedRoomID), []byte(mappedChannelID)))
	require.NoError(t, plugin.kvstore.Set(kvstore.BuildMatrixUserKey(serverID, "@bob:"+serverDomain), []byte(matrixUserMattermostID)))

	// The mapped room is a DM channel, so addUserToChannelTeam short-circuits on type
	// rather than dragging team membership into this test.
	api.On("GetChannel", mappedChannelID).Return(&model.Channel{Id: mappedChannelID, Type: model.ChannelTypeDirect}, nil).Maybe()

	createdPost := &model.Post{Id: model.NewId()}
	api.On("CreatePost", mock.Anything).Return(createdPost, nil).Once()

	stateKey := ghostUserID
	events := []MatrixEvent{
		{Type: "m.room.member", EventID: "$dm", RoomID: "!dm:" + serverDomain, Sender: matrixUserID, StateKey: &stateKey, Content: map[string]any{"membership": "join"}},
		{Type: "m.room.message", EventID: "$later", RoomID: mappedRoomID, Sender: "@bob:" + serverDomain, Content: map[string]any{"msgtype": "m.text", "body": "still delivered"}},
	}
	body, err := json.Marshal(MatrixTransaction{Events: events})
	require.NoError(t, err)

	txnID := "txn-" + model.NewId()
	w := httptest.NewRecorder()
	plugin.handleMatrixTransaction(w, transactionRequest(serverID, txnID, string(body)))

	assert.Equal(t, http.StatusOK, w.Result().StatusCode, "a permanently-failing event must be acknowledged, not retried forever")

	transactionsMutex.RLock()
	_, processed := processedTransactions[transactionKey{serverID: serverID, txnID: txnID}]
	transactionsMutex.RUnlock()
	assert.True(t, processed, "the transaction must be recorded so the homeserver stops redelivering it")

	api.AssertCalled(t, "CreatePost", mock.Anything)
	assert.Empty(t, kvstore.RoomIDForServer(mustParseMappings(t, plugin, dmChannelID), serverID), "the rejected DM must not have been mapped to this server")
}

func mustParseMappings(t *testing.T, plugin *Plugin, channelID string) []kvstore.ChannelServerMapping {
	t.Helper()
	raw, err := plugin.kvstore.Get(kvstore.BuildChannelMappingKey(channelID))
	require.NoError(t, err)
	mappings, err := kvstore.ParseChannelServerMappings(raw)
	require.NoError(t, err)
	return mappings
}

// TestCreateDMChannelForGhostUserReadFailure pins the distinction between "not our ghost
// user" and "the ghost user record could not be read". Both used to return ("", nil),
// which processMatrixEvent reads as an unmapped room - it acknowledges the transaction,
// the homeserver never redelivers it, and the Matrix-initiated DM is lost for good.
func TestCreateDMChannelForGhostUserReadFailure(t *testing.T) {
	const serverDomain = "a.example.com"

	setup := func(t *testing.T) (*Plugin, string, string) {
		t.Helper()
		plugin := newTestPluginForTransaction(t)
		client := createMatrixClientWithTestLogger(t, "https://"+serverDomain, "as", "")
		serverID, _ := registerTestServer(t, plugin, "https://"+serverDomain, serverDomain, client)

		mattermostUserID := model.NewId()
		ghostUserID := "@_mattermost_" + mattermostUserID + ":" + serverDomain
		plugin.kvstore = &erroringKVStore{
			KVStore:     plugin.kvstore,
			errOnGetKey: kvstore.BuildGhostUserKey(serverID, mattermostUserID),
		}
		return plugin, serverID, ghostUserID
	}

	t.Run("an unreadable ghost user record is an error, not a rejection", func(t *testing.T) {
		plugin, serverID, ghostUserID := setup(t)

		stateKey := ghostUserID
		event := MatrixEvent{
			Type:     "m.room.member",
			EventID:  "$dm",
			RoomID:   "!dm:" + serverDomain,
			Sender:   "@alice:" + serverDomain,
			StateKey: &stateKey,
			Content:  map[string]any{"membership": "join"},
		}

		channelID, err := plugin.handleMatrixMemberDM(serverID, event)

		require.Error(t, err, "a KV read failure must not be reported as \"not our ghost user\"")
		assert.Empty(t, channelID)
	})

	t.Run("the transaction is retried rather than acknowledged", func(t *testing.T) {
		plugin, serverID, ghostUserID := setup(t)

		body := `{"events":[{"type":"m.room.member","event_id":"$dm","room_id":"!dm:` + serverDomain +
			`","sender":"@alice:` + serverDomain + `","state_key":"` + ghostUserID +
			`","content":{"membership":"join"}}]}`

		txnID := "txn-" + model.NewId()
		w := httptest.NewRecorder()
		plugin.handleMatrixTransaction(w, transactionRequest(serverID, txnID, body))

		resp := w.Result()
		assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, "a lost DM must be retried, not acknowledged")
		assert.Equal(t, "5", resp.Header.Get("Retry-After"))

		transactionsMutex.RLock()
		_, processed := processedTransactions[transactionKey{serverID: serverID, txnID: txnID}]
		transactionsMutex.RUnlock()
		assert.False(t, processed, "the transaction must stay unrecorded so the homeserver redelivers it")
	})
}
