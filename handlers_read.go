package scopecache

import (
	"net/http"
	"strconv"
	"time"
)

// Read handlers on the public mux:
//
//   - /head      — oldest-first window, optional after_seq cursor
//   - /tail      — newest-first window, optional offset
//   - /get       — single-item lookup by id or seq, JSON envelope
//   - /render    — single-item raw payload, no JSON envelope
//
// /head and /tail share writeItemsHit / writeItemsMiss so the wire
// shape (`ok`, `hit`, `count`, `truncated`, `items`,
// `approx_response_mb`, `duration_us`) stays uniform across the
// list-returning read family. The shared writers also enforce two
// invariants that previously sat parallel in the handler bodies:
// (1) recordRead only on non-empty results, (2) per-response cap
// check before marshal-and-write.

// writeItemsHit assembles and writes the success response for a
// list-returning read endpoint (/head, /tail). The Store-layer read
// methods (Store.head, Store.tail) own the read-heat stamping; this
// helper is purely about HTTP response shape and the per-response
// byte cap.
//
// estimateMultiItemResponseBytes MUST run before writeJSONWithMeta:
// once the response body has been written there is no way to switch
// to a 507 without leaving a half-flushed body on the wire — the cap
// check is a one-shot opportunity per request.
//
// `extra` slots between `count` and `truncated` so /tail can carry
// its `offset` field at the right wire position; /head passes nil.
// Field order is load-bearing: matches the existing wire shape
// exactly. Do not reorder.
func (api *API) writeItemsHit(
	w http.ResponseWriter,
	started time.Time,
	items []Item,
	truncated bool,
	extra orderedFields,
) {
	if len(items) > 0 {
		if estimated := estimateMultiItemResponseBytes(items); estimated > api.maxResponseBytes {
			responseTooLarge(w, started, estimated, api.maxResponseBytes)
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
	writeJSONWithMetaCap(w, http.StatusOK, fields, started, api.maxResponseBytes)
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
	writeJSONWithMetaCap(w, http.StatusOK, fields, started, api.maxResponseBytes)
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

	items, truncated, found := api.store.head(q.Scope, afterSeq, q.Limit, !api.disableReadHeat)
	if !found {
		api.writeItemsMiss(w, started, nil)
		return
	}
	api.writeItemsHit(w, started, items, truncated, nil)
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

	items, truncated, found := api.store.tail(q.Scope, q.Limit, offset, !api.disableReadHeat)
	if !found {
		api.writeItemsMiss(w, started, offsetField)
		return
	}
	api.writeItemsHit(w, started, items, truncated, offsetField)
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

	var (
		item  Item
		found bool
	)
	if target.ByID {
		item, found = api.store.get(target.Scope, target.ID, 0, !api.disableReadHeat)
	} else {
		item, found = api.store.get(target.Scope, "", target.Seq, !api.disableReadHeat)
	}
	if !found {
		miss()
		return
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

	var (
		body  []byte
		found bool
	)
	if target.ByID {
		body, found = api.store.render(target.Scope, target.ID, 0, !api.disableReadHeat)
	} else {
		body, found = api.store.render(target.Scope, "", target.Seq, !api.disableReadHeat)
	}
	if !found {
		writeMiss()
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
