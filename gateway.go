package scopecache

// Gateway is the public Go-API for the cache. ALL in-process callers
// — adapters (Caddy module, standalone), addons (subscribers,
// publishers, content-hashers, …) — talk to scopecache via *Gateway.
// It is the wall around the core: the underlying *Store and its
// lowercase methods are NOT part of the public contract.
//
// Methods on Gateway are intentionally thin: each one validates input
// (where applicable) and delegates to the underlying *Store. The
// actual mutation logic lives in store.go, buffer_*.go, events.go,
// and subscribe.go — gateway.go is just the registration of which
// operations are part of the public API.
//
// Why one Gateway instead of separate *API (HTTP) and *Direct (Go):
// external addon authors learn ONE surface, program against ONE file.
// The HTTP adapter (*API) wraps Gateway internally; standalone and
// the Caddy module both wire NewGateway → NewAPI → RegisterRoutes.
// Future addons import scopecache, hold a *Gateway, and build their
// behaviour on top of the methods declared here.
//
// Method shape conventions:
//
//   - Write ops (Append, Upsert, Update, CounterAdd) take typed
//     parameters (Item or scope/id/by); validation runs first, then
//     delegates to the matching Store method.
//   - Delete ops (Delete, DeleteUpTo, DeleteScope, Wipe) take typed
//     parameters; same validate-then-delegate shape.
//   - Read ops (Head, Tail, Get, Render) take typed parameters and
//     return ([items|item|bytes], hit, found). No validation — read
//     paths are pure look-up.
//   - Bulk ops (Warm, Rebuild) take a `map[string][]Item` shape that
//     mirrors what `groupItemsByScope` produces from an ItemsRequest;
//     callers that have an `[]Item` flat list group it themselves.
//   - Observability (Stats, ScopeList) takes parameters where
//     applicable; returns the typed snapshot.
//   - Control-plane (Subscribe) is a pure passthrough — the lifecycle
//     primitive lives entirely in subscribe.go, Gateway just exports
//     it.
//
// Pre-1.0: signatures may shift. Post-1.0: semver-stable.
type Gateway struct {
	store *Store
}

// NewGateway constructs a Gateway around a fresh Store. Single
// entry-point external callers use; *Store is not exposed.
func NewGateway(c Config) *Gateway {
	return &Gateway{store: NewStore(c)}
}

// Stats is the public name for the typed /stats snapshot. Type alias
// (rather than separate type) so internal callers keep using the
// lowercase storeStats name without indirection.
type Stats = storeStats

// ScopeListEntry is the public name for one row of /scopelist.
type ScopeListEntry = scopeListEntry

// ReservedScopeEntry is the public name for one row of /stats's
// reserved_scopes array.
type ReservedScopeEntry = reservedScopeEntry

// --- Control-plane --------------------------------------------------

// Subscribe attaches a coalescing wake-up channel to the named
// reserved scope (`_events` or `_inbox`). See subscribe.go for the
// full contract: single-slot non-blocking sends, close-on-unsub,
// channel survives /wipe and /rebuild transparently.
//
// Pure passthrough — the lifecycle and lock-discipline live entirely
// in subscribe.go; Gateway just exports the same primitive under the
// public surface.
func (g *Gateway) Subscribe(scope string) (<-chan struct{}, func(), error) {
	return g.store.Subscribe(scope)
}

// --- Data-plane: writes ---------------------------------------------

// Append inserts a new item with cache-assigned seq and ts. Returns
// the committed item (with seq + ts populated). Validates item shape
// per validateWriteItem before delegating.
func (g *Gateway) Append(item Item) (Item, error) {
	if err := validateWriteItem(item, "/append", g.store.maxItemBytesFor(item.Scope)); err != nil {
		return Item{}, err
	}
	return g.store.appendOne(item)
}

// Upsert creates or replaces an item by (scope, id). Returns the
// committed item, a created bool (true on insert, false on replace),
// and any error. Validates per validateUpsertItem.
func (g *Gateway) Upsert(item Item) (Item, bool, error) {
	if err := validateUpsertItem(item, g.store.maxItemBytes); err != nil {
		return Item{}, false, err
	}
	return g.store.upsertOne(item)
}

// Update modifies the payload of an existing item addressed by
// scope+id or scope+seq. Returns updated_count (0 on miss, 1 on hit).
// Validates per validateUpdateItem.
func (g *Gateway) Update(item Item) (int, error) {
	if err := validateUpdateItem(item, g.store.maxItemBytes); err != nil {
		return 0, err
	}
	return g.store.updateOne(item)
}

// CounterAdd atomically increments (or creates) a numeric counter at
// (scope, id) by `by`. Returns the post-add value, a created bool,
// and any error. Validates the request shape (including the by != 0
// rule) via validateCounterAddRequest.
func (g *Gateway) CounterAdd(scope, id string, by int64) (int64, bool, error) {
	by, err := validateCounterAddRequest(CounterAddRequest{Scope: scope, ID: id, By: &by})
	if err != nil {
		return 0, false, err
	}
	return g.store.counterAddOne(scope, id, by)
}

// --- Data-plane: deletes --------------------------------------------

// Delete removes a single item addressed by scope+id (id != "") or
// scope+seq (id == ""). Returns deleted_count (0 on miss, 1 on hit).
// Validates the request shape via validateDeleteRequest.
func (g *Gateway) Delete(scope, id string, seq uint64) (int, error) {
	if err := validateDeleteRequest(DeleteRequest{Scope: scope, ID: id, Seq: seq}); err != nil {
		return 0, err
	}
	return g.store.deleteOne(scope, id, seq)
}

// DeleteUpTo removes every item in the scope with seq <= maxSeq.
// Returns deleted_count. Validates per validateDeleteUpToRequest.
func (g *Gateway) DeleteUpTo(scope string, maxSeq uint64) (int, error) {
	if err := validateDeleteUpToRequest(DeleteUpToRequest{Scope: scope, MaxSeq: maxSeq}); err != nil {
		return 0, err
	}
	return g.store.deleteUpTo(scope, maxSeq)
}

// DeleteScope removes the entire scope and every item in it. Returns
// (deleted_item_count, found). Validates the scope name shape +
// reserved-scope-rejection via validateDeleteScopeRequest.
func (g *Gateway) DeleteScope(scope string) (int, bool, error) {
	if err := validateDeleteScopeRequest(DeleteScopeRequest{Scope: scope}); err != nil {
		return 0, false, err
	}
	count, ok := g.store.deleteScope(scope)
	return count, ok, nil
}

// Wipe removes every user-managed scope from the store and resets the
// byte counter. Reserved scopes (`_events`, `_inbox`) are immediately
// re-created at boot baseline. Returns (scope_count, total_items,
// freed_bytes) — the pre-wipe sizes the operator can use to verify
// the call's scale.
func (g *Gateway) Wipe() (int, int, int64) {
	return g.store.wipe()
}

// --- Data-plane: bulk -----------------------------------------------

// Warm replaces the contents of every scope present in `grouped`.
// Scopes not in `grouped` are left untouched. Reserved scopes cannot
// be /warm targets — replaceScopes rejects that case at validation.
// Returns the number of scopes the call affected.
//
// Caller must group the items by scope (one slice per scope name);
// see groupItemsByScope in handlers.go for the typical HTTP-layer
// transformation from a flat ItemsRequest body.
func (g *Gateway) Warm(grouped map[string][]Item) (int, error) {
	return g.store.replaceScopes(grouped)
}

// Rebuild atomically replaces the entire user-managed cache state
// with `grouped`. Reserved scopes are wiped and re-created (so a
// drainer reading `_events` will see cursor-rewind on its next Tail).
// Returns (scope_count, item_count). Empty input is rejected by the
// HTTP layer (almost always a client bug); the Gateway accepts empty
// input and treats it as "wipe everything".
func (g *Gateway) Rebuild(grouped map[string][]Item) (int, int, error) {
	return g.store.rebuildAll(grouped)
}

// --- Data-plane: reads ----------------------------------------------

// Head returns up to `limit` oldest items in `scope` with seq >
// afterSeq. Returns (items, truncated, scope_found). truncated is
// true when more matching items exist past the limit window.
func (g *Gateway) Head(scope string, afterSeq uint64, limit int) ([]Item, bool, bool) {
	return g.store.head(scope, afterSeq, limit)
}

// Tail returns up to `limit` newest items in `scope`, skipping the
// first `offset` from the newest end. Returns (items, has_more,
// scope_found). has_more is true when older items exist before the
// returned window.
func (g *Gateway) Tail(scope string, limit, offset int) ([]Item, bool, bool) {
	return g.store.tail(scope, limit, offset)
}

// Get returns a single item by scope+id (id != "") or scope+seq
// (id == ""). Returns (item, hit). hit is false for either "scope
// missing" or "item missing within scope" — the Gateway does not
// distinguish those cases (the HTTP layer collapses both to count=0
// anyway).
func (g *Gateway) Get(scope, id string, seq uint64) (Item, bool) {
	return g.store.get(scope, id, seq)
}

// Render returns the rendered bytes (item.renderBytes if precomputed,
// otherwise item.Payload) for a single item, addressed by scope+id
// or scope+seq. Same hit semantics as Get.
func (g *Gateway) Render(scope, id string, seq uint64) ([]byte, bool) {
	return g.store.render(scope, id, seq)
}

// --- Observability --------------------------------------------------

// Stats returns the store-wide aggregate snapshot: scope_count,
// total_items, approx_store_mb, last_write_ts, plus reserved_scopes
// (per-scope state for `_events` and `_inbox`).
func (g *Gateway) Stats() Stats {
	return g.store.stats()
}

// ScopeList returns one row per scope with optional prefix filter and
// alphabetical cursor pagination. Returns (entries, truncated). See
// store.scopeList for the cost shape (O(N) shard walk + O(M log M)
// sort + O(limit) buf.stats() materialisations).
func (g *Gateway) ScopeList(prefix, after string, limit int) ([]ScopeListEntry, bool) {
	return g.store.scopeList(prefix, after, limit)
}
