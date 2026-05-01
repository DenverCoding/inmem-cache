package scopecache

// APIConfig bundles the HTTP/transport-layer knobs that adapters supply to
// NewAPI. The split between APIConfig and Config mirrors the boundary
// rule: Config carries cache-internal limits (per-scope item cap, store
// byte cap, per-item byte cap), APIConfig carries everything that only
// makes sense once a request is being served — response sizing, batch
// composition (/multi_call), HMAC auth (/guarded), shared-write
// allowlists (/inbox), the operator-only dispatcher (/admin) and the
// read-path heat-tracking opt-out.
//
// Both the standalone binary and the Caddy module construct one of each
// and hand them to NewStore + NewAPI. New HTTP-layer knobs land here
// rather than in Config; new cache-layer knobs land in Config.
//
// Fields:
//   - MaxResponseBytes:  per-response cap on read endpoints whose body can
//     grow with limit × per-item-cap. In bytes. Default MaxResponseMiB << 20.
//   - MaxMultiCallBytes: input body cap for /multi_call. In bytes. Default MaxMultiCallMiB << 20.
//   - MaxMultiCallCount: max sub-calls per /multi_call batch. Default MaxMultiCallCount (10).
//   - MaxInboxBytes:     per-call payload cap on /inbox. In bytes. Default MaxInboxKiB << 10 (64 KiB).
//   - ServerSecret:      HMAC key for /guarded and /inbox capability_id derivation.
//     Empty string disables /guarded entirely (route not registered).
//   - InboxScopes:       allowlist of scope names /inbox is permitted to write.
//     Empty disables /inbox entirely. Together with ServerSecret, the operator opt-in
//     for shared write-only ingestion.
//   - EnableAdmin:       gates registration of /admin. Default false on the Caddy
//     module (a misconfigured public proxy is a real risk); standalone defaults to true
//     because the Unix socket is the gating layer.
//   - DisableReadHeat:   opt-out of per-scope read-heat tracking on the hot read
//     path (/get, /render, /head, /tail, /ts_range). Saves time.Now() + atomic
//     adds at the cost of always-zero last_access_ts / last_7d_read_count /
//     read_count_total in /stats. Default false (heat tracked).
type APIConfig struct {
	MaxResponseBytes  int64
	MaxMultiCallBytes int64
	MaxMultiCallCount int
	MaxInboxBytes     int64
	ServerSecret      string
	InboxScopes       []string
	EnableAdmin       bool
	DisableReadHeat   bool
}

// WithDefaults returns a copy of c with non-positive numeric fields
// replaced by the package-level compile-time defaults. NewAPI calls this
// internally so callers can pass a partially-filled APIConfig (or a bare
// `APIConfig{}` for "all defaults") and still get a working API. String,
// slice and bool fields are left untouched: their zero-values are the
// documented kill-switch shapes (empty ServerSecret disables /guarded,
// empty InboxScopes disables /inbox, false EnableAdmin keeps /admin off,
// false DisableReadHeat keeps heat tracking on).
func (c APIConfig) WithDefaults() APIConfig {
	if c.MaxResponseBytes <= 0 {
		c.MaxResponseBytes = int64(MaxResponseMiB) << 20
	}
	if c.MaxMultiCallBytes <= 0 {
		c.MaxMultiCallBytes = int64(MaxMultiCallMiB) << 20
	}
	if c.MaxMultiCallCount <= 0 {
		c.MaxMultiCallCount = MaxMultiCallCount
	}
	if c.MaxInboxBytes <= 0 {
		c.MaxInboxBytes = int64(MaxInboxKiB) << 10
	}
	return c
}

// API is the HTTP layer in front of *Store. It owns request-shape
// concerns the core deliberately knows nothing about: response-size
// caps, /multi_call batching, /guarded HMAC authorisation, /inbox
// allowlisting, and the /admin gate.
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

	// HTTP-layer caps copied from APIConfig at construction time. Bytes
	// (not MiB) — the cap-arithmetic in handlers stays in bytes; the
	// MiB-facing presentation happens at adapter boundaries.
	maxResponseBytes  int64
	maxMultiCallBytes int64
	maxMultiCallCount int
	maxInboxBytes     int64

	// HMAC key for /guarded and /inbox capability_id derivation. Empty
	// string means /guarded is not registered (and /inbox needs both
	// this AND a non-empty inboxScopes set).
	serverSecret string

	// inboxScopes is the precomputed lookup set built from
	// APIConfig.InboxScopes. Stored as a set rather than a slice so
	// isInboxScope is O(1) per /inbox call.
	inboxScopes map[string]bool

	// enableAdmin gates whether RegisterRoutes mounts /admin on the mux.
	// False → /admin returns 404. Default-deny on the Caddy module;
	// default-allow on the standalone binary.
	enableAdmin bool

	// disableReadHeat skips recordRead() on every hot read-path call
	// (/get, /render, /head, /tail, /ts_range). Default zero-value
	// (false) keeps heat tracking on; setting true saves the time.Now()
	// call plus the atomic adds on the heat counters. See
	// APIConfig.DisableReadHeat.
	disableReadHeat bool

	// multiCallSpecs is the closed whitelist of paths /multi_call
	// dispatches to, paired with their fixed HTTP method and raw
	// handler. Built once in NewAPI; the handler reference is the
	// un-wrapped API method (the dispatcher applies its own per-sub-call
	// cap via cappedResponseWriter, so wrapping again on the way in
	// would just buffer twice). See CLAUDE.md → Phase 4 design signals →
	// /multi_call.
	multiCallSpecs map[string]subCallSpec
	// adminCallSpecs is the wider whitelist used by /admin. Includes
	// operator-only operations (/warm, /rebuild, /wipe, /stats,
	// /delete_scope_candidates, /delete_scope) that are removed from the
	// public mux — only /admin reaches them. See guardedflow.md §K.
	adminCallSpecs map[string]subCallSpec
	// guardedCallSpecs is the narrower whitelist used by /guarded — 11
	// per-scope tenant-safe operations. See guardedflow.md §E.
	guardedCallSpecs map[string]subCallSpec
}

// NewAPI wires the HTTP API to a Store and an APIConfig. Request caps that
// scale with the store's configuration (maxBulkBytes, maxSingleBytes) are
// derived from the store; everything else comes from the APIConfig.
func NewAPI(store *Store, cfg APIConfig) *API {
	cfg = cfg.WithDefaults()
	inboxSet := make(map[string]bool, len(cfg.InboxScopes))
	for _, name := range cfg.InboxScopes {
		if name != "" {
			inboxSet[name] = true
		}
	}
	api := &API{
		store:             store,
		maxBulkBytes:      bulkRequestBytesFor(store.maxStoreBytes),
		maxSingleBytes:    singleRequestBytesFor(store.maxItemBytes),
		maxResponseBytes:  cfg.MaxResponseBytes,
		maxMultiCallBytes: cfg.MaxMultiCallBytes,
		maxMultiCallCount: cfg.MaxMultiCallCount,
		maxInboxBytes:     cfg.MaxInboxBytes,
		serverSecret:      cfg.ServerSecret,
		inboxScopes:       inboxSet,
		enableAdmin:       cfg.EnableAdmin,
		disableReadHeat:   cfg.DisableReadHeat,
	}
	api.multiCallSpecs = api.buildMultiCallSpecs()
	api.adminCallSpecs = api.buildAdminCallSpecs()
	api.guardedCallSpecs = api.buildGuardedCallSpecs()
	return api
}

// isInboxScope reports whether name is in the operator-configured
// allowlist of /inbox target scopes. Used by handleInbox to reject writes
// to scope names the operator has not opted into.
func (api *API) isInboxScope(name string) bool {
	return api.inboxScopes[name]
}
