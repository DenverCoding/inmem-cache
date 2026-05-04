# Subscribe + subscriber architecture — decide-doc

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
                      →  buffer _events (append-only, with payload)
                                   │
                                   │ (single-slot coalescing notify via Subscribe)
                                   ▼
                          subscriber (operator-built, or chosen reference)
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
subscriber, and the subscriber's shape is **out of scope for the cache**.

This is the same lesson as the dropped `_notification` scope: ship
primitives, not solutions. Operators with different durability
preferences (filesystem vs SQLite vs external DB), different
processing semantics (real-time push vs batch every minute),
different downstream tooling (PHP workers vs Kubernetes jobs vs
serverless functions) all build their own subscriber or pick a
reference implementation that fits.

The cache itself ends at the three primitives — every "everything
must be possible" feature beyond that is operator-side.

## What this solves

| Concern | How |
|---|---|
| Persistence | Files on disk; cache may die, files survive |
| Audit-trail | Files ARE the audit log, append-only, timestamped |
| Decoupling | Consumers can be any language, any process, OS-event-driven |
| Recovery | Subscriber crash → restart from `lastSeq`, no state-loss |
| Pressure relief | `_events` / `_inbox` get drained continuously, never accumulate |
| Backup/replication | Standard tooling — rsync, cp, S3 sync — works on files |

## Cache-side primitives

### Subscribe

```go
func (s *Store) Subscribe(scope string) (<-chan struct{}, func(), error)
//                                          coalescing    unsubscribe  err
```

- **Restricted to reserved scopes (`_events`, `_inbox`).** Settled #2.
  Anything else → `ErrInvalidSubscribeScope`. User-managed scopes
  (which can run into the thousands) are observable via `_events`
  auto-populate; that's the point of `_events`.
- **Single subscriber per reserved scope.** Settled #20. A second
  Subscribe on the same scope while the first is active →
  `ErrAlreadySubscribed`. Realistic deployment is one internal Go
  subscriber per scope; multi-subscriber fanout would split work non-
  deterministically across the coalesced wake-up, the cache rejects
  that shape upfront.
- **Single-slot, size-1 buffered channel.** Non-blocking send: when
  the slot is full the cache drops the send (no-op). The pending
  notification already covers any subsequent write.
- **No slow-subscriber drop policy needed.** A subscriber that falls
  behind simply doesn't get a separate signal per missed write —
  they get one wake-up that covers all of them, and Tail-since-cursor
  catches them up.
- **Channel survives `/wipe` and `/rebuild` transparently.** The
  reserved scope is dropped + immediately re-created under the same
  all-shard write lock; the subscriber slot lives at Store level
  keyed by scope name (NOT in `*scopeBuffer`), so the subscription
  re-attaches to the freshly-recreated buffer without the subscriber
  having to reconnect. The subscriber detects wipe/rebuild via cursor-
  rewind on the next Tail call (`_events.lastSeq` < `lastSeenSeq`).
- **Capture-under-lock, emit-outside-lock.** Writes commit under
  `b.mu`; the notification fanout happens after `b.mu` is released,
  so slow channel-sends never extend the write-path's critical
  section.
- **Subscribe is a deliberate exception** to the "all `*Store`
  methods are lowercase" rule. Other Store methods are
  implementation details (sharding, byte-budget, lock order) that
  external callers must not touch. Subscribe is the opposite: a
  contract surface.

### `_events` scope (auto-populated)

The cache writes a log entry to `_events` on every successful mutation
to any scope (other than `_events` itself, to prevent recursion).

```jsonl
{"scope":"thread:42","id":"msg-99","seq":17,"ts":1746367932123456,"op":"append","payload":{...}}
{"scope":"thread:42","id":"msg-100","seq":18,"ts":1746367932124567,"op":"counter_add","payload":42}
{"scope":"thread:42","seq":18,"ts":1746367932125678,"op":"delete"}
{"scope":"_admin","seq":0,"ts":1746367932200000,"op":"wipe"}
```

Per Op, in `_events`:

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
race. Apps write to it; a subscriber addon drains it the same way as
`_events`. The cache never auto-writes here.

## Subscriber pattern

The subscriber is **not a structural component of the cache**; it is a
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

The repo may ship one or more reference subscribers as `addons/subscriber-*`
sub-packages, each demonstrating one common sink:

- `addons/subscriber-jsonl/` — per-batch JSONL files, inotify-friendly
- `addons/subscriber-sqlite/` — single SQLite database, transactional
- `addons/subscriber-postgres/` — INSERT into an external Postgres,
  treats Postgres as the source-of-truth-of-record
- (others as use cases emerge)

These are **examples and starting points**, not "the" subscriber.
Operators pick the one that fits, or write their own. None are
required for scopecache to function — the cache is fully usable
with `Subscribe + Tail + DeleteUpTo` and a custom consumer in 4
lines of code, regardless of whether any addon ships.

### Crash-safety invariant (applies to any subscriber)

Whatever the sink, the subscriber must persist its cursor to its
durable storage **before** calling `DeleteUpTo`. Otherwise: subscriber
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
| Subscriber crash | `_events` accumulates, eventually 507s on writes | Restart subscriber, resume from cursor |
| Disk full | Subscriber can't write file, can't `DeleteUpTo`, log fills | External monitoring (TODO operational) |
| Subscriber slow | `_events` accumulates, eventually the global `MaxStoreBytes` fires and 507s start; for the auto-populate path that's a silent drop (when wired), for external `/append` it's a real 507 | Bump `MaxStoreBytes`, tune drain cadence, fix subscriber |
| Cache crash | In-memory `_events` lost; whatever wasn't drained is gone | Source-of-truth (DB) replay via `/rebuild` |

## Decisions made so far (lock-in candidates)

These should NOT need re-litigation in the implementation phase:

1. **Single-slot coalescing channel** — not deep-buffered. `select { case ch <- struct{}{}: default: }`.
2. **Subscribe restricted to reserved scopes (`_events`, `_inbox`).** The cache does NOT support subscribing to user-managed scopes. Rationale: (a) `_events` is the singular monitoring channel for all writes — that is *why* it exists; thousands of per-user-scope subscribers would invert that design and add per-scope lock + fanout cost on the hot path for no realistic benefit. (b) The subscriber is an internal Go process, not many-clients-fan-out — there is one subscriber per reserved scope, sometimes one subscriber for both. (c) Reserved scopes always exist (pre-created at boot, re-created at /wipe and /rebuild), which closes Q16 and Q17 by construction. Implementation: `Subscribe` takes a `scope string` arg and returns `ErrInvalidSubscribeScope` for any non-reserved name; `isReservedScope(scope)` is the gate. Pre-1.0: the API stays string-based (rather than `SubscribeEvents()` / `SubscribeInbox()` per-method) so the validator carries the rule and a future expansion to more reserved names is a one-line constant change.
3. **`_events` is auto-populated by core** — not by an addon-side hook registry.
4. **`_events` includes payload** — required for the file-drain pattern to be useful (files must be self-contained).
5. **Bulk operations emit summaries, not per-item events** — `/warm` and `/rebuild` produce 1 log entry each.
6. **Admin events are Op-types in `_events`** — `wipe`, `scope_detached`, `rebuild`, `warm`. No separate `_admin_events` scope.
7. **No `_notification` scope.** Health/capacity alerting is external (see CLAUDE.md "Pre-1.0 TODO operational").
8. **The cache stops at three primitives: Subscribe, Tail, DeleteUpTo.** Subscriber shape (file format, sink type, batching cadence, fsync policy) is operator-side, not cache-side. The cache does not pick winners between JSONL / SQLite / external DB / Kafka / webhook sinks.
9. **Subscribe is a public Store method** — capitalised, the deliberate exception to "all *Store methods are lowercase".
10. **Basic boot-time init: cache pre-creates `_events` and `_inbox` unconditionally at boot.** Hardcoded in `NewStore` via `s.initReservedScopes()`. Subscribers can attach to either scope before any writes have happened (no "scope-doesn't-exist-yet" race). `_tokens` and other addon-convention scopes are NOT pre-created by core. Extensibility (config-driven scope list, boot-time hooks for external rebuild-scripts) is parked as a future-work TODO. **Implemented.**

11. **Two reserved scope names: `_events` and `_inbox`, with append-only drain-stream semantics.** The cache allows operations that fit the drain pattern and rejects the ones that don't:
    - **Allowed**: `/append` (apps → `_inbox`; cache auto → `_events`), `/delete` and `/delete_up_to` (subscriber cleanup), `/get`/`/head`/`/tail`/`/render` (reads), `/stats`/`/scopelist` (observability).
    - **Rejected (400)**: `/upsert`, `/update`, `/counter_add` (no in-place mutation on a drain-stream — items are either still in buffer or already drained), `/delete_scope`, `/warm` (target reserved), `/rebuild` (input contains reserved).
    - **Atomic re-init**: `/wipe` drops everything and immediately re-creates the reserved scopes under the same all-shard write lock so subscribers don't see a gap; `/rebuild` does the same after the swap.

    The reservation is exactly two names — broader `_*` prefix is unchanged naming convention. Helper `isReservedScope(scope)` is the single source of truth used by validators and bulk paths. **Implemented.**

12. **Bootstrap pre-creation does not bump `s.lastWriteTS` or `buf.lastWriteTS`.** `NewStore` leaves both at 0 so the "have I seen this cache before" sentinel still works for fresh boots. After `/wipe` and `/rebuild`, the surrounding destructive op bumps `s.lastWriteTS`, and the reserved-scope re-creation aligns `buf.lastWriteTS` with that store-wide tick (preserving the `s.lastWriteTS >= max(buf.lastWriteTS)` invariant). **Implemented.**

13. **Per-reserved-scope capacity knobs decoupled from globals only where it buys something.** Resolves Q4-Q7. Final shape:

    - `_inbox` is operator-tunable on **two axes**: `Inbox.MaxItems` (default = global `ScopeMaxItems`, env `SCOPECACHE_INBOX_MAX_ITEMS`) and `Inbox.MaxItemBytes` (default 64 KiB, env `SCOPECACHE_INBOX_MAX_ITEM_KB`). Apps writing fan-in events typically need a much smaller per-item cap than user-scopes, and the item-count cap is independently tunable for high-throughput inboxes.
    - `_events` is **fully derived**, no knobs. Per-item cap = `MaxItemBytes + 1 KiB envelope slack` so a log entry always fits the user-write that produced it (operators tune `MaxItemBytes`; `_events` follows). Item-count cap = unbounded (`_events` is best-effort observability gated only by the global byte budget; an arbitrary count cap on a drain-stream is meaningless).
    - **No separate byte budget** for either reserved scope. Both share the global `MaxStoreBytes`. A separate budget was considered but adds complexity (two budgets to size, two cap-fire failure modes for operators to reason about) without buying anything: operators who want to give `_events`/`_inbox` more headroom bump `MaxStoreBytes` globally, and the scheduler-addon (TODO operational) covers proactive monitoring.

    Implementation locus: `Store.maxItemBytesFor(scope)` and `Store.maxItemsFor(scope)` in [store.go](../store.go); enforcement at `handleAppend` and inside `appendItem` (sentinel `b.maxItems == 0`). **Implemented.**

14. **HTTP response cap derived, not configured.** Per-response byte cap on `/head`, `/tail`, `/render` equals `MaxStoreBytes` so any single-scope read fits in one response by construction. No separate `MaxResponseBytes` knob — a value below `MaxStoreBytes` is just a misconfiguration that makes subscribers flaky on full scopes; a value above is meaningless (no scope can hold more than the store). Resolved in `NewAPI` rather than `APIConfig.WithDefaults` because the derivation crosses structs. **Implemented.**

15. **External `/append` to reserved scopes is allowed (`_events` and `_inbox`).** Resolves Q15. The cache imposes shape rules on items targeting reserved scopes (per-item byte cap from §13, plus item-count cap on `_inbox`) but does not gate writes on caller identity — anyone with mux access can write. The cache itself never recurses (a future cache-internal write to `_events` triggered by an external write to `_events` would loop), so the auto-populate path will explicitly skip when the target scope IS `_events`. External writes to `_events` are unusual but harmless; external writes to `_inbox` are the **expected** path (apps populate `_inbox`; a subscriber drains it). Cap-overflow on either reserved scope returns 507 — same hard-fail semantics as user-scopes for *external* writes. The "best-effort drop on overflow" semantics applies only to the future cache-internal auto-populate path (when it lands), and is a per-write-path policy, not a per-scope policy. **Implemented for the external path; auto-populate-side is open until that wires in.**

16. **Reserved scope renamed: `_log` → `_events`.** The `_log` name was overloaded (Caddy logs, app logs, OS logs all compete for the term) and asymmetric with `_inbox`. `_events` describes what the scope contains (subscriber events) without committing to a transport direction (which `_outbox` would have done — we considered it but the email-flavor connotation made the name promise "leaves the system" semantics that are not actually required). Pre-1.0 breaking change applied to constants (`EventsScopeName`, `EventsItemEnvelopeOverhead`), Store fields (`eventsMaxItemBytes`), Config (`Events EventsConfig`), tests, RFC §2.6, and this document. **Implemented in step 5a.**

17. **Tri-state `EventsMode` enum, default `Off`.** Resolves Q1 + Q2 with one knob instead of two. Values: `EventsModeOff` (default — no auto-populate, zero overhead on the write path; opt-in when an operator has a subscriber ready), `EventsModeNotify` (events without payload — addressing only: op, scope, id?, seq, ts), `EventsModeFull` (events with action-payload). Adapter strings: `off`/`notify`/`full`; env `SCOPECACHE_EVENTS_MODE`; Caddyfile `events_mode notify`. Default = `Off` because the cost (extra write per operation) is paid 24/7 even without a subscriber; the benefit only materialises when a subscriber is consuming. **Config-shape implemented in step 5a; auto-populate behaviour wires in step 5b+.**

18. **Action-logging, not result-logging.** Resolves Q11 + Q12. Each event entry contains the inputs the caller sent plus the addressing the cache assigned (seq for newly-created or resolved-from-id paths). Never the result-side data: not "created vs replaced", not the new counter value, not deletion counts. Per endpoint, the `Full`-mode envelope is:

    | Endpoint | Envelope (Full mode) |
    |---|---|
    | `/append` | `{op, scope, id?, seq, ts, payload}` |
    | `/upsert` | `{op, scope, id, seq, ts, payload}` |
    | `/update by id` | `{op, scope, id, seq, ts, payload}` |
    | `/update by seq` | `{op, scope, seq, ts, payload}` |
    | `/counter_add` | `{op, scope, id, seq, by, ts}` *(no `value`)* |
    | `/delete by id` | `{op, scope, id, seq, ts}` |
    | `/delete by seq` | `{op, scope, seq, ts}` |
    | `/delete_up_to` | `{op, scope, max_seq, ts}` *(no `count`)* |
    | `/delete_scope` | `{op, scope, ts}` |
    | `/warm` | `{op, ts}` *(no scope-list, no count)* |
    | `/rebuild` | `{op, ts}` |
    | `/wipe` | `{op, ts}` |

    `Notify`-mode envelope is identical minus the `payload` field. Rationale: replay-able, matches WAL discipline downstream sinks expect, no waiting on the underlying op's result before emitting. Resolves Q13 (no separate `scope_detached` Op type — `/delete_scope` is itself the action-event). **Wires in step 5b+.**

19. **Reserved scope names hardcoded.** Resolves Q14. `_events` and `_inbox` are compile-time constants; not exposed as adapter knobs. Operators concerned about collision with their own scope-namespacing prefix their own scopes (e.g., `acme:posts:42`). An `EventsScopeName` / `InboxScopeName` operator-tunable knob would not solve the collision problem (third-party addons that also reference the constants would diverge per deployment). **Implemented.**

20. **Single subscriber per reserved scope.** A second `Subscribe` to the same scope while the first is still active returns `ErrAlreadySubscribed`. Multi-subscriber fanout (one wake-up to N subscribers) was considered but doesn't match any realistic deployment shape: the subscriber is an internal Go process, not a many-client broadcast, and two subscribers competing for the same scope would race on the coalesced single-slot wake-up — first one to read the slot wins, the other never sees the signal. The cache rejects that shape upfront. Composing two sinks (e.g. JSONL + webhook) is the subscriber's job: one subscriber reads, fans out to two writers internally. Implementation: per-scope `subscriber` field on `*Store` (NOT a list), set under `subsMu` on Subscribe, cleared on `unsub()`. ~10 lines saved vs the list-based shape.

21. **Subscribe state lives at Store level, not on `*scopeBuffer`.** Per-scope `subscriber` map keyed by scope name on `*Store`, guarded by `subsMu`. Survives buffer churn: when `/wipe` and `/rebuild` drop+recreate `_events` and `_inbox`, the subscriber slot stays attached and re-points at the freshly-recreated buffer transparently, so subscribers don't have to reconnect across destructive ops. Subscriber detects wipe/rebuild via cursor-rewind (`lastSeq` going backwards on the next Tail) and resets its own state — same shape as the no-emit decision for /wipe and /rebuild events.

22. **Default subscriber is a standard add-on, always linked, activated via one config knob.** Decision flips the previous "subscriber is operator-built or chosen reference" framing: the cache repo ships a default subscriber as a sub-package alongside the core (per Phase C standard-add-on tier in CLAUDE.md). It is **always** part of the build — no "minimal build without subscriber" mode — but its **runtime activation** is gated by one boolean config knob:

    - Standalone: `SCOPECACHE_SUBSCRIBER_ENABLED=true|false`
    - Caddyfile: `subscriber_enabled true|false`
    - Go API (Caddy module / standalone): `Subscriber.Enabled bool`

    When `false`: the adapter does not spawn the subscriber goroutine at all. `_events` and `_inbox` still exist (settled #10) but accumulate without being consumed; if the operator never wires their own subscriber, those scopes will eventually approach the global cap. Pure cache-only mode — the operator opted out of the auto-pipeline.

    When `true`: the adapter spawns the subscriber goroutine at boot via `store.Subscribe(EventsScopeName)` + `store.Subscribe(InboxScopeName)`, and the goroutine handles the wake-up loop, Tail batching, and DeleteUpTo cycle. `unsub()` is called automatically by the adapter at process shutdown / Caddy module Cleanup; operators never see it directly.

    What the activated subscriber actually **does** with consumed items is a separate, narrow set of additional knobs that the subscriber addon owns (sink choice, batch size, coalesce delay, etc.) — not the cache's concern. Those knobs live in the subscriber sub-package's own config block, not on `Config` or `APIConfig`. Cache-side this distinction is enforced architecturally: the subscriber package imports `scopecache` (uses `Subscribe`/`Tail`/`DeleteUpTo`); `scopecache` does not import the subscriber package.

23. **Subscriber default behaviour: invoke an operator-supplied script per batch.** The default subscriber, when activated, reads its sink choice from a single additional config knob (`subscriber_script /path/to/script`) that points at an executable. On each wake-up: `Tail` a batch, marshal items to the script's stdin as JSONL, exec the script, wait for exit code, `DeleteUpTo` on success. This pushes every sink-specific concern (file format, DB driver, HTTP endpoint, retry policy, fsync semantics) into the operator's script — the subscriber package itself stays stdlib-only and sink-agnostic. When `subscriber_enabled=true` AND `subscriber_script` is unset: the subscriber runs but does nothing (silent no-op — Tail + DeleteUpTo without invoking anything, useful for pure cap-relief). Sink-specific Go-side subscribers (subscriber-jsonl, subscriber-mercure, subscriber-sqlite, etc.) are an open Phase C question — the script-runner default may make them unnecessary. **Detail-level decisions for step 7.**

### Auto-populate semantics (still open)

- **Q3 — Drain coalescing delay.** Subscriber sleeps before tailing to
  let bursts coalesce. Default 0.5 s? Hardcoded or per-subscriber-instance
  config? Per-scope config (different cadence for `_events` vs `_inbox`)?

- **Event-emit ordering.** Decided: capture-under-lock, emit-outside-lock
  (option 1 of three considered). Same pattern as Subscribe's wake-up
  fanout (settled #9). Two parallel writes can land in `_events` in
  different `_events.seq` order than the underlying ops happened;
  subscribers that need strict per-scope replay sort by `(payload.scope,
  payload.seq)` within a batch, which they have to do anyway to
  handle interleaved events from different scopes. Documented as a
  known characteristic of the stream rather than a bug.

### File handling

*Removed — these are subscriber-side concerns, not cache-side. File
layout, cursor persistence format, fsync policy all live in the
subscriber's implementation choices, varying per sink. The cache
provides Tail + DeleteUpTo; how the subscriber remembers what it
consumed is the subscriber's business.*

If a reference subscriber ships (`addons/subscriber-jsonl/` etc.), its
own README/docs cover layout, persistence, and fsync — not this
document.

### Subscribe semantics

- **Q16 — Subscribe-on-not-yet-existent scope.** ✅ **Resolved by
  settled #2 (restrict to reserved scopes).** Reserved scopes are
  pre-created at boot via `initReservedScopes`, so a Subscribe to
  `_events` or `_inbox` always finds the buffer present. Non-reserved
  scopes return `ErrInvalidSubscribeScope` upfront — auto-create-on-
  Subscribe is not needed.

- **Q17 — Scope-deleted-while-subscribed.** ✅ **Resolved by
  settled #2 + #21.** Reserved scopes cannot be `/delete_scope`'d
  (validator rejects the request — the reserved-scopes contract pre-
  dates auto-populate). The only destructive ops that touch them are
  `/wipe` and `/rebuild`, both of which drop+recreate the reserved
  scopes under the same all-shard write lock. Per settled #21 the
  Subscribe state lives at Store level keyed by scope name, so the
  subscription transparently re-attaches to the recreated buffer; the
  channel never closes and the subscriber never has to reconnect. Wipe-
  detection is the subscriber's job via cursor-rewind on the next Tail.

- **Q18 — Lock order between `subsMu` and `b.mu`.** Subscribe pins a
  channel into the Store-level `subscriber` slot under `subsMu`.
  Writes pin the buffer under `b.mu` and need to read the subscriber
  slot for fanout. Proposal: `b.mu` first, snapshot the event under
  it, release `b.mu`, then `subsMu.RLock` for fanout. Verify no path
  takes them in the opposite order. **Mostly mechanical at this
  point**; one-decision-per-write-path so a `go vet` shadow-check is
  enough to keep us honest.

- **Q19 — Unsubscribe-during-fanout race.** ✅ **Resolved: close-
  on-unsub with lock-discipline.** `unsub()` takes `subsMu.Lock`,
  removes the entry from the map, then `close(sub.ch)` — all under
  the same Lock. Cache notify takes `subsMu.RLock` for the lookup
  AND the non-blocking send, releasing only after the `select` has
  returned. This serialises unsub against any in-flight notify: the
  notify either finds the subscriber and sends (before unsub gets
  the Lock) or doesn't find it (after unsub removed it from the
  map). Send-on-closed-channel cannot happen because the channel is
  only closed after the map entry is gone. The non-blocking
  `select` is microseconds, so holding RLock through it is safe.
  Subscriber's loop becomes the idiomatic `for range ch { … }` —
  exits naturally on close, no separate context.Done() machinery
  needed.

## Implementation outline

Already shipped (foundation):

- ✅ Boot-time + post-wipe + post-rebuild pre-creation of `_events` and
  `_inbox` (settled decisions 10, 11, 12).
- ✅ Reservation contract: scope-level destructive ops on reserved
  scopes return 400; in-place mutation (`/upsert`, `/update`,
  `/counter_add`) returns 400; item-level ops + reads work normally.
- ✅ Per-reserved-scope cap shape (settled decision 13): `Inbox.MaxItems`
  + `Inbox.MaxItemBytes` operator-tunable; `_events` derived
  (`MaxItemBytes + 1 KiB`) + exempt from item-count cap.
- ✅ HTTP response cap derived from `MaxStoreBytes` (settled 14).
- ✅ External `/append` to reserved scopes allowed with normal
  hard-fail-on-cap semantics (settled 15).
- ✅ Step 5a: rename `_log` → `_events` (settled 16); `EventsMode`
  tri-state config (settled 17); action-logging envelope shape
  (settled 18); hardcoded reserved-scope names (settled 19).
  No auto-populate behaviour wired yet — `Mode` stored on Store
  but consulted by no write path.

Already shipped (auto-populate, v0.7.25):

- ✅ Step 5b — `_events` auto-populate on `/append` (commit 18aadea).
- ✅ Step 5c — auto-populate hooks on `/upsert`, `/update`,
  `/counter_add`, `/delete`, `/delete_up_to` (commits 85d5d9a, 3208063).
- ✅ Step 5d — bulk-path: `/warm` and `/delete_scope` emit; `/wipe` and
  `/rebuild` deliberately do NOT emit (they wipe `_events` itself —
  drainer detects via cursor-rewind) (commit a674d00).
- ✅ /stats `reserved_scopes` block — per-scope state for `_events`
  and `_inbox` on every /stats call (commit ff4e1b9).

Remaining:

1. **Step 5e** — best-effort drop-on-overflow + `events_drops_total`
   atomic counter; surface on `/stats` (one-line addition to the
   /stats enrichment TODO). ~20 lines + tests. (Already partially
   wired via `eventsDropsTotal` atomic; just needs the /stats
   surface and tests for the drop-on-overflow path.)
2. **Step 6** — `Subscribe` primitive in core (~80 lines) in a new
   `subscribe.go` file:
   `Store.Subscribe(scope) (<-chan struct{}, func(), error)` —
   restricted to reserved scopes (settled #2), single subscriber
   per scope (settled #20), state at Store level keyed by name
   (settled #21), close-on-unsub with lock-discipline (settled Q19).
   Notify hook in `Store.appendOne` (single site — all writes to
   `_events` and `_inbox` route through this method).
3. **Step 7** — default subscriber addon as standard add-on
   (~150 lines): script-runner shape per settled #23. Sub-package
   under repo (Phase C). Adapter-side activation knob
   `subscriber_enabled` on standalone (env `SCOPECACHE_SUBSCRIBER_ENABLED`)
   and Caddy module (`subscriber_enabled true` Caddyfile directive).
   Operator-supplied `subscriber_script` is the only sink-specific
   knob the cache exposes; everything else is the script's
   responsibility.

## Pointers

- CLAUDE.md Phase A entry — the canonical (compact) summary.
- CLAUDE.md "Pre-1.0 TODO (operational)" — health-check scheduler
  discussion.
- docs/scopecache-core-rfc.md §2.6 — the canonical core spec for
  the reservation contract and per-reserved-scope cap shape;
  already updated to match settled decisions 10-15.
