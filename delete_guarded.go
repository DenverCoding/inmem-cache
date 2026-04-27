package scopecache

import (
	"errors"
	"net/http"
	"time"
)

// DeleteGuardedRequest is the body of /delete_guarded. The caller passes
// the tenant's capability_id directly — the handler builds the
// `_guarded:<capability_id>:` prefix internally so the caller cannot
// typo the prefix or forget the trailing ':' (which would silently
// match adjacent tenants whose ids share a prefix).
type DeleteGuardedRequest struct {
	CapabilityID string `json:"capability_id"`
}

// validateCapabilityID enforces the on-the-wire shape: exactly 64
// lowercase hex characters, matching the output of
// computeCapabilityID (hex-encoded HMAC-SHA256). Rejecting anything
// else here is what bounds /delete_guarded's damage radius: only
// well-formed tenant ids reach the store sweep.
func validateCapabilityID(id string) error {
	if id == "" {
		return errors.New("the 'capability_id' field is required")
	}
	if len(id) != 64 {
		return errors.New("the 'capability_id' field must be exactly 64 lowercase hex characters")
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return errors.New("the 'capability_id' field must contain only lowercase hex characters [0-9a-f]")
		}
	}
	return nil
}

// handleDeleteGuarded removes every scope a single tenant has ever
// written under their `_guarded:<capability_id>:*` namespace, in one
// atomic call. It is the data-cleanup half of the token-revocation
// pattern; the operator runs it in the same /admin batch that deletes
// the tenant's _tokens item and counter items.
//
// Damage radius is bounded by construction: the handler builds the
// prefix from a strictly-validated capability_id (64 lowercase hex
// chars), so the sweep can only ever match scopes that were created
// for a valid /guarded tenant. There is no free-form 'prefix' field
// — that intentional omission is the difference between this
// endpoint and the more general /delete_scope_prefix design proposal.
//
// Admin-only: registered in buildAdminCallSpecs() and reachable only
// through /admin's dispatcher. Not on the public mux, not in
// /multi_call's whitelist, not in /guarded's whitelist (a tenant
// cleaning up their own namespace would lock themselves out, and a
// tenant cleaning up someone else's would defeat the auth-gate).
func (api *API) handleDeleteGuarded(w http.ResponseWriter, r *http.Request) {
	started := time.Now()

	if r.Method != http.MethodPost {
		methodNotAllowed(w, started)
		return
	}

	var req DeleteGuardedRequest
	if err := decodeBody(w, r, api.maxSingleBytes, &req); err != nil {
		badRequest(w, started, err.Error())
		return
	}

	if err := validateCapabilityID(req.CapabilityID); err != nil {
		badRequest(w, started, err.Error())
		return
	}

	deletedScopes, deletedItems, freedBytes := api.store.deleteGuardedTenant(req.CapabilityID)

	writeJSONWithDuration(w, http.StatusOK, orderedFields{
		{"ok", true},
		{"deleted_scopes", deletedScopes},
		{"deleted_items", deletedItems},
		{"freed_mb", MB(freedBytes)},
	}, started)
}
