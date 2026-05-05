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
