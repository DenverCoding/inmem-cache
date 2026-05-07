// Defensive payload-byte cloning at the Gateway boundary.
//
// Item.Payload is json.RawMessage (= []byte). Without a defensive copy,
// the slice the caller passes to Gateway.Append (or one of the other
// write entry points) and the slice the cache stores share the same
// backing array. A Go caller mutating their payload after the call
// returns would silently mutate the cached item — bypassing every lock
// the cache holds. The same hazard applies in reverse on Items
// returned from Get/Head/Tail/Render.
//
// The HTTP path doesn't have this hazard because
// json.RawMessage.UnmarshalJSON copies its input by stdlib contract,
// so every HTTP-decoded payload is already detached from the request
// body. *API dispatches through *store directly (NewAPI extracts
// gw.store), so HTTP traffic never pays the cloning cost levied here.
//
// Unexported Item fields (renderBytes, counter) ALSO need clearing at
// the boundary, despite being unreachable to outside-package callers
// directly. The hazard is round-tripping: a caller does
//   item, _ := gw.GetByID("scope", "id")  // counter item — pointer rides on the Item
//   item.Scope = "other"; item.Payload = newBytes
//   _, _ = gw.Append(item)                // counter pointer rides through Append
// without the clearing, the cache treats the new item as a counter,
// approxItemSize charges counterCellOverhead instead of len(payload)
// (under-counting MaxItemBytes), and the next read materialises from
// the cell — silently returning the original counter value instead of
// the supplied payload. Output-side clearing alone closes the hazard
// (callers can never have set these fields themselves), but we clear
// on input too as belt-and-braces.

package scopecache

import "encoding/json"

// clonePayload returns a fresh copy of p with a newly allocated
// backing array. nil input → nil output (preserves the "no payload"
// sentinel zero-value Items carry); empty non-nil input → empty
// non-nil output (preserves the distinction observable to callers).
func clonePayload(p json.RawMessage) json.RawMessage {
	if p == nil {
		return nil
	}
	out := make(json.RawMessage, len(p))
	copy(out, p)
	return out
}

// cloneItemPayload returns a copy of item with item.Payload replaced
// by a fresh allocation and the unexported renderBytes + counter
// fields cleared. Other exported fields are value types and don't
// alias caller-side state. Applied symmetrically on input and output
// boundaries — the clearing closes the round-trip hazard.
func cloneItemPayload(item Item) Item {
	item.Payload = clonePayload(item.Payload)
	item.renderBytes = nil
	item.counter = nil
	return item
}

// cloneGroupedItemPayloads returns a deep copy of grouped where every
// Item.Payload has a fresh backing array. Used by Gateway.Warm and
// Gateway.Rebuild so a caller mutating their input map after the call
// returns cannot reach into stored items. The map and slices are
// freshly allocated too so the caller can safely mutate the slice
// headers themselves.
func cloneGroupedItemPayloads(grouped map[string][]Item) map[string][]Item {
	if grouped == nil {
		return nil
	}
	out := make(map[string][]Item, len(grouped))
	for scope, items := range grouped {
		cloned := make([]Item, len(items))
		for i := range items {
			cloned[i] = cloneItemPayload(items[i])
		}
		out[scope] = cloned
	}
	return out
}

// cloneItemsPayloads rewrites every Item.Payload in items with a fresh
// allocation and clears the unexported renderBytes + counter fields.
// Operates in place — the caller owns items (read paths already return
// a fresh slice header, so this never mutates cache storage). Returns
// the same slice for ergonomic chaining.
func cloneItemsPayloads(items []Item) []Item {
	for i := range items {
		items[i].Payload = clonePayload(items[i].Payload)
		items[i].renderBytes = nil
		items[i].counter = nil
	}
	return items
}
