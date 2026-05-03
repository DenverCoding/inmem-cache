package scopecache

import (
	"net/http"
	"time"
)

// Observability + meta handlers:
//
//   - /stats — store-wide snapshot
//   - /help  — text/plain pointer to the canonical RFC
//
// /help is the only handler in the file that returns text/plain rather
// than JSON — it is documentation the cache hands out about itself, not
// observability data.
//
// /stats enumerates every scope name in the store. In multi-tenant
// deployments where addons keep state in `_*` scopes, `/stats` will
// surface those names; the operator gates `/stats` at the transport
// layer when that matters. The cache itself draws no line.

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
