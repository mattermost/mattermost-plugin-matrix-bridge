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

// Deterministic serverIDs for the two-server registry.
const (
	multiServerAID = "serveraserveraserveraserv01"
	multiServerBID = "serverbserverbserverbserv02"
)

// MultiServerIntegrationTestSuite verifies multi-server behavior — the client
// registry, serverID-namespaced KV, per-server bridge operations, and inbound
// routing — against two independent, live Synapse homeservers.
//
// Scope note: inbound (Matrix→Mattermost) routing is now per-server — traffic is
// dispatched to the originating homeserver by its hs_token, as the TestInbound*
// tests exercise end-to-end. Outbound (Mattermost→Matrix) routing is still
// single-server (a later phase); the outbound tests here therefore drive per-server
// bridges directly rather than through channel-based server selection.
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
	suite.containerA = matrixtest.StartMatrixContainer(suite.T(), matrixtest.MatrixTestConfig{
		ServerName: "synapse-a.local",
		ASToken:    "as_token_server_a",
		HSToken:    "hs_token_server_a",
	})
	suite.containerB = matrixtest.StartMatrixContainer(suite.T(), matrixtest.MatrixTestConfig{
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

	plugin := &Plugin{remoteID: "test-remote-id"}
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
	plugin.serverID = multiServerAID // GetMatrixClient()/getSingleServerID() default

	// Registry entries mirroring what reconcileServerConfig would persist. Each
	// server carries its own hs_token so the auth middleware can resolve the
	// originating server from the presented bearer token.
	servers := []kvstore.ServerConfig{
		{ServerID: multiServerAID, ServerURL: suite.containerA.ServerURL, ServerName: suite.containerA.ServerDomain, ASToken: suite.containerA.ASToken, HSToken: suite.containerA.HSToken, UsernamePrefix: "matrixa", Enabled: true, RemoteID: plugin.remoteID},
		{ServerID: multiServerBID, ServerURL: suite.containerB.ServerURL, ServerName: suite.containerB.ServerDomain, ASToken: suite.containerB.ASToken, HSToken: suite.containerB.HSToken, UsernamePrefix: "matrixb", Enabled: true, RemoteID: plugin.remoteID},
	}
	data, err := json.Marshal(servers)
	suite.Require().NoError(err)
	suite.Require().NoError(plugin.kvstore.Set(kvstore.KeyServersConfig, data))

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
		RemoteID:            suite.plugin.remoteID,
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
	// The single-server accessor resolves the cached serverID (server A).
	assert.Same(t, suite.containerA.Client, suite.plugin.GetMatrixClient())

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

// TestInboundMessageRoutesToOriginatingServerNamespace verifies that an inbound
// message authenticated with one server's hs_token is processed against that
// server's KV namespace, and that a message for a room only mapped on server A
// is ignored when presented as server B (lookups are server-scoped).
func (suite *MultiServerIntegrationTestSuite) TestInboundMessageRoutesToOriginatingServerNamespace() {
	t := suite.T()

	channelID := model.NewId()
	mmUserID := model.NewId()
	sender := "@router_probe:" + suite.containerA.ServerDomain
	roomA := "!routed-room-a:" + suite.containerA.ServerDomain

	// The room is mapped to a channel only under server A's namespace.
	require.NoError(t, suite.plugin.kvstore.Set(kvstore.BuildRoomMappingKey(multiServerAID, roomA), []byte(channelID)))
	// The sender is already mapped to a Mattermost user under server A, so no ghost
	// user is created; the profile refresh against server A's live client is
	// best-effort and no-ops for this synthetic user.
	require.NoError(t, suite.plugin.kvstore.Set(kvstore.BuildMatrixUserKey(multiServerAID, sender), []byte(mmUserID)))

	suite.api.On("GetUser", mmUserID).Return(&model.User{Id: mmUserID, Username: "router_probe"}, nil).Maybe()
	suite.api.On("GetChannel", channelID).Return(&model.Channel{Id: channelID, TeamId: ""}, nil).Maybe()
	var createdPost *model.Post
	suite.api.On("CreatePost", mock.AnythingOfType("*model.Post")).Return(func(post *model.Post) *model.Post {
		if post.Id == "" {
			post.Id = model.NewId()
		}
		createdPost = post
		return post
	}, nil).Maybe()

	event := MatrixEvent{
		Type:      "m.room.message",
		EventID:   "$routed-event-a",
		Sender:    sender,
		RoomID:    roomA,
		Content:   map[string]any{"msgtype": "m.text", "body": "hello from server A"},
		Timestamp: 1,
	}

	// Presented as server A: the message is synced and its event→post mapping is
	// written under server A's namespace only.
	require.Equal(t, http.StatusOK, suite.putTransaction(suite.containerA.HSToken, "txn-routing-a", []MatrixEvent{event}))
	require.NotNil(t, createdPost, "server A message should create a Mattermost post")
	assert.Equal(t, channelID, createdPost.ChannelId)

	postMappingA, err := suite.plugin.kvstore.Get(kvstore.BuildMatrixEventPostKey(multiServerAID, event.EventID))
	require.NoError(t, err)
	assert.Equal(t, createdPost.Id, string(postMappingA), "event→post mapping should be stored under server A")

	postMappingB, err := suite.plugin.kvstore.Get(kvstore.BuildMatrixEventPostKey(multiServerBID, event.EventID))
	require.NoError(t, err)
	assert.Empty(t, postMappingB, "server B namespace must be untouched")

	// Presented as server B: the same room is unmapped in server B's namespace, so
	// the event is ignored and no additional post is created.
	createdPost = nil
	require.Equal(t, http.StatusOK, suite.putTransaction(suite.containerB.HSToken, "txn-routing-b", []MatrixEvent{event}))
	assert.Nil(t, createdPost, "server B has no mapping for this room, so no post should be created")
}

// TestInboundMessageDeliversToSecondServer is the symmetric counterpart to the
// routing test above: it proves an inbound message authenticated with server B's
// hs_token is actually *delivered* (a post is created) and that its state lands in
// server B's namespace, never server A's. Without this, a bug binding the inbound
// bridge to the wrong client/namespace would pass as long as server A worked.
func (suite *MultiServerIntegrationTestSuite) TestInboundMessageDeliversToSecondServer() {
	t := suite.T()

	channelID := model.NewId()
	mmUserID := model.NewId()
	sender := "@router_probe_b:" + suite.containerB.ServerDomain
	roomB := "!routed-room-b:" + suite.containerB.ServerDomain

	// Map the room and sender only under server B's namespace.
	require.NoError(t, suite.plugin.kvstore.Set(kvstore.BuildRoomMappingKey(multiServerBID, roomB), []byte(channelID)))
	require.NoError(t, suite.plugin.kvstore.Set(kvstore.BuildMatrixUserKey(multiServerBID, sender), []byte(mmUserID)))

	suite.api.On("GetUser", mmUserID).Return(&model.User{Id: mmUserID, Username: "router_probe_b"}, nil).Maybe()
	suite.api.On("GetChannel", channelID).Return(&model.Channel{Id: channelID, TeamId: ""}, nil).Maybe()
	var createdPost *model.Post
	suite.api.On("CreatePost", mock.AnythingOfType("*model.Post")).Return(func(post *model.Post) *model.Post {
		if post.Id == "" {
			post.Id = model.NewId()
		}
		createdPost = post
		return post
	}, nil).Maybe()

	event := MatrixEvent{
		Type:      "m.room.message",
		EventID:   "$routed-event-b",
		Sender:    sender,
		RoomID:    roomB,
		Content:   map[string]any{"msgtype": "m.text", "body": "hello from server B"},
		Timestamp: 1,
	}

	require.Equal(t, http.StatusOK, suite.putTransaction(suite.containerB.HSToken, "txn-b-deliver", []MatrixEvent{event}))
	require.NotNil(t, createdPost, "server B message should create a Mattermost post")
	assert.Equal(t, channelID, createdPost.ChannelId)

	postMappingB, err := suite.plugin.kvstore.Get(kvstore.BuildMatrixEventPostKey(multiServerBID, event.EventID))
	require.NoError(t, err)
	assert.Equal(t, createdPost.Id, string(postMappingB), "event→post mapping should be stored under server B")

	postMappingA, err := suite.plugin.kvstore.Get(kvstore.BuildMatrixEventPostKey(multiServerAID, event.EventID))
	require.NoError(t, err)
	assert.Empty(t, postMappingA, "server A namespace must be untouched")
}

// TestInboundMessagesFromBothServersRouteToTheirOwnChannels is the definitive
// end-to-end check: with two live homeservers configured at once, and two distinct
// Mattermost channels each bridged to a room on a *different* server, an inbound
// message from each server must land in that server's channel and only that channel.
// It cross-checks both the positive delivery (A→channelA, B→channelB) and the
// negative (A's event never touches channelB or server B's namespace, and vice
// versa) within a single scenario.
func (suite *MultiServerIntegrationTestSuite) TestInboundMessagesFromBothServersRouteToTheirOwnChannels() {
	t := suite.T()

	// Two distinct channels, each mapped to a room on a different server's namespace.
	channelA := model.NewId()
	channelB := model.NewId()
	require.NotEqual(t, channelA, channelB)

	userA := model.NewId()
	userB := model.NewId()
	senderA := "@sender_a:" + suite.containerA.ServerDomain
	senderB := "@sender_b:" + suite.containerB.ServerDomain
	roomA := "!both-room-a:" + suite.containerA.ServerDomain
	roomB := "!both-room-b:" + suite.containerB.ServerDomain
	const eventAID = "$both-event-a"
	const eventBID = "$both-event-b"

	require.NoError(t, suite.plugin.kvstore.Set(kvstore.BuildRoomMappingKey(multiServerAID, roomA), []byte(channelA)))
	require.NoError(t, suite.plugin.kvstore.Set(kvstore.BuildRoomMappingKey(multiServerBID, roomB), []byte(channelB)))
	require.NoError(t, suite.plugin.kvstore.Set(kvstore.BuildMatrixUserKey(multiServerAID, senderA), []byte(userA)))
	require.NoError(t, suite.plugin.kvstore.Set(kvstore.BuildMatrixUserKey(multiServerBID, senderB), []byte(userB)))

	suite.api.On("GetUser", userA).Return(&model.User{Id: userA, Username: "sender_a"}, nil).Maybe()
	suite.api.On("GetUser", userB).Return(&model.User{Id: userB, Username: "sender_b"}, nil).Maybe()
	suite.api.On("GetChannel", channelA).Return(&model.Channel{Id: channelA, TeamId: ""}, nil).Maybe()
	suite.api.On("GetChannel", channelB).Return(&model.Channel{Id: channelB, TeamId: ""}, nil).Maybe()

	// Capture every created post so we can look each up by ID afterwards.
	createdPosts := map[string]*model.Post{}
	suite.api.On("CreatePost", mock.AnythingOfType("*model.Post")).Return(func(post *model.Post) *model.Post {
		if post.Id == "" {
			post.Id = model.NewId()
		}
		createdPosts[post.Id] = post
		return post
	}, nil).Maybe()

	msg := func(eventID, sender, roomID, body string) MatrixEvent {
		return MatrixEvent{
			Type:      "m.room.message",
			EventID:   eventID,
			Sender:    sender,
			RoomID:    roomID,
			Content:   map[string]any{"msgtype": "m.text", "body": body},
			Timestamp: 1,
		}
	}

	// Deliver one message from each server, each authenticated with its own hs_token.
	require.Equal(t, http.StatusOK, suite.putTransaction(suite.containerA.HSToken, "txn-both-a", []MatrixEvent{msg(eventAID, senderA, roomA, "from A")}))
	require.Equal(t, http.StatusOK, suite.putTransaction(suite.containerB.HSToken, "txn-both-b", []MatrixEvent{msg(eventBID, senderB, roomB, "from B")}))

	// Resolve each event to its post via the originating server's namespace.
	postIDA, err := suite.plugin.kvstore.Get(kvstore.BuildMatrixEventPostKey(multiServerAID, eventAID))
	require.NoError(t, err)
	postIDB, err := suite.plugin.kvstore.Get(kvstore.BuildMatrixEventPostKey(multiServerBID, eventBID))
	require.NoError(t, err)
	require.NotEmpty(t, postIDA, "server A event should map to a post")
	require.NotEmpty(t, postIDB, "server B event should map to a post")

	postA := createdPosts[string(postIDA)]
	postB := createdPosts[string(postIDB)]
	require.NotNil(t, postA, "server A message should create a post")
	require.NotNil(t, postB, "server B message should create a post")

	// Positive: each message landed in its own server's channel.
	assert.Equal(t, channelA, postA.ChannelId, "server A message routes to channel A")
	assert.Equal(t, "from A", postA.Message)
	assert.Equal(t, channelB, postB.ChannelId, "server B message routes to channel B")
	assert.Equal(t, "from B", postB.Message)

	// Negative cross-checks: neither message leaked into the other channel...
	assert.NotEqual(t, channelB, postA.ChannelId, "server A message must NOT land in channel B")
	assert.NotEqual(t, channelA, postB.ChannelId, "server B message must NOT land in channel A")

	// ...and neither event appears in the other server's namespace.
	crossA, err := suite.plugin.kvstore.Get(kvstore.BuildMatrixEventPostKey(multiServerBID, eventAID))
	require.NoError(t, err)
	assert.Empty(t, crossA, "server A's event must not appear under server B's namespace")
	crossB, err := suite.plugin.kvstore.Get(kvstore.BuildMatrixEventPostKey(multiServerAID, eventBID))
	require.NoError(t, err)
	assert.Empty(t, crossB, "server B's event must not appear under server A's namespace")
}

// TestInboundReactionRoutesToOriginatingServerNamespace verifies that an inbound
// reaction resolves its target post and stores its mapping within the originating
// server's namespace, and that the same reaction presented as a different server
// does not resolve across namespaces.
func (suite *MultiServerIntegrationTestSuite) TestInboundReactionRoutesToOriginatingServerNamespace() {
	t := suite.T()

	channelID := model.NewId()
	postID := model.NewId()
	reactorMMUserID := model.NewId()
	reactor := "@reactor:" + suite.containerA.ServerDomain
	roomA := "!reaction-room-a:" + suite.containerA.ServerDomain
	const targetEventID = "$reaction-target-a"
	const reactionEventID = "$reaction-event-a"

	// Seed, under server A's namespace: the room mapping, the already-synced target
	// post, and the reactor's user mapping (so no ghost creation is needed).
	require.NoError(t, suite.plugin.kvstore.Set(kvstore.BuildRoomMappingKey(multiServerAID, roomA), []byte(channelID)))
	require.NoError(t, suite.plugin.kvstore.Set(kvstore.BuildMatrixEventPostKey(multiServerAID, targetEventID), []byte(postID)))
	require.NoError(t, suite.plugin.kvstore.Set(kvstore.BuildMatrixUserKey(multiServerAID, reactor), []byte(reactorMMUserID)))

	suite.api.On("GetUser", reactorMMUserID).Return(&model.User{Id: reactorMMUserID, Username: "reactor"}, nil).Maybe()
	suite.api.On("GetChannel", channelID).Return(&model.Channel{Id: channelID, TeamId: ""}, nil).Maybe()
	var addedReaction *model.Reaction
	suite.api.On("AddReaction", mock.AnythingOfType("*model.Reaction")).Return(func(r *model.Reaction) *model.Reaction {
		addedReaction = r
		return r
	}, nil).Maybe()

	reaction := MatrixEvent{
		Type:    "m.reaction",
		EventID: reactionEventID,
		Sender:  reactor,
		RoomID:  roomA,
		Content: map[string]any{
			"m.relates_to": map[string]any{
				"rel_type": "m.annotation",
				"event_id": targetEventID,
				"key":      "👍",
			},
		},
		Timestamp: 2,
	}

	// Presented as server A: the reaction resolves the target post and is stored
	// under server A's namespace.
	require.Equal(t, http.StatusOK, suite.putTransaction(suite.containerA.HSToken, "txn-reaction-a", []MatrixEvent{reaction}))
	require.NotNil(t, addedReaction, "server A reaction should be applied")
	assert.Equal(t, postID, addedReaction.PostId)

	stored, err := suite.plugin.kvstore.Get(kvstore.BuildMatrixReactionKey(multiServerAID, reactionEventID))
	require.NoError(t, err)
	assert.NotEmpty(t, stored, "reaction mapping should be stored under server A")

	// Presented as server B: the target post is not in server B's namespace, so the
	// reaction does not resolve and nothing is stored under server B.
	addedReaction = nil
	require.NoError(t, suite.plugin.kvstore.Set(kvstore.BuildRoomMappingKey(multiServerBID, roomA), []byte(channelID)))
	require.Equal(t, http.StatusOK, suite.putTransaction(suite.containerB.HSToken, "txn-reaction-b", []MatrixEvent{reaction}))
	assert.Nil(t, addedReaction, "server B cannot resolve the target post, so no reaction is applied")

	storedB, err := suite.plugin.kvstore.Get(kvstore.BuildMatrixReactionKey(multiServerBID, reactionEventID))
	require.NoError(t, err)
	assert.Empty(t, storedB, "server B namespace must not hold a reaction mapping")
}

func TestMultiServerIntegrationSuite(t *testing.T) {
	suite.Run(t, new(MultiServerIntegrationTestSuite))
}
