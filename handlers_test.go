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
	api := NewAPI(NewStore(Config{ScopeMaxItems: maxItems, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20, MaxResponseBytes: 25 << 20, MaxMultiCallBytes: 16 << 20, MaxMultiCallCount: 10}))
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
	if _, hasTS := item["ts"]; hasTS {
		t.Errorf("response item should not carry a 'ts' field: %v", item)
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
		"two objects":     `{"scope":"x","payload":{"v":1}}{"scope":"y","payload":{"v":2}}`,
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
	code, out, _ := doRequest(t, h, "POST", "/warm", body)
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
	code, _, _ := doRequest(t, h, "POST", "/warm", body)
	if code != 409 {
		t.Fatalf("code=%d want 409", code)
	}
}

func TestWarm_MissingScopeOnItem(t *testing.T) {
	h, _ := newTestHandler(10)
	body := `{"items":[{"id":"a","payload":{"v":1}}]}`
	code, _, _ := doRequest(t, h, "POST", "/warm", body)
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
	code, out, _ := doRequest(t, h, "POST", "/rebuild", body)
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

	code, _, _ := doRequest(t, h, "POST", "/rebuild", `{"items":[]}`)
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

	code, out, _ := doRequest(t, h, "POST", "/delete_scope", `{"scope":"s"}`)
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
	code, out, _ := doRequest(t, h, "POST", "/delete_scope", `{"scope":"nope"}`)
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

	code, out, _ := doRequest(t, h, "POST", "/wipe", "")
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if !mustBool(t, out, "ok") {
		t.Error("ok=false")
	}
	if mustFloat(t, out, "deleted_scopes") != 0 {
		t.Errorf("deleted_scopes=%v want 0", out["deleted_scopes"])
	}
	if mustFloat(t, out, "deleted_items") != 0 {
		t.Errorf("deleted_items=%v want 0", out["deleted_items"])
	}
}

func TestWipe_ClearsEveryScope(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"a","id":"1","payload":{"v":1}}`)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"a","id":"2","payload":{"v":2}}`)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"b","id":"1","payload":{"v":1}}`)

	code, out, _ := doRequest(t, h, "POST", "/wipe", "")
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if mustFloat(t, out, "deleted_scopes") != 2 {
		t.Errorf("deleted_scopes=%v want 2", out["deleted_scopes"])
	}
	if mustFloat(t, out, "deleted_items") != 3 {
		t.Errorf("deleted_items=%v want 3", out["deleted_items"])
	}
	if mustFloat(t, out, "freed_mb") <= 0 {
		t.Errorf("freed_mb=%v want >0", out["freed_mb"])
	}

	// Both scopes must now be gone.
	for _, scope := range []string{"a", "b"} {
		_, out, _ := doRequest(t, h, "GET", "/stats", "")
		scopes, ok := out["scopes"].(map[string]interface{})
		if !ok {
			t.Fatalf("stats has no scopes map: %+v", out)
		}
		if _, present := scopes[scope]; present {
			t.Errorf("scope %q still present after /wipe", scope)
		}
	}
}

// After /wipe the store-wide byte counter and scope count surfaced via
// /stats must both be zero — clients use those numbers to confirm the wipe.
func TestWipe_StatsReportEmptyAfterwards(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","id":"x","payload":{"v":1}}`)

	_, _, _ = doRequest(t, h, "POST", "/wipe", "")

	_, out, _ := doRequest(t, h, "GET", "/stats", "")
	if mustFloat(t, out, "scope_count") != 0 {
		t.Errorf("scope_count=%v want 0", out["scope_count"])
	}
	if mustFloat(t, out, "total_items") != 0 {
		t.Errorf("total_items=%v want 0", out["total_items"])
	}
	if mustFloat(t, out, "approx_store_mb") != 0 {
		t.Errorf("approx_store_mb=%v want 0", out["approx_store_mb"])
	}
}

func TestWipe_GETNotAllowed(t *testing.T) {
	h, _ := newTestHandler(10)

	code, _, _ := doRequest(t, h, "GET", "/wipe", "")
	if code != 405 {
		t.Fatalf("code=%d want 405", code)
	}
}

// /wipe accepts a POST with no body. A non-empty body is simply ignored;
// it has no effect on the operation.
func TestWipe_IgnoresBody(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","id":"x","payload":{"v":1}}`)

	code, out, _ := doRequest(t, h, "POST", "/wipe", `{"anything":"goes"}`)
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

	_, _, _ = doRequest(t, h, "POST", "/wipe", "")

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

// /render hits feed scope read-heat the same way /get hits do, so
// /delete_scope_candidates reflects render-driven traffic. Misses must not
// count (same rule as /get) — otherwise a hot 404 would skew eviction.
func TestRender_HitBumpsReadHeat_MissDoesNot(t *testing.T) {
	h, api := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","id":"a","payload":{"v":1}}`)

	_ = doRawRequest(t, h, "GET", "/render?scope=s&id=a")
	_ = doRawRequest(t, h, "GET", "/render?scope=s&id=a")
	_ = doRawRequest(t, h, "GET", "/render?scope=s&id=nonexistent") // miss — must not count

	buf, _ := api.store.getScope("s")
	buf.mu.RLock()
	got := buf.last7DReadCount
	buf.mu.RUnlock()
	if got != 2 {
		t.Errorf("last_7d_read_count=%d want 2 (two hits, miss must not count)", got)
	}
}

// --- /stats -------------------------------------------------------------------

func TestStats_Structure(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","payload":{"v":1}}`)

	_, out, _ := doRequest(t, h, "GET", "/stats", "")
	if mustFloat(t, out, "scope_count") != 1 {
		t.Errorf("scope_count=%v want 1", out["scope_count"])
	}
	if mustFloat(t, out, "total_items") != 1 {
		t.Errorf("total_items=%v want 1", out["total_items"])
	}
	if _, ok := out["scopes"].(map[string]interface{}); !ok {
		t.Error("scopes not a map")
	}
}

// --- /delete_scope_candidates ------------------------------------------------

func TestDeleteScopeCandidates_Basic(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"a","payload":{"v":1}}`)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"b","payload":{"v":1}}`)

	_, out, _ := doRequest(t, h, "GET", "/delete_scope_candidates", "")
	if mustFloat(t, out, "count") != 2 {
		t.Errorf("count=%v want 2", out["count"])
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
	if code, _, body := doRequest(t, h, "POST", "/rebuild", rebuildItems.String()); code != 200 {
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
	if code, _, body := doRequest(t, h, "POST", "/warm", warmItems.String()); code != 200 {
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

	// 11. append with a 129-byte scope name (MaxScopeBytes is 128). Must 400
	//     and must NOT register a new scope — verified via scope_count below.
	tooLong := strings.Repeat("a", MaxScopeBytes+1)
	if code, _, _ := doRequest(t, h, "POST", "/append",
		fmt.Sprintf(`{"scope":"%s","payload":{"v":1}}`, tooLong)); code != 400 {
		t.Fatalf("too-long scope: code=%d want 400", code)
	}

	// --- Assertions on /stats ---
	_, stats, _ := doRequest(t, h, "GET", "/stats", "")

	if got := mustFloat(t, stats, "scope_count"); got != 2 {
		t.Errorf("scope_count=%v want 2 (rejected too-long-scope must not register)", got)
	}
	// x: 100 rebuilt − 1 deleted (item_050) − 30 (delete_up_to) = 69
	// y: 50 warmed + 100 appended − 1 deleted (seq 75)         = 149
	if got := mustFloat(t, stats, "total_items"); got != 218 {
		t.Errorf("total_items=%v want 218", got)
	}

	scopes, ok := stats["scopes"].(map[string]interface{})
	if !ok {
		t.Fatalf("stats.scopes not a map: %v", stats["scopes"])
	}

	xStats, ok := scopes["x"].(map[string]interface{})
	if !ok {
		t.Fatalf("stats.scopes.x missing: %v", scopes)
	}
	if got := mustFloat(t, xStats, "item_count"); got != 69 {
		t.Errorf("x.item_count=%v want 69", got)
	}
	if got := mustFloat(t, xStats, "last_seq"); got != 100 {
		t.Errorf("x.last_seq=%v want 100 (delete_up_to must not rewind lastSeq)", got)
	}
	if got := mustFloat(t, xStats, "last_7d_read_count"); got < 1 {
		t.Errorf("x.last_7d_read_count=%v want >= 1 after /head", got)
	}
	// /head on x was the last touch, so its last_access_ts must sit inside
	// the window we bracketed around that call.
	if got := int64(mustFloat(t, xStats, "last_access_ts")); got < preHeadX || got > postHeadX {
		t.Errorf("x.last_access_ts=%d not in bracket [%d, %d] around /head", got, preHeadX, postHeadX)
	}

	yStats, ok := scopes["y"].(map[string]interface{})
	if !ok {
		t.Fatalf("stats.scopes.y missing: %v", scopes)
	}
	if got := mustFloat(t, yStats, "item_count"); got != 149 {
		t.Errorf("y.item_count=%v want 149", got)
	}
	if got := mustFloat(t, yStats, "last_seq"); got != 150 {
		t.Errorf("y.last_seq=%v want 150", got)
	}
	// /tail + /get byID both hit y — at least 2 reads, same calendar day → single bucket.
	if got := mustFloat(t, yStats, "last_7d_read_count"); got < 2 {
		t.Errorf("y.last_7d_read_count=%v want >= 2 after /tail + /get", got)
	}
	// /get byID was the last read on y, so last_access_ts must sit inside
	// that call's bracket.
	if got := int64(mustFloat(t, yStats, "last_access_ts")); got < preGetY || got > postGetY {
		t.Errorf("y.last_access_ts=%d not in bracket [%d, %d] around /get", got, preGetY, postGetY)
	}

	// --- Internal accounting invariants ---
	// 1. totalBytes matches what we'd compute from current items — proves the
	//    incremental counter never drifted across the whole workload.
	var ground int64
	for _, buf := range api.store.listScopes() {
		buf.mu.RLock()
		for i := range buf.items {
			ground += approxItemSize(buf.items[i])
		}
		buf.mu.RUnlock()
	}
	if got := api.store.totalBytes.Load(); got != ground {
		t.Errorf("totalBytes=%d but recomputed from items=%d (counter drift)", got, ground)
	}

	// 2. Sum of per-scope b.bytes == store totalBytes. Catches the ghost-bytes
	//    class of bug where store and scope counters silently diverge.
	var sumBufBytes int64
	for _, buf := range api.store.listScopes() {
		buf.mu.RLock()
		sumBufBytes += buf.bytes
		buf.mu.RUnlock()
	}
	if got := api.store.totalBytes.Load(); got != sumBufBytes {
		t.Errorf("totalBytes=%d but sum(buf.bytes)=%d", got, sumBufBytes)
	}
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
					// Delete-up-to with a small max_seq. Targets the oldest
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

	_, stats, _ := doRequest(t, h, "GET", "/stats", "")
	if got := int64(mustFloat(t, stats, "total_items")); got != expectedItems {
		t.Errorf("/stats total_items=%d want %d (appends=%d deletes=%d)",
			got, expectedItems, appendsOK, deletedN)
	}

	// Walk the store directly and re-verify every invariant.
	var sumBufBytes, recomputedBytes int64
	var totalItemsWalked int64
	var totalReadCount uint64
	for scopeName, buf := range api.store.listScopes() {
		buf.mu.RLock()
		sumBufBytes += buf.bytes
		totalItemsWalked += int64(len(buf.items))
		totalReadCount += buf.readCountTotal

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
	if got := api.store.totalBytes.Load(); got != sumBufBytes {
		t.Errorf("totalBytes=%d != Σ buf.bytes=%d (ghost bytes)", got, sumBufBytes)
	}
	if got := api.store.totalBytes.Load(); got != recomputedBytes {
		t.Errorf("totalBytes=%d != recomputed-from-items=%d (counter drift)", got, recomputedBytes)
	}
	// /head and /tail are the only read paths the workers use, and both call
	// recordRead exactly once on a hit. So Σ readCountTotal must equal the
	// sum of the workers' readsHit tallies — no approximation.
	if int64(totalReadCount) != readsHit {
		t.Errorf("Σ readCountTotal=%d != tallied readsHit=%d", totalReadCount, readsHit)
	}
}
