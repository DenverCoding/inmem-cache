package scopecache

import (
	"bytes"
	"encoding/json"
	"testing"
)

// Defensive payload-byte cloning at the Gateway boundary — the hazard
// description lives in gateway_clone.go. The tests below exercise both
// directions:
//
//   (a) caller-side mutation of an input slice after a write call
//       returns must NOT reach cached state;
//   (b) caller-side mutation of a slice returned from a read call must
//       NOT reach cached state.
//
// Helper convention: every test seeds a payload, hands it to the
// Gateway, mutates it (caller-side), then re-reads via the Gateway and
// asserts the cache still holds the pre-mutation bytes. Re-reads
// themselves go through the same clone discipline, so what the
// assertion observes IS the cache's byte image filtered through one
// more clone.

func newGatewayForCloneTest(t *testing.T) *Gateway {
	t.Helper()
	return NewGateway(Config{
		ScopeMaxItems: 100,
		MaxStoreBytes: 10 << 20,
		MaxItemBytes:  1 << 20,
	})
}

func mutateBytes(b []byte) {
	for i := range b {
		b[i] = 'X'
	}
}

// --- Direction (a): caller mutates input after the call returns -----

func TestGateway_AppendInputClone(t *testing.T) {
	g := newGatewayForCloneTest(t)
	original := []byte(`{"v":1}`)
	payload := append([]byte(nil), original...)

	if _, err := g.Append(Item{Scope: "posts", ID: "p-1", Payload: payload}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	mutateBytes(payload)

	item, hit := g.Get("posts", "p-1", 0)
	if !hit {
		t.Fatalf("Get: hit=false; want true")
	}
	if !bytes.Equal(item.Payload, original) {
		t.Errorf("Append input mutation reached cache: cached=%q want %q", item.Payload, original)
	}
}

func TestGateway_UpsertInputClone_Create(t *testing.T) {
	g := newGatewayForCloneTest(t)
	original := []byte(`{"v":1}`)
	payload := append([]byte(nil), original...)

	if _, _, err := g.Upsert(Item{Scope: "posts", ID: "p-1", Payload: payload}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	mutateBytes(payload)

	item, hit := g.Get("posts", "p-1", 0)
	if !hit {
		t.Fatalf("Get: hit=false; want true")
	}
	if !bytes.Equal(item.Payload, original) {
		t.Errorf("Upsert(create) input mutation reached cache: cached=%q want %q", item.Payload, original)
	}
}

func TestGateway_UpsertInputClone_Replace(t *testing.T) {
	g := newGatewayForCloneTest(t)
	if _, err := g.Append(Item{Scope: "posts", ID: "p-1", Payload: json.RawMessage(`{"old":true}`)}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	original := []byte(`{"new":1}`)
	payload := append([]byte(nil), original...)

	if _, created, err := g.Upsert(Item{Scope: "posts", ID: "p-1", Payload: payload}); err != nil || created {
		t.Fatalf("Upsert(replace): created=%v err=%v", created, err)
	}
	mutateBytes(payload)

	item, hit := g.Get("posts", "p-1", 0)
	if !hit {
		t.Fatalf("Get: hit=false; want true")
	}
	if !bytes.Equal(item.Payload, original) {
		t.Errorf("Upsert(replace) input mutation reached cache: cached=%q want %q", item.Payload, original)
	}
}

func TestGateway_UpdateInputClone(t *testing.T) {
	g := newGatewayForCloneTest(t)
	if _, err := g.Append(Item{Scope: "posts", ID: "p-1", Payload: json.RawMessage(`{"old":true}`)}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	original := []byte(`{"new":2}`)
	payload := append([]byte(nil), original...)

	n, err := g.Update(Item{Scope: "posts", ID: "p-1", Payload: payload})
	if err != nil || n != 1 {
		t.Fatalf("Update: n=%d err=%v", n, err)
	}
	mutateBytes(payload)

	item, hit := g.Get("posts", "p-1", 0)
	if !hit {
		t.Fatalf("Get: hit=false; want true")
	}
	if !bytes.Equal(item.Payload, original) {
		t.Errorf("Update input mutation reached cache: cached=%q want %q", item.Payload, original)
	}
}

func TestGateway_WarmInputClone(t *testing.T) {
	g := newGatewayForCloneTest(t)
	originalA := []byte(`{"a":1}`)
	originalB := []byte(`{"b":2}`)
	pA := append([]byte(nil), originalA...)
	pB := append([]byte(nil), originalB...)

	grouped := map[string][]Item{
		"posts": {
			{Scope: "posts", ID: "p-1", Payload: pA},
			{Scope: "posts", ID: "p-2", Payload: pB},
		},
	}
	if _, err := g.Warm(grouped); err != nil {
		t.Fatalf("Warm: %v", err)
	}
	mutateBytes(pA)
	mutateBytes(pB)

	for _, want := range []struct {
		id      string
		payload []byte
	}{{"p-1", originalA}, {"p-2", originalB}} {
		item, hit := g.Get("posts", want.id, 0)
		if !hit {
			t.Fatalf("Get(%s): hit=false; want true", want.id)
		}
		if !bytes.Equal(item.Payload, want.payload) {
			t.Errorf("Warm input mutation reached cache for %s: cached=%q want %q", want.id, item.Payload, want.payload)
		}
	}
}

func TestGateway_RebuildInputClone(t *testing.T) {
	g := newGatewayForCloneTest(t)
	original := []byte(`{"v":42}`)
	payload := append([]byte(nil), original...)

	grouped := map[string][]Item{
		"posts": {{Scope: "posts", ID: "p-1", Payload: payload}},
	}
	if _, _, err := g.Rebuild(grouped); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	mutateBytes(payload)

	item, hit := g.Get("posts", "p-1", 0)
	if !hit {
		t.Fatalf("Get: hit=false; want true")
	}
	if !bytes.Equal(item.Payload, original) {
		t.Errorf("Rebuild input mutation reached cache: cached=%q want %q", item.Payload, original)
	}
}

// --- Direction (b): caller mutates returned slice -------------------

func TestGateway_AppendOutputClone(t *testing.T) {
	g := newGatewayForCloneTest(t)
	original := []byte(`{"v":1}`)

	result, err := g.Append(Item{Scope: "posts", ID: "p-1", Payload: original})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	mutateBytes(result.Payload)

	item, hit := g.Get("posts", "p-1", 0)
	if !hit {
		t.Fatalf("Get: hit=false; want true")
	}
	if !bytes.Equal(item.Payload, original) {
		t.Errorf("Append output mutation reached cache: cached=%q want %q", item.Payload, original)
	}
}

func TestGateway_UpsertOutputClone(t *testing.T) {
	g := newGatewayForCloneTest(t)
	original := []byte(`{"v":1}`)

	result, _, err := g.Upsert(Item{Scope: "posts", ID: "p-1", Payload: original})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	mutateBytes(result.Payload)

	item, hit := g.Get("posts", "p-1", 0)
	if !hit {
		t.Fatalf("Get: hit=false; want true")
	}
	if !bytes.Equal(item.Payload, original) {
		t.Errorf("Upsert output mutation reached cache: cached=%q want %q", item.Payload, original)
	}
}

func TestGateway_GetOutputClone(t *testing.T) {
	g := newGatewayForCloneTest(t)
	original := []byte(`{"v":1}`)
	if _, err := g.Append(Item{Scope: "posts", ID: "p-1", Payload: original}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	first, hit := g.Get("posts", "p-1", 0)
	if !hit {
		t.Fatalf("Get#1: hit=false; want true")
	}
	mutateBytes(first.Payload)

	second, hit := g.Get("posts", "p-1", 0)
	if !hit {
		t.Fatalf("Get#2: hit=false; want true")
	}
	if !bytes.Equal(second.Payload, original) {
		t.Errorf("Get output mutation reached cache: cached=%q want %q", second.Payload, original)
	}
}

func TestGateway_HeadOutputClone(t *testing.T) {
	g := newGatewayForCloneTest(t)
	original := []byte(`{"v":1}`)
	if _, err := g.Append(Item{Scope: "posts", ID: "p-1", Payload: original}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	items, _, found := g.Head("posts", 0, 10)
	if !found || len(items) != 1 {
		t.Fatalf("Head: found=%v len=%d", found, len(items))
	}
	mutateBytes(items[0].Payload)

	again, hit := g.Get("posts", "p-1", 0)
	if !hit {
		t.Fatalf("Get: hit=false; want true")
	}
	if !bytes.Equal(again.Payload, original) {
		t.Errorf("Head output mutation reached cache: cached=%q want %q", again.Payload, original)
	}
}

func TestGateway_TailOutputClone(t *testing.T) {
	g := newGatewayForCloneTest(t)
	original := []byte(`{"v":1}`)
	if _, err := g.Append(Item{Scope: "posts", ID: "p-1", Payload: original}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	items, _, found := g.Tail("posts", 10, 0)
	if !found || len(items) != 1 {
		t.Fatalf("Tail: found=%v len=%d", found, len(items))
	}
	mutateBytes(items[0].Payload)

	again, hit := g.Get("posts", "p-1", 0)
	if !hit {
		t.Fatalf("Get: hit=false; want true")
	}
	if !bytes.Equal(again.Payload, original) {
		t.Errorf("Tail output mutation reached cache: cached=%q want %q", again.Payload, original)
	}
}

// /render returns either item.Payload (non-string payloads) or
// item.renderBytes (JSON-string payloads, decoded once at write time).
// Both paths must hand back a fresh allocation.

func TestGateway_RenderOutputClone_NonStringPayload(t *testing.T) {
	g := newGatewayForCloneTest(t)
	original := []byte(`{"v":1}`)
	if _, err := g.Append(Item{Scope: "posts", ID: "p-1", Payload: original}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	rendered, hit := g.Render("posts", "p-1", 0)
	if !hit {
		t.Fatalf("Render: hit=false; want true")
	}
	mutateBytes(rendered)

	item, hit := g.Get("posts", "p-1", 0)
	if !hit {
		t.Fatalf("Get: hit=false; want true")
	}
	if !bytes.Equal(item.Payload, original) {
		t.Errorf("Render(non-string) output mutation reached cache: cached=%q want %q", item.Payload, original)
	}
}

func TestGateway_RenderOutputClone_StringPayload(t *testing.T) {
	g := newGatewayForCloneTest(t)
	// JSON-string payload triggers the renderBytes precompute path.
	original := []byte(`"<h1>hello</h1>"`)
	if _, err := g.Append(Item{Scope: "posts", ID: "p-1", Payload: original}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	rendered, hit := g.Render("posts", "p-1", 0)
	if !hit {
		t.Fatalf("Render: hit=false; want true")
	}
	if string(rendered) != "<h1>hello</h1>" {
		t.Fatalf("Render: got %q; want %q", rendered, "<h1>hello</h1>")
	}
	mutateBytes(rendered)

	again, hit := g.Render("posts", "p-1", 0)
	if !hit {
		t.Fatalf("Render#2: hit=false; want true")
	}
	if string(again) != "<h1>hello</h1>" {
		t.Errorf("Render(string) output mutation reached cache: cached=%q want %q", again, "<h1>hello</h1>")
	}
}

// --- Helper-level coverage ------------------------------------------

func TestClonePayload_NilAndEmpty(t *testing.T) {
	if got := clonePayload(nil); got != nil {
		t.Errorf("clonePayload(nil)=%v; want nil", got)
	}
	in := json.RawMessage{}
	out := clonePayload(in)
	if out == nil {
		t.Errorf("clonePayload(empty)=nil; want non-nil empty slice")
	}
	if len(out) != 0 {
		t.Errorf("clonePayload(empty) len=%d; want 0", len(out))
	}
}

func TestClonePayload_DistinctBackingArray(t *testing.T) {
	in := json.RawMessage(`{"v":1}`)
	out := clonePayload(in)
	if &in[0] == &out[0] {
		t.Errorf("clonePayload returned slice aliases input backing array")
	}
	mutateBytes(out)
	if !bytes.Equal(in, []byte(`{"v":1}`)) {
		t.Errorf("mutation of clone reached input: in=%q", in)
	}
}

// --- Counter-pointer round-trip hazard -------------------------------
//
// approxItemSize and the read path both consult the unexported
// counter pointer on Item. If a counter Item retrieved via Gateway.Get
// keeps that pointer set, the caller can swap exported fields and pass
// it back through Append/Upsert/Warm/Rebuild — and the cache will
// happily store a "counter-shaped" item in the new slot. Result:
// MaxItemBytes is under-counted (counterCellOverhead replaces
// len(Payload)) and reads return the original counter value instead
// of the freshly-supplied payload bytes. Silent data corruption.
//
// The fix lives in two places:
//   - Gateway clone helpers strip counter+renderBytes on every cross
//     of the public boundary (input + output), so callers can never
//     observe nor smuggle the unexported state.
//   - Buffer write paths (insertNewItemLocked, buildReplacementState)
//     defensively clear counter on entry, covering future internal
//     callers that might bypass the Gateway clone.
//
// The tests below exercise both layers. Each one uses an actual
// Gateway round-trip (Get → caller-mutate → write → Get) so the
// observable contract is "caller sees what they wrote", not a more
// brittle "internal field is zero" assertion.

// Round-trip via Append: the counter pointer must not survive a
// Get-then-Append cycle that swaps scope/id and replaces the payload.
func TestGateway_CounterPointerDoesNotCrossBoundary_Append(t *testing.T) {
	g := newGatewayForCloneTest(t)

	if _, _, err := g.CounterAdd("counters", "c1", 42); err != nil {
		t.Fatalf("CounterAdd seed: %v", err)
	}

	// Caller pulls the counter item, swaps it into a fresh
	// (scope,id) and replaces the payload with a regular JSON object.
	carrier, hit := g.Get("counters", "c1", 0)
	if !hit {
		t.Fatal("Get(counters/c1) miss")
	}
	carrier.Scope = "posts"
	carrier.ID = ""
	carrier.Seq = 0
	carrier.Ts = 0
	carrier.Payload = json.RawMessage(`{"title":"hello"}`)

	committed, err := g.Append(carrier)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	got, hit := g.Get("posts", "", committed.Seq)
	if !hit {
		t.Fatal("readback miss")
	}
	if string(got.Payload) != `{"title":"hello"}` {
		t.Errorf("readback payload=%q want {\"title\":\"hello\"}; counter pointer survived the boundary",
			got.Payload)
	}
}

// Round-trip via Upsert (miss-branch): same hazard shape, different
// write entry. Upsert with no existing item at (scope,id) routes
// through insertNewItemLocked, exercising the defensive clear there.
func TestGateway_CounterPointerDoesNotCrossBoundary_Upsert(t *testing.T) {
	g := newGatewayForCloneTest(t)

	if _, _, err := g.CounterAdd("counters", "c1", 7); err != nil {
		t.Fatalf("CounterAdd seed: %v", err)
	}

	carrier, hit := g.Get("counters", "c1", 0)
	if !hit {
		t.Fatal("Get miss")
	}
	carrier.Scope = "posts"
	carrier.ID = "p-1"
	carrier.Seq = 0
	carrier.Ts = 0
	carrier.Payload = json.RawMessage(`["a","b","c"]`)

	if _, _, err := g.Upsert(carrier); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, hit := g.Get("posts", "p-1", 0)
	if !hit {
		t.Fatal("readback miss")
	}
	if string(got.Payload) != `["a","b","c"]` {
		t.Errorf("readback payload=%q want [\"a\",\"b\",\"c\"]; counter pointer survived Upsert",
			got.Payload)
	}
}

// Round-trip via Warm: counter pointer must not survive going through
// the bulk-replace pipeline either. buildReplacementState clears it
// defensively, mirroring insertNewItemLocked.
func TestGateway_CounterPointerDoesNotCrossBoundary_Warm(t *testing.T) {
	g := newGatewayForCloneTest(t)

	if _, _, err := g.CounterAdd("counters", "c1", 99); err != nil {
		t.Fatalf("CounterAdd seed: %v", err)
	}

	carrier, hit := g.Get("counters", "c1", 0)
	if !hit {
		t.Fatal("Get miss")
	}
	carrier.Scope = "posts"
	carrier.ID = "p-warmed"
	carrier.Seq = 0 // /warm rejects client-supplied seq; mirror that here.
	carrier.Ts = 0
	carrier.Payload = json.RawMessage(`{"warm":true}`)

	if _, err := g.Warm(map[string][]Item{"posts": {carrier}}); err != nil {
		t.Fatalf("Warm: %v", err)
	}

	got, hit := g.Get("posts", "p-warmed", 0)
	if !hit {
		t.Fatal("readback miss")
	}
	if string(got.Payload) != `{"warm":true}` {
		t.Errorf("readback payload=%q want {\"warm\":true}; counter pointer survived Warm",
			got.Payload)
	}
}

// Direct buffer-level test for the defensive clear. Constructs an
// Item with counter set (impossible from outside the package, but
// inside-package code paths and future addons living within the
// package would have access), passes it through insertNewItemLocked,
// and asserts the stored item has counter cleared.
func TestInsertNewItemLocked_ClearsStaleCounter(t *testing.T) {
	buf := newscopeBuffer(10)

	item := Item{
		Scope:   "s",
		ID:      "x",
		Payload: json.RawMessage(`{"v":1}`),
		counter: &counterCell{}, // stale pointer the caller smuggled in
	}
	stored, err := buf.appendItem(item)
	if err != nil {
		t.Fatalf("appendItem: %v", err)
	}
	if stored.counter != nil {
		t.Error("stored item still carries the stale counter pointer")
	}

	indexed, hit := buf.getByID("x")
	if !hit {
		t.Fatal("indexed item missing")
	}
	if indexed.counter != nil {
		t.Error("indexed item carries the stale counter pointer")
	}
	if string(indexed.Payload) != `{"v":1}` {
		t.Errorf("indexed payload=%q want {\"v\":1}", indexed.Payload)
	}
}
