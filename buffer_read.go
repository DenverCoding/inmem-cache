package scopecache

import "sort"

// Read paths on *ScopeBuffer:
//
//   - tailOffset  — newest-first window with offset (drives /tail)
//   - sinceSeq    — oldest-first window with after-seq cursor (drives /head)
//   - getByID     — single-item lookup by id (drives /get?id=, /render)
//   - getBySeq    — single-item lookup by seq (drives /get?seq=)
//
// All four take b.mu.RLock so multiple readers run concurrently. None
// of them check b.detached: reading from a detached buffer returns the
// state the buffer had at detach time, which is fine for reads — there
// is no orphan-write hazard, only an eventually-stale snapshot. The
// hot-path heat tracking (recordRead in buffer_heat.go) runs separately
// and is intentionally lock-free.

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
