package scopecache

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// computeCapForTest mirrors the cache's HMAC computation so tests can
// build the prefixed scope name PHP-side would produce.
func computeCapForTest(serverSecret, token string) string {
	h := hmac.New(sha256.New, []byte(serverSecret))
	h.Write([]byte(token))
	return hex.EncodeToString(h.Sum(nil))
}

const testServerSecret = "test-secret"

// provisionTenantScope creates a `_guarded:<capId>:<name>` scope via
// /admin so subsequent /guarded calls' scope-existence check passes.
// Equivalent to PHP's sc_admin_calls([['/upsert', sentinel item]]) at
// token issuance.
func provisionTenantScope(t *testing.T, h http.Handler, token, name string) string {
	t.Helper()
	capID := computeCapForTest(testServerSecret, token)
	scope := "_guarded:" + capID + ":" + name
	body := fmt.Sprintf(`{"calls":[{"path":"/upsert","body":{"scope":"%s","id":"_provisioned","payload":{"t":1}}}]}`, scope)
	code, _, raw := doRequest(t, h, "POST", "/admin", body)
	if code != 200 {
		t.Fatalf("provisionTenantScope: code=%d body=%s", code, raw)
	}
	return scope
}

// guardedSlot extracts the [first] result slot from a /guarded response.
func guardedSlot(t *testing.T, out map[string]interface{}, idx int) (int, map[string]interface{}) {
	t.Helper()
	results, ok := out["results"].([]interface{})
	if !ok || idx >= len(results) {
		t.Fatalf("results[%d] missing: %+v", idx, out)
	}
	slot := results[idx].(map[string]interface{})
	status := int(slot["status"].(float64))
	body, _ := slot["body"].(map[string]interface{})
	return status, body
}

// --- happy path ---------------------------------------------------------------

func TestGuarded_HappyPath(t *testing.T) {
	h, _ := newTestHandler(100)
	provisionTenantScope(t, h, "tenant-A-token", "events")

	body := `{"token":"tenant-A-token","calls":[{"path":"/append","body":{"scope":"events","payload":{"e":"signup"}}}]}`
	code, out, raw := doRequest(t, h, "POST", "/guarded", body)
	if code != 200 {
		t.Fatalf("code=%d body=%s", code, raw)
	}
	status, slotBody := guardedSlot(t, out, 0)
	if status != 200 {
		t.Errorf("slot status=%d want 200, body=%s", status, raw)
	}
	if ok, _ := slotBody["ok"].(bool); !ok {
		t.Errorf("slot body ok=false: %v", slotBody)
	}
}

// Prefix stripping: tenant sends `scope: "events"`, gets `scope:
// "events"` back, never the rewritten _guarded:<HMAC>:events form.
func TestGuarded_ResponseStripping_Append(t *testing.T) {
	h, _ := newTestHandler(100)
	provisionTenantScope(t, h, "tok-strip", "events")

	body := `{"token":"tok-strip","calls":[{"path":"/append","body":{"scope":"events","id":"item1","payload":{"v":1}}}]}`
	code, out, raw := doRequest(t, h, "POST", "/guarded", body)
	if code != 200 {
		t.Fatalf("code=%d body=%s", code, raw)
	}
	_, slotBody := guardedSlot(t, out, 0)
	item, ok := slotBody["item"].(map[string]interface{})
	if !ok {
		t.Fatalf("missing item in response: %v", slotBody)
	}
	scope, _ := item["scope"].(string)
	if scope != "events" {
		t.Errorf("got scope=%q want %q (prefix should be stripped)", scope, "events")
	}
	// Verify the prefix was actually present internally — sanity check
	// of the test setup, not strictly necessary.
	if strings.HasPrefix(scope, "_guarded:") {
		t.Errorf("response leaked internal prefix: %q", scope)
	}
}

func TestGuarded_ResponseStripping_Tail(t *testing.T) {
	h, _ := newTestHandler(100)
	provisionTenantScope(t, h, "tok-tail", "events")

	// Append two items first.
	for i := 0; i < 2; i++ {
		body := fmt.Sprintf(`{"token":"tok-tail","calls":[{"path":"/append","body":{"scope":"events","id":"i%d","payload":{"n":%d}}}]}`, i, i)
		if code, _, raw := doRequest(t, h, "POST", "/guarded", body); code != 200 {
			t.Fatalf("seed append %d: code=%d body=%s", i, code, raw)
		}
	}

	// Tail and check stripping in items[].
	body := `{"token":"tok-tail","calls":[{"path":"/tail","query":{"scope":"events","limit":10}}]}`
	code, out, raw := doRequest(t, h, "POST", "/guarded", body)
	if code != 200 {
		t.Fatalf("tail code=%d body=%s", code, raw)
	}
	_, slotBody := guardedSlot(t, out, 0)
	items, ok := slotBody["items"].([]interface{})
	if !ok {
		t.Fatalf("missing items in tail response: %v", slotBody)
	}
	for _, raw := range items {
		item := raw.(map[string]interface{})
		s, _ := item["scope"].(string)
		// /tail items omit scope when it equals the queried scope (Item
		// has scope as omitempty), so we accept either "events" or
		// empty — both indicate no leak.
		if s != "" && s != "events" {
			t.Errorf("tail item scope=%q want %q (stripped)", s, "events")
		}
		if strings.HasPrefix(s, "_guarded:") {
			t.Errorf("tail item leaked prefix: %q", s)
		}
	}
}

// --- token validation ---------------------------------------------------------

func TestGuarded_MissingToken(t *testing.T) {
	h, _ := newTestHandler(10)
	body := `{"calls":[{"path":"/get","query":{"scope":"x","id":"a"}}]}`
	code, out, _ := doRequest(t, h, "POST", "/guarded", body)
	if code != 401 {
		t.Fatalf("code=%d want 401", code)
	}
	if mustBool(t, out, "ok") {
		t.Error("ok=true on missing token")
	}
}

// --- scope-not-provisioned ----------------------------------------------------

// A random/forged token's HMAC names a scope that was never
// provisioned, so the existence check fails. No state mutated.
func TestGuarded_RandomTokenRejected(t *testing.T) {
	h, _ := newTestHandler(10)

	body := `{"token":"random-attacker","calls":[{"path":"/append","body":{"scope":"x","payload":"junk"}}]}`
	code, out, raw := doRequest(t, h, "POST", "/guarded", body)
	if code != 400 {
		t.Fatalf("code=%d want 400 (scope_not_provisioned), body=%s", code, raw)
	}
	if mustBool(t, out, "ok") {
		t.Error("ok=true on scope_not_provisioned")
	}
	if !strings.Contains(raw, "not provisioned") {
		t.Errorf("expected 'not provisioned' in error, got: %s", raw)
	}
}

// Tenant tries to escape into another scope by sending a literal
// `_guarded:other:X` — gets rewritten to `_guarded:<myHMAC>:_guarded:
// other:X`, which doesn't exist. Rejected.
func TestGuarded_PrefixInjectionAttempt(t *testing.T) {
	h, _ := newTestHandler(10)
	provisionTenantScope(t, h, "tok-inj", "events")

	body := `{"token":"tok-inj","calls":[{"path":"/get","query":{"scope":"_guarded:other_capId:events","id":"x"}}]}`
	code, _, raw := doRequest(t, h, "POST", "/guarded", body)
	if code != 400 {
		t.Fatalf("code=%d want 400, body=%s", code, raw)
	}
	if !strings.Contains(raw, "not provisioned") {
		t.Errorf("expected scope_not_provisioned, got: %s", raw)
	}
}

// A scope_not_provisioned reject must leave the store untouched — no
// side effect, in particular no lazy scope creation. Guards against a
// future regression that swaps the pre-dispatch existence check from
// getScope() (read-only) to ensureScope() (creates), which would let
// any token holder fill the store with empty buffers by spamming
// non-provisioned scope names.
func TestGuarded_RejectLeavesNoSideEffect(t *testing.T) {
	h, api := newTestHandler(10)
	provisionTenantScope(t, h, "tok-noside", "events")
	capID := computeCapForTest(testServerSecret, "tok-noside")
	rewritten := "_guarded:" + capID + ":ghosts"

	// Tenant has 'events' provisioned but not 'ghosts'. Attempting to
	// /append into 'ghosts' must be rejected.
	body := `{"token":"tok-noside","calls":[{"path":"/append","body":{"scope":"ghosts","id":"x","payload":1}}]}`
	code, _, raw := doRequest(t, h, "POST", "/guarded", body)
	if code != 400 {
		t.Fatalf("code=%d want 400, body=%s", code, raw)
	}
	if !strings.Contains(raw, "not provisioned") {
		t.Errorf("expected scope_not_provisioned, got: %s", raw)
	}

	// Store check: the rewritten scope must still not exist. If a
	// regression swapped getScope() for ensureScope() in the existence
	// check, this assertion fires.
	if _, ok := api.store.getScope(rewritten); ok {
		t.Errorf("rejected /append created scope %q as a side effect", rewritten)
	}
}

// Cross-tenant read attempt via body+query smuggling. A pre-fix
// rewriteCallScope rewrote body.scope and returned early, leaving
// query.scope un-rewritten. The dispatcher then built the GET URL
// from the un-rewritten query.scope, and the inner handler — running
// under admin context — happily read from another tenant's reserved
// scope. The fix rejects any call that carries `scope` in both body
// and query, whole-batch reject before the dispatch loop, no side
// effects, no counter ticks.
func TestGuarded_RejectsBodyAndQueryScopeSmuggle(t *testing.T) {
	h, api := newTestHandler(100)

	// Two tenants, each with their own provisioned scope. Tenant B's
	// scope holds a "secret" item we want to confirm tenant A cannot
	// read.
	provisionTenantScope(t, h, "tok-A", "events")
	provisionTenantScope(t, h, "tok-B", "events")
	capB := computeCapForTest(testServerSecret, "tok-B")
	bScope := "_guarded:" + capB + ":events"

	adminAppend := `{"calls":[{"path":"/append","body":{"scope":"` + bScope + `","id":"secret","payload":{"sensitive":"do-not-leak"}}}]}`
	if code, _, raw := doRequest(t, h, "POST", "/admin", adminAppend); code != 200 {
		t.Fatalf("seeding tenant B secret: code=%d body=%s", code, raw)
	}

	// Counter scope must not exist yet — used to verify the rejected
	// batch did not increment it.
	if _, ok := api.store.getScope(countersScopeCalls); ok {
		t.Fatal("counter scope already exists before any /guarded call")
	}

	// The attack: tenant A token, body.scope="events" (their own,
	// passes existence check post-rewrite), query.scope=tenant B's raw
	// reserved scope (would smuggle into the GET handler pre-fix).
	body := `{"token":"tok-A","calls":[{"path":"/get","body":{"scope":"events","id":"x"},"query":{"scope":"` + bScope + `","id":"secret"}}]}`
	code, _, raw := doRequest(t, h, "POST", "/guarded", body)
	if code != 400 {
		t.Fatalf("smuggle attempt: code=%d want 400, body=%s", code, raw)
	}
	if !strings.Contains(raw, "must be in body OR query") {
		t.Errorf("expected 'must be in body OR query' error, got: %s", raw)
	}

	// Side-effect-free: counters must not have been provisioned (the
	// rejection happens before the dispatch loop, so guardedIncrement-
	// Counters never runs).
	if _, ok := api.store.getScope(countersScopeCalls); ok {
		t.Errorf("rejected batch leaked a counter increment (scope exists)")
	}

	// Sanity: tenant B's secret is still reachable via /admin (proving
	// the seed worked) and was not touched by the attempt.
	getSecret := `{"calls":[{"path":"/get","query":{"scope":"` + bScope + `","id":"secret"}}]}`
	code, out, raw := doRequest(t, h, "POST", "/admin", getSecret)
	if code != 200 {
		t.Fatalf("admin re-read of B secret: code=%d body=%s", code, raw)
	}
	results := out["results"].([]interface{})
	resp := results[0].(map[string]interface{})["body"].(map[string]interface{})
	if hit, _ := resp["hit"].(bool); !hit {
		t.Errorf("tenant B secret no longer present after attack: %s", raw)
	}
}

// /guarded shares the pre-flight response-cap check with /multi_call.
// Runs before the token check so an undersized cap surfaces directly,
// not as a misleading 401.
func TestGuarded_TinyResponseCapRejectedPreflight(t *testing.T) {
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

	// No token, no scope provisioning — pre-flight must fire first.
	body := `{"token":"any","calls":[{"path":"/get","query":{"scope":"events","id":"a"}}]}`
	code, out, raw := doRequest(t, mux, "POST", "/guarded", body)
	if code != http.StatusInsufficientStorage {
		t.Fatalf("code=%d want 507, body=%s", code, raw)
	}
	if errStr, _ := out["error"].(string); !strings.Contains(errStr, "response cap too small") {
		t.Errorf("error message does not name the cap: %s", raw)
	}
}

// /guarded runs prepareSubCalls AFTER scope rewrite, so a malformed
// query at calls[k] still rejects the whole batch before calls[0..k-1]
// commit. Same regression class as the /multi_call and /admin tests.
func TestGuarded_NestedQueryRejectsBeforeSideEffects(t *testing.T) {
	h, _ := newTestHandler(10)
	provisionTenantScope(t, h, "tok-prep", "events")
	capID := computeCapForTest(testServerSecret, "tok-prep")

	// calls[1] uses a nested-object on `id`, not on `scope`. The scope-
	// rewrite pre-pass rejects non-string scopes already (see
	// TestGuarded_RejectsNestedScope), so to exercise the buildSubURL
	// pre-pass specifically, the malformed field has to be one rewrite
	// doesn't touch.
	body := `{
		"token": "tok-prep",
		"calls": [
			{"path": "/append", "body": {"scope": "events", "id": "a", "payload": {"v": 1}}},
			{"path": "/get",    "query": {"scope": "events", "id": {"nested": true}}}
		]
	}`
	code, _, raw := doRequest(t, h, "POST", "/guarded", body)
	if code != 400 {
		t.Fatalf("nested-query batch: code=%d want 400, body=%s", code, raw)
	}

	// Verify calls[0] /append did not commit: the scope only has the
	// _provisioned sentinel, no item with id="a".
	probe := `{"calls":[{"path":"/get","query":{"scope":"_guarded:` + capID + `:events","id":"a"}}]}`
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

// --- whitelist enforcement ----------------------------------------------------

func TestGuarded_WhitelistMiss(t *testing.T) {
	h, _ := newTestHandler(10)
	provisionTenantScope(t, h, "tok-wl", "events")

	body := `{"token":"tok-wl","calls":[{"path":"/wipe"}]}`
	code, _, raw := doRequest(t, h, "POST", "/guarded", body)
	if code != 400 {
		t.Fatalf("code=%d want 400 (not_in_guarded_whitelist), body=%s", code, raw)
	}
	if !strings.Contains(raw, "not allowed") {
		t.Errorf("expected 'not allowed' in error, got: %s", raw)
	}
}

func TestGuarded_BlocksDeleteScope(t *testing.T) {
	h, _ := newTestHandler(10)
	provisionTenantScope(t, h, "tok-ds", "events")

	body := `{"token":"tok-ds","calls":[{"path":"/delete_scope","body":{"scope":"events"}}]}`
	code, _, _ := doRequest(t, h, "POST", "/guarded", body)
	if code != 400 {
		t.Fatalf("code=%d want 400 (delete_scope not in /guarded whitelist)", code)
	}
}

// --- counter auto-create on first call ----------------------------------------

func TestGuarded_CounterAutoCreate(t *testing.T) {
	h, _ := newTestHandler(100)
	provisionTenantScope(t, h, "tok-counter", "events")
	capID := computeCapForTest(testServerSecret, "tok-counter")

	// First /guarded call from a brand-new capability_id.
	body := `{"token":"tok-counter","calls":[{"path":"/append","body":{"scope":"events","payload":{"v":1}}}]}`
	if code, _, raw := doRequest(t, h, "POST", "/guarded", body); code != 200 {
		t.Fatalf("first /guarded code=%d body=%s", code, raw)
	}

	// /admin /tail _counters_count_calls should now show one item with
	// our capID.
	tailBody := `{"calls":[{"path":"/tail","query":{"scope":"_counters_count_calls","limit":10}}]}`
	code, out, raw := doRequest(t, h, "POST", "/admin", tailBody)
	if code != 200 {
		t.Fatalf("admin tail code=%d body=%s", code, raw)
	}
	results := out["results"].([]interface{})
	tailResp := results[0].(map[string]interface{})["body"].(map[string]interface{})
	items, _ := tailResp["items"].([]interface{})
	if len(items) != 1 {
		t.Fatalf("expected 1 counter item, got %d (body=%s)", len(items), raw)
	}
	first := items[0].(map[string]interface{})
	if id, _ := first["id"].(string); id != capID {
		t.Errorf("counter id=%q want %q", id, capID)
	}
	if v, _ := first["payload"].(float64); int64(v) != 1 {
		t.Errorf("counter value=%v want 1 (one call)", v)
	}
}

// First /guarded call is a read (not a write). Counter scope must
// auto-create just the same — handleGuarded calls
// guardedIncrementCounters after the dispatch loop regardless of
// whether the sub-calls were reads or writes, but no test pinned that
// to a read code path until now.
func TestGuarded_CounterCreatesOnFirstRead(t *testing.T) {
	h, api := newTestHandler(100)
	provisionTenantScope(t, h, "tok-readfirst", "events")
	capID := computeCapForTest(testServerSecret, "tok-readfirst")

	// Counter scope must not exist yet.
	if _, ok := api.store.getScope(countersScopeCalls); ok {
		t.Fatal("counter scope already exists before first /guarded call")
	}

	// First /guarded call is a /get, not a /append. The provisioned
	// scope is empty so /get returns hit:false — that still counts as
	// cache work and must increment the calls counter.
	body := `{"token":"tok-readfirst","calls":[{"path":"/get","query":{"scope":"events","id":"none"}}]}`
	if code, _, raw := doRequest(t, h, "POST", "/guarded", body); code != 200 {
		t.Fatalf("first /guarded (read) code=%d body=%s", code, raw)
	}

	// Counter scope now exists; the entry for our capID has value 1.
	// Use the public /admin /get path instead of poking buf internals
	// — keeps the test transport-shaped and reuses the dispatch logic
	// the production code path uses.
	getBody := `{"calls":[{"path":"/get","query":{"scope":"_counters_count_calls","id":"` + capID + `"}}]}`
	code, out, raw := doRequest(t, h, "POST", "/admin", getBody)
	if code != 200 {
		t.Fatalf("admin get code=%d body=%s", code, raw)
	}
	results := out["results"].([]interface{})
	getResp := results[0].(map[string]interface{})["body"].(map[string]interface{})
	hit, _ := getResp["hit"].(bool)
	if !hit {
		t.Fatalf("counter for capID %q missing (body=%s)", capID, raw)
	}
	item := getResp["item"].(map[string]interface{})
	if v, _ := item["payload"].(float64); int64(v) != 1 {
		t.Errorf("counter value=%v want 1 (one read)", v)
	}
}

// _counters_count_kb skips the increment when the rounded KiB value is
// 0, so a small /guarded response (well under 1 KiB) must NOT cause
// the kb counter scope to come into existence. Bookends the calls
// counter test: calls always increments (so the scope always exists
// after one call), kb is conditional.
func TestGuarded_KBCounterSkippedForTinyResponse(t *testing.T) {
	h, api := newTestHandler(100)
	provisionTenantScope(t, h, "tok-tiny", "events")

	// One /guarded /append with a 1-byte payload — outer envelope is a
	// few hundred bytes, way under 1 KiB.
	body := `{"token":"tok-tiny","calls":[{"path":"/append","body":{"scope":"events","id":"x","payload":1}}]}`
	if code, _, raw := doRequest(t, h, "POST", "/guarded", body); code != 200 {
		t.Fatalf("tiny /guarded code=%d body=%s", code, raw)
	}

	// Calls counter exists (every successful /guarded bumps it).
	if _, ok := api.store.getScope(countersScopeCalls); !ok {
		t.Error("_counters_count_calls missing — calls increment must always fire")
	}
	// KB counter does NOT exist — sub-1-KiB response triggers the
	// kb > 0 skip in guardedIncrementCounters.
	if _, ok := api.store.getScope(countersScopeKB); ok {
		t.Error("_counters_count_kb created for sub-1-KiB response — kb-skip rule violated")
	}
}

// A batch of N sub-calls bumps _counters_count_calls by N (not 1) — the
// counter measures cache work, not HTTP requests, so a tenant who batches
// their work consumes the same number of "calls" as a tenant making N
// solo /guarded calls.
func TestGuarded_CounterIncrementsPerSubCall(t *testing.T) {
	h, _ := newTestHandler(100)
	provisionTenantScope(t, h, "tok-batch", "events")
	capID := computeCapForTest(testServerSecret, "tok-batch")

	// Single /guarded request with 3 sub-calls. Counter should land on 3.
	body := `{"token":"tok-batch","calls":[
		{"path":"/append","body":{"scope":"events","id":"a","payload":1}},
		{"path":"/append","body":{"scope":"events","id":"b","payload":2}},
		{"path":"/append","body":{"scope":"events","id":"c","payload":3}}
	]}`
	if code, _, raw := doRequest(t, h, "POST", "/guarded", body); code != 200 {
		t.Fatalf("batch /guarded code=%d body=%s", code, raw)
	}

	getBody := `{"calls":[{"path":"/get","query":{"scope":"_counters_count_calls","id":"` + capID + `"}}]}`
	code, out, raw := doRequest(t, h, "POST", "/admin", getBody)
	if code != 200 {
		t.Fatalf("admin get code=%d body=%s", code, raw)
	}
	results := out["results"].([]interface{})
	getResp := results[0].(map[string]interface{})["body"].(map[string]interface{})
	item := getResp["item"].(map[string]interface{})
	if v, _ := item["payload"].(float64); int64(v) != 3 {
		t.Errorf("counter value=%v want 3 (3 sub-calls in one batch)", v)
	}

	// A second batch of 2 sub-calls should land at 5.
	body2 := `{"token":"tok-batch","calls":[
		{"path":"/get","query":{"scope":"events","id":"a"}},
		{"path":"/get","query":{"scope":"events","id":"b"}}
	]}`
	if code, _, raw := doRequest(t, h, "POST", "/guarded", body2); code != 200 {
		t.Fatalf("second batch code=%d body=%s", code, raw)
	}

	code, out, _ = doRequest(t, h, "POST", "/admin", getBody)
	if code != 200 {
		t.Fatalf("admin get post-second code=%d", code)
	}
	results = out["results"].([]interface{})
	getResp = results[0].(map[string]interface{})["body"].(map[string]interface{})
	item = getResp["item"].(map[string]interface{})
	if v, _ := item["payload"].(float64); int64(v) != 5 {
		t.Errorf("counter value=%v want 5 (3 + 2 sub-calls)", v)
	}
}

// After /wipe the counter scopes are gone. The next /guarded call must
// re-provision them via ensureScope.
func TestGuarded_CountersSelfHealAfterWipe(t *testing.T) {
	h, _ := newTestHandler(100)
	provisionTenantScope(t, h, "tok-heal", "events")

	// First call creates counter scope.
	doRequest(t, h, "POST", "/guarded", `{"token":"tok-heal","calls":[{"path":"/append","body":{"scope":"events","payload":{"v":1}}}]}`)

	// /admin /wipe clears everything including counters.
	if code, _, raw := doRequest(t, h, "POST", "/admin", `{"calls":[{"path":"/wipe"}]}`); code != 200 {
		t.Fatalf("wipe code=%d body=%s", code, raw)
	}

	// Re-provision tenant scope (was wiped too).
	provisionTenantScope(t, h, "tok-heal", "events")

	// Next /guarded call: counter scopes should be re-created automatically.
	if code, _, raw := doRequest(t, h, "POST", "/guarded", `{"token":"tok-heal","calls":[{"path":"/append","body":{"scope":"events","payload":{"v":2}}}]}`); code != 200 {
		t.Fatalf("post-wipe /guarded code=%d body=%s", code, raw)
	}

	// Verify counter scopes re-exist.
	code, out, _ := doRequest(t, h, "POST", "/admin", `{"calls":[{"path":"/stats"}]}`)
	if code != 200 {
		t.Fatalf("stats code=%d", code)
	}
	results := out["results"].([]interface{})
	statsBody := results[0].(map[string]interface{})["body"].(map[string]interface{})
	scopes := statsBody["scopes"].(map[string]interface{})
	if _, ok := scopes["_counters_count_calls"]; !ok {
		t.Errorf("_counters_count_calls missing after self-heal")
	}
}

// --- input shape --------------------------------------------------------------

func TestGuarded_GETRejected(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "GET", "/guarded", "")
	if code != http.StatusMethodNotAllowed {
		t.Fatalf("code=%d want 405", code)
	}
}

func TestGuarded_MalformedBody(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "POST", "/guarded", `{not-json`)
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
}

func TestGuarded_MissingCallsField(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "POST", "/guarded", `{"token":"x"}`)
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
}

// --- two-tenant isolation -----------------------------------------------------

// Tenant A's /append lands in A's namespace; tenant B (with a different
// token) reads B's namespace and sees nothing of A's.
func TestGuarded_TenantIsolation(t *testing.T) {
	h, _ := newTestHandler(100)
	provisionTenantScope(t, h, "tenant-A", "events")
	provisionTenantScope(t, h, "tenant-B", "events")

	// Tenant A appends.
	doRequest(t, h, "POST", "/guarded", `{"token":"tenant-A","calls":[{"path":"/append","body":{"scope":"events","id":"a-only","payload":{"v":1}}}]}`)

	// Tenant B reads — should see nothing.
	code, out, raw := doRequest(t, h, "POST", "/guarded", `{"token":"tenant-B","calls":[{"path":"/get","query":{"scope":"events","id":"a-only"}}]}`)
	if code != 200 {
		t.Fatalf("B get code=%d body=%s", code, raw)
	}
	_, slotBody := guardedSlot(t, out, 0)
	if hit, _ := slotBody["hit"].(bool); hit {
		t.Errorf("tenant B saw tenant A's data: %v", slotBody)
	}
}

// --- /guarded disabled when SERVER_SECRET unset -------------------------------

func TestGuarded_NotRegisteredWithoutSecret(t *testing.T) {
	api := NewAPI(NewStore(Config{
		ScopeMaxItems:     10,
		MaxStoreBytes:     100 << 20,
		MaxItemBytes:      1 << 20,
		MaxResponseBytes:  25 << 20,
		MaxMultiCallBytes: 16 << 20,
		MaxMultiCallCount: 10,
		// ServerSecret deliberately empty.
	}))
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	code, _, _ := doRequest(t, mux, "POST", "/guarded", `{"token":"x","calls":[]}`)
	if code != 404 {
		t.Fatalf("code=%d want 404 (no SERVER_SECRET, /guarded should not be registered)", code)
	}
}

// --- routing self-check -------------------------------------------------------

func TestGuarded_HandlerReachableViaMux(t *testing.T) {
	h, _ := newTestHandler(10)
	provisionTenantScope(t, h, "tok-mux", "events")
	body := `{"token":"tok-mux","calls":[{"path":"/get","query":{"scope":"events","id":"missing"}}]}`
	code, _, raw := doRequest(t, h, "POST", "/guarded", body)
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
