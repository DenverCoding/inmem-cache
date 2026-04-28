package scopecache

import (
	"encoding/json"
	"fmt"
	"strconv"
	"sync/atomic"
	"testing"
)

// benchStore builds a store with the given shape and returns it along with
// the set of (scope, id, seq) triples populated, for the benchmark to index.
// numScopes * itemsPerScope items are inserted; each payload is payloadBytes
// of filler JSON. A 100-scope × 1000-item dataset with ~512-byte payloads
// produces roughly 57 MiB of approxItemSize — well above the 50 MiB floor
// we want to measure against.
func benchStore(b *testing.B, numScopes, itemsPerScope, payloadBytes int) (*Store, []string, []string) {
	b.Helper()

	// Cap tall enough for the dataset: 1 GiB store, 1M items per scope.
	store := NewStore(Config{ScopeMaxItems: 1_000_000, MaxStoreBytes: 1 << 30, MaxItemBytes: 1 << 20, MaxResponseBytes: 1 << 30, MaxMultiCallBytes: 16 << 20, MaxMultiCallCount: 10})

	payloadFiller := make([]byte, payloadBytes)
	for i := range payloadFiller {
		payloadFiller[i] = 'x'
	}
	payload, _ := json.Marshal(map[string]string{"data": string(payloadFiller)})

	scopes := make([]string, 0, numScopes)
	ids := make([]string, 0, numScopes*itemsPerScope)

	for s := 0; s < numScopes; s++ {
		scope := fmt.Sprintf("bench:%03d", s)
		scopes = append(scopes, scope)
		buf, err := store.getOrCreateScope(scope)
		if err != nil {
			b.Fatalf("getOrCreateScope: %v", err)
		}
		for i := 0; i < itemsPerScope; i++ {
			id := fmt.Sprintf("item_%06d", i)
			if _, err := buf.appendItem(Item{Scope: scope, ID: id, Payload: payload}); err != nil {
				b.Fatalf("appendItem: %v", err)
			}
			ids = append(ids, id)
		}
	}

	return store, scopes, ids
}

// BenchmarkStore_GetByID measures a single-item read by (scope, id) on a
// fully populated store of ~57 MiB. Reads take a scope RLock and do a single
// map lookup.
func BenchmarkStore_GetByID(b *testing.B) {
	store, scopes, ids := benchStore(b, 100, 1000, 512)
	numScopes := len(scopes)
	itemsPerScope := len(ids) / numScopes

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		scope := scopes[i%numScopes]
		id := ids[i%itemsPerScope]
		buf, _ := store.getScope(scope)
		_, _ = buf.getByID(id)
	}
}

// BenchmarkStore_GetBySeq measures a single-item read by (scope, seq).
// Same access pattern as GetByID — map lookup under an RLock — but keyed by
// the cache-local seq counter.
func BenchmarkStore_GetBySeq(b *testing.B) {
	store, scopes, _ := benchStore(b, 100, 1000, 512)
	numScopes := len(scopes)
	const itemsPerScope = 1000

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		scope := scopes[i%numScopes]
		seq := uint64((i % itemsPerScope) + 1)
		buf, _ := store.getScope(scope)
		_, _ = buf.getBySeq(seq)
	}
}

// BenchmarkStore_GetByID_Parallel runs the same lookup concurrently across
// GOMAXPROCS goroutines to show that RWMutex-guarded reads scale: multiple
// readers on the same scope do not serialize.
func BenchmarkStore_GetByID_Parallel(b *testing.B) {
	store, scopes, ids := benchStore(b, 100, 1000, 512)
	numScopes := len(scopes)
	itemsPerScope := len(ids) / numScopes

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			scope := scopes[i%numScopes]
			id := ids[i%itemsPerScope]
			buf, _ := store.getScope(scope)
			_, _ = buf.getByID(id)
			i++
		}
	})
}

// BenchmarkStore_Append measures a fresh /append on a pre-warmed store.
// Included as a sanity check — writes take the scope write-lock plus an
// atomic store-bytes CAS, so they should be noticeably slower than reads.
func BenchmarkStore_Append(b *testing.B) {
	store, _, _ := benchStore(b, 100, 1000, 512)

	payload := json.RawMessage(`{"data":"benchmark"}`)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		scope := fmt.Sprintf("writes:%03d", i%100)
		buf, err := store.getOrCreateScope(scope)
		if err != nil {
			b.Fatalf("getOrCreateScope: %v", err)
		}
		if _, err := buf.appendItem(Item{Scope: scope, Payload: payload}); err != nil {
			b.Fatalf("appendItem: %v", err)
		}
	}
}

// BenchmarkStore_AppendUniqueScope_Sequential measures /append cost when
// every request creates a fresh scope — the anti-pattern documented in
// CLAUDE.md "Scope modeling". Sequential variant; see _Parallel for the
// contended-shard case. No HTTP layer, so the profile is pure cache work.
//
// Useful for cpuprofile + memprofile to find what dominates after the
// scope-map mutex was sharded: candidates are atomic-CAS contention on
// totalBytes, *ScopeBuffer alloc + map header alloc, and fmt formatting.
func BenchmarkStore_AppendUniqueScope_Sequential(b *testing.B) {
	store := NewStore(Config{
		ScopeMaxItems:     50_000,    // matches phase4 Caddyfile setting
		MaxStoreBytes:     128 << 30, // 128 GiB; b.N is open-ended so don't risk a 507 mid-bench
		MaxItemBytes:      1 << 20,
		MaxResponseBytes:  1 << 30,
		MaxMultiCallBytes: 16 << 20,
		MaxMultiCallCount: 10,
	})

	payload := json.RawMessage(`{"data":"benchmark"}`)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		scope := "u:" + strconv.Itoa(i)
		if _, err := store.AppendOne(Item{Scope: scope, Payload: payload}); err != nil {
			b.Fatalf("AppendOne: %v", err)
		}
	}
}

// BenchmarkStore_AppendUniqueScope_Parallel is the parallel variant of
// the above — every goroutine pulls a unique counter slot and creates
// its own fresh scope. This is the workload that wrk hammered at c=50
// in phase 4. After sharding, throughput plateaus at ~4k req/s on the
// HTTP path; pprof-ing this benchmark reveals what's left to optimise
// (see phase-4 finding "Sharded scopes map: pre-existing-scope writes
// 2× faster, unique-scope writes barely").
//
// Run with:
//
//	go test -run=^$ -bench=AppendUniqueScope_Parallel \
//	    -benchtime=3s -cpuprofile=cpu.prof -memprofile=mem.prof
//	go tool pprof -top cpu.prof
//	go tool pprof -top -alloc_space mem.prof
func BenchmarkStore_AppendUniqueScope_Parallel(b *testing.B) {
	store := NewStore(Config{
		ScopeMaxItems:     1_000_000,
		MaxStoreBytes:     8 << 30,
		MaxItemBytes:      1 << 20,
		MaxResponseBytes:  1 << 30,
		MaxMultiCallBytes: 16 << 20,
		MaxMultiCallCount: 10,
	})

	payload := json.RawMessage(`{"data":"benchmark"}`)
	var counter atomic.Uint64

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			n := counter.Add(1)
			scope := "u:" + strconv.FormatUint(n, 10)
			if _, err := store.AppendOne(Item{Scope: scope, Payload: payload}); err != nil {
				b.Fatalf("AppendOne: %v", err)
			}
		}
	})
}

// benchTsScope builds a single scope populated with `n` items, each carrying
// a monotonically increasing `ts` equal to its seq. Returns the scope buffer
// so /ts_range benchmarks can call tsRange directly without the HTTP layer.
func benchTsScope(b *testing.B, n int) *ScopeBuffer {
	b.Helper()

	store := NewStore(Config{ScopeMaxItems: 1_000_000, MaxStoreBytes: 1 << 30, MaxItemBytes: 1 << 20, MaxResponseBytes: 1 << 30, MaxMultiCallBytes: 16 << 20, MaxMultiCallCount: 10})
	buf, err := store.getOrCreateScope("bench_ts")
	if err != nil {
		b.Fatalf("getOrCreateScope: %v", err)
	}

	payload := json.RawMessage(`{"v":1}`)
	for i := 0; i < n; i++ {
		ts := int64(i + 1)
		if _, err := buf.appendItem(Item{Scope: "bench_ts", Ts: &ts, Payload: payload}); err != nil {
			b.Fatalf("appendItem: %v", err)
		}
	}
	return buf
}

// BenchmarkStore_TsRange_Realistic measures /ts_range on a realistic 2,000-item
// scope where every item matches the window. The scan collects 1,000 items and
// early-exits on the 1,001st match — the "normal" case for a client paging
// through a modestly-sized time window. Cost is dominated by ~1,001 loop
// iterations plus one 1,000-cap slice allocation.
func BenchmarkStore_TsRange_Realistic(b *testing.B) {
	buf := benchTsScope(b, 2000)
	since := int64(0)
	until := int64(1 << 62)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		items, truncated := buf.tsRange(&since, &until, 1000)
		if len(items) != 1000 || !truncated {
			b.Fatalf("unexpected: len=%d truncated=%v", len(items), truncated)
		}
	}
}

// BenchmarkStore_TsRange_FullScope_Worst measures the upper bound: a maxed-out
// 100,000-item scope where the matching window sits at the tail end. The scan
// walks every non-matching item (98,999 of them) before entering the matching
// tail, collects 1,000 items, sees the 1,001st, and returns truncated=true.
// Total items touched: the full 100,000. This is the pathological /ts_range
// cost at the default per-scope cap.
func BenchmarkStore_TsRange_FullScope_Worst(b *testing.B) {
	buf := benchTsScope(b, 100_000)
	since := int64(99_000)
	until := int64(1 << 62)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		items, truncated := buf.tsRange(&since, &until, 1000)
		if len(items) != 1000 || !truncated {
			b.Fatalf("unexpected: len=%d truncated=%v", len(items), truncated)
		}
	}
}

// --- Parallel read-path benchmarks ----------------------------------
//
// These were added to profile the read path under real concurrency
// (32 cores in the bench host). The bare lookup methods (getByID,
// getBySeq, tailOffset, sinceSeq, tsRange) only take buf.mu.RLock(),
// so they should scale linearly with cores. The HTTP-equivalent paths
// (/get, /render, /head, /tail, /ts_range) additionally call
// buf.recordRead() after a successful lookup, which takes
// buf.mu.Lock() — a write lock on the same mutex the readers were
// just on. The "_WithRecordRead" suffix on benches below tells you
// whether that second lock is included.
//
// Run with -cpuprofile -memprofile -blockprofile to find what
// dominates the parallel read path.

// BenchmarkStore_GetBySeq_Parallel mirrors GetByID_Parallel but on the
// bySeq map. Same RLock + map lookup; included so the two read paths
// can be compared side by side.
func BenchmarkStore_GetBySeq_Parallel(b *testing.B) {
	store, scopes, _ := benchStore(b, 100, 1000, 512)
	numScopes := len(scopes)
	const itemsPerScope = 1000

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			scope := scopes[i%numScopes]
			seq := uint64((i % itemsPerScope) + 1)
			buf, _ := store.getScope(scope)
			_, _ = buf.getBySeq(seq)
			i++
		}
	})
}

// BenchmarkStore_GetByID_Parallel_WithRecordRead is the bench above
// plus the recordRead(now) call that /get and /render perform on a hit.
// recordRead takes buf.mu.Lock() (write lock), serialising every read
// on a given scope. The delta between this and GetByID_Parallel
// quantifies the heat-bucket cost on the hot read path.
func BenchmarkStore_GetByID_Parallel_WithRecordRead(b *testing.B) {
	store, scopes, ids := benchStore(b, 100, 1000, 512)
	numScopes := len(scopes)
	itemsPerScope := len(ids) / numScopes

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			scope := scopes[i%numScopes]
			id := ids[i%itemsPerScope]
			buf, _ := store.getScope(scope)
			if _, ok := buf.getByID(id); ok {
				buf.recordRead(nowUnixMicro())
			}
			i++
		}
	})
}

// BenchmarkStore_GetByID_Parallel_HeatDisabled is the equivalent of
// _WithRecordRead but with the path that handlers take when
// Config.DisableReadHeat is true: skip recordRead entirely (and the
// time.Now() that feeds it). Mimics what /get and /render do at runtime
// in heat-off mode. Compare to _WithRecordRead to see the per-op
// savings on the read-hot path when heat tracking is off.
func BenchmarkStore_GetByID_Parallel_HeatDisabled(b *testing.B) {
	store, scopes, ids := benchStore(b, 100, 1000, 512)
	numScopes := len(scopes)
	itemsPerScope := len(ids) / numScopes

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			scope := scopes[i%numScopes]
			id := ids[i%itemsPerScope]
			buf, _ := store.getScope(scope)
			_, _ = buf.getByID(id)
			// recordRead intentionally skipped — what disableReadHeat does.
			i++
		}
	})
}

// BenchmarkStore_Tail_Parallel measures /tail-style reads (most-recent
// items) at limit=10 — the typical small-window read pattern.
// tailOffset takes RLock, slices b.items[start:end], copies into a
// fresh slice, returns. Per-call slice alloc + copy is the dominant
// per-op cost.
func BenchmarkStore_Tail_Parallel(b *testing.B) {
	store, scopes, _ := benchStore(b, 100, 1000, 512)
	numScopes := len(scopes)

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			scope := scopes[i%numScopes]
			buf, _ := store.getScope(scope)
			_, _ = buf.tailOffset(10, 0)
			i++
		}
	})
}

// BenchmarkStore_Head_Parallel measures /head-style reads (oldest-
// first with afterSeq cursor). sinceSeq does sort.Search + slice copy
// under RLock.
func BenchmarkStore_Head_Parallel(b *testing.B) {
	store, scopes, _ := benchStore(b, 100, 1000, 512)
	numScopes := len(scopes)

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			scope := scopes[i%numScopes]
			buf, _ := store.getScope(scope)
			_, _ = buf.sinceSeq(0, 10)
			i++
		}
	})
}

// BenchmarkStore_TsRange_Parallel measures /ts_range under concurrency.
// Cost is O(n) scan over b.items under RLock; with limit=10 on a
// 1000-item scope where every item matches, the scan early-exits at
// the 11th match. Multiple readers hold RLock concurrently, so this
// should scale with cores up to RLock cache-line contention.
func BenchmarkStore_TsRange_Parallel(b *testing.B) {
	buf := benchTsScope(b, 1000)
	since := int64(0)
	until := int64(1 << 62)

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _ = buf.tsRange(&since, &until, 10)
		}
	})
}
