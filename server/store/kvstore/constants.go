package kvstore

// KV Store key prefixes and constants
// This file centralizes all KV store key patterns used throughout the plugin
// to ensure consistency and avoid key conflicts.

const (
	// CurrentKVStoreVersion is the current version requiring migrations
	CurrentKVStoreVersion = 2
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

	// KeyPrefixLegacyDMMapping was the old prefix for DM mappings (migrated to channel_mapping_)
	KeyPrefixLegacyDMMapping = "dm_mapping_"
	// KeyPrefixLegacyMatrixDMMapping was the old prefix for Matrix DM mappings (migrated to room_mapping_)
	KeyPrefixLegacyMatrixDMMapping = "matrix_dm_mapping_"
)

// Helper functions for building KV store keys

// BuildMatrixUserKey creates a key for Matrix user -> Mattermost user mapping
func BuildMatrixUserKey(matrixUserID string) string {
	return KeyPrefixMatrixUser + matrixUserID
}

// BuildMattermostUserKey creates a key for Mattermost user -> Matrix user mapping
func BuildMattermostUserKey(mattermostUserID string) string {
	return KeyPrefixMattermostUser + mattermostUserID
}

// BuildChannelMappingKey creates a key for channel -> room mapping
func BuildChannelMappingKey(channelID string) string {
	return KeyPrefixChannelMapping + channelID
}

// BuildRoomMappingKey creates a key for room -> channel mapping
func BuildRoomMappingKey(roomIdentifier string) string {
	return KeyPrefixRoomMapping + roomIdentifier
}

// BuildGhostUserKey creates a key for ghost user cache
func BuildGhostUserKey(mattermostUserID string) string {
	return KeyPrefixGhostUser + mattermostUserID
}

// BuildGhostRoomKey creates a key for ghost user room membership
func BuildGhostRoomKey(mattermostUserID, roomID string) string {
	return KeyPrefixGhostRoom + mattermostUserID + "_" + roomID
}

// BuildMatrixEventPostKey creates a key for Matrix event -> post mapping
func BuildMatrixEventPostKey(matrixEventID string) string {
	return KeyPrefixMatrixEventPost + matrixEventID
}

// BuildMatrixReactionKey creates a key for Matrix reaction storage
func BuildMatrixReactionKey(reactionEventID string) string {
	return KeyPrefixMatrixReaction + reactionEventID
}
