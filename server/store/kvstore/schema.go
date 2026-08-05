package kvstore

import (
	"encoding/json"
	"strings"

	"github.com/pkg/errors"
)

// ServerConfig describes a single Matrix homeserver registered with the plugin.
// The full registry is the JSON array of these stored under KeyServersConfig.
type ServerConfig struct {
	ServerID       string `json:"server_id"`    // model.NewId() - random, opaque
	ServerURL      string `json:"server_url"`   // homeserver base URL
	Endpoint       string `json:"endpoint"`     // normalized host:port - the uniqueness key
	ServerName     string `json:"server_name"`  // Matrix ID domain; NEVER empty, unique
	EventDomain    string `json:"event_domain"` // sanitized, immutable; keys matrix_event_id_<EventDomain>
	ASToken        string `json:"as_token"`
	HSToken        string `json:"hs_token"`
	UsernamePrefix string `json:"username_prefix"`
	Enabled        bool   `json:"enabled"`
	RemoteID       string `json:"remote_id"`          // shared-channels remote, one per server
	SiteURL        string `json:"site_url,omitempty"` // value passed to RegisterPluginForSharedChannels
}

// ErrChannelAlreadyMapped is returned by Plugin.SetChannelMapping (server/channel_mapping.go)
// when a channel is already mapped to maxServersPerChannel other live (registered)
// servers. Defined here rather than in the main package so command handlers (a
// different package) can match it with errors.Is without an import cycle.
var ErrChannelAlreadyMapped = errors.New("channel is already mapped to another Matrix server")

// ChannelServerMapping is one entry in the JSON array stored under
// channel_mapping_<channelID>. The value is always a list, even though policy
// currently limits it to at most one entry (see maxServersPerChannel).
type ChannelServerMapping struct {
	ServerID string `json:"server_id"`
	RoomID   string `json:"room_id"` // room ID or alias on that server
}

// ParseServersConfig parses the JSON array stored under KeyServersConfig.
// Empty input returns (nil, nil). Malformed input returns an error.
func ParseServersConfig(data []byte) ([]ServerConfig, error) {
	if len(data) == 0 {
		return nil, nil
	}

	var servers []ServerConfig
	if err := json.Unmarshal(data, &servers); err != nil {
		return nil, errors.Wrap(err, "failed to parse servers config")
	}

	return servers, nil
}

// MarshalServersConfig serializes the server registry for storage.
func MarshalServersConfig(servers []ServerConfig) ([]byte, error) {
	data, err := json.Marshal(servers)
	if err != nil {
		return nil, errors.Wrap(err, "failed to marshal servers config")
	}
	return data, nil
}

// ParseChannelServerMappings parses the JSON array stored under a
// channel_mapping_<channelID> key. Empty input returns (nil, nil), matching
// "unmapped". Malformed input returns an error - it must never be silently
// treated as unmapped, since that would mask a corrupt value.
func ParseChannelServerMappings(data []byte) ([]ChannelServerMapping, error) {
	if len(data) == 0 {
		return nil, nil
	}

	var mappings []ChannelServerMapping
	if err := json.Unmarshal(data, &mappings); err != nil {
		return nil, errors.Wrap(err, "failed to parse channel server mappings")
	}

	return mappings, nil
}

// MarshalChannelServerMappings serializes a channel's server mappings for storage.
func MarshalChannelServerMappings(m []ChannelServerMapping) ([]byte, error) {
	data, err := json.Marshal(m)
	if err != nil {
		return nil, errors.Wrap(err, "failed to marshal channel server mappings")
	}
	return data, nil
}

// BuildSingleChannelMapping builds the stored value for a channel mapped to exactly
// one server. Used by the v3 migration to convert the legacy bare-room-ID value.
func BuildSingleChannelMapping(serverID, roomID string) ([]byte, error) {
	return MarshalChannelServerMappings([]ChannelServerMapping{{ServerID: serverID, RoomID: roomID}})
}

// UpsertChannelServerMapping returns a copy of m with serverID's entry set to roomID,
// preserving every other server's entry. If serverID is not present it is appended.
func UpsertChannelServerMapping(m []ChannelServerMapping, serverID, roomID string) []ChannelServerMapping {
	result := make([]ChannelServerMapping, 0, len(m)+1)
	found := false
	for _, entry := range m {
		if entry.ServerID == serverID {
			result = append(result, ChannelServerMapping{ServerID: serverID, RoomID: roomID})
			found = true
			continue
		}
		result = append(result, entry)
	}
	if !found {
		result = append(result, ChannelServerMapping{ServerID: serverID, RoomID: roomID})
	}
	return result
}

// RemoveServerFromChannelMapping removes serverID's entry (if any) from the mapping
// stored under key, persisting the change. If the removal empties the list, the key
// is deleted rather than storing an empty array.
func RemoveServerFromChannelMapping(kv KVStore, key, serverID string) ([]ChannelServerMapping, error) {
	// A missing key surfaces as (nil, nil) - see KVStore.Get - which ParseChannelServerMappings
	// already turns into an empty mapping below. Any non-nil error here is a genuine read
	// failure and must be propagated rather than treated as "already unmapped".
	data, err := kv.Get(key)
	if err != nil {
		return nil, errors.Wrap(err, "failed to read channel mapping")
	}

	mappings, err := ParseChannelServerMappings(data)
	if err != nil {
		return nil, err
	}

	remaining := make([]ChannelServerMapping, 0, len(mappings))
	for _, entry := range mappings {
		if entry.ServerID == serverID {
			continue
		}
		remaining = append(remaining, entry)
	}

	if len(remaining) == 0 {
		if err := kv.Delete(key); err != nil {
			return nil, errors.Wrap(err, "failed to delete empty channel mapping")
		}
		return nil, nil
	}

	newData, err := MarshalChannelServerMappings(remaining)
	if err != nil {
		return nil, err
	}
	if err := kv.Set(key, newData); err != nil {
		return nil, errors.Wrap(err, "failed to persist channel mapping after removal")
	}

	return remaining, nil
}

// RoomIDForServer returns the room ID/alias mapped to serverID, or "" if that
// server is not among the channel's mappings. Callers must never index [0].
func RoomIDForServer(m []ChannelServerMapping, serverID string) string {
	for _, entry := range m {
		if entry.ServerID == serverID {
			return entry.RoomID
		}
	}
	return ""
}

// MappedServerIDs returns every server ID a channel is mapped to.
func MappedServerIDs(m []ChannelServerMapping) []string {
	ids := make([]string, 0, len(m))
	for _, entry := range m {
		ids = append(ids, entry.ServerID)
	}
	return ids
}

// isPlausibleRoomIdentifier reports whether s looks like a Matrix room ID or alias
// (starts with '!' or '#'). Used by the v3 migration to distinguish a legacy bare
// room identifier from garbage that should be skipped rather than persisted.
func isPlausibleRoomIdentifier(s string) bool {
	return strings.HasPrefix(s, "!") || strings.HasPrefix(s, "#")
}

// IsPlausibleRoomIdentifier is the exported form of isPlausibleRoomIdentifier, for
// use by the migration code in the main package.
func IsPlausibleRoomIdentifier(s string) bool {
	return isPlausibleRoomIdentifier(s)
}
