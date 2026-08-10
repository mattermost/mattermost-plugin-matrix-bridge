# Mattermost Bridge for Matrix

[![Build Status](https://github.com/mattermost/mattermost-plugin-matrix-bridge/actions/workflows/ci.yml/badge.svg)](https://github.com/mattermost/mattermost-plugin-matrix-bridge/actions/workflows/ci.yml)

A bidirectional bridge that connects Mattermost and Matrix, enabling real-time message synchronization between platforms.

## Requires

- Mattermost server v11.8.0 or newer, with Pro, Enterprise or higher license
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

### 2. Register a Matrix homeserver

Homeservers are managed entirely through `/matrix server` slash commands (System Admin
only) - there is no System Console configuration for them:

```text
/matrix server add https://matrix.example.com <as_token> <hs_token>
```

`<as_token>` and `<hs_token>` are shared secrets you choose yourself; they just need to
match what you put in the Application Service registration file in the next step. The
command reports the server's `server_id` and its discovered `server_name` (the domain
that will appear in this server's Matrix IDs - see `--server-name` below if it guesses
wrong).

### 3. Setup Matrix Homeserver

- Get the registration YAML for your server: `/matrix server registration [server_id]`
  (omit `server_id` if you only have one server registered)
- Add it to your Matrix homeserver's `app_service_config_files`
- Restart your homeserver

### 4. Connect Channels

Use slash commands to bridge channels:

```text
/matrix test                            # Test Matrix connection and configuration
/matrix create "Room Name"              # Create new Matrix room
/matrix map #room:matrix.example.com    # Map to existing room
/matrix status                          # Check bridge health for every server
```

`/matrix test`, `/matrix create`, `/matrix map` and `/matrix unmap` operate on the sole
registered server when there is exactly one. Once a second server is registered, use the
equivalent `/matrix server ...` commands and pass a `server_id` explicitly - see
[Multiple homeservers](#multiple-homeservers) below.

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

- Mattermost Server 11.8.0+
- Matrix homeserver with Application Service support (Synapse, Dendrite, etc.)
- Admin access to both platforms

## Configuration

Per-homeserver connection settings (URL, tokens, discovered server name, username
prefix, enabled state) live in a KV-backed registry managed entirely through
`/matrix server` slash commands - there is nothing to configure in System Console for
them. The one remaining System Console setting is:

| Setting             | Description                                                       |
| -------------------- | ------------------------------------------------------------------ |
| Matrix Rate Limiting | How aggressively the bridge throttles requests to Matrix servers   |

## Multiple homeservers

The bridge can be connected to more than one Matrix homeserver at once. **Each
Mattermost channel may be bridged to exactly one Matrix server at a time** - mapping a
channel that's already bridged to another server is rejected; unmap it first if you need
to move it.

Common commands (all require System Admin, except `/matrix status` which is open to
everyone and never prints secrets):

| Command                                                            | Behavior                                                                  |
| -------------------------------------------------------------------- | -------------------------------------------------------------------------- |
| `/matrix server list`                                               | List every registered server, its state, and how many channels map to it |
| `/matrix server add <url> <as_token> <hs_token> [username_prefix]`  | Register a new homeserver                                                |
| `/matrix server remove <server_id>`                                 | Unregister a server (see "Removing and restoring a server" below)        |
| `/matrix server map [server_id] <room_alias\|room_id>`              | Bridge the current channel to that server's room                         |
| `/matrix server unmap [server_id]`                                  | Remove the current channel's mapping to that server                      |
| `/matrix server enable\|disable <server_id>`                        | Stop or resume syncing for one server without touching the others        |
| `/matrix server status [server_id]`                                 | Health and mapping count for one server                                  |
| `/matrix server registration [server_id]`                           | Print the Application Service registration YAML for that server         |

`server_id` can be omitted from any of these when exactly one server is registered, and
elsewhere can be given as the ID itself, the server's discovered name, or its URL host.

**Server names are discovered, not typed in.** `/matrix server add` resolves the domain
that will appear in the new server's Matrix IDs automatically (via the homeserver's
federation key endpoint, falling back to the URL's hostname) - most admins never need to
think about it. If a homeserver's public URL and its Matrix ID domain genuinely differ
(e.g. behind a reverse proxy), override it with `--server-name <domain>`.

**Removing and restoring a server.** `/matrix server remove` unregisters the server but
deliberately keeps every one of its channel mappings and ghost users in the KV store -
only the registry entry goes away. The command prints the server's `server_id` as a
recovery key. To restore the server later - same room mappings, same ghost users, no
re-mapping needed - re-add it with that ID:

```text
/matrix server add https://matrix.example.com <as_token> <hs_token> --server-id <server_id>
```

This only works if the server is re-added at the **same URL** it originally had -
re-adding it at a different endpoint leaves pre-existing posts' edit/delete history
unreachable (the command warns if it detects this). The one server that came from a
pre-multi-server upgrade (see below) can't be removed at all - `server disable` is the
supported way to take it out of service.

**Upgrading from a single-homeserver install.** Existing installs that configured a
Matrix server via the old System Console fields are migrated automatically on first
activation after upgrading: a `/matrix server` entry is seeded from the old
`matrix_server_url`/tokens, keeping its original shared-channels remote and sync history
so nothing needs to be re-mapped.

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

For local development and testing, `docker-compose.yml` runs a Matrix Synapse server
(plus an Element web client) alongside the plugin. It can also bring up a **second**,
independent homeserver for exercising multi-server bridging locally.

See [docs/local-development.md](docs/local-development.md) for the full walkthrough,
including the one- and two-homeserver setups, the Element web client, and why the
second homeserver must use a distinct, port-less server name.

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

- Expose more Share Channels APIs in plugin API
- Improve support for private, encrypted channels

## License

MIT License - see [LICENSE](LICENSE) file for details.
