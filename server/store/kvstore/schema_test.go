package kvstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseServersConfig(t *testing.T) {
	t.Run("empty input returns nil, nil", func(t *testing.T) {
		servers, err := ParseServersConfig(nil)
		require.NoError(t, err)
		assert.Nil(t, servers)

		servers, err = ParseServersConfig([]byte{})
		require.NoError(t, err)
		assert.Nil(t, servers)
	})

	t.Run("valid input round-trips", func(t *testing.T) {
		original := []ServerConfig{
			{ServerID: "s1", ServerURL: "https://a.example.com", Endpoint: "a.example.com:443", ServerName: "a.example.com", Enabled: true},
			{ServerID: "s2", ServerURL: "https://b.example.com", Endpoint: "b.example.com:443", ServerName: "b.example.com", Enabled: false},
		}

		data, err := MarshalServersConfig(original)
		require.NoError(t, err)

		parsed, err := ParseServersConfig(data)
		require.NoError(t, err)
		assert.Equal(t, original, parsed)
	})

	t.Run("corrupt input errors", func(t *testing.T) {
		_, err := ParseServersConfig([]byte("not json"))
		require.Error(t, err)
	})
}

func TestParseChannelServerMappings(t *testing.T) {
	t.Run("empty input returns nil, nil", func(t *testing.T) {
		mappings, err := ParseChannelServerMappings(nil)
		require.NoError(t, err)
		assert.Nil(t, mappings)
	})

	t.Run("valid input round-trips", func(t *testing.T) {
		original := []ChannelServerMapping{{ServerID: "s1", RoomID: "!room:example.com"}}
		data, err := MarshalChannelServerMappings(original)
		require.NoError(t, err)

		parsed, err := ParseChannelServerMappings(data)
		require.NoError(t, err)
		assert.Equal(t, original, parsed)
	})

	t.Run("null and empty array both parse to nil/empty", func(t *testing.T) {
		parsed, err := ParseChannelServerMappings([]byte("null"))
		require.NoError(t, err)
		assert.Empty(t, parsed)

		parsed, err = ParseChannelServerMappings([]byte("[]"))
		require.NoError(t, err)
		assert.Empty(t, parsed)
	})

	t.Run("bare room ID string is not valid JSON and errors", func(t *testing.T) {
		_, err := ParseChannelServerMappings([]byte("!abc:example.org"))
		require.Error(t, err)
	})

	t.Run("corrupt input errors", func(t *testing.T) {
		_, err := ParseChannelServerMappings([]byte("{not valid"))
		require.Error(t, err)
	})
}

func TestBuildSingleChannelMapping(t *testing.T) {
	data, err := BuildSingleChannelMapping("server1", "!room:example.com")
	require.NoError(t, err)

	parsed, err := ParseChannelServerMappings(data)
	require.NoError(t, err)
	require.Len(t, parsed, 1)
	assert.Equal(t, "server1", parsed[0].ServerID)
	assert.Equal(t, "!room:example.com", parsed[0].RoomID)
}

// twoEntryMapping is the fixture used across the mapping-helper tests below: a channel
// already mapped to two servers. Policy forbids writing this today (maxServersPerChannel
// == 1), but the helpers themselves must already handle it correctly, since lifting the
// policy later must need no changes here (see §6 of the design doc).
func twoEntryMapping() []ChannelServerMapping {
	return []ChannelServerMapping{
		{ServerID: "serverA", RoomID: "!roomA:example.com"},
		{ServerID: "serverB", RoomID: "!roomB:example.com"},
	}
}

func TestUpsertChannelServerMapping(t *testing.T) {
	t.Run("appends a new server without disturbing others", func(t *testing.T) {
		single := []ChannelServerMapping{{ServerID: "serverA", RoomID: "!roomA:example.com"}}
		result := UpsertChannelServerMapping(single, "serverB", "!roomB:example.com")

		require.Len(t, result, 2)
		assert.Equal(t, "!roomA:example.com", RoomIDForServer(result, "serverA"))
		assert.Equal(t, "!roomB:example.com", RoomIDForServer(result, "serverB"))
	})

	t.Run("overwrites one server's room while preserving the other's, in a two-entry array", func(t *testing.T) {
		result := UpsertChannelServerMapping(twoEntryMapping(), "serverA", "!newRoomA:example.com")

		require.Len(t, result, 2)
		assert.Equal(t, "!newRoomA:example.com", RoomIDForServer(result, "serverA"))
		assert.Equal(t, "!roomB:example.com", RoomIDForServer(result, "serverB"), "serverB's entry must be untouched")
	})

	t.Run("upserting the same server twice does not duplicate it", func(t *testing.T) {
		result := UpsertChannelServerMapping(nil, "serverA", "!room1:example.com")
		result = UpsertChannelServerMapping(result, "serverA", "!room2:example.com")

		require.Len(t, result, 1)
		assert.Equal(t, "!room2:example.com", RoomIDForServer(result, "serverA"))
	})
}

func TestRoomIDForServer(t *testing.T) {
	mappings := twoEntryMapping()

	assert.Equal(t, "!roomA:example.com", RoomIDForServer(mappings, "serverA"))
	assert.Equal(t, "!roomB:example.com", RoomIDForServer(mappings, "serverB"))
	assert.Equal(t, "", RoomIDForServer(mappings, "serverC"), "unmapped server must return empty, not panic on index 0")
	assert.Equal(t, "", RoomIDForServer(nil, "serverA"))
}

func TestMappedServerIDs(t *testing.T) {
	assert.ElementsMatch(t, []string{"serverA", "serverB"}, MappedServerIDs(twoEntryMapping()))
	assert.Empty(t, MappedServerIDs(nil))
}

// fakeKV is a minimal in-memory KVStore for RemoveServerFromChannelMapping tests.
type fakeKV struct {
	data map[string][]byte
}

func newFakeKV() *fakeKV { return &fakeKV{data: make(map[string][]byte)} }

func (f *fakeKV) GetTemplateData(_ string) (string, error)                { return "", nil }
func (f *fakeKV) Get(key string) ([]byte, error)                          { return f.data[key], nil }
func (f *fakeKV) Set(key string, value []byte) error                      { f.data[key] = value; return nil }
func (f *fakeKV) Delete(key string) error                                 { delete(f.data, key); return nil }
func (f *fakeKV) ListKeys(_, _ int) ([]string, error)                     { return nil, nil }
func (f *fakeKV) ListKeysWithPrefix(_, _ int, _ string) ([]string, error) { return nil, nil }
func (f *fakeKV) SetAtomicWithRetries(key string, valueFunc func([]byte) ([]byte, error)) error {
	newValue, err := valueFunc(f.data[key])
	if err != nil {
		return err
	}
	return f.Set(key, newValue)
}

func TestRemoveServerFromChannelMapping(t *testing.T) {
	t.Run("removing one of two entries returns the remainder and persists it", func(t *testing.T) {
		kv := newFakeKV()
		data, err := MarshalChannelServerMappings(twoEntryMapping())
		require.NoError(t, err)
		require.NoError(t, kv.Set("channel_mapping_c1", data))

		remaining, err := RemoveServerFromChannelMapping(kv, "channel_mapping_c1", "serverA")
		require.NoError(t, err)
		require.Len(t, remaining, 1)
		assert.Equal(t, "serverB", remaining[0].ServerID)

		persisted, err := kv.Get("channel_mapping_c1")
		require.NoError(t, err)
		reparsed, err := ParseChannelServerMappings(persisted)
		require.NoError(t, err)
		assert.Equal(t, remaining, reparsed)
	})

	t.Run("removing the last entry deletes the key rather than storing an empty array", func(t *testing.T) {
		kv := newFakeKV()
		data, err := BuildSingleChannelMapping("serverA", "!room:example.com")
		require.NoError(t, err)
		require.NoError(t, kv.Set("channel_mapping_c1", data))

		remaining, err := RemoveServerFromChannelMapping(kv, "channel_mapping_c1", "serverA")
		require.NoError(t, err)
		assert.Empty(t, remaining)

		persisted, err := kv.Get("channel_mapping_c1")
		require.NoError(t, err)
		assert.Nil(t, persisted, "key must be deleted, not set to an empty array")
	})

	t.Run("removing a server not present is a no-op", func(t *testing.T) {
		kv := newFakeKV()
		data, err := MarshalChannelServerMappings(twoEntryMapping())
		require.NoError(t, err)
		require.NoError(t, kv.Set("channel_mapping_c1", data))

		remaining, err := RemoveServerFromChannelMapping(kv, "channel_mapping_c1", "serverC")
		require.NoError(t, err)
		assert.Len(t, remaining, 2)
	})

	t.Run("missing key is a no-op, not an error", func(t *testing.T) {
		kv := newFakeKV()
		remaining, err := RemoveServerFromChannelMapping(kv, "channel_mapping_missing", "serverA")
		require.NoError(t, err)
		assert.Nil(t, remaining)
	})
}

func TestIsPlausibleRoomIdentifier(t *testing.T) {
	assert.True(t, IsPlausibleRoomIdentifier("!room:example.com"))
	assert.True(t, IsPlausibleRoomIdentifier("#alias:example.com"))
	assert.False(t, IsPlausibleRoomIdentifier("example.com"))
	assert.False(t, IsPlausibleRoomIdentifier(""))
	assert.False(t, IsPlausibleRoomIdentifier("null"))
}

func TestBuildKeys(t *testing.T) {
	assert.Equal(t, "matrix_user_srv1_@alice:example.com", BuildMatrixUserKey("srv1", "@alice:example.com"))
	assert.Equal(t, "mattermost_user_srv1_user123", BuildMattermostUserKey("srv1", "user123"))
	assert.Equal(t, "channel_mapping_chan123", BuildChannelMappingKey("chan123"))
	assert.Equal(t, "room_mapping_srv1_!room:example.com", BuildRoomMappingKey("srv1", "!room:example.com"))
	assert.Equal(t, "ghost_user_srv1_user123", BuildGhostUserKey("srv1", "user123"))
	assert.Equal(t, "ghost_room_srv1_user123_!room:example.com", BuildGhostRoomKey("srv1", "user123", "!room:example.com"))
	assert.Equal(t, "matrix_event_post_srv1_$event123", BuildMatrixEventPostKey("srv1", "$event123"))
	assert.Equal(t, "matrix_reaction_srv1_$event123", BuildMatrixReactionKey("srv1", "$event123"))

	// Two servers must never collide on the same underlying id.
	assert.NotEqual(t, BuildMatrixUserKey("srv1", "x"), BuildMatrixUserKey("srv2", "x"))
}
