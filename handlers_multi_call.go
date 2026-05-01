package scopecache

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"time"
)

// subCallSpec pairs an allowed /multi_call path with the HTTP method
// and handler used to dispatch it. Each entry represents one slot in
// the closed whitelist; paths missing from the spec map are rejected
// with 400 for the whole batch.
//
// The handler reference here is the raw API method — NOT the
// capResponse-wrapped variant registered on the public mux. The
// dispatcher applies its own per-sub-call cap via cappedResponseWriter
// inside the loop, so wrapping again on the way in would just buffer
// twice.
type subCallSpec struct {
	method  string
	handler http.HandlerFunc
}

// buildMultiCallSpecs returns the closed whitelist of paths /multi_call
// dispatches to, paired with their fixed HTTP method and handler. The
// excluded set (/warm, /rebuild, /wipe, /delete_scope, /stats,
// /delete_scope_candidates, /render, /help and /multi_call itself) is
// documented in CLAUDE.md → Phase 4 design signals → /multi_call →
// Allowed paths. /delete_scope, /stats, and /delete_scope_candidates
// are admin-only — they are removed from the public mux entirely
// (handlers.go) and are only reachable via /admin, so they must not
// slip back in via /multi_call. Store-wide enumeration endpoints
// (/stats, /delete_scope_candidates) leak reserved scope names in
// multi-tenant deployments; admin-gating is the fix.
func (api *API) buildMultiCallSpecs() map[string]subCallSpec {
	return map[string]subCallSpec{
		"/append":       {http.MethodPost, api.handleAppend},
		"/get":          {http.MethodGet, api.handleGet},
		"/head":         {http.MethodGet, api.handleHead},
		"/tail":         {http.MethodGet, api.handleTail},
		"/ts_range":     {http.MethodGet, api.handleTsRange},
		"/update":       {http.MethodPost, api.handleUpdate},
		"/upsert":       {http.MethodPost, api.handleUpsert},
		"/counter_add":  {http.MethodPost, api.handleCounterAdd},
		"/delete":       {http.MethodPost, api.handleDelete},
		"/delete_up_to": {http.MethodPost, api.handleDeleteUpTo},
	}
}

// multiCallEntry is one sub-call inside a /multi_call request body.
// Path is required and must be in the whitelist. Query is used for
// GET-style sub-calls and serialized to a URL query string; Body is
// used for POSTs and forwarded verbatim. The dispatcher does no
// pre-flight semantic validation — each handler validates its own
// input, and whatever status/body it produces lands in the slot.
type multiCallEntry struct {
	Path  string                     `json:"path"`
	Query map[string]json.RawMessage `json:"query,omitempty"`
	Body  json.RawMessage            `json:"body,omitempty"`
}

// multiCallRequest is the top-level body for /multi_call. Calls is a
// pointer-to-slice so the dispatcher can distinguish "field absent"
// (nil → 400) from "explicitly empty" (non-nil empty → 200 with empty
// results), per the design contract.
type multiCallRequest struct {
	Calls *[]multiCallEntry `json:"calls"`
}

// multiCallResult is one slot in the outer envelope's results array.
// Status is the HTTP status code the standalone endpoint produced;
// Body is the literal JSON the standalone endpoint wrote. Body lives
// as json.RawMessage so the outer envelope nests it without
// re-marshalling.
type multiCallResult struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

// multiCallSuccessTrim is substituted for a 2xx slot whose own body
// fits but whose inclusion would push the outer envelope past the
// per-response cap. The status stays 2xx — the operation actually
// succeeded; only the metadata response is truncated.
var multiCallSuccessTrim = json.RawMessage(`{"ok":true,"response_truncated":true}`)

// multiCallErrorTrim is the non-2xx counterpart to multiCallSuccessTrim:
// the sub-call failed, the failure body is too big to fit alongside
// what's already accumulated, so we replace it with a minimal marker.
// The status stays whatever the handler produced (4xx/5xx); the caller
// at least knows it failed and that the body was trimmed.
var multiCallErrorTrim = json.RawMessage(`{"ok":false,"response_truncated":true}`)

// rawToQueryValue converts a JSON value to its URL-query string form.
// JSON strings are unwrapped (so "thread:900" becomes thread:900).
// Numbers and booleans use their raw JSON literal verbatim
// (10, true). null and empty raw yield "". Objects/arrays are rejected
// — a multi-value field doesn't compose with a flat query string and
// would silently lose the inner shape.
func rawToQueryValue(raw json.RawMessage) (string, error) {
	s := bytes.TrimSpace(raw)
	if len(s) == 0 || string(s) == "null" {
		return "", nil
	}
	switch s[0] {
	case '"':
		var v string
		if err := json.Unmarshal(s, &v); err != nil {
			return "", err
		}
		return v, nil
	case '{', '[':
		return "", fmt.Errorf("nested object/array is not supported as a query value")
	default:
		return string(s), nil
	}
}

// buildSubURL assembles `path` + optional `?query` for a sub-call.
// Returns the path unchanged when no query is present so the URL is
// minimal on the recorder request side.
func buildSubURL(path string, query map[string]json.RawMessage) (string, error) {
	if len(query) == 0 {
		return path, nil
	}
	vals := url.Values{}
	for k, raw := range query {
		s, err := rawToQueryValue(raw)
		if err != nil {
			return "", fmt.Errorf("invalid query value for %s.%s: %s", path, k, err)
		}
		vals.Set(k, s)
	}
	return path + "?" + vals.Encode(), nil
}

// preparedCall is the post-validation shape of one sub-call: the
// dispatch loop only consumes these, never raw multiCallEntry. Holding
// the assembled subURL and the normalised body lets the dispatcher
// fire blindly without any input-shape work that could produce 400 on
// the way through.
type preparedCall struct {
	spec   subCallSpec
	subURL string
	body   []byte // nil for GET; for POST always non-empty (defaulted to "{}")
}

// prepareSubCalls is the shape-only pre-validation pass shared by
// /multi_call, /admin, and /guarded. It walks every entry, builds the
// subURL (which is the only place a malformed query value — nested
// object/array — would surface), and returns either every sub-call
// fully prepared or an error so the caller can 400 the whole batch
// before any side effect lands.
//
// The caller has already validated each path against `specs`, so the
// missing-spec check here is defence in depth — if the whitelist gets
// out of sync with the dispatcher, this catches it instead of letting
// the dispatcher panic on a nil handler.
func prepareSubCalls(calls []multiCallEntry, specs map[string]subCallSpec) ([]preparedCall, error) {
	prepared := make([]preparedCall, len(calls))
	for i, call := range calls {
		spec, ok := specs[call.Path]
		if !ok {
			return nil, fmt.Errorf("path '%s' (calls[%d]) is not allowed", call.Path, i)
		}
		subURL, err := buildSubURL(call.Path, call.Query)
		if err != nil {
			return nil, fmt.Errorf("calls[%d]: %s", i, err.Error())
		}
		var body []byte
		if spec.method != http.MethodGet {
			body = []byte(call.Body)
			if len(body) == 0 {
				// Handlers expect a JSON body; empty would parse-error
				// with a misleading message. {} lets the handler's own
				// validator produce the right "missing scope/payload".
				body = []byte("{}")
			}
		}
		prepared[i] = preparedCall{spec: spec, subURL: subURL, body: body}
	}
	return prepared, nil
}

// validateBatchShape is the shape-only pre-validation prologue shared
// by /multi_call, /admin, and /guarded. It performs four checks in
// order — nil `calls`, count cap, response-cap pre-flight, whitelist
// — and writes the corresponding error response on the first failure.
//
// Returns (calls, false) on success: the dereferenced calls slice for
// the caller to use, and `done=false` meaning the caller should
// continue. Returns (nil, true) on failure: a response has already
// been written, the caller should `return`. Same shape as
// preflightResponseCap (which it wraps).
//
// `endpoint` is interpolated into the whitelist error string so the
// caller name matches the actual endpoint (`"...not allowed in
// /multi_call"`, `"...in /admin"`, `"...in /guarded"`). Same closed
// `specs` map the caller hands to prepareSubCalls — keeping the
// whitelist source of truth in one place per endpoint.
//
// The body cap (decodeBody) is NOT covered: /admin uses the larger
// bulk-request cap (it must accept a /rebuild body) while /multi_call
// and /guarded use SCOPECACHE_MAX_MULTI_CALL_MB. Each caller does its
// own decodeBody before passing the parsed `callsPtr` here.
func (api *API) validateBatchShape(
	w http.ResponseWriter,
	started time.Time,
	callsPtr *[]multiCallEntry,
	specs map[string]subCallSpec,
	endpoint string,
) ([]multiCallEntry, bool) {
	if callsPtr == nil {
		badRequest(w, started, "the 'calls' field is required")
		return nil, true
	}
	calls := *callsPtr
	if len(calls) > api.maxMultiCallCount {
		badRequest(w, started, fmt.Sprintf("the 'calls' array has %d entries; the maximum is %d", len(calls), api.maxMultiCallCount))
		return nil, true
	}
	if preflightResponseCap(w, started, len(calls), api.maxResponseBytes) {
		return nil, true
	}
	for i, call := range calls {
		if _, ok := specs[call.Path]; !ok {
			badRequest(w, started, fmt.Sprintf("path '%s' (calls[%d]) is not allowed in %s", call.Path, i, endpoint))
			return nil, true
		}
	}
	return calls, false
}

// envelope-budgeting constants. multiCallEnvelopeOverhead covers the
// outer JSON frame (`{"ok":true,"count":N,"approx_response_mb":...,
// "duration_us":...,"results":[]}`) plus a couple of brackets and
// commas; multiCallSlotOverhead covers per-slot framing
// (`{"status":NNN,"body":...},`). Both err on the conservative side so
// the trim path engages slightly earlier than strictly necessary —
// slack is preferable to overshoot.
const (
	multiCallEnvelopeOverhead = 256
	multiCallSlotOverhead     = 32
	// multiCallMaxTrimMarkerSize is the byte length of the largest
	// truncation marker (multiCallErrorTrim, currently 38 bytes) plus a
	// small slack. The pre-flight check uses it to verify that the
	// per-response cap can fit at minimum an all-trimmed envelope —
	// otherwise the trim mechanism cannot guarantee the outer response
	// stays under cap, and the wrapper would 507 the whole envelope
	// after slot-by-slot side effects already landed.
	multiCallMaxTrimMarkerSize = 40
)

// minMultiCallResponseSize returns the lower bound on the outer
// envelope's wire size for a batch of n sub-calls — the size produced
// when every slot's body has been trimmed to its `response_truncated`
// marker. If respCap is below this, even the trim mechanism cannot
// keep the response under cap, and the dispatcher must reject the
// whole batch up-front before any side effect lands.
func minMultiCallResponseSize(n int) int64 {
	return multiCallEnvelopeOverhead + int64(n)*(multiCallSlotOverhead+multiCallMaxTrimMarkerSize)
}

// preflightResponseCap returns true when respCap is too small to fit
// even an all-trimmed envelope for a batch of n sub-calls. The caller
// rejects with 507 and the canonical store-cap error shape, naming
// the minimum size the response would need vs. the configured cap so
// the operator can either raise SCOPECACHE_MAX_RESPONSE_MB or shrink
// the batch.
func preflightResponseCap(w http.ResponseWriter, started time.Time, n int, respCap int64) bool {
	minSize := minMultiCallResponseSize(n)
	if minSize <= respCap {
		return false
	}
	writeJSONWithDuration(w, http.StatusInsufficientStorage, orderedFields{
		{"ok", false},
		{"error", "response cap too small to dispatch this batch even with every slot trimmed; raise SCOPECACHE_MAX_RESPONSE_MB or reduce the call count"},
		{"approx_response_mb", MB(minSize)},
		{"max_response_mb", MB(respCap)},
	}, started)
	return true
}

// batchDispatchOptions parameterises the per-handler differences
// between /multi_call, /admin, and /guarded sub-call dispatch. Both
// fields are zero-valued (false / "") for /multi_call's no-policy
// case; /admin sets AdminContext; /guarded sets both AdminContext
// and StripPrefix.
type batchDispatchOptions struct {
	// AdminContext marks each synthetic sub-request with the admin
	// context-marker so the inner handler's rejectReservedScope check
	// skips. Required for /admin (operator can write to `_*` scopes
	// directly) and /guarded (the rewritten scope starts with
	// `_guarded:` and would otherwise be rejected by the inner handler).
	AdminContext bool
	// StripPrefix, when non-empty, is passed to stripGuardedPrefix on
	// every result body before envelope-budget accounting. Empty
	// string disables stripping entirely. The strip MUST run before
	// trim accounting (encoded inside this helper) so the truncation
	// marker, if it fires, cannot leak the un-stripped form — that's
	// a security-adjacent invariant /guarded relies on.
	StripPrefix string
}

// dispatchPreparedCalls is the shared sequential-dispatch loop for
// /multi_call, /admin, and /guarded. It runs every prepared sub-call
// through its registered handler, applies the optional admin-context
// marker and prefix-strip, enforces the per-response-cap trim
// asymmetry (2xx → success-marker, non-2xx → error-marker), and
// returns the assembled results plus the running body-bytes total
// (the latter is consumed by /guarded for counter increments;
// /multi_call and /admin discard it).
//
// Centralising the loop encodes three correctness invariants in one
// place that previously lived in three near-identical copies:
//   - admin context is applied AFTER subReq is built, never forgotten
//   - strip runs BEFORE trim accounting (truncation marker cannot
//     leak the un-stripped form)
//   - defensive copy of the slot body before the next iteration
//
// The respCap is read from api.maxResponseBytes — all three
// callers pass the same value, so it's not part of the options.
func (api *API) dispatchPreparedCalls(prepared []preparedCall, opts batchDispatchOptions) ([]multiCallResult, int64) {
	respCap := api.maxResponseBytes
	bodyBudget := respCap - multiCallEnvelopeOverhead - int64(len(prepared))*multiCallSlotOverhead
	if bodyBudget < 0 {
		bodyBudget = 0
	}

	results := make([]multiCallResult, 0, len(prepared))
	var bodyBytesUsed int64

	for _, p := range prepared {
		var subReq *http.Request
		if p.spec.method == http.MethodGet {
			subReq = httptest.NewRequest(p.spec.method, p.subURL, nil)
		} else {
			subReq = httptest.NewRequest(p.spec.method, p.subURL, bytes.NewReader(p.body))
			subReq.Header.Set("Content-Type", "application/json")
		}
		if opts.AdminContext {
			subReq = withAdminContext(subReq)
		}

		// Wrap the per-call recorder with cappedResponseWriter so a
		// pathological sub-call response is bounded in memory and
		// reported to the slot as 507 instead of being buffered in
		// full. crw.flush replays into rec, so rec.Body holds the
		// finalised slot body either way.
		rec := httptest.NewRecorder()
		crw := newCappedResponseWriter(rec, respCap, time.Now())
		p.spec.handler(crw, subReq)
		crw.flush()

		status := rec.Code
		// Strip the trailing newline that json.Encoder emits — keeps
		// the nested JSON tidy inside the outer results array.
		bodyBytes := bytes.TrimRight(rec.Body.Bytes(), "\n")

		// Strip MUST run before trim accounting. If trim fires after
		// strip, the truncation marker is what the client sees; if
		// trim fires before strip, the un-stripped body could be
		// trimmed away but a smaller-than-cap version could still
		// leak the rewritten scope name (`_guarded:<capId>:...`) in
		// the slot body. Order is load-bearing — do not reorder.
		if opts.StripPrefix != "" {
			bodyBytes = stripGuardedPrefix(bodyBytes, opts.StripPrefix)
		}

		// Outer-envelope cap: would including this slot push the
		// running body total past the conservative budget? If so, trim
		// asymmetrically — 2xx keeps its status with a success
		// truncation marker, non-2xx keeps its status with an error
		// truncation marker. The 507 sub-calls produced just above
		// take the error branch.
		if bodyBytesUsed+int64(len(bodyBytes)) > bodyBudget {
			if status >= 200 && status < 300 {
				bodyBytes = []byte(multiCallSuccessTrim)
			} else {
				bodyBytes = []byte(multiCallErrorTrim)
			}
		}
		bodyBytesUsed += int64(len(bodyBytes))

		// Defensive copy — rec.Body's underlying slice would otherwise
		// be the same buffer the next iteration's recorder writes
		// into. (httptest allocates a new recorder per call here, so
		// this is precaution rather than necessity, but it's cheap.)
		slot := make([]byte, len(bodyBytes))
		copy(slot, bodyBytes)
		results = append(results, multiCallResult{Status: status, Body: slot})
	}

	return results, bodyBytesUsed
}

// handleMultiCall dispatches N self-contained sub-calls through the
// existing API handlers and assembles the outer JSON envelope. See
// CLAUDE.md → Phase 4 design signals → /multi_call for the design
// contract this implements.
func (api *API) handleMultiCall(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodPost {
		methodNotAllowed(w, started)
		return
	}

	var req multiCallRequest
	if err := decodeBody(w, r, api.maxMultiCallBytes, &req); err != nil {
		badRequest(w, started, err.Error())
		return
	}

	// Shape-only validation prologue (nil calls, count cap, response
	// pre-flight, whitelist). On failure the helper has written the
	// response and we return; on success `calls` is the dereferenced
	// slice. Why pre-flight runs here: see preflightResponseCap doc —
	// without it, the dispatch loop still runs and every slot trims to
	// a marker, but the accumulated response can still exceed the cap,
	// and the outer wrapper would 507 the whole envelope after side
	// effects already landed.
	calls, done := api.validateBatchShape(w, started, req.Calls, api.multiCallSpecs, "/multi_call")
	if done {
		return
	}

	// Pre-build every subURL and normalised body in a separate pass so
	// a malformed query value (e.g. nested object/array) on calls[k]
	// rejects the whole batch before calls[0..k-1] have committed. The
	// dispatch loop below only handles handler-level errors, which are
	// expected and slot-local; input-shape errors are batch-level and
	// must land before any side effect.
	prepared, err := prepareSubCalls(calls, api.multiCallSpecs)
	if err != nil {
		badRequest(w, started, err.Error())
		return
	}

	results, _ := api.dispatchPreparedCalls(prepared, batchDispatchOptions{})

	writeJSONWithMeta(w, http.StatusOK, orderedFields{
		{"ok", true},
		{"count", len(results)},
		{"results", results},
	}, started)
}
