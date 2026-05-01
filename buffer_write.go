package scopecache

import (
	"encoding/json"
	"errors"
)

// Single-item write paths on *ScopeBuffer:
//
//   - appendItem    — insert a fresh item; rejects on dup id, capacity, or byte cap
//   - upsertByID    — insert-or-replace by id; replace-whole-item semantics on hit
//   - updateByID    — modify payload (and optional ts) at an existing id
//   - updateBySeq   — same, addressed by seq
//
// All four take b.mu exclusively, check b.detached first, and route
// their byte-budget reservation through Store.reserveBytes. Shared
// helpers — precomputeRenderBytes, indexBySeqLocked,
// reservePayloadDeltaLocked, replaceItemAtIndexLocked — live in
// buffer_locked.go.
//
// /upsert vs /update: /upsert has replace-the-whole-item semantics, so
// ts follows the client's input exactly (send → stored, omit → cleared).
// /update is a partial modify: ts present → overwrite, ts absent →
// preserve. The asymmetry is deliberate.

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
		// /upsert has replace-the-whole-item semantics, so ts follows the
		// client's input exactly: send ts → stored, omit → cleared. That
		// differs from /update (which treats absent ts as "preserve").
		b.items[i].Ts = item.Ts
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

// updateByID mutates the item at (scope, id). Payload is always overwritten.
// Ts follows "absent → preserve, present → overwrite" semantics: a nil ts
// leaves the stored ts alone, a non-nil ts replaces it. This asymmetry with
// /upsert (which blind-overwrites ts) is deliberate — /update is a partial
// modify, /upsert is a full replace.
func (b *ScopeBuffer) updateByID(id string, payload json.RawMessage, ts *int64) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.detached {
		return 0, &ScopeDetachedError{}
	}

	existing, ok := b.byID[id]
	if !ok {
		return 0, nil
	}

	// Only the payload changes on /update; scope/id/ts are unchanged in
	// size, so the byte delta reduces to (new_payload + new_renderBytes)
	// - (old_payload + old_renderBytes). renderBytes is non-nil only for
	// JSON-string payloads (precomputed at write so /render skips a
	// per-hit Unmarshal); approxItemSize counts it, so the cap check
	// must too. A shrink can't fail the cap check, but a grow must
	// reserve first.
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
	b.replaceItemAtIndexLocked(i, payload, ts, newRender, delta)
	return 1, nil
}

func (b *ScopeBuffer) updateBySeq(seq uint64, payload json.RawMessage, ts *int64) (int, error) {
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
	b.replaceItemAtIndexLocked(i, payload, ts, newRender, delta)
	return 1, nil
}
