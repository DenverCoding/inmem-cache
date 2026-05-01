package scopecache

import (
	"encoding/json"
	"strconv"
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

// counterAdd atomically adds `by` to the integer stored at scope+id, or
// creates a fresh counter with starting value `by` if no item exists. Both
// paths run under a single scope write-lock so concurrent increments cannot
// lose updates. The stored payload is a bare JSON integer (e.g. `42`), which
// is what /get, /render, /upsert and /update all see on this scope+id.
//
// Errors:
//   - *ScopeFullError  → create path hit the per-scope item cap
//   - *StoreFullError  → create or payload-grow hit the store byte cap
//   - *CounterPayloadError  → existing payload is not a valid counter value
//   - *CounterOverflowError → result would exceed ±MaxCounterValue
//
// The caller must have already rejected by==0 and by outside ±MaxCounterValue.
func (b *ScopeBuffer) counterAdd(scope, id string, by int64) (int64, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.detached {
		return 0, false, &ScopeDetachedError{}
	}

	if existing, exists := b.byID[id]; exists {
		current, err := parseCounterValue(existing.Payload)
		if err != nil {
			return 0, false, err
		}

		newValue := current + by
		if newValue > MaxCounterValue || newValue < -MaxCounterValue {
			return 0, false, &CounterOverflowError{Current: current, By: by}
		}

		newPayload := json.RawMessage(strconv.FormatInt(newValue, 10))
		delta, err := b.reservePayloadDeltaLocked(len(existing.Payload), len(newPayload))
		if err != nil {
			return 0, false, err
		}

		i, ok := b.indexBySeqLocked(existing.Seq)
		if !ok {
			// Unreachable under b.mu: b.byID confirmed the item exists and items/byID are kept in sync.
			return 0, false, nil
		}
		// /counter_add never updates ts (only the integer payload),
		// so pass nil for ts — the helper preserves the existing ts.
		// renderBytes is always nil for counter payloads (bare
		// integers, not JSON strings), so no renderBytes adjustment
		// is needed and the payload-delta is the full delta.
		b.replaceItemAtIndexLocked(i, newPayload, nil, nil, delta)
		return newValue, false, nil
	}

	if len(b.items) >= b.maxItems {
		return 0, false, &ScopeFullError{Count: len(b.items), Cap: b.maxItems}
	}

	item := Item{
		Scope:   scope,
		ID:      id,
		Payload: json.RawMessage(strconv.FormatInt(by, 10)),
	}
	// Counter payloads are bare integers, never JSON strings — so
	// precomputeRenderBytes returns nil here and approxItemSize is
	// unchanged. The call stays for consistency with other write paths
	// in case a future refactor makes counter payloads JSON-encoded.
	item.renderBytes = precomputeRenderBytes(item.Payload)
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
	b.bytes += size

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
