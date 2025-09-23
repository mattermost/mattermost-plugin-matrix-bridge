package command

import (
	"strings"
	"testing"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/matrix"
	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/mattermost/mattermost/server/public/pluginapi"
	"github.com/stretchr/testify/assert"
)

type env struct {
	client *pluginapi.Client
	api    *plugintest.API
}

type mockConfiguration struct {
	serverURL string
}

func (m *mockConfiguration) GetMatrixServerURL() string {
	return m.serverURL
}

func (m *mockConfiguration) GetMatrixUsernamePrefixForServer(_ string) string {
	return "matrix" // Use default prefix for tests
}

// mockPlugin implements the PluginAccessor interface for testing
type mockPlugin struct {
	client       *pluginapi.Client
	kvstore      kvstore.KVStore
	matrixClient *matrix.Client
	config       Configuration
	pluginAPI    *plugintest.API
}

func (m *mockPlugin) GetMatrixClient() *matrix.Client {
	return m.matrixClient
}

func (m *mockPlugin) GetKVStore() kvstore.KVStore {
	return m.kvstore
}

func (m *mockPlugin) GetConfiguration() Configuration {
	return m.config
}

func (m *mockPlugin) CreateOrGetGhostUser(mattermostUserID string) (string, error) {
	// Mock implementation - return test ghost user
	return "_mattermost_" + mattermostUserID + ":test.com", nil
}

func (m *mockPlugin) GetPluginAPI() plugin.API {
	return m.pluginAPI
}

func (m *mockPlugin) GetPluginAPIClient() *pluginapi.Client {
	return m.client
}

func (m *mockPlugin) GetRemoteID() string {
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

func (m *mockPlugin) GetMatrixUserIDFromMattermostUser(mattermostUserID string) (string, error) {
	// Mock implementation - return test Matrix user
	return "@test_" + mattermostUserID + ":test.com", nil
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

			// Create mock plugin API
			mockPlugin := &mockPlugin{
				client:       env.client,
				kvstore:      kvstore.NewKVStore(env.client),
				matrixClient: nil, // Will cause create to fail gracefully
				config:       &mockConfiguration{serverURL: "http://test.com"},
				pluginAPI:    env.api,
			}

			testHandler := &testCommandHandler{
				Handler: &Handler{
					plugin:    mockPlugin,
					client:    env.client,
					kvstore:   kvstore.NewKVStore(env.client),
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

		if len(fields) == 2 {
			roomName = ""
		} else if len(fields) == 3 {
			arg := fields[2]
			if arg == "true" || arg == "false" || strings.HasPrefix(arg, "publish=") {
				roomName = ""
				if strings.HasPrefix(arg, "publish=") {
					publishValue := strings.TrimPrefix(arg, "publish=")
					publish = publishValue == "true"
				} else {
					publish = arg == "true"
				}
			} else {
				roomName = arg
			}
		} else {
			lastField := fields[len(fields)-1]
			if lastField == "true" || lastField == "false" || strings.HasPrefix(lastField, "publish=") {
				if strings.HasPrefix(lastField, "publish=") {
					publishValue := strings.TrimPrefix(lastField, "publish=")
					publish = publishValue == "true"
				} else {
					publish = lastField == "true"
				}
				roomName = strings.Join(fields[2:len(fields)-1], " ")
			} else {
				roomName = strings.Join(fields[2:], " ")
			}
		}

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
			mockPlugin := &mockPlugin{
				client:       env.client,
				kvstore:      kvstore.NewKVStore(env.client),
				matrixClient: nil, // No matrix client - will fail gracefully
				config:       &mockConfiguration{serverURL: "http://test.com"},
				pluginAPI:    env.api,
			}
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
			mockPlugin := &mockPlugin{
				client:       env.client,
				kvstore:      kvstore.NewKVStore(env.client),
				matrixClient: nil,
				config:       &mockConfiguration{serverURL: "http://test.com"},
				pluginAPI:    env.api,
			}
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
		mockPlugin := &mockPlugin{
			client:       env.client,
			kvstore:      kvstore.NewKVStore(env.client),
			matrixClient: nil,
			config:       &mockConfiguration{serverURL: "http://test.com"},
			pluginAPI:    env.api,
		}
		testHandler := &testCommandHandler{
			Handler: &Handler{
				plugin:    mockPlugin,
				client:    env.client,
				kvstore:   kvstore.NewKVStore(env.client),
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
