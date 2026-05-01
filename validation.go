package scopecache

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"
)

// checkKeyField enforces the shape rules for scope/id strings:
// length cap, no surrounding whitespace, no embedded control characters.
// The transport layer does not permit NUL or control bytes in URL/JSON
// identifiers cleanly; rejecting them here avoids log/URL poisoning and
// keeps scope/id safe to splice into diagnostic output.
//
// The control-char check iterates bytes, not runes. Range-over-string would
// yield utf8.RuneError (0xFFFD) for malformed UTF-8, which is >0x7f and
// would pass the check even though a raw 0x00..0x1f byte was present.
// Byte iteration catches those regardless of UTF-8 validity; high bytes
// (0x80+) are left alone so valid multi-byte UTF-8 passes through.
func checkKeyField(fieldName, value string, maxLen int) error {
	if len(value) > maxLen {
		return errors.New("the '" + fieldName + "' field must not exceed " + strconv.Itoa(maxLen) + " bytes")
	}
	if value != strings.TrimSpace(value) {
		return errors.New("the '" + fieldName + "' field must not have leading or trailing whitespace")
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c < 0x20 || c == 0x7f {
			return errors.New("the '" + fieldName + "' field must not contain control characters")
		}
	}
	return nil
}

func validateScope(scope, endpoint string) error {
	if scope == "" {
		return errors.New("the 'scope' field is required for the '" + endpoint + "' endpoint")
	}
	return checkKeyField("scope", scope, MaxScopeBytes)
}

// hasReservedPrefix returns true when scope begins with '_', the reserved
// prefix used internally by /guarded (`_guarded:<HMAC>:*`) and the
// cache's own counter scopes (`_counters_*`). Public endpoints reject
// reserved scopes via rejectReservedScope; only /admin (raw access) and
// /guarded (after rewrite) reach them. See guardedflow.md §B.
func hasReservedPrefix(scope string) bool {
	return len(scope) > 0 && scope[0] == '_'
}

// validateID validates an id when one is provided. An empty id is legal
// (id is optional on writes); callers that require an id should use
// requireID instead.
func validateID(id string) error {
	if id == "" {
		return nil
	}
	return checkKeyField("id", id, MaxIDBytes)
}

func requireID(id, endpoint string) error {
	if id == "" {
		return errors.New("the 'id' field is required for the '" + endpoint + "' endpoint")
	}
	return checkKeyField("id", id, MaxIDBytes)
}

// payloadPresent is the single gate for "has the client actually supplied a
// payload?" Missing field → RawMessage is nil/empty. Explicit `null` → raw
// bytes "null". Both mean "no payload" and are rejected; every other JSON
// value (object, array, string, number, bool) is treated as opaque data.
func payloadPresent(p json.RawMessage) bool {
	if len(p) == 0 {
		return false
	}
	return !bytes.Equal(bytes.TrimSpace(p), []byte("null"))
}

// checkItemSize must measure what the write path will actually store, not
// the raw decoded request. JSON-string payloads carry a precomputed
// renderBytes shortcut (decoded form, populated at write time so /render
// skips a per-hit Unmarshal); approxItemSize counts those bytes too. The
// validator runs before the buffer-write path has filled renderBytes, so
// for string payloads we precompute it here and add its length. Without
// this, a 1 MiB JSON-string payload passes the per-item cap on raw bytes
// but is stored at ~2× the cap once renderBytes is materialised — the
// store-wide cap still catches aggregate, but the per-item cap silently
// admits items that violate the documented invariant (rfc §3).
//
// The duplicate precompute (here + buffer write path) is bounded by the
// per-item cap (default 1 MiB) and only fires for string payloads, so
// the cost is acceptable; the buffer paths are not changed because they
// own the canonical renderBytes assignment under b.mu.
func checkItemSize(item Item, maxItemBytes int64) error {
	size := approxItemSize(item)
	if item.renderBytes == nil {
		size += int64(len(precomputeRenderBytes(item.Payload)))
	}
	if size > maxItemBytes {
		return errors.New("the item's approximate size (" + strconv.FormatInt(size, 10) +
			" bytes) exceeds the maximum of " + strconv.FormatInt(maxItemBytes, 10) + " bytes")
	}
	return nil
}

func normalizeLimit(raw string) (int, error) {
	if raw == "" {
		return DefaultLimit, nil
	}

	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, errors.New("the 'limit' parameter must be a positive integer")
	}

	if n > MaxLimit {
		return MaxLimit, nil
	}

	return n, nil
}

func normalizeOffset(raw string) (int, error) {
	if raw == "" {
		return 0, nil
	}

	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, errors.New("the 'offset' parameter must be a non-negative integer")
	}

	return n, nil
}

func normalizeHours(raw string) (int64, error) {
	if raw == "" {
		return 0, nil
	}

	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, errors.New("the 'hours' parameter must be a non-negative integer")
	}

	// The caller multiplies hours by μs/hour (3.6e9). Values above this
	// threshold would overflow int64 during that multiplication — the
	// practical ceiling is still ~2.9 million years, far beyond any
	// sensible age filter.
	const usPerHour = int64(time.Hour / time.Microsecond)
	if n > math.MaxInt64/usPerHour {
		return 0, errors.New("the 'hours' parameter is unreasonably large")
	}

	return n, nil
}

// rejectClientTs catches a non-zero Ts on any write request body. Ts is
// cache-owned (assigned on every write that touches an item); a client
// supplying it is almost always a confused integration that expects the
// pre-v0.7.12 "client may set ts" semantics. Returning a clear 400 here
// is more useful than silently overwriting. ts=0 is the JSON-decode
// default for "field absent" and passes through — a client that
// explicitly sends ts=0 (1970-01-01) gets the same treatment as omitting
// the field; the buffer stamps now() either way.
func rejectClientTs(item Item, endpoint string) error {
	if item.Ts != 0 {
		return errors.New("the 'ts' field is managed by the cache and must not be provided to the '" + endpoint + "' endpoint")
	}
	return nil
}

func validateWriteItem(item Item, endpoint string, maxItemBytes int64) error {
	if err := validateScope(item.Scope, endpoint); err != nil {
		return err
	}
	if err := validateID(item.ID); err != nil {
		return err
	}
	if !payloadPresent(item.Payload) {
		return errors.New("the 'payload' field is required")
	}
	if item.Seq != 0 {
		return errors.New("the 'seq' field is managed by the cache and must not be provided to the '" + endpoint + "' endpoint")
	}
	if err := rejectClientTs(item, endpoint); err != nil {
		return err
	}
	return checkItemSize(item, maxItemBytes)
}

func validateUpsertItem(item Item, maxItemBytes int64) error {
	if err := validateScope(item.Scope, "/upsert"); err != nil {
		return err
	}
	if err := requireID(item.ID, "/upsert"); err != nil {
		return err
	}
	if !payloadPresent(item.Payload) {
		return errors.New("the 'payload' field is required")
	}
	if item.Seq != 0 {
		return errors.New("the 'seq' field is managed by the cache and must not be provided to the '/upsert' endpoint")
	}
	if err := rejectClientTs(item, "/upsert"); err != nil {
		return err
	}
	return checkItemSize(item, maxItemBytes)
}

func validateUpdateItem(item Item, maxItemBytes int64) error {
	if err := validateScope(item.Scope, "/update"); err != nil {
		return err
	}
	hasID := item.ID != ""
	hasSeq := item.Seq != 0
	if hasID == hasSeq {
		return errors.New("exactly one of 'id' or 'seq' must be provided for the '/update' endpoint")
	}
	if hasID {
		if err := checkKeyField("id", item.ID, MaxIDBytes); err != nil {
			return err
		}
	}
	if !payloadPresent(item.Payload) {
		return errors.New("the 'payload' field is required")
	}
	if err := rejectClientTs(item, "/update"); err != nil {
		return err
	}
	return checkItemSize(item, maxItemBytes)
}

// validateCounterAddRequest returns the parsed `by` on success so the handler
// can pass it straight to the store without re-dereferencing the pointer.
// Missing `by` is distinguished from an explicit zero by the pointer type.
func validateCounterAddRequest(req CounterAddRequest) (int64, error) {
	if err := validateScope(req.Scope, "/counter_add"); err != nil {
		return 0, err
	}
	if err := requireID(req.ID, "/counter_add"); err != nil {
		return 0, err
	}
	if req.By == nil {
		return 0, errors.New("the 'by' field is required for the '/counter_add' endpoint")
	}
	by := *req.By
	if by == 0 {
		return 0, errors.New("the 'by' field must not be zero")
	}
	if by > MaxCounterValue || by < -MaxCounterValue {
		return 0, errors.New("the 'by' field must be within ±(2^53-1)")
	}
	return by, nil
}

func validateDeleteRequest(req DeleteRequest) error {
	if err := validateScope(req.Scope, "/delete"); err != nil {
		return err
	}
	hasID := req.ID != ""
	hasSeq := req.Seq != 0
	if hasID == hasSeq {
		return errors.New("exactly one of 'id' or 'seq' must be provided for the '/delete' endpoint")
	}
	if hasID {
		return checkKeyField("id", req.ID, MaxIDBytes)
	}
	return nil
}

func validateDeleteScopeRequest(req DeleteScopeRequest) error {
	return validateScope(req.Scope, "/delete_scope")
}

func validateDeleteUpToRequest(req DeleteUpToRequest) error {
	if err := validateScope(req.Scope, "/delete_up_to"); err != nil {
		return err
	}
	if req.MaxSeq == 0 {
		return errors.New("the 'max_seq' field is required and must be a positive integer for the '/delete_up_to' endpoint")
	}
	return nil
}

func groupItemsByScope(items []Item) map[string][]Item {
	grouped := make(map[string][]Item)
	for _, item := range items {
		grouped[item.Scope] = append(grouped[item.Scope], item)
	}
	return grouped
}
