package scopecache

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// newInboxHandler builds a Store + handler with /inbox enabled for the
// scope names supplied. /inbox requires both a server secret and at
// least one inbox scope to register; without either the route is not
// in the mux.
func newInboxHandler(t *testing.T, scopes ...string) (http.Handler, *API) {
	t.Helper()
	api := NewAPI(
		NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20}),
		APIConfig{
			MaxResponseBytes:  25 << 20,
			MaxMultiCallBytes: 16 << 20,
			MaxMultiCallCount: 10,
			ServerSecret:      testServerSecret,
			InboxScopes:       scopes,
			EnableAdmin:       true,
		},
	)
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)
	return mux, api
}

// inboxItemIDPattern matches the cache-assigned id format:
// <64-hex-capId>:<32-hex-random>.
var inboxItemIDPattern = regexp.MustCompile(`^[a-f0-9]{64}:[a-f0-9]{32}$`)

// --- happy path ---------------------------------------------------------------

func TestInbox_HappyPath(t *testing.T) {
	h, api := newInboxHandler(t, "_inbox")
	provisionTenant(t, h, "alice")
	capID := computeCapForTest(testServerSecret, "alice")

	body := `{"token":"alice","scope":"_inbox","payload":{"event":"signup"}}`
	code, out, raw := doRequest(t, h, "POST", "/inbox", body)
	if code != 200 {
		t.Fatalf("code=%d body=%s", code, raw)
	}
	if !mustBool(t, out, "ok") {
		t.Errorf("ok=false: %s", raw)
	}
	if _, ok := out["ts"].(float64); !ok {
		t.Errorf("response missing ts: %s", raw)
	}
	// Response is intentionally minimal — no id, no seq, no scope echo.
	for _, leak := range []string{"id", "seq", "scope", "item"} {
		if _, present := out[leak]; present {
			t.Errorf("response leaks unexpected field %q: %s", leak, raw)
		}
	}

	// The item must exist in the inbox scope with cache-assigned id+ts.
	buf, ok := api.store.getScope("_inbox")
	if !ok {
		t.Fatal("_inbox scope was not created")
	}
	buf.mu.RLock()
	if len(buf.items) != 1 {
		t.Fatalf("inbox has %d items, want 1", len(buf.items))
	}
	item := buf.items[0]
	buf.mu.RUnlock()

	if !inboxItemIDPattern.MatchString(item.ID) {
		t.Errorf("id=%q does not match cache-assigned format <capId>:<32-hex>", item.ID)
	}
	if !strings.HasPrefix(item.ID, capID+":") {
		t.Errorf("id=%q does not start with caller's capId %q", item.ID, capID)
	}
	if item.Ts == 0 {
		t.Errorf("ts not auto-set: %v", item.Ts)
	}
	if string(item.Payload) != `{"event":"signup"}` {
		t.Errorf("payload mutated: %q", string(item.Payload))
	}
}

// --- attribution and randomness -----------------------------------------------

// Two writes from the same tenant must produce different ids — the
// random suffix is the uniqueness guarantee. Two tenants writing
// produce ids whose capId-prefix differs, so the operator can drain
// and attribute trivially.
func TestInbox_AttributionAndUniqueness(t *testing.T) {
	h, api := newInboxHandler(t, "_inbox")
	provisionTenant(t, h, "alice")
	provisionTenant(t, h, "bob")
	capA := computeCapForTest(testServerSecret, "alice")
	capB := computeCapForTest(testServerSecret, "bob")

	for _, who := range []string{"alice", "alice", "bob"} {
		body := fmt.Sprintf(`{"token":%q,"scope":"_inbox","payload":{"by":%q}}`, who, who)
		if code, _, raw := doRequest(t, h, "POST", "/inbox", body); code != 200 {
			t.Fatalf("inbox by %s: code=%d body=%s", who, code, raw)
		}
	}

	buf, _ := api.store.getScope("_inbox")
	buf.mu.RLock()
	defer buf.mu.RUnlock()

	if len(buf.items) != 3 {
		t.Fatalf("inbox has %d items, want 3", len(buf.items))
	}

	seen := map[string]bool{}
	aliceCount, bobCount := 0, 0
	for _, item := range buf.items {
		if seen[item.ID] {
			t.Errorf("duplicate id observed: %q", item.ID)
		}
		seen[item.ID] = true
		switch {
		case strings.HasPrefix(item.ID, capA+":"):
			aliceCount++
		case strings.HasPrefix(item.ID, capB+":"):
			bobCount++
		default:
			t.Errorf("id %q does not match either provisioned tenant prefix", item.ID)
		}
	}
	if aliceCount != 2 || bobCount != 1 {
		t.Errorf("attribution mismatch: alice=%d (want 2), bob=%d (want 1)", aliceCount, bobCount)
	}
}

// --- forbidden field rejection ------------------------------------------------

func TestInbox_ForbiddenFields(t *testing.T) {
	h, _ := newInboxHandler(t, "_inbox")
	provisionTenant(t, h, "alice")

	cases := []struct {
		name    string
		body    string
		wantSub string
	}{
		{"id forbidden", `{"token":"alice","scope":"_inbox","payload":1,"id":"x"}`, "'id' field is forbidden"},
		{"seq forbidden", `{"token":"alice","scope":"_inbox","payload":1,"seq":42}`, "'seq' field is forbidden"},
		{"ts forbidden", `{"token":"alice","scope":"_inbox","payload":1,"ts":1234567890}`, "'ts' field is forbidden"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, raw := doRequest(t, h, "POST", "/inbox", tc.body)
			if code != 400 {
				t.Fatalf("code=%d want 400, body=%s", code, raw)
			}
			if !strings.Contains(raw, tc.wantSub) {
				t.Errorf("error %q missing substring %q", raw, tc.wantSub)
			}
		})
	}
}

// --- missing-field rejection --------------------------------------------------

func TestInbox_MissingToken(t *testing.T) {
	h, _ := newInboxHandler(t, "_inbox")
	body := `{"scope":"_inbox","payload":1}`
	code, _, raw := doRequest(t, h, "POST", "/inbox", body)
	if code != 401 {
		t.Fatalf("code=%d want 401, body=%s", code, raw)
	}
	if !strings.Contains(raw, "'token' field is required") {
		t.Errorf("missing token error: %s", raw)
	}
}

func TestInbox_MissingScope(t *testing.T) {
	h, _ := newInboxHandler(t, "_inbox")
	provisionTenant(t, h, "alice")
	body := `{"token":"alice","payload":1}`
	code, _, raw := doRequest(t, h, "POST", "/inbox", body)
	if code != 400 {
		t.Fatalf("code=%d want 400, body=%s", code, raw)
	}
	if !strings.Contains(raw, "'scope' field is required") {
		t.Errorf("missing scope error: %s", raw)
	}
}

func TestInbox_MissingPayload(t *testing.T) {
	h, _ := newInboxHandler(t, "_inbox")
	provisionTenant(t, h, "alice")
	body := `{"token":"alice","scope":"_inbox"}`
	code, _, raw := doRequest(t, h, "POST", "/inbox", body)
	if code != 400 {
		t.Fatalf("code=%d want 400, body=%s", code, raw)
	}
	if !strings.Contains(raw, "'payload' field is required") {
		t.Errorf("missing payload error: %s", raw)
	}
}

func TestInbox_NullPayload(t *testing.T) {
	h, _ := newInboxHandler(t, "_inbox")
	provisionTenant(t, h, "alice")
	body := `{"token":"alice","scope":"_inbox","payload":null}`
	code, _, _ := doRequest(t, h, "POST", "/inbox", body)
	if code != 400 {
		t.Fatalf("null payload accepted: code=%d", code)
	}
}

// /inbox shares payloadPresent() with the other write paths
// (/append, /upsert, /update). That helper trims whitespace around
// `null` before comparing — so `" null "` must reject just like
// `null`. Without using payloadPresent, the inbox handler's earlier
// hand-rolled check let `" null "` slip past the first guard and
// only fail later inside validateWriteItem; same final 400, but
// inconsistent error path that would drift under refactor.
func TestInbox_PayloadNullWithWhitespace(t *testing.T) {
	h, _ := newInboxHandler(t, "_inbox")
	provisionTenant(t, h, "alice")
	body := `{"token":"alice","scope":"_inbox","payload": null }`
	code, _, raw := doRequest(t, h, "POST", "/inbox", body)
	if code != 400 {
		t.Fatalf("whitespace-padded null payload accepted: code=%d body=%s", code, raw)
	}
	if !strings.Contains(raw, "'payload' field is required") {
		t.Errorf("expected 'payload required' error, got: %s", raw)
	}
}

// --- scope allowlist ----------------------------------------------------------

func TestInbox_ScopeNotInAllowlist(t *testing.T) {
	h, _ := newInboxHandler(t, "_inbox", "audit")
	provisionTenant(t, h, "alice")
	body := `{"token":"alice","scope":"random_scope","payload":1}`
	code, _, raw := doRequest(t, h, "POST", "/inbox", body)
	if code != 400 {
		t.Fatalf("code=%d want 400, body=%s", code, raw)
	}
	if !strings.Contains(raw, "not configured as an inbox scope") {
		t.Errorf("expected allowlist error, got: %s", raw)
	}
}

func TestInbox_MultipleAllowedScopes(t *testing.T) {
	h, api := newInboxHandler(t, "_inbox", "audit")
	provisionTenant(t, h, "alice")

	for _, scope := range []string{"_inbox", "audit"} {
		body := fmt.Sprintf(`{"token":"alice","scope":%q,"payload":{"to":%q}}`, scope, scope)
		if code, _, raw := doRequest(t, h, "POST", "/inbox", body); code != 200 {
			t.Fatalf("inbox to %q: code=%d body=%s", scope, code, raw)
		}
		buf, ok := api.store.getScope(scope)
		if !ok || len(buf.items) != 1 {
			t.Errorf("scope %q did not receive its item", scope)
		}
	}
}

// --- auth-gate ---------------------------------------------------------------

func TestInbox_TenantNotProvisioned(t *testing.T) {
	h, _ := newInboxHandler(t, "_inbox")
	// no provisionTenant — token has no _tokens entry
	body := `{"token":"rogue","scope":"_inbox","payload":1}`
	code, _, raw := doRequest(t, h, "POST", "/inbox", body)
	if code != 400 {
		t.Fatalf("code=%d want 400, body=%s", code, raw)
	}
	if !strings.Contains(raw, "tenant_not_provisioned") {
		t.Errorf("expected tenant_not_provisioned, got: %s", raw)
	}
}

// Anchored: an unprovisioned token must not lazily create the inbox
// scope. /inbox uses getOrCreateScope, but the auth-gate runs before
// it, so the rogue token's call exits before any side effect lands.
func TestInbox_UnprovisionedTokenLeavesNoSideEffect(t *testing.T) {
	h, api := newInboxHandler(t, "_fresh_inbox")
	body := `{"token":"rogue","scope":"_fresh_inbox","payload":1}`
	if code, _, _ := doRequest(t, h, "POST", "/inbox", body); code != 400 {
		t.Fatal("rogue inbox call should have failed at the auth-gate")
	}
	if _, ok := api.store.getScope("_fresh_inbox"); ok {
		t.Error("rejected /inbox lazily created the scope")
	}
}

// --- method handling ----------------------------------------------------------

func TestInbox_GETRejected(t *testing.T) {
	h, _ := newInboxHandler(t, "_inbox")
	code, _, _ := doRequest(t, h, "GET", "/inbox", "")
	if code != http.StatusMethodNotAllowed {
		t.Fatalf("code=%d want 405", code)
	}
}

// --- registration ------------------------------------------------------------

// /inbox is conditionally registered: requires both a non-empty
// server secret AND at least one inbox scope. Either missing → route
// not in the mux, public callers get 404.
func TestInbox_NotRegisteredWithoutServerSecret(t *testing.T) {
	api := NewAPI(
		NewStore(Config{ScopeMaxItems: 10, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20}),
		APIConfig{
			MaxResponseBytes:  25 << 20,
			MaxMultiCallBytes: 16 << 20,
			MaxMultiCallCount: 10,
			// no ServerSecret
			InboxScopes: []string{"_inbox"},
		},
	)
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)
	code, _, _ := doRequest(t, mux, "POST", "/inbox", `{"token":"x","scope":"_inbox","payload":1}`)
	if code != 404 {
		t.Fatalf("code=%d want 404 (route not registered), body unused", code)
	}
}

func TestInbox_NotRegisteredWithoutInboxScopes(t *testing.T) {
	// Default test handler has ServerSecret but no InboxScopes.
	h, _ := newTestHandler(10)
	code, _, _ := doRequest(t, h, "POST", "/inbox", `{"token":"x","scope":"_inbox","payload":1}`)
	if code != 404 {
		t.Fatalf("code=%d want 404 (route not registered), body unused", code)
	}
}

// --- isolation: tenant cannot read what they wrote --------------------------

// Tenant writes via /inbox; tries to read same scope via /guarded.
// /guarded rewrites the scope to `_guarded:<capId>:_inbox`, which is
// a different scope name — no leak. The actual /inbox scope is only
// reachable via /admin (operator).
func TestInbox_TenantCannotReadOwnInbox(t *testing.T) {
	h, _ := newInboxHandler(t, "_inbox")
	provisionTenant(t, h, "alice")

	if code, _, raw := doRequest(t, h, "POST", "/inbox", `{"token":"alice","scope":"_inbox","payload":{"x":1}}`); code != 200 {
		t.Fatalf("inbox write: code=%d body=%s", code, raw)
	}

	// /guarded /tail scope=_inbox would rewrite to
	// `_guarded:<capId>:_inbox` (a scope that doesn't exist for this
	// tenant). Tail returns hit:false. The actual inbox is invisible.
	read := `{"token":"alice","calls":[{"path":"/tail","query":{"scope":"_inbox","limit":10}}]}`
	code, out, raw := doRequest(t, h, "POST", "/guarded", read)
	if code != 200 {
		t.Fatalf("guarded read: code=%d body=%s", code, raw)
	}
	results := out["results"].([]interface{})
	slot := results[0].(map[string]interface{})["body"].(map[string]interface{})
	if items, _ := slot["items"].([]interface{}); len(items) != 0 {
		t.Errorf("/guarded /tail leaked inbox items: %s", raw)
	}

	// Operator-side via /admin, however, sees the items just fine.
	adminTail := `{"calls":[{"path":"/tail","query":{"scope":"_inbox","limit":10}}]}`
	code, out, raw = doRequest(t, h, "POST", "/admin", adminTail)
	if code != 200 {
		t.Fatalf("admin tail: code=%d body=%s", code, raw)
	}
	results = out["results"].([]interface{})
	slot = results[0].(map[string]interface{})["body"].(map[string]interface{})
	items, _ := slot["items"].([]interface{})
	if len(items) != 1 {
		t.Fatalf("admin sees %d items, want 1: %s", len(items), raw)
	}
	first := items[0].(map[string]interface{})
	payload, _ := json.Marshal(first["payload"])
	if string(payload) != `{"x":1}` {
		t.Errorf("payload changed in transit: %s", payload)
	}
}

// --- /inbox payload cap ------------------------------------------------------

// newInboxHandlerCap builds a handler with a custom MaxInboxBytes so
// tests can probe the cap edge with small (deterministic) payloads.
// All other capacity knobs match newInboxHandler.
func newInboxHandlerCap(t *testing.T, maxInboxBytes int64, scopes ...string) (http.Handler, *API) {
	t.Helper()
	api := NewAPI(
		NewStore(Config{ScopeMaxItems: 100, MaxStoreBytes: 100 << 20, MaxItemBytes: 1 << 20}),
		APIConfig{
			MaxResponseBytes:  25 << 20,
			MaxMultiCallBytes: 16 << 20,
			MaxMultiCallCount: 10,
			MaxInboxBytes:     maxInboxBytes,
			ServerSecret:      testServerSecret,
			InboxScopes:       scopes,
			EnableAdmin:       true,
		},
	)
	mux := http.NewServeMux()
	api.RegisterRoutes(mux)
	return mux, api
}

// Default Config (zero MaxInboxBytes) must yield 64 KiB per /inbox
// payload — the documented compile-time default. Pinning this on the
// wire stops a future refactor from silently switching the default to
// MaxItemBytes again, which is the regression this whole feature
// exists to prevent.
func TestInbox_DefaultCapIs64KiB(t *testing.T) {
	h, _ := newInboxHandler(t, "_inbox") // no MaxInboxBytes → uses default
	provisionTenant(t, h, "alice")

	// 65 KiB payload (one byte over the default 64 KiB) must be
	// rejected with a 400 that names both the actual size and the cap.
	bigPayload := strings.Repeat("a", (64<<10)+1)
	body := fmt.Sprintf(`{"token":"alice","scope":"_inbox","payload":"%s"}`, bigPayload)
	code, out, raw := doRequest(t, h, "POST", "/inbox", body)
	if code != 400 {
		t.Fatalf("over-cap on default config: code=%d want 400, body=%.200s", code, raw)
	}
	errStr, _ := out["error"].(string)
	if !strings.Contains(errStr, "exceeds the /inbox cap") {
		t.Errorf("error message does not name /inbox cap: %s", errStr)
	}
	// Cap value 65536 must appear in the message — that's the
	// operator's hint to either shrink the payload or raise
	// SCOPECACHE_MAX_INBOX_KB.
	if !strings.Contains(errStr, "65536") {
		t.Errorf("error message does not name the cap value (65536): %s", errStr)
	}
}

// At-cap (exactly maxInboxBytes) must succeed; one byte over must
// fail. Boundary check on the inequality (`>` not `>=`).
func TestInbox_CapBoundary(t *testing.T) {
	const cap = 1024
	h, _ := newInboxHandlerCap(t, cap, "_inbox")
	provisionTenant(t, h, "alice")

	// JSON-encoded string payload of length cap. The serialized form is
	// `"<cap chars>"` — len = cap + 2 (the quotes). We need
	// len(req.Payload) == cap, so we use a string of length cap-2.
	atCap := `"` + strings.Repeat("a", cap-2) + `"`
	if int64(len(atCap)) != cap {
		t.Fatalf("test bug: at-cap payload is %d bytes, want %d", len(atCap), cap)
	}
	body := fmt.Sprintf(`{"token":"alice","scope":"_inbox","payload":%s}`, atCap)
	code, _, raw := doRequest(t, h, "POST", "/inbox", body)
	if code != 200 {
		t.Errorf("at-cap should succeed: code=%d body=%.200s", code, raw)
	}

	// One byte over.
	overCap := `"` + strings.Repeat("a", cap-1) + `"`
	if int64(len(overCap)) != cap+1 {
		t.Fatalf("test bug: over-cap payload is %d bytes, want %d", len(overCap), cap+1)
	}
	body = fmt.Sprintf(`{"token":"alice","scope":"_inbox","payload":%s}`, overCap)
	code, _, raw = doRequest(t, h, "POST", "/inbox", body)
	if code != 400 {
		t.Errorf("over-cap by 1 byte should fail: code=%d body=%.200s", code, raw)
	}
}

// The /inbox cap is independent of the per-item cap — even when a
// tenant could legitimately /append a 1 MiB item, /inbox rejects
// anything over the inbox cap. This pins the "tighter than per-item
// by design" guarantee.
func TestInbox_CapIsIndependentOfPerItemCap(t *testing.T) {
	// Per-item cap 1 MiB (default), inbox cap 16 KiB.
	h, _ := newInboxHandlerCap(t, 16<<10, "_inbox")
	provisionTenant(t, h, "alice")

	// 32 KiB payload — well under per-item cap, well over inbox cap.
	bigPayload := strings.Repeat("x", 32<<10)
	body := fmt.Sprintf(`{"token":"alice","scope":"_inbox","payload":"%s"}`, bigPayload)
	code, out, raw := doRequest(t, h, "POST", "/inbox", body)
	if code != 400 {
		t.Fatalf("over inbox-cap (well under item cap): code=%d body=%.200s", code, raw)
	}
	if errStr, _ := out["error"].(string); !strings.Contains(errStr, "/inbox cap") {
		t.Errorf("error should name /inbox cap, not per-item cap: %s", errStr)
	}
}

// The cap check runs BEFORE the auth-gate, mirroring /guarded's
// pre-flight response-cap check. Reasoning: a misconfigured tenant
// that hits the inbox cap should learn about the cap, not get a
// confusing tenant_not_provisioned hiding the real issue. This test
// pins that ordering.
func TestInbox_CapCheckRunsBeforeAuth(t *testing.T) {
	h, _ := newInboxHandlerCap(t, 1024, "_inbox")
	// Deliberately do NOT provision the tenant — auth will fail if
	// reached.

	bigPayload := strings.Repeat("a", 2048)
	body := fmt.Sprintf(`{"token":"unprovisioned","scope":"_inbox","payload":"%s"}`, bigPayload)
	code, out, raw := doRequest(t, h, "POST", "/inbox", body)
	if code != 400 {
		t.Fatalf("code=%d body=%.200s", code, raw)
	}
	errStr, _ := out["error"].(string)
	if strings.Contains(errStr, "tenant_not_provisioned") {
		t.Errorf("auth check ran before cap check (or instead of it): %s", errStr)
	}
	if !strings.Contains(errStr, "/inbox cap") {
		t.Errorf("expected /inbox cap error, got: %s", errStr)
	}
}
