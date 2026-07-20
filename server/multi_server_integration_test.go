package main

import (
	"encoding/json"
	"strings"
	"testing"

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

// MultiServerIntegrationTestSuite verifies the multi-server backend plumbing —
// the client registry, serverID-namespaced KV, and per-server bridge operations —
// against two independent, live Synapse homeservers.
//
// Scope note: the plugin provides the multi-server backend plumbing but not
// cross-server routing (inbound/outbound server selection is still single-server).
// These tests therefore exercise the registry and per-server isolation directly,
// not an end-to-end message flow across the two servers.
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

	// Client registry with one client per homeserver.
	plugin.matrixClients = map[string]*matrix.Client{
		multiServerAID: suite.containerA.Client,
		multiServerBID: suite.containerB.Client,
	}
	plugin.serverID = multiServerAID // GetMatrixClient()/getSingleServerID() default

	// Registry entries mirroring what reconcileServerConfig would persist.
	servers := []kvstore.ServerConfig{
		{ServerID: multiServerAID, ServerURL: suite.containerA.ServerURL, ServerName: suite.containerA.ServerDomain, UsernamePrefix: "matrixa", Enabled: true, RemoteID: plugin.remoteID},
		{ServerID: multiServerBID, ServerURL: suite.containerB.ServerURL, ServerName: suite.containerB.ServerDomain, UsernamePrefix: "matrixb", Enabled: true, RemoteID: plugin.remoteID},
	}
	data, err := json.Marshal(servers)
	suite.Require().NoError(err)
	suite.Require().NoError(plugin.kvstore.Set(kvstore.KeyServersConfig, data))

	suite.plugin = plugin
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

func TestMultiServerIntegrationSuite(t *testing.T) {
	suite.Run(t, new(MultiServerIntegrationTestSuite))
}
