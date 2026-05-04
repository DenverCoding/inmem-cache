package scopecache

import (
	"errors"
	"hash/maphash"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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

	// Reserved-scope derived caps. Computed once at NewStore time from
	// Config so write-path checks can read these directly without
	// re-deriving on every call. See types.go EventsConfig / InboxConfig
	// for the shape rationale (why `_events` has no item-count knob
	// and `_inbox` has both item-count and item-byte knobs).
	//
	//   - eventsMaxItemBytes: MaxItemBytes + EventsItemEnvelopeOverhead.
	//                         Derived, not a knob — an event entry must
	//                         always fit the user-write that produced it.
	//   - inboxMaxItems:      Inbox.MaxItems (default = ScopeMaxItems).
	//                         Operator-tunable independently of the global.
	//   - inboxMaxItemBytes:  Inbox.MaxItemBytes (default = InboxMaxItemBytes,
	//                         i.e. 64 KiB). Operator-tunable; typically
	//                         smaller than the global MaxItemBytes because
	//                         `_inbox` is for small fan-in events, not
	//                         arbitrary user payloads.
	//
	// `_events` does NOT have an item-count cap: ScopeMaxItems is
	// bypassed for that scope (best-effort observability — the only
	// real begrenzer is the global byte budget on MaxStoreBytes).
	eventsMaxItemBytes int64
	inboxMaxItems      int
	inboxMaxItemBytes  int64

	// eventsMode controls auto-populate of the reserved `_events` scope.
	// See types.go EventsMode for the three states. Resolved from
	// Config.Events.Mode at NewStore time. The auto-populate hooks
	// (Phase A "Subscribe + event drain architecture") consult this
	// field; with eventsMode == EventsModeOff (the default) the
	// hooks short-circuit immediately and the cache behaves
	// identically to a pre-Phase-A build.
	eventsMode EventsMode

	// eventsDropsTotal counts events that the cache attempted to
	// auto-populate into `_events` but had to drop because the byte
	// budget was already saturated (or, defensively, because
	// json.Marshal of the writeEvent failed). Incremented atomically
	// inside emitAppendEvent (and future per-op emit helpers) when
	// the inner s.appendOne to `_events` returns any error.
	// Surfaced on /stats by the future enrichment step (Phase A
	// roadmap §5e); read by tests directly today.
	//
	// A drop here NEVER affects the user-visible result of the
	// underlying mutation: the user-write already committed before
	// the emit ran. The drop is a degraded observability signal,
	// not a degraded primary operation.
	eventsDropsTotal atomic.Int64

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
	// phase-4 finding "approx_store_mb under-reports real memory cost
	// at high scope counts" for the open pre-v1.0 question.
	totalBytes atomic.Int64

	// totalItems and scopeCount mirror the totalBytes pattern: every
	// write path that creates or removes an item / scope adjusts the
	// matching atomic in lockstep with its per-scope mutation. This
	// makes /stats O(1) — three atomic loads and zero shard walks,
	// independent of how many scopes the store holds.
	//
	// Lockstep discipline (same as totalBytes):
	//   - item insert paths: insertNewItemLocked + counterAdd create
	//     branch do totalItems.Add(+1) under b.mu, alongside b.bytes
	//     accounting.
	//   - item delete paths: deleteIndexLocked does totalItems.Add(-1);
	//     deleteUpToSeq does Add(-N) for the batch.
	//   - bulk replace paths: commitReplacement(PreReserved) compute
	//     itemDelta = len(r.items) - len(b.items) at commit time and
	//     Add the delta. No pre-reservation needed (no store-wide item
	//     cap), so concurrent stale-pointer drift is handled implicitly
	//     by reading len(b.items) at commit time rather than a snapshot.
	//   - scope create paths: getOrCreateScopeTrackingCreated and
	//     ensureScope do scopeCount.Add(+1) inside the shard-write-lock
	//     critical section, alongside the map insert.
	//   - scope delete paths: cleanupIfEmptyAndUnused, deleteScope and
	//     deleteGuardedTenant do scopeCount.Add(-N) and totalItems
	//     .Add(-itemCount) inside their shard-write-lock section.
	//   - bulk reset paths: wipe does Store(0) on both; rebuildAll does
	//     Store(totalItems) / Store(totalScopes) under all-shard write
	//     lock, same shape as totalBytes.Store(totalNewBytes).
	//
	// Forgetting one of these is exactly the same class of bug as
	// forgetting a totalBytes update — corrupts /stats silently. The
	// invariant test (TestStore_StatsCounters_Invariant) walks every
	// shard and asserts totalItems == Σ len(buf.items) and scopeCount
	// == Σ len(sh.scopes); run it after touching any of the paths
	// above.
	totalItems atomic.Int64
	scopeCount atomic.Int64

	// lastWriteTS is a microsecond timestamp surfaced on /stats as the
	// "freshness" signal: a single number a polling client can compare
	// against its previous value to skip a refetch when nothing has
	// changed. Updated via bumpLastWriteTS (CAS-max) from every place
	// that bumps per-scope b.lastWriteTS, plus the store-level
	// destructive paths (deleteScope, wipe, deleteGuardedTenant,
	// rebuildAll) that don't go through a per-scope bump.
	//
	// CAS-max (rather than naive Store) is required: two writes in
	// different scopes can compute time.Now().UnixMicro() out of order
	// across CPUs, then a naive store could leave the counter at the
	// older value, lying about freshness. The CAS loop guarantees the
	// counter only ever advances.
	//
	// Default value is 0 (epoch start) — distinguishable from any real
	// timestamp the cache would produce. Cleared back to 0 by /wipe
	// only conceptually; in practice /wipe immediately bumps it to its
	// own commit time so a client polling right after a wipe still
	// sees a non-zero "something happened" tick.
	lastWriteTS atomic.Int64
}

// bumpLastWriteTS advances s.lastWriteTS to nowUs if and only if
// nowUs > current. Concurrent writers from different scopes can
// compute time.Now().UnixMicro() out of order across CPUs; a naive
// Store(nowUs) would let the older timestamp overwrite the newer
// one and lie about freshness. The CAS loop guarantees monotonicity.
//
// Loop terminates in 1 iteration when uncontested, and in O(few)
// iterations under contention because each retry re-loads a strictly
// greater current value. No lock taken — safe to call from inside
// b.mu critical sections (which is the common case: write paths
// stamp b.lastWriteTS = ts under b.mu, then call this with the same
// ts).
func (s *Store) bumpLastWriteTS(nowUs int64) {
	for {
		cur := s.lastWriteTS.Load()
		if nowUs <= cur {
			return
		}
		if s.lastWriteTS.CompareAndSwap(cur, nowUs) {
			return
		}
	}
}

// reservedScopeNames lists the cache-reserved scope names that are
// pre-created at boot, re-created after /wipe and /rebuild, and
// protected from scope-level destruction. Item-level operations
// (/append, /delete, /delete_up_to, /get, /head, /tail, /render)
// remain open on these scopes — the drainer pattern requires
// /delete_up_to and /tail on _events to function, and apps freely
// /append into _inbox. See types.go for per-scope rationale and
// CLAUDE.md "Two reserved scopes" for the principle.
var reservedScopeNames = [...]string{EventsScopeName, InboxScopeName}

// reservedScopesOverhead is the byte cost of pre-creating every
// reserved scope. Used by rebuildAll's cap check so a /rebuild input
// that fills the cache exactly doesn't blow past the cap when init
// re-creates the reserved scopes after the swap.
const reservedScopesOverhead = int64(len(reservedScopeNames)) * scopeBufferOverhead

// isReservedScope reports whether scope is a cache-reserved name.
// Used by /delete_scope, /warm and /rebuild to reject scope-level
// destructive operations targeting reserved scopes. See
// reservedScopeNames for the full discipline.
func isReservedScope(scope string) bool {
	for _, name := range reservedScopeNames {
		if scope == name {
			return true
		}
	}
	return false
}

// maxItemBytesFor returns the per-item byte cap that applies to writes
// against scope. The reserved scopes have caps decoupled from the
// global one (see types.go EventsConfig / InboxConfig and RFC §2.6):
//
//   - `_events` uses eventsMaxItemBytes (= MaxItemBytes + EventsItemEnvelopeOverhead)
//   - `_inbox`  uses inboxMaxItemBytes  (operator-tunable, default 64 KiB)
//   - everything else uses maxItemBytes (the global Config.MaxItemBytes)
//
// Called by handlers that mutate items (currently /append) right before
// the validator's checkItemSize. Other write handlers (/upsert,
// /update, /counter_add) reject reserved scopes at the validator
// before reaching this path, so they always see the global cap.
func (s *Store) maxItemBytesFor(scope string) int64 {
	switch scope {
	case EventsScopeName:
		return s.eventsMaxItemBytes
	case InboxScopeName:
		return s.inboxMaxItemBytes
	default:
		return s.maxItemBytes
	}
}

// maxItemsFor returns the per-scope item-count cap to install on a
// freshly-created buffer for scope. Mirrors maxItemBytesFor's
// per-reserved-scope dispatch, with one extra wrinkle: `_events`
// returns the unboundedScopeMaxItems sentinel so its appendItem path
// skips the count cap entirely (best-effort observability — only the
// global byte budget gates writes there). Used by
// initReservedScopes(Locked) to install the right cap at boot /
// after wipe / after rebuild.
func (s *Store) maxItemsFor(scope string) int {
	switch scope {
	case EventsScopeName:
		return unboundedScopeMaxItems
	case InboxScopeName:
		return s.inboxMaxItems
	default:
		return s.defaultMaxItems
	}
}

// unboundedScopeMaxItems is the sentinel value for "no item-count cap
// on this scope" stored in scopeBuffer.maxItems. The write paths in
// buffer_write.go treat 0 as "skip the count check" — only the
// reserved `_events` scope is created with this sentinel because
// best-effort observability writes are gated by the global byte
// budget, not by an arbitrary item-count cap.
const unboundedScopeMaxItems = 0

// initReservedScopes pre-creates every entry in reservedScopeNames at
// NewStore time. Idempotent and safe to call from NewStore (single-
// threaded, no contention). For paths that already hold all-shard
// write locks (wipe, rebuildAll), use initReservedScopesLocked
// instead — calling this version under those locks would deadlock on
// the per-shard write lock acquisition.
//
// Boot-time pre-creation is NOT counted as cache activity:
// s.lastWriteTS is NOT bumped, and the per-scope buf.lastWriteTS is
// reset to 0. A polling client that uses lastWriteTS as a "have I
// seen this cache before" sentinel should see 0 on a fresh boot;
// the invariant `s.lastWriteTS >= max(buf.lastWriteTS)` requires
// every scope's tick to also be 0 in that pristine state. The first
// real write is what advances both counters.
func (s *Store) initReservedScopes() {
	for _, name := range reservedScopeNames {
		sh := s.shardFor(name)
		sh.mu.Lock()
		if _, exists := sh.scopes[name]; !exists {
			if ok, _, _ := s.reserveBytes(scopeBufferOverhead); ok {
				buf := s.newscopeBuffer()
				// Per-scope cap dispatch: _events gets the unbounded
				// sentinel, _inbox gets the operator-tunable cap.
				// See maxItemsFor for the rationale.
				buf.maxItems = s.maxItemsFor(name)
				buf.lastWriteTS = 0 // bootstrap: no writes yet
				sh.scopes[name] = buf
				s.scopeCount.Add(1)
			}
		}
		sh.mu.Unlock()
	}
}

// initReservedScopesLocked re-creates the reserved scopes assuming the
// caller holds every shard's write lock (wipe, rebuildAll). Mirrors
// the create path of ensureScope but skips the lock dance since the
// caller already owns every shard. Idempotent: a scope that somehow
// survived the caller's reset (defensive against rebuildAll input
// slipping through validation) is left alone, no double-reserve.
//
// Like initReservedScopes, this path does NOT bump s.lastWriteTS:
// the surrounding wipe()/rebuildAll() already bumped store-wide for
// the destructive event itself. The per-scope buf.lastWriteTS is
// likewise NOT advanced beyond the surrounding-event tick — re-
// creating the scopes in lockstep is part of the same logical
// operation, not a separate one. We therefore keep buf.lastWriteTS
// at the surrounding-event value (which the wipe/rebuild call
// already established as s.lastWriteTS).
func (s *Store) initReservedScopesLocked() {
	for _, name := range reservedScopeNames {
		sh := s.shardFor(name)
		if _, exists := sh.scopes[name]; exists {
			continue
		}
		buf := s.newscopeBuffer()
		// Per-scope cap dispatch matches initReservedScopes; see
		// maxItemsFor for the rationale.
		buf.maxItems = s.maxItemsFor(name)
		buf.lastWriteTS = s.lastWriteTS.Load() // align with surrounding event
		sh.scopes[name] = buf
		s.totalBytes.Add(scopeBufferOverhead)
		s.scopeCount.Add(1)
	}
}

func NewStore(c Config) *Store {
	c = c.WithDefaults()
	s := &Store{
		hashSeed:           maphash.MakeSeed(),
		defaultMaxItems:    c.ScopeMaxItems,
		maxStoreBytes:      c.MaxStoreBytes,
		maxItemBytes:       c.MaxItemBytes,
		eventsMaxItemBytes: c.MaxItemBytes + EventsItemEnvelopeOverhead,
		inboxMaxItems:      c.Inbox.MaxItems,
		inboxMaxItemBytes:  c.Inbox.MaxItemBytes,
		eventsMode:         c.Events.Mode,
	}
	for i := range s.shards {
		s.shards[i].scopes = make(map[string]*scopeBuffer)
	}
	// Pre-create reserved scopes so subscribers can attach immediately
	// at boot (before any writes happen) and so the future log-scope
	// auto-populate hooks (Phase A "Subscribe + log-scope drain
	// architecture") have a destination ready.
	s.initReservedScopes()
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
	s.scopeCount.Add(1)
	// Scope creation changes scope_count — a /stats field — so bump
	// the freshness tick. Most callers immediately follow with a write
	// that bumps again with a strictly later timestamp; the CAS-max
	// makes the second bump a no-op or a tick advance, either way
	// honest. The transient "scope created but item insert failed"
	// rollback path leaves a one-tick blip that resolves by the next
	// real write — harmless under the polling contract (clients see a
	// tick, refetch, find nothing changed, move on).
	s.bumpLastWriteTS(preBuf.lastWriteTS)
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
	s.scopeCount.Add(-1)
	buf.detached = true
	buf.store = nil
}

// appendOne is the atomic /append write-path. It creates the target
// scope on demand, reserves item bytes, commits the item, and rolls
// back the empty scope on item-reservation failure so a 507 cannot
// leak per-scope overhead onto the store-byte cap. See
// cleanupIfEmptyAndUnused for the rollback semantics.
//
// On a successful commit the method invokes emitAppendEvent to
// auto-populate `_events` (Phase A step 5b). The emit happens
// AFTER buf.appendItem has returned (b.mu released): this is the
// "capture-under-lock, emit-outside-lock" pattern. With
// Config.Events.Mode == Off (the default) the emit is a one-branch
// no-op; with Notify or Full it does a second appendOne into the
// `_events` scope, which is recursion-guarded inside the helper.
// See events.go for the full discipline.
func (s *Store) appendOne(item Item) (Item, error) {
	buf, created, err := s.getOrCreateScopeTrackingCreated(item.Scope)
	if err != nil {
		return Item{}, err
	}
	result, appendErr := buf.appendItem(item)
	if appendErr != nil {
		if created {
			s.cleanupIfEmptyAndUnused(item.Scope, buf)
		}
		return result, appendErr
	}
	s.emitAppendEvent(result.Scope, result.ID, result.Seq, result.Ts, result.Payload)
	return result, nil
}

// upsertOne is the atomic /upsert write-path; same rollback contract
// as appendOne. Returns (item, created, err) where created reflects
// the upsert outcome, not the scope-creation outcome. On success it
// emits an upsert event into `_events` (Phase A auto-populate; gated
// on Config.Events.Mode — see events.go).
func (s *Store) upsertOne(item Item) (Item, bool, error) {
	buf, scopeCreated, err := s.getOrCreateScopeTrackingCreated(item.Scope)
	if err != nil {
		return Item{}, false, err
	}
	result, itemCreated, upsertErr := buf.upsertByID(item)
	if upsertErr != nil {
		if scopeCreated {
			s.cleanupIfEmptyAndUnused(item.Scope, buf)
		}
		return result, itemCreated, upsertErr
	}
	s.emitUpsertEvent(result.Scope, result.ID, result.Seq, result.Ts, result.Payload)
	return result, itemCreated, nil
}

// counterAddOne is the atomic /counter_add write-path; same rollback
// contract as appendOne. Returns (value, created, err) where created
// reflects the counter outcome, not the scope-creation outcome. On
// success it emits a counter_add event into `_events` carrying the
// increment `by` — never the post-add value (action-logging, not
// result-logging; see events.go).
func (s *Store) counterAddOne(scope, id string, by int64) (int64, bool, error) {
	buf, scopeCreated, err := s.getOrCreateScopeTrackingCreated(scope)
	if err != nil {
		return 0, false, err
	}
	value, counterCreated, addErr := buf.counterAdd(scope, id, by)
	if addErr != nil {
		if scopeCreated {
			s.cleanupIfEmptyAndUnused(scope, buf)
		}
		return value, counterCreated, addErr
	}
	s.emitCounterAddEvent(scope, id, by)
	return value, counterCreated, nil
}

// updateOne mutates the payload of an item addressed by scope+id or
// scope+seq. Returns (updated_count, err); a missing scope is reported
// as (0, nil), the same wire shape an absent id/seq inside an existing
// scope would produce. The caller-side validator enforces the
// id-xor-seq invariant; updateOne assumes id != "" picks the id path
// and otherwise routes by seq.
//
// Emits an update event into `_events` ONLY on hit (updated > 0): a
// miss is a no-op against cache state, so emitting it would be result-
// logging (the request) rather than action-logging (the change).
// Drainers replaying `_events` against an empty cache produce the
// same final state without these noise entries.
func (s *Store) updateOne(item Item) (int, error) {
	buf, ok := s.getScope(item.Scope)
	if !ok {
		return 0, nil
	}
	var (
		updated int
		err     error
	)
	if item.ID != "" {
		updated, err = buf.updateByID(item.ID, item.Payload)
	} else {
		updated, err = buf.updateBySeq(item.Seq, item.Payload)
	}
	if err != nil {
		return updated, err
	}
	if updated > 0 {
		s.emitUpdateEvent(item.Scope, item.ID, item.Seq, item.Payload)
	}
	return updated, nil
}

// deleteOne removes a single item by scope+id or scope+seq. Returns
// (deleted_count, err); missing scope reports (0, nil) — same miss
// shape as updateOne. Validator-enforced id-xor-seq invariant.
//
// Emits a delete event on hit (count > 0); see updateOne for the
// action-logging-on-effective-change rationale.
func (s *Store) deleteOne(scope, id string, seq uint64) (int, error) {
	buf, ok := s.getScope(scope)
	if !ok {
		return 0, nil
	}
	var (
		deleted int
		err     error
	)
	if id != "" {
		deleted, err = buf.deleteByID(id)
	} else {
		deleted, err = buf.deleteBySeq(seq)
	}
	if err != nil {
		return deleted, err
	}
	if deleted > 0 {
		s.emitDeleteEvent(scope, id, seq)
	}
	return deleted, nil
}

// deleteUpTo removes every item in the scope with seq <= maxSeq.
// Returns (deleted_count, err); missing scope reports (0, nil). Emits
// a delete_up_to event on hit (count > 0); a cursor that selects no
// items is a no-op against cache state and is not emitted.
func (s *Store) deleteUpTo(scope string, maxSeq uint64) (int, error) {
	buf, ok := s.getScope(scope)
	if !ok {
		return 0, nil
	}
	deleted, err := buf.deleteUpToSeq(maxSeq)
	if err != nil {
		return deleted, err
	}
	if deleted > 0 {
		s.emitDeleteUpToEvent(scope, maxSeq)
	}
	return deleted, nil
}

// head returns up to `limit` oldest items in the scope with seq >
// afterSeq. Returns (items, truncated, scopeFound). On a non-empty
// result the scope's read-bookkeeping atomics are bumped via
// recordRead. A missing scope reports (nil, false, false); a
// found-but-empty window reports (empty, false, true) — handlers
// translate that into hit:false / count:0.
func (s *Store) head(scope string, afterSeq uint64, limit int) ([]Item, bool, bool) {
	buf, ok := s.getScope(scope)
	if !ok {
		return nil, false, false
	}
	items, truncated := buf.sinceSeq(afterSeq, limit)
	if len(items) > 0 {
		buf.recordRead(nowUnixMicro())
	}
	return items, truncated, true
}

// tail returns up to `limit` newest items in the scope, skipping the
// first `offset`. Same return shape and bookkeeping as head.
func (s *Store) tail(scope string, limit, offset int) ([]Item, bool, bool) {
	buf, ok := s.getScope(scope)
	if !ok {
		return nil, false, false
	}
	items, truncated := buf.tailOffset(limit, offset)
	if len(items) > 0 {
		buf.recordRead(nowUnixMicro())
	}
	return items, truncated, true
}

// get returns one item by scope+id or scope+seq. (item, found) — the
// found flag is true only when both the scope and the item exist.
// recordRead fires on hit only.
func (s *Store) get(scope, id string, seq uint64) (Item, bool) {
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
	buf.recordRead(nowUnixMicro())
	return item, true
}

// render returns the bytes /render writes on the wire, peeling the
// renderBytes shortcut for JSON-string payloads at the Store boundary
// so the handler does not need to know the renderBytes field exists.
// (bytes, found) — same hit semantics as get; recordRead fires on hit.
func (s *Store) render(scope, id string, seq uint64) ([]byte, bool) {
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
	buf.recordRead(nowUnixMicro())
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
	s.scopeCount.Add(1)
	// Same reasoning as getOrCreateScopeTrackingCreated: scope_count
	// just changed, so the freshness tick advances.
	s.bumpLastWriteTS(buf.lastWriteTS)
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

	// The locked phase is wrapped in an inline closure so `defer
	// sh.mu.Unlock()` fires before emitDeleteScopeEvent below — emit
	// recurses into appendOne(_events) which acquires the _events
	// shard's lock, and that shard might be the very one we just
	// released. Without the closure the emit-while-locked path would
	// either deadlock (when the deleted scope and `_events` hash to
	// the same shard) or live-lock under heavy /delete_scope traffic.
	itemCount, ok := func() (int, bool) {
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
		s.totalItems.Add(-int64(itemCount))
		s.scopeCount.Add(-1)
		s.bumpLastWriteTS(nowUnixMicro())
		buf.detached = true
		buf.store = nil
		buf.mu.Unlock()
		return itemCount, true
	}()
	if !ok {
		return itemCount, false
	}
	s.emitDeleteScopeEvent(scope)
	return itemCount, true
}

// storeStats is the typed snapshot of the store. stats() returns it so the
// /stats handler can flatten it into orderedFields for the wire, and so any
// in-package caller (tests, future adapters) can read fields directly.
//
// The aggregate fields are intentionally not a per-scope map: at
// 100k+ scopes, per-scope enumeration was the dominant cost of /stats
// and routinely blew past practical client/proxy response limits.
// Per-scope enumeration for user-managed scopes lives at /scopelist,
// which paginates alphabetically.
//
// ReservedScopes is the small, fixed exception: `_events` and
// `_inbox` are cache infrastructure (pre-created at boot, drainer-
// fed, drainer-consumed), and operators monitoring the cache need
// their per-scope state — drainer-backlog on `_events`, fan-in queue
// depth on `_inbox` — without paging /scopelist for two known names.
// Length is bounded by the reserved-scope set (currently 2), so the
// O(1) /stats budget is preserved: two getScope() lookups + two
// buf.stats() calls regardless of total scope count.
type storeStats struct {
	ScopeCount     int
	TotalItems     int
	ApproxStoreMB  MB
	LastWriteTS    int64
	ReservedScopes []reservedScopeEntry
}

// reservedScopeEntry is one row in /stats's reserved_scopes array.
// Field set is the (ii)-tier of /scopelist's full row: scope, item
// count, last seq, byte size, created and last-write timestamps —
// what operators need to monitor drainer-backlog and fan-in depth.
// last_access_ts and read_count_total are intentionally omitted:
// reserved scopes are read by drainers/admins, not user-facing
// traffic, so those signals are noise on this endpoint. /scopelist
// still surfaces the full row for anyone who does want them.
//
// Field declaration order = wire field order (encoding/json honours
// it). Mirrors scopeListEntry's field order so a consumer who
// accepts both shapes can fold them through one parser.
type reservedScopeEntry struct {
	Scope         string `json:"scope"`
	ItemCount     int    `json:"item_count"`
	LastSeq       uint64 `json:"last_seq"`
	ApproxScopeMB MB     `json:"approx_scope_mb"`
	CreatedTS     int64  `json:"created_ts"`
	LastWriteTS   int64  `json:"last_write_ts"`
}

// stats returns the store-wide aggregate snapshot in O(1) — four
// atomic loads. No shard walks, no per-scope fan-out: every counter
// is maintained incrementally on the write paths (see the
// totalItems/scopeCount comment block on *Store for the lockstep
// discipline). Configured caps (MaxStoreBytes, etc.) are NOT echoed
// here — they are static config and belong on /help (or a future
// dedicated /config endpoint), not in a per-call state response.
//
// Each load is independent, so a concurrent burst of /append + /delete
// can produce a snapshot where (scope_count, total_items, approx_store_mb)
// reflect three slightly different instants. That's the same caveat
// the previous shard-walking version carried (and weaker — it walked
// shards sequentially, releasing locks between them). /stats has
// always been an approximation; this version is honest about it
// without paying the per-scope enumeration cost.
func (s *Store) stats() storeStats {
	return storeStats{
		ScopeCount:     int(s.scopeCount.Load()),
		TotalItems:     int(s.totalItems.Load()),
		ApproxStoreMB:  MB(s.totalBytes.Load()),
		LastWriteTS:    s.lastWriteTS.Load(),
		ReservedScopes: s.reservedScopeStats(),
	}
}

// reservedScopeStats materialises one entry per name in
// reservedScopeNames. Each entry is the slim-tier scopeStats: item
// count, last seq, byte size, creation + last-write timestamps. A
// reserved scope that doesn't exist (impossible during steady state
// but defensible against init-races / future refactors) is silently
// skipped — operators see "the scope wasn't there" as an empty entry
// instead of a hard error or a stale snapshot.
//
// getScope takes one shard RLock per call; buf.stats() takes the
// scope's own buf.mu.RLock briefly for the materialisation. A
// concurrent destructive op (/wipe or /rebuild) holds every shard in
// write mode for the whole sweep, so this method either runs entirely
// before or entirely after the destructive op — never observing a
// half-wiped state.
func (s *Store) reservedScopeStats() []reservedScopeEntry {
	out := make([]reservedScopeEntry, 0, len(reservedScopeNames))
	for _, name := range reservedScopeNames {
		buf, ok := s.getScope(name)
		if !ok {
			continue
		}
		st := buf.stats()
		out = append(out, reservedScopeEntry{
			Scope:         name,
			ItemCount:     st.ItemCount,
			LastSeq:       st.LastSeq,
			ApproxScopeMB: st.ApproxScopeMB,
			CreatedTS:     st.CreatedTS,
			LastWriteTS:   st.LastWriteTS,
		})
	}
	return out
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

// scopeListEntry is one row in a /scopelist response: the scope name plus
// the seven per-scope primitives §2.4 of the RFC maintains directly.
// Field declaration order = wire field order (encoding/json honours it).
type scopeListEntry struct {
	Scope          string `json:"scope"`
	ItemCount      int    `json:"item_count"`
	LastSeq        uint64 `json:"last_seq"`
	ApproxScopeMB  MB     `json:"approx_scope_mb"`
	CreatedTS      int64  `json:"created_ts"`
	LastWriteTS    int64  `json:"last_write_ts"`
	LastAccessTS   int64  `json:"last_access_ts"`
	ReadCountTotal uint64 `json:"read_count_total"`
}

// scopeList returns the per-scope detail rows for /scopelist: optional
// prefix filter, alphabetical sort, cursor pagination by name. Returns
// (entries, truncated) where truncated is true when more matching scopes
// exist past the limit window.
//
// Cost shape:
//   - O(N) walk across every shard map under each shard's RLock; the
//     filter (prefix match + after-cursor) runs inside the loop so only
//     matching names enter the sort buffer.
//   - O(M log M) sort, where M = filtered count.
//   - O(limit) buf.stats() materialisations after the locks are released.
//
// Stats() takes its own buf.mu.RLock per scope, so a slow stats() call
// cannot block writers on its shard for the duration of the listing.
// A scope deleted between the snapshot and stats() materialises its
// last-known state — same advisory-snapshot caveat as /stats (§7.3).
func (s *Store) scopeList(prefix, after string, limit int) ([]scopeListEntry, bool) {
	type ref struct {
		name string
		buf  *scopeBuffer
	}
	var refs []ref
	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.RLock()
		for name, buf := range sh.scopes {
			if prefix != "" && !strings.HasPrefix(name, prefix) {
				continue
			}
			if after != "" && name <= after {
				continue
			}
			refs = append(refs, ref{name: name, buf: buf})
		}
		sh.mu.RUnlock()
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].name < refs[j].name })

	truncated := len(refs) > limit
	if truncated {
		refs = refs[:limit]
	}

	out := make([]scopeListEntry, 0, len(refs))
	for _, r := range refs {
		st := r.buf.stats()
		out = append(out, scopeListEntry{
			Scope:          r.name,
			ItemCount:      st.ItemCount,
			LastSeq:        st.LastSeq,
			ApproxScopeMB:  st.ApproxScopeMB,
			CreatedTS:      st.CreatedTS,
			LastWriteTS:    st.LastWriteTS,
			LastAccessTS:   st.LastAccessTS,
			ReadCountTotal: st.ReadCountTotal,
		})
	}
	return out, truncated
}
