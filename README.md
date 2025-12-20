# Mattermost Bridge for Matrix

[![Build Status](https://github.com/mattermost/mattermost-plugin-matrix-bridge/actions/workflows/ci.yml/badge.svg)](https://github.com/mattermost/mattermost-plugin-matrix-bridge/actions/workflows/ci.yml)

A bidirectional bridge that connects Mattermost and Matrix, enabling real-time message synchronization between platforms.

## Requires

- Mattermost server v10.7.1 or newer, with Pro, Enterprise or higher license
- A Matrix server, such as [Synapse](https://github.com/element-hq/synapse) v1.119

## Features

- **Bidirectional Sync**: Messages, reactions, and edits sync automatically in both directions
- **Real Users**: Messages appear from actual users, not bots, with authentic names and avatars
- **Rich Content**: Full support for formatting, emoji reactions, threads, and file attachments
- **Easy Setup**: Simple configuration with automatic registration file generation
- **Secure**: Loop prevention, proper authentication, and namespace isolation

## Quick Start

### 1. Install Plugin
- Download from [releases](https://github.com/mattermost/mattermost-plugin-matrix-bridge/releases)
- Upload via System Console → Plugins → Plugin Management

### 2. Configure Settings
- Go to System Console → Plugins → Matrix Bridge
- Set your Matrix homeserver URL (e.g., `https://matrix.example.com`)
- Generate Application Service and Homeserver tokens
- Enable message synchronization

### 3. Setup Matrix Homeserver
- Download the registration file from the admin console
- Add it to your Matrix homeserver's `app_service_config_files`
- Restart your homeserver

### 4. Connect Channels
Use slash commands to bridge channels:

```
/matrix test                            # Test Matrix connection and configuration
/matrix create "Room Name"              # Create new Matrix room
/matrix map #room:matrix.example.com    # Map to existing room
/matrix status                          # Check bridge health
```

## How It Works

1. **Create Mapping**: Link a Mattermost channel to a Matrix room
2. **Enable Sharing**: Configure the channel for shared channels in Channel Settings
3. **Start Chatting**: Messages automatically sync between platforms with full user attribution

**What Gets Synced:**
- Messages (with formatting and mentions)
- Emoji reactions (4,400+ emoji support)
- Message edits and deletions
- User profiles with display names and avatars
- Reply threads

## Requirements

- Mattermost Server 10.7.1+
- Matrix homeserver with Application Service support (Synapse, Dendrite, etc.)
- Admin access to both platforms

## Configuration

| Setting | Description |
|---------|-------------|
| Matrix Server URL | Your Matrix homeserver URL |
| Application Service Token | Generated token for Matrix authentication |
| Homeserver Token | Generated token for webhook security |
| Enable Message Sync | Toggle bidirectional synchronization |

## Development

```bash
# Build everything
make all

# Run tests
make test

# Deploy to local Mattermost
make deploy
```

### Emoji Generation

To generate the emoji mappings in the `server/emoji_mappings_generated.go` file:

```bash
make generate-emoji
```

## Local Development with Matrix Synapse

For local development and testing, you can run a Matrix Synapse server using Docker Compose.

### Prerequisites

1. Install and configure the Mattermost Matrix Bridge plugin first
2. Generate the bridge registration file through the plugin configuration
3. Copy the generated registration file to `docker/mattermost-bridge-registration.yaml`

### Starting the Matrix Synapse Server

1. Start the services:
   ```bash
   docker-compose up -d
   ```

2. Create an admin user:
   ```bash
   docker exec -it mattermost-plugin-matrix-bridge-synapse-1 register_new_matrix_user -c /data/homeserver.yaml -u admin -p admin123 -a http://localhost:8008
   ```

3. The Matrix server will be available at `http://localhost:8888`

### Configuration Notes

- The Synapse server is configured to use PostgreSQL as the database
- Registration is enabled for development purposes
- App service configuration is loaded from `docker/mattermost-bridge-registration.yaml`
- Room list publication is restricted to the bridge user only

### Stopping the Services

```bash
docker-compose down
```

To completely reset (remove all data):
```bash
docker-compose down -v
```

## Troubleshooting

**Connection Issues:**
- Verify Matrix server URL and tokens are correct
- Check that registration file is installed on Matrix homeserver
- Use `/matrix status` to diagnose problems

**Sync Problems:**
- Ensure channel is configured for shared channels
- Check that both platforms can reach each other
- Review plugin logs for detailed error information

## Support

- [GitHub Issues](https://github.com/mattermost/mattermost-plugin-matrix-bridge/issues)
- [Matrix Specification](https://spec.matrix.org/)
- [Mattermost Plugin Documentation](https://developers.mattermost.com/integrate/plugins/)

## Roadmap

- Support multiple Matrix instances
- Expose more Share Channels APIs in plugin API
- Improve support for private, encrypted channels

## License

MIT License - see [LICENSE](LICENSE) file for details.
