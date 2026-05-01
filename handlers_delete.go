package scopecache

import (
	"net/http"
	"time"
)

// Delete handlers on the public mux + admin-only:
//
//   - /delete         — single-item delete by scope+id or scope+seq (public)
//   - /delete_up_to   — drain seq prefix in one shot (public, write-buffer pattern)
//   - /delete_scope   — remove a whole scope (admin-only via /admin)
//   - /wipe           — clear every scope, every item (admin-only via /admin)
//
// The four are grouped here by destructive-semantics rather than by mux:
// /delete and /delete_up_to live on the public mux for normal client
// pruning; /delete_scope and /wipe are reachable only via /admin's
// dispatcher because they enumerate / nuke beyond a single item.

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
