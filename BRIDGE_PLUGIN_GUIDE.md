# Mattermost Bridge Plugin Development Guide

This guide documents the architecture and patterns used in the Matrix bridge plugin to help with developing other bridge plugins (e.g., XMPP, Slack, Discord).

## Development Approach: Start Fresh with Heavy Reference

**Recommendation**: Create a new repository and selectively copy/reference this Matrix bridge rather than forking.

**Why Start Fresh**:
- Clean git history focused on your target protocol
- No Matrix-specific assumptions or naming to clean up
- Optimal dependency choices for your protocol
- Easier mental model and reasoning about protocol-specific requirements

**How to Reference**: This guide shows what to copy directly, what to use as templates, and what patterns to study and reimplement.

## Project Overview

This Mattermost plugin provides bidirectional message synchronization between Mattermost and Matrix servers. The core patterns are protocol-agnostic and can be adapted for any chat protocol while the Matrix-specific implementation serves as a reference.

These files contain no protocol-specific logic and can be copied directly to your new project:

### Core Abstractions
```bash
# Copy these files exactly as-is
server/logger.go                    # Logger interface abstraction
server/store/kvstore/              # Complete KV store abstraction
server/testhelpers_test.go         # Test utility functions (adapt as needed)
```

### Build System and CI/CD
```bash
# Copy these for consistent build process
Makefile                           # Build targets (adapt protocol names)
.github/workflows/                 # CI/CD workflows
build/                            # Build tools and scripts
.golangci.yml                     # Go linting configuration
```

### Project Metadata
```bash
# Copy and adapt these
.gitignore                        # Standard ignore patterns
README.md                         # Use as template, replace content
LICENSE.txt                       # If using same license
```

## Files to Use as Templates

These files contain good patterns but need protocol-specific adaptation:

### Configuration Files
```bash
# Study these files, copy structure, replace Matrix-specific content
plugin.json                        # Plugin manifest and settings schema
server/configuration.go            # Configuration struct and validation
```

**Key Pattern - Configurable Username Prefix** (lines 9-135 in `configuration.go`):
```go
const DefaultXMPPUsernamePrefix = "xmpp"  // Replace "matrix" with your protocol

type configuration struct {
    XMPPServerURL        string `json:"xmpp_server_url"`     // Replace Matrix fields
    XMPPUsername         string `json:"xmpp_username"`
    XMPPPassword         string `json:"xmpp_password"`
    EnableSync           bool   `json:"enable_sync"`
    XMPPUsernamePrefix   string `json:"xmpp_username_prefix"`
}

func (c *configuration) GetXMPPUsernamePrefix() string {
    if c.XMPPUsernamePrefix == "" {
        return DefaultXMPPUsernamePrefix
    }
    return c.XMPPUsernamePrefix
}
```

### Webapp Admin Console
```bash
# Copy component patterns, adapt for your protocol
webapp/src/components/admin_console_settings/
webapp/src/index.tsx               # Component registration patterns
```

## Patterns to Study and Reimplement

These files contain the core bridge logic that you should understand deeply and reimplement for your protocol:

### Core Plugin Architecture
```bash
# Study these files - understand patterns, reimplement for your protocol
server/plugin.go                   # Plugin lifecycle and bridge initialization
server/hooks.go                    # Mattermost event handling
server/api.go                      # HTTP API endpoints (optional)
server/sync_to_matrix.go           # Mattermost → Protocol sync (core bridge logic)
server/sync_to_mattermost.go       # Protocol → Mattermost sync (core bridge logic)
```

### Protocol Client Interface
```bash
# Study the interface design, implement for your protocol
server/matrix/client.go            # Protocol client abstraction
```

### Testing Infrastructure
```bash
# Study testing patterns, adapt for your protocol
server/*_test.go                   # Unit testing patterns
server/*_integration_test.go       # Integration testing with real servers
testcontainers/matrix/            # Protocol test server patterns
```

## Critical Architectural Patterns to Preserve

### 1. Bidirectional Bridge Pattern

The plugin implements two separate bridge classes:

#### MattermostToProtocolBridge
- Handles Mattermost events → Protocol server
- Implements loop prevention using `User.IsRemote()`
- Manages ghost user creation in protocol server
- **Reference**: `sync_to_matrix.go` (study and adapt)

#### ProtocolToMattermostBridge  
- Handles Protocol events → Mattermost
- Creates prefixed usernames for protocol users
- Sets `RemoteId` on Mattermost users for loop prevention
- **Reference**: `sync_to_mattermost.go` (study and adapt)

### 2. Loop Prevention Strategy (CRITICAL)

**Most Important Pattern**: Prevents infinite sync loops between systems

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
- **Reference**: `sync_to_mattermost.go:generateMattermostUsername()` for exact implementation

### 3. Bridge Initialization Pattern
```go
// Reference: server/plugin.go:initBridges()
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
// Reference: server/hooks.go
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

## Step-by-Step Implementation Guide

### Phase 1: Project Setup
1. **Create new repository**: `mattermost-plugin-xmpp-bridge`
2. **Copy protocol-agnostic files**:
   ```bash
   # Copy these exactly
   cp -r build/ ../xmpp-bridge/
   cp -r .github/ ../xmpp-bridge/
   cp server/logger.go ../xmpp-bridge/server/
   cp -r server/store/ ../xmpp-bridge/server/
   cp Makefile ../xmpp-bridge/  # Update protocol references
   ```
3. **Create basic project structure**:
   ```bash
   mkdir -p server/xmpp
   mkdir -p testcontainers/xmpp
   mkdir -p webapp/src/components/admin_console_settings
   ```

### Phase 2: Configuration and Core Plugin
1. **Adapt plugin.json**: Copy structure, replace Matrix fields with XMPP
2. **Adapt configuration.go**: Copy patterns, implement XMPP configuration
3. **Study and reimplement plugin.go**: Plugin lifecycle and initialization
4. **Study and reimplement hooks.go**: Event handling for XMPP

### Phase 3: Protocol Client
1. **Study server/matrix/client.go**: Understand interface design
2. **Implement server/xmpp/client.go**: XMPP protocol implementation
3. **Choose XMPP library**: Consider `mellium.im/xmpp` or `github.com/mattn/go-xmpp`

### Phase 4: Bridge Logic
1. **Study sync_to_matrix.go**: Understand Mattermost → Protocol patterns
2. **Implement sync_to_xmpp.go**: Mattermost → XMPP sync logic
3. **Study sync_to_mattermost.go**: Understand Protocol → Mattermost patterns  
4. **Implement sync_to_mattermost.go**: XMPP → Mattermost sync logic
5. **Implement loop prevention**: Critical - follow patterns exactly

### Phase 5: Testing
1. **Copy testhelpers_test.go**: Adapt test utilities
2. **Study testcontainers/matrix/**: Understand test server patterns
3. **Implement testcontainers/xmpp/**: XMPP test server container
4. **Study integration tests**: Adapt patterns for XMPP
5. **Implement unit tests**: Test message conversion and user mapping

## XMPP-Specific Implementation Considerations

### 1. Protocol Differences from Matrix
- **Connection**: XMPP uses persistent XML streams vs Matrix's HTTP REST API
- **Addressing**: XMPP JIDs (user@domain/resource) vs Matrix user IDs (@user:domain)
- **Rooms**: XMPP MUCs (Multi-User Chat) vs Matrix rooms
- **Authentication**: XMPP SASL vs Matrix access tokens

### 2. XMPP Client Interface Design
Based on `server/matrix/client.go`, implement:
```go
type XMPPClient interface {
    // Connection management
    Connect() error
    Disconnect() error
    
    // User management (if server supports user management)
    CreateUser(jid, password string) error
    GetUserInfo(jid string) (*UserInfo, error)
    
    // MUC (Multi-User Chat) management  
    CreateRoom(roomJID string) error
    JoinRoom(roomJID, nickname string) error
    
    // Message handling
    SendMessage(to, body string) error
    SendGroupMessage(roomJID, body string) error
    
    // File transfer (if supported)
    SendFile(to, filename string, data []byte) error
}
```

### 3. Username Mapping Strategy
```go
// XMPP JID: alice@xmpp.example.com/resource
// Mattermost username: xmpp:alice@xmpp.example.com

func (b *XMPPToMattermostBridge) generateMattermostUsername(jid string) string {
    config := b.getConfiguration()
    prefix := config.GetXMPPUsernamePrefix()
    
    // Remove resource part from JID
    bareJID := strings.Split(jid, "/")[0]
    
    return prefix + ":" + bareJID
}
```

### 4. Room/Channel Mapping
```go
// Store mappings in KV store
// Key: "channel_mapping_<mattermostChannelId>"
// Value: "room@conference.xmpp.example.com"

// Key: "xmpp_room_mapping_room@conference.xmpp.example.com"  
// Value: "<mattermostChannelId>"
```

## Testing Strategy Reference

### Study These Test Files
- `server/sync_to_matrix_integration_test.go`: Full integration test patterns
- `server/user_remote_detection_test.go`: Loop prevention testing
- `testcontainers/matrix/`: Test server container implementation
- `server/testhelpers_test.go`: Test utility patterns

### XMPP Test Container Considerations
- Use containers like `prosody/prosody` or `ejabberd/ejs`
- Configure MUC (mod_muc) for room testing
- Set up test users and authentication
- Consider in-band registration for dynamic user creation

## Reference Implementation

**Matrix Bridge Location**: `/home/dlauder/Development/wiggin77/mattermost-plugin-matrix-bridge/`

### Critical Files to Study:
1. **Loop Prevention**: `server/sync_to_mattermost.go:generateMattermostUsername()` (lines 120-140)
2. **Bridge Architecture**: `server/plugin.go:initBridges()` (lines 80-95)
3. **Configuration Pattern**: `server/configuration.go` (lines 9-135)
4. **Event Handling**: `server/hooks.go` (entire file)
5. **Client Interface**: `server/matrix/client.go` (lines 20-80)
6. **Integration Testing**: `server/sync_to_matrix_integration_test.go` (entire file)

### Key Implementation Patterns:
- **Configurable Username Prefix**: Enables flexible deployments
- **Bidirectional Bridges**: Separate classes for each sync direction
- **Loop Prevention**: `User.IsRemote()` checks and `RemoteId` setting
- **KV Store Usage**: Channel mappings and user tracking
- **Test Infrastructure**: Real protocol servers for integration tests

## Development Commands

```bash
# These commands work with any protocol after copying the build system
make dist        # Build plugin bundle
make test        # Run all tests  
make check-style # Lint code
make apply       # Regenerate manifest files
make help        # Show available targets
```

## Implementation Checklist

### Phase 1: Setup ✅
- [ ] Create new repository with XMPP-focused naming
- [ ] Copy protocol-agnostic files (logger, kvstore, build system)
- [ ] Set up basic project structure

### Phase 2: Configuration ✅  
- [ ] Adapt plugin.json with XMPP configuration fields
- [ ] Implement XMPP configuration struct and validation
- [ ] Add configurable username prefix for XMPP

### Phase 3: Core Plugin ✅
- [ ] Study and reimplement plugin.go for XMPP client initialization
- [ ] Study and reimplement hooks.go for XMPP event handling
- [ ] Implement XMPP client interface and connection management

### Phase 4: Bridge Logic ✅
- [ ] Study sync_to_matrix.go and implement sync_to_xmpp.go
- [ ] Study sync_to_mattermost.go and implement XMPP → Mattermost sync
- [ ] Implement loop prevention using exact patterns from Matrix bridge
- [ ] Test username generation and user mapping

### Phase 5: Testing ✅
- [ ] Set up XMPP test container infrastructure
- [ ] Adapt integration test patterns for XMPP
- [ ] Test full message round-trip with loop prevention
- [ ] Test edge cases and error handling

### Phase 6: Polish ✅
- [ ] Update admin console components for XMPP setup
- [ ] Document XMPP server configuration requirements
- [ ] Create deployment and setup documentation