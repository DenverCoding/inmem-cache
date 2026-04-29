package scopecache

import (
	"encoding/json"
	"errors"
	"hash/maphash"
	"sort"
	"strconv"
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
	scopes map[string]*ScopeBuffer
}

type ScopeBuffer struct {
	mu sync.RWMutex
	// store is set when the buffer is owned by a Store. When nil (orphan
	// buffers used in unit tests) byte-budget accounting is skipped — the
	// tests exercise item-count and seq logic without spinning up a store.
	store *Store
	// detached is set true when the buffer has been unlinked from its Store
	// by /delete_scope, /wipe or /rebuild. Writes that reach a detached
	// buffer via a stale pointer return *ScopeDetachedError so the caller
	// learns the write did not take effect, rather than silently writing
	// into an orphan buffer that is unreachable and about to be GC'd.
	detached bool
	items    []Item
	byID     map[string]Item
	bySeq    map[uint64]Item
	lastSeq  uint64
	maxItems int
	// bytes is the running sum of approxItemSize(item) over items. Only
	// mutated under b.mu; the store-level total is kept in sync via
	// Store.reserveBytes / commitReplacement.
	bytes     int64
	createdTS int64
	// lastAccessTS, readCountTotal, last7DReadCount and the heat-bucket
	// counts are atomic so the read-hot path (recordRead) does not need
	// to take b.mu. recordRead used to take b.mu.Lock() — a write lock
	// on the same mutex readers were just on under RLock — turning every
	// hit on /get, /render, /head, /tail and /ts_range into a serialise
	// point. Block profiling pinned ~88% of all read-path lock-wait time
	// to that one call. Moving the bucket state to atomics drops it to
	// effectively zero.
	lastAccessTS    atomic.Int64
	readCountTotal  atomic.Uint64
	last7DReadCount atomic.Uint64
	// Ring buffer indexed by day % ReadHeatWindowDays. Each bucket
	// carries the absolute day it represents so we can detect a stale
	// slot when it wraps. Day and Count inside each bucket are atomic;
	// expiry and slot-claim are coordinated via CAS on Day. See
	// recordRead for the state machine.
	readHeatBuckets [ReadHeatWindowDays]ScopeReadHeatBucket
}

func NewScopeBuffer(maxItems int) *ScopeBuffer {
	// items, byID and bySeq all grow on-demand. Pre-allocating any of
	// them on every scope-create is the wrong default: a unique-scope-
	// per-write workload creates millions of buffers, most of which
	// hold one item and never need byID at all (items without an `id`
	// have no byID entry). See:
	//
	//   - phase-4 finding "Sharded scopes map: pre-existing-scope
	//     writes 2× faster, unique-scope writes barely" — traces the
	//     items-slice pre-alloc that GC could not keep up with.
	//   - the follow-up "Lazy maps in NewScopeBuffer" — extends the
	//     same reasoning to byID and bySeq. Lazy bySeq saves the
	//     create-time alloc on every scope; lazy byID skips the
	//     allocation entirely on scopes whose items never carry an id.
	//
	// The write paths in this file (appendItem, upsertByID,
	// counterAdd, replaceItemAtIndexLocked) lazily initialise these
	// maps before assigning into them. Reads, deletes, len and range
	// are nil-safe in Go and need no guard.
	return &ScopeBuffer{
		maxItems:  maxItems,
		createdTS: nowUnixMicro(),
	}
}

// Methods with a "Locked" suffix assume the caller already holds b.mu.
// They exist so other locked methods (like stats) can compute without re-locking.
//
// approxSizeBytes is a richer estimate than the raw approxItemSize sum: it
// also folds in Go map/slice overhead for b.byID, b.bySeq, and the heat
// buckets. It drives the per-scope approx_scope_mb field in /stats and the
// Candidate.ApproxScopeMB field in /delete_scope_candidates. It is NOT used
// for cap enforcement — admission control uses Store.totalBytes
// (approxItemSize sum + scopeBufferOverhead per scope) so the 507 budget
// matches what reserveBytes accounts for. Per-item Go heap overhead
// (slice/map entries) is intentionally outside the cap; see phase4
// CLAUDE.md "max_store_mb underestimates real memory cost at high scope
// counts" for the open pre-v1.0 design question.
func (b *ScopeBuffer) approxSizeBytesLocked() int64 {
	var total int64
	total += 64
	total += int64(len(b.items)) * 32

	for _, item := range b.items {
		total += approxItemSize(item)
	}

	total += int64(len(b.byID)) * 32
	for k := range b.byID {
		total += int64(len(k))
	}

	total += int64(len(b.bySeq)) * 16
	total += int64(len(b.readHeatBuckets)) * 16

	return total
}

func (b *ScopeBuffer) approxSizeBytes() int64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.approxSizeBytesLocked()
}

// computeLast7DReadCount walks the heat buckets and returns the count
// of reads whose Day is within the rolling 7-day window ending at
// `now`. Used by stats() so a /stats or /delete_scope_candidates call
// observes a correct count even when no fresh read has happened to
// expire stale buckets via recordRead. All loads are atomic; the
// snapshot is eventually-consistent (a concurrent recordRead may
// land between two bucket reads here) which is acceptable for the
// observability use cases this drives.
func (b *ScopeBuffer) computeLast7DReadCount(now int64) uint64 {
	day := unixDay(now)
	oldestValidDay := day - ReadHeatWindowDays + 1
	var sum uint64
	for i := range b.readHeatBuckets {
		bucket := &b.readHeatBuckets[i]
		if bucket.Day.Load() >= oldestValidDay {
			sum += bucket.Count.Load()
		}
	}
	return sum
}

// recordRead is the lock-free heat-tracking path called on every hit
// of /get, /render, /head, /tail and /ts_range. It runs without
// taking b.mu so concurrent readers (which already hold b.mu.RLock)
// do not serialise behind a write lock here. The state machine has
// two phases:
//
//  1. Expiry: walk all 7 buckets and drain any whose Day is older
//     than the rolling 7-day window ending at `now`. CAS on
//     bucket.Day claims the expiry — only the winning goroutine
//     subtracts that bucket's Count from last7DReadCount and resets
//     it. Losers re-read and either find Day cleared (skip) or
//     a freshly-claimed Day (skip).
//
//  2. Increment today's bucket. The ring is indexed by
//     day % ReadHeatWindowDays. After phase 1 the slot for today is
//     either already on `day` (no-op claim) or empty (Day == 0); a
//     CAS on Day claims the slot for `day`. Then atomic adds bump
//     bucket.Count, b.readCountTotal, b.last7DReadCount, and store
//     b.lastAccessTS.
//
// The modulo trick (one slot per `day % 7`) means a slot whose
// Day != currentDay must be at least 7 days old, so phase 1 will
// always have cleared it before phase 2 runs. That invariant lets
// phase 2 treat any non-`day` slot as empty without losing counts.
func (b *ScopeBuffer) recordRead(now int64) {
	day := unixDay(now)
	oldestValidDay := day - ReadHeatWindowDays + 1

	// Phase 1 — expiry.
	for i := range b.readHeatBuckets {
		bucket := &b.readHeatBuckets[i]
		for {
			d := bucket.Day.Load()
			if d == 0 || d >= oldestValidDay {
				break
			}
			// Claim the expiry by CAS'ing Day to 0. Losing the CAS
			// means another goroutine just rolled or expired this
			// slot — re-read and re-evaluate.
			if !bucket.Day.CompareAndSwap(d, 0) {
				continue
			}
			// We won the claim. Drain the count atomically and
			// subtract it from last7DReadCount (saturating at 0).
			n := bucket.Count.Swap(0)
			for {
				cur := b.last7DReadCount.Load()
				next := uint64(0)
				if cur > n {
					next = cur - n
				}
				if b.last7DReadCount.CompareAndSwap(cur, next) {
					break
				}
			}
			break
		}
	}

	// Phase 2 — claim/find today's slot and increment.
	bucketIndex := int(day % ReadHeatWindowDays)
	bucket := &b.readHeatBuckets[bucketIndex]
	for {
		d := bucket.Day.Load()
		if d == day {
			break
		}
		// d is either 0 (empty after Phase 1) or a value that races
		// with a concurrent claim; CAS to today wins exactly once
		// per goroutine race. Losers re-read and re-evaluate.
		if bucket.Day.CompareAndSwap(d, day) {
			// Reset Count to 0 just in case Phase 1 raced with us
			// and we beat it to the slot before it could drain.
			// Safe: any in-flight Adds from this same goroutine
			// happen after this Store.
			bucket.Count.Store(0)
			break
		}
	}

	bucket.Count.Add(1)
	b.readCountTotal.Add(1)
	b.last7DReadCount.Add(1)
	b.lastAccessTS.Store(now)
}

// scopeReplacement holds a fully built scope state ready to be atomically
// swapped into a ScopeBuffer. Separating "prepare" from "commit" lets callers
// like /warm and /rebuild validate every scope up-front and only mutate state
// once they know all scopes will succeed.
type scopeReplacement struct {
	items   []Item
	byID    map[string]Item
	bySeq   map[uint64]Item
	lastSeq uint64
}

// buildReplacementState converts a caller-supplied item list into the
// internal state a scope buffer can adopt atomically. Callers are expected
// to have already enforced the per-scope capacity; this function does not
// trim — if len(items) exceeds the cap it would simply build an over-full
// state. The capacity check lives in the Store layer so one place owns it.
func buildReplacementState(items []Item) (scopeReplacement, error) {
	if len(items) == 0 {
		return scopeReplacement{
			items: []Item{},
			byID:  make(map[string]Item),
			bySeq: make(map[uint64]Item),
		}, nil
	}

	seen := make(map[string]struct{}, len(items))
	nonEmptyIDs := 0
	built := make([]Item, 0, len(items))
	bySeq := make(map[uint64]Item, len(items))

	// seq is a cache-local cursor that is NOT stable across /warm or /rebuild.
	// We regenerate it from 1 for every call so scope buffers have monotonic,
	// dense seq values even when the input items came from elsewhere.
	var lastSeq uint64
	for _, src := range items {
		if src.ID != "" {
			if _, ok := seen[src.ID]; ok {
				return scopeReplacement{}, errors.New("duplicate 'id' value within scope: '" + src.ID + "'")
			}
			seen[src.ID] = struct{}{}
			nonEmptyIDs++
		}

		lastSeq++
		item := src
		item.Seq = lastSeq

		built = append(built, item)
		bySeq[item.Seq] = item
	}

	byID := make(map[string]Item, nonEmptyIDs)
	for _, item := range built {
		if item.ID != "" {
			byID[item.ID] = item
		}
	}

	return scopeReplacement{
		items:   built,
		byID:    byID,
		bySeq:   bySeq,
		lastSeq: lastSeq,
	}, nil
}

func (b *ScopeBuffer) appendItem(item Item) (Item, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.detached {
		return Item{}, &ScopeDetachedError{}
	}

	if len(b.items) >= b.maxItems {
		return Item{}, &ScopeFullError{Count: len(b.items), Cap: b.maxItems}
	}

	if item.ID != "" {
		if _, exists := b.byID[item.ID]; exists {
			return Item{}, errors.New("an item with this 'id' already exists in the scope")
		}
	}

	// Reserve store-level bytes before mutating scope state: a failed
	// reservation leaves the scope untouched, same as a failed dup-id check.
	size := approxItemSize(item)
	if b.store != nil {
		ok, current, max := b.store.reserveBytes(size)
		if !ok {
			return Item{}, &StoreFullError{StoreBytes: current, AddedBytes: size, Cap: max}
		}
	}

	b.lastSeq++
	item.Seq = b.lastSeq

	b.items = append(b.items, item)
	if b.bySeq == nil {
		b.bySeq = make(map[uint64]Item)
	}
	b.bySeq[item.Seq] = item
	if item.ID != "" {
		if b.byID == nil {
			b.byID = make(map[string]Item)
		}
		b.byID[item.ID] = item
	}
	b.bytes += size

	return item, nil
}

// commitReplacement atomically swaps the scope's state and adjusts the store
// byte counter by the *actual* delta (newBytes - b.bytes at commit time).
// Reading b.bytes under b.mu here makes the commit robust against a
// concurrent /append that completed between the caller's pre-check and this
// commit: any bytes it added to the store counter are cancelled out by the
// fresh delta, because its item is being replaced anyway.
//
// The caller must have already validated and built the replacement via
// buildReplacementState — commitReplacement cannot fail, which is what lets
// multi-scope /warm behave atomically.
func (b *ScopeBuffer) commitReplacement(r scopeReplacement, newBytes int64) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.store != nil {
		b.store.totalBytes.Add(newBytes - b.bytes)
	}
	b.bytes = newBytes
	b.items = r.items
	b.byID = r.byID
	b.bySeq = r.bySeq
	b.lastSeq = r.lastSeq
}

// commitReplacementPreReserved is the batch-aware commit used by
// Store.replaceScopes. The caller has already atomically reserved
// (newBytes - oldSnapshot) bytes against the store counter via reserveBytes,
// so this commit must NOT re-add that delta; it only releases drift caused
// by concurrent writes to this scope between the snapshot and the commit,
// which keeps the store-wide byte cap strict across batch replacements.
//
// Drift handling, using oldSnapshot (b.bytes as read under RLock during
// the batch's cap check):
//
//   - Concurrent /append on this scope in the window: b.bytes grew by +X
//     and the appender did totalBytes.Add(+X). Drift = b.bytes - oldSnapshot
//     = X; we Add(-X), releasing that reservation (the appended item gets
//     discarded by the replacement anyway).
//   - Concurrent /delete on this scope in the window: b.bytes shrank by Y
//     and the deleter did totalBytes.Add(-Y). Drift is negative; Add(-drift)
//     is positive, compensating for the extra release so the scope's net
//     contribution to totalBytes is exactly (newBytes - oldSnapshot).
//   - No concurrent activity: drift = 0, no counter adjustment.
func (b *ScopeBuffer) commitReplacementPreReserved(r scopeReplacement, newBytes int64, oldSnapshot int64) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.store != nil {
		drift := b.bytes - oldSnapshot
		if drift != 0 {
			b.store.totalBytes.Add(-drift)
		}
	}
	b.bytes = newBytes
	b.items = r.items
	b.byID = r.byID
	b.bySeq = r.bySeq
	b.lastSeq = r.lastSeq
}

// sumItemBytes returns the total approxItemSize across a flat item slice.
// Used by batch operations to compute per-plan newBytes before commit.
func sumItemBytes(items []Item) int64 {
	var n int64
	for i := range items {
		n += approxItemSize(items[i])
	}
	return n
}

func (b *ScopeBuffer) replaceAll(items []Item) ([]Item, error) {
	if len(items) > b.maxItems {
		return nil, &ScopeFullError{Count: len(items), Cap: b.maxItems}
	}
	r, err := buildReplacementState(items)
	if err != nil {
		return nil, err
	}
	newBytes := sumItemBytes(r.items)
	b.commitReplacement(r, newBytes)

	b.mu.RLock()
	defer b.mu.RUnlock()
	return append([]Item(nil), b.items...), nil
}

// upsertByID replaces the payload of the item with this id if it exists,
// or appends a new item with this id if it does not. Both paths run under a
// single scope write-lock so concurrent upserts cannot race between the
// existence check and the mutation. Seq is preserved on replace (stable
// cursor for consumers) and freshly assigned on create (matches /append).
// Returns the final item and whether a new item was created.
func (b *ScopeBuffer) upsertByID(item Item) (Item, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.detached {
		return Item{}, false, &ScopeDetachedError{}
	}

	if existing, exists := b.byID[item.ID]; exists {
		delta := int64(len(item.Payload)) - int64(len(existing.Payload))
		if b.store != nil && delta != 0 {
			ok, current, max := b.store.reserveBytes(delta)
			if !ok {
				return Item{}, false, &StoreFullError{StoreBytes: current, AddedBytes: delta, Cap: max}
			}
		}

		i, ok := b.indexBySeqLocked(existing.Seq)
		if !ok {
			// Unreachable under b.mu: b.byID confirmed the item exists and items/byID are kept in sync.
			return Item{}, false, nil
		}
		b.items[i].Payload = item.Payload
		// /upsert has replace-the-whole-item semantics, so ts follows the
		// client's input exactly: send ts → stored, omit → cleared. That
		// differs from /update (which treats absent ts as "preserve").
		b.items[i].Ts = item.Ts

		updated := b.items[i]
		// b.byID hit above proves byID was already allocated; same for
		// bySeq (every item that lives in byID also lives in bySeq).
		b.bySeq[updated.Seq] = updated
		b.byID[item.ID] = updated
		b.bytes += delta
		return updated, false, nil
	}

	if len(b.items) >= b.maxItems {
		return Item{}, false, &ScopeFullError{Count: len(b.items), Cap: b.maxItems}
	}

	size := approxItemSize(item)
	if b.store != nil {
		ok, current, max := b.store.reserveBytes(size)
		if !ok {
			return Item{}, false, &StoreFullError{StoreBytes: current, AddedBytes: size, Cap: max}
		}
	}

	b.lastSeq++
	item.Seq = b.lastSeq

	b.items = append(b.items, item)
	if b.bySeq == nil {
		b.bySeq = make(map[uint64]Item)
	}
	b.bySeq[item.Seq] = item
	if b.byID == nil {
		b.byID = make(map[string]Item)
	}
	b.byID[item.ID] = item
	b.bytes += size

	return item, true, nil
}

// counterAdd atomically adds `by` to the integer stored at scope+id, or
// creates a fresh counter with starting value `by` if no item exists. Both
// paths run under a single scope write-lock so concurrent increments cannot
// lose updates. The stored payload is a bare JSON integer (e.g. `42`), which
// is what /get, /render, /upsert and /update all see on this scope+id.
//
// Errors:
//   - *ScopeFullError  → create path hit the per-scope item cap
//   - *StoreFullError  → create or payload-grow hit the store byte cap
//   - *CounterPayloadError  → existing payload is not a valid counter value
//   - *CounterOverflowError → result would exceed ±MaxCounterValue
//
// The caller must have already rejected by==0 and by outside ±MaxCounterValue.
func (b *ScopeBuffer) counterAdd(scope, id string, by int64) (int64, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.detached {
		return 0, false, &ScopeDetachedError{}
	}

	if existing, exists := b.byID[id]; exists {
		current, err := parseCounterValue(existing.Payload)
		if err != nil {
			return 0, false, err
		}

		newValue := current + by
		if newValue > MaxCounterValue || newValue < -MaxCounterValue {
			return 0, false, &CounterOverflowError{Current: current, By: by}
		}

		newPayload := json.RawMessage(strconv.FormatInt(newValue, 10))
		delta, err := b.reservePayloadDeltaLocked(len(existing.Payload), len(newPayload))
		if err != nil {
			return 0, false, err
		}

		i, ok := b.indexBySeqLocked(existing.Seq)
		if !ok {
			// Unreachable under b.mu: b.byID confirmed the item exists and items/byID are kept in sync.
			return 0, false, nil
		}
		// /counter_add never updates ts (only the integer payload),
		// so pass nil for ts — the helper preserves the existing ts.
		b.replaceItemAtIndexLocked(i, newPayload, nil, delta)
		return newValue, false, nil
	}

	if len(b.items) >= b.maxItems {
		return 0, false, &ScopeFullError{Count: len(b.items), Cap: b.maxItems}
	}

	item := Item{
		Scope:   scope,
		ID:      id,
		Payload: json.RawMessage(strconv.FormatInt(by, 10)),
	}
	size := approxItemSize(item)
	if b.store != nil {
		ok, total, max := b.store.reserveBytes(size)
		if !ok {
			return 0, false, &StoreFullError{StoreBytes: total, AddedBytes: size, Cap: max}
		}
	}

	b.lastSeq++
	item.Seq = b.lastSeq
	b.items = append(b.items, item)
	if b.bySeq == nil {
		b.bySeq = make(map[uint64]Item)
	}
	b.bySeq[item.Seq] = item
	if b.byID == nil {
		b.byID = make(map[string]Item)
	}
	b.byID[id] = item
	b.bytes += size

	return by, true, nil
}

// parseCounterValue decodes a payload as a JSON integer within ±MaxCounterValue.
// Anything else — a non-number, a float, a number outside the range — is a
// CounterPayloadError (409 Conflict) because the counter machinery cannot
// safely operate on it.
func parseCounterValue(payload json.RawMessage) (int64, error) {
	var num json.Number
	if err := json.Unmarshal(payload, &num); err != nil {
		return 0, &CounterPayloadError{Reason: "the existing item's payload is not a JSON number"}
	}
	v, err := num.Int64()
	if err != nil {
		return 0, &CounterPayloadError{Reason: "the existing item's payload is not an integer"}
	}
	if v > MaxCounterValue || v < -MaxCounterValue {
		return 0, &CounterPayloadError{Reason: "the existing counter value is outside the allowed range of ±(2^53-1)"}
	}
	return v, nil
}

// --- low-level mutation primitives ------------------------------------------
//
// These three helpers (`*Locked` suffix) factor out the bytes-accounting,
// index-syncing and GC-zeroing concerns that previously lived in parallel
// across updateByID, updateBySeq, counterAdd, deleteByID, and deleteBySeq.
// Each helper PRECONDITION: caller MUST hold b.mu. The helpers do not
// attempt to acquire the lock themselves — calling them without the lock
// is a race; calling them WITH the lock then re-acquiring is a deadlock.
// The Locked suffix is the convention; the comment above each helper
// repeats the precondition for callers reading top-down.

// reservePayloadDeltaLocked computes the byte delta between an old and a
// new payload (newSize − oldSize) and, if the delta is non-zero AND the
// buffer is store-attached, reserves the delta against the store-wide
// byte budget via reserveBytes. Returns the delta so the caller can
// update b.bytes consistently after a successful mutation.
//
// PRECONDITION: caller holds b.mu.
//
// Returns *StoreFullError when the cap reservation fails. No store
// state is mutated in that case — the caller can return the error
// without rolling anything back. Centralises the (b.store != nil &&
// delta != 0) guard: forgetting either condition produces a nil-
// pointer crash on orphan buffers (used in tests) or a CAS for a
// zero delta (cheap but pointless).
func (b *ScopeBuffer) reservePayloadDeltaLocked(oldSize, newSize int) (int64, error) {
	delta := int64(newSize) - int64(oldSize)
	if b.store != nil && delta != 0 {
		ok, current, max := b.store.reserveBytes(delta)
		if !ok {
			return 0, &StoreFullError{StoreBytes: current, AddedBytes: delta, Cap: max}
		}
	}
	return delta, nil
}

// replaceItemAtIndexLocked overwrites the payload at items[i] (always)
// and the ts (only if non-nil), then syncs both secondary indexes
// (bySeq unconditionally, byID only when the item has a non-empty id),
// and finally updates b.bytes by the supplied delta.
//
// PRECONDITION: caller holds b.mu and i is a valid index into b.items.
// Bounds-check is the caller's responsibility — the helper does not
// validate i because callers reach it via O(log n) binary-search or
// guaranteed-hit lookups, and re-checking would defeat that.
//
// The byID sync is conditional because /append accepts items without
// an id (id="" is legal), so not every item has a byID entry to keep
// in sync. bySeq is unconditional because every item has a seq.
func (b *ScopeBuffer) replaceItemAtIndexLocked(i int, payload json.RawMessage, ts *int64, delta int64) {
	b.items[i].Payload = payload
	if ts != nil {
		b.items[i].Ts = ts
	}
	updated := b.items[i]
	// replaceItemAtIndexLocked is only reachable when the item already
	// existed in this buffer, so bySeq and (when ID != "") byID have
	// already been allocated by the original write that created it.
	b.bySeq[updated.Seq] = updated
	if updated.ID != "" {
		b.byID[updated.ID] = updated
	}
	b.bytes += delta
}

// deleteIndexLocked removes the item at items[i] in O(n) tail-shift,
// zeroes the now-duplicate last slot (so the GC can reclaim the
// removed Item's payload bytes — without this the backing array
// keeps a reference and the payload leaks), removes the item from
// bySeq + byID (the latter only if the id is non-empty), and
// releases the item's bytes from both b.bytes and the store-wide
// totalBytes counter when store-attached.
//
// PRECONDITION: caller holds b.mu and i is a valid index into b.items.
//
// Centralises three invariants that previously lived parallel across
// deleteByID and deleteBySeq:
//
//  1. GC-zeroing of the duplicate last slot before truncate. Forgetting
//     this leaks payloads — silent until /stats drifts under load.
//  2. Lockstep b.bytes / s.totalBytes update. Forgetting one desyncs
//     the per-scope and store-wide counters and corrupts /stats.
//  3. Conditional byID delete. Forgetting the `if removed.ID != ""`
//     guard would `delete(map, "")` which is a no-op but signals the
//     reader didn't think about empty-id items.
func (b *ScopeBuffer) deleteIndexLocked(i int) {
	removed := b.items[i]
	removedSize := approxItemSize(removed)

	// Tail-shift then zero the now-duplicate last slot before
	// shrinking. Without the zero the backing array keeps a
	// reference to the removed Item (and its payload bytes) and
	// prevents GC.
	copy(b.items[i:], b.items[i+1:])
	b.items[len(b.items)-1] = Item{}
	b.items = b.items[:len(b.items)-1]

	delete(b.bySeq, removed.Seq)
	if removed.ID != "" {
		delete(b.byID, removed.ID)
	}

	b.bytes -= removedSize
	if b.store != nil {
		b.store.totalBytes.Add(-removedSize)
	}
}

// indexBySeqLocked returns the position of the item with the given seq
// inside b.items. Returns (0, false) when no such item exists. items is
// always ordered ascending by seq (appendItem assigns monotonic seqs and
// nothing inserts in the middle), so this is O(log n) — the canonical
// way to translate a byID/bySeq map hit into a slice index for in-place
// mutation. Caller must hold b.mu (read or write — both are fine,
// callers have a write lock in practice because every user is about to
// mutate b.items[i]).
func (b *ScopeBuffer) indexBySeqLocked(seq uint64) (int, bool) {
	i := sort.Search(len(b.items), func(i int) bool {
		return b.items[i].Seq >= seq
	})
	if i == len(b.items) || b.items[i].Seq != seq {
		return 0, false
	}
	return i, true
}

// updateByID mutates the item at (scope, id). Payload is always overwritten.
// Ts follows "absent → preserve, present → overwrite" semantics: a nil ts
// leaves the stored ts alone, a non-nil ts replaces it. This asymmetry with
// /upsert (which blind-overwrites ts) is deliberate — /update is a partial
// modify, /upsert is a full replace.
func (b *ScopeBuffer) updateByID(id string, payload json.RawMessage, ts *int64) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.detached {
		return 0, &ScopeDetachedError{}
	}

	existing, ok := b.byID[id]
	if !ok {
		return 0, nil
	}

	// Only the payload changes on /update; scope/id/ts are unchanged in size,
	// so the byte delta reduces to len(new_payload) - len(old_payload). A
	// shrink can't fail the cap check, but a grow must reserve first.
	delta, err := b.reservePayloadDeltaLocked(len(existing.Payload), len(payload))
	if err != nil {
		return 0, err
	}

	i, ok := b.indexBySeqLocked(existing.Seq)
	if !ok {
		// Unreachable under b.mu: b.byID confirmed the item exists and items/byID are kept in sync.
		return 0, nil
	}
	b.replaceItemAtIndexLocked(i, payload, ts, delta)
	return 1, nil
}

func (b *ScopeBuffer) updateBySeq(seq uint64, payload json.RawMessage, ts *int64) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.detached {
		return 0, &ScopeDetachedError{}
	}

	existing, ok := b.bySeq[seq]
	if !ok {
		return 0, nil
	}

	delta, err := b.reservePayloadDeltaLocked(len(existing.Payload), len(payload))
	if err != nil {
		return 0, err
	}

	i, ok := b.indexBySeqLocked(seq)
	if !ok {
		// Unreachable under b.mu: b.bySeq confirmed the item exists and items/bySeq are kept in sync.
		return 0, nil
	}
	b.replaceItemAtIndexLocked(i, payload, ts, delta)
	return 1, nil
}

func (b *ScopeBuffer) deleteByID(id string) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.detached {
		return 0, &ScopeDetachedError{}
	}

	existing, ok := b.byID[id]
	if !ok {
		return 0, nil
	}

	i, ok := b.indexBySeqLocked(existing.Seq)
	if !ok {
		// Unreachable under b.mu: b.byID confirmed the item exists and items/bySeq are kept in sync.
		return 0, nil
	}
	b.deleteIndexLocked(i)
	return 1, nil
}

func (b *ScopeBuffer) deleteBySeq(seq uint64) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.detached {
		return 0, &ScopeDetachedError{}
	}

	if _, ok := b.bySeq[seq]; !ok {
		return 0, nil
	}

	i, ok := b.indexBySeqLocked(seq)
	if !ok {
		// Unreachable under b.mu: b.bySeq confirmed the item exists and items/bySeq are kept in sync.
		return 0, nil
	}
	b.deleteIndexLocked(i)
	return 1, nil
}

// deleteUpToSeq removes every item with Seq <= maxSeq. b.items is always
// ordered ascending by Seq (appendItem assigns monotonic seqs and nothing
// removes from the middle), so binary search finds the cut point in O(log n).
// Returns the number of items removed and any *ScopeDetachedError if the
// buffer was orphaned by /delete_scope, /wipe, or /rebuild before the
// caller's mutation could land.
func (b *ScopeBuffer) deleteUpToSeq(maxSeq uint64) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.detached {
		return 0, &ScopeDetachedError{}
	}

	idx := sort.Search(len(b.items), func(i int) bool {
		return b.items[i].Seq > maxSeq
	})
	if idx == 0 {
		return 0, nil
	}

	var freedBytes int64
	for i := 0; i < idx; i++ {
		removed := b.items[i]
		freedBytes += approxItemSize(removed)
		delete(b.bySeq, removed.Seq)
		if removed.ID != "" {
			delete(b.byID, removed.ID)
		}
	}
	// Copy the kept suffix into a fresh backing array so the old one —
	// which still holds the removed payloads in its prefix — becomes
	// GC-eligible. A bare reslice (b.items[idx:]) would pin the full
	// original array behind a small remainder; this matters for the
	// write-buffer pattern where repeated drain-from-front otherwise
	// retains memory proportional to the historical high-watermark.
	rest := make([]Item, len(b.items)-idx)
	copy(rest, b.items[idx:])
	b.items = rest

	b.bytes -= freedBytes
	if b.store != nil {
		b.store.totalBytes.Add(-freedBytes)
	}
	return idx, nil
}

// tailOffset returns the newest-first window `[start, end)` of b.items and a
// hasMore flag. hasMore is true when older items exist before the window (i.e.
// start > 0), signalling to the caller that the response is clipped at the
// oldest end. It does NOT signal truncation at the newest end (that is what
// offset already describes to the client).
func (b *ScopeBuffer) tailOffset(limit int, offset int) ([]Item, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if limit <= 0 || offset < 0 {
		return []Item{}, false
	}
	if offset >= len(b.items) {
		return []Item{}, false
	}

	end := len(b.items) - offset
	start := end - limit
	hasMore := start > 0
	if start < 0 {
		start = 0
	}
	if start >= end {
		return []Item{}, false
	}

	return append([]Item(nil), b.items[start:end]...), hasMore
}

// sinceSeq returns items with seq > afterSeq, oldest-first, up to limit. The
// bool is true when more matching items exist beyond the returned slice, which
// lets the handler surface truncated=true without the client having to guess
// from count == limit.
func (b *ScopeBuffer) sinceSeq(afterSeq uint64, limit int) ([]Item, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if len(b.items) == 0 {
		return []Item{}, false
	}

	idx := sort.Search(len(b.items), func(i int) bool {
		return b.items[i].Seq > afterSeq
	})

	if idx >= len(b.items) {
		return []Item{}, false
	}

	available := len(b.items) - idx
	take := available
	hasMore := false
	if limit > 0 && available > limit {
		take = limit
		hasMore = true
	}
	out := make([]Item, take)
	copy(out, b.items[idx:idx+take])
	return out, hasMore
}

// tsRange scans the scope in seq order, returning items whose Ts falls inside
// the inclusive window defined by sinceTs and untilTs (either may be nil to
// leave that side unbounded). Items without a Ts are always skipped. The bool
// is true when at least one further matching item exists beyond the limit,
// so the handler can set truncated=true. This is an O(n) scan — unindexed
// because the per-scope cap (100k items) makes a linear pass sub-millisecond
// and the code stays trivially correct under concurrent ts mutations.
func (b *ScopeBuffer) tsRange(sinceTs, untilTs *int64, limit int) ([]Item, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	out := make([]Item, 0, limit)
	for _, it := range b.items {
		if it.Ts == nil {
			continue
		}
		if sinceTs != nil && *it.Ts < *sinceTs {
			continue
		}
		if untilTs != nil && *it.Ts > *untilTs {
			continue
		}
		if limit > 0 && len(out) == limit {
			// Found one more match beyond the cap — signal truncation and stop.
			return out, true
		}
		out = append(out, it)
	}
	return out, false
}

func (b *ScopeBuffer) getByID(id string) (Item, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	item, ok := b.byID[id]
	return item, ok
}

func (b *ScopeBuffer) getBySeq(seq uint64) (Item, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	item, ok := b.bySeq[seq]
	return item, ok
}

// ScopeStats is the typed snapshot of a single ScopeBuffer. It is what
// buf.stats() returns so callers inside the package (e.g. the candidate-
// selection path) can read fields directly, and what the /stats handler
// flattens into orderedFields for the wire format.
type ScopeStats struct {
	ItemCount       int
	LastSeq         uint64
	ApproxScopeMB   MB
	CreatedTS       int64
	LastAccessTS    int64
	ReadCountTotal  uint64
	Last7DReadCount uint64
}

// stats returns a snapshot of this scope's metrics. The caller passes
// `now` so Last7DReadCount reflects the rolling window ending at the
// caller's clock — last7DReadCount the runtime field is only updated
// by recordRead, so a scope that hasn't been read in 7+ days would
// otherwise still report a stale "warm" count to /stats and
// /delete_scope_candidates.
func (b *ScopeBuffer) stats(now int64) ScopeStats {
	b.mu.RLock()
	defer b.mu.RUnlock()

	return ScopeStats{
		ItemCount:       len(b.items),
		LastSeq:         b.lastSeq,
		ApproxScopeMB:   MB(b.approxSizeBytesLocked()),
		CreatedTS:       b.createdTS,
		LastAccessTS:    b.lastAccessTS.Load(),
		ReadCountTotal:  b.readCountTotal.Load(),
		Last7DReadCount: b.computeLast7DReadCount(now),
	}
}

type Store struct {
	// shards splits the scope map into numShards independently-locked
	// buckets. Per-scope hot paths (getOrCreate, lookup, delete) take
	// only one shard's lock; multi-shard ops (/wipe, /rebuild, /warm,
	// /admin /delete_guarded) take a sorted subset in ascending index
	// order. Replaces an earlier single store-wide RWMutex that
	// serialised every scope creation through one queue — see phase-4
	// finding "/append to a unique scope per request serializes on the
	// store-wide write lock".
	shards   [numShards]scopeShard
	hashSeed maphash.Seed

	defaultMaxItems int
	maxStoreBytes   int64
	maxItemBytes    int64
	// maxResponseBytes caps the byte size of responses on read endpoints
	// whose body can grow with limit × per-item-cap (/tail, /head,
	// /ts_range). Enforced at the response-writer layer. Conceptually an
	// HTTP-layer concern, but lives here so the existing pattern
	// (NewStore takes the whole Config; NewAPI reads from Store) holds for
	// every adapter. Surfaced (in MiB) on /help and inside 507 bodies.
	maxResponseBytes int64
	// maxMultiCallBytes caps the input body for /multi_call (in bytes);
	// maxMultiCallCount caps the number of sub-calls per batch. Same
	// reasoning as maxResponseBytes for living on Store: keeps adapters
	// flowing every knob through Config without piping HTTP-layer caps
	// through a separate constructor.
	maxMultiCallBytes int64
	maxMultiCallCount int
	// serverSecret is the HMAC key for /guarded. Empty string means
	// /guarded is disabled (route not registered). See guardedflow.md §I.
	serverSecret string
	// inboxScopes is the set of scope names /inbox is allowed to
	// write to. Empty (or nil) means /inbox is disabled — the route
	// is not registered. Operator opt-in for shared write-only
	// ingestion patterns.
	inboxScopes map[string]bool
	// maxInboxBytes caps the per-call payload size on /inbox. Tighter
	// than maxItemBytes by design — see Config.MaxInboxBytes for the
	// full reasoning. Enforced as a 400 (per-request shape rule, same
	// status class as the per-item cap), not a 507.
	maxInboxBytes int64
	// enableAdmin gates whether /admin is registered. False → /admin
	// returns 404. Operator opt-in to expose the operator-elevated
	// dispatcher. See Config.EnableAdmin for the rationale.
	enableAdmin bool
	// disableReadHeat skips recordRead() on every hot read-path call
	// (/get, /render, /head, /tail, /ts_range). Default zero-value
	// (false) keeps heat tracking on; setting true saves the
	// time.Now() call plus the atomic adds on the heat counters. See
	// Config.DisableReadHeat.
	disableReadHeat bool
	// totalBytes tracks the running sum of approxItemSize across every item
	// in every scope. Kept in an atomic so /append can reserve against it
	// without touching the store-level mutex; writes that would push it past
	// maxStoreBytes are rejected with StoreFullError.
	//
	// This is the authoritative counter for admission control and is surfaced
	// (converted to MiB) as approx_store_mb in /stats. It is deliberately
	// leaner than ScopeBuffer.approxSizeBytes — keeping it a pure approxItemSize
	// sum means the budget the client sees in a 507 response matches the budget
	// reserveBytes enforces.
	totalBytes atomic.Int64
}

func NewStore(c Config) *Store {
	c = c.WithDefaults()
	inboxSet := make(map[string]bool, len(c.InboxScopes))
	for _, name := range c.InboxScopes {
		if name != "" {
			inboxSet[name] = true
		}
	}
	s := &Store{
		hashSeed:          maphash.MakeSeed(),
		defaultMaxItems:   c.ScopeMaxItems,
		maxStoreBytes:     c.MaxStoreBytes,
		maxItemBytes:      c.MaxItemBytes,
		maxResponseBytes:  c.MaxResponseBytes,
		maxMultiCallBytes: c.MaxMultiCallBytes,
		maxMultiCallCount: c.MaxMultiCallCount,
		serverSecret:      c.ServerSecret,
		inboxScopes:       inboxSet,
		maxInboxBytes:     c.MaxInboxBytes,
		enableAdmin:       c.EnableAdmin,
		disableReadHeat:   c.DisableReadHeat,
	}
	for i := range s.shards {
		s.shards[i].scopes = make(map[string]*ScopeBuffer)
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
func (s *Store) shardsForScopes(scopes []string) []*scopeShard {
	var seen [numShards]bool
	indices := make([]int, 0, numShards)
	for _, scope := range scopes {
		idx := int(s.shardIdxFor(scope))
		if !seen[idx] {
			seen[idx] = true
			indices = append(indices, idx)
		}
	}
	sort.Ints(indices)
	out := make([]*scopeShard, len(indices))
	for i, idx := range indices {
		out[i] = &s.shards[idx]
	}
	return out
}

// isInboxScope reports whether `name` is in the operator-configured
// allowlist of /inbox target scopes. Used by handleInbox to reject
// writes to scope names the operator has not opted into.
func (s *Store) isInboxScope(name string) bool {
	return s.inboxScopes[name]
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
// scope, on top of the scope's items. Covers the *ScopeBuffer struct
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

// newScopeBuffer builds a fresh ScopeBuffer bound to this store so its
// mutations can participate in byte tracking. Keeping this helper on the
// store means every production path creates bound buffers; tests that
// exercise ScopeBuffer in isolation use NewScopeBuffer directly and
// accept that byte tracking is a no-op there.
func (s *Store) newScopeBuffer() *ScopeBuffer {
	b := NewScopeBuffer(s.defaultMaxItems)
	b.store = s
	return b
}

func (s *Store) getOrCreateScope(scope string) (*ScopeBuffer, error) {
	buf, _, err := s.getOrCreateScopeTrackingCreated(scope)
	return buf, err
}

// getOrCreateScopeTrackingCreated is the variant used by the atomic
// write paths (AppendOne, UpsertOne, CounterAddOne) that need to know
// whether the buffer was freshly allocated by this call. Callers use
// the `created` flag to roll the empty scope back when the subsequent
// item-byte reservation fails — see cleanupIfEmptyAndUnused. All other
// callers go through getOrCreateScope, which discards the flag.
func (s *Store) getOrCreateScopeTrackingCreated(scope string) (*ScopeBuffer, bool, error) {
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
	preBuf := s.newScopeBuffer()

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
func (s *Store) cleanupIfEmptyAndUnused(scope string, buf *ScopeBuffer) {
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

// AppendOne is the atomic /append write-path. It creates the target
// scope on demand, reserves item bytes, commits the item, and rolls
// back the empty scope on item-reservation failure so a 507 cannot
// leak per-scope overhead onto the store-byte cap. See
// cleanupIfEmptyAndUnused for the rollback semantics.
func (s *Store) AppendOne(item Item) (Item, error) {
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

// UpsertOne is the atomic /upsert write-path; same rollback contract
// as AppendOne. Returns (item, created, err) where created reflects
// the upsert outcome, not the scope-creation outcome.
func (s *Store) UpsertOne(item Item) (Item, bool, error) {
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

// CounterAddOne is the atomic /counter_add write-path; same rollback
// contract as AppendOne. Returns (value, created, err) where created
// reflects the counter outcome, not the scope-creation outcome.
func (s *Store) CounterAddOne(scope, id string, by int64) (int64, bool, error) {
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

// ensureScope returns the named scope, creating an empty buffer if it
// does not yet exist. Used by /guarded to lazily provision its internal
// counter scopes (`_counters_count_calls`, `_counters_count_kb`) without
// requiring operator pre-provisioning. Idempotent — safe to call on
// every request; cost is one map lookup under the read-lock when the
// scope already exists.
//
// Unlike getOrCreateScope, this method does not validate the scope name
// and is intended only for cache-internal infrastructure scopes whose
// names are compile-time constants.
//
// Reserves scopeBufferOverhead just like getOrCreateScope on the
// create path. This is required for accounting symmetry: deleteScope
// unconditionally subtracts (scopeBytes + scopeBufferOverhead), so an
// /admin /delete_scope on these counter scopes would otherwise drift
// totalBytes scopeBufferOverhead bytes too low per cycle (potentially
// negative) — the bytes-counter invariant is "totalBytes == sum of
// reservations". Returns nil when the cap is exhausted; callers
// (guardedIncrementCounters) treat counter writes as best-effort and
// silently skip on nil, since counters are observability, not auth.
func (s *Store) ensureScope(scope string) *ScopeBuffer {
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

	buf = s.newScopeBuffer()
	sh.scopes[scope] = buf
	return buf
}

func (s *Store) getScope(scope string) (*ScopeBuffer, bool) {
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

	for i := range s.shards {
		s.shards[i].mu.Lock()
	}
	defer func() {
		for i := range s.shards {
			s.shards[i].mu.Unlock()
		}
	}()

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
			deletedItems += len(buf.items)
			scopeBytes := buf.bytes
			delete(sh.scopes, scope)
			// Combined into one Add so observers never see a transient state
			// with one released and the other still charged. Same shape as
			// deleteScope: item bytes + per-scope overhead, in lockstep.
			s.totalBytes.Add(-(scopeBytes + scopeBufferOverhead))
			freedBytes += scopeBytes + scopeBufferOverhead
			buf.detached = true
			buf.store = nil
			buf.mu.Unlock()
			deletedScopes++
		}
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
	for i := range s.shards {
		s.shards[i].mu.Lock()
	}
	defer func() {
		for i := range s.shards {
			s.shards[i].mu.Unlock()
		}
	}()

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
	for i := range s.shards {
		s.shards[i].scopes = make(map[string]*ScopeBuffer)
	}

	return scopeCount, totalItems, freedBytes
}

// StoreStats is the typed snapshot of the store. stats() returns it so the
// /stats handler can flatten it into orderedFields for the wire, and so any
// in-package caller (tests, future adapters) can read fields directly.
type StoreStats struct {
	ScopeCount    int
	TotalItems    int
	ApproxStoreMB MB
	MaxStoreMB    MB
	Scopes        map[string]ScopeStats
}

func (s *Store) stats() StoreStats {
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
	scopeStats := make(map[string]ScopeStats)
	totalItems := 0

	for i := range s.shards {
		sh := &s.shards[i]
		sh.mu.RLock()
		for scope, buf := range sh.scopes {
			st := buf.stats(now)
			scopeStats[scope] = st
			totalItems += st.ItemCount
		}
		sh.mu.RUnlock()
	}

	return StoreStats{
		ScopeCount:    len(scopeStats),
		TotalItems:    totalItems,
		ApproxStoreMB: MB(s.totalBytes.Load()),
		MaxStoreMB:    MB(s.maxStoreBytes),
		Scopes:        scopeStats,
	}
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
	for _, sh := range shards {
		sh.mu.Lock()
	}
	defer func() {
		for _, sh := range shards {
			sh.mu.Unlock()
		}
	}()

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
	for _, p := range plans {
		sh := s.shardFor(p.scope)
		buf, ok := sh.scopes[p.scope]
		if !ok {
			buf = s.newScopeBuffer()
			sh.scopes[p.scope] = buf
		}
		buf.commitReplacementPreReserved(p.replacement, p.newBytes, p.oldBytes)
	}

	return len(plans), nil
}

func (s *Store) rebuildAll(grouped map[string][]Item) (int, int, error) {
	// Phase 1 — build every scope buffer off-map and distribute directly
	// into the per-shard maps that Phase 2 will swap in. If any scope
	// fails validation the existing store is left fully intact. Capacity
	// offenders are collected across the whole batch; any offender aborts
	// the rebuild.
	var newShardMaps [numShards]map[string]*ScopeBuffer
	for i := range newShardMaps {
		newShardMaps[i] = make(map[string]*ScopeBuffer)
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
		buf := s.newScopeBuffer()
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
	// buffer). Mirrors wipe and /delete_scope; see ScopeBuffer.detached.
	for i := range s.shards {
		s.shards[i].mu.Lock()
	}
	defer func() {
		for i := range s.shards {
			s.shards[i].mu.Unlock()
		}
	}()
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

	return totalScopes, totalItems, nil
}

func (s *Store) listScopes() map[string]*ScopeBuffer {
	out := make(map[string]*ScopeBuffer)
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
