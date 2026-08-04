package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
	matrixtest "github.com/mattermost/mattermost-plugin-matrix-bridge/testcontainers/matrix"
)

// MultiServerIntegrationTestSuite exercises the plugin's multi-Matrix-server support
// (spec/2026-07-28-multi-server-support-backend.md §5.2) against two independent, real
// Synapse containers. Both containers are given distinct, port-less ServerNames
// ("synapse1.local"/"synapse2.local") rather than anything shaped like localhost:PORT -
// this is deliberate: StartMatrixContainer bakes ServerName verbatim into the generated
// homeserver.yaml, so two distinct names give genuinely independent Matrix ID domains,
// which is exactly what every isolation assertion below depends on.
type MultiServerIntegrationTestSuite struct {
	suite.Suite
	containerA *matrixtest.Container
	containerB *matrixtest.Container
}

// SetupSuite starts both Matrix containers sequentially (not in parallel - require/t.Fatal
// inside StartMatrixContainer is unsafe to call from a non-test goroutine) before any test
// runs, and hard-asserts their server domains differ so a mis-provisioned setup fails
// loudly here rather than manifesting as a confusing failure lower down.
func (suite *MultiServerIntegrationTestSuite) SetupSuite() {
	suite.containerA = matrixtest.StartMatrixContainer(suite.T(), matrixtest.MatrixTestConfig{ //nolint:gosec // test fixture tokens, not real credentials
		ServerName: "synapse1.local",
		ASToken:    "as_token_synapse1_12345",
		HSToken:    "hs_token_synapse1_67890",
	})
	suite.containerB = matrixtest.StartMatrixContainer(suite.T(), matrixtest.MatrixTestConfig{ //nolint:gosec // test fixture tokens, not real credentials
		ServerName: "synapse2.local",
		ASToken:    "as_token_synapse2_12345",
		HSToken:    "hs_token_synapse2_67890",
	})

	require.NotEqual(suite.T(), suite.containerA.ServerDomain, suite.containerB.ServerDomain,
		"test setup bug: containers A and B must have distinct Matrix server domains, or every isolation assertion in this suite is meaningless")
}

// TearDownSuite cleans up both Matrix containers after all tests have run.
func (suite *MultiServerIntegrationTestSuite) TearDownSuite() {
	if suite.containerB != nil {
		suite.containerB.Cleanup(suite.T())
	}
	if suite.containerA != nil {
		suite.containerA.Cleanup(suite.T())
	}
}

// twoServerSetup bundles a fresh Plugin wired to both containers A and B via
// registerTestServer, for tests that need two independent live servers without
// exercising AddServer/RemoveServer themselves (scenarios 1-3; scenario 4 builds its own
// plugin through the real AddServer/RemoveServer path instead).
type twoServerSetup struct {
	plugin     *Plugin
	api        *plugintest.API
	serverIDA  string
	remoteIDA  string
	serverIDB  string
	remoteIDB  string
	m2mxA      *MattermostToMatrixBridge
	mx2mA      *MatrixToMattermostBridge
	m2mxB      *MattermostToMatrixBridge
	mx2mB      *MatrixToMattermostBridge
	testUserID string
}

// newTwoServerSetup builds a Plugin with both containers registered as independent
// servers, each with its own Matrix client pointed at its own container - never
// cross-wired.
func (suite *MultiServerIntegrationTestSuite) newTwoServerSetup() *twoServerSetup {
	t := suite.T()
	api := &plugintest.API{}

	plugin := &Plugin{}
	plugin.SetAPI(api)
	plugin.kvstore = NewMemoryKVStore()
	plugin.pendingFiles = NewPendingFileTracker()
	plugin.postTracker = NewPostTracker(DefaultPostTrackerMaxEntries)
	plugin.configuration = &configuration{}
	plugin.logger = &testLogger{t: t}

	clientA := createMatrixClientWithTestLogger(t, suite.containerA.ServerURL, suite.containerA.ASToken, "")
	clientA.SetServerDomain(suite.containerA.ServerDomain)
	serverIDA, remoteIDA := registerTestServer(t, plugin, suite.containerA.ServerURL, suite.containerA.ServerDomain, clientA)

	clientB := createMatrixClientWithTestLogger(t, suite.containerB.ServerURL, suite.containerB.ASToken, "")
	clientB.SetServerDomain(suite.containerB.ServerDomain)
	serverIDB, remoteIDB := registerTestServer(t, plugin, suite.containerB.ServerURL, suite.containerB.ServerDomain, clientB)

	m2mxA, mx2mA := plugin.testBridges(t, serverIDA)
	m2mxB, mx2mB := plugin.testBridges(t, serverIDB)

	testUserID := model.NewId()
	setupBasicMocks(api, testUserID)

	return &twoServerSetup{
		plugin:     plugin,
		api:        api,
		serverIDA:  serverIDA,
		remoteIDA:  remoteIDA,
		serverIDB:  serverIDB,
		remoteIDB:  remoteIDB,
		m2mxA:      m2mxA,
		mx2mA:      mx2mA,
		m2mxB:      m2mxB,
		mx2mB:      mx2mB,
		testUserID: testUserID,
	}
}

// setServerHSToken overwrites serverID's HSToken in the registry. registerTestServer
// does not set one (it has no need to, for tests that never drive the real webhook
// path), but the real webhook auth path (MatrixAuthorizationRequired) matches
// "Bearer "+HSToken against every registered server, so a test that wants to exercise it
// for real must seed this itself.
func setServerHSToken(t *testing.T, plugin *Plugin, serverID, hsToken string) {
	t.Helper()

	servers, err := plugin.getServers()
	require.NoError(t, err)

	found := false
	for i := range servers {
		if servers[i].ServerID == serverID {
			servers[i].HSToken = hsToken
			found = true
		}
	}
	require.True(t, found, "server %s must be registered before setting its hs_token", serverID)

	data, err := kvstore.MarshalServersConfig(servers)
	require.NoError(t, err)
	require.NoError(t, plugin.kvstore.Set(kvstore.KeyServersConfig, data))
}

// TestInboundRoutingIsolatedPerServer covers scenario 1: an inbound Matrix message,
// delivered through the real webhook path (plugin.ServeHTTP, with each server's own
// Authorization: Bearer <hs_token>), must land only in the Mattermost channel mapped to
// the server it arrived on, and ghost recognition must use each server's own domain.
func (suite *MultiServerIntegrationTestSuite) TestInboundRoutingIsolatedPerServer() {
	t := suite.T()
	ts := suite.newTwoServerSetup()

	// The real webhook auth path matches on each server's own hs_token.
	setServerHSToken(t, ts.plugin, ts.serverIDA, suite.containerA.HSToken)
	setServerHSToken(t, ts.plugin, ts.serverIDB, suite.containerB.HSToken)

	logger, err := CreateTransactionLogger()
	require.NoError(t, err)
	ts.plugin.transactionLogger = logger

	channelA := model.NewId()
	channelB := model.NewId()
	roomA := suite.containerA.CreateRoom(t, generateUniqueRoomName("Inbound Isolation Room A"))
	roomB := suite.containerB.CreateRoom(t, generateUniqueRoomName("Inbound Isolation Room B"))

	// MapChannelToServer sets both the forward channel->room mapping and the reverse
	// room->channel mapping that processMatrixEvent's getChannelIDFromMatrixRoom needs
	// to route an inbound event back to a channel.
	require.NoError(t, ts.plugin.MapChannelToServer(ts.serverIDA, channelA, roomA))
	require.NoError(t, ts.plugin.MapChannelToServer(ts.serverIDB, channelB, roomB))

	api := ts.api
	api.On("GetConfig").Return(&model.Config{})
	api.On("GetChannel", channelA).Return(&model.Channel{Id: channelA, Type: model.ChannelTypeOpen, TeamId: ""}, nil)
	api.On("GetChannel", channelB).Return(&model.Channel{Id: channelB, Type: model.ChannelTypeOpen, TeamId: ""}, nil)

	// generateMattermostUsername probes for a free username via GetUserByUsername; report
	// every candidate as free so a brand-new ghost-adjacent user is created each time.
	api.On("GetUserByUsername", mock.AnythingOfType("string")).Return(nil, &model.AppError{Message: "not found"})

	var createdPosts []*model.Post
	api.On("CreateUser", mock.AnythingOfType("*model.User")).Return(func(u *model.User) *model.User {
		created := u.DeepCopy()
		created.Id = model.NewId()
		return created
	}, nil)
	api.On("CreatePost", mock.AnythingOfType("*model.Post")).Run(func(args mock.Arguments) {
		createdPosts = append(createdPosts, args.Get(0).(*model.Post))
	}).Return(func(post *model.Post) *model.Post {
		created := post.Clone()
		created.Id = model.NewId()
		return created
	}, nil)

	senderA := "@alice-" + model.NewId()[:8] + ":" + suite.containerA.ServerDomain
	senderB := "@bob-" + model.NewId()[:8] + ":" + suite.containerB.ServerDomain

	deliver := func(container *matrixtest.Container, roomID, sender, body, hsToken string) *http.Response {
		txnID := "txn-" + model.NewId()
		transaction := MatrixTransaction{
			Events: []MatrixEvent{{
				Type:      "m.room.message",
				EventID:   "$evt-" + model.NewId() + ":" + container.ServerDomain,
				Sender:    sender,
				RoomID:    roomID,
				Timestamp: time.Now().UnixMilli(),
				Content:   map[string]any{"msgtype": "m.text", "body": body},
			}},
		}
		payload, marshalErr := json.Marshal(transaction)
		require.NoError(t, marshalErr)

		req := httptest.NewRequest(http.MethodPut, "/_matrix/app/v1/transactions/"+txnID, bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+hsToken)
		w := httptest.NewRecorder()
		ts.plugin.ServeHTTP(nil, w, req)
		return w.Result()
	}

	// Deliver a genuine (non-ghost) message on each server, through the real HTTP
	// webhook path, authenticated with that server's own hs_token.
	respA := deliver(suite.containerA, roomA, senderA, "hello from real matrix server A", suite.containerA.HSToken)
	require.Equal(t, http.StatusOK, respA.StatusCode, "server A's transaction must be accepted")

	respB := deliver(suite.containerB, roomB, senderB, "hello from real matrix server B", suite.containerB.HSToken)
	require.Equal(t, http.StatusOK, respB.StatusCode, "server B's transaction must be accepted")

	// Processing is synchronous within ServeHTTP (handleMatrixTransaction processes every
	// event before writing the response), so no polling/wait is needed here - by the time
	// each deliver() call returns, CreatePost has already been called or not.
	require.Len(t, createdPosts, 2, "each server's message must create exactly one Mattermost post, and never more (e.g. no cross-delivery into the other channel)")

	var postForA, postForB *model.Post
	for _, p := range createdPosts {
		switch p.ChannelId {
		case channelA:
			postForA = p
		case channelB:
			postForB = p
		}
	}
	require.NotNil(t, postForA, "server A's message must land in the channel mapped to server A")
	require.NotNil(t, postForB, "server B's message must land in the channel mapped to server B")
	assert.Equal(t, "hello from real matrix server A", postForA.Message)
	assert.Equal(t, "hello from real matrix server B", postForB.Message)

	// Ghost recognition must use each server's own domain: a ghost-shaped user ID for
	// server A's domain must not be mistaken for a ghost on server B, and vice versa.
	mattermostUserID := model.NewId()
	ghostOnA := "@_mattermost_" + mattermostUserID + ":" + suite.containerA.ServerDomain
	ghostOnB := "@_mattermost_" + mattermostUserID + ":" + suite.containerB.ServerDomain
	assert.True(t, ts.plugin.isGhostUser(ts.serverIDA, ghostOnA), "server A must recognize its own ghost-shaped user ID")
	assert.False(t, ts.plugin.isGhostUser(ts.serverIDA, ghostOnB), "server A must not recognize a ghost-shaped user ID for server B's domain")
	assert.True(t, ts.plugin.isGhostUser(ts.serverIDB, ghostOnB), "server B must recognize its own ghost-shaped user ID")
	assert.False(t, ts.plugin.isGhostUser(ts.serverIDB, ghostOnA), "server B must not recognize a ghost-shaped user ID for server A's domain")
}

// TestOutboundSyncIsolatedPerServer covers scenario 2: a Mattermost post synced to a
// channel mapped to one server's room must appear in that server's room only, proven in
// both directions (A and B).
func (suite *MultiServerIntegrationTestSuite) TestOutboundSyncIsolatedPerServer() {
	t := suite.T()
	ts := suite.newTwoServerSetup()

	channelA := model.NewId()
	channelB := model.NewId()
	roomA := suite.containerA.CreateRoom(t, generateUniqueRoomName("Outbound Isolation Room A"))
	roomB := suite.containerB.CreateRoom(t, generateUniqueRoomName("Outbound Isolation Room B"))

	require.NoError(t, ts.plugin.MapChannelToServer(ts.serverIDA, channelA, roomA))
	require.NoError(t, ts.plugin.MapChannelToServer(ts.serverIDB, channelB, roomB))

	// Direction 1: a post synced to the channel mapped to server A must appear on A only.
	postA := &model.Post{
		Id:        model.NewId(),
		UserId:    ts.testUserID,
		ChannelId: channelA,
		Message:   "hello server A only",
		CreateAt:  time.Now().UnixMilli(),
	}
	require.NoError(t, ts.m2mxA.SyncPostToMatrix(postA, channelA))

	var eventOnA *matrixtest.Event
	require.Eventually(t, func() bool {
		events := suite.containerA.GetRoomEvents(t, roomA)
		eventOnA = matrixtest.FindEventByPostID(events, postA.Id)
		return eventOnA != nil
	}, 10*time.Second, 500*time.Millisecond, "post synced to server A's room must appear there")

	// Check server B only after the positive wait window above has already elapsed - a
	// wrong-room delivery on the same plugin process would require an actual bug, so a
	// single check after that window is sufficient to avoid a false-negative pass from
	// checking too early.
	eventsOnB := suite.containerB.GetRoomEvents(t, roomB)
	assert.Nil(t, matrixtest.FindEventByPostID(eventsOnB, postA.Id), "post synced to server A must never appear in server B's room")

	// Direction 2 (mirror): a post synced to the channel mapped to server B must appear
	// on B only.
	postB := &model.Post{
		Id:        model.NewId(),
		UserId:    ts.testUserID,
		ChannelId: channelB,
		Message:   "hello server B only",
		CreateAt:  time.Now().UnixMilli(),
	}
	require.NoError(t, ts.m2mxB.SyncPostToMatrix(postB, channelB))

	var eventOnB *matrixtest.Event
	require.Eventually(t, func() bool {
		events := suite.containerB.GetRoomEvents(t, roomB)
		eventOnB = matrixtest.FindEventByPostID(events, postB.Id)
		return eventOnB != nil
	}, 10*time.Second, 500*time.Millisecond, "post synced to server B's room must appear there")

	eventsOnA := suite.containerA.GetRoomEvents(t, roomA)
	assert.Nil(t, matrixtest.FindEventByPostID(eventsOnA, postB.Id), "post synced to server B must never appear in server A's room")
}

// TestChannelMappingRejectsSecondServer covers scenario 3: once a channel is mapped to
// server A, attempting to map the same channel to server B must be rejected with
// ErrChannelAlreadyMapped, server A's mapping must be left completely intact, and
// nothing must be persisted for server B as a result.
func (suite *MultiServerIntegrationTestSuite) TestChannelMappingRejectsSecondServer() {
	t := suite.T()
	ts := suite.newTwoServerSetup()

	channelID := model.NewId()
	roomA := suite.containerA.CreateRoom(t, generateUniqueRoomName("Rejection Test Room A"))
	roomB := suite.containerB.CreateRoom(t, generateUniqueRoomName("Rejection Test Room B"))

	_, err := ts.plugin.SetChannelMapping(channelID, ts.serverIDA, roomA)
	require.NoError(t, err, "mapping the channel to server A must succeed")

	// Attempt to map the SAME channel to server B through the same production choke
	// point. This must be rejected - the one-server-per-channel policy must not be
	// weakened or bypassed.
	_, err = ts.plugin.SetChannelMapping(channelID, ts.serverIDB, roomB)
	require.Error(t, err, "mapping an already-mapped channel to a second live server must fail")
	assert.True(t, errors.Is(err, ErrChannelAlreadyMapped), "the error must be (or wrap) ErrChannelAlreadyMapped")

	// Server A's mapping must be left completely intact.
	raw, err := ts.plugin.kvstore.Get(kvstore.BuildChannelMappingKey(channelID))
	require.NoError(t, err)
	mappings, err := kvstore.ParseChannelServerMappings(raw)
	require.NoError(t, err)
	require.Len(t, mappings, 1, "the rejected attempt must not have added a second entry")
	assert.Equal(t, ts.serverIDA, mappings[0].ServerID, "server A must still be the only mapped server")
	assert.Equal(t, roomA, kvstore.RoomIDForServer(mappings, ts.serverIDA), "server A's room ID must be unchanged")

	// Nothing must have been persisted for server B as a result of the rejected attempt.
	resolvedForB, err := ts.m2mxB.GetMatrixRoomID(channelID)
	require.NoError(t, err)
	assert.Empty(t, resolvedForB, "no error-free mapping to server B must ever have been persisted for this channel")

	// Nothing must have been created on server B's side either: its room's membership
	// (only the application service bot, from room creation) must be untouched.
	membersB := suite.containerB.GetRoomMembers(t, roomB)
	asBotB := suite.containerB.GetApplicationServiceBotUserID()
	for _, member := range membersB {
		assert.Equal(t, asBotB, member.UserID, "no additional Matrix user should have joined server B's room as a result of the rejected mapping attempt")
	}
}

// TestReAdoptionRoundTrip covers scenario 4: removing a server via the real
// Plugin.RemoveServer, then re-adding it via the real Plugin.AddServer with the same
// serverID, must restore its channel mapping and ghost user with no re-mapping and no
// duplicate ghost creation. Unlike scenarios 1-3, this test deliberately does not use
// registerTestServer - the entire point is to exercise the production AddServer/
// RemoveServer lifecycle for real.
func (suite *MultiServerIntegrationTestSuite) TestReAdoptionRoundTrip() {
	t := suite.T()
	api := &plugintest.API{}

	plugin := &Plugin{}
	plugin.SetAPI(api)
	plugin.kvstore = NewMemoryKVStore()
	plugin.pendingFiles = NewPendingFileTracker()
	plugin.postTracker = NewPostTracker(DefaultPostTrackerMaxEntries)
	plugin.configuration = &configuration{}
	plugin.logger = &testLogger{t: t}

	// Mocks for AddServer/RemoveServer's best-effort shared-channels side effects, plus
	// initMatrixClients' rate-limit-config logging on every real matrix.Client it builds.
	api.On("GetUserByUsername", "mattermost-bridge").Return(nil, &model.AppError{Message: "not found"}).Maybe()
	api.On("GetUsers", mock.Anything).Return([]*model.User{{Id: model.NewId()}}, nil).Maybe()
	api.On("RegisterPluginForSharedChannels", mock.Anything).Return("remote-"+model.NewId(), nil).Maybe()
	api.On("PublishPluginClusterEvent", mock.Anything, mock.Anything).Return(nil).Maybe()
	api.On("UnregisterPluginRemoteForSharedChannels", mock.Anything).Return(nil).Maybe()
	mockAnyLogCalls(api)

	testUserID := model.NewId()
	setupBasicMocks(api, testUserID)

	// Register server A for real, through the production AddServer path.
	serverIDA, err := plugin.AddServer(suite.containerA.ServerURL, suite.containerA.ASToken, suite.containerA.HSToken, "", "", suite.containerA.ServerDomain)
	require.NoError(t, err)
	require.NotNil(t, plugin.GetMatrixClientForServer(serverIDA), "AddServer's own refreshServersAndBroadcast must have populated this node's client cache")

	// Map a channel to A and sync one post, which also creates a ghost user for
	// testUserID on server A.
	channelID := model.NewId()
	roomID := suite.containerA.CreateRoom(t, generateUniqueRoomName("Re-adoption Test Room"))
	_, err = plugin.SetChannelMapping(channelID, serverIDA, roomID)
	require.NoError(t, err)

	m2mxA, err := plugin.newMattermostToMatrixBridge(serverIDA)
	require.NoError(t, err)

	post := &model.Post{
		Id:        model.NewId(),
		UserId:    testUserID,
		ChannelId: channelID,
		Message:   "hello before removal",
		CreateAt:  time.Now().UnixMilli(),
	}
	require.NoError(t, m2mxA.SyncPostToMatrix(post, channelID))

	ghostKey := kvstore.BuildGhostUserKey(serverIDA, testUserID)
	ghostUserID, err := plugin.kvstore.Get(ghostKey)
	require.NoError(t, err)
	require.NotEmpty(t, ghostUserID, "syncing the post must have created a ghost user for testUserID on server A")

	// Remove the server for real.
	removed, err := plugin.RemoveServer(serverIDA)
	require.NoError(t, err)
	require.True(t, removed)

	managedAfterRemoval, err := plugin.GetManagedServers()
	require.NoError(t, err)
	for _, s := range managedAfterRemoval {
		assert.NotEqual(t, serverIDA, s.ServerID, "the removed server must not appear in the managed server list")
	}

	// Sync must stop resolving for the removed server: this node's client cache no
	// longer has an entry for it, so building a bridge for it must fail.
	_, err = plugin.newMattermostToMatrixBridge(serverIDA)
	assert.Error(t, err, "no Matrix client should be resolvable for a removed server")
	assert.Nil(t, plugin.GetMatrixClientForServer(serverIDA), "RemoveServer's refreshServersAndBroadcast must have dropped this node's client cache entry")

	// Re-adopt at the same serverID. Passing "" for serverNameOverride exercises the same
	// real /_matrix/key/v2/server discovery AddServer would use for a brand new server -
	// containerA is a real, running Synapse, so this genuinely re-discovers
	// "synapse1.local" rather than short-circuiting on a hardcoded name.
	reAdoptedID, err := plugin.AddServer(suite.containerA.ServerURL, suite.containerA.ASToken, suite.containerA.HSToken, "", serverIDA, "")
	require.NoError(t, err)
	require.Equal(t, serverIDA, reAdoptedID, "AddServer must re-adopt the supplied serverID verbatim")

	// AddServer's own refreshServersAndBroadcast call already rebuilds this node's
	// matrixClients cache - no manual plugin.initMatrixClients() call is needed.
	require.NotNil(t, plugin.GetMatrixClientForServer(reAdoptedID), "re-adding the server must make a Matrix client available again for it")

	// The channel mapping must resolve to the same room again, with no re-mapping call.
	m2mxA2, err := plugin.newMattermostToMatrixBridge(reAdoptedID)
	require.NoError(t, err)
	resolvedRoomID, err := m2mxA2.GetMatrixRoomID(channelID)
	require.NoError(t, err)
	assert.Equal(t, roomID, resolvedRoomID, "the channel mapping must resolve to the same room again after re-adoption, with no re-mapping needed")

	// The previously-created ghost user must still be reachable under the re-adopted
	// serverID - no duplicate ghost must have been created.
	reAdoptedGhostUserID, err := plugin.kvstore.Get(kvstore.BuildGhostUserKey(reAdoptedID, testUserID))
	require.NoError(t, err)
	assert.Equal(t, string(ghostUserID), string(reAdoptedGhostUserID), "the ghost user's KV record must resolve to the same Matrix user ID after re-adoption, proving no duplicate ghost was created")
}

// TestMultiServerIntegrationTestSuite runs the multi-server integration suite.
func TestMultiServerIntegrationTestSuite(t *testing.T) {
	suite.Run(t, new(MultiServerIntegrationTestSuite))
}
