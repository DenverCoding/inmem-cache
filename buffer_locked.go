// Cross-cutting helpers shared by scopeBuffer's mutation paths. The
// `*Locked` suffix signals: caller MUST hold b.mu. Calling without
// the lock is a race; calling with the lock then re-acquiring is a
// deadlock — these do not lock themselves. precomputeRenderBytes is
// pure and lock-agnostic.
//
// The helpers centralise three concerns that previously drifted
// across parallel call-sites: bytes-accounting, secondary-index
// sync, and GC-zeroing of removed payloads. Forgetting any of them
// leaks state silently.

package scopecache

import (
	"bytes"
	"encoding/json"
	"sort"
)

// precomputeRenderBytes returns the JSON-string-decoded form of payload
// when payload's first non-whitespace byte is `"`, or nil otherwise.
// Called at write-time so /render hits skip the per-call json.Unmarshal
// + []byte cast. Returns nil on a malformed JSON string (defensive — the
// validator already rejects malformed JSON on writes).
func precomputeRenderBytes(payload json.RawMessage) []byte {
	trimmed := bytes.TrimLeft(payload, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '"' {
		return nil
	}
	var s string
	if err := json.Unmarshal(payload, &s); err != nil {
		return nil
	}
	return []byte(s)
}

// reservePayloadDeltaLocked reserves (newSize − oldSize) against the
// store-wide byte budget when delta != 0 and the buffer is store-
// attached, and returns the delta so the caller can update b.bytes
// consistently after a successful mutation.
//
// PRECONDITION: caller holds b.mu.
//
// Returns *StoreFullError when the reservation fails — no store
// state mutated, caller returns the error without rollback.
func (b *scopeBuffer) reservePayloadDeltaLocked(oldSize, newSize int64) (int64, error) {
	delta := newSize - oldSize
	if b.store != nil && delta != 0 {
		ok, current, max := b.store.reserveBytes(delta)
		if !ok {
			return 0, &StoreFullError{StoreBytes: current, AddedBytes: delta, Cap: max}
		}
	}
	return delta, nil
}

// itemCapExceeded reports whether `proposed` items would violate the
// per-scope item-count cap. The unbounded sentinel (b.maxItems == 0,
// set by maxItemsFor for `_events`) disables the check — that
// scope's contract is "byte budget only".
//
// Single-item callers pass len(b.items) + 1; bulk callers pass the
// proposed batch size.
//
// Lock-agnostic: b.maxItems is set once at buffer construction and
// never reassigned. Callers that also read len(b.items) still hold
// b.mu so the (proposed, len) pair is consistent.
func (b *scopeBuffer) itemCapExceeded(proposed int) bool {
	return b.maxItems > 0 && proposed > b.maxItems
}

// payloadAndRenderBytes returns the byte cost approxItemSize charges
// for an item's payload-related fields: counterCellOverhead for
// counter items (where stored Payload is stale-by-construction),
// len(Payload) + len(renderBytes) otherwise. Used by replace paths
// (upsert/update) to compute size deltas correctly when either side
// is a counter item.
func payloadAndRenderBytes(item Item) int64 {
	if item.counter != nil {
		return counterCellOverhead
	}
	return int64(len(item.Payload)) + int64(len(item.renderBytes))
}

// replaceItemAtIndexLocked overwrites payload + ts + renderBytes at
// items[i], syncs the secondary indexes, applies delta to b.bytes,
// and stamps lastWriteTS.
//
// PRECONDITION: caller holds b.mu and i is valid. Bounds-check is
// the caller's job — callers reach i via O(log n) binary-search or
// guaranteed-hit lookups, and re-checking would defeat that.
//
// byID sync is conditional (id="" is legal on /append, so not every
// item has a byID entry); bySeq is unconditional. Ts is always the
// caller-supplied value (every caller stamps fresh under b.mu); the
// "always refresh" rule lives at the call site, not here.
// renderBytes is caller-supplied because the size delta the caller
// passed in already accounts for it.
func (b *scopeBuffer) replaceItemAtIndexLocked(i int, payload json.RawMessage, ts int64, renderBytes []byte, delta int64) {
	b.items[i].Payload = payload
	b.items[i].Ts = ts
	b.items[i].renderBytes = renderBytes
	// /update + /upsert replace the whole item shape: a previously-
	// installed counter cell is no longer canonical. Clear it so
	// subsequent /counter_add slow-path reaches the promote branch
	// (parsing the new payload) instead of an orphaned cell.
	b.items[i].counter = nil
	updated := b.items[i]
	// replaceItemAtIndexLocked is only reachable when the item already
	// existed in this buffer, so bySeq and (when ID != "") byID have
	// already been allocated by the original write that created it.
	b.bySeq[updated.Seq] = updated
	if updated.ID != "" {
		b.byID[updated.ID] = updated
	}
	b.bytes += delta
	b.lastWriteTS = ts
	if b.store != nil {
		b.store.bumpLastWriteTS(ts)
	}
}

// indexBySeqLocked returns the slice position of the item with seq in
// b.items via O(log n) binary search. Returns (0, false) on miss.
// items is always ordered ascending by seq (appendItem assigns
// monotonic seqs, nothing inserts in the middle), which is what makes
// the search valid.
//
// PRECONDITION: caller holds b.mu (read or write).
func (b *scopeBuffer) indexBySeqLocked(seq uint64) (int, bool) {
	i := sort.Search(len(b.items), func(i int) bool {
		return b.items[i].Seq >= seq
	})
	if i == len(b.items) || b.items[i].Seq != seq {
		return 0, false
	}
	return i, true
}
