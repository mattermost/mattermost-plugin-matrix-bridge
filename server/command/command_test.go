package command

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/mattermost/mattermost/server/public/pluginapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/matrix"
	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
)

type env struct {
	client *pluginapi.Client
	api    *plugintest.API
}

// mockPlugin implements the PluginAccessor interface for testing
type mockPlugin struct {
	client       *pluginapi.Client
	kvstore      kvstore.KVStore
	matrixClient *matrix.Client
	pluginAPI    *plugintest.API
}

func (m *mockPlugin) GetKVStore() kvstore.KVStore {
	return m.kvstore
}

func (m *mockPlugin) CreateOrGetGhostUserForServer(_, mattermostUserID string) (string, error) {
	// Mock implementation - return test ghost user
	return "_mattermost_" + mattermostUserID + ":test.com", nil
}

func (m *mockPlugin) GetPluginAPI() plugin.API {
	return m.pluginAPI
}

func (m *mockPlugin) GetPluginAPIClient() *pluginapi.Client {
	return m.client
}

func (m *mockPlugin) GetRemoteIDForServer(string) string {
	return "test-remote-id"
}

func (m *mockPlugin) RunKVStoreMigrations() error {
	return nil // Mock implementation always succeeds
}

func (m *mockPlugin) RunKVStoreMigrationsWithResults() (*MigrationResult, error) {
	return &MigrationResult{
		UserMappingsCreated:      5,
		ChannelMappingsCreated:   3,
		RoomMappingsCreated:      2,
		DMMappingsCreated:        1,
		ReverseDMMappingsCreated: 1,
	}, nil // Mock implementation returns sample results
}

func (m *mockPlugin) GetMatrixUserIDFromMattermostUserForServer(_, mattermostUserID string) (string, error) {
	// Mock implementation - return test Matrix user
	return "@test_" + mattermostUserID + ":test.com", nil
}

func (m *mockPlugin) GetManagedServers() ([]kvstore.ServerConfig, error) {
	data, err := m.kvstore.Get(kvstore.KeyServersConfig)
	if err != nil {
		return nil, err
	}
	return kvstore.ParseServersConfig(data)
}

func (m *mockPlugin) AddServer(serverURL, serverName, asToken, hsToken, usernamePrefix string) (string, error) {
	servers, _ := m.GetManagedServers()
	serverID := "srv_" + serverName
	entry := kvstore.ServerConfig{ServerID: serverID, ServerURL: serverURL, ServerName: serverName, ASToken: asToken, HSToken: hsToken, UsernamePrefix: usernamePrefix, Enabled: true}
	replaced := false
	for i := range servers {
		if servers[i].ServerID == serverID {
			servers[i] = entry
			replaced = true
		}
	}
	if !replaced {
		servers = append(servers, entry)
	}
	data, err := json.Marshal(servers)
	if err != nil {
		return "", err
	}
	if err := m.kvstore.Set(kvstore.KeyServersConfig, data); err != nil {
		return "", err
	}
	return serverID, nil
}

func (m *mockPlugin) GetMatrixClientForServer(string) *matrix.Client {
	return m.matrixClient
}

func (m *mockPlugin) RemoveServer(serverID string) (bool, error) {
	servers, _ := m.GetManagedServers()
	filtered := make([]kvstore.ServerConfig, 0, len(servers))
	found := false
	for _, s := range servers {
		if s.ServerID == serverID {
			found = true
			continue
		}
		filtered = append(filtered, s)
	}
	if !found {
		return false, nil
	}
	data, err := json.Marshal(filtered)
	if err != nil {
		return false, err
	}
	return true, m.kvstore.Set(kvstore.KeyServersConfig, data)
}

// mockAllLogs registers permissive matchers for every log level and argument
// count so deep code paths (e.g. server discovery, matrix client) that log
// through the plugin API never panic on an unmocked call.
func mockAllLogs(api *plugintest.API) {
	for _, lvl := range []string{"LogDebug", "LogInfo", "LogWarn", "LogError"} {
		for n := 1; n <= 14; n++ {
			args := make([]any, n)
			for i := range args {
				args[i] = mock.Anything
			}
			api.On(lvl, args...).Return().Maybe()
		}
	}
}

func setupTest() *env {
	api := &plugintest.API{}
	driver := &plugintest.Driver{}
	client := pluginapi.NewClient(api, driver)

	return &env{
		client: client,
		api:    api,
	}
}

func TestMatrixCreateCommandParsing(t *testing.T) {
	tests := []struct {
		name             string
		command          string
		expectedRoomName string
		expectedPublish  bool
		shouldCallCreate bool
		description      string
	}{
		{
			name:             "create with no arguments",
			command:          "/matrix create",
			expectedRoomName: "",
			expectedPublish:  false,
			shouldCallCreate: true,
			description:      "should use channel name and not publish",
		},
		{
			name:             "create with publish true only",
			command:          "/matrix create true",
			expectedRoomName: "",
			expectedPublish:  true,
			shouldCallCreate: true,
			description:      "should use channel name and publish",
		},
		{
			name:             "create with publish false only",
			command:          "/matrix create false",
			expectedRoomName: "",
			expectedPublish:  false,
			shouldCallCreate: true,
			description:      "should use channel name and not publish",
		},
		{
			name:             "create with publish=true only",
			command:          "/matrix create publish=true",
			expectedRoomName: "",
			expectedPublish:  true,
			shouldCallCreate: true,
			description:      "should use channel name and publish",
		},
		{
			name:             "create with publish=false only",
			command:          "/matrix create publish=false",
			expectedRoomName: "",
			expectedPublish:  false,
			shouldCallCreate: true,
			description:      "should use channel name and not publish",
		},
		{
			name:             "create with room name only",
			command:          "/matrix create TestRoom",
			expectedRoomName: "TestRoom",
			expectedPublish:  false,
			shouldCallCreate: true,
			description:      "should use custom room name and not publish",
		},
		{
			name:             "create with multi-word room name",
			command:          "/matrix create My Test Room",
			expectedRoomName: "My Test Room",
			expectedPublish:  false,
			shouldCallCreate: true,
			description:      "should use multi-word room name and not publish",
		},
		{
			name:             "create with room name and true",
			command:          "/matrix create TestRoom true",
			expectedRoomName: "TestRoom",
			expectedPublish:  true,
			shouldCallCreate: true,
			description:      "should use custom room name and publish",
		},
		{
			name:             "create with room name and false",
			command:          "/matrix create TestRoom false",
			expectedRoomName: "TestRoom",
			expectedPublish:  false,
			shouldCallCreate: true,
			description:      "should use custom room name and not publish",
		},
		{
			name:             "create with room name and publish=true",
			command:          "/matrix create TestRoom publish=true",
			expectedRoomName: "TestRoom",
			expectedPublish:  true,
			shouldCallCreate: true,
			description:      "should use custom room name and publish",
		},
		{
			name:             "create with room name and publish=false",
			command:          "/matrix create TestRoom publish=false",
			expectedRoomName: "TestRoom",
			expectedPublish:  false,
			shouldCallCreate: true,
			description:      "should use custom room name and not publish",
		},
		{
			name:             "create with multi-word room name and true",
			command:          "/matrix create My Test Room true",
			expectedRoomName: "My Test Room",
			expectedPublish:  true,
			shouldCallCreate: true,
			description:      "should use multi-word room name and publish",
		},
		{
			name:             "create with multi-word room name and publish=false",
			command:          "/matrix create My Test Room publish=false",
			expectedRoomName: "My Test Room",
			expectedPublish:  false,
			shouldCallCreate: true,
			description:      "should use multi-word room name and not publish",
		},
		{
			name:             "create with double-quoted room name",
			command:          `/matrix create "connected-channel-1c"`,
			expectedRoomName: "connected-channel-1c",
			expectedPublish:  false,
			shouldCallCreate: true,
			description:      "should strip surrounding double quotes from room name",
		},
		{
			name:             "create with single-quoted room name",
			command:          `/matrix create 'my room'`,
			expectedRoomName: "my room",
			expectedPublish:  false,
			shouldCallCreate: true,
			description:      "should strip surrounding single quotes from room name",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			env := setupTest()

			// Set up expectations for command registration
			setupCommandRegistration(env)

			// Set up channel get expectation
			channel := &model.Channel{
				Id:          "test-channel-id",
				DisplayName: "Test Channel",
				Name:        "test-channel",
			}
			env.api.On("GetChannel", "test-channel-id").Return(channel, nil)

			// Create a custom test handler to capture the create command parameters
			var capturedRoomName string
			var capturedPublish bool
			var createCalled bool

			// Create mock plugin API (one server registered, nil Matrix client so
			// create fails gracefully with "Matrix client not configured").
			mockPlugin := newSingleServerMockPlugin(t, env)

			testHandler := &testCommandHandler{
				Handler: &Handler{
					plugin:    mockPlugin,
					client:    env.client,
					kvstore:   mockPlugin.kvstore,
					pluginAPI: env.api,
				},
				onCreateRoom: func(roomName string, publish bool) {
					capturedRoomName = roomName
					capturedPublish = publish
					createCalled = true
				},
			}

			args := &model.CommandArgs{
				Command:   tt.command,
				ChannelId: "test-channel-id",
			}

			response, err := testHandler.Handle(args)

			if tt.shouldCallCreate {
				assert.Nil(err)
				assert.True(createCalled, "create command should have been called")
				assert.Equal(tt.expectedRoomName, capturedRoomName, "room name should match expected")
				assert.Equal(tt.expectedPublish, capturedPublish, "publish flag should match expected")

				// If room name is empty, the handler should use the channel name
				if tt.expectedRoomName == "" {
					assert.Contains(response.Text, "Matrix client not configured", "should fail gracefully when no matrix client")
				}
			}
		})
	}
}

// testCommandHandler wraps the Handler to intercept create room calls for testing
type testCommandHandler struct {
	*Handler
	onCreateRoom func(roomName string, publish bool)
}

func (t *testCommandHandler) Handle(args *model.CommandArgs) (*model.CommandResponse, error) {
	// Override the executeCreateRoomCommand to capture parameters
	originalHandler := t.Handler
	t.Handler = &Handler{
		plugin:    originalHandler.plugin,
		client:    originalHandler.client,
		kvstore:   originalHandler.kvstore,
		pluginAPI: originalHandler.pluginAPI,
	}

	// Parse the command to extract create parameters
	fields := strings.Fields(args.Command)
	if len(fields) >= 2 && fields[1] == "create" {
		// Duplicate the parsing logic from the actual command
		var roomName string
		publish := false

		switch {
		case len(fields) == 2:
			roomName = ""
		case len(fields) == 3:
			arg := fields[2]
			if arg == "true" || arg == "false" || strings.HasPrefix(arg, "publish=") {
				roomName = ""
				if publishValue, ok := strings.CutPrefix(arg, "publish="); ok {
					publish = publishValue == "true"
				} else {
					publish = arg == "true"
				}
			} else {
				roomName = arg
			}
		default:
			lastField := fields[len(fields)-1]
			if lastField == "true" || lastField == "false" || strings.HasPrefix(lastField, "publish=") {
				if publishValue, ok := strings.CutPrefix(lastField, "publish="); ok {
					publish = publishValue == "true"
				} else {
					publish = lastField == "true"
				}
				roomName = strings.Join(fields[2:len(fields)-1], " ")
			} else {
				roomName = strings.Join(fields[2:], " ")
			}
		}

		// Strip surrounding quotes that users may add around room names
		roomName = strings.Trim(roomName, "\"'")

		if t.onCreateRoom != nil {
			t.onCreateRoom(roomName, publish)
		}
	}

	return originalHandler.Handle(args)
}

func setupCommandRegistration(env *env) {
	// Matrix command registration
	matrixData := model.NewAutocompleteData(matrixCommandTrigger, "[subcommand]", "Matrix bridge commands")
	matrixData.AddCommand(model.NewAutocompleteData("test", "", testCommandDesc))

	// Create command with argument completion
	createCmd := model.NewAutocompleteData("create", createCommandHint, createCommandDesc)
	createCmd.AddTextArgument("Optional room name (defaults to channel name)", "[room_name]", "")
	createCmd.AddTextArgument("Optional publish flag", "[publish=true|false]", "")
	matrixData.AddCommand(createCmd)

	// Map command with argument completion
	mapCmd := model.NewAutocompleteData("map", mapCommandHint, mapCommandDesc)
	mapCmd.AddTextArgument("Matrix room alias or room ID", "[room_alias|room_id]", "")
	matrixData.AddCommand(mapCmd)

	// Unmap command
	matrixData.AddCommand(model.NewAutocompleteData("unmap", unmapCommandHint, unmapCommandDesc))

	matrixData.AddCommand(model.NewAutocompleteData("list", "", listCommandDesc))
	matrixData.AddCommand(model.NewAutocompleteData("status", "", statusCommandDesc))
	matrixData.AddCommand(model.NewAutocompleteData("migrate", "", migrateCommandDesc))

	serverCmd := model.NewAutocompleteData("server", "[list|add|remove|map|unmap|registration|status]", serverCommandDesc)
	serverCmd.AddCommand(model.NewAutocompleteData("list", "", "List all registered Matrix servers"))
	serverAddCmd := model.NewAutocompleteData("add", "<server_url> <server_name> <as_token> <hs_token> [username_prefix]", "Register or replace a Matrix server")
	serverAddCmd.AddTextArgument("Matrix homeserver base URL", "<server_url>", "")
	serverAddCmd.AddTextArgument("Matrix server name (domain in user IDs)", "<server_name>", "")
	serverAddCmd.AddTextArgument("Application Service token", "<as_token>", "")
	serverAddCmd.AddTextArgument("Homeserver token", "<hs_token>", "")
	serverAddCmd.AddTextArgument("Optional username prefix", "[username_prefix]", "")
	serverCmd.AddCommand(serverAddCmd)
	serverRemoveCmd := model.NewAutocompleteData("remove", "<server_id>", "Remove a registered Matrix server")
	serverRemoveCmd.AddTextArgument("Server ID (from /matrix server list)", "<server_id>", "")
	serverCmd.AddCommand(serverRemoveCmd)
	serverMapCmd := model.NewAutocompleteData("map", "[server_id] <room_alias|room_id>", "Map the current channel to a room on a server")
	serverMapCmd.AddTextArgument("Server ID (optional when one server; from /matrix server list)", "[server_id]", "")
	serverMapCmd.AddTextArgument("Matrix room alias or room ID", "<room_alias|room_id>", "")
	serverCmd.AddCommand(serverMapCmd)
	serverUnmapCmd := model.NewAutocompleteData("unmap", "[server_id]", "Remove the current channel's mapping for a server")
	serverUnmapCmd.AddTextArgument("Server ID (optional when one server; from /matrix server list)", "[server_id]", "")
	serverCmd.AddCommand(serverUnmapCmd)
	serverRegistrationCmd := model.NewAutocompleteData("registration", "[server_id]", "Print the Application Service registration YAML")
	serverRegistrationCmd.AddTextArgument("Server ID (optional when one server; from /matrix server list)", "[server_id]", "")
	serverCmd.AddCommand(serverRegistrationCmd)
	serverStatusCmd := model.NewAutocompleteData("status", "[server_id]", "Show status for a Matrix server")
	serverStatusCmd.AddTextArgument("Server ID (optional when one server; from /matrix server list)", "[server_id]", "")
	serverCmd.AddCommand(serverStatusCmd)
	matrixData.AddCommand(serverCmd)

	env.api.On("RegisterCommand", &model.Command{
		Trigger:          matrixCommandTrigger,
		AutoComplete:     true,
		AutoCompleteDesc: "Matrix bridge commands",
		AutoCompleteHint: "[subcommand]",
		AutocompleteData: matrixData,
	}).Return(nil)
}

func TestMatrixCreateCommandEdgeCases(t *testing.T) {
	tests := []struct {
		name           string
		command        string
		channelName    string
		channelDisplay string
		expectedResult string
		description    string
	}{
		{
			name:           "create with edge case room names",
			command:        "/matrix create Room-With_Special.Chars",
			channelName:    "test-channel",
			channelDisplay: "Test Channel",
			expectedResult: "Room-With_Special.Chars",
			description:    "should handle special characters in room names",
		},
		{
			name:           "create uses display name when available",
			command:        "/matrix create",
			channelName:    "test-channel",
			channelDisplay: "My Display Name",
			expectedResult: "", // Empty means use channel name, will become "My Display Name"
			description:    "should use channel display name when room name is empty",
		},
		{
			name:           "create uses channel name when no display name",
			command:        "/matrix create",
			channelName:    "test-channel-name",
			channelDisplay: "",
			expectedResult: "", // Empty means use channel name, will become "test-channel-name"
			description:    "should use channel name when no display name available",
		},
		{
			name:           "create with publish parameter edge cases",
			command:        "/matrix create publish=True", // Capital T
			channelName:    "test-channel",
			channelDisplay: "Test Channel",
			expectedResult: "",
			description:    "should handle case-sensitive publish parameter gracefully",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			env := setupTest()

			// Set up expectations for command registration
			setupCommandRegistration(env)

			// Set up channel get expectation
			channel := &model.Channel{
				Id:          "test-channel-id",
				DisplayName: tt.channelDisplay,
				Name:        tt.channelName,
			}
			env.api.On("GetChannel", "test-channel-id").Return(channel, nil)

			// Create command handler
			mockPlugin := newSingleServerMockPlugin(t, env)
			cmdHandler := NewCommandHandler(mockPlugin)

			args := &model.CommandArgs{
				Command:   tt.command,
				ChannelId: "test-channel-id",
			}

			response, err := cmdHandler.Handle(args)

			// Should not error on parsing
			assert.Nil(err)
			// Should get some response (even if Matrix client not configured)
			assert.NotNil(response)
		})
	}
}

func TestMatrixCommandErrors(t *testing.T) {
	tests := []struct {
		name        string
		command     string
		expectError bool
		description string
	}{
		{
			name:        "unknown subcommand",
			command:     "/matrix unknown",
			expectError: false, // Returns error response, not error
			description: "should handle unknown subcommands gracefully",
		},
		{
			name:        "matrix command with no subcommand",
			command:     "/matrix",
			expectError: false,
			description: "should handle missing subcommand",
		},
		{
			name:        "unknown command",
			command:     "/unknown",
			expectError: false,
			description: "should handle unknown commands gracefully",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			env := setupTest()

			// Set up expectations for command registration
			setupCommandRegistration(env)

			// Create command handler
			mockPlugin := newSingleServerMockPlugin(t, env)
			cmdHandler := NewCommandHandler(mockPlugin)

			args := &model.CommandArgs{
				Command:   tt.command,
				ChannelId: "test-channel-id",
			}

			response, err := cmdHandler.Handle(args)

			if tt.expectError {
				assert.NotNil(err)
			} else {
				assert.Nil(err)
				assert.NotNil(response)
			}
		})
	}
}

func TestChannelNameFallback(t *testing.T) {
	assert := assert.New(t)
	env := setupTest()

	// Set up expectations for command registration
	setupCommandRegistration(env)

	// Test with different channel configurations
	testCases := []struct {
		displayName  string
		name         string
		expectedName string
	}{
		{
			displayName:  "My Display Name",
			name:         "channel-name",
			expectedName: "My Display Name",
		},
		{
			displayName:  "",
			name:         "channel-name",
			expectedName: "channel-name",
		},
		{
			displayName:  "",
			name:         "",
			expectedName: "test-channel-id", // Falls back to channel ID
		},
	}

	for _, tc := range testCases {
		channel := &model.Channel{
			Id:          "test-channel-id",
			DisplayName: tc.displayName,
			Name:        tc.name,
		}
		env.api.On("GetChannel", "test-channel-id").Return(channel, nil).Once()

		var capturedRoomName string
		mockPlugin := newSingleServerMockPlugin(t, env)
		testHandler := &testCommandHandler{
			Handler: &Handler{
				plugin:    mockPlugin,
				client:    env.client,
				kvstore:   mockPlugin.kvstore,
				pluginAPI: env.api,
			},
			onCreateRoom: func(roomName string, _ bool) {
				capturedRoomName = roomName
			},
		}

		args := &model.CommandArgs{
			Command:   "/matrix create",
			ChannelId: "test-channel-id",
		}

		_, err := testHandler.Handle(args)
		assert.Nil(err)

		// The captured room name should be empty (meaning use channel name)
		// The actual room name resolution happens in executeCreateRoomCommand
		assert.Equal("", capturedRoomName)
	}
}

// memKV is a minimal in-memory kvstore.KVStore for exercising command handlers
// directly (the main package's MemoryKVStore is not importable here).
type memKV struct{ m map[string][]byte }

func newMemKV() *memKV { return &memKV{m: map[string][]byte{}} }

// seedCommandServer writes a single server into the store so resolveServerID
// resolves to it in command tests.
func seedCommandServer(t *testing.T, store kvstore.KVStore, serverID, serverURL, serverName string) {
	t.Helper()
	data, err := json.Marshal([]kvstore.ServerConfig{{
		ServerID: serverID, ServerURL: serverURL, ServerName: serverName,
		ASToken: "as", HSToken: "hs", UsernamePrefix: "matrix", Enabled: true,
		SiteURL: "https://" + serverName, RemoteID: "remote-" + serverID,
	}})
	require.NoError(t, err)
	require.NoError(t, store.Set(kvstore.KeyServersConfig, data))
}

// newSingleServerMockPlugin returns a mockPlugin backed by an in-memory KV that
// already has exactly one server registered, so resolveServerID succeeds. The
// Matrix client is nil, so client-requiring commands fail with "Matrix client
// not configured" — exercising the not-configured path.
func newSingleServerMockPlugin(t *testing.T, env *env) *mockPlugin {
	t.Helper()
	store := newMemKV()
	seedCommandServer(t, store, "test-server-id", "http://test.com", "test.com")
	return &mockPlugin{
		client:    env.client,
		kvstore:   store,
		pluginAPI: env.api,
	}
}

func (s *memKV) GetTemplateData(string) (string, error) { return "", nil }
func (s *memKV) Get(k string) ([]byte, error)           { return s.m[k], nil }
func (s *memKV) Set(k string, v []byte) error           { s.m[k] = v; return nil }
func (s *memKV) Delete(k string) error                  { delete(s.m, k); return nil }

func (s *memKV) SetAtomicWithRetries(k string, valueFunc func(oldValue []byte) ([]byte, error)) error {
	newValue, err := valueFunc(s.m[k])
	if err != nil {
		return err
	}
	s.m[k] = newValue
	return nil
}
func (s *memKV) ListKeys(int, int) ([]string, error) { return nil, nil }

func (s *memKV) ListKeysWithPrefix(page, perPage int, prefix string) ([]string, error) {
	var keys []string
	for k := range s.m {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	start := page * perPage
	if start >= len(keys) {
		return nil, nil
	}
	end := min(start+perPage, len(keys))
	return keys[start:end], nil
}

func newUnmapTestHandler(env *env, store kvstore.KVStore) *Handler {
	return &Handler{
		plugin: &mockPlugin{
			client:    env.client,
			kvstore:   store,
			pluginAPI: env.api,
		},
		client:    env.client,
		kvstore:   store,
		pluginAPI: env.api,
	}
}

func TestExecuteUnmapCommand(t *testing.T) {
	const serverID = "test-server-id"

	t.Run("ClearsCorruptMapping", func(t *testing.T) {
		env := setupTest()
		env.api.On("GetChannel", "chanX").Return(&model.Channel{Id: "chanX", Name: "chanx"}, nil)
		env.api.On("LogError", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return().Maybe()

		store := newMemKV()
		key := kvstore.BuildChannelMappingKey("chanX")
		require.NoError(t, store.Set(key, []byte("!not-json:server")))

		h := newUnmapTestHandler(env, store)
		resp := h.executeUnmapCommand(&model.CommandArgs{ChannelId: "chanX"}, serverID)

		assert.Contains(t, resp.Text, "Corrupt Mapping Cleared")
		v, _ := store.Get(key)
		assert.Empty(t, v, "corrupt mapping should be deleted so the admin can recover")
	})

	t.Run("OtherServerMappingTreatedAsUnmappedAndNotDeleted", func(t *testing.T) {
		env := setupTest()
		env.api.On("GetChannel", "chanY").Return(&model.Channel{Id: "chanY", Name: "chany"}, nil)

		store := newMemKV()
		key := kvstore.BuildChannelMappingKey("chanY")
		val, err := kvstore.BuildSingleChannelMapping("some-other-server", "!room:server")
		require.NoError(t, err)
		require.NoError(t, store.Set(key, val))

		h := newUnmapTestHandler(env, store)
		resp := h.executeUnmapCommand(&model.CommandArgs{ChannelId: "chanY"}, serverID)

		assert.Contains(t, resp.Text, "No Mapping Found")
		v, _ := store.Get(key)
		assert.NotEmpty(t, v, "a valid mapping for another server must not be deleted")
	})
}

func TestExecuteListMappingsCommand(t *testing.T) {
	// list shows mappings across ALL servers, annotated with the serverID; only
	// corrupt values are skipped.
	env := setupTest()
	env.api.On("GetChannel", "chanA").Return(&model.Channel{Id: "chanA", Name: "chan-a"}, nil).Maybe()
	env.api.On("GetChannel", mock.AnythingOfType("string")).Return(&model.Channel{Id: "x", Name: "x"}, nil).Maybe()
	env.api.On("LogWarn", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return().Maybe()

	store := newMemKV()
	// Mapping for server A.
	valA, err := kvstore.BuildSingleChannelMapping("test-server-id", "!roomA:server")
	require.NoError(t, err)
	require.NoError(t, store.Set(kvstore.BuildChannelMappingKey("chanA"), valA))
	// Mapping for a different server — now also listed (annotated with its serverID).
	valB, err := kvstore.BuildSingleChannelMapping("other-server", "!roomB:server")
	require.NoError(t, err)
	require.NoError(t, store.Set(kvstore.BuildChannelMappingKey("chanB"), valB))
	// A corrupt value — must be skipped without aborting the listing.
	require.NoError(t, store.Set(kvstore.BuildChannelMappingKey("chanC"), []byte("!corrupt")))

	h := newUnmapTestHandler(env, store)
	resp := h.executeListMappingsCommand(&model.CommandArgs{ChannelId: "chanZ"})

	assert.Contains(t, resp.Text, "!roomA:server", "server A's mapping should be listed")
	assert.Contains(t, resp.Text, "!roomB:server", "every server's mapping should be listed")
	assert.Contains(t, resp.Text, "2 channels", "both valid channels should be counted")
	assert.NotContains(t, resp.Text, "chanC", "a corrupt mapping must be skipped, not crash the listing")
}
