// Locking invariants for *scopeBuffer
// -----------------------------------
//
//  1. Lock-acquisition order is strictly TOP-DOWN:
//     Store-level code may acquire scopeShard.mu BEFORE buf.mu.
//     Multi-shard ops additionally acquire shard.mu in ascending
//     shard-index order (see numShards comment in store.go).
//
//  2. scopeBuffer methods MUST NOT reach back up to acquire any
//     Store-level lock — neither scopeShard.mu nor any future
//     Store-side mutex — while holding b.mu. The only Store-state a
//     scopeBuffer method may touch with b.mu held is the atomic
//     counter (b.store.totalBytes.Add / b.store.reserveBytes); those
//     take no locks. Reverse-direction locking (buf → shard) would
//     deadlock against deleteScope, replaceScopes, wipe and
//     rebuildAll, all of which take shard.mu first and then
//     individual buf.mu's.
//
//  3. Read-path bookkeeping (recordRead) runs without taking b.mu —
//     it bumps the readCountTotal and lastAccessTS atomics directly.
//     This is what lets concurrent readers hold b.mu.RLock simultaneously
//     without serialising on the read counters. See recordRead in
//     buffer_heat.go.
//
// Adding a new scopeBuffer method that violates rule 2 is the most
// likely future deadlock — flag it in code review.
//
// File layout for scopeBuffer methods:
//
//	buffer.go          — struct + ctor + this invariant header
//	buffer_locked.go   — cross-cutting helpers (precomputeRenderBytes,
//	                     indexBySeqLocked, replaceItemAtIndexLocked,
//	                     reservePayloadDeltaLocked)
//	buffer_heat.go     — lock-free recordRead bookkeeping
//	buffer_write.go    — appendItem, upsertByID, updateByID, updateBySeq
//	buffer_counter.go  — counterAdd, parseCounterValue
//	buffer_delete.go   — deleteByID, deleteBySeq, deleteUpToSeq, deleteIndexLocked
//	buffer_replace.go  — scopeReplacement type, build / commit pipeline, replaceAll
//	buffer_read.go     — tailOffset, sinceSeq, getByID, getBySeq
//	buffer_stats.go    — approxSizeBytes, scopeStats type, stats()

package scopecache

import (
	"sync"
	"sync/atomic"
)

type scopeBuffer struct {
	mu sync.RWMutex
	// store is set when the buffer is owned by a Store. When nil (orphan
	// buffers used in unit tests) byte-budget accounting is skipped — the
	// tests exercise item-count and seq logic without spinning up a store.
	store *store
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
	// maxItems caps the number of items this scope may hold; writes
	// past it produce *ScopeFullError. The unboundedScopeMaxItems
	// sentinel (= 0) means "no count cap" — the write paths skip the
	// check entirely. Only the reserved `_events` scope is created with
	// the sentinel (best-effort observability gated by the global byte
	// budget alone); every other scope, including `_inbox`, gets a
	// concrete positive cap installed at create time. See
	// Store.maxItemsFor in store.go for the per-scope dispatch.
	maxItems int
	// bytes is the running sum of approxItemSize(item) over items. Only
	// mutated under b.mu; the store-level total is kept in sync via
	// Store.reserveBytes (single-item write paths) and
	// scopeBuffer.commitReplacement (bulk /warm and /rebuild).
	bytes int64
	// idKeyBytes is the running sum of len(item.ID) over items with a
	// non-empty ID. Lets approxSizeBytesLocked surface a per-scope
	// memory estimate in O(1) without re-walking byID. Mutated under
	// b.mu by every path that mutates b.byID. Observability-only —
	// NOT charged against the store-byte cap (admission control stays
	// layout-independent).
	idKeyBytes int64
	createdTS  int64
	// lastWriteTS is the microsecond timestamp of the most recent
	// write that touched this scope. Set under b.mu by every write
	// path; read under b.mu.RLock by stats(). Initialised equal to
	// createdTS so a freshly-created scope reports a non-zero value
	// (creation is the first "touch"). Surfaced as last_write_ts on
	// /stats; distinct from lastAccessTS, which tracks reads.
	lastWriteTS int64
	// lastAccessTS and readCountTotal are atomic so the read-hot path
	// (recordRead) does not need to take b.mu. recordRead used to take
	// b.mu.Lock() — a write lock on the same mutex readers were just on
	// under RLock — turning every hit on /get, /render, /head and /tail
	// into a serialise point. Block profiling pinned ~88% of all
	// read-path lock-wait time to that one call. Atomic stores drop it
	// to effectively zero.
	lastAccessTS   atomic.Int64
	readCountTotal atomic.Uint64
}

func newscopeBuffer(maxItems int) *scopeBuffer {
	// items, byID and bySeq grow on-demand. Pre-allocating any of
	// them on every scope-create is the wrong default: under unique-
	// scope-per-write workloads (millions of one-item buffers) the
	// pre-alloc was a GC bottleneck, and items without an `id` never
	// need byID at all.
	//
	// Write paths (buffer_write.go, buffer_counter.go) and
	// replaceItemAtIndexLocked lazily initialise these maps before
	// assigning into them. Reads, deletes, len and range are nil-safe
	// in Go and need no guard.
	now := nowUnixMicro()
	return &scopeBuffer{
		maxItems:    maxItems,
		createdTS:   now,
		lastWriteTS: now,
	}
}
