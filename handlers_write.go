package scopecache

import (
	"errors"
	"net/http"
	"time"
)

// Single-item write handlers on the public mux:
//
//   - /append       — insert; rejects on dup id, capacity, or byte cap
//   - /upsert       — insert-or-replace by id; replace-whole-item semantics
//   - /update       — modify payload (and optional ts) at existing id/seq
//   - /counter_add  — atomic int64 add on existing id; auto-creates on miss
//
// All four decode an Item body, run shape validation, reject reserved
// scope prefixes for non-admin callers, route through the matching
// Store method (appendOne / upsertOne / counterAddOne / updateOne),
// and map *ScopeFullError / *ScopeCapacityError / *StoreFullError
// uniformly via writeStoreCapacityError.

// writeAck is the response shape /append and /upsert nest under "item".
// Mirrors Item's json layout for scope/id/seq/ts so multi_call slots
// remain self-correlating ("which sub-call succeeded into which scope?")
// without forcing the client to map results[i] back onto calls[i].
// Deliberately excludes Payload — the client supplied it on the way in,
// and echoing it would double the wire cost on a 1 MiB write. The
// struct's omitempty rules match Item's so a write without an id still
// produces the same response shape as Item-marshaled-without-Payload
// would.
type writeAck struct {
	Scope string `json:"scope,omitempty"`
	ID    string `json:"id,omitempty"`
	Seq   uint64 `json:"seq,omitempty"`
	Ts    int64  `json:"ts"`
}

func (api *API) handleAppend(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodPost {
		methodNotAllowed(w, started)
		return
	}

	var item Item
	if err := decodeBody(w, r, api.maxSingleBytes, &item); err != nil {
		badRequest(w, started, err.Error())
		return
	}

	if err := validateWriteItem(item, "/append", api.store.maxItemBytes); err != nil {
		badRequest(w, started, err.Error())
		return
	}
	if rejectReservedScope(r, w, started, item.Scope) {
		return
	}

	origScope := item.Scope
	item, err := api.store.appendOne(item)
	if err != nil {
		if writeStoreCapacityError(w, started, err, origScope) {
			return
		}
		conflict(w, started, err.Error())
		return
	}

	// Response nests scope/id/seq/ts under "item" so multi_call slots
	// stay self-correlating without the caller mapping results[i] onto
	// calls[i]. Payload is the only echoed field that's worth dropping —
	// scope/id are tiny strings (<= 256 B each) but a payload echo on a
	// 1 MiB write would double the wire cost.
	writeJSONWithDuration(w, http.StatusOK, orderedFields{
		{"ok", true},
		{"item", writeAck{Scope: item.Scope, ID: item.ID, Seq: item.Seq, Ts: item.Ts}},
	}, started)
}

// handleUpsert creates a new item or replaces an existing one by scope + id.
// Unlike /append (which rejects duplicate ids) or /update (which soft-misses
// on absent items), /upsert always writes — making it the idempotent, retry-
// safe write path. Seq is preserved on replace and freshly assigned on create.
func (api *API) handleUpsert(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodPost {
		methodNotAllowed(w, started)
		return
	}

	var item Item
	if err := decodeBody(w, r, api.maxSingleBytes, &item); err != nil {
		badRequest(w, started, err.Error())
		return
	}

	if err := validateUpsertItem(item, api.store.maxItemBytes); err != nil {
		badRequest(w, started, err.Error())
		return
	}
	if rejectReservedScope(r, w, started, item.Scope) {
		return
	}

	origScope := item.Scope
	result, created, err := api.store.upsertOne(item)
	if err != nil {
		if writeStoreCapacityError(w, started, err, origScope) {
			return
		}
		conflict(w, started, err.Error())
		return
	}

	// Same item-with-no-payload shape as /append; see comment there. Seq
	// is the pre-existing seq on a replace and the freshly-assigned seq
	// on a create.
	writeJSONWithDuration(w, http.StatusOK, orderedFields{
		{"ok", true},
		{"created", created},
		{"item", writeAck{Scope: result.Scope, ID: result.ID, Seq: result.Seq, Ts: result.Ts}},
	}, started)
}

// handleCounterAdd atomically increments (or creates) a numeric counter at
// scope+id by `by`. It is the only endpoint that reads or mutates a payload
// as a typed value — every other write path treats payloads as opaque bytes.
// Creates pay a fresh approxItemSize reservation; replaces pay only the byte
// delta of the new integer representation.
func (api *API) handleCounterAdd(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodPost {
		methodNotAllowed(w, started)
		return
	}

	var req CounterAddRequest
	if err := decodeBody(w, r, api.maxSingleBytes, &req); err != nil {
		badRequest(w, started, err.Error())
		return
	}

	by, err := validateCounterAddRequest(req)
	if err != nil {
		badRequest(w, started, err.Error())
		return
	}
	if rejectReservedScope(r, w, started, req.Scope) {
		return
	}

	origScope := req.Scope
	value, created, err := api.store.counterAddOne(req.Scope, req.ID, by)
	if err != nil {
		// Common capacity-class errors first (sfe + stfe). Counter-
		// specific errors (cpe → 409, coe → 400) are handled inline
		// below — they do not fit the helper because cpe maps to
		// `conflict` and coe maps to `badRequest`, not to the
		// scope/store-full responders.
		if writeStoreCapacityError(w, started, err, origScope) {
			return
		}
		var cpe *CounterPayloadError
		if errors.As(err, &cpe) {
			conflict(w, started, cpe.Error())
			return
		}
		var coe *CounterOverflowError
		if errors.As(err, &coe) {
			badRequest(w, started, coe.Error())
			return
		}
		conflict(w, started, err.Error())
		return
	}

	writeJSONWithDuration(w, http.StatusOK, orderedFields{
		{"ok", true},
		{"created", created},
		{"value", value},
	}, started)
}

func (api *API) handleUpdate(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodPost {
		methodNotAllowed(w, started)
		return
	}

	var item Item
	if err := decodeBody(w, r, api.maxSingleBytes, &item); err != nil {
		badRequest(w, started, err.Error())
		return
	}

	if err := validateUpdateItem(item, api.store.maxItemBytes); err != nil {
		badRequest(w, started, err.Error())
		return
	}
	if rejectReservedScope(r, w, started, item.Scope) {
		return
	}

	updated, err := api.store.updateOne(item)
	if err != nil {
		// /update only ever sees *StoreFullError on the cap path
		// (existing-item replace can grow byte size); scopeForSFE is
		// unused.
		if writeStoreCapacityError(w, started, err, "") {
			return
		}
		conflict(w, started, err.Error())
		return
	}

	writeJSONWithDuration(w, http.StatusOK, orderedFields{
		{"ok", true},
		{"hit", updated > 0},
		{"updated_count", updated},
	}, started)
}
