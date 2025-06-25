# Mattermost Bridge Plugin Development Guide

This guide documents the architecture and patterns used in the Matrix bridge plugin to help with developing other bridge plugins (e.g., XMPP, Slack, Discord).

## Project Overview

This Mattermost plugin provides bidirectional message synchronization between Mattermost and Matrix servers. The architecture can be adapted for other protocols by replacing the Matrix-specific components while keeping the core bridge patterns.

## Core Architecture Patterns

### 1. Plugin Structure

```
server/
├── plugin.go              # Main plugin entry point
├── configuration.go       # Plugin configuration management
├── sync_to_matrix.go      # Mattermost → Protocol sync logic
├── sync_to_mattermost.go  # Protocol → Mattermost sync logic
├── hooks.go               # Mattermost event hooks
├── api.go                 # HTTP API endpoints
├── logger.go              # Logging interface abstraction
├── matrix/                # Protocol-specific client
│   └── client.go
├── command/               # Slash command handlers
└── store/kvstore/         # Data persistence layer

webapp/
├── src/
│   ├── components/admin_console_settings/
│   │   ├── registration_download/     # Custom config components
│   │   └── homeserver_config/
│   └── index.tsx          # Component registration
```

### 2. Bidirectional Bridge Pattern

The plugin implements two separate bridge classes:

#### MattermostToProtocolBridge
- Handles Mattermost events → Protocol server
- Implements loop prevention using `User.IsRemote()`
- Manages ghost user creation in protocol server
- Located in: `sync_to_matrix.go` (rename to `sync_to_protocol.go`)

#### ProtocolToMattermostBridge  
- Handles Protocol events → Mattermost
- Creates prefixed usernames for protocol users
- Sets `RemoteId` on Mattermost users for loop prevention
- Located in: `sync_to_mattermost.go`

### 3. Loop Prevention Strategy

**Critical Pattern**: Prevents infinite sync loops between systems

```go
// In Mattermost → Protocol sync
if user.IsRemote() {
    // Skip - this user originated from the protocol server
    return nil
}

// In Protocol → Mattermost sync  
mattermostUser.RemoteId = &pluginRemoteID // Mark as remote-originated
```

**Key Implementation Points**:
- `User.IsRemote()` returns true when `RemoteId` field is set
- Each bridge has a unique `remoteID` identifier
- Always check `IsRemote()` before syncing to prevent loops

## Protocol-Specific Adaptations for XMPP

### 1. Replace Matrix Client with XMPP Client

**Current**: `server/matrix/client.go`
**New**: `server/xmpp/client.go`

**Key Methods to Implement**:
```go
type XMPPClient interface {
    // Connection management
    Connect() error
    Disconnect() error
    
    // User management
    CreateUser(userID, username, email string) error
    GetUserProfile(userID string) (*UserProfile, error)
    
    // Room/Channel management  
    CreateRoom(name string) (string, error)
    JoinRoom(roomID, userID string) error
    
    // Message handling
    SendMessage(req MessageRequest) (*MessageResponse, error)
    SendReaction(roomID, eventID, emoji string) error
    
    // File handling
    UploadFile(filename string, data []byte) (string, error)
}
```

### 2. Update Configuration Schema

**File**: `plugin.json` - settings_schema section

Replace Matrix-specific fields:
```json
{
    "key": "xmpp_server_url",
    "display_name": "XMPP Server URL", 
    "type": "text",
    "help_text": "The URL of the XMPP server (e.g., xmpp://chat.example.com)"
},
{
    "key": "xmpp_username",
    "display_name": "XMPP Bot Username",
    "type": "text", 
    "help_text": "Username for the XMPP bot account"
},
{
    "key": "xmpp_password", 
    "display_name": "XMPP Bot Password",
    "type": "password",
    "help_text": "Password for the XMPP bot account"
}
```

### 3. Adapt Message Sync Logic

**Key Areas to Update**:

1. **Message Format Conversion** (`sync_to_protocol.go`):
   - Convert Mattermost markdown → XMPP message format
   - Handle file attachments via XMPP file transfer
   - Map Mattermost reactions to XMPP equivalents

2. **User Mapping** (`sync_to_mattermost.go`):
   - Generate Mattermost usernames from XMPP JIDs
   - Use configurable prefix (e.g., "xmpp:user@domain.com")
   - Handle XMPP presence → Mattermost user status

3. **Room/Channel Mapping**:
   - Map XMPP MUCs (Multi-User Chat) ↔ Mattermost channels
   - Store mappings in KV store: `channel_mapping_<channelId>` → `xmpp_room_jid`

### 4. Configuration Management Updates

**File**: `server/configuration.go`

```go
type configuration struct {
    XMPPServerURL      string `json:"xmpp_server_url"`
    XMPPUsername       string `json:"xmpp_username"`  
    XMPPPassword       string `json:"xmpp_password"`
    EnableSync         bool   `json:"enable_sync"`
    XMPPUsernamePrefix string `json:"xmpp_username_prefix"`
}

// GetXMPPUsernamePrefix returns the username prefix for XMPP-originated users
func (c *configuration) GetXMPPUsernamePrefix() string {
    if c.XMPPUsernamePrefix == "" {
        return DefaultXMPPUsernamePrefix // e.g., "xmpp"
    }
    return c.XMPPUsernamePrefix
}
```

## Key Implementation Files to Update

### 1. Core Plugin Files
- `plugin.go`: Update protocol client initialization
- `configuration.go`: Replace Matrix config with XMPP config  
- `sync_to_protocol.go`: Mattermost → XMPP sync logic
- `sync_to_mattermost.go`: XMPP → Mattermost sync logic

### 2. Protocol Client
- `server/xmpp/client.go`: XMPP protocol implementation
- Consider using libraries like `mellium.im/xmpp` or `github.com/mattn/go-xmpp`

### 3. Testing Infrastructure
- `server/testcontainers/xmpp/`: XMPP test server container
- Update integration tests to use XMPP test server
- Adapt existing test patterns from `*_integration_test.go` files

### 4. Webapp Components (Optional)
- Update admin console components for XMPP-specific configuration
- Replace Matrix homeserver config with XMPP server setup instructions

## Important Patterns to Preserve

### 1. Logger Interface Pattern
```go
// server/logger.go - Keep this abstraction
type Logger interface {
    LogDebug(message string, keyValuePairs ...any)
    LogInfo(message string, keyValuePairs ...any) 
    LogWarn(message string, keyValuePairs ...any)
    LogError(message string, keyValuePairs ...any)
}
```

### 2. KV Store Abstraction
```go
// Keep the KV store interface for data persistence
type KVStore interface {
    Get(key string) ([]byte, error)
    Set(key string, value []byte) error
    Delete(key string) error
    ListKeys(page, perPage int) ([]string, error)
}
```

### 3. Bridge Initialization Pattern
```go
func (p *Plugin) initBridges() {
    p.mattermostToXMPPBridge = NewMattermostToXMPPBridge(
        p.xmppClient,
        p.logger,
        p.getConfiguration,
        // ... other dependencies
    )
    
    p.xmppToMattermostBridge = NewXMPPToMattermostBridge(
        p.API,
        p.logger, 
        p.getConfiguration,
        // ... other dependencies
    )
}
```

### 4. Event Hook Pattern
```go
// server/hooks.go
func (p *Plugin) MessageHasBeenPosted(c *plugin.Context, post *model.Post) {
    if !p.getConfiguration().EnableSync {
        return
    }
    
    // Sync to XMPP
    go func() {
        if err := p.mattermostToXMPPBridge.SyncPostToXMPP(post); err != nil {
            p.logger.LogError("Failed to sync post to XMPP", "error", err)
        }
    }()
}
```

## Testing Strategy

### 1. Unit Tests
- Test message format conversion
- Test user mapping logic
- Test configuration validation
- Use mocks for XMPP client

### 2. Integration Tests  
- Use testcontainers for real XMPP server
- Test full message round-trip
- Test loop prevention
- Test file transfer

### 3. Test Infrastructure Files to Adapt
- `server/testhelpers_test.go`: Update for XMPP client mocks
- `server/*_integration_test.go`: Adapt Matrix container → XMPP container
- `testcontainers/`: Create XMPP test container

## Protocol-Specific Considerations for XMPP

### 1. Connection Management
- XMPP uses persistent connections vs Matrix's HTTP API
- Implement connection retry logic
- Handle connection state in client

### 2. Message Addressing  
- XMPP uses JIDs (user@domain/resource)
- Map JIDs ↔ Mattermost user IDs consistently
- Handle resource part of JIDs appropriately

### 3. Presence and Status
- XMPP has rich presence (online/away/busy/offline)
- Map to Mattermost user status if desired
- Consider implementing presence sync

### 4. File Transfer
- XMPP file transfer uses different mechanisms
- May need HTTP file upload for larger files
- Consider XMPP's Stream Initiation (SI) protocol

## Build and Development

### Commands to Update
```bash
# These remain the same
make dist        # Build plugin
make test        # Run tests  
make check-style # Lint code

# Update documentation
make help        # Shows available targets
```

### Environment Variables
Update build scripts to use XMPP-specific environment variables as needed.

## Reference Implementation

This Matrix bridge plugin is located at:
`/home/dlauder/Development/wiggin77/mattermost-plugin-matrix-bridge/`

Key reference files:
- Architecture: `server/plugin.go`, `server/sync_to_*.go`
- Loop prevention: `server/sync_to_mattermost.go:generateMattermostUsername()`
- Configuration: `server/configuration.go`, `plugin.json`
- Testing: `server/*_test.go`, `testcontainers/matrix/`
- Protocol client: `server/matrix/client.go`

The configurable username prefix feature (lines 9-135 in `configuration.go`) demonstrates how to make the bridge flexible for different deployment scenarios.

## Migration Checklist

- [ ] Replace Matrix client with XMPP client implementation
- [ ] Update configuration schema in `plugin.json`
- [ ] Adapt message format conversion logic
- [ ] Update user and room mapping functions
- [ ] Create XMPP test infrastructure
- [ ] Update integration tests
- [ ] Update admin console components
- [ ] Test loop prevention thoroughly
- [ ] Document XMPP-specific setup requirements