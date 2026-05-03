package scopecache

import (
	"errors"
	"strings"
)

// deleteGuardedTenant removes every scope under the prefix
// `_guarded:<capabilityID>:` and releases the bytes those scopes occupy
// (per-item bytes plus the per-scope overhead reserved at create time).
// Mirrors deleteScope's detach-then-account discipline so a concurrent
// in-flight write reaching a stale buf pointer either commits before
// detach (bytes counted in scopeBytes) or wakes up after and returns
// *ScopeDetachedError. Locks every shard for the whole sweep — same
// lock discipline as wipe(); revocation is rare, traffic is hot, and
// the alternative (per-shard locking with separate sweeps) would let a
// concurrent /append create a fresh tenant scope on a shard the sweep
// has already passed, leaving the tenant partially deleted.
//
// The capabilityID is treated as an opaque string (the handler enforces
// the 64-hex-char shape upstream); the store concatenates the literal
// prefix and matches with strings.HasPrefix. No validation here.
func (s *Store) deleteGuardedTenant(capabilityID string) (int, int, int64) {
	prefix := "_guarded:" + capabilityID + ":"

	s.lockAllShards()
	defer s.unlockAllShards()

	var (
		deletedScopes int
		deletedItems  int
		freedBytes    int64
	)

	for i := range s.shards {
		sh := &s.shards[i]
		for scope, buf := range sh.scopes {
			if !strings.HasPrefix(scope, prefix) {
				continue
			}
			buf.mu.Lock()
			itemCount := len(buf.items)
			deletedItems += itemCount
			scopeBytes := buf.bytes
			delete(sh.scopes, scope)
			// Combined into one Add so observers never see a transient state
			// with one released and the other still charged. Same shape as
			// deleteScope: item bytes + per-scope overhead, in lockstep.
			s.totalBytes.Add(-(scopeBytes + scopeBufferOverhead))
			s.totalItems.Add(-int64(itemCount))
			freedBytes += scopeBytes + scopeBufferOverhead
			buf.detached = true
			buf.store = nil
			buf.mu.Unlock()
			deletedScopes++
		}
	}

	s.scopeCount.Add(-int64(deletedScopes))
	if deletedScopes > 0 {
		s.bumpLastWriteTS(nowUnixMicro())
	}
	return deletedScopes, deletedItems, freedBytes
}

// wipe removes every scope from the store and resets the byte counter to
// zero in one atomic step. Each scope buffer is detached under its own
// write-lock before the store map is replaced, mirroring the /delete_scope
// pattern: any in-flight write waiting on buf.mu wakes up on a detached
// buffer and returns *ScopeDetachedError, so orphaned work cannot silently
// "succeed" into a buffer that nobody can ever read from again.
//
// freedBytes is captured via totalBytes.Swap(0) AFTER every buf has been
// detached, so it covers any bytes a concurrent /append committed through
// reserveBytes while wipe was walking the map.
//
// The caller — the /wipe handler — surfaces (scopeCount, totalItems, freedBytes)
// in the response so a client can verify how much state the call released.
func (s *Store) wipe() (int, int, int64) {
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

	return scopeCount, totalItems, freedBytes
}

func (s *Store) replaceScopes(grouped map[string][]Item) (int, error) {
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
		if scope == "" {
			return 0, errors.New("the 'scope' field is required")
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
			return 0, errors.New("scope '" + scope + "': " + err.Error())
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
	scopeNames := make([]string, len(plans))
	for i, p := range plans {
		scopeNames[i] = p.scope
	}
	shards := s.shardsForScopes(scopeNames)
	lockShards(shards)
	defer unlockShards(shards)

	// Phase 1.5 — snapshot per-scope b.bytes (under each scope's RLock so
	// concurrent in-scope writers are observed consistently), compute the
	// net batch delta, and CAS-reserve it against the store counter.
	// Per-scope overhead is reserved here for plans that create a NEW
	// scope (one not yet in its shard); existing scopes already have
	// their overhead charged from when they were first allocated.
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

	// Phase 2 — create-on-demand and commit. We hold every relevant shard
	// in write mode so we can touch shard.scopes directly (calling
	// getOrCreateScope here would deadlock on its internal RLock/Lock
	// pair against our held write lock). Neither step can fail, so either
	// every scope is replaced or (if an earlier phase aborted) none are.
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
}

func (s *Store) rebuildAll(grouped map[string][]Item) (int, int, error) {
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
			return 0, 0, errors.New("scope '" + scope + "': " + err.Error())
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
	// (not a delta on top of the current counter).
	if totalNewBytes > s.maxStoreBytes {
		return 0, 0, &StoreFullError{
			StoreBytes: 0,
			AddedBytes: totalNewBytes,
			Cap:        s.maxStoreBytes,
		}
	}

	// Phase 2 — lock every shard in ascending order, detach every existing
	// buffer, swap the shard maps, reset the byte counter, release. Detach
	// is essential: without it, a concurrent /append holding a stale buf
	// pointer obtained via getOrCreateScope would run AFTER the swap and
	// call reserveBytes against the freshly reset counter, permanently
	// inflating totalBytes (its item lands in an unreachable orphan
	// buffer). Mirrors wipe and /delete_scope; see scopeBuffer.detached.
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
	// rebuildAll constructs new buffers off-side via newscopeBuffer
	// (which only seeds b.lastWriteTS to the buffer's creation time)
	// and never goes through commitReplacement, so the per-scope bumps
	// don't fire here. Stamp store-wide explicitly.
	s.bumpLastWriteTS(nowUnixMicro())

	return totalScopes, totalItems, nil
}
