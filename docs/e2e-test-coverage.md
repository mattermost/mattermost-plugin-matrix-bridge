# E2E test coverage report

How much of the bridge's feature set is covered by container-backed tests, for a single
homeserver and for multiple homeservers. The `server/` suites were surveyed at `ff9bec5`;
rows covered by the `e2e/` package are updated as its tests land.

## What "e2e" means in this repo

There are two container-backed harnesses. Both skip under `-short`, so `make test` runs
neither; `make e2e` (and the CI `e2e` job) builds the plugin bundle and runs both. The webapp
has no Playwright/Cypress tests (`webapp/tests/` only holds Jest setup).

- **`server/` suites: real Synapse, mocked Mattermost.** Go integration tests run against a
  real Synapse started with testcontainers (`testcontainers/matrix`), while Mattermost is a
  mocked `plugintest.API` plus an in-memory KV store.
- **`e2e/` package: real Mattermost and real Synapse.** `e2e/harness` starts Postgres, an
  unlicensed Mattermost Enterprise Edition with the plugin bundle installed, and Synapse on one
  Docker network, once per package run. The homeserver is registered through the plugin's REST
  API, channels are mapped with `/matrix map`, posts reach the plugin through the real
  shared-channels hooks, and Synapse delivers Application Service transactions to the plugin
  itself.

Three structural limits apply to the `server/` suites:

- **Outbound tests call bridge methods directly** (`SyncPostToMatrix`),
  never the shared-channels hooks (`OnSharedChannelsSyncMsg`,
  `OnSharedChannelsAttachmentSyncMsg`, `OnSharedChannelsProfileImageSyncMsg`). The per-server
  routing in `serverIDForSyncMsg` and the loop-skip logic in the hooks are only unit-tested.
- **Inbound events are hand-built.** Synapse never pushes a transaction to the plugin; the one
  inbound test builds a `MatrixTransaction` JSON and calls `ServeHTTP` itself.
- **Servers are mostly seeded, not added.** Except for `TestReAdoptionRoundTrip`, tests
  register servers with `registerTestServer`, which writes the registry directly and bypasses
  `servers.Add` (validation, discovery, shared-channels registration).

### Container-backed suites

| File | Suite | Servers |
| --- | --- | --- |
| `server/matrix_mentions_integration_test.go` | `TestMatrixMentionProcessing`, `TestMatrixMentionEdgeCases` | 1 |
| `server/plugin_integration_test.go` | `PluginIntegrationTestSuite` | 1 |
| `server/dm_room_creation_test.go` | `DMRoomCreationTestSuite` | 1 |
| `server/user_remote_detection_test.go` | `UserRemoteDetectionIntegrationTestSuite` | 1 |
| `server/thread_mapping_test.go` | `ThreadMappingIntegrationTestSuite` | 1 |
| `server/matrix/test/client_test.go` | `MatrixClientTestSuite` (Matrix client only, no plugin) | 1 |
| `server/multi_server_integration_test.go` | `MultiServerIntegrationTestSuite` (4 tests) | 2 |
| `e2e/smoke_test.go` | `TestSmokeMattermostToMatrix`, `TestSmokeMatrixToMattermost` (real Mattermost) | 1 |
| `e2e/mattermost_to_matrix_test.go` | `TestMattermostToMatrix*`: `TextMessage`, `Markdown`, `Mentions`, `ThreadReply`, `PostEdit`, `PostDeletion`, `ReactionAdd`, `ReactionRemoval`, `FileAttachments`, `FileAttachmentDeletion`, `GhostCreation`, `DisplayNameSync`, `AvatarSync`, `UserJoinsChannel`, `MatrixUserReinvited`, `DirectMessage` (real Mattermost) | 1 |

## Legend

- ✅ Covered: the plugin's code path runs against real Synapse and the outcome is asserted.
- ⚠️ Partial: exercised, but the outcome isn't asserted, the test re-implements the
  production logic, or a key step is bypassed (see notes).
- ❌ Not covered e2e. "Client only" means the underlying `matrix.Client` call is tested in
  `client_test.go`, but the plugin logic around it is not.
- — Not applicable.

## Coverage table

### Mattermost → Matrix

| Feature | E2E single server | E2E multi server | Notes |
| --- | --- | --- | --- |
| Text message sync | ✅ | ✅ | Single: `TestMattermostToMatrixTextMessage` and `TestSmokeMattermostToMatrix`, through the real shared-channels hook. Multi: `TestOutboundSyncIsolatedPerServer` checks the post reaches only the mapped server |
| Markdown → HTML formatting | ✅ | ❌ | `TestMattermostToMatrixMarkdown`: bold, italic, link, fenced code block; list items stay `- ` lines joined by `<br>` |
| @mentions → Matrix pills / `m.mentions` | ✅ | ❌ | `TestMattermostToMatrixMentions` (a Mattermost member's ghost and a Matrix-originated user's own ID), plus the mocked-Mattermost `TestMatrixMentionProcessing` and `TestMatrixMentionEdgeCases` in `server/`. Multi-server should check that the mention uses the target server's ghost domain |
| Thread replies | ✅ | ❌ | `TestMattermostToMatrixThreadReply` |
| Post edit | ✅ | ❌ | `TestMattermostToMatrixPostEdit` |
| Post deletion → redaction | ✅ | ❌ | `TestMattermostToMatrixPostDeletion` |
| Reaction add | ✅ | ❌ | `TestMattermostToMatrixReactionAdd` |
| Reaction removal | ✅ | ❌ | `TestMattermostToMatrixReactionRemoval` |
| File attachments | ✅ | ❌ | `TestMattermostToMatrixFileAttachments`: a PNG and a text file through `OnSharedChannelsAttachmentSyncMsg`, downloaded byte for byte with filename and mimetype. Multi-server (the pending-file flow is keyed per server) is untested |
| File attachment deletion | ⚠️ | ❌ | `TestMattermostToMatrixFileAttachmentDeletion`: post deletion redacts file events; `deleteFileFromMatrix` is never reached for an already-synced attachment, because Mattermost doesn't re-sync a deleted one (the removal subtest skips) |
| Ghost user creation | ✅ | ⚠️ | Single: `TestMattermostToMatrixGhostCreation`; the join hook creates the ghost with the full name and joins it before the first post (also `TestGhostUserCreationAndDetection`). Multi: a ghost is created on server A only; nothing checks that one Mattermost user gets separate ghosts on A and B |
| Display name sync (`SyncUserToMatrix`) | ✅ | ❌ | `TestMattermostToMatrixDisplayNameSync`: first and last name; the nickname isn't used (`ShowFullName`). Mattermost sends profile changes only for users it has already synced to the remote, so the test posts first |
| Avatar sync (profile image hook) | ✅ | ❌ | `TestMattermostToMatrixAvatarSync`: the ghost's avatar bytes equal Mattermost's re-encoded profile image. As with display names, the user posts first |
| DM / group DM room auto-creation | ⚠️ | ❌ | e2e skipped: needs `EnableSharedChannelsDMs` (`TestMattermostToMatrixDirectMessage` asserts both 403s, then skips); room naming still only in `DMRoomCreationTestSuite`, which doesn't check message delivery. Multi-server DM routing (DM created on the calling server) is unit-only |
| User joins channel → ghost joins room | ✅ | ❌ | `TestMattermostToMatrixUserJoinsChannel`, through the real `UserHasJoinedChannel` hook |
| Matrix-originated user re-invited to room | ✅ | ❌ | `TestMattermostToMatrixMatrixUserReinvited`, which supersedes `testInviteRemoteUserToMatrixRoom`. The plugin invites as the AS bot, which `/matrix map` leaves at power level 0; in a `public_chat` room (invite level 50) the invite fails with 403 `M_FORBIDDEN`, so the test grants the bot the invite level. The rule that users are never invited to a server they didn't come from is unit-only |
| Shared-channels hook routing (server resolved from `RemoteCluster`, own-remote skip) | ✅ | ❌ | Single: the message, file and profile rows above go through the `OnSharedChannelsSyncMsg`, attachment and profile-image hooks with a real `RemoteCluster`; the ghost creation, user join and re-invite rows go through `UserHasJoinedChannel`. The own-remote skip is unit-only (`TestServerIDForSyncMsg`); see Loop prevention under Cross-cutting |

### Matrix → Mattermost

| Feature | E2E single server | E2E multi server | Notes |
| --- | --- | --- | --- |
| Text message → post | ✅ | ✅ | Single: `TestSmokeMatrixToMattermost`, delivered by Synapse to a real Mattermost. Multi: `TestInboundRoutingIsolatedPerServer`, with a hand-built transaction through `ServeHTTP` |
| HTML → Markdown conversion | ❌ | ❌ | Unit-only (`TestExtractMatrixMessageContent`) |
| Matrix mentions → @username | ❌ | ❌ | Unit-only |
| Replies / threads (`m.in_reply_to`, `m.thread`) | ⚠️ | ❌ | `ThreadMappingIntegrationTestSuite` only calls `getThreadRootFromPostID` against mocked posts; no event is processed |
| Edit (`m.replace`) | ❌ | ❌ | No test at any level |
| Reaction add | ❌ | ❌ | No test at any level |
| Reaction removal (redaction) | ❌ | ❌ | No test at any level |
| Message deletion (redaction) | ❌ | ❌ | No test at any level |
| Files / images / video / audio | ❌ | ❌ | No test at any level |
| Matrix user → Mattermost user provisioning | ⚠️ | ⚠️ | Single: `TestSmokeMatrixToMattermost` asserts the author is a remote user of the server's remote; the username prefix is only checked through `generateMattermostUsername`. Multi: `CreateUser` is mocked, and the per-server prefix or remote ID isn't asserted |
| Member join / leave / ban → channel membership | ❌ | ❌ | No test at any level |
| Profile change (displayname / avatar) → Mattermost user | ❌ | ❌ | No test at any level |
| Matrix-initiated DM → Mattermost DM | ❌ | ❌ | Unit-only (`TestHandleMatrixMemberDM_*`) |
| Webhook auth (per-server `hs_token`) | ⚠️ | ✅ | Single: `TestSmokeMatrixToMattermost` covers only the accepted token. Multi: `TestInboundRoutingIsolatedPerServer` |
| Transaction dedupe / retry | ❌ | ❌ | Unit-only (`TestHandleMatrixTransaction`, including same txn ID from two servers) |

### Cross-cutting

| Feature | E2E single server | E2E multi server | Notes |
| --- | --- | --- | --- |
| Loop prevention (ghost-sender skip, own-remote skip, post-ID echo) | ⚠️ | ⚠️ | `UserRemoteDetectionIntegrationTestSuite` asserts `IsRemote()` on hand-built users rather than sending an echo through the bridge. Multi only checks `isGhostUser` directly |
| Ghost namespace isolation per server domain | — | ✅ | `TestInboundRoutingIsolatedPerServer` (`isGhostUser` for A vs B) |

### Server and channel management

| Feature | E2E single server | E2E multi server | Notes |
| --- | --- | --- | --- |
| Add server + `server_name` discovery | ✅ | ❌ | `TestReAdoptionRoundTrip` uses the real `servers.Add` and discovery, but with only one server. Adding a second server next to a live one (uniqueness checks) isn't tested e2e |
| Remove + re-adopt server (mappings and ghosts preserved) | ✅ | ❌ | Same test. Nothing checks that server B keeps syncing while A is removed |
| Enable / disable server | ❌ | ❌ | Unit-only (`hooks_test.go`, `api_test.go`) |
| Connection / AS permission check (`/matrix test`, ping, health) | ❌ | ❌ | Client only (`TestConnection`, `TestApplicationServicePermissions`) |
| Registration YAML generation | ❌ | ❌ | Unit-only |
| Map channel to room | ⚠️ | ✅ | Single: only used as test setup. Multi: `MapChannelToServer` on both servers |
| One server per channel enforcement | — | ✅ | `TestChannelMappingRejectsSecondServer` |
| Unmap channel | ❌ | ❌ | Unit-only (`channel_mapping_test.go`) |
| `/matrix create` (create room and map) | ❌ | ❌ | Client only (`CreateRoom`) |
| Slash commands (`/matrix …`, `/matrix server …`) | ❌ | ❌ | Unit-only against a mock plugin (`command_test.go`) |
| System Console UI + REST API (`/api/v1/servers…`) | ❌ | ❌ | REST is unit-only (`api_servers_test.go`); no UI tests |
| KV migration to the multi-server layout | ❌ | — | Unit-only (`migrations_test.go`) |
| Cluster broadcast of registry changes | ❌ | ❌ | No e2e |

## Summary

Out of 47 rows:

| | ✅ | ⚠️ | ❌ | — |
| --- | --- | --- | --- | --- |
| Single server | 18 | 7 | 20 | 2 |
| Multi server | 6 | 3 | 37 | 1 |

- **Mattermost → Matrix on one server is the best-covered area.** Every row except DMs runs
  through the real shared-channels and join hooks in `e2e/mattermost_to_matrix_test.go`. Two
  rows are partial (DMs, attachment deletion); see their notes.
- **Matrix → Mattermost is barely covered.** Apart from one plain-text message in each of
  the multi-server suite and the `e2e/` smoke test, no inbound event is ever processed. Edits,
  reactions, redactions, files and membership have **no tests at any level**, not even unit
  tests.
- **Multi-server coverage is limited to routing and plain text messages.** The 4 multi-server
  tests prove that inbound/outbound text messages and webhook auth are isolated per server,
  that a channel can't be mapped to two servers, and the remove/re-add lifecycle (on one
  server). None of the other actions (edit, reaction, delete, thread, mention, file, DM,
  profile) are checked for per-server isolation.
- **Some existing tests are weaker than their names suggest:**
  `UserRemoteDetectionIntegrationTestSuite` mostly asserts on hand-built structs,
  `ThreadMappingIntegrationTestSuite` doesn't need the container,
  `testSyncChannelMembersToMatrixRoom` re-implements the production loop, and
  `DMRoomCreationTestSuite` skips checking that the message arrived.

## Suggested next steps (by priority)

1. **Inbound single-server suite.** Cover Matrix → Mattermost edit, reaction add/remove,
   message redaction, file/image, reply/thread, mentions, HTML formatting, and member
   join/leave/profile change. Ideally, events are produced by real Synapse users (the
   container already supports `CreateUser` / `JoinRoomAsUser`) and read back from the room,
   not hand-built.
2. **Outbound gaps on a single server.** DM and group DM delivery with
   `EnableSharedChannelsDMs` on, and attachment deletion if Mattermost ever re-syncs a deleted
   attachment.
3. **Move `server/` suite cases onto the real entrypoints.** Those suites call the bridge
   directly and hand-build inbound transactions; the `e2e/` harness drives the shared-channels
   hooks and has Synapse deliver transactions itself.
4. **A multi-server version of each action.** Use a shared table-driven test over
   `twoServerSetup`: run each action on A and assert it appears on A only (and vice versa).
   Add multi-specific cases:
   - one Mattermost user gets distinct ghosts on A and B;
   - mentions resolve to the target server's ghost domain;
   - DM auto-creation lands on the calling server;
   - disabling or removing A doesn't affect B;
   - adding a second server through `servers.Add` while one is live.
5. **Grow the real Mattermost e2e suite.** `e2e/harness` runs a real Mattermost server;
   slash commands, the REST API, and the System Console server management UI (with
   Playwright) still need tests there.
