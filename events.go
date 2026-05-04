package scopecache

import "encoding/json"

// Auto-populate of the reserved `_events` scope.
//
// On every successful mutation to a non-`_events` scope, the cache
// emits a write-event entry to `_events` whose payload is a JSON
// object describing the action that just committed. Drainers
// subscribe to `_events` (Phase A "Subscribe + events drain
// architecture") and stream entries to whatever sink they prefer:
// JSONL files, SQLite, external DB, Kafka, webhook, …
//
// Three behaviour gates, in order:
//
//  1. Config.Events.Mode (off / notify / full). Default off — zero
//     overhead on the write path. Notify omits the user payload from
//     the event; Full includes it.
//  2. Recursion guard. A /append to `_events` itself (allowed by the
//     reservation contract — see RFC §2.6) does NOT trigger a second
//     event. Without this guard the cache would loop on every event
//     emit and saturate `_events` with self-referential entries.
//  3. Best-effort drop on cap-overflow. The event-emit goes through
//     the normal admission control (per-item cap on `_events`,
//     store-wide byte cap). If the byte cap fires the user-write
//     STILL succeeds; we just drop the event silently and bump
//     Store.eventsDropsTotal. The user-visible result of the
//     underlying mutation is never affected by an event drop.
//
// Capture-under-lock, emit-outside-lock: emit calls happen AFTER
// the user-scope's b.mu has been released (the wrapping Store
// method, e.g. appendOne, returns from buf.appendItem and only then
// invokes the emit). Field values are passed in by value from the
// returned Item snapshot; safe to use without re-locking.
//
// Single-level recursion: emitAppendEvent calls s.appendOne with
// scope=`_events`. That recursive call commits, then triggers
// another emitAppendEvent — which short-circuits on the recursion
// guard (scope == EventsScopeName). Two stack frames, no loop.

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
//	append      — scope, id?, seq, ts, payload?
//	upsert      — scope, id, seq, ts, payload?
//	update      — scope, id|seq, payload?      (no ts; updateByID/Seq don't return it)
//	counter_add — scope, id, by                (no payload, no ts)
//
// Optional fields use `omitempty`: a zero Seq is absent (so /update
// by-id envelopes don't carry seq:0), a nil Payload is absent (Notify
// mode strips it), a zero Ts is absent. By is *int64 so by:0 is
// representable (a literal "increment-by-zero" action) while leaving
// the field absent for non-counter ops.
type writeEvent struct {
	Op      string          `json:"op"`
	Scope   string          `json:"scope"`
	ID      string          `json:"id,omitempty"`
	Seq     uint64          `json:"seq,omitempty"`
	Ts      int64           `json:"ts,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
	By      *int64          `json:"by,omitempty"`
}

// emitEvent is the shared fan-out for every per-op emit helper. It
// owns the three gates from the file-level comment (mode check,
// recursion guard, drop-on-overflow) plus the Notify-mode payload
// strip. Per-op helpers (emitAppendEvent, emitUpsertEvent, …) build
// the writeEvent and hand it off here; this file is the single
// source of truth for "how an event reaches `_events`".
//
// The recursion guard reads evt.Scope (the user-scope being
// mutated). If a caller wrote directly to `_events` — allowed by
// RFC §2.6 — evt.Scope == EventsScopeName and we short-circuit.
// Without this guard the recursive appendOne below would emit a
// second event for "the cache observed the previous _events write",
// and so on, until the byte cap fires.
func (s *Store) emitEvent(evt writeEvent) {
	if s.eventsMode == EventsModeOff || evt.Scope == EventsScopeName {
		return
	}
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

// emitAppendEvent — see file-level comment. Called by Store.appendOne
// after a successful buf.appendItem commit.
func (s *Store) emitAppendEvent(scope, id string, seq uint64, ts int64, payload json.RawMessage) {
	s.emitEvent(writeEvent{
		Op: "append", Scope: scope, ID: id, Seq: seq, Ts: ts, Payload: payload,
	})
}

// emitUpsertEvent — same envelope as /append (scope, id, seq, ts,
// payload). The Op string is the only wire-level difference; drainers
// distinguish create-vs-replace by the `created` field on the HTTP
// response, not the event (action-logging: the action is "upsert this
// id with this payload", regardless of whether the cache was empty).
func (s *Store) emitUpsertEvent(scope, id string, seq uint64, ts int64, payload json.RawMessage) {
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
func (s *Store) emitUpdateEvent(scope, id string, seq uint64, payload json.RawMessage) {
	s.emitEvent(writeEvent{
		Op: "update", Scope: scope, ID: id, Seq: seq, Payload: payload,
	})
}

// emitCounterAddEvent carries the action-input By (the increment),
// not the post-add Value (the result). Replay against an empty cache
// reconstructs the same value because counter_add is associative:
// applying By repeatedly from zero produces the same total.
//
// By is *int64 so by:0 (a no-op action) is still representable on the
// wire; a non-counter envelope leaves it nil and `omitempty` drops
// the field entirely.
func (s *Store) emitCounterAddEvent(scope, id string, by int64) {
	s.emitEvent(writeEvent{
		Op: "counter_add", Scope: scope, ID: id, By: &by,
	})
}
