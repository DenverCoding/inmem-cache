package scopecache

import (
	"encoding/json"
	"errors"
	"time"
)

// Single-item write paths on *scopeBuffer:
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
// buffer_locked.go (cross-file reuse). insertNewItemLocked at the
// bottom of this file is the local helper that collapses the
// fresh-insert pipeline shared by appendItem and upsertByID's
// miss-branch.
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

func (b *scopeBuffer) appendItem(item Item) (Item, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.detached {
		return Item{}, &ScopeDetachedError{}
	}

	// maxItems == 0 is the unboundedScopeMaxItems sentinel — only the
	// reserved `_log` scope is created with it; every user scope gets a
	// positive cap installed at create time. See buffer.go's maxItems
	// comment for the rationale.
	if b.maxItems > 0 && len(b.items) >= b.maxItems {
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
		// Delta covers both Payload and renderBytes: approxItemSize
		// counts both, and the cap check must too. renderBytes is
		// non-nil only for JSON-string payloads. payloadAndRenderBytes
		// handles the counter-item case where `existing.Payload` is
		// stale-by-construction and the real cost lives in the cell —
		// a naive `len(existing.Payload)` would under-count.
		newRender := precomputeRenderBytes(item.Payload)
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
		// Clear any inherited counter cell — /upsert is "replace whole
		// item with the supplied shape", so the cell from a prior
		// /counter_add is no longer canonical. See replaceItemAtIndexLocked
		// in buffer_locked.go for the same reasoning on the /update path.
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

	if len(b.items) >= b.maxItems {
		return Item{}, false, &ScopeFullError{Count: len(b.items), Cap: b.maxItems}
	}

	// nowUs is shared with the replace branch above so create-vs-replace
	// is indistinguishable in Ts to observers.
	inserted, err := b.insertNewItemLocked(item, nowUs)
	if err != nil {
		return Item{}, false, err
	}
	return inserted, true, nil
}

// updateByID mutates the item at (scope, id). Payload is always overwritten;
// ts is refreshed to time.Now().UnixMicro() — every write that touches an
// item refreshes ts to "when did the cache write this content."
func (b *scopeBuffer) updateByID(id string, payload json.RawMessage) (int, error) {
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
	// size, so the byte delta reduces to new payload-bytes minus old
	// payload-bytes. payloadAndRenderBytes handles the counter-item
	// case (cell overhead vs len(Payload)+len(renderBytes)); see
	// buffer_locked.go for the rationale.
	newRender := precomputeRenderBytes(payload)
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

func (b *scopeBuffer) updateBySeq(seq uint64, payload json.RawMessage) (int, error) {
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

// insertNewItemLocked is the shared "append a fresh item to this scope"
// pipeline used by appendItem and upsertByID's miss-branch. It owns the
// nine-step sequence that must stay coherent across both paths:
// ts-stamp → renderBytes precompute → size → store-byte reservation →
// seq assignment → b.items append → b.bySeq sync → b.byID sync (when ID
// non-empty) → b.bytes update.
//
// PRECONDITIONS — caller responsibilities, not re-checked here:
//   - holds b.mu (write lock)
//   - has confirmed b.detached == false
//   - has confirmed len(b.items) < b.maxItems
//   - has ruled out duplicate-ID (when item.ID != "")
//   - has rejected any client-supplied Seq/Ts at the validator layer
//
// nowUs is caller-supplied so /upsert can keep create- and replace-
// paths on identical Ts, which prevents observers from inferring
// create-vs-replace from a timestamp drift. /append computes its own
// time.Now().UnixMicro() at call site since it has no replace branch
// to align with.
//
// Why renderBytes precompute happens here, not at the validator: the
// validator runs on a fresh decoded request body and renderBytes is a
// cache-internal field; computing it here means the size we reserve
// matches the size we store. checkItemSize does its own
// precomputeRenderBytes to enforce the per-item cap honestly — see the
// header comment on that function.
//
// Returns *StoreFullError on cap reservation failure; in that case the
// scope state is untouched (no Seq increment, no b.items mutation, no
// b.bytes increment), so the caller can return the error without
// rolling anything back.
func (b *scopeBuffer) insertNewItemLocked(item Item, nowUs int64) (Item, error) {
	item.Ts = nowUs
	item.renderBytes = precomputeRenderBytes(item.Payload)

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
