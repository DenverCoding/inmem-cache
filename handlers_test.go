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
		NewStore(Config{ScopeMaxItems: maxItems, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20}),
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

func TestWarm_DuplicateIDInSameScope(t *testing.T) {
	h, _ := newTestHandler(10)
	body := `{"items":[
		{"scope":"s","id":"a","payload":{"v":1}},
		{"scope":"s","id":"a","payload":{"v":2}}
	]}`
	code, _, _ := doAdminRequest(t, h, "/warm", body)
	if code != 409 {
		t.Fatalf("code=%d want 409", code)
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
	// Regression guard: /stats is intentionally aggregate-only since the
	// 100k-scope DoS observation. Per-scope enumeration belongs to the
	// (future) /scopelist endpoint.
	if _, present := out["scopes"]; present {
		t.Errorf("scopes key must NOT appear on /stats response (moved to /scopelist): %v", out["scopes"])
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
	api := NewAPI(NewStore(cfg), APIConfig{})
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
