package scopecache

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// inboxRequest is the body shape for a /inbox call. Token, scope and
// payload are required; id, seq and ts are forbidden — the cache
// owns identity and time on the inbox path. Pointer fields let the
// JSON decoder distinguish "absent" from "zero value" so the
// forbidden-field reject fires on any client supply.
type inboxRequest struct {
	Token   string          `json:"token"`
	Scope   string          `json:"scope"`
	Payload json.RawMessage `json:"payload"`
	ID      *string         `json:"id,omitempty"`
	Seq     *uint64         `json:"seq,omitempty"`
	Ts      *int64          `json:"ts,omitempty"`
}

// handleInbox is the shared write-only ingestion endpoint. Single
// /append per request, no multi-call envelope. Same _tokens auth-gate
// as /guarded, but no scope-rewrite — the request lands directly in
// one of the operator-configured inbox scopes.
//
// The cache assigns id (`<capabilityID>:<32-hex random>`, i.e.
// 16 random bytes hex-encoded) and ts (`now()` in microseconds).
// Tenants cannot read what they wrote — there is no GET /inbox; reads
// happen through /admin /tail and /admin /delete_up_to.
//
// Registered only when ServerSecret is set (HMAC needed) AND at least
// one InboxScope is configured. Either condition false → route not
// registered, public callers receive 404.
func (api *API) handleInbox(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodPost {
		methodNotAllowed(w, started)
		return
	}

	var req inboxRequest
	// /inbox payloads are bounded by the per-item cap (same shape as
	// /append); the body adds token + scope on top, which the
	// single-item request cap already covers via SingleRequestBytesOverhead.
	if err := decodeBody(w, r, api.maxSingleBytes, &req); err != nil {
		badRequest(w, started, err.Error())
		return
	}

	// Required fields. Token gets the 401 envelope (auth signal);
	// missing scope/payload get 400 (shape error).
	if req.Token == "" {
		writeJSONWithDuration(w, http.StatusUnauthorized, orderedFields{
			{"ok", false},
			{"error", "the 'token' field is required"},
		}, started)
		return
	}
	if req.Scope == "" {
		badRequest(w, started, "the 'scope' field is required")
		return
	}
	if !payloadPresent(req.Payload) {
		badRequest(w, started, "the 'payload' field is required")
		return
	}

	// Forbidden fields. /inbox is "fire and forget" — the cache owns
	// id (for attribution) and ts (for receive-time). seq is forbidden
	// on every write path. A client wanting historical timestamps puts
	// them in the payload itself, where the cache stays opaque.
	if req.ID != nil {
		badRequest(w, started, "the 'id' field is forbidden on /inbox; the cache assigns it")
		return
	}
	if req.Seq != nil {
		badRequest(w, started, "the 'seq' field is forbidden on /inbox")
		return
	}
	if req.Ts != nil {
		badRequest(w, started, "the 'ts' field is forbidden on /inbox; the cache assigns it (use payload for client-side timestamps)")
		return
	}

	// Scope must be in the operator's inbox-scope allowlist.
	if !api.isInboxScope(req.Scope) {
		badRequest(w, started, "scope is not configured as an inbox scope")
		return
	}

	// /inbox-specific payload cap. Tighter than the generic per-item
	// cap (validateWriteItem below) by design — /inbox is fire-and-
	// forget tenant ingestion that the operator drains in batches via
	// /admin /tail, so a single rogue tenant pushing /append-sized 1
	// MiB items eats /admin /tail's response budget very fast. 400
	// (not 507): this is a per-request shape rule, same status class
	// as checkItemSize, not store-side admission control. Runs after
	// the inbox-scope allowlist (so a wrong scope name produces the
	// scope-misconfigured error rather than a confusing cap error)
	// but before the auth-gate (so a misconfigured tenant learns the
	// real issue is request size, not auth — same pattern as
	// /guarded's pre-flight response-cap check). Default 64 KiB;
	// override via SCOPECACHE_MAX_INBOX_KB.
	if int64(len(req.Payload)) > api.maxInboxBytes {
		badRequest(w, started, fmt.Sprintf(
			"the 'payload' field (%d bytes) exceeds the /inbox cap of %d bytes",
			len(req.Payload), api.maxInboxBytes,
		))
		return
	}

	// Auth-gate: same _tokens lookup as /guarded.
	capabilityID := computeCapabilityID(api.serverSecret, req.Token)
	if !api.tenantIsProvisioned(capabilityID) {
		badRequest(w, started, "tenant_not_provisioned")
		return
	}

	// Generate id and ts.
	var randomBytes [16]byte
	if _, err := rand.Read(randomBytes[:]); err != nil {
		// crypto/rand.Read essentially cannot fail on a working OS;
		// surface as 500 if it does so the operator notices.
		writeJSONWithDuration(w, http.StatusInternalServerError, orderedFields{
			{"ok", false},
			{"error", "could not generate random id"},
		}, started)
		return
	}
	autoID := capabilityID + ":" + hex.EncodeToString(randomBytes[:])

	item := Item{
		Scope:   req.Scope,
		ID:      autoID,
		Payload: req.Payload,
		// Ts is left at its zero value here; appendOne stamps it to
		// time.Now().UnixMicro() under the scope write-lock — same
		// rule as every other write path.
	}

	// Validate the constructed item. Defensive: id and scope shapes
	// are constructed by the cache and known-good, but the payload
	// length check still applies and could fire 400 for an oversized
	// client payload.
	if err := validateWriteItem(item, "/inbox", api.store.maxItemBytes); err != nil {
		badRequest(w, started, err.Error())
		return
	}

	// Reach the scope buffer directly. Inbox scopes commonly start
	// with `_` (operator's choice) — going through appendOne bypasses
	// the public reserved-prefix check, which is the right semantic:
	// /inbox is operator-opted-in for these specific scope names.
	stored, err := api.store.appendOne(item)
	if err != nil {
		if writeStoreCapacityError(w, started, err, req.Scope) {
			return
		}
		// ScopeDetachedError or any other error → 409.
		conflict(w, started, err.Error())
		return
	}

	// Return the cache-assigned ts so clients with skewed clocks can
	// log the authoritative receive time without reading it back.
	writeJSONWithDuration(w, http.StatusOK, orderedFields{
		{"ok", true},
		{"ts", stored.Ts},
	}, started)
}
