# Multi-Matrix-Server Support — Phase 1: Config registry + client registry + KV v3 migration

- **Jira:** [MM-64622](https://mattermost.atlassian.net/browse/MM-64622) (Epic: MM-64621)
- **Branch:** `feat/multiple-server-support`
- **Status:** Ready to implement
- **Depends on:** nothing
- **Shippable independently:** yes (no user-visible change)

## Objective

Restructure the plugin internals so that a single Matrix homeserver is represented
as a **one-element registry keyed by a stable, opaque `serverID`**, and namespace all
per-server KV keys by that `serverID`. This is the foundation every later phase builds on.

**Hard requirement: zero behavior change.** The flat `plugin.json` fields remain the
source of truth for the single configured server. No REST API, no UI, no routing,
no slash-command changes in this phase.

## Key architectural decisions (from ticket)

- `serverID` is the stable join key: **derived deterministically from the hostname of the
  Matrix server URL**, not a random `model.NewId()`. It stays constant as long as the
  hostname is unchanged. Determinism is deliberate: a server re-created with the same URL
  **re-adopts its namespaced KV records** instead of orphaning them (recovery from an
  accidentally deleted registry entry). Recovery holds while an independent source of the URL
  survives (Phase 1: `plugin.json`); once the registry is the sole store (Phase 4+), the
  payoff is that *re-adding by the same URL* re-adopts records. A legitimate hostname change
  re-derives a new ID and orphans the old records (logged as a warning; full re-keying is out
  of scope for Phase 1).
- Config becomes a managed registry stored in KV (`servers_config`, JSON array), but in
  Phase 1 it is still _derived from_ the flat `plugin.json` config on every change.
- `matrix.Client` is already fully server-scoped (`server/matrix/client.go:182-199`), so the
  single `p.matrixClient` becomes `map[serverID]*matrix.Client`.
- `channel_mapping_<channelID>` keeps its key but its value becomes a **list** of
  `(serverID, roomID)` entries (length 1 enforced now) to keep future fan-out cheap.
- Global master `enable_sync` stays; each server entry also carries an `Enabled` flag
  (populated but not yet independently toggle-able via UI until Phase 5).

## `serverID`

- Derived from the `ServerConfig.ServerURL` field (hostname only), never from
  `configuration.go` directly — so the same derivation works when Phase 4 feeds the URL from
  the REST POST body. Reuses `matrix.ExtractServerDomain` to normalize (strip scheme, port,
  path; case-folded).
- Format: `base32(sha256(hostname)[:16])[:26]` using Mattermost's ID alphabet — a 26-char
  drop-in for `model.NewId()`, so no KV key-length regression.
- Single derivation site: `deriveServerID` in `server/servers.go`, called from
  `reconcileServerConfig` (the v3 migration reaches it through the same function).

## Data model

```go
// server-side registry entry (persisted as JSON in KV under "servers_config")
type ServerConfig struct {
    ServerID       string `json:"server_id"`       // stable, derived from hostname (see deriveServerID)
    ServerURL      string `json:"server_url"`
    ServerName     string `json:"server_name"`     // Matrix ID domain (may be empty -> discovery)
    ASToken        string `json:"as_token"`
    HSToken        string `json:"hs_token"`
    UsernamePrefix string `json:"username_prefix"`
    Enabled        bool   `json:"enabled"`
    RemoteID       string `json:"remote_id"`        // shared-channels remote; Phase 1 = the single global remoteID
}

// value shape for channel_mapping_<channelID> (length 1 in Phase 1)
type ChannelServerMapping struct {
    ServerID string `json:"server_id"`
    RoomID   string `json:"room_id"`
}
```

New KV key: `servers_config` (single JSON array). Global settings remaining flat in
`plugin.json`: `enable_sync`, `rate_limiting_mode`.

## KV key namespacing

Add a `serverID` dimension to these builders in `server/store/kvstore/constants.go`
(format: `<prefix><serverID>_<id>`):

- `matrix_user_` → `BuildMatrixUserKey(serverID, matrixUserID)`
- `mattermost_user_` → `BuildMattermostUserKey(serverID, mmUserID)`
- `ghost_user_` → `BuildGhostUserKey(serverID, mmUserID)`
- `ghost_room_` → `BuildGhostRoomKey(serverID, mmUserID, roomID)`
- `matrix_event_post_` → `BuildMatrixEventPostKey(serverID, eventID)`
- `matrix_reaction_` → `BuildMatrixReactionKey(serverID, reactionEventID)`
- `room_mapping_` → `BuildRoomMappingKey(serverID, roomIdentifier)`
- `channel_mapping_<channelID>`: **key unchanged**, value becomes `[]ChannelServerMapping`.

Fix the two hardcoded string literals that bypass the constants:

- `server/matrix_util.go:14` (`"ghost_user_" + ...`)
- `server/bridge_utils.go:287` (`"matrix_user_" + ...`)

## Files to change

- `server/store/kvstore/constants.go` — bump `CurrentKVStoreVersion` 2 → 3; new key builders; new `servers_config` key.
- `server/configuration.go` — reconcile flat config → single registry entry (stable `serverID`); make `GetMatrixUsernamePrefixForServer(serverID)` resolve per-entry.
- `server/plugin.go` — replace `matrixClient` with `matrixClients map[string]*matrix.Client`; add `getMatrixClient(serverID)`, `getSingleServerID()`; update `initMatrixClient`, `initBridges`, `GetMatrixClient`.
- `server/bridge_utils.go` — hold the registry + resolver; route existing calls to the single server.
- `server/migrations.go` — add `runMigrationToVersion3` (mint serverID, rekey all namespaced keys in batches, convert channel_mapping values).
- `server/sync_to_matrix.go`, `server/sync_to_mattermost.go`, `server/matrix_webhook.go`, `server/command/command.go` — update KV read/write sites to pass `serverID`.
- Tests + mocks (`server/mocks/*`, `server/command/mocks/*`).

## Tasks

1. Add `ServerConfig`, `ChannelServerMapping`, `servers_config` key, and new key builders.
2. Introduce the client registry on `Plugin` and the resolver on `BridgeUtils`; keep a
   single-server convenience accessor.
3. Reconcile flat config into the one-element registry on `OnConfigurationChange`, keeping
   `serverID` stable across restarts and config edits.
4. Thread `serverID` through every namespaced KV read/write.
5. Implement v3 migration (deterministic, idempotent).
6. Update/extend tests; run full check.

## Testing

- Migration v3: fresh install (no prior keys), single-server upgrade from v2, and
  re-run/idempotency (running v3 twice is a no-op).
- Existing unit + integration suites still green with new key formats.
- `make check-style` and `make test` (Go + webapp) pass. Webapp untouched.

## Out of scope (later phases)

Inbound/outbound routing, per-server shared-channels registration, set-based loop
prevention, REST API, admin UI, slash-command targeting, `min_server_version` bump,
server-deletion semantics.

## Acceptance criteria

- A v2 install upgrades cleanly to v3; all mappings attributed to one minted `serverID`.
- No functional/behavioral change for an operator with one server configured.
- `servers_config` holds exactly one entry after migration.
- Flat `plugin.json` fields remain intact (rollback window preserved for one release).
