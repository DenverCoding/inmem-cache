// buffer_stats.go owns per-scope observability primitives: the
// O(1) approxSizeBytes memory estimate (richer than admission
// control's totalBytes — folds in Go map/slice overhead) and the
// scopeStats snapshot returned to /stats and /scopelist.

package scopecache

// approxSizeBytesLocked is a richer per-scope memory estimate than
// the raw approxItemSize sum: it also folds in Go map/slice overhead
// for b.byID and b.bySeq. Surfaces as approx_scope_mb in observability
// snapshots.
//
// NOT used for cap enforcement — admission control uses
// Store.totalBytes (approxItemSize sum + scopeBufferOverhead per
// scope) so the 507 budget matches what reserveBytes accounts for.
// Per-item Go heap overhead is intentionally outside the cap:
// charging it would tie admission control to Go's internal data-
// structure layout. Trade-off: approx_store_mb under-reports real
// memory pressure at very high scope counts.
//
// O(1) by construction: every term is a constant, a slice/map length,
// or an incrementally-maintained counter (b.bytes, b.idKeyBytes).
//
// Term breakdown:
//   - 64                : *scopeBuffer struct overhead (constant)
//   - len(b.items) * 32 : Go slice slot overhead per item
//   - b.bytes           : Σ approxItemSize(item)
//   - len(b.byID) * 32  : map bucket overhead per byID entry
//   - b.idKeyBytes      : Σ len(item.ID) over the byID keys
//   - len(b.bySeq) * 16 : map bucket overhead per bySeq entry
//
// PRECONDITION: caller holds b.mu (read or write).
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
// counters); time-windowed aggregations are addon territory.
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
