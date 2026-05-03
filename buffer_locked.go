package scopecache

import (
	"bytes"
	"encoding/json"
	"sort"
)

// Cross-cutting helpers shared by scopeBuffer's mutation paths. The
// `*Locked` suffix on three of them signals: caller MUST hold b.mu.
// Calling them without the lock is a race; calling them WITH the lock
// then re-acquiring is a deadlock — they do not lock themselves. The
// non-Locked helper (precomputeRenderBytes) is pure and lock-agnostic.
//
// These helpers were extracted from the parallel call-sites in
// updateByID, updateBySeq, counterAdd, deleteByID, deleteBySeq and
// upsertByID to centralise three concerns that previously drifted:
// bytes-accounting, secondary-index sync, and GC-zeroing of removed
// payloads. Forgetting any of those leaks state silently, hence the
// dedicated low-level layer.

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

// reservePayloadDeltaLocked computes the byte delta between an old and a
// new payload (newSize − oldSize) and, if the delta is non-zero AND the
// buffer is store-attached, reserves the delta against the store-wide
// byte budget via reserveBytes. Returns the delta so the caller can
// update b.bytes consistently after a successful mutation.
//
// PRECONDITION: caller holds b.mu.
//
// Returns *StoreFullError when the cap reservation fails. No store
// state is mutated in that case — the caller can return the error
// without rolling anything back. Centralises the (b.store != nil &&
// delta != 0) guard: forgetting either condition produces a nil-
// pointer crash on orphan buffers (used in tests) or a CAS for a
// zero delta (cheap but pointless).
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

// payloadAndRenderBytes returns the byte cost approxItemSize attributes
// to an item's payload-related fields. For a counter item that's the
// fixed counterCellOverhead (cell heap + worst-case int64 string); for
// a regular item it's len(Payload) + len(renderBytes). Used by the
// replace paths (upsert/update) to compute the size delta between the
// old shape and the new shape correctly when either side is a counter
// item — a naive `len(payload)` comparison would under-count counter
// items because their stored Payload is stale-by-construction (see
// Item.counter's comment in types.go).
func payloadAndRenderBytes(item Item) int64 {
	if item.counter != nil {
		return counterCellOverhead
	}
	return int64(len(item.Payload)) + int64(len(item.renderBytes))
}

// replaceItemAtIndexLocked overwrites payload and ts at items[i],
// installs the caller-precomputed renderBytes, syncs the secondary
// indexes, and applies the caller's delta to b.bytes.
//
// PRECONDITION: caller holds b.mu and i is a valid index into
// b.items. Bounds-check is the caller's responsibility — the helper
// does not validate i because callers reach it via O(log n)
// binary-search or guaranteed-hit lookups, and re-checking would
// defeat that.
//
// The byID sync is conditional because /append accepts items without
// an id (id="" is legal), so not every item has a byID entry to keep
// in sync. bySeq is unconditional because every item has a seq.
//
// Ts is always overwritten by the caller-supplied value: every
// caller (updateByID, updateBySeq, counterAdd) computes a fresh
// time.Now().UnixMicro() under b.mu and passes it in. The "always
// refresh" rule is enforced at the call site, not here — this helper
// is a mechanical write of the values the caller has already decided
// on.
//
// The caller passes renderBytes (rather than computing it inside the
// helper) because approxItemSize counts renderBytes too — so the
// caller must precompute it to derive `delta`, then pass both. This
// keeps the cap accounting honest on string-payload updates whose
// decoded form changes length.
func (b *scopeBuffer) replaceItemAtIndexLocked(i int, payload json.RawMessage, ts int64, renderBytes []byte, delta int64) {
	b.items[i].Payload = payload
	b.items[i].Ts = ts
	b.items[i].renderBytes = renderBytes
	// /update and /upsert replace the whole item shape, so any
	// previously-installed counter cell is no longer canonical: future
	// reads must see the new Payload bytes, not a stale cell value.
	// Clear the pointer so subsequent /counter_add slow-path reaches
	// the promote branch (parsing the freshly-installed payload) rather
	// than the orphaned cell.
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

// indexBySeqLocked returns the position of the item with the given seq
// inside b.items. Returns (0, false) when no such item exists. items is
// always ordered ascending by seq (appendItem assigns monotonic seqs and
// nothing inserts in the middle), so this is O(log n) — the canonical
// way to translate a byID/bySeq map hit into a slice index for in-place
// mutation. Caller must hold b.mu (read or write — both are fine,
// callers have a write lock in practice because every user is about to
// mutate b.items[i]).
func (b *scopeBuffer) indexBySeqLocked(seq uint64) (int, bool) {
	i := sort.Search(len(b.items), func(i int) bool {
		return b.items[i].Seq >= seq
	})
	if i == len(b.items) || b.items[i].Seq != seq {
		return 0, false
	}
	return i, true
}
