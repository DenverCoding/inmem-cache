package scopecache

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// --- happy path ---------------------------------------------------------------

func TestAdmin_ProvisionAndStatsRoundtrip(t *testing.T) {
	h, _ := newTestHandler(100)

	// Provision a reserved-prefix scope via /admin /upsert. This is the
	// pattern PHP uses at token issuance to bring `_guarded:<HMAC>:*`
	// into existence.
	provisionBody := `{"calls":[{"path":"/upsert","body":{"scope":"_guarded:abc123:events","id":"_provisioned","payload":{"t":1}}}]}`
	code, out, raw := doRequest(t, h, "POST", "/admin", provisionBody)
	if code != 200 {
		t.Fatalf("admin upsert code=%d body=%s", code, raw)
	}
	if !mustBool(t, out, "ok") {
		t.Fatalf("ok=false: %s", raw)
	}

	// /stats via /admin should now show the provisioned scope.
	code, out, raw = doRequest(t, h, "POST", "/admin", `{"calls":[{"path":"/stats"}]}`)
	if code != 200 {
		t.Fatalf("admin stats code=%d body=%s", code, raw)
	}
	results := out["results"].([]interface{})
	statsBody := results[0].(map[string]interface{})["body"].(map[string]interface{})
	scopes := statsBody["scopes"].(map[string]interface{})
	if _, ok := scopes["_guarded:abc123:events"]; !ok {
		t.Fatalf("provisioned scope missing from /stats: %s", raw)
	}
}

// /wipe via /admin clears everything including reserved scopes —
// confirms the route move from public to admin-only preserves
// functionality.
func TestAdmin_WipeClearsReservedScopes(t *testing.T) {
	h, _ := newTestHandler(100)

	// Provision a reserved scope and a regular tenant scope.
	doRequest(t, h, "POST", "/admin", `{"calls":[{"path":"/upsert","body":{"scope":"_guarded:abc:events","id":"_p","payload":{"t":1}}}]}`)
	doRequest(t, h, "POST", "/append", `{"scope":"public_scope","payload":{"v":1}}`)

	// Wipe via /admin
	code, _, raw := doRequest(t, h, "POST", "/admin", `{"calls":[{"path":"/wipe"}]}`)
	if code != 200 {
		t.Fatalf("wipe code=%d body=%s", code, raw)
	}

	// Verify both gone
	code, out, _ := doRequest(t, h, "POST", "/admin", `{"calls":[{"path":"/stats"}]}`)
	if code != 200 {
		t.Fatalf("post-wipe stats code=%d", code)
	}
	results := out["results"].([]interface{})
	statsBody := results[0].(map[string]interface{})["body"].(map[string]interface{})
	if sc := statsBody["scope_count"].(float64); sc != 0 {
		t.Errorf("scope_count=%v want 0 after wipe", sc)
	}
}

// --- whitelist ----------------------------------------------------------------

func TestAdmin_WhitelistMissRejectsBatch(t *testing.T) {
	h, _ := newTestHandler(10)

	body := `{"calls":[{"path":"/get","query":{"scope":"x","id":"a"}},{"path":"/admin"}]}`
	code, out, _ := doRequest(t, h, "POST", "/admin", body)
	if code != 400 {
		t.Fatalf("code=%d want 400 (self-reference rejected)", code)
	}
	if mustBool(t, out, "ok") {
		t.Error("ok=true on whitelist miss")
	}
}

// /admin shares the pre-flight response-cap check with /multi_call. A
// pathologically small MaxResponseBytes must be rejected upfront with
// 507 (no side effects), not after dispatch when the wrapper would
// erase per-slot status.
func TestAdmin_TinyResponseCapRejectedPreflight(t *testing.T) {
	api := NewAPI(NewStore(Config{
		ScopeMaxItems:     10,
		MaxStoreBytes:     100 << 20,
		MaxItemBytes:      1 << 20,
		MaxResponseBytes:  200,
		MaxMultiCallBytes: 16 << 20,
		MaxMultiCallCount: 10,
		ServerSecret:      "test-secret",
	}))
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	body := `{"calls":[{"path":"/upsert","body":{"scope":"_test:x","id":"a","payload":1}}]}`
	code, out, raw := doRequest(t, mux, "POST", "/admin", body)
	if code != http.StatusInsufficientStorage {
		t.Fatalf("code=%d want 507, body=%s", code, raw)
	}
	if errStr, _ := out["error"].(string); !strings.Contains(errStr, "response cap too small") {
		t.Errorf("error message does not name the cap: %s", raw)
	}
	if _, ok := api.store.getScope("_test:x"); ok {
		t.Errorf("preflight reject leaked side effect: scope created")
	}
}

// /admin runs through the same prepareSubCalls pre-pass as /multi_call,
// so a malformed query at calls[k] must reject the whole batch before
// calls[0..k-1] commit. Same regression class as the /multi_call test.
func TestAdmin_NestedQueryRejectsBeforeSideEffects(t *testing.T) {
	h, _ := newTestHandler(10)

	body := `{
		"calls": [
			{"path": "/append", "body": {"scope": "_admin-preflight", "id": "a", "payload": {"v": 1}}},
			{"path": "/get",    "query": {"scope": {"nested": true}}}
		]
	}`
	code, _, raw := doRequest(t, h, "POST", "/admin", body)
	if code != 400 {
		t.Fatalf("nested-query batch: code=%d want 400, body=%s", code, raw)
	}

	// /admin /get on the would-be scope: still must not exist.
	probe := `{"calls":[{"path":"/get","query":{"scope":"_admin-preflight","id":"a"}}]}`
	code, out, raw := doRequest(t, h, "POST", "/admin", probe)
	if code != 200 {
		t.Fatalf("probe: code=%d body=%s", code, raw)
	}
	results := out["results"].([]interface{})
	getResp := results[0].(map[string]interface{})["body"].(map[string]interface{})
	if hit, _ := getResp["hit"].(bool); hit {
		t.Errorf("calls[0] /append leaked despite calls[1] rejection: %s", raw)
	}
}

func TestAdmin_BlocksMultiCallSelfReference(t *testing.T) {
	h, _ := newTestHandler(10)

	body := `{"calls":[{"path":"/multi_call","body":{"calls":[]}}]}`
	code, _, _ := doRequest(t, h, "POST", "/admin", body)
	if code != 400 {
		t.Fatalf("code=%d want 400 (multi_call not in admin whitelist)", code)
	}
}

func TestAdmin_BlocksGuardedReentry(t *testing.T) {
	h, _ := newTestHandler(10)

	body := `{"calls":[{"path":"/guarded","body":{"token":"x","calls":[]}}]}`
	code, _, _ := doRequest(t, h, "POST", "/admin", body)
	if code != 400 {
		t.Fatalf("code=%d want 400 (guarded not in admin whitelist)", code)
	}
}

func TestAdmin_BlocksHelp(t *testing.T) {
	h, _ := newTestHandler(10)

	body := `{"calls":[{"path":"/help"}]}`
	code, _, _ := doRequest(t, h, "POST", "/admin", body)
	if code != 400 {
		t.Fatalf("code=%d want 400 (/help not in admin whitelist)", code)
	}
}

// --- admin sees raw _* scopes -------------------------------------------------

// /admin can read and write to scopes starting with `_` — the reserved
// prefix block applies only to public endpoints.
func TestAdmin_RawReservedAccess(t *testing.T) {
	h, _ := newTestHandler(10)

	// Write to a reserved scope via /admin.
	body := `{"calls":[{"path":"/upsert","body":{"scope":"_guarded:capX:events","id":"item1","payload":{"v":42}}}]}`
	code, out, raw := doRequest(t, h, "POST", "/admin", body)
	if code != 200 {
		t.Fatalf("admin upsert code=%d body=%s", code, raw)
	}
	results := out["results"].([]interface{})
	if status := int(results[0].(map[string]interface{})["status"].(float64)); status != 200 {
		t.Fatalf("upsert slot status=%d want 200", status)
	}

	// Read back via /admin.
	body = `{"calls":[{"path":"/get","query":{"scope":"_guarded:capX:events","id":"item1"}}]}`
	code, out, raw = doRequest(t, h, "POST", "/admin", body)
	if code != 200 {
		t.Fatalf("admin get code=%d body=%s", code, raw)
	}
	results = out["results"].([]interface{})
	getBody := results[0].(map[string]interface{})["body"].(map[string]interface{})
	if hit, _ := getBody["hit"].(bool); !hit {
		t.Fatalf("expected hit on reserved scope read; body=%s", raw)
	}
}

// Public endpoints reject any scope starting with `_` (reserved-prefix
// rule). Reaffirm here so the test pair (admin-can, public-cannot) sits
// next to each other.
func TestAdmin_PublicEndpointsRejectReservedPrefix(t *testing.T) {
	h, _ := newTestHandler(10)

	// Public /append rejects.
	code, _, _ := doRequest(t, h, "POST", "/append", `{"scope":"_anything","id":"x","payload":{"v":1}}`)
	if code != 400 {
		t.Fatalf("public append on reserved scope: code=%d want 400", code)
	}

	// Public /get rejects.
	code, _, _ = doRequest(t, h, "GET", "/get?scope=_anything&id=x", "")
	if code != 400 {
		t.Fatalf("public get on reserved scope: code=%d want 400", code)
	}
}

// --- public-route removal -----------------------------------------------------

// The four admin-only paths must not appear on the public mux.
func TestAdmin_PublicRoutesRemoved(t *testing.T) {
	h, _ := newTestHandler(10)

	for _, path := range []string{"/wipe", "/warm", "/rebuild", "/delete_scope"} {
		code, _, _ := doRequest(t, h, "POST", path, "{}")
		if code != 404 {
			t.Errorf("public POST %s: code=%d want 404", path, code)
		}
	}
}

// --- input shape --------------------------------------------------------------

func TestAdmin_GETRejected(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "GET", "/admin", "")
	if code != http.StatusMethodNotAllowed {
		t.Fatalf("code=%d want 405", code)
	}
}

func TestAdmin_MalformedBody(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "POST", "/admin", `{not-json`)
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
}

func TestAdmin_MissingCallsField(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "POST", "/admin", `{}`)
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
}

func TestAdmin_EmptyCallsArray(t *testing.T) {
	h, _ := newTestHandler(10)
	code, out, _ := doRequest(t, h, "POST", "/admin", `{"calls":[]}`)
	if code != 200 {
		t.Fatalf("code=%d want 200 (empty batch is valid)", code)
	}
	if n := mustFloat(t, out, "count"); n != 0 {
		t.Errorf("count=%v want 0", n)
	}
}

func TestAdmin_CountOverflow(t *testing.T) {
	h, _ := newTestHandler(10)
	calls := make([]string, 0, 11)
	for i := 0; i < 11; i++ {
		calls = append(calls, `{"path":"/stats"}`)
	}
	body := `{"calls":[` + strings.Join(calls, ",") + `]}`
	code, _, _ := doRequest(t, h, "POST", "/admin", body)
	if code != 400 {
		t.Fatalf("code=%d want 400 on count overflow", code)
	}
}

// --- batch happy path ---------------------------------------------------------

// Multiple admin sub-calls in one batch — confirms shared-dispatcher
// reuse from /multi_call works correctly under /admin's wider whitelist.
func TestAdmin_BatchMixedOps(t *testing.T) {
	h, _ := newTestHandler(100)

	body := `{"calls":[
		{"path":"/upsert","body":{"scope":"_one","id":"a","payload":{"v":1}}},
		{"path":"/upsert","body":{"scope":"_two","id":"b","payload":{"v":2}}},
		{"path":"/stats"},
		{"path":"/delete_scope","body":{"scope":"_one"}},
		{"path":"/stats"}
	]}`
	code, out, raw := doRequest(t, h, "POST", "/admin", body)
	if code != 200 {
		t.Fatalf("code=%d body=%s", code, raw)
	}
	results := out["results"].([]interface{})
	if len(results) != 5 {
		t.Fatalf("got %d results, want 5", len(results))
	}

	// Sanity: stats slot at index 2 should show both scopes.
	stats2 := results[2].(map[string]interface{})["body"].(map[string]interface{})
	scopes2 := stats2["scopes"].(map[string]interface{})
	if _, ok := scopes2["_one"]; !ok {
		t.Errorf("scopes[_one] missing in pre-delete stats")
	}
	if _, ok := scopes2["_two"]; !ok {
		t.Errorf("scopes[_two] missing in pre-delete stats")
	}

	// Stats slot at index 4 (post-delete) should show only _two.
	stats4 := results[4].(map[string]interface{})["body"].(map[string]interface{})
	scopes4 := stats4["scopes"].(map[string]interface{})
	if _, ok := scopes4["_one"]; ok {
		t.Errorf("scopes[_one] still present after /delete_scope")
	}
	if _, ok := scopes4["_two"]; !ok {
		t.Errorf("scopes[_two] missing after /delete_scope of _one")
	}
}

// --- routing self-check -------------------------------------------------------

// Admin handler is reachable via the mux registered by RegisterRoutes.
func TestAdmin_HandlerReachableViaMux(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, raw := doRequest(t, h, "POST", "/admin", `{"calls":[]}`)
	if code != 200 {
		t.Fatalf("code=%d want 200, body=%s", code, raw)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("non-JSON body: %s", raw)
	}
	if _, ok := out["results"]; !ok {
		t.Errorf("missing results: %s", raw)
	}
}
