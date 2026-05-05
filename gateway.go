package scopecache

// Gateway is the public Go-API for the cache. ALL in-process callers
// — adapters (Caddy module, standalone), addons (subscribers,
// publishers, content-hashers, …) — talk to scopecache via *Gateway.
// It is the wall around the core: the underlying *Store and its
// lowercase methods are NOT part of the public contract.
//
// Methods on Gateway are near-passthroughs to *store: the only work
// they do beyond delegating is defensive payload-byte cloning at the
// boundary. Validation lives at the top of each *store method; the
// Gateway adds no shape checks. The actual mutation logic lives in
// store.go, buffer_*.go, events.go, and subscribe.go — gateway.go is
// the registration of which operations are part of the public
// contract, plus the clone discipline that keeps caller-owned and
// cache-owned []byte slices independent. See gateway_clone.go for
// the hazard description and helpers.
//
// Why one Gateway instead of separate *API (HTTP) and *Direct (Go):
// external addon authors learn ONE surface, program against ONE file.
// The HTTP adapter (*API) wraps Gateway internally; standalone and
// the Caddy module both wire NewGateway → NewAPI → RegisterRoutes.
// Future addons import scopecache, hold a *Gateway, and build their
// behaviour on top of the methods declared here.
//
// Pre-1.0: signatures may shift. Post-1.0: semver-stable.
type Gateway struct {
	store *store
}

// NewGateway constructs a Gateway around a fresh Store. Single
// entry-point external callers use; *Store is not exposed.
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
// reserved scope (`_events` or `_inbox`). See subscribe.go for the
// full contract.
func (g *Gateway) Subscribe(scope string) (<-chan struct{}, func(), error) {
	return g.store.Subscribe(scope)
}

// --- Data-plane: writes ---------------------------------------------

// Append inserts a new item with cache-assigned seq and ts. Returns
// the committed item; ErrInvalidInput on validation failure. The
// caller's item.Payload slice is cloned on the way in and the
// returned item.Payload is cloned on the way out, so neither side
// can mutate the other's bytes after the call returns. See
// gateway_clone.go for the rationale.
func (g *Gateway) Append(item Item) (Item, error) {
	item.Payload = clonePayload(item.Payload)
	result, err := g.store.appendOne(item)
	return cloneItemPayload(result), err
}

// Upsert creates or replaces an item by (scope, id). Returns
// (item, created, err); ErrInvalidInput on validation failure.
// Payload is cloned on entry and exit; see Append.
func (g *Gateway) Upsert(item Item) (Item, bool, error) {
	item.Payload = clonePayload(item.Payload)
	result, created, err := g.store.upsertOne(item)
	return cloneItemPayload(result), created, err
}

// Update modifies the payload of an existing item addressed by
// scope+id or scope+seq. Returns (updated_count, err);
// ErrInvalidInput on validation failure. The caller's item.Payload
// slice is cloned on entry so a post-call mutation cannot reach
// stored bytes; the return is just a count, no exit clone needed.
func (g *Gateway) Update(item Item) (int, error) {
	item.Payload = clonePayload(item.Payload)
	return g.store.updateOne(item)
}

// CounterAdd atomically increments (or creates) a counter at
// (scope, id) by `by`. Returns (post-add value, created, err);
// ErrInvalidInput on validation failure.
func (g *Gateway) CounterAdd(scope, id string, by int64) (int64, bool, error) {
	return g.store.counterAddOne(scope, id, by)
}

// --- Data-plane: deletes --------------------------------------------

// Delete removes a single item addressed by scope+id (id != "") or
// scope+seq (id == ""). Returns (deleted_count, err); ErrInvalidInput
// on validation failure.
func (g *Gateway) Delete(scope, id string, seq uint64) (int, error) {
	return g.store.deleteOne(scope, id, seq)
}

// DeleteUpTo removes every item in the scope with seq <= maxSeq.
// Returns (deleted_count, err); ErrInvalidInput on validation failure.
func (g *Gateway) DeleteUpTo(scope string, maxSeq uint64) (int, error) {
	return g.store.deleteUpTo(scope, maxSeq)
}

// DeleteScope removes the entire scope and every item in it. Returns
// (deleted_item_count, found, err); ErrInvalidInput on validation
// failure (empty or reserved scope).
func (g *Gateway) DeleteScope(scope string) (int, bool, error) {
	return g.store.deleteScope(scope)
}

// Wipe removes every user-managed scope from the store and resets
// the byte counter. Reserved scopes (`_events`, `_inbox`) are
// immediately re-created at boot baseline. Returns (scope_count,
// total_items, freed_bytes).
func (g *Gateway) Wipe() (int, int, int64) {
	return g.store.wipe()
}

// --- Data-plane: bulk -----------------------------------------------

// Warm replaces the contents of every scope present in `grouped`.
// Scopes not in `grouped` are left untouched. Reserved scopes cannot
// be /warm targets — replaceScopes rejects that case internally.
// Returns the number of scopes the call affected. Every payload in
// every input slice is cloned on entry; see Append for the rationale.
func (g *Gateway) Warm(grouped map[string][]Item) (int, error) {
	return g.store.replaceScopes(cloneGroupedItemPayloads(grouped))
}

// Rebuild atomically replaces the entire user-managed cache state
// with `grouped`. Reserved scopes are wiped and re-created. Returns
// (scope_count, item_count, err). Payloads are cloned on entry; see
// Append for the rationale.
func (g *Gateway) Rebuild(grouped map[string][]Item) (int, int, error) {
	return g.store.rebuildAll(cloneGroupedItemPayloads(grouped))
}

// --- Data-plane: reads ----------------------------------------------

// Head returns up to `limit` oldest items in `scope` with seq >
// afterSeq. Returns (items, truncated, scope_found). Each returned
// item.Payload is a fresh allocation; callers may mutate them
// without touching cached bytes. See gateway_clone.go.
func (g *Gateway) Head(scope string, afterSeq uint64, limit int) ([]Item, bool, bool) {
	items, truncated, found := g.store.head(scope, afterSeq, limit)
	return cloneItemsPayloads(items), truncated, found
}

// Tail returns up to `limit` newest items in `scope`, skipping the
// first `offset` from the newest end. Returns (items, has_more,
// scope_found). Each returned item.Payload is freshly allocated.
func (g *Gateway) Tail(scope string, limit, offset int) ([]Item, bool, bool) {
	items, hasMore, found := g.store.tail(scope, limit, offset)
	return cloneItemsPayloads(items), hasMore, found
}

// Get returns a single item by scope+id (id != "") or scope+seq
// (id == ""). Returns (item, hit). The returned item.Payload is a
// fresh allocation.
func (g *Gateway) Get(scope, id string, seq uint64) (Item, bool) {
	item, hit := g.store.get(scope, id, seq)
	return cloneItemPayload(item), hit
}

// Render returns the rendered bytes for a single item, addressed by
// scope+id or scope+seq. Returns (rendered, hit). The returned slice
// is a fresh allocation; callers may mutate it without touching
// cached bytes.
func (g *Gateway) Render(scope, id string, seq uint64) ([]byte, bool) {
	rendered, hit := g.store.render(scope, id, seq)
	return clonePayload(rendered), hit
}

// --- Observability --------------------------------------------------

// Stats returns the store-wide aggregate snapshot.
func (g *Gateway) Stats() Stats {
	return g.store.stats()
}

// ScopeList returns one row per scope with optional prefix filter and
// alphabetical cursor pagination.
func (g *Gateway) ScopeList(prefix, after string, limit int) ([]ScopeListEntry, bool) {
	return g.store.scopeList(prefix, after, limit)
}
