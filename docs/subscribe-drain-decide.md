# Subscribe + log-scope drain architecture — decide-doc

Working document. Captures the in-flight design for scopecache's
write-event subscription primitive and the operational shape that
sits on top of it. Decisions in this file are NOT yet locked into
core; the canonical entry in CLAUDE.md (Phase A) carries only a
summary and a pointer back here.

## Context — how we got here

Three pivots collapsed the original ambitious design into a much
smaller shape:

1. **Subscribe is core, not Phase B.** It's a property of the cache
   itself (writes happen, observers want to know), independent of
   whether the `*Direct` Go-interface ever ships. Anyone with a
   `*Store` from `NewStore()` can call it.

2. **Coalescing single-slot beats deep-buffered.** For in-process Go
   addons there is typically N=1 subscriber per scope, and the most
   common consumer pattern is "wake up, batch-Tail, sleep." A
   single-slot channel with non-blocking send + drop-on-full collapses
   bursts of writes into one wake-up signal — same information value,
   far fewer channel-sends, no slow-subscriber-drop policy needed.

3. **Notifications/health are out-of-process, not in-cache.** A cache
   cannot reliably monitor its own health (dead-man's-switch). External
   pollers on `/stats` cover capacity/health alerting. The cache itself
   only emits write-events, not internal-state events for ops alerting.

Net result: scopecache is a fast in-memory write-buffer with a
signal-mechanism. Files + OS-native event-tools (inotify, logrotate,
cron, PHP workers) do everything else.

## The architecture (3 layers)

```
write to scope X      →  buffer X (live state, for reads)
                      →  buffer _log (append-only, with payload)
                                   │
                                   │ (single-slot coalescing notify via Subscribe)
                                   ▼
                          drainer (operator-built, or chosen reference)
                          subscribe → batch → process → delete_up_to
                                   │
                                   ▼
                          whatever sink the operator chose:
                          JSONL files, SQLite, external DB, Kafka,
                          OpenSearch, HTTP webhook, mail, …
                                   │
                                   ▼
                          downstream consumers in any language /
                          process — file watchers, DB queries,
                          message-queue subscribers, etc.
```

The cache provides three primitives: **Subscribe** (signal),
**Tail** (batch fetch), **DeleteUpTo** (release). What sits between
those primitives and the operator's downstream tooling is the
drainer, and the drainer's shape is **out of scope for the cache**.

This is the same lesson as the dropped `_notification` scope: ship
primitives, not solutions. Operators with different durability
preferences (filesystem vs SQLite vs external DB), different
processing semantics (real-time push vs batch every minute),
different downstream tooling (PHP workers vs Kubernetes jobs vs
serverless functions) all build their own drainer or pick a
reference implementation that fits.

The cache itself ends at the three primitives — every "everything
must be possible" feature beyond that is operator-side.

## What this solves

| Concern | How |
|---|---|
| Persistence | Files on disk; cache may die, files survive |
| Audit-trail | Files ARE the audit log, append-only, timestamped |
| Decoupling | Consumers can be any language, any process, OS-event-driven |
| Recovery | Drainer crash → restart from `lastSeq`, no state-loss |
| Pressure relief | `_log` / `_inbox` get drained continuously, never accumulate |
| Backup/replication | Standard tooling — rsync, cp, S3 sync — works on files |

## Cache-side primitives

### Subscribe

```go
func (s *Store) Subscribe(scope string) (<-chan struct{}, func())
//                                          coalescing    unsubscribe
```

- **Single-slot, size-1 buffered channel.** Non-blocking send: when
  the slot is full the cache drops the send (no-op). The pending
  notification already covers any subsequent write.
- **Per-scope.** Each `*scopeBuffer` owns its own subscriber list.
- **No slow-subscriber drop policy needed.** A subscriber that falls
  behind simply doesn't get a separate signal per missed write —
  they get one wake-up that covers all of them, and Tail-since-cursor
  catches them up.
- **Capture-under-lock, emit-outside-lock.** Writes commit under
  `b.mu`; the notification fanout happens after `b.mu` is released,
  so slow channel-sends never extend the write-path's critical
  section.
- **Subscribe is a deliberate exception** to the "all `*Store`
  methods are lowercase" rule. Other Store methods are
  implementation details (sharding, byte-budget, lock order) that
  external callers must not touch. Subscribe is the opposite: a
  contract surface.

### `_log` scope (auto-populated)

The cache writes a log entry to `_log` on every successful mutation
to any scope (other than `_log` itself, to prevent recursion).

```jsonl
{"scope":"thread:42","id":"msg-99","seq":17,"ts":1746367932123456,"op":"append","payload":{...}}
{"scope":"thread:42","id":"msg-100","seq":18,"ts":1746367932124567,"op":"counter_add","payload":42}
{"scope":"thread:42","seq":18,"ts":1746367932125678,"op":"delete"}
{"scope":"_admin","seq":0,"ts":1746367932200000,"op":"wipe"}
```

Per Op, in `_log`:

| Op | Trigger | Per-item or summary | Payload? |
|---|---|---|---|
| `append` | `/append` | per-item | yes |
| `upsert` | `/upsert` | per-item | yes |
| `update` | `/update` | per-item | yes |
| `delete` | `/delete`, `/delete_up_to` | per-item | n/a |
| `counter_add` | `/counter_add` | per-item | yes (rendered value) |
| `warm` | `/warm` | summary (1 entry: scope + count) | no |
| `rebuild` | `/rebuild` | summary (1 entry: scope_count + item_count) | no |
| `wipe` | `/wipe` | summary (1 entry) | n/a |
| `scope_detached` | scope-delete | summary (1 entry: scope + item_count) | n/a |

Bulk-summary, not per-item, so a 1M-item rebuild produces 1 log
entry, not 1M. Replicators that see "rebuild" do a full re-sync from
the source-of-truth; they do not replay individual replaced items.

### `_inbox` scope (pre-created, app-populated)

Just a regular scope, pre-created at boot via `ensureScope("_inbox")`
so Subscribe works immediately without a "scope-doesn't-exist-yet"
race. Apps write to it; a drainer addon drains it the same way as
`_log`. The cache never auto-writes here.

## Drainer pattern

The drainer is **not a structural component of the cache**; it is a
pattern that operators implement (or pick a reference implementation
of) to bridge the cache's three primitives to whatever downstream
durability/processing their use case demands.

Generic shape regardless of sink:

```go
ch, unsub := store.Subscribe(scope)
defer unsub()

for range ch {
    time.Sleep(coalescingDelay)  // give bursts a chance to coalesce

    items, _, _, _ := direct.Tail(scope, batchLimit, lastSeq)
    if len(items) == 0 { continue }

    // ── operator-defined sink ─────────────────────────────
    // JSONL files? SQLite INSERT? HTTP webhook? Kafka publish?
    // External-DB UPSERT? Composite (file + DB)?
    // The cache does not care. It provides Tail + DeleteUpTo,
    // the operator implements process(items).
    process(items)
    persistCursor(scope, items[len(items)-1].Seq)
    // ──────────────────────────────────────────────────────

    direct.DeleteUpTo(scope, items[len(items)-1].Seq)
    lastSeq = items[len(items)-1].Seq
}
```

### Reference implementations (when they ship)

The repo may ship one or more reference drainers as `addons/drainer-*`
sub-packages, each demonstrating one common sink:

- `addons/drainer-jsonl/` — per-batch JSONL files, inotify-friendly
- `addons/drainer-sqlite/` — single SQLite database, transactional
- `addons/drainer-postgres/` — INSERT into an external Postgres,
  treats Postgres as the source-of-truth-of-record
- (others as use cases emerge)

These are **examples and starting points**, not "the" drainer.
Operators pick the one that fits, or write their own. None are
required for scopecache to function — the cache is fully usable
with `Subscribe + Tail + DeleteUpTo` and a custom consumer in 4
lines of code, regardless of whether any addon ships.

### Crash-safety invariant (applies to any drainer)

Whatever the sink, the drainer must persist its cursor to its
durable storage **before** calling `DeleteUpTo`. Otherwise: drainer
crashes between `DeleteUpTo` and the cursor-save → restart loses the
just-deleted-but-not-yet-recorded batch → data lost (the items may
still exist in the sink but the cursor would re-Tail past them).

Generic order:

1. Tail
2. Write to sink (durably — fsync, COMMIT, ack, …)
3. Persist cursor (durably, ideally same transaction as step 2 if
   the sink supports transactions)
4. DeleteUpTo

If step 4 crashes, the next run sees a cursor past where the items
still are; re-running DeleteUpTo is idempotent. The "sink + cursor
in the same transaction" trick removes the gap between steps 2 and
3 — only available when the sink is transactional (SQLite, Postgres,
etc); for filesystem sinks, atomic-rename of a tmp-file works as a
weaker substitute.

## Failure modes

| Failure | What happens | Recovery |
|---|---|---|
| Drainer crash | `_log` accumulates, eventually 507s on writes | Restart drainer, resume from cursor |
| Disk full | Drainer can't write file, can't `DeleteUpTo`, log fills | External monitoring (TODO operational) |
| Drainer slow | `_log` accumulates, eventually the global `MaxStoreBytes` fires and 507s start; for the auto-populate path that's a silent drop (when wired), for external `/append` it's a real 507 | Bump `MaxStoreBytes`, tune drain cadence, fix drainer |
| Cache crash | In-memory `_log` lost; whatever wasn't drained is gone | Source-of-truth (DB) replay via `/rebuild` |

## Decisions made so far (lock-in candidates)

These should NOT need re-litigation in the implementation phase:

1. **Single-slot coalescing channel** — not deep-buffered. `select { case ch <- struct{}{}: default: }`.
2. **Per-scope subscribe** — not global. The Subscribe primitive takes a scope arg; in practice subscribers only call it for `_log` and `_inbox`, but the primitive itself is general.
3. **`_log` is auto-populated by core** — not by an addon-side hook registry.
4. **`_log` includes payload** — required for the file-drain pattern to be useful (files must be self-contained).
5. **Bulk operations emit summaries, not per-item events** — `/warm` and `/rebuild` produce 1 log entry each.
6. **Admin events are Op-types in `_log`** — `wipe`, `scope_detached`, `rebuild`, `warm`. No separate `_admin_log` scope.
7. **No `_notification` scope.** Health/capacity alerting is external (see CLAUDE.md "Pre-1.0 TODO operational").
8. **The cache stops at three primitives: Subscribe, Tail, DeleteUpTo.** Drainer shape (file format, sink type, batching cadence, fsync policy) is operator-side, not cache-side. The cache does not pick winners between JSONL / SQLite / external DB / Kafka / webhook sinks.
9. **Subscribe is a public Store method** — capitalised, the deliberate exception to "all *Store methods are lowercase".
10. **Basic boot-time init: cache pre-creates `_log` and `_inbox` unconditionally at boot.** Hardcoded in `NewStore` via `s.initReservedScopes()`. Subscribers can attach to either scope before any writes have happened (no "scope-doesn't-exist-yet" race). `_tokens` and other addon-convention scopes are NOT pre-created by core. Extensibility (config-driven scope list, boot-time hooks for external rebuild-scripts) is parked as a future-work TODO. **Implemented.**

11. **Two reserved scope names: `_log` and `_inbox`, with append-only drain-stream semantics.** The cache allows operations that fit the drain pattern and rejects the ones that don't:
    - **Allowed**: `/append` (apps → `_inbox`; cache auto → `_log`), `/delete` and `/delete_up_to` (drainer cleanup), `/get`/`/head`/`/tail`/`/render` (reads), `/stats`/`/scopelist` (observability).
    - **Rejected (400)**: `/upsert`, `/update`, `/counter_add` (no in-place mutation on a drain-stream — items are either still in buffer or already drained), `/delete_scope`, `/warm` (target reserved), `/rebuild` (input contains reserved).
    - **Atomic re-init**: `/wipe` drops everything and immediately re-creates the reserved scopes under the same all-shard write lock so subscribers don't see a gap; `/rebuild` does the same after the swap.

    The reservation is exactly two names — broader `_*` prefix is unchanged naming convention. Helper `isReservedScope(scope)` is the single source of truth used by validators and bulk paths. **Implemented.**

12. **Bootstrap pre-creation does not bump `s.lastWriteTS` or `buf.lastWriteTS`.** `NewStore` leaves both at 0 so the "have I seen this cache before" sentinel still works for fresh boots. After `/wipe` and `/rebuild`, the surrounding destructive op bumps `s.lastWriteTS`, and the reserved-scope re-creation aligns `buf.lastWriteTS` with that store-wide tick (preserving the `s.lastWriteTS >= max(buf.lastWriteTS)` invariant). **Implemented.**

13. **Per-reserved-scope capacity knobs decoupled from globals only where it buys something.** Resolves Q4-Q7. Final shape:

    - `_inbox` is operator-tunable on **two axes**: `Inbox.MaxItems` (default = global `ScopeMaxItems`, env `SCOPECACHE_INBOX_MAX_ITEMS`) and `Inbox.MaxItemBytes` (default 64 KiB, env `SCOPECACHE_INBOX_MAX_ITEM_KB`). Apps writing fan-in events typically need a much smaller per-item cap than user-scopes, and the item-count cap is independently tunable for high-throughput inboxes.
    - `_log` is **fully derived**, no knobs. Per-item cap = `MaxItemBytes + 1 KiB envelope slack` so a log entry always fits the user-write that produced it (operators tune `MaxItemBytes`; `_log` follows). Item-count cap = unbounded (`_log` is best-effort observability gated only by the global byte budget; an arbitrary count cap on a drain-stream is meaningless).
    - **No separate byte budget** for either reserved scope. Both share the global `MaxStoreBytes`. A separate budget was considered but adds complexity (two budgets to size, two cap-fire failure modes for operators to reason about) without buying anything: operators who want to give `_log`/`_inbox` more headroom bump `MaxStoreBytes` globally, and the scheduler-addon (TODO operational) covers proactive monitoring.

    Implementation locus: `Store.maxItemBytesFor(scope)` and `Store.maxItemsFor(scope)` in [store.go](../store.go); enforcement at `handleAppend` and inside `appendItem` (sentinel `b.maxItems == 0`). **Implemented.**

14. **HTTP response cap derived, not configured.** Per-response byte cap on `/head`, `/tail`, `/render` equals `MaxStoreBytes` so any single-scope read fits in one response by construction. No separate `MaxResponseBytes` knob — a value below `MaxStoreBytes` is just a misconfiguration that makes drainers flaky on full scopes; a value above is meaningless (no scope can hold more than the store). Resolved in `NewAPI` rather than `APIConfig.WithDefaults` because the derivation crosses structs. **Implemented.**

15. **External `/append` to reserved scopes is allowed (`_log` and `_inbox`).** Resolves Q15. The cache imposes shape rules on items targeting reserved scopes (per-item byte cap from §13, plus item-count cap on `_inbox`) but does not gate writes on caller identity — anyone with mux access can write. The cache itself never recurses (a future cache-internal write to `_log` triggered by an external write to `_log` would loop), so the auto-populate path will explicitly skip when the target scope IS `_log`. External writes to `_log` are unusual but harmless; external writes to `_inbox` are the **expected** path (apps populate `_inbox`; a drainer drains it). Cap-overflow on either reserved scope returns 507 — same hard-fail semantics as user-scopes for *external* writes. The "best-effort drop on overflow" semantics applies only to the future cache-internal auto-populate path (when it lands), and is a per-write-path policy, not a per-scope policy. **Implemented for the external path; auto-populate-side is open until that wires in.**

## Open design decisions

### Auto-populate semantics (deferred until auto-populate lands)

- **Q1 — Log policy on/off.** Should there be a master switch
  `EnableLogScope` to disable `_log` auto-populate entirely? Some
  workloads (pure read-cache) don't need `_log`, and skipping the
  hook removes write-path overhead. Default presumably `true` (the
  feature only earns its keep if it's on by default).

- **Q2 — `_log` payload toggle.** We've decided "with payload" is
  required for the drain-to-file architecture. Should there still
  be a `LogScopeIncludePayload` knob for operators who want only
  metadata (different use case: cheap audit-trail without persistence)?
  Or is the simpler "always with payload" the v1.0 commitment?

- **Q3 — Drain coalescing delay.** Drainer sleeps before tailing to
  let bursts coalesce. Default 0.5 s? Hardcoded or per-drainer-instance
  config? Per-scope config (different cadence for `_log` vs `_inbox`)?

### File handling

*Removed — these are drainer-side concerns, not cache-side. File
layout, cursor persistence format, fsync policy all live in the
drainer's implementation choices, varying per sink. The cache
provides Tail + DeleteUpTo; how the drainer remembers what it
consumed is the drainer's business.*

If a reference drainer ships (`addons/drainer-jsonl/` etc.), its
own README/docs cover layout, persistence, and fsync — not this
document.

### Event shapes

- **Q11 — Counter_add event payload.** When `/counter_add` produces a
  log entry, does the payload contain (a) the new value as JSON
  number, (b) the new value AND the delta `{value, by}`, or (c) just
  the delta? Affects replicator design.

- **Q12 — Delete-event metadata.** The delete log entry has no
  payload, but does it carry the pre-delete metadata (last-state's
  ts, last seq)? Useful for audit; tiny extra cost. Probably yes.

- **Q13 — `scope_detached` trigger.** When does this fire — only on
  `/delete_scope`, or also on `/wipe` (per scope) and `/rebuild`
  (per dropped scope)? Probably only `/delete_scope`; the bulk events
  cover wipe/rebuild themselves.

### Naming and scope-management

- **Q14 — `_log` and `_inbox` names hardcoded or configurable?**
  Proposal: hardcoded names, simpler. Operators concerned about
  collision with their own scope-namespacing prefix the cache with
  their own scheme. Or: `LogScopeName` / `InboxScopeName` config
  knobs defaulting to `_log` / `_inbox`?

### Subscribe semantics

- **Q16 — Subscribe-on-not-yet-existent scope.** Should
  `Store.Subscribe("foo")` succeed when scope "foo" has not been
  created yet? If yes, the cache must auto-create the (empty) scope
  buffer on Subscribe — same overhead reservation as `ensureScope`.
  In the new architecture the only realistic Subscribe targets are
  `_log` and `_inbox`, both pre-created — so this question matters
  less, but the primitive is generic.

- **Q17 — Scope-deleted-while-subscribed.** When `/delete_scope`
  removes a scope that has subscribers, the channel should close
  cleanly. Sentinel "scope detached" event before close, or silent
  close? Under coalescing-channel-of-struct{} this question reduces:
  there's no payload to carry the sentinel. Silent close + the
  `scope_detached` entry in `_log` is probably enough — subscribers
  who care can see that entry in their next drain.

- **Q18 — Lock order between `subsMu` and `b.mu`.** Subscribe pins a
  channel into the per-scope subscriber list under `subsMu`. Writes
  pin the buffer under `b.mu` and need to read the subscriber list
  for fanout. Proposal: `b.mu` first, snapshot the event under it,
  release `b.mu`, then `subsMu.RLock` for fanout. Verify no path
  takes them in the opposite order.

- **Q19 — Unsubscribe-during-fanout race.** Cache is in the
  `select { case ch <- evt: default: }` send when the subscriber's
  `unsub()` runs. If `unsub` closes `ch`, the send becomes
  send-on-closed → panic. Proposal: `unsub` does not close the
  channel; it sets a `closed` flag on the subscriber-entry under
  `subsMu`, and the channel is GC'd later. Cache checks `closed`
  before send. ~5 lines, no Go-idiom violation.

## Implementation outline

Already shipped (foundation):

- ✅ Boot-time + post-wipe + post-rebuild pre-creation of `_log` and
  `_inbox` (settled decisions 10, 11, 12).
- ✅ Reservation contract: scope-level destructive ops on reserved
  scopes return 400; in-place mutation (`/upsert`, `/update`,
  `/counter_add`) returns 400; item-level ops + reads work normally.
- ✅ Per-reserved-scope cap shape (settled decision 13): `Inbox.MaxItems`
  + `Inbox.MaxItemBytes` operator-tunable; `_log` derived
  (`MaxItemBytes + 1 KiB`) + exempt from item-count cap.
- ✅ HTTP response cap derived from `MaxStoreBytes` (settled 14).
- ✅ External `/append` to reserved scopes allowed with normal
  hard-fail-on-cap semantics (settled 15).

Remaining for Subscribe + auto-populate, once Q1-Q3, Q11-Q14, Q16-Q19
are answered:

1. **`subscribe.go`** in core (~120 lines): `Subscribe` method,
   `subscriber` struct, per-scope subscriber list with `subsMu`.
2. **Hooks in 5 write paths** (~30 lines total): `appendItem`,
   `upsertByID`, `updateByID`, `deleteByID`, `counterAdd`. Plus
   the bulk paths (`replaceScopes`, `wipe`, `rebuildAll`) and
   scope-delete (`deleteScope`).
3. **`_log` auto-populate** (~50 lines): a `logEvent` helper
   called from each write path that constructs and appends a log
   entry to the `_log` buffer (with recursion-guard for `_log`
   itself, and best-effort drop-on-overflow + `log_drops_total`
   counter exposed on `/stats`).
4. **Tests** (~250 lines): race detector, coalescing semantics,
   crash-safety, scope-detached-during-fanout, recursion-guard,
   drop-on-overflow.
5. **`addons/drainer/`** (~200 lines): drainer addon + integration
   tests.

Total remaining: ~600 lines of code, ~250 lines of tests, ~1 day
for core Subscribe + auto-populate and ~1 day for the drainer.

## Pointers

- CLAUDE.md Phase A entry — the canonical (compact) summary.
- CLAUDE.md "Pre-1.0 TODO (operational)" — health-check scheduler
  discussion.
- docs/scopecache-core-rfc.md §2.6 — the canonical core spec for
  the reservation contract and per-reserved-scope cap shape;
  already updated to match settled decisions 10-15.
