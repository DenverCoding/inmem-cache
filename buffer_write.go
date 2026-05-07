package scopecache

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Single-item write paths on *scopeBuffer:
//
//   - appendItem    — insert a fresh item; rejects on dup id, capacity, or byte cap
//   - upsertByID    — insert-or-replace by id; replace-whole-item on hit
//   - updateByID    — modify payload at an existing id
//   - updateBySeq   — same, addressed by seq
//
// All four take b.mu exclusively, check b.detached first, and route
// their byte-budget reservation through Store.reserveBytes. Shared
// helpers (precomputeRenderBytes, indexBySeqLocked,
// reservePayloadDeltaLocked, replaceItemAtIndexLocked) live in
// buffer_locked.go. insertNewItemLocked at the bottom is the local
// helper that collapses the fresh-insert pipeline shared by
// appendItem and upsertByID's miss-branch.
//
// Every path stamps time.Now().UnixMicro() onto Item.Ts under b.mu
// before storing or replacing — Ts contract lives on the Item type
// in types.go.

func (b *scopeBuffer) appendItem(item Item) (Item, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.detached {
		return Item{}, &ScopeDetachedError{}
	}

	if b.itemCapExceeded(len(b.items) + 1) {
		return Item{}, &ScopeFullError{Count: len(b.items), Cap: b.maxItems}
	}

	if item.ID != "" {
		if _, exists := b.byID[item.ID]; exists {
			return Item{}, errors.New("an item with this 'id' already exists in the scope")
		}
	}

	return b.insertNewItemLocked(item, time.Now().UnixMicro())
}

// upsertByID replaces the payload of the item with this id if it exists,
// or appends a new item with this id if it does not. Both paths run under a
// single scope write-lock so concurrent upserts cannot race between the
// existence check and the mutation. Seq is preserved on replace (stable
// cursor for consumers) and freshly assigned on create (matches /append).
// Returns the final item and whether a new item was created.
func (b *scopeBuffer) upsertByID(item Item) (Item, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.detached {
		return Item{}, false, &ScopeDetachedError{}
	}

	nowUs := time.Now().UnixMicro()

	if existing, exists := b.byID[item.ID]; exists {
		// validateUpsertItem fills item.renderBytes for string
		// payloads; recompute only when the call came in via a
		// Gateway path that bypassed the validator.
		newRender := item.renderBytes
		if newRender == nil {
			newRender = precomputeRenderBytes(item.Payload)
		}
		delta := int64(len(item.Payload)+len(newRender)) - payloadAndRenderBytes(existing)
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
		// /upsert replaces the whole item shape — clear any prior
		// counter cell so it's no longer canonical.
		b.items[i].counter = nil

		updated := b.items[i]
		// b.byID hit above proves byID was already allocated; same for
		// bySeq (every item that lives in byID also lives in bySeq).
		b.bySeq[updated.Seq] = updated
		b.byID[item.ID] = updated
		b.bytes += delta
		b.lastWriteTS = nowUs
		if b.store != nil {
			b.store.bumpLastWriteTS(nowUs)
		}
		return updated, false, nil
	}

	if b.itemCapExceeded(len(b.items) + 1) {
		return Item{}, false, &ScopeFullError{Count: len(b.items), Cap: b.maxItems}
	}

	// Reuse the replace branch's nowUs so create-vs-replace is
	// indistinguishable in Ts to observers.
	inserted, err := b.insertNewItemLocked(item, nowUs)
	if err != nil {
		return Item{}, false, err
	}
	return inserted, true, nil
}

// updateByID mutates the item at (scope, id). Payload is always overwritten;
// ts is refreshed to time.Now().UnixMicro() — every write that touches an
// item refreshes ts to "when did the cache write this content."
//
// preRender is the validator's precomputed renderBytes for the new payload.
// Pass nil from direct Gateway callers that bypass the validator path; the
// helper falls back to precomputeRenderBytes(payload) in that case.
func (b *scopeBuffer) updateByID(id string, payload json.RawMessage, preRender []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.detached {
		return 0, &ScopeDetachedError{}
	}

	existing, ok := b.byID[id]
	if !ok {
		return 0, nil
	}

	// Scope/id are unchanged on /update, so the byte delta reduces to
	// new vs old payload-bytes (payloadAndRenderBytes handles the
	// counter-item case).
	newRender := preRender
	if newRender == nil {
		newRender = precomputeRenderBytes(payload)
	}
	delta, err := b.reservePayloadDeltaLocked(
		payloadAndRenderBytes(existing),
		int64(len(payload)+len(newRender)),
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

// preRender mirrors updateByID: the validator's renderBytes for the new
// payload, or nil to recompute.
func (b *scopeBuffer) updateBySeq(seq uint64, payload json.RawMessage, preRender []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.detached {
		return 0, &ScopeDetachedError{}
	}

	existing, ok := b.bySeq[seq]
	if !ok {
		return 0, nil
	}

	newRender := preRender
	if newRender == nil {
		newRender = precomputeRenderBytes(payload)
	}

	// Per-item cap re-check on the fully-materialised post-update
	// item. The validator's checkItemSize ran on the request body,
	// where ID is empty for seq-based updates — so its measurement
	// undercounts by len(existing.ID). Without this re-check a
	// long-id scope can bypass MaxItemBytes by addressing the item
	// via seq. updateByID needs no re-check: its validator path sees
	// the stored id (the request *is* the address), so request-side
	// and stored-side approxItemSize agree by construction.
	if b.store != nil {
		maxItemBytes := b.store.maxItemBytesFor(existing.Scope)
		candidate := Item{
			Scope:       existing.Scope,
			ID:          existing.ID,
			Payload:     payload,
			renderBytes: newRender,
		}
		if size := approxItemSize(candidate); size > maxItemBytes {
			return 0, fmt.Errorf("%w: the item's approximate size (%d bytes) exceeds the maximum of %d bytes",
				ErrInvalidInput, size, maxItemBytes)
		}
	}

	delta, err := b.reservePayloadDeltaLocked(
		payloadAndRenderBytes(existing),
		int64(len(payload)+len(newRender)),
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

// insertNewItemLocked is the shared fresh-insert pipeline used by
// appendItem and upsertByID's miss-branch. The nine steps must stay
// coherent across both paths:
// ts-stamp → renderBytes precompute → size → store-byte reservation
// → seq assignment → b.items append → b.bySeq sync → b.byID sync
// (when ID != "") → b.bytes update.
//
// PRECONDITIONS — caller responsibilities, not re-checked:
//   - holds b.mu (write lock)
//   - b.detached == false
//   - len(b.items) < b.maxItems (or maxItems == sentinel)
//   - duplicate-ID ruled out (when item.ID != "")
//   - client-supplied Seq/Ts already rejected at the validator
//
// nowUs is caller-supplied so /upsert keeps create- and replace-
// paths on identical Ts (observers cannot infer create-vs-replace
// from timestamp drift). /append computes its own at the call site.
//
// Returns *StoreFullError on cap reservation failure; scope state is
// untouched in that case (no Seq increment, no b.items mutation, no
// b.bytes increment), so the caller returns without rollback.
func (b *scopeBuffer) insertNewItemLocked(item Item, nowUs int64) (Item, error) {
	// Defensive clear of counter — Gateway boundary already strips it,
	// HTTP json-decode never sets it, but internal callers (helpers,
	// direct-construction tests) might. A leaked non-nil counter
	// would make approxItemSize charge counterCellOverhead instead of
	// len(Payload), and reads would materialise from the orphaned
	// cell instead of the new payload bytes.
	item.counter = nil
	item.Ts = nowUs
	// validator's checkItemSize normally fills renderBytes already.
	// Recompute only when a direct Gateway caller bypassed it.
	if item.renderBytes == nil {
		item.renderBytes = precomputeRenderBytes(item.Payload)
	}

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
		b.idKeyBytes += int64(len(item.ID))
	}
	b.bytes += size
	if b.store != nil {
		b.store.totalItems.Add(1)
		b.store.bumpLastWriteTS(nowUs)
	}
	b.lastWriteTS = nowUs

	return item, nil
}
