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
// Notify mode omits Payload via the json `omitempty` tag (a nil
// json.RawMessage has len 0, which encoding/json treats as empty).
// Full mode assigns the user payload verbatim.
type writeEvent struct {
	Op      string          `json:"op"`
	Scope   string          `json:"scope"`
	ID      string          `json:"id,omitempty"`
	Seq     uint64          `json:"seq"`
	Ts      int64           `json:"ts"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// emitAppendEvent fans out a single /append event to the reserved
// `_events` scope. See the file-level comment for the three gates
// (mode, recursion guard, drop-on-overflow) and the
// capture/emit lock discipline.
//
// Caller is Store.appendOne after a successful buf.appendItem
// commit. The values passed in are a snapshot from the returned
// Item; the caller has already released b.mu of the user-scope
// buffer, so this method is free to take any other lock without
// nesting.
func (s *Store) emitAppendEvent(scope, id string, seq uint64, ts int64, payload json.RawMessage) {
	if s.eventsMode == EventsModeOff || scope == EventsScopeName {
		return
	}
	evt := writeEvent{
		Op:    "append",
		Scope: scope,
		ID:    id,
		Seq:   seq,
		Ts:    ts,
	}
	if s.eventsMode == EventsModeFull {
		evt.Payload = payload
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
