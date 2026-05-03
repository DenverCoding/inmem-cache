package scopecache

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"
)

// handlers.go is the shared HTTP-layer infrastructure for *API:
//
//   - error-class mapping (writeStoreCapacityError + the 4xx/5xx
//     responder family: badRequest, conflict, scopeFull, storeFull,
//     methodNotAllowed)
//   - body decoding (decodeBody)
//   - response shaping (orderedFields type + writeJSONWithDuration /
//     writeJSONWithMeta / writeJSONWithMetaCap / marshalWithApproxSize)
//   - common request parsers (parseLookupTarget, parseScopeLimit)
//   - mux registration (RegisterRoutes)
//
// Per-endpoint families live in handlers_*.go siblings:
//
//   handlers_write.go    — /append, /upsert, /update, /counter_add
//   handlers_read.go     — /head, /tail, /get, /render
//   handlers_delete.go   — /delete, /delete_up_to, /delete_scope, /wipe
//   handlers_bulk.go     — /warm, /rebuild
//   handlers_observe.go  — /stats, /help

// writeStoreCapacityError centralises the per-handler error-handling
// for the three capacity-class errors the store can return on a write
// path: *ScopeFullError (single-item over per-scope cap), the bulk
// equivalent *ScopeCapacityError (carries an offender list), and
// *StoreFullError (over the store-wide byte cap). All seven write
// handlers (/append, /upsert, /counter_add, /inbox single-item +
// /warm, /rebuild bulk + /update which only sees stfe) call this
// before doing any handler-specific error dispatch.
//
// Returns true when one of the three was matched and the response
// has been written — the caller should `return` immediately. Returns
// false otherwise; the caller falls back to its own error handling
// (typically `conflict(...)`, plus counter-specific errors for
// /counter_add).
//
// `scopeForSFE` is the scope name plumbed into the single-element
// offenders list on the *ScopeFullError path. It is **unused** for
// callers that cannot produce sfe — /warm and /rebuild produce
// *ScopeCapacityError (which carries its own offender list) and
// /update produces only *StoreFullError. Those callers pass "".
// The unused-param wart is preferable to splitting into two helpers
// that would duplicate the stfe block (the most likely candidate
// for future drift).
func writeStoreCapacityError(w http.ResponseWriter, started time.Time, err error, scopeForSFE string) bool {
	var sfe *ScopeFullError
	if errors.As(err, &sfe) {
		scopeFull(w, started, []ScopeCapacityOffender{
			{Scope: scopeForSFE, Count: sfe.Count, Cap: sfe.Cap},
		})
		return true
	}
	var sce *ScopeCapacityError
	if errors.As(err, &sce) {
		scopeFull(w, started, sce.Offenders)
		return true
	}
	var stfe *StoreFullError
	if errors.As(err, &stfe) {
		storeFull(w, started, stfe)
		return true
	}
	return false
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

// orderedFields is a JSON object whose keys are emitted in insertion order.
// encoding/json sorts map keys alphabetically, which scatters ok, errors,
// counts, and payloads through the output in whichever order the alphabet
// dictates. orderedFields lets every response put ok first, config/caps
// before aggregates, heavy or variable-size fields last, and duration_us
// at the very end — a shape a human eye (and a log scanner) can read at
// a glance.
type orderedFields []kv

type kv struct {
	K string
	V interface{}
}

func (o orderedFields) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, f := range o {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, err := json.Marshal(f.K)
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteByte(':')
		val, err := json.Marshal(f.V)
		if err != nil {
			return nil, err
		}
		buf.Write(val)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func writeJSONWithDuration(w http.ResponseWriter, code int, payload orderedFields, started time.Time) {
	payload = append(payload, kv{"duration_us", time.Since(started).Microseconds()})
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(payload)
}

// marshalWithApproxSize is the shared splice helper used by
// writeJSONWithMeta and writeJSONWithMetaCap. It marshals payload,
// then appends a self-referential approx_response_mb field that
// reports the body's own byte length back to the client. Returns
// the spliced bytes plus the duration_us-augmented payload (the
// caller may need it for a fallback path on marshal failure).
//
// Single-marshal + patch: marshal the body once, then splice in the
// size field just before the closing '}'. Self-referential — the
// size includes the field's own bytes — but converges in 1-2
// iterations because MB has 4-decimal precision (0.0001 MiB ≈ 104
// bytes) and the patch only adds ~30 bytes total. Cost over
// writeJSONWithDuration is one extra json.Marshal of the MB value
// plus a few slice appends — well under 100 µs even for multi-MiB
// responses.
func marshalWithApproxSize(payload orderedFields, started time.Time) ([]byte, orderedFields, error) {
	payload = append(payload, kv{"duration_us", time.Since(started).Microseconds()})
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, payload, err
	}

	// bodyBytes ends in '}'. Strip it, append `,"approx_response_mb":N.NNNN}`.
	// Iterate so the reported size includes the bytes we are about to add.
	const fieldKey = `,"approx_response_mb":`
	finalSize := len(bodyBytes) + len(fieldKey) + 8 // initial guess: 8-byte value
	var valueBytes []byte
	for i := 0; i < 3; i++ {
		v, mErr := json.Marshal(MB(finalSize))
		if mErr != nil {
			break
		}
		valueBytes = v
		candidate := len(bodyBytes) - 1 + len(fieldKey) + len(valueBytes) + 1
		if candidate == finalSize {
			break
		}
		finalSize = candidate
	}

	out := make([]byte, 0, finalSize+1)
	out = append(out, bodyBytes[:len(bodyBytes)-1]...)
	out = append(out, fieldKey...)
	out = append(out, valueBytes...)
	out = append(out, '}', '\n')

	return out, payload, nil
}

// writeJSONWithMeta is writeJSONWithDuration plus an approx_response_mb
// field. Used on read-item endpoints whose response size is bounded by
// the operation (e.g. /get is single-item). For limit-scaled endpoints
// (/head, /tail) use writeJSONWithMetaCap instead, which
// rejects oversized bodies up-front.
func writeJSONWithMeta(w http.ResponseWriter, code int, payload orderedFields, started time.Time) {
	out, augmented, err := marshalWithApproxSize(payload, started)
	if err != nil {
		// orderedFields encoding cannot fail in practice (we control every
		// value type); fall through to the simpler writer if it ever does.
		writeJSONWithDuration(w, code, augmented[:len(augmented)-1], started)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write(out)
}

// writeJSONWithMetaCap is writeJSONWithMeta with a per-response byte
// cap baked in. Used on /head, /tail — endpoints whose
// response can grow with limit × per-item-cap. Marshals the body
// once, checks against maxBytes, and either emits the response or
// replaces it with a 507 envelope. Replaces the older capResponse
// middleware that buffered the handler's whole output a second time.
func writeJSONWithMetaCap(w http.ResponseWriter, code int, payload orderedFields, started time.Time, maxBytes int64) {
	out, augmented, err := marshalWithApproxSize(payload, started)
	if err != nil {
		writeJSONWithDuration(w, code, augmented[:len(augmented)-1], started)
		return
	}

	if int64(len(out)) > maxBytes {
		responseTooLarge(w, started, int64(len(out)), maxBytes)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write(out)
}

func badRequest(w http.ResponseWriter, started time.Time, message string) {
	writeJSONWithDuration(w, http.StatusBadRequest, orderedFields{
		{"ok", false},
		{"error", message},
	}, started)
}

func conflict(w http.ResponseWriter, started time.Time, message string) {
	writeJSONWithDuration(w, http.StatusConflict, orderedFields{
		{"ok", false},
		{"error", message},
	}, started)
}

// scopeFull responds with 507 Insufficient Storage and the full offender list.
// Used when an /append, /warm, or /rebuild would push one or more scopes past
// the per-scope capacity. The client is expected to drain (e.g. /delete_up_to
// or /delete_scope) or chunk the batch and retry.
func scopeFull(w http.ResponseWriter, started time.Time, offenders []ScopeCapacityOffender) {
	msg := "scope is at capacity"
	if len(offenders) > 1 {
		msg = "multiple scopes are at capacity"
	}
	writeJSONWithDuration(w, http.StatusInsufficientStorage, orderedFields{
		{"ok", false},
		{"error", msg},
		{"scopes", offenders},
	}, started)
}

// storeFull responds with 507 when the aggregate byte cap would be exceeded.
// The body carries the store-level totals (all in MiB, matching /stats) so a
// client can judge how much headroom remains and whether draining one scope
// will fix the next retry.
func storeFull(w http.ResponseWriter, started time.Time, e *StoreFullError) {
	writeJSONWithDuration(w, http.StatusInsufficientStorage, orderedFields{
		{"ok", false},
		{"error", "store is at byte capacity"},
		{"approx_store_mb", MB(e.StoreBytes)},
		{"added_mb", MB(e.AddedBytes)},
		{"max_store_mb", MB(e.Cap)},
	}, started)
}

// responseTooLarge writes the 507 envelope used by the cap-protected
// read endpoints (/head, /tail) when the marshalled body would exceed
// the per-response cap. Body shape mirrors the other 507 helpers
// (storeFull, scopeFull): {ok, error, approx_response_mb,
// max_response_mb, duration_us}.
//
// Side effects already applied by the handler are NOT rolled back. This
// matches every other 507 in the cache: 2xx is not durability, and 507
// does not roll back. In practice the cap-protected endpoints are
// read-only, so there is nothing to roll back.
func responseTooLarge(w http.ResponseWriter, started time.Time, written, cap int64) {
	writeJSONWithDuration(w, http.StatusInsufficientStorage, orderedFields{
		{"ok", false},
		{"error", "the response would exceed the maximum allowed size"},
		{"approx_response_mb", MB(written)},
		{"max_response_mb", MB(cap)},
	}, started)
}

func methodNotAllowed(w http.ResponseWriter, started time.Time) {
	writeJSONWithDuration(w, http.StatusMethodNotAllowed, orderedFields{
		{"ok", false},
		{"error", "the HTTP method is not allowed for this endpoint"},
	}, started)
}

// lookupTarget is the parsed form of /get's and /render's URL query:
// a scope plus exactly one of id or seq. Built by parseLookupTarget.
type lookupTarget struct {
	Scope string
	ByID  bool
	ID    string
	Seq   uint64
}

// parseLookupTarget pulls scope + exactly one of id/seq from the query
// string and validates each. Scope errors are labelled with the endpoint;
// the id/seq shape errors are endpoint-agnostic since the rule is the same
// on every single-item read.
func parseLookupTarget(r *http.Request, endpoint string) (lookupTarget, error) {
	query := r.URL.Query()
	scope := query.Get("scope")
	id := query.Get("id")
	seqStr := query.Get("seq")

	if err := validateScope(scope, endpoint); err != nil {
		return lookupTarget{}, err
	}

	hasID := id != ""
	hasSeq := seqStr != ""
	if hasID == hasSeq {
		return lookupTarget{}, errors.New("exactly one of 'id' or 'seq' must be provided")
	}

	if hasID {
		if err := validateID(id); err != nil {
			return lookupTarget{}, err
		}
		return lookupTarget{Scope: scope, ByID: true, ID: id}, nil
	}

	seq, err := strconv.ParseUint(seqStr, 10, 64)
	if err != nil {
		return lookupTarget{}, errors.New("the 'seq' parameter must be a valid unsigned integer")
	}
	return lookupTarget{Scope: scope, Seq: seq}, nil
}

// scopeLimit is the parsed form of the scope+limit query pair used by every
// multi-item read (/head, /tail). Endpoint-specific params
// (offset, after_seq) are parsed by the handler itself — this
// helper deliberately stops at the common pair.
type scopeLimit struct {
	Scope string
	Limit int
}

// parseScopeLimit validates scope and normalizes limit in the order every
// caller expects (scope first, then limit), so the returned error matches
// the handlers' historical behaviour.
func parseScopeLimit(r *http.Request, endpoint string) (scopeLimit, error) {
	query := r.URL.Query()
	scope := query.Get("scope")
	if err := validateScope(scope, endpoint); err != nil {
		return scopeLimit{}, err
	}
	limit, err := normalizeLimit(query.Get("limit"))
	if err != nil {
		return scopeLimit{}, err
	}
	return scopeLimit{Scope: scope, Limit: limit}, nil
}

func (api *API) RegisterRoutes(mux *http.ServeMux) {
	// Single-item write paths.
	mux.HandleFunc("/append", api.handleAppend)
	mux.HandleFunc("/update", api.handleUpdate)
	mux.HandleFunc("/upsert", api.handleUpsert)
	mux.HandleFunc("/counter_add", api.handleCounterAdd)
	mux.HandleFunc("/delete", api.handleDelete)
	mux.HandleFunc("/delete_up_to", api.handleDeleteUpTo)
	// Single-item / multi-item read paths.
	// /head and /tail enforce the per-response cap inside their shared
	// writer (writeJSONWithMetaCap, called from writeItemsHit) — no
	// outer middleware needed.
	mux.HandleFunc("/head", api.handleHead)
	mux.HandleFunc("/tail", api.handleTail)
	mux.HandleFunc("/get", api.handleGet)
	mux.HandleFunc("/render", api.handleRender)
	// Bulk and store-wide write paths.
	mux.HandleFunc("/wipe", api.handleWipe)
	mux.HandleFunc("/warm", api.handleWarm)
	mux.HandleFunc("/rebuild", api.handleRebuild)
	mux.HandleFunc("/delete_scope", api.handleDeleteScope)
	// Observability.
	mux.HandleFunc("/stats", api.handleStats)
	mux.HandleFunc("/scopelist", api.handleScopeList)
	mux.HandleFunc("/help", api.handleHelp)
}
