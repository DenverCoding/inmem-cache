package scopecache

import (
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
)

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
	detached        bool
	items           []Item
	byID            map[string]Item
	bySeq           map[uint64]Item
	lastSeq         uint64
	maxItems        int
	// bytes is the running sum of approxItemSize(item) over items. Only
	// mutated under b.mu; the store-level total is kept in sync via
	// Store.reserveBytes / commitReplacement.
	bytes           int64
	createdTS       int64
	lastAccessTS    int64
	readCountTotal  uint64
	last7DReadCount uint64
	// Ring buffer indexed by day % ReadHeatWindowDays. Each bucket carries the
	// absolute day it represents so we can detect a stale slot when it wraps.
	readHeatBuckets [ReadHeatWindowDays]ScopeReadHeatBucket
}

func NewScopeBuffer(maxItems int) *ScopeBuffer {
	return &ScopeBuffer{
		items:     make([]Item, 0, maxItems),
		byID:      make(map[string]Item),
		bySeq:     make(map[uint64]Item),
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
// Candidate.ApproxScopeMB field in /delete_scope_candidates. It is NOT used for
// cap enforcement — admission control uses Store.totalBytes (a pure
// approxItemSize sum) so the 507 budget matches what reserveBytes accounts
// for.
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

func (b *ScopeBuffer) recordRead(now int64) {
	b.mu.Lock()
	defer b.mu.Unlock()

	day := unixDay(now)
	oldestValidDay := day - ReadHeatWindowDays + 1

	// Expire any bucket whose day has fallen out of the rolling window.
	// A bucket with Day == 0 has never been touched and needs no cleanup.
	for i := range b.readHeatBuckets {
		bucket := &b.readHeatBuckets[i]
		if bucket.Day > 0 && bucket.Day < oldestValidDay {
			if b.last7DReadCount >= bucket.Count {
				b.last7DReadCount -= bucket.Count
			} else {
				b.last7DReadCount = 0
			}
			*bucket = ScopeReadHeatBucket{}
		}
	}

	// After the expiry pass, the current slot is either empty or already on today.
	// Other days in the same slot would be >= 7 days old and were expired above.
	bucketIndex := int(day % ReadHeatWindowDays)
	bucket := &b.readHeatBuckets[bucketIndex]
	if bucket.Day != day {
		bucket.Day = day
		bucket.Count = 0
	}

	bucket.Count++
	b.readCountTotal++
	b.last7DReadCount++
	b.lastAccessTS = now
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
	b.bySeq[item.Seq] = item
	if item.ID != "" {
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

		for i := range b.items {
			if b.items[i].ID != item.ID {
				continue
			}
			b.items[i].Payload = item.Payload
			// /upsert has replace-the-whole-item semantics, so ts follows the
			// client's input exactly: send ts → stored, omit → cleared. That
			// differs from /update (which treats absent ts as "preserve").
			b.items[i].Ts = item.Ts

			updated := b.items[i]
			b.bySeq[updated.Seq] = updated
			b.byID[item.ID] = updated
			b.bytes += delta
			return updated, false, nil
		}

		// Unreachable under b.mu: b.byID confirmed the item exists and items/byID are kept in sync.
		return Item{}, false, nil
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
	b.bySeq[item.Seq] = item
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
		delta := int64(len(newPayload)) - int64(len(existing.Payload))
		if b.store != nil && delta != 0 {
			ok, total, max := b.store.reserveBytes(delta)
			if !ok {
				return 0, false, &StoreFullError{StoreBytes: total, AddedBytes: delta, Cap: max}
			}
		}

		for i := range b.items {
			if b.items[i].ID != id {
				continue
			}
			b.items[i].Payload = newPayload

			updated := b.items[i]
			b.bySeq[updated.Seq] = updated
			b.byID[id] = updated
			b.bytes += delta
			return newValue, false, nil
		}

		// Unreachable under b.mu: b.byID confirmed the item exists and items/byID are kept in sync.
		return 0, false, nil
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
	b.bySeq[item.Seq] = item
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
	delta := int64(len(payload)) - int64(len(existing.Payload))
	if b.store != nil && delta != 0 {
		ok, current, max := b.store.reserveBytes(delta)
		if !ok {
			return 0, &StoreFullError{StoreBytes: current, AddedBytes: delta, Cap: max}
		}
	}

	for i := range b.items {
		if b.items[i].ID != id {
			continue
		}

		b.items[i].Payload = payload
		if ts != nil {
			b.items[i].Ts = ts
		}

		updated := b.items[i]
		b.bySeq[updated.Seq] = updated
		b.byID[id] = updated
		b.bytes += delta
		return 1, nil
	}

	// Unreachable under b.mu: b.byID confirmed the item exists and items/byID are kept in sync.
	return 0, nil
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

	delta := int64(len(payload)) - int64(len(existing.Payload))
	if b.store != nil && delta != 0 {
		ok, current, max := b.store.reserveBytes(delta)
		if !ok {
			return 0, &StoreFullError{StoreBytes: current, AddedBytes: delta, Cap: max}
		}
	}

	i := sort.Search(len(b.items), func(i int) bool {
		return b.items[i].Seq >= seq
	})
	if i == len(b.items) || b.items[i].Seq != seq {
		// Unreachable under b.mu: b.bySeq confirmed the item exists and items/bySeq are kept in sync.
		return 0, nil
	}

	b.items[i].Payload = payload
	if ts != nil {
		b.items[i].Ts = ts
	}

	updated := b.items[i]
	b.bySeq[seq] = updated
	if updated.ID != "" {
		b.byID[updated.ID] = updated
	}
	b.bytes += delta
	return 1, nil
}

func (b *ScopeBuffer) deleteByID(id string) int {
	b.mu.Lock()
	defer b.mu.Unlock()

	existing, ok := b.byID[id]
	if !ok {
		return 0
	}

	// items is ordered ascending by seq (monotonic append, no mid-slice
	// inserts), so binary search finds the index in O(log n) rather than
	// scanning the slice by id.
	i := sort.Search(len(b.items), func(i int) bool {
		return b.items[i].Seq >= existing.Seq
	})
	if i == len(b.items) || b.items[i].Seq != existing.Seq {
		// Unreachable under b.mu: b.byID confirmed the item exists and items/bySeq are kept in sync.
		return 0
	}

	removedSize := approxItemSize(existing)
	// Shift the tail down, then zero the now-duplicate last slot before
	// shrinking. Without this the backing array keeps a reference and
	// the Item's payload cannot be GC'd.
	copy(b.items[i:], b.items[i+1:])
	b.items[len(b.items)-1] = Item{}
	b.items = b.items[:len(b.items)-1]
	delete(b.bySeq, existing.Seq)
	delete(b.byID, id)

	b.bytes -= removedSize
	if b.store != nil {
		b.store.totalBytes.Add(-removedSize)
	}
	return 1
}

func (b *ScopeBuffer) deleteBySeq(seq uint64) int {
	b.mu.Lock()
	defer b.mu.Unlock()

	if _, ok := b.bySeq[seq]; !ok {
		return 0
	}

	// items is ordered ascending by seq (monotonic append, no mid-slice
	// inserts), so binary search finds the index in O(log n).
	i := sort.Search(len(b.items), func(i int) bool {
		return b.items[i].Seq >= seq
	})
	if i == len(b.items) || b.items[i].Seq != seq {
		return 0
	}

	removed := b.items[i]
	removedSize := approxItemSize(removed)
	copy(b.items[i:], b.items[i+1:])
	b.items[len(b.items)-1] = Item{}
	b.items = b.items[:len(b.items)-1]
	delete(b.bySeq, seq)
	if removed.ID != "" {
		delete(b.byID, removed.ID)
	}

	b.bytes -= removedSize
	if b.store != nil {
		b.store.totalBytes.Add(-removedSize)
	}
	return 1
}

// deleteUpToSeq removes every item with Seq <= maxSeq. b.items is always
// ordered ascending by Seq (appendItem assigns monotonic seqs and nothing
// removes from the middle), so binary search finds the cut point in O(log n).
// Returns the number of items removed.
func (b *ScopeBuffer) deleteUpToSeq(maxSeq uint64) int {
	b.mu.Lock()
	defer b.mu.Unlock()

	idx := sort.Search(len(b.items), func(i int) bool {
		return b.items[i].Seq > maxSeq
	})
	if idx == 0 {
		return 0
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
	return idx
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

func (b *ScopeBuffer) stats() ScopeStats {
	b.mu.RLock()
	defer b.mu.RUnlock()

	return ScopeStats{
		ItemCount:       len(b.items),
		LastSeq:         b.lastSeq,
		ApproxScopeMB:   MB(b.approxSizeBytesLocked()),
		CreatedTS:       b.createdTS,
		LastAccessTS:    b.lastAccessTS,
		ReadCountTotal:  b.readCountTotal,
		Last7DReadCount: b.last7DReadCount,
	}
}

type Store struct {
	mu              sync.RWMutex
	scopes          map[string]*ScopeBuffer
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
	return &Store{
		scopes:            make(map[string]*ScopeBuffer),
		defaultMaxItems:   c.ScopeMaxItems,
		maxStoreBytes:     c.MaxStoreBytes,
		maxItemBytes:      c.MaxItemBytes,
		maxResponseBytes:  c.MaxResponseBytes,
		maxMultiCallBytes: c.MaxMultiCallBytes,
		maxMultiCallCount: c.MaxMultiCallCount,
		serverSecret:      c.ServerSecret,
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
	if scope == "" {
		return nil, errors.New("the 'scope' field is required")
	}

	s.mu.RLock()
	buf, ok := s.scopes[scope]
	s.mu.RUnlock()
	if ok {
		return buf, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	buf, ok = s.scopes[scope]
	if ok {
		return buf, nil
	}

	buf = s.newScopeBuffer()
	s.scopes[scope] = buf
	return buf, nil
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
func (s *Store) ensureScope(scope string) *ScopeBuffer {
	s.mu.RLock()
	buf, ok := s.scopes[scope]
	s.mu.RUnlock()
	if ok {
		return buf
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	buf, ok = s.scopes[scope]
	if ok {
		return buf
	}

	buf = s.newScopeBuffer()
	s.scopes[scope] = buf
	return buf
}

func (s *Store) getScope(scope string) (*ScopeBuffer, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	buf, ok := s.scopes[scope]
	return buf, ok
}

func (s *Store) deleteScope(scope string) (int, bool) {
	if scope == "" {
		return 0, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	buf, ok := s.scopes[scope]
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
	delete(s.scopes, scope)
	if scopeBytes > 0 {
		s.totalBytes.Add(-scopeBytes)
	}
	buf.detached = true
	buf.store = nil
	buf.mu.Unlock()
	return itemCount, true
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
	s.mu.Lock()
	defer s.mu.Unlock()

	scopeCount := len(s.scopes)
	totalItems := 0

	for _, buf := range s.scopes {
		buf.mu.Lock()
		totalItems += len(buf.items)
		buf.detached = true
		buf.store = nil
		buf.mu.Unlock()
	}

	freedBytes := s.totalBytes.Swap(0)
	s.scopes = make(map[string]*ScopeBuffer)

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
	// Single-pass snapshot under one store lock: scope_count and total_items
	// reflect the same set of scopes. A prior version released the store lock
	// between the per-scope walk and a separate pass, allowing the counts in
	// one response to disagree.
	s.mu.RLock()
	defer s.mu.RUnlock()

	scopeStats := make(map[string]ScopeStats, len(s.scopes))
	totalItems := 0

	for scope, buf := range s.scopes {
		st := buf.stats()
		scopeStats[scope] = st
		totalItems += st.ItemCount
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

	// Phase 1.5 + Phase 2 run under the store-level write lock to serialise
	// against /delete_scope, /wipe, and /rebuild. Without that mutual
	// exclusion the byte counter desyncs from Σ buf.bytes when one of those
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
	// Concurrent appends/updates/etc. on the SAME scopes /warm is replacing
	// still proceed: they take buf.mu, not s.mu, and the drift comp inside
	// commitReplacementPreReserved still reconciles per-scope races as
	// before. What this lock blocks is the destructive store-map ops and
	// new-scope creation (getOrCreateScope's slow path) — both intentional.
	s.mu.Lock()
	defer s.mu.Unlock()

	// Phase 1.5 — snapshot per-scope b.bytes (under each scope's RLock so
	// concurrent in-scope writers are observed consistently), compute the
	// net batch delta, and CAS-reserve it against the store counter.
	var totalDelta int64
	for i := range plans {
		var old int64
		if buf, ok := s.scopes[plans[i].scope]; ok {
			buf.mu.RLock()
			old = buf.bytes
			buf.mu.RUnlock()
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

	// Phase 2 — create-on-demand and commit. We hold s.mu.Lock() so we can
	// touch s.scopes directly (calling getOrCreateScope here would deadlock
	// on its internal RLock/Lock pair). Neither step can fail, so either
	// every scope is replaced or (if an earlier phase aborted) none are.
	for _, p := range plans {
		buf, ok := s.scopes[p.scope]
		if !ok {
			buf = s.newScopeBuffer()
			s.scopes[p.scope] = buf
		}
		buf.commitReplacementPreReserved(p.replacement, p.newBytes, p.oldBytes)
	}

	return len(plans), nil
}

func (s *Store) rebuildAll(grouped map[string][]Item) (int, int, error) {
	// Phase 1 — build every scope buffer off-map. If any scope fails
	// validation the existing store is left fully intact. Capacity offenders
	// are collected across the whole batch; any offender aborts the rebuild.
	newScopes := make(map[string]*ScopeBuffer, len(grouped))
	totalItems := 0
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
		// store counter is reset in phase 2 once the new map is swapped.
		buf := s.newScopeBuffer()
		buf.items = r.items
		buf.byID = r.byID
		buf.bySeq = r.bySeq
		buf.lastSeq = r.lastSeq
		buf.bytes = sumItemBytes(r.items)
		newScopes[scope] = buf
		totalItems += len(r.items)
		totalNewBytes += buf.bytes
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

	// Phase 2 — detach every existing buffer, then swap the store map and
	// reset the byte counter under one lock. Detaching is essential:
	// without it, a concurrent /append holding a stale buf pointer obtained
	// via getOrCreateScope would run AFTER the swap and call reserveBytes
	// against the freshly reset counter, permanently inflating totalBytes
	// (its item lands in an unreachable orphan buffer). Mirrors wipe and
	// /delete_scope; see ScopeBuffer.detached.
	//
	// The scope count is returned to the /rebuild handler, so it must
	// reflect the state as handed over — not the state after a concurrent
	// getOrCreateScope has already begun writing into s.scopes (which is
	// the same map as newScopes after the swap). defer-Unlock plus a return
	// expression keeps the read under the lock.
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, buf := range s.scopes {
		buf.mu.Lock()
		buf.detached = true
		buf.store = nil
		buf.mu.Unlock()
	}
	s.scopes = newScopes
	s.totalBytes.Store(totalNewBytes)

	return len(newScopes), totalItems, nil
}

func (s *Store) listScopes() map[string]*ScopeBuffer {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make(map[string]*ScopeBuffer, len(s.scopes))
	for k, v := range s.scopes {
		out[k] = v
	}
	return out
}
