package scopecache

import (
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"
)

const (
	DefaultLimit      = 1000    // read response size when client omits ?limit
	MaxLimit          = 10000   // hard ceiling on ?limit; higher values are clamped, not rejected
	ScopeMaxItems     = 100000  // per-scope capacity default; writes that would exceed this are rejected (507). Overridable via SCOPECACHE_SCOPE_MAX_ITEMS
	MaxStoreMiB       = 100     // store-wide aggregate approxItemSize default in MiB; writes past this are rejected (507). Tuned for ~1 GB VPS footprints. Overridable via SCOPECACHE_MAX_STORE_MB
	MaxItemBytes      = 1 << 20 // per-item cap default in bytes on approxItemSize (overhead + scope + id + payload). Overridable via SCOPECACHE_MAX_ITEM_MB (integer MiB)
	MaxResponseMiB    = 25      // per-response cap default in MiB; applies to read endpoints whose response can grow with limit × per-item-cap (/tail, /head, /ts_range). Overridable via SCOPECACHE_MAX_RESPONSE_MB
	MaxMultiCallMiB   = 16      // per-request body cap default for /multi_call in MiB. Overridable via SCOPECACHE_MAX_MULTI_CALL_MB
	MaxMultiCallCount = 10      // max sub-calls per /multi_call batch by default. Overridable via SCOPECACHE_MAX_MULTI_CALL_COUNT
	// MaxInboxKiB is the per-call payload cap default for /inbox in KiB.
	// Tighter than the generic per-item cap on purpose: /inbox is
	// fire-and-forget tenant ingestion (no read-back, drained by the
	// operator in batches), so a single rogue tenant pushing 1 MiB items
	// fills the operator's /admin /tail response cap fast. 64 KiB
	// comfortably fits rich event payloads (signups with form data,
	// audit events with context, nested notifications) while rejecting
	// accidental blob uploads. Overridable via SCOPECACHE_MAX_INBOX_KB
	// (integer KiB — KiB-granular because the meaningful range is
	// sub-MiB; all other byte-knobs are MiB).
	MaxInboxKiB   = 64
	MaxScopeBytes = 256
	MaxIDBytes    = 256

	// SingleRequestBytesOverhead is the headroom added on top of the configured
	// per-item cap to produce the request body cap for single-item endpoints
	// (/append, /update, /upsert, /delete, /delete_scope, /delete_up_to,
	// /counter_add). Covers JSON framing — keys ("scope","id","payload"),
	// quotes, colons, braces — on top of the item payload. The scope and id
	// bytes themselves are already counted inside approxItemSize, so the
	// framing overhead is tiny and constant. 4 KiB leaves generous slack.
	SingleRequestBytesOverhead = 4096

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

// Config bundles the cache-internal capacity knobs into one value that
// every adapter (standalone, Caddy module) fills in its own way — env vars
// for the standalone binary, JSON config for the Caddy module — and hands
// to NewStore. Keeping the shape in one place means new cache knobs land
// in a single file instead of rippling through every adapter's call site.
//
// HTTP/transport-layer knobs (response sizing, /multi_call composition,
// /guarded auth, /inbox allowlist, /admin gate, read-heat opt-out) live
// on APIConfig in api.go and are passed to NewAPI separately. The split
// matches the boundary rule in CLAUDE.md: the core stays
// transport-agnostic; HTTP concerns are an adapter-layer concept.
//
// Fields:
//   - ScopeMaxItems: per-scope item cap; 507 on overflow. Default ScopeMaxItems (100_000).
//   - MaxStoreBytes: aggregate approxItemSize cap, in bytes. Default MaxStoreMiB << 20.
//   - MaxItemBytes:  per-item approxItemSize cap, in bytes. Default MaxItemBytes (1 MiB).
//
// Bytes (not MiB) are the core unit because admission control arithmetic
// lives in bytes; adapters convert their MiB-facing configuration at the
// boundary.
type Config struct {
	ScopeMaxItems int
	MaxStoreBytes int64
	MaxItemBytes  int64
}

// WithDefaults returns a copy of c with non-positive numeric fields
// replaced by the package-level compile-time defaults. NewStore calls
// this internally so library users can pass a partially-filled Config
// (or `Config{}` for "all defaults") and still get a working Store —
// without it, every cap would be zero and every positive write would
// be rejected with 507.
func (c Config) WithDefaults() Config {
	if c.ScopeMaxItems <= 0 {
		c.ScopeMaxItems = ScopeMaxItems
	}
	if c.MaxStoreBytes <= 0 {
		c.MaxStoreBytes = int64(MaxStoreMiB) << 20
	}
	if c.MaxItemBytes <= 0 {
		c.MaxItemBytes = int64(MaxItemBytes)
	}
	return c
}

// MB is an int64 byte count that serializes to JSON as a number in MiB with
// 4 decimals (e.g. 79845 bytes → 0.0762). One display unit across every
// user-facing surface (/stats, /delete_scope_candidates, 507 responses) keeps clients from
// juggling units. The underlying byte value is preserved for arithmetic.
type MB int64

func (m MB) MarshalJSON() ([]byte, error) {
	return []byte(fmt.Sprintf("%.4f", float64(m)/1048576.0)), nil
}

// Payload is opaque application data. json.RawMessage defers decoding and
// keeps the raw bytes as the client sent them, which both honors the
// "cache must not inspect payload" contract and avoids a recursive walk
// every time we need to estimate an item's size.
//
// Ts is an optional, client-supplied millisecond timestamp. The cache never
// generates or mutates it on its own; its only server-side use is filtering
// inside /ts_range. A pointer (not a plain int64) distinguishes "absent"
// from the legitimate value 0 (unix epoch). Write-path semantics:
//   - /append, /warm, /rebuild, /upsert: stored exactly as sent (absent → nil)
//   - /update: absent → preserve existing, present → overwrite
type Item struct {
	Scope   string          `json:"scope,omitempty"`
	ID      string          `json:"id,omitempty"`
	Seq     uint64          `json:"seq,omitempty"`
	Ts      *int64          `json:"ts,omitempty"`
	Payload json.RawMessage `json:"payload"`

	// renderBytes is the JSON-string-decoded form of Payload, populated
	// at write-time (appendItem / upsertByID / updateByID /
	// counterAdd / buildReplacementState) for payloads whose first
	// non-whitespace byte is `"`. Nil for any other payload kind
	// (object/array/number/bool/null), in which case /render writes
	// Payload as-is. This trades a one-shot Unmarshal + alloc at write
	// for a per-hit Unmarshal + []byte cast at read on the /render
	// path — measured ~2.4× faster /render on JSON-string payloads.
	//
	// Unexported, so encoding/json never marshals it: it is in-process
	// state, not part of the wire format that /append, /get, /head,
	// /tail and friends serialise.
	renderBytes []byte
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

// ScopeReadHeatBucket is one slot in the per-scope rolling-7-day read-
// heat ring buffer. Day is the unix-day this slot is currently
// representing (0 = empty); Count is the number of reads recorded on
// that day. Both fields are atomic so the read-hot path
// (ScopeBuffer.recordRead) can update them without taking the scope
// write lock — see the lock-free state machine in recordRead for the
// claim/expire protocol.
type ScopeReadHeatBucket struct {
	Day   atomic.Int64
	Count atomic.Uint64
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

// ScopeDetachedError is returned by a scope-buffer write method when the
// buffer has been unlinked from its Store (by /delete_scope, /wipe, or
// /rebuild) between the handler's getScope/getOrCreateScope call and the
// buffer-level mutation. The write would otherwise succeed into an orphan
// buffer that no subsequent reader can reach, so the caller is told the
// write did not take effect. The handler converts this to 409 Conflict.
type ScopeDetachedError struct{}

func (e *ScopeDetachedError) Error() string {
	return "the scope was deleted while the request was in flight; please retry"
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
	n += 8 // Ts pointer slot; the pointee (8 more bytes) when set is noise at this granularity
	n += int64(len(item.Payload))
	// renderBytes is heap-resident only for JSON-string payloads
	// (precomputed at write time so /render skips a per-hit
	// json.Unmarshal). Count it against the cap so approx_store_mb
	// reflects the real memory budget and a cache full of HTML
	// pages 507's at the right point — without this, a string-
	// payload-heavy store reports ~half the memory it actually uses.
	n += int64(len(item.renderBytes))
	return n
}

// Multi-item read pre-flight constants. Used to short-circuit /head,
// /tail, and /ts_range with a 507 BEFORE writeJSONWithMeta calls
// json.Marshal on the full payload — the post-flight cappedResponseWriter
// catches the same case but only after the body has been built in
// memory. Without pre-flight, an operator who raises
// SCOPECACHE_MAX_RESPONSE_MB to a large value (or a misbehaving client
// hitting /tail?limit=10000 against 1 MiB items) lets the marshaller
// allocate the full body before the cap fires.
//
// Both constants are STRICT lower bounds — overestimating per-item or
// envelope cost would reject legitimate calls. The post-flight
// cappedResponseWriter remains the authoritative cap; pre-flight is
// a memory optimisation, not a correctness gate.
const (
	// multiItemEnvelopeMinBytes is the minimum byte cost of the outer
	// JSON envelope ({ok, hit, count, truncated, items, duration_us,
	// approx_response_mb}). 80 bytes is below the smallest possible
	// envelope encoding for any of the three handlers, even when count
	// is single-digit and the float fields are at minimum width.
	multiItemEnvelopeMinBytes = 80
	// multiItemPerItemMinBytes is the minimum on-wire cost of one
	// item's JSON skeleton (keys, quotes, separators, seq digits).
	// Even a stripped-down `{"scope":"","seq":1,"payload":null}` is
	// 35 bytes; 25 is below that floor and below every realistic
	// item produced by the cache.
	multiItemPerItemMinBytes = 25
)

// estimateMultiItemResponseBytes returns a strict lower bound on the
// JSON-marshalled response size for a /head, /tail, or /ts_range
// payload with the given items. Used by the pre-flight check; see
// multiItemEnvelopeMinBytes for the rationale.
//
// Counts only bytes that MUST appear on the wire: the envelope
// minimum, a fixed per-item skeleton cost, and the scope/id/payload
// values written verbatim (JSON-string escaping only ADDS bytes
// during marshal, never removes — so raw len() is an underestimate
// of the encoded form, which is what we need).
func estimateMultiItemResponseBytes(items []Item) int64 {
	n := int64(multiItemEnvelopeMinBytes) + int64(len(items))*multiItemPerItemMinBytes
	for i := range items {
		n += int64(len(items[i].Scope))
		n += int64(len(items[i].ID))
		n += int64(len(items[i].Payload))
	}
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

// singleRequestBytesFor returns the per-request body cap for single-item
// endpoints, derived from the configured per-item cap. The item cap is a
// semantic limit on approxItemSize (enforced in the validator); this request
// cap is a DoS guardrail on the raw HTTP body (enforced by MaxBytesReader).
// The 4 KiB overhead covers JSON framing (keys, quotes, braces) on top of the
// item bytes — scope and id are already counted inside approxItemSize, so the
// framing is tiny and constant.
func singleRequestBytesFor(maxItemBytes int64) int64 {
	return maxItemBytes + SingleRequestBytesOverhead
}
