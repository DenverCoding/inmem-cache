package scopecache

import (
	"encoding/json"
	"fmt"
	"time"
)

const (
	DefaultLimit       = 1000   // read response size when client omits ?limit
	MaxLimit           = 10000  // hard ceiling on ?limit; higher values are clamped, not rejected
	ScopeMaxItems      = 100000 // per-scope capacity default; writes that would exceed this are rejected (507). Overridable via SCOPECACHE_SCOPE_MAX_ITEMS
	MaxStoreMiB        = 100    // store-wide aggregate approxItemSize default in MiB; writes past this are rejected (507). Tuned for ~1 GB VPS footprints. Overridable via SCOPECACHE_MAX_STORE_MB
	MaxItemBytes       = 1 << 20 // 1 MiB cap on approxItemSize (overhead + scope + id + payload), not on raw payload alone
	MaxScopeBytes      = 128
	MaxIDBytes         = 128
	// Request body cap for single-item endpoints. Sits above MaxItemBytes to
	// allow for JSON framing overhead.
	MaxSingleRequestBytes = 2 << 20 // 2 MiB  — /append, /update, /delete, /delete-scope

	// BulkRequestBytesOverhead is the headroom added on top of the configured
	// store cap to produce the per-request cap for /warm and /rebuild. See
	// bulkRequestBytesFor: a full cache must fit into a single bulk request,
	// plus JSON framing (keys, quotes, separators, wrapper object).
	BulkRequestBytesOverhead = 16 << 20 // 16 MiB

	ReadHeatWindowDays = 7

	// MaxCounterValue is the largest absolute value a /counter_add operation
	// may observe or produce. Matches the JavaScript safe-integer range
	// (2^53 - 1), so counter values marshalled into JSON round-trip through
	// every client without loss of precision. Applies to `by`, the existing
	// counter value, and the result of the addition.
	MaxCounterValue int64 = (1 << 53) - 1 // 9,007,199,254,740,991
)

// MB is an int64 byte count that serializes to JSON as a number in MiB with
// 4 decimals (e.g. 79845 bytes → 0.0762). One display unit across every
// user-facing surface (/stats, /delete-scope-candidates, 507 responses) keeps clients from
// juggling units. The underlying byte value is preserved for arithmetic.
type MB int64

func (m MB) MarshalJSON() ([]byte, error) {
	return []byte(fmt.Sprintf("%.4f", float64(m)/1048576.0)), nil
}

// Payload is opaque application data. json.RawMessage defers decoding and
// keeps the raw bytes as the client sent them, which both honors the
// "cache must not inspect payload" contract and avoids a recursive walk
// every time we need to estimate an item's size.
type Item struct {
	Scope   string          `json:"scope,omitempty"`
	ID      string          `json:"id,omitempty"`
	Seq     uint64          `json:"seq,omitempty"`
	Payload json.RawMessage `json:"payload"`
}

type DeleteRequest struct {
	Scope string `json:"scope"`
	ID    string `json:"id,omitempty"`
	Seq   uint64 `json:"seq,omitempty"`
}

type DeleteScopeRequest struct {
	Scope string `json:"scope"`
}

type DeleteUpToRequest struct {
	Scope  string `json:"scope"`
	MaxSeq uint64 `json:"max_seq"`
}

// CounterAddRequest is the body of the `/counter_add` endpoint. `By` is a
// pointer so the handler can distinguish a missing field from an explicit
// zero — the latter is a client bug and is rejected with 400.
type CounterAddRequest struct {
	Scope string `json:"scope"`
	ID    string `json:"id"`
	By    *int64 `json:"by"`
}

type ItemsRequest struct {
	Items []Item `json:"items"`
}

type ScopeReadHeatBucket struct {
	Day   int64
	Count uint64
}

type Candidate struct {
	Scope           string `json:"scope"`
	CreatedTS       int64  `json:"created_ts"`
	LastAccessTS    int64  `json:"last_access_ts"`
	Last7dReadCount uint64 `json:"last_7d_read_count"`
	ItemCount       int    `json:"item_count"`
	ApproxScopeMB   MB     `json:"approx_scope_mb"`
}

// ScopeCapacityOffender is one entry in a 507 response body: which scope
// overflowed, how many items the request/state held, and the active cap.
type ScopeCapacityOffender struct {
	Scope string `json:"scope"`
	Count int    `json:"count"`
	Cap   int    `json:"cap"`
}

// ScopeFullError is returned by ScopeBuffer.appendItem when the buffer is at
// capacity. The handler converts it to a 507 response with the scope name.
type ScopeFullError struct {
	Count int
	Cap   int
}

func (e *ScopeFullError) Error() string {
	return "scope is at capacity"
}

// ScopeCapacityError is returned by Store.replaceScopes and Store.rebuildAll
// when one or more scopes in a batch would exceed the per-scope cap. The
// batch is rejected as a whole (no partial apply).
type ScopeCapacityError struct {
	Offenders []ScopeCapacityOffender
}

func (e *ScopeCapacityError) Error() string {
	if len(e.Offenders) == 1 {
		o := e.Offenders[0]
		return "scope '" + o.Scope + "' is at capacity"
	}
	return "multiple scopes are at capacity"
}

// StoreFullError is returned when a write would push the store's aggregate
// approxItemSize past the configured byte cap. AddedBytes is the net delta
// the rejected write attempted to commit; it may be larger than the free
// budget even when StoreBytes itself is under the cap (e.g. a /warm that
// replaces a small scope with a large one).
type StoreFullError struct {
	StoreBytes int64
	AddedBytes int64
	Cap        int64
}

func (e *StoreFullError) Error() string {
	return "store is at byte capacity"
}

// CounterPayloadError is returned by ScopeBuffer.counterAdd when the existing
// item at scope+id cannot participate in a counter operation: its payload is
// not a JSON number, not an integer, or outside the allowed ±MaxCounterValue
// range. The handler converts it to 409 Conflict.
type CounterPayloadError struct {
	Reason string
}

func (e *CounterPayloadError) Error() string {
	return e.Reason
}

// CounterOverflowError is returned by ScopeBuffer.counterAdd when the
// resulting value would exceed ±MaxCounterValue. The handler converts it to
// 400 Bad Request — the caller supplied a `by` that combined with the current
// value would push the counter outside the JS-safe integer range.
type CounterOverflowError struct {
	Current int64
	By      int64
}

func (e *CounterOverflowError) Error() string {
	return "the counter operation would exceed the allowed range of ±(2^53-1)"
}

func nowUnixMicro() int64 {
	return time.Now().UnixMicro()
}

func unixDay(tsMicro int64) int64 {
	return tsMicro / 86400000000
}

func approxItemSize(item Item) int64 {
	var n int64
	n += 32
	n += int64(len(item.Scope))
	n += int64(len(item.ID))
	n += 8
	n += int64(len(item.Payload))
	return n
}

// bulkRequestBytesFor returns the per-request body cap for /warm and
// /rebuild, derived from the configured store cap. A full store must always
// fit into a single bulk request; the extra 10% plus BulkRequestBytesOverhead
// covers JSON framing (keys, quotes, array separators, wrapper object) and
// provides a sane floor for very small store caps.
func bulkRequestBytesFor(maxStoreBytes int64) int64 {
	return maxStoreBytes + maxStoreBytes/10 + BulkRequestBytesOverhead
}
