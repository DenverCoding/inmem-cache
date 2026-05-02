package scopecache

import (
	"net/http"
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

	list := api.store.scopeCandidates(hours, limit)

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
			{"last_write_ts", sc.LastWriteTS},
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

	// Placeholder until the dedicated /help finetune pass closer to v1.0;
	// keeping a stale long-form here would just drift out of sync with
	// the RFC. One-line pointer is the lowest-maintenance shape.
	helpText := "scopecache — see instructions at https://github.com/VeloxCoding/scopecache/blob/main/scopecache-rfc.md\n"

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(helpText))
}
