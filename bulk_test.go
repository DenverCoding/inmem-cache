package scopecache

import (
	"errors"
	"sync"
	"testing"
)

func jsonRaw(s string) []byte { return []byte(s) }

// Race regression: /warm's Phase 1.5 reservation and Phase 2 commit must
// serialise against /wipe and /rebuild. Pre-fix /wipe's Swap(0) (or
// /rebuild's Store) erased a /warm reservation, and the drift comp in
// commitReplacementPreReserved then over-credited totalBytes by the
// snapshot's oldBytes — leaving the store-wide invariant
// (totalBytes == Σ buf.bytes) violated.
//
// The race window is tiny (a few ns between reserveBytes and the per-
// scope commit), so a naive "fire both goroutines and see what happens"
// test mostly observes one finishing before the other starts. Stress
// with many iterations to make the race probabilistic. With the fix in
// place every iteration should pass.
func TestStore_ReplaceScopes_RaceVsWipe(t *testing.T) {
	const iterations = 5000

	for i := 0; i < iterations; i++ {
		s := NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})

		// Seed multiple scopes so /warm has multiple oldBytes snapshots and
		// a wider Phase 1.5 → Phase 2 window for /wipe to slip into.
		seed := map[string][]Item{}
		for s2 := 0; s2 < 5; s2++ {
			scope := "s" + string(rune('0'+s2))
			items := []Item{}
			for j := 0; j < 4; j++ {
				items = append(items, Item{Scope: scope, ID: "k" + string(rune('0'+j)), Payload: jsonRaw(`{"v":1}`)})
			}
			seed[scope] = items
		}
		if _, err := s.replaceScopes(seed); err != nil {
			t.Fatalf("seed: %v", err)
		}

		// Concurrent /warm (replace all 5 scopes with smaller payloads) + /wipe.
		// Either order is legal; what's NOT legal is them interleaving in a
		// way that leaves totalBytes != Σ buf.bytes.
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			repl := map[string][]Item{}
			for s2 := 0; s2 < 5; s2++ {
				scope := "s" + string(rune('0'+s2))
				repl[scope] = []Item{{Scope: scope, ID: "newkey", Payload: jsonRaw(`{"v":42}`)}}
			}
			_, _ = s.replaceScopes(repl)
		}()
		go func() {
			defer wg.Done()
			s.wipe()
		}()
		wg.Wait()

		assertBytesInvariant(t, s, i, "warm-vs-wipe")
	}
}

func TestStore_ReplaceScopes_RaceVsRebuild(t *testing.T) {
	const iterations = 5000

	for i := 0; i < iterations; i++ {
		s := NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})

		seed := map[string][]Item{}
		for s2 := 0; s2 < 5; s2++ {
			scope := "s" + string(rune('0'+s2))
			items := []Item{}
			for j := 0; j < 4; j++ {
				items = append(items, Item{Scope: scope, ID: "k" + string(rune('0'+j)), Payload: jsonRaw(`{"v":1}`)})
			}
			seed[scope] = items
		}
		if _, err := s.replaceScopes(seed); err != nil {
			t.Fatalf("seed: %v", err)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			repl := map[string][]Item{}
			for s2 := 0; s2 < 5; s2++ {
				scope := "s" + string(rune('0'+s2))
				repl[scope] = []Item{{Scope: scope, ID: "newkey", Payload: jsonRaw(`{"v":42}`)}}
			}
			_, _ = s.replaceScopes(repl)
		}()
		go func() {
			defer wg.Done()
			rebuildItems := map[string][]Item{
				"new-after-rebuild": {{Scope: "new-after-rebuild", ID: "x", Payload: jsonRaw(`{"v":99}`)}},
			}
			_, _, _ = s.rebuildAll(rebuildItems)
		}()
		wg.Wait()

		assertBytesInvariant(t, s, i, "warm-vs-rebuild")
	}
}

func assertBytesInvariant(t *testing.T, s *Store, iter int, label string) {
	t.Helper()

	var sum int64
	var scopeCount int
	for shIdx := range s.shards {
		s.shards[shIdx].mu.RLock()
		scopeCount += len(s.shards[shIdx].scopes)
		for _, buf := range s.shards[shIdx].scopes {
			buf.mu.RLock()
			sum += buf.bytes
			buf.mu.RUnlock()
		}
		s.shards[shIdx].mu.RUnlock()
	}

	// Since v0.5.14 totalBytes also includes scopeBufferOverhead per
	// allocated scope (admission control sees the real per-scope memory
	// cost). Σ buf.bytes is item-bytes only, so add the overhead per
	// surviving scope to get the expected counter value.
	expected := sum + int64(scopeCount)*scopeBufferOverhead
	total := s.totalBytes.Load()
	if total != expected {
		t.Errorf("[%s iter %d] totalBytes=%d but expected=%d (Σ buf.bytes=%d + %d×%d overhead, drift=%d)",
			label, iter, total, expected, sum, scopeCount, scopeBufferOverhead, total-expected)
	}
}

// --- Store.wipe ---------------------------------------------------------------

func TestStore_Wipe_EmptyStore(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})

	scopes, items, freed := s.wipe()
	if scopes != 0 || items != 0 || freed != 0 {
		t.Fatalf("wipe empty: scopes=%d items=%d freed=%d want 0,0,0", scopes, items, freed)
	}
}

func TestStore_Wipe_RemovesEveryScopeAndCountsItems(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})

	for _, name := range []string{"a", "b", "c"} {
		buf, _ := s.getOrCreateScope(name)
		for i := 0; i < 4; i++ {
			if _, err := buf.appendItem(newItem(name, "", nil)); err != nil {
				t.Fatalf("append %s/%d: %v", name, i, err)
			}
		}
	}

	scopes, items, freed := s.wipe()
	if scopes != 3 || items != 12 {
		t.Fatalf("wipe: scopes=%d items=%d want 3,12", scopes, items)
	}
	if freed <= 0 {
		t.Fatalf("wipe: freed=%d want >0", freed)
	}

	for _, name := range []string{"a", "b", "c"} {
		if _, ok := s.getScope(name); ok {
			t.Errorf("scope %q should be gone after wipe", name)
		}
	}
}

// wipe must bring s.totalBytes exactly back to zero so the next append's
// reservation starts from a clean baseline. This is the property /wipe
// promises to clients that are about to /rebuild into a freshly empty store.
func TestStore_Wipe_ResetsTotalBytes(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})

	buf, _ := s.getOrCreateScope("s")
	for i := 0; i < 5; i++ {
		if _, err := buf.appendItem(newItem("s", "", nil)); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	if s.totalBytes.Load() == 0 {
		t.Fatal("totalBytes should be non-zero before wipe")
	}

	_, _, freed := s.wipe()
	if got := s.totalBytes.Load(); got != 0 {
		t.Fatalf("totalBytes=%d want 0 after wipe", got)
	}
	if freed == 0 {
		t.Fatal("freed bytes reported as 0 despite non-empty store")
	}
}

// After /wipe the next /append must succeed even when the pre-wipe store
// was at its byte cap — the cap budget has been fully released.
func TestStore_Wipe_FreesHeadroomForNextAppend(t *testing.T) {
	itemSize := approxItemSize(newItem("s", "", nil))
	// Cap room for: per-scope overhead (allocated by getOrCreateScope)
	// + 3 items. The fourth item then exceeds the cap.
	capBytes := scopeBufferOverhead + itemSize*3

	s := NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: capBytes, MaxItemBytes: 1 << 20})
	buf, _ := s.getOrCreateScope("s")
	for i := 0; i < 3; i++ {
		if _, err := buf.appendItem(newItem("s", "", nil)); err != nil {
			t.Fatalf("fill %d: %v", i, err)
		}
	}
	// At the cap: the next append must fail.
	if _, err := buf.appendItem(newItem("s", "", nil)); err == nil {
		t.Fatal("expected StoreFullError before wipe")
	}

	s.wipe()

	buf2, err := s.getOrCreateScope("s")
	if err != nil {
		t.Fatalf("getOrCreateScope after wipe: %v", err)
	}
	if _, err := buf2.appendItem(newItem("s", "", nil)); err != nil {
		t.Fatalf("append after wipe: %v", err)
	}
}

// A scopeBuffer pointer held before wipe must detach cleanly: further
// writes on it must return *ScopeDetachedError rather than silently
// succeeding into an orphan buffer no reader can reach. The store's byte
// counter must also remain at zero.
func TestStore_Wipe_DetachesOrphanedBuffers(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})

	buf, _ := s.getOrCreateScope("s")
	if _, err := buf.appendItem(newItem("s", "a", nil)); err != nil {
		t.Fatalf("append: %v", err)
	}

	s.wipe()

	// The caller still has `buf`; a write must now fail with ScopeDetachedError.
	_, err := buf.appendItem(newItem("s", "b", nil))
	var sde *ScopeDetachedError
	if !errors.As(err, &sde) {
		t.Fatalf("orphan append: got %v, want *ScopeDetachedError", err)
	}
	if got := s.totalBytes.Load(); got != 0 {
		t.Fatalf("orphan mutation leaked into totalBytes: got %d want 0", got)
	}
}

// /rebuild swaps the entire store map. Any stale scopeBuffer pointer held
// by an in-flight /append must be detached — otherwise reserveBytes on the
// post-rebuild counter inflates totalBytes permanently, while the item
// lands in an orphan buffer that no reader can reach. Mirrors the wipe and
// delete_scope guarantees.
func TestStore_RebuildAll_DetachesOrphanedBuffers(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})

	buf, _ := s.getOrCreateScope("old")
	if _, err := buf.appendItem(newItem("old", "a", nil)); err != nil {
		t.Fatalf("pre-rebuild append: %v", err)
	}

	grouped := map[string][]Item{"new": {newItem("new", "x", nil)}}
	if _, _, err := s.rebuildAll(grouped); err != nil {
		t.Fatalf("rebuildAll: %v", err)
	}

	// The pre-rebuild buf pointer is stale; a write must now fail.
	_, err := buf.appendItem(newItem("old", "b", nil))
	var sde *ScopeDetachedError
	if !errors.As(err, &sde) {
		t.Fatalf("orphan append: got %v, want *ScopeDetachedError", err)
	}
	// Counter must still match only the rebuilt scope's item + its
	// per-scope buffer overhead — the orphan write must not have
	// inflated it.
	newBuf, _ := s.getScope("new")
	if got, want := s.totalBytes.Load(), newBuf.bytes+scopeBufferOverhead; got != want {
		t.Fatalf("totalBytes=%d want %d (orphan leaked into counter)", got, want)
	}
}

func TestStore_ReplaceScopes_LeavesOtherScopesUntouched(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})

	keep, _ := s.getOrCreateScope("keep")
	_, _ = keep.appendItem(newItem("keep", "k1", nil))
	keepLen := len(keep.items)

	grouped := map[string][]Item{
		"replace": {newItem("replace", "r1", nil)},
	}
	n, err := s.replaceScopes(grouped)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("replaced=%d want 1", n)
	}
	if len(keep.items) != keepLen {
		t.Fatal("untouched scope was mutated")
	}
}

// A grouped map with an empty scope key must be rejected with a shape error
// (not an offender list), since the empty scope could not have passed the
// handler's per-item validation.
func TestStore_ReplaceScopes_RejectsEmptyScope(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})
	grouped := map[string][]Item{
		"": {newItem("", "a", nil)},
	}
	if _, err := s.replaceScopes(grouped); err == nil {
		t.Fatal("expected empty-scope rejection")
	}
}

func TestStore_RebuildAll_WipesEverything(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})

	old, _ := s.getOrCreateScope("old")
	_, _ = old.appendItem(newItem("old", "", nil))

	grouped := map[string][]Item{
		"new": {newItem("new", "n1", nil)},
	}
	nScopes, nItems, err := s.rebuildAll(grouped)
	if err != nil {
		t.Fatal(err)
	}
	if nScopes != 1 || nItems != 1 {
		t.Fatalf("rebuildAll: scopes=%d items=%d", nScopes, nItems)
	}
	if _, ok := s.getScope("old"); ok {
		t.Fatal("old scope should be wiped")
	}
	if _, ok := s.getScope("new"); !ok {
		t.Fatal("new scope should exist")
	}
}

// /warm must reject the whole batch (not partial-apply) when any scope in
// the request exceeds the cap. The error carries every offending scope so a
// client can fix all at once rather than discovering them one-by-one.
func TestStore_ReplaceScopes_RejectsOverCapWithAllOffenders(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 3, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})

	keep, _ := s.getOrCreateScope("untouched")
	_, _ = keep.appendItem(newItem("untouched", "k", nil))
	preLen := len(keep.items)

	grouped := map[string][]Item{
		"fits": {newItem("fits", "a", nil), newItem("fits", "b", nil)},
		"too_big_1": {
			newItem("too_big_1", "a", nil),
			newItem("too_big_1", "b", nil),
			newItem("too_big_1", "c", nil),
			newItem("too_big_1", "d", nil),
		},
		"too_big_2": {
			newItem("too_big_2", "a", nil),
			newItem("too_big_2", "b", nil),
			newItem("too_big_2", "c", nil),
			newItem("too_big_2", "d", nil),
			newItem("too_big_2", "e", nil),
		},
	}

	_, err := s.replaceScopes(grouped)
	if err == nil {
		t.Fatal("expected ScopeCapacityError when batch exceeds cap")
	}
	var sce *ScopeCapacityError
	if !errors.As(err, &sce) {
		t.Fatalf("expected *ScopeCapacityError, got %T: %v", err, err)
	}
	if len(sce.Offenders) != 2 {
		t.Fatalf("expected 2 offenders, got %d: %+v", len(sce.Offenders), sce.Offenders)
	}
	// Validate each offender carries scope/count/cap; map ordering means the
	// offender list order is not deterministic.
	seen := map[string]ScopeCapacityOffender{}
	for _, o := range sce.Offenders {
		seen[o.Scope] = o
	}
	if o, ok := seen["too_big_1"]; !ok || o.Count != 4 || o.Cap != 3 {
		t.Fatalf("offender for too_big_1 missing or wrong: %+v", o)
	}
	if o, ok := seen["too_big_2"]; !ok || o.Count != 5 || o.Cap != 3 {
		t.Fatalf("offender for too_big_2 missing or wrong: %+v", o)
	}

	// Atomic: the well-sized "fits" scope must NOT have been applied, and
	// the pre-existing "untouched" scope must still have its original item.
	if _, ok := s.getScope("fits"); ok {
		t.Fatal("fits scope was applied despite batch error")
	}
	if len(keep.items) != preLen {
		t.Fatal("existing scope was mutated by a failed batch")
	}
}

func TestStore_RebuildAll_RejectsOverCapWithAllOffenders(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 2, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})

	priorScope, _ := s.getOrCreateScope("prior")
	_, _ = priorScope.appendItem(newItem("prior", "p", nil))

	grouped := map[string][]Item{
		"too_big_1": {
			newItem("too_big_1", "a", nil),
			newItem("too_big_1", "b", nil),
			newItem("too_big_1", "c", nil),
		},
		"too_big_2": {
			newItem("too_big_2", "a", nil),
			newItem("too_big_2", "b", nil),
			newItem("too_big_2", "c", nil),
		},
	}

	_, _, err := s.rebuildAll(grouped)
	if err == nil {
		t.Fatal("expected ScopeCapacityError")
	}
	var sce *ScopeCapacityError
	if !errors.As(err, &sce) {
		t.Fatalf("expected *ScopeCapacityError, got %T: %v", err, err)
	}
	if len(sce.Offenders) != 2 {
		t.Fatalf("expected 2 offenders, got %d", len(sce.Offenders))
	}
	// rebuildAll replaces the entire store on success. On failure the old
	// store must remain intact — verify the prior scope is still there.
	if _, ok := s.getScope("prior"); !ok {
		t.Fatal("prior scope was wiped by a failed rebuild")
	}
}

func TestStore_RebuildAll_RejectsDuplicateIDs(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})

	s2, _ := s.getOrCreateScope("keep")
	_, _ = s2.appendItem(newItem("keep", "k1", nil))

	grouped := map[string][]Item{
		"bad": {
			newItem("bad", "x", nil),
			newItem("bad", "x", nil),
		},
	}
	if _, _, err := s.rebuildAll(grouped); err == nil {
		t.Fatal("expected duplicate id error")
	}
	// Validation happens before wiping, so existing scopes must be intact.
	if _, ok := s.getScope("keep"); !ok {
		t.Fatal("rebuildAll must not wipe on validation failure")
	}
}

// /warm's byte-cap check runs across all scopes in the batch. A request
// whose net byte delta would push the store over the cap is rejected as a
// whole with StoreFullError, and no scope is applied.
func TestStore_ReplaceScopes_RejectsAtByteCap(t *testing.T) {
	itemSize := approxItemSize(newItem("s", "", nil))
	// One scope-buffer overhead (for the pre-seed) + 3 items worth.
	// The /warm batch needs 2 new scopes (overhead × 2) + 4 items, so
	// expected post-batch usage = 3 overheads + 5 items, well past
	// the cap of 1 overhead + 3 items.
	capBytes := scopeBufferOverhead + itemSize*3

	s := NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: capBytes, MaxItemBytes: 1 << 20})

	// Pre-seed an unrelated scope so we can assert it survives the reject.
	pre, _ := s.getOrCreateScope("untouched")
	if _, err := pre.appendItem(newItem("untouched", "u", nil)); err != nil {
		t.Fatalf("pre-seed: %v", err)
	}
	preLen := len(pre.items)

	// Batch adds 4 items + 2 new scopes — store already holds 1 item +
	// 1 scope overhead, so delta pushes total over the cap.
	grouped := map[string][]Item{
		"a": {newItem("a", "", nil), newItem("a", "", nil)},
		"b": {newItem("b", "", nil), newItem("b", "", nil)},
	}
	_, err := s.replaceScopes(grouped)
	if err == nil {
		t.Fatal("expected StoreFullError for over-cap batch")
	}
	var stfe *StoreFullError
	if !errors.As(err, &stfe) {
		t.Fatalf("expected *StoreFullError, got %T: %v", err, err)
	}

	// No scope in the batch may have been applied, and the pre-seeded scope
	// must be untouched.
	if _, ok := s.getScope("a"); ok {
		t.Fatal("scope 'a' applied despite reject")
	}
	if _, ok := s.getScope("b"); ok {
		t.Fatal("scope 'b' applied despite reject")
	}
	if len(pre.items) != preLen {
		t.Fatalf("untouched scope mutated: len=%d want %d", len(pre.items), preLen)
	}
	preSize := approxItemSize(newItem("untouched", "u", nil))
	if got, want := s.totalBytes.Load(), int64(scopeBufferOverhead)+preSize; got != want {
		t.Fatalf("totalBytes=%d want %d (only pre-seed scope+item should count)", got, want)
	}
}

// /warm must never push totalBytes past the cap, even when concurrent
// /append traffic targets scopes outside the batch. The pre-fix design
// snapshotted totalBytes before commit, so an appender that slipped in
// between snapshot and commit could collectively overshoot the cap:
// the batch's pre-check would see room, the appender's own check would
// also see room, but once both committed the sum exceeded the cap.
// reserveBytes-based admission closes that window: the batch reserves
// its delta atomically via CAS, after which any concurrent appender
// sees the post-reserve total and is rejected if the cap is exceeded.
//
// Setup: cap = 5 item-sizes, store pre-seeded with 2 items, /warm grows
// the batch scope by +2 items, 16 concurrent appenders each try +1 on
// a different scope. Safe outcome: /warm fits (+2 → 4) and at most ONE
// appender fits (4 → 5). Anything past that violates the cap.
func TestStore_ReplaceScopes_StrictCapAgainstConcurrentAppends(t *testing.T) {
	itemSize := approxItemSize(newItem("s", "x", nil))
	const appendersPerIter = 16
	// Per-scope overhead × (1 batch + 16 appenders) + 5 items worth.
	// Originally just `itemSize*5`; bumped to make room for the
	// per-scope overhead introduced in v0.5.14.
	capBytes := scopeBufferOverhead*(appendersPerIter+1) + itemSize*5

	const iterations = 500

	for iter := 0; iter < iterations; iter++ {
		s := NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: capBytes, MaxItemBytes: 1 << 20})

		// Pre-seed "batch" with 2 items; /warm will replace it with 4
		// items, net delta +2 item-sizes.
		batch, _ := s.getOrCreateScope("batch")
		_, _ = batch.appendItem(newItem("batch", "a", nil))
		_, _ = batch.appendItem(newItem("batch", "b", nil))

		grouped := map[string][]Item{
			"batch": {
				newItem("batch", "c", nil),
				newItem("batch", "d", nil),
				newItem("batch", "e", nil),
				newItem("batch", "f", nil),
			},
		}

		var wg sync.WaitGroup
		wg.Add(appendersPerIter + 1)
		for i := 0; i < appendersPerIter; i++ {
			i := i
			go func() {
				defer wg.Done()
				scope := "other" + string(rune('A'+i))
				// getOrCreateScope can fail with StoreFullError when
				// the per-scope overhead reservation hits the cap;
				// the test deliberately provisions just enough room
				// for all appenders, but a tight race may still see
				// a transient over-cap. Skip on err — the test
				// asserts the cap-strict invariant on whatever
				// landed.
				buf, err := s.getOrCreateScope(scope)
				if err != nil {
					return
				}
				_, _ = buf.appendItem(newItem(scope, "w", nil))
			}()
		}
		go func() {
			defer wg.Done()
			_, _ = s.replaceScopes(grouped)
		}()
		wg.Wait()

		got := s.totalBytes.Load()
		if got > capBytes {
			t.Fatalf("iter %d: totalBytes=%d exceeds cap=%d (race let writers overshoot)",
				iter, got, capBytes)
		}

		// Accounting invariant: totalBytes must equal sum of per-scope
		// item bytes PLUS scope-buffer overhead × scope count.
		var sum int64
		scopes := s.listScopes()
		for _, buf := range scopes {
			buf.mu.RLock()
			sum += buf.bytes
			buf.mu.RUnlock()
		}
		expected := sum + int64(len(scopes))*scopeBufferOverhead
		if got != expected {
			t.Fatalf("iter %d: totalBytes=%d != Σ buf.bytes + overhead = %d", iter, got, expected)
		}
	}
}

// /rebuild is all-or-nothing: if the new total bytes exceed the cap the
// rebuild aborts and the prior store is left intact.
func TestStore_RebuildAll_RejectsAtByteCap(t *testing.T) {
	itemSize := approxItemSize(newItem("s", "", nil))
	// Cap fits 1 scope-overhead + 2 items. The /rebuild target tries
	// 1 scope-overhead + 3 items — should fail by 1 itemSize.
	capBytes := scopeBufferOverhead + itemSize*2

	s := NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: capBytes, MaxItemBytes: 1 << 20})
	pre, _ := s.getOrCreateScope("prior")
	if _, err := pre.appendItem(newItem("prior", "p", nil)); err != nil {
		t.Fatalf("pre-seed: %v", err)
	}

	grouped := map[string][]Item{
		"big": {newItem("big", "", nil), newItem("big", "", nil), newItem("big", "", nil)},
	}
	_, _, err := s.rebuildAll(grouped)
	if err == nil {
		t.Fatal("expected StoreFullError for over-cap rebuild")
	}
	var stfe *StoreFullError
	if !errors.As(err, &stfe) {
		t.Fatalf("expected *StoreFullError, got %T: %v", err, err)
	}

	// Prior state must still be intact — no partial swap.
	if _, ok := s.getScope("prior"); !ok {
		t.Fatal("prior scope wiped despite rebuild reject")
	}
	if _, ok := s.getScope("big"); ok {
		t.Fatal("rebuild was partially applied")
	}
}

// A successful /rebuild replaces the store entirely: totalBytes must match
// the sum of the newly installed items, not be additive to the prior state.
func TestStore_RebuildAll_ResetsByteCounter(t *testing.T) {
	s := NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20})

	pre, _ := s.getOrCreateScope("old")
	for i := 0; i < 5; i++ {
		if _, err := pre.appendItem(newItem("old", "", nil)); err != nil {
			t.Fatalf("pre-seed %d: %v", i, err)
		}
	}

	grouped := map[string][]Item{
		"new": {newItem("new", "n1", nil), newItem("new", "n2", nil)},
	}
	if _, _, err := s.rebuildAll(grouped); err != nil {
		t.Fatal(err)
	}

	// 1 new scope (overhead) + 2 items.
	expected := int64(scopeBufferOverhead) + approxItemSize(newItem("new", "n1", nil)) + approxItemSize(newItem("new", "n2", nil))
	if got := s.totalBytes.Load(); got != expected {
		t.Fatalf("totalBytes=%d want %d (counter must be reset to new total)", got, expected)
	}
}
