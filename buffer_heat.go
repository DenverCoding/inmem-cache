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
//     than the rolling 7-day window ending at `now`. The drain is
//     ordered Count.Swap(0) → subtract from last7DReadCount → CAS
//     Day stale→0. The Swap is the exclusion point (atomic, returns
//     non-zero exactly once), so only one goroutine subtracts. The
//     ordering matters: see "drain-ordering invariant" below.
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
//
// Drain-ordering invariant (Phase 1):
//
//	Count.Swap(0) ──► subtract from last7DReadCount ──► CAS(Day, 0)
//
// Doing the Day CAS first would expose a window where Day==0 but
// Count still holds the stale day's reads (also still summed into
// last7DReadCount). A concurrent Phase 2 that observed Day==0 would
// then CAS(0, today) and Count.Store(0), wiping the stale count
// without subtracting from last7DReadCount — leaving the runtime
// counter drifted upward by the stale-day read total. Draining first
// closes that window: if another goroutine sees Day==0, the stale
// Count and the last7DReadCount entry have already been reconciled,
// so its Count.Store(0) and Count.Add(1) operate on a clean slot.
//
// /stats observability is not affected by this race (it always
// recomputes from the buckets via computeLast7DReadCount), but the
// runtime b.last7DReadCount is the canonical "last 7 days of reads"
// counter for any future in-process consumer.
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
			// Drain Count BEFORE CAS'ing Day — see the drain-ordering
			// invariant in the function header. Swap is the exclusion
			// point: exactly one concurrent caller gets a non-zero n
			// for any given stale residue, so the subtract below runs
			// at most once per cohort of stale reads.
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
			// Now expose the cleared slot. A failing CAS means another
			// goroutine already moved Day forward (to 0 via its own
			// Phase 1, or to today via its Phase 2); both outcomes are
			// fine — our drain has already subtracted whatever was
			// stale, so no reconciliation is needed.
			bucket.Day.CompareAndSwap(d, 0)
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
		// Phase 1 guarantees d ∈ {0, day} here: it walks every bucket
		// (including this one) and drives any stale Day to 0 before we
		// reach Phase 2. CAS to today wins exactly once per goroutine
		// race; losers re-read and either see day (break) or 0 again
		// (retry).
		if bucket.Day.CompareAndSwap(d, day) {
			// Count is 0 by the drain-ordering invariant when d was 0.
			// Store(0) is defensive against any future change to that
			// invariant; it cannot wipe live data under the current
			// ordering.
			bucket.Count.Store(0)
			break
		}
	}

	bucket.Count.Add(1)
	b.readCountTotal.Add(1)
	b.last7DReadCount.Add(1)
	b.lastAccessTS.Store(now)
}
