package scopecache

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// newCappedHandler is a focused variant of newTestHandler that lets a test
// pin MaxResponseBytes to an arbitrary value. The other caps stay generous
// so the test can drive only the per-response cap.
func newCappedHandler(maxResponseBytes int64) http.Handler {
	api := NewAPI(
		NewStore(Config{ScopeMaxItems: 1000, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20}),
		APIConfig{MaxResponseBytes: maxResponseBytes},
	)
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)
	return mux
}

func TestResponseCap_HeadIncludesApproxResponseMB(t *testing.T) {
	h := newCappedHandler(25 << 20)
	for i := 0; i < 3; i++ {
		_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","payload":{"v":1}}`)
	}

	code, out, raw := doRequest(t, h, "GET", "/head?scope=s&limit=10", "")
	if code != 200 {
		t.Fatalf("code=%d want 200, body=%s", code, raw)
	}
	if _, ok := out["approx_response_mb"]; !ok {
		t.Fatalf("missing approx_response_mb in response: %s", raw)
	}
	v := mustFloat(t, out, "approx_response_mb")
	if v <= 0 {
		t.Errorf("approx_response_mb=%v want > 0", v)
	}
	// The reported MB value must match the body length itself, in MiB with
	// 4-decimal precision (matching MB.MarshalJSON). We round both to 4
	// decimals before comparing so float noise from JSON parsing does not
	// produce a spurious mismatch.
	gotBytes := float64(len(raw))
	wantMB := float64(int64(gotBytes/1048576.0*10000.0+0.5)) / 10000.0
	if v != wantMB {
		t.Errorf("approx_response_mb=%v want %v (body=%d bytes)", v, wantMB, len(raw))
	}
}

func TestResponseCap_TailIncludesApproxResponseMB(t *testing.T) {
	h := newCappedHandler(25 << 20)
	for i := 0; i < 3; i++ {
		_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","payload":{"v":1}}`)
	}

	code, out, raw := doRequest(t, h, "GET", "/tail?scope=s&limit=10", "")
	if code != 200 {
		t.Fatalf("code=%d want 200, body=%s", code, raw)
	}
	if _, ok := out["approx_response_mb"]; !ok {
		t.Fatalf("missing approx_response_mb in response: %s", raw)
	}
}

func TestResponseCap_HeadOverflowReturns507(t *testing.T) {
	// 50 bytes is small enough that any non-trivial JSON envelope blows
	// past it — perfect for forcing the cap path without having to load
	// 25 MiB of data.
	h := newCappedHandler(50)
	for i := 0; i < 3; i++ {
		_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","payload":{"v":1}}`)
	}

	code, out, raw := doRequest(t, h, "GET", "/head?scope=s&limit=10", "")
	if code != http.StatusInsufficientStorage {
		t.Fatalf("code=%d want 507, body=%s", code, raw)
	}
	if mustBool(t, out, "ok") {
		t.Fatal("ok=true on overflow response")
	}
	if _, ok := out["approx_response_mb"]; !ok {
		t.Errorf("507 body missing approx_response_mb: %s", raw)
	}
	if _, ok := out["max_response_mb"]; !ok {
		t.Errorf("507 body missing max_response_mb: %s", raw)
	}
	if errMsg, _ := out["error"].(string); !strings.Contains(errMsg, "exceed") {
		t.Errorf("error=%q want substring 'exceed'", errMsg)
	}
}

func TestResponseCap_TailOverflowReturns507(t *testing.T) {
	h := newCappedHandler(50)
	for i := 0; i < 3; i++ {
		_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","payload":{"v":1}}`)
	}

	code, _, raw := doRequest(t, h, "GET", "/tail?scope=s&limit=10", "")
	if code != http.StatusInsufficientStorage {
		t.Fatalf("code=%d want 507, body=%s", code, raw)
	}
}

// TestResponseCap_OtherEndpointsUnaffected confirms the cap is wired only
// on the three endpoints that can produce limit-scaled bodies. /append
// is a small write, /get is single-item — neither goes through capResponse
// so a tiny cap must not affect them.
func TestResponseCap_OtherEndpointsUnaffected(t *testing.T) {
	h := newCappedHandler(50)

	if code, _, raw := doRequest(t, h, "POST", "/append", `{"scope":"s","id":"a","payload":{"v":1}}`); code != 200 {
		t.Fatalf("/append code=%d want 200, body=%s", code, raw)
	}
	if code, _, raw := doRequest(t, h, "GET", "/get?scope=s&id=a", ""); code != 200 {
		t.Fatalf("/get code=%d want 200, body=%s", code, raw)
	}
}

// TestResponseCap_BoundaryAtCap exercises the wrapper's behaviour around
// the configured cap. We do NOT test exact equality (cap == body length)
// because the response body contains a duration_us field whose width
// varies with wall-clock jitter (80 µs vs 800 µs is one extra digit),
// so the body produced by a second call against a cap measured from the
// first call can be a few bytes larger than the calibration sample. CI
// runners have enough variance to flip the strict `written > cap` check
// over the boundary; localhost runs hide it. Test the two halves of the
// contract that ARE deterministic: a response with comfortable headroom
// passes, a response well above the cap fails.
func TestResponseCap_BoundaryAtCap(t *testing.T) {
	// Calibrate: measure /head?scope=missing under a generous cap.
	h := newCappedHandler(25 << 20)
	_, _, raw := doRequest(t, h, "GET", "/head?scope=missing", "")
	bodyLen := int64(len(raw))

	// Cap with a 32-byte margin above the measured body — absorbs any
	// duration_us width drift between calibration and the real call.
	h = newCappedHandler(bodyLen + 32)
	if code, _, body := doRequest(t, h, "GET", "/head?scope=missing", ""); code != 200 {
		t.Fatalf("margin-above-cap code=%d want 200, body=%s", code, body)
	}

	// Cap at half the measured size — definitely too small, must 507.
	h = newCappedHandler(bodyLen / 2)
	if code, _, body := doRequest(t, h, "GET", "/head?scope=missing", ""); code != http.StatusInsufficientStorage {
		t.Fatalf("well-below-cap code=%d want 507, body=%s", code, body)
	}
}

// TestGet_IncludesCountAndApproxResponseMB pins the uniform read-family
// response shape: every read-item endpoint (/head, /tail, /get)
// emits {count, approx_response_mb, duration_us} alongside its endpoint-
// specific fields. /get is the single-item member of that family — count
// is 1 on hit and 0 on miss; approx_response_mb is included regardless.
func TestGet_IncludesCountAndApproxResponseMB(t *testing.T) {
	h := newCappedHandler(25 << 20)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","id":"a","payload":{"v":1}}`)

	// Hit: count=1
	code, out, raw := doRequest(t, h, "GET", "/get?scope=s&id=a", "")
	if code != 200 {
		t.Fatalf("hit code=%d want 200, body=%s", code, raw)
	}
	if !mustBool(t, out, "hit") {
		t.Fatal("hit=false on present id")
	}
	if got := mustFloat(t, out, "count"); got != 1 {
		t.Errorf("hit count=%v want 1", got)
	}
	if _, ok := out["approx_response_mb"]; !ok {
		t.Errorf("hit body missing approx_response_mb: %s", raw)
	}
	if _, ok := out["duration_us"]; !ok {
		t.Errorf("hit body missing duration_us: %s", raw)
	}

	// Miss: count=0
	code, out, raw = doRequest(t, h, "GET", "/get?scope=s&id=missing", "")
	if code != 200 {
		t.Fatalf("miss code=%d want 200, body=%s", code, raw)
	}
	if mustBool(t, out, "hit") {
		t.Error("hit=true on missing id")
	}
	if got := mustFloat(t, out, "count"); got != 0 {
		t.Errorf("miss count=%v want 0", got)
	}
	if _, ok := out["approx_response_mb"]; !ok {
		t.Errorf("miss body missing approx_response_mb: %s", raw)
	}
}

// TestEstimateMultiItemResponseBytes_IsLowerBound guards the pre-flight
// helper's correctness invariant: the returned estimate must be at or
// below the actual JSON-marshal size of the envelope that handleHead /
// handleTail produces. Overestimating would reject
// legitimate calls; the post-flight cappedResponseWriter would never
// see them.
//
// We sample a few representative shapes — empty payload, small
// payload, large payload, multi-item batches — and compare the
// estimate against an actual marshal of the same envelope shape.
func TestEstimateMultiItemResponseBytes_IsLowerBound(t *testing.T) {
	cases := []struct {
		name  string
		items []Item
	}{
		{"empty", []Item{}},
		{"one minimal", []Item{
			{Scope: "s", Seq: 1, Payload: json.RawMessage(`null`)},
		}},
		{"one with id and payload", []Item{
			{Scope: "scope-name", ID: "item-id", Seq: 42, Payload: json.RawMessage(`{"v":1,"name":"hello"}`)},
		}},
		{"three small", []Item{
			{Scope: "s", ID: "a", Seq: 1, Payload: json.RawMessage(`{"v":1}`)},
			{Scope: "s", ID: "b", Seq: 2, Payload: json.RawMessage(`{"v":2}`)},
			{Scope: "s", ID: "c", Seq: 3, Payload: json.RawMessage(`{"v":3}`)},
		}},
		{"large payload", []Item{
			{Scope: "s", ID: "big", Seq: 1, Payload: json.RawMessage(`"` + strings.Repeat("a", 4096) + `"`)},
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			estimate := estimateMultiItemResponseBytes(tc.items)
			envelope := map[string]interface{}{
				"ok":                 true,
				"hit":                len(tc.items) > 0,
				"count":              len(tc.items),
				"truncated":          false,
				"items":              tc.items,
				"duration_us":        9999,
				"approx_response_mb": 0.0001,
			}
			actual, err := json.Marshal(envelope)
			if err != nil {
				t.Fatalf("marshal failed: %v", err)
			}
			actualLen := int64(len(actual))
			if estimate > actualLen {
				t.Errorf("estimate=%d > actual=%d (overestimate would reject legitimate calls)\nbody=%s",
					estimate, actualLen, actual)
			}
		})
	}
}

// TestResponseCap_ApproxResponseMBJSONShape verifies the patched-in size
// field is valid JSON and parses as a number (MB type's MarshalJSON outputs
// a bare float, not a string). Defends writeJSONWithMeta's slice-splice
// against accidentally producing malformed output.
func TestResponseCap_ApproxResponseMBJSONShape(t *testing.T) {
	h := newCappedHandler(25 << 20)
	_, _, _ = doRequest(t, h, "POST", "/append", `{"scope":"s","payload":{"v":1}}`)

	_, _, raw := doRequest(t, h, "GET", "/head?scope=s&limit=10", "")

	var v map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("response is not valid JSON: %v\nbody=%s", err, raw)
	}
	mb, ok := v["approx_response_mb"]
	if !ok {
		t.Fatalf("missing approx_response_mb")
	}
	var n float64
	if err := json.Unmarshal(mb, &n); err != nil {
		t.Fatalf("approx_response_mb not a number: %v (raw=%s)", err, mb)
	}
}
