# Multi-Matrix-Server Support — Phase 5: Admin UI (custom server-management console)

- **Jira:** [MM-64622](https://mattermost.atlassian.net/browse/MM-64622)
- **Branch:** `feat/multiple-server-support`
- **Status:** Blocked on Phase 4 (REST API) and on Figma designs
- **Depends on:** Phase 4
- **Shippable independently:** yes (UI over existing API)

## Objective

Replace the flat, single-server `plugin.json` settings UI with a custom System Console
component that manages the list of Matrix servers (add / edit / enable / remove / download
registration / test) backed by the Phase 4 REST API.

## ⚠️ Design dependency — action required before building

The Jira remote link points to Figma file
`Matrix-support-for-connected-workspaces` (`OUH75QZFSY0PvDHUlbToIc`), but its **main branch
contains only a project Cover page — no admin-UI frames**. The actual designs are expected
to live on a **Figma branch** of that file.

**Blocker:** obtain the branch/frame-specific URL, i.e. either
`https://www.figma.com/design/<fileKey>/branch/<branchKey>/...` or a main-file URL with a
`?node-id=<frame>` for the admin screens. Reconcile the implementation with those frames
before writing UI code (ticket calls this out as a risk).

## Scope

- New React admin-console setting component rendering the server list and add/edit forms.
- Retire the flat single-server fields and the DOM-scraping `registration_download`
  component (superseded by the Phase 4 server-side endpoint).
- Global `enable_sync` master toggle remains; per-server `Enabled` toggle in the list.
- Global settings (`rate_limiting_mode`) stay as standard settings.

## Files to change

`webapp/src/components/admin_console_settings/*` (new server-manager component;
remove/replace `registration_download` and `homeserver_config`), `webapp/src/index.tsx`
(registration), `plugin.json` (settings_schema restructure), webapp tests.

## Testing

- Component tests for list/add/edit/delete/enable and registration download.
- Manual QA against a running server with 2 homeservers configured.
- Verify graceful behavior on API errors and token redaction.

## Out of scope

Slash-command targeting (Phase 6).

## Acceptance criteria

- Admin manages N servers entirely from the System Console.
- UI matches the approved Figma frames.
- Single-server upgrade presents the migrated server pre-populated in the list.
