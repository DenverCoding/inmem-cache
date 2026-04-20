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
	store := NewStore(1_000_000, 1<<30)

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
