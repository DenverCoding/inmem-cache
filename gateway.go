package scopecache

// Gateway is the public Go-API for the cache. ALL in-process callers
// — adapters (Caddy module, standalone), addons — talk to scopecache
// via *Gateway. The underlying *store and its lowercase methods are
// NOT part of the public contract.
//
// Methods on Gateway are near-passthroughs to *store with ONE
// invariant beyond delegation: defensive payload-byte cloning at
// every boundary, plus blocking counter-pointer leaks in either
// direction. See gateway_clone.go for the hazard description and
// helpers; the per-method comments below do not repeat it.
//
// Validation lives at the top of each *store method; Gateway adds no
// shape checks. Mutation logic lives in store.go, buffer_*.go,
// events.go, subscribe.go.
//
// Pre-1.0: signatures may shift. Post-1.0: semver-stable.
type Gateway struct {
	store *store
}

// NewGateway constructs a Gateway around a fresh Store.
func NewGateway(c Config) *Gateway {
	return &Gateway{store: newStore(c)}
}

// Stats is the public name for the typed /stats snapshot.
type Stats = storeStats

// ScopeListEntry is the public name for one row of /scopelist.
type ScopeListEntry = scopeListEntry

// ReservedScopeEntry is the public name for one row of /stats's
// reserved_scopes array.
type ReservedScopeEntry = reservedScopeEntry

// --- Control-plane --------------------------------------------------

// Subscribe attaches a coalescing wake-up channel to the named
// reserved scope (`_events` or `_inbox`).
// See subscribe.go for the full contract.
func (g *Gateway) Subscribe(scope string) (<-chan struct{}, func(), error) {
	return g.store.Subscribe(scope)
}

// --- Data-plane: writes ---------------------------------------------
//
// Every write returns ErrInvalidInput when shape validation fails.
// Items are cloned on entry; returned items are cloned on exit.

// Append inserts a new item with cache-assigned seq and ts.
// Returns the committed item.
func (g *Gateway) Append(item Item) (Item, error) {
	item = cloneItemPayload(item)
	result, err := g.store.appendOne(item)
	return cloneItemPayload(result), err
}

// Upsert creates or replaces an item by (scope, id).
// Returns (item, created, err).
func (g *Gateway) Upsert(item Item) (Item, bool, error) {
	item = cloneItemPayload(item)
	result, created, err := g.store.upsertOne(item)
	return cloneItemPayload(result), created, err
}

// Update modifies the payload of an item addressed by scope+id or
// scope+seq.
// Returns (updated_count, err).
func (g *Gateway) Update(item Item) (int, error) {
	item = cloneItemPayload(item)
	return g.store.updateOne(item)
}

// CounterAdd atomically increments (or creates) a counter at
// (scope, id) by `by`.
// Returns (post-add value, created, err).
func (g *Gateway) CounterAdd(scope, id string, by int64) (int64, bool, error) {
	return g.store.counterAddOne(scope, id, by)
}

// --- Data-plane: deletes --------------------------------------------

// Delete removes a single item addressed by scope+id (id != "") or
// scope+seq (id == "").
// Returns (deleted_count, err).
func (g *Gateway) Delete(scope, id string, seq uint64) (int, error) {
	return g.store.deleteOne(scope, id, seq)
}

// DeleteUpTo removes every item in the scope with seq <= maxSeq.
// Returns (deleted_count, err).
func (g *Gateway) DeleteUpTo(scope string, maxSeq uint64) (int, error) {
	return g.store.deleteUpTo(scope, maxSeq)
}

// DeleteScope removes the entire scope and every item in it.
// Returns (deleted_item_count, found, err).
// Reserved scopes are rejected with ErrInvalidInput.
func (g *Gateway) DeleteScope(scope string) (int, bool, error) {
	return g.store.deleteScope(scope)
}

// Wipe removes every user-managed scope and resets the byte counter.
// Returns (scope_count, total_items, freed_bytes).
// Reserved scopes (`_events`, `_inbox`) are immediately re-created.
func (g *Gateway) Wipe() (int, int, int64) {
	return g.store.wipe()
}

// --- Data-plane: bulk -----------------------------------------------
//
// Payloads are cloned on entry. Returns ErrInvalidInput on shape
// validation failure.

// Warm replaces the contents of every scope present in `grouped`.
// Returns the number of scopes affected.
// Scopes not in `grouped` are left untouched; reserved scopes are
// rejected.
func (g *Gateway) Warm(grouped map[string][]Item) (int, error) {
	return g.store.replaceScopes(cloneGroupedItemPayloads(grouped))
}

// Rebuild atomically replaces the entire user-managed cache state
// with `grouped`.
// Returns (scope_count, item_count, err).
// Reserved scopes are wiped and re-created.
func (g *Gateway) Rebuild(grouped map[string][]Item) (int, int, error) {
	return g.store.rebuildAll(cloneGroupedItemPayloads(grouped))
}

// --- Data-plane: reads ----------------------------------------------
//
// Reads are PERMISSIVE: invalid scope/id shapes (over-length, control
// chars, whitespace) silently miss with hit=false instead of erroring.
// Strict validation lives at the HTTP layer (parseLookupTarget);
// addons proxying HTTP inherit that.
//
// Returned payloads are fresh allocations — callers may mutate freely.
//
// ByID/BySeq are split (rather than one Get with id-or-seq args) so
// caller intent is explicit at the call site, with no precedence rule
// to remember.

// Head returns up to `limit` oldest items in `scope` with seq > afterSeq.
// Returns (items, truncated, scope_found).
func (g *Gateway) Head(scope string, afterSeq uint64, limit int) ([]Item, bool, bool) {
	items, truncated, found := g.store.head(scope, afterSeq, limit)
	return cloneItemsPayloads(items), truncated, found
}

// Tail returns up to `limit` newest items in `scope`, skipping the
// first `offset` from the newest end.
// Returns (items, has_more, scope_found).
func (g *Gateway) Tail(scope string, limit, offset int) ([]Item, bool, bool) {
	items, hasMore, found := g.store.tail(scope, limit, offset)
	return cloneItemsPayloads(items), hasMore, found
}

// GetByID returns the item at (scope, id).
// Returns (item, hit).
func (g *Gateway) GetByID(scope, id string) (Item, bool) {
	item, hit := g.store.get(scope, id, 0)
	return cloneItemPayload(item), hit
}

// GetBySeq returns the item at (scope, seq).
// Returns (item, hit).
func (g *Gateway) GetBySeq(scope string, seq uint64) (Item, bool) {
	item, hit := g.store.get(scope, "", seq)
	return cloneItemPayload(item), hit
}

// RenderByID returns the rendered bytes for the item at (scope, id).
// Returns (rendered, hit).
func (g *Gateway) RenderByID(scope, id string) ([]byte, bool) {
	rendered, hit := g.store.render(scope, id, 0)
	return clonePayload(rendered), hit
}

// RenderBySeq returns the rendered bytes for the item at (scope, seq).
// Returns (rendered, hit).
func (g *Gateway) RenderBySeq(scope string, seq uint64) ([]byte, bool) {
	rendered, hit := g.store.render(scope, "", seq)
	return clonePayload(rendered), hit
}

// --- Observability --------------------------------------------------

// Stats returns the store-wide aggregate snapshot.
func (g *Gateway) Stats() Stats {
	return g.store.stats()
}

// ScopeList returns one row per scope, with optional prefix filter
// and alphabetical cursor pagination.
func (g *Gateway) ScopeList(prefix, after string, limit int) ([]ScopeListEntry, bool) {
	return g.store.scopeList(prefix, after, limit)
}
