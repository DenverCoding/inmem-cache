// Observability + meta handlers:
//
//   - /stats     — store-wide aggregate snapshot
//   - /scopelist — per-scope detail with prefix filter and cursor pagination
//   - /help      — text/plain pointer to the canonical RFC
//
// /help is the only handler in the file that returns text/plain rather
// than JSON — it is documentation the cache hands out about itself, not
// observability data.
//
// /stats is aggregate-only for user-managed scopes — at 100k+ scopes
// the per-scope enumeration dominated /stats latency and the response
// routinely blew past practical client/proxy limits. Per-scope detail
// for user scopes lives at /scopelist, which pages it via a stable
// alphabetical cursor.
//
// reserved_scopes is the small, fixed exception: `_events` and
// `_inbox` are cache infrastructure (drainer-backlog and fan-in queue
// depth) that operators monitor independently of user scopes. The set
// is bounded by the reserved-scope list (currently 2 entries), so
// /stats stays O(1) regardless of total scope count.

package scopecache

import (
	"net/http"
	"time"
)

func (api *API) handleStats(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodGet {
		methodNotAllowed(w, started)
		return
	}

	st := api.store.stats()

	// /stats is a pure state endpoint: aggregate scope/item counts,
	// current byte usage, a freshness tick, and the per-scope state
	// of the cache's reserved infrastructure scopes. Static config
	// (DefaultLimit, MaxLimit, per-scope/per-item/store caps) lives
	// on /help, not here — those values do not change between calls
	// and re-emitting them on every poll is pure noise. last_write_ts
	// lets a polling client decide "anything changed since I last
	// looked?" with a single integer comparison instead of refetching
	// state. duration_us is appended by the helper.
	writeJSONWithDuration(w, http.StatusOK, orderedFields{
		{"ok", true},
		{"scope_count", st.ScopeCount},
		{"total_items", st.TotalItems},
		{"approx_store_mb", st.ApproxStoreMB},
		{"last_write_ts", st.LastWriteTS},
		{"events_drops_total", st.EventsDropsTotal},
		{"reserved_scopes", st.ReservedScopes},
	}, started)
}

// handleScopeList serves /scopelist — the per-scope counterpart of /stats.
// /stats is store-wide aggregate (O(1)); /scopelist is per-scope detail with
// alphabetical sort and cursor pagination, so even a 100k-scope store can be
// walked in fixed-size pages without re-introducing the per-scope DoS that
// got the old /stats shape stripped.
//
// Query parameters:
//   - prefix : optional, literal strings.HasPrefix filter on scope name
//     (per-tenant footprint visibility without fetch-and-filter on the client)
//   - after  : optional cursor; returns scopes with name > after (strict)
//   - limit  : page size; defaults to DefaultLimit, clamped to MaxLimit
//
// Sort order is alphabetical, the only mode shipped: scope names don't move
// once created, so the cursor stays stable under concurrent writes. The
// next-page cursor is just the last `scope` field of the response — no
// dedicated next_cursor field, since the client already has it.
//
// Read-bookkeeping (§8) is NOT bumped on /scopelist hits: it is observability,
// not a content read, and would otherwise corrupt eviction-candidate signals
// that addons compute from read_count_total deltas.
func (api *API) handleScopeList(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodGet {
		methodNotAllowed(w, started)
		return
	}

	query := r.URL.Query()
	prefix := query.Get("prefix")
	after := query.Get("after")

	// Reuse the scope shape rules for both filter inputs: same byte cap,
	// same control-character ban, same whitespace check. Empty values are
	// legitimate (no filter, start from beginning) and skip validation.
	if prefix != "" {
		if err := checkKeyField("prefix", prefix, MaxScopeBytes); err != nil {
			badRequest(w, started, err.Error())
			return
		}
	}
	if after != "" {
		if err := checkKeyField("after", after, MaxScopeBytes); err != nil {
			badRequest(w, started, err.Error())
			return
		}
	}

	limit, err := normalizeLimit(query.Get("limit"))
	if err != nil {
		badRequest(w, started, err.Error())
		return
	}

	entries, truncated := api.store.scopeList(prefix, after, limit)

	// Cap-aware writer: a max-limit response with long scope names can
	// approach api.maxResponseBytes; same shape as /head and /tail.
	// `hit` is `count > 0` — same semantic as on /head/tail (which also
	// derive it from len(items)>0), kept here so the list-return read
	// family (`/head`, `/tail`, `/scopelist`) shares one wire prefix:
	// `ok, hit, count, truncated, <list>`.
	writeJSONWithMetaCap(w, http.StatusOK, orderedFields{
		{"ok", true},
		{"hit", len(entries) > 0},
		{"count", len(entries)},
		{"truncated", truncated},
		{"scopes", entries},
	}, started, api.maxResponseBytes)
}

func (api *API) handleHelp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = w.Write([]byte("method not allowed\n"))
		return
	}

	// Placeholder until the dedicated /help finetune pass closer to v1.0;
	// keeping a stale long-form here would just drift out of sync with
	// the RFC. One-line pointer is the lowest-maintenance shape.
	helpText := "scopecache — see instructions at https://github.com/VeloxCoding/scopecache/blob/main/docs/scopecache-core-rfc.md\n"

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(helpText))
}
