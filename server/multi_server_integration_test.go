package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/matrix"
	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
	matrixtest "github.com/mattermost/mattermost-plugin-matrix-bridge/testcontainers/matrix"
)

// Deterministic serverIDs and per-server shared-channels remote IDs for the
// two-server registry.
const (
	multiServerAID     = "serveraserveraserveraserv01"
	multiServerBID     = "serverbserverbserverbserv02"
	multiServerARemote = "remote-a"
	multiServerBRemote = "remote-b"
)

// MultiServerIntegrationTestSuite verifies multi-server behavior — the client
// registry, serverID-namespaced KV, per-server bridge operations, and inbound
// routing — against two independent, live Synapse homeservers.
//
// Scope note: both directions are now per-server. Inbound (Matrix→Mattermost) is
// dispatched to the originating homeserver by its hs_token (TestInbound* tests).
// Outbound (Mattermost→Matrix) is routed by the channel→server mapping and fans
// out to every server a channel is shared with (TestOutboundPostFansOut*).
type MultiServerIntegrationTestSuite struct {
	suite.Suite
	containerA *matrixtest.Container
	containerB *matrixtest.Container
	plugin     *Plugin
	api        *plugintest.API
}

func (suite *MultiServerIntegrationTestSuite) SetupSuite() {
	// Two independent homeservers with distinct domains (each gets its own
	// dynamically-assigned port from testcontainers).
	suite.containerA = matrixtest.StartMatrixContainer(suite.T(), matrixtest.MatrixTestConfig{ //nolint:gosec // test-only fixture tokens
		ServerName: "synapse-a.local",
		ASToken:    "as_token_server_a",
		HSToken:    "hs_token_server_a",
	})
	suite.containerB = matrixtest.StartMatrixContainer(suite.T(), matrixtest.MatrixTestConfig{ //nolint:gosec // test-only fixture tokens
		ServerName: "synapse-b.local",
		ASToken:    "as_token_server_b",
		HSToken:    "hs_token_server_b",
	})
	suite.containerA.Client.SetServerDomain(suite.containerA.ServerDomain)
	suite.containerB.Client.SetServerDomain(suite.containerB.ServerDomain)
}

func (suite *MultiServerIntegrationTestSuite) TearDownSuite() {
	if suite.containerA != nil {
		suite.containerA.Cleanup(suite.T())
	}
	if suite.containerB != nil {
		suite.containerB.Cleanup(suite.T())
	}
}

// SetupTest builds a plugin whose registry holds both servers, keyed by serverID.
func (suite *MultiServerIntegrationTestSuite) SetupTest() {
	suite.api = &plugintest.API{}
	suite.api.On("LogDebug", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()
	suite.api.On("LogInfo", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()
	suite.api.On("LogWarn", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()
	suite.api.On("LogError", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()

	plugin := &Plugin{}
	plugin.SetAPI(suite.api)
	plugin.logger = &testLogger{t: suite.T()}
	plugin.kvstore = NewMemoryKVStore()
	plugin.pendingFiles = NewPendingFileTracker()
	plugin.postTracker = NewPostTracker(DefaultPostTrackerMaxEntries)
	plugin.maxProfileImageSize = DefaultMaxProfileImageSize
	plugin.maxFileSize = DefaultMaxFileSize

	// EnableSync is the master gate checked by the inbound auth middleware.
	plugin.configuration = &configuration{EnableSync: true}

	// The transaction handler logs the raw body through this logger.
	transactionLogger, err := CreateTransactionLogger()
	suite.Require().NoError(err)
	plugin.transactionLogger = transactionLogger

	// Client registry with one client per homeserver.
	plugin.matrixClients = map[string]*matrix.Client{
		multiServerAID: suite.containerA.Client,
		multiServerBID: suite.containerB.Client,
	}
	// With two servers registered getSingleServerID is ambiguous (""); tests that
	// need a specific client use getMatrixClient(serverID) directly.

	// Registry entries mirroring what the plugin persists. Each server carries its
	// own hs_token (so inbound auth resolves the originating server) and its own
	// distinct remote ID (so loop attribution is per-server).
	servers := []kvstore.ServerConfig{
		{ServerID: multiServerAID, ServerURL: suite.containerA.ServerURL, ServerName: suite.containerA.ServerDomain, ASToken: suite.containerA.ASToken, HSToken: suite.containerA.HSToken, UsernamePrefix: "matrixa", Enabled: true, RemoteID: multiServerARemote, SiteURL: "https://" + suite.containerA.ServerDomain},
		{ServerID: multiServerBID, ServerURL: suite.containerB.ServerURL, ServerName: suite.containerB.ServerDomain, ASToken: suite.containerB.ASToken, HSToken: suite.containerB.HSToken, UsernamePrefix: "matrixb", Enabled: true, RemoteID: multiServerBRemote, SiteURL: "https://" + suite.containerB.ServerDomain},
	}
	data, err := json.Marshal(servers)
	suite.Require().NoError(err)
	suite.Require().NoError(plugin.kvstore.Set(kvstore.KeyServersConfig, data))

	// Populate the remote→server and own-remote maps as initMatrixClient would, so
	// inbound routing and loop prevention resolve per-server without a p.remoteID
	// fallback.
	plugin.remoteToServerID = map[string]string{multiServerARemote: multiServerAID, multiServerBRemote: multiServerBID}
	plugin.ownRemoteIDs = map[string]struct{}{multiServerARemote: {}, multiServerBRemote: {}}

	// The transaction handler reads the server config to bound the request body size.
	suite.api.On("GetConfig").Return(&model.Config{}).Maybe()

	suite.plugin = plugin
}

// putTransaction drives an Application Service transaction through the full HTTP
// stack (auth middleware + handler) using the given bearer token, returning the
// HTTP status code.
func (suite *MultiServerIntegrationTestSuite) putTransaction(bearerToken, txnID string, events []MatrixEvent) int {
	body, err := json.Marshal(MatrixTransaction{Events: events})
	suite.Require().NoError(err)

	req := httptest.NewRequest(http.MethodPut, "/_matrix/app/v1/transactions/"+txnID, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+bearerToken)
	rec := httptest.NewRecorder()
	suite.plugin.ServeHTTP(nil, rec, req)
	return rec.Code
}

// bridgeFor builds a MattermostToMatrixBridge scoped to a single server, as the
// plugin would when it constructs one bridge per configured server.
func (suite *MultiServerIntegrationTestSuite) bridgeFor(serverID string, client *matrix.Client) *MattermostToMatrixBridge {
	utils := NewBridgeUtils(BridgeUtilsConfig{
		Logger:              suite.plugin.logger,
		API:                 suite.plugin.API,
		KVStore:             suite.plugin.kvstore,
		MatrixClient:        client,
		ServerID:            serverID,
		RemoteID:            suite.plugin.remoteIDForServer(serverID),
		MaxProfileImageSize: DefaultMaxProfileImageSize,
		MaxFileSize:         DefaultMaxFileSize,
		ConfigGetter:        suite.plugin,
	})
	return NewMattermostToMatrixBridge(utils, suite.plugin.pendingFiles, suite.plugin.postTracker)
}

// TestClientRegistryRoutesToCorrectHomeserver verifies getMatrixClient returns the
// right client per serverID and that each client talks to its own homeserver.
func (suite *MultiServerIntegrationTestSuite) TestClientRegistryRoutesToCorrectHomeserver() {
	t := suite.T()

	assert.Same(t, suite.containerA.Client, suite.plugin.getMatrixClient(multiServerAID))
	assert.Same(t, suite.containerB.Client, suite.plugin.getMatrixClient(multiServerBID))
	assert.Nil(t, suite.plugin.getMatrixClient("no-such-server"))
	// The single-server accessor is ambiguous with two servers registered, so it
	// resolves to no client; callers must target a specific serverID.
	assert.Nil(t, suite.plugin.GetMatrixClient())

	// Both live homeservers are reachable through their registered clients.
	assert.NoError(t, suite.plugin.getMatrixClient(multiServerAID).TestConnection())
	assert.NoError(t, suite.plugin.getMatrixClient(multiServerBID).TestConnection())

	// A room created through each client lives on that client's homeserver, as
	// shown by the server domain embedded in the returned room ID.
	roomA, err := suite.plugin.getMatrixClient(multiServerAID).CreateRoom("room-on-a", "", suite.containerA.ServerDomain, true, "")
	require.NoError(t, err)
	roomB, err := suite.plugin.getMatrixClient(multiServerBID).CreateRoom("room-on-b", "", suite.containerB.ServerDomain, true, "")
	require.NoError(t, err)

	assert.True(t, strings.HasSuffix(roomA, ":"+suite.containerA.ServerDomain), "room A %q should live on %q", roomA, suite.containerA.ServerDomain)
	assert.True(t, strings.HasSuffix(roomB, ":"+suite.containerB.ServerDomain), "room B %q should live on %q", roomB, suite.containerB.ServerDomain)
	assert.NotEqual(t, roomA, roomB, "each client created a room on its own homeserver")
}

// TestKVNamespacingIsolatesServers verifies serverID-namespaced keys and the
// per-server channel_mapping value keep the two servers' data independent.
func (suite *MultiServerIntegrationTestSuite) TestKVNamespacingIsolatesServers() {
	t := suite.T()

	// One Mattermost channel bridged to a different room on each server.
	channelID := model.NewId()
	roomA := "!roomA:" + suite.containerA.ServerDomain
	roomB := "!roomB:" + suite.containerB.ServerDomain
	value, err := kvstore.MarshalChannelServerMappings([]kvstore.ChannelServerMapping{
		{ServerID: multiServerAID, RoomID: roomA},
		{ServerID: multiServerBID, RoomID: roomB},
	})
	require.NoError(t, err)
	require.NoError(t, suite.plugin.kvstore.Set(kvstore.BuildChannelMappingKey(channelID), value))

	// Each per-server bridge resolves only its own server's room.
	bridgeA := suite.bridgeFor(multiServerAID, suite.containerA.Client)
	bridgeB := suite.bridgeFor(multiServerBID, suite.containerB.Client)

	gotA, err := bridgeA.GetMatrixRoomID(channelID)
	require.NoError(t, err)
	assert.Equal(t, roomA, gotA)

	gotB, err := bridgeB.GetMatrixRoomID(channelID)
	require.NoError(t, err)
	assert.Equal(t, roomB, gotB)

	// The same Matrix user ID maps independently under each server's namespace.
	matrixUserID := "@shared:example.org"
	require.NoError(t, suite.plugin.kvstore.Set(kvstore.BuildMatrixUserKey(multiServerAID, matrixUserID), []byte("mmuser-on-a")))
	require.NoError(t, suite.plugin.kvstore.Set(kvstore.BuildMatrixUserKey(multiServerBID, matrixUserID), []byte("mmuser-on-b")))

	valA, err := suite.plugin.kvstore.Get(kvstore.BuildMatrixUserKey(multiServerAID, matrixUserID))
	require.NoError(t, err)
	valB, err := suite.plugin.kvstore.Get(kvstore.BuildMatrixUserKey(multiServerBID, matrixUserID))
	require.NoError(t, err)
	assert.Equal(t, "mmuser-on-a", string(valA))
	assert.Equal(t, "mmuser-on-b", string(valB))
}

// TestGhostUsersAreCreatedPerServer verifies that creating a ghost user for the
// same Mattermost user on each server produces distinct ghosts on the respective
// homeservers, cached under serverID-namespaced keys.
func (suite *MultiServerIntegrationTestSuite) TestGhostUsersAreCreatedPerServer() {
	t := suite.T()

	mmUserID := model.NewId()
	suite.api.On("GetUser", mmUserID).Return(&model.User{
		Id:       mmUserID,
		Username: "alice",
		Nickname: "Alice",
		Email:    "alice@example.com",
	}, nil)
	suite.api.On("GetProfileImage", mmUserID).Return([]byte("fake-image-data"), nil)

	bridgeA := suite.bridgeFor(multiServerAID, suite.containerA.Client)
	bridgeB := suite.bridgeFor(multiServerBID, suite.containerB.Client)

	ghostA, err := bridgeA.CreateOrGetGhostUser(mmUserID)
	require.NoError(t, err)
	ghostB, err := bridgeB.CreateOrGetGhostUser(mmUserID)
	require.NoError(t, err)

	// Distinct ghost users, each on its own homeserver.
	assert.True(t, strings.HasPrefix(ghostA, "@_mattermost_"), "ghost A %q", ghostA)
	assert.True(t, strings.HasSuffix(ghostA, ":"+suite.containerA.ServerDomain), "ghost A %q on server A", ghostA)
	assert.True(t, strings.HasSuffix(ghostB, ":"+suite.containerB.ServerDomain), "ghost B %q on server B", ghostB)
	assert.NotEqual(t, ghostA, ghostB)

	// The ghost cache is namespaced per server.
	cachedA, err := suite.plugin.kvstore.Get(kvstore.BuildGhostUserKey(multiServerAID, mmUserID))
	require.NoError(t, err)
	cachedB, err := suite.plugin.kvstore.Get(kvstore.BuildGhostUserKey(multiServerBID, mmUserID))
	require.NoError(t, err)
	assert.Equal(t, ghostA, string(cachedA))
	assert.Equal(t, ghostB, string(cachedB))

	// Re-requesting returns the cached ghost per server (no duplicate creation).
	ghostA2, err := bridgeA.CreateOrGetGhostUser(mmUserID)
	require.NoError(t, err)
	assert.Equal(t, ghostA, ghostA2)
}

// TestInboundAuthRoutesByHSToken verifies the auth middleware resolves the
// originating server from the presented hs_token and rejects unknown tokens.
func (suite *MultiServerIntegrationTestSuite) TestInboundAuthRoutesByHSToken() {
	t := suite.T()

	// A probe handler records the serverID the middleware injected into the context.
	var resolvedServerID string
	var handlerRan bool
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerRan = true
		resolvedServerID, _ = serverIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	handler := suite.plugin.MatrixAuthorizationRequired(probe)

	serve := func(bearer string) (int, string, bool) {
		resolvedServerID, handlerRan = "", false
		req := httptest.NewRequest(http.MethodPut, "/_matrix/app/v1/transactions/txn1", nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code, resolvedServerID, handlerRan
	}

	// Each homeserver's real hs_token resolves to its own serverID.
	status, serverID, ran := serve(suite.containerA.HSToken)
	assert.Equal(t, http.StatusOK, status)
	assert.True(t, ran)
	assert.Equal(t, multiServerAID, serverID)

	status, serverID, ran = serve(suite.containerB.HSToken)
	assert.Equal(t, http.StatusOK, status)
	assert.True(t, ran)
	assert.Equal(t, multiServerBID, serverID)

	// An unknown token is rejected and the handler never runs.
	status, _, ran = serve("not-a-valid-token")
	assert.Equal(t, http.StatusUnauthorized, status)
	assert.False(t, ran)
}

// TestInboundTransactionDedupIsPerServer verifies that the same txnID from two
// different servers is processed independently, while a repeat from the same
// server is deduplicated.
func (suite *MultiServerIntegrationTestSuite) TestInboundTransactionDedupIsPerServer() {
	t := suite.T()

	// Isolate this test from any residual state in the process-global map.
	transactionsMutex.Lock()
	processedTransactions = make(map[txnKey]time.Time)
	transactionsMutex.Unlock()

	const sharedTxnID = "shared-txn-id-across-servers"

	hasKey := func(serverID string) bool {
		transactionsMutex.RLock()
		defer transactionsMutex.RUnlock()
		_, ok := processedTransactions[txnKey{serverID: serverID, txnID: sharedTxnID}]
		return ok
	}

	// Empty-event transactions exercise dedup without any Mattermost side effects.
	assert.Equal(t, http.StatusOK, suite.putTransaction(suite.containerA.HSToken, sharedTxnID, nil))
	assert.True(t, hasKey(multiServerAID), "server A transaction should be recorded")
	assert.False(t, hasKey(multiServerBID), "server B must not be marked by server A's transaction")

	// The same txnID from server B is a distinct key, not a duplicate.
	assert.Equal(t, http.StatusOK, suite.putTransaction(suite.containerB.HSToken, sharedTxnID, nil))
	assert.True(t, hasKey(multiServerBID), "server B transaction should be recorded independently")

	// A repeat from server A hits the per-(serverID,txnID) dedup path and is a no-op.
	firstTime := func() time.Time {
		transactionsMutex.RLock()
		defer transactionsMutex.RUnlock()
		return processedTransactions[txnKey{serverID: multiServerAID, txnID: sharedTxnID}]
	}()
	assert.Equal(t, http.StatusOK, suite.putTransaction(suite.containerA.HSToken, sharedTxnID, nil))
	assert.Equal(t, firstTime, func() time.Time {
		transactionsMutex.RLock()
		defer transactionsMutex.RUnlock()
		return processedTransactions[txnKey{serverID: multiServerAID, txnID: sharedTxnID}]
	}(), "duplicate transaction must not update the recorded timestamp")
}

func TestMultiServerIntegrationSuite(t *testing.T) {
	suite.Run(t, new(MultiServerIntegrationTestSuite))
}
