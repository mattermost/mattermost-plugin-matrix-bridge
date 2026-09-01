# Multi-Matrix-server support — backend implementation plan

This document is a self-contained implementation brief. Start from `master` on a fresh
branch and implement everything below.

## 1. Goal

The plugin currently bridges exactly **one** Matrix homeserver, configured through flat
System Console fields (`matrix_server_url`, `matrix_server_name`, `matrix_as_token`,
`matrix_hs_token`). Replace that with a **registry of N homeservers stored in the KV
store**, managed entirely through `/matrix server …` slash commands, with per-server
clients, per-server KV namespaces, per-server shared-channels remotes, and routing in
both directions.

Backend only. Do not build admin-console UI.

## 2. Scope and constraints

**In scope**

- KV-backed server registry as the _sole_ source of truth for homeserver connection settings.
- Removal of the flat per-server System Console fields. **No upgrade path**: the new
  layout is not backward compatible, a one-time purge clears the old records, and
  operators re-register their homeservers and re-map their channels (§3.8).
- **Removal of the global `enable_sync` toggle.** Sync is enabled per server
  (`ServerConfig.Enabled`, `/matrix server enable|disable`). A global off-switch is
  redundant with deactivating the plugin, and with N servers it is actively wrong — it
  cannot express "stop bridging server B while A keeps working", which is the operation an
  admin actually wants (§3.11).
- `/matrix server` command group for management; `/matrix status` reports every server.
- Inbound routing (Matrix → Mattermost) by `hs_token`.
- Outbound routing (Mattermost → Matrix) by the invoking `*model.RemoteCluster`.
- **Exactly one Matrix server per Mattermost channel**, enforced at every write path.
- Unit tests for all new/changed logic; integration tests covering single-server and
  multi-server operation.

**Out of scope**

- Webapp changes. `webapp/src/index.tsx` registers custom admin-console settings for
  `registration_download` and `homeserver_config`; those keys leave `plugin.json`, so the
  components become inert. Leave the webapp untouched.
- Relaying Matrix-originated content between homeservers (impossible under one-server-per-channel).
- Any System Console UI for per-server enable/disable beyond what the slash commands expose.

**Platform requirement**

`plugin.json` `min_server_version` must move to **`11.8.0`**:
`RegisterPluginOpts.SiteURL` (multiple remotes per plugin) and
`UnregisterPluginRemoteForSharedChannels` (11.7) are both required.

---

## 3. Target design

### 3.1 The registry

New value schema in `server/store/kvstore/schema.go`, stored as a JSON array under the
KV key `servers` (`kvstore.KeyServersConfig`):

```go
type ServerConfig struct {
    ServerID       string `json:"server_id"`        // model.NewId() — random, opaque
    ServerURL      string `json:"server_url"`       // homeserver base URL
    Endpoint       string `json:"endpoint"`         // normalized host:port — the uniqueness key
    ServerName     string `json:"server_name"`      // Matrix ID domain; NEVER empty, unique (§3.1.2)
    EventDomain    string `json:"event_domain"`     // sanitized, immutable; keys matrix_event_id_<EventDomain> (§3.6)
    ASToken        string `json:"as_token"`
    HSToken        string `json:"hs_token"`
    UsernamePrefix string `json:"username_prefix"`
    Enabled        bool   `json:"enabled"`
    RemoteID       string `json:"remote_id"`          // shared-channels remote, one per server
    SiteURL        string `json:"site_url,omitempty"` // value passed to RegisterPluginForSharedChannels
}
```

Both `ServerName` and `EventDomain` are resolved **once, at `server add`** and never
recomputed. They answer different questions and must not share a derivation:

| Field         | Answers                                               | Must be                                   |
| ------------- | ----------------------------------------------------- | ----------------------------------------- |
| `ServerName`  | which domain appears in this server's Matrix IDs      | correct — ghost recognition depends on it |
| `EventDomain` | which post-property key holds this server's event IDs | unique and stable — nothing else          |

**Identity: random ID, endpoint-based collision detection, re-adoption by supplying the ID.**

- `ServerID` is `model.NewId()`. Do **not** derive it from the URL.
- Uniqueness is enforced on a normalized endpoint. Write
  `normalizeServerEndpoint(rawURL string) (string, error)` that parses the URL,
  lowercases the host, fills in the default port from the scheme (`http`→80,
  `https`→443), and returns `"host:port"`. Two registry entries may never share an
  endpoint.
- `/matrix server add` with an endpoint that is already **live in the registry** is
  **rejected** with a message pointing at `/matrix server remove <server_id>` (no silent
  in-place replacement — that was the old branch's behaviour and it depended on ID
  determinism).
- `ServerName` may never be empty, and no two entries may share one (§3.1.2).
- Because every record is addressed as `<prefix><serverID>_<id>` (§3.2), **the `ServerID`
  is the only thing needed to recover a removed server's records** — re-create the entry
  with the same ID and every key resolves again. `remove` therefore deletes nothing but the
  registry entry, and re-adoption is just `add --server-id <prior_id>` (§3.1.1). No
  side-table, tombstone or ledger is required: the ID is already embedded in the keys of the
  records it addresses. Reclaiming a removed server's space is deliberately deferred until
  someone asks for it.

### 3.1.1 Removal and re-adoption

`remove` deletes the registry entry and unregisters the shared-channels remote. It deletes
**no** KV records. Every namespaced key stays exactly where it was, addressed by a
`ServerID` that the command prints in its response as the recovery key.

Re-adoption is supplying that ID back:

```go
func (p *Plugin) AddServer(serverURL, asToken, hsToken, usernamePrefix, serverID string) (string, error)
```

Empty `serverID` mints `model.NewId()`. A non-empty one must satisfy `model.IsValidId` and
must not collide with a live entry's `ServerID` (rejected — that is the duplicate-endpoint
case in disguise, or a typo). It is **not** checked against any record of previously-used
IDs, because no such record is kept and none is needed: if records exist under that prefix
they resolve, and if they do not the entry is simply new.

Three things must match for re-adoption to fully restore a server, and only the first is
enforced:

1. **`ServerID`** — restores every `<prefix><serverID>_<id>` key and the
   `channel_mapping_` entries naming that server. This is the mechanism.
2. **`EventDomain`** — derived from the endpoint (§3.6), so re-adding at the same URL
   reproduces it automatically. Re-adding the _same_ homeserver at a _different_ endpoint
   does not, and previously-synced posts' event IDs become unreachable. Warn when an entry
   is created with a `serverID` whose surviving `matrix_event_post_` records exist but whose
   computed `EventDomain` differs from any in the current registry — this is the one case
   the operator cannot discover any other way.
3. **`SiteURL`** — always `"https://<endpoint>"`; see §3.4.

**Finding an orphaned `ServerID` without the recovery key.** If the operator lost it, it is
still derivable: scan the namespaced prefixes with `ListAllKeysByPrefix` (§3.8),
collect the distinct `serverID` segments, and subtract the IDs present in `servers`. The
remainder is exactly the set of orphaned identities. No stored record is needed for this,
which is why none is kept. Expose it only if it proves necessary — the `remove` response is
the primary channel.

**Stale mappings must be tolerated.** Because `remove` leaves `channel_mapping_` records in
place, a channel can hold an entry for a `ServerID` that is no longer registered.
`setChannelMapping`'s `maxServersPerChannel` check therefore counts **live** entries only —
otherwise a channel left pointing at a removed server hits an `ErrChannelAlreadyMapped`
dead end naming a server that no longer exists — and drops the stale entry in
the same CAS write. Read the live registry **once before** entering the CAS callback and
close over the result: the callback may run several times and must stay a pure function of
the mapping slice (§3.1). Re-adopting the prior `ServerID` makes such entries live again,
which is the point — the channel↔room links come back with no re-mapping.

### 3.1.2 Resolving `ServerName` — never empty, never shared

`ServerName` is the domain that appears in this homeserver's Matrix IDs. Ghost recognition
depends on it (`isGhostUser`, §3.5), so a wrong value silently breaks inbound routing and a
shared value makes two servers' ghosts indistinguishable.

One function resolves it:

```go
// server/servers.go
func (p *Plugin) resolveServerName(serverURL, configuredName string) (string, error)
```

Resolution order, first success wins:

1. **`configuredName`** — the `--server-name` override on `server add`.
2. **`GET <serverURL>/_matrix/key/v2/server`** → the `server_name` field of the response.
   Unauthenticated, and an explicit field rather than a domain parsed out of an MXID:

    ```json
    { "server_name": "example.org", "verify_keys": { … }, "valid_until_ts": 1052262000000 }
    ```

    This is a **federation** endpoint, so it is only reachable at `serverURL` when the
    homeserver serves the `client` and `federation` resources on the same listener. The dev
    Synapse does (`docker/synapse_config.yaml:6-11`), so the integration suites can rely on
    it; a production homeserver behind a proxy that routes only `/_matrix/client`, or one with
    federation disabled, will 404 — hence step 3. Do not verify the response signature: the
    key needed to check it is what the response carries, so it is self-referential without an
    out-of-band key, and the value is already trusted at the same level as the TLS connection
    to `serverURL`.

3. **`Hostname(serverURL)`** — always succeeds for a parseable URL, so resolution never
   fails and `ServerName` is never empty. We should warn in the logs if we end up at this,
   since the above endpoint should always be reachable, extracting the correct server name
   set by the server.

Then normalize with `matrix.NormalizeServerName` and validate:

- non-empty after normalization (defensive — step 3 guarantees it);
- **not equal to any existing entry's `ServerName`**, else reject the add, naming the
  conflicting `server_id`. Two entries sharing a server name would mint ghosts with identical
  MXIDs (`@<prefix>_<userid>:<name>`), so `isGhostUser` could not attribute them and inbound
  events from one server would be treated as the other's. Rejecting is the correct outcome:
  such a pair genuinely cannot be bridged simultaneously.

**Uniqueness, not non-emptiness, is what prevents collisions.** `NormalizeServerName` strips
the port (`server_discovery.go:166-169`), so two homeservers differing only by port —
`localhost:8008` and `localhost:8009`, both legal Matrix server names — collapse to
`localhost`. Step 2 avoids this whenever it is reachable, since it returns each homeserver's
real name; when only step 3 applies, the duplicate check turns the collision into an explicit
rejection at `add` time instead of silent ghost cross-attribution. It is also why the
multi-server integration suites must use distinct port-less server names (§5.2). Do **not**
change `NormalizeServerName` to preserve ports as part of this work: it feeds ghost MXID
construction, so changing it would rewrite the identity of every existing install's ghost
users.

`.well-known/matrix/server` is deliberately **not** in this chain. The existing
`ServerDiscovery.DiscoverServerName` returns the hostname it queried, not `m.server`
(`server_discovery.go:132`) — identical to step 3 — so it cannot contribute a value step 3
does not already produce. It stays where it is for the client's own use; nothing here calls
it.

All registry mutations go through one helper:

```go
func (p *Plugin) mutateServers(mutator func([]kvstore.ServerConfig) ([]kvstore.ServerConfig, error)) error
```

implemented with `kvstore.SetAtomicWithRetries` (compare-and-set). The mutator must be a
pure function of the slice — it may run more than once, so no network or plugin-API calls
inside it.

`KVStore` (`server/store/kvstore/kvstore.go`) needs a new method; add it to the
interface, implement it in `startertemplate.go` by delegating to
`client.KV.SetAtomicWithRetries`, and regenerate `server/mocks/mock_kvstore.go`:

```go
SetAtomicWithRetries(key string, valueFunc func(oldValue []byte) (newValue []byte, err error)) error
```

### 3.2 Per-server KV namespacing

These prefixes gain a `serverID` dimension — `<prefix>_<serverID>_<id>`:

`matrix_user_`, `mattermost_user_`, `ghost_user_`, `ghost_room_`, `matrix_event_post_`,
`matrix_reaction_`, `room_mapping_`.

Add key builders in `server/store/kvstore/constants.go` taking `serverID` as the first
argument (`BuildMatrixUserKey`, `BuildMattermostUserKey`, `BuildGhostUserKey`,
`BuildGhostRoomKey`, `BuildMatrixEventPostKey`, `BuildMatrixReactionKey`,
`BuildRoomMappingKey`). `BuildChannelMappingKey(channelID)` stays server-agnostic — the
server lives in the _value_.

Bump `kvstore.CurrentKVStoreVersion` to `3`.

### 3.3 Channel ↔ room mapping: list shape now, one server enforced by policy

**The stored shape must already be multi-server.** One server per channel is a _policy_
limit for this release, not a schema limit — the value is a JSON **array** so lifting the
limit later is a one-line policy change and needs no KV migration.

```go
// channel_mapping_<channelID> → JSON array, currently always length 0 or 1
type ChannelServerMapping struct {
    ServerID string `json:"server_id"`
    RoomID   string `json:"room_id"` // room ID or alias on that server
}
```

Helpers in `kvstore` (all list-shaped, none assuming length 1):

```go
func ParseChannelServerMappings(data []byte) ([]ChannelServerMapping, error) // empty → (nil, nil); corrupt → error
func MarshalChannelServerMappings(m []ChannelServerMapping) ([]byte, error)
func BuildSingleChannelMapping(serverID, roomID string) ([]byte, error)
func UpsertChannelServerMapping(m []ChannelServerMapping, serverID, roomID string) []ChannelServerMapping
func RemoveServerFromChannelMapping(kv KVStore, key, serverID string) (remaining []ChannelServerMapping, err error)
func RoomIDForServer(m []ChannelServerMapping, serverID string) string // "" if not mapped there
func MappedServerIDs(m []ChannelServerMapping) []string
```

**Reads never index `[0]`.** Look a room up with `RoomIDForServer(mappings, serverID)`;
enumerate with `MappedServerIDs`. `BridgeUtils.GetMatrixRoomID` returns _this bridge's
server's_ room, and `("", nil)` when the channel is mapped only to another server — an
absent mapping is not an error, but a corrupt value is.

**One write choke point.** Every mapping write goes through a single function so the policy
lives in exactly one place:

```go
// maxServersPerChannel is the number of Matrix servers a single channel may be bridged to.
// The stored value and every helper are already list-shaped; raise this (or drop the check)
// to allow a channel to be mapped to several homeservers.
const maxServersPerChannel = 1

var ErrChannelAlreadyMapped = errors.New("channel is already mapped to another Matrix server")

// setChannelMapping upserts serverID's entry via compare-and-set. The policy check runs
// INSIDE the CAS callback so two concurrent maps on one channel cannot both win.
func (p *Plugin) setChannelMapping(channelID, serverID, roomID string) ([]kvstore.ChannelServerMapping, error)
```

Callers: `/matrix map`, `/matrix server map`, `/matrix create`, and
`BridgeUtils.setChannelRoomMapping` (inbound Matrix-initiated DM rooms, outbound DM room
auto-creation). The bridge path needs the same choke point, so either expose it on the
`ConfigGetter`-style interface `BridgeUtils` already holds or move the helper into a small
shared type both can call — do **not** duplicate the CAS + policy logic.

Semantics:

- Channel unmapped → append the entry.
- Already mapped to **this** server → overwrite the room, and delete the stale
  `room_mapping_<serverID>_…` reverse keys (both the alias form and the resolved-room-ID
  form) before writing the new ones.
- Already mapped to `maxServersPerChannel` **other** servers → return
  `ErrChannelAlreadyMapped`. Commands surface it as an ephemeral error naming the
  currently-mapped server and pointing at `/matrix server unmap`. The inbound/outbound DM
  paths log it and skip — never silently write a second entry.
- Entries for servers no longer in the registry are **stale** and do not count toward
  `maxServersPerChannel`; they are dropped in the same CAS write (§3.1.1). Only live
  entries can block a map.
- Removing the last entry **deletes the key** rather than storing `[]`, so "unmapped"
  has exactly one on-disk representation and readers never have to treat an empty array as
  a special case.

Everything downstream of the write is already per-server and needs no change when the limit
is lifted: `room_mapping_<serverID>_<room>` keys, ghost/event/reaction namespaces, the
`matrix_event_id_<domain>` post property, the per-server trackers, and rc-based outbound
routing (§3.6). The one place that will need revisiting is
`Plugin.UserHasJoinedChannel`, which has no `RemoteCluster` and so loops over
`MappedServerIDs` — write that loop now, correct for N, rather than hardcoding one server.

### 3.4 Per-server clients and shared-channels remotes

`Plugin` state (`server/plugin.go`) replaces the singleton `matrixClient`/`remoteID` with:

```go
matrixClients    map[string]*matrix.Client // serverID → client
remoteToServerID map[string]string         // shared-channels remoteID → serverID
ownRemoteIDs     map[string]struct{}       // loop prevention across all our remotes
matrixClientsLock   sync.RWMutex           // guards the three maps (swap only)
initMatrixClientsMu sync.Mutex             // serializes read-compute-swap in initMatrixClients
```

`initMatrixClients()` reads the registry, builds one `matrix.Client` per entry (each
carrying its own `RemoteID` so created posts/users attribute correctly), rebuilds the two
index maps, and swaps all three under `matrixClientsLock`. It returns an error on registry
read failure — never leave the maps silently stale — and no-ops when `p.kvstore == nil`
(`OnConfigurationChange` can fire before `OnActivate` initializes the store).

**One shared-channels remote per Matrix server.** This is the backbone of the whole design,
so it is spelled out end to end.

`RegisterPluginForSharedChannels` is keyed by `RegisterPluginOpts.SiteURL` (unique across
all remote clusters, enforced by a DB unique index on `(SiteURL, RemoteTeamId)`) and is
idempotent: re-registering the same `SiteURL` returns the same `remoteID` and preserves
sync cursors. A plugin registers N remotes by calling it N times with N distinct
`SiteURL`s. So:

`registerForSharedChannels()` calls it **once per registry entry**, collects the returned
remote IDs in a local map, and merges them into the registry in a _single_ `mutateServers`
call. The API calls must happen outside the CAS callback — a CAS retry would re-issue real
network calls. Merge against whatever the registry looks like at write time (not the
snapshot read before the calls), so a concurrent `AddServer`/`RemoveServer` is not
clobbered; a concurrently-removed server is simply absent and its remote ID is dropped.

`SiteURL` rules:

- Derived from the **normalized endpoint** (`"https://" + endpoint`) so servers that share
  a hostname but differ by port get distinct remotes — this matters in dev and tests
  (`localhost:8008` vs `localhost:8009`).
- Always non-empty: every entry comes from `AddServer`. Never let `AddServer` re-key an
  existing entry's `SiteURL` — the remote it resolves to is what carries that server's sync
  cursors.

Lifecycle:

| When              | What happens to remotes                                                                                                                                                                                                                                                                                                 |
| ----------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `OnActivate`      | migrations first (the purge must land before anything reads KV), then `registerForSharedChannels()` for every entry, then `initMatrixClients()` — clients must be built _after_ registration so each carries its own `RemoteID`.                                                                                                              |
| `server add`      | register immediately, so the new server has its own remote (and correct loop attribution) without waiting for a restart. Failure is non-fatal — warn and continue; the next activation retries.                                                                                                                         |
| `server remove`   | `UnregisterPluginRemoteForSharedChannels(entry.RemoteID)`. Non-fatal: the registry write already happened and is the source of truth. Re-adopting the `ServerID` later re-registers a fresh remote (`SiteURL` is derived from the endpoint, so the platform hands back the same `remoteID` and preserves sync cursors). |
| `map` a channel   | `ShareChannel` once, then `InviteRemoteToChannel(channelID, remoteIDForServer(serverID), …)` — invite **that server's** remote.                                                                                                                                                                                         |
| `unmap` a channel | `UninviteRemoteFromChannel(channelID, remoteIDForServer(serverID))`. Because each server has its own invitation, this never disturbs another server's.                                                                                                                                                                  |

Where the per-server remote ID is consumed:

1. `matrix.NewClientWithRateLimit(..., server.RemoteID, ...)` — posts/users the client
   creates attribute to the right remote.
2. `BridgeUtilsConfig.RemoteID` on both bridge directions, including the **inbound** one, so
   a message arriving from server B is written to Mattermost as B's remote.
3. `remoteToServerID` — reverse index, the basis of rc-based outbound routing (§3.6) and
   `OnSharedChannelsPing`.
4. `ownRemoteIDs` — loop prevention must reject **every** one of our remotes, not just one:
   `isOwnRemoteID(post.GetRemoteID())`, same for reactions and `fi.RemoteId`.
5. Distinguishing a Matrix-originated user's _own_ homeserver from the others, so ghosts and
   invites are not relayed across servers:
   `serverIDForRemoteID(user.GetRemoteID()) == serverID`.
6. `InviteRemoteToChannel` / `UninviteRemoteFromChannel` on map/unmap.
7. `UnregisterPluginRemoteForSharedChannels` on remove.

Two consequences worth internalizing:

- **The platform invokes the outbound hooks once per invited remote.** With one remote per
  server, a channel shared with two servers delivers the same `SyncMsg` twice, once per
  `rc`. That is exactly why §3.6 resolves a single server from `rc` instead of fanning out —
  fan-out would process every event N times. Today `maxServersPerChannel = 1` means one
  invitation per channel and one invocation, but the routing is already written for N.
- **Remote IDs are per-server but the maps holding them are per-node**, so any registry
  mutation must broadcast (§3.7).

Bridges are built **on demand per server**, not held as fields:

```go
func (p *Plugin) newMatrixToMattermostBridge(serverID string) (*MatrixToMattermostBridge, error)
func (p *Plugin) newMattermostToMatrixBridge(serverID string) (*MattermostToMatrixBridge, error)
```

Both return an error (never a bridge with a nil `MatrixClient`) when this node has no
client for that server. `BridgeUtilsConfig` gains a `ServerID` field; `BridgeUtils` grows
`serverID`, `matrixUsernamePrefix()` and `serverDomain()` helpers that read the registry
live.

### 3.5 Inbound routing (Matrix → Mattermost)

`server/api.go` — `MatrixAuthorizationRequired` middleware:

- Compare the presented `Authorization` header against **every** server's `HSToken` with
  `subtle.ConstantTimeCompare`. Skip entries with an empty `HSToken` (so an empty
  presented token can never match). Scan the whole list without an early return.
- Reject when the matched server has `Enabled == false`.
- Inject the resolved `serverID` into the request context (unexported `contextKey`).

`server/matrix_webhook.go`:

- Read the `serverID` from the context; 401 if absent.
- If `p.getMatrixClient(serverID) == nil` (this node's map lags the cluster-shared
  registry), respond **503 with `Retry-After`** and do **not** record the transaction as
  processed — the AS spec has the homeserver retry the same `txnId`, so the events survive
  and a retry can land on an up-to-date node.
- The transaction is recorded in the processed-transaction dedupe map **only after every
  event in it has been processed successfully**. If `processMatrixEvent` returns an error
  for any event, stop iterating immediately and respond **503 with `Retry-After`**, the same
  as the missing-client path, instead of recording the transaction and returning 200. Marking
  it processed up front (or continuing past a failed event and still returning 200) would
  make the failure invisible to the homeserver — it would never retry, and if it did, the
  txnId would already be in the dedupe map and get dropped as a duplicate.
- The processed-transaction dedupe map must be keyed by `struct{serverID, txnID string}`:
  Matrix transaction IDs are only unique per homeserver.
- Thread `serverID` through `processMatrixEvent`, `getChannelIDFromMatrixRoom`,
  `isGhostUser`, `handleMatrixInitiatedDM`, `handleMatrixMemberDM`,
  `createDMChannelForGhostUser`.

**Autocomplete endpoint.** `GET /api/v1/autocomplete/servers` on the existing
`apiRouter` (behind `MattermostAuthorizationRequired`) serves the dynamic server list for
§3.9's autocomplete. Two requirements:

- That middleware only proves the caller is logged in, so the handler **additionally**
  requires `model.PermissionManageSystem` — server IDs are admin-only information and this
  route is reachable by any authenticated user.
- Serve from the per-node `serverConfigs` cache, not KV, and return an **empty array rather
  than an error** whenever there is nothing to suggest (including before the cache is first
  built). Autocomplete then shows no suggestions instead of erroring while someone types.

Items are `{Item: ServerID, Hint: ServerName, HelpText: "<url> (enabled|disabled)"}`.

`isGhostUser(serverID, userID)` resolves the domain from the registry entry's
**`ServerName`**, with **no URL-host fallback** — `ServerName` is guaranteed non-empty and
unique by §3.1.2, so a fallback would only ever mask a bug. Ghosts are created with the
Matrix server name, which differs from the connection host under `.well-known` delegation
and in tests.

### 3.6 Outbound routing (Mattermost → Matrix) — use the RemoteCluster, never iterate

One remote is registered per homeserver, and the platform invokes the outbound hooks
**once per invited remote**. So every hook resolves exactly one target server from `rc`
and does no fan-out:

```go
// server/plugin.go
func (p *Plugin) serverIDForSyncMsg(channelID string, rc *model.RemoteCluster) (serverID string, shouldSync bool, err error)
```

The `error` return distinguishes an intentional no-op from an operational failure (KV read/parse
failure, registry read failure, `GetChannel` failure). Both outbound hooks must propagate a
non-nil `err` rather than fold it into `shouldSync == false`: Mattermost only retains the
shared-channels sync cursor and retries the batch when the hook returns an error, so a swallowed
operational failure silently and permanently drops the posts/reactions/files in that batch. The
inbound webhook path (§3.5) applies the same principle with its 503/`Retry-After` response for
the analogous node-cache-lag case.

1. `rc == nil || rc.RemoteId == ""` → log and skip (defensive; should not happen in production);
   not an error.
2. `serverIDForRemoteID(rc.RemoteId)` unresolved → this node's `remoteToServerID` cache may lag a
   registry mutation made on another node that hasn't reached this node's cluster event handler
   yet, so refresh the client caches once (`initMatrixClients`) and retry the lookup before giving
   up. A refresh failure is an operational failure. Still unresolved after the refresh → log and
   skip (genuinely not one of our remotes); not an error.
3. `serverConfigForRouting(serverID)` failure → operational failure (registry read failure).
4. The resolved server has `Enabled == false` → skip (§3.11). This replaces the global
   `EnableSync` gate the hooks used to check, and is the only thing stopping outbound traffic
   for a disabled server — its remote stays registered and invited, so the hooks keep firing.
5. Read `channel_mapping_<channelID>` and check `rc`'s server against the list — **membership,
   not identity**, so this keeps working unchanged when a channel may hold N entries. A KV
   read/parse failure here is an operational failure. Otherwise:
    - `RoomIDForServer(mappings, serverID) != ""` → return `serverID`;
    - the list is non-empty but has no entry for `serverID` → skip (the remote is still
      invited but its server is no longer mapped — do not relay its traffic elsewhere);
    - list empty → `isChannelDirect(channelID)` failure is an operational failure; otherwise
      the channel is a DM → return `rc`'s server so the DM room can be auto-created on it;
      not a DM → skip.

Apply in `server/hooks.go`:

- `OnSharedChannelsSyncMsg` / `OnSharedChannelsAttachmentSyncMsg` both check `err` from
  `serverIDForSyncMsg` first and return it immediately (log-and-return, same as every other
  error path in these hooks) before checking `shouldSync`.
- `OnSharedChannelsSyncMsg` — one bridge, no loop over servers. Loop prevention uses
  `isOwnRemoteID(post/reaction.GetRemoteID())` across all our remotes. Matrix-originated
  users are only re-invited when `serverIDForRemoteID(user.GetRemoteID()) == serverID`.
- `OnSharedChannelsAttachmentSyncMsg` / `deleteFileFromMatrix` — single target server;
  the `mxc://` URI is only valid on the server it was uploaded to.
- `OnSharedChannelsPing` — resolve `rc.RemoteId` to its server and health-check **only**
  that server. Healthy-but-idle (`true`) when no servers are registered or the pinged server
  is disabled.
- `OnSharedChannelsProfileImageSyncMsg` — `rc` is available; use it, then update the ghost
  on that server only (do not sweep every server for a ghost).
- `UserHasJoinedChannel` (`server/plugin.go`) — no `rc` available; loop over
  `MappedServerIDs(mappings)` and act on each (one entry today, N later), skipping unmapped
  channels **and disabled servers** (§3.11). This is the one legitimate loop: it iterates the
  channel's mappings, not the server registry.

Per-server in-memory trackers: `PostTracker` and `PendingFileTracker` keys must include
`serverID` (`trackerKey(serverID, postID)`), and the `FileTracker` /
`PostTrackerInterface` method signatures gain a leading `serverID` parameter.

Per-server post property key: `matrix_event_id_<EventDomain>`, read straight off the
registry entry — **never recomputed at use time.** `EventDomain` is resolved once at
`server add` from the **normalized endpoint** (`host:port`, sanitized `.`/`:` → `_`), and
persisted (§3.1).

The endpoint is the right input and `ServerName` is not, for two reasons. The endpoint is
the only field the registry enforces unique _and_ it distinguishes `localhost:8008` from
`localhost:8009`, whereas `NormalizeServerName` strips the port so those two collapse to one
key (§3.1.2). And recomputing from `ServerName` at use time is unsafe even when unique: the
value can legitimately change (a `--server-name` correction, or a `/_matrix/key/v2/server`
probe that starts succeeding once federation is routed), and the
moment it does, every property written under the old key becomes unreachable — edits and
deletes of those posts silently stop working, because the code sees no existing event ID and
treats an edit as a new message. Persisting the resolved value makes that impossible.

**Bridge filtering aliases.** Rooms also get a `#mattermost-bridge-<name>:<ServerName>`
alias, built by one shared helper (`matrix.CreateBridgeAlias`) rather than concatenated at
each site — `CreateRoom` and the manual `map` path both need it, and the prefix has to stay
inside the `aliases` namespace regex the registration declares (§3.9) or the homeserver will
not route those rooms to the bridge. Note the two paths still sanitize the localpart
differently: `CreateRoom` strips characters invalid in an alias localpart and falls back when
nothing survives, while the `map` path only lowercases and replaces spaces/underscores, so a
punctuated channel name can still produce an alias the homeserver rejects. Best-effort in
both cases (a failure is a warning, not an error), so unifying that is a follow-up.

**Pre-upgrade posts keep a key nothing resolves.** Earlier builds computed this property
as `matrix_event_id_<sanitized URL hostname>` (portless), and every post such an install
synced still carries it. Post properties live on posts, not in KV, so the §3.8 purge cannot
touch them — rewriting them would mean an `UpdatePost` per synced post, bumping `UpdateAt`
and tripping the very edit-detection the `postTracker` exists to suppress
(`sync_to_matrix.go:315-330`). Those posts are simply left unlinked, which is the accepted
cost of the clean break: they stay in place, but edits and deletions no longer propagate.
`EventDomain` remains a stored rather than derived field so that a server re-added at a
different endpoint is detectable (§3.1.1).

### 3.7 Cluster invalidation

The registry is cluster-shared KV, but `matrixClients` / `remoteToServerID` /
`ownRemoteIDs` are **per-node**. Any runtime mutation must broadcast:

```go
const clusterEventServersChanged = "servers_config_changed"

func (p *Plugin) refreshServersAndBroadcast(reason string) error // initMatrixClients + PublishPluginClusterEvent (reliable)
func (p *Plugin) OnPluginClusterEvent(_ *plugin.Context, ev model.PluginClusterEvent) // → initMatrixClients
```

`AddServer` and `RemoveServer` call `refreshServersAndBroadcast`, never a bare
`initMatrixClients`. A failed publish is non-fatal (single-node installs have no cluster) —
warn and continue. Activation-time work is exempt: every node runs `OnActivate` itself.

### 3.8 Migration (purge to KV v1)

The multi-server layout **is** KV version 1. It is not backward compatible with any earlier
layout — every per-server record gains a `<serverID>_` key segment and every
`channel_mapping_` value becomes a JSON array — and no attempt is made to translate the old
layout into it. `server/migrations.go` holds exactly one step:

1. Read `kvstore.KeySchemaVersion` (`kv_schema_version`). Deliberately **not** the earlier
   builds' `kv_store_version`, which those builds left at `2`: a version-1 baseline reading
   that marker would conclude the store is already ahead of current and skip the purge. With
   a distinct key, an unpurged install is indistinguishable from a fresh one — which is
   exactly how it should be treated. A missing marker means 0; an unparseable one is a hard
   error rather than a silent 0, which would purge a healthy store.
2. If the marker is already ≥ `CurrentKVStoreVersion`, return.
3. `purgeStaleRecords()` — `ListAllKeysByPrefix` over `kvstore.BridgeDataPrefixes` and
   delete every key, then delete the `kv_store_version` marker. `BridgeDataPrefixes` covers
   the seven namespaced prefixes, `channel_mapping_`, and the two pre-unification DM
   prefixes (`dm_mapping_`, `matrix_dm_mapping_`) that no live code reads. The server
   registry (`KeyServersConfig`) is **not** in that list and is never deleted.
4. Stamp the marker — only after the purge fully succeeded.

**Why this is safe to run unconditionally.** A fresh install runs the purge too and finds
nothing, so there is no shape check anywhere that has to tell an old record apart from a
current one — the entire `isNamespacedKey` / `classifyChannelMappingForV3` problem class
disappears. It can only ever run before the marker exists, which is before `OnActivate` has
registered the slash command, so no server can have been added and no channel mapped yet.

**Fail closed.** Any error — scan, delete, or the marker read — returns before the marker is
stamped, so the purge retries on the next activation. A marker claiming the store is current
while unreadable records survive is the one state no later run would correct.

There is no `/matrix migrate` command: with a single unconditional step and no version
ladder, there is nothing for an operator to re-run.

**Key enumeration must page the raw keyspace.** `pluginapi`'s `ListKeysWithPrefix` fetches
an unfiltered page and filters client-side, so a short filtered batch means "few matches in
this raw page", not "no more pages" — paging on it silently stops after page 0. Add and use:

```go
// server/store/kvstore/listall.go
func ListAllKeysWithPrefix(store KVStore, prefix string, batchSize int) ([]string, error)
func ListAllKeysByPrefix(store KVStore, batchSize int, prefixes ...string) (map[string][]string, error)
```

`ListAllKeysByPrefix` does one pass for all prefixes instead of one pass per prefix.

### 3.9 Slash commands

`server/command/command.go`. The `PluginAccessor` interface becomes server-scoped:

```go
GetManagedServers() ([]kvstore.ServerConfig, error)
AddServer(serverURL, asToken, hsToken, usernamePrefix, serverID, serverNameOverride string) (string, error) // §3.1.1, §3.1.2
RemoveServer(serverID string) (bool, error) // §3.1.1
SetServerEnabled(serverID string, enabled bool) error // §3.11
GetMatrixClientForServer(serverID string) *matrix.Client
GetRemoteIDForServer(serverID string) string
CreateOrGetGhostUserForServer(serverID, mattermostUserID string) (string, error)
GetMatrixUserIDFromMattermostUserForServer(serverID, mattermostUserID string) (string, error)
MapChannelToServer(serverID, channelID, matrixRoomIdentifier string) error   // the §3.3 choke point
UnmapChannelFromServer(serverID, channelID string) error
GetPluginID() string // from the generated manifest; for plugin-relative URLs (below)
```

**Every `/matrix` subcommand is gated on `model.PermissionManageSystem`**, checked once in
`executeMatrixCommand` before the subcommand switch so no branch can forget it. Note that
`AutocompleteData.RoleID = model.SystemAdminRoleId` is **not** that gate — it only hides a
suggestion from non-admins in the autocomplete UI and does not stop anyone typing the
command out in full. Set it for tidiness, but never rely on it for authorization.

**`/matrix server` subcommands:**

| Command                                                                                                           | Behaviour                                                                                                                                                                                                                                                                                                                                                                                                                       |
| ----------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `server list`                                                                                                     | All entries: display name, `server_id`, URL, username prefix, mapped-channel count, enabled state. Never print tokens.                                                                                                                                                                                                                                                                                                          |
| `server add <server_url> <as_token> <hs_token> [username_prefix] [--server-id <prior_id>] [--server-name <name>]` | Validate + normalize the URL, reject an endpoint that is live in the registry, resolve `ServerName` via `resolveServerName` and reject the add if it duplicates another entry's (§3.1.2), resolve `EventDomain` from the endpoint (§3.6), mint `model.NewId()` (or re-adopt `--server-id`, §3.1.1), register the shared-channels remote, `refreshServersAndBroadcast`. Report the `server_id` and the discovered `server_name`. |
| `server remove <server_id>`                                                                                       | Remove the entry, `UnregisterPluginRemoteForSharedChannels`, `refreshServersAndBroadcast`. The server's KV records are **kept** (§3.1.1); the response must print the `server_id` as the recovery key for re-adopting them.                                                                                    |
| `server map [server_id] <room_alias\|room_id>`                                                                    | Map the current channel — refuses if already mapped to another server.                                                                                                                                                                                                                                                                                                                                                          |
| `server unmap [server_id]`                                                                                        | Unmap the current channel for that server.                                                                                                                                                                                                                                                                                                                                                                                      |
| `server registration [server_id]`                                                                                 | Print the Application Service registration YAML, built server-side from the registry entry (the webapp download is gone). See the `url` rule below — getting it wrong silently kills inbound sync.                                                                                                                                                                                                                               |
| `server status [server_id]`                                                                                       | Status for one server.                                                                                                                                                                                                                                                                                                                                                                                                          |
| `server test [server_id]`                                                                                         | Full connection diagnostic for one server: registry entry, client initialized, `TestConnection`, homeserver name/version, and `TestApplicationServicePermissions`. The AS-permission check exists nowhere else, and `/matrix test` cannot reach it once a second server is registered, so this subcommand is required rather than a convenience.                                                                                   |
| `server enable\|disable <server_id>`                                                                              | **Required, not optional** — flips `Enabled` and refreshes. Takes a server out of service without discarding its registry entry, which `remove` would.                                                                                                                                                                                                                                                      |

`[server_id]` may be omitted when exactly one server is registered. `resolveServerIDArg`
matches by `server_id` first, then by `ServerName`, then by URL host.

`--server-id` and `--server-name` must be stripped **before** positional parsing (accept
both `--flag <value>` and `--flag=<value>`) so they can appear anywhere without shifting the
optional trailing `username_prefix`. An unrecognized `--flag` is an error, not a positional
value — otherwise a typo silently becomes a username prefix. These two flags belong to
`server add` only; no other subcommand takes a flag.

**The registration `url` is the plugin base path and nothing more:**

```yaml
url: <SiteURL>/plugins/<plugin_id>
```

The homeserver appends `/_matrix/app/v1/transactions/{txnId}` itself, matching the route
registered in `server/api.go`. Including `/_matrix/app/v1` in the `url` produces
`/plugins/<plugin_id>/_matrix/app/v1/_matrix/app/v1/transactions/{txnId}`, which matches no
route: **every inbound transaction for that server is dropped while outbound keeps working**,
because outbound never reads the registration. Build `<plugin_id>` from the generated
manifest (`GetPluginID`), never a literal. Assert the exact `url` line in a test — this is a
silent, one-directional failure that no other check catches.

**Strict positional counts.** Every subcommand validates its argument count through one
helper and rejects anything outside its range with its usage string:

```go
func requireArgs(rest []string, minArgs, maxArgs int, usage string) *model.CommandResponse
```

Extra positionals must **not** be silently dropped: a stray word would otherwise change
which server a command acted on. This applies to the zero-argument subcommands too
(`server list`, `/matrix list|status|unmap|test`). `optionalServerIDArg` (0–1, for
`server unmap|registration|status|test`) is built on the same helper. `/matrix create` is the
one exception — its room name is deliberately variadic, so it validates its own arguments.

**Server-ID autocomplete.** Copying a 26-character opaque ID out of `server list` is the
main friction in the multi-server UX, so subcommands taking a server identifier offer the
registered servers as a dynamic list:

- `AddDynamicListArgument("Matrix server", <url>, required)` on `server remove|unmap|`
  `registration|status|test|enable|disable`, and on `server map`'s optional first
  positional.
- The fetch URL is `/plugins/<plugin_id>/api/v1/autocomplete/servers`, built by one exported
  helper (`command.ServerAutocompleteURL`) from `GetPluginID()` so it cannot drift from the
  route (§3.5).
- `server map`'s server ID stays **positional**. A named `--server` flag would avoid
  suggesting servers in the room-identifier slot when the ID is omitted, but it adds a second
  accepted grammar for one command; the positional list is the accepted trade-off.

Typing a `ServerName` or URL host by hand still works — the list is a convenience over
`resolveServerIDArg`, not a replacement for it.

**Existing commands**

- `/matrix status` — admin-gated like every other subcommand (it exposes server names, URLs
  and health), and never prints tokens. Lists **every** configured server with
  enabled state and live connection health. Probe connections
  **concurrently under a single deadline** (~8s, in a `var` so tests can shorten it): the
  Matrix HTTP client allows 30s per request, so sequential probes would outlast
  Mattermost's slash-command timeout. Servers whose probe misses the deadline render as
  "timed out", never as healthy.
- `/matrix map`, `/matrix create`, `/matrix unmap`, `/matrix test` — resolve the sole
  registered server; with zero or several, return an ephemeral error pointing at
  `/matrix server`.
- `/matrix list` — show `channel → room (server)`.
- No command other than `/matrix list` reports a mapped-channel count. Counting requires
  paging the whole KV keyspace (pluginapi's prefix filter is client-side) plus one `Get`
  per mapping — hundreds of round trips inside a slash command on a large install. `list`
  pays that cost because enumerating the mappings *is* its output; `status`,
  `server list` and `server status` do not.

`unmap` sequencing matters: clear the Matrix room state first (`RemoveMattermostChannelID`
— if this fails, abort, or sync messages keep flowing), then delete the mapping key
(delete the key, do not store an empty value), then both `room_mapping_` reverse keys
(alias _and_ resolved room ID), then `UninviteRemoteFromChannel` with **that server's**
remote ID. A corrupt (unparseable) mapping value should be cleared with an explanatory
message rather than reported as "not mapped".

### 3.10 System console configuration

`plugin.json` keeps **one** setting: `rate_limiting_mode`. Remove `matrix_server_url`,
`matrix_server_name`, `matrix_as_token`, `matrix_hs_token`, `registration_download`,
`homeserver_config` — and `enable_sync` (§3.11). Update the `header` to say homeservers are
managed with `/matrix server`.

`server/configuration.go`: `configuration` shrinks to `{RateLimitingMode}`; drop the
`GetMatrix*` accessors, `GetEnableSync`, and the per-server validation from
`validateConfiguration` (per-server input is validated in `AddServer`), which leaves only
rate-limit-mode normalization. `OnConfigurationChange` still calls `initMatrixClients()`
(rate-limit mode feeds client construction) and returns its error, and still fails when
`ConnectedWorkspacesSettings.EnableSharedChannels` is off — that check is unrelated to
`enable_sync` and stays.

### 3.11 Per-server sync enablement

The global `enable_sync` gate is replaced by `ServerConfig.Enabled`. Seven call sites on
master read `config.EnableSync`; each maps to a specific replacement:

| Site                                                 | Replacement                                                                                  |
| ---------------------------------------------------- | -------------------------------------------------------------------------------------------- |
| `api.go:49` (inbound middleware)                     | Drop it. The per-server `Enabled == false` rejection (§3.5) already covers this request.     |
| `hooks.go:11` `OnSharedChannelsSyncMsg`              | Resolved server's `Enabled` — checked inside `serverIDForSyncMsg` (below).                   |
| `hooks.go:99` `OnSharedChannelsAttachmentSyncMsg`    | Same.                                                                                        |
| `hooks.go:265` `OnSharedChannelsProfileImageSyncMsg` | Same.                                                                                        |
| `hooks.go:70` `OnSharedChannelsPing`                 | Return `true` (healthy-but-idle) when the **pinged** server is disabled (§3.6).              |
| `plugin.go:288` `UserHasJoinedChannel`               | No `rc` available: skip each mapped server whose entry is disabled while acting on the rest. |
| `configuration.go:111` `validateConfiguration`       | Deleted with the field (§3.10).                                                              |

**`serverIDForSyncMsg` gains an `Enabled` check.** §3.6's resolution steps currently test
`rc`, the remote index, and the channel mapping — not `Enabled`. Add it: a resolved server
with `Enabled == false` is skipped exactly like an unmapped one. This is now the _only_ thing
stopping outbound traffic, so omitting it would make `server disable` silently ineffective
for posts, edits, reactions and attachments.

**Disabling does not touch the remote or its invitations.** The shared-channels remote stays
registered and the channel invitations stay in place, so the platform keeps invoking the
hooks for a disabled server and the `Enabled` checks above are what make it a no-op. That
keeps `enable` a pure flag flip — no re-registration, no re-invites, no cursor reset — and it
is why the check has to live at the routing layer rather than by tearing down registrations.

`initMatrixClients` still builds a client for a disabled entry, so `/matrix status` can probe
it and report health. Only routing consults `Enabled`.

---

## 4. Suggested build order

Each step should compile, pass `make check-style`, and land with its own tests, and
finish with a commit following the conventional commits notation.

1. **kvstore foundation** — `SetAtomicWithRetries` on the interface + implementation +
   regenerated mock; `listall.go`; `schema.go` (`ServerConfig`, `ChannelServerMapping`,
   parse/marshal helpers); serverID-aware key builders; `CurrentKVStoreVersion = 1`.
2. **Registry** — `server/servers.go`: `getServers`, `mutateServers`,
   `normalizeServerEndpoint`, `resolveServerName`, `AddServer` (mint or re-adopt; resolve +
   validate `ServerName`; resolve `EventDomain`), `RemoveServer`,
   `GetManagedServers`, `serverDomainForID`. Also a `matrix.Client` method for
   `GET /_matrix/key/v2/server` returning its `server_name` field (§3.1.2).
3. **Plugin wiring** — per-server client maps, `initMatrixClients`, on-demand bridge
   constructors, `registerForSharedChannels` with per-server `SiteURL`,
   `refreshServersAndBroadcast` + `OnPluginClusterEvent`, `remoteIDForServer`,
   `isOwnRemoteID`, `serverIDForRemoteID`. Delete `p.matrixClient` / `p.remoteID` /
   `initBridges`.
4. **Config slimming** — `plugin.json` + `configuration.go` + `min_server_version`; delete
   `enable_sync` and replace all seven of its call sites with the per-server `Enabled` checks
   (§3.11). Do this step **after** step 3 so `serverIDForSyncMsg` exists to host the routing
   check, and never leave a state where the global gate is gone but the per-server one is not
   yet wired — that would sync unconditionally.
5. **Migration** — the single purge step and the `kv_schema_version` marker (§3.8).
6. **Bridges** — thread `serverID` through `BridgeUtils`, `sync_to_matrix.go`,
   `sync_to_mattermost.go`, `matrix_util.go`, `post_tracker.go`; per-server property key,
   username prefix, ghost keys.
7. **Inbound routing** — `api.go` middleware + `matrix_webhook.go`.
8. **Outbound routing** — `serverIDForSyncMsg` + all of `hooks.go` + `UserHasJoinedChannel`.
9. **One-server-per-channel enforcement** — in `setChannelRoomMapping` and every command
   map path.
10. **Commands** — `/matrix server` group (including `test`), multi-server `status`/`list`,
    registration YAML with the plugin-base `url`, strict positional counts via `requireArgs`,
    and the autocomplete endpoint plus its dynamic-list arguments.
11. **Tests** — unit throughout (steps 1–10 should each bring their own), then the
    integration suites.
12. **Docs** — README "Multiple homeservers" section (state the one-server-per-channel rule
    explicitly; document the remove/re-adopt recovery path — `remove` keeps the server's
    records and prints its `server_id`, and `add --server-id <id>` restores them; warn that
    upgrading from a single-homeserver install wipes all stored bridge state; and note that
    `server_name` is discovered from
    the homeserver rather than supplied, with `--server-name` as the escape hatch; and that
    `server registration` output is copied verbatim — admins must not append
    `/_matrix/app/v1` to its `url`), and
    `docs/local-development.md` + `docker-compose.yml` for a second Synapse — stating the
    distinct-port-less-`server_name` requirement from §5.2.

---

## 5. Testing requirements

### 5.1 Unit tests (all required)

- **`kvstore`** — `ParseServersConfig` / `ParseChannelServerMappings` on empty, valid,
  corrupt input; key builders; `ListAllKeysWithPrefix` **across multiple raw pages** with
  matches beyond page 0 (this is the regression the paging design exists for);
  `ListAllKeysByPrefix` bucketing.
- **Mapping helpers are N-ready** — `UpsertChannelServerMapping`,
  `RemoveServerFromChannelMapping`, `RoomIDForServer` and `MappedServerIDs` must be tested
  against a **two-entry** array (which policy forbids writing today, but the helpers will
  see once the limit is lifted): upsert preserves the other server's entry, remove returns
  the remainder, `RoomIDForServer` picks the right one, and removing the last entry deletes
  the key.
- **`servers.go`** — endpoint normalization (default ports, case, trailing slash, invalid
  URLs); `AddServer` rejects an endpoint that is live in the registry; `AddServer` registers
  a remote and refreshes; `RemoveServer` returns `false` for an unknown ID, unregisters the
  remote, survives a failing unregister, and **leaves the server's namespaced keys and
  channel mappings intact** (assert they are still readable afterwards — this is the
  behaviour re-adoption depends on); `mutateServers` CAS retry on conflict.
- **`resolveServerName` (§3.1.2)** — each step wins in order: `configuredName` short-circuits
  without any HTTP call; `/_matrix/key/v2/server` supplies the name when it is absent; a 404
  or transport error falls through to `Hostname(serverURL)`; a malformed or `server_name`-less
  response body also falls through rather than erroring. Resolution **never fails** for a
  parseable URL. A name equal to an existing entry's is rejected by `AddServer` and the message
  names the conflicting `server_id`. `--server-name` lands on step 1.
- **`EventDomain` (§3.6)** — resolved from the endpoint at add time, persisted, and read
  back verbatim by every property-key call site; distinct for `localhost:8008` vs
  `localhost:8009`; **never recomputed** — mutating an entry's `ServerName` afterwards leaves
  the property key unchanged (this is the regression the stored field exists to prevent).
- **Re-adoption (§3.1.1)** — `AddServer` with an explicit `serverID` reuses it verbatim;
  rejects a malformed ID and one colliding with a live entry; remove-then-re-adopt makes the
  server's namespaced records and channel mappings reachable again (assert a ghost-user key
  and a `channel_mapping_` entry resolve after re-adoption); re-adding the same `serverID` at a
  _different_ endpoint warns that surviving `matrix_event_post_` records are under a
  different `EventDomain`.
- **Stale mappings** — a mapping entry for a deregistered server does not count toward
  `maxServersPerChannel` (mapping a different server succeeds and drops the stale entry in
  the same write), and `RoomIDForServer` still resolves that entry's room, which is what
  makes re-adoption restore the link.
- **Migration (§3.8)** — fresh install (empty store → nothing deleted, marker still
  advances to 1); an upgrade with a record under every prefix plus a `kv_store_version`
  marker purges all of them and the old marker; `KeyServersConfig` is **never** deleted;
  a store already at version 1 is left untouched; a failed delete, a failed keyspace scan,
  an unreadable marker, and a non-numeric marker each leave the marker unstamped **and**
  the records in place; a retry after a failed run completes the purge and stamps.
- **`initMatrixClients`** — builds one client per entry **including disabled ones** (§3.11, so
  `/matrix status` can probe them), populates `remoteToServerID` / `ownRemoteIDs`, no-ops with
  a nil kvstore, returns an error on registry read failure, and concurrent rebuilds do not
  resurrect a stale snapshot.
- **Per-server enablement (§3.11)** — with server A enabled and B disabled: a post in a
  channel mapped to B is **not** synced while an equivalent post to A is; `serverIDForSyncMsg`
  skips the disabled server; the inbound middleware rejects B's `hs_token` and accepts A's;
  ping returns `true` for B; `UserHasJoinedChannel` acts on A's mapping and skips B's;
  `server enable B` makes it sync again with **no** re-registration or re-invite (assert
  `RegisterPluginForSharedChannels` and `InviteRemoteToChannel` are not called). Assert no
  code path reads a global sync flag — the field is gone.
- **Inbound auth** — token matches the right server; empty `HSToken` entries never match;
  unknown token → 401; disabled server → 503; `serverID` reaches the handler; missing
  client → 503 + `Retry-After` and the txn is **not** marked processed; identical `txnId`
  from two different servers is processed twice; a `processMatrixEvent` failure partway
  through a transaction → 503 + `Retry-After`, the txn is **not** marked processed, and a
  retry of the same `txnId` is processed (not deduped).
- **Outbound routing** — `serverIDForSyncMsg` for: mapped-to-`rc`'s-server, mapped-to-
  another-server, unmapped DM, unmapped non-DM, nil `rc`, unknown remote, **disabled server**.
  Assert the hooks call the bridge **exactly once** and never for a non-matching server.
- **Hooks** — loop prevention rejects every own remote ID (not just one); ping resolves
  `rc` to a single server and returns `true` for disabled/unconfigured; attachment upload
  targets one server; profile-image sync uses `rc`'s server.
- **One-server-per-channel policy** — mapping a second server to a mapped channel returns
  `ErrChannelAlreadyMapped` via command _and_ via the bridge path; re-mapping the same
  server to a new room succeeds and clears the stale reverse keys; concurrent maps on one
  channel produce exactly one winner; the stored value is a **JSON array** (assert the
  encoded bytes, so a future `maxServersPerChannel` bump needs no migration).
- **Shared-channels remotes** — `registerForSharedChannels` calls
  `RegisterPluginForSharedChannels` once per entry with distinct `SiteURL`s and persists each
  `RemoteID`; a failure for one server does not
  block the others; the merge does not clobber a concurrently added/removed server; each
  client and both bridge directions carry their own server's `RemoteID`; `map`/`unmap`
  invite/uninvite that server's remote only.
- **Trackers** — `PostTracker` / `PendingFileTracker` isolate identical post IDs across
  two `serverID`s.
- **Commands** — `server add/remove/list/map/unmap/registration/status/test` happy paths and
  arg-count errors; `--server-id <id>` and `--server-id=<id>` stripping does not shift the
  optional trailing `username_prefix` regardless of position (same for `--server-name`), and
  an unknown `--flag` errors instead of being taken as a positional; `remove` prints the
  recovery key; `server enable`/`disable` flip `Enabled` and
  refresh; non-admin is rejected for the whole `server` group;
- **Registration YAML** — assert the exact `url:` line equals `<SiteURL>/plugins/<plugin_id>`
  with **no** `_matrix/app/v1` anywhere in the output, over both a plain SiteURL and one with
  a trailing slash (§3.9). This is the guard for a silent inbound-only failure.
- **Strict arg counts** — every subcommand rejects one argument beyond its maximum with its
  usage string, including the zero-argument ones; `server map` accepts one or two and rejects
  zero or three; `optionalServerIDArg` returns the value for 0–1 and a usage response beyond.
- **`server test`** — targets a named server while two are registered and does **not** touch
  the other; resolves the sole server from a bare invocation; is ambiguous with several; errors
  on an unknown identifier.
- **Autocomplete endpoint** — a non-admin gets 403 and **no server IDs in the body**; an admin
  gets one item per server with the right `Hint` and an enabled/disabled `HelpText`; zero
  servers yields `[]` with 200, not an error; `ServerAutocompleteURL` matches the route
  registered in `ServeHTTP`. `/matrix status`
  with 0/1/N servers and with a probe that exceeds the deadline; registration YAML content;
  `resolveServerIDArg` matching by ID, server name and host; ambiguity errors when several
  servers exist.
- **Cluster** — `refreshServersAndBroadcast` publishes the event and tolerates a publish
  failure; `OnPluginClusterEvent` rebuilds on the known ID and ignores unknown IDs.

### 5.2 Integration tests (testcontainers + live Synapse)

Keep the existing single-server suites green — they are the regression net proving the
refactor changed no behaviour for one server. If your Docker runtime doesn't expose the
default socket (e.g. OrbStack on macOS), set `DOCKER_HOST` to its socket, or the
testcontainers suites fail to start.

Add multi-server suites against **two** independent homeservers with dynamically assigned
ports and — this is a hard requirement, not a convention — **genuinely distinct, port-less
`server_name`s** (`synapse1.local` / `synapse2.local`, or `example.com` / `example2.com`).

Do **not** configure the two Synapses as `localhost:8008` / `localhost:8009`. Those are
legal Matrix server names, but `NormalizeServerName` strips the port
(`server_discovery.go:166-169`), so both collapse to `localhost` and:

- every ghost on both servers becomes `@<prefix>_<userid>:localhost`, so `isGhostUser`
  cannot attribute an inbound event to the right server — and unlike the event-ID property
  key this is not fixable by choosing a different discriminator, because the domain is baked
  into the Matrix user ID by the homeserver itself;
- the §3.1.2 uniqueness check rejects the second `server add` outright, so the suite would
  fail at setup rather than exercising routing.

Add an assertion at suite setup that the two discovered `ServerName`s differ, so a
mis-provisioned second Synapse fails loudly with that reason instead of surfacing later as
an unexplained ghost-attribution or isolation failure.

The suites themselves:

- **Registry / client wiring** — two clients, isolated KV namespaces, per-server ghost users.
- **Inbound** — an event delivered with server A's `hs_token` lands in A's mapped channel
  and never in B's; ghost recognition uses each server's own domain; Matrix-initiated DM
  creation on each server; a `txnId` reused across servers is not deduped away.
- **Outbound** — a post in a channel mapped to A reaches A's room only; the same for B;
  attachments, edits, deletes, reactions, profile images, and member joins all route to the
  single mapped server; the `matrix_event_id_<domain>` property keys stay distinct.
- **Rejection** — attempting to map a channel already mapped to A onto B fails, leaves A's
  mapping intact, and creates nothing on B (this replaces the old branch's
  "same channel, two servers" fan-out tests).
- **Lifecycle** — `server add` makes a server usable without restart; `server remove`
  stops its sync and unregisters its remote while the other server keeps working.
- **Re-adoption round trip** — with a channel mapped and ghosts created against server A:
  `server remove A`, confirm sync stops, then `server add <A's URL> … --server-id <A's id>`
  and confirm the channel resumes bridging to the same room with the **same ghost users**
  (no duplicate ghosts, no re-map needed) and that the platform hands back A's original
  `remoteID`. Then repeat the removal without re-adopting and confirm the channel can be
  mapped to server B instead (the stale-entry path, §3.1.1).
- **Full feature sweep** — run the complete feature matrix (posts, edits, deletes,
  reactions, threads, attachments, mentions, DMs, avatars) once with one server configured
  and once with two, to prove nothing regresses in either topology.

Dev infrastructure for this exists on the old branch and can be reused as-is:
`docker-compose.yml` (`synapse2`/`element2` on host ports 8889/8081),
`docker/synapse2_config.yaml`, `docker/mattermost-bridge-registration2.yaml`,
`docker/element*-config.json`, and `docs/local-development.md`.

---

## 6. Forward compatibility: lifting one-server-per-channel later

The design target is that allowing N servers per channel is a **policy** change, not a data
or routing change. When the work here is done, that future change should need only:

1. Raise or drop `maxServersPerChannel` (§3.3).
2. Relax the command UX: `server map` stops erroring, and the response mentions the other
   homeservers the channel is already bridged to.
3. Decide the product question this release sidesteps: Matrix-originated content is
   delivered to Mattermost only and is **not** relayed to the other homeservers sharing the
   channel, so Matrix users on different homeservers would not see each other. Either
   accept and document it (what the old branch did, with a warning on `map`) or build
   relaying.

Nothing else should have to move. Verify that claim while building, and treat a violation
as a bug in this plan's implementation:

- Storage is already a JSON array; no migration needed to hold a second entry.
- Every mapping helper takes/returns lists and is unit-tested against a two-entry array (§5.1).
- `room_mapping_`, ghost, event, reaction keys and the `matrix_event_id_<domain>` post
  property are namespaced per server, so two servers on one channel cannot collide.
- `PostTracker` / `PendingFileTracker` are keyed by `(serverID, postID)`, so a post shared
  to two servers keeps independent mxc URIs and edit timestamps.
- Each server has its own shared-channels remote and its own channel invitation, so the
  platform will deliver one hook invocation per server and `unmap` of one leaves the other
  invited (§3.4).
- Outbound routing tests membership in the mapping list rather than equality with a single
  server (§3.6).
- Reads never index `[0]`; they resolve by `serverID`.

Unchanged and worth keeping: the KV namespacing scheme, the
`hs_token`→server middleware, the `/matrix server` command surface, the cluster-event
invalidation, the per-server trackers, and the dev/test infrastructure.
