package scopecache

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
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
//   - Excludes /render: raw bytes don't fit a JSON results array
//     cleanly — the standalone endpoint's defining property is "no
//     envelope, Content-Type from the fronting proxy", which is a
//     category mismatch with batch-dispatcher slot semantics. Tenants
//     reach /render via a Caddy middleware that rewrites the scope
//     from a bearer token (see CLAUDE.md helpers — scopecache_bearer_prefix);
//     /guarded body is not the right transport for raw-byte streams.
func (api *API) buildGuardedCallSpecs() map[string]subCallSpec {
	return map[string]subCallSpec{
		"/append":       {http.MethodPost, api.handleAppend},
		"/get":          {http.MethodGet, api.handleGet},
		"/head":         {http.MethodGet, api.handleHead},
		"/tail":         {http.MethodGet, api.handleTail},
		"/update":       {http.MethodPost, api.handleUpdate},
		"/upsert":       {http.MethodPost, api.handleUpsert},
		"/counter_add":  {http.MethodPost, api.handleCounterAdd},
		"/delete":       {http.MethodPost, api.handleDelete},
		"/delete_up_to": {http.MethodPost, api.handleDeleteUpTo},
	}
}

// guardedRequest is the top-level body for /guarded. Token is required;
// Calls is the same shape as /multi_call. See guardedflow.md §C.
type guardedRequest struct {
	Token string            `json:"token"`
	Calls *[]multiCallEntry `json:"calls"`
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

// rewriteCallScope mutates a multiCallEntry's body or query so the
// `scope` field has the prefix prepended. Returns the resulting
// (rewritten) scope value for use in the existence check.
//
// Method-aware since v0.5.20: GET sub-calls must carry `scope` in
// the URL query; POST sub-calls must carry it in the body. The
// inner handlers read only one of the two (matching their HTTP
// method), so a misplaced scope was previously rewritten by this
// function and then ignored by the dispatched handler — the call
// failed downstream with a "missing scope" error from a different
// code path. Rejecting up-front per method gives the caller a
// clear, single error site and keeps rewrite/dispatch in lockstep.
//
// Also refuses scope in BOTH body and query (kept from the original
// implementation): a caller setting body.scope=allowed AND
// query.scope=cross-tenant would have passed the existence check on
// whichever value was rewritten first while the inner handler read
// the other un-rewritten one. There is no legitimate reason to set
// both, so reject the whole batch up-front.
func rewriteCallScope(call *multiCallEntry, prefix, method string) (string, error) {
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

	switch method {
	case http.MethodGet:
		if bodyHasScope {
			return "", fmt.Errorf("'scope' must be in query for GET sub-calls (body.scope is ignored by the inner handler)")
		}
		if !queryHasScope {
			return "", fmt.Errorf("missing 'scope' in query for GET sub-call")
		}
		var s string
		if err := json.Unmarshal(call.Query["scope"], &s); err != nil {
			return "", fmt.Errorf("'scope' in query is not a string: %s", err)
		}
		rewritten := prefix + s
		newScopeRaw, _ := json.Marshal(rewritten)
		call.Query["scope"] = newScopeRaw
		return rewritten, nil

	case http.MethodPost:
		if queryHasScope {
			return "", fmt.Errorf("'scope' must be in body for POST sub-calls (query.scope is ignored by the inner handler)")
		}
		if !bodyHasScope {
			return "", fmt.Errorf("missing 'scope' in body for POST sub-call")
		}
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

	default:
		// /guarded's whitelist only contains GET and POST handlers;
		// any other method here is an internal bug, not a client error.
		return "", fmt.Errorf("internal: unexpected method %q for sub-call", method)
	}
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
	if err := decodeBody(w, r, api.maxMultiCallBytes, &req); err != nil {
		badRequest(w, started, err.Error())
		return
	}

	// Steps 1.5-2: shape-only validation prologue (nil calls, count
	// cap, response pre-flight, whitelist) — shared with /multi_call
	// and /admin via validateBatchShape.
	//
	// Note: whitelist runs BEFORE the token/auth check (it used to be
	// step 6, after auth). Same rationale as preflightResponseCap-
	// before-auth: a misconfigured caller learns the real issue is
	// request shape, not auth. A whitelist miss leaks no more than the
	// publicly-documented set of /guarded paths (scopecache-rfc.md
	// §6.4); auth still fires before any sub-call work or side effect,
	// which is the load-bearing security guarantee.
	calls, done := api.validateBatchShape(w, started, req.Calls, api.guardedCallSpecs, "/guarded")
	if done {
		return
	}

	// Step 3: extract token.
	if req.Token == "" {
		writeJSONWithDuration(w, http.StatusUnauthorized, orderedFields{
			{"ok", false},
			{"error", "the 'token' field is required"},
		}, started)
		return
	}

	// Step 4-5: compute capability_id and prefix.
	capabilityID := computeCapabilityID(api.serverSecret, req.Token)
	prefix := "_guarded:" + capabilityID + ":"

	// Step 6: auth-gate. Single lookup in the _tokens scope: does an
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

	// Step 7: scope rewrite per call. Method-aware — GET sub-calls
	// must carry scope in the query, POST sub-calls in the body.
	// Whitelist was already enforced in validateBatchShape above, so
	// guardedCallSpecs lookup is guaranteed to hit; the defensive
	// `ok` check just makes future refactors loud instead of silent.
	for i := range calls {
		spec, ok := api.guardedCallSpecs[calls[i].Path]
		if !ok {
			badRequest(w, started, fmt.Sprintf("calls[%d]: path %q is not allowed in /guarded", i, calls[i].Path))
			return
		}
		if _, err := rewriteCallScope(&calls[i], prefix, spec.method); err != nil {
			badRequest(w, started, fmt.Sprintf("calls[%d]: %s", i, err.Error()))
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

	// Step 8-9: dispatch via the shared helper. AdminContext lets the
	// inner handler accept rewritten `_guarded:*` scopes; StripPrefix
	// peels the rewritten prefix off every result body before the trim
	// check so the truncation marker (if it fires) cannot leak the
	// rewritten form.
	results, bodyBytesUsed := api.dispatchPreparedCalls(prepared, batchDispatchOptions{
		AdminContext: true,
		StripPrefix:  prefix,
	})

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
		if callsBuf := api.store.ensureScope(countersScopeCalls); callsBuf != nil {
			_, _, _ = callsBuf.counterAdd(countersScopeCalls, capabilityID, calls)
		}
	}

	if kb > 0 {
		if kbBuf := api.store.ensureScope(countersScopeKB); kbBuf != nil {
			_, _, _ = kbBuf.counterAdd(countersScopeKB, capabilityID, kb)
		}
	}
}
