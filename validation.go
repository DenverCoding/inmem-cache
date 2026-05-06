package scopecache

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// ErrInvalidInput is the sentinel that every top-level validator wraps
// its return error with. Lets HTTP handlers distinguish a validation
// failure (→ 400 Bad Request) from a conflict (→ 409, e.g. duplicate
// id) or a capacity error (→ 507) without per-validator type
// inspection.
//
// Validators wrap via `fmt.Errorf("%w: <reason>", ErrInvalidInput)` so
// errors.Is(err, ErrInvalidInput) on the handler side suffices. The
// underlying reason string is preserved in the wrapped error for
// human-readable diagnostic output (badRequest writes the full chain
// to the response body).
var ErrInvalidInput = errors.New("scopecache: invalid input")

// wrapValidation tags a non-nil error as a validation failure by
// wrapping it with ErrInvalidInput. Top-level validators use it via a
// deferred call so every return path picks up the wrap automatically;
// see validateWriteItem etc. for the pattern.
func wrapValidation(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrInvalidInput, err.Error())
}

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

// validateIDOrSeq enforces the "exactly one of id or seq" addressing
// contract used by /update and /delete: passing neither (id=="" &&
// seq==0) or both (id!="" && seq!=0) is rejected with the same
// endpoint-aware message. When an id is supplied, its shape is
// validated via checkKeyField; seq has no shape to validate beyond
// the non-zero check.
//
// Centralises the pattern that previously lived parallel in
// validateUpdateItem and validateDeleteRequest. Other endpoints take
// a plain `Item` whose seq is pre-rejected (writeItem / upsert) and
// don't need this gate. Reads (GetByID / GetBySeq) split the address
// shape at the Gateway boundary so the helper is unused there.
func validateIDOrSeq(endpoint, id string, seq uint64) error {
	hasID := id != ""
	hasSeq := seq != 0
	if hasID == hasSeq {
		return errors.New("exactly one of 'id' or 'seq' must be provided for the '" + endpoint + "' endpoint")
	}
	if hasID {
		return checkKeyField("id", id, MaxIDBytes)
	}
	return nil
}

// validatePayload enforces the two-part payload shape contract from RFC
// §4.1: payload must be present (not missing, not literal `null`) AND
// must be a syntactically valid JSON value. Both cases produce a 400
// Bad Request via the validator's wrapValidation defer.
//
// Why both checks live here:
//
//   - Missing / null → RawMessage is nil/empty, or the bytes "null"
//     (possibly with whitespace). Treated as "no payload" and rejected
//     with a clear error so callers don't conflate "field absent" with
//     "field set to JSON null".
//   - Malformed JSON → the bytes are not a valid JSON encoding. The
//     HTTP path's encoding/json decode would already fail this case
//     during the structural scan that populates RawMessage, but a
//     direct Gateway caller (Append/Upsert/Update/Warm/Rebuild) hands
//     the slice in as-is. Without an explicit json.Valid check the
//     invalid bytes would be stored opaquely and re-served by /get,
//     /head, /tail, /render and `_events` envelopes, breaking any
//     downstream consumer that json.Unmarshals.
//
// Cost: one O(n) scan per write. ~30 ns on small payloads, ~5 µs on a
// 1 MiB payload — well below the HTTP path's per-request overhead and
// the existing precomputeRenderBytes / approxItemSize passes the
// validator already runs.
func validatePayload(p json.RawMessage) error {
	if len(p) == 0 || bytes.Equal(bytes.TrimSpace(p), []byte("null")) {
		return errors.New("the 'payload' field is required")
	}
	if !json.Valid(p) {
		return errors.New("the 'payload' field must be a valid JSON value")
	}
	return nil
}

// checkItemSize must measure what the write path will actually store, not
// the raw decoded request. JSON-string payloads carry a precomputed
// renderBytes shortcut (decoded form, populated at write time so /render
// skips a per-hit Unmarshal); approxItemSize counts those bytes too.
//
// The validator owns the renderBytes assignment for write paths: it
// computes the decoded form once here, stores it on the item, and the
// downstream buffer-write paths (insertNewItemLocked, upsertByID hit
// branch, updateByID/updateBySeq, buildReplacementState) reuse it
// instead of recomputing. Without the carry-through a JSON-string
// payload would be json.Unmarshal'd twice per write (once for the
// size check, once for storage), wasting CPU and one allocation
// proportional to the decoded length.
//
// PRECONDITION: item must already have passed validatePayload — the
// JSON validity invariant lets precomputeRenderBytes treat the input
// as well-formed JSON.
func checkItemSize(item *Item, maxItemBytes int64) error {
	if item.renderBytes == nil {
		item.renderBytes = precomputeRenderBytes(item.Payload)
	}
	size := approxItemSize(*item)
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

func validateWriteItem(item *Item, endpoint string, maxItemBytes int64) (returnErr error) {
	defer func() { returnErr = wrapValidation(returnErr) }()
	if err := validateScope(item.Scope, endpoint); err != nil {
		return err
	}
	if err := validateID(item.ID); err != nil {
		return err
	}
	if err := validatePayload(item.Payload); err != nil {
		return err
	}
	if item.Seq != 0 {
		return errors.New("the 'seq' field is managed by the cache and must not be provided to the '" + endpoint + "' endpoint")
	}
	if err := rejectClientTs(*item, endpoint); err != nil {
		return err
	}
	return checkItemSize(item, maxItemBytes)
}

func validateUpsertItem(item *Item, maxItemBytes int64) (returnErr error) {
	defer func() { returnErr = wrapValidation(returnErr) }()
	if err := validateScope(item.Scope, "/upsert"); err != nil {
		return err
	}
	if isReservedScope(item.Scope) {
		return errors.New("scope '" + item.Scope + "' is reserved; in-place mutation (/upsert) is not supported on the drain-stream scopes (use /append)")
	}
	if err := requireID(item.ID, "/upsert"); err != nil {
		return err
	}
	if err := validatePayload(item.Payload); err != nil {
		return err
	}
	if item.Seq != 0 {
		return errors.New("the 'seq' field is managed by the cache and must not be provided to the '/upsert' endpoint")
	}
	if err := rejectClientTs(*item, "/upsert"); err != nil {
		return err
	}
	return checkItemSize(item, maxItemBytes)
}

func validateUpdateItem(item *Item, maxItemBytes int64) (returnErr error) {
	defer func() { returnErr = wrapValidation(returnErr) }()
	if err := validateScope(item.Scope, "/update"); err != nil {
		return err
	}
	if isReservedScope(item.Scope) {
		return errors.New("scope '" + item.Scope + "' is reserved; in-place mutation (/update) is not supported on the drain-stream scopes")
	}
	if err := validateIDOrSeq("/update", item.ID, item.Seq); err != nil {
		return err
	}
	if err := validatePayload(item.Payload); err != nil {
		return err
	}
	if err := rejectClientTs(*item, "/update"); err != nil {
		return err
	}
	return checkItemSize(item, maxItemBytes)
}

// validateCounterAddRequest returns the parsed `by` on success so the handler
// can pass it straight to the store without re-dereferencing the pointer.
// Missing `by` is distinguished from an explicit zero by the pointer type.
//
// maxItemBytes is the per-item cap that applies to the resulting counter
// item. Counter items have a fully-determined size (48 fixed overhead +
// len(scope) + len(id) + counterCellOverhead) — no payload-size variance —
// so the candidate is checkable up-front, mirroring /append's checkItemSize
// gate. Without this check the create and promote paths in counterAddSlow
// silently commit counter items past MaxItemBytes whenever scope+id+56
// exceeds the cap (and on small caps, that's any scope+id at all).
func validateCounterAddRequest(req counterAddRequest, maxItemBytes int64) (by int64, returnErr error) {
	defer func() { returnErr = wrapValidation(returnErr) }()
	if err := validateScope(req.Scope, "/counter_add"); err != nil {
		return 0, err
	}
	if isReservedScope(req.Scope) {
		return 0, errors.New("scope '" + req.Scope + "' is reserved; counters are not supported on the drain-stream scopes")
	}
	if err := requireID(req.ID, "/counter_add"); err != nil {
		return 0, err
	}
	if req.By == nil {
		return 0, errors.New("the 'by' field is required for the '/counter_add' endpoint")
	}
	by = *req.By
	if by == 0 {
		return 0, errors.New("the 'by' field must not be zero")
	}
	if by > MaxCounterValue || by < -MaxCounterValue {
		return 0, errors.New("the 'by' field must be within ±(2^53-1)")
	}
	// Per-item cap pre-flight on the candidate counter shape. The
	// candidate carries a non-nil counter marker so approxItemSize
	// charges counterCellOverhead in place of len(Payload) +
	// len(renderBytes); Payload itself stays nil because counter items
	// never store payload bytes (readers materialise from cell.value
	// at the boundary). Sentinel maxItemBytes <= 0 disables the
	// check — fuzz callers pass 0 to exercise the shape rules
	// without provisioning a realistic per-item budget.
	if maxItemBytes > 0 {
		candidate := Item{Scope: req.Scope, ID: req.ID, counter: &counterCell{}}
		if size := approxItemSize(candidate); size > maxItemBytes {
			return 0, fmt.Errorf("the counter item's approximate size (%d bytes) exceeds the maximum of %d bytes", size, maxItemBytes)
		}
	}
	return by, nil
}

func validateDeleteRequest(req deleteRequest) (returnErr error) {
	defer func() { returnErr = wrapValidation(returnErr) }()
	if err := validateScope(req.Scope, "/delete"); err != nil {
		return err
	}
	return validateIDOrSeq("/delete", req.ID, req.Seq)
}

func validateDeleteScopeRequest(req deleteScopeRequest) (returnErr error) {
	defer func() { returnErr = wrapValidation(returnErr) }()
	if err := validateScope(req.Scope, "/delete_scope"); err != nil {
		return err
	}
	if isReservedScope(req.Scope) {
		return errors.New("scope '" + req.Scope + "' is reserved and cannot be deleted")
	}
	return nil
}

func validateDeleteUpToRequest(req deleteUpToRequest) (returnErr error) {
	defer func() { returnErr = wrapValidation(returnErr) }()
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
