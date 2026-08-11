# Matrix homeserver management in the System Console — API + UI implementation plan

This document is a self-contained implementation brief. Start from `multi-server-support-backend`
on a fresh branch and implement everything below. It builds on
`spec/2026-07-28-multi-server-support-backend.md`, which is implemented on that branch;
references of the form (backend §X) point at it.

## 1. Goal

The backend brief deliberately left the webapp untouched. Consequently homeservers can only be
managed through `/matrix server …`, and the two admin-console components that remain
(`registration_download`, `homeserver_config`) are **inert**: they scrape DOM inputs for
config keys (`matrix_server_url`, `matrix_as_token`, `matrix_hs_token`) that no longer exist in
`plugin.json`.

Give System Admins a first-class System Console surface over the same KV registry: a plugin
REST API under `/api/v1`, and a custom admin-console section that lists, adds, edits,
enables/disables, tests and removes homeservers, shows each one's Application Service
registration, and lists the channels bridged to it (read-only, with unmap).

Slash commands stay, with unchanged behaviour. Both surfaces must call **one** implementation.

## 2. Scope and constraints

**In scope**

- **A `server/servers` package owning the registry** (§3.2). The console needs the same
  read/diagnostic logic the slash commands have, and that logic is currently split three ways:
  `*Plugin` methods, private `command.Handler` methods, and an inline copy in `BridgeUtils`.
  Rather than adding a third caller and more methods to a `*Plugin` that already has 89, the
  registry moves into a leaf package with a narrow `Host` interface for the things only the
  plugin runtime can do.
- A plugin REST API for the server registry, System Admin only (§3.5).
- **`Service.Update`** — a new registry mutation (§3.3). The console needs to rotate a token or
  fix a URL without remove-then-re-add, which the migrated legacy entry cannot do at all
  (backend §3.1.1).
- **Typed sentinel errors** in the new package (§3.4), so the API can answer 404/409/400
  instead of 500-for-everything.
- `plugin.json` `settings_schema` restructured into `sections` (§3.6) — with the trap that this
  hides `rate_limiting_mode` if done naively.
- A custom admin-console **section** with the views in §3.8.
- Deleting the two inert components, preserving the one piece of real content
  `homeserver_config` carried (§3.7).
- Unit tests throughout: Go for the API, `Service.Update` and error mapping; jest for the webapp.

**Out of scope**

- **Mapping a new channel from the console.** `mapChannelCore` (`server/command/command.go:572-664`)
  joins the room, creates a ghost for the invoking user, adds the bridge alias, syncs every
  channel member, shares the channel and invites that server's remote — all against
  `args.ChannelId`. Driving that from the console needs a channel picker and a
  channel-context-free rewrite of the flow. `/matrix map` stays the way to create a mapping;
  the console lists and removes them.
- `/matrix create` (room creation) and `/matrix migrate` equivalents.
- Lifting one-server-per-channel (backend §6).
- **i18n.** `webapp/i18n/en.json` is `{}` and no existing component uses `react-intl`. New
  components use plain strings; translating the webapp is separate work.
- Any non-admin surface. Everything here is `PermissionManageSystem`.

**Platform requirement**

None new. `min_server_version` stays `11.8.0`; `registerAdminConsoleCustomSection` and
`settings_schema.sections` both long predate it.

---

## 3. Target design

### 3.1 Where the registry logic lives today

The registry is currently spread across three layers, and the console would be the fourth
caller of the same reads:

| Existing                                                                      | Fate                                                              |
| ----------------------------------------------------------------------------- | ----------------------------------------------------------------- |
| `p.getServers`, `p.mutateServers`, `p.serverByID`, `p.serverDomainForID`      | **Move** to `servers.Service` (§3.2).                             |
| `p.AddServer`, `p.RemoveServer` (`servers.go:139,246`)                         | **Move**; behaviour unchanged, errors typed (§3.4).               |
| `p.SetServerEnabled` (`plugin.go:555`)                                         | **Move** → `Service.SetEnabled`.                                  |
| `normalizeServerEndpoint`, `eventDomainFromEndpoint`, `p.resolveServerName`   | **Move**; `ResolveServerName` stays exported (the migration needs the same function — backend §3.8). |
| `Handler.probeServerHealth` (`command.go:413`)                                | **Move** → `Service.ProbeHealth`.                                 |
| `Handler.countMappedChannelsPerServer` (`command.go:387`)                      | **Move** → `Service.CountMappedChannels`.                         |
| `Handler.resolveServerIDArg` (`command.go:318`)                                | **Move** → `Service.ResolveIdentifier`.                           |
| `Handler.testServerConnection` (`command.go:748`)                              | **Move** its checks as structured data → `Service.Diagnose`.       |
| Registration YAML in `executeServerRegistrationCommand` (`command.go:1133`)    | **Move** → `Service.RegistrationYAML`; must never be duplicated.  |
| `BridgeUtils.serverConfig` (`bridge_utils.go:151-166`)                         | **Delete the inline copy**; delegate to the service (§3.2).       |
| `DefaultMatrixUsernamePrefix` (`configuration.go:13`)                          | **Move** → `servers.DefaultUsernamePrefix`; 3 use sites.          |
| `p.materializeServerFromLegacyConfig`, `legacyServerConfig` (`servers.go:324-409`) | **Stay in `main`** — they read the _platform's_ plugin config (§3.2). |
| `p.cachedServerConfigs` (`plugin.go:301`), `p.getMatrixClient`, `p.remoteIDForServer`, `p.initMatrixClients`, `p.refreshServersAndBroadcast`, `p.registerForSharedChannels` | **Stay in `main`** — per-node caches and platform calls; reached through `Host` (§3.2). |
| `p.MapChannelToServer`, `p.UnmapChannelFromServer` (`channel_mapping.go`)      | **Stay in `main`** — they need `BridgeUtils` and the remote. Reuse as-is. |

### 3.2 The registry becomes `server/servers`

`main` imports `command`, so `command` cannot import `main` — which is why the helpers ended up
duplicated in `command` in the first place. A **leaf package imported by both** breaks that
constraint permanently:

```
kvstore, matrix  ←  servers  ←  main
                         ↖────────  command
```

No cycle: `servers` imports only `kvstore` and `matrix`; `main` and `command` both import
`servers`.

```go
// server/servers/service.go
package servers

// Host is everything the service needs from the plugin runtime: per-node Matrix clients,
// shared-channels remote lifecycle, cache invalidation, and the site/plugin identity used to
// build the registration URL. Package main implements it with a thin adapter; tests use a fake.
type Host interface {
    MatrixClient(serverID string) *matrix.Client
    RegisterRemote(serverID string) error
    UnregisterRemote(remoteID string) error
    RefreshAndBroadcast(reason string) error
    SiteURL() string
    PluginID() string
}

// Logger is declared here rather than imported so this package depends on nothing above it;
// main's Logger satisfies it structurally.
type Logger interface {
    LogDebug(message string, keyValuePairs ...any)
    LogInfo(message string, keyValuePairs ...any)
    LogWarn(message string, keyValuePairs ...any)
    LogError(message string, keyValuePairs ...any)
}

type Service struct {
    kv     kvstore.KVStore
    logger Logger
    host   Host
}

func New(kv kvstore.KVStore, logger Logger, host Host) *Service
```

Method set:

```go
// Registry reads
func (s *Service) List() ([]kvstore.ServerConfig, error)          // was p.getServers
func (s *Service) Get(serverID string) (kvstore.ServerConfig, error) // was p.serverByID
func (s *Service) Domain(serverID string) (string, error)         // was p.serverDomainForID
func (s *Service) ResolveIdentifier(arg string) (string, error)    // ID, then ServerName, then URL host

// Registry mutations
func (s *Service) Add(req AddRequest) (kvstore.ServerConfig, error)
func (s *Service) Update(serverID string, u Update) (kvstore.ServerConfig, []string, error) // §3.3
func (s *Service) Remove(serverID string) (bool, error)
func (s *Service) SetEnabled(serverID string, enabled bool) error
func (s *Service) Seed(entry kvstore.ServerConfig) (string, error) // migration only; see below

// Derived views
func (s *Service) ProbeHealth(servers []kvstore.ServerConfig) map[string]string
func (s *Service) CountMappedChannels() (map[string]int, error)
func (s *Service) Mappings(serverID string) ([]ChannelMapping, error) // {ChannelID, RoomID} from KV
func (s *Service) Diagnose(serverID string) Diagnostics
func (s *Service) RegistrationYAML(serverID string) (filename, content string, err error)
func (s *Service) ResolveServerName(serverURL, configuredName string) (string, error)

// Helpers that were package-level in main
func NormalizeEndpoint(rawURL string) (string, error) // was normalizeServerEndpoint
```

`AddRequest` replaces `AddServer`'s six positional string parameters, four of which are
optional and two of which are interchangeable at the call site (`serverID`, `serverNameOverride`
— both opaque strings):

```go
type AddRequest struct {
    ServerURL      string
    ASToken        string
    HSToken        string
    UsernamePrefix string // "" → DefaultUsernamePrefix
    ServerID       string // "" → model.NewId(); non-empty re-adopts (backend §3.1.1)
    ServerName     string // "" → discovered (backend §3.1.2)
}
```

**Wiring in `main`.** `Plugin` gains **one field and no methods**; the `Host` implementation is a
separate adapter type so `Plugin`'s method set does not widen:

```go
// server/servers_host.go — package main
//
// pluginHost adapts *Plugin to servers.Host without adding those methods to Plugin itself.
type pluginHost struct{ p *Plugin }

func (h pluginHost) MatrixClient(serverID string) *matrix.Client { return h.p.getMatrixClient(serverID) }
func (h pluginHost) RegisterRemote(serverID string) error        { return h.p.registerServerForSharedChannels(serverID) }
func (h pluginHost) RefreshAndBroadcast(reason string) error     { return h.p.refreshServersAndBroadcast(reason) }
func (h pluginHost) PluginID() string                            { return manifest.Id }

// UnregisterRemote must convert explicitly: UnregisterPluginRemoteForSharedChannels returns
// *model.AppError, and returning a nil *AppError as an error yields a NON-nil error interface.
func (h pluginHost) UnregisterRemote(remoteID string) error {
    if appErr := h.p.API.UnregisterPluginRemoteForSharedChannels(remoteID); appErr != nil {
        return appErr
    }
    return nil
}
```

**Construction order matters.** `p.servers = servers.New(p.kvstore, p.logger, pluginHost{p})`
goes in `OnActivate` immediately after `p.kvstore = kvstore.NewKVStore(p.client)` and **before**
`p.runKVStoreMigrations()` — the migrations read and seed the registry (backend §3.8). And
because `OnConfigurationChange` can fire before `OnActivate`, every existing `p.kvstore == nil`
guard must also cover `p.servers == nil`; `initMatrixClients` in particular now reads
`p.servers.List()` and would panic on a nil service.

**The migration keeps its own seeding path.** `materializeServerFromLegacyConfig` stays in
`main` because it calls `p.API.LoadPluginConfiguration` — a platform dependency this package
must not acquire. It composes the entry itself (preserving the deliberate `SiteURL: ""` and
master-derived `EventDomain` from backend §3.8) and stores it with `Service.Seed`, which is an
idempotent insert-if-endpoint-absent and deliberately **not** `Add`: no name resolution, no
`SiteURL` derivation, no remote registration. It calls `Service.ResolveServerName` directly for
the name, which is what keeps backend §3.8's "same function as add" property true.

**`BridgeUtils` stops duplicating the read.** `serverConfig()` is a hand-rolled copy of
`Get` down to the error string. Give `BridgeUtils` a one-method getter — the same pattern it
already uses for `channelMapper` — and delete the copy:

```go
type ServerGetter interface {
    Get(serverID string) (kvstore.ServerConfig, error)
}
```

**Exactly one implementation of the registration YAML is non-negotiable.** Backend §3.9
documents that a wrong `url:` line silently kills inbound sync in one direction only, while
outbound keeps working. Two copies of that string is two chances to get it wrong, and a copy in
`api.go` would not be covered by the existing command test. `RegistrationYAML` owns the filename
too, so the API and any future download path agree.

`Diagnose` returns data, not prose:

```go
type Diagnostics struct {
    ServerID   string
    Checks     []Check           // ordered: registry, client, connection, appservice
    ServerInfo *matrix.ServerInfo // nil when unavailable
}

type Check struct {
    Key    string // "registry" | "client" | "connection" | "appservice"
    Label  string
    Status string // "ok" | "fail" | "skip"
    Detail string // error text or supporting detail; may be empty
}
```

Preserve `testServerConnection`'s short-circuit exactly: an unregistered server yields only a
failed `registry` check; a nil client yields `ok`/`fail` then `skip` for the rest. A skipped
check must never render as a pass.

`statusProbeDeadline` (`command.go:76`) and `listAllKeysBatchSize` (`servers.go:17`) move into
the package and stay `var`s so tests can shorten them.

**`command` sheds interface surface instead of gaining it.** `PluginAccessor` **loses**
`GetManagedServers`, `AddServer`, `RemoveServer` and `SetServerEnabled`, and gains one accessor:

```go
Servers() *servers.Service
```

`Handler`'s `probeServerHealth`, `countMappedChannelsPerServer`, `resolveServerIDArg`, the
registration YAML literal and `testServerConnection`'s check sequence are deleted, replaced by
calls through `c.plugin.Servers()`. The markdown formatting stays in `command` — the service
returns data, the command renders it, the API marshals it. Regenerate `server/command/mocks`.

`GetMatrixClientForServer`, `GetRemoteIDForServer`, `CreateOrGetGhostUserForServer`,
`GetMatrixUserIDFromMattermostUserForServer`, `MapChannelToServer`, `UnmapChannelFromServer`,
`GetKVStore`, `GetPluginAPI`, `GetPluginAPIClient`, `GetPluginID` and the migration methods all
stay on `PluginAccessor` — they are genuinely plugin-runtime concerns, not registry ones.

**This step must change no behaviour.** Every existing test in `server/command` passes
unmodified. `server/servers_test.go` (694 lines) moves to `server/servers/service_test.go` and
its harness gets *simpler*: instead of assembling a `Plugin`, it constructs
`servers.New(mockKV, testLogger, &fakeHost{})`, and the assertions that currently reach into
plugin state ("registered a remote", "refreshed and broadcast") become recorded calls on the
fake. That harness swap is the bulk of the diff and is why this is its own step and its own
commit.

### 3.3 `Service.Update`

```go
// Update carries a partial update. A nil field means "leave alone" — an absent field and an
// empty string are NOT the same thing, which is what lets the edit form send only what the
// admin actually changed.
type Update struct {
    ServerURL      *string
    ASToken        *string
    HSToken        *string
    UsernamePrefix *string
    ServerName     *string
}

func (s *Service) Update(serverID string, u Update) (kvstore.ServerConfig, []string, error)
```

Returns the updated entry and a list of human-readable **warnings** (not errors) for changes
that succeeded but have consequences the admin must know about.

| Field            | Editable | Rule, and what goes wrong otherwise                                                                                                                                                                                                                                                                                                                                          |
| ---------------- | -------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `ServerID`       | never    | It is the KV namespace (backend §3.2). Changing it orphans every record.                                                                                                                                                                                                                                                                                                     |
| `EventDomain`    | never    | Not settable and **never recomputed**, even when `ServerURL` changes. Recomputing orphans the `matrix_event_id_<domain>` property on every already-synced post, so edits and deletes of those posts silently stop working — the exact regression backend §3.6 persists the field to prevent.                                                                                    |
| `SiteURL`        | never    | It is the shared-channels remote's identity key, not a reachable URL. Re-keying it makes the platform hand back a _different_ remote, leaving every previously-synced ghost attributed to a remote its server no longer owns (backend §3.4). Do not surface it in the UI as a "URL"; if shown at all, label it as the remote key.                                               |
| `RemoteID`       | never    | Derived from registration.                                                                                                                                                                                                                                                                                                                                                   |
| `Endpoint`       | derived  | Re-derived from `ServerURL` via `normalizeServerEndpoint`; still the uniqueness key.                                                                                                                                                                                                                                                                                          |
| `ServerURL`      | yes      | Re-derive `Endpoint`; reject an endpoint live on **another** entry (`servers.ErrEndpointTaken`). Do **not** re-resolve `ServerName` and do **not** touch `EventDomain`/`SiteURL`. When the endpoint changes, return a warning stating that `EventDomain` and the remote key stay at their original values by design.                                                              |
| `ASToken`        | yes      | Reject `""` (`servers.ErrInvalidInput`).                                                                                                                                                                                                                                                                                                                                        |
| `HSToken`        | yes      | Reject `""`. An empty `HSToken` is skipped by the inbound middleware's non-empty guard (`api.go:96-98`), so storing one silently makes every inbound transaction 401 with nothing in the logs explaining why.                                                                                                                                                                  |
| `UsernamePrefix` | yes      | `""` resets to `DefaultMatrixUsernamePrefix`, matching `AddServer`. Low risk: the prefix only names the _Mattermost_ users minted for Matrix-originated users (`bridge_utils.go:168-182`); existing users keep their usernames. Say so in the UI so admins do not fear it.                                                                                                     |
| `ServerName`     | yes      | **Dangerous.** `isGhostUser` matches `@_mattermost_<id>:<ServerName>` (`matrix_webhook.go:302-315`), so changing it makes every existing ghost unrecognized and inbound events from them get treated as real Matrix users' events. Resolve through `s.ResolveServerName(url, name)` so normalization matches add time, re-check uniqueness (`servers.ErrNameTaken`), and warn. |

Implementation notes:

- Validate and resolve **before** entering the CAS mutator — `ResolveServerName` performs a
  network probe and the callback must stay pure (backend §3.1).
- Uniqueness checks run **inside** the callback, against the live slice, excluding the entry
  being edited. Two concurrent edits must not both win.
- On success call `host.RefreshAndBroadcast("server_updated")`. New tokens and URLs only reach
  `matrixClients` through `initMatrixClients`, and other cluster nodes only through the
  broadcast; skipping it leaves a rotated token working on one node and failing on the rest.
- Do **not** re-register or unregister the shared-channels remote. `SiteURL` is unchanged, so
  there is nothing to re-key, and re-registration buys nothing while risking cursors.
- A migrated entry (`SiteURL == ""`) **is** editable — that is the whole point, since it cannot
  be removed and re-added. Only `Remove` refuses it.

### 3.4 Typed errors, so the API can answer with the right status

Every registry failure today is an opaque `errors.Errorf` string, so a REST handler cannot tell
"duplicate endpoint" from "KV unavailable". Add sentinels to `server/servers`, named without a
redundant prefix since the package qualifies them at every call site (`servers.ErrNotRegistered`),
and wrap them **keeping today's message text** so command output does not change:

```go
var (
    ErrNotRegistered     = errors.New("server is not registered")
    ErrEndpointTaken     = errors.New("a server is already registered at this endpoint")
    ErrNameTaken         = errors.New("server name conflicts with an existing server")
    ErrIDTaken           = errors.New("server ID is already registered")
    ErrMigratedImmutable = errors.New("server was migrated from the legacy configuration and cannot be removed")
    ErrInvalidInput      = errors.New("invalid server input")
)
```

Call sites to convert as they move: `Add`'s three in-mutator rejections plus its URL and ID
validation (was `servers.go:140-171`), `Remove`'s `SiteURL == ""` refusal (was
`servers.go:262-264`), `Get` (was `servers.go:308`), `SetEnabled`'s not-found (was
`plugin.go:572`), `BridgeUtils.serverConfig`'s replaced error, and the new `Update`.

These sentinels are returned from inside the CAS callback, so they travel out through
`kvstore.SetAtomicWithRetries` → `pluginapi`'s `KV.SetAtomicWithRetries`. **Add a test asserting
`errors.Is` still matches after that round trip.** If `pluginapi` flattens the chain, `mutate`
must capture the sentinel in a closed-over variable and return that directly rather than relying
on the wrap.

| Condition                                              | Status | Notes                                        |
| ------------------------------------------------------ | ------ | -------------------------------------------- |
| `servers.ErrNotRegistered`                             | 404    |                                              |
| `servers.ErrEndpointTaken` / `ErrNameTaken` / `ErrIDTaken` | 409    | Message already names the conflicting server. |
| `servers.ErrMigratedImmutable`                          | 409    | UI offers Disable instead.                   |
| `servers.ErrInvalidInput`, malformed JSON, bad URL      | 400    |                                              |
| `kvstore.ErrChannelAlreadyMapped`                      | 409    | Only reachable from a future map endpoint.   |
| No Matrix client for the server on this node           | 503    | Same meaning as the inbound 503 (backend §3.5). |
| Anything else                                          | 500    | Log with `p.logger.LogError`, return a generic message. |

### 3.5 REST API

All routes hang off the existing `apiRouter` (`/api/v1`, `api.go:41`) behind a **new**
`SystemAdminRequired` middleware, applied on top of `MattermostAuthorizationRequired`:

```go
adminRouter := apiRouter.NewRoute().Subrouter()
adminRouter.Use(p.SystemAdminRequired) // model.PermissionManageSystem, 403 otherwise
```

Move the autocomplete handler onto `adminRouter` and delete its inline permission check
(`api.go:150-154`) so the gate lives in exactly one place. Its existing 403 test must still pass
unchanged.

Cookie-authenticated browser requests only get `Mattermost-User-ID` populated when they carry
`X-Requested-With: XMLHttpRequest`, and non-GET requests additionally need the CSRF token.
`Client4.getOptions()` supplies both — see §3.8. A request missing them fails the existing
`MattermostAuthorizationRequired` with 401, which reads as "not logged in" and is confusing to
debug; note it in the client module's comment.

| Method   | Path                                        | Body                | Success                                        |
| -------- | ------------------------------------------- | ------------------- | ---------------------------------------------- |
| `GET`    | `/servers`                                  | —                   | 200 `{servers: [ServerView], counts_unavailable?: bool}` |
| `POST`   | `/servers`                                  | `AddServerRequest`  | 201 `{server: ServerView, warnings: [string]}` |
| `PATCH`  | `/servers/{server_id}`                      | `UpdateServerRequest` | 200 `{server: ServerView, warnings: [string]}` |
| `DELETE` | `/servers/{server_id}`                      | —                   | 200 `{server_id, recovery_command}`            |
| `PUT`    | `/servers/{server_id}/enabled`              | `{enabled: bool}`   | 200 `{server: ServerView}`                     |
| `GET`    | `/servers/health`                           | —                   | 200 `{health: {<server_id>: string}}`          |
| `POST`   | `/servers/{server_id}/test`                 | —                   | 200 `servers.Diagnostics`                        |
| `GET`    | `/servers/{server_id}/registration`         | —                   | 200 `{filename, content}`                      |
| `GET`    | `/servers/{server_id}/mappings`             | —                   | 200 `{total_count, mappings: [MappingView]}`   |
| `DELETE` | `/servers/{server_id}/mappings/{channel_id}` | —                   | 200 `{}`                                       |

Errors use one shape — `{"message": "..."}` — with the status from §3.4. Messages from the
registry are already written for humans (they name the conflicting `server_id`, point at the
recovery command), so the UI renders them verbatim rather than substituting its own.

**`ServerView` never contains a token:**

```json
{
    "server_id": "sd8f7g6h5j4k3l2m1n0p9q8r7s",
    "server_url": "https://matrix.example.com",
    "server_name": "example.com",
    "endpoint": "matrix.example.com:443",
    "event_domain": "matrix_example_com_443",
    "username_prefix": "matrix",
    "enabled": true,
    "remote_id": "xyz…",
    "is_migrated": false,
    "has_as_token": true,
    "has_hs_token": true,
    "mapped_channel_count": 3
}
```

- `is_migrated` is `SiteURL == ""`. The console uses it to disable Remove and explain why
  (backend §3.1.1). Do not serialize `SiteURL` itself.
- `mapped_channel_count` is `null` and the response sets `counts_unavailable: true` when the
  keyspace scan fails. The UI renders "unavailable", **never 0** — the same rule the commands
  follow, because 0 reads as "nothing is bridged" and invites an admin to remove a live server.
- Tokens are never serialized. `has_as_token` / `has_hs_token` let the edit form show
  "configured" placeholders without leaking values.

`GET /servers` reads the registry via `p.servers.List()` (a fresh KV read), **not**
`cachedServerConfigs`. The cache exists for the inbound hot path; an admin who just added a
server and sees a stale list will add it again.

**Health is a separate endpoint** because probing costs up to `statusProbeDeadline` (8s) and
would otherwise make the table unusable on first paint. The UI renders from `GET /servers`
immediately, then fills the Health column from `GET /servers/health`. It probes only enabled
servers with a live client, exactly as the command does, and reports `timed out` rather than
healthy for a probe that misses the deadline.

`POST /servers/{id}/test` is a POST, not a GET: it performs real network calls including the
Application Service permission probe, and must not be cached by any intermediary.

**Registration** returns JSON (`{filename, content}`), not `text/yaml`, so the UI can show a
copy box and offer a download without a second request. This handler's response carries **both
tokens** — log nothing from it, not even at debug level.

**`MappingView`:**

```json
{
    "channel_id": "…",
    "channel_name": "Town Square",
    "team_name": "core",
    "room_id": "!abc:example.com",
    "channel_missing": false
}
```

- Split along the package boundary: `Service.Mappings(serverID)` does the KV half — one
  `ListAllKeysWithPrefix(KeyPrefixChannelMapping)` scan, `ParseChannelServerMappings` on each
  value, keep entries whose `ServerID` matches (never index `[0]` — backend §3.3) — and returns
  `[]ChannelMapping{ChannelID, RoomID}`. The **handler in `main`** decorates those with channel
  and team display names through the plugin API with a memoized team lookup, sorts by team then
  channel name, and paginates in memory. Keeping the name lookup out of the service is what lets
  `Host` stay at six methods and the package stay platform-free.
- `page`/`per_page` query params, default 50, max 200, `total_count` in the response.
- A DM or group channel has no team; render `team_name: ""` and let the UI label it "Direct
  message".
- A mapping whose channel no longer exists sets `channel_missing: true` and is still listed —
  otherwise the admin cannot unmap a deleted channel's stale record.
- This is a **full-keyspace scan per request**, so the UI fetches it only when the admin opens a
  server's mappings panel, never as part of the list render. `mapped_channel_count` in
  `GET /servers` costs the same scan once for all servers, which is why the list must not
  auto-poll (§3.8).

`DELETE …/mappings/{channel_id}` delegates to `UnmapChannelFromServer`. Its "channel is not
mapped to server X" maps to 404 and its missing-client error to 503. Note that this call clears
Matrix room state first and aborts if that fails (`channel_mapping.go:151-153`), so a failure
genuinely means nothing was changed and the UI can safely say so.

Every mutating handler logs one structured line via `p.logger.LogInfo` naming the action, the
`server_id` and the acting user ID — and **never** a token value.

### 3.6 `plugin.json` — `settings_schema` becomes `sections`

**The trap:** the admin console renders `sections` **or** top-level `settings`, never both.
`custom_plugin_settings/index.ts` does `if (schema.sections) {…} else if (schema.settings) {…}`
and returns `settings: undefined` whenever sections exist. Adding a section for the server UI
without also moving `rate_limiting_mode` into a section makes the rate-limiting dropdown
**silently disappear from the System Console** while its stored value keeps taking effect —
an admin cannot see or change it, and nothing logs a complaint.

```json
"settings_schema": {
    "header": "Matrix homeservers are managed in the section below, or with the /matrix server slash command.",
    "footer": "For more information, see the [Matrix specification](https://spec.matrix.org/v1.14/)",
    "sections": [
        {
            "key": "matrix_servers",
            "title": "Matrix homeservers",
            "subtitle": "Add, test and manage the homeservers this bridge connects to. Changes in this section apply immediately and do not require Save.",
            "custom": true,
            "fallback": false,
            "settings": []
        },
        {
            "key": "advanced",
            "title": "Advanced",
            "settings": [ { "key": "rate_limiting_mode", "…": "unchanged" } ]
        }
    ]
}
```

**Ordering: `matrix_servers` must be declared first, and that is as high as it can go.**
`schema_admin_settings.tsx` renders sections with a plain `schema.sections.forEach` — no sorting,
no key lookup — so **the page order is the `sections` array order**. Declaring the servers
section first is therefore all that is required, and it must stay first if a section is ever
added; `rate_limiting_mode` goes _below_ it in `advanced`, not above.

One qualification, because it cannot be worked around: the console **unconditionally prepends**
its own section holding the plugin enable/disable toggle (`custom_plugin_settings/index.ts` does
`sections.unshift(...)` whenever a plugin declares sections). So the servers section is the first
**plugin-defined** section but the second block on the page, under the Enable toggle. A plugin
cannot reorder or suppress that toggle, and it is where admins expect it. The prepended section
also carries the schema-level `header`/`footer`, which is why the header text below introduces
the servers section that follows it.

- Section keys are lowercased on both registration and lookup (the plugins reducer and
  `custom_plugin_settings`), so register with the same lowercase string.
- `custom: true, fallback: false` means: with the plugin disabled, its webapp bundle is not
  loaded, no component is registered, and the console renders the built-in "In order to view
  this section, enable the plugin and click Save" banner. That is correct — there is no static
  fallback worth rendering for a live registry.

### 3.7 Webapp registration, typings, and deletions

```ts
// webapp/src/index.tsx
registry.registerAdminConsoleCustomSection('matrix_servers', MatrixServersSection);
```

`webapp/src/types/mattermost-webapp/index.d.ts` declares only `registerPostTypeComponent` and
`registerAdminConsoleCustomSetting`; add the verified signature

```ts
registerAdminConsoleCustomSection(key: string, component: React.ElementType);
```

The console passes a custom section `{settingsList, sectionTitle, sectionDescription}`
(`schema_admin_settings.tsx`, the `section.component` branch). `settingsList` is the rendered
list of that section's declared settings — empty here, since the section declares none — so the
component ignores it but must still accept the props without tripping `strict` TS.

**Delete** `webapp/src/components/admin_console_settings/registration_download/` and
`.../homeserver_config/`, and their registrations. `registration_download` is replaced by the
registration view. `homeserver_config` carries one thing worth keeping — the
`room_list_publication_rules` snippet restricting room-directory publication to
`@_mattermost_bridge:<domain>` — so **move that into the registration view** (§3.8), where the
domain comes from the server's `ServerName` instead of being scraped out of a DOM input with a
500 ms polling loop.

### 3.8 Webapp architecture

| File                                                             | Contents                                                          |
| ---------------------------------------------------------------- | ----------------------------------------------------------------- |
| `webapp/src/client/index.ts`                                     | Typed fetch wrappers, one per endpoint                            |
| `webapp/src/types/matrix.ts`                                     | `ServerView`, request/response and diagnostics types               |
| `webapp/src/components/admin_console_settings/servers/index.tsx` | Section entry: header, table, panel/modal orchestration            |
| `…/servers/use_servers.ts`                                       | Data hook: load, mutate, refresh, error state                      |
| `…/servers/server_table.tsx`, `server_row.tsx`                   | Table and row (including the enable toggle)                       |
| `…/servers/add_server_modal.tsx`                                 | Add form                                                          |
| `…/servers/edit_server_modal.tsx`                                | Edit form                                                         |
| `…/servers/remove_server_dialog.tsx`                             | Removal confirm carrying the recovery key                          |
| `…/servers/test_results_modal.tsx`                               | Diagnostics checklist                                             |
| `…/servers/registration_modal.tsx`                               | YAML + homeserver config guidance                                  |
| `…/servers/mappings_panel.tsx`                                   | Per-server bridged-channel list                                   |

The client module:

```ts
import {Client4} from 'mattermost-redux/client';
import manifest from '@/manifest';

const base = () => `${Client4.getPluginRoute(manifest.id)}/api/v1`;
```

`Client4.doFetch` is `protected` and therefore unusable from plugin code. Use `window.fetch`
with `Client4.getOptions({method, body})`, which is public and supplies credentials,
`X-Requested-With` and the CSRF token that the plugin HTTP path needs for cookie-authenticated
non-GET requests (§3.5).

Every helper must parse a non-2xx body for `{message}` and throw an `Error` carrying it, so
views surface the registry's own wording. A body that is not JSON (a proxy error page, say)
falls back to the status text — never to a silent success.

**Views**

1. **Table** — columns: Name (`server_name`), URL, State, Health, Mapped channels, actions
   (Test, Registration, Mappings, Edit, Remove) plus an enable/disable toggle. `server_id` is
   shown in a copyable monospace cell or on the row's detail, because it is the recovery key and
   the argument every slash command takes. Empty state explains how to add the first server and
   notes that bridging a channel is done with `/matrix map` from inside the channel.
2. **Add** — `server_url`, `as_token`, `hs_token`, optional `username_prefix`; behind an
   "Advanced" disclosure, `server_id` (restore a previously removed server) and `server_name`
   (override discovery). Explain both: the name is discovered from the homeserver and should be
   overridden only when the public URL and the Matrix ID domain genuinely differ. On success,
   show the resolved `server_name` and offer to open the registration YAML immediately — the
   server does not work until that file is installed on the homeserver.
3. **Edit** — URL, tokens, prefix. Token inputs render empty, labelled "configured — leave
   blank to keep"; the client **omits** blank fields from the PATCH rather than sending `""`,
   which the API would reject anyway (§3.3). `server_name` sits behind an Advanced disclosure
   with the ghost-recognition warning and a confirm checkbox. Warnings returned by the API are
   displayed after a successful save, not swallowed.
4. **Remove** — shows the `server_id` as the recovery key and the exact restore command
   (`/matrix server add <url> <as_token> <hs_token> --server-id <id>`), because `Service.Remove`
   keeps every KV record (backend §3.1.1) and an admin who loses the ID loses the cheap path
   back. For `is_migrated` entries the action is disabled with the reason, and the dialog offers
   Disable instead.
5. **Enable/disable** — applies immediately; optimistic with rollback on failure. The tooltip
   states that disabling stops sync without touching mappings, ghosts or the remote
   (backend §3.11), since that is what makes it the safe alternative to Remove.
6. **Test** — the diagnostics checklist, with `skip` rendered distinctly from `fail`. Expect it
   to take seconds; show a spinner and do not let a second click stack requests.
7. **Registration** — YAML in a copy box, a download button using the response `filename`, the
   **"copy verbatim, do not append `/_matrix/app/v1`"** warning from backend §3.9, and the
   `room_list_publication_rules` snippet with the domain filled in from `server_name`.
8. **Mappings** — expandable per row, lazy-loaded on open, paginated, each row offering Unmap
   behind a confirm that names the channel and the room. A `channel_missing` row is labelled
   "channel deleted" and can still be unmapped.

**No auto-polling.** The list and health both cost a keyspace scan or a fanned-out network
probe. Refresh on mutation and on an explicit Refresh control only.

**Save-button interplay.** The console's Save applies to the settings in _other_ sections
(rate limiting). Everything in this section applies immediately over REST. The section subtitle
must say so, or admins will assume a newly added server did not take until they press Save —
and pressing Save will appear to be what worked.

### 3.9 Slash commands keep working

No command behaviour changes. After the §3.2 move, `command.Handler` calls
`c.plugin.Servers()` for the registry, health probe, mapping counts, identifier resolution,
registration YAML and diagnostics, and formats the results as it does today. Every existing
command test must pass **unmodified** — if one needs editing, the move changed behaviour and
that is a bug in this step.

`/matrix server` remains fully capable on its own; the console is an additional surface, not a
replacement, and a headless or CLI-driven install must not depend on the webapp. There is
deliberately no console equivalent of `/matrix migrate`.

---

## 4. Suggested build order

Each step should compile, pass `make check-style`, and land with its own tests and a commit
following conventional commits.

1. **The `server/servers` package** — §3.2, a pure move plus the typed errors of §3.4, with **no
   user-visible behaviour change**. Create the package, `Host`, and the `pluginHost` adapter;
   move the registry, the four `command.Handler` helpers and the registration YAML; delete
   `BridgeUtils.serverConfig`'s inline copy; shrink `PluginAccessor` and regenerate its mocks;
   port `servers_test.go` to the fake `Host`. Every existing command test passes unmodified.
   This is the largest single step and lands as its own commit — do not fold new behaviour into
   it, or a behavioural regression becomes indistinguishable from a move artefact in review.
2. **`Service.Update`** — §3.3, with its immutability rules and warnings.
3. **`SystemAdminRequired` + read endpoints** — the middleware, the autocomplete route moved
   onto it, `GET /servers`, `GET /servers/health`.
4. **Mutating endpoints** — `POST`, `PATCH`, `DELETE`, `PUT …/enabled`, with the §3.4 status
   mapping.
5. **Diagnostics, registration, mappings endpoints** — including pagination and unmap.
6. **`plugin.json` sections** — §3.6. Verify by hand that `rate_limiting_mode` still renders and
   still saves.
7. **Webapp foundation** — client module, types, registry typing, section shell, table, health
   fill-in, enable toggle.
8. **Modals** — add, edit, remove, test, registration.
9. **Mappings panel** — lazy load, pagination, unmap.
10. **Cleanup and docs** — delete the two inert components, README section for managing
    homeservers from the System Console (stating that mapping channels is still
    `/matrix map`, that changes apply without Save, and that the registration YAML must be
    copied verbatim), and note the console surface in `docs/local-development.md`.

---

## 5. Testing requirements

### 5.1 Go unit tests

- **The move is behaviour-preserving** — `server/servers_test.go` ports to
  `server/servers/service_test.go` against `servers.New(mockKV, testLogger, &fakeHost{})` with
  its assertions intact, and the existing `server/command` suite passes with **no** edits.
- **`Host` is honoured, not bypassed** — `Add` calls `RegisterRemote` then
  `RefreshAndBroadcast`; `Remove` calls `UnregisterRemote` then `RefreshAndBroadcast`;
  `SetEnabled` and `Update` call **only** `RefreshAndBroadcast` (asserting no
  register/unregister is what pins backend §3.11's "disabling is a pure flag flip"). A `Host`
  method returning an error is non-fatal in exactly the cases the current code treats as
  non-fatal, and fatal in none of them.
- **`pluginHost.UnregisterRemote` returns a nil `error`** when the platform returns a nil
  `*model.AppError` — the typed-nil trap, which would otherwise make every successful removal
  log a spurious warning.
- **The service never reaches the platform directly** — a `Service` built with a `Host` whose
  every method panics still satisfies `List`, `Get`, `CountMappedChannels`, `Mappings` and
  `ResolveIdentifier`. This is the structural guarantee that keeps the package a leaf.
- **Registration YAML** — the `url:` line is exactly `<SiteURL>/plugins/<plugin_id>` with **no**
  `_matrix/app/v1` anywhere in the output, over both a plain `SiteURL` and one with a trailing
  slash — carried over from backend §5.1, now covering the single shared implementation.
- **`Service.Update`** — each field updates in isolation; a nil field leaves the stored value
  untouched; `EventDomain`, `SiteURL`, `RemoteID` and `ServerID` are **unchanged** after a
  `ServerURL` change (assert the stored bytes, since this is the silent-orphaning regression);
  an endpoint colliding with another entry is rejected while re-submitting the entry's _own_
  endpoint succeeds; empty `ASToken`/`HSToken` rejected; empty `UsernamePrefix` resets to the
  default; a `ServerName` colliding with another entry is rejected and a successful change
  returns a warning; a migrated entry (`SiteURL == ""`) is editable; concurrent edits produce one
  winner.
- **Typed errors** — `errors.Is` matches each sentinel after the value has travelled out of the
  CAS callback through `SetAtomicWithRetries`; the message text still contains what the commands
  print today.
- **`Seed` is not `Add`** — seeding preserves a `SiteURL: ""` and a caller-supplied
  `EventDomain` verbatim, performs no name resolution and registers no remote; it is idempotent
  by endpoint. The existing migration tests cover the rest and must pass unmodified.
- **`SystemAdminRequired`** — a non-admin gets 403 with **no server data in the body** on every
  route including autocomplete; an unauthenticated request gets 401; an admin passes.
- **`GET /servers`** — zero servers yields `{"servers": []}` with 200, not an error; tokens are
  absent from the body (assert on the raw JSON, not the struct); `has_as_token`/`has_hs_token`
  reflect the stored values; `is_migrated` is true exactly for `SiteURL == ""`; a failing
  keyspace scan yields `mapped_channel_count: null` plus `counts_unavailable: true` rather than
  zeros or a 500.
- **`POST /servers`** — happy path returns 201 and the created view; duplicate endpoint, name
  conflict and duplicate `server_id` each return 409 with the registry's message; a malformed
  URL and a malformed body return 400; a `server_id` re-adoption is passed through verbatim.
- **`PATCH`** — partial updates apply; unknown `server_id` is 404; conflicts are 409; warnings
  are present in the body.
- **`DELETE`** — success returns the `server_id` and a recovery command containing it; a
  migrated entry is 409; an unknown ID is 404.
- **`PUT …/enabled`** — flips the flag both ways, 404 for unknown, and asserts
  `RegisterPluginForSharedChannels` / `InviteRemoteToChannel` are **not** called (backend §3.11).
- **`GET /servers/health`** — reports `disabled` for a disabled server without probing it,
  `unavailable` with no client, `timed out` for a probe exceeding a shortened
  `statusProbeDeadline`, and never `healthy` for any of those.
- **`POST …/test`** — check ordering and short-circuit: a nil client yields `skip` (not `fail`)
  for the connection and appservice checks; an unregistered server yields only a failed
  `registry` check with 404 semantics matching §3.4.
- **`GET …/registration`** — 404 for unknown; body contains both tokens (that is the point) and
  the handler emits **no** log line.
- **`GET …/mappings`** — filters to the requested server and ignores another server's entries in
  the same channel's array (build the fixture with a two-entry array so the filter is genuinely
  exercised); pagination bounds (`per_page` clamped to 200, `page` beyond the end yields an
  empty list with the true `total_count`); a deleted channel yields `channel_missing: true` and
  is still listed; a DM yields an empty `team_name`.
- **`DELETE …/mappings/{channel_id}`** — success; not-mapped is 404; a missing client is 503;
  a failure to clear Matrix room state does **not** remove the mapping (assert it is still
  readable afterwards).

### 5.2 Webapp unit tests

`@testing-library/react` is **not** installed — only `@testing-library/jest-dom` is. Add
`@testing-library/react@12` (the React 17 line) as a devDependency rather than writing new
tests in enzyme.

- **Client module** — builds URLs from `Client4.getPluginRoute(manifest.id)`; sends
  `Client4.getOptions`-derived headers; throws an `Error` carrying `message` from a non-2xx JSON
  body, and falls back to status text for a non-JSON body.
- **Table** — renders name/URL/state/health/count; "unavailable" (not 0) when
  `counts_unavailable`; Remove disabled with an explanation for `is_migrated`; the
  enable toggle rolls back its optimistic state when the request fails.
- **Add form** — omits empty optional fields from the request; surfaces a 409 message verbatim.
- **Edit form** — blank token inputs are **omitted** from the PATCH body (the regression that
  would otherwise clear a token); the `server_name` change is blocked until the confirm is
  checked; returned warnings render.
- **Remove dialog** — contains the `server_id` and the `--server-id` restore command.
- **Test modal** — renders `skip` distinctly from `fail`.
- **Registration modal** — content is rendered verbatim and includes no `_matrix/app/v1`; the
  `room_list_publication_rules` snippet interpolates `server_name`.
- **Mappings panel** — does not fetch until opened; paginates; a `channel_missing` row is
  labelled and still unmappable.
- **Section registration** — `index.tsx` registers `matrix_servers` and no longer registers
  `registration_download` or `homeserver_config`.

### 5.3 Manual verification (nothing above covers these)

- After the §3.6 restructure, **`rate_limiting_mode` still appears in the System Console and
  still saves.** This is the one change that fails silently and invisibly.
- The custom section renders inside Plugins → Mattermost bridge for Matrix, and shows the
  built-in "enable the plugin" banner when the plugin is disabled.
- **Section order on the page is: Enable toggle → Matrix homeservers → Advanced** (§3.6). The
  first is injected by the console and the other two follow the `sections` array; a future
  section must not be inserted above the servers one.
- A full add → install registration on the homeserver → test → `/matrix map` → mappings panel
  shows the channel → unmap round trip against the dev Synapse (`docs/local-development.md`).
- Token rotation: `PATCH` a new `as_token`/`hs_token`, confirm inbound and outbound both keep
  working without a plugin restart (this is what `Host.RefreshAndBroadcast` buys).

---

## 6. Deliberately deferred

- Mapping a channel from the console (needs a channel picker and a channel-context-free rewrite
  of `mapChannelCore` — §2).
- Room creation (`/matrix create`) and `/matrix migrate` equivalents.
- i18n of the new components.
- Reclaiming a removed server's KV space, and surfacing orphaned `ServerID`s in the UI (backend
  §3.1.1 describes how they are derivable if it ever becomes necessary).
- **Moving the rest of `*Plugin` into packages.** `server/servers` establishes the pattern —
  a leaf package plus a narrow `Host` for runtime effects — and the obvious next candidates are
  the bridges, channel mapping and migrations. Do **not** do them in this PR; the point of
  stopping here is that step 1 stays a reviewable pure move. If this shape works, the count of
  `*Plugin` methods is the metric to watch.
- Per-channel bridging UI outside the System Console (e.g. a channel-settings tab), which is
  where a non-admin-facing mapping flow would eventually belong.
