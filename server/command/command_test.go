package command

import (
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/plugin/plugintest"
	"github.com/mattermost/mattermost/server/public/pluginapi"
	"github.com/stretchr/testify/assert"
	"github.com/wiggin77/mattermost-plugin-matrix-bridge/server/matrix"
	"github.com/wiggin77/mattermost-plugin-matrix-bridge/server/store/kvstore"
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

func setupTest() *env {
	api := &plugintest.API{}
	driver := &plugintest.Driver{}
	client := pluginapi.NewClient(api, driver)

	return &env{
		client: client,
		api:    api,
	}
}

func TestHelloCommand(t *testing.T) {
	assert := assert.New(t)
	env := setupTest()

	// Expect both command registrations
	env.api.On("RegisterCommand", &model.Command{
		Trigger:          helloCommandTrigger,
		AutoComplete:     true,
		AutoCompleteDesc: "Say hello to someone",
		AutoCompleteHint: "[@username]",
		AutocompleteData: model.NewAutocompleteData("hello", "[@username]", "Username to say hello to"),
	}).Return(nil)

	matrixData := model.NewAutocompleteData(matrixCommandTrigger, "[subcommand]", "Matrix bridge commands")
	matrixData.AddCommand(model.NewAutocompleteData("test", "", "Test Matrix connection"))
	matrixData.AddCommand(model.NewAutocompleteData("create", "[room_name]", "Create a new Matrix room and map to current channel"))
	matrixData.AddCommand(model.NewAutocompleteData("map", "[room_alias|room_id]", "Map current channel to Matrix room (prefer #alias:server.com)"))
	matrixData.AddCommand(model.NewAutocompleteData("list", "", "List all channel-to-room mappings"))
	matrixData.AddCommand(model.NewAutocompleteData("status", "", "Show bridge status"))

	env.api.On("RegisterCommand", &model.Command{
		Trigger:          matrixCommandTrigger,
		AutoComplete:     true,
		AutoCompleteDesc: "Matrix bridge commands",
		AutoCompleteHint: "[subcommand]",
		AutocompleteData: matrixData,
	}).Return(nil)

	// Create mock dependencies
	mockKVStore := kvstore.NewKVStore(env.client)
	mockMatrixClient := matrix.NewClient("http://test.com", "test_token")
	mockGetConfig := func() Configuration {
		return &mockConfiguration{serverURL: "http://test.com"}
	}
	
	cmdHandler := NewCommandHandler(env.client, mockKVStore, mockMatrixClient, mockGetConfig)

	args := &model.CommandArgs{
		Command: "/hello world",
	}
	response, err := cmdHandler.Handle(args)
	assert.Nil(err)
	assert.Equal("Hello, world", response.Text)
}
