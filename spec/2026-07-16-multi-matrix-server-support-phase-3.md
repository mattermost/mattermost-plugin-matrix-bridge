# Multi-Matrix-Server Support — Phase 3: Outbound routing + per-server registration

- **Jira:** [MM-64622](https://mattermost.atlassian.net/browse/MM-64622)
- **Branch:** `feat/multiple-server-support`
- **Status:** Blocked on Phase 1 (and coordinated with Phase 2)
- **Depends on:** Phase 1; loop-prevention change coordinated with Phase 2
- **Shippable independently:** yes (completes the core; phases 1–3 are the core)

## Objective

Route **outbound** Mattermost → Matrix traffic to the correct homeserver, selecting the
Matrix client by the channel→server mapping, and register the plugin per Matrix server so
loop prevention and sync cursors are per-remote.

## Tasks

1. **Client selection** (`server/sync_to_matrix.go`): resolve `(serverID, roomID)` from
   `channel_mapping_<channelID>` (the Phase 1 list value), pick `matrixClients[serverID]`,
   and send through it. Same for attachment sync and DM-room creation in `hooks.go`.
2. **Per-server ghost/user builders**: ghost user IDs, bridge aliases, and username
   generation must bind to the target server's domain/prefix
   (`GetMatrixUsernamePrefixForServer(serverID)`), not the single config.
3. **Fix `extractUsernameFromMatrixUserID`** (`server/sync_to_mattermost.go:695-708`): retain
   the server component instead of discarding it, so round-tripping `@user:server` is
   server-aware.
4. **Per-server shared-channels registration** (`server/plugin.go:183-219`, ticket #5):
   call `RegisterPluginForSharedChannels` **once per Matrix server** with a distinct
   `SiteURL` (e.g. `https://<server_name>`), storing each returned `remoteID` on the
   server's registry entry. Re-registration by the same SiteURL is idempotent.
5. **Set-based loop prevention** (`server/hooks.go:39,52,108,274`): replace
   `x.GetRemoteID() == p.remoteID` with membership in the set of the plugin's own
   `remoteID`s (one per server).

## Data / config

- `ServerConfig.RemoteID` becomes populated per server (Phase 1 seeded it with the single
  global remote; here it becomes genuinely per-server).
- Maintain a fast `map[remoteID]serverID` lookup for loop attribution.

## Files to change

`server/sync_to_matrix.go`, `server/sync_to_mattermost.go`, `server/hooks.go`,
`server/plugin.go`, `server/bridge_utils.go`, `server/matrix_util.go`, plus tests.

## `min_server_version`

Confirm the earliest Mattermost server version that ships the `SiteURL` field on
`RegisterPluginOpts` (per-remote registration) and bump `plugin.json` `min_server_version`
accordingly (currently `10.7.1`).

## Testing

- Two-server: a post in a channel mapped to server A sends only via A's client; a remote
  post from A is not re-synced (its `remoteID` is in the own-set), while a genuinely local
  post still syncs.
- `extractUsernameFromMatrixUserID` round-trips server-qualified IDs.
- Single-server behavior unchanged.

## Out of scope

REST API, admin UI, slash-command targeting.

## Acceptance criteria

- Outbound posts/reactions/files/profile-images target the mapped server only.
- Distinct `remoteID` per server; loop prevention correct across servers.
- Core (phases 1–3) supports N servers end-to-end when servers_config has N entries
  (even though only phases 4–6 make N configurable/usable by an admin).
