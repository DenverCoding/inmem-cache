package scopecache

// approxSizeBytes is a richer estimate than the raw approxItemSize sum: it
// also folds in Go map/slice overhead for b.byID, b.bySeq, and the heat
// buckets. It surfaces as the per-scope approx_scope_mb value in
// observability snapshots. It is NOT used for cap enforcement —
// admission control uses Store.totalBytes (approxItemSize sum +
// scopeBufferOverhead per scope) so the 507 budget matches what
// reserveBytes accounts for. Per-item Go heap overhead (slice/map
// entries) is intentionally outside the cap: charging it would tie
// admission control to Go's internal data-structure layout, while the
// current accounting (approxItemSize + per-scope overhead) is
// layout-independent and matches reserveBytes exactly. Trade-off:
// approx_store_mb under-reports real memory pressure at very high
// scope counts, where Go heap overhead per scope becomes non-trivial
// relative to item bytes.
//
// PRECONDITION: caller holds b.mu.
func (b *scopeBuffer) approxSizeBytesLocked() int64 {
	var total int64
	total += 64
	total += int64(len(b.items)) * 32

	for _, item := range b.items {
		total += approxItemSize(item)
	}

	total += int64(len(b.byID)) * 32
	for k := range b.byID {
		total += int64(len(k))
	}

	total += int64(len(b.bySeq)) * 16
	total += int64(len(b.readHeatBuckets)) * 16

	return total
}

func (b *scopeBuffer) approxSizeBytes() int64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.approxSizeBytesLocked()
}

// scopeStats is the typed snapshot of a single scopeBuffer. It is what
// buf.stats() returns so callers inside the package (e.g. the candidate-
// selection path) can read fields directly, and what API-layer handlers
// flatten into orderedFields for the wire format.
type scopeStats struct {
	ItemCount       int
	LastSeq         uint64
	ApproxScopeMB   MB
	CreatedTS       int64
	LastAccessTS    int64
	ReadCountTotal  uint64
	Last7DReadCount uint64
}

// stats returns a snapshot of this scope's metrics. The caller passes
// `now` so Last7DReadCount reflects the rolling window ending at the
// caller's clock — last7DReadCount the runtime field is only updated
// by recordRead, so a scope that hasn't been read in 7+ days would
// otherwise still report a stale "warm" count to observability
// callers.
func (b *scopeBuffer) stats(now int64) scopeStats {
	b.mu.RLock()
	defer b.mu.RUnlock()

	return scopeStats{
		ItemCount:       len(b.items),
		LastSeq:         b.lastSeq,
		ApproxScopeMB:   MB(b.approxSizeBytesLocked()),
		CreatedTS:       b.createdTS,
		LastAccessTS:    b.lastAccessTS.Load(),
		ReadCountTotal:  b.readCountTotal.Load(),
		Last7DReadCount: b.computeLast7DReadCount(now),
	}
}
