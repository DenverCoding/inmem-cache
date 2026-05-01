package scopecache

import (
	"encoding/json"
	"testing"
)

func TestNormalizeLimit(t *testing.T) {
	tests := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"", DefaultLimit, false},
		{"500", 500, false},
		{"99999", MaxLimit, false},
		{"0", 0, true},
		{"-1", 0, true},
		{"abc", 0, true},
	}
	for _, tc := range tests {
		got, err := normalizeLimit(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("normalizeLimit(%q): err=%v wantErr=%v", tc.in, err, tc.wantErr)
			continue
		}
		if err == nil && got != tc.want {
			t.Errorf("normalizeLimit(%q)=%d want %d", tc.in, got, tc.want)
		}
	}
}

func TestNormalizeOffset(t *testing.T) {
	tests := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"", 0, false},
		{"5", 5, false},
		{"0", 0, false},
		{"-1", 0, true},
		{"abc", 0, true},
	}
	for _, tc := range tests {
		got, err := normalizeOffset(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("normalizeOffset(%q): err=%v wantErr=%v", tc.in, err, tc.wantErr)
			continue
		}
		if err == nil && got != tc.want {
			t.Errorf("normalizeOffset(%q)=%d want %d", tc.in, got, tc.want)
		}
	}
}

func TestNormalizeHours(t *testing.T) {
	got, err := normalizeHours("")
	if err != nil || got != 0 {
		t.Errorf("empty: got=%d err=%v", got, err)
	}
	got, err = normalizeHours("24")
	if err != nil || got != 24 {
		t.Errorf("24: got=%d err=%v", got, err)
	}
	if _, err := normalizeHours("-1"); err == nil {
		t.Error("-1 should error")
	}
	if _, err := normalizeHours("xx"); err == nil {
		t.Error("xx should error")
	}
	// Value that would overflow int64 when multiplied by μs/hour. The
	// ceiling is MaxInt64 / 3_600_000_000 ≈ 2.56e9; pick something clearly
	// past it so the overflow guard fires regardless of rounding.
	if _, err := normalizeHours("9223372036854775807"); err == nil {
		t.Error("MaxInt64 hours should error (overflow guard)")
	}
}

func TestValidateWriteItem(t *testing.T) {
	ok := Item{Scope: "s", Payload: json.RawMessage(`{"v":1}`)}
	if err := validateWriteItem(ok, "/append", MaxItemBytes); err != nil {
		t.Errorf("valid item rejected: %v", err)
	}

	if err := validateWriteItem(Item{Scope: "s"}, "/append", MaxItemBytes); err == nil {
		t.Error("missing payload should error")
	}

	if err := validateWriteItem(Item{}, "/append", MaxItemBytes); err == nil {
		t.Error("missing scope should error")
	}

	if err := validateWriteItem(Item{Scope: "s", Payload: json.RawMessage(`{}`), Seq: 5}, "/append", MaxItemBytes); err == nil {
		t.Error("non-zero seq should error")
	}
}

// Payload is opaque — the cache must accept any valid JSON value, not just
// objects. These are the shapes the previous "must be a JSON object" rule
// would have rejected.
func TestValidateWriteItem_AcceptsAnyJSONShape(t *testing.T) {
	shapes := []json.RawMessage{
		json.RawMessage(`"a bare string"`),
		json.RawMessage(`42`),
		json.RawMessage(`3.14`),
		json.RawMessage(`true`),
		json.RawMessage(`false`),
		json.RawMessage(`[1,2,3]`),
		json.RawMessage(`[]`),
		json.RawMessage(`{}`),
	}
	for _, p := range shapes {
		item := Item{Scope: "s", Payload: p}
		if err := validateWriteItem(item, "/append", MaxItemBytes); err != nil {
			t.Errorf("shape %s rejected: %v", string(p), err)
		}
	}
}

// Missing payload and literal `null` are both treated as "no payload".
// Every other value — including `""`, `0`, `false`, `[]`, `{}` — is accepted.
func TestValidateWriteItem_RejectsMissingAndNullPayload(t *testing.T) {
	cases := []struct {
		name string
		item Item
	}{
		{"missing (nil RawMessage)", Item{Scope: "s"}},
		{"literal null", Item{Scope: "s", Payload: json.RawMessage(`null`)}},
		{"literal null with whitespace", Item{Scope: "s", Payload: json.RawMessage(`  null  `)}},
	}
	for _, tc := range cases {
		if err := validateWriteItem(tc.item, "/append", MaxItemBytes); err == nil {
			t.Errorf("%s: expected error", tc.name)
		}
	}
}

func TestValidateWriteItem_RejectsOversizedPayload(t *testing.T) {
	// Build a raw payload that alone exceeds the per-item cap.
	buf := make([]byte, MaxItemBytes+1)
	for i := range buf {
		buf[i] = 'x'
	}
	item := Item{Scope: "s", Payload: json.RawMessage(buf)}
	if err := validateWriteItem(item, "/append", MaxItemBytes); err == nil {
		t.Error("expected oversized item to be rejected")
	}
}

func TestValidateWriteItem_AcceptsExactCapPayload(t *testing.T) {
	buf := make([]byte, MaxItemBytes/2)
	for i := range buf {
		buf[i] = 'y'
	}
	item := Item{Scope: "s", Payload: json.RawMessage(buf)}
	if err := validateWriteItem(item, "/append", MaxItemBytes); err != nil {
		t.Errorf("under-cap item rejected: %v", err)
	}
}

// JSON-string payloads materialise a precomputed renderBytes shortcut at
// write time (decoded form, used by /render to skip a per-hit Unmarshal);
// approxItemSize counts those bytes against the per-item cap. The validator
// runs before the buffer-write path has filled renderBytes, so it must
// inflate the size for string payloads itself — otherwise an N-byte JSON
// string passes the cap on raw bytes but is stored at ~2N + overhead,
// silently exceeding the documented per-item invariant (rfc §3).
//
// Constructed payload: `"AAAA…"` whose raw size is just over half the
// per-item cap. Post-precompute size = raw payload + decoded renderBytes
// (raw − 2) + ~48 overhead, which clears the cap.
func TestValidateWriteItem_StringPayloadCountsRenderBytes(t *testing.T) {
	n := MaxItemBytes/2 + 100
	buf := make([]byte, n)
	buf[0] = '"'
	for i := 1; i < n-1; i++ {
		buf[i] = 'A'
	}
	buf[n-1] = '"'

	item := Item{Scope: "s", Payload: json.RawMessage(buf)}
	if err := validateWriteItem(item, "/append", MaxItemBytes); err == nil {
		t.Fatal("expected rejection: JSON-string payload + renderBytes exceeds per-item cap")
	}
}

// Non-string payloads carry no renderBytes (precomputeRenderBytes returns
// nil unless the first non-whitespace byte is `"`). The validator must NOT
// inflate their size — a JSON array of the same raw byte count as the
// rejected string payload above must still pass the cap.
func TestValidateWriteItem_NonStringPayloadSkipsRenderBytesInflation(t *testing.T) {
	n := MaxItemBytes/2 + 100
	buf := make([]byte, n)
	buf[0] = '['
	for i := 1; i < n-1; i++ {
		if i%2 == 1 {
			buf[i] = '1'
		} else {
			buf[i] = ','
		}
	}
	if buf[n-2] == ',' {
		buf[n-2] = '1'
	}
	buf[n-1] = ']'

	item := Item{Scope: "s", Payload: json.RawMessage(buf)}
	if err := validateWriteItem(item, "/append", MaxItemBytes); err != nil {
		t.Errorf("non-string payload of raw size %d unexpectedly rejected: %v", n, err)
	}
}

func TestValidateWriteItem_ScopeAndIDShapeRules(t *testing.T) {
	longScope := string(make([]byte, MaxScopeBytes+1))
	for i := range longScope {
		_ = longScope[i]
	}
	big := make([]byte, MaxScopeBytes+1)
	for i := range big {
		big[i] = 'a'
	}
	bigID := make([]byte, MaxIDBytes+1)
	for i := range bigID {
		bigID[i] = 'b'
	}

	cases := []struct {
		name string
		item Item
	}{
		{"scope too long", Item{Scope: string(big), Payload: json.RawMessage(`{}`)}},
		{"scope has leading space", Item{Scope: " s", Payload: json.RawMessage(`{}`)}},
		{"scope has trailing newline", Item{Scope: "s\n", Payload: json.RawMessage(`{}`)}},
		{"scope has embedded tab", Item{Scope: "a\tb", Payload: json.RawMessage(`{}`)}},
		{"scope has NUL", Item{Scope: "a\x00b", Payload: json.RawMessage(`{}`)}},
		{"scope has DEL", Item{Scope: "a\x7fb", Payload: json.RawMessage(`{}`)}},
		{"id too long", Item{Scope: "s", ID: string(bigID), Payload: json.RawMessage(`{}`)}},
		{"id has surrounding whitespace", Item{Scope: "s", ID: " x ", Payload: json.RawMessage(`{}`)}},
		{"id has control char", Item{Scope: "s", ID: "x\x01", Payload: json.RawMessage(`{}`)}},
	}
	for _, tc := range cases {
		if err := validateWriteItem(tc.item, "/append", MaxItemBytes); err == nil {
			t.Errorf("%s: expected error", tc.name)
		}
	}
}

// hasReservedPrefix flags scopes starting with '_'. Used by the handler-
// level rejectReservedScope check (see handlers.go); validateScope itself
// stays focused on shape (length, whitespace, control chars) so /admin
// can bypass the reserved-prefix block via request context. Endpoint-
// level enforcement is exercised in admin_test.go and the per-endpoint
// public-handler tests.
func TestHasReservedPrefix(t *testing.T) {
	reserved := []string{"_", "_anything", "_guarded", "_guarded:abc:thread", "_counters_count_calls", "_token"}
	for _, scope := range reserved {
		if !hasReservedPrefix(scope) {
			t.Errorf("hasReservedPrefix(%q) = false, want true", scope)
		}
	}

	notReserved := []string{"name_with_underscore", "a_", "abc_def_ghi", "thread:1_2_3", "regular", ""}
	for _, scope := range notReserved {
		if hasReservedPrefix(scope) {
			t.Errorf("hasReservedPrefix(%q) = true, want false", scope)
		}
	}
}

// Anchors the scope/id length cap to 256 bytes. The cap was raised
// from 128 to 256 in v0.5.11 to make room for namespace prefixes —
// notably the `_guarded:<64 hex>:` rewrite, which leaves only
// 128-65=63 bytes for the tenant's own portion under the old cap.
// Tests that rely on MaxScopeBytes/MaxIDBytes constants continue to
// pass for any cap; this one fails specifically if the literal value
// gets reverted to something smaller.
func TestValidateWriteItem_KeyCapsAreAtLeast256(t *testing.T) {
	if MaxScopeBytes < 256 {
		t.Errorf("MaxScopeBytes=%d, expected >= 256", MaxScopeBytes)
	}
	if MaxIDBytes < 256 {
		t.Errorf("MaxIDBytes=%d, expected >= 256", MaxIDBytes)
	}
}

// Exact cap lengths should be accepted; Unicode (non-control) should pass.
func TestValidateWriteItem_AcceptsKeyEdges(t *testing.T) {
	maxScope := make([]byte, MaxScopeBytes)
	for i := range maxScope {
		maxScope[i] = 'a'
	}
	maxID := make([]byte, MaxIDBytes)
	for i := range maxID {
		maxID[i] = 'b'
	}

	cases := []struct {
		name string
		item Item
	}{
		{"scope at exact cap", Item{Scope: string(maxScope), Payload: json.RawMessage(`{}`)}},
		{"id at exact cap", Item{Scope: "s", ID: string(maxID), Payload: json.RawMessage(`{}`)}},
		{"unicode scope", Item{Scope: "user:café/ø", Payload: json.RawMessage(`{}`)}},
		{"empty id is allowed", Item{Scope: "s", ID: "", Payload: json.RawMessage(`{}`)}},
	}
	for _, tc := range cases {
		if err := validateWriteItem(tc.item, "/append", MaxItemBytes); err != nil {
			t.Errorf("%s: unexpectedly rejected: %v", tc.name, err)
		}
	}
}

func TestValidateUpdateItem(t *testing.T) {
	goodByID := Item{Scope: "s", ID: "a", Payload: json.RawMessage(`{"v":1}`)}
	if err := validateUpdateItem(goodByID, MaxItemBytes); err != nil {
		t.Errorf("valid update (by id) rejected: %v", err)
	}

	goodBySeq := Item{Scope: "s", Seq: 7, Payload: json.RawMessage(`{"v":1}`)}
	if err := validateUpdateItem(goodBySeq, MaxItemBytes); err != nil {
		t.Errorf("valid update (by seq) rejected: %v", err)
	}

	cases := []struct {
		name string
		item Item
	}{
		{"no scope", Item{ID: "a", Payload: json.RawMessage(`{}`)}},
		{"neither id nor seq", Item{Scope: "s", Payload: json.RawMessage(`{}`)}},
		{"both id and seq", Item{Scope: "s", ID: "a", Seq: 1, Payload: json.RawMessage(`{}`)}},
		{"no payload (by id)", Item{Scope: "s", ID: "a"}},
		{"no payload (by seq)", Item{Scope: "s", Seq: 1}},
		{"null payload", Item{Scope: "s", ID: "a", Payload: json.RawMessage(`null`)}},
		{"id with control char", Item{Scope: "s", ID: "a\x01", Payload: json.RawMessage(`{}`)}},
	}
	for _, tc := range cases {
		if err := validateUpdateItem(tc.item, MaxItemBytes); err == nil {
			t.Errorf("%s: expected error", tc.name)
		}
	}
}

func TestValidateDeleteRequest(t *testing.T) {
	if err := validateDeleteRequest(DeleteRequest{Scope: "s", ID: "a"}); err != nil {
		t.Errorf("valid (by id) rejected: %v", err)
	}
	if err := validateDeleteRequest(DeleteRequest{Scope: "s", Seq: 3}); err != nil {
		t.Errorf("valid (by seq) rejected: %v", err)
	}

	cases := []struct {
		name string
		req  DeleteRequest
	}{
		{"no scope", DeleteRequest{ID: "a"}},
		{"neither id nor seq", DeleteRequest{Scope: "s"}},
		{"both id and seq", DeleteRequest{Scope: "s", ID: "a", Seq: 1}},
		{"id with control char", DeleteRequest{Scope: "s", ID: "a\x01"}},
	}
	for _, tc := range cases {
		if err := validateDeleteRequest(tc.req); err == nil {
			t.Errorf("%s: expected error", tc.name)
		}
	}
}

func TestValidateDeleteUpToRequest(t *testing.T) {
	if err := validateDeleteUpToRequest(DeleteUpToRequest{Scope: "s", MaxSeq: 5}); err != nil {
		t.Errorf("valid rejected: %v", err)
	}
	if err := validateDeleteUpToRequest(DeleteUpToRequest{MaxSeq: 5}); err == nil {
		t.Error("empty scope should error")
	}
	if err := validateDeleteUpToRequest(DeleteUpToRequest{Scope: "s"}); err == nil {
		t.Error("zero max_seq should error")
	}
}

// The bulk request cap must always exceed the store cap so a fully-loaded
// cache can be restored in a single /warm or /rebuild call, regardless of
// how SCOPECACHE_MAX_STORE_MB is configured.
func TestBulkRequestBytesFor_ScalesWithStoreCap(t *testing.T) {
	cases := []int64{
		1 << 20,    // 1 MiB — pathological small store
		64 << 20,   // 64 MiB
		100 << 20,  // default
		500 << 20,  // 500 MiB
		1024 << 20, // 1 GiB
	}
	for _, storeBytes := range cases {
		got := bulkRequestBytesFor(storeBytes)
		if got <= storeBytes {
			t.Errorf("storeBytes=%d: bulk cap %d is not greater than store cap", storeBytes, got)
		}
		want := storeBytes + storeBytes/10 + BulkRequestBytesOverhead
		if got != want {
			t.Errorf("storeBytes=%d: got %d want %d", storeBytes, got, want)
		}
	}
}

func TestGroupItemsByScope(t *testing.T) {
	items := []Item{
		{Scope: "a", ID: "1"},
		{Scope: "b", ID: "2"},
		{Scope: "a", ID: "3"},
	}
	grouped := groupItemsByScope(items)
	if len(grouped) != 2 {
		t.Fatalf("got %d groups", len(grouped))
	}
	if len(grouped["a"]) != 2 {
		t.Errorf("scope a: got %d items", len(grouped["a"]))
	}
	if len(grouped["b"]) != 1 {
		t.Errorf("scope b: got %d items", len(grouped["b"]))
	}
}
