package scopecache

import (
	"net/http"
	"strconv"
	"time"
)

// Bulk write handlers (admin-only, reachable via /admin's dispatcher):
//
//   - /warm     — replace the scopes carried in the request, leave others alone
//   - /rebuild  — atomically replace the entire store
//
// Both decode an ItemsRequest, validate every item up-front, then route
// through Store.replaceScopes / Store.rebuildAll which each take the
// appropriate ascending-shard-index locks. /rebuild explicitly refuses
// an empty items array because that is almost always a client bug rather
// than an intentional clear-everything request.

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
