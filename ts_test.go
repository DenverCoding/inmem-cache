package scopecache

import (
	"encoding/json"
	"fmt"
	"testing"
)

// --- Item round-trip ----------------------------------------------------------

func TestItem_TsRoundtripsViaAppendAndGet(t *testing.T) {
	h, _ := newTestHandler(10)

	_, out, _ := doRequest(t, h, "POST", "/append",
		`{"scope":"s","id":"a","ts":1700000000000,"payload":{"v":1}}`)
	item := out["item"].(map[string]interface{})
	if ts, ok := item["ts"].(float64); !ok || int64(ts) != 1700000000000 {
		t.Fatalf("ts not round-tripped through /append: %v", item["ts"])
	}

	_, out, _ = doRequest(t, h, "GET", "/get?scope=s&id=a", "")
	got := out["item"].(map[string]interface{})
	if ts, ok := got["ts"].(float64); !ok || int64(ts) != 1700000000000 {
		t.Fatalf("ts not round-tripped through /get: %v", got["ts"])
	}
}

func TestItem_NoTsWhenOmitted(t *testing.T) {
	h, _ := newTestHandler(10)

	_, out, _ := doRequest(t, h, "POST", "/append",
		`{"scope":"s","id":"a","payload":{"v":1}}`)
	item := out["item"].(map[string]interface{})
	if _, has := item["ts"]; has {
		t.Fatalf("ts should be absent when omitted, got: %+v", item)
	}
}

func TestAppend_RejectsNonIntegerTs(t *testing.T) {
	h, _ := newTestHandler(10)
	// A fractional or string ts fails JSON unmarshal into *int64, which the
	// decoder surfaces as "the request body must contain valid JSON" (400).
	code, _, _ := doRequest(t, h, "POST", "/append",
		`{"scope":"s","id":"a","ts":"notanumber","payload":{"v":1}}`)
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
}

// --- /update: omit preserves, present overwrites ------------------------------

func TestUpdate_OmittedTsPreserves(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append",
		`{"scope":"s","id":"a","ts":1000,"payload":{"v":1}}`)

	// No ts in update body — stored ts must remain 1000.
	_, _, _ = doRequest(t, h, "POST", "/update",
		`{"scope":"s","id":"a","payload":{"v":2}}`)

	_, out, _ := doRequest(t, h, "GET", "/get?scope=s&id=a", "")
	got := out["item"].(map[string]interface{})
	if ts, _ := got["ts"].(float64); int64(ts) != 1000 {
		t.Fatalf("ts=%v want 1000 (preserved on omit)", got["ts"])
	}
}

func TestUpdate_PresentTsOverwrites(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append",
		`{"scope":"s","id":"a","ts":1000,"payload":{"v":1}}`)

	_, _, _ = doRequest(t, h, "POST", "/update",
		`{"scope":"s","id":"a","ts":5000,"payload":{"v":2}}`)

	_, out, _ := doRequest(t, h, "GET", "/get?scope=s&id=a", "")
	got := out["item"].(map[string]interface{})
	if ts, _ := got["ts"].(float64); int64(ts) != 5000 {
		t.Fatalf("ts=%v want 5000 (overwritten)", got["ts"])
	}
}

// --- /upsert: replace semantics — omit clears, present sets -------------------

func TestUpsert_ReplaceFollowsClientTs(t *testing.T) {
	h, _ := newTestHandler(10)

	// First upsert with ts.
	_, _, _ = doRequest(t, h, "POST", "/upsert",
		`{"scope":"s","id":"a","ts":1000,"payload":{"v":1}}`)

	// Second upsert without ts — replace semantics: ts must be cleared.
	_, _, _ = doRequest(t, h, "POST", "/upsert",
		`{"scope":"s","id":"a","payload":{"v":2}}`)

	_, out, _ := doRequest(t, h, "GET", "/get?scope=s&id=a", "")
	got := out["item"].(map[string]interface{})
	if _, has := got["ts"]; has {
		t.Fatalf("ts should be cleared after upsert without ts, got: %v", got["ts"])
	}
}

// --- /ts_range: validation ---------------------------------------------------

func TestTsRange_MissingBoundsRejected(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "GET", "/ts_range?scope=s", "")
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
}

func TestTsRange_InvertedWindowRejected(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "GET",
		"/ts_range?scope=s&since_ts=2000&until_ts=1000", "")
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
}

func TestTsRange_MalformedTsRejected(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "GET",
		"/ts_range?scope=s&since_ts=abc", "")
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
}

// --- /ts_range: filtering -----------------------------------------------------

func TestTsRange_FiltersByWindowInclusive(t *testing.T) {
	h, _ := newTestHandler(100)
	// Seed items at ts 1000, 2000, 3000, 4000, 5000.
	for i := 1; i <= 5; i++ {
		body := fmt.Sprintf(`{"scope":"s","ts":%d,"payload":{"v":%d}}`, i*1000, i)
		_, _, _ = doRequest(t, h, "POST", "/append", body)
	}

	// [2000, 4000] inclusive → ts 2000, 3000, 4000.
	_, out, _ := doRequest(t, h, "GET",
		"/ts_range?scope=s&since_ts=2000&until_ts=4000", "")
	items := out["items"].([]interface{})
	if len(items) != 3 {
		t.Fatalf("count=%d want 3", len(items))
	}
	gotTs := []int64{}
	for _, raw := range items {
		it := raw.(map[string]interface{})
		gotTs = append(gotTs, int64(it["ts"].(float64)))
	}
	want := []int64{2000, 3000, 4000}
	for i, v := range want {
		if gotTs[i] != v {
			t.Errorf("items[%d].ts=%d want %d", i, gotTs[i], v)
		}
	}
}

func TestTsRange_SinceOnly(t *testing.T) {
	h, _ := newTestHandler(100)
	for i := 1; i <= 5; i++ {
		body := fmt.Sprintf(`{"scope":"s","ts":%d,"payload":{"v":%d}}`, i*1000, i)
		_, _, _ = doRequest(t, h, "POST", "/append", body)
	}
	_, out, _ := doRequest(t, h, "GET", "/ts_range?scope=s&since_ts=3000", "")
	items := out["items"].([]interface{})
	if len(items) != 3 {
		t.Fatalf("count=%d want 3 (ts 3000,4000,5000)", len(items))
	}
}

func TestTsRange_UntilOnly(t *testing.T) {
	h, _ := newTestHandler(100)
	for i := 1; i <= 5; i++ {
		body := fmt.Sprintf(`{"scope":"s","ts":%d,"payload":{"v":%d}}`, i*1000, i)
		_, _, _ = doRequest(t, h, "POST", "/append", body)
	}
	_, out, _ := doRequest(t, h, "GET", "/ts_range?scope=s&until_ts=3000", "")
	items := out["items"].([]interface{})
	if len(items) != 3 {
		t.Fatalf("count=%d want 3 (ts 1000,2000,3000)", len(items))
	}
}

func TestTsRange_ExcludesItemsWithoutTs(t *testing.T) {
	h, _ := newTestHandler(100)
	// Two items with ts, two without.
	_, _, _ = doRequest(t, h, "POST", "/append",
		`{"scope":"s","ts":1000,"payload":{"v":"a"}}`)
	_, _, _ = doRequest(t, h, "POST", "/append",
		`{"scope":"s","payload":{"v":"no_ts_1"}}`)
	_, _, _ = doRequest(t, h, "POST", "/append",
		`{"scope":"s","ts":2000,"payload":{"v":"b"}}`)
	_, _, _ = doRequest(t, h, "POST", "/append",
		`{"scope":"s","payload":{"v":"no_ts_2"}}`)

	// Very wide window — ts-less items must still be excluded.
	_, out, _ := doRequest(t, h, "GET",
		"/ts_range?scope=s&since_ts=0&until_ts=999999999", "")
	items := out["items"].([]interface{})
	if len(items) != 2 {
		t.Fatalf("count=%d want 2 (ts-less items must be skipped)", len(items))
	}
}

func TestTsRange_ScopeMissReturnsEmpty(t *testing.T) {
	h, _ := newTestHandler(10)
	code, out, _ := doRequest(t, h, "GET",
		"/ts_range?scope=missing&since_ts=0", "")
	if code != 200 {
		t.Fatalf("code=%d want 200", code)
	}
	if mustBool(t, out, "hit") {
		t.Error("hit=true on missing scope")
	}
	if mustBool(t, out, "truncated") {
		t.Error("truncated=true on missing scope")
	}
}

// --- /ts_range: truncation ----------------------------------------------------

func TestTsRange_TruncatedWhenMoreAvailable(t *testing.T) {
	h, _ := newTestHandler(100)
	for i := 1; i <= 5; i++ {
		body := fmt.Sprintf(`{"scope":"s","ts":%d,"payload":{"v":%d}}`, i*1000, i)
		_, _, _ = doRequest(t, h, "POST", "/append", body)
	}
	// limit=3, window matches all 5 → truncated=true.
	_, out, _ := doRequest(t, h, "GET",
		"/ts_range?scope=s&since_ts=0&until_ts=999999999&limit=3", "")
	if !mustBool(t, out, "truncated") {
		t.Error("truncated=false but more matches existed beyond limit")
	}
	if mustFloat(t, out, "count") != 3 {
		t.Errorf("count=%v want 3", out["count"])
	}
}

func TestTsRange_NotTruncatedWhenExactFit(t *testing.T) {
	h, _ := newTestHandler(100)
	for i := 1; i <= 3; i++ {
		body := fmt.Sprintf(`{"scope":"s","ts":%d,"payload":{"v":%d}}`, i*1000, i)
		_, _, _ = doRequest(t, h, "POST", "/append", body)
	}
	// limit=3, exactly 3 match → truncated=false.
	_, out, _ := doRequest(t, h, "GET",
		"/ts_range?scope=s&since_ts=0&until_ts=999999999&limit=3", "")
	if mustBool(t, out, "truncated") {
		t.Error("truncated=true but batch fit exactly (no more matches)")
	}
}

// --- truncated flag on /head and /tail ---------------------------------------

func TestHead_TruncatedFlag(t *testing.T) {
	h, _ := newTestHandler(100)
	for i := 0; i < 5; i++ {
		_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","payload":{"v":1}}`)
	}

	// limit=3, 5 items total → truncated=true.
	_, out, _ := doRequest(t, h, "GET", "/head?scope=s&limit=3", "")
	if !mustBool(t, out, "truncated") {
		t.Error("truncated=false but 5 items exist, only 3 returned")
	}

	// limit=10, only 5 items → truncated=false.
	_, out, _ = doRequest(t, h, "GET", "/head?scope=s&limit=10", "")
	if mustBool(t, out, "truncated") {
		t.Error("truncated=true but all 5 items fit in limit=10")
	}
}

func TestTail_TruncatedFlag(t *testing.T) {
	h, _ := newTestHandler(100)
	for i := 0; i < 5; i++ {
		_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","payload":{"v":1}}`)
	}

	// limit=3, offset=0 → newest 3 of 5; 2 older items hidden → truncated=true.
	_, out, _ := doRequest(t, h, "GET", "/tail?scope=s&limit=3", "")
	if !mustBool(t, out, "truncated") {
		t.Error("truncated=false but older items exist before the tail window")
	}

	// limit=10 → all 5 fit, no older items hidden → truncated=false.
	_, out, _ = doRequest(t, h, "GET", "/tail?scope=s&limit=10", "")
	if mustBool(t, out, "truncated") {
		t.Error("truncated=true but whole scope fits")
	}
}

// --- /warm and /rebuild preserve ts ------------------------------------------

func TestWarm_PreservesTs(t *testing.T) {
	h, _ := newTestHandler(100)
	body := `{"items":[` +
		`{"scope":"s","id":"a","ts":1000,"payload":{"v":1}},` +
		`{"scope":"s","id":"b","payload":{"v":2}}` +
		`]}`
	_, _, _ = doRequest(t, h, "POST", "/warm", body)

	_, out, _ := doRequest(t, h, "GET", "/get?scope=s&id=a", "")
	a := out["item"].(map[string]interface{})
	if ts, _ := a["ts"].(float64); int64(ts) != 1000 {
		t.Fatalf("a.ts=%v want 1000 (preserved through /warm)", a["ts"])
	}

	_, out, _ = doRequest(t, h, "GET", "/get?scope=s&id=b", "")
	b := out["item"].(map[string]interface{})
	if _, has := b["ts"]; has {
		t.Errorf("b had ts stored but was appended without one: %v", b["ts"])
	}
}

// --- scope-buffer level: tsRange method ---------------------------------------

func TestScopeBuffer_TsRangeIgnoresItemsWithoutTs(t *testing.T) {
	buf := NewScopeBuffer(10)

	_, _ = buf.appendItem(Item{Scope: "s", Payload: json.RawMessage(`1`)})
	ts := int64(500)
	_, _ = buf.appendItem(Item{Scope: "s", Ts: &ts, Payload: json.RawMessage(`2`)})
	_, _ = buf.appendItem(Item{Scope: "s", Payload: json.RawMessage(`3`)})

	since := int64(0)
	until := int64(1000)
	items, truncated := buf.tsRange(&since, &until, 100)
	if len(items) != 1 {
		t.Fatalf("len=%d want 1 (only one item has ts)", len(items))
	}
	if truncated {
		t.Error("truncated=true but scan finished within limit")
	}
	if items[0].Ts == nil || *items[0].Ts != 500 {
		t.Fatalf("returned item missing ts=500: %+v", items[0])
	}
}

func TestScopeBuffer_TsRangeHonorsLimitAndSignalsTruncation(t *testing.T) {
	buf := NewScopeBuffer(10)
	for i := 1; i <= 5; i++ {
		ts := int64(i)
		_, _ = buf.appendItem(Item{
			Scope:   "s",
			Ts:      &ts,
			Payload: json.RawMessage(`1`),
		})
	}
	// limit=2, 5 items match → truncated=true.
	since := int64(0)
	items, truncated := buf.tsRange(&since, nil, 2)
	if len(items) != 2 {
		t.Fatalf("len=%d want 2", len(items))
	}
	if !truncated {
		t.Error("truncated=false despite more matches beyond limit")
	}
}
