package scopecache

// approxSizeBytes is a richer estimate than the raw approxItemSize sum: it
// also folds in Go map/slice overhead for b.byID and b.bySeq. It surfaces
// as the per-scope approx_scope_mb value in observability snapshots. It
// is NOT used for cap enforcement — admission control uses
// Store.totalBytes (approxItemSize sum + scopeBufferOverhead per scope)
// so the 507 budget matches what reserveBytes accounts for. Per-item Go
// heap overhead (slice/map entries) is intentionally outside the cap:
// charging it would tie admission control to Go's internal data-structure
// layout, while the current accounting (approxItemSize + per-scope
// overhead) is layout-independent and matches reserveBytes exactly.
// Trade-off: approx_store_mb under-reports real memory pressure at very
// high scope counts, where Go heap overhead per scope becomes non-trivial
// relative to item bytes.
//
// O(1) by construction: every term is either a constant, a length on
// an existing field, or a counter (b.bytes, b.idKeyBytes) the write
// paths maintain incrementally. The earlier walk-based version was
// O(items) per /stats call and dominated /stats latency on scopes
// with thousands of items.
//
// Term breakdown:
//   - 64                          : *scopeBuffer struct overhead (constant)
//   - len(b.items) * 32           : Go slice slot overhead per item
//   - b.bytes                     : Σ approxItemSize(item) — admission-control byte sum
//   - len(b.byID) * 32            : map bucket overhead per byID entry
//   - b.idKeyBytes                : Σ len(item.ID) over the byID keys
//   - len(b.bySeq) * 16           : map bucket overhead per bySeq entry
//
// PRECONDITION: caller holds b.mu (or b.mu.RLock — the formula reads
// only mu-protected state).
func (b *scopeBuffer) approxSizeBytesLocked() int64 {
	const structOverhead = int64(64)
	const itemSlotOverhead = int64(32)
	const byIDBucketOverhead = int64(32)
	const bySeqBucketOverhead = int64(16)

	return structOverhead +
		int64(len(b.items))*itemSlotOverhead +
		b.bytes +
		int64(len(b.byID))*byIDBucketOverhead +
		b.idKeyBytes +
		int64(len(b.bySeq))*bySeqBucketOverhead
}

func (b *scopeBuffer) approxSizeBytes() int64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.approxSizeBytesLocked()
}

// scopeStats is the typed snapshot of a single scopeBuffer. It is what
// buf.stats() returns so callers inside the package can read fields
// directly, and what API-layer handlers flatten into orderedFields for
// the wire format.
type scopeStats struct {
	ItemCount      int
	LastSeq        uint64
	ApproxScopeMB  MB
	CreatedTS      int64
	LastWriteTS    int64
	LastAccessTS   int64
	ReadCountTotal uint64
}

// stats returns a snapshot of this scope's metrics. All fields are
// primitives the cache maintains directly (timestamps + monotonic
// counters); time-windowed aggregations are addon territory — see
// recordRead in buffer_heat.go.
func (b *scopeBuffer) stats() scopeStats {
	b.mu.RLock()
	defer b.mu.RUnlock()

	return scopeStats{
		ItemCount:      len(b.items),
		LastSeq:        b.lastSeq,
		ApproxScopeMB:  MB(b.approxSizeBytesLocked()),
		CreatedTS:      b.createdTS,
		LastWriteTS:    b.lastWriteTS,
		LastAccessTS:   b.lastAccessTS.Load(),
		ReadCountTotal: b.readCountTotal.Load(),
	}
}
