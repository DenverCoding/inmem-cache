package scopecache

import (
	"encoding/json"
	"strconv"
	"testing"
)

// One-off Gateway-vs-store benchmarks for the defensive payload
// cloning landed in [gateway_clone.go]. Side-by-side measurements
// across payload sizes so the per-call clone cost is observable as a
// concrete ns/op delta and B/op delta — not hand-waved as "tiny" or
// "doesn't matter".
//
// Naming is intentionally parallel: every BenchmarkGateway_X has a
// matching BenchmarkStore_X that walks the same path WITHOUT clones.
// Running both:
//
//	go test -run=^$ -bench='Gateway_(Append|Upsert|Get|Tail|Render)|Store_(AppendDirect|UpsertDirect|GetDirect|TailDirect|RenderDirect)' -benchmem -benchtime=1s ./...
//
// Then read clone cost as Gateway − Store at each size. If the delta
// is dwarfed by the rest of the call (small payloads, low overhead in
// dispatcher), nothing more to do; if it dominates at large payloads
// that's expected (memmove of N bytes scales linearly).
//
// This file is decoration around the existing test suite — none of
// the benchmarks here are required by the cloning correctness tests
// in gateway_clone_test.go. Once the numbers are read once, the
// whole file can be deleted (`rm bench_gateway_test.go`) without
// affecting any other test.

// payloadSizes covers the realistic span of cache payloads:
//
//   - 64 B    — tiny structured event ("op":"create","id":"abc")
//   - 1 KiB   — typical small record
//   - 64 KiB  — _inbox per-item cap default; medium structured doc
//   - 1 MiB   — global per-item cap default; pre-rendered HTML page
//
// The clone cost is dominated by memmove → expect ns/op to scale
// roughly linearly with size. B/op shows the extra alloc per call.
var payloadSizes = []struct {
	label string
	bytes int
}{
	{"64B", 64},
	{"1KiB", 1024},
	{"64KiB", 64 * 1024},
	{"1MiB", 1 << 20},
}

// makePayload returns a json.RawMessage of approximately n bytes
// shaped as `{"data":"xxxx…"}` so it parses as JSON (the validator
// accepts it) and survives precomputeRenderBytes (object payload, not
// JSON-string, so renderBytes stays nil — keeps the comparison clean).
func makePayload(n int) json.RawMessage {
	if n < 16 {
		n = 16
	}
	filler := make([]byte, n-len(`{"data":""}`))
	for i := range filler {
		filler[i] = 'x'
	}
	return json.RawMessage(`{"data":"` + string(filler) + `"}`)
}

func newBenchGateway(b *testing.B) *Gateway {
	b.Helper()
	return NewGateway(Config{
		ScopeMaxItems: 1_000_000_000,
		// 1 PiB — fits in int64 and ensures the byte counter never
		// trips 507 during benchtime calibration sweeps, even at 1 MiB
		// per op × millions of iterations. Real memory is bounded by
		// the shared payload backing array, not by what totalBytes
		// reports.
		MaxStoreBytes: 1 << 50,
		// 4 MiB — accommodates a 1 MiB JSON-string payload (stored
		// payload bytes + decoded renderBytes ≈ 2 MiB) plus envelope.
		MaxItemBytes: 4 << 20,
	})
}

func newBenchStore(b *testing.B) *store {
	b.Helper()
	return newStore(Config{
		ScopeMaxItems: 1_000_000_000,
		MaxStoreBytes: 1 << 50,
		MaxItemBytes:  4 << 20,
	})
}

// --- Append (entry+exit clone) --------------------------------------

// BenchmarkGateway_Append measures gw.Append at varied payload sizes.
// The Gateway path clones once on entry and once on exit, so the
// delta against BenchmarkStore_AppendDirect at the same size is
// (entry clone + exit clone) cost.
func BenchmarkGateway_Append(b *testing.B) {
	for _, sz := range payloadSizes {
		b.Run(sz.label, func(b *testing.B) {
			gw := newBenchGateway(b)
			payload := makePayload(sz.bytes)
			scope := "bench:append"

			b.ReportAllocs()
			b.SetBytes(int64(sz.bytes))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				if _, err := gw.Append(Item{Scope: scope, Payload: payload}); err != nil {
					b.Fatalf("Append: %v", err)
				}
			}
		})
	}
}

// BenchmarkStore_AppendDirect is the no-clone baseline for Gateway_Append.
// Same workload, calls store.appendOne directly so no boundary-clone
// runs.
func BenchmarkStore_AppendDirect(b *testing.B) {
	for _, sz := range payloadSizes {
		b.Run(sz.label, func(b *testing.B) {
			s := newBenchStore(b)
			payload := makePayload(sz.bytes)
			scope := "bench:append"

			b.ReportAllocs()
			b.SetBytes(int64(sz.bytes))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				if _, err := s.appendOne(Item{Scope: scope, Payload: payload}); err != nil {
					b.Fatalf("appendOne: %v", err)
				}
			}
		})
	}
}

// --- Upsert (entry+exit clone) --------------------------------------

// BenchmarkGateway_Upsert hits the create branch on every iteration
// (fresh id per call) so the workload matches Append's. Replace branch
// would be a different bench (read-modify-write).
func BenchmarkGateway_Upsert(b *testing.B) {
	for _, sz := range payloadSizes {
		b.Run(sz.label, func(b *testing.B) {
			gw := newBenchGateway(b)
			payload := makePayload(sz.bytes)
			scope := "bench:upsert"

			b.ReportAllocs()
			b.SetBytes(int64(sz.bytes))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				id := "u-" + strconv.Itoa(i)
				if _, _, err := gw.Upsert(Item{Scope: scope, ID: id, Payload: payload}); err != nil {
					b.Fatalf("Upsert: %v", err)
				}
			}
		})
	}
}

func BenchmarkStore_UpsertDirect(b *testing.B) {
	for _, sz := range payloadSizes {
		b.Run(sz.label, func(b *testing.B) {
			s := newBenchStore(b)
			payload := makePayload(sz.bytes)
			scope := "bench:upsert"

			b.ReportAllocs()
			b.SetBytes(int64(sz.bytes))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				id := "u-" + strconv.Itoa(i)
				if _, _, err := s.upsertOne(Item{Scope: scope, ID: id, Payload: payload}); err != nil {
					b.Fatalf("upsertOne: %v", err)
				}
			}
		})
	}
}

// --- Get (exit-only clone) ------------------------------------------

// seedGateway pre-populates a single scope with `count` items each
// carrying a payload of `bytes` bytes. The IDs are deterministic
// (`p-0` … `p-<count-1>`) so read benches can index into them
// modulo count.
func seedGateway(b *testing.B, gw *Gateway, scope string, count, bytes int) []string {
	b.Helper()
	payload := makePayload(bytes)
	ids := make([]string, count)
	for i := 0; i < count; i++ {
		id := "p-" + strconv.Itoa(i)
		ids[i] = id
		if _, err := gw.Append(Item{Scope: scope, ID: id, Payload: payload}); err != nil {
			b.Fatalf("seed: %v", err)
		}
	}
	return ids
}

func seedStore(b *testing.B, s *store, scope string, count, bytes int) []string {
	b.Helper()
	payload := makePayload(bytes)
	ids := make([]string, count)
	for i := 0; i < count; i++ {
		id := "p-" + strconv.Itoa(i)
		ids[i] = id
		if _, err := s.appendOne(Item{Scope: scope, ID: id, Payload: payload}); err != nil {
			b.Fatalf("seed: %v", err)
		}
	}
	return ids
}

// BenchmarkGateway_Get measures the exit-side clone on a single-item
// read. Delta vs BenchmarkStore_GetDirect is the per-call clone cost
// for that payload size.
func BenchmarkGateway_Get(b *testing.B) {
	for _, sz := range payloadSizes {
		b.Run(sz.label, func(b *testing.B) {
			gw := newBenchGateway(b)
			scope := "bench:get"
			const count = 100
			ids := seedGateway(b, gw, scope, count, sz.bytes)

			b.ReportAllocs()
			b.SetBytes(int64(sz.bytes))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				if _, ok := gw.Get(scope, ids[i%count], 0); !ok {
					b.Fatalf("Get miss")
				}
			}
		})
	}
}

func BenchmarkStore_GetDirect(b *testing.B) {
	for _, sz := range payloadSizes {
		b.Run(sz.label, func(b *testing.B) {
			s := newBenchStore(b)
			scope := "bench:get"
			const count = 100
			ids := seedStore(b, s, scope, count, sz.bytes)

			b.ReportAllocs()
			b.SetBytes(int64(sz.bytes))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				if _, ok := s.get(scope, ids[i%count], 0); !ok {
					b.Fatalf("get miss")
				}
			}
		})
	}
}

// --- Tail (exit clones N items) -------------------------------------

// BenchmarkGateway_Tail measures the exit-clone cost across a
// limit=10 window — the typical drainer/UI read shape. Each call
// clones 10 payloads, so the delta vs Store_TailDirect is roughly
// 10× single-payload clone cost.
func BenchmarkGateway_Tail(b *testing.B) {
	for _, sz := range payloadSizes {
		b.Run(sz.label, func(b *testing.B) {
			gw := newBenchGateway(b)
			scope := "bench:tail"
			seedGateway(b, gw, scope, 100, sz.bytes)

			b.ReportAllocs()
			b.SetBytes(int64(sz.bytes) * 10)
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				items, _, found := gw.Tail(scope, 10, 0)
				if !found || len(items) != 10 {
					b.Fatalf("Tail: found=%v len=%d", found, len(items))
				}
			}
		})
	}
}

func BenchmarkStore_TailDirect(b *testing.B) {
	for _, sz := range payloadSizes {
		b.Run(sz.label, func(b *testing.B) {
			s := newBenchStore(b)
			scope := "bench:tail"
			seedStore(b, s, scope, 100, sz.bytes)

			b.ReportAllocs()
			b.SetBytes(int64(sz.bytes) * 10)
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				items, _, found := s.tail(scope, 10, 0)
				if !found || len(items) != 10 {
					b.Fatalf("tail: found=%v len=%d", found, len(items))
				}
			}
		})
	}
}

// --- Render (exit-only clone) ---------------------------------------

// BenchmarkGateway_Render exercises the JSON-string renderBytes
// shortcut — the path that benefits most from being a "free" pointer
// hand-back today, and now incurs the per-call clone cost. Pre-cloned
// renderBytes are large strings (HTML pages, pre-rendered fragments),
// so the delta here is the most operationally meaningful one.
func BenchmarkGateway_Render(b *testing.B) {
	for _, sz := range payloadSizes {
		b.Run(sz.label, func(b *testing.B) {
			gw := newBenchGateway(b)
			scope := "bench:render"
			// JSON-string payload triggers the renderBytes precompute
			// at write time, so Render's exit returns the precomputed
			// bytes through clonePayload.
			html := make([]byte, sz.bytes-2) // 2 for the surrounding quotes
			for i := range html {
				html[i] = 'x'
			}
			payload, _ := json.Marshal(string(html))
			ids := make([]string, 100)
			for i := range ids {
				ids[i] = "p-" + strconv.Itoa(i)
				if _, err := gw.Append(Item{Scope: scope, ID: ids[i], Payload: payload}); err != nil {
					b.Fatalf("seed: %v", err)
				}
			}

			b.ReportAllocs()
			b.SetBytes(int64(sz.bytes))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				if _, ok := gw.Render(scope, ids[i%len(ids)], 0); !ok {
					b.Fatalf("Render miss")
				}
			}
		})
	}
}

func BenchmarkStore_RenderDirect(b *testing.B) {
	for _, sz := range payloadSizes {
		b.Run(sz.label, func(b *testing.B) {
			s := newBenchStore(b)
			scope := "bench:render"
			html := make([]byte, sz.bytes-2)
			for i := range html {
				html[i] = 'x'
			}
			payload, _ := json.Marshal(string(html))
			ids := make([]string, 100)
			for i := range ids {
				ids[i] = "p-" + strconv.Itoa(i)
				if _, err := s.appendOne(Item{Scope: scope, ID: ids[i], Payload: payload}); err != nil {
					b.Fatalf("seed: %v", err)
				}
			}

			b.ReportAllocs()
			b.SetBytes(int64(sz.bytes))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				if _, ok := s.render(scope, ids[i%len(ids)], 0); !ok {
					b.Fatalf("render miss")
				}
			}
		})
	}
}
