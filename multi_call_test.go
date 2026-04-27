package scopecache

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// multiCallResults extracts the results array from a /multi_call response,
// failing the test if the shape is wrong. Each entry is the parsed slot
// object (status + body).
func multiCallResults(t *testing.T, out map[string]interface{}) []map[string]interface{} {
	t.Helper()
	raw, ok := out["results"]
	if !ok {
		t.Fatalf("missing 'results' in response: %+v", out)
	}
	arr, ok := raw.([]interface{})
	if !ok {
		t.Fatalf("'results' is not array: %v", raw)
	}
	slots := make([]map[string]interface{}, len(arr))
	for i, v := range arr {
		slot, ok := v.(map[string]interface{})
		if !ok {
			t.Fatalf("results[%d] is not object: %v", i, v)
		}
		slots[i] = slot
	}
	return slots
}

// slotStatus returns the integer status of a parsed multi_call slot.
func slotStatus(t *testing.T, slot map[string]interface{}) int {
	t.Helper()
	v, ok := slot["status"]
	if !ok {
		t.Fatalf("slot missing 'status': %v", slot)
	}
	n, ok := v.(float64)
	if !ok {
		t.Fatalf("slot status not number: %v", v)
	}
	return int(n)
}

// slotBody returns the parsed body of a multi_call slot as a map.
func slotBody(t *testing.T, slot map[string]interface{}) map[string]interface{} {
	t.Helper()
	v, ok := slot["body"]
	if !ok {
		t.Fatalf("slot missing 'body': %v", slot)
	}
	m, ok := v.(map[string]interface{})
	if !ok {
		t.Fatalf("slot body not object: %v", v)
	}
	return m
}

// --- happy path ---------------------------------------------------------------

func TestMultiCall_MixedReadWriteRead(t *testing.T) {
	h, _ := newTestHandler(100)

	// Seed thread:432 with one item so the first /get hits.
	if code, _, raw := doRequest(t, h, "POST", "/append", `{"scope":"thread:432","id":"msg1","payload":{"text":"seed"}}`); code != 200 {
		t.Fatalf("seed /append: code=%d body=%s", code, raw)
	}

	body := `{"calls": [
		{"path": "/get",    "query": {"scope": "thread:432", "id": "msg1"}},
		{"path": "/append", "body":  {"scope": "thread:900", "id": "post_1", "payload": {"text": "hello"}}},
		{"path": "/tail",   "query": {"scope": "thread:900", "limit": 10}}
	]}`

	code, out, raw := doRequest(t, h, "POST", "/multi_call", body)
	if code != 200 {
		t.Fatalf("code=%d want 200, body=%s", code, raw)
	}
	if !mustBool(t, out, "ok") {
		t.Fatalf("ok=false: %s", raw)
	}
	if n := mustFloat(t, out, "count"); n != 3 {
		t.Errorf("count=%v want 3", n)
	}
	if _, ok := out["approx_response_mb"]; !ok {
		t.Errorf("missing approx_response_mb in outer envelope: %s", raw)
	}
	if _, ok := out["duration_us"]; !ok {
		t.Errorf("missing duration_us in outer envelope: %s", raw)
	}

	slots := multiCallResults(t, out)
	if len(slots) != 3 {
		t.Fatalf("len(results)=%d want 3", len(slots))
	}

	// Slot 0: /get hit on thread:432/msg1
	if got := slotStatus(t, slots[0]); got != 200 {
		t.Errorf("slot0 status=%d want 200", got)
	}
	b0 := slotBody(t, slots[0])
	if hit, _ := b0["hit"].(bool); !hit {
		t.Errorf("slot0 hit=false want true: %v", b0)
	}

	// Slot 1: /append succeeded
	if got := slotStatus(t, slots[1]); got != 200 {
		t.Errorf("slot1 status=%d want 200", got)
	}
	b1 := slotBody(t, slots[1])
	if ok, _ := b1["ok"].(bool); !ok {
		t.Errorf("slot1 ok=false: %v", b1)
	}

	// Slot 2: /tail saw the append at index 1 (sequential dispatch)
	if got := slotStatus(t, slots[2]); got != 200 {
		t.Errorf("slot2 status=%d want 200", got)
	}
	b2 := slotBody(t, slots[2])
	if cnt, _ := b2["count"].(float64); cnt != 1 {
		t.Errorf("slot2 count=%v want 1 (the just-appended item)", cnt)
	}
}

func TestMultiCall_EmptyCallsArray(t *testing.T) {
	h, _ := newTestHandler(10)
	code, out, raw := doRequest(t, h, "POST", "/multi_call", `{"calls": []}`)
	if code != 200 {
		t.Fatalf("code=%d want 200, body=%s", code, raw)
	}
	if !mustBool(t, out, "ok") {
		t.Errorf("ok=false: %s", raw)
	}
	if n := mustFloat(t, out, "count"); n != 0 {
		t.Errorf("count=%v want 0", n)
	}
	slots := multiCallResults(t, out)
	if len(slots) != 0 {
		t.Errorf("len(results)=%d want 0", len(slots))
	}
}

func TestMultiCall_MissingCallsField(t *testing.T) {
	h, _ := newTestHandler(10)
	// `{}` parses successfully but Calls pointer stays nil → 400.
	code, out, _ := doRequest(t, h, "POST", "/multi_call", `{}`)
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
	if mustBool(t, out, "ok") {
		t.Error("ok=true for missing calls field")
	}
}

func TestMultiCall_GETRejected(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "GET", "/multi_call", "")
	if code != http.StatusMethodNotAllowed {
		t.Fatalf("code=%d want 405", code)
	}
}

// --- whitelist enforcement ----------------------------------------------------

func TestMultiCall_PathNotInWhitelist(t *testing.T) {
	h, _ := newTestHandler(10)
	body := `{"calls": [
		{"path": "/get", "query": {"scope": "s", "id": "a"}},
		{"path": "/wipe"}
	]}`
	code, out, _ := doRequest(t, h, "POST", "/multi_call", body)
	if code != 400 {
		t.Fatalf("code=%d want 400 (whole batch rejected on whitelist miss)", code)
	}
	if mustBool(t, out, "ok") {
		t.Error("ok=true on whitelist reject")
	}
}

func TestMultiCall_ExcludedPaths(t *testing.T) {
	// Each of these must be rejected — store-wide ops, raw-byte /render, and
	// /multi_call itself are deliberately excluded.
	excluded := []string{"/warm", "/rebuild", "/wipe", "/delete_scope", "/render", "/help", "/multi_call"}
	for _, p := range excluded {
		t.Run(strings.TrimPrefix(p, "/"), func(t *testing.T) {
			h, _ := newTestHandler(10)
			body := fmt.Sprintf(`{"calls": [{"path": "%s"}]}`, p)
			code, _, raw := doRequest(t, h, "POST", "/multi_call", body)
			if code != 400 {
				t.Fatalf("path=%s code=%d want 400, body=%s", p, code, raw)
			}
		})
	}
}

func TestMultiCall_UnknownPath(t *testing.T) {
	h, _ := newTestHandler(10)
	body := `{"calls": [{"path": "/does-not-exist"}]}`
	code, _, _ := doRequest(t, h, "POST", "/multi_call", body)
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
}

// --- caps ---------------------------------------------------------------------

func TestMultiCall_CountOverflow(t *testing.T) {
	h, _ := newTestHandler(10)
	// Default test handler has MaxMultiCallCount=10. Build 11 calls.
	calls := make([]string, 0, 11)
	for i := 0; i < 11; i++ {
		calls = append(calls, fmt.Sprintf(`{"path": "/get", "query": {"scope": "s", "id": "x%d"}}`, i))
	}
	body := `{"calls": [` + strings.Join(calls, ",") + `]}`
	code, out, _ := doRequest(t, h, "POST", "/multi_call", body)
	if code != 400 {
		t.Fatalf("code=%d want 400 on count overflow", code)
	}
	if mustBool(t, out, "ok") {
		t.Error("ok=true on count overflow")
	}
}

func TestMultiCall_BodyOverflow(t *testing.T) {
	// Build a tiny-cap handler so we can blow the body budget cheaply.
	api := NewAPI(NewStore(Config{
		ScopeMaxItems:     10,
		MaxStoreBytes:     100 << 20,
		MaxItemBytes:      1 << 20,
		MaxResponseBytes:  25 << 20,
		MaxMultiCallBytes: 64, // ridiculously small
		MaxMultiCallCount: 10,
	}))
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	body := `{"calls": [{"path": "/get", "query": {"scope": "s", "id": "a"}}, {"path": "/get", "query": {"scope": "s", "id": "b"}}]}`
	code, _, _ := doRequest(t, mux, "POST", "/multi_call", body)
	if code != 400 {
		t.Fatalf("code=%d want 400 on body overflow", code)
	}
}

// --- query value coercion -----------------------------------------------------

func TestMultiCall_QueryNumberAndBool(t *testing.T) {
	h, _ := newTestHandler(10)
	// Seed scope.
	for i := 0; i < 3; i++ {
		_, _, _ = doRequest(t, h, "POST", "/append", fmt.Sprintf(`{"scope":"s","payload":{"v":%d}}`, i))
	}

	// limit is a number; seed it as raw JSON 2 in the query map.
	body := `{"calls": [{"path": "/tail", "query": {"scope": "s", "limit": 2}}]}`
	code, out, raw := doRequest(t, h, "POST", "/multi_call", body)
	if code != 200 {
		t.Fatalf("code=%d body=%s", code, raw)
	}
	slots := multiCallResults(t, out)
	if got := slotStatus(t, slots[0]); got != 200 {
		t.Fatalf("slot0 status=%d want 200, body=%s", got, raw)
	}
	b := slotBody(t, slots[0])
	if cnt, _ := b["count"].(float64); cnt != 2 {
		t.Errorf("count=%v want 2 (limit honoured), body=%s", cnt, raw)
	}
}

func TestMultiCall_QueryNestedRejected(t *testing.T) {
	h, _ := newTestHandler(10)
	// nested object in a query value is rejected — would silently lose shape
	// when flattened to URL query string.
	body := `{"calls": [{"path": "/get", "query": {"scope": {"nested": true}}}]}`
	code, _, _ := doRequest(t, h, "POST", "/multi_call", body)
	if code != 400 {
		t.Fatalf("code=%d want 400 on nested query value", code)
	}
}

// Pre-flight: when MaxResponseBytes is too small to fit even an
// all-trimmed envelope for the requested batch size, /multi_call must
// reject with 507 BEFORE any sub-call runs. Without this the dispatch
// loop would land side effects, the trim mechanism would still
// produce more bytes than respCap, and the outer capResponse wrapper
// would 507 the whole envelope — losing all per-slot status info that
// is the whole point of /multi_call.
func TestMultiCall_TinyResponseCapRejectedPreflight(t *testing.T) {
	// 200 bytes is below minMultiCallResponseSize for any non-trivial
	// batch (envelope ~256 alone). Single /append batch easily trips it.
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

	body := `{"calls":[{"path":"/append","body":{"scope":"x","id":"a","payload":1}}]}`
	code, out, raw := doRequest(t, mux, "POST", "/multi_call", body)
	if code != http.StatusInsufficientStorage {
		t.Fatalf("code=%d want 507 (preflight cap), body=%s", code, raw)
	}
	if errStr, _ := out["error"].(string); !strings.Contains(errStr, "response cap too small") {
		t.Errorf("error message does not name the response cap: %s", raw)
	}

	// Side-effect-free: the scope must not have been created.
	if _, ok := api.store.getScope("x"); ok {
		t.Errorf("preflight reject leaked side effect: scope x was created")
	}
}

// Pre-fix /multi_call validated the path whitelist up-front but built
// each subURL inside the dispatch loop. A malformed query on calls[k]
// then 400'd the whole batch only after calls[0..k-1] had committed.
// Post-fix prepareSubCalls runs before any dispatch, so a side effect
// at slot 0 cannot land if slot 1's query is unreachable.
func TestMultiCall_NestedQueryRejectsBeforeSideEffects(t *testing.T) {
	h, _ := newTestHandler(10)

	// calls[0] is a valid /append on scope "preflight". calls[1] has a
	// nested-object query that buildSubURL must reject. Without the fix
	// the /append commits and the response is still 400.
	body := `{
		"calls": [
			{"path": "/append", "body": {"scope": "preflight", "id": "a", "payload": {"v": 1}}},
			{"path": "/get",    "query": {"scope": {"nested": true}}}
		]
	}`
	code, _, raw := doRequest(t, h, "POST", "/multi_call", body)
	if code != 400 {
		t.Fatalf("nested-query batch: code=%d want 400, body=%s", code, raw)
	}

	// Verify nothing committed: scope "preflight" must not exist.
	code, out, raw := doRequest(t, h, "GET", "/get?scope=preflight&id=a", "")
	if code != 200 {
		t.Fatalf("post-batch /get: code=%d body=%s", code, raw)
	}
	if hit, _ := out["hit"].(bool); hit {
		t.Errorf("calls[0] /append leaked despite calls[1] rejection: %s", raw)
	}
}

// --- sub-call response cap ----------------------------------------------------

// TestMultiCall_SubCallResponseTooLarge fills a scope with items large enough
// that /tail's response exceeds the per-response cap. The slot must carry 507,
// the batch must continue (in this case, only one slot).
func TestMultiCall_SubCallResponseTooLarge(t *testing.T) {
	// 1 MiB per-response cap; one /tail of ~3 items at ~512 KiB each will
	// trip the cap on the *sub-call*, producing 507 in the slot.
	api := NewAPI(NewStore(Config{
		ScopeMaxItems:     100,
		MaxStoreBytes:     100 << 20,
		MaxItemBytes:      1 << 20,
		MaxResponseBytes:  1 << 20, // 1 MiB
		MaxMultiCallBytes: 16 << 20,
		MaxMultiCallCount: 10,
	}))
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	// Seed three items, each ~400 KiB of payload — comfortably under the
	// per-item cap (1 MiB), but three of them blow the per-response cap.
	bigFiller := strings.Repeat("x", 400*1024)
	for i := 0; i < 3; i++ {
		body := fmt.Sprintf(`{"scope":"big","id":"i%d","payload":{"data":%q}}`, i, bigFiller)
		if code, _, raw := doRequest(t, mux, "POST", "/append", body); code != 200 {
			t.Fatalf("seed append %d: code=%d body=%s", i, code, raw)
		}
	}

	body := `{"calls": [{"path": "/tail", "query": {"scope": "big", "limit": 10}}]}`
	code, out, raw := doRequest(t, mux, "POST", "/multi_call", body)
	if code != 200 {
		t.Fatalf("outer code=%d want 200, body=%s", code, raw)
	}
	slots := multiCallResults(t, out)
	if len(slots) != 1 {
		t.Fatalf("len(slots)=%d want 1", len(slots))
	}
	if got := slotStatus(t, slots[0]); got != http.StatusInsufficientStorage {
		t.Errorf("slot0 status=%d want 507, body=%s", got, raw)
	}
}

// --- outer envelope cap (asymmetric trim) -------------------------------------

// TestMultiCall_OuterEnvelopeTrimSuccess accumulates several 2xx /tail calls
// whose combined body bytes exceed the per-response cap. Sub-calls past the
// budget should keep status 200 but get the success-truncation marker.
func TestMultiCall_OuterEnvelopeTrimSuccess(t *testing.T) {
	// 2 MiB cap. Each sub-call returns ~800 KiB of body; 3+ of those overflow
	// the outer envelope so the later slots get the {"ok":true,"response_truncated":true}
	// marker but keep status 200.
	api := NewAPI(NewStore(Config{
		ScopeMaxItems:     100,
		MaxStoreBytes:     100 << 20,
		MaxItemBytes:      1 << 20,
		MaxResponseBytes:  2 << 20,
		MaxMultiCallBytes: 16 << 20,
		MaxMultiCallCount: 10,
	}))
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	// Seed a small scope: one item with a moderate payload — each /tail
	// returns one item. Five /tail sub-calls produce ~5 × 600 KiB = 3 MiB.
	filler := strings.Repeat("y", 600*1024)
	body := fmt.Sprintf(`{"scope":"s","id":"i","payload":{"data":%q}}`, filler)
	if code, _, raw := doRequest(t, mux, "POST", "/append", body); code != 200 {
		t.Fatalf("seed append: code=%d body=%s", code, raw)
	}

	calls := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		calls = append(calls, `{"path": "/tail", "query": {"scope": "s", "limit": 1}}`)
	}
	mcBody := `{"calls": [` + strings.Join(calls, ",") + `]}`

	code, out, raw := doRequest(t, mux, "POST", "/multi_call", mcBody)
	if code != 200 {
		t.Fatalf("outer code=%d want 200, body=%s", code, raw)
	}
	slots := multiCallResults(t, out)
	if len(slots) != 5 {
		t.Fatalf("len(slots)=%d want 5", len(slots))
	}

	// At least one later slot must have the truncation marker but keep 200.
	trimmed := 0
	for _, s := range slots {
		if slotStatus(t, s) != 200 {
			t.Errorf("expected every slot status=200, got %d", slotStatus(t, s))
		}
		b := slotBody(t, s)
		if v, _ := b["response_truncated"].(bool); v {
			trimmed++
		}
	}
	if trimmed == 0 {
		t.Errorf("expected at least one truncated slot, got body=%s", raw)
	}
}

// --- side-effect non-rollback -------------------------------------------------

// TestMultiCall_SideEffectsNotRolledBack writes one item, then issues an
// invalid sub-call that forces a 400 in its slot. The first write must remain
// applied — no cross-call atomicity, by design.
func TestMultiCall_SideEffectsNotRolledBack(t *testing.T) {
	h, _ := newTestHandler(10)
	body := `{"calls": [
		{"path": "/append", "body": {"scope": "s", "id": "alive", "payload": {"v": 1}}},
		{"path": "/get",    "query": {"scope": "s"}}
	]}`
	code, out, raw := doRequest(t, h, "POST", "/multi_call", body)
	if code != 200 {
		t.Fatalf("outer code=%d want 200, body=%s", code, raw)
	}
	slots := multiCallResults(t, out)
	if len(slots) != 2 {
		t.Fatalf("len(slots)=%d want 2", len(slots))
	}
	if got := slotStatus(t, slots[0]); got != 200 {
		t.Errorf("slot0 (write) status=%d want 200", got)
	}
	if got := slotStatus(t, slots[1]); got != 400 {
		t.Errorf("slot1 (invalid /get without id/seq) status=%d want 400", got)
	}

	// Write at slot 0 must have landed — verify via standalone /get.
	code2, out2, raw2 := doRequest(t, h, "GET", "/get?scope=s&id=alive", "")
	if code2 != 200 {
		t.Fatalf("post-batch /get code=%d body=%s", code2, raw2)
	}
	if hit, _ := out2["hit"].(bool); !hit {
		t.Errorf("expected slot0 write to remain applied; hit=%v body=%s", out2["hit"], raw2)
	}
}

// --- input shape validation ---------------------------------------------------

func TestMultiCall_MalformedJSON(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "POST", "/multi_call", `{"calls": [`)
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
}

func TestMultiCall_InputBodyOversize(t *testing.T) {
	// Build a request body large enough to trip the 16 MiB default cap.
	// Cheaper to use a small-cap handler.
	api := NewAPI(NewStore(Config{
		ScopeMaxItems:     10,
		MaxStoreBytes:     100 << 20,
		MaxItemBytes:      1 << 20,
		MaxResponseBytes:  25 << 20,
		MaxMultiCallBytes: 1024,
		MaxMultiCallCount: 10,
	}))
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	bigPad := strings.Repeat("a", 4096)
	body := fmt.Sprintf(`{"calls":[{"path":"/append","body":{"scope":"s","id":"i","payload":{"data":%q}}}]}`, bigPad)
	code, _, _ := doRequest(t, mux, "POST", "/multi_call", body)
	if code != 400 {
		t.Fatalf("code=%d want 400 on input-body oversize", code)
	}
}

// --- routing self-check -------------------------------------------------------

// TestMultiCall_HandlerReachableViaMux confirms the /multi_call handler is
// actually wired by RegisterRoutes (would be a silent miss otherwise: the
// dispatcher functions exist independently of mux registration).
func TestMultiCall_HandlerReachableViaMux(t *testing.T) {
	h, _ := newTestHandler(10)
	req := httptest.NewRequest(http.MethodPost, "/multi_call", strings.NewReader(`{"calls": []}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d want 200", rec.Code)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("non-JSON body: %s", rec.Body.String())
	}
	if _, ok := out["results"]; !ok {
		t.Errorf("missing results: %s", rec.Body.String())
	}
}
