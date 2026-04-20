package scopecache

import (
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
)

type ScopeBuffer struct {
	mu sync.RWMutex
	// store is set when the buffer is owned by a Store. When nil (orphan
	// buffers used in unit tests) byte-budget accounting is skipped — the
	// tests exercise item-count and seq logic without spinning up a store.
	store           *Store
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
// Candidate.ApproxScopeMB field in /delete-scope-candidates. It is NOT used for
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

func (b *ScopeBuffer) updateByID(id string, payload json.RawMessage) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	existing, ok := b.byID[id]
	if !ok {
		return 0, nil
	}

	// Only the payload changes on /update; scope/id are unchanged, so the
	// byte delta reduces to len(new_payload) - len(old_payload). A shrink
	// can't fail the cap check, but a grow must reserve first.
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

		updated := b.items[i]
		b.bySeq[updated.Seq] = updated
		b.byID[id] = updated
		b.bytes += delta
		return 1, nil
	}

	// Unreachable under b.mu: b.byID confirmed the item exists and items/byID are kept in sync.
	return 0, nil
}

func (b *ScopeBuffer) updateBySeq(seq uint64, payload json.RawMessage) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

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

	if _, ok := b.byID[id]; !ok {
		return 0
	}

	for i := range b.items {
		if b.items[i].ID != id {
			continue
		}

		removed := b.items[i]
		removedSize := approxItemSize(removed)
		// Shift the tail down, then zero the now-duplicate last slot before
		// shrinking. Without this the backing array keeps a reference and
		// the Item's payload cannot be GC'd.
		copy(b.items[i:], b.items[i+1:])
		b.items[len(b.items)-1] = Item{}
		b.items = b.items[:len(b.items)-1]
		delete(b.bySeq, removed.Seq)
		delete(b.byID, id)

		b.bytes -= removedSize
		if b.store != nil {
			b.store.totalBytes.Add(-removedSize)
		}
		return 1
	}

	return 0
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

	// Zero each slot before resliceing past it. Reslicing alone leaves the
	// Items reachable via the backing array, pinning their payloads against
	// GC.
	var freedBytes int64
	for i := 0; i < idx; i++ {
		removed := b.items[i]
		freedBytes += approxItemSize(removed)
		delete(b.bySeq, removed.Seq)
		if removed.ID != "" {
			delete(b.byID, removed.ID)
		}
		b.items[i] = Item{}
	}
	b.items = b.items[idx:]

	b.bytes -= freedBytes
	if b.store != nil {
		b.store.totalBytes.Add(-freedBytes)
	}
	return idx
}

func (b *ScopeBuffer) tailOffset(limit int, offset int) []Item {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if limit <= 0 || offset < 0 {
		return []Item{}
	}
	if offset >= len(b.items) {
		return []Item{}
	}

	end := len(b.items) - offset
	start := end - limit
	if start < 0 {
		start = 0
	}
	if start >= end {
		return []Item{}
	}

	return append([]Item(nil), b.items[start:end]...)
}

func (b *ScopeBuffer) sinceSeq(afterSeq uint64, limit int) []Item {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if len(b.items) == 0 {
		return []Item{}
	}

	idx := sort.Search(len(b.items), func(i int) bool {
		return b.items[i].Seq > afterSeq
	})

	if idx >= len(b.items) {
		return []Item{}
	}

	out := append([]Item(nil), b.items[idx:]...)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
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

func (b *ScopeBuffer) stats() map[string]interface{} {
	b.mu.RLock()
	defer b.mu.RUnlock()

	return map[string]interface{}{
		"item_count":         len(b.items),
		"last_seq":           b.lastSeq,
		"created_ts":         b.createdTS,
		"last_access_ts":     b.lastAccessTS,
		"last_7d_read_count": b.last7DReadCount,
		"read_count_total":   b.readCountTotal,
		"approx_scope_mb":    MB(b.approxSizeBytesLocked()),
	}
}

type Store struct {
	mu              sync.RWMutex
	scopes          map[string]*ScopeBuffer
	defaultMaxItems int
	maxStoreBytes   int64
	// totalBytes tracks the running sum of approxItemSize across every item
	// in every scope. Kept in an atomic so /append can reserve against it
	// without touching the store-level mutex; writes that would push it past
	// maxStoreBytes are rejected with StoreFullError.
	//
	// This is the authoritative counter for admission control and is surfaced
	// (converted to MiB) as tracked_store_mb in /stats. It is deliberately
	// leaner than ScopeBuffer.approxSizeBytes — keeping it a pure approxItemSize
	// sum means the budget the client sees in a 507 response matches the budget
	// reserveBytes enforces.
	totalBytes atomic.Int64
}

func NewStore(defaultMaxItems int, maxStoreBytes int64) *Store {
	return &Store{
		scopes:          make(map[string]*ScopeBuffer),
		defaultMaxItems: defaultMaxItems,
		maxStoreBytes:   maxStoreBytes,
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
	// Crucially we also clear buf.store: any mutator that wakes up afterwards
	// sees a detached buf and skips all store-counter accounting, so its
	// orphaned work cannot inflate or drain s.totalBytes. The orphan buf
	// stays internally consistent (items, byID, bySeq, bytes keep tracking
	// each other) and is collected once the last stale pointer drops.
	buf.mu.Lock()
	itemCount := len(buf.items)
	scopeBytes := buf.bytes
	delete(s.scopes, scope)
	if scopeBytes > 0 {
		s.totalBytes.Add(-scopeBytes)
	}
	buf.store = nil
	buf.mu.Unlock()
	return itemCount, true
}

func (s *Store) stats() map[string]interface{} {
	// Single-pass snapshot under one store lock: scope_count and total_items
	// reflect the same set of scopes. A prior version released the store lock
	// between the per-scope walk and a separate pass, allowing the counts in
	// one response to disagree.
	s.mu.RLock()
	defer s.mu.RUnlock()

	scopeStats := make(map[string]interface{}, len(s.scopes))
	totalItems := 0

	for scope, buf := range s.scopes {
		st := buf.stats()
		scopeStats[scope] = st
		if n, ok := st["item_count"].(int); ok {
			totalItems += n
		}
	}

	return map[string]interface{}{
		"ok":               true,
		"scope_count":      len(scopeStats),
		"total_items":      totalItems,
		"default_limit":    DefaultLimit,
		"max_limit":        MaxLimit,
		"tracked_store_mb": MB(s.totalBytes.Load()),
		"max_store_mb":     MB(s.maxStoreBytes),
		"scopes":           scopeStats,
	}
}

func (s *Store) replaceScopes(grouped map[string][]Item) (int, error) {
	type plan struct {
		scope       string
		replacement scopeReplacement
		newBytes    int64
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

	// Phase 1.5 — compute net byte delta of the batch against the current
	// store and check the byte cap before committing anything. Snapshots are
	// taken under each scope's RLock so the per-scope contribution is a
	// point-in-time read; concurrent /append to a scope being replaced is
	// tolerated — commitReplacement re-reads b.bytes under b.mu and the
	// fresh per-scope delta cancels out any in-flight additions to
	// s.totalBytes (the appended item is being replaced anyway).
	snapshot := s.totalBytes.Load()
	var totalDelta int64
	for _, p := range plans {
		var old int64
		if buf, ok := s.getScope(p.scope); ok {
			buf.mu.RLock()
			old = buf.bytes
			buf.mu.RUnlock()
		}
		totalDelta += p.newBytes - old
	}
	if snapshot+totalDelta > s.maxStoreBytes {
		return 0, &StoreFullError{
			StoreBytes: snapshot,
			AddedBytes: totalDelta,
			Cap:        s.maxStoreBytes,
		}
	}

	// Phase 2 — get-or-create and commit. Neither step can fail, so either
	// every scope is replaced or (if an earlier phase aborted) none are.
	for _, p := range plans {
		buf, _ := s.getOrCreateScope(p.scope)
		buf.commitReplacement(p.replacement, p.newBytes)
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

	// Phase 2 — swap the store map and reset the byte counter under one
	// lock. Any /append that was blocked on s.mu.RLock sees the new map
	// and the new baseline as a single coherent state.
	s.mu.Lock()
	s.scopes = newScopes
	s.totalBytes.Store(totalNewBytes)
	s.mu.Unlock()

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
