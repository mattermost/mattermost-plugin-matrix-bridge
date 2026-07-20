package kvstore

// KV Store key prefixes and constants
// This file centralizes all KV store key patterns used throughout the plugin
// to ensure consistency and avoid key conflicts.

const (
	// CurrentKVStoreVersion is the current version requiring migrations
	CurrentKVStoreVersion = 3
	// KeyPrefixMatrixUser is the prefix for Matrix user ID -> Mattermost user ID mappings
	KeyPrefixMatrixUser = "matrix_user_"
	// KeyPrefixMattermostUser is the prefix for Mattermost user ID -> Matrix user ID mappings
	KeyPrefixMattermostUser = "mattermost_user_"

	// KeyPrefixChannelMapping is the prefix for Mattermost channel ID -> Matrix room mappings
	KeyPrefixChannelMapping = "channel_mapping_"
	// KeyPrefixRoomMapping is the prefix for Matrix room identifier -> Mattermost channel ID mappings
	KeyPrefixRoomMapping = "room_mapping_"

	// KeyPrefixGhostUser is the prefix for Mattermost user ID -> Matrix ghost user ID cache
	KeyPrefixGhostUser = "ghost_user_"
	// KeyPrefixGhostRoom is the prefix for ghost user room membership tracking
	KeyPrefixGhostRoom = "ghost_room_"

	// KeyPrefixMatrixEventPost is the prefix for Matrix event ID -> Mattermost post ID mappings
	KeyPrefixMatrixEventPost = "matrix_event_post_"
	// KeyPrefixMatrixReaction is the prefix for Matrix reaction event ID -> reaction info mappings
	KeyPrefixMatrixReaction = "matrix_reaction_"

	// KeyStoreVersion is the key for tracking the current KV store schema version
	KeyStoreVersion = "kv_store_version"

	// KeyServersConfig is the key for the managed Matrix server registry (JSON array of ServerConfig)
	KeyServersConfig = "servers_config"

	// KeyPrefixLegacyDMMapping was the old prefix for DM mappings (migrated to channel_mapping_)
	KeyPrefixLegacyDMMapping = "dm_mapping_"
	// KeyPrefixLegacyMatrixDMMapping was the old prefix for Matrix DM mappings (migrated to room_mapping_)
	KeyPrefixLegacyMatrixDMMapping = "matrix_dm_mapping_"
)

// Helper functions for building KV store keys
//
// Per-server keys are namespaced by a stable serverID derived deterministically
// from each configured Matrix homeserver's hostname, producing keys of the form
// "<prefix><serverID>_<id>". This keeps every homeserver's mappings isolated so
// the plugin can bridge more than one server. The channel_mapping_ key is the
// exception: its key stays server-agnostic and the per-server association lives
// in its value (see ChannelServerMapping).

// BuildMatrixUserKey creates a key for Matrix user -> Mattermost user mapping
func BuildMatrixUserKey(serverID, matrixUserID string) string {
	return KeyPrefixMatrixUser + serverID + "_" + matrixUserID
}

// BuildMattermostUserKey creates a key for Mattermost user -> Matrix user mapping
func BuildMattermostUserKey(serverID, mattermostUserID string) string {
	return KeyPrefixMattermostUser + serverID + "_" + mattermostUserID
}

// BuildChannelMappingKey creates a key for channel -> room mapping. The key is
// intentionally not namespaced by serverID; the server association is carried in
// the value (a []ChannelServerMapping).
func BuildChannelMappingKey(channelID string) string {
	return KeyPrefixChannelMapping + channelID
}

// BuildRoomMappingKey creates a key for room -> channel mapping
func BuildRoomMappingKey(serverID, roomIdentifier string) string {
	return KeyPrefixRoomMapping + serverID + "_" + roomIdentifier
}

// BuildGhostUserKey creates a key for ghost user cache
func BuildGhostUserKey(serverID, mattermostUserID string) string {
	return KeyPrefixGhostUser + serverID + "_" + mattermostUserID
}

// BuildGhostRoomKey creates a key for ghost user room membership
func BuildGhostRoomKey(serverID, mattermostUserID, roomID string) string {
	return KeyPrefixGhostRoom + serverID + "_" + mattermostUserID + "_" + roomID
}

// BuildMatrixEventPostKey creates a key for Matrix event -> post mapping
func BuildMatrixEventPostKey(serverID, matrixEventID string) string {
	return KeyPrefixMatrixEventPost + serverID + "_" + matrixEventID
}

// BuildMatrixReactionKey creates a key for Matrix reaction storage
func BuildMatrixReactionKey(serverID, reactionEventID string) string {
	return KeyPrefixMatrixReaction + serverID + "_" + reactionEventID
}
