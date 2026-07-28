package kvstore

import "encoding/json"

// This file defines the value schemas for structured KV records. Key builders
// and prefixes live in constants.go.

// ServerConfig is a single entry in the managed Matrix server registry,
// persisted as a JSON array under the KeyServersConfig key. The registry holds
// every configured homeserver; entries are managed via the `/matrix server`
// slash commands and, on upgrade from a single-server install, seeded once by
// the v3 migration from the legacy flat plugin.json config.
type ServerConfig struct {
	// ServerID is a stable identifier derived deterministically from the
	// homeserver hostname (see deriveServerID). It is the join key for every
	// per-server KV record and stays constant as long as the hostname is
	// unchanged, so a server re-created with the same URL re-adopts its records.
	ServerID string `json:"server_id"`
	// ServerURL is the Matrix homeserver base URL.
	ServerURL string `json:"server_url"`
	// ServerName is the Matrix ID domain. May be empty, in which case it is
	// resolved via server discovery (.well-known).
	ServerName string `json:"server_name"`
	// ASToken is the Application Service token.
	ASToken string `json:"as_token"`
	// HSToken is the Homeserver token.
	HSToken string `json:"hs_token"`
	// UsernamePrefix is the prefix applied to Matrix-originated usernames.
	UsernamePrefix string `json:"username_prefix"`
	// Enabled indicates whether this server participates in sync. Populated but
	// not yet independently toggle-able via UI.
	Enabled bool `json:"enabled"`
	// RemoteID is the shared-channels remote identifier returned by
	// RegisterPluginForSharedChannels for this server (one remote per server).
	RemoteID string `json:"remote_id"`
	// SiteURL is the value passed to RegisterPluginForSharedChannels to identify
	// this server's remote. It must be unique and stable per server so each gets
	// its own remoteID. An empty SiteURL resolves to the legacy
	// "plugin_<PluginID>" remote and is reserved for the single server migrated
	// from a pre-multi-server (v2) install, so that upgrade preserves its existing
	// shared channels without re-keying the remote. Servers added via
	// `/matrix server add` derive it from the homeserver hostname.
	SiteURL string `json:"site_url,omitempty"`
}

// ChannelServerMapping is one element of the value stored under a
// channel_mapping_<channelID> key. It associates a Mattermost channel with a
// Matrix room on a specific server. The list currently always has length 1.
type ChannelServerMapping struct {
	// ServerID is the serverID of the Matrix server hosting RoomID.
	ServerID string `json:"server_id"`
	// RoomID is the mapped Matrix room ID (or alias) on that server.
	RoomID string `json:"room_id"`
}

// ParseServersConfig unmarshals a servers_config value (the managed server
// registry). An empty value yields a nil slice and no error; a malformed value
// returns an error so callers never mistake a corrupt registry for "no servers".
func ParseServersConfig(data []byte) ([]ServerConfig, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var servers []ServerConfig
	if err := json.Unmarshal(data, &servers); err != nil {
		return nil, err
	}
	return servers, nil
}

// ServerConfigForID returns the registry entry for the given serverID and true,
// or a zero entry and false if none matches.
func ServerConfigForID(servers []ServerConfig, serverID string) (ServerConfig, bool) {
	for _, s := range servers {
		if s.ServerID == serverID {
			return s, true
		}
	}
	return ServerConfig{}, false
}

// ParseChannelServerMappings unmarshals a channel_mapping_ value. An empty value
// yields a nil slice and no error. A malformed (non-JSON) value returns an error
// so callers can distinguish an unmapped channel from a corrupt record; after the
// v3 migration all stored values are well-formed JSON arrays.
func ParseChannelServerMappings(data []byte) ([]ChannelServerMapping, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var mappings []ChannelServerMapping
	if err := json.Unmarshal(data, &mappings); err != nil {
		return nil, err
	}
	return mappings, nil
}

// MarshalChannelServerMappings serializes a channel_mapping_ value.
func MarshalChannelServerMappings(mappings []ChannelServerMapping) ([]byte, error) {
	return json.Marshal(mappings)
}

// BuildSingleChannelMapping serializes a single-entry channel_mapping_ value, the
// only shape currently produced.
func BuildSingleChannelMapping(serverID, roomID string) ([]byte, error) {
	return MarshalChannelServerMappings([]ChannelServerMapping{{ServerID: serverID, RoomID: roomID}})
}

// UpsertChannelServerMapping sets the RoomID for serverID within mappings,
// replacing an existing entry for that server or appending a new one, and
// returns the result. Entries for other servers are preserved so a channel can
// be mapped to rooms on multiple homeservers.
func UpsertChannelServerMapping(mappings []ChannelServerMapping, serverID, roomID string) []ChannelServerMapping {
	for i := range mappings {
		if mappings[i].ServerID == serverID {
			mappings[i].RoomID = roomID
			return mappings
		}
	}
	return append(mappings, ChannelServerMapping{ServerID: serverID, RoomID: roomID})
}

// RoomIDForServer returns the RoomID mapped for the given serverID, or "" if none.
func RoomIDForServer(mappings []ChannelServerMapping, serverID string) string {
	for _, m := range mappings {
		if m.ServerID == serverID {
			return m.RoomID
		}
	}
	return ""
}
