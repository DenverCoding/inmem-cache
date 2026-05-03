package scopecache

// recordRead bumps the read-bookkeeping atomics on every successful
// hit of the read endpoints (/get, /render, /head, /tail). It runs
// without taking b.mu so concurrent readers (which already hold
// b.mu.RLock) do not serialise behind a write lock here.
//
// The two fields it maintains are intentional primitives:
//   - readCountTotal: monotonic lifetime read count, never expires.
//   - lastAccessTS:   microsecond timestamp of the most recent read.
//
// Any time-windowed aggregation of read-heat (rolling 7-day count,
// hourly histogram, exponential decay, …) is policy, not a primitive,
// and lives in addons that poll readCountTotal deltas off a scheduler.
// The core does not bake a window in — see CLAUDE.md, "Roadmap to v1.0
// and beyond".
func (b *scopeBuffer) recordRead(now int64) {
	b.readCountTotal.Add(1)
	b.lastAccessTS.Store(now)
}
