# Local Development with Matrix Synapse

For local development and testing, you can run a Matrix Synapse server using Docker Compose.

## Table of Contents

- [Prerequisites](#prerequisites)
- [Starting the Matrix Synapse Server](#starting-the-matrix-synapse-server)
- [Accessing the Web Chat Interface (Element)](#accessing-the-web-chat-interface-element)
- [Configuration Notes](#configuration-notes)
- [Multi-Server Testing (two homeservers)](#multi-server-testing-two-homeservers)
    - [Connecting a channel to a room on the second server](#connecting-a-channel-to-a-room-on-the-second-server)
- [Stopping the Services](#stopping-the-services)

## Prerequisites

1. Install the plugin and enable **Enable Message Sync** under **System Console >
   Plugins > Mattermost bridge for Matrix**. Homeservers are managed with the
   `/matrix server` slash commands.
2. Register the homeserver with the plugin, choosing your own Application Service
   and homeserver tokens (any strings; keep them secret):

    ```text
    /matrix server add http://localhost:8888 localhost <as_token> <hs_token>
    ```

3. Fetch the generated registration file and save it as
   `docker/mattermost-bridge-registration.yaml`:

    ```text
    /matrix server registration
    ```

## Starting the Matrix Synapse Server

1. Start the services:

    ```bash
    docker-compose up -d
    ```

2. Create an admin user:

    ```bash
    docker compose exec synapse register_new_matrix_user -c /data/homeserver.yaml -u admin -p admin123 -a http://localhost:8008
    ```

3. The Matrix server will be available at `http://localhost:8888`

## Accessing the Web Chat Interface (Element)

Synapse is only a homeserver and has no built-in chat UI. The Docker Compose stack
includes an [Element Web](https://element.io/) client for testing:

1. Start the Element service (included in `docker-compose up -d`, or start it alone):

    ```bash
    docker-compose up -d element
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
exercise multi-server bridging locally. It runs alongside the first:

|                   | Server 1                                     | Server 2                                      |
| ----------------- | -------------------------------------------- | --------------------------------------------- |
| Server name       | `localhost`                                  | `synapse2.localhost`                          |
| Matrix API (host) | `http://localhost:8888`                      | `http://synapse2.localhost:8889`              |
| Element client    | `http://localhost:8880`                      | `http://localhost:8881`                       |
| Registration file | `docker/mattermost-bridge-registration.yaml` | `docker/mattermost-bridge-registration2.yaml` |

`synapse2.localhost` resolves to `127.0.0.1` automatically (the `.localhost` TLD),
so no `/etc/hosts` changes are needed. The two servers must use **distinct
hostnames** — the plugin derives each server's ID from its URL hostname, and both
homeservers carry **distinct `hs_token`s** so inbound traffic routes to the right one.

Bring everything up and create an admin user on the second server:

```bash
docker-compose up -d
docker compose exec synapse2 register_new_matrix_user -c /data/homeserver.yaml -u admin -p admin123 -a http://localhost:8008
```

Register **Server 1** as usual (see [Prerequisites](#prerequisites)), then register
**Server 2** with the same `/matrix server add` command (the tokens come from
`docker/mattermost-bridge-registration2.yaml`):

```text
/matrix server add http://synapse2.localhost:8889 synapse2.localhost syn2_5Kd9WmXpQ2rLtVnB7cYhFgJ4sZaEuNi syn2_Tb3Rk8VpMxWq6NcYdL2fHgJ7sUaZ0eYi matrix2
```

Other server-management commands:

- `/matrix server list` — show all registered servers and their IDs
- `/matrix server remove <server_id>` — remove an injected server

### Connecting a channel to a room on the second server

The regular `/matrix map` and `/matrix create` commands always target the primary
server (the first one registered). To bridge a channel to a room on the **second**
server, use the server-scoped map command:

1. Create (or find) a room on server 2 — for example, sign in at
   `http://localhost:8881` (Element for server 2) and create a room.
2. Find the second server's ID with `/matrix server list`.
3. In the Mattermost channel you want to bridge, run:

    ```text
    /matrix server map <server_id> <room_alias_or_id>
    ```

    for example:

    ```text
    /matrix server map yjfnsajexkwmmmphkd4ceyte9a #room:synapse2.localhost
    ```

This resolves the room, joins the bridge bot on server 2, and stores the mapping
under that server's namespace (preserving any mapping the channel already has to the
primary server). Send a message in the Matrix room and it appears in the Mattermost
channel.

Notes:

- These commands require System Administrator privileges.

## Stopping the Services

```bash
docker-compose down
```

To completely reset (remove all data):

```bash
docker-compose down -v
```
