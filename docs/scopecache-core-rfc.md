# scopecache — Core RFC

> **Status: pre-v1.0 draft.** All sections (§1–§10) are in place.
> Wording, structure, and detail levels remain open for revision.
> [scopecache-rfc-old.md](scopecache-rfc-old.md) is kept as a
> historical reference for the v0.7.16-era multi-tenant gateway
> design; addon-specific RFCs will land alongside this document as
> the addon sub-packages ship.

---

## 1. Scope and boundary

### 1.1 What scopecache is

`scopecache` is a small, local, rebuildable in-memory cache and write
buffer. It is addressed by `scope` (namespace), `id`, and `seq`; it
holds opaque JSON payloads; it can be wiped and rebuilt from the
source of truth at any time. The source of truth lives outside the
cache — a database, a JSON file, data generated in code, anything.

The cache supports two main use patterns:

- **Hot-read cache.** Keep frequently queried fragments in memory so
  they don't hit the database on every request. A fronting proxy
  (Caddy, nginx, apache) can serve cached HTML, JSON, or XML straight
  from `/render` without any application layer in between.
- **Write buffer.** Append high-frequency events; a background worker
  drains the buffer in batches via `/tail` followed by
  `/delete_up_to`.

### 1.2 Deployment modes

scopecache runs in two modes:

- **Standalone binary.** Listens on a Unix domain socket, reachable
  from any HTTP client in any language (`curl`, PHP `cURL`, Python
  `requests`, Node `fetch`, …). The lowest-friction setup: any
  webstack that already speaks HTTP can use it.
- **Caddy module.** The cache lives inside the same process as
  Caddy and is served on the Caddyfile-defined listener. This is
  the recommended deployment when the cache sits behind a webserver
  that already terminates client connections.

The Caddy-module path is the one that gets the most out of the
cache: the webserver answers cache hits directly from memory
instead of forwarding the request to PHP/Python/etc., querying a
separate cache like Redis, and serialising the response back.
Benchmarks measured roughly 5× the throughput of an
equivalent webserver → application → Redis → response path serving
the same bytes — even when the application path is FrankenPHP in
worker mode.

When scopecache runs as a Caddy module it lives in the same Go
process as Caddy itself — no extra hop over a Unix socket, no
serialisation between processes. That in-process call is where the
largest performance gain comes from.

### 1.3 What scopecache is not

The core does not implement:

- a database, search engine, analytics store, or query language
- payload inspection, joins, or filtering beyond `scope`/`id`/`seq`
- TTL, eviction, schedulers, drains, or any background policy
- authentication, authorization, tenant management, or rate limiting
- business workflows of any kind

Anything in this list that you need is operator policy or addon
territory. See §1.4.

### 1.4 The boundary rule

The core has no business logic and no policy logic. It owns:

- memory and capacity enforcement
- `scope`/`id`/`seq` addressing
- write, read, delete, and bulk primitives
- raw payload rendering (`/render`) — cached HTML, XML, or other
  bytes served directly to a client with no JSON envelope
- operational stats and lightweight read-bookkeeping metadata
  (`readCountTotal`, `lastAccessTS`)
- a public, validated Go API for in-process callers

The core has no request context (who is calling, what tenant,
what permission). Those concerns live in **addons** — separate Go
sub-packages built on top of the core's public Go API — or are
handled outside the cache entirely (Caddyfile route guards,
nginx/apache equivalents, Unix-socket filesystem permissions, or
any other transport-layer policy).

The current set of standard addons covers multi-tenant gateways,
batch dispatch, write-only ingestion, operator-elevated dispatch,
and eviction-hint queries. Their RFCs live alongside this one in
`docs/`. Third-party addons follow the same pattern.

### 1.5 Modular architecture

The core is the foundation: a small set of building blocks — the
data model, the capacity rules, the address primitives, and the
public Go API surface (`*scopecache.Gateway` and the methods it
exposes). Anything beyond that comes from **addons**: separate Go
sub-packages built on top of `*Gateway`. Some addons ship with
`scopecache` as part of the standard distribution (multi-tenant
gateways, batch dispatch, write-only ingestion, …); anyone can
build their own addons against the same public interface without
touching the core.

One addon-shaped concern lives in core by exception: the subscriber
bridge (§7.5) that turns the in-process `Subscribe` primitive into
external command invocations. That single bridge is universal
enough that every realistic deployment wants it; the cost of
keeping it in `package scopecache` is one stdlib `os/exec`
dependency, no third-party imports.

The result is a clean separation of concerns:

- The core does one job — store and address items, enforce
  capacity, expose the primitives — and stays small, fast, and
  heavily tested.
- Addons add request-context-aware behaviour (auth, tenants, batch
  composition, custom ingestion shapes) without ever needing
  privileged access to core internals.

This separation is what allows the core to remain stable under
heavy testing and benchmarking while addons can evolve, be added,
or be removed without risk to the core itself.

### 1.6 Status

Pre-v1.0. Core HTTP and Go API surfaces are subject to breaking
change between minor versions. After v1.0 the core becomes
semver-stable; addon RFCs version independently of the core RFC.

---

## 2. Data model

### 2.1 Item

The cache stores **items**. An item has the following fields:

| field     | type           | required on writes | owner  | searchable |
|-----------|----------------|--------------------|--------|------------|
| `scope`   | string         | yes                | client | yes        |
| `id`      | string         | no                 | client | yes        |
| `seq`     | uint64         | n/a                | cache  | yes        |
| `ts`      | int64          | n/a                | cache  | no         |
| `payload` | any JSON value | yes                | client | no         |

- **`scope`** — required on every operation. Free-form, ≤ 256 bytes.
  Items inside the same scope share the same per-scope buffer (and
  its mutex).
- **`id`** — optional on writes. When present, must be unique within
  its scope. Free-form, ≤ 256 bytes.
- **`seq`** — cache-assigned monotonic counter, scoped per buffer.
  Clients **must not** send `seq` on write endpoints; reads accept
  `seq` as an addressing key.
- **`ts`** — cache-assigned microsecond Unix timestamp
  (`time.Now().UnixMicro()`), refreshed on every write that touches
  the item. Observability only — not searchable, not indexed, not
  used for ordering.
- **`payload`** — required, any valid JSON value (object, array,
  string, number, boolean). Literal `null` is rejected. The cache
  treats payload bytes as opaque; nothing inspects, parses, or
  searches inside them.

### 2.2 Addressing

Items are addressed via `scope`, `scope`+`id`, or `scope`+`seq`.
There is no global index, no secondary lookup by payload contents,
and no range query other than a `seq`-prefix drain
(`/delete_up_to`) and a sequential read (`/tail`, `/head`).

Within a scope, items appear in `seq` order. `seq` starts at 1 for
the first write into a scope and increases monotonically per scope.
`seq` numbers are not reused after deletion.

The flexibility of the addressing surface comes from how clients
**name** scopes and IDs. A wide range of real-world access patterns
can be modelled by encoding the relevant query key into the scope
name, the ID, or both. If an application needs a query the core
cannot answer (for example: "all events with `level=error`"), the
standard pattern is a **materialized view** — a separate scope
maintained by the application alongside the primary scope,
containing only the items that match the query. Reads from the view
are then a plain `/tail` or `/head` against a smaller,
query-specific scope. The core makes no special accommodations for
this pattern; it simply allows it through naming flexibility.

Adding more filter axes to the core is explicitly **not** on the
roadmap. See §10.3.

### 2.3 Scopes and IDs are opaque strings

Both `scope` and `id` are free-form strings of up to 256 bytes
(see §4.1). The core imposes no structure beyond the size and
encoding limits: names like `thread:42`, `user:alice:inbox`, or
HMAC-derived prefixes are equally valid. The core does not parse
them and does not split on `:`.

Lookup by `scope`, `id`, or `seq` runs against in-memory hashmaps
and is independent of both name length and scope size — the
underlying Go-map lookup is constant-time on average. In-process
measurements show roughly 35–40 ns per single-item read on
commodity hardware (`BenchmarkStore_GetByID` and
`BenchmarkStore_GetBySeq`; 100 scopes × 1,000 items × 512-byte
payloads, ~57 MiB store).

The core makes **two specific exceptions** to the "names are
opaque, no special interpretation" rule: the scope names `_events`
and `_inbox` are reserved infrastructure scopes that the cache
pre-creates at boot and protects from scope-level destruction
(see §2.6). Other underscore-prefixed names (`_tokens`,
`_counters_*`, `_guarded:*`, …) remain pure naming convention
that addons follow for operator recognisability; the core does
not parse or interpret those prefixes — see §10.2 for the
addon-state protection model.

### 2.4 Per-scope metadata

In addition to the items themselves, every scope carries a small set
of metadata fields. Most are bookkeeping for cap accounting and
addressing; the two read-bookkeeping fields (`lastAccessTS`,
`readCountTotal`) are exposed as primitives. Time-windowed
aggregations (rolling N-day counts, hourly histograms, exponential
decay, …) are policy and live in addons that poll `readCountTotal`
deltas off a scheduler — see §9.

| field                | purpose                                                                        |
|----------------------|--------------------------------------------------------------------------------|
| `items []Item`       | the items themselves, in `seq` order                                           |
| `byID`, `bySeq`      | id→item and seq→item lookup maps (lazy-allocated; absent until first item)     |
| `lastSeq`            | monotonic `seq` counter; never rewinds; deleted seqs are not reused            |
| `maxItems`           | per-scope item cap (writes past this are rejected with 507)                    |
| `bytes`              | Σ `approxItemSize` over items — feeds store-wide `totalBytes`                  |
| `idKeyBytes`         | Σ `len(item.ID)` over `byID` — keeps `approxSizeBytes` O(1)                    |
| `createdTS`          | microsecond timestamp of scope creation                                        |
| `lastWriteTS`        | microsecond timestamp of the most recent write that touched the scope — `/append`, `/upsert` (create and replace), `/update`, `/delete`, `/delete_up_to`, `/warm`, `/rebuild`. `/counter_add` is excluded by design; see note below. |
| `lastAccessTS`       | microsecond timestamp of the most recent read hit (atomic, lock-free)         |
| `readCountTotal`     | lifetime read-hit count (atomic, lock-free)                                    |
| `detached`           | lifecycle flag set by `/delete_scope`, `/wipe`, `/rebuild` to fail in-flight writes against an orphan buffer |

The seven primitives (`item_count`, `last_seq`, `approx_scope_mb`,
`created_ts`, `last_write_ts`, `last_access_ts`, `read_count_total`)
are surfaced as a per-scope bundle on `/scopelist` (§6.5), with
optional prefix filtering and alphabetical cursor pagination.
`/stats` itself stays aggregate-only (§2.5). In-process Go callers
read the same per-scope fields via `*Gateway.ScopeList` (§7.2).

**`/counter_add` and the freshness signal.** `/counter_add` refreshes
the per-item `ts` on every create and increment, but does NOT bump the
scope's `lastWriteTS` or the store-wide `last_write_ts` (§2.5).
Counters are typically used to track read-driven activity (view
counters bumping on every page hit), and folding those increments into
`lastWriteTS` would degrade it from a "did meaningful content change?"
signal into a heartbeat. Consumers who specifically care about a
counter's freshness do `GET /get?scope=…&id=…` and read `item.ts`,
which is the per-counter "last activity" timestamp.

### 2.5 Store-wide metadata

Four store-wide fields are surfaced via the `/stats` endpoint
(§6.5). Each is maintained incrementally on the write/delete/bulk
paths so `/stats` can answer in O(1) — four atomic loads,
independent of how many scopes the store holds. Configured caps
(`Config.MaxStoreBytes`, `Config.ScopeMaxItems`, `Config.MaxItemBytes`)
are static config and live on `/help`, not `/stats` — they do not
change between calls and re-emitting them on every poll is pure noise.

| field             | purpose                                                                  |
|-------------------|--------------------------------------------------------------------------|
| `scope_count`     | live scope count (atomic; updated on scope create / delete / wipe / rebuild) |
| `total_items`     | Σ `len(scope.items)` across all live scopes (atomic; updated on every write/delete) |
| `approx_store_mb` | running byte-budget reservation in MiB (atomic; updated on every write that reserves or releases bytes) |
| `last_write_ts`   | microsecond timestamp of the most recent state-changing event anywhere in the cache (atomic, monotonic via CAS-max). 0 means no writes since process start. Bumped by every per-scope `lastWriteTS` update plus the destructive store-level paths (`/delete_scope`, `/wipe`, `/rebuild`) that don't go through a per-scope bump. |

The four atomics (`scope_count`, `total_items`, `approx_store_mb`,
`last_write_ts`) are loaded independently. Concurrent writes between
the loads can produce a snapshot where the fields reflect slightly
different instants; the `Σ scope.bytes == approx_store_mb`, `Σ
len(scope.items) == total_items`, and `last_write_ts >= max(scope.lastWriteTS)`
invariants hold at quiesce, not at every observation. See §8.3.

**Polling pattern.** Clients that maintain a local view of cache
state can compare `last_write_ts` against the value from their
previous `/stats` call: if equal, nothing changed and the local view
is still authoritative. Useful for hot dashboards and ETag-style
cache validation that don't want to refetch state on every tick.
The CAS-max update guarantees the counter only ever advances, so a
strict `>` comparison is enough; concurrent writes from different
scopes can't reorder the counter backwards even when their wall
clocks advance out of order across CPUs.

### 2.6 Reserved scopes and boot-time initialisation

The cache reserves exactly two scope names: `_events` and `_inbox`.
Both are pre-created at `NewGateway` time so subscribers (§7.4–7.5) and
apps can attach to a known-stable target without a "scope-doesn't-
exist-yet" race, and both are re-created automatically after
`/wipe` and `/rebuild` so the cache always lands on a consistent
post-event baseline.

| Reserved scope | Role | Writer | Reader |
|---|---|---|---|
| `_events` | Cache-managed write-event log | The cache itself, on every successful mutation to any non-reserved scope | The in-core subscriber bridge (§7.5), or any caller of `Gateway.Subscribe` |
| `_inbox` | Application-side fan-in ingestion scope | External clients via `/append` | The in-core subscriber bridge (§7.5), or any caller of `Gateway.Subscribe` |

**Operations on reserved scopes** follow the
append-only-drain-stream pattern. The cache allows the operations
that fit that pattern and rejects the operations that don't:

| Operation | On `_events` / `_inbox` | Rationale |
|---|---|---|
| `/append` | ✅ allowed | Apps write to `_inbox`; cache auto-populates `_events` |
| `/delete`, `/delete_up_to` | ✅ allowed | Drainer cleanup — releasing items the consumer has handled |
| `/get`, `/head`, `/tail`, `/render` | ✅ allowed | Drainers and observers must be able to read |
| `/stats`, `/scopelist` | ✅ allowed | Operators must see infrastructure scopes for capacity planning |
| `/upsert`, `/update`, `/counter_add` | ❌ 400 | Drain-stream semantics: there is no "in-flight item to update" — entries are either still in the buffer or already drained. |
| `/delete_scope` | ❌ 400 | Would break the reservation invariant (subscribers attached to a vanished scope) |
| `/warm` (target = reserved) | ❌ 400 | Idem |
| `/rebuild` (input contains reserved) | ❌ 400 | Idem |
| `/wipe` | ✅ drops + immediately re-creates | Atomic under all-shard write lock; subscribers don't see a gap |

The reservation is **scope-level only** — item-level cleanup
(`/delete`, `/delete_up_to`) and item-level reads work normally,
because the drainer pattern fundamentally requires them.

**Boot-time initialisation.** `NewGateway` calls
`s.initReservedScopes()` after the per-shard maps are allocated.
This pre-creates `_events` and `_inbox` via `ensureScope`-equivalent
logic, charges `len(reservedScopeNames) × scopeBufferOverhead`
(2 × 1024 = 2048 bytes by default) against the store byte budget,
and bumps `scope_count` by 2. Boot-time pre-creation does NOT bump
`s.lastWriteTS` — a fresh store still reports `last_write_ts = 0`
so the "have I seen this cache before" sentinel works for polling
clients.

**Lifecycle interactions.**

- `/wipe` drops every scope (including the reserved ones), then —
  under the same all-shard write lock, before releasing —
  re-creates the reserved scopes via `s.initReservedScopesLocked()`.
  The re-creation is atomic with the wipe: subscribers attached to
  `_events` or `_inbox` never observe a moment where their scope
  doesn't exist. Post-wipe baseline is `scope_count = 2`,
  `total_items = 0`, `approx_store_mb = reservedScopesOverhead`.

- `/rebuild` validates input first (rejects the call with 400 if
  any input scope is reserved), then under all-shard write lock
  detaches every existing buffer, swaps in the new shard maps,
  resets the store-wide counters to the new totals, and finally
  calls `s.initReservedScopesLocked()` to re-create the reserved
  scopes. The cap check includes `reservedScopesOverhead` in the
  expected post-rebuild byte total so an input that fills the cap
  exactly doesn't blow past it once init runs.

- `/delete_scope` on a reserved name is rejected at the validator
  layer (400). To release the items in a reserved scope without
  removing the scope itself, use `/delete_up_to` with a
  sufficiently-large `max_seq` — the standard drainer cleanup
  pattern.

- `/append`, `/delete`, `/delete_up_to`, and reads operate on
  reserved scopes with no additional bookkeeping — same paths as
  user-managed scopes.

**Per-reserved-scope capacity knobs.** The reserved scopes use
caps that are deliberately decoupled from the global ones, but
only where decoupling buys something. The rest is derived.

| Scope | Item count cap | Per-item byte cap | Configurable? |
|---|---|---|---|
| `_inbox` | `Inbox.MaxItems` (default = `ScopeMaxItems`) | `Inbox.MaxItemBytes` (default = 64 KiB) | Both knobs are operator-tunable |
| `_events` | *(none — exempt from `ScopeMaxItems`)* | `MaxItemBytes + 1 KiB` (derived) | Neither is a knob |

`_inbox` is **operator-tunable on both axes**. It carries app-
written fan-in events that are typically much smaller than user
payloads, so the per-item cap defaults to 64 KiB rather than the
global 1 MiB. Operators who need bigger or smaller `_inbox`
events tune `Inbox.MaxItemBytes` independently. The item-count
cap defaults to the global `ScopeMaxItems` but is also separately
tunable so a high-throughput inbox can hold a different number of
events than user scopes.

`_events` is **fully derived**. Two reasons:

1. *Per-item byte cap.* A `_events` entry wraps the user payload
   produced by the write that triggered it (op-type, scope, seq,
   ts, payload). It must always be at least as wide as the
   user-write itself, plus a small envelope overhead. Setting it
   independently invites the misconfiguration "log smaller than
   user-item" → max-size user-writes silently drop their log
   entry. The cache derives `_events`'s cap as
   `MaxItemBytes + 1 KiB` so operators only ever tune
   `MaxItemBytes` and `_events` follows.

2. *Item-count cap.* `_events` is best-effort observability, not
   durable user data. The only meaningful begrenzer is the
   global byte budget (`MaxStoreBytes`), which `_events` shares
   with every other scope. A separate item-count cap on `_events`
   would be arbitrary; `ScopeMaxItems = 100,000` is no more
   right for `_events` than `1,000,000` would be. The cache
   exempts `_events` from `ScopeMaxItems` entirely; bytes-pressure
   alone gates writes there.

**HTTP response cap.** The per-response byte cap on `/head`,
`/tail`, and `/render` is derived from `MaxStoreBytes`, not
configured separately. By construction no scope can exceed the
store budget, so any single-scope read is bounded by the store
cap; making the response cap equal to the store cap guarantees
every full-scope read fits in one response. This matters most
for drainers that need to slurp `_events` or `_inbox` in large
batches without artificial response-size 507s.

**`_events` auto-populate mode (`Events.Mode`).** Controls whether
the cache writes a write-event entry to `_events` on every
successful mutation, and how much each entry contains:

| Mode | Semantics |
|---|---|
| `off` *(default)* | Auto-populate disabled; zero overhead on the write path. Operators opt in when a drainer is ready to consume. |
| `notify` | Each committed mutation produces an event with addressing only — `{op, scope, id?, seq, ts}`. Sufficient for drainers that re-fetch from cache state on wake-up. |
| `full` | Each committed mutation produces an event with the action-payload included. Sufficient for drainers replicating state without re-querying. |

Action-payload here means the inputs the caller sent, not the
result the cache computed. `/counter_add` events carry `by` (the
increment), not the new value; `/delete_up_to` events carry
`max_seq`, not the deleted-count. This makes the event stream
replay-able and matches the WAL discipline downstream sinks
expect.

**Implementation locus.** Constants `EventsScopeName` and
`InboxScopeName` live in `types.go`, alongside `InboxMaxItemBytes`
(default 64 KiB) and `eventsItemEnvelopeOverhead` (1 KiB derivation
slack). The `reservedScopeNames` array, the
`reservedScopesOverhead` constant, the `isReservedScope(scope)`
helper, and the `initReservedScopes` / `initReservedScopesLocked`
methods all live in `store.go`, together with the per-scope cap
dispatchers `maxItemBytesFor(scope)` and `maxItemsFor(scope)` that
single-source the "which cap applies here?" decision. The
`_events` exemption from the item-count cap is implemented as the
`unboundedScopeMaxItems` (= 0) sentinel installed on the buffer
at create time; `appendItem` in `buffer_write.go` skips the count
check when the sentinel is present. The validators in
`validation.go` and the bulk paths in `bulk.go` consult
`isReservedScope` to enforce the scope-level rejection contract;
`handleAppend` in `handlers_write.go` consults
`maxItemBytesFor(item.Scope)` to enforce the per-item byte cap.

Future addon-convention scopes that need pre-creation (e.g.
`_tokens` for the `guarded` addon) are not part of the core's
reservation list — they are operator-side concerns and will land
via the planned config-driven init-script (see CLAUDE.md
"Pre-1.0 TODO operational" for the deferred design).

---

## 3. Capacity and limits

The cache enforces the following capacity limits on every write
path. All knobs below are configurable via `Config` (Go API),
env-vars (standalone binary), or Caddyfile / JSON directives (Caddy
module). Setting a value to its zero / empty form (0 for integers,
`""` for strings) selects the default.

### 3.0 Knob mapping

Single canonical mapping from Go API to adapter wire-shape. Defaults
in the rightmost column reflect what `Config{}.WithDefaults()` plus
`NewGateway` produce out of the box.

| Go-API field                  | Env-var                              | Caddyfile / JSON       | Default       |
|-------------------------------|--------------------------------------|------------------------|---------------|
| `Config.ScopeMaxItems`        | `SCOPECACHE_SCOPE_MAX_ITEMS`         | `scope_max_items`      | 100,000 items |
| `Config.MaxStoreBytes`        | `SCOPECACHE_MAX_STORE_MB` *(MiB)*    | `max_store_mb`         | 100 MiB       |
| `Config.MaxItemBytes`         | `SCOPECACHE_MAX_ITEM_MB` *(MiB)*     | `max_item_mb`          | 1 MiB         |
| `Config.Inbox.MaxItems`       | `SCOPECACHE_INBOX_MAX_ITEMS`         | `inbox_max_items`      | = `ScopeMaxItems` |
| `Config.Inbox.MaxItemBytes`   | `SCOPECACHE_INBOX_MAX_ITEM_KB` *(KiB)* | `inbox_max_item_kb`  | 64 KiB        |
| `Config.Events.Mode`          | `SCOPECACHE_EVENTS_MODE`             | `events_mode`          | `off`         |
| *(adapter-level)*             | `SCOPECACHE_SUBSCRIBER_COMMAND`      | `subscriber_command`   | *(empty — no subscriber spawned)* |

The last row is an **adapter-level knob**, not a `Config` field. It
activates the in-core subscriber bridge at boot — see §7.5 for the
contract.

The remaining caps in the cache are **derived** from the rows above
and not exposed as knobs:

- **`_events` per-item byte cap** = `MaxItemBytes + 1 KiB envelope
  slack`. A log entry must always fit the user-write that produced
  it (§2.6).
- **`_events` per-scope item cap** = unbounded. `_events` is
  best-effort observability; the global byte budget is the only
  real begrenzer (§2.6).
- **HTTP response cap** (`/head`, `/tail`, `/render`) =
  `MaxStoreBytes`. By construction no scope can exceed the store
  budget, so any single-scope read fits in one response.

### Per-cap behaviour

Per-cap enforcement detail follows in §3.1-§3.4 for the global
caps; the reserved-scope-specific caps (Inbox, Events) are covered
in §2.6.

| limit             | scope        | default   | exceeded → |
|-------------------|--------------|-----------|------------|
| `ScopeMaxItems`   | per-scope    | 100,000   | 507        |
| `MaxItemBytes`    | per-item     | 1 MiB     | 400        |
| `MaxStoreBytes`   | store-wide   | 100 MiB   | 507        |

### 3.1 Per-scope item cap

Writes that would push the per-scope item count past
`ScopeMaxItems` are rejected with `507 Insufficient Storage`. The
response body identifies the offending scope and its current count:

```json
{
  "ok": false,
  "error": "scope is at capacity",
  "scopes": [{"scope": "...", "count": 100000, "cap": 100000}]
}
```

The cache never auto-evicts. Clients free space by deleting items
(`/delete_up_to`, `/delete`) or replacing the scope contents
(`/warm`).

### 3.2 Per-item byte cap

The size of an item is the sum of its scope, id, fixed-overhead, and
payload bytes (see `approxItemSize` in code). Writes whose item
exceeds `MaxItemBytes` are rejected with `400 Bad Request` — this
is a request-shape error, not capacity exhaustion.

### 3.3 Store-wide byte cap

Writes that would push the aggregate stored-item bytes past
`MaxStoreBytes` are rejected with `507 Insufficient Storage`. The
response body reports current usage, the attempted addition, and the
cap:

```json
{
  "ok": false,
  "error": "store is at byte capacity",
  "approx_store_mb": 99.9,
  "added_mb": 0.1,
  "max_store_mb": 100.0
}
```

This is the cache-wide equivalent of `ScopeMaxItems`. Free space by
deletion as before.

### 3.4 No automatic eviction

The cache never evicts. There is no LRU, no LFU, no TTL, no
background sweeper. Whatever you write stays until you delete it
(or until process restart, which clears the entire cache by
definition — the cache is in-memory only).

Operator tools to manage capacity:

- `/delete_up_to` — drain a `seq`-prefix in one call (write-buffer
  pattern)
- `/delete_scope` — remove a whole scope
- `/wipe` — clear every scope, every item, every byte
- `/warm` — atomically replace a scope's contents (frees the
  previous contents' bytes in the same call)
- `/rebuild` — atomically replace the entire store

Read-bookkeeping metadata (§9) helps operators identify which
scopes are cold enough to evict, but the cache itself never
decides.

---

## 4. Validation

All write paths share the same validation pass before the request
reaches the store. Errors are returned as `400 Bad Request` with a
JSON body — see §5.2.

### 4.1 Field-shape rules

Validation rules for the fields that appear across the API:

| field       | type            | shape rule                                               |
|-------------|-----------------|----------------------------------------------------------|
| `scope`     | string          | required; ≤ 256 bytes; no leading/trailing whitespace; no control characters (0x00–0x1F, 0x7F) |
| `id`        | string          | optional or required (per endpoint); same shape as `scope` when present |
| `payload`   | any JSON value  | required; literal `null` is rejected; bytes are opaque to the cache |
| `seq`       | uint64          | cache-assigned; clients must omit on every write; reads accept it as an addressing key |
| `ts`        | int64           | cache-assigned; clients must omit on every write |
| `by`        | int64           | required for `/counter_add`; non-zero; within ±(2^53 − 1) |
| `max_seq`   | uint64          | required for `/delete_up_to`; must be > 0 |

Per-item byte size (the sum of `scope`, `id`, fixed overhead, and
payload bytes — see `approxItemSize` in code) is checked against
`MaxItemBytes` after field-shape validation. An over-cap item is
rejected with `400 Bad Request`. See §3.2.

### 4.2 Query parameters

Read endpoints accept query parameters with the following rules:

| parameter   | type    | default | rule                                          |
|-------------|---------|---------|-----------------------------------------------|
| `scope`     | string  | —       | same shape rules as the body field            |
| `id`        | string  | —       | same shape rules as the body field            |
| `seq`       | uint64  | —       | parsed as unsigned integer                    |
| `limit`     | int     | 1000    | must be > 0; values above 10000 are clamped, not rejected |
| `offset`    | int     | 0       | must be ≥ 0                                   |
| `after_seq` | uint64  | 0       | parsed as unsigned integer; 0 means "from the start" |

Single-item read endpoints (`/get`, `/render`) require exactly one
of `id` or `seq`. Supplying both, or neither, is rejected with
`400 Bad Request`.

### 4.3 Why the cache rejects client-supplied `seq` and `ts`

Both fields are owned by the cache and stamped on every write that
touches an item. Accepting client-supplied values would silently
break the invariant that `seq` is monotonic per scope and that `ts`
reflects the cache's own write time. The validator rejects them
with an explicit error rather than overwriting silently — clients
that need a "client timestamp" can carry it inside `payload`, where
the cache stays opaque.

---

## 5. HTTP contract

### 5.1 Response envelope

Successful responses use a JSON envelope. The shape of the envelope
varies per endpoint, but two fields are universal:

- `ok` — boolean, always present, `true` on success and `false` on
  error.
- `duration_us` — integer, always present, the handler's internal
  duration in microseconds (measured from the start of request
  processing to the start of response write).

Read endpoints whose response size scales with the request (`/get`,
`/head`, `/tail`) additionally include:

- `approx_response_mb` — number, the approximate marshalled size
  of the response body in MiB (4-decimal precision).

Two endpoints break the JSON-envelope rule by design:

- **`/render`** — returns raw payload bytes (or empty body on
  miss); see §6.4.
- **`/help`** — returns `text/plain`; see §6.5.

### 5.2 Error envelope

Error responses use the same JSON envelope shape with `ok: false`
and a string `error` field describing the failure. Capacity errors
(`507`) include additional structured fields naming the offending
scope or store-wide totals — see §3.

```json
{
  "ok": false,
  "error": "the 'scope' field is required for the '/append' endpoint",
  "duration_us": 12
}
```

### 5.3 Status codes

The cache uses a small, deterministic set of HTTP status codes:

| status | meaning                                     | examples                                |
|--------|---------------------------------------------|-----------------------------------------|
| 200    | success                                     | every successful operation              |
| 400    | request-shape error (validation, parse)     | missing field, oversize item, malformed |
| 404    | resource not found (raw-bytes endpoint only)| `/render` miss                          |
| 405    | method not allowed                          | `GET /append`, `POST /get`              |
| 409    | scope detached mid-flight                   | concurrent `/wipe` or `/delete_scope`   |
| 507    | capacity exceeded                           | per-scope or store-wide cap reached     |

The JSON-envelope reads (`/get`, `/head`, `/tail`) deliberately do
**not** use 404 for misses. A miss is a successful query that
happened to find nothing; the envelope carries `hit: false` instead.
This keeps client error-handling on read paths simple — only network
failures and 4xx-as-validation-errors need attention; misses are
ordinary results.

### 5.4 Content types

| endpoint            | response content-type                      |
|---------------------|--------------------------------------------|
| every JSON endpoint | `application/json; charset=utf-8`          |
| `/render`           | `application/octet-stream`                 |
| `/help`             | `text/plain; charset=utf-8`                |

`/render` deliberately uses `application/octet-stream` — a neutral
default that the fronting proxy is expected to override per-route
(`header Content-Type text/html`, etc.). The cache does not sniff
content or guess the real MIME type.

### 5.5 Method matching

Every endpoint accepts exactly one HTTP method (`GET` for reads
and observability, `POST` for writes and bulk operations). Calling
an endpoint with the wrong method returns `405 Method Not Allowed`
with the standard error envelope. The exact method per endpoint is
listed in §6.

---

## 6. Endpoints

scopecache exposes the following endpoints, grouped by purpose:

| group                       | endpoints                                                  |
|-----------------------------|------------------------------------------------------------|
| §6.1 Single-item writes     | `/append`, `/upsert`, `/update`, `/counter_add`            |
| §6.2 Deletes                | `/delete`, `/delete_up_to`, `/delete_scope`, `/wipe`       |
| §6.3 Bulk                   | `/warm`, `/rebuild`                                         |
| §6.4 Reads                  | `/get`, `/render`, `/head`, `/tail`                         |
| §6.5 Observability          | `/stats`, `/scopelist`, `/help`                             |

### Endpoint conventions

To keep per-endpoint sections compact, the following errors are
implicit on every endpoint and are not repeated:

- **`405 Method Not Allowed`** — wrong HTTP method (§5.5).
- **`400 Bad Request`** — request-shape errors from §4 (missing
  required field, oversized item, malformed JSON, client-supplied
  `seq` or `ts`, etc.).

In addition, the following errors are implicit on every **write**
endpoint (§6.1, §6.2, §6.3):

- **`409 Conflict`** — `scope was deleted while the request was in
  flight; please retry`. Fires when a concurrent `/wipe`,
  `/delete_scope`, or `/rebuild` detached the scope between the
  handler's lookup and its mutation.
- **`507 Insufficient Storage`** — per-scope or store-wide capacity
  exceeded (§3.1, §3.3).

Each endpoint section lists only the errors that are specific to
it (typically: the body fields it requires, and any
endpoint-specific 4xx that does not fit the universal patterns
above).

### 6.1 Single-item writes

#### `POST /append`

Insert a new item into a scope. Rejects on duplicate `id`-in-scope.

**Request body**

| field     | type           | required | notes                                |
|-----------|----------------|----------|--------------------------------------|
| `scope`   | string         | yes      | shape per §4.1                       |
| `id`      | string         | no       | shape per §4.1; cache assigns if absent |
| `payload` | any JSON value | yes      | not literal `null`                   |

**Response (200)**

```json
{
  "ok": true,
  "item": {"scope": "events", "id": "e1", "seq": 1, "ts": 1700000000000000},
  "duration_us": 42
}
```

The `item` object echoes the stored `scope`, `id`, `seq`, and `ts`.
Payload bytes are not echoed (they doubled the wire cost on the
write path that just delivered them).

**Endpoint-specific errors**

| status | error                                  | when                          |
|--------|----------------------------------------|-------------------------------|
| 409    | `the 'id' is already in use`           | duplicate `id` within `scope` |

**Example**

```bash
curl -s -X POST http://localhost:8080/append \
  -H 'Content-Type: application/json' \
  -d '{"scope":"events","id":"e1","payload":{"v":1}}'
# → {"ok":true,"item":{"scope":"events","id":"e1","seq":1,"ts":...},"duration_us":42}
```

---

#### `POST /upsert`

Insert a new item, or replace an existing one with the same
`scope`+`id`. On replace, `seq` is preserved and `ts` is
refreshed. Always succeeds (within capacity); the response
distinguishes create from replace via `created`.

**Request body**

| field     | type           | required | notes                                |
|-----------|----------------|----------|--------------------------------------|
| `scope`   | string         | yes      | shape per §4.1                       |
| `id`      | string         | yes      | shape per §4.1                       |
| `payload` | any JSON value | yes      | not literal `null`                   |

**Response (200)**

```json
{
  "ok": true,
  "created": false,
  "item": {"scope": "events", "id": "e1", "seq": 5, "ts": 1700000000000000},
  "duration_us": 42
}
```

`created` is `true` when the item did not exist before this call,
`false` when an existing item was replaced. On replace, `seq`
keeps its original value; on create, `seq` is freshly assigned.
`ts` is always refreshed.

**Endpoint-specific errors**

| status | error                                  | when                          |
|--------|----------------------------------------|-------------------------------|
| 400    | `scope '<name>' is reserved …`         | `scope` is `_events` or `_inbox` (§2.6) |

**Example**

```bash
curl -s -X POST http://localhost:8080/upsert \
  -H 'Content-Type: application/json' \
  -d '{"scope":"events","id":"e1","payload":{"v":2}}'
# → {"ok":true,"created":false,"item":{"scope":"events","id":"e1","seq":5,"ts":...},"duration_us":42}
```

---

#### `POST /update`

Modify the payload of an existing item, addressed by `scope`+`id`
or `scope`+`seq`. Soft-misses on a non-existent item (returns 200
with `hit: false`).

**Request body**

| field     | type           | required               | notes                                |
|-----------|----------------|------------------------|--------------------------------------|
| `scope`   | string         | yes                    | shape per §4.1                       |
| `id`      | string         | exactly one of id/seq  | shape per §4.1 when present          |
| `seq`     | uint64         | exactly one of id/seq  | parsed as unsigned integer           |
| `payload` | any JSON value | yes                    | not literal `null`                   |

**Response (200)**

```json
{"ok": true, "hit": true, "updated_count": 1, "duration_us": 42}
```

- `hit` — whether an item was found and updated (`updated_count > 0`).
- `updated_count` — number of items modified (always 0 or 1 since
  `id`/`seq` is unique-in-scope).

**Side effects**

`ts` is refreshed on a hit. `seq` is preserved. If the new payload
is larger than the old one and would push the store past
`MaxStoreBytes`, the request is rejected with 507 and no change is
applied.

**Endpoint-specific errors**

| status | error                                  | when                          |
|--------|----------------------------------------|-------------------------------|
| 400    | `scope '<name>' is reserved …`         | `scope` is `_events` or `_inbox` (§2.6) |

**Example**

```bash
curl -s -X POST http://localhost:8080/update \
  -H 'Content-Type: application/json' \
  -d '{"scope":"events","id":"e1","payload":{"v":3}}'
# → {"ok":true,"hit":true,"updated_count":1,"duration_us":42}
```

---

#### `POST /counter_add`

Atomically increment (or create) a numeric counter at `scope`+`id`
by `by`. The only endpoint that reads or mutates a payload as a
typed value — every other write path treats payloads as opaque
bytes.

**Request body**

| field   | type    | required | notes                                                      |
|---------|---------|----------|------------------------------------------------------------|
| `scope` | string  | yes      | shape per §4.1                                             |
| `id`    | string  | yes      | shape per §4.1                                             |
| `by`    | int64   | yes      | non-zero; within ±(2^53 − 1)                               |

**Response (200)**

```json
{"ok": true, "created": false, "value": 7, "duration_us": 42}
```

- `created` — `true` when the counter did not exist (item created
  with payload `by`); `false` when an existing counter was
  incremented.
- `value` — the post-increment counter value.

**Endpoint-specific errors**

| status | error                                                      | when                                              |
|--------|------------------------------------------------------------|---------------------------------------------------|
| 400    | `the counter operation would exceed the allowed range of ±(2^53-1)` | result would overflow the JS-safe integer range |
| 400    | `scope '<name>' is reserved …`                             | `scope` is `_events` or `_inbox` (§2.6)              |
| 409    | `payload is not a JSON integer` (or similar)               | existing item's payload is not a valid integer    |

**Side effects**

The counter value is incremented atomically by `by`. Increments run
lock-free under the scope's read lock (CAS on an atomic int64 sidecar
on the item) — concurrent `/counter_add` calls on the same counter do
not serialise on each other or on read endpoints, only on actual
mutations of the same scope (`/append`, `/upsert`, `/delete`).

A worst-case payload reservation (max int64 width plus cell heap) is
charged against the per-item and store-wide caps once at creation
time, so subsequent increments never re-evaluate the byte budget;
`99 → 100` and `999_999 → 1_000_000` are both free at the cap level.

`item.ts` (visible via `/get`) advances on every successful
increment. The scope's `lastWriteTS` and `/stats.last_write_ts` do
NOT — see §2.4 for the rationale.

**Example**

```bash
curl -s -X POST http://localhost:8080/counter_add \
  -H 'Content-Type: application/json' \
  -d '{"scope":"hits","id":"page-42","by":1}'
# → {"ok":true,"created":false,"value":7,"duration_us":42}
```

### 6.2 Deletes

#### `POST /delete`

Delete a single item by `scope`+`id` or `scope`+`seq`. Soft-misses
on a non-existent item.

**Request body**

| field   | type   | required              | notes                       |
|---------|--------|-----------------------|-----------------------------|
| `scope` | string | yes                   | shape per §4.1              |
| `id`    | string | exactly one of id/seq | shape per §4.1 when present |
| `seq`   | uint64 | exactly one of id/seq | parsed as unsigned integer  |

**Response (200)**

```json
{"ok": true, "hit": true, "deleted_count": 1, "duration_us": 42}
```

**Example**

```bash
curl -s -X POST http://localhost:8080/delete \
  -H 'Content-Type: application/json' \
  -d '{"scope":"events","id":"e1"}'
# → {"ok":true,"hit":true,"deleted_count":1,"duration_us":42}
```

---

#### `POST /delete_up_to`

Drain a `seq`-prefix from a scope: removes every item with
`seq ≤ max_seq`. The write-buffer drain primitive — pair with
`/tail` to read items, then `/delete_up_to` with the last drained
`seq` to release the buffer.

**Request body**

| field      | type   | required | notes                          |
|------------|--------|----------|--------------------------------|
| `scope`    | string | yes      | shape per §4.1                 |
| `max_seq`  | uint64 | yes      | must be > 0                    |

**Response (200)**

```json
{"ok": true, "hit": true, "deleted_count": 100, "duration_us": 42}
```

`deleted_count` is the number of items actually removed (may be 0
if no items had `seq ≤ max_seq`).

**Example**

```bash
curl -s -X POST http://localhost:8080/delete_up_to \
  -H 'Content-Type: application/json' \
  -d '{"scope":"events","max_seq":100}'
# → {"ok":true,"hit":true,"deleted_count":100,"duration_us":42}
```

---

#### `POST /delete_scope`

Remove a whole scope, including its buffer and all its items.
Soft-misses on a non-existent scope.

**Request body**

| field   | type   | required | notes          |
|---------|--------|----------|----------------|
| `scope` | string | yes      | shape per §4.1 |

**Response (200)**

```json
{"ok": true, "hit": true, "deleted_scope": true, "deleted_items": 42, "duration_us": 42}
```

- `hit` / `deleted_scope` — `true` when the scope existed and was
  removed; `false` when the scope did not exist.
- `deleted_items` — the number of items the scope held at deletion
  time.

**Side effects**

In-flight writes against the scope detach (return 409 to their
callers); the scope's bytes are released back to the store-wide
budget.

**Endpoint-specific errors**

| status | error                                  | when                          |
|--------|----------------------------------------|-------------------------------|
| 400    | `scope '<name>' is reserved …`         | `scope` is `_events` or `_inbox` (§2.6); use `/delete_up_to` to release items without removing the scope |

**Example**

```bash
curl -s -X POST http://localhost:8080/delete_scope \
  -H 'Content-Type: application/json' \
  -d '{"scope":"events"}'
# → {"ok":true,"hit":true,"deleted_scope":true,"deleted_items":42,"duration_us":42}
```

---

#### `POST /wipe`

Clear every scope, every item, and every byte reservation in one
call. The store-wide complement of `/delete_scope` — a single
operation rather than N per-scope calls.

**Request body**

None. A non-empty body is silently ignored. The cache caps the body
at 1 KiB to prevent memory abuse.

**Response (200)**

```json
{"ok": true, "deleted_scopes": 12, "deleted_items": 5400, "freed_mb": 12.3456, "duration_us": 42}
```

**Side effects**

In-flight writes against any scope detach (409). The reserved
scopes `_events` and `_inbox` (§2.6) are dropped along with everything
else, then immediately re-created under the same all-shard write
lock — subscribers attached to either reserved scope do not observe
a gap. Post-call baseline is `scope_count = 2`,
`total_items = 0`, `approx_store_mb` ≈ `reservedScopesOverhead /
1 MiB` (~0.002 MiB by default).

This is **not** an eviction policy: the cache never wipes itself.
`/wipe` exists so operators can clear-and-rebuild atomically rather
than coordinating N delete calls.

**Example**

```bash
curl -s -X POST http://localhost:8080/wipe
# → {"ok":true,"deleted_scopes":12,"deleted_items":5400,"freed_mb":12.3456,"duration_us":42}
```

### 6.3 Bulk

#### `POST /warm`

Atomically replace the contents of one or more scopes with the
items in the request body. Scopes not mentioned in the body are
left alone. Old contents are released in the same call (the
freed bytes can be reused by the new contents within the same
capacity check).

**Request body**

```json
{"items": [{"scope":"...","id":"...","payload":...}, ...]}
```

Each item validates against the same rules as `/append` (§4.1).
Items are grouped by `scope` server-side; every scope mentioned in
the batch is replaced as a unit.

**Response (200)**

```json
{"ok": true, "count": 100, "replaced_scopes": 3, "duration_us": 42}
```

- `count` — total number of items written.
- `replaced_scopes` — number of distinct scopes touched by the
  replacement.

**Side effects**

The request body cap for `/warm` is much larger than for
single-item writes — large enough that a fully-loaded store can
always be expressed as one bulk call. In-flight writes against any
replaced scope detach (409).

**Endpoint-specific errors**

| status | error                                  | when                          |
|--------|----------------------------------------|-------------------------------|
| 400    | `scope '<name>' is reserved …`         | any item's `scope` is `_events` or `_inbox` (§2.6) |

**Example**

```bash
curl -s -X POST http://localhost:8080/warm \
  -H 'Content-Type: application/json' \
  -d '{"items":[{"scope":"events","id":"e1","payload":{"v":1}}]}'
# → {"ok":true,"count":1,"replaced_scopes":1,"duration_us":42}
```

---

#### `POST /rebuild`

Atomically replace the entire store. Every scope and every item is
discarded; the request body becomes the new state. Equivalent to
`/wipe` immediately followed by `/warm` for every scope, but in a
single atomic operation.

**Request body**

```json
{"items": [{"scope":"...","id":"...","payload":...}, ...]}
```

**Endpoint-specific errors**

| status | error                                              | when                  |
|--------|----------------------------------------------------|-----------------------|
| 400    | `the 'items' array must not be empty for the '/rebuild' endpoint` | empty `items[]`       |
| 400    | `scope '<name>' is reserved …`                     | any item's `scope` is `_events` or `_inbox` (§2.6) |

An empty `items[]` is rejected explicitly because it would silently
wipe the store — almost always a client bug. Operators that really
want to clear the store should call `/wipe`.

**Response (200)**

```json
{"ok": true, "count": 100, "rebuilt_scopes": 3, "rebuilt_items": 100, "duration_us": 42}
```

**Side effects**

After the swap, the cache re-creates the reserved scopes (`_events`,
`_inbox`) under the same all-shard write lock so subscribers don't
observe a gap (§2.6). The post-rebuild byte total is `Σ items +
Σ scopeBufferOverhead + reservedScopesOverhead`; the cap check
includes the reserved overhead so an input that fills the cap
exactly does not blow past it once init runs.

**Example**

```bash
curl -s -X POST http://localhost:8080/rebuild \
  -H 'Content-Type: application/json' \
  -d '{"items":[{"scope":"events","id":"e1","payload":{"v":1}}]}'
# → {"ok":true,"count":1,"rebuilt_scopes":1,"rebuilt_items":1,"duration_us":42}
```

### 6.4 Reads

#### `GET /get`

Look up a single item by `scope`+`id` or `scope`+`seq`. Returns
200 in both the hit and miss case; the response carries `hit:
true|false`. See §5.3 for why misses are not 404.

**Query parameters**

| parameter | type   | required              |
|-----------|--------|-----------------------|
| `scope`   | string | yes                   |
| `id`      | string | exactly one of id/seq |
| `seq`     | uint64 | exactly one of id/seq |

**Response (200, hit)**

```json
{
  "ok": true,
  "hit": true,
  "count": 1,
  "item": {"scope":"events","id":"e1","seq":1,"ts":1700000000000000,"payload":{"v":1}},
  "duration_us": 42,
  "approx_response_mb": 0.0001
}
```

**Response (200, miss)**

```json
{"ok": true, "hit": false, "count": 0, "item": null, "duration_us": 42, "approx_response_mb": 0.0001}
```

**Side effects**

A successful hit bumps the per-scope read-bookkeeping atomics (§9).

**Example**

```bash
curl -s 'http://localhost:8080/get?scope=events&id=e1'
# → {"ok":true,"hit":true,"count":1,"item":{...},"duration_us":42,"approx_response_mb":0.0001}
```

---

#### `GET /render`

Serve a single item's payload as raw bytes, with no JSON envelope.
Designed for fronting proxies (Caddy, nginx, apache) to pipe cached
HTML, JSON, XML, or text fragments straight to the client without
an application layer in between.

**Query parameters**

Same as `/get`: `scope`, plus exactly one of `id` or `seq`.

**Response (200, hit)**

- `Content-Type: application/octet-stream` (override at the proxy)
- Body: raw payload bytes, with one layer of JSON-string decoding
  if the stored payload is a JSON string. Other JSON values
  (object, array, number, boolean) are written verbatim.

**Response (404, miss)**

- `Content-Type: application/octet-stream`
- Empty body. `/render` is the **only** endpoint that uses 404 for
  a not-found resource — the use case (proxy-fronted byte
  streaming) does not benefit from a JSON envelope, so a status
  code is the lowest-friction signal.

**Side effects**

A successful hit bumps the per-scope read-bookkeeping atomics (§9).

**Example**

```bash
curl -i 'http://localhost:8080/render?scope=html&id=page-1'
# HTTP/1.1 200 OK
# Content-Type: application/octet-stream
#
# <html>...</html>
```

---

#### `GET /head`

Return the oldest items in a scope, optionally cursoring past a
given `after_seq`. Returns 200 in both the hit and miss case
(empty scope or unknown scope yields `hit: false`).

**Query parameters**

| parameter   | type   | default | notes                                       |
|-------------|--------|---------|---------------------------------------------|
| `scope`     | string | —       | required, shape per §4.1                    |
| `limit`     | int    | 1000    | clamped to ≤ 10000                          |
| `after_seq` | uint64 | 0       | return items with `seq > after_seq`         |

`offset` is **not** supported on `/head` — use `after_seq` for
cursor-based forward paging (stable under `/delete_up_to`), or
`/tail` for position-based paging.

**Response (200, hit)**

```json
{
  "ok": true,
  "hit": true,
  "count": 10,
  "truncated": false,
  "items": [{"scope":"events","id":"e1","seq":1,"ts":...,"payload":{"v":1}}, ...],
  "duration_us": 42,
  "approx_response_mb": 0.0042
}
```

`truncated` is `true` when more items exist beyond the returned
`limit` window.

**Response (200, miss)** — empty scope or unknown scope:

```json
{"ok": true, "hit": false, "count": 0, "truncated": false, "items": [], "duration_us": 42, "approx_response_mb": 0.0001}
```

**Endpoint-specific errors**

| status | error                                                     | when                       |
|--------|-----------------------------------------------------------|----------------------------|
| 507    | `the response would exceed the maximum allowed size`      | response > `MaxResponseBytes` |

**Side effects**

A successful hit bumps the per-scope read-bookkeeping atomics (§9).

**Example**

```bash
curl -s 'http://localhost:8080/head?scope=events&limit=10'
# → {"ok":true,"hit":true,"count":10,"truncated":false,"items":[...],"duration_us":42,...}
```

---

#### `GET /tail`

Return the newest items in a scope, optionally offset back from the
tail. Returns 200 in both the hit and miss case.

**Query parameters**

| parameter | type   | default | notes                                       |
|-----------|--------|---------|---------------------------------------------|
| `scope`   | string | —       | required, shape per §4.1                    |
| `limit`   | int    | 1000    | clamped to ≤ 10000                          |
| `offset`  | int    | 0       | skip this many items from the tail before reading |

**Response (200, hit)**

```json
{
  "ok": true,
  "hit": true,
  "count": 10,
  "offset": 0,
  "truncated": true,
  "items": [...],
  "duration_us": 42,
  "approx_response_mb": 0.0042
}
```

**Endpoint-specific errors**

| status | error                                                     | when                       |
|--------|-----------------------------------------------------------|----------------------------|
| 507    | `the response would exceed the maximum allowed size`      | response > `MaxResponseBytes` |

**Side effects**

A successful hit bumps the per-scope read-bookkeeping atomics (§9).

**Example**

```bash
curl -s 'http://localhost:8080/tail?scope=events&limit=10'
# → {"ok":true,"hit":true,"count":10,"offset":0,"truncated":true,"items":[...],"duration_us":42,...}
```

### 6.5 Observability

#### `GET /stats`

Return a **store-wide aggregate snapshot**: scope count, total item
count, approximate stored bytes, the freshness tick, the auto-populate
drop counter, and a small fixed per-scope block for the reserved
scopes (`_events`, `_inbox`). No enumeration of user-managed scopes —
that lives on `/scopelist` (paginated, with optional prefix filter).
Per-field semantics for the aggregates are documented in §2.5.

**Query parameters**

None.

**Response (200)**

```json
{
  "ok": true,
  "scope_count": 14,
  "total_items": 5400,
  "approx_store_mb": 12.3456,
  "last_write_ts": 1700000000123456,
  "events_drops_total": 0,
  "reserved_scopes": [
    {
      "scope": "_events",
      "item_count": 0,
      "last_seq": 0,
      "approx_scope_mb": 0.0001,
      "created_ts": 1700000000000000,
      "last_write_ts": 1700000000000000
    },
    {
      "scope": "_inbox",
      "item_count": 0,
      "last_seq": 0,
      "approx_scope_mb": 0.0001,
      "created_ts": 1700000000000000,
      "last_write_ts": 0
    }
  ],
  "duration_us": 0
}
```

**Field semantics**

- `scope_count` — total number of scopes in the cache, **including**
  the two reserved scopes. A freshly-wiped store has `scope_count=2`,
  not `0`, because the reservation contract recreates `_events` and
  `_inbox` immediately on `/wipe` and `/rebuild` (§2.6).
- `total_items` — sum of items across all scopes, including reserved.
  When `events_mode=full` this also counts the auto-populated event
  entries.
- `approx_store_mb` — sum of `approxItemSize(item)` over every item
  plus `scopeBufferOverhead` per scope. Independent of the number of
  scopes; counts items + per-scope overhead, not Go heap overhead per
  scope (§2.4).
- `last_write_ts` — the freshness-tick polling client `last_write_ts`
  (§2.5). Strictly advances on every write/delete/bulk-op including
  `/wipe`.
- `events_drops_total` — monotonic counter of auto-populate event
  emits that the cache had to drop because `_events` was at the byte
  cap. **The user write that triggered the emit always succeeds** —
  the drop is a degraded observability signal, not a degraded
  primary operation. Operators monitor this for slow / dead
  subscriber + cap-undersized deployments. Defensive: also bumped on
  the (effectively unreachable) `json.Marshal` failure path.
- `reserved_scopes` — bounded array (currently length 2) carrying the
  per-scope state of the cache's infrastructure scopes. Per-row
  fields mirror `/scopelist`'s shape minus the read-bookkeeping
  signals (`last_access_ts`, `read_count_total`) which are noise on
  reserved scopes that no user-facing traffic reads. Bounded by the
  reserved-scope set, so `/stats` stays O(1) regardless of total
  scope count.

**Cost**

`/stats` is **O(1)**: five atomic loads (`scope_count`, `total_items`,
`approx_store_mb`, `last_write_ts`, `events_drops_total`) plus two
`getScope() + buf.stats()` materialisations for the reserved-scope
rows. Cost is independent of the number of user-managed scopes in the
store. Each aggregate counter is maintained incrementally on every
write/delete/bulk path. See §2.5 for the polling pattern that
`last_write_ts` enables. Configured caps are NOT echoed here — they
are static config and live on `/help`.

The previous shape included a per-scope `scopes` map keyed by scope
name across **all** user scopes. At 100k+ scopes that response
routinely blew past practical client and proxy response-size limits,
and the per-scope enumeration (one `buffer.stats()` materialisation
per scope) dominated `/stats` latency. Per-scope listing of user
scopes moved to a dedicated paginated endpoint (`/scopelist`); the
small fixed `reserved_scopes` block is the bounded exception so
operators can monitor drainer-backlog and fan-in queue depth without
paging.

**Side effects**

None. `/stats` is read-only — no user-scope names appear in the
response, so it does not leak the per-tenant identifier surface
(`_*` scopes etc.) that the per-scope map previously did. The
reserved-scope names are compile-time constants (settled #19), not
operator-facing identifiers, so listing them carries no tenant
information. Operators may still wish to gate `/stats` for other
reasons (capacity-disclosure, side-channel timing); see §1.3.

**Example**

```bash
curl -s 'http://localhost:8080/stats' | jq .events_drops_total
# → 0   # alert on > 0 in your monitoring pipeline
```

---

#### `GET /scopelist`

Return per-scope detail rows in alphabetical order, with an optional
prefix filter and cursor pagination by name. The per-scope counterpart
of `/stats`: where `/stats` is store-wide aggregate, `/scopelist` walks
the scopes themselves and surfaces the seven primitives §2.4 maintains
on every buffer.

**Query parameters**

| parameter | type   | default | notes                                                       |
|-----------|--------|---------|-------------------------------------------------------------|
| `prefix`  | string | —       | optional, literal `strings.HasPrefix` filter on scope name; shape per §4.1 when present |
| `after`   | string | —       | optional cursor; returns scopes whose name `>` `after` (strict); shape per §4.1 when present |
| `limit`   | int    | 1000    | clamped to ≤ 10000                                          |

Sort order is alphabetical, the only mode shipped. Scope names do not
move once created, so the cursor stays stable under concurrent writes
— resuming with `?after=<last-scope-of-previous-page>` walks the next
page deterministically. Other ranking modes (by byte size, by
read-count) are explicitly out of core; addons that want them poll
`/scopelist` and rank client-side.

**Response (200)**

```json
{
  "ok": true,
  "hit": true,
  "count": 2,
  "truncated": true,
  "scopes": [
    {
      "scope": "events",
      "item_count": 42,
      "last_seq": 50,
      "approx_scope_mb": 0.0042,
      "created_ts": 1700000000000000,
      "last_write_ts": 1700000001234567,
      "last_access_ts": 1700000002345678,
      "read_count_total": 99
    },
    {
      "scope": "thread:42",
      "item_count": 3,
      "last_seq": 3,
      "approx_scope_mb": 0.0008,
      "created_ts": 1700000001000000,
      "last_write_ts": 1700000001500000,
      "last_access_ts": 0,
      "read_count_total": 0
    }
  ],
  "duration_us": 42,
  "approx_response_mb": 0.0009
}
```

- `hit` — `true` when at least one scope matched, `false` otherwise.
  Equivalent to `count > 0`; carried for symmetry with `/head` and
  `/tail` so the list-return read family shares one wire prefix.
- `count` — number of scopes in this page.
- `truncated` — `true` when more matching scopes exist past the limit;
  resume by repeating the call with `?after=<last scope of this page>`.
- `scopes` — array of per-scope detail rows; field semantics per §2.4.
  Empty array (and `hit: false`, `count: 0`, `truncated: false`) when
  no scopes match.

**Endpoint-specific errors**

| status | error                                                | when                                  |
|--------|------------------------------------------------------|---------------------------------------|
| 507    | `the response would exceed the maximum allowed size` | response > `MaxResponseBytes`         |

**Cost**

`O(N)` walk across every shard map (under each shard's RLock) to apply
the prefix and `after` filters, `O(M log M)` sort on the surviving
names where M is the filtered count, and `O(limit)` `buf.stats()`
materialisations once the locks are released. Filtering happens inside
the walk so a narrow prefix is much cheaper than a full enumeration.

This is fundamentally an `O(scope_count)` endpoint — unlike `/stats`,
which is `O(1)` — so use it for periodic enumeration and addon polling
rather than per-request hot paths.

**Side effects**

None. `/scopelist` is observability and does NOT bump per-scope
read-bookkeeping (§9). An addon that polls `/scopelist` to compute
read-count deltas would otherwise see its own polls inflate the
counters it is trying to measure.

**Examples**

Three illustrative calls against a store that holds an `events` scope
plus a handful of `thread:42:*` scopes. Responses are abbreviated for
readability: `created_ts`, `last_write_ts`, `last_access_ts`,
`read_count_total`, `duration_us`, and `approx_response_mb` are present
on the wire but omitted from the prose below.

*First page, every scope:*

```bash
curl -s 'http://localhost:8080/scopelist?limit=100'
```

```json
{
  "ok": true,
  "hit": true,
  "count": 4,
  "truncated": false,
  "scopes": [
    {"scope": "events",        "item_count": 42, "last_seq": 50, "approx_scope_mb": 0.0042, ...},
    {"scope": "thread:42:abc", "item_count": 3,  "last_seq": 3,  "approx_scope_mb": 0.0008, ...},
    {"scope": "thread:42:def", "item_count": 5,  "last_seq": 5,  "approx_scope_mb": 0.0011, ...},
    {"scope": "thread:42:xyz", "item_count": 1,  "last_seq": 1,  "approx_scope_mb": 0.0003, ...}
  ]
}
```

*Prefix search — narrow to one tenant's footprint:*

```bash
curl -s 'http://localhost:8080/scopelist?prefix=thread:42:&limit=100'
```

```json
{
  "ok": true,
  "hit": true,
  "count": 3,
  "truncated": false,
  "scopes": [
    {"scope": "thread:42:abc", "item_count": 3, "last_seq": 3, "approx_scope_mb": 0.0008, ...},
    {"scope": "thread:42:def", "item_count": 5, "last_seq": 5, "approx_scope_mb": 0.0011, ...},
    {"scope": "thread:42:xyz", "item_count": 1, "last_seq": 1, "approx_scope_mb": 0.0003, ...}
  ]
}
```

The filter is literal `strings.HasPrefix`: `prefix=thread:42:` matches
every scope name starting with that exact byte sequence and skips
`events`. There is no glob, regex, or `:` parsing — `thread:` would
match every `thread:*:*` scope across tenants, `thread:42:` narrows
to one.

*Cursor pagination — combine `prefix` with `limit` and `after` to walk
a tenant's scopes in pages:*

```bash
curl -s 'http://localhost:8080/scopelist?prefix=thread:42:&limit=2'
```

```json
{
  "ok": true,
  "hit": true,
  "count": 2,
  "truncated": true,
  "scopes": [
    {"scope": "thread:42:abc", "item_count": 3, ...},
    {"scope": "thread:42:def", "item_count": 5, ...}
  ]
}
```

`truncated: true` signals there is more behind this page. Resume by
passing the last `scope` value as `after`:

```bash
curl -s 'http://localhost:8080/scopelist?prefix=thread:42:&limit=2&after=thread:42:def'
```

```json
{
  "ok": true,
  "hit": true,
  "count": 1,
  "truncated": false,
  "scopes": [
    {"scope": "thread:42:xyz", "item_count": 1, ...}
  ]
}
```

`truncated: false` on this page means the walk is complete.

---

#### `GET /help`

Return a one-line plain-text pointer to the canonical RFC. Intended
as a low-maintenance self-documentation hook; rich endpoint listings
live in this RFC, not in `/help`.

**Query parameters**

None.

**Response (200)**

- `Content-Type: text/plain; charset=utf-8`
- Body: a single line of text, currently:

```
scopecache — see instructions at https://github.com/VeloxCoding/scopecache/blob/main/docs/scopecache-core-rfc.md
```

**Example**

```bash
curl -s 'http://localhost:8080/help'
# → scopecache — see instructions at https://...
```

---

## 7. Go API surface (`*scopecache.Gateway`)

Sections 5 and 6 describe the **HTTP** contract. This section
describes the symmetric **in-process Go** contract — the surface
addons, the standalone binary, the Caddy module, and any embedded
consumer reach the cache through.

### 7.1 What `*Gateway` is

`*scopecache.Gateway` is the **only** Go entry point into the
cache. The underlying `*store` and its lowercase methods are
unreachable from outside `package scopecache`; every external
caller — adapter, addon, test — holds a `*Gateway` and calls its
methods.

```
            HTTP world                      Go world
                ↓                              ↓
           *API (handlers)                *Gateway
                ↓                              ↓
                └─────── *store ───────────────┘
                          (internal)
```

Two paths into the same store, with different responsibilities:

- HTTP requests enter through `*API`. `NewAPI(gw, …)` extracts
  `gw.store` once at construction and dispatches every handler
  directly through `*store`. Gateway is bypassed; the request
  body is already detached from the wire by
  `json.RawMessage.UnmarshalJSON`.
- Go callers enter through `*Gateway`. Every data-plane method
  validates and clones (§7.3) before delegating to `*store`.
  Validation lives at the `*store` method top so callers cannot
  bypass shape rules by entering via Go instead of HTTP.

The package-internal rule, for completeness: in-package code
(events.go, subscribe.go, subscriber_command.go) reaches `*store`
directly, not via `*Gateway` — internal callers don't pay the
boundary tax. External code never has that option.

`NewGateway(c Config) *Gateway` is the only constructor. Adapters
pass a fully-populated `Config` (item caps, store-byte cap,
events mode, inbox tuning); see §3.0 for the knob mapping. The
returned `*Gateway` is ready to use; it owns the store and is
goroutine-safe across all methods.

### 7.2 Method catalog

Every public HTTP endpoint in §6 has a 1:1 `*Gateway` method with
the identical contract. The shape: HTTP request body fields become
Go method arguments; HTTP response fields become return values.

| HTTP endpoint   | `*Gateway` method                                                                       | Returns                              |
|-----------------|-----------------------------------------------------------------------------------------|--------------------------------------|
| `POST /append`         | `Append(item Item) (Item, error)`                                                | committed item (Seq + Ts assigned)   |
| `POST /upsert`         | `Upsert(item Item) (Item, bool, error)`                                          | item, `created` (true on new)        |
| `POST /update`         | `Update(item Item) (int, error)`                                                 | updated count (0 = miss)             |
| `POST /counter_add`    | `CounterAdd(scope, id string, by int64) (int64, bool, error)`                    | post-add value, `created`            |
| `POST /delete`         | `Delete(scope, id string, seq uint64) (int, error)`                              | deleted count                        |
| `POST /delete_up_to`   | `DeleteUpTo(scope string, maxSeq uint64) (int, error)`                           | deleted count                        |
| `POST /delete_scope`   | `DeleteScope(scope string) (int, bool, error)`                                   | item count, `found`                  |
| `POST /wipe`           | `Wipe() (int, int, int64)`                                                       | scopes, items, freed bytes           |
| `POST /warm`           | `Warm(grouped map[string][]Item) (int, error)`                                   | replaced scope count                 |
| `POST /rebuild`        | `Rebuild(grouped map[string][]Item) (int, int, error)`                           | scope count, item count              |
| `GET /get`             | `Get(scope, id string, seq uint64) (Item, bool)`                                 | item, hit                            |
| `GET /render`          | `Render(scope, id string, seq uint64) ([]byte, bool)`                            | rendered bytes, hit                  |
| `GET /head`            | `Head(scope string, afterSeq uint64, limit int) ([]Item, bool, bool)`            | items, truncated, scope_found        |
| `GET /tail`            | `Tail(scope string, limit, offset int) ([]Item, bool, bool)`                     | items, has_more, scope_found         |
| `GET /stats`           | `Stats() Stats`                                                                  | typed snapshot                       |
| `GET /scopelist`       | `ScopeList(prefix, after string, limit int) ([]ScopeListEntry, bool)`            | rows, truncated                      |

For single-item lookup (`Get`, `Render`, `Delete`), pass `id != ""`
to use the id path; pass `id == ""` and a non-zero `seq` to use
the seq path. The validator enforces the id-xor-seq invariant.

Two control-plane methods have no HTTP equivalent because they
return Go-only types (a channel, a stop function):

| `*Gateway` method                                                        | Purpose                                       |
|--------------------------------------------------------------------------|-----------------------------------------------|
| `Subscribe(scope) (<-chan struct{}, func(), error)`                      | wake-up channel for a reserved scope (§7.4)   |
| `StartSubscriber(scope, command string) (stop func(), err error)`        | spawn an exec-driven subscriber goroutine (§7.5) |

Errors returned by data-plane methods are typed and match the HTTP
status mapping in §5.3:

- `ErrInvalidInput` (wrapped) — bad shape: 400 over HTTP. Use
  `errors.Is(err, scopecache.ErrInvalidInput)`.
- `*ScopeFullError`, `*StoreFullError`, `*ScopeCapacityError` —
  capacity rejections: 507 over HTTP. Type-switch to read the
  fields (counts, caps, offenders).
- `*ScopeDetachedError` — concurrent destructive op detached the
  buffer mid-write: 409 over HTTP. Retry-safe.
- `*CounterPayloadError`, `*CounterOverflowError` — counter-
  specific rejections: 409 / 400 over HTTP.
- `ErrInvalidSubscribeScope`, `ErrAlreadySubscribed` — control-
  plane only; see §7.4.

### 7.3 Defensive payload-byte cloning

`Item.Payload` is `json.RawMessage` — a `[]byte`. Without
defensive copies, the slice the caller passes to `gw.Append`
and the slice the cache stores would share the same backing
array. A Go caller mutating their payload after `Append` returns
would silently mutate the cached item, **bypassing every lock the
cache holds**. The same hazard applies in reverse on items
returned from `Get` / `Head` / `Tail` / `Render`.

`*Gateway` closes this hazard by cloning at every boundary:

- **Entry clone.** `Append`, `Upsert`, `Update`, `Warm`, `Rebuild`
  copy the caller's `Item.Payload` (and every payload inside the
  `map[string][]Item` for the bulk paths) into a fresh allocation
  before delegating to `*store`. The caller may mutate or release
  their original slice immediately after the call returns; the
  cached state is independent.
- **Exit clone.** `Append` and `Upsert` (return the committed
  item), `Get`, `Head`, `Tail`, and `Render` (return cached state)
  copy out into fresh allocations before returning. The caller
  may mutate or retain the returned slice freely; cached state is
  independent.

`Subscribe` and `StartSubscriber` carry no payload bytes — they
return a wake-up channel and a stop function — so the clone
discipline does not apply. `Stats` and `ScopeList` return only
metadata. `CounterAdd`, `Delete`, `DeleteUpTo`, `DeleteScope`,
`Wipe` take and return value types only.

The HTTP path does not pay the cloning cost. `NewAPI` extracts
`gw.store` and dispatches handlers directly; the request body
arrives via `json.RawMessage.UnmarshalJSON`, which copies its
input by stdlib contract, so HTTP-decoded payloads are already
detached. Production HTTP traffic never crosses the Gateway
boundary.

For Go callers the cost scales with payload size: sub-microsecond
for ≤1 KiB items, memcpy-bound (~10–20 GB/s) for large payloads.
A 1 MiB single-clone is roughly 50 µs; an `Append` (entry + exit
clone) of the same is roughly twice that.

### 7.4 Subscribe primitive

`Subscribe` is the in-process Go primitive on top of which addons
build automatic drainers, write-event publishers, and reactive
pipelines. The core ships one ready-to-deploy bridge on top of it
(§7.5); custom Go subscribers are first-class — see the addon
skeleton in §7.9.

```go
ch, unsub, err := gw.Subscribe(scope)
```

- `scope` MUST be one of the reserved scopes (`_events` or
  `_inbox`); any other value returns `ErrInvalidSubscribeScope`.
  The cache deliberately does not allow subscribing to user
  scopes — `_events`'s auto-populate is the route to observe
  user-scope mutations.
- A second `Subscribe` to the same scope while the first is still
  active returns `ErrAlreadySubscribed`. Single subscriber per
  reserved scope is by design (multi-fan-out belongs in the
  subscriber, not in the cache).
- `ch` is a single-slot, size-1 buffered `chan struct{}`. The
  cache sends non-blockingly: when the slot is full the send is
  dropped. A burst of N writes during a busy subscriber coalesces
  into one wake-up; the subscriber catches up via cursor on its
  next drain. There is no slow-subscriber drop policy and no deep
  buffer.
- The channel survives `/wipe` and `/rebuild` transparently: the
  subscriber slot lives at the cache level (keyed by scope name),
  so when those destructive ops drop+recreate the reserved scope
  buffers, the subscriber stays attached and re-points at the new
  buffer. The subscriber detects wipe/rebuild via cursor-rewind on
  the next read (last-seen seq going backwards) and resets its own
  state.
- `unsub()` is idempotent: the second call after the entry is
  already gone is a no-op. This lets shutdown paths use a
  signal-handler call and a `defer` backstop without double-close
  panics.
- `unsub()` is graceful, not abortive: it closes the wake-up
  channel (so the subscriber's `for range ch` loop exits), but any
  work the subscriber goroutine had already started runs to
  completion.

### 7.5 StartSubscriber bridge

```go
stop, err := gw.StartSubscriber(scope, command)
```

The bridge spawns a goroutine that subscribes to `scope`, then on
every wake-up invokes `command` (any path that `exec.Command` can
run — shell scripts, Python/PHP/Ruby with shebang, compiled
binaries) and waits for it to exit. The goroutine never reads,
marshals, or writes item data — the command does that itself,
typically via `curl /tail` and `curl /delete_up_to` against the
cache's HTTP endpoints.

One environment variable is set per invocation:

- `SCOPECACHE_SCOPE` — the reserved scope that fired the wake-up
  (`_events` or `_inbox`). A single command can serve both
  scopes by branching on this value.

Everything else the command needs (cache socket path, HTTP base
URL, auth headers) is the operator's responsibility — the bridge
does not know how the command reaches the cache.

`stop` is graceful and synchronous: it closes the wake-up channel
**and** blocks until the goroutine has fully exited (including any
in-flight `cmd.Run` for the currently-executing command). This
lets adapters sequence "stop subscribers → tear down server"
deterministically — see §7.7 below for the standalone-binary
shutdown ordering.

Concurrency: one goroutine per scope, one command run at a time
per scope, in strict order. Wake-ups arriving while a command is
running coalesce in the cache's single-slot wake-up channel; the
next loop iteration sees one pending wake-up and triggers one more
command run.

### 7.6 Activation knob

The bridge is wired by the adapters via the single
`subscriber_command` knob (see §3.0):

- Standalone: `SCOPECACHE_SUBSCRIBER_COMMAND=/path/to/exec ./scopecache`
- Caddyfile: `subscriber_command /path/to/exec` inside the `scopecache { ... }` block.

When **empty** (default): no subscriber goroutine is spawned. The
reserved scopes still exist (§2.6) but accumulate without being
consumed; if the operator never wires their own subscriber, those
scopes will eventually approach the global byte cap. Pure
cache-only mode — the operator opted out of the auto-pipeline.

When **set to a path**: the adapter calls
`gw.StartSubscriber(EventsScopeName, command)` and
`gw.StartSubscriber(InboxScopeName, command)` at boot. Both
goroutines invoke the same command; the command branches on
`SCOPECACHE_SCOPE` to know which scope fired. `stop()` for both is
called automatically at process shutdown / Caddy module Cleanup;
operators never see it directly.

A path that points at a missing or non-executable file is **not**
a startup error — the bridge logs the per-invocation failure and
the next wake-up retries. This lets operators deploy the command
after the cache has booted.

"Command" not "script": the bridge's `exec.Command` accepts any
executable. The reference implementation in
[`scripts/drain_events.sh`](../scripts/drain_events.sh) is a POSIX
shell script that demonstrates the drain pattern (sleep 0.5, GET
/head, write JSONL, POST /delete_up_to, repeat until empty) but
operators may swap in any language or compiled binary.

### 7.7 Lifecycle ordering at shutdown

The standalone binary stops subscribers **before** calling
`server.Shutdown(ctx)` so any running drain command can complete
its HTTP roundtrips against a still-listening cache. The order is:

1. `SIGINT` / `SIGTERM` arrives.
2. `stopSubscribers()` runs — blocks until each subscriber goroutine
   has finished its current command run. The HTTP server is still
   accepting requests during this phase; in-flight `curl /head` /
   `curl /delete_up_to` calls complete normally.
3. `server.Shutdown(ctx)` runs — drains any non-subscriber HTTP
   requests, then closes the listener.
4. Socket file removed; process exits.

The Caddy module mirrors this in `Cleanup()`. Shutdown ordering is
adapter responsibility, not core — the bridge exposes a synchronous
`stop()` so adapters can wire it correctly without needing to know
internals.

### 7.8 Crash-safety invariant for any subscriber

This applies to **any** subscriber pattern — the in-core bridge,
operators writing their own Go-side subscribers via `Subscribe`,
or commands wired through the bridge. The cache cannot enforce it;
it's a contract the subscriber must follow:

> Persist your cursor to durable storage **before** calling
> `/delete_up_to`. Otherwise a crash between the durable write and
> the cursor save loses the just-deleted-but-not-yet-recorded batch.

Generic safe order:

1. Read a batch (`/tail` or `/head`).
2. Write to the sink **durably** (fsync, COMMIT, ack, …).
3. Persist the cursor durably (ideally in the same transaction as
   step 2, if the sink is transactional).
4. Call `/delete_up_to` with the persisted cursor.

If step 4 crashes, the next run sees a cursor past where the items
still are; re-running `/delete_up_to` is idempotent. Combining
steps 2 and 3 in one transaction (SQLite, Postgres, etc.) removes
the gap between them; for filesystem sinks an atomic-rename of a
tmp-file is the closest portable equivalent. The cache itself
holds nothing recoverable — `_events` is in-memory only — so the
durability burden is entirely on the sink side.

### 7.9 Addon skeleton — reactive event drain

The canonical push-shaped addon: subscribe to a reserved scope,
drain new items in batches on every wake-up, hand each item to a
caller-supplied handler. Roughly the shape a Mercure publisher,
in-process logger, or content-routing addon would take.

```go
package eventdrain

import (
	"context"
	"fmt"
	"log"

	"github.com/VeloxCoding/scopecache"
)

// Drain wakes up on every write to scope and processes new items
// in seq order via handler. Cursor advances per processed item;
// items with seq <= cursor are removed via DeleteUpTo so the
// reserved scope does not accumulate indefinitely.
//
// Crash-safety: this skeleton acks (DeleteUpTo) after the handler
// returns. Real deployments should persist cursor durably before
// calling DeleteUpTo — see §7.8.
type Drain struct {
	gw      *scopecache.Gateway
	scope   string
	handler func(scopecache.Item) error
}

func New(gw *scopecache.Gateway, scope string, handler func(scopecache.Item) error) *Drain {
	return &Drain{gw: gw, scope: scope, handler: handler}
}

// Run blocks until ctx is cancelled or the subscription closes.
// Returns ctx.Err() on cancellation, the first handler error if
// the handler ever fails, or nil if the subscription was closed
// from elsewhere.
func (d *Drain) Run(ctx context.Context) error {
	ch, unsub, err := d.gw.Subscribe(d.scope)
	if err != nil {
		return fmt.Errorf("subscribe %s: %w", d.scope, err)
	}
	defer unsub()

	var cursor uint64
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case _, ok := <-ch:
			if !ok {
				return nil
			}
			for {
				items, _, _ := d.gw.Head(d.scope, cursor, 100)
				if len(items) == 0 {
					break
				}
				for _, ev := range items {
					if err := d.handler(ev); err != nil {
						log.Printf("eventdrain: handler error at seq %d: %v", ev.Seq, err)
						return err
					}
					cursor = ev.Seq
				}
				if _, err := d.gw.DeleteUpTo(d.scope, cursor); err != nil {
					return fmt.Errorf("ack: %w", err)
				}
			}
		}
	}
}
```

Wiring into an adapter:

```go
gw := scopecache.NewGateway(cfg)
drain := eventdrain.New(gw, scopecache.EventsScopeName, func(ev scopecache.Item) error {
    // Item.Payload is safe to retain — Gateway has cloned it.
    return publishToMercure(ev.Scope, ev.ID, ev.Payload)
})
go func() {
    if err := drain.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
        log.Printf("drain exited: %v", err)
    }
}()
```

Notes:

- `Subscribe` is restricted to reserved scopes; pass
  `scopecache.EventsScopeName` (cache-managed write events) or
  `scopecache.InboxScopeName` (app-side fan-in).
- The wake-up channel coalesces — a burst of N writes can produce
  one wake-up; the inner loop drains until `Head` returns no items.
- `Item.Payload` is a fresh slice (Gateway clones on exit, §7.3).
  The handler can retain it, hash it, ship it elsewhere — the
  cache's storage is unaffected.
- A single subscriber per scope is enforced; if your addon needs
  multi-fan-out, drain into an internal channel and split there.

### 7.10 Addon skeleton — wrapper-on-write

The canonical pre-write transformation addon: wrap `*Gateway`,
intercept a write method, transform a field, forward. A
content-hasher (id = SHA-256 of payload), an idempotency-key
deduplicator, or a payload validator all share this shape.

```go
package contenthash

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/VeloxCoding/scopecache"
)

// Hasher wraps *Gateway and rewrites Append calls so the SHA-256
// of the payload becomes the item's id. Caller-supplied id is
// overridden. All other Gateway methods are reachable via Gw().
type Hasher struct {
	gw *scopecache.Gateway
}

func New(gw *scopecache.Gateway) *Hasher {
	return &Hasher{gw: gw}
}

// Gw returns the underlying *Gateway for callers that need
// non-wrapped methods (Get, Tail, Stats, etc.).
func (h *Hasher) Gw() *scopecache.Gateway { return h.gw }

// Append computes sha256(payload) and substitutes the hex digest
// as item.ID, then forwards to gw.Append. The committed item
// (with cache-assigned Seq + Ts) is returned unchanged.
func (h *Hasher) Append(item scopecache.Item) (scopecache.Item, error) {
	sum := sha256.Sum256(item.Payload)
	item.ID = hex.EncodeToString(sum[:])
	return h.gw.Append(item)
}
```

Wiring:

```go
gw := scopecache.NewGateway(cfg)
hasher := contenthash.New(gw)

// Writes go through the wrapper:
hasher.Append(scopecache.Item{Scope: "blobs", Payload: payload})

// Reads go through the underlying gateway:
item, hit := hasher.Gw().Get("blobs", contentID, 0)
```

Notes:

- The wrapper holds `*Gateway`, not `*store`. Validation, capacity
  enforcement, and clone discipline still apply — the wrapper
  cannot bypass them.
- Wrapping is selective: only methods you transform need wrapping.
  Everything else is reachable via `Gw()`.
- Multiple wrappers compose: `validate(hash(gw))` is a chain where
  each layer adds one transformation. Each layer holds a
  `*Gateway` (or another wrapper); the cache never sees the
  composition.

### 7.11 Reference materials

- [`scripts/drain_events.sh`](../scripts/drain_events.sh) —
  reference command implementation for the StartSubscriber bridge
  (§7.5). POSIX shell, depends on `curl` + `jq`. Reads
  `SCOPECACHE_SCOPE`, `SCOPECACHE_SOCKET_PATH`, and
  `SCOPECACHE_OUTPUT_DIR`; drains the scope into timestamped JSONL
  files and exits.
- [`scripts/e2e_subscriber.sh`](../scripts/e2e_subscriber.sh) —
  end-to-end smoke test that boots the standalone binary with
  `subscriber_command` set, performs 10 writes, polls until
  `_events` is empty, and verifies the JSONL output shape.
- [`gateway.go`](../gateway.go) +
  [`gateway_clone.go`](../gateway_clone.go) — implementation of
  the Gateway boundary and clone discipline.
- [`gateway_test.go`](../gateway_test.go) +
  [`gateway_clone_test.go`](../gateway_clone_test.go) —
  Gateway-level tests, including both directions of the clone
  discipline (caller mutates input after write; caller mutates
  output after read).

---

## 8. Operational model

### 8.1 Locking

The cache uses three concentric layers of locks:

- **Shard locks** — the scope registry is split into independently
  locked shards. Scope creation and lookup take only the relevant
  shard's lock, so unrelated scopes parallelise across shards.
- **Per-scope locks** — every scope has its own `sync.RWMutex`.
  Single-item writes take the shard lock to look up the scope,
  then operate at scope level under the scope's mutex. Concurrent
  writes to the same scope serialise on this mutex; writes to
  different scopes do not.
- **Multi-shard locks** — store-wide mutations (`/wipe`,
  `/rebuild`, multi-scope `/warm`) acquire shard locks in
  ascending shard-index order to prevent deadlock between
  concurrent multi-shard operations.

The byte-budget counter (used by `MaxStoreBytes` admission) is a
single atomic value, modified via compare-and-swap. Writes reserve
their net byte delta on this counter before taking the per-scope
lock, so an over-cap write fails fast without acquiring the scope
mutex.

### 8.2 Durability contract

A `200 OK` response from any write endpoint confirms the write was
applied to the cache at the moment of commit. It is **not** a
persistence guarantee:

- The cache is in-memory only. Process restart clears everything.
- A concurrent `/rebuild` or `/wipe` replaces or clears the entire
  store and erases writes that committed moments earlier. This is
  intentional: both endpoints express "the source of truth says
  this is the new state," and the cache is explicitly subordinate
  to that source.
- `/delete_scope` and `/delete_up_to` erase earlier writes within
  their scope by design. Appends that committed just before a
  delete can vanish from the cache (but not from the source of
  truth, which is where they came from).
- The orphan-detach mechanism (returning `409` when a scope is
  unlinked mid-write) protects the store's internal accounting —
  the byte counter cannot be corrupted by a write committing into
  a buffer unlinked by a concurrent swap — but it does not, and
  cannot, retroactively preserve the write itself.

Clients MUST be idempotent against cache loss (items re-fetchable
or re-derivable from the source of truth) and MUST NOT treat a
`200 OK` as a durable acknowledgement.

### 8.3 Read consistency

Read endpoints answer under per-scope read locks: the contents of
any single scope returned by `/get`, `/head`, `/tail`, or `/render`
are internally consistent at the moment of the read.

`/stats` is an **advisory snapshot**, not a transaction:

- Every field comes from an independent atomic load. The three
  counters (`scope_count`, `total_items`, `approx_store_mb`) are
  maintained incrementally on every write/delete/bulk path, but
  they are not loaded under a shared lock — concurrent writes
  between the three loads can produce a snapshot where the fields
  reflect three slightly different instants.
- The `Σ scope.bytes == total_bytes` and `Σ len(scope.items) ==
  total_items` invariants hold at quiesce, not at every observation.

Operators using `/stats` to drive decisions (capacity planning,
admission-control headroom) should treat its output as approximate
rather than transactional.

---

## 9. Read bookkeeping

Every successful read hit on `/get`, `/render`, `/head` or `/tail`
bumps two per-scope atomics so addons (and operators) can tell
which scopes are active. The cache deliberately stops at primitives:
any time-windowed aggregation — rolling 7-day count, hourly
histogram, exponential decay, eviction ranking — is policy and
lives in addons that poll the primitives off a scheduler.

### 9.1 Tracked fields

The fields are maintained on the per-scope buffer; `/stats` is
aggregate-only and does not surface them. They are exposed via
`/scopelist` (§6.5) as part of the per-scope detail bundle, and
also readable via the in-process Go API (§7.2). See §2.4 for the
full list of per-scope fields.

| field              | type   | meaning                                                 |
|--------------------|--------|---------------------------------------------------------|
| `last_access_ts`   | int64  | microsecond timestamp of the most recent successful read hit |
| `read_count_total` | uint64 | monotonic lifetime hit count; never expires             |

That is the entire read-side surface in core. There is no rolling
window, no day-bucket ringbuffer, no decay model. `read_count_total`
+ a wall-clock at observation time is enough to compute any window
the caller wants — the addon owns the policy, the cache owns the
primitive.

### 9.2 What counts as a read

The following endpoints stamp `last_access_ts` and increment
`read_count_total` on a successful hit:

- `GET /get`
- `GET /render`
- `GET /head`
- `GET /tail`

Misses (no scope, no item, empty result) do **not** count.
Observability endpoints (`/stats`, `/scopelist`) do not count.

### 9.3 Concurrency

Bookkeeping updates are lock-free: `lastAccessTS` is an `atomic.Int64`
store, `readCountTotal` is an `atomic.Uint64` add. The hot read path
does not take the scope's write lock to update them, so concurrent
readers do not serialise on bookkeeping.

### 9.4 Read bookkeeping as a building block

The two primitives are exposed unconditionally — there is no
"disable read bookkeeping" knob. Two atomic stores are not
expensive enough to gate behind configuration. The cache itself
never uses these fields to decide anything; eviction decisions
live in addons. A typical eviction-candidates addon walks
`/scopelist` on a scheduler, snapshots `read_count_total` per
scope, computes deltas against the previous snapshot, and feeds
the deltas into whatever windowed model it implements (daily
buckets, exponential decay, etc.). Different addons can ship
different models without the core baking any one of them in.

---

## 10. Out of scope

The following are **deliberately not** core features. Anything
listed here either lives in operator policy (gating, networking,
deployment), in addon sub-packages (per-addon RFCs), or is a
non-goal entirely.

### 10.1 Not in core: deferred to addons

- **Authentication and authorization.** Tokens, signatures, mTLS,
  bearer headers, capability checks — none of this is core. Addons
  (e.g. a tenant-gateway addon, an operator-elevated dispatcher
  addon) add request-context-aware auth on top of the public Go
  API.
- **Multi-tenancy.** Per-tenant scope isolation, prefix rewrites,
  scope-name conventions tied to tenant identity. Live in addons.
- **Batch dispatch.** Combining N sub-calls into one HTTP roundtrip
  (`/multi_call`-shaped). Lives in an addon.
- **Write-only ingestion shapes.** Cache-assigned IDs, fire-and-
  forget append patterns, payload-cap variations — addon territory.
- **Eviction-hint queries.** Sorting scopes by activity, age, or
  byte-size to recommend which to drop — addon, built on
  `/scopelist` and §9 data.

### 10.2 Not in core: operator policy

- **Access control.** Which clients reach which endpoints is a
  transport-layer decision. Caddyfile route guards (or nginx /
  apache equivalents), Unix-socket filesystem permissions,
  separate listeners per trust level — all lives outside the
  cache.
- **Reserved-scope enforcement.** The `_` prefix is a social
  convention for state managed by addons (`_tokens`,
  `_counters_*`, addon-internal scopes). The core does not parse
  scope names or treat any prefix as reserved. If an addon wants
  its state protected from public writes, that protection comes
  from the operator (gating which endpoints are publicly
  reachable) or from the addon itself (signed payloads,
  unguessable scope names) — see §1.4 for the boundary rule.
- **Network exposure.** The cache speaks HTTP on whatever the
  adapter mounts it on (Unix socket for the standalone binary,
  Caddy listener for the module). The adapter and the operator
  jointly decide what's reachable from where.

### 10.3 Not in core: non-goals

- **Persistence.** The core is in-memory only. Process restart
  clears everything; rebuild from the source of truth. Addons
  could be built to periodically drain to disk, a database, or a
  remote store — but that lives outside the core.
- **TTL or background eviction.** No scheduler, no LRU, no LFU,
  no time-based expiration. Whatever you write stays until you
  delete it.
- **Payload-content filters.** No queries against payload contents,
  no indexes on payload fields, no joins. Anything beyond
  `scope`/`id`/`seq` belongs in the source-of-truth or in an
  application layer above the cache.
- **Cross-instance replication or coordination.** Each scopecache
  process is independent. Multi-instance deployments coordinate at
  the source-of-truth layer, not in the cache.
