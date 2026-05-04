package scopecache

import (
	"errors"
)

// Subscribe primitive — see docs/subscribe-drain-decide.md for the
// full design discussion. In short: a Go-only, in-process mechanism
// by which an addon (the "subscriber") gets coalesced wake-up signals
// when items land in `_events` or `_inbox`. The subscriber drains the
// scope via Tail + DeleteUpTo in its own loop; this file owns ONLY
// the wake-up channel + lifecycle.
//
// Settled invariants (decide-doc decisions referenced inline):
//
//   - #2  Restricted to reserved scopes (`_events`, `_inbox`); other
//         scopes return ErrInvalidSubscribeScope. User-managed scopes
//         are observable via `_events` auto-populate — that is why
//         `_events` exists.
//   - #20 Single subscriber per reserved scope; a second Subscribe
//         to the same scope returns ErrAlreadySubscribed.
//   - #21 Subscriber state at Store level keyed by scope name, NOT
//         on `*scopeBuffer`. Survives /wipe and /rebuild buffer churn
//         transparently — drainer doesn't reconnect across destructive
//         ops; cursor-rewind detection on the next Tail is enough.
//   - #1  Single-slot, size-1 buffered channel with non-blocking send
//         + drop-on-full. 10k writes while subscriber is busy coalesce
//         to one wake-up; subscriber re-Tails and processes the batch
//         via cursor.
//   - Q19 close-on-unsub with lock-discipline: unsub() takes subsMu
//         .Lock, removes the map entry, then close(ch) — all under the
//         same Lock. Notify takes subsMu.RLock through the select-send
//         (microseconds, non-blocking). Send-on-closed-channel cannot
//         happen because the channel is only closed AFTER the map entry
//         is gone.
//
// Lock-order:
//
//	Notify path:    [b.mu released] → subsMu.RLock → select-send → RUnlock
//	Subscribe path: subsMu.Lock → mutate map → Unlock
//	Unsubscribe:    subsMu.Lock → mutate map → close(ch) → Unlock
//
// b.mu and subsMu are independent — the notify hook in Store.appendOne
// fires AFTER buf.appendItem returns (b.mu already released). No path
// nests one inside the other.

// ErrInvalidSubscribeScope is returned by Store.Subscribe when the
// supplied scope is not one of the cache's reserved scope names
// (`_events`, `_inbox`). See decide-doc settled #2.
var ErrInvalidSubscribeScope = errors.New("scopecache: subscribe is restricted to reserved scopes")

// ErrAlreadySubscribed is returned by Store.Subscribe when the
// supplied scope already has an active subscriber. See decide-doc
// settled #20: the cache rejects multi-subscriber fanout outright;
// composing two sinks (e.g. JSONL + webhook) is the subscriber's
// job, not the cache's.
var ErrAlreadySubscribed = errors.New("scopecache: scope already has an active subscriber")

// subscriber is the per-scope subscription state held at Store level
// in s.subscribers. ch is the size-1 coalescing wake-up channel; the
// same channel is returned to the subscriber goroutine. close-on-
// unsub means the channel itself doubles as the loop-exit signal —
// no separate closed-flag, no context.Done() machinery needed in
// caller code.
//
// Lifetime:
//   - Created in Subscribe under subsMu.Lock, inserted into
//     s.subscribers[scope].
//   - Read by notifySubscriber under subsMu.RLock for fanout.
//   - Removed and channel closed by Subscribe's returned unsub func
//     under subsMu.Lock.
//
// No additional fields needed: scope identity is implicit in the map
// key; close(ch) is the only "shutdown signal" surface.
type subscriber struct {
	ch chan struct{}
}

// Subscribe attaches a coalescing wake-up channel to the named
// reserved scope. Returns the channel, an unsub function, and an
// error. The subscriber goroutine loops `for range ch { … }`; calling
// unsub() closes the channel and the loop exits naturally.
//
// Errors:
//
//   - ErrInvalidSubscribeScope when scope is not a reserved scope
//     (the cache only supports subscribing to `_events` and `_inbox`).
//   - ErrAlreadySubscribed when the scope already has an active
//     subscriber.
//
// The returned channel survives /wipe and /rebuild transparently: the
// subscriber slot lives at Store level (keyed by scope name, NOT on
// the underlying *scopeBuffer), so when those destructive ops
// drop+recreate the reserved scope buffers the subscriber stays
// attached and re-points at the freshly-recreated buffer. The
// subscriber detects wipe/rebuild via cursor-rewind on the next Tail
// (lastSeq going backwards relative to the cursor it persisted) and
// resets its own state accordingly.
//
// Subscribe is the only capitalised method on *Store besides
// NewStore / NewAPI / RegisterRoutes — see CLAUDE.md "Public API
// surface" for the rationale.
func (s *store) Subscribe(scope string) (<-chan struct{}, func(), error) {
	if !isReservedScope(scope) {
		return nil, nil, ErrInvalidSubscribeScope
	}

	s.subsMu.Lock()
	defer s.subsMu.Unlock()

	if _, exists := s.subscribers[scope]; exists {
		return nil, nil, ErrAlreadySubscribed
	}

	sub := &subscriber{ch: make(chan struct{}, 1)}
	s.subscribers[scope] = sub

	unsub := func() { s.unsubscribe(scope) }
	return sub.ch, unsub, nil
}

// unsubscribe releases the subscription on `scope`. Idempotent: a
// second call after the entry is already gone is a no-op (a caller
// pattern of `defer unsub()` plus an explicit unsub() during shutdown
// won't double-close the channel).
//
// Order of operations under subsMu.Lock:
//  1. Find subscriber; bail if absent.
//  2. delete from map (no further notify will see the subscriber).
//  3. close(ch) (the subscriber's `for range ch` exits).
//
// Step 2 before step 3 is what makes this race-free: a notify that
// already passed the RLock-acquire-and-find phase is in flight with
// the channel pointer captured locally; it will either complete its
// send (slot empty) or hit the default branch (slot full) — either
// way it returns before unsub's Lock-acquire even completes. After
// step 2, no new notify can find the subscriber, so close(ch) in
// step 3 cannot race with a send.
func (s *store) unsubscribe(scope string) {
	s.subsMu.Lock()
	defer s.subsMu.Unlock()

	sub, exists := s.subscribers[scope]
	if !exists {
		return
	}
	delete(s.subscribers, scope)
	close(sub.ch)
}

// notifySubscriber fires a coalescing wake-up to the subscriber on
// `scope`, if one exists. Single-slot non-blocking send: when the slot
// is full the send is dropped (`default` branch) — the pending
// notification already covers any subsequent write, and the
// subscriber catches up via cursor on its next drain.
//
// Caller invariants (NOT re-checked here):
//
//   - b.mu (the per-scopeBuffer write lock) has been released;
//     notifySubscriber must not nest inside b.mu.
//   - Called from Store.appendOne after a successful commit + emit.
//     This is the ONLY call site, because every write that targets
//     `_events` or `_inbox` routes through appendOne (validator
//     rejects /upsert /update /counter_add on reserved scopes).
//
// RLock is held through the select-send so unsub cannot run concurrently
// (which would close the channel underneath the send and panic). The
// non-blocking select is microseconds, so holding RLock through it is
// cheap.
func (s *store) notifySubscriber(scope string) {
	s.subsMu.RLock()
	if sub, ok := s.subscribers[scope]; ok {
		select {
		case sub.ch <- struct{}{}:
		default:
		}
	}
	s.subsMu.RUnlock()
}
