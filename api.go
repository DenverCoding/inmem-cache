package scopecache

// APIConfig bundles the HTTP/transport-layer knobs that adapters supply
// to NewAPI. The split between APIConfig and Config mirrors the
// boundary rule: Config carries cache-internal limits (per-scope item
// cap, store byte cap, per-item byte cap), APIConfig carries everything
// that only makes sense once a request is being served.
//
// Currently empty: the only HTTP-layer knob (response-byte cap) is now
// derived from the store's MaxStoreBytes inside NewAPI rather than
// configured separately — by construction no single scope can hold
// more than the store, so the response cap that's "guaranteed to fit
// every full-scope tail in one response" is simply equal to the store
// cap. The struct is kept (rather than dropped) so the first new
// HTTP-layer knob (CORS, request tracing, …) lands without breaking
// every adapter's `NewAPI(store, scopecache.APIConfig{})` call site.
//
// Multi-tenancy, auth, batching and operator-policy concerns previously
// in this struct have moved out of core; they belong in addons that
// will reintroduce /guarded, /admin, /inbox and /multi_call as
// separate sub-packages. See core-and-addons.md.
type APIConfig struct{}

// API is the HTTP layer in front of *Store. It owns request-shape
// concerns the core deliberately knows nothing about: response-size
// caps. Multi-tenancy, batching and operator-policy concerns live in
// addon packages built on top of the public *API surface.
type API struct {
	store *Store
	// maxBulkBytes is the per-request body cap for /warm and /rebuild,
	// derived from store.maxStoreBytes via bulkRequestBytesFor so a
	// fully-loaded store can always be expressed as a single bulk
	// request.
	maxBulkBytes int64
	// maxSingleBytes is the per-request body cap for single-item
	// endpoints (/append, /update, /upsert, /delete, /delete_scope,
	// /delete_up_to, /counter_add). Derived from store.maxItemBytes via
	// singleRequestBytesFor so the HTTP guardrail sits just above the
	// semantic item-size limit enforced in the validator.
	maxSingleBytes int64

	// maxResponseBytes is the per-response byte cap for /head, /tail —
	// endpoints whose response can grow with limit × per-item-cap.
	// Derived from store.maxStoreBytes (not operator-configurable):
	// any single scope is bounded by the store budget, so a response
	// cap equal to the store cap guarantees every full-scope read
	// fits in one response — including drainer reads of `_log` which
	// must never be artificially capped (drainer lag → silent event
	// drop is the failure mode, not a 507 on tail).
	maxResponseBytes int64
}

// NewAPI wires the HTTP API to a Store and an APIConfig. Every byte
// cap on this layer is derived from the store's configuration so the
// HTTP guardrails always track the underlying cache budget without
// the operator having to keep two sets of knobs in sync.
func NewAPI(store *Store, _ APIConfig) *API {
	return &API{
		store:            store,
		maxBulkBytes:     bulkRequestBytesFor(store.maxStoreBytes),
		maxSingleBytes:   singleRequestBytesFor(store.maxItemBytes),
		maxResponseBytes: store.maxStoreBytes,
	}
}
