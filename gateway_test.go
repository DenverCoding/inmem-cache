package scopecache

import (
	"encoding/json"
	"errors"
	"strconv"
	"testing"
)

// newGatewayForTest builds a *Gateway with generous caps + events_mode=full
// so the data-plane methods exercise the same auto-populate path that
// production deployments will use.
func newGatewayForTest(t *testing.T) *Gateway {
	t.Helper()
	return NewGateway(Config{
		ScopeMaxItems: 1000,
		MaxStoreBytes: 100 << 20,
		MaxItemBytes:  1 << 20,
		Events:        EventsConfig{Mode: EventsModeFull},
	})
}

// Subscribe via Gateway must mirror Store.Subscribe — same scope rules,
// same single-subscriber rule, same channel semantics. Smoke-test:
// Gateway.Subscribe returns ErrInvalidSubscribeScope on a user scope,
// ErrAlreadySubscribed on a duplicate, and a working channel on the
// happy path.
func TestGateway_Subscribe(t *testing.T) {
	g := newGatewayForTest(t)

	// Reject non-reserved scope.
	if _, _, err := g.Subscribe("posts"); !errors.Is(err, ErrInvalidSubscribeScope) {
		t.Errorf("Subscribe(posts): err=%v want ErrInvalidSubscribeScope", err)
	}

	// Happy path: _events accepted.
	ch1, unsub1, err := g.Subscribe(EventsScopeName)
	if err != nil {
		t.Fatalf("Subscribe(_events): %v", err)
	}

	// Second Subscribe to same scope must reject.
	if _, _, err := g.Subscribe(EventsScopeName); !errors.Is(err, ErrAlreadySubscribed) {
		t.Errorf("second Subscribe(_events): err=%v want ErrAlreadySubscribed", err)
	}

	// A write triggers the wake-up — same as Store.Subscribe.
	if _, err := g.Append(Item{
		Scope:   "posts",
		ID:      "p-1",
		Payload: json.RawMessage(`{"v":1}`),
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	select {
	case <-ch1:
	default:
		t.Errorf("expected wake-up after Append; channel was empty")
	}

	unsub1()
	// Re-subscribe after unsub must succeed (slot reusable).
	_, unsub2, err := g.Subscribe(EventsScopeName)
	if err != nil {
		t.Errorf("Subscribe after unsub: %v", err)
	}
	unsub2()
}

// Direct Gateway callers can hand a json.RawMessage with arbitrary
// bytes — encoding/json's structural-scan-during-Decode (which the
// HTTP path relies on) does not run on the Go-API path. The
// validator's json.Valid check is the one place this is caught
// before the bytes reach the store and propagate to readers.
//
// All five write-path entry points share validateWriteItem /
// validateUpsertItem / validateUpdateItem, so one test per path
// proves the wiring is correct end-to-end.
func TestGateway_RejectsInvalidJSONPayload(t *testing.T) {
	g := newGatewayForTest(t)

	bad := json.RawMessage(`{"a":`)

	if _, err := g.Append(Item{Scope: "s", Payload: bad}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("Append: err=%v want ErrInvalidInput", err)
	}
	if _, _, err := g.Upsert(Item{Scope: "s", ID: "x", Payload: bad}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("Upsert: err=%v want ErrInvalidInput", err)
	}
	if _, err := g.Update(Item{Scope: "s", ID: "x", Payload: bad}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("Update: err=%v want ErrInvalidInput", err)
	}
	// Warm/Rebuild validate every item via validateWriteItem; one bad
	// item in the grouped map must reject the whole batch (no partial
	// apply).
	if _, err := g.Warm(map[string][]Item{"s": {{Scope: "s", Payload: bad}}}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("Warm: err=%v want ErrInvalidInput", err)
	}
	if _, _, err := g.Rebuild(map[string][]Item{"s": {{Scope: "s", Payload: bad}}}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("Rebuild: err=%v want ErrInvalidInput", err)
	}
}

// Gateway.Append exercises the validation pass: a missing scope is
// rejected without ever reaching Store.appendOne.
func TestGateway_AppendValidates(t *testing.T) {
	g := newGatewayForTest(t)

	// Missing scope — validation fails.
	if _, err := g.Append(Item{Payload: json.RawMessage(`{}`)}); err == nil {
		t.Errorf("Append with empty scope: err=nil; want validation error")
	}

	// Missing payload — validation fails.
	if _, err := g.Append(Item{Scope: "posts"}); err == nil {
		t.Errorf("Append with empty payload: err=nil; want validation error")
	}

	// Happy path — committed.
	result, err := g.Append(Item{
		Scope:   "posts",
		ID:      "p-1",
		Payload: json.RawMessage(`{"v":1}`),
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if result.Seq == 0 {
		t.Errorf("Append result.Seq=0; want > 0")
	}
}

// Gateway data-plane round-trip: write via Append, read back via Get
// and Tail. Confirms the wiring across the public surface end-to-end.
func TestGateway_DataplaneRoundtrip(t *testing.T) {
	g := newGatewayForTest(t)

	// Append 3 items.
	for i := 0; i < 3; i++ {
		body := []byte(`{"v":` + string(rune('0'+i)) + `}`)
		if _, err := g.Append(Item{
			Scope:   "posts",
			ID:      "p-" + string(rune('0'+i)),
			Payload: json.RawMessage(body),
		}); err != nil {
			t.Fatalf("Append #%d: %v", i, err)
		}
	}

	// Get by id.
	item, hit := g.Get("posts", "p-0", 0)
	if !hit {
		t.Fatalf("Get(p-0): hit=false; want true")
	}
	if item.ID != "p-0" {
		t.Errorf("Get(p-0).ID=%q want p-0", item.ID)
	}

	// Tail returns newest first window.
	items, _, found := g.Tail("posts", 10, 0)
	if !found {
		t.Fatalf("Tail: scope not found")
	}
	if len(items) != 3 {
		t.Errorf("Tail: %d items; want 3", len(items))
	}

	// Stats reflects the writes.
	st := g.Stats()
	if st.TotalItems < 3 {
		t.Errorf("Stats.TotalItems=%d want >= 3 (3 user + N events)", st.TotalItems)
	}
}

// Gateway.Delete + DeleteUpTo + DeleteScope round-trip.
func TestGateway_DeletesRoundtrip(t *testing.T) {
	g := newGatewayForTest(t)

	for i := 0; i < 5; i++ {
		if _, err := g.Append(Item{
			Scope:   "posts",
			ID:      "p-" + string(rune('0'+i)),
			Payload: json.RawMessage(`{}`),
		}); err != nil {
			t.Fatalf("Append #%d: %v", i, err)
		}
	}

	// Delete by id.
	n, err := g.Delete("posts", "p-0", 0)
	if err != nil || n != 1 {
		t.Errorf("Delete(p-0): n=%d err=%v want (1, nil)", n, err)
	}

	// DeleteUpTo: remove items with seq <= 3 (so seq 1,2,3 — but 1 was
	// already gone via id delete above; items 2 and 3 remain).
	n, err = g.DeleteUpTo("posts", 3)
	if err != nil {
		t.Fatalf("DeleteUpTo: %v", err)
	}
	if n != 2 {
		t.Errorf("DeleteUpTo: deleted=%d want 2", n)
	}

	// DeleteScope removes the rest.
	n, ok, err := g.DeleteScope("posts")
	if err != nil {
		t.Fatalf("DeleteScope: %v", err)
	}
	if !ok || n != 2 {
		t.Errorf("DeleteScope: n=%d ok=%v want (2, true)", n, ok)
	}
}

// Wipe + Stats verify the gateway exposes the destructive op cleanly.
func TestGateway_WipeStats(t *testing.T) {
	g := newGatewayForTest(t)

	for i := 0; i < 3; i++ {
		_, _ = g.Append(Item{Scope: "posts", Payload: json.RawMessage(`{}`)})
	}
	pre := g.Stats()
	if pre.ScopeCount < 1 {
		t.Errorf("pre-wipe ScopeCount=%d want >= 1", pre.ScopeCount)
	}

	scopeCount, totalItems, _ := g.Wipe()
	if scopeCount < 1 || totalItems < 3 {
		t.Errorf("Wipe: scopeCount=%d totalItems=%d", scopeCount, totalItems)
	}

	post := g.Stats()
	// Reserved scopes always re-created post-wipe.
	if post.ScopeCount != 2 {
		t.Errorf("post-wipe ScopeCount=%d want 2 (only _events + _inbox)", post.ScopeCount)
	}
	if post.TotalItems != 0 {
		t.Errorf("post-wipe TotalItems=%d want 0", post.TotalItems)
	}
}

// ScopeList round-trip: pagination + prefix filter.
func TestGateway_ScopeList(t *testing.T) {
	g := newGatewayForTest(t)

	// Seed three user scopes.
	for _, scope := range []string{"alpha", "beta", "gamma"} {
		_, _ = g.Append(Item{Scope: scope, Payload: json.RawMessage(`{}`)})
	}

	// No filter — returns reserved + user scopes.
	entries, _ := g.ScopeList("", "", 100)
	if len(entries) < 5 {
		t.Errorf("ScopeList: %d entries; want >= 5 (3 user + 2 reserved)", len(entries))
	}

	// Prefix filter.
	entries, _ = g.ScopeList("a", "", 10)
	if len(entries) != 1 || entries[0].Scope != "alpha" {
		t.Errorf("ScopeList prefix=a: %v want [alpha]", entries)
	}
}

// Gateway.ScopeList passes its int limit straight to store.scopeList.
// Pre-fix, store.scopeList computed `truncated := len(refs) > limit`
// (true for any non-empty store when limit < 0) and then sliced
// `refs[:limit]`, which panics with "slice bounds out of range" on a
// negative index. The HTTP path's normalizeLimit blocks 0/negative
// with 400 before reaching the store, but a Go-API caller can pass
// any int — including an uninitialised one. The store-level guard
// (limit <= 0 → empty) makes Gateway.ScopeList safe at any input.
func TestGateway_ScopeList_NonPositiveLimitDoesNotPanic(t *testing.T) {
	g := newGatewayForTest(t)
	for _, scope := range []string{"alpha", "beta", "gamma"} {
		_, _ = g.Append(Item{Scope: scope, Payload: json.RawMessage(`{}`)})
	}

	for _, limit := range []int{-1, 0, -1000} {
		entries, more := g.ScopeList("", "", limit)
		if len(entries) != 0 {
			t.Errorf("limit=%d: entries=%d want 0", limit, len(entries))
		}
		if more {
			t.Errorf("limit=%d: more=true want false", limit)
		}
	}
}

// Every multi-item read on Gateway answers limit ≤ 0 the same way:
// empty slice, no panic, no "give me everything" surprise.
//
// Pre-fix the three methods diverged: Tail returned empty (defensive
// from day one), Head returned every matching item (limit=0 was
// treated as "no truncation"), ScopeList panicked on negative limit
// (truncated branch hit `refs[:limit]` directly). HTTP-callers never
// saw any of this because normalizeLimit rejected 0/negative with
// 400, but Go-API callers — addons, tests, a future drainer with an
// uninitialised cursor — got three different answers for the same
// input shape.
//
// This test pins the new uniformity: `Head`, `Tail`, `ScopeList`,
// each with limit ∈ {-1, 0}, all return empty + more=false.
func TestGateway_ReadMethods_NonPositiveLimitUniformlyEmpty(t *testing.T) {
	g := newGatewayForTest(t)
	for i := 0; i < 3; i++ {
		_, _ = g.Append(Item{
			Scope:   "posts",
			ID:      "p-" + string(rune('0'+i)),
			Payload: json.RawMessage(`{"v":1}`),
		})
	}
	_, _ = g.Append(Item{Scope: "other", Payload: json.RawMessage(`{}`)})

	for _, limit := range []int{-1, 0} {
		t.Run("limit="+strconv.Itoa(limit), func(t *testing.T) {
			items, more, found := g.Head("posts", 0, limit)
			if len(items) != 0 || more || !found {
				t.Errorf("Head: items=%d more=%v found=%v want (0,false,true)",
					len(items), more, found)
			}

			tailItems, hasMore, tailFound := g.Tail("posts", limit, 0)
			if len(tailItems) != 0 || hasMore || !tailFound {
				t.Errorf("Tail: items=%d more=%v found=%v want (0,false,true)",
					len(tailItems), hasMore, tailFound)
			}

			entries, slMore := g.ScopeList("", "", limit)
			if len(entries) != 0 || slMore {
				t.Errorf("ScopeList: entries=%d more=%v want (0,false)",
					len(entries), slMore)
			}
		})
	}
}
