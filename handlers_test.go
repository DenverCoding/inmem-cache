package scopecache

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func newTestHandler(maxItems int) (http.Handler, *API) {
	// 100 MiB byte budget is more than enough for handler tests with tiny
	// payloads; dedicated byte-cap behaviour tests construct stores with a
	// small maxStoreBytes so their writes can fail the store cap on purpose.
	api := NewAPI(
		NewGateway(Config{ScopeMaxItems: maxItems, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20}),
		APIConfig{},
	)
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)
	return mux, api
}

func doRequest(t *testing.T, h http.Handler, method, path, body string) (int, map[string]interface{}, string) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if r != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	raw := rec.Body.String()
	var out map[string]interface{}
	if strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		_ = json.Unmarshal([]byte(raw), &out)
	}
	return rec.Code, out, raw
}

// doAdminRequest is a thin compatibility shim left over from when /wipe,
// /warm, /rebuild, /delete_scope, /stats lived behind /admin's envelope.
// Those endpoints are now public on the mux (no admin gate, no envelope —
// see core-and-addons.md), so this helper just dispatches directly with
// the right HTTP method. Kept rather than renamed so existing test
// callers stay unchanged during the refactor; new tests should call
// doRequest directly.
func doAdminRequest(t *testing.T, h http.Handler, path, body string) (int, map[string]interface{}, string) {
	t.Helper()
	method := http.MethodPost
	if path == "/stats" {
		method = http.MethodGet
	}
	return doRequest(t, h, method, path, body)
}

func mustBool(t *testing.T, m map[string]interface{}, key string) bool {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("missing key %q in response: %+v", key, m)
	}
	b, ok := v.(bool)
	if !ok {
		t.Fatalf("key %q is not bool: %v", key, v)
	}
	return b
}

func mustFloat(t *testing.T, m map[string]interface{}, key string) float64 {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("missing key %q in response: %+v", key, m)
	}
	n, ok := v.(float64)
	if !ok {
		t.Fatalf("key %q is not a number: %v", key, v)
	}
	return n
}

// --- /help --------------------------------------------------------------------

func TestHelp_GETReturnsText(t *testing.T) {
	h, _ := newTestHandler(10)
	req := httptest.NewRequest(http.MethodGet, "/help", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "scopecache") {
		t.Fatal("help body missing 'scopecache'")
	}
}

func TestHelp_POSTRejected(t *testing.T) {
	h, _ := newTestHandler(10)
	req := httptest.NewRequest(http.MethodPost, "/help", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("code=%d want 405", rec.Code)
	}
}

// --- /append ------------------------------------------------------------------

func TestAppend_Success(t *testing.T) {
	h, _ := newTestHandler(10)
	code, out, _ := doRequest(t, h, "POST", "/append", `{"scope":"s","id":"a","payload":{"v":1}}`)

	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if !mustBool(t, out, "ok") {
		t.Fatal("ok=false")
	}
	item, ok := out["item"].(map[string]interface{})
	if !ok {
		t.Fatalf("item not object: %v", out["item"])
	}
	if item["seq"].(float64) != 1 {
		t.Errorf("seq=%v want 1", item["seq"])
	}
	// ts is cache-owned: every item carries one after a write. Detailed
	// ts behaviour is exercised in ts_test.go.
	if _, hasTS := item["ts"]; !hasTS {
		t.Errorf("response item must carry a cache-assigned 'ts' field: %v", item)
	}
	// /append response echoes scope/id/seq/ts under "item", but NOT
	// payload — the client supplied that on the way in. This pin
	// catches a regression that would re-introduce the payload echo
	// (doubling wire cost on large writes).
	if _, present := item["payload"]; present {
		t.Errorf("/append response item must not echo payload back: %v", item)
	}
}

// Drives a 200-byte scope and 200-byte id end-to-end: over the pre-v0.5.11
// cap of 128 but under the new 256 cap. Both must be accepted by the
// validator and round-trip through /get unchanged. Anchors the "long
// scope/id keys reach the public mux without being truncated or
// rejected" property.
func TestAppend_Acceps200ByteScopeAndID(t *testing.T) {
	h, _ := newTestHandler(10)
	longScope := strings.Repeat("s", 200)
	longID := strings.Repeat("i", 200)

	body := fmt.Sprintf(`{"scope":%q,"id":%q,"payload":{"v":1}}`, longScope, longID)
	if code, _, raw := doRequest(t, h, "POST", "/append", body); code != 200 {
		t.Fatalf("append 200/200: code=%d body=%s", code, raw)
	}

	getURL := fmt.Sprintf("/get?scope=%s&id=%s", longScope, longID)
	code, out, raw := doRequest(t, h, "GET", getURL, "")
	if code != 200 {
		t.Fatalf("get: code=%d body=%s", code, raw)
	}
	if !mustBool(t, out, "hit") {
		t.Errorf("get returned hit=false: %s", raw)
	}
	item := out["item"].(map[string]interface{})
	if item["scope"].(string) != longScope {
		t.Errorf("scope round-trip mismatch")
	}
	if item["id"].(string) != longID {
		t.Errorf("id round-trip mismatch")
	}
}

func TestAppend_MissingPayload(t *testing.T) {
	h, _ := newTestHandler(10)
	code, out, _ := doRequest(t, h, "POST", "/append", `{"scope":"s","id":"a"}`)
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
	if mustBool(t, out, "ok") {
		t.Error("ok=true for invalid request")
	}
}

func TestAppend_MissingScope(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "POST", "/append", `{"id":"a","payload":{"v":1}}`)
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
}

func TestAppend_SeqForbidden(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "POST", "/append", `{"scope":"s","seq":5,"payload":{"v":1}}`)
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
}

func TestAppend_DuplicateIDReturns409(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","id":"a","payload":{"v":1}}`)
	code, _, _ := doRequest(t, h, "POST", "/append", `{"scope":"s","id":"a","payload":{"v":2}}`)
	if code != 409 {
		t.Fatalf("code=%d want 409", code)
	}
}

func TestAppend_InvalidJSON(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "POST", "/append", `not json`)
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
}

// Guards decodeBody against a json.Decoder quirk: without a trailing-EOF
// check, a body containing two concatenated JSON values (or one value plus
// garbage) would silently decode the first and ignore the rest.
func TestAppend_RejectsTrailingContent(t *testing.T) {
	cases := map[string]string{
		"two objects":      `{"scope":"x","payload":{"v":1}}{"scope":"y","payload":{"v":2}}`,
		"trailing garbage": `{"scope":"x","payload":{"v":1}} garbage`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			h, api := newTestHandler(10)
			code, _, _ := doRequest(t, h, "POST", "/append", body)
			if code != 400 {
				t.Fatalf("code=%d want 400", code)
			}
			if _, ok := api.store.getScope("x"); ok {
				t.Fatalf("scope 'x' must not exist: the first value must not be committed")
			}
		})
	}
}

func TestAppend_MethodNotAllowed(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "GET", "/append", "")
	if code != http.StatusMethodNotAllowed {
		t.Fatalf("code=%d want 405", code)
	}
}

// --- /warm --------------------------------------------------------------------

func TestWarm_LeavesOtherScopesUntouched(t *testing.T) {
	h, api := newTestHandler(10)

	keep, _ := api.store.getOrCreateScope("keep")
	_, _ = keep.appendItem(newItem("keep", "k1", nil))

	body := `{"items":[
		{"scope":"target","id":"t1","payload":{"v":1}},
		{"scope":"target","id":"t2","payload":{"v":2}}
	]}`
	code, out, _ := doAdminRequest(t, h, "/warm", body)
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if mustFloat(t, out, "replaced_scopes") != 1 {
		t.Errorf("replaced_scopes=%v want 1", out["replaced_scopes"])
	}

	kept, _ := api.store.getScope("keep")
	if len(kept.items) != 1 {
		t.Fatalf("untouched scope lost items: %d", len(kept.items))
	}
}

// Duplicate id within the same scope in /warm input is a request-shape
// problem (the same primary-key field appears twice in one batch), not
// a conflict against existing state — so 400 is the right code, not
// 409. Pre-step-6.7 this returned 409 because dup-detection happened
// inside buildReplacementState and the handler defaulted unknown
// errors to 409; post-6.7 the validation wrap promotes it to 400.
func TestWarm_DuplicateIDInSameScope(t *testing.T) {
	h, _ := newTestHandler(10)
	body := `{"items":[
		{"scope":"s","id":"a","payload":{"v":1}},
		{"scope":"s","id":"a","payload":{"v":2}}
	]}`
	code, _, _ := doAdminRequest(t, h, "/warm", body)
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
}

func TestWarm_MissingScopeOnItem(t *testing.T) {
	h, _ := newTestHandler(10)
	body := `{"items":[{"id":"a","payload":{"v":1}}]}`
	code, _, _ := doAdminRequest(t, h, "/warm", body)
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
}

// --- /rebuild -----------------------------------------------------------------

func TestRebuild_WipesExistingScopes(t *testing.T) {
	h, api := newTestHandler(10)

	old, _ := api.store.getOrCreateScope("old")
	_, _ = old.appendItem(newItem("old", "", nil))

	body := `{"items":[{"scope":"new","id":"n1","payload":{"v":1}}]}`
	code, out, _ := doAdminRequest(t, h, "/rebuild", body)
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if mustFloat(t, out, "rebuilt_scopes") != 1 {
		t.Errorf("rebuilt_scopes=%v want 1", out["rebuilt_scopes"])
	}

	if _, ok := api.store.getScope("old"); ok {
		t.Fatal("old scope should be wiped")
	}
}

// An empty items[] would wipe the store. That's almost always a client bug,
// so /rebuild rejects it with 400 instead of silently clearing everything.
func TestRebuild_RejectsEmptyItems(t *testing.T) {
	h, api := newTestHandler(10)

	keep, _ := api.store.getOrCreateScope("keep")
	_, _ = keep.appendItem(newItem("keep", "k", nil))

	code, _, _ := doAdminRequest(t, h, "/rebuild", `{"items":[]}`)
	if code != 400 {
		t.Fatalf("code=%d want 400 on empty rebuild", code)
	}
	// Store must be untouched after the rejected rebuild.
	if _, ok := api.store.getScope("keep"); !ok {
		t.Fatal("keep scope was wiped despite rejected rebuild")
	}
}

// --- /update ------------------------------------------------------------------

func TestUpdate_Hit(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","id":"a","payload":{"v":1}}`)

	code, out, _ := doRequest(t, h, "POST", "/update", `{"scope":"s","id":"a","payload":{"v":2}}`)
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if !mustBool(t, out, "hit") {
		t.Error("hit=false")
	}
	if mustFloat(t, out, "updated_count") != 1 {
		t.Errorf("updated_count=%v want 1", out["updated_count"])
	}

	_, got, _ := doRequest(t, h, "GET", "/get?scope=s&id=a", "")
	item := got["item"].(map[string]interface{})
	if item["payload"].(map[string]interface{})["v"].(float64) != 2 {
		t.Error("payload not updated")
	}
}

func TestUpdate_MissScope(t *testing.T) {
	h, _ := newTestHandler(10)
	code, out, _ := doRequest(t, h, "POST", "/update", `{"scope":"none","id":"x","payload":{"v":1}}`)
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if mustBool(t, out, "hit") {
		t.Error("hit=true for missing scope")
	}
}

func TestUpdate_MissID(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","id":"a","payload":{"v":1}}`)
	code, out, _ := doRequest(t, h, "POST", "/update", `{"scope":"s","id":"zzz","payload":{"v":1}}`)
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if mustBool(t, out, "hit") {
		t.Error("hit=true for missing id")
	}
}

func TestUpdate_BySeq_Hit(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","payload":{"v":1}}`)

	code, out, _ := doRequest(t, h, "POST", "/update", `{"scope":"s","seq":1,"payload":{"v":9}}`)
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if !mustBool(t, out, "hit") {
		t.Error("hit=false")
	}

	_, got, _ := doRequest(t, h, "GET", "/get?scope=s&seq=1", "")
	item := got["item"].(map[string]interface{})
	if item["payload"].(map[string]interface{})["v"].(float64) != 9 {
		t.Error("payload not updated")
	}
}

func TestUpdate_BySeq_Miss(t *testing.T) {
	h, _ := newTestHandler(10)
	code, out, _ := doRequest(t, h, "POST", "/update", `{"scope":"s","seq":42,"payload":{"v":1}}`)
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if mustBool(t, out, "hit") {
		t.Error("hit=true for missing seq")
	}
}

func TestUpdate_RejectsBothIDAndSeq(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "POST", "/update", `{"scope":"s","id":"a","seq":1,"payload":{"v":1}}`)
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
}

func TestUpdate_RejectsNeitherIDNorSeq(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "POST", "/update", `{"scope":"s","payload":{"v":1}}`)
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
}

// --- /upsert ------------------------------------------------------------------

func TestUpsert_Creates(t *testing.T) {
	h, _ := newTestHandler(10)

	code, out, _ := doRequest(t, h, "POST", "/upsert", `{"scope":"s","id":"a","payload":{"v":1}}`)
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if !mustBool(t, out, "created") {
		t.Error("created=false on first upsert")
	}
	item := out["item"].(map[string]interface{})
	if item["seq"].(float64) != 1 {
		t.Errorf("seq=%v want 1", item["seq"])
	}
	if _, present := item["payload"]; present {
		t.Errorf("/upsert response item must not echo payload back: %v", item)
	}

	_, got, _ := doRequest(t, h, "GET", "/get?scope=s&id=a", "")
	if !mustBool(t, got, "hit") {
		t.Error("get after upsert missed")
	}
}

func TestUpsert_Replaces(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/upsert", `{"scope":"s","id":"a","payload":{"v":1}}`)

	code, out, _ := doRequest(t, h, "POST", "/upsert", `{"scope":"s","id":"a","payload":{"v":2}}`)
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if mustBool(t, out, "created") {
		t.Error("created=true on replace")
	}
	item := out["item"].(map[string]interface{})
	if item["seq"].(float64) != 1 {
		t.Errorf("seq=%v want 1 (preserved)", item["seq"])
	}

	_, got, _ := doRequest(t, h, "GET", "/get?scope=s&id=a", "")
	gotItem := got["item"].(map[string]interface{})
	if gotItem["payload"].(map[string]interface{})["v"].(float64) != 2 {
		t.Error("payload not replaced")
	}
}

func TestUpsert_MissingID(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "POST", "/upsert", `{"scope":"s","payload":{"v":1}}`)
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
}

func TestUpsert_MissingScope(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "POST", "/upsert", `{"id":"a","payload":{"v":1}}`)
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
}

func TestUpsert_MissingPayload(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "POST", "/upsert", `{"scope":"s","id":"a"}`)
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
}

func TestUpsert_SeqForbidden(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "POST", "/upsert", `{"scope":"s","id":"a","seq":5,"payload":{"v":1}}`)
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
}

func TestUpsert_MethodNotAllowed(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "GET", "/upsert", "")
	if code != http.StatusMethodNotAllowed {
		t.Fatalf("code=%d want 405", code)
	}
}

// --- /counter_add -------------------------------------------------------------

func TestCounterAdd_CreatesOnMiss(t *testing.T) {
	h, _ := newTestHandler(10)

	code, out, _ := doRequest(t, h, "POST", "/counter_add", `{"scope":"views","id":"article_1","by":1}`)
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if !mustBool(t, out, "created") {
		t.Error("created=false on first call")
	}
	if mustFloat(t, out, "value") != 1 {
		t.Errorf("value=%v want 1", out["value"])
	}

	// Round-trip through /get — payload is a bare JSON number.
	_, got, _ := doRequest(t, h, "GET", "/get?scope=views&id=article_1", "")
	if !mustBool(t, got, "hit") {
		t.Error("round-trip /get miss")
	}
}

func TestCounterAdd_Increments(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/counter_add", `{"scope":"c","id":"k","by":10}`)

	code, out, _ := doRequest(t, h, "POST", "/counter_add", `{"scope":"c","id":"k","by":5}`)
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if mustBool(t, out, "created") {
		t.Error("created=true on existing counter")
	}
	if mustFloat(t, out, "value") != 15 {
		t.Errorf("value=%v want 15", out["value"])
	}
}

func TestCounterAdd_NegativeBy(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/counter_add", `{"scope":"c","id":"k","by":100}`)

	_, out, _ := doRequest(t, h, "POST", "/counter_add", `{"scope":"c","id":"k","by":-40}`)
	if mustFloat(t, out, "value") != 60 {
		t.Errorf("value=%v want 60", out["value"])
	}
}

func TestCounterAdd_ConflictOnNonNumericExisting(t *testing.T) {
	h, _ := newTestHandler(10)
	// Seed with an HTML-ish string payload via /append.
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"pages","id":"home","payload":"<html/>"}`)

	code, out, _ := doRequest(t, h, "POST", "/counter_add", `{"scope":"pages","id":"home","by":1}`)
	if code != http.StatusConflict {
		t.Fatalf("code=%d want 409", code)
	}
	if mustBool(t, out, "ok") {
		t.Error("ok=true on conflict")
	}
}

func TestCounterAdd_ConflictOnFloatExisting(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/upsert", `{"scope":"c","id":"k","payload":3.14}`)

	code, _, _ := doRequest(t, h, "POST", "/counter_add", `{"scope":"c","id":"k","by":1}`)
	if code != http.StatusConflict {
		t.Fatalf("code=%d want 409", code)
	}
}

func TestCounterAdd_BadRequestOnZeroBy(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "POST", "/counter_add", `{"scope":"c","id":"k","by":0}`)
	if code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400", code)
	}
}

func TestCounterAdd_BadRequestOnMissingBy(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "POST", "/counter_add", `{"scope":"c","id":"k"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400", code)
	}
}

func TestCounterAdd_BadRequestOnMissingID(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "POST", "/counter_add", `{"scope":"c","by":1}`)
	if code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400", code)
	}
}

func TestCounterAdd_BadRequestOnMissingScope(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "POST", "/counter_add", `{"id":"k","by":1}`)
	if code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400", code)
	}
}

func TestCounterAdd_BadRequestOnByOutOfRange(t *testing.T) {
	h, _ := newTestHandler(10)
	// 2^53 is one past MaxCounterValue.
	code, _, _ := doRequest(t, h, "POST", "/counter_add", `{"scope":"c","id":"k","by":9007199254740992}`)
	if code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400", code)
	}
}

func TestCounterAdd_BadRequestOnOverflow(t *testing.T) {
	h, _ := newTestHandler(10)
	// Seed at the maximum allowed counter value.
	_, _, _ = doRequest(t, h, "POST", "/counter_add", `{"scope":"c","id":"k","by":9007199254740991}`)

	code, _, _ := doRequest(t, h, "POST", "/counter_add", `{"scope":"c","id":"k","by":1}`)
	if code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400", code)
	}
}

func TestCounterAdd_MethodNotAllowed(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "GET", "/counter_add", "")
	if code != http.StatusMethodNotAllowed {
		t.Fatalf("code=%d want 405", code)
	}
}

// --- /delete ------------------------------------------------------------------

func TestDelete_Hit(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","id":"a","payload":{"v":1}}`)

	code, out, _ := doRequest(t, h, "POST", "/delete", `{"scope":"s","id":"a"}`)
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if !mustBool(t, out, "hit") {
		t.Error("hit=false")
	}
}

func TestDelete_Miss(t *testing.T) {
	h, _ := newTestHandler(10)
	code, out, _ := doRequest(t, h, "POST", "/delete", `{"scope":"s","id":"none"}`)
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if mustBool(t, out, "hit") {
		t.Error("hit=true for missing id")
	}
}

func TestDelete_BySeq_Hit(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","payload":{"v":1}}`)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","payload":{"v":2}}`)

	code, out, _ := doRequest(t, h, "POST", "/delete", `{"scope":"s","seq":1}`)
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if !mustBool(t, out, "hit") {
		t.Error("hit=false")
	}
}

func TestDelete_BySeq_Miss(t *testing.T) {
	h, _ := newTestHandler(10)
	code, out, _ := doRequest(t, h, "POST", "/delete", `{"scope":"s","seq":42}`)
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if mustBool(t, out, "hit") {
		t.Error("hit=true for missing seq")
	}
}

func TestDelete_RejectsBothIDAndSeq(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "POST", "/delete", `{"scope":"s","id":"a","seq":1}`)
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
}

func TestDelete_RejectsNeitherIDNorSeq(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "POST", "/delete", `{"scope":"s"}`)
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
}

// --- /delete_scope ------------------------------------------------------------

func TestDeleteScope_Hit(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","id":"a","payload":{"v":1}}`)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","id":"b","payload":{"v":2}}`)

	code, out, _ := doAdminRequest(t, h, "/delete_scope", `{"scope":"s"}`)
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if !mustBool(t, out, "hit") {
		t.Error("hit=false")
	}
	if mustFloat(t, out, "deleted_items") != 2 {
		t.Errorf("deleted_items=%v want 2", out["deleted_items"])
	}
}

func TestDeleteScope_Miss(t *testing.T) {
	h, _ := newTestHandler(10)
	code, out, _ := doAdminRequest(t, h, "/delete_scope", `{"scope":"nope"}`)
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if mustBool(t, out, "hit") {
		t.Error("hit=true for missing scope")
	}
}

// --- /wipe --------------------------------------------------------------------

func TestWipe_EmptyStore(t *testing.T) {
	h, _ := newTestHandler(10)

	code, out, _ := doAdminRequest(t, h, "/wipe", "")
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if !mustBool(t, out, "ok") {
		t.Error("ok=false")
	}
	// "Empty" still has the two reserved scopes (_events, _inbox) pre-created
	// at boot. /wipe drops them (counted in deleted_scopes) then immediately
	// re-creates them, so the cache lands back at its boot baseline.
	if mustFloat(t, out, "deleted_scopes") != float64(len(reservedScopeNames)) {
		t.Errorf("deleted_scopes=%v want %d (reserved scopes were dropped + re-created)", out["deleted_scopes"], len(reservedScopeNames))
	}
	if mustFloat(t, out, "deleted_items") != 0 {
		t.Errorf("deleted_items=%v want 0", out["deleted_items"])
	}
}

func TestWipe_ClearsEveryScope(t *testing.T) {
	h, api := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"a","id":"1","payload":{"v":1}}`)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"a","id":"2","payload":{"v":2}}`)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"b","id":"1","payload":{"v":1}}`)

	code, out, _ := doAdminRequest(t, h, "/wipe", "")
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	// 2 user scopes + 2 reserved scopes (_events, _inbox) all dropped by /wipe.
	wantDeleted := float64(2 + len(reservedScopeNames))
	if mustFloat(t, out, "deleted_scopes") != wantDeleted {
		t.Errorf("deleted_scopes=%v want %v", out["deleted_scopes"], wantDeleted)
	}
	if mustFloat(t, out, "deleted_items") != 3 {
		t.Errorf("deleted_items=%v want 3", out["deleted_items"])
	}
	if mustFloat(t, out, "freed_mb") <= 0 {
		t.Errorf("freed_mb=%v want >0", out["freed_mb"])
	}

	// User scopes must be gone; reserved scopes were re-created by post-wipe init.
	_, out, _ = doAdminRequest(t, h, "/stats", "")
	if got := mustFloat(t, out, "scope_count"); got != float64(len(reservedScopeNames)) {
		t.Errorf("scope_count=%v want %d after /wipe (reserved scopes restored)", got, len(reservedScopeNames))
	}
	for _, scope := range []string{"a", "b"} {
		if _, ok := api.store.getScope(scope); ok {
			t.Errorf("scope %q still present after /wipe", scope)
		}
	}
}

// After /wipe the store-wide byte counter and scope count surfaced via
// /stats must both be zero — clients use those numbers to confirm the wipe.
func TestWipe_StatsReportEmptyAfterwards(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","id":"x","payload":{"v":1}}`)

	_, _, _ = doAdminRequest(t, h, "/wipe", "")

	_, out, _ := doAdminRequest(t, h, "/stats", "")
	// Post-wipe baseline: reserved scopes are immediately re-created, so
	// scope_count = len(reservedScopeNames), approx_store_mb = the
	// reserved-scope overhead (still very small but non-zero).
	if mustFloat(t, out, "scope_count") != float64(len(reservedScopeNames)) {
		t.Errorf("scope_count=%v want %d", out["scope_count"], len(reservedScopeNames))
	}
	if mustFloat(t, out, "total_items") != 0 {
		t.Errorf("total_items=%v want 0", out["total_items"])
	}
	// approx_store_mb is the reserved-scope overhead (2 × 1024 bytes = 2048
	// bytes ≈ 0.0020 MiB). Just assert it's non-negative and small.
	if got := mustFloat(t, out, "approx_store_mb"); got < 0 || got > 0.01 {
		t.Errorf("approx_store_mb=%v want a small reserved-scope baseline (0.001..0.005)", got)
	}
}

// /wipe accepts a POST with no body. A non-empty body is simply ignored;
// it has no effect on the operation.
func TestWipe_IgnoresBody(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","id":"x","payload":{"v":1}}`)

	code, out, _ := doAdminRequest(t, h, "/wipe", `{"anything":"goes"}`)
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if mustFloat(t, out, "deleted_items") != 1 {
		t.Errorf("deleted_items=%v want 1", out["deleted_items"])
	}
}

// After /wipe fresh writes must succeed — the cap budget is fully released,
// no stale scope state blocks a re-used id, seq counters restart from 1.
func TestWipe_FreshWritesAfterwards(t *testing.T) {
	h, _ := newTestHandler(10)
	for i := 0; i < 3; i++ {
		_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","id":"`+fmt.Sprint(i)+`","payload":{"v":1}}`)
	}

	_, _, _ = doAdminRequest(t, h, "/wipe", "")

	// Re-use an id that existed before the wipe — must succeed.
	code, out, _ := doRequest(t, h, "POST", "/append", `{"scope":"s","id":"0","payload":{"v":42}}`)
	if code != 200 {
		t.Fatalf("re-append after wipe: code=%d want 200", code)
	}
	item := out["item"].(map[string]interface{})
	// seq restarts from 1 because the scope was fully removed.
	if item["seq"].(float64) != 1 {
		t.Errorf("post-wipe seq=%v want 1", item["seq"])
	}
}

// --- /head / /tail ------------------------------------------------------------

func TestHead_DefaultLimitAndMiss(t *testing.T) {
	h, _ := newTestHandler(10)

	// Missing scope param
	code, _, _ := doRequest(t, h, "GET", "/head", "")
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}

	// Scope does not exist → 200 hit=false
	code, out, _ := doRequest(t, h, "GET", "/head?scope=none", "")
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if mustBool(t, out, "hit") {
		t.Error("hit=true for missing scope")
	}
}

func TestHead_RejectsOffset(t *testing.T) {
	h, _ := newTestHandler(10)
	// offset was dropped on /head — any attempt to use it must 400 so
	// clients are nudged toward after_seq or /tail instead of silently
	// getting position-paged results that drift under /delete_up_to.
	code, _, _ := doRequest(t, h, "GET", "/head?scope=s&offset=1", "")
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
}

func TestHead_DefaultReturnsOldest(t *testing.T) {
	h, _ := newTestHandler(10)
	for i := 0; i < 5; i++ {
		_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","payload":{"v":1}}`)
	}

	// No after_seq → treat as 0 → full scope (oldest first).
	_, out, _ := doRequest(t, h, "GET", "/head?scope=s&limit=3", "")
	items := out["items"].([]interface{})
	if len(items) != 3 {
		t.Fatalf("items=%d want 3", len(items))
	}
	first := items[0].(map[string]interface{})
	if first["seq"].(float64) != 1 {
		t.Errorf("first.seq=%v want 1", first["seq"])
	}
}

func TestHead_WithAfterSeq(t *testing.T) {
	h, _ := newTestHandler(10)
	for i := 0; i < 5; i++ {
		_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","payload":{"v":1}}`)
	}

	// after_seq=2 → only seq 3, 4, 5; limit=2 clips to seq 3, 4.
	_, out, _ := doRequest(t, h, "GET", "/head?scope=s&limit=2&after_seq=2", "")
	if mustFloat(t, out, "count") != 2 {
		t.Fatalf("count=%v want 2", out["count"])
	}
	items := out["items"].([]interface{})
	first := items[0].(map[string]interface{})
	if first["seq"].(float64) != 3 {
		t.Errorf("first.seq=%v want 3", first["seq"])
	}
}

func TestHead_RejectsMalformedAfterSeq(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "GET", "/head?scope=s&after_seq=notanumber", "")
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
}

func TestTail_WithOffset(t *testing.T) {
	h, _ := newTestHandler(10)
	for i := 0; i < 5; i++ {
		_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","payload":{"v":1}}`)
	}

	_, out, _ := doRequest(t, h, "GET", "/tail?scope=s&limit=2&offset=1", "")
	items := out["items"].([]interface{})
	if len(items) != 2 {
		t.Fatalf("items=%d want 2", len(items))
	}
	last := items[1].(map[string]interface{})
	if last["seq"].(float64) != 4 {
		t.Errorf("last.seq=%v want 4", last["seq"])
	}
}

// --- /get ---------------------------------------------------------------------

func TestGet_RequiresExactlyOneOfIDOrSeq(t *testing.T) {
	h, _ := newTestHandler(10)

	// Neither
	code, _, _ := doRequest(t, h, "GET", "/get?scope=s", "")
	if code != 400 {
		t.Fatalf("neither: code=%d want 400", code)
	}

	// Both
	code, _, _ = doRequest(t, h, "GET", "/get?scope=s&id=a&seq=1", "")
	if code != 400 {
		t.Fatalf("both: code=%d want 400", code)
	}
}

func TestGet_ByIDAndBySeq(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","id":"a","payload":{"v":1}}`)

	_, out, _ := doRequest(t, h, "GET", "/get?scope=s&id=a", "")
	if !mustBool(t, out, "hit") {
		t.Error("by id: hit=false")
	}

	_, out, _ = doRequest(t, h, "GET", "/get?scope=s&seq=1", "")
	if !mustBool(t, out, "hit") {
		t.Error("by seq: hit=false")
	}

	_, out, _ = doRequest(t, h, "GET", "/get?scope=s&id=missing", "")
	if mustBool(t, out, "hit") {
		t.Error("missing id: hit=true")
	}
}

// --- /render ------------------------------------------------------------------

// doRawRequest is a slimmed-down variant of doRequest that returns the full
// ResponseRecorder. /render tests need header access (Content-Type) and raw
// body access (no JSON unmarshaling) — doRequest hides both.
func doRawRequest(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRender_MethodNotAllowed(t *testing.T) {
	h, _ := newTestHandler(10)
	rec := doRawRequest(t, h, "POST", "/render?scope=s&id=a")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("code=%d want 405", rec.Code)
	}
}

func TestRender_RejectsMissingScope(t *testing.T) {
	h, _ := newTestHandler(10)
	rec := doRawRequest(t, h, "GET", "/render?id=a")
	if rec.Code != 400 {
		t.Fatalf("code=%d want 400", rec.Code)
	}
}

func TestRender_RequiresExactlyOneOfIDOrSeq(t *testing.T) {
	h, _ := newTestHandler(10)

	rec := doRawRequest(t, h, "GET", "/render?scope=s")
	if rec.Code != 400 {
		t.Fatalf("neither: code=%d want 400", rec.Code)
	}

	rec = doRawRequest(t, h, "GET", "/render?scope=s&id=a&seq=1")
	if rec.Code != 400 {
		t.Fatalf("both: code=%d want 400", rec.Code)
	}
}

func TestRender_RejectsMalformedSeq(t *testing.T) {
	h, _ := newTestHandler(10)
	rec := doRawRequest(t, h, "GET", "/render?scope=s&seq=notanumber")
	if rec.Code != 400 {
		t.Fatalf("code=%d want 400", rec.Code)
	}
}

// Miss (scope doesn't exist, or scope exists but item doesn't) must return
// 404 with an empty body and a neutral Content-Type — the envelope-free
// contract that distinguishes /render from /get.
func TestRender_MissReturns404EmptyBody(t *testing.T) {
	h, _ := newTestHandler(10)

	rec := doRawRequest(t, h, "GET", "/render?scope=nope&id=a")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing scope: code=%d want 404", rec.Code)
	}
	if body := rec.Body.String(); body != "" {
		t.Errorf("missing scope: body=%q want empty", body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("missing scope: Content-Type=%q want application/octet-stream", ct)
	}

	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","id":"a","payload":{"v":1}}`)

	rec = doRawRequest(t, h, "GET", "/render?scope=s&id=nonexistent")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing id: code=%d want 404", rec.Code)
	}
	if body := rec.Body.String(); body != "" {
		t.Errorf("missing id: body=%q want empty", body)
	}

	rec = doRawRequest(t, h, "GET", "/render?scope=s&seq=42")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing seq: code=%d want 404", rec.Code)
	}
}

// JSON object payloads are written raw — no envelope, no transformation.
// The consumer gets exactly the bytes that were stored.
func TestRender_JSONObjectPayload(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","id":"a","payload":{"text":"hello"}}`)

	rec := doRawRequest(t, h, "GET", "/render?scope=s&id=a")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type=%q want application/octet-stream", ct)
	}
	if body := rec.Body.String(); body != `{"text":"hello"}` {
		t.Errorf("body=%q want %q (raw JSON object, no envelope)", body, `{"text":"hello"}`)
	}
}

func TestRender_JSONArrayPayload(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","id":"a","payload":[1,2,3]}`)

	rec := doRawRequest(t, h, "GET", "/render?scope=s&id=a")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200", rec.Code)
	}
	if body := rec.Body.String(); body != `[1,2,3]` {
		t.Errorf("body=%q want raw JSON array", body)
	}
}

// The core use case: HTML/XML/text stored as a JSON string. /render must
// strip exactly one layer of JSON string-encoding so the consumer receives
// real HTML bytes, not the quoted/escaped form. Without this, a browser
// served by Caddy would receive a literal `"<html>..."` and render it as
// text instead of as a webpage.
func TestRender_JSONStringPayload_DecodesOneLayer(t *testing.T) {
	h, _ := newTestHandler(10)
	htmlBody := "<html><body>Hi \"quoted\" and\nnewline and \\ backslash</body></html>"
	encodedPayload, err := json.Marshal(htmlBody)
	if err != nil {
		t.Fatalf("marshal htmlBody: %v", err)
	}
	appendBody := fmt.Sprintf(`{"scope":"pages","id":"home","payload":%s}`, encodedPayload)
	if code, _, raw := doRequest(t, h, "POST", "/append", appendBody); code != 200 {
		t.Fatalf("append: code=%d body=%s", code, raw)
	}

	rec := doRawRequest(t, h, "GET", "/render?scope=pages&id=home")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200", rec.Code)
	}
	if got := rec.Body.String(); got != htmlBody {
		t.Errorf("body=%q want %q (JSON string must be decoded one layer)", got, htmlBody)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type=%q want application/octet-stream (cache does not sniff MIME)", ct)
	}
}

func TestRender_BySeq(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","payload":{"v":7}}`)

	rec := doRawRequest(t, h, "GET", "/render?scope=s&seq=1")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200", rec.Code)
	}
	if body := rec.Body.String(); body != `{"v":7}` {
		t.Errorf("body=%q want raw JSON object", body)
	}
}

// /render hits bump scope read-bookkeeping the same way /get hits do.
// Misses must not count (same rule as /get) — otherwise a hot 404
// would skew downstream observability.
func TestRender_HitBumpsReadCount_MissDoesNot(t *testing.T) {
	h, api := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","id":"a","payload":{"v":1}}`)

	_ = doRawRequest(t, h, "GET", "/render?scope=s&id=a")
	_ = doRawRequest(t, h, "GET", "/render?scope=s&id=a")
	_ = doRawRequest(t, h, "GET", "/render?scope=s&id=nonexistent") // miss — must not count

	buf, _ := api.store.getScope("s")
	got := buf.readCountTotal.Load()
	if got != 2 {
		t.Errorf("read_count_total=%d want 2 (two hits, miss must not count)", got)
	}
}

// --- /stats -------------------------------------------------------------------

func TestStats_Structure(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","payload":{"v":1}}`)

	_, out, _ := doAdminRequest(t, h, "/stats", "")
	// 1 user scope ("s") + reserved scopes (_events, _inbox).
	if got := mustFloat(t, out, "scope_count"); got != float64(1+len(reservedScopeNames)) {
		t.Errorf("scope_count=%v want %d (user + reserved)", got, 1+len(reservedScopeNames))
	}
	if mustFloat(t, out, "total_items") != 1 {
		t.Errorf("total_items=%v want 1", out["total_items"])
	}
	if _, present := out["approx_store_mb"]; !present {
		t.Error("approx_store_mb missing")
	}
	// Regression guard: /stats is a pure state endpoint. Configured
	// caps (max_store_mb, scope_max_items, etc.) are static config
	// and belong on /help — they MUST NOT reappear on /stats.
	if _, present := out["max_store_mb"]; present {
		t.Errorf("max_store_mb must NOT appear on /stats (config belongs on /help): %v", out["max_store_mb"])
	}
	// Regression guard: /stats is intentionally aggregate-only for
	// user-managed scopes since the 100k-scope DoS observation. The
	// `scopes` key (full per-scope enumeration) belongs on /scopelist;
	// the small fixed `reserved_scopes` block on /stats is the bounded
	// exception for cache infrastructure scopes.
	if _, present := out["scopes"]; present {
		t.Errorf("scopes key must NOT appear on /stats response (moved to /scopelist): %v", out["scopes"])
	}
}

// /stats always carries events_drops_total — a monotonic atomic
// counter that ticks up every time the auto-populate path failed to
// land an event in `_events` (cap overflow on _events, or — defensive
// only — json.Marshal failure). Operators monitor this for drainer-
// lag / cap-undersized deployments. On a fresh store with no writes
// (or events_mode=off), the field is present with value 0 — its
// presence is the contract, not its non-zeroness.
func TestStats_EventsDropsTotal_PresentByDefault(t *testing.T) {
	h, _ := newTestHandler(10)

	_, out, _ := doAdminRequest(t, h, "/stats", "")
	got, ok := out["events_drops_total"]
	if !ok {
		t.Fatalf("events_drops_total missing from /stats response: %+v", out)
	}
	if v, ok := got.(float64); !ok || v != 0 {
		t.Errorf("events_drops_total=%v want 0 (fresh store, no writes)", got)
	}
}

// Under tight cap pressure with events_mode=full, each user write
// commits but the auto-populate path drops its event because there's
// no room in _events for the envelope. /stats must report the
// resulting drop count — that's the operator-monitorable signal that
// "events are silently being lost" (drainer slow, cap undersized,
// etc.).
func TestStats_EventsDropsTotal_TicksOnDrops(t *testing.T) {
	cfg := Config{
		ScopeMaxItems: 100,
		MaxStoreBytes: reservedScopesOverhead + scopeBufferOverhead + 100,
		MaxItemBytes:  1 << 20,
		Events:        EventsConfig{Mode: EventsModeFull},
	}
	api := NewAPI(NewGateway(cfg), APIConfig{})
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	// Force a drop: tiny user payload fits in the 100-byte slack but
	// the Full-mode event envelope doesn't.
	if code, _, raw := doRequest(t, mux, "POST", "/append",
		`{"scope":"posts","id":"a","payload":{"v":1}}`); code != 200 {
		t.Fatalf("user /append: code=%d body=%s want 200", code, raw)
	}

	_, out, _ := doRequest(t, mux, "GET", "/stats", "")
	got := mustFloat(t, out, "events_drops_total")
	if got < 1 {
		t.Errorf("events_drops_total=%v want >= 1 (a drop should have happened)", got)
	}
	// Atomic counter must agree with what /stats reports.
	if int64(got) != api.store.eventsDropsTotal.Load() {
		t.Errorf("events_drops_total wire (%v) != atomic (%d)",
			got, api.store.eventsDropsTotal.Load())
	}
}

// /stats includes a reserved_scopes array — one row per cache-managed
// reserved scope (currently `_events`, `_inbox`). The row carries the
// (ii)-tier scope-stats: item_count, last_seq, approx_scope_mb,
// created_ts, last_write_ts. Behaviour is independent of events_mode:
// the reserved scopes exist at boot regardless, so /stats surfaces
// their state always.
func TestStats_ReservedScopesBlock(t *testing.T) {
	h, _ := newTestHandler(10)

	_, out, _ := doAdminRequest(t, h, "/stats", "")
	rawList, ok := out["reserved_scopes"]
	if !ok {
		t.Fatalf("missing reserved_scopes in /stats response: %+v", out)
	}
	list, ok := rawList.([]interface{})
	if !ok {
		t.Fatalf("reserved_scopes is not an array: %T", rawList)
	}
	if len(list) != len(reservedScopeNames) {
		t.Fatalf("reserved_scopes len=%d want %d", len(list), len(reservedScopeNames))
	}

	// Reserved scopes appear in reservedScopeNames declaration order
	// (currently alphabetical: _events then _inbox). Verify both names
	// land + every required field is present and well-typed.
	wantFields := []string{
		"scope", "item_count", "last_seq",
		"approx_scope_mb", "created_ts", "last_write_ts",
	}
	wantNames := reservedScopeNames
	for i, raw := range list {
		entry, ok := raw.(map[string]interface{})
		if !ok {
			t.Fatalf("reserved_scopes[%d] is not an object: %T", i, raw)
		}
		if entry["scope"] != wantNames[i] {
			t.Errorf("reserved_scopes[%d].scope=%v want %v", i, entry["scope"], wantNames[i])
		}
		for _, f := range wantFields {
			if _, present := entry[f]; !present {
				t.Errorf("reserved_scopes[%d] missing field %q: %+v", i, f, entry)
			}
		}
		// item_count = 0 on a fresh store with no events config and
		// no /inbox writes; floats by JSON decode.
		if got := entry["item_count"].(float64); got != 0 {
			t.Errorf("reserved_scopes[%d].item_count=%v want 0 (fresh store)", i, got)
		}
		// created_ts is set at boot, so > 0 always.
		if got := entry["created_ts"].(float64); got <= 0 {
			t.Errorf("reserved_scopes[%d].created_ts=%v want > 0", i, got)
		}
		// approx_scope_mb is the buffer's overhead — small but positive.
		if got := entry["approx_scope_mb"].(float64); got <= 0 {
			t.Errorf("reserved_scopes[%d].approx_scope_mb=%v want > 0 (buffer overhead)", i, got)
		}
	}

	// Forbidden fields: last_access_ts and read_count_total are on
	// /scopelist's full row but intentionally NOT here — reserved scopes
	// are read by drainers, not user-facing traffic, so those signals
	// are noise on /stats.
	for i, raw := range list {
		entry := raw.(map[string]interface{})
		for _, f := range []string{"last_access_ts", "read_count_total"} {
			if _, present := entry[f]; present {
				t.Errorf("reserved_scopes[%d] must not carry %q on /stats: %+v", i, f, entry)
			}
		}
	}
}

// reserved_scopes reflects writes: with events_mode=full, every user-
// scope mutation auto-populates `_events`, so /stats's _events.item_count
// and last_seq advance in lockstep with the source-of-truth scope state.
func TestStats_ReservedScopes_TracksEventsMode(t *testing.T) {
	h, _ := newReservedScopesTestHandler(t, Config{
		ScopeMaxItems: 100,
		MaxStoreBytes: 100 << 20,
		MaxItemBytes:  1 << 20,
		Events:        EventsConfig{Mode: EventsModeFull},
	})

	for i := 0; i < 5; i++ {
		body := fmt.Sprintf(`{"scope":"posts","id":"p-%d","payload":{"v":%d}}`, i, i)
		if code, _, raw := doRequest(t, h, "POST", "/append", body); code != 200 {
			t.Fatalf("/append #%d: code=%d body=%s", i, code, raw)
		}
	}

	_, out, _ := doRequest(t, h, "GET", "/stats", "")
	list, _ := out["reserved_scopes"].([]interface{})
	if len(list) != len(reservedScopeNames) {
		t.Fatalf("reserved_scopes len=%d want %d", len(list), len(reservedScopeNames))
	}

	// Find the _events entry by name (don't assume index ordering).
	var events map[string]interface{}
	for _, raw := range list {
		entry := raw.(map[string]interface{})
		if entry["scope"] == EventsScopeName {
			events = entry
			break
		}
	}
	if events == nil {
		t.Fatalf("reserved_scopes missing _events entry: %+v", list)
	}
	if got := events["item_count"].(float64); got != 5 {
		t.Errorf("_events.item_count=%v want 5 (one event per /append)", got)
	}
	if got := events["last_seq"].(float64); got != 5 {
		t.Errorf("_events.last_seq=%v want 5", got)
	}
	if got := events["last_write_ts"].(float64); got <= 0 {
		t.Errorf("_events.last_write_ts=%v want > 0 (events were written)", got)
	}
}

// --- /scopelist ---------------------------------------------------------------

// mustScopelistEntries pulls the typed list of {scope, item_count, ...}
// rows out of a /scopelist response. Lets tests read the wire shape
// without re-asserting the same json.RawMessage dance per case.
func mustScopelistEntries(t *testing.T, out map[string]interface{}) []map[string]interface{} {
	t.Helper()
	raw, ok := out["scopes"]
	if !ok {
		t.Fatalf("missing 'scopes' in /scopelist response: %+v", out)
	}
	arr, ok := raw.([]interface{})
	if !ok {
		t.Fatalf("'scopes' is not an array: %T", raw)
	}
	out2 := make([]map[string]interface{}, 0, len(arr))
	for i, e := range arr {
		m, ok := e.(map[string]interface{})
		if !ok {
			t.Fatalf("scopes[%d] is not an object: %T", i, e)
		}
		out2 = append(out2, m)
	}
	return out2
}

func mustScopeNames(t *testing.T, out map[string]interface{}) []string {
	t.Helper()
	entries := mustScopelistEntries(t, out)
	names := make([]string, 0, len(entries))
	for i, e := range entries {
		s, ok := e["scope"].(string)
		if !ok {
			t.Fatalf("scopes[%d].scope is not a string: %v", i, e["scope"])
		}
		names = append(names, s)
	}
	return names
}

// /scopelist with no params returns every scope, sorted alphabetically,
// each row carrying the seven §2.4 primitives + scope name.
func TestScopelist_AlphaSortAndShape(t *testing.T) {
	h, _ := newTestHandler(10)
	for _, s := range []string{"thread:42", "alpha", "echo", "thread:1"} {
		body := fmt.Sprintf(`{"scope":%q,"payload":{"v":1}}`, s)
		if code, _, raw := doRequest(t, h, "POST", "/append", body); code != 200 {
			t.Fatalf("seed %q: code=%d body=%s", s, code, raw)
		}
	}

	code, out, raw := doRequest(t, h, "GET", "/scopelist", "")
	if code != 200 {
		t.Fatalf("code=%d body=%s", code, raw)
	}
	if !mustBool(t, out, "ok") {
		t.Fatal("ok=false")
	}
	if !mustBool(t, out, "hit") {
		t.Error("hit=false with non-empty scopes (must be count>0)")
	}
	// 4 user scopes + 2 reserved scopes (_events, _inbox) = 6 total.
	// Reserved scopes sort before user scopes because '_' (0x5F) < 'a' (0x61).
	if mustFloat(t, out, "count") != float64(4+len(reservedScopeNames)) {
		t.Errorf("count=%v want %d (4 user + %d reserved)", out["count"], 4+len(reservedScopeNames), len(reservedScopeNames))
	}
	if mustBool(t, out, "truncated") {
		t.Error("truncated=true with limit > scope count")
	}

	got := mustScopeNames(t, out)
	want := []string{"_events", "_inbox", "alpha", "echo", "thread:1", "thread:42"}
	if len(got) != len(want) {
		t.Fatalf("scopes=%v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("scopes[%d]=%q want %q (alpha sort)", i, got[i], want[i])
		}
	}

	// Per-row shape: every entry must carry the seven primitives + scope.
	row := mustScopelistEntries(t, out)[0]
	for _, key := range []string{
		"scope", "item_count", "last_seq", "approx_scope_mb",
		"created_ts", "last_write_ts", "last_access_ts", "read_count_total",
	} {
		if _, ok := row[key]; !ok {
			t.Errorf("row missing %q: %+v", key, row)
		}
	}
}

// Prefix filter is literal strings.HasPrefix — no regex, no wildcard parsing.
// Empty prefix is the no-filter case, equivalent to omitting the param.
func TestScopelist_PrefixFilter(t *testing.T) {
	h, _ := newTestHandler(10)
	for _, s := range []string{"thread:42", "alpha", "echo", "thread:1"} {
		body := fmt.Sprintf(`{"scope":%q,"payload":{"v":1}}`, s)
		_, _, _ = doRequest(t, h, "POST", "/append", body)
	}

	_, out, _ := doRequest(t, h, "GET", "/scopelist?prefix=thread:", "")
	got := mustScopeNames(t, out)
	want := []string{"thread:1", "thread:42"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("prefix=thread: got=%v want %v", got, want)
	}

	// Empty prefix is treated as "no filter" — must equal the unfiltered
	// call (4 user scopes + 2 reserved).
	_, out2, _ := doRequest(t, h, "GET", "/scopelist?prefix=", "")
	if got := mustFloat(t, out2, "count"); got != float64(4+len(reservedScopeNames)) {
		t.Errorf("empty prefix: count=%v want %d", got, 4+len(reservedScopeNames))
	}

	// No matches → empty array, not null.
	_, out3, _ := doRequest(t, h, "GET", "/scopelist?prefix=zzz", "")
	if mustFloat(t, out3, "count") != 0 {
		t.Errorf("no-match prefix: count=%v want 0", out3["count"])
	}
	if mustBool(t, out3, "hit") {
		t.Error("no-match prefix: hit=true want false (count==0)")
	}
	if got := mustScopelistEntries(t, out3); len(got) != 0 {
		t.Errorf("no-match prefix: scopes=%v want []", got)
	}
}

// Cursor pagination: limit + after is the only paging mode shipped.
// `truncated` flips when more matching scopes exist past the page,
// and resuming with after=<last scope> walks the next page.
//
// Reserved scopes (_events, _inbox) sort before every user scope because
// '_' (0x5F) < 'a' (0x61). This test uses after=_zzz as the initial
// cursor to skip past reserved scopes and exercise pagination on the
// user-managed scopes alone.
func TestScopelist_LimitAndAfterCursor(t *testing.T) {
	h, _ := newTestHandler(10)
	scopes := []string{"a", "b", "c", "d", "e"}
	for _, s := range scopes {
		body := fmt.Sprintf(`{"scope":%q,"payload":{"v":1}}`, s)
		_, _, _ = doRequest(t, h, "POST", "/append", body)
	}

	_, out, _ := doRequest(t, h, "GET", "/scopelist?limit=2&after=_zzz", "")
	if !mustBool(t, out, "truncated") {
		t.Error("truncated=false on limit=2 with 5 scopes")
	}
	got := mustScopeNames(t, out)
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("page1=%v want [a b]", got)
	}

	_, out, _ = doRequest(t, h, "GET", "/scopelist?limit=2&after=b", "")
	if !mustBool(t, out, "truncated") {
		t.Error("truncated=false on second page with more behind")
	}
	got = mustScopeNames(t, out)
	if len(got) != 2 || got[0] != "c" || got[1] != "d" {
		t.Errorf("page2=%v want [c d]", got)
	}

	_, out, _ = doRequest(t, h, "GET", "/scopelist?limit=2&after=d", "")
	if mustBool(t, out, "truncated") {
		t.Error("truncated=true on final page")
	}
	got = mustScopeNames(t, out)
	if len(got) != 1 || got[0] != "e" {
		t.Errorf("page3=%v want [e]", got)
	}

	// after past every scope name → empty page, not truncated.
	_, out, _ = doRequest(t, h, "GET", "/scopelist?after=zzz", "")
	if mustFloat(t, out, "count") != 0 {
		t.Errorf("after=zzz: count=%v want 0", out["count"])
	}
}

// /scopelist must not bump per-scope read-bookkeeping. Eviction-candidate
// addons that poll /scopelist would otherwise see their own polls inflate
// the read_count_total they're trying to measure. Same rule the RFC §8.2
// applies to /stats.
func TestScopelist_DoesNotBumpReadBookkeeping(t *testing.T) {
	h, api := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"x","id":"a","payload":{"v":1}}`)

	for i := 0; i < 5; i++ {
		_, _, _ = doRequest(t, h, "GET", "/scopelist", "")
	}

	buf, ok := api.store.getScope("x")
	if !ok {
		t.Fatal("scope x missing")
	}
	if got := buf.readCountTotal.Load(); got != 0 {
		t.Errorf("read_count_total=%d want 0 (/scopelist must not count as a read)", got)
	}
	if got := buf.lastAccessTS.Load(); got != 0 {
		t.Errorf("last_access_ts=%d want 0 (/scopelist must not stamp access)", got)
	}
}

// Validation errors share the standard 400 envelope: prefix and after both
// flow through checkKeyField (same shape rules as scope), so an embedded
// control character or oversize value is rejected up-front.
func TestScopelist_ValidationErrors(t *testing.T) {
	h, _ := newTestHandler(10)
	cases := []struct{ url, wantSubstr string }{
		{"/scopelist?prefix=" + strings.Repeat("a", MaxScopeBytes+1), "256"},
		{"/scopelist?after=" + strings.Repeat("b", MaxScopeBytes+1), "256"},
		{"/scopelist?limit=0", "positive"},
		{"/scopelist?limit=-3", "positive"},
		{"/scopelist?limit=abc", "positive"},
	}
	for _, c := range cases {
		code, out, raw := doRequest(t, h, "GET", c.url, "")
		if code != 400 {
			t.Errorf("%s: code=%d want 400 body=%s", c.url, code, raw)
			continue
		}
		errStr, _ := out["error"].(string)
		if !strings.Contains(errStr, c.wantSubstr) {
			t.Errorf("%s: error=%q does not mention %q", c.url, errStr, c.wantSubstr)
		}
	}
}

func TestScopelist_MethodNotAllowed(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "POST", "/scopelist", "")
	if code != 405 {
		t.Errorf("POST /scopelist: code=%d want 405", code)
	}
}

// Empty store → empty array, count=0, truncated=false. Wire-format check
// that no client sees null instead of []. Uses after=_zzz to skip past
// the reserved scopes (_events, _inbox) that NewStore pre-creates so the
// "empty" assertion exercises the empty-result code path.
func TestScopelist_EmptyStore(t *testing.T) {
	h, _ := newTestHandler(10)
	_, out, _ := doRequest(t, h, "GET", "/scopelist?after=_zzz", "")
	if mustFloat(t, out, "count") != 0 {
		t.Errorf("count=%v want 0", out["count"])
	}
	if mustBool(t, out, "truncated") {
		t.Error("truncated=true on empty store")
	}
	entries := mustScopelistEntries(t, out)
	if len(entries) != 0 {
		t.Errorf("scopes=%v want []", entries)
	}
}

// Sanity: the seven primitives carry the values the buffer actually holds.
// Reading a per-scope row from /scopelist must report the same numbers as
// poking the *scopeBuffer directly.
func TestScopelist_ReportsPerScopePrimitives(t *testing.T) {
	h, api := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"x","id":"a","payload":{"v":1}}`)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"x","id":"b","payload":{"v":2}}`)
	_, _, _ = doRequest(t, h, "GET", "/get?scope=x&id=a", "") // bumps read_count_total + last_access_ts

	_, out, _ := doRequest(t, h, "GET", "/scopelist?prefix=x", "")
	row := mustScopelistEntries(t, out)[0]
	if row["scope"].(string) != "x" {
		t.Errorf("scope=%v want x", row["scope"])
	}
	if row["item_count"].(float64) != 2 {
		t.Errorf("item_count=%v want 2", row["item_count"])
	}
	if row["last_seq"].(float64) != 2 {
		t.Errorf("last_seq=%v want 2", row["last_seq"])
	}
	if row["read_count_total"].(float64) != 1 {
		t.Errorf("read_count_total=%v want 1 (one /get hit)", row["read_count_total"])
	}

	buf, _ := api.store.getScope("x")
	wantLastWrite := buf.lastWriteTS
	if got := int64(row["last_write_ts"].(float64)); got != wantLastWrite {
		t.Errorf("last_write_ts=%d want %d", got, wantLastWrite)
	}
}

// --- integration: mixed workload ---------------------------------------------

// TestIntegration_MixedWorkload_StatsAndInvariants drives the full API through
// a realistic sequence (rebuild → warm → appends → update → deletes → reads →
// failed write) and verifies /stats reports exactly what the operations imply,
// plus the internal byte-accounting invariants. This is the one test that
// catches interaction bugs that per-endpoint unit tests miss: byte drift
// across operations, seq rewinds after deletes, read-heat not firing, silent
// side effects of rejected requests, and so on.
func TestIntegration_MixedWorkload_StatsAndInvariants(t *testing.T) {
	h, api := newTestHandler(200)

	// 1. rebuild: 100 items in scope x, 100 items in scope y.
	//    Fresh IDs item_000..item_099 and uniform 7-byte payloads keep the
	//    byte math predictable across the whole test.
	var rebuildItems strings.Builder
	rebuildItems.WriteString(`{"items":[`)
	first := true
	addItem := func(b *strings.Builder, s, id, payload string) {
		if !first {
			b.WriteString(",")
		}
		first = false
		fmt.Fprintf(b, `{"scope":"%s","id":"%s","payload":%s}`, s, id, payload)
	}
	for i := 0; i < 100; i++ {
		addItem(&rebuildItems, "x", fmt.Sprintf("item_%03d", i), `{"v":1}`)
	}
	for i := 0; i < 100; i++ {
		addItem(&rebuildItems, "y", fmt.Sprintf("item_%03d", i), `{"v":1}`)
	}
	rebuildItems.WriteString(`]}`)
	if code, _, body := doAdminRequest(t, h, "/rebuild", rebuildItems.String()); code != 200 {
		t.Fatalf("rebuild: code=%d body=%s", code, body)
	}

	// 2. warm y with 50 fresh items. warm resets y's lastSeq to 50.
	var warmItems strings.Builder
	warmItems.WriteString(`{"items":[`)
	first = true
	for i := 0; i < 50; i++ {
		addItem(&warmItems, "y", fmt.Sprintf("warm_%03d", i), `{"v":1}`)
	}
	warmItems.WriteString(`]}`)
	if code, _, body := doAdminRequest(t, h, "/warm", warmItems.String()); code != 200 {
		t.Fatalf("warm: code=%d body=%s", code, body)
	}

	// 3. 100 appends to y (no ID). seqs 51..150, lastSeq ends at 150.
	for i := 0; i < 100; i++ {
		if code, _, body := doRequest(t, h, "POST", "/append", `{"scope":"y","payload":{"v":1}}`); code != 200 {
			t.Fatalf("append #%d: code=%d body=%s", i, code, body)
		}
	}

	// 4. update byID on x.item_099. Same-size payload → 0 byte delta.
	if code, _, body := doRequest(t, h, "POST", "/update",
		`{"scope":"x","id":"item_099","payload":{"v":2}}`); code != 200 {
		t.Fatalf("update: code=%d body=%s", code, body)
	}

	// 5. delete byID on x.item_050 (removes seq 51 from x).
	if code, _, body := doRequest(t, h, "POST", "/delete",
		`{"scope":"x","id":"item_050"}`); code != 200 {
		t.Fatalf("delete byID: code=%d body=%s", code, body)
	}

	// 6. delete bySeq 75 on y (within the appended tail — no ID on that item).
	if code, _, body := doRequest(t, h, "POST", "/delete",
		`{"scope":"y","seq":75}`); code != 200 {
		t.Fatalf("delete bySeq: code=%d body=%s", code, body)
	}

	// 7. delete_up_to x: drop every item with seq <= 30. Does NOT rewind lastSeq.
	if code, _, body := doRequest(t, h, "POST", "/delete_up_to",
		`{"scope":"x","max_seq":30}`); code != 200 {
		t.Fatalf("delete_up_to: code=%d body=%s", code, body)
	}

	// 8. head x with a limit that returns >= 1 item (otherwise recordRead is skipped).
	//    Bracket the call so we can later assert last_access_ts falls inside it.
	preHeadX := nowUnixMicro()
	if code, out, _ := doRequest(t, h, "GET", "/head?scope=x&limit=5", ""); code != 200 {
		t.Fatalf("head: code=%d", code)
	} else if !mustBool(t, out, "hit") {
		t.Fatal("head: hit=false, expected items after operations")
	}
	postHeadX := nowUnixMicro()

	// 9. tail y — read on a different scope so read-heat is per-scope, not global.
	if code, out, _ := doRequest(t, h, "GET", "/tail?scope=y&limit=3", ""); code != 200 {
		t.Fatalf("tail: code=%d", code)
	} else if !mustBool(t, out, "hit") {
		t.Fatal("tail: hit=false")
	}

	// 10. get byID y.warm_000 — the last read on y, so its last_access_ts is
	//     stamped here. Bracket the call for an exact window check.
	preGetY := nowUnixMicro()
	if code, out, _ := doRequest(t, h, "GET", "/get?scope=y&id=warm_000", ""); code != 200 {
		t.Fatalf("get: code=%d", code)
	} else if !mustBool(t, out, "hit") {
		t.Fatal("get: hit=false — warm-phase id should still exist")
	}
	postGetY := nowUnixMicro()

	// 11. append with an over-cap scope name (MaxScopeBytes+1 bytes). Must
	//     400 and must NOT register a new scope — verified via scope_count
	//     below.
	tooLong := strings.Repeat("a", MaxScopeBytes+1)
	if code, _, _ := doRequest(t, h, "POST", "/append",
		fmt.Sprintf(`{"scope":"%s","payload":{"v":1}}`, tooLong)); code != 400 {
		t.Fatalf("too-long scope: code=%d want 400", code)
	}

	// --- Assertions on /stats ---
	_, stats, _ := doAdminRequest(t, h, "/stats", "")

	// 2 user scopes (x, y) + reserved scopes (_events, _inbox).
	wantScopeCount := float64(2 + len(reservedScopeNames))
	if got := mustFloat(t, stats, "scope_count"); got != wantScopeCount {
		t.Errorf("scope_count=%v want %v (rejected too-long-scope must not register; reserved baseline)", got, wantScopeCount)
	}
	// x: 100 rebuilt − 1 deleted (item_050) − 30 (delete_up_to) = 69
	// y: 50 warmed + 100 appended − 1 deleted (seq 75)         = 149
	if got := mustFloat(t, stats, "total_items"); got != 218 {
		t.Errorf("total_items=%v want 218", got)
	}

	// Per-scope assertions read directly from *scopeBuffer — /stats is
	// aggregate-only since the 100k-scope DoS observation; per-scope
	// detail moves to the (future) /scopelist endpoint, but the
	// underlying buffer fields are still the source of truth for these
	// invariants and we want them pinned here.
	xBuf, ok := api.store.getScope("x")
	if !ok {
		t.Fatalf("scope x missing from store")
	}
	xBuf.mu.RLock()
	xItemCount := len(xBuf.items)
	xLastSeq := xBuf.lastSeq
	xBuf.mu.RUnlock()
	xReadCount := xBuf.readCountTotal.Load()
	xLastAccess := xBuf.lastAccessTS.Load()

	if xItemCount != 69 {
		t.Errorf("x.item_count=%d want 69", xItemCount)
	}
	if xLastSeq != 100 {
		t.Errorf("x.last_seq=%d want 100 (delete_up_to must not rewind lastSeq)", xLastSeq)
	}
	if xReadCount < 1 {
		t.Errorf("x.read_count_total=%d want >= 1 after /head", xReadCount)
	}
	// /head on x was the last touch, so its last_access_ts must sit inside
	// the window we bracketed around that call.
	if xLastAccess < preHeadX || xLastAccess > postHeadX {
		t.Errorf("x.last_access_ts=%d not in bracket [%d, %d] around /head", xLastAccess, preHeadX, postHeadX)
	}

	yBuf, ok := api.store.getScope("y")
	if !ok {
		t.Fatalf("scope y missing from store")
	}
	yBuf.mu.RLock()
	yItemCount := len(yBuf.items)
	yLastSeq := yBuf.lastSeq
	yBuf.mu.RUnlock()
	yReadCount := yBuf.readCountTotal.Load()
	yLastAccess := yBuf.lastAccessTS.Load()

	if yItemCount != 149 {
		t.Errorf("y.item_count=%d want 149", yItemCount)
	}
	if yLastSeq != 150 {
		t.Errorf("y.last_seq=%d want 150", yLastSeq)
	}
	// /tail + /get byID both hit y — so at least two reads landed on this scope.
	if yReadCount < 2 {
		t.Errorf("y.read_count_total=%d want >= 2 after /tail + /get", yReadCount)
	}
	// /get byID was the last read on y, so last_access_ts must sit inside
	// that call's bracket.
	if yLastAccess < preGetY || yLastAccess > postGetY {
		t.Errorf("y.last_access_ts=%d not in bracket [%d, %d] around /get", yLastAccess, preGetY, postGetY)
	}

	// --- Internal accounting invariants ---
	// Since v0.5.14 totalBytes also charges per-scope buffer overhead.
	// scopeOverhead = scope-count × scopeBufferOverhead is added to the
	// expected counter value.
	storeScopes := api.store.listScopes()
	scopeOverhead := int64(len(storeScopes)) * scopeBufferOverhead

	// 1. totalBytes matches what we'd compute from current items + overhead
	//    — proves the incremental counter never drifted across the workload.
	var ground int64
	for _, buf := range storeScopes {
		buf.mu.RLock()
		for i := range buf.items {
			ground += approxItemSize(buf.items[i])
		}
		buf.mu.RUnlock()
	}
	if got := api.store.totalBytes.Load(); got != ground+scopeOverhead {
		t.Errorf("totalBytes=%d but recomputed-from-items+overhead=%d (counter drift)", got, ground+scopeOverhead)
	}

	// 2. Sum of per-scope b.bytes + overhead == store totalBytes. Catches
	//    the ghost-bytes class of bug where store and scope counters
	//    silently diverge.
	var sumBufBytes int64
	for _, buf := range storeScopes {
		buf.mu.RLock()
		sumBufBytes += buf.bytes
		buf.mu.RUnlock()
	}
	if got := api.store.totalBytes.Load(); got != sumBufBytes+scopeOverhead {
		t.Errorf("totalBytes=%d but Σ buf.bytes + overhead=%d", got, sumBufBytes+scopeOverhead)
	}

	// 3. Same shape, item count + scope count: proves the totalItems and
	//    scopeCount atomics that drive the O(1) /stats stayed lockstep
	//    with the per-scope state through every mutation in this test.
	assertStatsCountersInvariant(t, api.store, "after mixed workload")
}

// --- integration: parallel race workload -------------------------------------

// TestRace_ParallelMixedWorkload hammers the API from many goroutines at once
// and checks the state that survives against concretely tallied expectations.
//
// Each worker keeps its own counters (successful appends, deleted items via
// /delete and /delete_up_to, reads-with-hit) so the hot loop is lock-free. At
// the end we sum the tallies and require the API's own state to match to the
// item. Everything we can derive from the workload is checked exactly:
//
//   - total_items from /stats == Σ appendsOK − Σ deletedN
//   - same total matches len(items) walked across every live scope
//   - per scope: items slice, bySeq map, byID map are mutually consistent
//   - per scope: items are still sorted ascending by seq (append contract)
//   - per scope: lastSeq >= the highest seq present (never rewinds)
//   - store.totalBytes == Σ buf.bytes == Σ approxItemSize recomputed from items
//   - Σ buf.readCountTotal == Σ readsHit (the workers only issue reads that
//     trigger recordRead, so the two counts are equal, not just related)
//
// Ops per workload are chosen to exercise every mutation path that takes the
// scope write lock (append, delete, update, delete_up_to) as well as the read
// paths that take RLock + recordRead. /warm, /rebuild and /delete_scope are
// intentionally excluded here: they wipe or swap scope state, which would
// destroy the "Σ appends − Σ deletes = current items" relation that makes the
// concrete check possible. Races touching those paths live in separate tests.
func TestRace_ParallelMixedWorkload(t *testing.T) {
	const (
		workers      = 32
		opsPerWorker = 2000
		scopeCap     = 20000 // head-room: ~3.6k avg appends/scope with skew slack
	)
	scopes := []string{"sa", "sb", "sc", "sd", "se", "sf", "sg", "sh"}

	h, api := newTestHandler(scopeCap)

	type tally struct {
		appendsOK int64 // successful /append (200 response)
		deletedN  int64 // sum of deleted_count from /delete and /delete_up_to
		readsHit  int64 // /head and /tail calls that returned hit=true
	}
	tallies := make([]tally, workers)

	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(id int) {
			defer wg.Done()
			// Each worker gets its own RNG so the hot path has zero shared
			// state. Seeds are worker-unique so different workers explore
			// different op/seq sequences.
			rng := rand.New(rand.NewSource(int64(id) + 1))
			ts := &tallies[id]

			for i := 0; i < opsPerWorker; i++ {
				scope := scopes[rng.Intn(len(scopes))]
				switch roll := rng.Intn(100); {
				case roll < 45:
					// Append without id. Skipping id avoids dup-id rejections
					// muddying the appendsOK tally — every 200 really is a new item.
					body := fmt.Sprintf(`{"scope":"%s","payload":{"w":%d,"i":%d}}`, scope, id, i)
					if code, _, _ := doRequest(t, h, "POST", "/append", body); code == 200 {
						ts.appendsOK++
					}
				case roll < 65:
					// Delete by seq. Random seq in [1, 500] — most miss, which
					// is fine: we only count actual hits via deleted_count.
					seq := rng.Intn(500) + 1
					body := fmt.Sprintf(`{"scope":"%s","seq":%d}`, scope, seq)
					if _, out, _ := doRequest(t, h, "POST", "/delete", body); out != nil {
						if n, ok := out["deleted_count"].(float64); ok {
							ts.deletedN += int64(n)
						}
					}
				case roll < 75:
					// Update by seq — doesn't change item count but exercises
					// the byte-delta reservation path under the write lock.
					seq := rng.Intn(500) + 1
					body := fmt.Sprintf(`{"scope":"%s","seq":%d,"payload":{"u":%d}}`, scope, seq, i)
					_, _, _ = doRequest(t, h, "POST", "/update", body)
				case roll < 85:
					path := fmt.Sprintf("/head?scope=%s&limit=5", scope)
					if _, out, _ := doRequest(t, h, "GET", path, ""); out != nil {
						if hit, ok := out["hit"].(bool); ok && hit {
							ts.readsHit++
						}
					}
				case roll < 95:
					path := fmt.Sprintf("/tail?scope=%s&limit=5", scope)
					if _, out, _ := doRequest(t, h, "GET", path, ""); out != nil {
						if hit, ok := out["hit"].(bool); ok && hit {
							ts.readsHit++
						}
					}
				default:
					// Delete_up_to with a small max_seq. Targets the oldest
					// slice of each scope so the prefix drain path is hammered
					// while appends are still extending the tail.
					maxSeq := rng.Intn(50) + 1
					body := fmt.Sprintf(`{"scope":"%s","max_seq":%d}`, scope, maxSeq)
					if _, out, _ := doRequest(t, h, "POST", "/delete_up_to", body); out != nil {
						if n, ok := out["deleted_count"].(float64); ok {
							ts.deletedN += int64(n)
						}
					}
				}
			}
		}(w)
	}
	wg.Wait()

	var appendsOK, deletedN, readsHit int64
	for _, s := range tallies {
		appendsOK += s.appendsOK
		deletedN += s.deletedN
		readsHit += s.readsHit
	}
	t.Logf("race workload: appendsOK=%d deletedN=%d readsHit=%d (workers=%d ops/worker=%d)",
		appendsOK, deletedN, readsHit, workers, opsPerWorker)

	expectedItems := appendsOK - deletedN
	if expectedItems < 0 {
		t.Fatalf("tally arithmetic impossible: appendsOK=%d < deletedN=%d", appendsOK, deletedN)
	}

	_, stats, _ := doAdminRequest(t, h, "/stats", "")
	if got := int64(mustFloat(t, stats, "total_items")); got != expectedItems {
		t.Errorf("/stats total_items=%d want %d (appends=%d deletes=%d)",
			got, expectedItems, appendsOK, deletedN)
	}

	// Walk the store directly and re-verify every invariant.
	var sumBufBytes, recomputedBytes int64
	var totalItemsWalked int64
	var totalReadCount uint64
	storeScopes := api.store.listScopes()
	scopeOverhead := int64(len(storeScopes)) * scopeBufferOverhead
	for scopeName, buf := range storeScopes {
		buf.mu.RLock()
		sumBufBytes += buf.bytes
		totalItemsWalked += int64(len(buf.items))
		totalReadCount += buf.readCountTotal.Load()

		if got, want := len(buf.bySeq), len(buf.items); got != want {
			t.Errorf("scope %q: len(bySeq)=%d != len(items)=%d", scopeName, got, want)
		}
		nonEmptyIDs := 0
		for i := range buf.items {
			if buf.items[i].ID != "" {
				nonEmptyIDs++
			}
		}
		if got := len(buf.byID); got != nonEmptyIDs {
			t.Errorf("scope %q: len(byID)=%d != items-with-non-empty-id=%d", scopeName, got, nonEmptyIDs)
		}
		for i := 1; i < len(buf.items); i++ {
			if buf.items[i].Seq <= buf.items[i-1].Seq {
				t.Errorf("scope %q: items out of order at index %d: seq=%d <= prev=%d",
					scopeName, i, buf.items[i].Seq, buf.items[i-1].Seq)
				break
			}
		}
		if n := len(buf.items); n > 0 {
			if maxSeq := buf.items[n-1].Seq; buf.lastSeq < maxSeq {
				t.Errorf("scope %q: lastSeq=%d < max item seq=%d (must never rewind)",
					scopeName, buf.lastSeq, maxSeq)
			}
		}
		for i := range buf.items {
			recomputedBytes += approxItemSize(buf.items[i])
		}
		buf.mu.RUnlock()
	}

	if totalItemsWalked != expectedItems {
		t.Errorf("items-walked-from-store=%d != expectedItems=%d", totalItemsWalked, expectedItems)
	}
	if got := api.store.totalBytes.Load(); got != sumBufBytes+scopeOverhead {
		t.Errorf("totalBytes=%d != Σ buf.bytes + overhead=%d (ghost bytes)", got, sumBufBytes+scopeOverhead)
	}
	if got := api.store.totalBytes.Load(); got != recomputedBytes+scopeOverhead {
		t.Errorf("totalBytes=%d != recomputed-from-items + overhead=%d (counter drift)", got, recomputedBytes+scopeOverhead)
	}
	// /head and /tail are the only read paths the workers use, and both call
	// recordRead exactly once on a hit. So Σ readCountTotal must equal the
	// sum of the workers' readsHit tallies — no approximation.
	if int64(totalReadCount) != readsHit {
		t.Errorf("Σ readCountTotal=%d != tallied readsHit=%d", totalReadCount, readsHit)
	}

	// Stats counters must agree with the post-race ground truth — same
	// invariant as in the single-threaded mixed-workload test, but here
	// it also exercises every concurrent write/delete path against the
	// atomic counters.
	assertStatsCountersInvariant(t, api.store, "after parallel race workload")
}

// --- Reserved-scope HTTP-level integration tests -----------------------------
//
// These exercise the reservation contract end-to-end via the HTTP handler
// rather than via the validator or Store-method layer alone. They pin the
// guarantees an operator actually relies on:
//   - /append on _inbox round-trips the item, and the per-scope counters
//     in /scopelist plus the store-wide counters in /stats reflect the
//     append correctly.
//   - The drainer pattern (append → tail → delete_up_to) actually frees
//     items + bytes on a reserved scope.
//   - The HTTP layer rejects every operation that the reservation contract
//     forbids (/upsert, /update, /counter_add, /delete_scope, /warm,
//     /rebuild) with status 400.
//   - /wipe restores the reserved-scope baseline (scope_count=2, items=0,
//     small but non-zero approx_store_mb) so subscribers attached to
//     either reserved scope find their target still present.

// TestReservedScopes_AppendInboxRoundTrip drives the happy path: append a
// single item to _inbox and verify it shows up everywhere — /get, /tail,
// /scopelist row, /stats counters, byte budget.
//
// The byte-counter assertions go through api.store.totalBytes directly
// rather than via /stats's `approx_store_mb` because the MB serialiser
// rounds to 4 decimals (~105-byte resolution); a small payload would
// not produce a detectable delta. /stats is checked for shape + the
// values it CAN render exactly (scope_count, total_items).
func TestReservedScopes_AppendInboxRoundTrip(t *testing.T) {
	h, api := newTestHandler(10)

	// Snapshot the boot baseline via the store's exact atomic counters.
	preBytes := api.store.totalBytes.Load()
	preItems := api.store.totalItems.Load()
	preScopes := api.store.scopeCount.Load()
	if preScopes != int64(len(reservedScopeNames)) {
		t.Fatalf("pre-append scopeCount=%d want %d (reserved baseline)", preScopes, len(reservedScopeNames))
	}
	if preItems != 0 {
		t.Fatalf("pre-append totalItems=%d want 0", preItems)
	}
	if preBytes <= 0 {
		t.Fatalf("pre-append totalBytes=%d want >0 (reserved-overhead baseline)", preBytes)
	}

	// Append one item to _inbox. Payload is sized to be unambiguous at
	// MB-precision too (>105 bytes per item), but the exact byte check
	// happens against totalBytes regardless.
	body := `{"scope":"_inbox","id":"msg-1","payload":{"text":"this is a moderately sized inbox message used to ensure the byte delta is visible at MB precision"}}`
	code, out, raw := doRequest(t, h, "POST", "/append", body)
	if code != 200 {
		t.Fatalf("/append _inbox: code=%d body=%s", code, raw)
	}
	if !mustBool(t, out, "ok") {
		t.Errorf("/append: ok=false body=%s", raw)
	}
	item, _ := out["item"].(map[string]interface{})
	seq := mustFloat(t, item, "seq")
	if seq <= 0 {
		t.Errorf("/append: seq=%v want >0", seq)
	}

	// Internal counters: scope_count unchanged (append to existing scope),
	// totalItems +1, totalBytes strictly greater.
	if got := api.store.scopeCount.Load(); got != preScopes {
		t.Errorf("post-append scopeCount=%d want %d (no new scope created)", got, preScopes)
	}
	if got := api.store.totalItems.Load(); got != preItems+1 {
		t.Errorf("post-append totalItems=%d want %d", got, preItems+1)
	}
	if got := api.store.totalBytes.Load(); got <= preBytes {
		t.Errorf("post-append totalBytes=%d want >%d (item bytes added)", got, preBytes)
	}

	// /get must return the item bytes back.
	_, out, _ = doRequest(t, h, "GET", "/get?scope=_inbox&id=msg-1", "")
	if !mustBool(t, out, "hit") {
		t.Errorf("/get _inbox: hit=false")
	}
	gotItem, _ := out["item"].(map[string]interface{})
	if gotID, _ := gotItem["id"].(string); gotID != "msg-1" {
		t.Errorf("/get _inbox: id=%v want msg-1", gotItem["id"])
	}

	// /tail must surface the item too.
	_, out, _ = doRequest(t, h, "GET", "/tail?scope=_inbox&limit=10", "")
	if !mustBool(t, out, "hit") {
		t.Errorf("/tail _inbox: hit=false")
	}
	if mustFloat(t, out, "count") != 1 {
		t.Errorf("/tail _inbox: count=%v want 1", out["count"])
	}

	// /stats reports the same counts.
	_, stats, _ := doAdminRequest(t, h, "/stats", "")
	if got := mustFloat(t, stats, "scope_count"); got != float64(len(reservedScopeNames)) {
		t.Errorf("/stats scope_count=%v want %d", got, len(reservedScopeNames))
	}
	if got := mustFloat(t, stats, "total_items"); got != 1 {
		t.Errorf("/stats total_items=%v want 1", got)
	}

	// /scopelist must include _inbox with the new item count.
	_, out, _ = doRequest(t, h, "GET", "/scopelist", "")
	rows := mustScopelistEntries(t, out)
	var inboxRow map[string]interface{}
	for _, r := range rows {
		if name, _ := r["scope"].(string); name == InboxScopeName {
			inboxRow = r
			break
		}
	}
	if inboxRow == nil {
		t.Fatalf("/scopelist: _inbox row missing from %v", rows)
	}
	if got := mustFloat(t, inboxRow, "item_count"); got != 1 {
		t.Errorf("/scopelist _inbox row: item_count=%v want 1", got)
	}
	if got := mustFloat(t, inboxRow, "last_seq"); got != seq {
		t.Errorf("/scopelist _inbox row: last_seq=%v want %v", got, seq)
	}

	// Cross-check store internals match the per-scope row.
	if buf, ok := api.store.getScope(InboxScopeName); ok {
		buf.mu.RLock()
		if len(buf.items) != 1 {
			t.Errorf("buf.items=%d want 1", len(buf.items))
		}
		bufBytes := buf.bytes
		buf.mu.RUnlock()
		if bufBytes <= 0 {
			t.Errorf("buf.bytes=%d want >0", bufBytes)
		}
	} else {
		t.Errorf("api.store.getScope(_inbox) returned !ok")
	}
}

// TestReservedScopes_DrainerPattern exercises the canonical drainer flow:
// append-many → /tail → /delete_up_to → verify scope still exists with
// item_count=0. This is the operational pattern the reservation is
// designed to support.
func TestReservedScopes_DrainerPattern(t *testing.T) {
	h, _ := newTestHandler(100)

	const N = 10
	for i := 0; i < N; i++ {
		body := fmt.Sprintf(`{"scope":"_inbox","id":"msg-%d","payload":{"i":%d}}`, i, i)
		if code, _, raw := doRequest(t, h, "POST", "/append", body); code != 200 {
			t.Fatalf("/append iter %d: code=%d body=%s", i, code, raw)
		}
	}

	// Drainer reads in bulk via /tail.
	_, out, _ := doRequest(t, h, "GET", "/tail?scope=_inbox&limit=100", "")
	if got := mustFloat(t, out, "count"); got != N {
		t.Fatalf("/tail _inbox: count=%v want %d", got, N)
	}
	items, _ := out["items"].([]interface{})
	last, _ := items[len(items)-1].(map[string]interface{})
	lastSeq := mustFloat(t, last, "seq")

	// Drainer cleanup via /delete_up_to.
	body := fmt.Sprintf(`{"scope":"_inbox","max_seq":%d}`, int64(lastSeq))
	code, out, raw := doRequest(t, h, "POST", "/delete_up_to", body)
	if code != 200 {
		t.Fatalf("/delete_up_to _inbox: code=%d body=%s", code, raw)
	}
	if got := mustFloat(t, out, "deleted_count"); got != N {
		t.Errorf("/delete_up_to _inbox: deleted_count=%v want %d", got, N)
	}

	// Scope must still exist, but be empty.
	_, out, _ = doRequest(t, h, "GET", "/scopelist?prefix=_inbox", "")
	rows := mustScopelistEntries(t, out)
	if len(rows) != 1 {
		t.Fatalf("/scopelist after drain: got %d rows want 1", len(rows))
	}
	if got := mustFloat(t, rows[0], "item_count"); got != 0 {
		t.Errorf("/scopelist _inbox after drain: item_count=%v want 0", got)
	}

	// Re-appending to the drained _inbox must still work — this is the
	// whole point of "drain doesn't destroy the scope".
	if code, _, raw := doRequest(t, h, "POST", "/append",
		`{"scope":"_inbox","id":"msg-after-drain","payload":{"v":1}}`); code != 200 {
		t.Fatalf("/append after drain: code=%d body=%s", code, raw)
	}
}

// TestReservedScopes_HTTPRejections asserts every forbidden operation
// returns 400 over the wire on _events and _inbox.
func TestReservedScopes_HTTPRejections(t *testing.T) {
	for _, scope := range reservedScopeNames {
		scope := scope
		t.Run("scope="+scope, func(t *testing.T) {
			h, _ := newTestHandler(10)

			// Pre-seed an item so /update has something to (notionally) target.
			if code, _, _ := doRequest(t, h, "POST", "/append",
				fmt.Sprintf(`{"scope":%q,"id":"x","payload":{"v":1}}`, scope)); code != 200 {
				t.Fatalf("pre-seed append: code=%d", code)
			}

			cases := []struct {
				name, method, path, body string
			}{
				{"upsert", "POST", "/upsert", fmt.Sprintf(`{"scope":%q,"id":"x","payload":{"v":2}}`, scope)},
				{"update", "POST", "/update", fmt.Sprintf(`{"scope":%q,"id":"x","payload":{"v":3}}`, scope)},
				{"counter_add", "POST", "/counter_add", fmt.Sprintf(`{"scope":%q,"id":"c","by":1}`, scope)},
				{"delete_scope", "POST", "/delete_scope", fmt.Sprintf(`{"scope":%q}`, scope)},
				{"warm", "POST", "/warm", fmt.Sprintf(`{"items":[{"scope":%q,"id":"x","payload":{"v":1}}]}`, scope)},
				{"rebuild", "POST", "/rebuild", fmt.Sprintf(`{"items":[{"scope":%q,"id":"x","payload":{"v":1}}]}`, scope)},
			}
			for _, c := range cases {
				code, out, raw := doRequest(t, h, c.method, c.path, c.body)
				if code != 400 {
					t.Errorf("%s on %q: code=%d want 400 body=%s", c.name, scope, code, raw)
					continue
				}
				errStr, _ := out["error"].(string)
				if !strings.Contains(errStr, "reserved") {
					t.Errorf("%s on %q: error=%q does not mention 'reserved'", c.name, scope, errStr)
				}
			}

			// Sanity: legitimate ops MUST still work after the rejections.
			if code, _, raw := doRequest(t, h, "POST", "/append",
				fmt.Sprintf(`{"scope":%q,"id":"y","payload":{"v":1}}`, scope)); code != 200 {
				t.Errorf("/append after rejections: code=%d body=%s", code, raw)
			}
			if code, out, _ := doRequest(t, h, "GET",
				fmt.Sprintf("/get?scope=%s&id=x", scope), ""); code != 200 || !mustBool(t, out, "hit") {
				t.Errorf("/get after rejections: code=%d hit=%v", code, out["hit"])
			}
		})
	}
}

// TestReservedScopes_WipeRestoresBaseline pins the lifecycle guarantee:
// after /wipe, the reserved scopes are still present (re-created under the
// same all-shard write lock) so subscribers attached to either reserved
// scope find their target waiting for them. Pre-Subscribe this is verified
// at the cache level only; once Subscribe ships the same property
// guarantees subscribers don't see channel-close.
func TestReservedScopes_WipeRestoresBaseline(t *testing.T) {
	h, _ := newTestHandler(10)

	// Add a user scope plus content in both reserved scopes.
	if code, _, _ := doRequest(t, h, "POST", "/append",
		`{"scope":"user-data","id":"u1","payload":{"v":1}}`); code != 200 {
		t.Fatalf("/append user-data: code=%d", code)
	}
	if code, _, _ := doRequest(t, h, "POST", "/append",
		`{"scope":"_inbox","id":"i1","payload":{"v":1}}`); code != 200 {
		t.Fatalf("/append _inbox: code=%d", code)
	}
	if code, _, _ := doRequest(t, h, "POST", "/append",
		`{"scope":"_events","id":"l1","payload":{"v":1}}`); code != 200 {
		t.Fatalf("/append _events: code=%d", code)
	}

	// Wipe everything.
	if code, _, _ := doAdminRequest(t, h, "/wipe", ""); code != 200 {
		t.Fatalf("/wipe: code=%d", code)
	}

	// /stats must show reserved baseline restored: 2 scopes, 0 items,
	// approx_store_mb is the reserved-scope overhead (small but non-zero).
	_, stats, _ := doAdminRequest(t, h, "/stats", "")
	if got := mustFloat(t, stats, "scope_count"); got != float64(len(reservedScopeNames)) {
		t.Errorf("post-wipe scope_count=%v want %d (reserved restored)", got, len(reservedScopeNames))
	}
	if got := mustFloat(t, stats, "total_items"); got != 0 {
		t.Errorf("post-wipe total_items=%v want 0", got)
	}
	if got := mustFloat(t, stats, "approx_store_mb"); got <= 0 {
		t.Errorf("post-wipe approx_store_mb=%v want >0 (reserved overhead)", got)
	}

	// /scopelist enumerates exactly the two reserved names.
	_, out, _ := doRequest(t, h, "GET", "/scopelist", "")
	rows := mustScopelistEntries(t, out)
	if len(rows) != len(reservedScopeNames) {
		t.Fatalf("post-wipe /scopelist: got %d rows want %d", len(rows), len(reservedScopeNames))
	}
	got := make(map[string]bool)
	for _, r := range rows {
		name, _ := r["scope"].(string)
		got[name] = true
		if c := mustFloat(t, r, "item_count"); c != 0 {
			t.Errorf("post-wipe row %q: item_count=%v want 0", name, c)
		}
	}
	for _, name := range reservedScopeNames {
		if !got[name] {
			t.Errorf("post-wipe: reserved scope %q missing from /scopelist", name)
		}
	}

	// User scope is gone (wiped, not restored).
	_, out, _ = doRequest(t, h, "GET", "/get?scope=user-data&id=u1", "")
	if mustBool(t, out, "hit") {
		t.Error("post-wipe: user-data/u1 still present (should be wiped)")
	}

	// Reserved scopes are usable again — the post-wipe baseline isn't a
	// dead state, the cache is ready for new traffic immediately.
	if code, _, raw := doRequest(t, h, "POST", "/append",
		`{"scope":"_inbox","id":"after-wipe","payload":{"v":1}}`); code != 200 {
		t.Fatalf("/append _inbox after wipe: code=%d body=%s", code, raw)
	}
}

// TestReservedScopes_RebuildRestoresBaseline mirrors WipeRestoresBaseline
// but for /rebuild, which has the additional twist that input containing a
// reserved scope must be rejected before any state mutation.
func TestReservedScopes_RebuildRestoresBaseline(t *testing.T) {
	h, _ := newTestHandler(10)

	// Pre-seed both reserved and user content.
	if code, _, _ := doRequest(t, h, "POST", "/append",
		`{"scope":"_inbox","id":"i1","payload":{"v":1}}`); code != 200 {
		t.Fatalf("/append _inbox: code=%d", code)
	}
	if code, _, _ := doRequest(t, h, "POST", "/append",
		`{"scope":"original","id":"o1","payload":{"v":1}}`); code != 200 {
		t.Fatalf("/append original: code=%d", code)
	}

	// Rebuild with input that does NOT include reserved scopes.
	rebuildBody := `{"items":[
		{"scope":"new-a","id":"a1","payload":{"v":1}},
		{"scope":"new-b","id":"b1","payload":{"v":1}}
	]}`
	if code, _, raw := doRequest(t, h, "POST", "/rebuild", rebuildBody); code != 200 {
		t.Fatalf("/rebuild: code=%d body=%s", code, raw)
	}

	// Post-rebuild: 2 user scopes from input + 2 reserved scopes = 4.
	_, stats, _ := doAdminRequest(t, h, "/stats", "")
	if got := mustFloat(t, stats, "scope_count"); got != float64(2+len(reservedScopeNames)) {
		t.Errorf("post-rebuild scope_count=%v want %d", got, 2+len(reservedScopeNames))
	}
	if got := mustFloat(t, stats, "total_items"); got != 2 {
		t.Errorf("post-rebuild total_items=%v want 2 (only the 2 input items)", got)
	}

	// Original user scope is gone; reserved scope contents are also gone
	// (rebuild dropped everything, then re-init created empty reserved).
	_, out, _ := doRequest(t, h, "GET", "/get?scope=original&id=o1", "")
	if mustBool(t, out, "hit") {
		t.Error("post-rebuild: original/o1 still present")
	}
	_, out, _ = doRequest(t, h, "GET", "/get?scope=_inbox&id=i1", "")
	if mustBool(t, out, "hit") {
		t.Error("post-rebuild: _inbox/i1 still present (rebuild drops, init re-creates empty)")
	}

	// Reserved scopes accept new traffic immediately.
	if code, _, raw := doRequest(t, h, "POST", "/append",
		`{"scope":"_events","id":"after-rebuild","payload":{"v":1}}`); code != 200 {
		t.Fatalf("/append _events after rebuild: code=%d body=%s", code, raw)
	}
}

// newReservedScopesTestHandler constructs a Store + API with a custom
// Config so the per-reserved-scope cap tests can drive the knobs that
// the default newTestHandler doesn't expose. Same shape as that helper
// — accepts Config, hands you (mux, api).
func newReservedScopesTestHandler(t *testing.T, cfg Config) (http.Handler, *API) {
	t.Helper()
	api := NewAPI(NewGateway(cfg), APIConfig{})
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)
	return mux, api
}

// _events is exempt from ScopeMaxItems: best-effort observability gated
// only by the global byte budget, never by an arbitrary item-count.
// Cross-checks that the same store still enforces ScopeMaxItems on
// user-scopes — the exemption must be scoped to `_events` alone, not
// leaked store-wide.
func TestReservedScopes_EventsExemptFromScopeMaxItems(t *testing.T) {
	// Tiny ScopeMaxItems = 3. _events should accept far more than that;
	// a regular scope should 507 on the 4th item.
	h, _ := newReservedScopesTestHandler(t, Config{
		ScopeMaxItems: 3,
		MaxStoreBytes: 100 << 20,
		MaxItemBytes:  1 << 20,
	})

	// Append 10 items to _events — all must succeed even though
	// ScopeMaxItems = 3 globally.
	for i := 0; i < 10; i++ {
		body := fmt.Sprintf(`{"scope":"_events","id":"e-%d","payload":{"i":%d}}`, i, i)
		code, _, raw := doRequest(t, h, "POST", "/append", body)
		if code != 200 {
			t.Fatalf("append #%d to _events: code=%d body=%s (cap exemption broken)", i, code, raw)
		}
	}

	// Cross-check: the same store still 507s a user-scope at the 4th
	// item, proving the exemption is scoped to _events only.
	for i := 0; i < 3; i++ {
		body := fmt.Sprintf(`{"scope":"user","id":"u-%d","payload":{"i":%d}}`, i, i)
		if code, _, raw := doRequest(t, h, "POST", "/append", body); code != 200 {
			t.Fatalf("append #%d to user-scope: code=%d body=%s (under cap, must succeed)", i, code, raw)
		}
	}
	body := `{"scope":"user","id":"u-overflow","payload":{"i":3}}`
	if code, _, raw := doRequest(t, h, "POST", "/append", body); code != 507 {
		t.Errorf("4th append to user-scope: code=%d (want 507) body=%s", code, raw)
	}
}

// _inbox enforces an operator-tunable per-item byte cap that defaults
// to 64 KiB and is independent of MaxItemBytes (which targets user-
// scopes). A payload below the inbox cap succeeds; above it produces
// 400. The same payload must still succeed against a user-scope that
// only has to clear the larger global cap — proves the cap is scoped
// to _inbox.
func TestReservedScopes_InboxRespectsCustomItemBytes(t *testing.T) {
	// Inbox cap deliberately smaller than the global MaxItemBytes so
	// the test can construct a payload that's legal globally but
	// rejected by _inbox.
	h, _ := newReservedScopesTestHandler(t, Config{
		ScopeMaxItems: 1000,
		MaxStoreBytes: 100 << 20,
		MaxItemBytes:  1 << 20,                                            // 1 MiB
		Inbox:         InboxConfig{MaxItems: 1000, MaxItemBytes: 4 << 10}, // 4 KiB
	})

	// 8 KiB of opaque bytes — over the 4 KiB inbox cap, well under
	// the 1 MiB global cap.
	bigPayload := strings.Repeat("a", 8<<10)

	// Reject on _inbox.
	body := fmt.Sprintf(`{"scope":"_inbox","id":"big","payload":"%s"}`, bigPayload)
	code, out, raw := doRequest(t, h, "POST", "/append", body)
	if code != 400 {
		t.Fatalf("/append _inbox with 8 KiB payload: code=%d body=%s want 400", code, raw)
	}
	if errMsg, _ := out["error"].(string); !strings.Contains(errMsg, "size") {
		t.Errorf("expected size-related error, got %q", errMsg)
	}

	// Same payload to a user-scope succeeds (global cap is 1 MiB).
	body = fmt.Sprintf(`{"scope":"user","id":"big","payload":"%s"}`, bigPayload)
	if code, _, raw := doRequest(t, h, "POST", "/append", body); code != 200 {
		t.Errorf("/append user with 8 KiB payload: code=%d body=%s want 200 (under global cap)", code, raw)
	}

	// Small payload to _inbox succeeds (under inbox cap).
	body = `{"scope":"_inbox","id":"small","payload":{"v":1}}`
	if code, _, raw := doRequest(t, h, "POST", "/append", body); code != 200 {
		t.Errorf("/append _inbox with small payload: code=%d body=%s want 200", code, raw)
	}
}

// _inbox enforces an operator-tunable per-scope item-count cap that
// defaults to ScopeMaxItems but can be tuned independently. With a
// custom small Inbox.MaxItems, /append to _inbox 507s past the cap
// while user-scopes are still bounded by the (different) global
// ScopeMaxItems.
func TestReservedScopes_InboxRespectsCustomItemCount(t *testing.T) {
	h, _ := newReservedScopesTestHandler(t, Config{
		ScopeMaxItems: 100,
		MaxStoreBytes: 100 << 20,
		MaxItemBytes:  1 << 20,
		Inbox:         InboxConfig{MaxItems: 3, MaxItemBytes: 64 << 10},
	})

	// Three appends fit; the fourth 507s.
	for i := 0; i < 3; i++ {
		body := fmt.Sprintf(`{"scope":"_inbox","id":"i-%d","payload":{"i":%d}}`, i, i)
		if code, _, raw := doRequest(t, h, "POST", "/append", body); code != 200 {
			t.Fatalf("/append #%d _inbox: code=%d body=%s", i, code, raw)
		}
	}
	body := `{"scope":"_inbox","id":"i-overflow","payload":{"i":3}}`
	if code, _, raw := doRequest(t, h, "POST", "/append", body); code != 507 {
		t.Errorf("4th /append _inbox: code=%d body=%s want 507 (Inbox.MaxItems=3)", code, raw)
	}

	// Cross-check: user-scope tracks the global ScopeMaxItems = 100,
	// so a 4th user-write succeeds where the inbox 507'd.
	for i := 0; i < 4; i++ {
		body := fmt.Sprintf(`{"scope":"user","id":"u-%d","payload":{"i":%d}}`, i, i)
		if code, _, raw := doRequest(t, h, "POST", "/append", body); code != 200 {
			t.Fatalf("/append #%d user: code=%d body=%s (global cap is 100)", i, code, raw)
		}
	}
}

// _events's per-item byte cap is derived (MaxItemBytes + 1 KiB envelope
// slack) so the cap is always at least as wide as any user-write. A
// payload that's accepted by _events but rejected by user-scope proves
// _events's cap really is the global cap plus the slack — and a payload
// past the derived cap is still rejected on _events itself.
//
// The test uses a JSON-object payload (not a top-level string)
// deliberately: top-level JSON-string payloads pre-render at write
// time and are charged for both the raw bytes AND the decoded form
// in approxItemSize, so payload-byte arithmetic doubles. Objects
// skip renderBytes; len(Payload) is the only payload cost the cap
// sees.
func TestReservedScopes_EventsDerivedItemBytesCap(t *testing.T) {
	const globalCap = 8 << 10 // 8 KiB; _events derives globalCap + 1 KiB.
	h, _ := newReservedScopesTestHandler(t, Config{
		ScopeMaxItems: 1000,
		MaxStoreBytes: 100 << 20,
		MaxItemBytes:  globalCap,
	})

	// Object payload: `{"d":"<filler>"}` — 8 bytes of JSON framing
	// plus the filler. Envelope (scope+id+seq+ts+base) adds ~56 B
	// for the short scope/id strings used here, so 200 B of
	// headroom is comfortable.
	mkPayload := func(totalBytes int) string {
		filler := strings.Repeat("a", totalBytes-8) // -len(`{"d":""}`)
		return fmt.Sprintf(`{"d":"%s"}`, filler)
	}

	// Fits _events's derived 9 KiB cap (with envelope ≈ 8556 B), past
	// the user's 8 KiB cap.
	logOnly := mkPayload(globalCap + 256)

	body := fmt.Sprintf(`{"scope":"user","id":"big","payload":%s}`, logOnly)
	if code, _, raw := doRequest(t, h, "POST", "/append", body); code != 400 {
		t.Errorf("/append user with payload > global cap: code=%d body=%s want 400", code, raw)
	}
	body = fmt.Sprintf(`{"scope":"_events","id":"big","payload":%s}`, logOnly)
	if code, _, raw := doRequest(t, h, "POST", "/append", body); code != 200 {
		t.Errorf("/append _events with same payload: code=%d body=%s want 200 (within derived cap)", code, raw)
	}

	// Past _events's derived cap (global + 1 KiB) — even _events says no.
	overEventsCap := mkPayload(globalCap + (1 << 10) + 256)
	body = fmt.Sprintf(`{"scope":"_events","id":"too-big","payload":%s}`, overEventsCap)
	if code, _, raw := doRequest(t, h, "POST", "/append", body); code != 400 {
		t.Errorf("/append _events with payload past derived cap: code=%d body=%s want 400", code, raw)
	}
}

// eventsTailCount is a tiny helper for the auto-populate tests that
// follow: hits /tail on `_events`, asserts the response is OK, and
// returns the count + items list. Mirrors the inlined pattern used
// elsewhere in this file but factored out because the auto-populate
// tests all need the same probe.
func eventsTailCount(t *testing.T, h http.Handler) (int, []map[string]interface{}) {
	t.Helper()
	code, out, raw := doRequest(t, h, "GET", "/tail?scope=_events&limit=100", "")
	if code != 200 {
		t.Fatalf("/tail _events: code=%d body=%s", code, raw)
	}
	count := int(mustFloat(t, out, "count"))
	rawItems, _ := out["items"].([]interface{})
	items := make([]map[string]interface{}, 0, len(rawItems))
	for _, ri := range rawItems {
		if m, ok := ri.(map[string]interface{}); ok {
			items = append(items, m)
		}
	}
	return count, items
}

// EventsModeOff is the default and zero-value: a fresh Config{} with
// no Events field set must produce an empty `_events` regardless of
// how many user-scope writes happen.
func TestEvents_AutoPopulate_Off(t *testing.T) {
	h, _ := newReservedScopesTestHandler(t, Config{
		ScopeMaxItems: 100,
		MaxStoreBytes: 100 << 20,
		MaxItemBytes:  1 << 20,
		// Events.Mode left at zero-value = EventsModeOff.
	})

	for i := 0; i < 5; i++ {
		body := fmt.Sprintf(`{"scope":"posts","id":"p-%d","payload":{"v":%d}}`, i, i)
		if code, _, raw := doRequest(t, h, "POST", "/append", body); code != 200 {
			t.Fatalf("/append #%d: code=%d body=%s", i, code, raw)
		}
	}

	if count, _ := eventsTailCount(t, h); count != 0 {
		t.Errorf("EventsModeOff: _events count=%d want 0", count)
	}
}

// EventsModeNotify produces one event per /append. Each event's
// payload is a JSON object with the action-vector (op, scope, id,
// seq, ts) but NO `payload` field — Notify mode strips the user
// payload. Drainers re-fetching from cache state on wake-up don't
// need the inline payload; addressing is sufficient.
func TestEvents_AutoPopulate_Notify(t *testing.T) {
	h, api := newReservedScopesTestHandler(t, Config{
		ScopeMaxItems: 100,
		MaxStoreBytes: 100 << 20,
		MaxItemBytes:  1 << 20,
		Events:        EventsConfig{Mode: EventsModeNotify},
	})

	for i := 0; i < 3; i++ {
		body := fmt.Sprintf(`{"scope":"posts","id":"p-%d","payload":{"v":%d}}`, i, i)
		if code, _, raw := doRequest(t, h, "POST", "/append", body); code != 200 {
			t.Fatalf("/append #%d: code=%d body=%s", i, code, raw)
		}
	}

	count, items := eventsTailCount(t, h)
	if count != 3 {
		t.Fatalf("Notify mode: _events count=%d want 3", count)
	}
	for i, evt := range items {
		evtPayload, ok := evt["payload"].(map[string]interface{})
		if !ok {
			t.Fatalf("event %d: payload not an object: %v", i, evt["payload"])
		}
		if evtPayload["op"] != "append" {
			t.Errorf("event %d: op=%v want append", i, evtPayload["op"])
		}
		if evtPayload["scope"] != "posts" {
			t.Errorf("event %d: scope=%v want posts", i, evtPayload["scope"])
		}
		if _, hasUserPayload := evtPayload["payload"]; hasUserPayload {
			t.Errorf("event %d: Notify mode must omit user payload, got %v", i, evtPayload)
		}
		// id and seq must be carried in the event envelope.
		if evtPayload["id"] == nil {
			t.Errorf("event %d: id missing", i)
		}
		if evtPayload["seq"] == nil {
			t.Errorf("event %d: seq missing", i)
		}
	}

	if drops := api.store.eventsDropsTotal.Load(); drops != 0 {
		t.Errorf("Notify mode (no cap pressure): eventsDropsTotal=%d want 0", drops)
	}
}

// EventsModeFull adds the user payload to the event envelope. The
// inner JSON object is what /tail returns under "payload"; inside
// that, a nested "payload" field carries the original user-write.
func TestEvents_AutoPopulate_Full(t *testing.T) {
	h, _ := newReservedScopesTestHandler(t, Config{
		ScopeMaxItems: 100,
		MaxStoreBytes: 100 << 20,
		MaxItemBytes:  1 << 20,
		Events:        EventsConfig{Mode: EventsModeFull},
	})

	body := `{"scope":"posts","id":"hello","payload":{"title":"Hi","n":42}}`
	if code, _, raw := doRequest(t, h, "POST", "/append", body); code != 200 {
		t.Fatalf("/append: code=%d body=%s", code, raw)
	}

	count, items := eventsTailCount(t, h)
	if count != 1 {
		t.Fatalf("Full mode: _events count=%d want 1", count)
	}
	evtPayload, ok := items[0]["payload"].(map[string]interface{})
	if !ok {
		t.Fatalf("event payload not an object: %v", items[0]["payload"])
	}
	userPayload, ok := evtPayload["payload"].(map[string]interface{})
	if !ok {
		t.Fatalf("Full mode must include user payload as nested object, got %v", evtPayload)
	}
	if userPayload["title"] != "Hi" {
		t.Errorf("user payload title=%v want Hi", userPayload["title"])
	}
	if userPayload["n"] != float64(42) { // JSON numbers decode to float64
		t.Errorf("user payload n=%v want 42", userPayload["n"])
	}
}

// EventsModeFull at scale: 100 distinct /append calls produce 100
// envelopes in `_events`, in append-order, each carrying its own
// action-vector + nested user-payload. Verifies the auto-populate
// path is per-write (not batched, not deduped) and that the
// envelope-shape is stable across N writes — not just the 1-call
// happy path.
//
// /tail returns oldest-first within the requested window
// (buffer_read.go tailOffset), so rawItems[i] corresponds to the
// i-th /append: id="p-i", user-scope seq=i+1 (fresh "posts"
// scope), and the nested payload carries n=i + label="item-i".
//
// Dumps the first 3 envelopes via t.Logf so `go test -v` makes the
// wire shape concrete for human readers.
func TestEvents_AutoPopulate_Full_Many(t *testing.T) {
	h, _ := newReservedScopesTestHandler(t, Config{
		ScopeMaxItems: 1000,
		MaxStoreBytes: 100 << 20,
		MaxItemBytes:  1 << 20,
		Events:        EventsConfig{Mode: EventsModeFull},
	})

	const N = 100
	for i := 0; i < N; i++ {
		body := fmt.Sprintf(
			`{"scope":"posts","id":"p-%d","payload":{"n":%d,"label":"item-%d"}}`,
			i, i, i,
		)
		if code, _, raw := doRequest(t, h, "POST", "/append", body); code != 200 {
			t.Fatalf("/append #%d: code=%d body=%s", i, code, raw)
		}
	}

	code, out, raw := doRequest(t, h, "GET",
		fmt.Sprintf("/tail?scope=_events&limit=%d", N), "")
	if code != 200 {
		t.Fatalf("/tail _events: code=%d body=%s", code, raw)
	}
	count := int(mustFloat(t, out, "count"))
	if count != N {
		t.Fatalf("Full mode N=%d: _events count=%d want %d", N, count, N)
	}
	rawItems, _ := out["items"].([]interface{})
	if len(rawItems) != N {
		t.Fatalf("Full mode N=%d: items len=%d want %d", N, len(rawItems), N)
	}

	for i, ri := range rawItems {
		envItem, ok := ri.(map[string]interface{})
		if !ok {
			t.Fatalf("event %d: not an object: %v", i, ri)
		}
		evt, ok := envItem["payload"].(map[string]interface{})
		if !ok {
			t.Fatalf("event %d: envelope payload not an object: %v", i, envItem["payload"])
		}
		if evt["op"] != "append" {
			t.Errorf("event %d: op=%v want append", i, evt["op"])
		}
		if evt["scope"] != "posts" {
			t.Errorf("event %d: scope=%v want posts", i, evt["scope"])
		}
		wantID := fmt.Sprintf("p-%d", i)
		if evt["id"] != wantID {
			t.Errorf("event %d: id=%v want %s", i, evt["id"], wantID)
		}
		seq, ok := evt["seq"].(float64)
		if !ok || seq != float64(i+1) {
			t.Errorf("event %d: seq=%v want %d", i, evt["seq"], i+1)
		}
		userPayload, ok := evt["payload"].(map[string]interface{})
		if !ok {
			t.Fatalf("event %d: nested user payload not object: %v", i, evt["payload"])
		}
		if userPayload["n"] != float64(i) {
			t.Errorf("event %d: user payload n=%v want %d", i, userPayload["n"], i)
		}
		wantLabel := fmt.Sprintf("item-%d", i)
		if userPayload["label"] != wantLabel {
			t.Errorf("event %d: user payload label=%v want %s", i, userPayload["label"], wantLabel)
		}
	}

	// Dump the first 3 envelopes so `go test -v -run
	// TestEvents_AutoPopulate_Full_Many` makes the wire shape
	// concrete for human readers.
	for i := 0; i < 3 && i < len(rawItems); i++ {
		pretty, err := json.MarshalIndent(rawItems[i], "", "  ")
		if err != nil {
			continue
		}
		t.Logf("envelope #%d:\n%s", i, pretty)
	}
}

// Recursion guard: an external /append directly to `_events`
// (allowed but unusual per RFC §2.6 + decide-doc settled #15) must
// NOT trigger the auto-populate machinery to write a SECOND event
// for "the cache observed the previous write to _events". Without
// the guard the cache would loop, doubling _events on every
// external write.
func TestEvents_AutoPopulate_RecursionGuard(t *testing.T) {
	h, _ := newReservedScopesTestHandler(t, Config{
		ScopeMaxItems: 100,
		MaxStoreBytes: 100 << 20,
		MaxItemBytes:  1 << 20,
		Events:        EventsConfig{Mode: EventsModeFull},
	})

	body := `{"scope":"_events","id":"manual","payload":{"v":1}}`
	if code, _, raw := doRequest(t, h, "POST", "/append", body); code != 200 {
		t.Fatalf("/append _events: code=%d body=%s", code, raw)
	}

	// Exactly 1 entry — the manual write — and zero auto-populated.
	if count, _ := eventsTailCount(t, h); count != 1 {
		t.Errorf("recursion guard broken: _events count=%d want 1 (only the manual write)", count)
	}
}

// Drop-on-overflow: when `_events` cannot accept the auto-populated
// event because the store byte budget is saturated, the underlying
// user-write STILL succeeds (the user-scope commit happened first)
// and Store.eventsDropsTotal is bumped. Operators see the drop via
// /stats once that enrichment lands; for now the test reads the
// counter directly.
func TestEvents_AutoPopulate_DropsOnOverflow(t *testing.T) {
	// Tight byte budget: reserved-scope overhead for _events +
	// _inbox (2 KiB) + scope-buffer overhead for "posts" (1 KiB) +
	// just enough room for a tiny user item, but NOT for the event
	// the auto-populate path would emit (Full mode events are larger
	// than the user item because they wrap the payload in an envelope
	// with op/scope/id/seq/ts).
	cfg := Config{
		ScopeMaxItems: 100,
		MaxStoreBytes: reservedScopesOverhead + scopeBufferOverhead + 100,
		MaxItemBytes:  1 << 20,
		Events:        EventsConfig{Mode: EventsModeFull},
	}
	api := NewAPI(NewGateway(cfg), APIConfig{})
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	// Tiny user payload — fits in the 100-byte slack after reserved
	// + posts-buffer overhead.
	if code, _, raw := doRequest(t, mux, "POST", "/append",
		`{"scope":"posts","id":"a","payload":{"v":1}}`); code != 200 {
		t.Fatalf("user /append: code=%d body=%s want 200 (event drop must not affect user-write)",
			code, raw)
	}

	if drops := api.store.eventsDropsTotal.Load(); drops == 0 {
		t.Errorf("eventsDropsTotal=0; expected at least one drop on tight cap (Full mode event > 100 bytes slack)")
	}

	// Cross-check: the user-scope item really is in place.
	if code, out, _ := doRequest(t, mux, "GET", "/get?scope=posts&id=a", ""); code != 200 || !mustBool(t, out, "hit") {
		t.Errorf("user-write not reachable post-drop: code=%d hit=%v", code, out["hit"])
	}
}

// /upsert auto-populate: same envelope shape as /append (scope, id,
// seq, ts, payload?) — only the op string differs. Test covers both
// the create branch (fresh id) and the replace branch (existing id);
// drainers should see "upsert" in both cases (action-logging: the
// action is "upsert this id with this payload" regardless of outcome).
func TestEvents_AutoPopulate_Upsert(t *testing.T) {
	h, _ := newReservedScopesTestHandler(t, Config{
		ScopeMaxItems: 100,
		MaxStoreBytes: 100 << 20,
		MaxItemBytes:  1 << 20,
		Events:        EventsConfig{Mode: EventsModeFull},
	})

	// Create branch.
	if code, _, raw := doRequest(t, h, "POST", "/upsert",
		`{"scope":"posts","id":"a","payload":{"v":1}}`); code != 200 {
		t.Fatalf("/upsert create: code=%d body=%s", code, raw)
	}
	// Replace branch.
	if code, _, raw := doRequest(t, h, "POST", "/upsert",
		`{"scope":"posts","id":"a","payload":{"v":2}}`); code != 200 {
		t.Fatalf("/upsert replace: code=%d body=%s", code, raw)
	}

	count, items := eventsTailCount(t, h)
	if count != 2 {
		t.Fatalf("upsert auto-populate: _events count=%d want 2", count)
	}
	for i, evt := range items {
		envelope, ok := evt["payload"].(map[string]interface{})
		if !ok {
			t.Fatalf("event %d: envelope payload not an object: %v", i, evt["payload"])
		}
		if envelope["op"] != "upsert" {
			t.Errorf("event %d: op=%v want upsert", i, envelope["op"])
		}
		if envelope["scope"] != "posts" || envelope["id"] != "a" {
			t.Errorf("event %d: addressing wrong, got scope=%v id=%v", i, envelope["scope"], envelope["id"])
		}
		// Both events carry the user payload (Full mode).
		userPayload, ok := envelope["payload"].(map[string]interface{})
		if !ok {
			t.Fatalf("event %d: nested user payload not object: %v", i, envelope["payload"])
		}
		if userPayload["v"] != float64(i+1) { // 1, then 2
			t.Errorf("event %d: user payload v=%v want %d", i, userPayload["v"], i+1)
		}
	}
}

// /upsert in Notify mode: action-vector preserved, user payload
// stripped (same Notify rule as /append).
func TestEvents_AutoPopulate_Upsert_Notify(t *testing.T) {
	h, _ := newReservedScopesTestHandler(t, Config{
		ScopeMaxItems: 100,
		MaxStoreBytes: 100 << 20,
		MaxItemBytes:  1 << 20,
		Events:        EventsConfig{Mode: EventsModeNotify},
	})
	if code, _, raw := doRequest(t, h, "POST", "/upsert",
		`{"scope":"posts","id":"a","payload":{"v":1}}`); code != 200 {
		t.Fatalf("/upsert: code=%d body=%s", code, raw)
	}
	count, items := eventsTailCount(t, h)
	if count != 1 {
		t.Fatalf("Notify mode: _events count=%d want 1", count)
	}
	envelope, _ := items[0]["payload"].(map[string]interface{})
	if envelope["op"] != "upsert" {
		t.Errorf("op=%v want upsert", envelope["op"])
	}
	if _, hasPayload := envelope["payload"]; hasPayload {
		t.Errorf("Notify mode must strip user payload, got %v", envelope)
	}
}

// /update auto-populate: emit on hit only. Tests three branches:
// (1) update-by-id (envelope carries id, no seq)
// (2) update-by-seq (envelope carries seq, no id)
// (3) update on a missing id (no emit — action-logging principle:
//
//	a no-op request is not a state change worth logging)
func TestEvents_AutoPopulate_Update(t *testing.T) {
	h, _ := newReservedScopesTestHandler(t, Config{
		ScopeMaxItems: 100,
		MaxStoreBytes: 100 << 20,
		MaxItemBytes:  1 << 20,
		Events:        EventsConfig{Mode: EventsModeFull},
	})

	// Seed: one item we can update by id, one by seq.
	if code, _, raw := doRequest(t, h, "POST", "/append",
		`{"scope":"posts","id":"a","payload":{"v":0}}`); code != 200 {
		t.Fatalf("seed by-id: code=%d body=%s", code, raw)
	}
	if code, _, raw := doRequest(t, h, "POST", "/append",
		`{"scope":"posts","payload":{"v":0}}`); code != 200 {
		t.Fatalf("seed by-seq: code=%d body=%s", code, raw)
	}
	// Two seed events expected; baseline.
	baseline, _ := eventsTailCount(t, h)
	if baseline != 2 {
		t.Fatalf("after seed: _events count=%d want 2", baseline)
	}

	// (1) update-by-id.
	if code, _, raw := doRequest(t, h, "POST", "/update",
		`{"scope":"posts","id":"a","payload":{"v":1}}`); code != 200 {
		t.Fatalf("/update by-id: code=%d body=%s", code, raw)
	}
	// (2) update-by-seq (the second seeded item is at seq=2).
	if code, _, raw := doRequest(t, h, "POST", "/update",
		`{"scope":"posts","seq":2,"payload":{"v":2}}`); code != 200 {
		t.Fatalf("/update by-seq: code=%d body=%s", code, raw)
	}
	// (3) update miss (id that doesn't exist) — must NOT emit.
	if code, _, _ := doRequest(t, h, "POST", "/update",
		`{"scope":"posts","id":"nope","payload":{"v":3}}`); code != 200 {
		t.Fatalf("/update miss: code=%d want 200 (miss is not an error)", code)
	}

	count, items := eventsTailCount(t, h)
	if count != 4 { // 2 seeds + 2 updates (miss skipped)
		t.Fatalf("after updates: _events count=%d want 4 (2 seeds + 2 updates, miss must not emit)", count)
	}

	// items[2] is the by-id update; items[3] is the by-seq update.
	byID, _ := items[2]["payload"].(map[string]interface{})
	if byID["op"] != "update" || byID["id"] != "a" {
		t.Errorf("by-id update envelope wrong: %v", byID)
	}
	if _, hasSeq := byID["seq"]; hasSeq {
		t.Errorf("by-id update envelope must omit seq (omitempty); got %v", byID["seq"])
	}
	bySeq, _ := items[3]["payload"].(map[string]interface{})
	if bySeq["op"] != "update" {
		t.Errorf("by-seq update op=%v want update", bySeq["op"])
	}
	if bySeq["seq"] != float64(2) {
		t.Errorf("by-seq update seq=%v want 2", bySeq["seq"])
	}
	if _, hasID := bySeq["id"]; hasID {
		t.Errorf("by-seq update envelope must omit id (omitempty); got %v", bySeq["id"])
	}
}

// /counter_add auto-populate: envelope carries the increment `by`,
// never the post-add value. Tests both create-on-miss (auto-creates
// the cell) and increment-on-hit. Notify and Full are functionally
// identical for counter_add — counter cells have no opaque user
// payload to strip.
func TestEvents_AutoPopulate_CounterAdd(t *testing.T) {
	h, _ := newReservedScopesTestHandler(t, Config{
		ScopeMaxItems: 100,
		MaxStoreBytes: 100 << 20,
		MaxItemBytes:  1 << 20,
		Events:        EventsConfig{Mode: EventsModeFull},
	})

	// Create-on-miss: by=5 against a non-existent id.
	if code, _, raw := doRequest(t, h, "POST", "/counter_add",
		`{"scope":"counters","id":"hits","by":5}`); code != 200 {
		t.Fatalf("/counter_add create: code=%d body=%s", code, raw)
	}
	// Increment-on-hit: by=-2 (negative is allowed).
	if code, _, raw := doRequest(t, h, "POST", "/counter_add",
		`{"scope":"counters","id":"hits","by":-2}`); code != 200 {
		t.Fatalf("/counter_add increment: code=%d body=%s", code, raw)
	}

	count, items := eventsTailCount(t, h)
	if count != 2 {
		t.Fatalf("counter_add auto-populate: _events count=%d want 2", count)
	}
	wantBy := []float64{5, -2}
	for i, evt := range items {
		envelope, ok := evt["payload"].(map[string]interface{})
		if !ok {
			t.Fatalf("event %d: envelope not object: %v", i, evt["payload"])
		}
		if envelope["op"] != "counter_add" {
			t.Errorf("event %d: op=%v want counter_add", i, envelope["op"])
		}
		if envelope["scope"] != "counters" || envelope["id"] != "hits" {
			t.Errorf("event %d: addressing wrong, got scope=%v id=%v", i, envelope["scope"], envelope["id"])
		}
		by, ok := envelope["by"].(float64)
		if !ok || by != wantBy[i] {
			t.Errorf("event %d: by=%v want %v (action-input, not the post-add value)", i, envelope["by"], wantBy[i])
		}
		// Counter envelopes carry NO payload field (counter cells are
		// typed int64, not opaque JSON).
		if _, hasPayload := envelope["payload"]; hasPayload {
			t.Errorf("event %d: counter_add must not carry payload, got %v", i, envelope["payload"])
		}
		// And NO seq — counterAddOne doesn't pass it through.
		if _, hasSeq := envelope["seq"]; hasSeq {
			t.Errorf("event %d: counter_add envelope must omit seq, got %v", i, envelope["seq"])
		}
	}
}

// /delete auto-populate: three branches.
// (1) delete by-id (envelope carries id, no seq)
// (2) delete by-seq (envelope carries seq, no id)
// (3) delete miss (id absent in scope) — no emit
func TestEvents_AutoPopulate_Delete(t *testing.T) {
	h, _ := newReservedScopesTestHandler(t, Config{
		ScopeMaxItems: 100,
		MaxStoreBytes: 100 << 20,
		MaxItemBytes:  1 << 20,
		Events:        EventsConfig{Mode: EventsModeFull},
	})

	// Seed: two items so we can delete one by id, one by seq.
	if code, _, raw := doRequest(t, h, "POST", "/append",
		`{"scope":"posts","id":"a","payload":{"v":1}}`); code != 200 {
		t.Fatalf("seed by-id: code=%d body=%s", code, raw)
	}
	if code, _, raw := doRequest(t, h, "POST", "/append",
		`{"scope":"posts","payload":{"v":2}}`); code != 200 {
		t.Fatalf("seed by-seq: code=%d body=%s", code, raw)
	}
	baseline, _ := eventsTailCount(t, h)
	if baseline != 2 {
		t.Fatalf("after seed: _events count=%d want 2", baseline)
	}

	// (1) delete by-id.
	if code, _, raw := doRequest(t, h, "POST", "/delete",
		`{"scope":"posts","id":"a"}`); code != 200 {
		t.Fatalf("/delete by-id: code=%d body=%s", code, raw)
	}
	// (2) delete by-seq (the second seeded item is at seq=2).
	if code, _, raw := doRequest(t, h, "POST", "/delete",
		`{"scope":"posts","seq":2}`); code != 200 {
		t.Fatalf("/delete by-seq: code=%d body=%s", code, raw)
	}
	// (3) delete miss — must NOT emit.
	if code, _, _ := doRequest(t, h, "POST", "/delete",
		`{"scope":"posts","id":"nope"}`); code != 200 {
		t.Fatalf("/delete miss: code=%d want 200 (miss is not an error)", code)
	}

	count, items := eventsTailCount(t, h)
	if count != 4 { // 2 seeds + 2 deletes (miss skipped)
		t.Fatalf("after deletes: _events count=%d want 4 (2 seeds + 2 deletes, miss must not emit)", count)
	}

	byID, _ := items[2]["payload"].(map[string]interface{})
	if byID["op"] != "delete" || byID["id"] != "a" {
		t.Errorf("by-id delete envelope wrong: %v", byID)
	}
	if _, hasSeq := byID["seq"]; hasSeq {
		t.Errorf("by-id delete envelope must omit seq (omitempty); got %v", byID["seq"])
	}
	if _, hasPayload := byID["payload"]; hasPayload {
		t.Errorf("delete envelope must not carry payload; got %v", byID["payload"])
	}
	bySeq, _ := items[3]["payload"].(map[string]interface{})
	if bySeq["op"] != "delete" {
		t.Errorf("by-seq delete op=%v want delete", bySeq["op"])
	}
	if bySeq["seq"] != float64(2) {
		t.Errorf("by-seq delete seq=%v want 2", bySeq["seq"])
	}
	if _, hasID := bySeq["id"]; hasID {
		t.Errorf("by-seq delete envelope must omit id (omitempty); got %v", bySeq["id"])
	}
}

// High-volume verification: 1000 /append + 1000 /delete, all 2000
// events round-trip correctly into `_events` in commit-order, with
// the right op + id per envelope. Catches regressions where:
//   - emit drops silently under load (eventsDropsTotal would tick up;
//     we assert it stays 0 on this configured-roomy budget)
//   - envelope ordering desyncs (e.g. emits land in a wrong order due
//     to a future async-emit refactor that broke commit-order)
//   - per-op envelope shape regresses for either /append or /delete
//     under volume (per-op tests above use small N; this catches
//     "first N work but Nth+1 drifts" cliff regressions).
//
// Pre-seeded user-scope cap is generous (10k items) and store byte
// cap is 256 MiB — well above 1000 items × ~50 bytes each plus 2000
// events × ~150 bytes each. eventsDropsTotal must stay 0; any non-
// zero value here is a regression.
func TestEvents_AutoPopulate_HighVolume_AppendThenDelete(t *testing.T) {
	cfg := Config{
		ScopeMaxItems: 10_000,
		MaxStoreBytes: 256 << 20,
		MaxItemBytes:  1 << 20,
		Events:        EventsConfig{Mode: EventsModeFull},
	}
	api := NewAPI(NewGateway(cfg), APIConfig{})
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	const N = 1000

	for i := 0; i < N; i++ {
		body := fmt.Sprintf(`{"scope":"posts","id":"p-%d","payload":{"v":%d}}`, i, i)
		if code, _, raw := doRequest(t, mux, "POST", "/append", body); code != 200 {
			t.Fatalf("/append #%d: code=%d body=%s", i, code, raw)
		}
	}
	for i := 0; i < N; i++ {
		body := fmt.Sprintf(`{"scope":"posts","id":"p-%d"}`, i)
		if code, _, raw := doRequest(t, mux, "POST", "/delete", body); code != 200 {
			t.Fatalf("/delete #%d: code=%d body=%s", i, code, raw)
		}
	}

	if drops := api.store.eventsDropsTotal.Load(); drops != 0 {
		t.Errorf("eventsDropsTotal=%d want 0 (cap is generous, no drops expected)", drops)
	}

	// Pull the full _events tail. limit is 2*N+50 slack; at 256 MiB
	// cap the response cap is well above the resulting body size.
	code, out, raw := doRequest(t, mux, "GET",
		fmt.Sprintf("/tail?scope=_events&limit=%d", 2*N+50), "")
	if code != 200 {
		t.Fatalf("/tail _events: code=%d body=%s", code, raw)
	}
	count := int(mustFloat(t, out, "count"))
	if count != 2*N {
		t.Fatalf("after %d appends + %d deletes: _events count=%d want %d", N, N, count, 2*N)
	}
	rawItems, _ := out["items"].([]interface{})
	if len(rawItems) != 2*N {
		t.Fatalf("items len=%d want %d", len(rawItems), 2*N)
	}

	// First N events are the appends (commit-order: append 0..N-1).
	for i := 0; i < N; i++ {
		envelope, _ := rawItems[i].(map[string]interface{})
		evt, ok := envelope["payload"].(map[string]interface{})
		if !ok {
			t.Fatalf("append event %d: envelope payload not object: %v", i, envelope["payload"])
		}
		if evt["op"] != "append" {
			t.Fatalf("event %d: op=%v want append at this position", i, evt["op"])
		}
		wantID := fmt.Sprintf("p-%d", i)
		if evt["id"] != wantID {
			t.Fatalf("append event %d: id=%v want %s", i, evt["id"], wantID)
		}
	}

	// Next N events are the deletes, in delete-order (which matches
	// append-order in this test: we delete p-0 through p-N-1).
	for i := 0; i < N; i++ {
		envelope, _ := rawItems[N+i].(map[string]interface{})
		evt, ok := envelope["payload"].(map[string]interface{})
		if !ok {
			t.Fatalf("delete event %d: envelope payload not object: %v", N+i, envelope["payload"])
		}
		if evt["op"] != "delete" {
			t.Fatalf("event %d: op=%v want delete at this position", N+i, evt["op"])
		}
		wantID := fmt.Sprintf("p-%d", i)
		if evt["id"] != wantID {
			t.Fatalf("delete event %d: id=%v want %s", N+i, evt["id"], wantID)
		}
		// Delete events must NOT carry payload (and we asserted the
		// shape in the per-op tests above, but verify it survives
		// volume too).
		if _, hasPayload := evt["payload"]; hasPayload {
			t.Fatalf("delete event %d carries payload (violates op shape): %v", N+i, evt["payload"])
		}
	}
}

// /warm auto-populate: envelope is exactly {op:"warm", ts:N} — no
// scope list, no item count. Drainers needing the warmed-scope
// list /scopelist after waking up; the event is just a "large-
// scale change happened, reconcile" pulse. Tests both the bare-
// envelope shape AND that the action committed (the warmed scope's
// items are reachable post-emit).
func TestEvents_AutoPopulate_Warm(t *testing.T) {
	h, _ := newReservedScopesTestHandler(t, Config{
		ScopeMaxItems: 100,
		MaxStoreBytes: 100 << 20,
		MaxItemBytes:  1 << 20,
		Events:        EventsConfig{Mode: EventsModeFull},
	})

	body := `{"items":[` +
		`{"scope":"posts","id":"a","payload":{"v":1}},` +
		`{"scope":"posts","id":"b","payload":{"v":2}}` +
		`]}`
	if code, _, raw := doRequest(t, h, "POST", "/warm", body); code != 200 {
		t.Fatalf("/warm: code=%d body=%s", code, raw)
	}

	count, items := eventsTailCount(t, h)
	if count != 1 {
		t.Fatalf("warm auto-populate: _events count=%d want 1", count)
	}
	envelope, ok := items[0]["payload"].(map[string]interface{})
	if !ok {
		t.Fatalf("envelope payload not an object: %v", items[0]["payload"])
	}
	if envelope["op"] != "warm" {
		t.Errorf("op=%v want warm", envelope["op"])
	}
	if _, hasScope := envelope["scope"]; hasScope {
		t.Errorf("warm envelope must not carry scope (omitempty); got %v", envelope["scope"])
	}
	if _, hasID := envelope["id"]; hasID {
		t.Errorf("warm envelope must not carry id; got %v", envelope["id"])
	}
	if _, hasSeq := envelope["seq"]; hasSeq {
		t.Errorf("warm envelope must not carry seq; got %v", envelope["seq"])
	}
	if _, hasPayload := envelope["payload"]; hasPayload {
		t.Errorf("warm envelope must not carry payload; got %v", envelope["payload"])
	}
	if ts, ok := envelope["ts"].(float64); !ok || ts <= 0 {
		t.Errorf("warm envelope must carry positive ts; got %v", envelope["ts"])
	}

	// Sanity: the warmed items are actually present.
	code, out, _ := doRequest(t, h, "GET", "/get?scope=posts&id=a", "")
	if code != 200 || !mustBool(t, out, "hit") {
		t.Errorf("/warm did not commit: posts/a not reachable")
	}
}

// /delete_scope auto-populate: envelope is {op:"delete_scope", scope:X,
// ts:N}. Tests the success path AND that emit is gated on the actual
// deletion (calling /delete_scope on a non-existent scope must NOT emit).
func TestEvents_AutoPopulate_DeleteScope(t *testing.T) {
	h, _ := newReservedScopesTestHandler(t, Config{
		ScopeMaxItems: 100,
		MaxStoreBytes: 100 << 20,
		MaxItemBytes:  1 << 20,
		Events:        EventsConfig{Mode: EventsModeFull},
	})

	// Seed one scope so we can delete it (and one prior /append event).
	if code, _, raw := doRequest(t, h, "POST", "/append",
		`{"scope":"posts","id":"a","payload":{"v":1}}`); code != 200 {
		t.Fatalf("seed /append: code=%d body=%s", code, raw)
	}
	baseline, _ := eventsTailCount(t, h)
	if baseline != 1 {
		t.Fatalf("after seed: _events count=%d want 1", baseline)
	}

	// Hit: delete the scope we just created.
	if code, _, raw := doRequest(t, h, "POST", "/delete_scope",
		`{"scope":"posts"}`); code != 200 {
		t.Fatalf("/delete_scope hit: code=%d body=%s", code, raw)
	}

	// No-op: deleting a scope that doesn't exist must NOT emit.
	if code, _, _ := doRequest(t, h, "POST", "/delete_scope",
		`{"scope":"nope"}`); code != 200 {
		t.Fatalf("/delete_scope miss: code=%d want 200", code)
	}

	count, items := eventsTailCount(t, h)
	if count != 2 { // 1 seed-append + 1 delete_scope (miss skipped)
		t.Fatalf("after delete_scope: _events count=%d want 2 (1 seed + 1 hit, miss must not emit)", count)
	}

	envelope, _ := items[1]["payload"].(map[string]interface{})
	if envelope["op"] != "delete_scope" {
		t.Errorf("op=%v want delete_scope", envelope["op"])
	}
	if envelope["scope"] != "posts" {
		t.Errorf("scope=%v want posts", envelope["scope"])
	}
	if ts, ok := envelope["ts"].(float64); !ok || ts <= 0 {
		t.Errorf("delete_scope envelope must carry positive ts; got %v", envelope["ts"])
	}
	if _, hasID := envelope["id"]; hasID {
		t.Errorf("delete_scope envelope must not carry id; got %v", envelope["id"])
	}
	if _, hasPayload := envelope["payload"]; hasPayload {
		t.Errorf("delete_scope envelope must not carry payload; got %v", envelope["payload"])
	}

	// Post-state: the deleted scope's items must actually be gone.
	// The closure refactor of deleteScope (so emit fires after unlock)
	// is verified to still commit the delete itself.
	if code, out, _ := doRequest(t, h, "GET", "/get?scope=posts&id=a", ""); code != 200 || mustBool(t, out, "hit") {
		t.Errorf("/delete_scope under events_mode=full did not actually delete: posts/a still reachable, hit=%v", out["hit"])
	}
}

// /wipe explicitly does NOT emit into _events (the wipe wipes _events
// itself, so any event written would either land in the about-to-be-
// wiped buffer or paradoxically as seq=1 in the freshly-recreated
// buffer). Drainers detect wipe via _events.lastSeq going backwards.
//
// Test verifies: events_mode=full, several /append events accumulate,
// /wipe runs successfully, _events is empty post-wipe (no wipe-event
// snuck in). Also verifies eventsDropsTotal stays 0 (the wipe didn't
// trigger an emit-attempt that got dropped).
func TestEvents_AutoPopulate_WipeNoEmit(t *testing.T) {
	cfg := Config{
		ScopeMaxItems: 100,
		MaxStoreBytes: 100 << 20,
		MaxItemBytes:  1 << 20,
		Events:        EventsConfig{Mode: EventsModeFull},
	}
	api := NewAPI(NewGateway(cfg), APIConfig{})
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	for i := 0; i < 3; i++ {
		body := fmt.Sprintf(`{"scope":"posts","id":"p-%d","payload":{"v":%d}}`, i, i)
		if code, _, raw := doRequest(t, mux, "POST", "/append", body); code != 200 {
			t.Fatalf("seed /append #%d: code=%d body=%s", i, code, raw)
		}
	}
	pre, _ := eventsTailCount(t, mux)
	if pre != 3 {
		t.Fatalf("pre-wipe: _events count=%d want 3", pre)
	}

	if code, _, raw := doRequest(t, mux, "POST", "/wipe", ""); code != 200 {
		t.Fatalf("/wipe: code=%d body=%s", code, raw)
	}

	post, _ := eventsTailCount(t, mux)
	if post != 0 {
		t.Errorf("post-wipe: _events count=%d want 0 (wipe must NOT emit)", post)
	}
	if drops := api.store.eventsDropsTotal.Load(); drops != 0 {
		t.Errorf("post-wipe: eventsDropsTotal=%d want 0 (no emit attempt should have happened)", drops)
	}

	// Post-state: every seeded user item must actually be gone — the
	// no-emit decision should not weaken /wipe's destructive contract.
	for i := 0; i < 3; i++ {
		path := fmt.Sprintf("/get?scope=posts&id=p-%d", i)
		if code, out, _ := doRequest(t, mux, "GET", path, ""); code != 200 || mustBool(t, out, "hit") {
			t.Errorf("/wipe under events_mode=full did not actually wipe posts/p-%d, hit=%v", i, out["hit"])
		}
	}
	// And /stats should agree: only the 2 reserved scopes remain.
	code, out, _ := doRequest(t, mux, "GET", "/stats", "")
	if code != 200 {
		t.Fatalf("/stats: code=%d", code)
	}
	if scopeCount := mustFloat(t, out, "scope_count"); scopeCount != 2 {
		t.Errorf("post-wipe scope_count=%v want 2 (only _events + _inbox)", scopeCount)
	}
	if totalItems := mustFloat(t, out, "total_items"); totalItems != 0 {
		t.Errorf("post-wipe total_items=%v want 0", totalItems)
	}
}

// /rebuild explicitly does NOT emit into _events — same rationale as
// /wipe (rebuild drops + recreates _events). Test verifies the negative.
func TestEvents_AutoPopulate_RebuildNoEmit(t *testing.T) {
	cfg := Config{
		ScopeMaxItems: 100,
		MaxStoreBytes: 100 << 20,
		MaxItemBytes:  1 << 20,
		Events:        EventsConfig{Mode: EventsModeFull},
	}
	api := NewAPI(NewGateway(cfg), APIConfig{})
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	// Seed 2 events from /append.
	for i := 0; i < 2; i++ {
		body := fmt.Sprintf(`{"scope":"old","id":"o-%d","payload":{"v":%d}}`, i, i)
		if code, _, raw := doRequest(t, mux, "POST", "/append", body); code != 200 {
			t.Fatalf("seed /append #%d: code=%d body=%s", i, code, raw)
		}
	}

	rebuildBody := `{"items":[{"scope":"new","id":"n-0","payload":{"v":42}}]}`
	if code, _, raw := doRequest(t, mux, "POST", "/rebuild", rebuildBody); code != 200 {
		t.Fatalf("/rebuild: code=%d body=%s", code, raw)
	}

	post, _ := eventsTailCount(t, mux)
	if post != 0 {
		t.Errorf("post-rebuild: _events count=%d want 0 (rebuild must NOT emit)", post)
	}
	if drops := api.store.eventsDropsTotal.Load(); drops != 0 {
		t.Errorf("post-rebuild: eventsDropsTotal=%d want 0", drops)
	}

	// The rebuilt scope's data IS reachable — confirms rebuild itself
	// committed; only the auto-populate skip is being verified.
	if code, out, _ := doRequest(t, mux, "GET", "/get?scope=new&id=n-0", ""); code != 200 || !mustBool(t, out, "hit") {
		t.Errorf("/rebuild did not commit: new/n-0 not reachable")
	}
	// And the OLD scope is fully gone — /rebuild is replace, not merge.
	for i := 0; i < 2; i++ {
		path := fmt.Sprintf("/get?scope=old&id=o-%d", i)
		if code, out, _ := doRequest(t, mux, "GET", path, ""); code != 200 || mustBool(t, out, "hit") {
			t.Errorf("/rebuild under events_mode=full left old/o-%d behind, hit=%v", i, out["hit"])
		}
	}
}

// /delete_up_to auto-populate: envelope carries scope + max_seq.
// Hit emits one event; a no-op cursor (max_seq below any stored item)
// does NOT emit.
func TestEvents_AutoPopulate_DeleteUpTo(t *testing.T) {
	h, _ := newReservedScopesTestHandler(t, Config{
		ScopeMaxItems: 100,
		MaxStoreBytes: 100 << 20,
		MaxItemBytes:  1 << 20,
		Events:        EventsConfig{Mode: EventsModeFull},
	})

	// Seed 5 items in "posts" — they get seq 1..5.
	for i := 0; i < 5; i++ {
		body := fmt.Sprintf(`{"scope":"posts","payload":{"v":%d}}`, i)
		if code, _, raw := doRequest(t, h, "POST", "/append", body); code != 200 {
			t.Fatalf("seed #%d: code=%d body=%s", i, code, raw)
		}
	}
	baseline, _ := eventsTailCount(t, h)
	if baseline != 5 {
		t.Fatalf("after seed: _events count=%d want 5", baseline)
	}

	// Hit: delete_up_to=3 removes seq 1..3 → 3 items deleted, 1 emit.
	if code, _, raw := doRequest(t, h, "POST", "/delete_up_to",
		`{"scope":"posts","max_seq":3}`); code != 200 {
		t.Fatalf("/delete_up_to hit: code=%d body=%s", code, raw)
	}

	// No-op: delete_up_to=2 again — items <= 2 are already gone, so 0
	// deleted; must NOT emit.
	if code, _, _ := doRequest(t, h, "POST", "/delete_up_to",
		`{"scope":"posts","max_seq":2}`); code != 200 {
		t.Fatalf("/delete_up_to no-op: code=%d want 200", code)
	}

	count, items := eventsTailCount(t, h)
	if count != 6 { // 5 seeds + 1 effective delete_up_to (no-op skipped)
		t.Fatalf("after delete_up_to: _events count=%d want 6 (5 seeds + 1 hit, no-op skipped)", count)
	}

	envelope, _ := items[5]["payload"].(map[string]interface{})
	if envelope["op"] != "delete_up_to" {
		t.Errorf("op=%v want delete_up_to", envelope["op"])
	}
	if envelope["scope"] != "posts" {
		t.Errorf("scope=%v want posts", envelope["scope"])
	}
	if envelope["max_seq"] != float64(3) {
		t.Errorf("max_seq=%v want 3 (the cursor, not the count)", envelope["max_seq"])
	}
	if _, hasID := envelope["id"]; hasID {
		t.Errorf("delete_up_to envelope must omit id, got %v", envelope["id"])
	}
	if _, hasSeq := envelope["seq"]; hasSeq {
		t.Errorf("delete_up_to envelope must omit seq (per-item seq is not in scope), got %v", envelope["seq"])
	}
	if _, hasPayload := envelope["payload"]; hasPayload {
		t.Errorf("delete_up_to envelope must not carry payload, got %v", envelope["payload"])
	}
}
