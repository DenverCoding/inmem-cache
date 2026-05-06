package scopecache

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

// Fuzz targets live at the validation boundary: every byte a client can
// control lands in one of these four functions before it reaches the store.
// The invariants checked here are the contracts the rest of the code relies
// on — if a fuzzer-found input breaks one, the bug is in the validator, not
// downstream.

// FuzzCheckKeyField: no accepted string may violate any of the three shape
// rules. This catches UTF-8 encoding tricks and off-by-one length bugs.
func FuzzCheckKeyField(f *testing.F) {
	f.Add("user:42", 128)
	f.Add(" leading", 128)
	f.Add("trailing\n", 128)
	f.Add("a\x00b", 128)
	f.Add("a\x7fb", 128)
	f.Add("", 0)
	f.Add("café/ø", 128)
	f.Fuzz(func(t *testing.T, s string, maxLen int) {
		if maxLen < 0 || maxLen > 1<<16 {
			t.Skip()
		}
		if err := checkKeyField("id", s, maxLen); err != nil {
			return
		}
		if len(s) > maxLen {
			t.Fatalf("accepted len=%d > maxLen=%d: %q", len(s), maxLen, s)
		}
		if s != strings.TrimSpace(s) {
			t.Fatalf("accepted surrounding whitespace: %q", s)
		}
		for i := 0; i < len(s); i++ {
			c := s[i]
			if c < 0x20 || c == 0x7f {
				t.Fatalf("accepted control byte %#x at %d: %q", c, i, s)
			}
		}
	})
}

// FuzzNormalizeHours: every accepted value must be non-negative AND safe to
// multiply by μs/hour without int64 overflow. The overflow guard was added
// after a code review — this target pins the invariant.
func FuzzNormalizeHours(f *testing.F) {
	f.Add("")
	f.Add("0")
	f.Add("24")
	f.Add("-1")
	f.Add("xx")
	f.Add("9223372036854775807")
	f.Fuzz(func(t *testing.T, s string) {
		n, err := normalizeHours(s)
		if err != nil {
			return
		}
		if n < 0 {
			t.Fatalf("accepted negative: %q -> %d", s, n)
		}
		const usPerHour = int64(time.Hour / time.Microsecond)
		if n > math.MaxInt64/usPerHour {
			t.Fatalf("accepted overflow-prone value: %q -> %d", s, n)
		}
	})
}

// FuzzValidateWriteItem: integration fuzzer across the full write validation
// path. Invariants cover scope/id shape, payload presence, and per-item size.
func FuzzValidateWriteItem(f *testing.F) {
	f.Add("s", "", []byte(`{}`))
	f.Add("s", "a", []byte(`"str"`))
	f.Add("", "", []byte(`{}`))
	f.Add("s", "\x01", []byte(`{}`))
	f.Add("s", "", []byte(``))
	f.Add("s", "", []byte(`null`))
	f.Add("s", "", []byte(`   null   `))
	f.Fuzz(func(t *testing.T, scope, id string, payload []byte) {
		item := Item{Scope: scope, ID: id, Payload: json.RawMessage(payload)}
		if err := validateWriteItem(&item, "/append", MaxItemBytes); err != nil {
			return
		}
		if scope == "" {
			t.Fatalf("accepted empty scope")
		}
		if len(scope) > MaxScopeBytes {
			t.Fatalf("accepted scope len=%d > cap", len(scope))
		}
		if len(id) > MaxIDBytes {
			t.Fatalf("accepted id len=%d > cap", len(id))
		}
		if len(payload) == 0 {
			t.Fatalf("accepted empty payload")
		}
		if bytes.Equal(bytes.TrimSpace(payload), []byte("null")) {
			t.Fatalf("accepted literal null payload: %q", payload)
		}
		if !json.Valid(payload) {
			t.Fatalf("accepted malformed JSON payload: %q", payload)
		}
		if approxItemSize(item) > MaxItemBytes {
			t.Fatalf("accepted item size=%d > cap=%d", approxItemSize(item), MaxItemBytes)
		}
	})
}

// FuzzValidateCounterAddRequest: by-value must be non-zero and within the
// documented JS-safe-integer range. Target pins both bounds.
func FuzzValidateCounterAddRequest(f *testing.F) {
	f.Add("s", "id", int64(1))
	f.Add("s", "id", int64(-1))
	f.Add("s", "id", int64(0))
	f.Add("", "id", int64(1))
	f.Add("s", "", int64(1))
	f.Add("s", "id", MaxCounterValue)
	f.Add("s", "id", MaxCounterValue+1)
	f.Add("s", "id", -MaxCounterValue-1)
	f.Fuzz(func(t *testing.T, scope, id string, by int64) {
		req := counterAddRequest{Scope: scope, ID: id, By: &by}
		// 0 disables the per-item-cap check — fuzz the shape rules
		// only; cap arithmetic is covered by FuzzValidateWriteItem
		// and the dedicated cap-bypass regression tests.
		got, err := validateCounterAddRequest(req, 0)
		if err != nil {
			return
		}
		if got != by {
			t.Fatalf("parsed by=%d != input by=%d", got, by)
		}
		if by == 0 {
			t.Fatalf("accepted by=0")
		}
		if by > MaxCounterValue || by < -MaxCounterValue {
			t.Fatalf("accepted out-of-range by=%d", by)
		}
		if scope == "" {
			t.Fatalf("accepted empty scope")
		}
		if id == "" {
			t.Fatalf("accepted empty id")
		}
	})
}
