# Multi-Matrix-Server Support — Phase 2: Inbound routing (hs_token → serverID)

- **Jira:** [MM-64622](https://mattermost.atlassian.net/browse/MM-64622)
- **Branch:** `feat/multiple-server-support`
- **Status:** Blocked on Phase 1
- **Depends on:** Phase 1 (config registry, client registry, namespaced KV)
- **Shippable independently:** yes (core routing; still safe with one server)

## Objective

Route **inbound** Matrix Application Service traffic to the correct server. Identify the
originating homeserver by its `hs_token`, resolve the `serverID`, and perform all lookups
in that server's KV namespace.

## Key architectural decision (from ticket #4)

An AS appends the fixed suffix `/_matrix/app/v1/transactions/{txnId}` to the registration
`url`, so a discriminator cannot sit mid-path. **Give each server a unique `hs_token`** and
match the presented bearer token against every server's token to resolve `serverID` — zero
URL restructuring. (Fallback if needed: put `serverID` as the first path segment via the
registration `url` base.)

## Tasks

1. **Auth middleware** (`server/api.go:44-73`): replace the single-token compare with a
   constant-time match of the presented `Bearer` token against each server's `HSToken`.
   On match, resolve `serverID` and inject it into the request context; 401 on no match.
   Keep the `enable_sync` master check plus per-server `Enabled` check.
2. **Transaction dedup** (`server/matrix_webhook.go:39-42`): change the process-global
   `processedTransactions` map keyed by `txnID` to be keyed by `(serverID, txnID)` — Matrix
   `txnID`s are only unique per homeserver.
3. **Event routing** (`server/matrix_webhook.go:166-243`): pass `serverID` into
   `processMatrixEvent`; look up `room_mapping_<serverID>_<roomID>`; fallback room-state
   lookup uses that server's client from the registry.
4. **Ghost-user detection** (`isGhostUser`, `matrix_webhook.go:246-266`): check the suffix
   against the resolved server's domain, not the single config.
5. **Inbound persistence** (`server/sync_to_mattermost.go`): all `matrix_user_`,
   `mattermost_user_`, `matrix_event_post_`, `matrix_reaction_` reads/writes use `serverID`;
   store post property `matrix_event_id_<serverDomain>` for the resolved server.

## Files to change

`server/api.go`, `server/matrix_webhook.go`, `server/sync_to_mattermost.go`,
`server/bridge_utils.go` (server-aware helpers), plus tests.

## Testing

- Two-server unit tests: transactions with distinct `hs_token`s route to distinct
  namespaces; colliding `txnID`s across servers are NOT deduped against each other.
- Single-server behavior unchanged.
- Unknown//invalid token → 401.

## Out of scope

Outbound client selection (Phase 3), per-server registration (see below), UI, REST API.

## Open questions / risks

- Per-server shared-channels registration (unique `remoteID` per server) is a prerequisite
  for correct inbound loop attribution; coordinate the loop-prevention change with Phase 3.
- Confirm the auth middleware’s token scan is constant-time and bounded (few servers).

## Acceptance criteria

- Inbound events from server A never read/write server B's namespace.
- Duplicate detection is per `(serverID, txnID)`.
- One-server installs behave exactly as before.
