# Multi-Matrix-Server Support — Phase 6: Slash-command server targeting

- **Jira:** [MM-64622](https://mattermost.atlassian.net/browse/MM-64622)
- **Branch:** `feat/multiple-server-support`
- **Status:** Blocked on Phase 1 (needs 2–3 for real routing)
- **Depends on:** Phases 1–3
- **Shippable independently:** yes

## Objective

Give the `/matrix` slash commands server awareness so operators can target a specific
Matrix homeserver, while keeping single-server behavior unchanged.

## Tasks

1. **`/matrix map`** (`server/command/command.go:332-492`): infer the target server from the
   room domain (`#alias:server` / `!id:server`) by matching against configured servers'
   domains; error clearly if ambiguous/unknown. Store the mapping with the resolved
   `serverID` (Phase 1 list value).
2. **`/matrix create`** (`command.go:578-668`): add a `--server <serverID|domain>` flag to
   choose the homeserver; default to the single server when only one is configured.
3. **`/matrix servers`**: new subcommand listing configured servers (id, domain, enabled,
   mapped-channel count). Add to dispatch (`command.go:762-853`) and autocomplete
   (`command.go:266-293`).
4. **`/matrix status` / `list`**: show per-server breakdown.
5. Update the `Configuration`/`PluginAccessor` command interfaces
   (`command.go:18-36`) to expose per-server lookups and the client registry.

## Files to change

`server/command/command.go`, command mocks, tests.

## Testing

- Single server: all existing commands behave identically (no `--server` needed).
- Multi server: `map` infers server from domain; `create --server` targets correctly;
  `servers` lists all; ambiguous input errors cleanly.

## Out of scope

Nothing further — this completes the epic's listed phases.

## Acceptance criteria

- Every `/matrix` subcommand works with N servers and is unchanged with one server.
- `/matrix servers` accurately reflects `servers_config`.
