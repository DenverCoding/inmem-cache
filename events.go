package scopecache

import "encoding/json"

// Auto-populate of the reserved `_events` scope.
//
// On every successful mutation to a non-`_events` scope, the cache
// emits a write-event entry to `_events` whose payload is a JSON
// object describing the action that just committed. Drainers
// subscribe to `_events` and stream entries to whatever sink they
// prefer (JSONL, SQLite, external DB, Kafka, webhook, …).
//
// Three behaviour gates, in order:
//
//  1. Config.Events.Mode (off / notify / full). Default off — zero
//     overhead on the write path. Notify omits the user payload;
//     Full includes it.
//  2. Recursion guard. A /append to `_events` itself does NOT
//     trigger a second event. Without this the cache would loop on
//     every emit and saturate `_events` with self-referential entries.
//  3. Best-effort drop on cap-overflow. The emit goes through normal
//     admission control. If the byte cap fires the user-write STILL
//     succeeds; we drop the event and bump eventsDropsTotal. The
//     user-visible result of the underlying mutation is never
//     affected by an event drop.
//
// Capture-under-lock, emit-outside-lock: emit calls happen AFTER
// the user-scope's b.mu has been released. Field values are passed
// in by value from the returned Item snapshot; safe to use without
// re-locking.
//
// Single-level recursion: emitAppendEvent → appendOne(_events) →
// emitAppendEvent short-circuits on the guard. Two stack frames, no
// loop.

// writeEvent is the JSON shape of an entry's payload in the
// reserved `_events` scope. The cache marshals one writeEvent per
// committed mutation (when Mode != Off) and stores the marshaled
// bytes as the entry's Item.Payload.
//
// Action-payload, not result-payload: the cache logs the inputs the
// caller sent, never the result it computed. /counter_add events
// carry `By` (the increment), not the new value; /delete_up_to
// events carry `MaxSeq`, not the deleted-count. This matches the
// WAL discipline downstream sinks expect — events are replay-able
// against an empty cache to reconstruct state.
//
// Field shape per op:
//
//	append       — scope, id?, seq, ts, payload?
//	upsert       — scope, id, seq, ts, payload?
//	update       — scope, id|seq, payload?      (no ts; updateByID/Seq don't return it)
//	counter_add  — scope, id, by                (no payload, no ts)
//	delete       — scope, id|seq                (no payload, no ts)
//	delete_up_to — scope, max_seq               (no id, no per-item seq, no payload, no ts)
//	warm         — ts                           (no scope: /warm replaces multiple scopes;
//	                                             list omitted to avoid wire bloat)
//	delete_scope — scope, ts                    (the scope being deleted)
//
// Notably absent: /wipe and /rebuild. Both ops obliterate `_events`
// itself as part of their work (the cache is reset to its boot
// configuration including a fresh empty `_events`), so emitting an
// event INTO the very scope being wiped is paradoxical. Drainers
// detect wipe/rebuild via cursor-rewind (`_events.lastSeq` <
// `lastSeenSeq`) and reset their state accordingly.
//
// Optional fields use `omitempty`. By is *int64 so by:0 (a no-op
// counter action) is representable on the wire while non-counter
// envelopes leave the field absent. MaxSeq=0 cannot collide because
// /delete_up_to with max_seq=0 is a no-op (assignments start at 1)
// and we skip emitting on count=0 anyway.
type writeEvent struct {
	Op      string          `json:"op"`
	Scope   string          `json:"scope,omitempty"`
	ID      string          `json:"id,omitempty"`
	Seq     uint64          `json:"seq,omitempty"`
	Ts      int64           `json:"ts,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
	By      *int64          `json:"by,omitempty"`
	MaxSeq  uint64          `json:"max_seq,omitempty"`
}

// emitEvent is the shared back-half: Notify-mode payload strip +
// json.Marshal + recursive appendOne into `_events` + drop-on-error.
// The two early gates (mode check, recursion guard) live at each
// per-op caller instead of here so events_mode=off — the default —
// pays exactly one atomic load + compare per write, never the cost
// of constructing the writeEvent struct or the function call into
// this body.
//
// Caller invariants (NOT re-checked here):
//   - s.eventsMode != EventsModeOff
//   - evt.Scope != EventsScopeName (recursion guard)
//
// If both checks moved here the off-path would still construct the
// 8-field struct value-arg before reaching the early-return — a
// measured ~+150 ns/op regression on the BenchmarkStore_Append
// hot path. Splitting the gates out keeps the off-path one branch
// short.
func (s *store) emitEvent(evt writeEvent) {
	if s.eventsMode == EventsModeNotify {
		// Notify keeps the action-vector (op/scope/id/seq/ts + any
		// op-specific fields like By) but drops the user payload.
		// Drainers waking up on Notify re-fetch from cache state,
		// which is faster and cheaper than carrying the payload
		// inline twice (in the user scope and in `_events`).
		evt.Payload = nil
	}
	body, err := json.Marshal(evt)
	if err != nil {
		// json.Marshal on a writeEvent whose fields are all stdlib
		// types should never fail in practice — defensive only.
		s.eventsDropsTotal.Add(1)
		return
	}
	if _, err := s.appendOne(Item{Scope: EventsScopeName, Payload: body}); err != nil {
		// Cap overflow on `_events` (or any other failure) — drop
		// silently. The original user-write already committed.
		s.eventsDropsTotal.Add(1)
	}
}

// eventsEnabled is the shared off-mode/recursion-guard fast path. Per-op
// emit helpers call this first; on miss they return WITHOUT building
// the writeEvent struct (which is what made the centralised version
// slow on events_mode=off). Inlined easily by the compiler at every
// call site (3-instruction body).
func (s *store) eventsEnabled(scope string) bool {
	return s.eventsMode != EventsModeOff && scope != EventsScopeName
}

// emitAppendEvent fires after a successful Store.appendOne commit.
func (s *store) emitAppendEvent(scope, id string, seq uint64, ts int64, payload json.RawMessage) {
	if !s.eventsEnabled(scope) {
		return
	}
	s.emitEvent(writeEvent{
		Op: "append", Scope: scope, ID: id, Seq: seq, Ts: ts, Payload: payload,
	})
}

// emitUpsertEvent — same envelope as /append (scope, id, seq, ts,
// payload). The Op string is the only wire-level difference; drainers
// distinguish create-vs-replace by the `created` field on the HTTP
// response, not the event (action-logging: the action is "upsert this
// id with this payload", regardless of whether the cache was empty).
func (s *store) emitUpsertEvent(scope, id string, seq uint64, ts int64, payload json.RawMessage) {
	if !s.eventsEnabled(scope) {
		return
	}
	s.emitEvent(writeEvent{
		Op: "upsert", Scope: scope, ID: id, Seq: seq, Ts: ts, Payload: payload,
	})
}

// emitUpdateEvent emits one of two shapes depending on how the user
// addressed the item:
//   - by id: id non-empty, seq=0 (omitempty drops it from the wire)
//   - by seq: id empty, seq non-zero
//
// Either way the action is "set this address to this payload"; the
// post-update Ts is not carried (updateByID/Seq don't return it and
// changing those signatures was not worth the spread for the small
// observability win — drainers needing freshness can /get the item).
func (s *store) emitUpdateEvent(scope, id string, seq uint64, payload json.RawMessage) {
	if !s.eventsEnabled(scope) {
		return
	}
	s.emitEvent(writeEvent{
		Op: "update", Scope: scope, ID: id, Seq: seq, Payload: payload,
	})
}

// emitCounterAddEvent carries the action-input By (the increment),
// not the post-add Value. Replay against an empty cache reconstructs
// the same total because counter_add is associative.
func (s *store) emitCounterAddEvent(scope, id string, by int64) {
	if !s.eventsEnabled(scope) {
		return
	}
	s.emitEvent(writeEvent{
		Op: "counter_add", Scope: scope, ID: id, By: &by,
	})
}

// emitDeleteEvent — addressed by id OR seq, whichever the user
// supplied. id == "" means "addressed by seq"; the validator already
// enforced id-xor-seq at the request layer. No payload (delete carries
// none) and no ts (deleteByID/Seq return only the deleted-count, not
// the deleted item's stored ts — and the action-vector is fully
// captured by scope+address anyway).
//
// Caller (Store.deleteOne) emits only on hit (count > 0); a miss is a
// no-op against cache state and replay reconstructs the same final
// state without it.
func (s *store) emitDeleteEvent(scope, id string, seq uint64) {
	if !s.eventsEnabled(scope) {
		return
	}
	s.emitEvent(writeEvent{
		Op: "delete", Scope: scope, ID: id, Seq: seq,
	})
}

// emitDeleteUpToEvent — bulk-delete with the cursor as action-vector.
// MaxSeq is the only meaningful field beyond scope; per-item seqs are
// not enumerated (drainers replaying against a populated cache walk
// b.items and apply the same delete-everything-<=N rule).
//
// Caller (Store.deleteUpTo) emits only on hit (count > 0); a no-op
// /delete_up_to (no items at or below the cursor) does not change
// cache state and is not emitted.
func (s *store) emitDeleteUpToEvent(scope string, maxSeq uint64) {
	if !s.eventsEnabled(scope) {
		return
	}
	s.emitEvent(writeEvent{
		Op: "delete_up_to", Scope: scope, MaxSeq: maxSeq,
	})
}

// emitWarmEvent fires after a successful /warm (Store.replaceScopes).
// Envelope is exactly {op: "warm", ts: nowUs} — no scope list (would
// bloat the wire on big batches), no item count (a result, not an
// action input). Drainers needing the list of warmed scopes can
// /scopelist after waking up; the event is just a "something
// large-scale happened, you should reconcile" pulse.
//
// No eventsEnabled scope check — /warm rejects reserved scopes at the
// validator (see replaceScopes), so the recursion guard is never
// reachable. Direct mode-check is enough.
func (s *store) emitWarmEvent() {
	if s.eventsMode == EventsModeOff {
		return
	}
	s.emitEvent(writeEvent{Op: "warm", Ts: nowUnixMicro()})
}

// emitDeleteScopeEvent fires after a successful /delete_scope on a
// non-empty user scope. Envelope: {op: "delete_scope", scope: X, ts:
// nowUs}. The eventsEnabled check is academic here (the validator
// rejects reserved scopes from /delete_scope upstream) but kept for
// uniformity with the per-op helpers above.
//
// /delete_scope on a missing scope is a no-op; caller (Store.deleteScope)
// gates the emit on the success bool, so empty/non-existent scopes
// never reach this helper.
func (s *store) emitDeleteScopeEvent(scope string) {
	if !s.eventsEnabled(scope) {
		return
	}
	s.emitEvent(writeEvent{
		Op: "delete_scope", Scope: scope, Ts: nowUnixMicro(),
	})
}
