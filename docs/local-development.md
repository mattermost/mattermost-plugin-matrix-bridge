# Local Development with Matrix Synapse

For local development and testing, you can run one or two Matrix Synapse servers using
Docker Compose.

## Table of Contents

- [Prerequisites](#prerequisites)
- [Starting the Matrix Synapse Server](#starting-the-matrix-synapse-server)
- [Accessing the Web Chat Interface (Element)](#accessing-the-web-chat-interface-element)
- [Configuration Notes](#configuration-notes)
- [Multi-Server Testing (two homeservers)](#multi-server-testing-two-homeservers)
    - [Connecting a channel to a room on the second server](#connecting-a-channel-to-a-room-on-the-second-server)
- [Stopping the Services](#stopping-the-services)

## Prerequisites

1. Install the plugin. Homeservers can be managed either with the `/matrix server`
   slash commands or from **System Console → Plugins → Mattermost bridge for Matrix →
   Matrix homeservers** (both System Admin only, both equivalent - the steps below use
   the slash commands, since that's the fastest path from a terminal, but "Add Matrix
   server" in the console does the same thing and can open the registration YAML for
   you immediately afterward).
2. Register the homeserver with the plugin, choosing your own Application Service and
   homeserver tokens (any strings; keep them secret):

    ```text
    /matrix server add http://localhost:8888 <as_token> <hs_token>
    ```

    The command discovers the server's `server_name` for you (it queries the
    homeserver's federation key endpoint, falling back to the URL's hostname) and
    reports it back along with the new `server_id`. For the dev Synapse above it will
    discover `localhost`.

3. Fetch the generated registration file and save it as
   `docker/mattermost-bridge-registration.yaml`:

    ```text
    /matrix server registration
    ```

    (or use the "Registration" action on the server's row in the console, which shows
    the same YAML with a copy/download button - copy it verbatim, including the `url:`
    line; do not append `/_matrix/app/v1` to it).

## Starting the Matrix Synapse Server

1. Start the services:

    ```bash
    docker compose up -d
    ```

2. Create an admin user:

    ```bash
    docker compose exec synapse register_new_matrix_user -c /data/homeserver.yaml -u admin -p admin123 -a http://localhost:8008
    ```

3. The Matrix server will be available at `http://localhost:8888`

## Accessing the Web Chat Interface (Element)

Synapse is only a homeserver and has no built-in chat UI. The Docker Compose stack
includes an [Element Web](https://element.io/) client for testing:

1. Start the Element service (included in `docker compose up -d`, or start it alone):

    ```bash
    docker compose up -d element
    ```

2. Open `http://localhost:8880` in your browser.

3. It is pre-configured to use the local homeserver (`http://localhost:8888`), so you
   can register or sign in with a test user directly. Registration is enabled for
   development, so you can create new users from the login screen.

Element's configuration lives in `docker/element-config.json`.

## Configuration Notes

- The Synapse server is configured to use PostgreSQL as the database
- Registration is enabled for development purposes
- App service configuration is loaded from `docker/mattermost-bridge-registration.yaml`
- Room list publication is restricted to the bridge user only
- An Element Web client is available at `http://localhost:8880` for manual testing

## Multi-Server Testing (two homeservers)

The Docker Compose stack includes a **second, independent homeserver** so you can
exercise multi-server bridging locally without touching production infrastructure. It
runs alongside the first:

|                   | Server 1                                     | Server 2                                      |
| ----------------- | --------------------------------------------- | ---------------------------------------------- |
| Server name       | `localhost`                                   | `synapse2.localhost`                           |
| Matrix API (host) | `http://localhost:8888`                       | `http://synapse2.localhost:8889`               |
| Element client    | `http://localhost:8880`                       | `http://localhost:8081`                        |
| Registration file | `docker/mattermost-bridge-registration.yaml`  | `docker/mattermost-bridge-registration2.yaml`  |

`synapse2.localhost` resolves to `127.0.0.1` automatically (the `.localhost` TLD is
reserved by RFC 6761 for loopback use), so no `/etc/hosts` changes are needed.

**Why the two server names must be distinct and port-less, explicitly.** The plugin
derives the domain that appears in a homeserver's Matrix IDs (`ServerName`) from what
the homeserver itself reports, and it strips any port before comparing/using it
(`NormalizeServerName` - this feeds ghost user IDs like `@<prefix>_<userid>:<domain>`,
so it can never carry a port). If you configured the second Synapse as, say,
`localhost:8009` instead of giving it its own hostname, its discovered server name
would normalize to the exact same `localhost` as the first server - `/matrix server
add` would then either reject the second server outright (server names must be unique)
or, worse, if the check were bypassed, both servers' ghost users would end up with
identical Matrix IDs and the plugin would have no way to tell which homeserver an
inbound event actually came from. That failure surfaces as messages appearing in the
wrong room or ghost users behaving strangely - far from the misconfigured
`server_name` that caused it. Two independent, port-less hostnames like `localhost`
and `synapse2.localhost` avoid this entirely, which is why the second Synapse in this
stack is deliberately given its own hostname rather than just a different port.

Bring everything up and create an admin user on the second server:

```bash
docker compose up -d
docker compose exec synapse2 register_new_matrix_user -c /data/homeserver.yaml -u admin -p admin123 -a http://localhost:8008
```

Register **Server 1** as usual (see [Prerequisites](#prerequisites)), then register
**Server 2** the same way, using the tokens already checked into
`docker/mattermost-bridge-registration2.yaml`:

```text
/matrix server add http://synapse2.localhost:8889 syn2_5Kd9WmXpQ2rLtVnB7cYhFgJ4sZaEuNi syn2_Tb3Rk8VpMxWq6NcYdL2fHgJ7sUaZ0eYi
```

This should report a discovered `server_name` of `synapse2.localhost`, distinct from
Server 1's `localhost`. If it doesn't - e.g. the two report the same name - stop and
fix the Synapse configuration before going further; the plugin cannot bridge two
homeservers that share a server name (see above).

Other server-management commands:

- `/matrix server list` - show all registered servers, their IDs and server names
- `/matrix server remove <server_id>` - remove a registered server (see the
  "Removing and restoring a server" section of the main README for the recovery path)

### Connecting a channel to a room on the second server

The regular `/matrix map` and `/matrix create` commands only work while exactly one
server is registered. Once a second server is registered, use the server-scoped
commands and pass the target `server_id` explicitly:

1. Create (or find) a room on server 2 - for example, sign in at
   `http://localhost:8081` (Element for server 2) and create a room.
2. Find the second server's ID with `/matrix server list`.
3. In the Mattermost channel you want to bridge, run:

    ```text
    /matrix server map <server_id> <room_alias_or_id>
    ```

    for example:

    ```text
    /matrix server map yjfnsajexkwmmmphkd4ceyte9a #room:synapse2.localhost
    ```

This resolves the room, joins the bridge bot on server 2, and stores the mapping under
that server's namespace - preserving any mapping the channel already has to a
different server. **A channel can only be bridged to one Matrix server at a time**;
mapping a channel that's already mapped to another server is rejected with an error
naming the currently-mapped server. Send a message in the Matrix room and it appears
in the Mattermost channel, and vice versa.

Notes:

- These commands require System Administrator privileges.
- `/matrix status` always reports on every registered server regardless of which one
  is "current," and never prints tokens.

## Stopping the Services

```bash
docker compose down
```

To completely reset (remove all data):

```bash
docker compose down -v
```
