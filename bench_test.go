package scopecache

import (
	"encoding/json"
	"fmt"
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
	store := NewStore(1_000_000, 1<<30, 1<<20)

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

// benchTsScope builds a single scope populated with `n` items, each carrying
// a monotonically increasing `ts` equal to its seq. Returns the scope buffer
// so /ts_range benchmarks can call tsRange directly without the HTTP layer.
func benchTsScope(b *testing.B, n int) *ScopeBuffer {
	b.Helper()

	store := NewStore(1_000_000, 1<<30, 1<<20)
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
