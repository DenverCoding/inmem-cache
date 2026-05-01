package scopecache

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestStress_MixedOps hammers the store from many goroutines with a realistic
// mix of operations (reads, appends, upserts, counter_add, updates, deletes,
// trims, delete_scope, rebuilds) for a fixed duration. After the storm it
// verifies the invariants the rest of the code relies on:
//
//   - s.totalBytes (atomic counter) == sum of buf.bytes across all scopes
//   - buf.bytes == sum of approxItemSize(item) within the scope
//   - len(buf.items) == len(buf.bySeq)
//   - buf.items is strictly seq-ordered
//   - every buf.byID[id] has a matching entry in buf.bySeq
//
// A broken invariant after concurrent load almost always points to a missed
// lock, a missed counter delta, or an index that drifted from items. This
// test is the integration-level counterpart to the unit-tests: it does not
// care about any single operation, only that the aggregate state is coherent.
//
// Run with -race to also catch data races. -short mode shortens the duration
// to keep go test ./... fast.
func TestStress_MixedOps(t *testing.T) {
	const (
		workers   = 16
		numScopes = 8
	)
	duration := 3 * time.Second
	if testing.Short() {
		duration = 500 * time.Millisecond
	}

	s := NewStore(Config{ScopeMaxItems: 100_000, MaxStoreBytes: 500 << 20, MaxItemBytes: 1 << 20})

	scopeNames := make([]string, numScopes)
	for i := range scopeNames {
		scopeNames[i] = "stress_" + strconv.Itoa(i)
		buf, _ := s.getOrCreateScope(scopeNames[i])
		for j := 0; j < 20; j++ {
			_, _ = buf.appendItem(Item{
				Scope:   scopeNames[i],
				ID:      "seed_" + strconv.Itoa(j),
				Payload: json.RawMessage(strconv.Itoa(j)),
			})
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	var (
		wg       sync.WaitGroup
		reads    atomic.Uint64
		appends  atomic.Uint64
		upserts  atomic.Uint64
		counters atomic.Uint64
		updates  atomic.Uint64
		deletes  atomic.Uint64
		trims    atomic.Uint64
		dscopes  atomic.Uint64
		rebuilds atomic.Uint64
	)

	worker := func(wid int) {
		defer wg.Done()
		rng := rand.New(rand.NewSource(int64(wid)*997 + 1))
		localAppend := 0

		pickScope := func() string { return scopeNames[rng.Intn(len(scopeNames))] }

		for ctx.Err() == nil {
			kind := rng.Intn(1000)
			scope := pickScope()

			switch {
			case kind < 450:
				// 45% reads — getByID / tailOffset / sinceSeq
				buf, ok := s.getScope(scope)
				if !ok {
					continue
				}
				switch rng.Intn(3) {
				case 0:
					_, _ = buf.getByID("seed_" + strconv.Itoa(rng.Intn(20)))
				case 1:
					_, _ = buf.tailOffset(10, 0)
				case 2:
					_, _ = buf.sinceSeq(0, 10)
				}
				reads.Add(1)

			case kind < 700:
				// 25% appends
				buf, _ := s.getOrCreateScope(scope)
				_, _ = buf.appendItem(Item{
					Scope:   scope,
					ID:      fmt.Sprintf("w%d_a%d", wid, localAppend),
					Payload: json.RawMessage(`{"x":42,"pad":"abcdefghij"}`),
				})
				localAppend++
				appends.Add(1)

			case kind < 800:
				// 10% upserts — limited id space so we exercise replace paths
				buf, _ := s.getOrCreateScope(scope)
				_, _, _ = buf.upsertByID(Item{
					Scope:   scope,
					ID:      "upsert_" + strconv.Itoa(rng.Intn(30)),
					Payload: json.RawMessage(`{"v":` + strconv.Itoa(rng.Intn(1000)) + `}`),
				})
				upserts.Add(1)

			case kind < 880:
				// 8% counter_add — a few hot counters per scope
				buf, _ := s.getOrCreateScope(scope)
				_, _, _ = buf.counterAdd(scope, "ctr_"+strconv.Itoa(rng.Intn(4)), int64(rng.Intn(20)-10))
				if rng.Intn(20) == 0 {
					// occasionally bump by a non-zero delta we know is non-zero
					// even if rng hit zero above (zero is rejected by counterAdd)
					_, _, _ = buf.counterAdd(scope, "ctr_nonzero", 1)
				}
				counters.Add(1)

			case kind < 920:
				// 4% updates — only hit upsert_* ids that might exist
				buf, ok := s.getScope(scope)
				if !ok {
					continue
				}
				_, _ = buf.updateByID(
					"upsert_"+strconv.Itoa(rng.Intn(30)),
					json.RawMessage(`{"updated":true}`),
				)
				updates.Add(1)

			case kind < 950:
				// 3% delete-by-id
				buf, ok := s.getScope(scope)
				if !ok {
					continue
				}
				_, _ = buf.deleteByID(fmt.Sprintf("w%d_a%d", wid, rng.Intn(localAppend+1)))
				deletes.Add(1)

			case kind < 975:
				// 2.5% delete_up_to — trim the oldest half of whatever is there
				buf, ok := s.getScope(scope)
				if !ok {
					continue
				}
				buf.mu.RLock()
				var half uint64
				if len(buf.items) > 2 {
					half = buf.items[len(buf.items)/2].Seq
				}
				buf.mu.RUnlock()
				if half > 0 {
					_, _ = buf.deleteUpToSeq(half)
				}
				trims.Add(1)

			case kind < 993:
				// 1.8% delete_scope + immediate recreate
				_, _ = s.deleteScope(scope)
				buf, _ := s.getOrCreateScope(scope)
				_, _ = buf.appendItem(Item{
					Scope:   scope,
					ID:      "reborn",
					Payload: json.RawMessage(`{"reborn":true}`),
				})
				dscopes.Add(1)

			default:
				// 0.7% rebuildAll — the heaviest operation; rare on purpose.
				// A minimal replacement so we exercise the detach-and-swap path
				// without overwhelming everything else the workers are doing.
				grouped := map[string][]Item{
					"rebuilt": {{Scope: "rebuilt", ID: "x", Payload: json.RawMessage(`"r"`)}},
				}
				_, _, _ = s.rebuildAll(grouped)
				rebuilds.Add(1)
			}
		}
	}

	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go worker(w)
	}
	wg.Wait()

	verifyInvariants(t, s)

	t.Logf("ops: reads=%d appends=%d upserts=%d counters=%d updates=%d deletes=%d trims=%d delete_scopes=%d rebuilds=%d",
		reads.Load(), appends.Load(), upserts.Load(), counters.Load(),
		updates.Load(), deletes.Load(), trims.Load(), dscopes.Load(), rebuilds.Load())
}

// TestStress_RecordRead_NoLast7DDriftAcrossRollover hammers the lock-
// free heat-tracking path through a 7-day window rollover and verifies
// that b.last7DReadCount (the runtime atomic counter) ends up agreeing
// with computeLast7DReadCount (the bucket walk used by /stats) and
// equals exactly the new-day read total — i.e. the stale residue
// from 7 days ago was correctly subtracted, no more and no less.
//
// The race this guards against: Phase 1 in recordRead drains a stale
// bucket in two steps (Count.Swap → subtract from last7DReadCount,
// then CAS Day stale→0). If those steps were ordered the other way
// (CAS Day first, then Count.Swap second), a concurrent Phase 2 that
// observed Day==0 between them would CAS(0, today) and Count.Store(0),
// wiping the stale Count without subtracting from last7DReadCount —
// drifting the runtime counter upward by the stale-day read total.
//
// The bug was rare in absolute terms (the window is just two atomic
// operations wide), so this test is not guaranteed to fail on every
// run of a regressed implementation; it does fail reliably on a
// reverted-to-buggy implementation when the scheduler preempts inside
// the window. With the fix, drift is structurally impossible and the
// test passes deterministically.
func TestStress_RecordRead_NoLast7DDriftAcrossRollover(t *testing.T) {
	const (
		staleCount = 5000
		readers    = 64
		perReader  = 5000
	)
	iterations := perReader
	if testing.Short() {
		iterations = 500
	}

	// Day 1000 and day 1007 share bucket index (1000 % 7 == 1007 % 7 == 6),
	// so the today-bucket on day 1007 starts populated with stale day-1000
	// reads, forcing every concurrent caller to race through Phase 1's
	// drain on the same slot.
	buf := NewScopeBuffer(10)
	for i := 0; i < staleCount; i++ {
		buf.recordRead(microsOnDay(1000))
	}
	if got := buf.last7DReadCount.Load(); got != staleCount {
		t.Fatalf("pre-stress last7DReadCount=%d want %d", got, staleCount)
	}

	var wg sync.WaitGroup
	wg.Add(readers)
	for r := 0; r < readers; r++ {
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				buf.recordRead(microsOnDay(1007))
			}
		}()
	}
	wg.Wait()

	expected := uint64(readers * iterations)
	if got := buf.last7DReadCount.Load(); got != expected {
		t.Errorf("runtime last7DReadCount=%d want %d (drift=%d — stale residue not subtracted)",
			got, expected, int64(got)-int64(expected))
	}
	if got := buf.computeLast7DReadCount(microsOnDay(1007)); got != expected {
		t.Errorf("bucket-walk computeLast7DReadCount=%d want %d", got, expected)
	}
	// Final cross-check: the runtime counter and the bucket walk must
	// agree at rest. Drift between the two is the canonical signature
	// of the race, regardless of whether either matches `expected`.
	if buf.last7DReadCount.Load() != buf.computeLast7DReadCount(microsOnDay(1007)) {
		t.Errorf("runtime field=%d != bucket walk=%d (drift between the two accountings)",
			buf.last7DReadCount.Load(), buf.computeLast7DReadCount(microsOnDay(1007)))
	}
}

// verifyInvariants walks every scope under lock and checks coherence between
// the atomic counter, buf.bytes, the items slice, and the two indices. Any
// failure here indicates the concurrency story is broken somewhere above.
func verifyInvariants(t *testing.T, s *Store) {
	t.Helper()

	for shIdx := range s.shards {
		s.shards[shIdx].mu.RLock()
	}
	defer func() {
		for shIdx := range s.shards {
			s.shards[shIdx].mu.RUnlock()
		}
	}()

	var totalBytesSum int64
	var scopeCount int64
	for shIdx := range s.shards {
		scopeCount += int64(len(s.shards[shIdx].scopes))
	}
	for shIdx := range s.shards {
		for name, buf := range s.shards[shIdx].scopes {
			buf.mu.RLock()

			var sum int64
			for _, it := range buf.items {
				sum += approxItemSize(it)
			}
			if sum != buf.bytes {
				t.Errorf("scope %q: sum(approxItemSize)=%d != buf.bytes=%d", name, sum, buf.bytes)
			}

			if len(buf.bySeq) != len(buf.items) {
				t.Errorf("scope %q: len(bySeq)=%d != len(items)=%d", name, len(buf.bySeq), len(buf.items))
			}

			for i := 1; i < len(buf.items); i++ {
				if buf.items[i].Seq <= buf.items[i-1].Seq {
					t.Errorf("scope %q: items not strictly seq-ordered at %d (seq %d <= %d)",
						name, i, buf.items[i].Seq, buf.items[i-1].Seq)
					break
				}
			}

			for id, item := range buf.byID {
				bySeqItem, ok := buf.bySeq[item.Seq]
				if !ok {
					t.Errorf("scope %q: byID[%q].Seq=%d not in bySeq", name, id, item.Seq)
					continue
				}
				if bySeqItem.ID != id {
					t.Errorf("scope %q: byID[%q] points at seq=%d but bySeq[%d].ID=%q",
						name, id, item.Seq, item.Seq, bySeqItem.ID)
				}
			}

			totalBytesSum += buf.bytes
			buf.mu.RUnlock()
		}
	}

	expected := totalBytesSum + scopeCount*scopeBufferOverhead
	if got := s.totalBytes.Load(); got != expected {
		t.Errorf("totalBytes=%d != sum(buf.bytes)=%d + %d×%d overhead = %d (drift=%d)",
			got, totalBytesSum, scopeCount, scopeBufferOverhead, expected, got-expected)
	}
	if totalBytesSum < 0 {
		t.Errorf("sum(buf.bytes)=%d is negative", totalBytesSum)
	}
}
