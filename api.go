// api.go owns the HTTP layer (*API + APIConfig) in front of *store.
// Request-shape concerns the core deliberately knows nothing about
// (per-request body cap, per-response byte cap) live here; cache
// semantics live in store.go and the buffer family.

package scopecache

// APIConfig bundles the HTTP/transport-layer knobs adapters supply
// to NewAPI. The split mirrors the boundary rule: Config carries
// cache-internal limits, APIConfig carries everything that only
// makes sense once a request is being served.
//
// Currently empty — the response-byte cap is derived from the
// store's MaxStoreBytes (no separate knob), and multi-tenancy / auth
// / batching belong in addon packages, not core. Kept (rather than
// dropped) so the first new HTTP-layer knob lands without breaking
// every adapter's `NewAPI(gw, scopecache.APIConfig{})` call site.
type APIConfig struct{}

// API is the HTTP layer in front of *store. Owns the request-shape
// concerns the core deliberately knows nothing about (request- and
// response-byte caps).
type API struct {
	store *store
	// maxBulkBytes is the per-request body cap for /warm and /rebuild,
	// derived from store.maxStoreBytes via bulkRequestBytesFor so a
	// fully-loaded store can always be expressed as a single bulk
	// request.
	maxBulkBytes int64
	// maxSingleBytes is the per-request body cap for single-item
	// endpoints (/append, /update, /upsert, /delete, /delete_scope,
	// /delete_up_to, /counter_add). Derived from the store's largest
	// per-item cap (maxItemBytesAnyScope) via singleRequestBytesFor so
	// the HTTP guardrail sits just above the semantic item-size limit
	// enforced in the validator. Using the largest cap (not just the
	// user-scope one) keeps reserved-scope writes wire-symmetric with
	// the Go API: an _inbox configured with Inbox.MaxItemBytes >
	// MaxItemBytes must not be HTTP-rejected at decodeBody for a
	// payload its semantic validator would have accepted.
	maxSingleBytes int64

	// maxResponseBytes is the per-response byte cap for /head, /tail —
	// endpoints whose response can grow with limit × per-item-cap.
	// Derived from store.maxStoreBytes (not operator-configurable):
	// any single scope is bounded by the store budget, so a response
	// cap equal to the store cap guarantees every full-scope read
	// fits in one response — including drainer reads of `_events` which
	// must never be artificially capped (drainer lag → silent event
	// drop is the failure mode, not a 507 on tail).
	maxResponseBytes int64
}

// NewAPI wires the HTTP API to a Gateway. Adapters call
// NewGateway(cfg) → NewAPI(gw, ...) → RegisterRoutes(mux) to mount
// the HTTP surface. Every byte cap is derived from the store's
// config so HTTP guardrails track the cache budget without the
// operator keeping two sets of knobs in sync.
//
// Takes *Gateway (the public type) rather than *store so adapters,
// addons and tests don't need to know the private store type. The
// HTTP layer reaches into gw.store for validation-cap reads and
// dispatches handlers through gw.store.* — validation runs at the
// store entry, not in the handlers.
func NewAPI(gw *Gateway, _ APIConfig) *API {
	return &API{
		store:            gw.store,
		maxBulkBytes:     bulkRequestBytesFor(gw.store.maxStoreBytes),
		maxSingleBytes:   singleRequestBytesFor(gw.store.maxItemBytesAnyScope()),
		maxResponseBytes: gw.store.maxStoreBytes,
	}
}
