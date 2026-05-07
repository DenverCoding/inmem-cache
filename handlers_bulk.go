// Bulk write handlers on the public mux:
//
//   - /warm     — replace the scopes in the request, leave others alone
//   - /rebuild  — atomically replace the entire store
//
// Both decode an itemsRequest and route through Store.replaceScopes /
// Store.rebuildAll. Per-item shape validation lives in those Store
// methods, so handlers decode, delegate, and map errors.
//
// /rebuild explicitly refuses an empty items array because that is
// almost always a client bug rather than an intentional clear-
// everything request — endpoint-specific HTTP policy, not a per-item
// shape rule.

package scopecache

import (
	"net/http"
	"time"
)

func (api *API) handleWarm(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodPost {
		methodNotAllowed(w, started)
		return
	}

	var req itemsRequest
	if err := decodeBody(w, r, api.maxBulkBytes, &req); err != nil {
		badRequest(w, started, err.Error())
		return
	}

	grouped := groupItemsByScope(req.Items)
	replacedScopes, err := api.store.replaceScopes(grouped)
	if err != nil {
		// /warm cannot produce *ScopeFullError (only single-item paths do);
		// scopeForSFE is unused here.
		writeMutationError(w, started, err, "")
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

	var req itemsRequest
	if err := decodeBody(w, r, api.maxBulkBytes, &req); err != nil {
		badRequest(w, started, err.Error())
		return
	}

	// An empty items[] would wipe the entire store. That is almost always a
	// client bug (missing payload, wrong key, serialization glitch) rather
	// than an intentional "clear everything" call. Refuse it explicitly;
	// clients that really want to clear the cache should /delete_scope per
	// scope or restart the service. This /rebuild-specific guard stays in
	// the handler — it's an HTTP policy ("explicit-non-empty-required"),
	// not a per-item shape check; Go-API callers of Gateway.Rebuild who
	// want a wipe-shaped rebuild can pass an empty map intentionally.
	if len(req.Items) == 0 {
		badRequest(w, started, "the 'items' array must not be empty for the '/rebuild' endpoint")
		return
	}

	grouped := groupItemsByScope(req.Items)
	rebuiltScopes, rebuiltItems, err := api.store.rebuildAll(grouped)
	if err != nil {
		// /rebuild cannot produce *ScopeFullError (only single-item paths
		// do); scopeForSFE is unused here.
		writeMutationError(w, started, err, "")
		return
	}

	writeJSONWithDuration(w, http.StatusOK, orderedFields{
		{"ok", true},
		{"count", len(req.Items)},
		{"rebuilt_scopes", rebuiltScopes},
		{"rebuilt_items", rebuiltItems},
	}, started)
}
