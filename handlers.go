package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"time"
)

type API struct {
	store *Store
	// maxBulkBytes is the per-request body cap for /warm and /rebuild. It is
	// derived from the store's configured byte cap via bulkRequestBytesFor so
	// a fully-loaded store can always be expressed as a single bulk request.
	maxBulkBytes int64
}

// NewAPI wires the HTTP API to a Store and derives request caps that scale
// with the store's configuration.
func NewAPI(store *Store) *API {
	return &API{
		store:        store,
		maxBulkBytes: bulkRequestBytesFor(store.maxStoreBytes),
	}
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

func writeJSONWithDuration(w http.ResponseWriter, code int, payload map[string]interface{}, started time.Time) {
	payload["duration_us"] = time.Since(started).Microseconds()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(payload)
}

func badRequest(w http.ResponseWriter, started time.Time, message string) {
	writeJSONWithDuration(w, http.StatusBadRequest, map[string]interface{}{
		"ok":    false,
		"error": message,
	}, started)
}

func conflict(w http.ResponseWriter, started time.Time, message string) {
	writeJSONWithDuration(w, http.StatusConflict, map[string]interface{}{
		"ok":    false,
		"error": message,
	}, started)
}

// scopeFull responds with 507 Insufficient Storage and the full offender list.
// Used when an /append, /warm, or /rebuild would push one or more scopes past
// the per-scope capacity. The client is expected to drain (e.g. /delete-up-to
// or /delete-scope) or chunk the batch and retry.
func scopeFull(w http.ResponseWriter, started time.Time, offenders []ScopeCapacityOffender) {
	msg := "scope is at capacity"
	if len(offenders) > 1 {
		msg = "multiple scopes are at capacity"
	}
	writeJSONWithDuration(w, http.StatusInsufficientStorage, map[string]interface{}{
		"ok":     false,
		"error":  msg,
		"scopes": offenders,
	}, started)
}

// storeFull responds with 507 when the aggregate byte cap would be exceeded.
// The body carries the store-level totals (all in MiB, matching /stats) so a
// client can judge how much headroom remains and whether draining one scope
// will fix the next retry.
func storeFull(w http.ResponseWriter, started time.Time, e *StoreFullError) {
	writeJSONWithDuration(w, http.StatusInsufficientStorage, map[string]interface{}{
		"ok":               false,
		"error":            "store is at byte capacity",
		"tracked_store_mb": MB(e.StoreBytes),
		"added_mb":         MB(e.AddedBytes),
		"max_store_mb":     MB(e.Cap),
	}, started)
}

func methodNotAllowed(w http.ResponseWriter, started time.Time) {
	writeJSONWithDuration(w, http.StatusMethodNotAllowed, map[string]interface{}{
		"ok":    false,
		"error": "the HTTP method is not allowed for this endpoint",
	}, started)
}

func (api *API) handleAppend(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodPost {
		methodNotAllowed(w, started)
		return
	}

	var item Item
	if err := decodeBody(w, r, MaxSingleRequestBytes, &item); err != nil {
		badRequest(w, started, err.Error())
		return
	}

	if err := validateWriteItem(item, "/append"); err != nil {
		badRequest(w, started, err.Error())
		return
	}

	buf, err := api.store.getOrCreateScope(item.Scope)
	if err != nil {
		badRequest(w, started, err.Error())
		return
	}

	origScope := item.Scope
	item, err = buf.appendItem(item)
	if err != nil {
		var sfe *ScopeFullError
		if errors.As(err, &sfe) {
			scopeFull(w, started, []ScopeCapacityOffender{
				{Scope: origScope, Count: sfe.Count, Cap: sfe.Cap},
			})
			return
		}
		var stfe *StoreFullError
		if errors.As(err, &stfe) {
			storeFull(w, started, stfe)
			return
		}
		conflict(w, started, err.Error())
		return
	}

	writeJSONWithDuration(w, http.StatusOK, map[string]interface{}{
		"ok":   true,
		"item": item,
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
		if err := validateWriteItem(req.Items[i], "/warm"); err != nil {
			badRequest(w, started, "invalid item at index "+strconv.Itoa(i)+": "+err.Error())
			return
		}
	}

	grouped := groupItemsByScope(req.Items)
	replacedScopes, err := api.store.replaceScopes(grouped)
	if err != nil {
		var sce *ScopeCapacityError
		if errors.As(err, &sce) {
			scopeFull(w, started, sce.Offenders)
			return
		}
		var stfe *StoreFullError
		if errors.As(err, &stfe) {
			storeFull(w, started, stfe)
			return
		}
		conflict(w, started, err.Error())
		return
	}

	writeJSONWithDuration(w, http.StatusOK, map[string]interface{}{
		"ok":              true,
		"count":           len(req.Items),
		"replaced_scopes": replacedScopes,
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
	// clients that really want to clear the cache should /delete-scope per
	// scope or restart the service.
	if len(req.Items) == 0 {
		badRequest(w, started, "the 'items' array must not be empty for the '/rebuild' endpoint")
		return
	}

	for i := range req.Items {
		if err := validateWriteItem(req.Items[i], "/rebuild"); err != nil {
			badRequest(w, started, "invalid item at index "+strconv.Itoa(i)+": "+err.Error())
			return
		}
	}

	grouped := groupItemsByScope(req.Items)
	rebuiltScopes, rebuiltItems, err := api.store.rebuildAll(grouped)
	if err != nil {
		var sce *ScopeCapacityError
		if errors.As(err, &sce) {
			scopeFull(w, started, sce.Offenders)
			return
		}
		var stfe *StoreFullError
		if errors.As(err, &stfe) {
			storeFull(w, started, stfe)
			return
		}
		conflict(w, started, err.Error())
		return
	}

	writeJSONWithDuration(w, http.StatusOK, map[string]interface{}{
		"ok":             true,
		"count":          len(req.Items),
		"rebuilt_scopes": rebuiltScopes,
		"rebuilt_items":  rebuiltItems,
	}, started)
}

func (api *API) handleUpdate(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodPost {
		methodNotAllowed(w, started)
		return
	}

	var item Item
	if err := decodeBody(w, r, MaxSingleRequestBytes, &item); err != nil {
		badRequest(w, started, err.Error())
		return
	}

	if err := validateUpdateItem(item); err != nil {
		badRequest(w, started, err.Error())
		return
	}

	buf, ok := api.store.getScope(item.Scope)
	if !ok {
		writeJSONWithDuration(w, http.StatusOK, map[string]interface{}{
			"ok":            true,
			"hit":           false,
			"updated_count": 0,
		}, started)
		return
	}

	var updated int
	var err error
	if item.ID != "" {
		updated, err = buf.updateByID(item.ID, item.Payload)
	} else {
		updated, err = buf.updateBySeq(item.Seq, item.Payload)
	}
	if err != nil {
		var stfe *StoreFullError
		if errors.As(err, &stfe) {
			storeFull(w, started, stfe)
			return
		}
		conflict(w, started, err.Error())
		return
	}

	writeJSONWithDuration(w, http.StatusOK, map[string]interface{}{
		"ok":            true,
		"hit":           updated > 0,
		"updated_count": updated,
	}, started)
}

func (api *API) handleDelete(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodPost {
		methodNotAllowed(w, started)
		return
	}

	var req DeleteRequest
	if err := decodeBody(w, r, MaxSingleRequestBytes, &req); err != nil {
		badRequest(w, started, err.Error())
		return
	}

	if err := validateDeleteRequest(req); err != nil {
		badRequest(w, started, err.Error())
		return
	}

	buf, ok := api.store.getScope(req.Scope)
	if !ok {
		writeJSONWithDuration(w, http.StatusOK, map[string]interface{}{
			"ok":            true,
			"hit":           false,
			"deleted_count": 0,
		}, started)
		return
	}

	var deleted int
	if req.ID != "" {
		deleted = buf.deleteByID(req.ID)
	} else {
		deleted = buf.deleteBySeq(req.Seq)
	}

	writeJSONWithDuration(w, http.StatusOK, map[string]interface{}{
		"ok":            true,
		"hit":           deleted > 0,
		"deleted_count": deleted,
	}, started)
}

func (api *API) handleDeleteUpTo(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodPost {
		methodNotAllowed(w, started)
		return
	}

	var req DeleteUpToRequest
	if err := decodeBody(w, r, MaxSingleRequestBytes, &req); err != nil {
		badRequest(w, started, err.Error())
		return
	}

	if err := validateDeleteUpToRequest(req); err != nil {
		badRequest(w, started, err.Error())
		return
	}

	buf, ok := api.store.getScope(req.Scope)
	if !ok {
		writeJSONWithDuration(w, http.StatusOK, map[string]interface{}{
			"ok":            true,
			"hit":           false,
			"deleted_count": 0,
		}, started)
		return
	}

	deleted := buf.deleteUpToSeq(req.MaxSeq)

	writeJSONWithDuration(w, http.StatusOK, map[string]interface{}{
		"ok":            true,
		"hit":           deleted > 0,
		"deleted_count": deleted,
	}, started)
}

func (api *API) handleDeleteScope(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodPost {
		methodNotAllowed(w, started)
		return
	}

	var req DeleteScopeRequest
	if err := decodeBody(w, r, MaxSingleRequestBytes, &req); err != nil {
		badRequest(w, started, err.Error())
		return
	}

	if err := validateDeleteScopeRequest(req); err != nil {
		badRequest(w, started, err.Error())
		return
	}

	deletedItems, deleted := api.store.deleteScope(req.Scope)

	writeJSONWithDuration(w, http.StatusOK, map[string]interface{}{
		"ok":            true,
		"hit":           deleted,
		"deleted_scope": deleted,
		"deleted_items": deletedItems,
	}, started)
}

func (api *API) handleHead(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodGet {
		methodNotAllowed(w, started)
		return
	}

	query := r.URL.Query()
	scope := query.Get("scope")
	limit, err := normalizeLimit(query.Get("limit"))

	if scopeErr := validateScope(scope, "/head"); scopeErr != nil {
		badRequest(w, started, scopeErr.Error())
		return
	}
	if err != nil {
		badRequest(w, started, err.Error())
		return
	}
	// /head reads forward by cursor only. Positional 'offset' addressing
	// lives on /tail exclusively because seq-based forward reads are stable
	// under /delete-up-to while position-based forward reads are not.
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

	buf, ok := api.store.getScope(scope)
	if !ok {
		writeJSONWithDuration(w, http.StatusOK, map[string]interface{}{
			"ok":    true,
			"hit":   false,
			"count": 0,
			"items": []Item{},
		}, started)
		return
	}

	items := buf.sinceSeq(afterSeq, limit)
	// Only a non-empty result counts toward read-heat. An empty window is
	// effectively a miss and should not skew eviction.
	if len(items) > 0 {
		buf.recordRead(nowUnixMicro())
	}

	writeJSONWithDuration(w, http.StatusOK, map[string]interface{}{
		"ok":    true,
		"hit":   len(items) > 0,
		"count": len(items),
		"items": items,
	}, started)
}

func (api *API) handleTail(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodGet {
		methodNotAllowed(w, started)
		return
	}

	scope := r.URL.Query().Get("scope")
	limit, err := normalizeLimit(r.URL.Query().Get("limit"))
	offset, offsetErr := normalizeOffset(r.URL.Query().Get("offset"))

	if scopeErr := validateScope(scope, "/tail"); scopeErr != nil {
		badRequest(w, started, scopeErr.Error())
		return
	}
	if err != nil {
		badRequest(w, started, err.Error())
		return
	}
	if offsetErr != nil {
		badRequest(w, started, offsetErr.Error())
		return
	}

	buf, ok := api.store.getScope(scope)
	if !ok {
		writeJSONWithDuration(w, http.StatusOK, map[string]interface{}{
			"ok":     true,
			"hit":    false,
			"count":  0,
			"offset": offset,
			"items":  []Item{},
		}, started)
		return
	}

	items := buf.tailOffset(limit, offset)
	if len(items) > 0 {
		buf.recordRead(nowUnixMicro())
	}

	writeJSONWithDuration(w, http.StatusOK, map[string]interface{}{
		"ok":     true,
		"hit":    len(items) > 0,
		"count":  len(items),
		"offset": offset,
		"items":  items,
	}, started)
}

func (api *API) handleGet(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodGet {
		methodNotAllowed(w, started)
		return
	}

	scope := r.URL.Query().Get("scope")
	id := r.URL.Query().Get("id")
	seqStr := r.URL.Query().Get("seq")

	if scopeErr := validateScope(scope, "/get"); scopeErr != nil {
		badRequest(w, started, scopeErr.Error())
		return
	}

	hasID := id != ""
	hasSeq := seqStr != ""

	if hasID == hasSeq {
		badRequest(w, started, "exactly one of 'id' or 'seq' must be provided")
		return
	}

	if hasID {
		if idErr := validateID(id); idErr != nil {
			badRequest(w, started, idErr.Error())
			return
		}
	}

	buf, ok := api.store.getScope(scope)
	if !ok {
		writeJSONWithDuration(w, http.StatusOK, map[string]interface{}{
			"ok":   true,
			"hit":  false,
			"item": nil,
		}, started)
		return
	}

	if hasID {
		item, found := buf.getByID(id)
		if !found {
			writeJSONWithDuration(w, http.StatusOK, map[string]interface{}{
				"ok":   true,
				"hit":  false,
				"item": nil,
			}, started)
			return
		}

		// Only a hit counts toward scope read-heat. A miss by explicit id/seq
		// should not inflate last_7d_read_count, since the signal is surfaced
		// on /delete-scope-candidates for client-side eviction decisions.
		buf.recordRead(nowUnixMicro())

		writeJSONWithDuration(w, http.StatusOK, map[string]interface{}{
			"ok":   true,
			"hit":  true,
			"item": item,
		}, started)
		return
	}

	seq, err := strconv.ParseUint(seqStr, 10, 64)
	if err != nil {
		badRequest(w, started, "the 'seq' parameter must be a valid unsigned integer")
		return
	}

	item, found := buf.getBySeq(seq)
	if !found {
		writeJSONWithDuration(w, http.StatusOK, map[string]interface{}{
			"ok":   true,
			"hit":  false,
			"item": nil,
		}, started)
		return
	}

	buf.recordRead(nowUnixMicro())

	writeJSONWithDuration(w, http.StatusOK, map[string]interface{}{
		"ok":   true,
		"hit":  true,
		"item": item,
	}, started)
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
		stats := buf.stats()

		createdTS := stats["created_ts"].(int64)
		if hours > 0 && now-createdTS < minAgeMicros {
			continue
		}

		list = append(list, Candidate{
			Scope:           name,
			CreatedTS:       createdTS,
			LastAccessTS:    stats["last_access_ts"].(int64),
			Last7dReadCount: stats["last_7d_read_count"].(uint64),
			ItemCount:       stats["item_count"].(int),
			ApproxScopeMB:   stats["approx_scope_mb"].(MB),
		})
	}

	sort.Slice(list, func(i, j int) bool {
		return list[i].LastAccessTS < list[j].LastAccessTS
	})

	if len(list) > limit {
		list = list[:limit]
	}

	writeJSONWithDuration(w, http.StatusOK, map[string]interface{}{
		"ok":         true,
		"count":      len(list),
		"hours":      hours,
		"candidates": list,
	}, started)
}

func (api *API) handleStats(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodGet {
		methodNotAllowed(w, started)
		return
	}

	stats := api.store.stats()
	writeJSONWithDuration(w, http.StatusOK, stats, started)
}

func (api *API) handleHelp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = w.Write([]byte("method not allowed\n"))
		return
	}

	helpText := `inmem-cache v2

RULES:
- payload must be a valid JSON value (object, array, string, number, bool); its contents are opaque to the cache — never inspected or searched
- payload must be present on writes; literal null is treated as missing
- per-item size cap is 1 MiB (measured against the raw JSON bytes of payload plus scope/id overhead)
- scope and id must be <= 128 bytes, with no surrounding whitespace and no control characters
- filtering only operates on scope, id and seq
- read endpoints use a default limit of 1,000 when ?limit is omitted, and a maximum of 10,000 (higher values are clamped, not rejected)
- id is optional
- if id is present, it must be unique within its scope
- write operations reject duplicates for the same scope + id
- per-scope capacity is 100,000 items by default (override with INMEM_SCOPE_MAX_ITEMS); writes that would exceed the cap are rejected with 507 Insufficient Storage — nothing is silently evicted
- /append past the cap returns 507 with the offending scope. /warm and /rebuild reject the entire batch with the full list of over-cap scopes; make room first with /delete-up-to or /delete-scope
- store-wide byte cap is 100 MiB by default (override with INMEM_MAX_STORE_MB, integer MiB); writes that would push the aggregate approxItemSize past it are rejected with 507. The response carries tracked_store_mb, added_mb, and max_store_mb; free room with /delete-scope or /delete-up-to
- per-request body cap for /warm and /rebuild scales with the store cap (~store + 10% + 16 MiB), so a full cache always fits in one bulk request. Single-item endpoints have a fixed 2 MiB body cap.
- every byte-ish field in JSON responses (tracked_store_mb, max_store_mb, approx_scope_mb, added_mb) is expressed in MiB with 4 decimals — one unit across /stats, /delete-scope-candidates and 507 responses
- the listening socket path defaults to /run/inmem.sock on Linux and $TMPDIR/inmem.sock on macOS/Windows; override with INMEM_SOCKET_PATH

ENDPOINTS:
GET  /help - show this help text
POST /append - append one item to a scope
POST /warm - warm or refresh one or more scopes
POST /rebuild - rebuild the entire cache
POST /update - update one item by scope + id or scope + seq (exactly one of id/seq required)
POST /delete - delete one item by scope + id or scope + seq (exactly one of id/seq required)
POST /delete-up-to - delete every item in a scope with seq <= max_seq
POST /delete-scope - delete one entire scope from the cache
GET  /head - get the oldest items from a scope; supports optional after_seq for cursor-based forward reads (offset is not supported, use /tail for position-based paging)
GET  /tail - get the most recent items from a scope (supports optional offset)
GET  /get - get one item by scope + id or scope + seq
GET  /stats - show store stats and approximate store size
GET  /delete-scope-candidates - list scope eviction candidates, sorted by oldest last_access_ts (response includes last_7d_read_count for client-side filtering/sorting)

NOTES:
- /warm replaces only the scopes present in the request
- /rebuild replaces the entire store
- /delete-up-to is designed for write-buffer patterns: read with /head?after_seq=…, commit to the DB, then trim with /delete-up-to up to the last committed seq
- /delete-scope removes all items, indexes and scope-level metadata for one scope
- /delete-scope-candidates is advisory only: returns candidates, never deletes; the client decides
- /delete-scope-candidates supports optional ?hours=N to exclude recently created scopes
`

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(helpText))
}

func (api *API) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/append", api.handleAppend)
	mux.HandleFunc("/warm", api.handleWarm)
	mux.HandleFunc("/rebuild", api.handleRebuild)
	mux.HandleFunc("/update", api.handleUpdate)
	mux.HandleFunc("/delete", api.handleDelete)
	mux.HandleFunc("/delete-up-to", api.handleDeleteUpTo)
	mux.HandleFunc("/delete-scope", api.handleDeleteScope)
	mux.HandleFunc("/head", api.handleHead)
	mux.HandleFunc("/tail", api.handleTail)
	mux.HandleFunc("/get", api.handleGet)
	mux.HandleFunc("/stats", api.handleStats)
	mux.HandleFunc("/help", api.handleHelp)
	mux.HandleFunc("/delete-scope-candidates", api.handleDeleteScopeCandidates)
}
