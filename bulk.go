// bulk.go owns the multi-shard mutations: wipe, replaceScopes (/warm),
// rebuildAll (/rebuild). All three serialise against single-scope
// writers via shard write locks, and against each other via the
// ascending-shard-index lock order declared in store.go.
//
// Cross-cutting invariants:
//
//   - **Lock order.** wipe and rebuildAll lock every shard via
//     lockAllShards. replaceScopes locks only the shards its input
//     touches via lockShards(shardsForScopes(...)) — both helpers
//     enforce the ascending-index order that prevents deadlock with
//     each other and with /delete_scope.
//   - **Buffer detach.** Every buffer the op replaces is set
//     detached=true under its own buf.mu before the shard map is
//     swapped/cleared. A concurrent in-flight write that wakes up on
//     a stale buf pointer returns *ScopeDetachedError instead of
//     committing into an unreachable orphan; without this, its
//     reserveBytes call would permanently inflate totalBytes against
//     the freshly-reset counter.
//   - **`_events` is silent on bulk destructive ops.** wipe and
//     rebuildAll wipe `_events` itself as part of their work; any
//     event written here would land in the about-to-be-wiped buffer
//     or paradoxically as seq=1 in the freshly-recreated one.
//     Drainers detect via `_events.lastSeq < lastSeenSeq` (cursor-
//     rewind) and reset their state. /warm DOES emit (`{op:"warm"}`)
//     because it does not touch `_events`.
//   - **Reserved scopes are re-created.** wipe and rebuildAll call
//     initReservedScopesLocked under the same all-shard write lock
//     that performed the destructive step, so subscribers never
//     observe a gap. `_events` and `_inbox` are pre-validated out of
//     /warm and /rebuild input so the input never targets them.

package scopecache

import (
	"fmt"
)

// wipe removes every scope and resets the byte counter to zero in
// one atomic step. Returns (scopeCount, totalItems, freedBytes) so
// the /wipe handler can echo what was released.
//
// freedBytes is captured via totalBytes.Swap(0) AFTER every buf has
// been detached, so it covers any bytes a concurrent /append
// committed through reserveBytes while wipe was walking the map.
func (s *store) wipe() (int, int, int64) {
	s.lockAllShards()
	defer s.unlockAllShards()

	scopeCount := 0
	totalItems := 0

	for i := range s.shards {
		sh := &s.shards[i]
		scopeCount += len(sh.scopes)
		for _, buf := range sh.scopes {
			buf.mu.Lock()
			totalItems += len(buf.items)
			buf.detached = true
			buf.store = nil
			buf.mu.Unlock()
		}
	}

	freedBytes := s.totalBytes.Swap(0)
	s.totalItems.Store(0)
	s.scopeCount.Store(0)
	// /wipe is a destructive event, not a per-scope b.lastWriteTS bump
	// (the scopes are gone). Bump the store-wide tick so a polling
	// client sees "something happened" even when the cache lands at
	// scope_count=0. CAS-max means a concurrent in-flight write that
	// snuck through with a strictly later nowUs would still win, which
	// is the correct ordering — that write committed after the wipe.
	s.bumpLastWriteTS(nowUnixMicro())
	for i := range s.shards {
		s.shards[i].scopes = make(map[string]*scopeBuffer)
	}
	// Re-create reserved scopes under the same all-shard write lock so
	// subscribers don't observe a gap. /wipe means "drop user-managed
	// state and reset to the cache's default boot configuration"; the
	// cache's default configuration includes the reserved scopes.
	s.initReservedScopesLocked()

	return scopeCount, totalItems, freedBytes
}

func (s *store) replaceScopes(grouped map[string][]Item) (int, error) {
	type plan struct {
		scope       string
		replacement scopeReplacement
		newBytes    int64
		oldBytes    int64 // per-scope snapshot taken in Phase 1.5
	}

	// Phase 1 — validate and build replacements. Pure function of the input,
	// no store mutation. Capacity offenders are collected across the whole
	// batch so the caller gets one complete error rather than one per
	// round-trip. Any offender aborts the whole batch (no partial apply).
	plans := make([]plan, 0, len(grouped))
	var offenders []ScopeCapacityOffender

	for scope, items := range grouped {
		// Validate the map KEY itself before the reserved-scope check.
		// Empty-slice batches (`grouped["bad ": nil}`) would otherwise
		// bypass the per-item loop's validateWriteItem entirely and
		// let a Go caller create a scope whose name violates the
		// normal shape rules. The HTTP path is safe (groupItemsByScope
		// keys come from already-validated item.Scope), but Gateway
		// callers compose maps by hand.
		if err := validateScope(scope, "/warm"); err != nil {
			return 0, wrapValidation(err)
		}
		if isReservedScope(scope) {
			return 0, fmt.Errorf("%w: scope '%s' is reserved and cannot be the target of /warm", ErrInvalidInput, scope)
		}
		// item.Scope must match the map key. A Go caller could
		// otherwise pass `grouped["actual"]` containing
		// Item{Scope:"wrong"}; the buffer would store under "actual"
		// while item.Scope reads "wrong", silently breaking the
		// scope-identity invariant. Reject rather than normalise — a
		// silent rewrite would mask a misconstructed map.
		for i := range items {
			if err := validateWriteItem(&items[i], "/warm", s.maxItemBytes); err != nil {
				return 0, fmt.Errorf("scope '%s', item at index %d: %w", scope, i, err)
			}
			if items[i].Scope != scope {
				return 0, fmt.Errorf("%w: scope '%s', item at index %d: item.scope %q does not match the map key", ErrInvalidInput, scope, i, items[i].Scope)
			}
		}
		if len(items) > s.defaultMaxItems {
			offenders = append(offenders, ScopeCapacityOffender{
				Scope: scope,
				Count: len(items),
				Cap:   s.defaultMaxItems,
			})
			continue
		}
		r, err := buildReplacementState(items)
		if err != nil {
			return 0, fmt.Errorf("%w: scope '%s': %s", ErrInvalidInput, scope, err.Error())
		}
		plans = append(plans, plan{scope: scope, replacement: r, newBytes: sumItemBytes(r.items)})
	}

	if len(offenders) > 0 {
		return 0, &ScopeCapacityError{Offenders: offenders}
	}

	// Phase 1.5 + Phase 2 run with every shard the batch touches held in
	// write mode, in ascending shard-index order, to serialise against
	// /delete_scope, /wipe, and /rebuild. Without that mutual exclusion
	// the byte counter desyncs from Σ buf.bytes when one of those
	// destructive ops fires between snapshot and commit:
	//
	//   - /wipe does totalBytes.Swap(0), erasing this batch's pre-reservation.
	//     The drift comp then over-credits by oldSnapshot, leaving totalBytes
	//     too high by exactly the original scope size.
	//   - /rebuild does totalBytes.Store(newAggregate), same shape.
	//   - /delete_scope's per-scope Add(-scopeBytes) happens to balance the
	//     drift comp by accident, but only when the deleted scope's b.bytes
	//     equals the snapshot — fragile, and we'd rather not depend on that.
	//
	// /wipe and /rebuild lock every shard, so any subset we hold blocks
	// them. /delete_scope locks one shard, so it serialises against us
	// only when it targets a scope on a shard we hold; that is exactly
	// the case where the drift would matter (delete on a scope in our
	// batch).
	//
	// Concurrent appends/updates/etc. on the SAME scopes /warm is replacing
	// still proceed via getOrCreateScope: they take buf.mu, not the shard
	// lock, after a brief sh.mu.RLock for the lookup — and our shard write
	// lock blocks even that RLock until the batch is committed.
	//
	// The locked phase is wrapped in an inline closure so `defer
	// unlockShards(shards)` fires before emitWarmEvent below — the emit
	// recurses into appendOne(_events) which acquires the _events shard's
	// lock, and that shard might be among the ones we hold here. Without
	// the closure the emit-while-locked path would deadlock.
	n, err := func() (int, error) {
		scopeNames := make([]string, len(plans))
		for i, p := range plans {
			scopeNames[i] = p.scope
		}
		shards := s.shardsForScopes(scopeNames)
		lockShards(shards)
		defer unlockShards(shards)

		// Phase 1.5 — snapshot per-scope b.bytes (under each scope's RLock
		// so concurrent in-scope writers are observed consistently),
		// compute the net batch delta, and CAS-reserve it against the
		// store counter. Per-scope overhead is reserved here for plans
		// that create a NEW scope (one not yet in its shard); existing
		// scopes already have their overhead charged from when they were
		// first allocated.
		var totalDelta int64
		for i := range plans {
			sh := s.shardFor(plans[i].scope)
			var old int64
			if buf, ok := sh.scopes[plans[i].scope]; ok {
				buf.mu.RLock()
				old = buf.bytes
				buf.mu.RUnlock()
			} else {
				// New scope — Phase 2 will create it. Reserve the
				// per-scope overhead now so the cap check sees it.
				totalDelta += scopeBufferOverhead
			}
			plans[i].oldBytes = old
			totalDelta += plans[i].newBytes - old
		}
		if ok, current, max := s.reserveBytes(totalDelta); !ok {
			return 0, &StoreFullError{
				StoreBytes: current,
				AddedBytes: totalDelta,
				Cap:        max,
			}
		}

		// Phase 2 — create-on-demand and commit. We hold every relevant
		// shard in write mode so we can touch shard.scopes directly
		// (calling getOrCreateScope here would deadlock on its internal
		// RLock/Lock pair against our held write lock). Neither step can
		// fail, so either every scope is replaced or (if an earlier phase
		// aborted) none are.
		//
		// scopeCount delta accumulates here for new-scope inserts and is
		// applied once at the end. totalItems is handled by
		// commitReplacementPreReserved itself (it computes the per-scope
		// item delta under b.mu against the post-drift len(b.items)).
		var newScopes int64
		for _, p := range plans {
			sh := s.shardFor(p.scope)
			buf, ok := sh.scopes[p.scope]
			if !ok {
				buf = s.newscopeBuffer()
				sh.scopes[p.scope] = buf
				newScopes++
			}
			buf.commitReplacementPreReserved(p.replacement, p.newBytes, p.oldBytes)
		}
		if newScopes > 0 {
			s.scopeCount.Add(newScopes)
		}

		return len(plans), nil
	}()
	if err != nil {
		return n, err
	}
	// Gate the emit on actual work. An empty input map produces n=0
	// with no scope replacements; emitting `{op:"warm"}` then would
	// wake every _events subscriber and add a no-op entry to the
	// replay stream — pure noise. Mirrors the gate-on-success pattern
	// every other write-event helper uses (emitDeleteScopeEvent only
	// fires when the scope existed, single-item helpers only fire
	// when their commit actually landed).
	if n > 0 {
		s.emitWarmEvent()
	}
	return n, nil
}

// rebuildAll replaces every scope in the store with the supplied
// input. Same lock + detach + reserved-scope-recreate discipline as
// wipe; emits no `_events` entry for the same reason (cursor-rewind
// is the drainer's signal).
func (s *store) rebuildAll(grouped map[string][]Item) (int, int, error) {
	// Phase 1 — build every scope buffer off-map and distribute directly
	// into the per-shard maps that Phase 2 will swap in. If any scope
	// fails validation the existing store is left fully intact. Capacity
	// offenders are collected across the whole batch; any offender aborts
	// the rebuild.
	var newShardMaps [numShards]map[string]*scopeBuffer
	for i := range newShardMaps {
		newShardMaps[i] = make(map[string]*scopeBuffer)
	}
	totalItems := 0
	totalScopes := 0
	var totalNewBytes int64
	var offenders []ScopeCapacityOffender

	for scope, items := range grouped {
		if err := validateScope(scope, "/rebuild"); err != nil {
			return 0, 0, wrapValidation(err)
		}
		if isReservedScope(scope) {
			return 0, 0, fmt.Errorf("%w: scope '%s' is reserved and cannot appear in /rebuild input", ErrInvalidInput, scope)
		}
		for i := range items {
			if err := validateWriteItem(&items[i], "/rebuild", s.maxItemBytes); err != nil {
				return 0, 0, fmt.Errorf("scope '%s', item at index %d: %w", scope, i, err)
			}
			if items[i].Scope != scope {
				return 0, 0, fmt.Errorf("%w: scope '%s', item at index %d: item.scope %q does not match the map key", ErrInvalidInput, scope, i, items[i].Scope)
			}
		}
		if len(items) > s.defaultMaxItems {
			offenders = append(offenders, ScopeCapacityOffender{
				Scope: scope,
				Count: len(items),
				Cap:   s.defaultMaxItems,
			})
			continue
		}
		r, err := buildReplacementState(items)
		if err != nil {
			return 0, 0, fmt.Errorf("%w: scope '%s': %s", ErrInvalidInput, scope, err.Error())
		}
		// buf is not yet shared; bypass commitReplacement (which would try
		// to adjust the store counter) and initialize state directly. The
		// store counter is reset in phase 2 once the new maps are swapped.
		buf := s.newscopeBuffer()
		buf.items = r.items
		buf.byID = r.byID
		buf.bySeq = r.bySeq
		buf.lastSeq = r.lastSeq
		buf.bytes = sumItemBytes(r.items)
		newShardMaps[s.shardIdxFor(scope)][scope] = buf
		totalScopes++
		totalItems += len(r.items)
		totalNewBytes += buf.bytes
		// Per-scope overhead — every scope in the new map gets one
		// charge, just like getOrCreateScope does on the lazy path.
		totalNewBytes += scopeBufferOverhead
	}

	if len(offenders) > 0 {
		return 0, 0, &ScopeCapacityError{Offenders: offenders}
	}

	// Rebuild wipes the store, so the cap check is against the new total
	// (not a delta on top of the current counter). Include the
	// reserved-scope overhead because initReservedScopesLocked will
	// re-create those after the swap; otherwise an input that fills the
	// cap exactly would push past the cap once init runs.
	if totalNewBytes+reservedScopesOverhead > s.maxStoreBytes {
		return 0, 0, &StoreFullError{
			StoreBytes: 0,
			AddedBytes: totalNewBytes + reservedScopesOverhead,
			Cap:        s.maxStoreBytes,
		}
	}

	// Phase 2 — lock every shard, detach every existing buffer, swap
	// the maps, reset counters, release. The detach is what blocks a
	// stale-pointer concurrent /append from inflating totalBytes
	// against the freshly-reset counter (file-header invariant).
	s.lockAllShards()
	defer s.unlockAllShards()
	for i := range s.shards {
		for _, buf := range s.shards[i].scopes {
			buf.mu.Lock()
			buf.detached = true
			buf.store = nil
			buf.mu.Unlock()
		}
	}
	for i := range s.shards {
		s.shards[i].scopes = newShardMaps[i]
	}
	s.totalBytes.Store(totalNewBytes)
	s.totalItems.Store(int64(totalItems))
	s.scopeCount.Store(int64(totalScopes))
	// New buffers were built off-side without commitReplacement, so
	// the per-scope bump chain never fires — stamp store-wide
	// explicitly.
	s.bumpLastWriteTS(nowUnixMicro())
	s.initReservedScopesLocked()

	return totalScopes, totalItems, nil
}
