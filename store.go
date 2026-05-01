package scopecache

import (
	"errors"
	"hash/maphash"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// numShards splits the scope map into independently-locked shards.
// Power of 2 so the modulo collapses to a bitmask.
//
// Multi-shard operations (/wipe, /rebuild, /admin /delete_guarded,
// /warm) MUST acquire shard locks in ascending shard-index order to
// avoid deadlock with each other.
const (
	numShards = 32
	shardMask = numShards - 1
)

type scopeShard struct {
	mu     sync.RWMutex
	scopes map[string]*scopeBuffer
}

type Store struct {
	// shards splits the scope map into numShards independently-locked
	// buckets. Per-scope hot paths (getOrCreate, lookup, delete) take
	// only one shard's lock; multi-shard ops (/wipe, /rebuild, /warm,
	// /admin /delete_guarded) take a sorted subset in ascending index
	// order. Pre-sharding the store had a single store-wide RWMutex that
	// serialised every scope creation through one queue — see phase-4
	// finding "/append to a unique scope per request serializes on the
	// store-wide write lock".
	shards   [numShards]scopeShard
	hashSeed maphash.Seed

	defaultMaxItems int
	maxStoreBytes   int64
	maxItemBytes    int64

	// totalBytes tracks the running sum of every store-byte reservation:
	// approxItemSize per item plus scopeBufferOverhead per allocated
	// scope. Kept in an atomic so write paths can reserve against the
	// global budget without touching the store-level mutex; writes that
	// would push it past maxStoreBytes are rejected with StoreFullError.
	//
	// This is the authoritative counter for admission control and is
	// surfaced (converted to MiB) as approx_store_mb in /stats. It is
	// intentionally aligned with what reserveBytes enforces: the budget
	// a client sees in a 507 response, the value reported by /stats,
	// and the value compared against maxStoreBytes on the next write
	// all describe the same accounting model.
	//
	// totalBytes is leaner than scopeBuffer.approxSizeBytes — the
	// per-scope estimate folds in Go map/slice overhead for byID, bySeq
	// and the heat-bucket ring, which admission control deliberately
	// does NOT charge against the cap. Counting per-scope overhead is
	// what closes the empty-scope-spam DoS (see scopeBufferOverhead's
	// own comment); counting per-item Go heap overhead would be
	// honest-er to real memory pressure but at the cost of making the
	// cap arithmetic depend on internal data-structure layout. See
	// phase-4 finding "max_store_mb underestimates real memory cost at
	// high scope counts" for the open pre-v1.0 question.
	totalBytes atomic.Int64
}

func NewStore(c Config) *Store {
	c = c.WithDefaults()
	s := &Store{
		hashSeed:        maphash.MakeSeed(),
		defaultMaxItems: c.ScopeMaxItems,
		maxStoreBytes:   c.MaxStoreBytes,
		maxItemBytes:    c.MaxItemBytes,
	}
	for i := range s.shards {
		s.shards[i].scopes = make(map[string]*scopeBuffer)
	}
	return s
}

// shardIdxFor maps a scope name to a shard index in [0, numShards).
// maphash uses a per-process random seed, so distribution is uniform
// across shards and not predictable from the scope name — adversarial
// scope-name picking cannot deliberately collide on one shard.
func (s *Store) shardIdxFor(scope string) uint64 {
	return maphash.String(s.hashSeed, scope) & shardMask
}

func (s *Store) shardFor(scope string) *scopeShard {
	return &s.shards[s.shardIdxFor(scope)]
}

// shardsForScopes returns the unique set of shards covering the given
// scope names, in ascending shard-index order. Used by /warm to lock
// only the shards it touches (rather than all numShards) while still
// preserving the "all relevant shards held simultaneously" invariant
// that serialises against /wipe and /rebuild.
//
// `seen` is indexed by shard-index, so iterating it in order produces
// the ascending sequence directly — no sort, no intermediate index
// slice.
func (s *Store) shardsForScopes(scopes []string) []*scopeShard {
	var seen [numShards]bool
	for _, scope := range scopes {
		seen[s.shardIdxFor(scope)] = true
	}
	out := make([]*scopeShard, 0, numShards)
	for i := 0; i < numShards; i++ {
		if seen[i] {
			out = append(out, &s.shards[i])
		}
	}
	return out
}

// lockAllShards / unlockAllShards / lockShards / unlockShards are the
// helpers every multi-shard mutation MUST use. They encode the
// ascending-shard-index lock order that the `numShards` comment block
// above declares — relying on each call-site to spell out the loop
// correctly is exactly how a future op silently introduces a deadlock
// against /wipe, /rebuild, or /warm.
//
// Unlock order is forward. Go's sync.Mutex has no unlock-order
// requirement; the only correctness constraint is consistent ascending
// acquisition (see the numShards comment block above).

// lockAllShards locks every shard in ascending index order. Used by
// /wipe, /rebuild, /admin /delete_guarded.
func (s *Store) lockAllShards() {
	for i := range s.shards {
		s.shards[i].mu.Lock()
	}
}

// unlockAllShards is the matching release for lockAllShards.
func (s *Store) unlockAllShards() {
	for i := range s.shards {
		s.shards[i].mu.Unlock()
	}
}

// lockShards locks the given subset. The slice MUST already be in
// ascending shard-index order — `shardsForScopes` returns it that way.
// Used by /warm so it only blocks the shards its batch touches.
func lockShards(shards []*scopeShard) {
	for _, sh := range shards {
		sh.mu.Lock()
	}
}

// unlockShards is the matching release for lockShards.
func unlockShards(shards []*scopeShard) {
	for _, sh := range shards {
		sh.mu.Unlock()
	}
}

// reserveBytes atomically adjusts the store byte counter by delta, enforcing
// the cap for positive deltas. Negative deltas (releases) always succeed.
// Returns (ok, totalAfterAttempt, cap). Positive deltas use a CAS loop so
// concurrent /append writers never collectively over-commit the cap.
func (s *Store) reserveBytes(delta int64) (bool, int64, int64) {
	if delta <= 0 {
		n := s.totalBytes.Add(delta)
		return true, n, s.maxStoreBytes
	}
	for {
		current := s.totalBytes.Load()
		next := current + delta
		if next > s.maxStoreBytes {
			return false, current, s.maxStoreBytes
		}
		if s.totalBytes.CompareAndSwap(current, next) {
			return true, next, s.maxStoreBytes
		}
	}
}

// scopeBufferOverhead is the byte-cost the cache charges per allocated
// scope, on top of the scope's items. Covers the *scopeBuffer struct
// itself (mutex, slice header, two map headers, heat-bucket
// ringbuffer, scope-name string in its shard's map), plus slack for the
// per-key map entry overhead. A conservative single-KiB number.
//
// Including it in totalBytes admission control means an attacker
// holding a valid token who tries to spam empty scopes within their
// `_guarded:<capId>:*` prefix will hit the store-byte cap (default
// 100 MiB → ~100k empty scopes) and 507 instead of growing memory
// unbounded. Without this, totalBytes only counts payload bytes —
// 1M empty scopes consume ~1 GiB of struct memory but report
// approx_store_mb = 0.
//
// This is also a /stats accuracy improvement: approx_store_mb now
// matches actual memory pressure, not just item bytes.
const scopeBufferOverhead = 1024

// newscopeBuffer builds a fresh scopeBuffer bound to this store so its
// mutations can participate in byte tracking. Keeping this helper on the
// store means every production path creates bound buffers; tests that
// exercise scopeBuffer in isolation use newscopeBuffer directly and
// accept that byte tracking is a no-op there.
func (s *Store) newscopeBuffer() *scopeBuffer {
	b := newscopeBuffer(s.defaultMaxItems)
	b.store = s
	return b
}

func (s *Store) getOrCreateScope(scope string) (*scopeBuffer, error) {
	buf, _, err := s.getOrCreateScopeTrackingCreated(scope)
	return buf, err
}

// getOrCreateScopeTrackingCreated is the variant used by the atomic
// write paths (appendOne, upsertOne, counterAddOne) that need to know
// whether the buffer was freshly allocated by this call. Callers use
// the `created` flag to roll the empty scope back when the subsequent
// item-byte reservation fails — see cleanupIfEmptyAndUnused. All other
// callers go through getOrCreateScope, which discards the flag.
func (s *Store) getOrCreateScopeTrackingCreated(scope string) (*scopeBuffer, bool, error) {
	if scope == "" {
		return nil, false, errors.New("the 'scope' field is required")
	}

	sh := s.shardFor(scope)

	sh.mu.RLock()
	buf, ok := sh.scopes[scope]
	sh.mu.RUnlock()
	if ok {
		return buf, false, nil
	}

	// Allocate the buffer BEFORE taking the shard write lock — the
	// expensive part (struct init, map slot reservation, GC pressure
	// at high create rates) happens while other goroutines can still
	// progress on this shard. The byte reservation stays INSIDE the
	// lock so /wipe's totalBytes.Swap(0) and /rebuild's
	// totalBytes.Store() (both held under all shard locks) cannot
	// race with our reservation: while we hold this shard's lock,
	// neither op can run, and our reservation is observed atomically
	// alongside the map insert.
	//
	// Race-loss path (a concurrent goroutine inserted the same scope
	// while we were allocating) discards the unused buffer for GC and
	// makes no reservation. In the unique-scope-per-write workload
	// that drove this rewrite, race-loss is essentially never (every
	// scope name is distinct); in same-scope writes the caller hits
	// the RLock fast-path above and never reaches this branch.
	preBuf := s.newscopeBuffer()

	sh.mu.Lock()
	if existing, ok := sh.scopes[scope]; ok {
		sh.mu.Unlock()
		return existing, false, nil
	}
	if ok, current, max := s.reserveBytes(scopeBufferOverhead); !ok {
		sh.mu.Unlock()
		return nil, false, &StoreFullError{
			StoreBytes: current,
			AddedBytes: scopeBufferOverhead,
			Cap:        max,
		}
	}
	sh.scopes[scope] = preBuf
	sh.mu.Unlock()
	return preBuf, true, nil
}

// cleanupIfEmptyAndUnused rolls back a freshly-created scope when the
// caller's subsequent item-byte reservation failed. Without this, every
// failed write to a new scope would leak scopeBufferOverhead bytes onto
// the store-byte cap, which a multi-tenant attacker could exploit to
// fill the cap with empty scopes (DoS).
//
// Three guards prevent collateral damage:
//   - cur == buf: another caller may have wiped+recreated the scope
//     between our create and our cleanup; only delete if our buffer is
//     still the one mapped at this name.
//   - len(buf.items) == 0: a concurrent writer that grabbed our buf
//     pointer through the fast path may have committed an item before
//     we acquired buf.mu; if so, the scope is no longer "empty" and we
//     must leave it alone.
//   - detached + store=nil: matches deleteScope's pattern. Any
//     concurrent in-flight writer that wakes up on this buf after we
//     released the locks returns *ScopeDetachedError, same semantics
//     as a /delete_scope race.
func (s *Store) cleanupIfEmptyAndUnused(scope string, buf *scopeBuffer) {
	sh := s.shardFor(scope)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	cur, ok := sh.scopes[scope]
	if !ok || cur != buf {
		return
	}

	buf.mu.Lock()
	defer buf.mu.Unlock()

	if len(buf.items) != 0 {
		return
	}

	delete(sh.scopes, scope)
	s.totalBytes.Add(-scopeBufferOverhead)
	buf.detached = true
	buf.store = nil
}

// appendOne is the atomic /append write-path. It creates the target
// scope on demand, reserves item bytes, commits the item, and rolls
// back the empty scope on item-reservation failure so a 507 cannot
// leak per-scope overhead onto the store-byte cap. See
// cleanupIfEmptyAndUnused for the rollback semantics.
func (s *Store) appendOne(item Item) (Item, error) {
	buf, created, err := s.getOrCreateScopeTrackingCreated(item.Scope)
	if err != nil {
		return Item{}, err
	}
	result, appendErr := buf.appendItem(item)
	if appendErr != nil && created {
		s.cleanupIfEmptyAndUnused(item.Scope, buf)
	}
	return result, appendErr
}

// upsertOne is the atomic /upsert write-path; same rollback contract
// as appendOne. Returns (item, created, err) where created reflects
// the upsert outcome, not the scope-creation outcome.
func (s *Store) upsertOne(item Item) (Item, bool, error) {
	buf, scopeCreated, err := s.getOrCreateScopeTrackingCreated(item.Scope)
	if err != nil {
		return Item{}, false, err
	}
	result, itemCreated, upsertErr := buf.upsertByID(item)
	if upsertErr != nil && scopeCreated {
		s.cleanupIfEmptyAndUnused(item.Scope, buf)
	}
	return result, itemCreated, upsertErr
}

// counterAddOne is the atomic /counter_add write-path; same rollback
// contract as appendOne. Returns (value, created, err) where created
// reflects the counter outcome, not the scope-creation outcome.
func (s *Store) counterAddOne(scope, id string, by int64) (int64, bool, error) {
	buf, scopeCreated, err := s.getOrCreateScopeTrackingCreated(scope)
	if err != nil {
		return 0, false, err
	}
	value, counterCreated, addErr := buf.counterAdd(scope, id, by)
	if addErr != nil && scopeCreated {
		s.cleanupIfEmptyAndUnused(scope, buf)
	}
	return value, counterCreated, addErr
}

// updateOne mutates the payload of an item addressed by scope+id or
// scope+seq. Returns (updated_count, err); a missing scope is reported
// as (0, nil), the same wire shape an absent id/seq inside an existing
// scope would produce. The caller-side validator enforces the
// id-xor-seq invariant; updateOne assumes id != "" picks the id path
// and otherwise routes by seq.
func (s *Store) updateOne(item Item) (int, error) {
	buf, ok := s.getScope(item.Scope)
	if !ok {
		return 0, nil
	}
	if item.ID != "" {
		return buf.updateByID(item.ID, item.Payload)
	}
	return buf.updateBySeq(item.Seq, item.Payload)
}

// deleteOne removes a single item by scope+id or scope+seq. Returns
// (deleted_count, err); missing scope reports (0, nil) — same miss
// shape as updateOne. Validator-enforced id-xor-seq invariant.
func (s *Store) deleteOne(scope, id string, seq uint64) (int, error) {
	buf, ok := s.getScope(scope)
	if !ok {
		return 0, nil
	}
	if id != "" {
		return buf.deleteByID(id)
	}
	return buf.deleteBySeq(seq)
}

// deleteUpTo removes every item in the scope with seq <= maxSeq.
// Returns (deleted_count, err); missing scope reports (0, nil).
func (s *Store) deleteUpTo(scope string, maxSeq uint64) (int, error) {
	buf, ok := s.getScope(scope)
	if !ok {
		return 0, nil
	}
	return buf.deleteUpToSeq(maxSeq)
}

// head returns up to `limit` oldest items in the scope with seq >
// afterSeq. Returns (items, truncated, scopeFound). recordHeat=true
// stamps the scope's read-heat (only on non-empty result, mirroring
// the prior handler behaviour). A missing scope reports (nil, false,
// false); a found-but-empty window reports (empty, false, true) —
// handlers translate that into hit:false / count:0.
func (s *Store) head(scope string, afterSeq uint64, limit int, recordHeat bool) ([]Item, bool, bool) {
	buf, ok := s.getScope(scope)
	if !ok {
		return nil, false, false
	}
	items, truncated := buf.sinceSeq(afterSeq, limit)
	if recordHeat && len(items) > 0 {
		buf.recordRead(nowUnixMicro())
	}
	return items, truncated, true
}

// tail returns up to `limit` newest items in the scope, skipping the
// first `offset`. Same return shape and recordHeat semantics as head.
func (s *Store) tail(scope string, limit, offset int, recordHeat bool) ([]Item, bool, bool) {
	buf, ok := s.getScope(scope)
	if !ok {
		return nil, false, false
	}
	items, truncated := buf.tailOffset(limit, offset)
	if recordHeat && len(items) > 0 {
		buf.recordRead(nowUnixMicro())
	}
	return items, truncated, true
}

// get returns one item by scope+id or scope+seq. (item, found) — the
// found flag is true only when both the scope and the item exist.
// recordHeat fires on hit only.
func (s *Store) get(scope, id string, seq uint64, recordHeat bool) (Item, bool) {
	buf, ok := s.getScope(scope)
	if !ok {
		return Item{}, false
	}
	var item Item
	var found bool
	if id != "" {
		item, found = buf.getByID(id)
	} else {
		item, found = buf.getBySeq(seq)
	}
	if !found {
		return Item{}, false
	}
	if recordHeat {
		buf.recordRead(nowUnixMicro())
	}
	return item, true
}

// render returns the bytes /render writes on the wire, peeling the
// renderBytes shortcut for JSON-string payloads at the Store boundary
// so the handler does not need to know the renderBytes field exists.
// (bytes, found) — same hit semantics as get; recordHeat fires on hit.
func (s *Store) render(scope, id string, seq uint64, recordHeat bool) ([]byte, bool) {
	buf, ok := s.getScope(scope)
	if !ok {
		return nil, false
	}
	var item Item
	var found bool
	if id != "" {
		item, found = buf.getByID(id)
	} else {
		item, found = buf.getBySeq(seq)
	}
	if !found {
		return nil, false
	}
	if recordHeat {
		buf.recordRead(nowUnixMicro())
	}
	if item.renderBytes != nil {
		return item.renderBytes, true
	}
	return item.Payload, true
}

// ensureScope returns the named scope, creating an empty buffer if it
// does not yet exist. Used by API-layer features that lazily provision
// cache-owned infrastructure scopes (e.g. observability counters)
// without requiring operator pre-provisioning. Idempotent — safe to
// call on every request; cost is one map lookup under the read-lock
// when the scope already exists.
//
// Unlike getOrCreateScope, this method does not validate the scope
// name and is intended only for cache-internal infrastructure scopes
// whose names are compile-time constants on the caller side.
//
// Reserves scopeBufferOverhead just like getOrCreateScope on the
// create path. This is required for accounting symmetry: deleteScope
// unconditionally subtracts (scopeBytes + scopeBufferOverhead), so a
// later admin-driven delete of these scopes would otherwise drift
// totalBytes scopeBufferOverhead bytes too low per cycle (potentially
// negative) — the bytes-counter invariant is "totalBytes == sum of
// reservations". Returns nil when the cap is exhausted; callers are
// expected to treat such infrastructure writes as best-effort and
// silently skip on nil (observability is not auth — losing a counter
// must never block a legitimate request).
func (s *Store) ensureScope(scope string) *scopeBuffer {
	sh := s.shardFor(scope)

	sh.mu.RLock()
	buf, ok := sh.scopes[scope]
	sh.mu.RUnlock()
	if ok {
		return buf
	}

	sh.mu.Lock()
	defer sh.mu.Unlock()

	buf, ok = sh.scopes[scope]
	if ok {
		return buf
	}

	if ok, _, _ := s.reserveBytes(scopeBufferOverhead); !ok {
		return nil
	}

	buf = s.newscopeBuffer()
	sh.scopes[scope] = buf
	return buf
}

func (s *Store) getScope(scope string) (*scopeBuffer, bool) {
	sh := s.shardFor(scope)
	sh.mu.RLock()
	defer sh.mu.RUnlock()

	buf, ok := sh.scopes[scope]
	return buf, ok
}

func (s *Store) deleteScope(scope string) (int, bool) {
	if scope == "" {
		return 0, false
	}

	sh := s.shardFor(scope)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	buf, ok := sh.scopes[scope]
	if !ok {
		return 0, false
	}

	// Hold buf.mu as a write lock across the whole sequence so an in-flight
	// mutator on this buf (via a stale pointer obtained before we ran) either
	// completes before we touch the counter or waits until after we're done.
	// Crucially we also detach the buffer: any write that wakes up afterwards
	// returns *ScopeDetachedError instead of silently writing into an orphan
	// that is unreachable and about to be GC'd. store is cleared too so any
	// remaining code path that survives the detach check still skips
	// store-counter accounting.
	buf.mu.Lock()
	itemCount := len(buf.items)
	scopeBytes := buf.bytes
	delete(sh.scopes, scope)
	// Release item bytes AND the per-scope overhead reserved at create
	// time. Combined into one Add so observers never see a transient
	// state with one released and the other still charged.
	s.totalBytes.Add(-(scopeBytes + scopeBufferOverhead))
	buf.detached = true
	buf.store = nil
	buf.mu.Unlock()
	return itemCount, true
}

// storeStats is the typed snapshot of the store. stats() returns it so the
// /stats handler can flatten it into orderedFields for the wire, and so any
// in-package caller (tests, future adapters) can read fields directly.
type storeStats struct {
	ScopeCount    int
	TotalItems    int
	ApproxStoreMB MB
	MaxStoreMB    MB
	Scopes        map[string]scopeStats
}

func (s *Store) stats() storeStats {
	// Per-shard snapshot: each shard is RLock'd, walked, RUnlock'd in
	// turn. scope_count and total_items still reflect the same set of
	// scopes that were captured into Scopes (loop derives them from
	// the same per-shard walk), so the response is internally
	// consistent even though it is no longer a global instant — a
	// concurrent /append on shard 5 may land after we release shard 5
	// and before we take shard 6, but the response always agrees with
	// itself. approx_store_mb is read once from the atomic counter so
	// it can show the post-release total even when the per-scope
	// walk shows the pre-release set; that mismatch existed pre-
	// sharding too (totalBytes is read after the lock, by design) and
	// /stats has always been an approximation.
	now := nowUnixMicro()
	byScope := make(map[string]scopeStats)
	totalItems := 0

	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.RLock()
		for scope, buf := range sh.scopes {
			st := buf.stats(now)
			byScope[scope] = st
			totalItems += st.ItemCount
		}
		sh.mu.RUnlock()
	}

	return storeStats{
		ScopeCount:    len(byScope),
		TotalItems:    totalItems,
		ApproxStoreMB: MB(s.totalBytes.Load()),
		MaxStoreMB:    MB(s.maxStoreBytes),
		Scopes:        byScope,
	}
}

func (s *Store) listScopes() map[string]*scopeBuffer {
	out := make(map[string]*scopeBuffer)
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.RLock()
		for k, v := range sh.scopes {
			out[k] = v
		}
		sh.mu.RUnlock()
	}
	return out
}

// scopeCandidates returns the eviction-candidate list used by
// /delete_scope_candidates: every scope with its heat metadata,
// optionally filtered to scopes older than `hours`, sorted by
// last_access_ts ascending (stalest first), then truncated to
// `limit`. Walks every shard via listScopes — per-scope-consistent
// but not a global atomic snapshot, same caveat as stats(). Advisory
// data, not authoritative.
func (s *Store) scopeCandidates(hours int64, limit int) []Candidate {
	now := nowUnixMicro()
	minAgeMicros := hours * int64(time.Hour/time.Microsecond)

	scopes := s.listScopes()
	list := make([]Candidate, 0, len(scopes))
	for name, buf := range scopes {
		st := buf.stats(now)
		if hours > 0 && now-st.CreatedTS < minAgeMicros {
			continue
		}
		list = append(list, Candidate{
			Scope:           name,
			CreatedTS:       st.CreatedTS,
			LastAccessTS:    st.LastAccessTS,
			Last7dReadCount: st.Last7DReadCount,
			ItemCount:       st.ItemCount,
			ApproxScopeMB:   st.ApproxScopeMB,
		})
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].LastAccessTS < list[j].LastAccessTS
	})
	if len(list) > limit {
		list = list[:limit]
	}
	return list
}
