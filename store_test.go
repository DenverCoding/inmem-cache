package scopecache

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

// --- Store --------------------------------------------------------------------

func TestStore_GetOrCreateScope_RequiresScope(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})
	if _, err := s.getOrCreateScope(""); err == nil {
		t.Fatal("expected error for empty scope")
	}
}

func TestStore_GetOrCreateScope_ReturnsSameBuffer(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})
	b1, _ := s.getOrCreateScope("x")
	b2, _ := s.getOrCreateScope("x")
	if b1 != b2 {
		t.Fatal("scope buffers should be identical")
	}
}

// NewStore(Config{}) + NewAPI(s, APIConfig{}) must produce a usable
// Store + API. Pre-fix the zero Config carried zero caps to every field,
// so any positive write failed with StoreFullError or worse — the public
// package was effectively dead-on-arrival for library users.
func TestNewStore_ZeroConfigUsesDefaults(t *testing.T) {
	s := NewStore(Config{})
	api := NewAPI(s, APIConfig{})
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	// A normal /append must just work.
	body := `{"scope":"smoke","id":"a","payload":{"v":1}}`
	code, _, raw := doRequest(t, mux, "POST", "/append", body)
	if code != 200 {
		t.Fatalf("/append on default-config Store: code=%d body=%s", code, raw)
	}

	// Cache caps must match the package-level compile-time defaults.
	if s.defaultMaxItems != ScopeMaxItems {
		t.Errorf("defaultMaxItems=%d want %d", s.defaultMaxItems, ScopeMaxItems)
	}
	if s.maxStoreBytes != int64(MaxStoreMiB)<<20 {
		t.Errorf("maxStoreBytes=%d want %d", s.maxStoreBytes, int64(MaxStoreMiB)<<20)
	}
	if s.maxItemBytes != int64(MaxItemBytes) {
		t.Errorf("maxItemBytes=%d want %d", s.maxItemBytes, int64(MaxItemBytes))
	}
	// HTTP caps must match the package-level compile-time defaults.
	if api.maxResponseBytes != int64(MaxResponseMiB)<<20 {
		t.Errorf("api.maxResponseBytes=%d want %d", api.maxResponseBytes, int64(MaxResponseMiB)<<20)
	}
}

// Config.WithDefaults treats <= 0 as "use default" (matching the
// standalone binary's env-var helpers) but leaves explicit positive
// values alone.
func TestConfig_WithDefaults(t *testing.T) {
	t.Run("zero fields fall back to defaults", func(t *testing.T) {
		got := Config{}.WithDefaults()
		if got.ScopeMaxItems != ScopeMaxItems {
			t.Errorf("ScopeMaxItems=%d", got.ScopeMaxItems)
		}
		if got.MaxStoreBytes != int64(MaxStoreMiB)<<20 {
			t.Errorf("MaxStoreBytes=%d", got.MaxStoreBytes)
		}
		if got.MaxItemBytes != int64(MaxItemBytes) {
			t.Errorf("MaxItemBytes=%d", got.MaxItemBytes)
		}
	})

	t.Run("positive fields preserved", func(t *testing.T) {
		in := Config{ScopeMaxItems: 5, MaxStoreBytes: 7, MaxItemBytes: 11}
		got := in.WithDefaults()
		if got.ScopeMaxItems != in.ScopeMaxItems ||
			got.MaxStoreBytes != in.MaxStoreBytes ||
			got.MaxItemBytes != in.MaxItemBytes {
			t.Errorf("positive Config mutated: got %+v want %+v", got, in)
		}
	})

	t.Run("negative treated as zero", func(t *testing.T) {
		// Same lenient policy as the standalone env-var helpers (n<=0 → default).
		// The Caddy module rejects negatives explicitly via validateConfig
		// before even calling NewStore, so this path only fires for direct
		// library callers — friendlier to fall back than to crash.
		got := Config{ScopeMaxItems: -1, MaxStoreBytes: -100}.WithDefaults()
		if got.ScopeMaxItems != ScopeMaxItems {
			t.Errorf("negative ScopeMaxItems not defaulted: %d", got.ScopeMaxItems)
		}
		if got.MaxStoreBytes != int64(MaxStoreMiB)<<20 {
			t.Errorf("negative MaxStoreBytes not defaulted: %d", got.MaxStoreBytes)
		}
	})
}

// APIConfig.WithDefaults mirrors Config.WithDefaults: numeric zeros fall
// back to compile-time defaults.
func TestAPIConfig_WithDefaults(t *testing.T) {
	t.Run("zero fields fall back to defaults", func(t *testing.T) {
		got := APIConfig{}.WithDefaults()
		if got.MaxResponseBytes != int64(MaxResponseMiB)<<20 {
			t.Errorf("MaxResponseBytes=%d", got.MaxResponseBytes)
		}
	})

	t.Run("positive fields preserved", func(t *testing.T) {
		in := APIConfig{MaxResponseBytes: 13}
		got := in.WithDefaults()
		if got.MaxResponseBytes != in.MaxResponseBytes {
			t.Errorf("positive APIConfig mutated: got %+v want %+v", got, in)
		}
	})
}

// Empty-scope spam — a malicious client with auto-create access
// (e.g., a /guarded tenant under v0.5.12+) creating thousands of
// empty scopes — must eventually hit the store-byte cap. Pre-v0.5.14
// this attack was unbounded: scope-buffer overhead was not charged
// against totalBytes, so 1M empty scopes consumed ~1 GiB while
// approx_store_mb stayed at 0. This test anchors the bound.
func TestStore_EmptyScopeSpam_HitsByteCap(t *testing.T) {
	// Cap big enough for reserved scopes (_log, _inbox) + ~10 attacker
	// scopes' worth of overhead, no item room.
	capBytes := reservedScopesOverhead + int64(scopeBufferOverhead)*10
	s := NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: capBytes, MaxItemBytes: 1 << 20})

	created := 0
	var lastErr error
	for i := 0; i < 100; i++ {
		_, err := s.getOrCreateScope(fmt.Sprintf("scope_%d", i))
		if err != nil {
			lastErr = err
			break
		}
		created++
	}

	if created >= 100 {
		t.Fatalf("created %d empty scopes without hitting cap (cap=%d, overhead=%d)",
			created, capBytes, scopeBufferOverhead)
	}
	if lastErr == nil {
		t.Fatal("expected StoreFullError after the cap is reached")
	}
	var stfe *StoreFullError
	if !errors.As(lastErr, &stfe) {
		t.Errorf("expected *StoreFullError, got %T: %v", lastErr, lastErr)
	}

	// totalBytes equals reserved + created × overhead — a clean
	// accounting of pure scope-buffer cost, no items in any scope.
	wantBytes := reservedScopesOverhead + int64(created)*scopeBufferOverhead
	if got := s.totalBytes.Load(); got != wantBytes {
		t.Errorf("totalBytes=%d want %d (reserved=%d + created=%d × overhead=%d)",
			got, wantBytes, reservedScopesOverhead, created, scopeBufferOverhead)
	}
}

// /delete_scope must release the per-scope overhead, not just the
// items. Without this, a workload that churns scopes (create, fill,
// delete, repeat) would slowly leak overhead and eventually 507 even
// when the store looks empty.
func TestStore_DeleteScope_ReleasesOverhead(t *testing.T) {
	// Cap fits reserved scopes (_log, _inbox) + 5 attacker scopes.
	capBytes := reservedScopesOverhead + int64(scopeBufferOverhead)*5
	s := NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: capBytes, MaxItemBytes: 1 << 20})

	// Fill the cap with empty user scopes — 5 scopes × overhead = remaining cap.
	for i := 0; i < 5; i++ {
		if _, err := s.getOrCreateScope(fmt.Sprintf("s_%d", i)); err != nil {
			t.Fatalf("getOrCreateScope %d: %v", i, err)
		}
	}
	// 6th must fail.
	if _, err := s.getOrCreateScope("s_overflow"); err == nil {
		t.Fatal("expected StoreFullError at scope #6")
	}

	// Delete one scope — its overhead is released.
	if _, ok := s.deleteScope("s_0"); !ok {
		t.Fatal("deleteScope s_0 reported miss")
	}

	// Now there's room for one more.
	if _, err := s.getOrCreateScope("s_replaced"); err != nil {
		t.Fatalf("getOrCreateScope after delete: %v", err)
	}

	// totalBytes is reserved-overhead + 5 user-scope overheads.
	if got, want := s.totalBytes.Load(), reservedScopesOverhead+int64(scopeBufferOverhead)*5; got != want {
		t.Errorf("totalBytes=%d want %d after delete+create cycle", got, want)
	}
}

func TestStore_GetScope_Miss(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})
	if _, ok := s.getScope("nope"); ok {
		t.Fatal("expected miss")
	}
}

// updateOne, deleteOne, deleteUpTo all share a "missing scope = (0, nil)"
// contract that handlers translate into hit:false / count:0 wire shape.
// Pin it explicitly so a future refactor cannot quietly change miss
// semantics to (0, ScopeNotFoundError) — handlers would then surface 409
// for what should be a 200 miss response.

func TestStore_updateOne_MissingScope(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})
	item := Item{Scope: "nope", ID: "x", Payload: json.RawMessage(`"v"`)}
	n, err := s.updateOne(item)
	if err != nil {
		t.Fatalf("err=%v; want nil", err)
	}
	if n != 0 {
		t.Errorf("updated=%d; want 0", n)
	}
}

func TestStore_deleteOne_MissingScope(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})
	n, err := s.deleteOne("nope", "x", 0)
	if err != nil {
		t.Fatalf("err=%v; want nil", err)
	}
	if n != 0 {
		t.Errorf("deleted=%d; want 0", n)
	}
}

func TestStore_deleteUpTo_MissingScope(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})
	n, err := s.deleteUpTo("nope", 100)
	if err != nil {
		t.Fatalf("err=%v; want nil", err)
	}
	if n != 0 {
		t.Errorf("deleted=%d; want 0", n)
	}
}

// head, tail, get, render report a missing scope by setting their
// found-flag to false. handleHead/Tail use it to pick writeItemsMiss
// vs writeItemsHit; handleGet/Render use it to write the miss
// response. A future change that returned (nil, true, false) for
// missing scopes would silently break /head and /tail by routing
// misses through writeItemsHit (different wire shape).

func TestStore_head_MissingScope(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})
	items, truncated, found := s.head("nope", 0, 10)
	if found {
		t.Error("found=true; want false")
	}
	if len(items) != 0 {
		t.Errorf("items=%v; want empty", items)
	}
	if truncated {
		t.Error("truncated=true; want false")
	}
}

func TestStore_tail_MissingScope(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})
	items, truncated, found := s.tail("nope", 10, 0)
	if found {
		t.Error("found=true; want false")
	}
	if len(items) != 0 {
		t.Errorf("items=%v; want empty", items)
	}
	if truncated {
		t.Error("truncated=true; want false")
	}
}

func TestStore_get_MissingScope(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})
	if _, found := s.get("nope", "x", 0); found {
		t.Error("found=true; want false")
	}
}

func TestStore_render_MissingScope(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})
	if _, found := s.render("nope", "x", 0); found {
		t.Error("found=true; want false")
	}
}

// render peels the renderBytes shortcut for JSON-string payloads — a
// store-level invariant that handleRender used to enforce inline.
// Pin it on the Store boundary now that the handler is dumb.
func TestStore_render_PeelsJSONString(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})
	_, err := s.appendOne(Item{Scope: "s", ID: "html", Payload: json.RawMessage(`"<h1>hi</h1>"`)})
	if err != nil {
		t.Fatalf("appendOne: %v", err)
	}
	body, found := s.render("s", "html", 0)
	if !found {
		t.Fatal("found=false; want true")
	}
	if got := string(body); got != "<h1>hi</h1>" {
		t.Errorf("body=%q; want %q (renderBytes shortcut should peel JSON-string layer)", got, "<h1>hi</h1>")
	}
}

// appendOne, upsertOne, counterAddOne must roll back the freshly-created
// scope when the item-byte reservation fails. Without rollback, every
// failed write to a new scope would leak scopeBufferOverhead onto the
// store-byte cap — a multi-tenant attacker could fill the cap with
// empty scopes and DoS legitimate writers (see ChatGPT bug review).

// bigPayload returns a JSON string payload of approximately the given
// total byte count. Used by the rollback tests to push past the
// store-byte cap without hitting the per-item cap.
func bigPayload(n int) json.RawMessage {
	buf := make([]byte, n+2)
	buf[0] = '"'
	for i := 1; i < n+1; i++ {
		buf[i] = 'a'
	}
	buf[n+1] = '"'
	return json.RawMessage(buf)
}

func TestStore_appendOne_RollsBackEmptyScopeOnFailure(t *testing.T) {
	// Cap = reserved-scope overhead (NewStore pre-creates _log/_inbox)
	// + 1 user-scope overhead + 50 bytes. appendOne reserves overhead
	// first, then the item-bytes reservation overflows — scope must be
	// rolled back so the overhead is released.
	capBytes := reservedScopesOverhead + int64(scopeBufferOverhead) + 50
	s := NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: capBytes, MaxItemBytes: 1 << 20})

	bigItem := Item{Scope: "victim", ID: "x", Payload: bigPayload(200)}
	_, err := s.appendOne(bigItem)
	var stfe *StoreFullError
	if !errors.As(err, &stfe) {
		t.Fatalf("expected StoreFullError, got %T: %v", err, err)
	}

	// After rollback, only the reserved scopes' overhead remains —
	// the rolled-back "victim" scope released its 1024-byte overhead.
	if got := s.totalBytes.Load(); got != reservedScopesOverhead {
		t.Errorf("totalBytes=%d after rolled-back appendOne; want %d (reserved-scope baseline)", got, reservedScopesOverhead)
	}
	if _, ok := s.getScope("victim"); ok {
		t.Errorf("scope 'victim' still present in s.scopes after rollback")
	}
}

func TestStore_upsertOne_RollsBackEmptyScopeOnFailure(t *testing.T) {
	capBytes := reservedScopesOverhead + int64(scopeBufferOverhead) + 50
	s := NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: capBytes, MaxItemBytes: 1 << 20})

	bigItem := Item{Scope: "victim", ID: "x", Payload: bigPayload(200)}
	_, _, err := s.upsertOne(bigItem)
	var stfe *StoreFullError
	if !errors.As(err, &stfe) {
		t.Fatalf("expected StoreFullError, got %T: %v", err, err)
	}

	if got := s.totalBytes.Load(); got != reservedScopesOverhead {
		t.Errorf("totalBytes=%d after rolled-back upsertOne; want %d", got, reservedScopesOverhead)
	}
	if _, ok := s.getScope("victim"); ok {
		t.Errorf("scope 'victim' still present in s.scopes after rollback")
	}
}

func TestStore_counterAddOne_RollsBackEmptyScopeOnFailure(t *testing.T) {
	// Cap = reserved-scope overhead + 1 user-scope overhead + 1 byte.
	// Even the smallest counter payload (a one-digit integer)
	// overflows on the item-bytes reservation after the per-scope
	// overhead has been claimed.
	capBytes := reservedScopesOverhead + int64(scopeBufferOverhead) + 1
	s := NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: capBytes, MaxItemBytes: 1 << 20})

	_, _, err := s.counterAddOne("victim", "c1", 42)
	var stfe *StoreFullError
	if !errors.As(err, &stfe) {
		t.Fatalf("expected StoreFullError, got %T: %v", err, err)
	}

	if got := s.totalBytes.Load(); got != reservedScopesOverhead {
		t.Errorf("totalBytes=%d after rolled-back counterAddOne; want %d", got, reservedScopesOverhead)
	}
	if _, ok := s.getScope("victim"); ok {
		t.Errorf("scope 'victim' still present in s.scopes after rollback")
	}
}

// appendOne loop with new scope names + oversized items must not leak
// per-scope overhead. Without the rollback this is the multi-tenant
// DoS path: ~100k requests fill the default 100 MiB cap with empty
// scopes, after which all legitimate writes 507.
func TestStore_appendOne_DoSPathStaysClean(t *testing.T) {
	capBytes := reservedScopesOverhead + int64(scopeBufferOverhead) + 50
	s := NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: capBytes, MaxItemBytes: 1 << 20})

	for i := 0; i < 1000; i++ {
		item := Item{Scope: fmt.Sprintf("attempt_%d", i), ID: "x", Payload: bigPayload(200)}
		_, err := s.appendOne(item)
		var stfe *StoreFullError
		if !errors.As(err, &stfe) {
			t.Fatalf("iter %d: expected StoreFullError, got %T: %v", i, err, err)
		}
	}

	var scopeCount int
	for shIdx := range s.shards {
		s.shards[shIdx].mu.RLock()
		scopeCount += len(s.shards[shIdx].scopes)
		s.shards[shIdx].mu.RUnlock()
	}
	// Reserved scopes (_log, _inbox) are pre-created and stay around;
	// only "attempt_*" scopes must have been rolled back.
	if scopeCount != len(reservedScopeNames) {
		t.Errorf("after 1000 failed appendOne calls, scopeCount=%d want %d (reserved baseline)", scopeCount, len(reservedScopeNames))
	}
	if got := s.totalBytes.Load(); got != reservedScopesOverhead {
		t.Errorf("totalBytes=%d after 1000 rolled-back appendOne calls; want %d (reserved-scope baseline)", got, reservedScopesOverhead)
	}
}

// appendOne must NOT roll back the scope when a concurrent caller has
// successfully committed an item to the same scope between our create
// and our cleanup. The cleanup helper checks len(buf.items)==0 under
// buf.mu, so a successful concurrent write keeps the scope alive.
//
// Race-detector-friendly: pairs of goroutines per scope — one tries an
// oversized write, the other a small write. No empty scopes may leak.
func TestStore_appendOne_ConcurrentSuccessSurvivesCleanup(t *testing.T) {
	const N = 50
	// Cap room for N small items + their scope overheads, plus slack
	// for the oversized writers' interleaving overhead-reservations.
	capBytes := int64(N) * (int64(scopeBufferOverhead) + 256)
	s := NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: capBytes, MaxItemBytes: 1 << 20})

	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(2)
		scope := fmt.Sprintf("shared_%d", i)
		go func(scope string) {
			defer wg.Done()
			big := Item{Scope: scope, ID: "big", Payload: bigPayload(int(capBytes))}
			_, _ = s.appendOne(big)
		}(scope)
		go func(scope string) {
			defer wg.Done()
			small := Item{Scope: scope, ID: "small", Payload: json.RawMessage(`"hi"`)}
			_, _ = s.appendOne(small)
		}(scope)
	}
	wg.Wait()

	for shIdx := range s.shards {
		s.shards[shIdx].mu.RLock()
		for name, buf := range s.shards[shIdx].scopes {
			// Reserved scopes are intentionally empty at this point
			// (no test writes target them); skip the leak check.
			if isReservedScope(name) {
				continue
			}
			buf.mu.Lock()
			empty := len(buf.items) == 0
			buf.mu.Unlock()
			if empty {
				t.Errorf("empty scope %q leaked through concurrent cleanup", name)
			}
		}
		s.shards[shIdx].mu.RUnlock()
	}
}

// TestStore_appendOne_DetachRaceErrorContract pins the error contract for
// the race between a failed create+rollback and a concurrent fast-path
// writer on the same scope. cleanupIfEmptyAndUnused detaches the buffer
// it created when its caller's item-reservation failed; a writer that
// grabbed buf via the RLock fast-path before that detach lands either
// commits its item (saving the scope) or wakes up on a detached buf and
// must see exactly *ScopeDetachedError.
//
// Two legal outcomes for B (the small writer):
//
//	Case 1 — B grabs buf.mu before A's cleanup. B commits its item;
//	         A's cleanup observes len(items) > 0 and aborts. B's err = nil.
//
//	Case 2 — A's cleanup grabs buf.mu first, marks detached, releases.
//	         B then grabs buf.mu, sees b.detached, returns *ScopeDetachedError
//	         without reserving bytes (the detach check is the first thing
//	         after the lock acquisition).
//
// Anything else from B — *StoreFullError, *ScopeFullError, a raw
// errors.New — would surface to the handler as the wrong status class and
// break the documented detach contract that /delete_scope, /wipe and
// /rebuild also rely on.
//
// The Errorf on caseBDetached == 0 guards against a future refactor that
// stealthily closes the race window: without exercising Case 2 the test
// is silently meaningless. 5000 iterations matches the cadence of the
// other race-window tests in bulk_test.go.
func TestStore_appendOne_DetachRaceErrorContract(t *testing.T) {
	const iterations = 5000
	// Cap fits reserved scopes (_log, _inbox) + 1 user-scope overhead + slack.
	capBytes := reservedScopesOverhead + int64(scopeBufferOverhead) + 1000

	var caseACommit, caseBDetached, unexpected int

	for iter := 0; iter < iterations; iter++ {
		s := NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: capBytes, MaxItemBytes: 1 << 20})

		var bErr error
		var wg sync.WaitGroup
		wg.Add(2)

		// A: oversized — fails on item-reservation, triggers cleanupIfEmptyAndUnused.
		go func() {
			defer wg.Done()
			big := Item{Scope: "shared", ID: "big", Payload: bigPayload(2000)}
			_, _ = s.appendOne(big)
		}()

		// B: small — must observe either nil (Case 1) or *ScopeDetachedError (Case 2).
		go func() {
			defer wg.Done()
			small := Item{Scope: "shared", ID: "small", Payload: json.RawMessage(`"hi"`)}
			_, bErr = s.appendOne(small)
		}()
		wg.Wait()

		var sde *ScopeDetachedError
		switch {
		case bErr == nil:
			caseACommit++
		case errors.As(bErr, &sde):
			caseBDetached++
		default:
			unexpected++
			if unexpected <= 5 {
				t.Errorf("iter %d: B got unexpected err type %T: %v", iter, bErr, bErr)
			}
		}

		// Bytes invariant must hold whichever branch fired. Reuses the
		// helper from bulk_test.go (same package).
		assertBytesInvariant(t, s, iter, "detach-race")
	}

	if unexpected > 0 {
		t.Errorf("total unexpected error types from B: %d / %d", unexpected, iterations)
	}
	if caseBDetached == 0 {
		t.Errorf("Case 2 (cleanup-before-commit) never fired across %d iterations — "+
			"the race window may have closed; this test is no longer exercising the path",
			iterations)
	}
	t.Logf("outcomes: Case 1 (commit-before-cleanup) = %d, Case 2 (detach) = %d", caseACommit, caseBDetached)
}

func TestStore_EnsureScope_CreatesEmpty(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})
	buf := s.ensureScope("_counters_count_calls")
	if buf == nil {
		t.Fatal("ensureScope returned nil")
	}
	if got, ok := s.getScope("_counters_count_calls"); !ok || got != buf {
		t.Fatal("scope not registered or different buffer returned")
	}
	if n := len(buf.items); n != 0 {
		t.Errorf("new scope should be empty, got %d items", n)
	}
}

// ensureScope must charge scopeBufferOverhead against totalBytes so a
// later /admin /delete_scope releases exactly what was reserved.
// Without this, deleteScope's unconditional `-(scopeBytes + overhead)`
// would underflow totalBytes by 1024 bytes per cycle on these
// internal counter scopes — bounded, but a real invariant break.
func TestStore_EnsureScope_ReservesOverheadAndRoundTrips(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})

	before := s.totalBytes.Load()
	buf := s.ensureScope("_counters_count_calls")
	if buf == nil {
		t.Fatal("ensureScope returned nil with ample cap")
	}
	if got := s.totalBytes.Load() - before; got != int64(scopeBufferOverhead) {
		t.Fatalf("ensureScope reserved %d bytes; want %d (scopeBufferOverhead)", got, scopeBufferOverhead)
	}

	if _, ok := s.deleteScope("_counters_count_calls"); !ok {
		t.Fatal("deleteScope reported miss on the freshly ensured scope")
	}
	if got := s.totalBytes.Load(); got != before {
		t.Errorf("totalBytes drift after ensureScope+deleteScope round-trip: got=%d want=%d", got, before)
	}
}

// On cap exhaustion ensureScope must return nil, not panic and not
// double-charge — guardedIncrementCounters is best-effort and skips
// silently on nil, so observability counters never block legitimate
// /guarded calls.
func TestStore_EnsureScope_NilOnCapExhausted(t *testing.T) {
	// Cap = 100 bytes, well below scopeBufferOverhead (1024).
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100, MaxItemBytes: 1 << 20})

	if buf := s.ensureScope("_counters_count_calls"); buf != nil {
		t.Errorf("ensureScope returned %p with cap below overhead; want nil", buf)
	}
	if got := s.totalBytes.Load(); got != 0 {
		t.Errorf("totalBytes=%d after failed ensureScope; want 0 (no leak)", got)
	}
	if _, ok := s.getScope("_counters_count_calls"); ok {
		t.Errorf("ensureScope leaked the scope into s.scopes despite cap-fail")
	}
}

func TestStore_EnsureScope_Idempotent(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})
	b1 := s.ensureScope("_counters_count_calls")
	b2 := s.ensureScope("_counters_count_calls")
	if b1 != b2 {
		t.Fatal("repeat ensureScope should return same buffer")
	}
}

// ensureScope under concurrent access must not double-create or panic.
func TestStore_EnsureScope_Concurrent(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})

	const N = 50
	bufs := make([]*scopeBuffer, N)
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(idx int) {
			defer wg.Done()
			bufs[idx] = s.ensureScope("_counters_count_calls")
		}(i)
	}
	wg.Wait()

	first := bufs[0]
	for i, b := range bufs {
		if b != first {
			t.Errorf("ensureScope returned different buffer at idx %d", i)
		}
	}
}

// ensureScope on already-existing scope must not wipe its items.
func TestStore_EnsureScope_PreservesExisting(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})
	buf, _ := s.getOrCreateScope("_counters_count_calls")
	if _, _, err := buf.counterAdd("_counters_count_calls", "cap1", 42); err != nil {
		t.Fatalf("counterAdd: %v", err)
	}
	again := s.ensureScope("_counters_count_calls")
	if again != buf {
		t.Fatal("ensureScope returned different buffer")
	}
	if got, _, err := again.counterAdd("_counters_count_calls", "cap1", 0); err != nil {
		// counterAdd with by=0 isn't allowed by /counter_add validation, but at
		// the buffer level it should still let us read the existing value via
		// a noop add — except that the buffer rejects zero too. So instead
		// just check items length.
		_ = got
		_ = err
	}
	if n := len(again.items); n != 1 {
		t.Errorf("expected 1 existing item preserved, got %d", n)
	}
}

func TestStore_DeleteScope(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})
	buf, _ := s.getOrCreateScope("x")
	_, _ = buf.appendItem(newItem("x", "a", nil))
	_, _ = buf.appendItem(newItem("x", "b", nil))

	n, ok := s.deleteScope("x")
	if !ok || n != 2 {
		t.Fatalf("deleteScope: ok=%v n=%d", ok, n)
	}
	if _, found := s.getScope("x"); found {
		t.Fatal("scope should be gone")
	}

	n, ok = s.deleteScope("missing")
	if ok || n != 0 {
		t.Fatalf("deleteScope(missing): ok=%v n=%d", ok, n)
	}

	// Empty scope is a shape bug from the caller — the store refuses it up
	// front rather than walking the map for a key that cannot exist.
	n, ok = s.deleteScope("")
	if ok || n != 0 {
		t.Fatalf("deleteScope(\"\"): ok=%v n=%d", ok, n)
	}
}

// Orphan deletes (deleteByID, deleteBySeq, deleteUpToSeq) must surface
// *ScopeDetachedError rather than silently mutate a buffer no reader
// can reach. Pre-fix the delete methods skipped the detached check, so
// a /delete handler that grabbed buf before /delete_scope (or /wipe,
// or /rebuild) detached it would mutate the orphan and return
// hit:true,deleted_count:1 to the client — meanwhile the live store
// either has no such scope or has a freshly-created one with the item
// still present. The fix returns *ScopeDetachedError; the handlers
// surface it as 409 Conflict, matching every other write path.
func TestScopeBuffer_DeletesDetectDetached(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})

	buf, _ := s.getOrCreateScope("s")
	it1, _ := buf.appendItem(newItem("s", "a", nil))
	_, _ = buf.appendItem(newItem("s", "b", nil))
	_, _ = buf.appendItem(newItem("s", "c", nil))

	// Detach by deleting the scope. buf is now an orphan.
	if _, ok := s.deleteScope("s"); !ok {
		t.Fatal("deleteScope reported miss on a scope that exists")
	}

	for _, tc := range []struct {
		name string
		fn   func() (int, error)
	}{
		{"deleteByID", func() (int, error) { return buf.deleteByID("a") }},
		{"deleteBySeq", func() (int, error) { return buf.deleteBySeq(it1.Seq) }},
		{"deleteUpToSeq", func() (int, error) { return buf.deleteUpToSeq(99) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, err := tc.fn()
			var sde *ScopeDetachedError
			if !errors.As(err, &sde) {
				t.Fatalf("got err=%v, want *ScopeDetachedError", err)
			}
			if n != 0 {
				t.Errorf("returned n=%d on detached buffer; want 0", n)
			}
		})
	}

	// Counter must remain at the reserved-scope baseline — orphan deletes
	// must not leak into totalBytes (the b.store guard exists, but with
	// the detached check we never reach it).
	if got := s.totalBytes.Load(); got != reservedScopesOverhead {
		t.Errorf("totalBytes=%d want %d (reserved-scope baseline; orphan deletes leaked into counter)", got, reservedScopesOverhead)
	}
}

// --- store-level byte budget --------------------------------------------------

// Byte-cap is the aggregate approxItemSize across all scopes; writes that
// would push the running total past maxStoreBytes are rejected with
// StoreFullError. State must stay untouched on rejection — same contract as
// the per-scope ScopeFullError.
func TestStore_Append_RejectsAtByteCap(t *testing.T) {
	itemSize := approxItemSize(newItem("s", "", nil))
	// Cap fits reserved scopes (_log, _inbox) + 1 user-scope overhead + 3 items.
	capBytes := reservedScopesOverhead + int64(scopeBufferOverhead) + itemSize*3

	s := NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: capBytes, MaxItemBytes: 1 << 20})
	buf, _ := s.getOrCreateScope("s")

	for i := 0; i < 3; i++ {
		if _, err := buf.appendItem(newItem("s", "", nil)); err != nil {
			t.Fatalf("append %d within cap: %v", i, err)
		}
	}

	_, err := buf.appendItem(newItem("s", "", nil))
	if err == nil {
		t.Fatal("expected StoreFullError when append would exceed byte cap")
	}
	var stfe *StoreFullError
	if !errors.As(err, &stfe) {
		t.Fatalf("expected *StoreFullError, got %T: %v", err, err)
	}
	if stfe.Cap != capBytes {
		t.Fatalf("Cap=%d want %d", stfe.Cap, capBytes)
	}
	if stfe.AddedBytes != itemSize {
		t.Fatalf("AddedBytes=%d want %d", stfe.AddedBytes, itemSize)
	}

	if len(buf.items) != 3 {
		t.Fatalf("rejected write mutated buffer: len=%d want 3", len(buf.items))
	}
	if got, want := s.totalBytes.Load(), reservedScopesOverhead+int64(scopeBufferOverhead)+itemSize*3; got != want {
		t.Fatalf("totalBytes=%d want %d after rejected append (reserved + user-overhead + 3 items)", got, want)
	}
}

// Freeing capacity via /delete must let subsequent appends succeed: the
// byte counter has to drop by the removed item's size or the store drifts
// into a permanently "full" state.
func TestStore_Delete_FreesBytes(t *testing.T) {
	itemSize := approxItemSize(newItem("s", "a", nil))
	// Cap fits reserved + 1 user-scope overhead + 2 items.
	capBytes := reservedScopesOverhead + int64(scopeBufferOverhead) + itemSize*2

	s := NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: capBytes, MaxItemBytes: 1 << 20})
	buf, _ := s.getOrCreateScope("s")
	if _, err := buf.appendItem(newItem("s", "a", nil)); err != nil {
		t.Fatalf("append a: %v", err)
	}
	if _, err := buf.appendItem(newItem("s", "b", nil)); err != nil {
		t.Fatalf("append b: %v", err)
	}

	// At cap now — a third append must fail.
	if _, err := buf.appendItem(newItem("s", "c", nil)); err == nil {
		t.Fatal("expected StoreFullError at cap")
	}

	if n, _ := buf.deleteByID("a"); n != 1 {
		t.Fatalf("deleteByID a: n=%d want 1", n)
	}

	// After freeing one item's worth, a new append must succeed.
	if _, err := buf.appendItem(newItem("s", "c", nil)); err != nil {
		t.Fatalf("append c after delete: %v", err)
	}
	if got, want := s.totalBytes.Load(), reservedScopesOverhead+int64(scopeBufferOverhead)+itemSize*2; got != want {
		t.Fatalf("totalBytes=%d want %d after delete+append", got, want)
	}
}

func TestStore_DeleteUpTo_FreesBytes(t *testing.T) {
	itemSize := approxItemSize(newItem("s", "", nil))
	capBytes := reservedScopesOverhead + int64(scopeBufferOverhead) + itemSize*3

	s := NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: capBytes, MaxItemBytes: 1 << 20})
	buf, _ := s.getOrCreateScope("s")
	for i := 0; i < 3; i++ {
		if _, err := buf.appendItem(newItem("s", "", nil)); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	if n, _ := buf.deleteUpToSeq(2); n != 2 {
		t.Fatalf("deleteUpToSeq: n=%d want 2", n)
	}

	// Two items freed, so room for two more.
	for i := 0; i < 2; i++ {
		if _, err := buf.appendItem(newItem("s", "", nil)); err != nil {
			t.Fatalf("append after drain %d: %v", i, err)
		}
	}
	if got, want := s.totalBytes.Load(), reservedScopesOverhead+int64(scopeBufferOverhead)+itemSize*3; got != want {
		t.Fatalf("totalBytes=%d want %d", got, want)
	}
}

func TestStore_DeleteScope_FreesBytes(t *testing.T) {
	itemSize := approxItemSize(newItem("s", "", nil))
	capBytes := reservedScopesOverhead + int64(scopeBufferOverhead) + itemSize*4

	s := NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: capBytes, MaxItemBytes: 1 << 20})
	buf, _ := s.getOrCreateScope("s")
	for i := 0; i < 4; i++ {
		if _, err := buf.appendItem(newItem("s", "", nil)); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	n, ok := s.deleteScope("s")
	if !ok || n != 4 {
		t.Fatalf("deleteScope: ok=%v n=%d", ok, n)
	}
	// Only the reserved-scope baseline remains (the user scope and its
	// 4 items were all freed).
	if got := s.totalBytes.Load(); got != reservedScopesOverhead {
		t.Fatalf("totalBytes=%d want %d after deleteScope (reserved baseline)", got, reservedScopesOverhead)
	}
}

// /update that grows the payload must reserve the delta — a grow past the
// byte cap returns StoreFullError without mutating the stored item.
func TestStore_Update_RejectsGrowAtByteCap(t *testing.T) {
	small := newItem("s", "a", map[string]interface{}{"v": 1})
	// Cap fits reserved + 1 user-scope overhead + small item + tiny slack
	// (no room for the large replacement payload).
	capBytes := reservedScopesOverhead + int64(scopeBufferOverhead) + approxItemSize(small) + 8

	s := NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: capBytes, MaxItemBytes: 1 << 20})
	buf, _ := s.getOrCreateScope("s")
	if _, err := buf.appendItem(small); err != nil {
		t.Fatalf("append small: %v", err)
	}

	// A payload with 100 extra bytes overflows the tiny headroom.
	bigPayload, _ := json.Marshal(map[string]interface{}{
		"v":    1,
		"blob": "x_________________________________________________________________________________________________",
	})
	n, err := buf.updateByID("a", bigPayload)
	if err == nil {
		t.Fatal("expected StoreFullError on grow past cap")
	}
	var stfe *StoreFullError
	if !errors.As(err, &stfe) {
		t.Fatalf("expected *StoreFullError, got %T: %v", err, err)
	}
	if n != 0 {
		t.Fatalf("updated=%d want 0 on reject", n)
	}
	// Payload must still be the small original.
	got, _ := buf.getByID("a")
	if string(got.Payload) != string(small.Payload) {
		t.Fatalf("payload changed despite reject: %s", string(got.Payload))
	}
}

// reserveBytes is the atomic admission primitive. Positive deltas honor the
// cap; negative deltas always succeed. A CAS loop isn't directly observable,
// so this test just validates the return-value contract.
func TestStore_ReserveBytes_RejectsPositiveOverCap(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: 100, MaxItemBytes: 1 << 20})
	if ok, _, _ := s.reserveBytes(80); !ok {
		t.Fatal("reserve 80/100 should succeed")
	}
	ok, current, cap := s.reserveBytes(30)
	if ok {
		t.Fatal("reserve 30 on top of 80 should fail (cap 100)")
	}
	if current != 80 {
		t.Fatalf("current=%d want 80 (unchanged on failed reserve)", current)
	}
	if cap != 100 {
		t.Fatalf("cap=%d want 100", cap)
	}
	if ok, _, _ := s.reserveBytes(-50); !ok {
		t.Fatal("negative reserve (release) must always succeed")
	}
	if got := s.totalBytes.Load(); got != 30 {
		t.Fatalf("totalBytes=%d want 30 after 80 + (-50)", got)
	}
}

// deleteScope must not race with an /append that obtained the buf pointer
// before the scope was removed from the map. Under the old RLock-snapshot
// pattern, the appended item's bytes leaked into s.totalBytes after the
// subtract happened on a stale value. This test drives many rounds of
// parallel append/delete on the same scope and asserts the final counter
// matches the items that survived in s.scopes.
func TestStore_DeleteScope_RaceWithAppend(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 1000, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})

	const rounds = 200
	for i := 0; i < rounds; i++ {
		scope := "race"
		buf, _ := s.getOrCreateScope(scope)
		// Prime with one item so deleteScope has real bytes to subtract.
		if _, err := buf.appendItem(newItem(scope, "", nil)); err != nil {
			t.Fatalf("prime append: %v", err)
		}

		done := make(chan struct{}, 2)
		go func() {
			buf, _ := s.getOrCreateScope(scope)
			_, _ = buf.appendItem(newItem(scope, "", nil))
			done <- struct{}{}
		}()
		go func() {
			_, _ = s.deleteScope(scope)
			done <- struct{}{}
		}()
		<-done
		<-done
	}

	// Final invariant: s.totalBytes == sum(buf.bytes) + scope-overhead
	// per live scope. Any ghost bytes from the race would inflate
	// totalBytes above the sum.
	var liveBytes int64
	live := s.listScopes()
	for _, buf := range live {
		buf.mu.RLock()
		liveBytes += buf.bytes
		buf.mu.RUnlock()
	}
	expected := liveBytes + int64(len(live))*scopeBufferOverhead
	if got := s.totalBytes.Load(); got != expected {
		t.Fatalf("totalBytes=%d but live scopes hold %d bytes + %d overhead = %d (ghost bytes from race)",
			got, liveBytes, int64(len(live))*scopeBufferOverhead, expected)
	}
}

// --- /stats counter invariants -----------------------------------------------

// assertStatsCountersInvariant walks every shard and verifies the
// invariants that make the O(1) /stats shape correct:
//
//   - s.totalItems  == Σ len(buf.items) over every live scope buffer
//   - s.scopeCount  == Σ len(sh.scopes) over every shard
//   - s.lastWriteTS >= max(buf.lastWriteTS) over every live scope buffer
//
// Forgetting to update one of these counters from a write/delete/bulk
// path silently corrupts /stats output without affecting any cache
// behaviour — exactly the class of bug a routine assertion catches and
// a hand-test never finds. Call this after any sequence of mutations.
//
// The lastWriteTS check is `>=` not `==` because store-level
// destructive paths (deleteScope, wipe) bump s.lastWriteTS without
// leaving any per-scope b.lastWriteTS behind (the scope is gone), so
// after such an event the store-wide value is strictly greater than
// any surviving scope's.
//
// Takes per-scope read locks during the walk; safe to call from tests
// that have other goroutines mutating the store, but any concurrent
// mutation observed mid-walk is the caller's tolerance to weigh.
func assertStatsCountersInvariant(t *testing.T, s *Store, ctx string) {
	t.Helper()

	var sumItems int64
	var sumScopes int64
	var maxScopeLastWriteTS int64
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.RLock()
		sumScopes += int64(len(sh.scopes))
		for _, buf := range sh.scopes {
			buf.mu.RLock()
			sumItems += int64(len(buf.items))
			if buf.lastWriteTS > maxScopeLastWriteTS {
				maxScopeLastWriteTS = buf.lastWriteTS
			}
			buf.mu.RUnlock()
		}
		sh.mu.RUnlock()
	}

	if got := s.totalItems.Load(); got != sumItems {
		t.Errorf("[%s] totalItems=%d but Σ len(buf.items)=%d (counter drift)", ctx, got, sumItems)
	}
	if got := s.scopeCount.Load(); got != sumScopes {
		t.Errorf("[%s] scopeCount=%d but Σ len(sh.scopes)=%d (counter drift)", ctx, got, sumScopes)
	}
	if got := s.lastWriteTS.Load(); got < maxScopeLastWriteTS {
		t.Errorf("[%s] lastWriteTS=%d but max(buf.lastWriteTS)=%d (store-wide tick lags scope)", ctx, got, maxScopeLastWriteTS)
	}
}

// TestStore_StatsCounters_Invariant_AcrossPaths drives every write/delete/
// bulk path that mutates totalItems or scopeCount and re-asserts the
// invariant after each step. If a future change forgets to update one
// counter on one path, the assertion fails with the path's name in the
// context string — much friendlier than chasing a "scope_count=42 but
// got=43" report from production.
func TestStore_StatsCounters_Invariant_AcrossPaths(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})
	assertStatsCountersInvariant(t, s, "fresh store")

	// appendOne — single-item write into a freshly created scope.
	if _, err := s.appendOne(Item{Scope: "a", ID: "1", Payload: json.RawMessage(`"v"`)}); err != nil {
		t.Fatalf("appendOne: %v", err)
	}
	assertStatsCountersInvariant(t, s, "after appendOne (new scope)")

	if _, err := s.appendOne(Item{Scope: "a", ID: "2", Payload: json.RawMessage(`"v"`)}); err != nil {
		t.Fatalf("appendOne 2: %v", err)
	}
	assertStatsCountersInvariant(t, s, "after appendOne (existing scope)")

	// upsertOne create branch.
	if _, _, err := s.upsertOne(Item{Scope: "a", ID: "3", Payload: json.RawMessage(`"v"`)}); err != nil {
		t.Fatalf("upsertOne create: %v", err)
	}
	assertStatsCountersInvariant(t, s, "after upsertOne (create)")

	// upsertOne replace branch — must NOT change totalItems.
	if _, _, err := s.upsertOne(Item{Scope: "a", ID: "3", Payload: json.RawMessage(`"v2"`)}); err != nil {
		t.Fatalf("upsertOne replace: %v", err)
	}
	assertStatsCountersInvariant(t, s, "after upsertOne (replace)")

	// counterAddOne create branch.
	if _, _, err := s.counterAddOne("a", "ctr", 5); err != nil {
		t.Fatalf("counterAddOne create: %v", err)
	}
	assertStatsCountersInvariant(t, s, "after counterAddOne (create)")

	// counterAddOne increment branch — must NOT change totalItems.
	if _, _, err := s.counterAddOne("a", "ctr", 3); err != nil {
		t.Fatalf("counterAddOne increment: %v", err)
	}
	assertStatsCountersInvariant(t, s, "after counterAddOne (increment)")

	// updateOne — must NOT change totalItems.
	if _, err := s.updateOne(Item{Scope: "a", ID: "1", Payload: json.RawMessage(`"new"`)}); err != nil {
		t.Fatalf("updateOne: %v", err)
	}
	assertStatsCountersInvariant(t, s, "after updateOne")

	// deleteOne by id — drops one item.
	if _, err := s.deleteOne("a", "1", 0); err != nil {
		t.Fatalf("deleteOne: %v", err)
	}
	assertStatsCountersInvariant(t, s, "after deleteOne")

	// Build up a second scope so deleteUpTo has multiple items to drop.
	for i := 0; i < 5; i++ {
		if _, err := s.appendOne(Item{Scope: "b", Payload: json.RawMessage(`"v"`)}); err != nil {
			t.Fatalf("appendOne b: %v", err)
		}
	}
	assertStatsCountersInvariant(t, s, "after building scope b")

	// deleteUpTo — drops the first 3 items in b. lastSeq is 5; cut at 3.
	if n, err := s.deleteUpTo("b", 3); err != nil || n != 3 {
		t.Fatalf("deleteUpTo n=%d err=%v want n=3", n, err)
	}
	assertStatsCountersInvariant(t, s, "after deleteUpTo")

	// deleteScope — drops scope b entirely (2 items + 1 scope).
	if n, ok := s.deleteScope("b"); !ok || n != 2 {
		t.Fatalf("deleteScope n=%d ok=%v want n=2 ok=true", n, ok)
	}
	assertStatsCountersInvariant(t, s, "after deleteScope")

	// ensureScope — pure scope-create, no items.
	_ = s.ensureScope("_ctrl")
	assertStatsCountersInvariant(t, s, "after ensureScope")

	// replaceScopes (the path /warm uses) — replaces existing scope a
	// AND creates a brand new scope c. Item delta on a goes from
	// (whatever's left) to 1; c goes from 0 to 2.
	grouped := map[string][]Item{
		"a": {{Scope: "a", Payload: json.RawMessage(`"warmed"`)}},
		"c": {
			{Scope: "c", Payload: json.RawMessage(`"v1"`)},
			{Scope: "c", Payload: json.RawMessage(`"v2"`)},
		},
	}
	if _, err := s.replaceScopes(grouped); err != nil {
		t.Fatalf("replaceScopes: %v", err)
	}
	assertStatsCountersInvariant(t, s, "after replaceScopes")

	// rebuildAll — wipes everything and rebuilds.
	rebuild := map[string][]Item{
		"x": {
			{Scope: "x", Payload: json.RawMessage(`"v"`)},
			{Scope: "x", Payload: json.RawMessage(`"v"`)},
		},
		"y": {{Scope: "y", Payload: json.RawMessage(`"v"`)}},
	}
	if _, _, err := s.rebuildAll(rebuild); err != nil {
		t.Fatalf("rebuildAll: %v", err)
	}
	assertStatsCountersInvariant(t, s, "after rebuildAll")
	if got := s.totalItems.Load(); got != 3 {
		t.Errorf("after rebuildAll: totalItems=%d want 3", got)
	}
	// 2 user scopes (x, y) + 2 reserved scopes (post-rebuild init).
	wantScopes := int64(2 + len(reservedScopeNames))
	if got := s.scopeCount.Load(); got != wantScopes {
		t.Errorf("after rebuildAll: scopeCount=%d want %d", got, wantScopes)
	}

	// wipe — items back to 0, but reserved scopes are immediately re-created.
	_, _, _ = s.wipe()
	assertStatsCountersInvariant(t, s, "after wipe")
	if got := s.totalItems.Load(); got != 0 {
		t.Errorf("after wipe: totalItems=%d want 0", got)
	}
	if got := s.scopeCount.Load(); got != int64(len(reservedScopeNames)) {
		t.Errorf("after wipe: scopeCount=%d want %d (reserved baseline)", got, len(reservedScopeNames))
	}
}

// TestStore_StatsCounters_Invariant_DoSCleanup ensures the
// cleanupIfEmptyAndUnused rollback path keeps scopeCount in sync. The
// flow: appendOne creates a new scope, the per-item byte reservation
// fails, and the empty scope is rolled back. scopeCount must end at 0.
func TestStore_StatsCounters_Invariant_DoSCleanup(t *testing.T) {
	// MaxItemBytes large enough to allow scope creation but small enough
	// that the item itself fails on the per-item cap. Actually easier:
	// fill the store cap with overhead first, then try one more append.
	s := NewStore(Config{
		ScopeMaxItems: 10,
		// Cap fits reserved scopes (_log, _inbox) + one user scope + a tiny item.
		MaxStoreBytes: reservedScopesOverhead + int64(scopeBufferOverhead) + 100,
		MaxItemBytes:  1 << 20,
	})
	assertStatsCountersInvariant(t, s, "fresh DoS-bounded store")

	// First append fits within cap.
	if _, err := s.appendOne(Item{Scope: "first", Payload: json.RawMessage(`"v"`)}); err != nil {
		t.Fatalf("first appendOne: %v", err)
	}
	assertStatsCountersInvariant(t, s, "after first appendOne")
	scopeCountBefore := s.scopeCount.Load()
	totalItemsBefore := s.totalItems.Load()

	// Second append into a NEW scope: scope-overhead reservation may
	// succeed or fail depending on remaining cap; either way the
	// invariant must hold after the call returns.
	_, err := s.appendOne(Item{
		Scope:   "second",
		Payload: json.RawMessage(`"this payload is large enough to push the store over the cap easily"`),
	})
	assertStatsCountersInvariant(t, s, "after second appendOne (likely DoS-rejected)")

	if err == nil {
		// Append unexpectedly succeeded — fine, just verify counters
		// agree with new state.
		t.Logf("second appendOne succeeded (cap had room): scopeCount=%d totalItems=%d",
			s.scopeCount.Load(), s.totalItems.Load())
	} else {
		// Rejected: the rollback must have restored scopeCount and
		// totalItems to their pre-call values.
		if got := s.scopeCount.Load(); got != scopeCountBefore {
			t.Errorf("scopeCount drifted after rejected append: %d -> %d", scopeCountBefore, got)
		}
		if got := s.totalItems.Load(); got != totalItemsBefore {
			t.Errorf("totalItems drifted after rejected append: %d -> %d", totalItemsBefore, got)
		}
	}
}

// TestStore_LastWriteTS_StartsAtZero pins the "freshness sentinel"
// contract: a fresh store with no writes must report 0, so a polling
// client can use 0 as the unambiguous "I've never seen this cache
// before" marker.
func TestStore_LastWriteTS_StartsAtZero(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})
	if got := s.lastWriteTS.Load(); got != 0 {
		t.Errorf("fresh store lastWriteTS=%d want 0", got)
	}
}

// TestNewStore_PreCreatesReservedScopes pins the boot-time init contract:
// after NewStore, every entry in reservedScopeNames must exist as a
// scopeBuffer with zero items, lastWriteTS=0 (bootstrap is not activity),
// and the store-wide counters must reflect exactly the reserved-scope
// overhead — no more, no less.
//
// This is the explicit contract that subscribers, drainer addons, and
// the auto-populate hooks (Phase A) all rely on. If a future refactor
// forgets to wire init or moves it past a returning code path, this
// test fails with the offending invariant.
func TestNewStore_PreCreatesReservedScopes(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})

	// Each reserved scope exists, is empty, and has lastWriteTS=0.
	for _, name := range reservedScopeNames {
		buf, ok := s.getScope(name)
		if !ok {
			t.Errorf("reserved scope %q not pre-created", name)
			continue
		}
		buf.mu.RLock()
		itemCount := len(buf.items)
		bufLastWrite := buf.lastWriteTS
		bufBytes := buf.bytes
		buf.mu.RUnlock()
		if itemCount != 0 {
			t.Errorf("reserved scope %q has %d items at boot, want 0", name, itemCount)
		}
		if bufLastWrite != 0 {
			t.Errorf("reserved scope %q lastWriteTS=%d at boot, want 0 (bootstrap is not activity)", name, bufLastWrite)
		}
		if bufBytes != 0 {
			t.Errorf("reserved scope %q b.bytes=%d at boot, want 0 (no items)", name, bufBytes)
		}
	}

	// Store-wide counters: scope_count == len(reservedScopeNames),
	// total_items == 0, totalBytes == reservedScopesOverhead, and
	// lastWriteTS == 0 (the "fresh boot" sentinel).
	if got := s.scopeCount.Load(); got != int64(len(reservedScopeNames)) {
		t.Errorf("fresh store scope_count=%d want %d", got, len(reservedScopeNames))
	}
	if got := s.totalItems.Load(); got != 0 {
		t.Errorf("fresh store total_items=%d want 0", got)
	}
	if got := s.totalBytes.Load(); got != reservedScopesOverhead {
		t.Errorf("fresh store totalBytes=%d want %d (reserved-scope overhead only)",
			got, reservedScopesOverhead)
	}
	if got := s.lastWriteTS.Load(); got != 0 {
		t.Errorf("fresh store lastWriteTS=%d want 0 (bootstrap is not activity)", got)
	}

	// And the same invariant the assertion helper enforces everywhere
	// else: counters agree with the per-shard ground truth.
	assertStatsCountersInvariant(t, s, "fresh store")
}

// TestNewStore_PreCreatesReservedScopes_NonReserved verifies the negative
// half of the init contract: NewStore creates exactly the reserved scopes,
// nothing else. Probes a handful of names that are NOT in the
// reservedScopeNames list (including underscore-prefixed names that
// might be confused with reserved-by-prefix) to make sure they don't
// exist on a fresh store.
func TestNewStore_PreCreatesReservedScopes_NonReserved(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})

	for _, name := range []string{
		"thread:42",   // ordinary user scope
		"events",      // ordinary user scope
		"_tokens",     // addon-convention prefix; NOT reserved by core
		"_counters_x", // same
		"_log_extra",  // close to reserved name but not exactly
		"_inbox2",     // same
		"_",           // underscore alone
	} {
		if _, ok := s.getScope(name); ok {
			t.Errorf("scope %q exists on fresh store; only the reserved names should be pre-created", name)
		}
	}
}

// TestStore_LastWriteTS_BumpsOnEveryWritePath drives every path that
// is supposed to bump s.lastWriteTS and asserts each one strictly
// advances the counter. If a future change forgets to wire the bump
// into one path, this test fails with the path's name in the context.
func TestStore_LastWriteTS_BumpsOnEveryWritePath(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})

	// step runs `op` and asserts s.lastWriteTS strictly advances. The
	// helper sleeps a microsecond first so the post-op time.Now() value
	// is guaranteed to be greater than the pre-op snapshot — without
	// this two consecutive calls inside the same microsecond would
	// register as "no advance" even though the bump fired.
	step := func(name string, op func()) {
		t.Helper()
		time.Sleep(time.Microsecond)
		before := s.lastWriteTS.Load()
		op()
		after := s.lastWriteTS.Load()
		if after <= before {
			t.Errorf("after %s: lastWriteTS=%d want > before=%d (path forgot to bump?)", name, after, before)
		}
	}

	step("appendOne", func() {
		if _, err := s.appendOne(Item{Scope: "a", ID: "1", Payload: json.RawMessage(`"v"`)}); err != nil {
			t.Fatalf("appendOne: %v", err)
		}
	})
	step("upsertOne replace", func() {
		if _, _, err := s.upsertOne(Item{Scope: "a", ID: "1", Payload: json.RawMessage(`"v2"`)}); err != nil {
			t.Fatalf("upsertOne replace: %v", err)
		}
	})
	step("upsertOne create", func() {
		if _, _, err := s.upsertOne(Item{Scope: "a", ID: "2", Payload: json.RawMessage(`"v"`)}); err != nil {
			t.Fatalf("upsertOne create: %v", err)
		}
	})
	// counterAddOne is intentionally absent from this list: counter
	// operations do not bump s.lastWriteTS by design (see file header
	// on buffer_counter.go). A separate test below
	// (TestStore_LastWriteTS_NotBumpedByCounterAdd) pins that contract.
	step("updateOne", func() {
		if _, err := s.updateOne(Item{Scope: "a", ID: "1", Payload: json.RawMessage(`"v3"`)}); err != nil {
			t.Fatalf("updateOne: %v", err)
		}
	})
	step("deleteOne", func() {
		if _, err := s.deleteOne("a", "1", 0); err != nil {
			t.Fatalf("deleteOne: %v", err)
		}
	})
	// Build up some items in scope b for deleteUpTo.
	for i := 0; i < 3; i++ {
		if _, err := s.appendOne(Item{Scope: "b", Payload: json.RawMessage(`"v"`)}); err != nil {
			t.Fatalf("appendOne b: %v", err)
		}
	}
	step("deleteUpTo", func() {
		if _, err := s.deleteUpTo("b", 2); err != nil {
			t.Fatalf("deleteUpTo: %v", err)
		}
	})
	step("deleteScope", func() {
		if _, ok := s.deleteScope("b"); !ok {
			t.Fatal("deleteScope: scope b missing")
		}
	})
	step("replaceScopes (warm)", func() {
		if _, err := s.replaceScopes(map[string][]Item{
			"warmed": {{Scope: "warmed", Payload: json.RawMessage(`"v"`)}},
		}); err != nil {
			t.Fatalf("replaceScopes: %v", err)
		}
	})
	step("rebuildAll", func() {
		if _, _, err := s.rebuildAll(map[string][]Item{
			"r": {{Scope: "r", Payload: json.RawMessage(`"v"`)}},
		}); err != nil {
			t.Fatalf("rebuildAll: %v", err)
		}
	})
	step("wipe", func() {
		_, _, _ = s.wipe()
	})
}

// TestStore_LastWriteTS_NotBumpedByCounterAdd pins the inverse contract
// to TestStore_LastWriteTS_BumpsOnEveryWritePath: counter activity must
// never advance s.lastWriteTS, regardless of branch (create / promote
// / increment). View-counter-style read-driven workloads would
// otherwise turn the store-wide freshness signal into a heartbeat and
// break consumers polling /stats.last_write_ts to skip needless
// refetches. See the file header on buffer_counter.go for the design
// rationale.
//
// scopeCreated is the one bump that legitimately fires when /counter_add
// runs against a brand-new scope: getOrCreateScopeTrackingCreated
// bumps s.lastWriteTS as part of scope provisioning (scope_count grew
// — a structural /stats change). That bump is incidental to the counter
// operation itself, so the test seeds the scope first to keep the
// counter ops on an existing-scope path where no other bump source
// exists.
func TestStore_LastWriteTS_NotBumpedByCounterAdd(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})

	// Seed the scope via /append so getOrCreateScope's structural bump
	// has fired before we measure counter activity.
	if _, err := s.appendOne(Item{Scope: "a", ID: "seed", Payload: json.RawMessage(`"v"`)}); err != nil {
		t.Fatalf("seed append: %v", err)
	}
	// Also seed an int-payload item so we have something to promote.
	if _, err := s.appendOne(Item{Scope: "a", ID: "promotable", Payload: json.RawMessage(`5`)}); err != nil {
		t.Fatalf("seed int append: %v", err)
	}

	check := func(name string, op func()) {
		t.Helper()
		// time.Microsecond gap so ANY accidental bump would be visible
		// in the comparison; without it a same-microsecond no-op would
		// look identical to a same-microsecond bump.
		time.Sleep(time.Microsecond)
		before := s.lastWriteTS.Load()
		op()
		after := s.lastWriteTS.Load()
		if after != before {
			t.Errorf("after %s: lastWriteTS bumped from %d to %d (counter op must be silent)",
				name, before, after)
		}
	}

	check("counter create", func() {
		if _, _, err := s.counterAddOne("a", "ctr", 5); err != nil {
			t.Fatalf("counter create: %v", err)
		}
	})
	check("counter increment (fast path)", func() {
		if _, _, err := s.counterAddOne("a", "ctr", 1); err != nil {
			t.Fatalf("counter increment: %v", err)
		}
	})
	check("counter promote", func() {
		if _, _, err := s.counterAddOne("a", "promotable", 1); err != nil {
			t.Fatalf("counter promote: %v", err)
		}
	})
	// Subsequent increment on the just-promoted counter — verifies
	// promotion installed a cell that the fast path now uses.
	check("counter increment (post-promote, fast path)", func() {
		if _, _, err := s.counterAddOne("a", "promotable", 1); err != nil {
			t.Fatalf("counter post-promote increment: %v", err)
		}
	})
}

// TestStore_LastWriteTS_MonotonicUnderRace verifies the CAS-max
// guarantee: even under aggressive concurrent writers from many
// scopes (whose individual time.Now().UnixMicro() readings can
// land in the counter out of order), the store-wide counter only
// ever advances. The post-condition is the simplest possible:
// the final counter value equals the maximum of every per-write
// timestamp the workers observed.
func TestStore_LastWriteTS_MonotonicUnderRace(t *testing.T) {
	const (
		workers      = 16
		opsPerWorker = 200
	)
	s := NewStore(Config{ScopeMaxItems: 1000, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})

	maxObserved := make([]int64, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(id int) {
			defer wg.Done()
			scope := fmt.Sprintf("race_%d", id)
			for i := 0; i < opsPerWorker; i++ {
				if _, err := s.appendOne(Item{
					Scope:   scope,
					Payload: json.RawMessage(`"v"`),
				}); err != nil {
					t.Errorf("worker %d op %d: %v", id, i, err)
					return
				}
				if got := s.lastWriteTS.Load(); got > maxObserved[id] {
					maxObserved[id] = got
				}
			}
		}(w)
	}
	wg.Wait()

	final := s.lastWriteTS.Load()
	for id, v := range maxObserved {
		if final < v {
			t.Errorf("final lastWriteTS=%d < worker[%d] max-observed=%d (CAS-max regressed)", final, id, v)
		}
	}
	// Also assert >= max(buf.lastWriteTS) per the standard invariant.
	assertStatsCountersInvariant(t, s, "after race workload")
}
