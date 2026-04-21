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

func checkItemSize(item Item, maxItemBytes int64) error {
	if size := approxItemSize(item); size > maxItemBytes {
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

// parseTsParam decodes a since_ts/until_ts query string as a signed int64
// (milliseconds since unix epoch, by convention; the cache is opaque to the
// unit). An empty raw value returns (nil, nil) so callers can distinguish
// "absent" from "present but zero". Any value that does not round-trip as an
// int64 fails validation rather than silently clamping.
func parseTsParam(name, raw string) (*int64, error) {
	if raw == "" {
		return nil, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return nil, errors.New("the '" + name + "' parameter must be a valid int64 (milliseconds since unix epoch)")
	}
	return &v, nil
}

// validateTsRangeParams enforces the shape rules for /ts_range's time window:
// at least one bound must be present, and the window must be non-inverted.
// Bounds are inclusive on both ends (SQL BETWEEN convention).
func validateTsRangeParams(sinceTs, untilTs *int64) error {
	if sinceTs == nil && untilTs == nil {
		return errors.New("at least one of 'since_ts' or 'until_ts' must be provided for the '/ts_range' endpoint")
	}
	if sinceTs != nil && untilTs != nil && *sinceTs > *untilTs {
		return errors.New("the 'since_ts' must not be greater than 'until_ts'")
	}
	return nil
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
	return validateScope(req.Scope, "/delete-scope")
}

func validateDeleteUpToRequest(req DeleteUpToRequest) error {
	if err := validateScope(req.Scope, "/delete-up-to"); err != nil {
		return err
	}
	if req.MaxSeq == 0 {
		return errors.New("the 'max_seq' field is required and must be a positive integer for the '/delete-up-to' endpoint")
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
