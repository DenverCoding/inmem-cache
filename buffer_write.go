package scopecache

import (
	"encoding/json"
	"errors"
	"time"
)

// Single-item write paths on *ScopeBuffer:
//
//   - appendItem    — insert a fresh item; rejects on dup id, capacity, or byte cap
//   - upsertByID    — insert-or-replace by id; replace-whole-item semantics on hit
//   - updateByID    — modify payload at an existing id
//   - updateBySeq   — same, addressed by seq
//
// All four take b.mu exclusively, check b.detached first, and route
// their byte-budget reservation through Store.reserveBytes. Shared
// helpers — precomputeRenderBytes, indexBySeqLocked,
// reservePayloadDeltaLocked, replaceItemAtIndexLocked — live in
// buffer_locked.go.
//
// Ts is cache-owned: every path that mutates an item stamps
// time.Now().UnixMicro() onto Item.Ts under b.mu before storing or
// replacing. Clients cannot supply ts (rejected at the validator);
// the field is observability only — "when did the cache last write
// this item" — so refresh-on-every-write is the simplest honest model.
// Microsecond granularity (not milliseconds): two writes inside the
// same millisecond are still distinguishable, which matters for ordered
// /inbox draining and for "last activity" stamping on counters under
// burst load.

func (b *ScopeBuffer) appendItem(item Item) (Item, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.detached {
		return Item{}, &ScopeDetachedError{}
	}

	if len(b.items) >= b.maxItems {
		return Item{}, &ScopeFullError{Count: len(b.items), Cap: b.maxItems}
	}

	if item.ID != "" {
		if _, exists := b.byID[item.ID]; exists {
			return Item{}, errors.New("an item with this 'id' already exists in the scope")
		}
	}

	// Stamp the cache-owned ts before sizing/storing. Validator already
	// rejected any client-supplied ts; this is the single source of truth.
	item.Ts = time.Now().UnixMicro()

	// Precompute the render-bytes shortcut before sizing — approxItemSize
	// counts renderBytes against the cap, so the reservation must reflect
	// the post-precompute Item.
	item.renderBytes = precomputeRenderBytes(item.Payload)

	// Reserve store-level bytes before mutating scope state: a failed
	// reservation leaves the scope untouched, same as a failed dup-id check.
	size := approxItemSize(item)
	if b.store != nil {
		ok, current, max := b.store.reserveBytes(size)
		if !ok {
			return Item{}, &StoreFullError{StoreBytes: current, AddedBytes: size, Cap: max}
		}
	}

	b.lastSeq++
	item.Seq = b.lastSeq

	b.items = append(b.items, item)
	if b.bySeq == nil {
		b.bySeq = make(map[uint64]Item)
	}
	b.bySeq[item.Seq] = item
	if item.ID != "" {
		if b.byID == nil {
			b.byID = make(map[string]Item)
		}
		b.byID[item.ID] = item
	}
	b.bytes += size

	return item, nil
}

// upsertByID replaces the payload of the item with this id if it exists,
// or appends a new item with this id if it does not. Both paths run under a
// single scope write-lock so concurrent upserts cannot race between the
// existence check and the mutation. Seq is preserved on replace (stable
// cursor for consumers) and freshly assigned on create (matches /append).
// Returns the final item and whether a new item was created.
func (b *ScopeBuffer) upsertByID(item Item) (Item, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.detached {
		return Item{}, false, &ScopeDetachedError{}
	}

	nowUs := time.Now().UnixMicro()

	if existing, exists := b.byID[item.ID]; exists {
		// Delta covers both Payload and renderBytes: approxItemSize
		// counts both, and the cap check must too. renderBytes is
		// non-nil only for JSON-string payloads.
		newRender := precomputeRenderBytes(item.Payload)
		delta := int64(len(item.Payload)+len(newRender)) -
			int64(len(existing.Payload)+len(existing.renderBytes))
		if b.store != nil && delta != 0 {
			ok, current, max := b.store.reserveBytes(delta)
			if !ok {
				return Item{}, false, &StoreFullError{StoreBytes: current, AddedBytes: delta, Cap: max}
			}
		}

		i, ok := b.indexBySeqLocked(existing.Seq)
		if !ok {
			// Unreachable under b.mu: b.byID confirmed the item exists and items/byID are kept in sync.
			return Item{}, false, nil
		}
		b.items[i].Payload = item.Payload
		// /upsert is whole-item replacement: refresh ts to "now" so the
		// stored ts always reflects when the current content arrived.
		b.items[i].Ts = nowUs
		b.items[i].renderBytes = newRender

		updated := b.items[i]
		// b.byID hit above proves byID was already allocated; same for
		// bySeq (every item that lives in byID also lives in bySeq).
		b.bySeq[updated.Seq] = updated
		b.byID[item.ID] = updated
		b.bytes += delta
		return updated, false, nil
	}

	if len(b.items) >= b.maxItems {
		return Item{}, false, &ScopeFullError{Count: len(b.items), Cap: b.maxItems}
	}

	// Stamp ts on the create path. Same value as the replace path above
	// so observers cannot distinguish create-vs-replace by timestamp drift.
	item.Ts = nowUs

	// Precompute renderBytes before sizing (approxItemSize counts it).
	item.renderBytes = precomputeRenderBytes(item.Payload)

	size := approxItemSize(item)
	if b.store != nil {
		ok, current, max := b.store.reserveBytes(size)
		if !ok {
			return Item{}, false, &StoreFullError{StoreBytes: current, AddedBytes: size, Cap: max}
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
	b.byID[item.ID] = item
	b.bytes += size

	return item, true, nil
}

// updateByID mutates the item at (scope, id). Payload is always overwritten;
// ts is refreshed to time.Now().UnixMicro() — every write that touches an
// item refreshes ts to "when did the cache write this content."
func (b *ScopeBuffer) updateByID(id string, payload json.RawMessage) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.detached {
		return 0, &ScopeDetachedError{}
	}

	existing, ok := b.byID[id]
	if !ok {
		return 0, nil
	}

	// Only the payload changes on /update; scope/id are unchanged in
	// size, so the byte delta reduces to (new_payload + new_renderBytes)
	// - (old_payload + old_renderBytes). renderBytes is non-nil only for
	// JSON-string payloads (precomputed at write so /render skips a
	// per-hit Unmarshal); approxItemSize counts it, so the cap check
	// must too. A shrink can't fail the cap check, but a grow must
	// reserve first. Ts is a fixed-width int64, no delta contribution.
	newRender := precomputeRenderBytes(payload)
	delta, err := b.reservePayloadDeltaLocked(
		len(existing.Payload)+len(existing.renderBytes),
		len(payload)+len(newRender),
	)
	if err != nil {
		return 0, err
	}

	i, ok := b.indexBySeqLocked(existing.Seq)
	if !ok {
		// Unreachable under b.mu: b.byID confirmed the item exists and items/byID are kept in sync.
		return 0, nil
	}
	b.replaceItemAtIndexLocked(i, payload, time.Now().UnixMicro(), newRender, delta)
	return 1, nil
}

func (b *ScopeBuffer) updateBySeq(seq uint64, payload json.RawMessage) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.detached {
		return 0, &ScopeDetachedError{}
	}

	existing, ok := b.bySeq[seq]
	if !ok {
		return 0, nil
	}

	// See updateByID for the renderBytes-aware delta rationale.
	newRender := precomputeRenderBytes(payload)
	delta, err := b.reservePayloadDeltaLocked(
		len(existing.Payload)+len(existing.renderBytes),
		len(payload)+len(newRender),
	)
	if err != nil {
		return 0, err
	}

	i, ok := b.indexBySeqLocked(seq)
	if !ok {
		// Unreachable under b.mu: b.bySeq confirmed the item exists and items/bySeq are kept in sync.
		return 0, nil
	}
	b.replaceItemAtIndexLocked(i, payload, time.Now().UnixMicro(), newRender, delta)
	return 1, nil
}
