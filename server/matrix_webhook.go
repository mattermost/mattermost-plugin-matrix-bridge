package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/mattermost/logr/v2"
	"github.com/pkg/errors"

	"github.com/mattermost/mattermost-plugin-matrix-bridge/server/store/kvstore"
)

// MatrixEvent represents a Matrix event received via Application Service webhook
type MatrixEvent struct {
	Type       string         `json:"type"`
	EventID    string         `json:"event_id"`
	Sender     string         `json:"sender"`
	RoomID     string         `json:"room_id"`
	Content    map[string]any `json:"content"`
	StateKey   *string        `json:"state_key,omitempty"`
	Timestamp  int64          `json:"origin_server_ts"`
	Unsigned   map[string]any `json:"unsigned,omitempty"`
	PrevEvents []string       `json:"prev_events,omitempty"`
}

// MatrixTransaction represents a transaction from the Matrix homeserver
type MatrixTransaction struct {
	Events []MatrixEvent `json:"events"`
}

// transactionKey identifies a processed transaction. Matrix transaction IDs are only
// unique per homeserver, so the dedupe map must be keyed by (serverID, txnID), not
// txnID alone - otherwise a txnId reused across two servers would be dropped as a
// duplicate of the other server's transaction.
type transactionKey struct {
	serverID string
	txnID    string
}

// processedTransactions stores processed transaction keys to prevent duplicate processing
var (
	processedTransactions = make(map[transactionKey]time.Time)
	transactionsMutex     sync.RWMutex
)

// cleanupOldTransactions removes transaction keys older than 1 hour
func (p *Plugin) cleanupOldTransactions() {
	cutoff := time.Now().Add(-time.Hour)

	// Create a list of transaction keys to delete to avoid holding the lock during iteration
	var toDelete []transactionKey

	transactionsMutex.RLock()
	for key, timestamp := range processedTransactions {
		if timestamp.Before(cutoff) {
			toDelete = append(toDelete, key)
		}
	}
	transactionsMutex.RUnlock()

	// Delete the old transactions with write lock
	if len(toDelete) > 0 {
		transactionsMutex.Lock()
		for _, key := range toDelete {
			// Double-check the timestamp in case it was updated between read and write lock
			if timestamp, exists := processedTransactions[key]; exists && timestamp.Before(cutoff) {
				delete(processedTransactions, key)
			}
		}
		transactionsMutex.Unlock()
	}
}

// handleMatrixTransaction processes a Matrix Application Service transaction
func (p *Plugin) handleMatrixTransaction(w http.ResponseWriter, r *http.Request) {
	// Verify HTTP method
	if r.Method != http.MethodPut {
		p.logger.LogWarn("Matrix webhook received non-PUT request", "method", r.Method)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	serverID, ok := serverIDFromContext(r.Context())
	if !ok || serverID == "" {
		p.logger.LogError("Matrix webhook reached handler without a resolved serverID")
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// Extract transaction ID from URL path
	vars := mux.Vars(r)
	txnID := vars["txnId"]
	if txnID == "" {
		p.logger.LogWarn("Matrix webhook received request without transaction ID")
		http.Error(w, "Missing transaction ID", http.StatusBadRequest)
		return
	}

	// If this node's client cache lags the cluster-shared registry (e.g. a server was
	// just added on another node), respond 503 with Retry-After and do NOT record the
	// transaction as processed - the AS spec has the homeserver retry the same txnId,
	// so the events survive and a retry can land on an up-to-date node.
	if p.getMatrixClient(serverID) == nil {
		p.logger.LogWarn("No Matrix client for server; asking homeserver to retry", "server_id", serverID, "txn_id", txnID)
		w.Header().Set("Retry-After", "5")
		http.Error(w, "Server temporarily unavailable", http.StatusServiceUnavailable)
		return
	}

	key := transactionKey{serverID: serverID, txnID: txnID}

	// Check for duplicate transaction (idempotency)
	transactionsMutex.RLock()
	timestamp, exists := processedTransactions[key]
	transactionsMutex.RUnlock()

	if exists {
		p.logger.LogDebug("Duplicate Matrix transaction ignored", "server_id", serverID, "txn_id", txnID, "previous_timestamp", timestamp)
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("{}")); err != nil {
			p.logger.LogWarn("Failed to write webhook response", "error", err)
		}
		return
	}

	// Read transaction body with size limit to prevent memory exhaustion attacks
	// Use the server's configured MaxFileSize as the limit for webhook payloads
	// This ensures consistency with the server's file handling limits
	config := p.API.GetConfig()
	maxRequestBodySize := int64(10 << 20) // Default 10MB fallback
	if config.FileSettings.MaxFileSize != nil {
		maxRequestBodySize = *config.FileSettings.MaxFileSize
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		p.logger.LogError("Failed to read Matrix webhook body", "error", err, "server_id", serverID, "txn_id", txnID)
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	// Log the raw transaction body for debugging (only if MM_MATRIX_LOG_FILESPEC is set)
	p.transactionLogger.Debug("Received Matrix transaction", logr.String("server_id", serverID), logr.String("txn_id", txnID), logr.String("body", string(body)))

	// Parse transaction JSON
	var transaction MatrixTransaction
	if err := json.Unmarshal(body, &transaction); err != nil {
		p.logger.LogError("Failed to parse Matrix transaction JSON", "error", err, "server_id", serverID, "txn_id", txnID, "body", string(body))
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// Mark transaction as processed
	transactionsMutex.Lock()
	processedTransactions[key] = time.Now()
	shouldCleanup := len(processedTransactions)%100 == 0
	transactionsMutex.Unlock()

	// Clean up old transactions periodically
	if shouldCleanup {
		go p.cleanupOldTransactions()
	}

	p.logger.LogDebug("Processing Matrix transaction", "server_id", serverID, "txn_id", txnID, "event_count", len(transaction.Events))

	// Process each event in the transaction
	for _, event := range transaction.Events {
		if err := p.processMatrixEvent(serverID, event); err != nil {
			p.logger.LogError("Failed to process Matrix event", "error", err, "event_id", event.EventID, "event_type", event.Type, "room_id", event.RoomID, "server_id", serverID, "txn_id", txnID)
			// Continue processing other events even if one fails
			continue
		}
	}

	p.logger.LogDebug("Successfully processed Matrix transaction", "server_id", serverID, "txn_id", txnID, "event_count", len(transaction.Events))

	// Return success response
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte("{}")); err != nil {
		p.logger.LogWarn("Failed to write webhook response", "error", err)
	}
}

// processMatrixEvent routes a single Matrix event, received on serverID, to the
// appropriate handler
func (p *Plugin) processMatrixEvent(serverID string, event MatrixEvent) error {
	// Check if we have an existing mapping for this room
	channelID, err := p.getChannelIDFromMatrixRoom(serverID, event.RoomID)
	if err != nil {
		return errors.Wrap(err, "failed to get channel ID from Matrix room")
	}

	// If no existing mapping, check if this is a DM that we should create
	if channelID == "" {
		// Only attempt DM creation for room membership events or messages
		if event.Type == "m.room.member" || event.Type == "m.room.message" {
			// Check if this is a Matrix-initiated DM that we should bridge
			newChannelID, err := p.handleMatrixInitiatedDM(serverID, event)
			if err != nil {
				p.logger.LogWarn("Failed to handle Matrix-initiated DM", "room_id", event.RoomID, "server_id", serverID, "error", err)
				return nil // Don't error - just skip processing
			}
			if newChannelID != "" {
				channelID = newChannelID
				p.logger.LogInfo("Created new DM channel for Matrix room", "room_id", event.RoomID, "server_id", serverID, "channel_id", channelID)
			}
		}

		// If still no mapping, ignore the event
		if channelID == "" {
			p.logger.LogDebug("Ignoring event from unmapped Matrix room", "room_id", event.RoomID, "server_id", serverID, "event_type", event.Type)
			return nil // Not an error - just not mapped
		}
	}

	// Skip events from our own ghost users to prevent loops
	if p.isGhostUser(serverID, event.Sender) {
		p.logger.LogDebug("Ignoring event from ghost user", "sender", event.Sender, "event_type", event.Type, "room_id", event.RoomID, "server_id", serverID)
		return nil
	}

	p.logger.LogDebug("Processing Matrix event", "event_id", event.EventID, "event_type", event.Type, "sender", event.Sender, "room_id", event.RoomID, "server_id", serverID, "channel_id", channelID)

	bridge, err := p.newMatrixToMattermostBridge(serverID)
	if err != nil {
		return errors.Wrap(err, "failed to build Matrix-to-Mattermost bridge")
	}

	// Route event based on type
	switch event.Type {
	case "m.room.message":
		return bridge.syncMatrixMessageToMattermost(event, channelID)
	case "m.reaction":
		return bridge.syncMatrixReactionToMattermost(event, channelID)
	case "m.room.member":
		return bridge.syncMatrixMemberEventToMattermost(event, channelID)
	case "m.room.redaction":
		return bridge.syncMatrixRedactionToMattermost(event, channelID)
	default:
		p.logger.LogDebug("Ignoring unsupported event type", "event_type", event.Type, "event_id", event.EventID, "room_id", event.RoomID, "server_id", serverID)
		return nil
	}
}

// getChannelIDFromMatrixRoom finds the Mattermost channel ID for a Matrix room ID on serverID
func (p *Plugin) getChannelIDFromMatrixRoom(serverID, roomID string) (string, error) {
	// First check KV store mapping (trusted source): room_mapping_<serverID>_<roomID> -> channelID
	roomMappingKey := kvstore.BuildRoomMappingKey(serverID, roomID)
	channelIDBytes, err := p.kvstore.Get(roomMappingKey)
	if err == nil && len(channelIDBytes) > 0 {
		channelID := string(channelIDBytes)
		p.logger.LogDebug("Found channel ID in KV store", "room_id", roomID, "server_id", serverID, "channel_id", channelID)
		return channelID, nil
	}

	// Fallback: get channel ID from Matrix room state (for race condition during room creation)
	if client := p.getMatrixClient(serverID); client != nil {
		channelID, err := client.GetMattermostChannelID(roomID)
		if err != nil {
			p.logger.LogDebug("Failed to get channel ID from room state", "room_id", roomID, "server_id", serverID, "error", err)
		} else if channelID != "" {
			p.logger.LogDebug("Found channel ID in room state (no KV mapping yet)", "room_id", roomID, "server_id", serverID, "channel_id", channelID)
			return channelID, nil
		}
	}

	return "", nil // No mapping found
}

// isGhostUser checks if a Matrix user ID belongs to one of our ghost users on serverID.
// The domain is resolved from the registry entry's ServerName, which is guaranteed
// non-empty and unique (§3.1.2) - there is deliberately no URL-host fallback, since one
// would only ever mask a bug: ghosts are created with the Matrix server name, which
// differs from the connection host under .well-known delegation and in tests.
func (p *Plugin) isGhostUser(serverID, userID string) bool {
	serverDomain, err := p.serverDomainForID(serverID)
	if err != nil || serverDomain == "" {
		p.logger.LogDebug("isGhostUser: could not resolve server domain", "server_id", serverID, "user_id", userID, "error", err)
		return false
	}

	// Ghost users follow the pattern: @_mattermost_<mattermost_user_id>:<server_domain>
	ghostUserPrefix := "@_mattermost_"
	ghostUserSuffix := fmt.Sprintf(":%s", serverDomain)

	return strings.HasPrefix(userID, ghostUserPrefix) && strings.HasSuffix(userID, ghostUserSuffix)
}

// handleMatrixInitiatedDM checks if an unmapped Matrix room on serverID is a potential
// DM that should be bridged. This uses a heuristic approach based on the event
// information available
func (p *Plugin) handleMatrixInitiatedDM(serverID string, event MatrixEvent) (string, error) {
	// Handle different event types that might indicate DM creation
	switch event.Type {
	case "m.room.member":
		return p.handleMatrixMemberDM(serverID, event)
	case "m.room.message":
		return p.handleMatrixMessageDM(serverID, event)
	default:
		return "", nil // Only handle member and message events
	}
}

// handleMatrixMemberDM processes member events to detect DM room creation with ghost users
func (p *Plugin) handleMatrixMemberDM(serverID string, event MatrixEvent) (string, error) {
	// Only handle join events to avoid creating channels for leave/invite events
	if event.Content == nil {
		p.logger.LogDebug("Member event has no content", "room_id", event.RoomID, "event_id", event.EventID)
		return "", nil
	}

	membership, ok := event.Content["membership"].(string)
	if !ok || (membership != "join" && membership != "invite") {
		p.logger.LogDebug("Member event is not a join or invite", "room_id", event.RoomID, "membership", membership)
		return "", nil
	}

	// The state_key in member events is the user being affected
	if event.StateKey == nil {
		p.logger.LogDebug("Member event has no state_key", "room_id", event.RoomID, "event_id", event.EventID)
		return "", nil
	}

	targetUserID := *event.StateKey // User being invited/joined
	actingUserID := event.Sender    // User doing the invite/join

	p.logger.LogDebug("Processing member event", "room_id", event.RoomID, "server_id", serverID, "membership", membership, "target_user", targetUserID, "acting_user", actingUserID)

	// Check if either user is one of our ghost users
	var ghostUserID, matrixUserID string
	switch {
	case p.isGhostUser(serverID, targetUserID):
		ghostUserID = targetUserID
		matrixUserID = actingUserID
	case p.isGhostUser(serverID, actingUserID):
		ghostUserID = actingUserID
		matrixUserID = targetUserID
	default:
		// Neither user is a ghost user - not a DM we care about
		p.logger.LogDebug("Neither user is a ghost user", "room_id", event.RoomID, "server_id", serverID, "target_user", targetUserID, "acting_user", actingUserID, "membership", membership)
		return "", nil
	}

	return p.createDMChannelForGhostUser(serverID, event.RoomID, ghostUserID, matrixUserID)
}

// handleMatrixMessageDM processes message events that might be in a DM with a ghost user
func (p *Plugin) handleMatrixMessageDM(serverID string, event MatrixEvent) (string, error) {
	// For message events, we can only work with the sender
	// If the sender is not a ghost user, we can't determine if this should be a DM
	matrixUserID := event.Sender

	// We can't reliably determine DM participants from a message event alone
	// without querying Matrix for room members. Skip for now.
	p.logger.LogDebug("Cannot determine DM participants from message event alone", "room_id", event.RoomID, "server_id", serverID, "sender", matrixUserID)
	return "", nil
}

// createDMChannelForGhostUser creates a Mattermost DM channel for a Matrix DM on
// serverID involving a verified ghost user
func (p *Plugin) createDMChannelForGhostUser(serverID, roomID, ghostUserID, matrixUserID string) (string, error) {
	// Extract Mattermost user ID from ghost user
	mattermostUserID := p.extractMattermostUserIDFromGhost(ghostUserID)
	if mattermostUserID == "" {
		return "", errors.New("failed to extract Mattermost user ID from ghost user")
	}

	// Verify that this ghost user exists in our KV store (meaning we created it)
	ghostUserKey := kvstore.BuildGhostUserKey(serverID, mattermostUserID)
	ghostUserData, err := p.kvstore.Get(ghostUserKey)
	if err != nil || len(ghostUserData) == 0 {
		p.logger.LogDebug("Rejecting DM creation for unrecognized ghost user", "ghost_user_id", ghostUserID, "mattermost_user_id", mattermostUserID, "server_id", serverID)
		return "", nil // Not our ghost user - reject silently
	}

	// Verify the stored ghost user ID matches what we expect
	storedGhostUserID := string(ghostUserData)
	if storedGhostUserID != ghostUserID {
		p.logger.LogWarn("Ghost user ID mismatch in KV store", "expected", ghostUserID, "stored", storedGhostUserID, "mattermost_user_id", mattermostUserID, "server_id", serverID)
		return "", nil // ID mismatch - reject silently
	}

	// Get the Mattermost user to verify they exist
	mattermostUser, appErr := p.API.GetUser(mattermostUserID)
	if appErr != nil {
		return "", errors.Wrap(appErr, "failed to get Mattermost user")
	}

	matrixToMattermostBridge, err := p.newMatrixToMattermostBridge(serverID)
	if err != nil {
		return "", err
	}

	// Create or get the Matrix user in Mattermost (using a dummy team - will be fixed by addUserToChannelTeam)
	mattermostMatrixUserID, err := matrixToMattermostBridge.getOrCreateMattermostUser(matrixUserID, "")
	if err != nil {
		return "", errors.Wrap(err, "failed to get or create Mattermost user for Matrix user")
	}

	// Create DM channel in Mattermost between the original Mattermost user and the Matrix user
	dmChannel, appErr := p.API.GetDirectChannel(mattermostUserID, mattermostMatrixUserID)
	if appErr != nil {
		return "", errors.Wrap(appErr, "failed to create DM channel in Mattermost")
	}

	// Store the mapping between Matrix room and Mattermost channel, through the
	// same choke point every other mapping write uses.
	if err := matrixToMattermostBridge.setChannelRoomMapping(dmChannel.Id, roomID); err != nil {
		return "", errors.Wrap(err, "failed to store channel room mapping")
	}

	p.logger.LogInfo("Created Matrix-initiated DM",
		"matrix_room_id", roomID,
		"mattermost_channel_id", dmChannel.Id,
		"matrix_user", matrixUserID,
		"mattermost_user", mattermostUser.Username,
		"ghost_user", ghostUserID,
		"server_id", serverID)

	return dmChannel.Id, nil
}
