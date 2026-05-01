package scopecache

import (
	"bytes"
	"encoding/json"
	"strings"
)

// stripGuardedPrefix walks a sub-call response body and removes the
// `_guarded:<capabilityID>:` prefix from every recognised scope field.
// Operates on the response shapes produced by the endpoints in
// /guarded's whitelist:
//
//   - /get, /append, /upsert: body.item.scope
//   - /head, /tail, /ts_range: body.items[].scope
//
// Other endpoints (/update, /delete, /delete_up_to, /counter_add)
// don't carry a scope field in their response, so the body passes
// through unchanged.
//
// Implementation note: parses the body to a typed shape rather than
// string-replacing on raw bytes — payloads stored at the rewritten
// scope can coincidentally contain the prefix string. JSON-aware
// stripping is the only safe approach. See guardedflow.md §G.
//
// On any parse failure (non-JSON body, malformed structure), the
// original bytes are returned unchanged. Worst case is a slightly
// leaky response; better than corrupting client output.
func stripGuardedPrefix(body []byte, prefix string) []byte {
	body = bytes.TrimSpace(body)
	if len(body) == 0 || !looksLikeJSONObject(body) {
		return body
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}

	mutated := false

	// Single-item shape: body.item.scope (used by /get, /append, /upsert).
	if itemRaw, ok := m["item"]; ok {
		if newItem, changed := stripScopeField(itemRaw, prefix); changed {
			m["item"] = newItem
			mutated = true
		}
	}

	// Multi-item shape: body.items[].scope (used by /head, /tail, /ts_range).
	if itemsRaw, ok := m["items"]; ok {
		if newItems, changed := stripScopeFieldArray(itemsRaw, prefix); changed {
			m["items"] = newItems
			mutated = true
		}
	}

	if !mutated {
		return body
	}

	out, err := json.Marshal(m)
	if err != nil {
		// Marshal of a map[string]json.RawMessage is nearly impossible to
		// fail — but defensively pass through original bytes if it does.
		return body
	}
	return out
}

// stripScopeField rewrites the "scope" field of a single JSON object,
// stripping the prefix if present. Returns (newBytes, changed). On any
// parse failure or absent scope field, returns the input unchanged.
func stripScopeField(raw json.RawMessage, prefix string) (json.RawMessage, bool) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return raw, false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return raw, false
	}
	scopeRaw, ok := obj["scope"]
	if !ok {
		return raw, false
	}
	var scope string
	if err := json.Unmarshal(scopeRaw, &scope); err != nil {
		return raw, false
	}
	if !strings.HasPrefix(scope, prefix) {
		// Defensive: only strip what we put there. Scopes that don't
		// carry the current request's prefix pass through.
		return raw, false
	}
	stripped := scope[len(prefix):]
	newScopeRaw, _ := json.Marshal(stripped)
	obj["scope"] = newScopeRaw

	out, err := json.Marshal(obj)
	if err != nil {
		return raw, false
	}
	return out, true
}

// stripScopeFieldArray walks a JSON array of objects and strips the
// prefix from each element's scope field. Returns (newBytes, changed).
func stripScopeFieldArray(raw json.RawMessage, prefix string) (json.RawMessage, bool) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return raw, false
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return raw, false
	}
	if len(arr) == 0 {
		return raw, false
	}

	mutated := false
	for i := range arr {
		if newItem, changed := stripScopeField(arr[i], prefix); changed {
			arr[i] = newItem
			mutated = true
		}
	}

	if !mutated {
		return raw, false
	}
	out, err := json.Marshal(arr)
	if err != nil {
		return raw, false
	}
	return out, true
}

// looksLikeJSONObject is a fast check before attempting full unmarshal
// — avoids building a map on non-object payloads that pass through
// unchanged.
func looksLikeJSONObject(b []byte) bool {
	for _, c := range b {
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			continue
		}
		return c == '{'
	}
	return false
}
