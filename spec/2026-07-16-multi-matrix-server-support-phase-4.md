# Multi-Matrix-Server Support — Phase 4: REST API + server-side registration generation

- **Jira:** [MM-64622](https://mattermost.atlassian.net/browse/MM-64622)
- **Branch:** `feat/multiple-server-support`
- **Status:** Blocked on Phase 1 (functionally needs 1–3 for the servers to do anything)
- **Depends on:** Phase 1 (registry); best after Phases 2–3
- **Shippable independently:** yes (API only; UI in Phase 5)

## Objective

Expose the `servers_config` registry through a plugin REST API so servers can be listed,
added, edited, and removed, and generate each server's Application Service registration
file server-side (replacing the browser DOM-scraping component).

## REST API (under `/api/v1`, `MattermostAuthorizationRequired` + sysadmin check)

- `GET    /servers` — list servers (tokens redacted/masked).
- `POST   /servers` — add a server; **derive `serverID` deterministically from the URL
  hostname** (`deriveServerID`, Phase 1); persist; (re)register shared-channels remote;
  rebuild client registry. Because the ID is derived, a re-added server re-adopts its
  orphaned KV records, and a duplicate URL resolves to an existing `serverID` — treat that as
  an idempotent add/update, not a second entry.
- `PUT    /servers/{serverID}` — edit URL/name/tokens/prefix/enabled.
- `DELETE /servers/{serverID}` — remove a server (see deletion semantics).
- `GET    /servers/{serverID}/registration` — download that server's AS registration YAML,
  built server-side from its URL/domain/tokens (see below).
- `POST   /servers/{serverID}/test` — connectivity/health check via that server's client.

All writes go through a single serialized path that updates `servers_config` and refreshes
the in-memory client registry atomically.

## Server-side registration generation (ticket #7)

Move YAML generation out of `webapp/.../registration_download` into the server. Build from
the server entry: `id`, `url` (SiteURL + plugin path), `as_token`, `hs_token`,
`sender_localpart`, and namespaces derived from the server's domain
(`@_mattermost_.*:<domain>`, alias/room regexes). Keep the emitted YAML byte-compatible
with today's file for the existing single server.

## Deletion semantics (resolved)

Mirror Mattermost Connected Workspaces: removing a server **stops syncing** and tears down
its shared-channels remote; it does **not** delete channel content and does not hard-block.
Provide a guard/warning when channels are still mapped, and clean up (or orphan-mark) that
server's namespaced KV keys. Finalize exact cleanup batch here.

## Files to change

`server/api.go` (routes + handlers), new `server/servers_api.go` (or similar),
`server/configuration.go` (registry mutation helpers), `plugin.json` (drop/mark the
registration-download custom setting once server-side generation lands), tests.

## Testing

- CRUD happy paths + validation (dupe URL, malformed URL, missing tokens).
- Registration YAML for the migrated single server is byte-identical to the legacy output.
- AuthZ: non-admins are rejected; tokens are never returned in plaintext by `GET`.

## Out of scope

Admin UI (Phase 5), slash-command targeting (Phase 6).

## Acceptance criteria

- Admin can fully manage servers over REST without editing `plugin.json`.
- Per-server registration file downloads correctly and matches homeserver expectations.
