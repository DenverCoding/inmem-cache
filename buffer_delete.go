package scopecache

import "sort"

// Delete paths on *scopeBuffer:
//
//   - deleteByID     — single-item delete by id
//   - deleteBySeq    — single-item delete by seq
//   - deleteUpToSeq  — drain the prefix [seq=1 .. seq=maxSeq] in one shot
//
// All three take b.mu exclusively, check b.detached first, and route
// byte releases through the store-wide totalBytes counter. The
// low-level helper deleteIndexLocked centralises the GC-zeroing,
// secondary-index sync, and counter update so the three callers cannot
// drift.

// deleteIndexLocked removes the item at items[i] in O(n) tail-shift,
// zeroes the now-duplicate last slot (so the GC can reclaim the
// removed Item's payload bytes — without this the backing array
// keeps a reference and the payload leaks), removes the item from
// bySeq + byID (the latter only if the id is non-empty), and
// releases the item's bytes from both b.bytes and the store-wide
// totalBytes counter when store-attached.
//
// PRECONDITION: caller holds b.mu and i is a valid index into b.items.
//
// Centralises three invariants that previously lived parallel across
// deleteByID and deleteBySeq:
//
//  1. GC-zeroing of the duplicate last slot before truncate. Forgetting
//     this leaks payloads — silent until observability metrics drift
//     under load.
//  2. Lockstep b.bytes / s.totalBytes update. Forgetting one desyncs
//     the per-scope and store-wide counters and corrupts the
//     observability output.
//  3. Conditional byID delete. Forgetting the `if removed.ID != ""`
//     guard would `delete(map, "")` which is a no-op but signals the
//     reader didn't think about empty-id items.
func (b *scopeBuffer) deleteIndexLocked(i int) {
	removed := b.items[i]
	removedSize := approxItemSize(removed)

	// Tail-shift then zero the now-duplicate last slot before
	// shrinking. Without the zero the backing array keeps a
	// reference to the removed Item (and its payload bytes) and
	// prevents GC.
	copy(b.items[i:], b.items[i+1:])
	b.items[len(b.items)-1] = Item{}
	b.items = b.items[:len(b.items)-1]

	delete(b.bySeq, removed.Seq)
	if removed.ID != "" {
		delete(b.byID, removed.ID)
		b.idKeyBytes -= int64(len(removed.ID))
	}

	b.bytes -= removedSize
	now := nowUnixMicro()
	if b.store != nil {
		b.store.totalBytes.Add(-removedSize)
		b.store.totalItems.Add(-1)
		b.store.bumpLastWriteTS(now)
	}
	b.lastWriteTS = now
	b.resetIfEmptyLocked()
}

// resetIfEmptyLocked drops the high-watermark backing storage when a
// scope has just been drained to zero items. The reslice in
// deleteIndexLocked (b.items = b.items[:len-1]) reduces len but not
// cap, so a scope that briefly held N items keeps the N-element
// backing array alive after every item has been deleted. The same
// goes for b.bySeq and b.byID: Go maps don't shrink their bucket
// arrays on delete().
//
// In the write-buffer pattern (drain-and-refill on a long-lived
// scope) the wasted capacity sits idle between bursts. At ~104 bytes
// per Item slot plus map-bucket overhead, a 1k-item scope that
// drains to empty leaks ~100 KiB until appendItem grows it again.
//
// nil-ing is safe because appendItem (buffer_write.go) lazy-inits
// both maps on first write after a reset, and append() on a nil
// slice grows naturally. b.lastSeq is intentionally NOT reset —
// the seq cursor must remain monotonic across drain/refill cycles
// so downstream consumers tracking the cursor cannot see a regression
// and a fresh /append after the reset still produces a strictly-
// greater seq than anything observers may have remembered.
//
// PRECONDITION: caller holds b.mu and the delete that produced the
// empty state has already updated b.bytes / counters.
func (b *scopeBuffer) resetIfEmptyLocked() {
	if len(b.items) != 0 {
		return
	}
	b.items = nil
	b.bySeq = nil
	b.byID = nil
	// b.idKeyBytes is already zero — every removed item subtracted its
	// id length on delete; an explicit assignment here is belt-and-
	// braces against future delete-paths that forget the subtract.
	b.idKeyBytes = 0
}

func (b *scopeBuffer) deleteByID(id string) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.detached {
		return 0, &ScopeDetachedError{}
	}

	existing, ok := b.byID[id]
	if !ok {
		return 0, nil
	}

	i, ok := b.indexBySeqLocked(existing.Seq)
	if !ok {
		// Unreachable under b.mu: b.byID confirmed the item exists and items/bySeq are kept in sync.
		return 0, nil
	}
	b.deleteIndexLocked(i)
	return 1, nil
}

func (b *scopeBuffer) deleteBySeq(seq uint64) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.detached {
		return 0, &ScopeDetachedError{}
	}

	if _, ok := b.bySeq[seq]; !ok {
		return 0, nil
	}

	i, ok := b.indexBySeqLocked(seq)
	if !ok {
		// Unreachable under b.mu: b.bySeq confirmed the item exists and items/bySeq are kept in sync.
		return 0, nil
	}
	b.deleteIndexLocked(i)
	return 1, nil
}

// deleteUpToSeq removes every item with Seq <= maxSeq. b.items is always
// ordered ascending by Seq (appendItem assigns monotonic seqs and nothing
// removes from the middle), so binary search finds the cut point in O(log n).
// Returns the number of items removed and any *ScopeDetachedError if the
// buffer was orphaned by /delete_scope, /wipe, or /rebuild before the
// caller's mutation could land.
func (b *scopeBuffer) deleteUpToSeq(maxSeq uint64) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.detached {
		return 0, &ScopeDetachedError{}
	}

	idx := sort.Search(len(b.items), func(i int) bool {
		return b.items[i].Seq > maxSeq
	})
	if idx == 0 {
		return 0, nil
	}

	var freedBytes int64
	var freedIDKeyBytes int64
	for i := 0; i < idx; i++ {
		removed := b.items[i]
		freedBytes += approxItemSize(removed)
		delete(b.bySeq, removed.Seq)
		if removed.ID != "" {
			delete(b.byID, removed.ID)
			freedIDKeyBytes += int64(len(removed.ID))
		}
	}
	// Copy the kept suffix into a fresh backing array so the old one —
	// which still holds the removed payloads in its prefix — becomes
	// GC-eligible. A bare reslice (b.items[idx:]) would pin the full
	// original array behind a small remainder; this matters for the
	// write-buffer pattern where repeated drain-from-front otherwise
	// retains memory proportional to the historical high-watermark.
	rest := make([]Item, len(b.items)-idx)
	copy(rest, b.items[idx:])
	b.items = rest

	b.bytes -= freedBytes
	b.idKeyBytes -= freedIDKeyBytes
	now := nowUnixMicro()
	if b.store != nil {
		b.store.totalBytes.Add(-freedBytes)
		b.store.totalItems.Add(-int64(idx))
		b.store.bumpLastWriteTS(now)
	}
	b.lastWriteTS = now
	// `rest` already replaced b.items with a fresh backing array, so
	// the items-slice high-watermark is freed regardless of len. The
	// reset still matters for the maps: their bucket arrays do not
	// shrink on delete(), and resetIfEmptyLocked nil's them when this
	// drain emptied the scope.
	b.resetIfEmptyLocked()
	return idx, nil
}
