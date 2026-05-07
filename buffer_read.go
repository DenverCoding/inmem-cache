package scopecache

import "sort"

// Read paths on *scopeBuffer:
//
//   - tailOffset  — newest-first window with offset (drives /tail)
//   - sinceSeq    — oldest-first window with after-seq cursor (drives /head)
//   - getByID     — single-item lookup by id (drives /get?id=, /render)
//   - getBySeq    — single-item lookup by seq (drives /get?seq=)
//
// All four take b.mu.RLock so multiple readers run concurrently. None
// check b.detached: reading from a detached buffer returns the state
// the buffer had at detach time, which is fine for reads — no
// orphan-write hazard, only an eventually-stale snapshot. The
// read-bookkeeping (recordRead) runs separately and is lock-free.
//
// Every read path runs returned items through materialiseCounter so
// consumers see the cell's current value/ts rather than the stored
// (stale-by-design) Payload bytes. A single atomic load + fresh
// strconv on counters, no-op otherwise.

func materialiseCounter(item Item) Item {
	if item.counter == nil {
		return item
	}
	item.Payload = renderCounterPayload(item.counter)
	item.Ts = item.counter.ts.Load()
	// renderBytes is never set on counter items — bare integers don't
	// hit the JSON-string shortcut path — so nothing to clear.
	return item
}

// materialiseCountersInPlace walks `items` and rewrites any counter-
// shaped entry with a fresh Payload/Ts derived from its cell. Used by
// the multi-item read paths (tailOffset, sinceSeq) on the slice they
// are about to return; the caller owns the slice (it's a fresh copy of
// b.items) so in-place is safe.
func materialiseCountersInPlace(items []Item) []Item {
	for i := range items {
		if items[i].counter != nil {
			items[i] = materialiseCounter(items[i])
		}
	}
	return items
}

// tailOffset returns the newest-first window `[start, end)` of b.items and a
// hasMore flag. hasMore is true when older items exist before the window (i.e.
// start > 0), signalling to the caller that the response is clipped at the
// oldest end. It does NOT signal truncation at the newest end (that is what
// offset already describes to the client).
func (b *scopeBuffer) tailOffset(limit int, offset int) ([]Item, bool) {
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

	return materialiseCountersInPlace(append([]Item(nil), b.items[start:end]...)), hasMore
}

// sinceSeq returns items with seq > afterSeq, oldest-first, up to
// limit. limit ≤ 0 returns an empty slice — matches every other
// multi-item read on the public surface. HTTP rejects 0/negative
// with 400 via normalizeLimit; the guard here exists for direct
// Gateway callers.
//
// The bool is true when more matching items exist beyond the
// returned slice, so the handler surfaces truncated=true without
// the client guessing from count == limit.
func (b *scopeBuffer) sinceSeq(afterSeq uint64, limit int) ([]Item, bool) {
	if limit <= 0 {
		return []Item{}, false
	}

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
	if available > limit {
		take = limit
		hasMore = true
	}
	out := make([]Item, take)
	copy(out, b.items[idx:idx+take])
	return materialiseCountersInPlace(out), hasMore
}

func (b *scopeBuffer) getByID(id string) (Item, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	item, ok := b.byID[id]
	if !ok {
		return Item{}, false
	}
	return materialiseCounter(item), true
}

func (b *scopeBuffer) getBySeq(seq uint64) (Item, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	item, ok := b.bySeq[seq]
	if !ok {
		return Item{}, false
	}
	return materialiseCounter(item), true
}
