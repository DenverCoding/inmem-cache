package scopecache

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
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
	if api.maxMultiCallBytes != int64(MaxMultiCallMiB)<<20 {
		t.Errorf("api.maxMultiCallBytes=%d want %d", api.maxMultiCallBytes, int64(MaxMultiCallMiB)<<20)
	}
	if api.maxMultiCallCount != MaxMultiCallCount {
		t.Errorf("api.maxMultiCallCount=%d want %d", api.maxMultiCallCount, MaxMultiCallCount)
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
// back to compile-time defaults; string/slice/bool zeros stay unchanged
// because their zero-values are documented kill-switches (empty
// ServerSecret disables /guarded, empty InboxScopes disables /inbox).
func TestAPIConfig_WithDefaults(t *testing.T) {
	t.Run("zero fields fall back to defaults", func(t *testing.T) {
		got := APIConfig{}.WithDefaults()
		if got.MaxResponseBytes != int64(MaxResponseMiB)<<20 {
			t.Errorf("MaxResponseBytes=%d", got.MaxResponseBytes)
		}
		if got.MaxMultiCallBytes != int64(MaxMultiCallMiB)<<20 {
			t.Errorf("MaxMultiCallBytes=%d", got.MaxMultiCallBytes)
		}
		if got.MaxMultiCallCount != MaxMultiCallCount {
			t.Errorf("MaxMultiCallCount=%d", got.MaxMultiCallCount)
		}
		if got.MaxInboxBytes != int64(MaxInboxKiB)<<10 {
			t.Errorf("MaxInboxBytes=%d", got.MaxInboxBytes)
		}
	})

	t.Run("positive fields preserved", func(t *testing.T) {
		in := APIConfig{
			MaxResponseBytes:  13,
			MaxMultiCallBytes: 17,
			MaxMultiCallCount: 19,
			MaxInboxBytes:     23,
			ServerSecret:      "real-secret",
		}
		got := in.WithDefaults()
		if got.MaxResponseBytes != in.MaxResponseBytes ||
			got.MaxMultiCallBytes != in.MaxMultiCallBytes ||
			got.MaxMultiCallCount != in.MaxMultiCallCount ||
			got.MaxInboxBytes != in.MaxInboxBytes ||
			got.ServerSecret != in.ServerSecret {
			t.Errorf("positive APIConfig mutated: got %+v want %+v", got, in)
		}
	})

	t.Run("empty server_secret stays empty (kill-switch)", func(t *testing.T) {
		got := APIConfig{ServerSecret: ""}.WithDefaults()
		if got.ServerSecret != "" {
			t.Errorf("ServerSecret got %q want empty", got.ServerSecret)
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
	// Cap big enough for ~10 scopes' worth of overhead, no item room.
	capBytes := int64(scopeBufferOverhead) * 10
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

	// totalBytes equals exactly created × overhead — a clean accounting
	// of pure scope-buffer cost, no items in any scope.
	wantBytes := int64(created) * scopeBufferOverhead
	if got := s.totalBytes.Load(); got != wantBytes {
		t.Errorf("totalBytes=%d want %d (created=%d × overhead=%d)",
			got, wantBytes, created, scopeBufferOverhead)
	}
}

// /delete_scope must release the per-scope overhead, not just the
// items. Without this, a workload that churns scopes (create, fill,
// delete, repeat) would slowly leak overhead and eventually 507 even
// when the store looks empty.
func TestStore_DeleteScope_ReleasesOverhead(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: int64(scopeBufferOverhead) * 5, MaxItemBytes: 1 << 20})

	// Fill the cap with empty scopes — 5 scopes × overhead = cap.
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

	// totalBytes is still exactly 5 × overhead.
	if got, want := s.totalBytes.Load(), int64(scopeBufferOverhead)*5; got != want {
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
	items, truncated, found := s.head("nope", 0, 10, false)
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
	items, truncated, found := s.tail("nope", 10, 0, false)
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
	if _, found := s.get("nope", "x", 0, false); found {
		t.Error("found=true; want false")
	}
}

func TestStore_render_MissingScope(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})
	if _, found := s.render("nope", "x", 0, false); found {
		t.Error("found=true; want false")
	}
}

// scopeCandidates is the walk-filter-sort-truncate pipeline that
// /delete_scope_candidates surfaces. The HTTP-level tests cover the
// happy path; this pin is for the empty-store contract and the
// limit-truncation invariant — both are easy to silently break by a
// future "always allocate len(scopes)" or "skip the truncate when
// limit > 0" change.
func TestStore_scopeCandidates_EmptyStore(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})
	got := s.scopeCandidates(0, 100)
	if len(got) != 0 {
		t.Errorf("scopeCandidates on empty store=%v; want empty slice", got)
	}
}

func TestStore_scopeCandidates_TruncatesToLimit(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})
	for i := 0; i < 5; i++ {
		_, err := s.appendOne(Item{Scope: fmt.Sprintf("s%d", i), Payload: json.RawMessage(`"v"`)})
		if err != nil {
			t.Fatalf("appendOne %d: %v", i, err)
		}
	}
	got := s.scopeCandidates(0, 3)
	if len(got) != 3 {
		t.Errorf("len=%d; want 3 (truncated)", len(got))
	}
}

// scopeCandidates must surface LastWriteTS so operators ranking
// eviction can distinguish a cold-read scope that is still being
// actively written to (a write-buffer) from a truly idle one. Without
// this signal, write-only workloads always rank as "stalest" because
// they never touch lastAccessTS.
func TestStore_scopeCandidates_SurfacesLastWriteTS(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})

	pre := nowUnixMicro()
	if _, err := s.appendOne(Item{Scope: "s", Payload: json.RawMessage(`"v"`)}); err != nil {
		t.Fatalf("appendOne: %v", err)
	}
	got := s.scopeCandidates(0, 10)
	if len(got) != 1 {
		t.Fatalf("len=%d; want 1", len(got))
	}
	if got[0].LastWriteTS < pre {
		t.Errorf("LastWriteTS=%d pre=%d (must be >= pre-call stamp)", got[0].LastWriteTS, pre)
	}
	// Internally consistent with /stats: candidate and scopeStats read
	// from the same underlying field, so they must match.
	buf, _ := s.getScope("s")
	if got[0].LastWriteTS != buf.lastWriteTS {
		t.Errorf("candidate.LastWriteTS=%d buf.lastWriteTS=%d (must mirror)",
			got[0].LastWriteTS, buf.lastWriteTS)
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
	body, found := s.render("s", "html", 0, false)
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
	// Cap = overhead + 50 bytes. appendOne reserves overhead first,
	// then the item-bytes reservation overflows — scope must be
	// rolled back so the overhead is released.
	capBytes := int64(scopeBufferOverhead) + 50
	s := NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: capBytes, MaxItemBytes: 1 << 20})

	bigItem := Item{Scope: "victim", ID: "x", Payload: bigPayload(200)}
	_, err := s.appendOne(bigItem)
	var stfe *StoreFullError
	if !errors.As(err, &stfe) {
		t.Fatalf("expected StoreFullError, got %T: %v", err, err)
	}

	if got := s.totalBytes.Load(); got != 0 {
		t.Errorf("totalBytes=%d after rolled-back appendOne; want 0", got)
	}
	if _, ok := s.getScope("victim"); ok {
		t.Errorf("scope 'victim' still present in s.scopes after rollback")
	}
}

func TestStore_upsertOne_RollsBackEmptyScopeOnFailure(t *testing.T) {
	capBytes := int64(scopeBufferOverhead) + 50
	s := NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: capBytes, MaxItemBytes: 1 << 20})

	bigItem := Item{Scope: "victim", ID: "x", Payload: bigPayload(200)}
	_, _, err := s.upsertOne(bigItem)
	var stfe *StoreFullError
	if !errors.As(err, &stfe) {
		t.Fatalf("expected StoreFullError, got %T: %v", err, err)
	}

	if got := s.totalBytes.Load(); got != 0 {
		t.Errorf("totalBytes=%d after rolled-back upsertOne; want 0", got)
	}
	if _, ok := s.getScope("victim"); ok {
		t.Errorf("scope 'victim' still present in s.scopes after rollback")
	}
}

func TestStore_counterAddOne_RollsBackEmptyScopeOnFailure(t *testing.T) {
	// Cap = overhead + 1 byte. Even the smallest counter payload
	// (a one-digit integer) overflows on the item-bytes reservation
	// after the per-scope overhead has been claimed.
	capBytes := int64(scopeBufferOverhead) + 1
	s := NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: capBytes, MaxItemBytes: 1 << 20})

	_, _, err := s.counterAddOne("victim", "c1", 42)
	var stfe *StoreFullError
	if !errors.As(err, &stfe) {
		t.Fatalf("expected StoreFullError, got %T: %v", err, err)
	}

	if got := s.totalBytes.Load(); got != 0 {
		t.Errorf("totalBytes=%d after rolled-back counterAddOne; want 0", got)
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
	capBytes := int64(scopeBufferOverhead) + 50
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
	if scopeCount != 0 {
		t.Errorf("after 1000 failed appendOne calls, %d empty scopes leaked", scopeCount)
	}
	if got := s.totalBytes.Load(); got != 0 {
		t.Errorf("totalBytes=%d after 1000 rolled-back appendOne calls; want 0", got)
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
	capBytes := int64(scopeBufferOverhead) + 1000

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

	// Counter must remain at zero — orphan deletes must not leak into
	// totalBytes either way (the b.store guard exists, but with the
	// detached check we never reach it).
	if got := s.totalBytes.Load(); got != 0 {
		t.Errorf("totalBytes=%d want 0 (orphan deletes leaked into counter)", got)
	}
}

// --- store-level byte budget --------------------------------------------------

// Byte-cap is the aggregate approxItemSize across all scopes; writes that
// would push the running total past maxStoreBytes are rejected with
// StoreFullError. State must stay untouched on rejection — same contract as
// the per-scope ScopeFullError.
func TestStore_Append_RejectsAtByteCap(t *testing.T) {
	itemSize := approxItemSize(newItem("s", "", nil))
	capBytes := scopeBufferOverhead + itemSize*3

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
	if got, want := s.totalBytes.Load(), int64(scopeBufferOverhead)+itemSize*3; got != want {
		t.Fatalf("totalBytes=%d want %d after rejected append (overhead + 3 items)", got, want)
	}
}

// Freeing capacity via /delete must let subsequent appends succeed: the
// byte counter has to drop by the removed item's size or the store drifts
// into a permanently "full" state.
func TestStore_Delete_FreesBytes(t *testing.T) {
	itemSize := approxItemSize(newItem("s", "a", nil))
	capBytes := scopeBufferOverhead + itemSize*2

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
	if got, want := s.totalBytes.Load(), int64(scopeBufferOverhead)+itemSize*2; got != want {
		t.Fatalf("totalBytes=%d want %d after delete+append", got, want)
	}
}

func TestStore_DeleteUpTo_FreesBytes(t *testing.T) {
	itemSize := approxItemSize(newItem("s", "", nil))
	capBytes := scopeBufferOverhead + itemSize*3

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
	if got, want := s.totalBytes.Load(), int64(scopeBufferOverhead)+itemSize*3; got != want {
		t.Fatalf("totalBytes=%d want %d", got, want)
	}
}

func TestStore_DeleteScope_FreesBytes(t *testing.T) {
	itemSize := approxItemSize(newItem("s", "", nil))
	capBytes := scopeBufferOverhead + itemSize*4

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
	if got := s.totalBytes.Load(); got != 0 {
		t.Fatalf("totalBytes=%d want 0 after deleteScope", got)
	}
}

// /update that grows the payload must reserve the delta — a grow past the
// byte cap returns StoreFullError without mutating the stored item.
func TestStore_Update_RejectsGrowAtByteCap(t *testing.T) {
	small := newItem("s", "a", map[string]interface{}{"v": 1})
	capBytes := scopeBufferOverhead + approxItemSize(small) + 8 // room for the small item, not a large replacement

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
