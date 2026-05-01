package scopecache

import (
	"net/http"
	"sort"
	"time"
)

// Observability + meta handlers:
//
//   - /stats                      — store-wide snapshot (admin-only)
//   - /delete_scope_candidates    — heat-ranked eviction hints (admin-only)
//   - /help                       — text/plain rules and endpoint reference
//
// /stats and /delete_scope_candidates are admin-only because they
// enumerate every scope name in the store, which in a multi-tenant
// deployment leaks `_tokens`, `_guarded:<capID>:*`, `_counters_*` and
// the per-scope item-counts/heat-stats those carry. Reachable only as
// sub-calls through /admin's dispatcher.
//
// /help is the only handler in the file that returns text/plain rather
// than JSON — it is documentation the cache hands out about itself, not
// observability data.

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
