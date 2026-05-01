package scopecache

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// Real-shape capability ids used across the tests below. Hex-only,
// exactly 64 chars — matches the on-the-wire shape (HMAC-SHA256 hex).
var (
	capA = strings.Repeat("a", 64)
	capB = strings.Repeat("b", 64)
)

// --- happy path ---------------------------------------------------------------

// Tenant A and tenant B both have data under their own _guarded:<cap>:*
// namespaces. /delete_guarded for A must drop every A-scope and leave
// every B-scope intact. Also pins the response shape against the
// fields documented in the RFC (deleted_scopes, deleted_items, freed_mb).
func TestDeleteGuarded_DeletesOnlyTargetTenant(t *testing.T) {
	h, api := newTestHandler(100)

	// Provision 3 scopes for capA and 2 for capB via /admin /upsert.
	for _, scope := range []string{
		"_guarded:" + capA + ":thread:1",
		"_guarded:" + capA + ":thread:2",
		"_guarded:" + capA + ":dm:42",
		"_guarded:" + capB + ":thread:1",
		"_guarded:" + capB + ":notes",
	} {
		body := fmt.Sprintf(`{"path":"/upsert","body":{"scope":%q,"id":"x","payload":{"v":1}}}`, scope)
		code, _, raw := doRequest(t, h, "POST", "/admin", `{"calls":[`+body+`]}`)
		if code != 200 {
			t.Fatalf("provision %s: code=%d body=%s", scope, code, raw)
		}
	}

	// Sanity: the store now sees 5 scopes.
	if got := api.store.stats().ScopeCount; got != 5 {
		t.Fatalf("pre-delete scope count = %d, want 5", got)
	}

	// Delete capA's namespace via /admin.
	body := fmt.Sprintf(`{"capability_id":%q}`, capA)
	status, slot, raw := doAdminRequest(t, h, "/delete_guarded", body)
	if status != 200 {
		t.Fatalf("delete_guarded slot status=%d body=%s", status, raw)
	}
	if !mustBool(t, slot, "ok") {
		t.Fatalf("ok=false: %s", raw)
	}
	if got := mustFloat(t, slot, "deleted_scopes"); got != 3 {
		t.Errorf("deleted_scopes=%v want 3 (body=%s)", got, raw)
	}
	if got := mustFloat(t, slot, "deleted_items"); got != 3 {
		t.Errorf("deleted_items=%v want 3 (body=%s)", got, raw)
	}
	if _, ok := slot["freed_mb"]; !ok {
		t.Errorf("response missing freed_mb: %s", raw)
	}

	// Post-delete: only capB's two scopes survive.
	stats := api.store.stats()
	if stats.ScopeCount != 2 {
		t.Errorf("post-delete scope count = %d, want 2", stats.ScopeCount)
	}
	for scope := range stats.Scopes {
		if !strings.HasPrefix(scope, "_guarded:"+capB+":") {
			t.Errorf("non-capB scope survived: %q", scope)
		}
	}
}

// /delete_guarded must not match scopes whose capability_id shares a
// prefix with the target. The trailing ':' in the constructed prefix is
// the load-bearing separator: without it the sweep would match
// `_guarded:<capA><suffix>:*` for any suffix.
//
// We can't easily construct two 64-char capability_ids where one is a
// prefix of the other (they're fixed-length), so this test instead
// pins that an unrelated scope under a *different* tenant whose first
// 63 chars match the target is left alone. Catches a regression where
// someone replaces strings.HasPrefix with a substring match or drops
// the trailing colon.
func TestDeleteGuarded_PrefixCollisionIsImpossibleByConstruction(t *testing.T) {
	h, api := newTestHandler(10)

	capNeighbour := strings.Repeat("a", 63) + "b" // shares first 63 chars with capA
	for _, scope := range []string{
		"_guarded:" + capA + ":data",
		"_guarded:" + capNeighbour + ":data",
	} {
		body := fmt.Sprintf(`{"path":"/upsert","body":{"scope":%q,"id":"x","payload":1}}`, scope)
		doRequest(t, h, "POST", "/admin", `{"calls":[`+body+`]}`)
	}

	body := fmt.Sprintf(`{"capability_id":%q}`, capA)
	status, slot, raw := doAdminRequest(t, h, "/delete_guarded", body)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	if got := mustFloat(t, slot, "deleted_scopes"); got != 1 {
		t.Errorf("deleted_scopes=%v want 1 (only capA, NOT capNeighbour)", got)
	}
	if _, ok := api.store.getScope("_guarded:" + capNeighbour + ":data"); !ok {
		t.Error("neighbour tenant scope erroneously deleted")
	}
}

// Calling /delete_guarded for a tenant that has no data scopes is not an
// error — it returns deleted_scopes=0. This matches the "best-effort
// cleanup" contract: the operator can fan out the four-call revocation
// batch unconditionally without first checking which scopes exist.
func TestDeleteGuarded_NoScopesReturnsZero(t *testing.T) {
	h, _ := newTestHandler(10)

	body := fmt.Sprintf(`{"capability_id":%q}`, capA)
	status, slot, raw := doAdminRequest(t, h, "/delete_guarded", body)
	if status != 200 {
		t.Fatalf("status=%d body=%s", status, raw)
	}
	if got := mustFloat(t, slot, "deleted_scopes"); got != 0 {
		t.Errorf("deleted_scopes=%v want 0 (body=%s)", got, raw)
	}
	if got := mustFloat(t, slot, "deleted_items"); got != 0 {
		t.Errorf("deleted_items=%v want 0 (body=%s)", got, raw)
	}
}

// After /delete_guarded the store's totalBytes must equal the sum of
// remaining scope reservations. This catches accounting drift —
// historically the easiest way to corrupt the byte counter is to
// release item bytes without releasing scopeBufferOverhead (or vice
// versa) on a delete path.
func TestDeleteGuarded_BytesAccountingStaysBalanced(t *testing.T) {
	h, api := newTestHandler(10)

	// Three scopes for capA, two for capB. After deleting capA, the
	// remaining totalBytes must equal sum(buf.bytes) + 2 * scopeBufferOverhead.
	for _, scope := range []string{
		"_guarded:" + capA + ":a",
		"_guarded:" + capA + ":b",
		"_guarded:" + capA + ":c",
		"_guarded:" + capB + ":a",
		"_guarded:" + capB + ":b",
	} {
		body := fmt.Sprintf(`{"path":"/upsert","body":{"scope":%q,"id":"x","payload":{"big":"enough-to-not-be-zero"}}}`, scope)
		doRequest(t, h, "POST", "/admin", `{"calls":[`+body+`]}`)
	}

	body := fmt.Sprintf(`{"capability_id":%q}`, capA)
	doAdminRequest(t, h, "/delete_guarded", body)

	// Compute expected totalBytes by walking remaining scopes directly.
	var expected int64
	for shIdx := range api.store.shards {
		api.store.shards[shIdx].mu.RLock()
		for _, buf := range api.store.shards[shIdx].scopes {
			buf.mu.RLock()
			expected += buf.bytes
			buf.mu.RUnlock()
			expected += scopeBufferOverhead
		}
		api.store.shards[shIdx].mu.RUnlock()
	}

	got := api.store.totalBytes.Load()
	if got != expected {
		t.Errorf("totalBytes=%d, want %d (drift=%d)", got, expected, got-expected)
	}
}

// --- validation ---------------------------------------------------------------

func TestDeleteGuarded_ValidationRejects(t *testing.T) {
	h, _ := newTestHandler(10)

	cases := []struct {
		name       string
		body       string
		wantSubstr string
	}{
		{"empty capability_id", `{"capability_id":""}`, "required"},
		{"missing field", `{}`, "required"},
		{"too short", `{"capability_id":"abc"}`, "exactly 64"},
		{"too long", `{"capability_id":"` + strings.Repeat("a", 65) + `"}`, "exactly 64"},
		{"uppercase hex", `{"capability_id":"` + strings.Repeat("A", 64) + `"}`, "lowercase hex"},
		{"non-hex char", `{"capability_id":"` + strings.Repeat("a", 63) + `g"}`, "lowercase hex"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, slot, raw := doAdminRequest(t, h, "/delete_guarded", tc.body)
			if status != 400 {
				t.Fatalf("status=%d want 400 (body=%s)", status, raw)
			}
			errStr, _ := slot["error"].(string)
			if !strings.Contains(errStr, tc.wantSubstr) {
				t.Errorf("error=%q does not contain %q", errStr, tc.wantSubstr)
			}
		})
	}
}

// --- exposure --------------------------------------------------------------------

// /delete_guarded MUST NOT be on the public mux. A direct POST should
// 404 — the only legitimate way to reach it is via /admin (and from
// there only when EnableAdmin is set).
func TestDeleteGuarded_NotOnPublicMux(t *testing.T) {
	h, _ := newTestHandler(10)

	body := fmt.Sprintf(`{"capability_id":%q}`, capA)
	code, _, raw := doRequest(t, h, "POST", "/delete_guarded", body)
	if code != http.StatusNotFound {
		t.Fatalf("public POST /delete_guarded: code=%d want 404, body=%s", code, raw)
	}
}

// /delete_guarded MUST NOT appear in the /multi_call whitelist.
// /multi_call is the tenant-anonymous public dispatcher; admitting
// /delete_guarded there would make tenant-data deletion reachable
// without auth, defeating the whole point of admin-gating it.
func TestDeleteGuarded_NotInMultiCallWhitelist(t *testing.T) {
	h, _ := newTestHandler(10)

	body := fmt.Sprintf(`{"calls":[{"path":"/delete_guarded","body":{"capability_id":%q}}]}`, capA)
	code, out, raw := doRequest(t, h, "POST", "/multi_call", body)
	if code != 400 {
		t.Fatalf("multi_call /delete_guarded: code=%d want 400, body=%s", code, raw)
	}
	if errStr, _ := out["error"].(string); !strings.Contains(errStr, "not allowed") {
		t.Errorf("error message does not say 'not allowed': %s", raw)
	}
}

// /delete_guarded MUST NOT appear in the /guarded whitelist. A tenant
// authenticated via /guarded must not be able to nuke their own (or
// anyone else's) namespace — that is an operator-only operation.
func TestDeleteGuarded_NotInGuardedWhitelist(t *testing.T) {
	h, api := newTestHandler(10)

	// Provision the tenant token in _tokens so the auth-gate passes
	// and we actually exercise the whitelist check (rather than
	// failing earlier on tenant_not_provisioned).
	tokenIssue := `{"calls":[{"path":"/upsert","body":{"scope":"_tokens","id":"%s","payload":{"issued":1}}}]}`
	capID := computeCapabilityID(api.serverSecret, "tenant-token")
	doRequest(t, h, "POST", "/admin", fmt.Sprintf(tokenIssue, capID))

	body := fmt.Sprintf(`{"token":"tenant-token","calls":[{"path":"/delete_guarded","body":{"capability_id":%q}}]}`, capA)
	code, out, raw := doRequest(t, h, "POST", "/guarded", body)
	if code != 400 {
		t.Fatalf("guarded /delete_guarded: code=%d want 400, body=%s", code, raw)
	}
	if errStr, _ := out["error"].(string); !strings.Contains(errStr, "not allowed") {
		t.Errorf("error message does not say 'not allowed': %s", raw)
	}
}

// The full revocation pattern — one /admin batch deletes the token
// item, both counter items, and every data scope under
// _guarded:<capA>:*. After the batch nothing keyed on capA remains
// anywhere in the store.
func TestDeleteGuarded_FullRevocationBatch(t *testing.T) {
	h, api := newTestHandler(10)

	// Set up: token item, two counter items (via a /guarded call so
	// counter scopes auto-provision), and tenant data.
	capID := computeCapabilityID(api.serverSecret, "rev-token")
	provision := fmt.Sprintf(`{"calls":[{"path":"/upsert","body":{"scope":"_tokens","id":%q,"payload":{"u":1}}}]}`, capID)
	doRequest(t, h, "POST", "/admin", provision)

	// One real /guarded call: this writes a tenant scope AND bumps both counters.
	guardedCall := fmt.Sprintf(`{"token":"rev-token","calls":[{"path":"/upsert","body":{"scope":"thread:1","id":"x","payload":{"v":1}}}]}`)
	code, _, raw := doRequest(t, h, "POST", "/guarded", guardedCall)
	if code != 200 {
		t.Fatalf("seed guarded call: code=%d body=%s", code, raw)
	}

	// Sanity: token + counters + tenant data all present.
	if _, ok := api.store.getScope("_tokens"); !ok {
		t.Fatal("setup: _tokens missing")
	}
	if _, ok := api.store.getScope("_guarded:" + capID + ":thread:1"); !ok {
		t.Fatal("setup: tenant scope missing")
	}

	// Full four-call revocation batch.
	revoke := fmt.Sprintf(`{"calls":[
		{"path":"/delete",         "body":{"scope":"_tokens",               "id":%q}},
		{"path":"/delete",         "body":{"scope":"_counters_count_calls", "id":%q}},
		{"path":"/delete",         "body":{"scope":"_counters_count_kb",    "id":%q}},
		{"path":"/delete_guarded", "body":{"capability_id":%q}}
	]}`, capID, capID, capID, capID)

	code, out, raw := doRequest(t, h, "POST", "/admin", revoke)
	if code != 200 {
		t.Fatalf("revoke batch: code=%d body=%s", code, raw)
	}
	results := out["results"].([]interface{})
	for i, r := range results {
		slot := r.(map[string]interface{})
		if status := slot["status"].(float64); status != 200 {
			t.Errorf("results[%d] status=%v want 200 (body=%s)", i, status, raw)
		}
	}

	// Token item gone → /guarded for that token now rejects.
	rejected := fmt.Sprintf(`{"token":"rev-token","calls":[{"path":"/get","query":{"scope":"thread:1","id":"x"}}]}`)
	code, _, _ = doRequest(t, h, "POST", "/guarded", rejected)
	if code != 400 {
		t.Errorf("post-revoke /guarded: code=%d want 400 (tenant_not_provisioned)", code)
	}

	// Tenant data scope gone.
	if _, ok := api.store.getScope("_guarded:" + capID + ":thread:1"); ok {
		t.Error("post-revoke: tenant data scope still present")
	}
}
