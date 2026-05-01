package scopecache

// Lock-free read-heat tracking for *ScopeBuffer.
//
// The hot read path (/get, /render, /head, /tail) holds
// b.mu.RLock during the actual data fetch. Adding heat tracking under
// b.mu.Lock would turn that into an exclusive serialise point — block
// profiling pinned ~88% of read-path lock-wait time to that one call
// before the algorithm here moved bucket state to atomics. Now reads
// scale with cores up to RLock cache-line contention.
//
// State lives entirely in atomic fields on ScopeBuffer:
//   - readHeatBuckets: ring buffer indexed by day % ReadHeatWindowDays,
//     each bucket has atomic Day + atomic Count.
//   - lastAccessTS, readCountTotal, last7DReadCount: per-scope atomic
//     counters surfaced via ScopeStats.
//
// Coordination uses CAS on bucket.Day for both expiry and slot-claim;
// Count is a simple atomic add per increment.

// computeLast7DReadCount walks the heat buckets and returns the count
// of reads whose Day is within the rolling 7-day window ending at
// `now`. Used by stats() so observability callers see a correct count
// even when no fresh read has happened to expire stale buckets via
// recordRead. All loads are atomic; the snapshot is eventually-
// consistent (a concurrent recordRead may land between two bucket
// reads here) which is acceptable for the observability use cases
// this drives.
func (b *ScopeBuffer) computeLast7DReadCount(now int64) uint64 {
	day := unixDay(now)
	oldestValidDay := day - ReadHeatWindowDays + 1
	var sum uint64
	for i := range b.readHeatBuckets {
		bucket := &b.readHeatBuckets[i]
		if bucket.Day.Load() >= oldestValidDay {
			sum += bucket.Count.Load()
		}
	}
	return sum
}

// recordRead is the lock-free heat-tracking path called on every hit
// of the read endpoints. It runs without taking b.mu so concurrent
// readers (which already hold b.mu.RLock) do not serialise behind a
// write lock here. The state machine has two phases:
//
//  1. Expiry: walk all 7 buckets and drain any whose Day is older
//     than the rolling 7-day window ending at `now`. CAS on
//     bucket.Day claims the expiry — only the winning goroutine
//     subtracts that bucket's Count from last7DReadCount and resets
//     it. Losers re-read and either find Day cleared (skip) or
//     a freshly-claimed Day (skip).
//
//  2. Increment today's bucket. The ring is indexed by
//     day % ReadHeatWindowDays. After phase 1 the slot for today is
//     either already on `day` (no-op claim) or empty (Day == 0); a
//     CAS on Day claims the slot for `day`. Then atomic adds bump
//     bucket.Count, b.readCountTotal, b.last7DReadCount, and store
//     b.lastAccessTS.
//
// The modulo trick (one slot per `day % 7`) means a slot whose
// Day != currentDay must be at least 7 days old, so phase 1 will
// always have cleared it before phase 2 runs. That invariant lets
// phase 2 treat any non-`day` slot as empty without losing counts.
func (b *ScopeBuffer) recordRead(now int64) {
	day := unixDay(now)
	oldestValidDay := day - ReadHeatWindowDays + 1

	// Phase 1 — expiry.
	for i := range b.readHeatBuckets {
		bucket := &b.readHeatBuckets[i]
		for {
			d := bucket.Day.Load()
			if d == 0 || d >= oldestValidDay {
				break
			}
			// Claim the expiry by CAS'ing Day to 0. Losing the CAS
			// means another goroutine just rolled or expired this
			// slot — re-read and re-evaluate.
			if !bucket.Day.CompareAndSwap(d, 0) {
				continue
			}
			// We won the claim. Drain the count atomically and
			// subtract it from last7DReadCount (saturating at 0).
			n := bucket.Count.Swap(0)
			for {
				cur := b.last7DReadCount.Load()
				next := uint64(0)
				if cur > n {
					next = cur - n
				}
				if b.last7DReadCount.CompareAndSwap(cur, next) {
					break
				}
			}
			break
		}
	}

	// Phase 2 — claim/find today's slot and increment.
	bucketIndex := int(day % ReadHeatWindowDays)
	bucket := &b.readHeatBuckets[bucketIndex]
	for {
		d := bucket.Day.Load()
		if d == day {
			break
		}
		// d is either 0 (empty after Phase 1) or a value that races
		// with a concurrent claim; CAS to today wins exactly once
		// per goroutine race. Losers re-read and re-evaluate.
		if bucket.Day.CompareAndSwap(d, day) {
			// Reset Count to 0 just in case Phase 1 raced with us
			// and we beat it to the slot before it could drain.
			// Safe: any in-flight Adds from this same goroutine
			// happen after this Store.
			bucket.Count.Store(0)
			break
		}
	}

	bucket.Count.Add(1)
	b.readCountTotal.Add(1)
	b.last7DReadCount.Add(1)
	b.lastAccessTS.Store(now)
}
