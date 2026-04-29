package scopecache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"time"
)

// adminCtxKey marks a synthetic sub-request as originating from /admin's
// dispatcher. Public-mux handlers check this via isAdminContext to
// decide whether to enforce the reserved-prefix block on incoming
// scopes — admin can write to `_guarded:*` and `_counters_*`, public
// callers cannot. See guardedflow.md §B, §K.
type adminCtxKey struct{}

func withAdminContext(r *http.Request) *http.Request {
	return r.WithContext(contextWithAdmin(r.Context()))
}

func contextWithAdmin(ctx context.Context) context.Context {
	return context.WithValue(ctx, adminCtxKey{}, true)
}

func isAdminContext(ctx context.Context) bool {
	v, _ := ctx.Value(adminCtxKey{}).(bool)
	return v
}

// rejectReservedScope rejects requests whose scope begins with the
// reserved '_' prefix UNLESS the request was dispatched via /admin.
// Helper called at the top of every scope-bearing public handler.
// Returns true when the handler should write a 400 and stop.
func rejectReservedScope(r *http.Request, w http.ResponseWriter, started time.Time, scope string) bool {
	if isAdminContext(r.Context()) {
		return false
	}
	if hasReservedPrefix(scope) {
		badRequest(w, started, "the 'scope' field must not begin with '_' (reserved prefix)")
		return true
	}
	return false
}

// writeStoreCapacityError centralises the per-handler error-handling
// for the three capacity-class errors the store can return on a write
// path: *ScopeFullError (single-item over per-scope cap), the bulk
// equivalent *ScopeCapacityError (carries an offender list), and
// *StoreFullError (over the store-wide byte cap). All seven write
// handlers (/append, /upsert, /counter_add, /inbox single-item +
// /warm, /rebuild bulk + /update which only sees stfe) call this
// before doing any handler-specific error dispatch.
//
// Returns true when one of the three was matched and the response
// has been written — the caller should `return` immediately. Returns
// false otherwise; the caller falls back to its own error handling
// (typically `conflict(...)`, plus counter-specific errors for
// /counter_add).
//
// `scopeForSFE` is the scope name plumbed into the single-element
// offenders list on the *ScopeFullError path. It is **unused** for
// callers that cannot produce sfe — /warm and /rebuild produce
// *ScopeCapacityError (which carries its own offender list) and
// /update produces only *StoreFullError. Those callers pass "".
// The unused-param wart is preferable to splitting into two helpers
// that would duplicate the stfe block (the most likely candidate
// for future drift).
func writeStoreCapacityError(w http.ResponseWriter, started time.Time, err error, scopeForSFE string) bool {
	var sfe *ScopeFullError
	if errors.As(err, &sfe) {
		scopeFull(w, started, []ScopeCapacityOffender{
			{Scope: scopeForSFE, Count: sfe.Count, Cap: sfe.Cap},
		})
		return true
	}
	var sce *ScopeCapacityError
	if errors.As(err, &sce) {
		scopeFull(w, started, sce.Offenders)
		return true
	}
	var stfe *StoreFullError
	if errors.As(err, &stfe) {
		storeFull(w, started, stfe)
		return true
	}
	return false
}

type API struct {
	store *Store
	// maxBulkBytes is the per-request body cap for /warm and /rebuild. It is
	// derived from the store's configured byte cap via bulkRequestBytesFor so
	// a fully-loaded store can always be expressed as a single bulk request.
	maxBulkBytes int64
	// maxSingleBytes is the per-request body cap for single-item endpoints
	// (/append, /update, /upsert, /delete, /delete_scope, /delete_up_to,
	// /counter_add). Derived from the store's per-item cap via
	// singleRequestBytesFor so the HTTP guardrail always sits just above the
	// semantic item-size limit enforced in the validator.
	maxSingleBytes int64
	// multiCallSpecs is the closed whitelist of paths /multi_call dispatches
	// to, paired with their fixed HTTP method and raw handler. Built once in
	// NewAPI; the handler reference is the un-wrapped API method (the
	// dispatcher applies its own per-sub-call cap via cappedResponseWriter,
	// so wrapping again on the way in would just buffer twice). See
	// CLAUDE.md → Phase 4 design signals → /multi_call.
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

// NewAPI wires the HTTP API to a Store and derives request caps that scale
// with the store's configuration.
func NewAPI(store *Store) *API {
	api := &API{
		store:          store,
		maxBulkBytes:   bulkRequestBytesFor(store.maxStoreBytes),
		maxSingleBytes: singleRequestBytesFor(store.maxItemBytes),
	}
	api.multiCallSpecs = api.buildMultiCallSpecs()
	api.adminCallSpecs = api.buildAdminCallSpecs()
	api.guardedCallSpecs = api.buildGuardedCallSpecs()
	return api
}

// decodeBody caps the request body at max bytes and decodes JSON into out.
// The MaxBytesReader guard runs at read time, so it protects against clients
// that omit Content-Length or stream chunked bodies just as much as sized ones.
// An exceeded-cap error is distinguished from a plain JSON syntax error so
// callers can return a meaningful message. A second Decode is used to reject
// trailing content (a second object or garbage after the first value), which
// json.Decoder would otherwise silently ignore.
func decodeBody(w http.ResponseWriter, r *http.Request, max int64, out interface{}) error {
	r.Body = http.MaxBytesReader(w, r.Body, max)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(out); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return errors.New("the request body exceeds the maximum allowed size of " +
				strconv.FormatInt(mbe.Limit, 10) + " bytes")
		}
		return errors.New("the request body must contain valid JSON")
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("the request body must contain exactly one JSON value")
	}
	return nil
}

// orderedFields is a JSON object whose keys are emitted in insertion order.
// encoding/json sorts map keys alphabetically, which scatters ok, errors,
// counts, and payloads through the output in whichever order the alphabet
// dictates. orderedFields lets every response put ok first, config/caps
// before aggregates, heavy or variable-size fields last, and duration_us
// at the very end — a shape a human eye (and a log scanner) can read at
// a glance.
type orderedFields []kv

type kv struct {
	K string
	V interface{}
}

func (o orderedFields) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, f := range o {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, err := json.Marshal(f.K)
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteByte(':')
		val, err := json.Marshal(f.V)
		if err != nil {
			return nil, err
		}
		buf.Write(val)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func writeJSONWithDuration(w http.ResponseWriter, code int, payload orderedFields, started time.Time) {
	payload = append(payload, kv{"duration_us", time.Since(started).Microseconds()})
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(payload)
}

// writeJSONWithMeta is writeJSONWithDuration plus an approx_response_mb field
// that reports the body's own byte length back to the client. Used on every
// read-item endpoint (/head, /tail, /ts_range, /get) so the read family
// produces a uniform response shape — duration_us, count, approx_response_mb
// regardless of item count. On the limit-scaled endpoints (/head, /tail,
// /ts_range) it also lets callers see how close they sit to the per-response
// cap and narrow their query before they hit a 507.
//
// Single-marshal + patch: marshal the body once, then splice in the size
// field just before the closing '}'. Self-referential — the size includes
// the field's own bytes — but converges in 1-2 iterations because MB has
// 4-decimal precision (0.0001 MiB = ~104 bytes), and the patch only adds
// ~30 bytes total. Cost over writeJSONWithDuration is a single
// json.Marshal of the MB value plus a few slice appends: well under 100 µs
// even for multi-MiB responses.
// marshalWithApproxSize is the shared splice helper used by
// writeJSONWithMeta and writeJSONWithMetaCap. It marshals payload,
// then appends a self-referential approx_response_mb field that
// reports the body's own byte length back to the client. Returns
// the spliced bytes plus the duration_us-augmented payload (the
// caller may need it for a fallback path on marshal failure).
func marshalWithApproxSize(payload orderedFields, started time.Time) ([]byte, orderedFields, error) {
	payload = append(payload, kv{"duration_us", time.Since(started).Microseconds()})
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, payload, err
	}

	// bodyBytes ends in '}'. Strip it, append `,"approx_response_mb":N.NNNN}`.
	// Iterate so the reported size includes the bytes we are about to add.
	const fieldKey = `,"approx_response_mb":`
	finalSize := len(bodyBytes) + len(fieldKey) + 8 // initial guess: 8-byte value
	var valueBytes []byte
	for i := 0; i < 3; i++ {
		v, mErr := json.Marshal(MB(finalSize))
		if mErr != nil {
			break
		}
		valueBytes = v
		candidate := len(bodyBytes) - 1 + len(fieldKey) + len(valueBytes) + 1
		if candidate == finalSize {
			break
		}
		finalSize = candidate
	}

	out := make([]byte, 0, finalSize+1)
	out = append(out, bodyBytes[:len(bodyBytes)-1]...)
	out = append(out, fieldKey...)
	out = append(out, valueBytes...)
	out = append(out, '}', '\n')

	return out, payload, nil
}

// writeJSONWithMeta is writeJSONWithDuration plus an approx_response_mb
// field. Used on read-item endpoints whose response size is bounded by
// the operation (e.g. /get is single-item). For limit-scaled endpoints
// (/head, /tail, /ts_range) use writeJSONWithMetaCap instead, which
// rejects oversized bodies up-front.
func writeJSONWithMeta(w http.ResponseWriter, code int, payload orderedFields, started time.Time) {
	out, augmented, err := marshalWithApproxSize(payload, started)
	if err != nil {
		// orderedFields encoding cannot fail in practice (we control every
		// value type); fall through to the simpler writer if it ever does.
		writeJSONWithDuration(w, code, augmented[:len(augmented)-1], started)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write(out)
}

// writeJSONWithMetaCap is writeJSONWithMeta with a per-response byte
// cap baked in. Used on /head, /tail, /ts_range — endpoints whose
// response can grow with limit × per-item-cap. Marshals the body
// once, checks against maxBytes, and either emits the response or
// replaces it with a 507 envelope. Replaces the older capResponse
// middleware that buffered the handler's whole output a second time.
func writeJSONWithMetaCap(w http.ResponseWriter, code int, payload orderedFields, started time.Time, maxBytes int64) {
	out, augmented, err := marshalWithApproxSize(payload, started)
	if err != nil {
		writeJSONWithDuration(w, code, augmented[:len(augmented)-1], started)
		return
	}

	if int64(len(out)) > maxBytes {
		responseTooLarge(w, started, int64(len(out)), maxBytes)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write(out)
}

func badRequest(w http.ResponseWriter, started time.Time, message string) {
	writeJSONWithDuration(w, http.StatusBadRequest, orderedFields{
		{"ok", false},
		{"error", message},
	}, started)
}

func conflict(w http.ResponseWriter, started time.Time, message string) {
	writeJSONWithDuration(w, http.StatusConflict, orderedFields{
		{"ok", false},
		{"error", message},
	}, started)
}

// scopeFull responds with 507 Insufficient Storage and the full offender list.
// Used when an /append, /warm, or /rebuild would push one or more scopes past
// the per-scope capacity. The client is expected to drain (e.g. /delete_up_to
// or /delete_scope) or chunk the batch and retry.
func scopeFull(w http.ResponseWriter, started time.Time, offenders []ScopeCapacityOffender) {
	msg := "scope is at capacity"
	if len(offenders) > 1 {
		msg = "multiple scopes are at capacity"
	}
	writeJSONWithDuration(w, http.StatusInsufficientStorage, orderedFields{
		{"ok", false},
		{"error", msg},
		{"scopes", offenders},
	}, started)
}

// storeFull responds with 507 when the aggregate byte cap would be exceeded.
// The body carries the store-level totals (all in MiB, matching /stats) so a
// client can judge how much headroom remains and whether draining one scope
// will fix the next retry.
func storeFull(w http.ResponseWriter, started time.Time, e *StoreFullError) {
	writeJSONWithDuration(w, http.StatusInsufficientStorage, orderedFields{
		{"ok", false},
		{"error", "store is at byte capacity"},
		{"approx_store_mb", MB(e.StoreBytes)},
		{"added_mb", MB(e.AddedBytes)},
		{"max_store_mb", MB(e.Cap)},
	}, started)
}

func methodNotAllowed(w http.ResponseWriter, started time.Time) {
	writeJSONWithDuration(w, http.StatusMethodNotAllowed, orderedFields{
		{"ok", false},
		{"error", "the HTTP method is not allowed for this endpoint"},
	}, started)
}

// lookupTarget is the parsed form of /get's and /render's URL query:
// a scope plus exactly one of id or seq. Built by parseLookupTarget.
type lookupTarget struct {
	Scope string
	ByID  bool
	ID    string
	Seq   uint64
}

// parseLookupTarget pulls scope + exactly one of id/seq from the query
// string and validates each. Scope errors are labelled with the endpoint;
// the id/seq shape errors are endpoint-agnostic since the rule is the same
// on every single-item read.
func parseLookupTarget(r *http.Request, endpoint string) (lookupTarget, error) {
	query := r.URL.Query()
	scope := query.Get("scope")
	id := query.Get("id")
	seqStr := query.Get("seq")

	if err := validateScope(scope, endpoint); err != nil {
		return lookupTarget{}, err
	}
	if !isAdminContext(r.Context()) && hasReservedPrefix(scope) {
		return lookupTarget{}, errors.New("the 'scope' field must not begin with '_' (reserved prefix)")
	}

	hasID := id != ""
	hasSeq := seqStr != ""
	if hasID == hasSeq {
		return lookupTarget{}, errors.New("exactly one of 'id' or 'seq' must be provided")
	}

	if hasID {
		if err := validateID(id); err != nil {
			return lookupTarget{}, err
		}
		return lookupTarget{Scope: scope, ByID: true, ID: id}, nil
	}

	seq, err := strconv.ParseUint(seqStr, 10, 64)
	if err != nil {
		return lookupTarget{}, errors.New("the 'seq' parameter must be a valid unsigned integer")
	}
	return lookupTarget{Scope: scope, Seq: seq}, nil
}

// scopeLimit is the parsed form of the scope+limit query pair used by every
// multi-item read (/head, /tail, /ts_range). Endpoint-specific params
// (offset, after_seq, since_ts, …) are parsed by the handler itself — this
// helper deliberately stops at the common pair.
type scopeLimit struct {
	Scope string
	Limit int
}

// parseScopeLimit validates scope and normalizes limit in the order every
// caller expects (scope first, then limit), so the returned error matches
// the handlers' historical behaviour.
func parseScopeLimit(r *http.Request, endpoint string) (scopeLimit, error) {
	query := r.URL.Query()
	scope := query.Get("scope")
	if err := validateScope(scope, endpoint); err != nil {
		return scopeLimit{}, err
	}
	if !isAdminContext(r.Context()) && hasReservedPrefix(scope) {
		return scopeLimit{}, errors.New("the 'scope' field must not begin with '_' (reserved prefix)")
	}
	limit, err := normalizeLimit(query.Get("limit"))
	if err != nil {
		return scopeLimit{}, err
	}
	return scopeLimit{Scope: scope, Limit: limit}, nil
}

func (api *API) handleAppend(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodPost {
		methodNotAllowed(w, started)
		return
	}

	var item Item
	if err := decodeBody(w, r, api.maxSingleBytes, &item); err != nil {
		badRequest(w, started, err.Error())
		return
	}

	if err := validateWriteItem(item, "/append", api.store.maxItemBytes); err != nil {
		badRequest(w, started, err.Error())
		return
	}
	if rejectReservedScope(r, w, started, item.Scope) {
		return
	}

	origScope := item.Scope
	item, err := api.store.AppendOne(item)
	if err != nil {
		if writeStoreCapacityError(w, started, err, origScope) {
			return
		}
		conflict(w, started, err.Error())
		return
	}

	writeJSONWithDuration(w, http.StatusOK, orderedFields{
		{"ok", true},
		{"item", item},
	}, started)
}

func (api *API) handleWarm(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodPost {
		methodNotAllowed(w, started)
		return
	}

	var req ItemsRequest
	if err := decodeBody(w, r, api.maxBulkBytes, &req); err != nil {
		badRequest(w, started, err.Error())
		return
	}

	for i := range req.Items {
		if err := validateWriteItem(req.Items[i], "/warm", api.store.maxItemBytes); err != nil {
			badRequest(w, started, "invalid item at index "+strconv.Itoa(i)+": "+err.Error())
			return
		}
	}

	grouped := groupItemsByScope(req.Items)
	replacedScopes, err := api.store.replaceScopes(grouped)
	if err != nil {
		// /warm cannot produce *ScopeFullError (only single-item paths do);
		// scopeForSFE is unused here.
		if writeStoreCapacityError(w, started, err, "") {
			return
		}
		conflict(w, started, err.Error())
		return
	}

	writeJSONWithDuration(w, http.StatusOK, orderedFields{
		{"ok", true},
		{"count", len(req.Items)},
		{"replaced_scopes", replacedScopes},
	}, started)
}

func (api *API) handleRebuild(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodPost {
		methodNotAllowed(w, started)
		return
	}

	var req ItemsRequest
	if err := decodeBody(w, r, api.maxBulkBytes, &req); err != nil {
		badRequest(w, started, err.Error())
		return
	}

	// An empty items[] would wipe the entire store. That is almost always a
	// client bug (missing payload, wrong key, serialization glitch) rather
	// than an intentional "clear everything" call. Refuse it explicitly;
	// clients that really want to clear the cache should /delete_scope per
	// scope or restart the service.
	if len(req.Items) == 0 {
		badRequest(w, started, "the 'items' array must not be empty for the '/rebuild' endpoint")
		return
	}

	for i := range req.Items {
		if err := validateWriteItem(req.Items[i], "/rebuild", api.store.maxItemBytes); err != nil {
			badRequest(w, started, "invalid item at index "+strconv.Itoa(i)+": "+err.Error())
			return
		}
	}

	grouped := groupItemsByScope(req.Items)
	rebuiltScopes, rebuiltItems, err := api.store.rebuildAll(grouped)
	if err != nil {
		// /rebuild cannot produce *ScopeFullError (only single-item paths
		// do); scopeForSFE is unused here.
		if writeStoreCapacityError(w, started, err, "") {
			return
		}
		conflict(w, started, err.Error())
		return
	}

	writeJSONWithDuration(w, http.StatusOK, orderedFields{
		{"ok", true},
		{"count", len(req.Items)},
		{"rebuilt_scopes", rebuiltScopes},
		{"rebuilt_items", rebuiltItems},
	}, started)
}

// handleUpsert creates a new item or replaces an existing one by scope + id.
// Unlike /append (which rejects duplicate ids) or /update (which soft-misses
// on absent items), /upsert always writes — making it the idempotent, retry-
// safe write path. Seq is preserved on replace and freshly assigned on create.
func (api *API) handleUpsert(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodPost {
		methodNotAllowed(w, started)
		return
	}

	var item Item
	if err := decodeBody(w, r, api.maxSingleBytes, &item); err != nil {
		badRequest(w, started, err.Error())
		return
	}

	if err := validateUpsertItem(item, api.store.maxItemBytes); err != nil {
		badRequest(w, started, err.Error())
		return
	}
	if rejectReservedScope(r, w, started, item.Scope) {
		return
	}

	origScope := item.Scope
	result, created, err := api.store.UpsertOne(item)
	if err != nil {
		if writeStoreCapacityError(w, started, err, origScope) {
			return
		}
		conflict(w, started, err.Error())
		return
	}

	writeJSONWithDuration(w, http.StatusOK, orderedFields{
		{"ok", true},
		{"created", created},
		{"item", result},
	}, started)
}

// handleCounterAdd atomically increments (or creates) a numeric counter at
// scope+id by `by`. It is the only endpoint that reads or mutates a payload
// as a typed value — every other write path treats payloads as opaque bytes.
// Creates pay a fresh approxItemSize reservation; replaces pay only the byte
// delta of the new integer representation.
func (api *API) handleCounterAdd(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodPost {
		methodNotAllowed(w, started)
		return
	}

	var req CounterAddRequest
	if err := decodeBody(w, r, api.maxSingleBytes, &req); err != nil {
		badRequest(w, started, err.Error())
		return
	}

	by, err := validateCounterAddRequest(req)
	if err != nil {
		badRequest(w, started, err.Error())
		return
	}
	if rejectReservedScope(r, w, started, req.Scope) {
		return
	}

	origScope := req.Scope
	value, created, err := api.store.CounterAddOne(req.Scope, req.ID, by)
	if err != nil {
		// Common capacity-class errors first (sfe + stfe). Counter-
		// specific errors (cpe → 409, coe → 400) are handled inline
		// below — they do not fit the helper because cpe maps to
		// `conflict` and coe maps to `badRequest`, not to the
		// scope/store-full responders.
		if writeStoreCapacityError(w, started, err, origScope) {
			return
		}
		var cpe *CounterPayloadError
		if errors.As(err, &cpe) {
			conflict(w, started, cpe.Error())
			return
		}
		var coe *CounterOverflowError
		if errors.As(err, &coe) {
			badRequest(w, started, coe.Error())
			return
		}
		conflict(w, started, err.Error())
		return
	}

	writeJSONWithDuration(w, http.StatusOK, orderedFields{
		{"ok", true},
		{"created", created},
		{"value", value},
	}, started)
}

func (api *API) handleUpdate(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodPost {
		methodNotAllowed(w, started)
		return
	}

	var item Item
	if err := decodeBody(w, r, api.maxSingleBytes, &item); err != nil {
		badRequest(w, started, err.Error())
		return
	}

	if err := validateUpdateItem(item, api.store.maxItemBytes); err != nil {
		badRequest(w, started, err.Error())
		return
	}
	if rejectReservedScope(r, w, started, item.Scope) {
		return
	}

	buf, ok := api.store.getScope(item.Scope)
	if !ok {
		writeJSONWithDuration(w, http.StatusOK, orderedFields{
			{"ok", true},
			{"hit", false},
			{"updated_count", 0},
		}, started)
		return
	}

	var updated int
	var err error
	if item.ID != "" {
		updated, err = buf.updateByID(item.ID, item.Payload, item.Ts)
	} else {
		updated, err = buf.updateBySeq(item.Seq, item.Payload, item.Ts)
	}
	if err != nil {
		// /update only ever sees *StoreFullError on the cap path
		// (existing-item replace can grow byte size); scopeForSFE is
		// unused.
		if writeStoreCapacityError(w, started, err, "") {
			return
		}
		conflict(w, started, err.Error())
		return
	}

	writeJSONWithDuration(w, http.StatusOK, orderedFields{
		{"ok", true},
		{"hit", updated > 0},
		{"updated_count", updated},
	}, started)
}

func (api *API) handleDelete(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodPost {
		methodNotAllowed(w, started)
		return
	}

	var req DeleteRequest
	if err := decodeBody(w, r, api.maxSingleBytes, &req); err != nil {
		badRequest(w, started, err.Error())
		return
	}

	if err := validateDeleteRequest(req); err != nil {
		badRequest(w, started, err.Error())
		return
	}
	if rejectReservedScope(r, w, started, req.Scope) {
		return
	}

	buf, ok := api.store.getScope(req.Scope)
	if !ok {
		writeJSONWithDuration(w, http.StatusOK, orderedFields{
			{"ok", true},
			{"hit", false},
			{"deleted_count", 0},
		}, started)
		return
	}

	var deleted int
	var err error
	if req.ID != "" {
		deleted, err = buf.deleteByID(req.ID)
	} else {
		deleted, err = buf.deleteBySeq(req.Seq)
	}
	if err != nil {
		// *ScopeDetachedError: the scope was wiped/deleted/rebuilt
		// between getScope and the mutation. Surface as 409 — same
		// stance as /append, /upsert, /update, /counter_add. A retry
		// will see the new state (possibly miss, possibly a fresh
		// scope with no such id).
		conflict(w, started, err.Error())
		return
	}

	writeJSONWithDuration(w, http.StatusOK, orderedFields{
		{"ok", true},
		{"hit", deleted > 0},
		{"deleted_count", deleted},
	}, started)
}

func (api *API) handleDeleteUpTo(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodPost {
		methodNotAllowed(w, started)
		return
	}

	var req DeleteUpToRequest
	if err := decodeBody(w, r, api.maxSingleBytes, &req); err != nil {
		badRequest(w, started, err.Error())
		return
	}

	if err := validateDeleteUpToRequest(req); err != nil {
		badRequest(w, started, err.Error())
		return
	}
	if rejectReservedScope(r, w, started, req.Scope) {
		return
	}

	buf, ok := api.store.getScope(req.Scope)
	if !ok {
		writeJSONWithDuration(w, http.StatusOK, orderedFields{
			{"ok", true},
			{"hit", false},
			{"deleted_count", 0},
		}, started)
		return
	}

	deleted, err := buf.deleteUpToSeq(req.MaxSeq)
	if err != nil {
		// Same orphan-detect rationale as handleDelete above.
		conflict(w, started, err.Error())
		return
	}

	writeJSONWithDuration(w, http.StatusOK, orderedFields{
		{"ok", true},
		{"hit", deleted > 0},
		{"deleted_count", deleted},
	}, started)
}

func (api *API) handleDeleteScope(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodPost {
		methodNotAllowed(w, started)
		return
	}

	var req DeleteScopeRequest
	if err := decodeBody(w, r, api.maxSingleBytes, &req); err != nil {
		badRequest(w, started, err.Error())
		return
	}

	if err := validateDeleteScopeRequest(req); err != nil {
		badRequest(w, started, err.Error())
		return
	}
	if rejectReservedScope(r, w, started, req.Scope) {
		return
	}

	deletedItems, deleted := api.store.deleteScope(req.Scope)

	writeJSONWithDuration(w, http.StatusOK, orderedFields{
		{"ok", true},
		{"hit", deleted},
		{"deleted_scope", deleted},
		{"deleted_items", deletedItems},
	}, started)
}

// handleWipe clears the entire store: every scope, every item, every byte
// reservation. It is the store-wide complement of /delete_scope — destructive
// in one call, with no request body. The response carries the counts and
// freed bytes so the client can verify what was released.
//
// This is *not* an eviction policy: the cache never wipes on its own.
// /wipe exists because a client-side "for each scope: /delete_scope" is
// N calls and not atomic, whereas a server-side wipe is one lock and one
// map replacement.
func (api *API) handleWipe(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodPost {
		methodNotAllowed(w, started)
		return
	}

	// /wipe takes no request body. We still cap what Go's auto-drain might
	// read so a misbehaving client cannot pin server memory by pushing a
	// large body to a body-less endpoint.
	r.Body = http.MaxBytesReader(w, r.Body, 1024)

	deletedScopes, deletedItems, freedBytes := api.store.wipe()

	writeJSONWithDuration(w, http.StatusOK, orderedFields{
		{"ok", true},
		{"deleted_scopes", deletedScopes},
		{"deleted_items", deletedItems},
		{"freed_mb", MB(freedBytes)},
	}, started)
}

// writeItemsHit assembles and writes the success response for a
// list-returning read endpoint (/head, /tail, /ts_range). Two
// load-bearing invariants live in this helper that previously sat
// parallel in three handler bodies:
//
//  1. recordRead is called only when items is non-empty. An empty
//     result window (e.g. /tail of an existing scope where no items
//     match the offset, or /ts_range with no items in the time
//     window) is effectively a miss and must NOT inflate the scope's
//     last_7d_read_count — that signal drives operator decisions on
//     /delete_scope_candidates and would silently corrupt if the
//     `len > 0` guard was forgotten in any of the three handlers.
//  2. estimateMultiItemResponseBytes runs BEFORE writeJSONWithMeta.
//     Once the response body has been written there is no way to
//     switch to a 507 without leaving a half-flushed body on the
//     wire — the cap check is a one-shot opportunity per request.
//
// `extra` slots between `count` and `truncated` so /tail can carry
// its `offset` field at the right wire position; /head and
// /ts_range pass nil. Field order is load-bearing: matches the
// existing wire shape exactly. Do not reorder.
func (api *API) writeItemsHit(
	w http.ResponseWriter,
	started time.Time,
	buf *ScopeBuffer,
	items []Item,
	truncated bool,
	extra orderedFields,
) {
	if len(items) > 0 {
		if !api.store.disableReadHeat {
			buf.recordRead(nowUnixMicro())
		}
		if estimated := estimateMultiItemResponseBytes(items); estimated > api.store.maxResponseBytes {
			responseTooLarge(w, started, estimated, api.store.maxResponseBytes)
			return
		}
	}

	fields := make(orderedFields, 0, 5+len(extra))
	fields = append(fields,
		kv{"ok", true},
		kv{"hit", len(items) > 0},
		kv{"count", len(items)},
	)
	fields = append(fields, extra...)
	fields = append(fields,
		kv{"truncated", truncated},
		kv{"items", items},
	)
	// Single-marshal cap check via writeJSONWithMetaCap. Replaces an
	// outer capResponse middleware that buffered the whole handler
	// output a second time — for multi-MiB responses the saving is the
	// full body size in heap. The pre-flight estimateMultiItemResponseBytes
	// above still runs first to short-circuit pathological queries
	// (limit=10000 against 1 MiB items) before the marshal.
	writeJSONWithMetaCap(w, http.StatusOK, fields, started, api.store.maxResponseBytes)
}

// writeItemsMiss writes the canonical "scope does not exist" response
// for a list-returning read endpoint. Same field order as
// writeItemsHit's success path; truncated is always false; items is
// the sentinel empty slice (NOT nil — `[]Item{}` marshals as `[]`,
// nil would marshal as `null` and break clients that iterate). Goes
// through the same cap-aware writer as the hit path so an absurdly
// small per-response cap rejects misses too — preserves the symmetry
// the cap-test suite relies on.
func (api *API) writeItemsMiss(
	w http.ResponseWriter,
	started time.Time,
	extra orderedFields,
) {
	fields := make(orderedFields, 0, 5+len(extra))
	fields = append(fields,
		kv{"ok", true},
		kv{"hit", false},
		kv{"count", 0},
	)
	fields = append(fields, extra...)
	fields = append(fields,
		kv{"truncated", false},
		kv{"items", []Item{}},
	)
	writeJSONWithMetaCap(w, http.StatusOK, fields, started, api.store.maxResponseBytes)
}

func (api *API) handleHead(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodGet {
		methodNotAllowed(w, started)
		return
	}

	q, err := parseScopeLimit(r, "/head")
	if err != nil {
		badRequest(w, started, err.Error())
		return
	}
	query := r.URL.Query()
	// /head reads forward by cursor only. Positional 'offset' addressing
	// lives on /tail exclusively because seq-based forward reads are stable
	// under /delete_up_to while position-based forward reads are not.
	if query.Has("offset") {
		badRequest(w, started, "the 'offset' parameter is not supported on /head; use 'after_seq' instead, or call /tail for position-based paging")
		return
	}

	// after_seq is optional: omitting it (or passing 0) returns the oldest
	// items from the scope, which covers the "give me the start of this
	// scope" case without requiring the client to know any seq values.
	var afterSeq uint64
	if raw := query.Get("after_seq"); raw != "" {
		afterSeq, err = strconv.ParseUint(raw, 10, 64)
		if err != nil {
			badRequest(w, started, "the 'after_seq' parameter must be a valid unsigned integer")
			return
		}
	}

	buf, ok := api.store.getScope(q.Scope)
	if !ok {
		api.writeItemsMiss(w, started, nil)
		return
	}

	items, truncated := buf.sinceSeq(afterSeq, q.Limit)
	api.writeItemsHit(w, started, buf, items, truncated, nil)
}

func (api *API) handleTail(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodGet {
		methodNotAllowed(w, started)
		return
	}

	q, err := parseScopeLimit(r, "/tail")
	if err != nil {
		badRequest(w, started, err.Error())
		return
	}
	offset, err := normalizeOffset(r.URL.Query().Get("offset"))
	if err != nil {
		badRequest(w, started, err.Error())
		return
	}

	// /tail's wire shape carries `offset` between `count` and `truncated`
	// — the helpers slot `extra` at exactly that position.
	offsetField := orderedFields{kv{"offset", offset}}

	buf, ok := api.store.getScope(q.Scope)
	if !ok {
		api.writeItemsMiss(w, started, offsetField)
		return
	}

	items, truncated := buf.tailOffset(q.Limit, offset)
	api.writeItemsHit(w, started, buf, items, truncated, offsetField)
}

// handleTsRange answers time-window queries: return every item in a scope
// whose client-supplied Ts falls inside [since_ts, until_ts]. At least one
// bound must be provided; both bounds are inclusive (SQL BETWEEN convention);
// items without a Ts are always excluded. No pagination cursor — if the
// response is capped by ?limit, the truncated flag tells the client to narrow
// the window and retry rather than chase a seq cursor (seq has no meaningful
// relationship to a ts-filtered view, especially because ts is user-mutable).
func (api *API) handleTsRange(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodGet {
		methodNotAllowed(w, started)
		return
	}

	q, err := parseScopeLimit(r, "/ts_range")
	if err != nil {
		badRequest(w, started, err.Error())
		return
	}
	query := r.URL.Query()

	sinceTs, err := parseTsParam("since_ts", query.Get("since_ts"))
	if err != nil {
		badRequest(w, started, err.Error())
		return
	}
	untilTs, err := parseTsParam("until_ts", query.Get("until_ts"))
	if err != nil {
		badRequest(w, started, err.Error())
		return
	}
	if err := validateTsRangeParams(sinceTs, untilTs); err != nil {
		badRequest(w, started, err.Error())
		return
	}

	buf, ok := api.store.getScope(q.Scope)
	if !ok {
		api.writeItemsMiss(w, started, nil)
		return
	}

	items, truncated := buf.tsRange(sinceTs, untilTs, q.Limit)
	api.writeItemsHit(w, started, buf, items, truncated, nil)
}

func (api *API) handleGet(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodGet {
		methodNotAllowed(w, started)
		return
	}

	target, err := parseLookupTarget(r, "/get")
	if err != nil {
		badRequest(w, started, err.Error())
		return
	}

	miss := func() {
		writeJSONWithMeta(w, http.StatusOK, orderedFields{
			{"ok", true},
			{"hit", false},
			{"count", 0},
			{"item", nil},
		}, started)
	}

	buf, ok := api.store.getScope(target.Scope)
	if !ok {
		miss()
		return
	}

	var item Item
	var found bool
	if target.ByID {
		item, found = buf.getByID(target.ID)
	} else {
		item, found = buf.getBySeq(target.Seq)
	}
	if !found {
		miss()
		return
	}

	// Only a hit counts toward scope read-heat. A miss by explicit id/seq
	// should not inflate last_7d_read_count, since the signal is surfaced
	// on /delete_scope_candidates for client-side eviction decisions.
	if !api.store.disableReadHeat {
		buf.recordRead(nowUnixMicro())
	}

	writeJSONWithMeta(w, http.StatusOK, orderedFields{
		{"ok", true},
		{"hit", true},
		{"count", 1},
		{"item", item},
	}, started)
}

// handleRender serves a single item as raw payload bytes with no JSON
// envelope. The use case is serving cached HTML/XML/JSON/text fragments
// directly from the cache (typically fronted by Caddy, nginx, or apache).
//
// Design rules — deliberately minimal:
//   - Hit and miss paths are envelope-free: 200 carries raw payload bytes,
//     404 carries an empty body. Both use Content-Type application/octet-stream
//     — a neutral default the fronting proxy is expected to override via its
//     own route config (e.g. `header Content-Type text/html`). The cache does
//     NOT sniff content or guess the real MIME type.
//   - Validation errors (missing scope, malformed seq, etc.) still use the
//     standard JSON error envelope. Those are developer-facing, not content-facing.
//   - If the stored payload is a JSON string (first non-whitespace byte is `"`),
//     one layer of JSON string-encoding is peeled so `"<html>..."` is served
//     as `<html>...` on the wire. All other JSON values (object, array, number,
//     bool) are written raw; the consumer is expected to parse them as JSON.
func (api *API) handleRender(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodGet {
		methodNotAllowed(w, started)
		return
	}

	target, err := parseLookupTarget(r, "/render")
	if err != nil {
		badRequest(w, started, err.Error())
		return
	}

	writeMiss := func() {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusNotFound)
	}

	buf, ok := api.store.getScope(target.Scope)
	if !ok {
		writeMiss()
		return
	}

	var item Item
	var found bool
	if target.ByID {
		item, found = buf.getByID(target.ID)
	} else {
		item, found = buf.getBySeq(target.Seq)
	}
	if !found {
		writeMiss()
		return
	}

	// A hit counts toward scope read-heat, same as /get.
	if !api.store.disableReadHeat {
		buf.recordRead(nowUnixMicro())
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)

	// renderBytes is non-nil iff the payload is a JSON string, populated
	// at write-time. Skip the per-hit json.Unmarshal + []byte cast.
	if item.renderBytes != nil {
		_, _ = w.Write(item.renderBytes)
		return
	}
	_, _ = w.Write(item.Payload)
}

// Per-scope stats are read under each buffer's own lock, not store-wide:
// the response is per-scope consistent but not a global atomic snapshot,
// which is acceptable because this endpoint is advisory.
func (api *API) handleDeleteScopeCandidates(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodGet {
		methodNotAllowed(w, started)
		return
	}

	limit, err := normalizeLimit(r.URL.Query().Get("limit"))
	if err != nil {
		badRequest(w, started, err.Error())
		return
	}

	hours, err := normalizeHours(r.URL.Query().Get("hours"))
	if err != nil {
		badRequest(w, started, err.Error())
		return
	}

	scopes := api.store.listScopes()
	list := make([]Candidate, 0, len(scopes))
	now := nowUnixMicro()
	minAgeMicros := hours * int64(time.Hour/time.Microsecond)

	for name, buf := range scopes {
		st := buf.stats(now)

		if hours > 0 && now-st.CreatedTS < minAgeMicros {
			continue
		}

		list = append(list, Candidate{
			Scope:           name,
			CreatedTS:       st.CreatedTS,
			LastAccessTS:    st.LastAccessTS,
			Last7dReadCount: st.Last7DReadCount,
			ItemCount:       st.ItemCount,
			ApproxScopeMB:   st.ApproxScopeMB,
		})
	}

	sort.Slice(list, func(i, j int) bool {
		return list[i].LastAccessTS < list[j].LastAccessTS
	})

	if len(list) > limit {
		list = list[:limit]
	}

	writeJSONWithDuration(w, http.StatusOK, orderedFields{
		{"ok", true},
		{"count", len(list)},
		{"hours", hours},
		{"candidates", list},
	}, started)
}

func (api *API) handleStats(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodGet {
		methodNotAllowed(w, started)
		return
	}

	st := api.store.stats()

	// Build the scopes map with each entry as orderedFields so per-scope
	// fields emit in a logical order (counts first, timestamps, then reads).
	// The outer map keys are scope names and will sort alphabetically, which
	// is appropriate for an arbitrary identifier set.
	scopes := make(map[string]orderedFields, len(st.Scopes))
	for name, sc := range st.Scopes {
		scopes[name] = orderedFields{
			{"item_count", sc.ItemCount},
			{"last_seq", sc.LastSeq},
			{"approx_scope_mb", sc.ApproxScopeMB},
			{"created_ts", sc.CreatedTS},
			{"last_access_ts", sc.LastAccessTS},
			{"read_count_total", sc.ReadCountTotal},
			{"last_7d_read_count", sc.Last7DReadCount},
		}
	}

	// /stats is a state endpoint: scope/item counts and current byte usage.
	// Static config (DefaultLimit, MaxLimit, per-item/per-scope caps) lives
	// in /help, not here. max_store_mb is the one cap that *does* appear —
	// it pairs with approx_store_mb so a client can compute headroom in a
	// single call. duration_us is appended by the helper.
	writeJSONWithDuration(w, http.StatusOK, orderedFields{
		{"ok", true},
		{"scope_count", st.ScopeCount},
		{"total_items", st.TotalItems},
		{"approx_store_mb", st.ApproxStoreMB},
		{"max_store_mb", st.MaxStoreMB},
		{"scopes", scopes},
	}, started)
}

func (api *API) handleHelp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = w.Write([]byte("method not allowed\n"))
		return
	}

	helpText := `scopecache v2

RULES:
- payload must be a valid JSON value (object, array, string, number, bool); its contents are opaque to the cache — never inspected or searched
- payload must be present on writes; literal null is treated as missing
- per-item size cap is 1 MiB by default (override with SCOPECACHE_MAX_ITEM_MB, integer MiB); measured against the raw JSON bytes of payload plus scope/id overhead
- scope and id must be <= 128 bytes, with no surrounding whitespace and no control characters
- filtering only operates on scope, id, seq and the optional top-level ts
- items carry an optional 'ts' field (signed int64, milliseconds since unix epoch by convention — the cache is opaque to the unit). ts is user-supplied on writes (absent → no ts); on /update, omitting ts preserves the existing value; on /upsert, omitting ts clears it (replace semantics).
- /ts_range filters by the ts window; items without a ts are always excluded from ts-filtered reads
- read endpoints use a default limit of 1,000 when ?limit is omitted, and a maximum of 10,000 (higher values are clamped, not rejected)
- /head, /tail and /ts_range responses carry a "truncated" boolean: true when more matching items exist beyond the returned window
- /head, /tail, /ts_range and /get responses carry "count" and "approx_response_mb" fields alongside duration_us so the read family produces a uniform response shape; on /head, /tail and /ts_range approx_response_mb also lets clients see how close they sit to the per-response cap (see below) without having to measure the body themselves
- id is optional
- if id is present, it must be unique within its scope
- write operations reject duplicates for the same scope + id
- per-scope capacity is 100,000 items by default (override with SCOPECACHE_SCOPE_MAX_ITEMS); writes that would exceed the cap are rejected with 507 Insufficient Storage — nothing is silently evicted
- /append past the cap returns 507 with the offending scope. /warm and /rebuild reject the entire batch with the full list of over-cap scopes; make room first with /delete_up_to or /delete_scope
- store-wide byte cap is 100 MiB by default (override with SCOPECACHE_MAX_STORE_MB, integer MiB); writes that would push the aggregate approxItemSize past it are rejected with 507. The response carries approx_store_mb, added_mb, and max_store_mb; free room with /delete_scope or /delete_up_to
- per-response byte cap is 25 MiB by default (override with SCOPECACHE_MAX_RESPONSE_MB, integer MiB); applied to /head, /tail, /ts_range and /multi_call whose response size scales with limit × per-item-cap (or with batch fanout). A response that would exceed the cap is rejected with 507 carrying approx_response_mb and max_response_mb; narrow with a smaller ?limit (or fewer sub-calls). Already-applied side effects are not rolled back — same as every other 507 in this cache.
- per-request body cap for /warm and /rebuild scales with the store cap (~store + 10% + 16 MiB), so a full cache always fits in one bulk request. Single-item endpoints use a body cap derived from the per-item cap (item + 4 KiB). /multi_call has its own input body cap of 16 MiB by default (override with SCOPECACHE_MAX_MULTI_CALL_MB, integer MiB).
- /multi_call accepts at most 10 sub-calls per batch by default (override with SCOPECACHE_MAX_MULTI_CALL_COUNT, positive int)
- every byte-ish field in JSON responses (approx_store_mb, max_store_mb, approx_scope_mb, added_mb) is expressed in MiB with 4 decimals — one unit across /stats, /delete_scope_candidates and 507 responses
- the listening socket path defaults to /run/scopecache.sock on Linux and $TMPDIR/scopecache.sock on macOS/Windows; override with SCOPECACHE_SOCKET_PATH
- per-scope read-heat tracking (powers /delete_scope_candidates and the last_access_ts / last_7d_read_count / read_count_total fields on /stats) is on by default. Operators that don't use /delete_scope_candidates can turn it off (SCOPECACHE_DISABLE_READ_HEAT=1 on standalone, 'disable_read_heat yes' on the Caddy module) for a smaller hot-read overhead — measurable on minimal Caddy stacks (~5-7% on /get) and Go-direct usage (~40% in benchmarks); HTTP-fronted deployments with a thicker stack (FrankenPHP, multiple matchers, etc.) see the cache-side gain absorbed by the request-handling floor

ENDPOINTS (public mux):
GET  /help - show this help text
POST /append - append one item to a scope
POST /update - update one item by scope + id or scope + seq (exactly one of id/seq required)
POST /upsert - create or replace one item by scope + id; response carries "created": true for a fresh item, false for a replace
POST /counter_add - atomically add 'by' (signed int64, non-zero, within ±(2^53-1)) to the integer counter at scope + id; creates a fresh counter with starting value 'by' on miss; 409 if the existing item is not a counter-valued integer; response carries {ok, created, value}
POST /delete - delete one item by scope + id or scope + seq (exactly one of id/seq required)
POST /delete_up_to - delete every item in a scope with seq <= max_seq
GET  /head - get the oldest items from a scope; supports optional after_seq for cursor-based forward reads (offset is not supported, use /tail for position-based paging)
GET  /tail - get the most recent items from a scope (supports optional offset)
GET  /ts_range - get items whose optional top-level ts falls inside [since_ts, until_ts] (both inclusive, either may be omitted but at least one is required); returns seq-order, items without ts are skipped, no pagination cursor — narrow the window and retry if truncated=true
GET  /get - get one item by scope + id or scope + seq
GET  /render - serve one item's payload as raw bytes (no JSON envelope); miss returns 404; JSON-string payloads are decoded one layer so cached HTML/XML/text is served as-is; Content-Type is application/octet-stream — fronting proxy is expected to set the real type if browser-facing
POST /multi_call - sequentially dispatch N independent sub-calls in one HTTP roundtrip; body is {"calls": [{"path": "/get|/append|...", "query": {...}, "body": {...}}, ...]}; allowed paths: /append, /get, /head, /tail, /ts_range, /update, /upsert, /counter_add, /delete, /delete_up_to; response is {ok, count, results: [{status, body}, ...], approx_response_mb, duration_us} in input order. No cross-call atomicity — a write at index 0 stays applied even if index 1 fails. Outer envelope honours the per-response cap (SCOPECACHE_MAX_RESPONSE_MB); slot bodies that would push the envelope past the cap are replaced with a minimal {"ok":true|false,"response_truncated":true} marker while the slot's status is preserved.

ADMIN-ONLY (gated outside the cache by socket permissions or Caddyfile route):
POST /admin - operator-elevated dispatcher; same {"calls":[...]} body and {"results":[...]} envelope as /multi_call. Reaches reserved scopes (_*) directly; no rewrite. Wider whitelist than /multi_call: /append, /get, /head, /tail, /ts_range, /update, /upsert, /counter_add, /delete, /delete_up_to, /delete_scope, /warm, /rebuild, /wipe, /stats, /delete_scope_candidates. Excluded: /help (text/plain), /render (raw bytes don't fit a JSON results array), /multi_call/guarded/admin (self-reference loops). /warm, /rebuild, /wipe, /delete_scope, /stats, /delete_scope_candidates are reachable ONLY through /admin — they are not on the public mux. /stats and /delete_scope_candidates are admin-only because they enumerate every scope name in the store, which leaks reserved scopes (_tokens, _guarded:*, _counters_*) and per-scope heat metadata in multi-tenant deployments. Registered only when EnableAdmin is set: standalone defaults true (Unix-socket permission gating); Caddy module defaults false (operator must opt in via 'enable_admin yes' AND add a Caddyfile route guard, since /admin has no body-level auth).

OPTIONAL ENDPOINTS (registered only when configured):
POST /guarded - tenant-facing multi-call gateway; body {"token":"<opaque>","calls":[...]} derives capability_id = HMAC_SHA256(SCOPECACHE_SERVER_SECRET, token), gates on _tokens membership, and rewrites every sub-call's scope to _guarded:<capability_id>:<original-scope>. Whitelist excludes /delete_scope, /stats, /delete_scope_candidates, /wipe, /warm, /rebuild, and /render. Registered only when SCOPECACHE_SERVER_SECRET is set; otherwise the route returns 404.
POST /inbox - shared write-only ingestion; single /append per request (no envelope). Cache assigns id (capability_id:<16-hex random>) and ts (now in millis). Tenants cannot read what they wrote — reads happen via /admin. Registered only when SCOPECACHE_SERVER_SECRET is set AND at least one inbox scope name is configured (SCOPECACHE_INBOX_SCOPES on the standalone binary; repeated 'inbox_scope <name>' directives in the Caddy module).

NOTES:
- /warm replaces only the scopes present in the request
- /rebuild replaces the entire store
- /delete_up_to is designed for write-buffer patterns: read with /head?after_seq=…, commit to the DB, then trim with /delete_up_to up to the last committed seq
- /delete_scope removes all items, indexes and scope-level metadata for one scope
- /delete_scope_candidates is advisory only: returns candidates, never deletes; the client decides
- /delete_scope_candidates supports optional ?hours=N to exclude recently created scopes
- /render has a deliberately envelope-free hit/miss contract: 200 carries raw payload bytes, 404 carries an empty body; both use Content-Type application/octet-stream. Validation errors (400) still use the JSON error envelope. The cache does not sniff or guess MIME types — browser-facing setups must set Content-Type in the fronting proxy.
`

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(helpText))
}

func (api *API) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/append", api.handleAppend)
	mux.HandleFunc("/update", api.handleUpdate)
	mux.HandleFunc("/upsert", api.handleUpsert)
	mux.HandleFunc("/counter_add", api.handleCounterAdd)
	mux.HandleFunc("/delete", api.handleDelete)
	mux.HandleFunc("/delete_up_to", api.handleDeleteUpTo)
	// /head, /tail, /ts_range enforce the per-response cap inside their
	// shared writer (writeJSONWithMetaCap, called from writeItemsHit) —
	// no outer middleware needed. Earlier versions wrapped these with
	// a capResponse middleware that buffered the full handler output a
	// second time; folding the cap check into the marshal removed that
	// duplicate buffering.
	mux.HandleFunc("/head", api.handleHead)
	mux.HandleFunc("/tail", api.handleTail)
	mux.HandleFunc("/ts_range", api.handleTsRange)
	mux.HandleFunc("/get", api.handleGet)
	mux.HandleFunc("/render", api.handleRender)
	mux.HandleFunc("/help", api.handleHelp)
	// /stats and /delete_scope_candidates are admin-only — they enumerate
	// every scope name in the store, which in a multi-tenant deployment
	// would leak `_tokens`, `_guarded:<capID>:*`, `_counters_*` and the
	// per-scope item-counts/heat-stats those carry. Reachable only as
	// sub-calls through /admin (their handler functions stay on *API for
	// the dispatcher).
	// /multi_call, /admin and /guarded are NOT wrapped with capResponse:
	// they manage the per-response cap themselves via preflightResponseCap
	// (rejects batches the cap can't fit) plus the per-slot trim mechanism
	// (replaces oversized slot bodies with response_truncated markers).
	// Wrapping them again would buffer the whole envelope twice and turn
	// the pre-flight 507's specific error message into the wrapper's
	// generic "response would exceed maximum" — losing the actionable
	// guidance to either raise the cap or reduce the call count.
	mux.HandleFunc("/multi_call", api.handleMultiCall)
	// Admin-elevated endpoint. /wipe, /warm, /rebuild, /delete_scope,
	// /stats, /delete_scope_candidates are reachable only via /admin
	// (their handler functions still exist; they're removed from the
	// public mux). See guardedflow.md §J, §K.
	//
	// Gated on Config.EnableAdmin because /admin has no body-level auth
	// and trusts the transport layer entirely. Default-deny on the
	// Caddy module (a misconfigured public proxy is a real risk; the
	// operator must opt in AND add a route guard); default-allow on the
	// standalone binary (Unix-socket permissions are the gating layer).
	// Without the flag the route is not registered, public callers
	// get 404 — same shape as /guarded and /inbox.
	if api.store.enableAdmin {
		mux.HandleFunc("/admin", api.handleAdmin)
	}
	// Tenant-facing /guarded gateway. Registered only when the operator
	// configured a server secret — without one, HMAC computation would
	// produce identical capability_ids for every token, defeating
	// isolation. Empty secret → /guarded route not registered, public
	// callers get 404. See guardedflow.md §I.
	//
	// Counter scopes (`_counters_count_calls`, `_counters_count_kb`) are
	// NOT eagerly provisioned here — the first /guarded call creates
	// them via ensureScope, and they self-heal after a /wipe the same
	// way. Eager provisioning would clutter `/stats` for operators who
	// haven't yet seen any /guarded traffic.
	if api.store.serverSecret != "" {
		mux.HandleFunc("/guarded", api.handleGuarded)
	}
	// Shared write-only ingestion endpoint. Requires both a server
	// secret (for HMAC-derived capability_id) AND at least one
	// configured inbox scope. Either missing → route not registered,
	// public callers receive 404. See inbox.go.
	if api.store.serverSecret != "" && len(api.store.inboxScopes) > 0 {
		mux.HandleFunc("/inbox", api.handleInbox)
	}
}
