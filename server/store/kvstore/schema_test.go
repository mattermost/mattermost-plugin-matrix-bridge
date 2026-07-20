package kvstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseChannelServerMappings(t *testing.T) {
	t.Run("EmptyInput", func(t *testing.T) {
		mappings, err := ParseChannelServerMappings(nil)
		require.NoError(t, err)
		assert.Nil(t, mappings)
	})

	t.Run("MalformedInputReturnsError", func(t *testing.T) {
		// A bare room ID / alias is the legacy value shape and is not valid JSON.
		_, err := ParseChannelServerMappings([]byte("!room:hs"))
		assert.Error(t, err)
		_, err = ParseChannelServerMappings([]byte("#alias:hs"))
		assert.Error(t, err)
	})

	t.Run("RoundTripsSingleMapping", func(t *testing.T) {
		data, err := BuildSingleChannelMapping("srv1", "!room:hs")
		require.NoError(t, err)

		mappings, err := ParseChannelServerMappings(data)
		require.NoError(t, err)
		require.Len(t, mappings, 1)
		assert.Equal(t, "srv1", mappings[0].ServerID)
		assert.Equal(t, "!room:hs", mappings[0].RoomID)
	})
}

func TestRoomIDForServer(t *testing.T) {
	mappings := []ChannelServerMapping{
		{ServerID: "srv1", RoomID: "!one:hs"},
		{ServerID: "srv2", RoomID: "!two:hs"},
	}

	assert.Equal(t, "!one:hs", RoomIDForServer(mappings, "srv1"))
	assert.Equal(t, "!two:hs", RoomIDForServer(mappings, "srv2"))
	assert.Equal(t, "", RoomIDForServer(mappings, "srv3"), "unknown server yields no room")
	assert.Equal(t, "", RoomIDForServer(mappings, ""), "empty server yields no room")
	assert.Equal(t, "", RoomIDForServer(nil, "srv1"), "nil mappings yield no room")
}
