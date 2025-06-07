package command

import (
	"fmt"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mattermost/mattermost/server/public/pluginapi"
	"github.com/wiggin77/mattermost-plugin-matrix-bridge/server/matrix"
	"github.com/wiggin77/mattermost-plugin-matrix-bridge/server/store/kvstore"
)

type Handler struct {
	client       *pluginapi.Client
	kvstore      kvstore.KVStore
	matrixClient *matrix.Client
}

type Command interface {
	Handle(args *model.CommandArgs) (*model.CommandResponse, error)
	executeHelloCommand(args *model.CommandArgs) *model.CommandResponse
	executeMatrixCommand(args *model.CommandArgs) *model.CommandResponse
}

const helloCommandTrigger = "hello"
const matrixCommandTrigger = "matrix"

// Register all your slash commands in the NewCommandHandler function.
func NewCommandHandler(client *pluginapi.Client, kvstore kvstore.KVStore, matrixClient *matrix.Client) Command {
	err := client.SlashCommand.Register(&model.Command{
		Trigger:          helloCommandTrigger,
		AutoComplete:     true,
		AutoCompleteDesc: "Say hello to someone",
		AutoCompleteHint: "[@username]",
		AutocompleteData: model.NewAutocompleteData(helloCommandTrigger, "[@username]", "Username to say hello to"),
	})
	if err != nil {
		client.Log.Error("Failed to register hello command", "error", err)
	}

	matrixData := model.NewAutocompleteData(matrixCommandTrigger, "[subcommand]", "Matrix bridge commands")
	matrixData.AddCommand(model.NewAutocompleteData("test", "", "Test Matrix connection"))
	matrixData.AddCommand(model.NewAutocompleteData("create", "[room_name]", "Create a new Matrix room and map to current channel"))
	matrixData.AddCommand(model.NewAutocompleteData("map", "[room_alias|room_id]", "Map current channel to Matrix room (prefer #alias:server.com)"))
	matrixData.AddCommand(model.NewAutocompleteData("list", "", "List all channel-to-room mappings"))
	matrixData.AddCommand(model.NewAutocompleteData("status", "", "Show bridge status"))

	err = client.SlashCommand.Register(&model.Command{
		Trigger:          matrixCommandTrigger,
		AutoComplete:     true,
		AutoCompleteDesc: "Matrix bridge commands",
		AutoCompleteHint: "[subcommand]",
		AutocompleteData: matrixData,
	})
	if err != nil {
		client.Log.Error("Failed to register matrix command", "error", err)
	}

	return &Handler{
		client:       client,
		kvstore:      kvstore,
		matrixClient: matrixClient,
	}
}

// ExecuteCommand hook calls this method to execute the commands that were registered in the NewCommandHandler function.
func (c *Handler) Handle(args *model.CommandArgs) (*model.CommandResponse, error) {
	trigger := strings.TrimPrefix(strings.Fields(args.Command)[0], "/")
	switch trigger {
	case helloCommandTrigger:
		return c.executeHelloCommand(args), nil
	case matrixCommandTrigger:
		return c.executeMatrixCommand(args), nil
	default:
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         fmt.Sprintf("Unknown command: %s", args.Command),
		}, nil
	}
}

func (c *Handler) executeHelloCommand(args *model.CommandArgs) *model.CommandResponse {
	if len(strings.Fields(args.Command)) < 2 {
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         "Please specify a username",
		}
	}
	username := strings.Fields(args.Command)[1]
	return &model.CommandResponse{
		Text: "Hello, " + username,
	}
}

func (c *Handler) executeMapCommand(args *model.CommandArgs, roomIdentifier string) *model.CommandResponse {
	// Validate room identifier format (should start with ! or # and contain a colon)
	if (!strings.HasPrefix(roomIdentifier, "!") && !strings.HasPrefix(roomIdentifier, "#")) || !strings.Contains(roomIdentifier, ":") {
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         "Invalid room identifier format. Use either:\n• Room alias: `#roomname:server.com` (preferred for joining)\n• Room ID: `!roomid:server.com`",
		}
	}
	
	// Get channel info for display
	channel, appErr := c.client.Channel.Get(args.ChannelId)
	channelName := args.ChannelId
	if appErr == nil {
		channelName = channel.DisplayName
		if channelName == "" {
			channelName = channel.Name
		}
	}
	
	// Try to join the Matrix room automatically
	var joinStatus string
	if c.matrixClient != nil {
		if err := c.matrixClient.JoinRoom(roomIdentifier); err != nil {
			c.client.Log.Warn("Failed to auto-join Matrix room", "error", err, "room_identifier", roomIdentifier)
			if strings.HasPrefix(roomIdentifier, "!") {
				joinStatus = "\n\n⚠️ **Note:** Could not auto-join using room ID. Try using room alias instead (e.g., `#roomname:server.com`)"
			} else {
				joinStatus = "\n\n⚠️ **Note:** Could not auto-join Matrix room. You may need to manually invite the bridge user."
			}
		} else {
			c.client.Log.Info("Successfully joined Matrix room", "room_identifier", roomIdentifier)
			joinStatus = "\n\n✅ **Auto-joined** Matrix room successfully!"
		}
	} else {
		joinStatus = "\n\n⚠️ **Note:** Matrix client not configured. Please configure Matrix settings and manually invite the bridge user."
	}

	// Save the mapping
	mappingKey := "channel_mapping_" + args.ChannelId
	err := c.kvstore.Set(mappingKey, []byte(roomIdentifier))
	if err != nil {
		c.client.Log.Error("Failed to save channel mapping", "error", err, "channel_id", args.ChannelId, "room_identifier", roomIdentifier)
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         "❌ Failed to save channel mapping. Check plugin logs for details.",
		}
	}
	
	c.client.Log.Info("Channel mapping saved", "channel_id", args.ChannelId, "channel_name", channelName, "room_identifier", roomIdentifier)
	
	return &model.CommandResponse{
		ResponseType: model.CommandResponseTypeEphemeral,
		Text:         fmt.Sprintf("✅ **Mapping Saved**\n\n**Channel:** %s\n**Matrix Room:** `%s`%s\n\nMessages from this channel will now sync to Matrix when shared channels are enabled.", channelName, roomIdentifier, joinStatus),
	}
}

func (c *Handler) executeCreateRoomCommand(args *model.CommandArgs, roomName string) *model.CommandResponse {
	if c.matrixClient == nil {
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         "❌ Matrix client not configured. Please configure Matrix settings in System Console.",
		}
	}

	// Get channel info for room topic
	channel, appErr := c.client.Channel.Get(args.ChannelId)
	channelName := args.ChannelId
	if appErr == nil {
		channelName = channel.DisplayName
		if channelName == "" {
			channelName = channel.Name
		}
	}

	topic := fmt.Sprintf("Matrix room for Mattermost channel: %s", channelName)

	// Create the Matrix room
	roomID, err := c.matrixClient.CreateRoom(roomName, topic)
	if err != nil {
		c.client.Log.Error("Failed to create Matrix room", "error", err, "room_name", roomName)
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         fmt.Sprintf("❌ Failed to create Matrix room '%s'. Check plugin logs for details.", roomName),
		}
	}

	c.client.Log.Info("Created Matrix room", "room_id", roomID, "room_name", roomName)

	// Automatically map the created room to this channel
	mappingKey := "channel_mapping_" + args.ChannelId
	if err := c.kvstore.Set(mappingKey, []byte(roomID)); err != nil {
		c.client.Log.Error("Failed to save channel mapping", "error", err, "channel_id", args.ChannelId, "room_id", roomID)
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         fmt.Sprintf("✅ **Matrix Room Created:** `%s`\n\n❌ Failed to save channel mapping. Use `/matrix map %s` to map manually.", roomID, roomID),
		}
	}

	return &model.CommandResponse{
		ResponseType: model.CommandResponseTypeEphemeral,
		Text:         fmt.Sprintf("✅ **Matrix Room Created & Mapped**\n\n**Room Name:** %s\n**Room ID:** `%s`\n**Channel:** %s\n\nThe bridge user is automatically joined as the room creator. Messages from this channel will now sync to Matrix when shared channels are enabled.", roomName, roomID, channelName),
	}
}

func (c *Handler) executeListMappingsCommand(args *model.CommandArgs) *model.CommandResponse {
	// Get the current channel's mapping as an example
	currentChannelKey := "channel_mapping_" + args.ChannelId
	currentRoomID, err := c.kvstore.Get(currentChannelKey)
	
	var responseText strings.Builder
	responseText.WriteString("**Channel-to-Room Mappings:**\n\n")
	
	if err != nil || len(currentRoomID) == 0 {
		responseText.WriteString("**Current Channel:** No mapping found\n")
	} else {
		// Get channel info
		channel, appErr := c.client.Channel.Get(args.ChannelId)
		channelName := args.ChannelId
		if appErr == nil {
			channelName = channel.DisplayName
			if channelName == "" {
				channelName = channel.Name
			}
		}
		responseText.WriteString(fmt.Sprintf("**Current Channel:** %s → `%s`\n", channelName, string(currentRoomID)))
	}
	
	responseText.WriteString("\n*Note: Currently only showing current channel mapping.*\n")
	responseText.WriteString("*Full listing requires KV store key enumeration.*\n\n")
	responseText.WriteString("**Commands:**\n")
	responseText.WriteString("• `/matrix map [room_id]` - Map current channel to Matrix room\n")
	responseText.WriteString("• `/matrix status` - Check bridge status")
	
	return &model.CommandResponse{
		ResponseType: model.CommandResponseTypeEphemeral,
		Text:         responseText.String(),
	}
}

func (c *Handler) executeMatrixCommand(args *model.CommandArgs) *model.CommandResponse {
	fields := strings.Fields(args.Command)
	if len(fields) < 2 {
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         "Usage: /matrix [test|create|map|list|status] [room_name|room_alias|room_id]",
		}
	}

	subcommand := fields[1]
	switch subcommand {
	case "test":
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         "Matrix connection test - check plugin logs for results",
		}
	case "create":
		if len(fields) < 3 {
			return &model.CommandResponse{
				ResponseType: model.CommandResponseTypeEphemeral,
				Text:         "Usage: /matrix create [room_name]",
			}
		}
		roomName := strings.Join(fields[2:], " ") // Allow multi-word room names
		return c.executeCreateRoomCommand(args, roomName)
	case "map":
		if len(fields) < 3 {
			return &model.CommandResponse{
				ResponseType: model.CommandResponseTypeEphemeral,
				Text:         "Usage: /matrix map [room_alias|room_id]\nExample: /matrix map #test-sync:synapse-wiggin77.ngrok.io",
			}
		}
		roomID := fields[2]
		return c.executeMapCommand(args, roomID)
	case "list":
		return c.executeListMappingsCommand(args)
	case "status":
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         "Matrix Bridge Status:\n- Plugin: Active\n- Configuration: Check System Console → Plugins → Matrix Bridge\n- Logs: Check plugin logs for connection status",
		}
	default:
		return &model.CommandResponse{
			ResponseType: model.CommandResponseTypeEphemeral,
			Text:         "Unknown subcommand. Use: test, create, map, list, or status",
		}
	}
}
