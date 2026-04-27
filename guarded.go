package scopecache

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"
)

// Cache-internal counter scopes used by /guarded for usage tracking.
// Auto-provisioned by ensureScope at handler init and defensively on
// each request — see guardedflow.md §M.
const (
	countersScopeCalls = "_counters_count_calls"
	countersScopeKB    = "_counters_count_kb"

	// tokensScope is the cache-internal auth-gate. Each active tenant
	// has one item under this scope, keyed by the tenant's
	// capability_id. /guarded checks for the item's presence as the
	// single "is this token valid?" gate. Operator manages the items
	// via /admin: /upsert at token issuance, /delete at revocation.
	// Item payload is opaque to the cache — operators commonly store
	// {user_id, issued_at, ...} for their own bookkeeping.
	tokensScope = "_tokens"
)

// buildGuardedCallSpecs returns the closed whitelist of paths /guarded
// dispatches to, paired with their fixed HTTP method and raw handler.
// Narrower than /multi_call's whitelist:
//   - Excludes /delete_scope: tenants can't deprovision their own
//     namespace (would lock themselves out until operator re-provisions).
//   - Excludes /stats and /delete_scope_candidates: store-wide views.
//   - Excludes /wipe, /warm, /rebuild: admin-only operations.
//
// /render IS included; /guarded handles its raw-bytes response (single-
// call returns raw, multi-call slot base64-encodes — see guardedflow.md
// §G).
func (api *API) buildGuardedCallSpecs() map[string]subCallSpec {
	return map[string]subCallSpec{
		"/append":      {http.MethodPost, api.handleAppend},
		"/get":         {http.MethodGet, api.handleGet},
		"/head":        {http.MethodGet, api.handleHead},
		"/tail":        {http.MethodGet, api.handleTail},
		"/ts_range":    {http.MethodGet, api.handleTsRange},
		"/update":      {http.MethodPost, api.handleUpdate},
		"/upsert":      {http.MethodPost, api.handleUpsert},
		"/counter_add": {http.MethodPost, api.handleCounterAdd},
		"/delete":      {http.MethodPost, api.handleDelete},
		"/delete_up_to": {http.MethodPost, api.handleDeleteUpTo},
		"/render":      {http.MethodGet, api.handleRender},
	}
}

// guardedRequest is the top-level body for /guarded. Token is required;
// Calls is the same shape as /multi_call. See guardedflow.md §C.
type guardedRequest struct {
	Token string             `json:"token"`
	Calls *[]multiCallEntry  `json:"calls"`
}

// tenantIsProvisioned answers /guarded's auth-gate: is there an item
// with id=capabilityID in the _tokens scope? Returns false when the
// scope itself does not exist (no tokens issued yet) or when the
// tenant's id is missing (token never issued, or revoked via
// /admin /delete).
//
// Lookup is two map operations (s.scopes -> _tokens, then byID), both
// under read locks — no contention with concurrent /guarded reads.
func (api *API) tenantIsProvisioned(capabilityID string) bool {
	buf, ok := api.store.getScope(tokensScope)
	if !ok {
		return false
	}
	buf.mu.RLock()
	_, hit := buf.byID[capabilityID]
	buf.mu.RUnlock()
	return hit
}

// computeCapabilityID returns hex(HMAC_SHA256(serverSecret, token)) as a
// 64-character lowercase hex string. Both scopecache (per /guarded
// request) and the application using it (PHP/workers) compute this from
// the same inputs to derive the prefix `_guarded:<capabilityID>:`. See
// guardedflow.md §D.
func computeCapabilityID(serverSecret, token string) string {
	h := hmac.New(sha256.New, []byte(serverSecret))
	h.Write([]byte(token))
	return hex.EncodeToString(h.Sum(nil))
}

// rewriteCallScope mutates a multiCallEntry's body and query so any
// `scope` field has the prefix prepended. Returns the resulting
// (rewritten) scope value for use in the existence check, or "" if the
// call carries no scope at all (e.g. an /admin /stats call would, but
// /guarded's whitelist excludes those — every /guarded sub-call has a
// scope in either body or query).
//
// Refuses to rewrite a call that carries `scope` in BOTH body and
// query: a GET sub-call's handler reads only the URL query, but the
// existence check would have run on whichever scope this function
// happened to rewrite first. A caller setting body.scope=allowed AND
// query.scope=cross-tenant would otherwise pass the existence check
// on the body value while the GET handler reads the un-rewritten
// query value — a cross-tenant read. There is no legitimate reason to
// set both, so reject the whole batch up-front.
func rewriteCallScope(call *multiCallEntry, prefix string) (string, error) {
	bodyHasScope := false
	var bodyMap map[string]json.RawMessage
	if len(call.Body) > 0 {
		if err := json.Unmarshal(call.Body, &bodyMap); err != nil {
			return "", fmt.Errorf("invalid JSON body: %s", err)
		}
		_, bodyHasScope = bodyMap["scope"]
	}
	_, queryHasScope := call.Query["scope"]

	if bodyHasScope && queryHasScope {
		return "", fmt.Errorf("'scope' must be in body OR query, not both")
	}

	// Body shape: rewrite body.scope if present.
	if bodyHasScope {
		var s string
		if err := json.Unmarshal(bodyMap["scope"], &s); err != nil {
			return "", fmt.Errorf("'scope' in body is not a string: %s", err)
		}
		rewritten := prefix + s
		newScopeRaw, _ := json.Marshal(rewritten)
		bodyMap["scope"] = newScopeRaw
		newBody, err := json.Marshal(bodyMap)
		if err != nil {
			return "", err
		}
		call.Body = newBody
		return rewritten, nil
	}
	// Query shape: rewrite query["scope"] if present.
	if queryHasScope {
		var s string
		if err := json.Unmarshal(call.Query["scope"], &s); err != nil {
			return "", fmt.Errorf("'scope' in query is not a string: %s", err)
		}
		rewritten := prefix + s
		newScopeRaw, _ := json.Marshal(rewritten)
		call.Query["scope"] = newScopeRaw
		return rewritten, nil
	}
	return "", nil
}

// handleGuarded is the tenant-facing multi-call gateway. Token in body
// derives the capability_id and namespace prefix; sub-calls operate
// only on operator-provisioned `_guarded:<capId>:*` scopes. See
// guardedflow.md §F for the full validation flow.
func (api *API) handleGuarded(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodPost {
		methodNotAllowed(w, started)
		return
	}

	// Step 1: parse body.
	var req guardedRequest
	if err := decodeBody(w, r, api.store.maxMultiCallBytes, &req); err != nil {
		badRequest(w, started, err.Error())
		return
	}
	if req.Calls == nil {
		badRequest(w, started, "the 'calls' field is required")
		return
	}
	calls := *req.Calls
	if len(calls) > api.store.maxMultiCallCount {
		badRequest(w, started, fmt.Sprintf("the 'calls' array has %d entries; the maximum is %d", len(calls), api.store.maxMultiCallCount))
		return
	}

	// Pre-flight response cap (see preflightResponseCap doc). Runs
	// before the token check so a misconfigured tenant doesn't think
	// auth failed when the real issue is operator-side cap sizing.
	if preflightResponseCap(w, started, len(calls), api.store.maxResponseBytes) {
		return
	}

	// Step 2: extract token.
	if req.Token == "" {
		writeJSONWithDuration(w, http.StatusUnauthorized, orderedFields{
			{"ok", false},
			{"error", "the 'token' field is required"},
		}, started)
		return
	}

	// Step 3-4: compute capability_id and prefix.
	capabilityID := computeCapabilityID(api.store.serverSecret, req.Token)
	prefix := "_guarded:" + capabilityID + ":"

	// Step 5: auth-gate. Single lookup in the _tokens scope: does an
	// item with id=capabilityID exist? If yes, this token was issued
	// by the operator and not revoked. If no (or _tokens itself does
	// not exist yet), reject the whole batch — no further work runs,
	// no side effects, no counter ticks.
	//
	// This replaces the previous per-scope existence check
	// (`_guarded:<capId>:<scope>` provisioning per tenant per scope).
	// Tenants now self-organize within their `_guarded:<capId>:*`
	// prefix; the operator's only obligation is to register/revoke
	// the tenant's capabilityID in _tokens.
	if !api.tenantIsProvisioned(capabilityID) {
		badRequest(w, started, "tenant_not_provisioned")
		return
	}

	// Step 6: whitelist check per call. Whole-batch reject on miss.
	for i, call := range calls {
		if _, ok := api.guardedCallSpecs[call.Path]; !ok {
			badRequest(w, started, fmt.Sprintf("path '%s' (calls[%d]) is not allowed in /guarded", call.Path, i))
			return
		}
	}

	// Step 7: scope rewrite per call. The rewrite itself never fails
	// (any string is acceptable as a prefixed scope); but a call that
	// carries no scope at all is malformed and rejects the batch.
	for i := range calls {
		rewritten, err := rewriteCallScope(&calls[i], prefix)
		if err != nil {
			badRequest(w, started, fmt.Sprintf("calls[%d]: %s", i, err.Error()))
			return
		}
		if rewritten == "" {
			// /guarded's whitelist is per-scope only; a call with no
			// scope in body or query is malformed.
			badRequest(w, started, fmt.Sprintf("calls[%d]: missing 'scope'", i))
			return
		}
	}

	// Step 7.5: pre-build subURLs and bodies after scope rewrite so a
	// malformed query value (nested object/array) on calls[k] rejects
	// the whole batch before calls[0..k-1] commit any side effect.
	// Rewrite already mutated call.Body/call.Query in place, so the
	// prepared URLs reflect the per-tenant scope.
	prepared, err := prepareSubCalls(calls, api.guardedCallSpecs)
	if err != nil {
		badRequest(w, started, err.Error())
		return
	}

	// Step 8-9: dispatch via shared loop, strip prefix from each result body.
	respCap := api.store.maxResponseBytes
	bodyBudget := respCap - multiCallEnvelopeOverhead - int64(len(calls))*multiCallSlotOverhead
	if bodyBudget < 0 {
		bodyBudget = 0
	}

	results := make([]multiCallResult, 0, len(calls))
	var bodyBytesUsed int64

	for _, p := range prepared {
		var subReq *http.Request
		if p.spec.method == http.MethodGet {
			subReq = httptest.NewRequest(p.spec.method, p.subURL, nil)
		} else {
			subReq = httptest.NewRequest(p.spec.method, p.subURL, bytes.NewReader(p.body))
			subReq.Header.Set("Content-Type", "application/json")
		}
		// Mark as admin-context so the inner handler skips the public
		// reserved-prefix check (the rewritten scope starts with
		// `_guarded:` and would otherwise be rejected).
		subReq = withAdminContext(subReq)

		rec := httptest.NewRecorder()
		crw := newCappedResponseWriter(rec, respCap, time.Now())
		p.spec.handler(crw, subReq)
		crw.flush()

		status := rec.Code
		bodyBytes := bytes.TrimRight(rec.Body.Bytes(), "\n")

		// Strip prefix from response body before envelope-budget
		// accounting — the trim path then sees the post-strip size.
		bodyBytes = stripGuardedPrefix(bodyBytes, prefix)

		if bodyBytesUsed+int64(len(bodyBytes)) > bodyBudget {
			if status >= 200 && status < 300 {
				bodyBytes = []byte(multiCallSuccessTrim)
			} else {
				bodyBytes = []byte(multiCallErrorTrim)
			}
		}
		bodyBytesUsed += int64(len(bodyBytes))

		slot := make([]byte, len(bodyBytes))
		copy(slot, bodyBytes)
		results = append(results, multiCallResult{Status: status, Body: slot})
	}

	// Step 10: counter increments. Best-effort — failures are silently
	// ignored (observability counter, not auth). Calls counter advances
	// by len(calls) so a batch of N sub-calls counts as N units of cache
	// work, matching how the kb counter (which scales with the outer
	// envelope's byte size) already behaves. Approximate response bytes
	// from the slots' bytes used (the outer envelope adds ~256 constant
	// bytes; the granularity at KiB matters less than at bytes).
	// See guardedflow.md §M.
	approxKB := (bodyBytesUsed + multiCallEnvelopeOverhead) / 1024
	api.guardedIncrementCounters(capabilityID, int64(len(calls)), approxKB)

	writeJSONWithMeta(w, http.StatusOK, orderedFields{
		{"ok", true},
		{"count", len(results)},
		{"results", results},
	}, started)
}

// guardedIncrementCounters bumps the two cache-internal counter scopes
// for a /guarded request. Auto-provisions both scopes via ensureScope —
// self-heals after a /wipe. Failures are silently ignored: this is an
// observability counter, not auth. See guardedflow.md §M.
func (api *API) guardedIncrementCounters(capabilityID string, calls, kb int64) {
	if calls > 0 {
		callsBuf := api.store.ensureScope(countersScopeCalls)
		_, _, _ = callsBuf.counterAdd(countersScopeCalls, capabilityID, calls)
	}

	if kb > 0 {
		kbBuf := api.store.ensureScope(countersScopeKB)
		_, _, _ = kbBuf.counterAdd(countersScopeKB, capabilityID, kb)
	}
}
