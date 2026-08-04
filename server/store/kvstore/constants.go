package kvstore

// KV Store key prefixes and constants
// This file centralizes all KV store key patterns used throughout the plugin
// to ensure consistency and avoid key conflicts.

const (
	// CurrentKVStoreVersion is the current version requiring migrations
	CurrentKVStoreVersion = 3

	// KeyServersConfig is the key under which the JSON array of ServerConfig entries
	// (the homeserver registry) is stored.
	KeyServersConfig = "servers"

	// KeyPrefixMatrixUser is the prefix for Matrix user ID -> Mattermost user ID mappings.
	// Namespaced per server: matrix_user_<serverID>_<matrixUserID>
	KeyPrefixMatrixUser = "matrix_user_"
	// KeyPrefixMattermostUser is the prefix for Mattermost user ID -> Matrix user ID mappings.
	// Namespaced per server: mattermost_user_<serverID>_<mattermostUserID>
	KeyPrefixMattermostUser = "mattermost_user_"

	// KeyPrefixChannelMapping is the prefix for Mattermost channel ID -> Matrix server/room mappings.
	// NOT namespaced per server - the server(s) live in the value (see ChannelServerMapping).
	KeyPrefixChannelMapping = "channel_mapping_"
	// KeyPrefixRoomMapping is the prefix for Matrix room identifier -> Mattermost channel ID mappings.
	// Namespaced per server: room_mapping_<serverID>_<roomIdentifier>
	KeyPrefixRoomMapping = "room_mapping_"

	// KeyPrefixGhostUser is the prefix for Mattermost user ID -> Matrix ghost user ID cache.
	// Namespaced per server: ghost_user_<serverID>_<mattermostUserID>
	KeyPrefixGhostUser = "ghost_user_"
	// KeyPrefixGhostRoom is the prefix for ghost user room membership tracking.
	// Namespaced per server: ghost_room_<serverID>_<mattermostUserID>_<roomID>
	KeyPrefixGhostRoom = "ghost_room_"

	// KeyPrefixMatrixEventPost is the prefix for Matrix event ID -> Mattermost post ID mappings.
	// Namespaced per server: matrix_event_post_<serverID>_<matrixEventID>
	KeyPrefixMatrixEventPost = "matrix_event_post_"
	// KeyPrefixMatrixReaction is the prefix for Matrix reaction event ID -> reaction info mappings.
	// Namespaced per server: matrix_reaction_<serverID>_<reactionEventID>
	KeyPrefixMatrixReaction = "matrix_reaction_"

	// KeyStoreVersion is the key for tracking the current KV store schema version
	KeyStoreVersion = "kv_store_version"

	// KeyPrefixLegacyDMMapping was the old prefix for DM mappings (migrated to channel_mapping_)
	KeyPrefixLegacyDMMapping = "dm_mapping_"
	// KeyPrefixLegacyMatrixDMMapping was the old prefix for Matrix DM mappings (migrated to room_mapping_)
	KeyPrefixLegacyMatrixDMMapping = "matrix_dm_mapping_"
)

// namespacedKeysBySeverIDPrefixes lists every KV prefix that gained a serverID dimension in v3.
// Used by the v2->v3 migration to rekey the legacy un-namespaced layout.
var NamespacedKeyPrefixes = []string{ //nolint:revive // exported for use by migrations.go
	KeyPrefixMatrixUser,
	KeyPrefixMattermostUser,
	KeyPrefixGhostUser,
	KeyPrefixGhostRoom,
	KeyPrefixMatrixEventPost,
	KeyPrefixMatrixReaction,
	KeyPrefixRoomMapping,
}

// Helper functions for building KV store keys.
//
// The seven prefixes above are namespaced per Matrix server: <prefix><serverID>_<id>.
// BuildChannelMappingKey stays server-agnostic - the server(s) a channel is bridged to
// live in the stored value (see ChannelServerMapping), not the key.

// BuildMatrixUserKey creates a key for Matrix user -> Mattermost user mapping
func BuildMatrixUserKey(serverID, matrixUserID string) string {
	return KeyPrefixMatrixUser + serverID + "_" + matrixUserID
}

// BuildMattermostUserKey creates a key for Mattermost user -> Matrix user mapping
func BuildMattermostUserKey(serverID, mattermostUserID string) string {
	return KeyPrefixMattermostUser + serverID + "_" + mattermostUserID
}

// BuildChannelMappingKey creates a key for channel -> server/room mapping
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
