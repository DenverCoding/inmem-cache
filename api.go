package scopecache

// APIConfig bundles the HTTP/transport-layer knobs that adapters supply to
// NewAPI. The split between APIConfig and Config mirrors the boundary
// rule: Config carries cache-internal limits (per-scope item cap, store
// byte cap, per-item byte cap), APIConfig carries everything that only
// makes sense once a request is being served — response sizing and the
// read-path heat-tracking opt-out.
//
// Multi-tenancy, auth, batching and operator-policy concerns previously
// in this struct (ServerSecret, InboxScopes, EnableAdmin, MaxMultiCall*,
// MaxInboxBytes) have moved out of core; they belong in the addons that
// will reintroduce /guarded, /admin, /inbox and /multi_call as separate
// sub-packages. See core-and-addons.md.
//
// Both the standalone binary and the Caddy module construct one of each
// and hand them to NewStore + NewAPI. New HTTP-layer knobs land here
// rather than in Config; new cache-layer knobs land in Config.
//
// Fields:
//   - MaxResponseBytes:  per-response cap on read endpoints whose body can
//     grow with limit × per-item-cap. In bytes. Default MaxResponseMiB << 20.
//   - DisableReadHeat:   opt-out of per-scope read-heat tracking on the hot read
//     path (/get, /render, /head, /tail). Saves time.Now() + atomic
//     adds at the cost of always-zero last_access_ts / last_7d_read_count /
//     read_count_total in /stats. Default false (heat tracked).
type APIConfig struct {
	MaxResponseBytes int64
	DisableReadHeat  bool
}

// WithDefaults returns a copy of c with non-positive numeric fields
// replaced by the package-level compile-time defaults. NewAPI calls this
// internally so callers can pass a partially-filled APIConfig (or a bare
// `APIConfig{}` for "all defaults") and still get a working API. Bool
// fields are left untouched: their zero-value is the documented shape
// (false DisableReadHeat keeps heat tracking on).
func (c APIConfig) WithDefaults() APIConfig {
	if c.MaxResponseBytes <= 0 {
		c.MaxResponseBytes = int64(MaxResponseMiB) << 20
	}
	return c
}

// API is the HTTP layer in front of *Store. It owns request-shape
// concerns the core deliberately knows nothing about: response-size
// caps and the read-heat opt-out. Multi-tenancy, batching and
// operator-policy concerns live in addon packages built on top of the
// public *API surface.
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
	maxResponseBytes int64

	// disableReadHeat skips recordRead() on every hot read-path call
	// (/get, /render, /head, /tail). Default zero-value
	// (false) keeps heat tracking on; setting true saves the time.Now()
	// call plus the atomic adds on the heat counters. See
	// APIConfig.DisableReadHeat.
	disableReadHeat bool
}

// NewAPI wires the HTTP API to a Store and an APIConfig. Request caps that
// scale with the store's configuration (maxBulkBytes, maxSingleBytes) are
// derived from the store; everything else comes from the APIConfig.
func NewAPI(store *Store, cfg APIConfig) *API {
	cfg = cfg.WithDefaults()
	return &API{
		store:            store,
		maxBulkBytes:     bulkRequestBytesFor(store.maxStoreBytes),
		maxSingleBytes:   singleRequestBytesFor(store.maxItemBytes),
		maxResponseBytes: cfg.MaxResponseBytes,
		disableReadHeat:  cfg.DisableReadHeat,
	}
}
