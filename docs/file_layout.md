# File layout

Volledige boomstructuur en uitleg per bestand-familie. CLAUDE.md verwijst hierheen voor de details; daar staat alleen een verwijzing.

## Tree

```
caddy_module/                         (root module github.com/VeloxCoding/scopecache)
├── go.mod                            (stdlib-only)
├── go.work                           (binds root + caddymodule for local dev)
├── Dockerfile                        (standalone binary)
├── Dockerfile.caddyscope             (xcaddy build: Caddy + scopecache module)
├── docker-compose.yml
├── CLAUDE.md                         (gitignored — local-only)
├── README.md
│
│   ─── package scopecache (core, stdlib-only) ─────────────────────────────────
│
│   ── scopeBuffer family — per-scope ring under b.mu (split per concern):
├── buffer.go                         ── struct, ctor, locking-invariant header
├── buffer_locked.go                  ── shared `*Locked` helpers used across paths
├── buffer_heat.go                    ── lock-free recordRead bookkeeping (readCountTotal + lastAccessTS)
├── buffer_write.go                   ── appendItem, upsertByID, updateByID, updateBySeq
├── buffer_counter.go                 ── counterAdd + payload parsing
├── buffer_delete.go                  ── deleteByID/BySeq/UpToSeq + deleteIndexLocked
├── buffer_replace.go                 ── prepare-then-commit pipeline (warm/rebuild)
├── buffer_read.go                    ── tailOffset, sinceSeq, tsRange, getByID/Seq
├── buffer_stats.go                   ── approxSizeBytes + scopeStats + stats()
├── buffer_test.go                    ── monolithic; mirrors all buffer_*.go via t-prefix
│
│   ── Store coordinator: sharding + cap budget:
├── store.go                          ── *store, shards, single-shard ops, appendOne,
│                                        upsertOne, counterAddOne, deleteScope, …
├── store_test.go
│
│   ── Multi-shard mutations (ascending-shard-index lock discipline):
├── bulk.go                           ── wipe, rebuildAll, replaceScopes
│                                        + lockAllShards/lockShards
├── bulk_test.go
│
│   ── Public Go API + HTTP API:
├── gateway.go                        ── *Gateway facade — every public Go method
├── gateway_test.go
├── api.go                            ── APIConfig + *API + NewAPI(*Gateway, …)
│
│   ── Handler family — native HTTP endpoints only:
├── handlers.go                       ── shared infra: error helpers, body decode,
│                                        response writers, RegisterRoutes
├── handlers_write.go                 ── /append, /upsert, /update, /counter_add
├── handlers_read.go                  ── /head, /tail, /get, /render
├── handlers_delete.go                ── /delete, /delete_up_to, /delete_scope, /wipe
├── handlers_bulk.go                  ── /warm, /rebuild
├── handlers_observe.go               ── /stats, /scopelist, /help
├── handlers_test.go                  ── shared-infra + per-endpoint tests
│
│   ── Subscribe + subscriber bridge:
├── subscribe.go                      ── Subscribe primitive + subscriber registry
├── subscribe_test.go
├── events.go                         ── _events auto-populate (action-vector envelopes)
├── subscriber_command.go             ── Gateway.StartSubscriber bridge (in-core)
├── subscriber_command_test.go
│
│   ── Init command (boot-time hook):
├── init_command.go                   ── Gateway.RunInitCommand bridge
├── init_command_test.go
│
│   ── Cross-cutting:
├── types.go                          ── Config, Item, error types, MB, reserved-scope constants, …
├── validation.go                     ── input shape rules (scope/id/payload limits)
├── validation_test.go
│
│   ── Test infra (not source-mirroring):
├── bench_test.go                     ── Go-level benchmarks
├── bench_http_test.go                ── HTTP-level benchmarks (build tag: unix)
├── stress_test.go                    ── concurrency stress tests
├── fuzz_test.go                      ── fuzz harness
├── ts_test.go                        ── ts-related tests
│
├── cmd/
│   └── scopecache/
│       ├── main.go                   ── package main  (standalone binary; init runs behind private temp socket when SCOPECACHE_INIT_COMMAND is set)
│       ├── socket_linux.go
│       └── socket_other.go
│
├── caddymodule/                      (separate module github.com/VeloxCoding/scopecache/caddymodule)
│   ├── go.mod                        (requires caddy/v2 + core; pin auto-bumped on tag push)
│   ├── go.sum
│   ├── module.go                     ── package caddymodule (init runs behind private temp socket during Provision)
│   └── module_test.go
│
├── docs/
│   ├── scopecache-core-rfc.md        ── canonical core spec (operator-facing)
│   ├── scopecache-rfc-old.md         ── v0.7.16-era snapshot, kept for distinct-design reference
│   └── file_layout.md                ── this file
│
├── deploy/
│   ├── Caddyfile                     ── smoke-test placeholder
│   └── Caddyfile.caddyscope          ── working Caddy + scopecache demo
│
├── scripts/
│   ├── bench.sh                      ── repeatable single-version bench driver
│   ├── bench_stats_oneoff.sh         ── one-off /stats benchmark helper
│   ├── e2e_test.sh                   ── end-to-end curl suite (full HTTP-surface coverage)
│   ├── run_e2e.sh                    ── wraps e2e against standalone + caddyscope
│   ├── drain_events.sh               ── reference subscriber-command (POSIX shell, drains _events)
│   └── e2e_subscriber.sh             ── end-to-end test for the subscriber bridge
│
├── archive/                          (gitignored; pre-strip handler files for reference)
├── harness/                          (gitignored; live FrankenPHP harness)
└── .github/workflows/
    ├── ci.yml                        (Go build + test on PR/push)
    ├── release.yml                   (release pipeline)
    └── sync-caddymodule-tag.yml      (auto-bumps caddymodule pin on tag push)
```

When add-ons start landing, they will appear under `addons/<name>/` as Go sub-packages (Phase C). The folder doesn't exist today; nothing currently fits the addon shape better than living in core.

## Core file split: lock discipline, not handler grouping

The three core files (`buffer.go` family, `store.go`, `bulk.go`) are split by **concurrency pattern**, not by "which HTTP handler calls them" — every `*store` method is called by some handler, so that would be no criterion at all.
The actual boundary:

- **`buffer*.go` (the scopeBuffer family)** — `*scopeBuffer` and everything that
  operates on a single scope under that scope's `b.mu`. No knowledge of shards or
  the store-wide byte counter beyond the `b.store` back-pointer. Internally split
  by mutation kind (`buffer_write.go`, `buffer_delete.go`, …) but they share the
  same lock discipline; see [`buffer.go`](../buffer.go)'s file-level header for the
  three rules.
- **`store.go`** — `*store` coordinator. Sharding (`shardIdxFor`, `shardFor`),
  byte-budget admission control (`reserveBytes`), scope lifecycle, and **single-shard
  mutations** that take at most one `sh.mu` plus one `buf.mu`. Single-item
  write/mutate methods: `appendOne`, `upsertOne`, `counterAddOne`,
  `updateOne`, `deleteOne`, `deleteUpTo`. Single-item reads: `head`,
  `tail`, `get`, `render`. Scope-level: `deleteScope`, `ensureScope`,
  `getScope`. Every HTTP handler routes through one of these methods —
  handlers do decode + validate + one Store call + respond, no direct
  `*scopeBuffer` access. Eviction-candidate ranking is no longer a
  core concern (see Phase C `scopecache/heat/` addon).
- **`bulk.go`** — **multi-shard mutations only**, which MUST acquire shard locks
  in ascending shard-index order to avoid deadlock with each other (see the
  `numShards` comment block at the top of `store.go`, plus the `lockAllShards`/
  `lockShards` helpers that encode the order in code rather than per-call-site
  loops). Currently: `wipe`, `rebuildAll` (all-shard sweep) and `replaceScopes`
  (subset via `shardsForScopes`, still ascending). If you ever see a deadlock
  on shard locks, this is the file to read.

`stats()` and `listScopes()` walk all shards too but stay in `store.go` because
they are read-only `RLock` sweeps with no lock-order invariant to worry about —
`bulk.go` is reserved for the subtle mutation discipline.

## scopeBuffer file family: `buffer_*.go`

Within the scopeBuffer family the split is by **mutation kind**, mirroring the
cache's verb-set: writes, counters, deletes, reads, replacements, plus two
self-contained subsystems (read bookkeeping, sizing/stats) and a shared-helpers
file. Cross-file references are fine — everything is in `package scopecache`
and the sub-files are about **navigation and read-window size**, not about
package boundaries. See the file-layout header in [`buffer.go`](../buffer.go) for
the canonical map "concept → file". Method placement rules:

- A method goes in the file matching its primary verb (`appendItem` →
  `buffer_write.go`, `deleteByID` → `buffer_delete.go`).
- A `*Locked` helper used by callers across multiple files lives in
  `buffer_locked.go` (e.g. `indexBySeqLocked`, `replaceItemAtIndexLocked`).
- A `*Locked` helper used only inside one verb-family stays with its callers
  (e.g. `deleteIndexLocked` lives in `buffer_delete.go`, `parseCounterValue`
  lives in `buffer_counter.go`).
- `recordRead` lives separately in `buffer_heat.go` because it bumps two
  atomics (`readCountTotal`, `lastAccessTS`) without taking `b.mu`.

`buffer_test.go` is monolithic — covers all `buffer_*.go` families via test
naming (`TestAppendItem_*`, `TestDeleteUpToSeq_*`, etc.) rather than
file-mirroring. That's a deliberate choice: tests for related scopeBuffer
behaviours often need to share helpers (`newItem`) and are easier to navigate
as one file with comment-headers per group.

## Handler file family: `handlers_*.go`

Same rule as the buffer family: file-per-feature, all in `package scopecache`.
The naming is uniform — every HTTP-handler file starts with `handlers_` so a
plain `ls` reveals the full surface. `handlers.go` is the **shared infra hub**:
error helpers, body decoding, response writers, the request parsers
(`parseLookupTarget`, `parseScopeLimit`), and `RegisterRoutes` which mounts
every endpoint on the mux.

**Handlers are thin by design.** Every HTTP handler follows the same
shape: decode the body or query params, run shape validation, call
exactly one Store method, write the response. Handlers must not reach
into `*scopeBuffer` directly — every store-touching operation has a
matching Store method (see "Core file split" above). When adding a
new endpoint, if there is no Store method that fits, add one; do not
skip the layer.

Per-feature file content:

- `handlers_write.go` — `/append`, `/upsert`, `/update`, `/counter_add`
- `handlers_read.go` — `/head`, `/tail`, `/get`, `/render`
- `handlers_delete.go` — `/delete`, `/delete_up_to`, `/delete_scope`, `/wipe`
- `handlers_bulk.go` — `/warm`, `/rebuild`
- `handlers_observe.go` — `/stats`, `/help`

When adding a new core endpoint *(rare — core is feature-complete; bug fixes and clarifications only)*:

- Pure CRUD on existing primitives → add to the matching `handlers_*` group file.
- A piece of shared infra used by ≥ 2 handler files → `handlers.go`.
- Always add the matching `*Gateway` method so Go callers and HTTP callers stay symmetric.

When adding request-context-aware behaviour (auth, tenants, batch dispatch, write-only ingestion shapes, custom routing): build it as an addon under `addons/<name>/`, not in core. See §1.4 of the core RFC and Phase C below.

## Public API surface of `package scopecache`

Kept deliberately small so the core stays refactorable. Authoritative shape (`go doc github.com/VeloxCoding/scopecache`):

- **Constructors**: `NewGateway(c Config) *Gateway`, `NewAPI(gw *Gateway, _ APIConfig) *API`, `ParseEventsMode(s string) (EventsMode, error)`.
- **Types**: `*Gateway`, `*API`, `Config`, `APIConfig`, `EventsConfig`, `InboxConfig`, `Item`, `MB`, `Stats`, `ScopeListEntry`, `ReservedScopeEntry`, `EventsMode` + `EventsModeOff/Notify/Full`.
- **Methods on `*Gateway`** — full mirror of the HTTP surface plus the two control-plane primitives:
  - **HTTP-mirrored**: `Append`, `Upsert`, `Update`, `CounterAdd`, `Delete`, `DeleteUpTo`, `DeleteScope`, `Wipe`, `Warm`, `Rebuild`, `Get`, `Render`, `Head`, `Tail`, `ScopeList`, `Stats`.
  - **Control-plane**: `Subscribe(scope) (<-chan struct{}, func(), error)`, `StartSubscriber(scope, command) (stop func(), err error)`, and `RunInitCommand(command, extraEnv, logf) error`. Subscribe primitives are restricted to reserved scopes; see RFC §7.4. Init command is fire-once at boot.
- **Method on `*API`**: `RegisterRoutes(mux *http.ServeMux)`.
- **Errors**: `ErrInvalidInput`, `ErrInvalidSubscribeScope`, `ErrAlreadySubscribed`, `ScopeFullError`, `StoreFullError`, `ScopeCapacityError`, `ScopeCapacityOffender`, `ScopeDetachedError`, `CounterPayloadError`, `CounterOverflowError`.
- **Constants**: `MaxItemBytes`, `MaxStoreMiB`, `ScopeMaxItems`, `InboxMaxItemBytes`, `MaxScopeBytes`, `MaxIDBytes`, `MaxCounterValue`, `DefaultLimit`, `MaxLimit`, `EventsScopeName` (= `"_events"`), `InboxScopeName` (= `"_inbox"`).

Everything else — `*store`, all lowercase methods, internal helpers — is unreachable from outside the package. Adding a public method or type is a deliberate API decision; pre-1.0 it just lands, post-1.0 it's a major bump.

The split between `Config` and `APIConfig` enforces the boundary rule in code: `Config` knobs (per-scope item cap, store byte cap, per-item byte cap, reserved-scope caps, `Events.Mode`) reach the core via `NewGateway`; `APIConfig` is currently empty but kept as the future home for HTTP-layer knobs (CORS, request tracing, …). New cache knobs land on `Config`; new HTTP-layer knobs land on `APIConfig`. Anything that needs request context (auth, tenants, batch dispatch) is an addon, not a core knob.

The subscriber bridge is the one exception to "addon territory" — its activation knob lives in **adapter** config (env var on standalone, Caddyfile key on the module), not on `Config`, because the cache itself doesn't know what an executable path is. See RFC §7.4.3. The init-command bridge follows the same shape: `SCOPECACHE_INIT_COMMAND` / `init_command` Caddyfile key.

Env-var parsing (`SCOPECACHE_SCOPE_MAX_ITEMS`, `SCOPECACHE_MAX_STORE_MB`, `SCOPECACHE_MAX_ITEM_MB`, `SCOPECACHE_SOCKET_PATH`, `SCOPECACHE_SUBSCRIBER_COMMAND`, `SCOPECACHE_INIT_COMMAND`, …) lives in `cmd/scopecache/` — the core package takes plain values through `Config` so the Caddy module can supply them from its own JSON config instead. Both adapters build the config + Gateway + API and hand the API's mux to their listener.

Handler methods (`handleAppend`, `handleWarm`, …) stay exported on `*API` so the Caddy module can dispatch to them directly, but normal consumers go through `RegisterRoutes`. Socket-specific concerns (`DefaultSocketPath`, `UnixSocketPerm`, platform `socket_*.go`) live in `cmd/scopecache/` — they are not part of the core.
