# Mattermost Bridge for Matrix

[![Build Status](https://github.com/wiggin77/mattermost-plugin-matrix-bridge/actions/workflows/ci.yml/badge.svg)](https://github.com/wiggin77/mattermost-plugin-matrix-bridge/actions/workflows/ci.yml)
[![E2E Status](https://github.com/wiggin77/mattermost-plugin-matrix-bridge/actions/workflows/e2e.yml/badge.svg)](https://github.com/wiggin77/mattermost-plugin-matrix-bridge/actions/workflows/e2e.yml)

A powerful communication bridge that seamlessly connects Mattermost and Matrix servers, enabling real-time message synchronization between platforms. This plugin leverages Mattermost's Shared Channels API and Matrix's Application Service interface to provide transparent cross-platform communication.

## Features

### 🚀 Core Functionality
- **Real-time Message Sync**: Automatically syncs messages from Mattermost shared channels to Matrix rooms
- **Ghost User System**: Creates Matrix ghost users that represent Mattermost users for authentic message attribution
- **Room Management**: Create and map Matrix rooms to Mattermost channels via slash commands
- **Health Monitoring**: Built-in health checks and connection monitoring

### 👤 User Experience
- **Authentic Attribution**: Messages appear from the actual user (via ghost users), not a generic bot account
- **Display Name Matching**: Ghost users automatically inherit Mattermost user display names
- **Seamless Integration**: Works transparently with existing Mattermost workflows
- **Channel Mapping**: Flexible room-to-channel mapping with support for both aliases and room IDs

### 🔧 Administration
- **Easy Configuration**: Simple admin console setup with generated security tokens
- **Auto-Generated Registration**: Download Matrix Application Service registration files directly from admin console
- **Comprehensive Logging**: Detailed logging for troubleshooting and monitoring
- **Slash Commands**: Full set of `/matrix` commands for room management and status checking

### 🔒 Security & Architecture
- **Application Service Integration**: Uses Matrix AS API for proper user impersonation and namespace management
- **Token-Based Authentication**: Secure token generation and management
- **Namespace Protection**: Exclusive control over ghost user and room aliases
- **Configurable Sync**: Enable/disable message synchronization as needed

## Quick Start

### Prerequisites
- Mattermost Server 10.7.1 or higher
- Matrix homeserver with Application Service support (e.g., Synapse)
- Admin access to both platforms

### Installation

1. **Install the Plugin**
   - Download the latest release from the [releases page](https://github.com/wiggin77/mattermost-plugin-matrix-bridge/releases)
   - Upload via Mattermost System Console → Plugins → Plugin Management

2. **Configure Matrix Settings**
   - Navigate to System Console → Plugins → Matrix Bridge
   - Enter your Matrix homeserver URL (e.g., `https://matrix.example.com`)
   - Generate Application Service and Homeserver tokens (use the "Generate" buttons)
   - Enable message synchronization

3. **Download and Install Registration File**
   - Click "Download Registration File" in the admin console
   - Copy the downloaded `mattermost-bridge-registration.yaml` to your Matrix homeserver
   - Add the file path to your `homeserver.yaml` under `app_service_config_files:`
   ```yaml
   app_service_config_files:
     - /path/to/mattermost-bridge-registration.yaml
   ```
   - Restart your Matrix homeserver

### Usage

#### Slash Commands

The plugin provides comprehensive `/matrix` commands for room management:

```bash
# Test Matrix connection
/matrix test

# Create a new Matrix room and map to current channel
/matrix create "Room Name"

# Map current channel to existing Matrix room
/matrix map #roomalias:matrix.example.com
/matrix map !roomid:matrix.example.com

# List channel mappings
/matrix list

# Check bridge status
/matrix status
```

#### Setting Up Shared Channels

1. Create or identify a Mattermost channel to bridge
2. Use `/matrix create "Room Name"` to create and map a Matrix room, or
3. Use `/matrix map #existing-room:matrix.example.com` to map to an existing room
4. Enable the channel for shared channels in Channel Settings
5. Messages posted in the channel will automatically sync to Matrix

## Architecture

### Components

- **Server Plugin** (`server/`): Go backend implementing Mattermost plugin hooks
  - `hooks.go`: Shared Channels integration and message sync logic
  - `matrix/client.go`: Matrix Client-Server and Application Service API client
  - `command/`: Slash command handlers for room management
  - `configuration.go`: Plugin configuration and validation

- **Web App** (`webapp/`): React/TypeScript admin interface
  - Admin console settings with token generation
  - Registration file download functionality
  - Built with Mattermost UI components

### Message Flow

1. **Message Posted**: User posts message in Mattermost shared channel
2. **Hook Triggered**: `OnSharedChannelsSyncMsg` hook receives message
3. **Ghost User**: Create or retrieve Matrix ghost user for Mattermost user
4. **Room Join**: Ensure ghost user is joined to target Matrix room
5. **Message Sync**: Send message to Matrix as ghost user
6. **Attribution**: Message appears in Matrix from user's display name

### Ghost User System

Ghost users follow the pattern `@_mattermost_{userID}:{homeserver}` and provide:
- Authentic message attribution (messages appear from actual users)
- Proper display names matching Mattermost users
- Namespace isolation (controlled by Application Service)
- Automatic room membership management

## Configuration

### Plugin Settings

| Setting | Description | Required |
|---------|-------------|----------|
| Matrix Server URL | URL of your Matrix homeserver | Yes |
| Application Service Token | Generated token for AS authentication | Yes |
| Homeserver Token | Generated token for homeserver communication | Yes |
| Enable Message Sync | Toggle for message synchronization | No |

### Matrix Homeserver Setup

Your Matrix homeserver must support Application Services. For Synapse:

1. Ensure AS support is enabled in `homeserver.yaml`
2. Add the registration file to `app_service_config_files`
3. Restart the homeserver
4. Verify the bridge appears in homeserver logs

## Development

### Building

```bash
# Full build with linting and tests
make all

# Build server only
make server

# Build webapp only  
make webapp

# Create distribution bundle
make dist
```

### Testing

```bash
# Run all tests
make test

# Run server tests only
cd server && go test ./...

# Run webapp tests only
cd webapp && npm test
```

### Plugin Management

```bash
# Deploy to local Mattermost
make deploy

# Watch for changes during development
make watch

# View plugin logs
make logs
```

## Troubleshooting

### Common Issues

**"Matrix client not configured" error**
- Verify Matrix Server URL is set correctly
- Ensure both AS and HS tokens are generated
- Check that message sync is enabled

**"User not in room" error** 
- Verify the registration file is properly installed on Matrix homeserver
- Check that the bridge user has permission to join rooms
- Try using room aliases instead of room IDs for mapping

**Messages not syncing**
- Ensure the channel is configured for shared channels
- Verify plugin health with `/matrix status`
- Check plugin logs for detailed error information

### Logs and Debugging

- Use `/matrix status` to check bridge health
- View detailed logs with `make logs`
- Enable debug logging in Mattermost for more verbose output
- Check Matrix homeserver logs for AS-related errors

## API Documentation

This plugin implements the Matrix specification:
- [Matrix Client-Server API v1.14](https://spec.matrix.org/v1.14/client-server-api/)
- [Matrix Application Service API v1.14](https://spec.matrix.org/v1.14/application-service-api/)

## Contributing

1. Fork the repository
2. Create a feature branch
3. Make your changes following the coding standards
4. Add tests for new functionality
5. Submit a pull request

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.

## Support

- [GitHub Issues](https://github.com/wiggin77/mattermost-plugin-matrix-bridge/issues)
- [Matrix Specification](https://spec.matrix.org/)
- [Mattermost Plugin Documentation](https://developers.mattermost.com/integrate/plugins/)

