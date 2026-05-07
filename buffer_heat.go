package scopecache

// recordRead bumps the read-bookkeeping atomics on every successful
// hit of /get, /render, /head, /tail. Atomic so concurrent readers
// (already holding b.mu.RLock) do not serialise behind a write lock.
//
// readCountTotal is a monotonic lifetime count; lastAccessTS is the
// microsecond timestamp of the most recent read. Time-windowed
// aggregations (rolling counts, decay, histograms) are addon policy,
// not core primitives.
func (b *scopeBuffer) recordRead(now int64) {
	b.readCountTotal.Add(1)
	b.lastAccessTS.Store(now)
}
