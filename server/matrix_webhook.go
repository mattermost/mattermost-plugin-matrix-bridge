package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/mattermost/logr/v2"
	"github.com/pkg/errors"
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

// processedTransactions stores transaction IDs to prevent duplicate processing
var processedTransactions = make(map[string]time.Time)

// cleanupOldTransactions removes transaction IDs older than 1 hour
func (p *Plugin) cleanupOldTransactions() {
	cutoff := time.Now().Add(-time.Hour)
	for txnID, timestamp := range processedTransactions {
		if timestamp.Before(cutoff) {
			delete(processedTransactions, txnID)
		}
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

	// Extract transaction ID from URL path
	vars := mux.Vars(r)
	txnID := vars["txnId"]
	if txnID == "" {
		p.logger.LogWarn("Matrix webhook received request without transaction ID")
		http.Error(w, "Missing transaction ID", http.StatusBadRequest)
		return
	}

	// Authentication is handled by MatrixAuthorizationRequired middleware

	// Check for duplicate transaction (idempotency)
	if timestamp, exists := processedTransactions[txnID]; exists {
		p.logger.LogDebug("Duplicate Matrix transaction ignored", "txn_id", txnID, "previous_timestamp", timestamp)
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte("{}")); err != nil {
			p.logger.LogWarn("Failed to write webhook response", "error", err)
		}
		return
	}

	// Read transaction body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		p.logger.LogError("Failed to read Matrix webhook body", "error", err, "txn_id", txnID)
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	// Log the raw transaction body for debugging (only if MM_MATRIX_LOG_FILESPEC is set)
	p.transactionLogger.Debug("Received Matrix transaction", logr.String("txn_id", txnID), logr.String("body", string(body)))

	// Parse transaction JSON
	var transaction MatrixTransaction
	if err := json.Unmarshal(body, &transaction); err != nil {
		p.logger.LogError("Failed to parse Matrix transaction JSON", "error", err, "txn_id", txnID, "body", string(body))
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// Mark transaction as processed
	processedTransactions[txnID] = time.Now()

	// Clean up old transactions periodically
	if len(processedTransactions)%100 == 0 {
		go p.cleanupOldTransactions()
	}

	p.logger.LogDebug("Processing Matrix transaction", "txn_id", txnID, "event_count", len(transaction.Events))

	// Process each event in the transaction
	for _, event := range transaction.Events {
		if err := p.processMatrixEvent(event); err != nil {
			p.logger.LogError("Failed to process Matrix event", "error", err, "event_id", event.EventID, "event_type", event.Type, "room_id", event.RoomID, "txn_id", txnID)
			// Continue processing other events even if one fails
			continue
		}
	}

	p.logger.LogDebug("Successfully processed Matrix transaction", "txn_id", txnID, "event_count", len(transaction.Events))

	// Return success response
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte("{}")); err != nil {
		p.logger.LogWarn("Failed to write webhook response", "error", err)
	}
}

// processMatrixEvent routes a single Matrix event to the appropriate handler
func (p *Plugin) processMatrixEvent(event MatrixEvent) error {
	// Only process events from rooms we have mapped to Mattermost channels
	channelID, err := p.getChannelIDFromMatrixRoom(event.RoomID)
	if err != nil {
		return errors.Wrap(err, "failed to get channel ID from Matrix room")
	}
	if channelID == "" {
		p.logger.LogDebug("Ignoring event from unmapped Matrix room", "room_id", event.RoomID, "event_type", event.Type)
		return nil // Not an error - just not mapped
	}

	// Skip events from our own ghost users to prevent loops
	if p.isGhostUser(event.Sender) {
		p.logger.LogDebug("Ignoring event from ghost user", "sender", event.Sender, "event_type", event.Type, "room_id", event.RoomID)
		return nil
	}

	p.logger.LogDebug("Processing Matrix event", "event_id", event.EventID, "event_type", event.Type, "sender", event.Sender, "room_id", event.RoomID, "channel_id", channelID)

	// Route event based on type
	switch event.Type {
	case "m.room.message":
		return p.matrixToMattermostBridge.syncMatrixMessageToMattermost(event, channelID)
	case "m.reaction":
		return p.matrixToMattermostBridge.syncMatrixReactionToMattermost(event, channelID)
	case "m.room.member":
		return p.matrixToMattermostBridge.syncMatrixMemberEventToMattermost(event, channelID)
	case "m.room.redaction":
		return p.matrixToMattermostBridge.syncMatrixRedactionToMattermost(event, channelID)
	default:
		p.logger.LogDebug("Ignoring unsupported event type", "event_type", event.Type, "event_id", event.EventID, "room_id", event.RoomID)
		return nil
	}
}

// getChannelIDFromMatrixRoom finds the Mattermost channel ID for a Matrix room ID
func (p *Plugin) getChannelIDFromMatrixRoom(roomID string) (string, error) {
	// Iterate through all channel mappings to find the one that maps to this room
	keys, err := p.kvstore.ListKeys(0, 1000)
	if err != nil {
		return "", errors.Wrap(err, "failed to list kvstore keys")
	}

	channelMappingPrefix := "channel_mapping_"
	for _, key := range keys {
		if strings.HasPrefix(key, channelMappingPrefix) {
			roomIdentifierBytes, err := p.kvstore.Get(key)
			if err != nil {
				continue
			}

			roomIdentifier := string(roomIdentifierBytes)

			// Check if this mapping points to our room
			// Handle both room aliases and room IDs
			if roomIdentifier == roomID {
				// Direct match with room ID
				channelID := strings.TrimPrefix(key, channelMappingPrefix)
				return channelID, nil
			}

			// If it's a room alias, resolve it to room ID and compare
			if strings.HasPrefix(roomIdentifier, "#") && p.matrixClient != nil {
				resolvedRoomID, err := p.matrixClient.ResolveRoomAlias(roomIdentifier)
				if err == nil && resolvedRoomID == roomID {
					channelID := strings.TrimPrefix(key, channelMappingPrefix)
					return channelID, nil
				}
			}
		}
	}

	return "", nil // No mapping found
}

// isGhostUser checks if a Matrix user ID belongs to one of our ghost users
func (p *Plugin) isGhostUser(userID string) bool {
	config := p.getConfiguration()
	if config.MatrixServerURL == "" {
		p.logger.LogDebug("isGhostUser: no MatrixServerURL configured", "user_id", userID)
		return false
	}

	// Extract server domain (get the real domain, not sanitized for property keys)
	parsedURL, err := url.Parse(config.MatrixServerURL)
	if err != nil || parsedURL.Hostname() == "" {
		p.logger.LogDebug("isGhostUser: failed to parse MatrixServerURL", "url", config.MatrixServerURL, "error", err)
		return false
	}
	serverDomain := parsedURL.Hostname()

	// Ghost users follow the pattern: @_mattermost_<mattermost_user_id>:<server_domain>
	ghostUserPrefix := "@_mattermost_"
	ghostUserSuffix := fmt.Sprintf(":%s", serverDomain)

	return strings.HasPrefix(userID, ghostUserPrefix) && strings.HasSuffix(userID, ghostUserSuffix)
}
