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

- **Outbound tests call bridge methods directly** (`SyncPostToMatrix`, `SyncReactionToMatrix`),
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
| `server/sync_to_matrix_integration_test.go` | `MatrixSyncTestSuite` | 1 |
| `server/matrix_mentions_integration_test.go` | `TestMatrixMentionProcessing`, `TestMatrixMentionEdgeCases` | 1 |
| `server/plugin_integration_test.go` | `PluginIntegrationTestSuite` | 1 |
| `server/dm_room_creation_test.go` | `DMRoomCreationTestSuite` | 1 |
| `server/user_remote_detection_test.go` | `UserRemoteDetectionIntegrationTestSuite` | 1 |
| `server/matrix/test/client_test.go` | `MatrixClientTestSuite` (Matrix client only, no plugin) | 1 |
| `server/multi_server_integration_test.go` | `MultiServerIntegrationTestSuite` (4 tests) | 2 |
| `e2e/smoke_test.go` | `TestSmokeMattermostToMatrix`, `TestSmokeMatrixToMattermost` (real Mattermost) | 1 |
| `e2e/matrix_to_mattermost_test.go` | `TestMatrixToMattermost*` (real Mattermost; most events sent by real Synapse users) | 1 |

## Legend

- ✅ Covered: the plugin's code path runs against real Synapse and the outcome is asserted.
- ⚠️ Partial: exercised, but the outcome isn't asserted, the test re-implements the
  production logic, a key step is bypassed, or part or all of the row is skipped on a known
  plugin bug (see notes).
- ❌ Not covered e2e. "Client only" means the underlying `matrix.Client` call is tested in
  `client_test.go`, but the plugin logic around it is not.
- — Not applicable.

## Coverage table

### Mattermost → Matrix

| Feature | E2E single server | E2E multi server | Notes |
| --- | --- | --- | --- |
| Text message sync | ✅ | ✅ | `TestBasicMessageSync`, and `TestSmokeMattermostToMatrix` through the real shared-channels hook; `TestOutboundSyncIsolatedPerServer` checks the post reaches only the mapped server |
| Markdown → HTML formatting | ✅ | ❌ | `TestMarkdownMessageSync` |
| @mentions → Matrix pills / `m.mentions` | ✅ | ❌ | `TestMessageWithMentions`, `TestMatrixMentionProcessing`, `TestMatrixMentionEdgeCases`. Multi-server should check that the mention uses the target server's ghost domain |
| Thread replies | ✅ | ❌ | `TestThreadedMessage`; the parent's Matrix event ID is injected into `Props` by hand |
| Post edit | ✅ | ❌ | `TestMessageEdit`; event ID in `Props` injected by hand |
| Post deletion → redaction | ❌ | ❌ | Client only (`RedactEvent`); `deletePostFromMatrix` never runs |
| Reaction add | ✅ | ❌ | `TestReactionSync` |
| Reaction removal | ❌ | ❌ | `removeReactionFromMatrix` has no test |
| File attachments | ❌ | ❌ | Client only (`TestMatrixClientWithFiles`). The hook upload + pending-file attach flow (keyed per server) is untested |
| File attachment deletion | ❌ | ❌ | `deleteFileFromMatrix` has no test |
| Ghost user creation | ✅ | ⚠️ | `TestGhostUserCreationAndDetection`. Multi: a ghost is created on server A only; nothing checks that one Mattermost user gets separate ghosts on A and B |
| Display name sync (`SyncUserToMatrix`) | ❌ | ❌ | Client only (`SetDisplayName`) |
| Avatar sync (profile image hook) | ❌ | ❌ | Client only (`UpdateGhostUserAvatar`) |
| DM / group DM room auto-creation | ⚠️ | ❌ | `DMRoomCreationTestSuite` checks the room's creation and name, but explicitly skips checking that the message was delivered. Multi-server DM routing (DM created on the calling server) is unit-only |
| User joins channel → ghost joins room | ⚠️ | ❌ | `testSyncChannelMembersToMatrixRoom` re-implements the loop inside the test instead of calling `UserHasJoinedChannel` |
| Matrix-originated user re-invited to room | ✅ | ❌ | `testInviteRemoteUserToMatrixRoom`. The rule that users are never invited to a server they didn't come from is unit-only |
| Shared-channels hook routing (server resolved from `RemoteCluster`, own-remote skip) | ⚠️ | ❌ | Single: `TestSmokeMattermostToMatrix` goes through `OnSharedChannelsSyncMsg` with a real `RemoteCluster`, but the own-remote skip is unit-only (`TestServerIDForSyncMsg`) |

### Matrix → Mattermost

| Feature | E2E single server | E2E multi server | Notes |
| --- | --- | --- | --- |
| Text message → post | ✅ | ✅ | Single: `TestMatrixToMattermostTextMessage` (remote author, `from_matrix` and event ID props) and `TestSmokeMatrixToMattermost`. Multi: `TestInboundRoutingIsolatedPerServer`, with a hand-built transaction through `ServeHTTP` |
| HTML → Markdown conversion | ✅ | ❌ | `TestMatrixToMattermostHTMLFormatting` |
| Matrix mentions → @username | ✅ | ❌ | `TestMatrixToMattermostMentions`: pills to a ghost and to a provisioned Matrix user |
| Replies / threads (`m.in_reply_to`, `m.thread`) | ✅ | ❌ | `TestMatrixToMattermostReplies`. An `m.in_reply_to` reply without `m.thread` becomes a thread reply under the parent's root post |
| Edit (`m.replace`) | ✅ | ❌ | `TestMatrixToMattermostEdit` |
| Reaction add | ✅ | ❌ | `TestMatrixToMattermostReactions`, on Matrix- and Mattermost-originated posts |
| Reaction removal (redaction) | ✅ | ❌ | `TestMatrixToMattermostReactions` |
| Message deletion (redaction) | ✅ | ❌ | `TestMatrixToMattermostMessageDeletion` |
| Files / images / video / audio | ✅ | ❌ | `TestMatrixToMattermostFiles`: bytes, name and mimetype of `m.image`, `m.file`, `m.video` and `m.audio` |
| Matrix user → Mattermost user provisioning | ✅ | ⚠️ | Single: `TestMatrixToMattermostUserProvisioning`: one remote user, reused, named from the prefix and profile, and a `username_prefix` change applies to new users. Multi: `CreateUser` is mocked, and the per-server prefix or remote ID isn't asserted |
| Member join / leave / ban → channel membership | ⚠️ | ❌ | `TestMatrixToMattermostMembership`: join, leave and rejoin pass. Kick and ban are skipped on a plugin bug: member events use `Sender` instead of `state_key` |
| Profile change (displayname / avatar) → Mattermost user | ✅ | ❌ | `TestMatrixToMattermostProfileChange` |
| Matrix-initiated DM → Mattermost DM | ⚠️ | ❌ | `TestMatrixToMattermostDirectMessage` is skipped on a plugin bug: Mattermost refuses a DM with a remote user. In a manual run with Mattermost's `EnableSharedChannelsDMs` flag, the DM was created but its messages never arrived (the ghost never joins the room). Unit: `TestHandleMatrixMemberDM_*` |
| Webhook auth (per-server `hs_token`) | ✅ | ✅ | Single: `TestMatrixToMattermostWebhookAuth` (missing or wrong token gets `401`, no post); Synapse's own deliveries prove the accepted token. Multi: `TestInboundRoutingIsolatedPerServer` |
| Transaction dedupe / retry | ✅ | ❌ | Dedupe: `TestMatrixToMattermostTransactionDedupe`. Retry (`503` without recording the txn) stays unit-only (`TestHandleMatrixTransaction`, including same txn ID from two servers) |

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
| Single server | 23 | 7 | 15 | 2 |
| Multi server | 6 | 3 | 37 | 1 |

- **Mattermost → Matrix on one server is the best-covered area.** Messages, markdown,
  mentions, threads, edits and reaction-add all have real assertions. Deletions, reaction
  removal, files and profile sync do not.
- **Matrix → Mattermost on one server is covered end to end.** Real Synapse users send every
  inbound event type to a real Mattermost. Kick/ban and Matrix-initiated DMs are skipped on
  plugin bugs, and retry after a failed transaction is unit-only.
- **Multi-server coverage is limited to routing and plain text messages.** The 4 multi-server
  tests prove that inbound/outbound text messages and webhook auth are isolated per server,
  that a channel can't be mapped to two servers, and the remove/re-add lifecycle (on one
  server). None of the other actions (edit, reaction, delete, thread, mention, file, DM,
  profile) are checked for per-server isolation.
- **Some existing tests are weaker than their names suggest:**
  `UserRemoteDetectionIntegrationTestSuite` mostly asserts on hand-built structs,
  `testSyncChannelMembersToMatrixRoom` re-implements the production loop, and the DM tests
  skip checking that the message arrived.

## Suggested next steps (by priority)

1. **Fix the inbound bugs the e2e suite skips.** Member events should use `state_key`, so a
   kick or ban removes the target instead of the kicker. Matrix-initiated DMs need a direct
   channel Mattermost accepts with a remote user, and the ghost has to join the room so its
   messages are delivered.
2. **Outbound gaps on a single server.** Post deletion, reaction removal, file attachments
   through `OnSharedChannelsAttachmentSyncMsg` + `SyncPostToMatrix`, display name and avatar
   sync, and DM message delivery.
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
