# Mattermost Bridge for Matrix

[![Build Status](https://github.com/wiggin77/mattermost-plugin-matrix-bridge/actions/workflows/ci.yml/badge.svg)](https://github.com/wiggin77/mattermost-plugin-matrix-bridge/actions/workflows/ci.yml)

A seamless bridge that connects Mattermost and Matrix, enabling real-time bidirectional message synchronization between platforms.

## Features

- **Bidirectional Sync**: Messages, reactions, and edits sync automatically in both directions
- **Real Users**: Messages appear from actual users, not bots, with authentic names and avatars
- **Rich Content**: Full support for formatting, emoji reactions, threads, and file attachments
- **Easy Setup**: Simple configuration with automatic registration file generation
- **Secure**: Loop prevention, proper authentication, and namespace isolation

## Quick Start

### 1. Install Plugin
- Download from [releases](https://github.com/wiggin77/mattermost-plugin-matrix-bridge/releases)
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

- [GitHub Issues](https://github.com/wiggin77/mattermost-plugin-matrix-bridge/issues)
- [Matrix Specification](https://spec.matrix.org/)
- [Mattermost Plugin Documentation](https://developers.mattermost.com/integrate/plugins/)

## Roadmap

- Support multiple Matrix instances
- Expose more Share Channels APIs in plugin API
- Fix Matrix mention pill display
- Improve support for private, encrypted channels

## License

MIT License - see [LICENSE](LICENSE) file for details.