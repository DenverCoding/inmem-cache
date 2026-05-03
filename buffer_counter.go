package scopecache

import (
	"encoding/json"
	"strconv"
	"time"
)

// /counter_add is the cache's one and only payload-aware mutation:
// every other write path treats the payload as opaque bytes, but
// counterAdd reads it as a JSON integer and rewrites it with the
// updated value. The semantics live entirely in this file because
// they are conceptually distinct from the other write paths — a
// future contributor extending counter behaviour shouldn't have to
// scroll past appendItem/upsertByID/updateByID to find the relevant
// code, and the integer-payload contract (rejected non-integer,
// ±MaxCounterValue range) belongs co-located with its enforcement.
//
// Lock discipline:
//
//   1. **Fast path — RLock + atomic.** When the target item already
//      has a counterCell installed, increment runs under b.mu.RLock:
//      a CAS loop on cell.value plus a CAS-max on cell.ts. No
//      exclusive lock, no items-slice mutation, no byte accounting.
//      Concurrent fast-path increments on the same scope run truly
//      in parallel; only real mutations (append/upsert/delete on the
//      same scope) serialise against them.
//
//   2. **Slow path — Lock.** When the target item does not exist
//      (create) or exists without a cell (promote), counterAdd takes
//      b.mu.Lock to install the cell and adjust byte accounting.
//      Slow-path is rare: every counter incurs it exactly once
//      (creation), or twice (creation + first /upsert + first
//      /counter_add after the upsert). Subsequent increments are
//      always fast-path.
//
// lastWriteTS semantics:
//
//   - cell.ts (the per-counter atomic timestamp): bumped on every
//     successful increment via CAS-max. Surfaced as item.Ts at read
//     time by materialiseCounter.
//   - b.lastWriteTS (per-scope): NOT bumped by any counter_add path —
//     create, promote, or increment. Counter activity is intentionally
//     invisible to the per-scope freshness signal so view-counter-
//     style read-driven workloads do not pollute it.
//   - s.lastWriteTS (store-wide): NOT bumped by counter_add either,
//     for the same reason. Scope creation (which may happen
//     incidentally when counter_add hits a brand-new scope) DOES
//     bump via the getOrCreateScope path; that bump is structural
//     (scope_count grew on /stats) and is not a counter_add concern.
//
// Consumers who genuinely need "did this counter just change?" read
// cell.ts via /get?id=X and observe item.Ts directly — that is the
// granularity counters are observable at, by design.

// counterAdd atomically adds `by` to the integer stored at scope+id, or
// creates a fresh counter with starting value `by` if no item exists.
// The fast path (RLock + atomic) handles the increment case — by far the
// dominant call shape — without taking the scope's exclusive write
// lock; create and promote fall through to the slow path under Lock.
//
// Errors:
//   - *ScopeFullError       → create path hit the per-scope item cap
//   - *StoreFullError       → create or promote hit the store byte cap
//   - *CounterPayloadError  → existing payload is not a valid counter value (promote path)
//   - *CounterOverflowError → result would exceed ±MaxCounterValue
//   - *ScopeDetachedError   → buffer was unlinked between caller's getScope and this call
//
// The caller must have already rejected by==0 and by outside ±MaxCounterValue.
func (b *scopeBuffer) counterAdd(scope, id string, by int64) (int64, bool, error) {
	// Fast path: RLock-only, lock-free atomic add on an existing
	// counter cell. Concurrent fast-path increments on the same scope
	// share the RLock and never serialise on each other. The detached
	// check is belt-and-braces — a detached buffer cannot have its
	// b.byID mutated either, so any cell we observe is safe to
	// increment, but the explicit check matches the slow path's
	// observable error and avoids returning a misleading "success" on a
	// buffer the caller no longer reaches.
	b.mu.RLock()
	if !b.detached {
		if existing, ok := b.byID[id]; ok && existing.counter != nil {
			value, err := atomicCounterAdd(existing.counter, by)
			b.mu.RUnlock()
			if err != nil {
				return 0, false, err
			}
			return value, false, nil
		}
	}
	b.mu.RUnlock()

	// Slow path: exclusive lock for create / promote.
	return b.counterAddSlow(scope, id, by)
}

// atomicCounterAdd applies `by` to cell.value with overflow guard, then
// CAS-maxes cell.ts to time.Now().UnixMicro(). Used by both the fast
// path (under RLock) and the slow path's "found-cell race" branch (the
// case where a concurrent caller installed a cell between this caller's
// RUnlock and Lock). Pure atomic; no lock taken.
//
// The value loop is a CAS retry rather than a bare atomic.AddInt64
// because the overflow check has to read the pre-add value AND
// publish the post-add value indivisibly: a naive AddInt64 followed
// by a range check would briefly expose an out-of-range value to a
// concurrent reader. CAS retry stays at most O(few) iterations under
// the contention this primitive is designed to handle.
func atomicCounterAdd(cell *counterCell, by int64) (int64, error) {
	for {
		cur := cell.value.Load()
		next := cur + by
		if next > MaxCounterValue || next < -MaxCounterValue {
			return 0, &CounterOverflowError{Current: cur, By: by}
		}
		if cell.value.CompareAndSwap(cur, next) {
			nowUs := time.Now().UnixMicro()
			for {
				curTs := cell.ts.Load()
				if nowUs <= curTs {
					return next, nil
				}
				if cell.ts.CompareAndSwap(curTs, nowUs) {
					return next, nil
				}
			}
		}
		// CAS lost — another increment landed first; retry with the
		// fresh value. Bounded by contention pressure (typical: 1
		// retry; pathological: a few).
	}
}

// counterAddSlow handles the cases the fast path cannot: scope-detached
// guard, brand-new counter (insert), and "promote" (an existing item
// without a counter cell — the result of a prior /upsert or /update
// having replaced a counter shape with a regular int payload). All
// three need b.mu.Lock for items-slice / map mutation and byte
// accounting; the cost is amortised across the counter's lifetime
// because subsequent increments hit the fast path.
func (b *scopeBuffer) counterAddSlow(scope, id string, by int64) (int64, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.detached {
		return 0, false, &ScopeDetachedError{}
	}

	if existing, exists := b.byID[id]; exists {
		// Race: the fast path saw counter == nil but a concurrent
		// caller may have promoted the item (or detached + recreated
		// it) between our RUnlock and our Lock. Re-check the cell
		// pointer; if it's now non-nil, run the atomic add and return
		// — exactly what the fast path would have done.
		if existing.counter != nil {
			value, err := atomicCounterAdd(existing.counter, by)
			if err != nil {
				return 0, false, err
			}
			return value, false, nil
		}

		// Promote: the existing item has an integer payload but no
		// cell. Parse the payload, install a cell pre-loaded with
		// (current + by), update byte accounting for the size delta,
		// and return.
		current, err := parseCounterValue(existing.Payload)
		if err != nil {
			return 0, false, err
		}
		newValue := current + by
		if newValue > MaxCounterValue || newValue < -MaxCounterValue {
			return 0, false, &CounterOverflowError{Current: current, By: by}
		}

		i, ok := b.indexBySeqLocked(existing.Seq)
		if !ok {
			// Unreachable under b.mu: b.byID confirmed the item exists
			// and items/byID are kept in sync.
			return 0, false, nil
		}

		// Build the new (counter-shaped) Item view of this slot to
		// derive the byte delta. Scope/ID/Seq are unchanged on
		// promote, so size differs only in the payload-related fields:
		// approxItemSize charges counterCellOverhead for counter
		// items, len(Payload)+len(renderBytes) for regular ones.
		oldSize := approxItemSize(existing)
		promoted := existing
		cell := &counterCell{}
		cell.value.Store(newValue)
		nowUs := time.Now().UnixMicro()
		cell.ts.Store(nowUs)
		promoted.counter = cell
		// Payload and renderBytes are now stale-by-construction:
		// readers materialise from cell.value at the boundary. Clear
		// them so the byte accounting we compute matches the heap
		// state we actually keep.
		promoted.Payload = nil
		promoted.renderBytes = nil
		newSize := approxItemSize(promoted)

		delta := newSize - oldSize
		if b.store != nil && delta != 0 {
			ok, total, max := b.store.reserveBytes(delta)
			if !ok {
				return 0, false, &StoreFullError{StoreBytes: total, AddedBytes: delta, Cap: max}
			}
		}

		// Write the promoted item back into items + indexes. We do
		// NOT bump b.lastWriteTS or s.lastWriteTS — promote is
		// semantically an increment from the caller's perspective, and
		// counter increments do not bump the scope / store freshness
		// signals (see file header). The cell's own ts captures the
		// "when did this counter last change" view directly.
		b.items[i] = promoted
		b.bySeq[promoted.Seq] = promoted
		b.byID[id] = promoted
		b.bytes += delta

		return newValue, false, nil
	}

	// Create: brand-new counter item.
	if len(b.items) >= b.maxItems {
		return 0, false, &ScopeFullError{Count: len(b.items), Cap: b.maxItems}
	}

	cell := &counterCell{}
	cell.value.Store(by)
	nowUs := time.Now().UnixMicro()
	cell.ts.Store(nowUs)

	item := Item{
		Scope:   scope,
		ID:      id,
		Ts:      nowUs,
		counter: cell,
	}
	// approxItemSize sees item.counter != nil and charges
	// counterCellOverhead instead of len(Payload). No
	// precomputeRenderBytes needed — counters never have a renderBytes
	// shortcut (their payload is a bare integer, not a JSON string).
	size := approxItemSize(item)
	if b.store != nil {
		ok, total, max := b.store.reserveBytes(size)
		if !ok {
			return 0, false, &StoreFullError{StoreBytes: total, AddedBytes: size, Cap: max}
		}
	}

	b.lastSeq++
	item.Seq = b.lastSeq
	b.items = append(b.items, item)
	if b.bySeq == nil {
		b.bySeq = make(map[uint64]Item)
	}
	b.bySeq[item.Seq] = item
	if b.byID == nil {
		b.byID = make(map[string]Item)
	}
	b.byID[id] = item
	b.idKeyBytes += int64(len(id))
	b.bytes += size
	if b.store != nil {
		b.store.totalItems.Add(1)
		// NOT bumping s.lastWriteTS here: counter creation is a
		// structural change (total_items grew) but the rule "counter
		// activity is silent vs the freshness signal" applies
		// uniformly across create/promote/increment so consumers can
		// reason about counter ops as a single category. Operators who
		// need to size the cache should poll /stats periodically
		// regardless. See file header.
	}
	// b.lastWriteTS likewise unchanged.

	return by, true, nil
}

// parseCounterValue decodes a payload as a JSON integer within ±MaxCounterValue.
// Anything else — a non-number, a float, a number outside the range — is a
// CounterPayloadError (409 Conflict) because the counter machinery cannot
// safely operate on it.
func parseCounterValue(payload json.RawMessage) (int64, error) {
	var num json.Number
	if err := json.Unmarshal(payload, &num); err != nil {
		return 0, &CounterPayloadError{Reason: "the existing item's payload is not a JSON number"}
	}
	v, err := num.Int64()
	if err != nil {
		return 0, &CounterPayloadError{Reason: "the existing item's payload is not an integer"}
	}
	if v > MaxCounterValue || v < -MaxCounterValue {
		return 0, &CounterPayloadError{Reason: "the existing counter value is outside the allowed range of ±(2^53-1)"}
	}
	return v, nil
}

// renderCounterPayload formats a counter cell's current value as the
// bare-integer JSON bytes /get, /head, /tail and /render expect to find
// in Item.Payload. Used by materialiseCounter at the read boundary —
// see buffer_read.go for the call sites and rationale.
func renderCounterPayload(cell *counterCell) json.RawMessage {
	return json.RawMessage(strconv.AppendInt(nil, cell.value.Load(), 10))
}
