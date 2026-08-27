package kvstore

import (
	"encoding/json"

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
	RoomID   string `json:"room_id"` // resolved room ID, or the raw identifier if it could not be resolved
	// Alias is the room alias the mapping was created from, empty when the caller
	// supplied a room ID.
	Alias string `json:"alias,omitempty"`
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

// UpsertChannelServerMapping returns a copy of m with entry replacing the one for
// entry.ServerID, preserving every other server's entry. If that server is not present
// the entry is appended.
func UpsertChannelServerMapping(m []ChannelServerMapping, entry ChannelServerMapping) []ChannelServerMapping {
	result := make([]ChannelServerMapping, 0, len(m)+1)
	found := false
	for _, existing := range m {
		if existing.ServerID == entry.ServerID {
			result = append(result, entry)
			found = true
			continue
		}
		result = append(result, existing)
	}
	if !found {
		result = append(result, entry)
	}
	return result
}

// RemoveServerFromChannelMapping removes serverID's entry (if any) from the mapping
// stored under key, persisting the change via compare-and-set so it cannot lose a
// concurrent writer's update (e.g. SetChannelMapping, which targets the same key). If
// the removal empties the list, the stored value is set to nil - which Mattermost's KV
// store treats as key deletion - rather than storing an empty array.
func RemoveServerFromChannelMapping(kv KVStore, key, serverID string) ([]ChannelServerMapping, error) {
	// remaining is captured from inside the CAS callback below. The callback may run
	// more than once on retry, so it must be reassigned (not appended to) on every
	// invocation - otherwise a retry would leave stale data from an earlier attempt.
	var remaining []ChannelServerMapping

	err := kv.SetAtomicWithRetries(key, func(oldValue []byte) ([]byte, error) {
		mappings, err := ParseChannelServerMappings(oldValue)
		if err != nil {
			return nil, err
		}

		remaining = make([]ChannelServerMapping, 0, len(mappings))
		for _, entry := range mappings {
			if entry.ServerID == serverID {
				continue
			}
			remaining = append(remaining, entry)
		}

		if len(remaining) == 0 {
			remaining = nil
			return nil, nil
		}

		return MarshalChannelServerMappings(remaining)
	})
	if err != nil {
		return nil, errors.Wrap(err, "failed to remove server from channel mapping")
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

// ChannelMappingForServer returns serverID's whole entry, so callers that need more
// than the room ID (the alias, for reverse-key cleanup) do not have to rescan.
func ChannelMappingForServer(m []ChannelServerMapping, serverID string) (ChannelServerMapping, bool) {
	for _, entry := range m {
		if entry.ServerID == serverID {
			return entry, true
		}
	}
	return ChannelServerMapping{}, false
}

// MappedServerIDs returns every server ID a channel is mapped to.
func MappedServerIDs(m []ChannelServerMapping) []string {
	ids := make([]string, 0, len(m))
	for _, entry := range m {
		ids = append(ids, entry.ServerID)
	}
	return ids
}
