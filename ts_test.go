package scopecache

import (
	"testing"
	"time"
)

// ts is cache-owned: every write that touches an item stamps
// time.Now().UnixMicro() under b.mu. Clients cannot supply ts on
// any write endpoint — the validator rejects a non-zero Item.Ts
// with 400. The field is observability only (not searchable, not
// indexed); these tests pin the contract: rejection on input,
// always-set on output, refresh-on-every-write semantics.

// --- Rejection: client-supplied ts is forbidden -------------------------------

func TestAppend_RejectsClientSuppliedTs(t *testing.T) {
	h, _ := newTestHandler(10)

	code, out, _ := doRequest(t, h, "POST", "/append",
		`{"scope":"s","id":"a","ts":1700000000000,"payload":{"v":1}}`)
	if code != 400 {
		t.Fatalf("code=%d want 400 (client-supplied ts must be rejected)", code)
	}
	if errMsg, _ := out["error"].(string); errMsg == "" {
		t.Errorf("expected an error message, got: %+v", out)
	}
}

func TestUpsert_RejectsClientSuppliedTs(t *testing.T) {
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "POST", "/upsert",
		`{"scope":"s","id":"a","ts":1700000000000,"payload":{"v":1}}`)
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
}

func TestUpdate_RejectsClientSuppliedTs(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","id":"a","payload":{"v":1}}`)

	code, _, _ := doRequest(t, h, "POST", "/update",
		`{"scope":"s","id":"a","ts":1700000000000,"payload":{"v":2}}`)
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
}

func TestAppend_RejectsNonIntegerTs(t *testing.T) {
	h, _ := newTestHandler(10)
	// A fractional or string ts fails JSON unmarshal into int64, which the
	// decoder surfaces as "the request body must contain valid JSON" (400).
	code, _, _ := doRequest(t, h, "POST", "/append",
		`{"scope":"s","id":"a","ts":"notanumber","payload":{"v":1}}`)
	if code != 400 {
		t.Fatalf("code=%d want 400", code)
	}
}

// --- Cache assigns ts on every write path -------------------------------------

func TestAppend_AssignsTs(t *testing.T) {
	h, _ := newTestHandler(10)

	before := time.Now().UnixMicro()
	_, out, _ := doRequest(t, h, "POST", "/append",
		`{"scope":"s","id":"a","payload":{"v":1}}`)
	after := time.Now().UnixMicro()

	item := out["item"].(map[string]interface{})
	ts, ok := item["ts"].(float64)
	if !ok {
		t.Fatalf("ts missing or wrong type in response: %+v", item)
	}
	if int64(ts) < before || int64(ts) > after {
		t.Fatalf("ts=%d outside expected window [%d, %d]", int64(ts), before, after)
	}
}

func TestUpsert_AssignsTsOnCreate(t *testing.T) {
	h, _ := newTestHandler(10)
	before := time.Now().UnixMicro()
	_, out, _ := doRequest(t, h, "POST", "/upsert",
		`{"scope":"s","id":"a","payload":{"v":1}}`)
	after := time.Now().UnixMicro()

	item := out["item"].(map[string]interface{})
	ts := int64(item["ts"].(float64))
	if ts < before || ts > after {
		t.Fatalf("create ts=%d outside [%d, %d]", ts, before, after)
	}
	if !mustBool(t, out, "created") {
		t.Errorf("expected created=true on first upsert")
	}
}

func TestUpsert_RefreshesTsOnReplace(t *testing.T) {
	h, _ := newTestHandler(10)

	_, out1, _ := doRequest(t, h, "POST", "/upsert",
		`{"scope":"s","id":"a","payload":{"v":1}}`)
	firstTs := int64(out1["item"].(map[string]interface{})["ts"].(float64))

	// Sleep 2ms so the refresh is observable even on fast clocks.
	time.Sleep(2 * time.Millisecond)

	_, out2, _ := doRequest(t, h, "POST", "/upsert",
		`{"scope":"s","id":"a","payload":{"v":2}}`)
	secondTs := int64(out2["item"].(map[string]interface{})["ts"].(float64))

	if secondTs <= firstTs {
		t.Fatalf("upsert replace did not refresh ts: first=%d, second=%d", firstTs, secondTs)
	}
	if mustBool(t, out2, "created") {
		t.Errorf("expected created=false on replace, got true")
	}
}

func TestUpdate_RefreshesTs(t *testing.T) {
	h, _ := newTestHandler(10)
	_, out1, _ := doRequest(t, h, "POST", "/append",
		`{"scope":"s","id":"a","payload":{"v":1}}`)
	firstTs := int64(out1["item"].(map[string]interface{})["ts"].(float64))

	time.Sleep(2 * time.Millisecond)

	_, _, _ = doRequest(t, h, "POST", "/update",
		`{"scope":"s","id":"a","payload":{"v":2}}`)

	_, getOut, _ := doRequest(t, h, "GET", "/get?scope=s&id=a", "")
	updatedTs := int64(getOut["item"].(map[string]interface{})["ts"].(float64))

	if updatedTs <= firstTs {
		t.Fatalf("update did not refresh ts: append=%d, post-update=%d", firstTs, updatedTs)
	}
}

func TestCounterAdd_AssignsTsOnCreate(t *testing.T) {
	h, _ := newTestHandler(10)
	before := time.Now().UnixMicro()
	_, _, _ = doRequest(t, h, "POST", "/counter_add",
		`{"scope":"s","id":"c","by":5}`)
	after := time.Now().UnixMicro()

	_, out, _ := doRequest(t, h, "GET", "/get?scope=s&id=c", "")
	ts := int64(out["item"].(map[string]interface{})["ts"].(float64))
	if ts < before || ts > after {
		t.Fatalf("create ts=%d outside [%d, %d]", ts, before, after)
	}
}

func TestCounterAdd_RefreshesTsOnIncrement(t *testing.T) {
	h, _ := newTestHandler(10)
	_, _, _ = doRequest(t, h, "POST", "/counter_add", `{"scope":"s","id":"c","by":5}`)
	_, getOut, _ := doRequest(t, h, "GET", "/get?scope=s&id=c", "")
	firstTs := int64(getOut["item"].(map[string]interface{})["ts"].(float64))

	time.Sleep(2 * time.Millisecond)

	_, _, _ = doRequest(t, h, "POST", "/counter_add", `{"scope":"s","id":"c","by":3}`)
	_, getOut2, _ := doRequest(t, h, "GET", "/get?scope=s&id=c", "")
	secondTs := int64(getOut2["item"].(map[string]interface{})["ts"].(float64))

	if secondTs <= firstTs {
		t.Fatalf("counter increment did not refresh ts: first=%d, second=%d", firstTs, secondTs)
	}
}

// --- /warm and /rebuild stamp ts on every item --------------------------------

func TestWarm_StampsTs(t *testing.T) {
	h, _ := newTestHandler(100)
	body := `{"items":[` +
		`{"scope":"s","id":"a","payload":{"v":1}},` +
		`{"scope":"s","id":"b","payload":{"v":2}}` +
		`]}`

	before := time.Now().UnixMicro()
	_, _, _ = doAdminRequest(t, h, "/warm", body)
	after := time.Now().UnixMicro()

	for _, id := range []string{"a", "b"} {
		_, out, _ := doRequest(t, h, "GET", "/get?scope=s&id="+id, "")
		ts := int64(out["item"].(map[string]interface{})["ts"].(float64))
		if ts < before || ts > after {
			t.Errorf("/warm item %q ts=%d outside [%d, %d]", id, ts, before, after)
		}
	}
}

func TestWarm_RejectsClientSuppliedTs(t *testing.T) {
	h, _ := newTestHandler(100)
	body := `{"items":[{"scope":"s","id":"a","ts":1000,"payload":{"v":1}}]}`

	code, _, _ := doAdminRequest(t, h, "/warm", body)
	if code != 400 {
		t.Fatalf("code=%d want 400 (client-supplied ts on /warm must be rejected)", code)
	}
}
