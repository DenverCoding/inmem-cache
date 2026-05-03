package scopecache

import (
	"net/http"
	"time"
)

// Observability + meta handlers:
//
//   - /stats — store-wide aggregate snapshot
//   - /help  — text/plain pointer to the canonical RFC
//
// /help is the only handler in the file that returns text/plain rather
// than JSON — it is documentation the cache hands out about itself, not
// observability data.
//
// /stats is intentionally aggregate-only: scope_count, total_items and
// approx_store_mb. The previous shape included a per-scope map keyed
// by scope name; at 100k+ scopes that response routinely blew past
// practical client and proxy limits, and the per-scope enumeration
// dominated /stats latency. Per-scope listing moves to a separate
// paginated /scopelist endpoint (see Phase A roadmap).

func (api *API) handleStats(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodGet {
		methodNotAllowed(w, started)
		return
	}

	st := api.store.stats()

	// /stats is a pure state endpoint: aggregate scope/item counts,
	// current byte usage, and a freshness tick. Static config
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
	helpText := "scopecache — see instructions at https://github.com/VeloxCoding/scopecache/blob/main/docs/scopecache-core-rfc.md\n"

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(helpText))
}
