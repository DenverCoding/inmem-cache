package scopecache

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
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
	store := NewStore(Config{ScopeMaxItems: 1_000_000, MaxStoreBytes: 1 << 30, MaxItemBytes: 1 << 20})

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
		ScopeMaxItems: 50_000,    // matches phase4 Caddyfile setting
		MaxStoreBytes: 128 << 30, // 128 GiB; b.N is open-ended so don't risk a 507 mid-bench
		MaxItemBytes:  1 << 20,
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
		ScopeMaxItems: 1_000_000,
		MaxStoreBytes: 8 << 30,
		MaxItemBytes:  1 << 20,
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

// BenchmarkStore_RenderStringPayload_Parallel times the in-process
// /render hot path on a JSON-string payload (no HTTP, no socket).
// Pre-fix the handler ran bytes.TrimLeft + json.Unmarshal +
// []byte cast on every hit; post-fix it uses item.renderBytes
// precomputed at write time. The HTTP-fronted measurement absorbs
// most of this difference into the request-handling floor — this
// bench is the cleaner read of the per-call cache-side savings.
func BenchmarkStore_RenderStringPayload_Parallel(b *testing.B) {
	store := NewStore(Config{
		ScopeMaxItems: 100_000,
		MaxStoreBytes: 1 << 30,
		MaxItemBytes:  1 << 20,
	})
	const scope = "html"
	buf, err := store.getOrCreateScope(scope)
	if err != nil {
		b.Fatalf("getOrCreateScope: %v", err)
	}
	rawHTML := strings.Repeat("x", 500)
	payload, _ := json.Marshal("<html>" + rawHTML + "</html>")
	const n = 1000
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		id := "page-" + strconv.Itoa(i)
		ids[i] = id
		if _, err := buf.appendItem(Item{Scope: scope, ID: id, Payload: payload}); err != nil {
			b.Fatalf("appendItem: %v", err)
		}
	}

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			item, _ := buf.getByID(ids[i%n])
			var out []byte
			if item.renderBytes != nil {
				out = item.renderBytes
			} else {
				out = item.Payload
			}
			if len(out) == 0 {
				b.Fatalf("empty render output")
			}
			i++
		}
	})
}

// BenchmarkStore_CounterAdd_LargeScope measures /counter_add on an
// existing item inside a scope that already has many other items. The
// pre-rewrite path linear-scanned b.items by ID after a byID map hit;
// after the indexBySeqLocked rewrite this is a binary search on Seq
// (O(log n)). On a 10k-item scope the asymptotic difference is
// ~5000-vs-13 comparisons per call. Counter and update workloads that
// hot-loop on a handful of scopes with many items see this directly.
func BenchmarkStore_CounterAdd_LargeScope(b *testing.B) {
	store := NewStore(Config{
		ScopeMaxItems: 100_000,
		MaxStoreBytes: 1 << 30,
		MaxItemBytes:  1 << 20,
	})
	const scope = "counters"
	buf, err := store.getOrCreateScope(scope)
	if err != nil {
		b.Fatalf("getOrCreateScope: %v", err)
	}
	// Seed 10 000 counter items so the slice scan path has enough length
	// to dominate over the map lookup. The target id is at the *end* of
	// the slice — worst case for the old linear scan, identical cost
	// for the new binary search.
	const n = 10_000
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("counter_%05d", i)
		if _, _, err := buf.counterAdd(scope, id, 1); err != nil {
			b.Fatalf("seed counterAdd: %v", err)
		}
	}
	targetID := fmt.Sprintf("counter_%05d", n-1)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if _, _, err := buf.counterAdd(scope, targetID, 1); err != nil {
			b.Fatalf("counterAdd: %v", err)
		}
	}
}

// --- Parallel read-path benchmarks ----------------------------------
//
// These were added to profile the read path under real concurrency
// (32 cores in the bench host). The bare lookup methods (getByID,
// getBySeq, tailOffset, sinceSeq) only take buf.mu.RLock(),
// so they should scale linearly with cores. The HTTP-equivalent paths
// (/get, /render, /head, /tail) additionally call
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
// APIConfig.DisableReadHeat is true: skip recordRead entirely (and the
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

// --- Post-sharding write-path contention probes -------------------------------
//
// After sharding the scopes map, the store-wide RWMutex is no longer the
// scaling ceiling on the write path. The next-most-shared resource is
// totalBytes (atomic.Int64) — every reserveBytes() call hits its CAS loop.
// These three benchmarks isolate where the remaining contention sits so a
// future "shard-local byte counter" rewrite can be measured-justified
// instead of preemptively guessed.
//
// Run all three with cpuprofile + memprofile:
//
//	go test -run=^$ -bench='Append1Scope_Parallel|Append32Scopes_Parallel|AppendNearCap_Parallel' \
//	    -benchtime=3s -cpuprofile=cpu.prof -memprofile=mem.prof
//	go tool pprof -top -cum cpu.prof
//
// Look for reserveBytes / (*atomic.Int64).CompareAndSwap in the top
// frames. If they dominate, sharded byte counters become a real option;
// if they don't, leave the central counter alone.

// BenchmarkStore_Append1Scope_Parallel pins every goroutine to a single
// pre-existing scope. Maximises buf.mu serialisation and hits totalBytes
// CAS contention on top. The scope-map fast-path (sh.mu.RLock + map
// lookup) is essentially free since the scope already exists.
//
// Compare against _Append32Scopes_Parallel: the throughput delta is the
// upper bound on how much buf.mu costs us.
func BenchmarkStore_Append1Scope_Parallel(b *testing.B) {
	store := NewStore(Config{
		ScopeMaxItems: 100_000_000, // far above any realistic b.N
		MaxStoreBytes: 128 << 30,
		MaxItemBytes:  1 << 20,
	})
	if _, err := store.getOrCreateScope("shared"); err != nil {
		b.Fatalf("pre-create: %v", err)
	}
	payload := json.RawMessage(`{"data":"benchmark"}`)

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := store.AppendOne(Item{Scope: "shared", Payload: payload}); err != nil {
				b.Fatalf("AppendOne: %v", err)
			}
		}
	})
}

// BenchmarkStore_Append32Scopes_Parallel uses 32 pre-existing scopes,
// goroutines round-robin across them via an atomic counter. With the
// per-process maphash seed, 32 names land on ~21 unique shards on
// average (birthday-paradox), so buf.mu contention drops by ~32× vs
// the 1-scope variant and shard-write-lock contention is zero (RLock
// fast-path on every iteration). What's left of the contention budget
// is overwhelmingly totalBytes CAS — this is the benchmark whose CPU
// profile most directly answers "is the central byte counter the next
// bottleneck?".
//
// Compare to _Append1Scope_Parallel (much higher buf.mu pressure) and
// to _AppendUniqueScope_Parallel (adds shard-write-lock + scope-create
// + struct-alloc overhead on top of the same totalBytes CAS).
func BenchmarkStore_Append32Scopes_Parallel(b *testing.B) {
	store := NewStore(Config{
		ScopeMaxItems: 100_000_000,
		MaxStoreBytes: 128 << 30,
		MaxItemBytes:  1 << 20,
	})
	const numScopes = 32
	scopes := make([]string, numScopes)
	for i := range scopes {
		scopes[i] = "shard-" + strconv.Itoa(i)
		if _, err := store.getOrCreateScope(scopes[i]); err != nil {
			b.Fatalf("pre-create %d: %v", i, err)
		}
	}
	payload := json.RawMessage(`{"data":"benchmark"}`)
	var counter atomic.Uint64

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			i := counter.Add(1) - 1
			scope := scopes[i%numScopes]
			if _, err := store.AppendOne(Item{Scope: scope, Payload: payload}); err != nil {
				b.Fatalf("AppendOne: %v", err)
			}
		}
	})
}

// BenchmarkStore_AppendNearCap_Parallel measures the 507-rejection path.
// Cap is sized so the pre-fill anchor item + scope overhead exactly
// fits, leaving zero headroom; every parallel benchmark iteration then
// fails reserveBytes() with *StoreFullError. Useful to:
//
//   - profile what dominates the rejection path (allocation cost in
//     the *StoreFullError construction is a candidate)
//   - confirm reserveBytes' CAS loop only retries on lost CAS, not on
//     cap-exceeded — under heavy rejection traffic the loop should
//     execute exactly once per call (Load + compare + return false)
//   - rule out pathological behaviour (livelock, unbounded retry) when
//     admission control is the hot path on a multi-tenant cache that's
//     pegged at its byte cap.
//
// The anchor item exists so cleanupIfEmptyAndUnused never runs (its
// guard sees len(buf.items) > 0 and returns early), keeping the
// rejection path pure — no scope-create churn.
func BenchmarkStore_AppendNearCap_Parallel(b *testing.B) {
	// scopeBufferOverhead (1024) + room for one 61-byte anchor item.
	// approxItemSize(anchor) = 32 + 6 (scope "victim") + 6 (id "anchor")
	// + 8 (Seq) + 8 (Ts slot) + 1 (payload "1") = 61.
	const anchorSize = 61
	capBytes := int64(scopeBufferOverhead) + anchorSize
	store := NewStore(Config{
		ScopeMaxItems: 100_000_000,
		MaxStoreBytes: capBytes,
		MaxItemBytes:  1 << 20,
	})
	if _, err := store.AppendOne(Item{Scope: "victim", ID: "anchor", Payload: json.RawMessage(`1`)}); err != nil {
		b.Fatalf("anchor: %v", err)
	}
	payload := json.RawMessage(`{"data":"benchmark"}`)

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			// Expected: every call returns *StoreFullError. We don't
			// inspect the error type here — the benchmark cost IS the
			// rejection-path cost.
			_, _ = store.AppendOne(Item{Scope: "victim", Payload: payload})
		}
	})
}
