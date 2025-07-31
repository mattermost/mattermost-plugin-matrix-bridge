# End-to-End Encrypted Rooms Support

This document outlines the research and implementation plan for adding E2E encrypted room support to the Mattermost-Matrix bridge plugin.

## Matrix E2E Encryption Overview

Matrix uses a dual-layer encryption approach:
- **Olm** (`m.olm.v1.curve25519-aes-sha2`): One-to-one messaging encryption
- **Megolm** (`m.megolm.v1.aes-sha2`): Group messaging encryption

Key components include:
- Device keys (Ed25519 identity keys, Curve25519 one-time keys)
- Room keys for group chat encryption
- Cross-signing for device verification
- Key backup and recovery mechanisms

## Architecture Analysis

The current bridge architecture presents several challenges for E2E encryption:

### Current Bridge Flow
```
Mattermost → hooks.go → sync_to_matrix.go → matrix/client.go → Matrix
Matrix → matrix_webhook.go → sync_to_mattermost.go → Mattermost
```

### Application Service Limitations
- Application Services are "passive" and can only observe events and inject events
- Cannot prevent or modify events in transit
- Limited device management capabilities for ghost users
- No explicit E2E encryption support in AS specification

## Key Challenges

### 1. Application Service Constraints
Application Services have fundamental limitations:
- Cannot participate in device-to-device key exchange protocols
- Ghost users created via AS registration don't include device key upload
- AS tokens don't support the full client-server API needed for E2E

### 2. Bridge Architectural Conflict
The bridge acts as a trusted intermediary that must:
- Decrypt Matrix messages to forward them to Mattermost
- Encrypt Mattermost messages for Matrix delivery
- This breaks the "end-to-end" nature of the encryption

### 3. Device Key Management
Each ghost user would need:
- Ed25519 identity keys for device verification
- Curve25519 one-time keys for initial key exchange
- Megolm session keys for group messaging
- Key rotation and backup strategies

### 4. Protocol Implementation Complexity
Full E2E support requires implementing:
- Olm library integration for key management
- Megolm session handling for group encryption
- Key verification workflows (SAS, QR codes, cross-signing)
- Key backup and recovery mechanisms

## Implementation Approaches

### Option 1: Transparent Bridge (Recommended)
**Concept**: Bridge participates in E2E encryption as a trusted intermediary

**Architecture**:
```
Mattermost ←→ Bridge (with keys) ←→ Encrypted Matrix Room
                  ↑
            Decrypts/Encrypts
```

**Implementation**:
- Bridge maintains device keys for each ghost user
- Implements Olm/Megolm protocols for key exchange
- Decrypts Matrix messages, forwards plaintext to Mattermost
- Encrypts Mattermost messages for Matrix delivery
- Stores encryption keys securely in KV store

**Advantages**:
- Full functionality maintained between platforms
- Users see properly formatted messages on both sides
- Reactions, edits, and file attachments work normally
- Bridge can participate in key verification

**Disadvantages**:
- Bridge has access to all plaintext messages
- Requires significant cryptographic implementation
- Security model becomes "bridge-to-end" rather than "end-to-end"

### Option 2: E2E-Aware Bridge
**Concept**: Bridge detects encrypted rooms but cannot decrypt content

**Implementation**:
- Detect encrypted events (`m.room.encrypted`)
- Forward encrypted payloads as special message types to Mattermost
- Display "🔒 Encrypted message (not supported)" placeholders
- Support unencrypted metadata (reactions, typing indicators)
- Allow room management commands

**Advantages**:
- Simpler implementation
- Preserves true end-to-end encryption
- Clear user expectations about limitations

**Disadvantages**:
- Very limited functionality in encrypted rooms
- Poor user experience for encrypted content
- Reactions and edits may not work on encrypted messages

### Option 3: Hybrid Approach
**Concept**: Combine both approaches with user choice

**Implementation**:
- Detect encryption status of rooms
- Offer configuration option for encryption handling
- "Transparent" mode for organizations that accept bridge access
- "Aware" mode for maximum security with limited functionality

## Detailed Implementation Plan

### Phase 1: Detection and Basic Support (2-3 weeks)

#### 1.1 Encrypted Room Detection
- **File**: `server/matrix_webhook.go`
  - Add handler for `m.room.encryption` state events
  - Store encryption status in room mappings
- **File**: `server/store/kvstore/constants.go`
  - Add `ROOM_ENCRYPTION_STATUS_PREFIX = "room_encryption_"`
- **File**: `server/sync_to_mattermost.go`
  - Update room joining logic to check encryption status

#### 1.2 Message Handling Updates
- **File**: `server/matrix_webhook.go:712`
  - Add detection for `m.room.encrypted` event types
  - Route encrypted events to new handler
- **File**: `server/sync_to_mattermost.go`
  - Add `handleEncryptedMessage()` function
  - Display "🔒 Encrypted message (not supported)" in Mattermost

#### 1.3 Configuration Options
- **File**: `server/configuration.go`
  - Add `SupportEncryptedRooms` boolean setting
  - Add `EncryptedRoomPolicy` enum (REJECT, AWARE, TRANSPARENT)
- **File**: `webapp/src/components/admin_console_settings/`
  - Add UI for encryption configuration options

### Phase 2: Transparent Bridge Implementation (6-8 weeks)

#### 2.1 Device Key Management
- **File**: `server/matrix/encryption.go` (new)
  - Implement Olm account creation for ghost users
  - Add device key generation and storage
  - Implement key upload during ghost user creation
- **File**: `server/store/kvstore/constants.go`
  - Add encryption key storage prefixes
- **File**: `server/matrix/client.go`
  - Extend `CreateGhostUser()` to include device key setup

#### 2.2 Megolm Implementation
- **File**: `server/matrix/megolm.go` (new)
  - Implement Megolm session management
  - Add key sharing for group rooms
  - Handle session persistence
- **File**: `server/matrix/encryption.go`
  - Add room key distribution logic
  - Implement key backup and recovery

#### 2.3 Protocol Integration
- **File**: `server/matrix/verification.go` (new)
  - Implement key verification flows
  - Add cross-signing support for ghost users
- **File**: `server/matrix/client.go`
  - Integrate encryption/decryption in `SendMessage()`
  - Add encrypted event handling in webhook processing

### Phase 3: Advanced Features (4-6 weeks)

#### 3.1 Security Enhancements
- **File**: `server/security/keystore.go` (new)
  - Implement secure key storage with encryption at rest
  - Add key escrow/backup for bridge continuity
  - Audit logging for key operations
- **File**: `server/configuration.go`
  - Add security configuration options
  - Key rotation policies

#### 3.2 User Experience
- **File**: `server/sync_to_mattermost.go`
  - Add encryption status indicators in Mattermost messages
  - Handle room upgrade scenarios (unencrypted → encrypted)
- **File**: `webapp/src/components/`
  - Add encryption status display in admin console
  - Key verification workflow UI

## Technical Dependencies

### Go Libraries Required
```go
// Add to go.mod
github.com/matrix-org/gomatrix v0.0.0-20220926102614-ceba4d9f7530
github.com/matrix-org/olm v3.2.14+incompatible
github.com/pkg/errors v0.9.1 // already present
```

### New Configuration Fields
```go
type Configuration struct {
    // ... existing fields ...
    
    // E2E Encryption Support
    SupportEncryptedRooms   bool   `json:"support_encrypted_rooms"`
    EncryptedRoomPolicy     string `json:"encrypted_room_policy"` // "reject", "aware", "transparent"
    EnableKeyBackup         bool   `json:"enable_key_backup"`
    KeyRotationIntervalDays int    `json:"key_rotation_interval_days"`
}
```

### Database Schema Changes
```go
// KV Store Keys
const (
    DEVICE_KEYS_PREFIX      = "device_keys_"      // device_keys_{ghost_user_id}
    MEGOLM_SESSIONS_PREFIX  = "megolm_sessions_"  // megolm_sessions_{room_id}_{session_id}
    ROOM_ENCRYPTION_PREFIX  = "room_encryption_"  // room_encryption_{room_id}
    OLMP_ACCOUNTS_PREFIX    = "olm_accounts_"     // olm_accounts_{ghost_user_id}
)
```

## Security Considerations

### Risk Assessment
- **Risk**: Bridge has access to all plaintext messages
- **Impact**: Breaks end-to-end encryption model
- **Likelihood**: Inherent to transparent bridge design

### Mitigation Strategies
1. **Secure Key Storage**: Encrypt all keys at rest using AES-256
2. **Audit Logging**: Log all key operations and message decryption events
3. **Access Controls**: Restrict bridge server access to authorized personnel
4. **Key Rotation**: Implement regular key rotation policies
5. **User Communication**: Clear documentation about bridge access to messages

### Compliance Considerations
- Document that bridge can access message content for regulatory requirements
- Implement data retention policies for encrypted content
- Consider geographic restrictions for key storage

## Testing Strategy

### Unit Tests
- **File**: `server/matrix/encryption_test.go`
  - Test key generation and storage
  - Test encryption/decryption workflows
- **File**: `server/matrix/megolm_test.go`
  - Test session management
  - Test key sharing protocols

### Integration Tests
- **File**: `server/e2ee_integration_test.go`
  - Test full encrypted message flow
  - Test room encryption status detection
  - Test ghost user device key management

### Security Tests
- Key storage security validation
- Encryption algorithm correctness
- Key rotation functionality

## Recommendations

### Immediate Next Steps
1. **Start with Phase 1**: Implement basic detection and awareness
2. **User Communication**: Add clear warnings about encryption limitations
3. **Configuration**: Allow administrators to control encryption support

### Long-term Strategy
1. **Transparent Bridge**: Recommended for organizations prioritizing functionality
2. **Security First**: Implement comprehensive key management and audit logging
3. **User Choice**: Provide clear options and documentation about security trade-offs

### Alternative Approaches
Consider implementing a "per-room" policy where:
- Critical rooms remain unencrypted for full bridge functionality
- Less sensitive rooms can use E2E encryption with limited bridge support
- Users choose encryption vs. functionality trade-offs per room

## Conclusion

E2E encrypted room support is technically feasible but requires significant implementation effort and careful security considerations. The transparent bridge approach provides the best user experience while maintaining bridge functionality, but fundamentally changes the security model from "end-to-end" to "bridge-to-end."

Organizations should carefully evaluate their security requirements against functionality needs when deciding whether to implement and enable E2E support in their bridge deployment.