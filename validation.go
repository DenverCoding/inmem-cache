package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

// checkKeyField enforces the shape rules for scope/id strings:
// length cap, no surrounding whitespace, no embedded control characters.
// The transport layer does not permit NUL or control bytes in URL/JSON
// identifiers cleanly; rejecting them here avoids log/URL poisoning and
// keeps scope/id safe to splice into diagnostic output.
func checkKeyField(fieldName, value string, maxLen int) error {
	if len(value) > maxLen {
		return errors.New("the '" + fieldName + "' field must not exceed " + strconv.Itoa(maxLen) + " bytes")
	}
	if value != strings.TrimSpace(value) {
		return errors.New("the '" + fieldName + "' field must not have leading or trailing whitespace")
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
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

func checkItemSize(item Item) error {
	if size := approxItemSize(item); size > MaxItemBytes {
		return errors.New("the item's approximate size (" + strconv.FormatInt(size, 10) +
			" bytes) exceeds the maximum of " + strconv.Itoa(MaxItemBytes) + " bytes")
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

	return n, nil
}

func validateWriteItem(item Item, endpoint string) error {
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
	return checkItemSize(item)
}

func validateUpdateItem(item Item) error {
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
	return checkItemSize(item)
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
