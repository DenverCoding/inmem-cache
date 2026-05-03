# scopecache — Core RFC

> **Status: pre-v1.0 draft.** All sections (§1–§9) are in place.
> Wording, structure, and detail levels remain open for revision.
> [scopecache-rfc-old.md](scopecache-rfc-old.md) is kept as a
> historical reference for the v0.7.16-era multi-tenant gateway
> design; addon-specific RFCs will land alongside this document as
> the addon sub-packages ship.

---

## 1. Scope and boundary

### 1.1 What scopecache is

`scopecache` is a small, local, rebuildable in-memory cache and write
buffer. It is addressed by `scope` (namespace), `id`, and `seq`; it
holds opaque JSON payloads; it can be wiped and rebuilt from the
source of truth at any time. The source of truth lives outside the
cache — a database, a JSON file, data generated in code, anything.

The cache supports two main use patterns:

- **Hot-read cache.** Keep frequently queried fragments in memory so
  they don't hit the database on every request. A fronting proxy
  (Caddy, nginx, apache) can serve cached HTML, JSON, or XML straight
  from `/render` without any application layer in between.
- **Write buffer.** Append high-frequency events; a background worker
  drains the buffer in batches via `/tail` followed by
  `/delete_up_to`.

### 1.2 Deployment modes

scopecache runs in two modes:

- **Standalone binary.** Listens on a Unix domain socket, reachable
  from any HTTP client in any language (`curl`, PHP `cURL`, Python
  `requests`, Node `fetch`, …). The lowest-friction setup: any
  webstack that already speaks HTTP can use it.
- **Caddy module.** The cache lives inside the same process as
  Caddy and is served on the Caddyfile-defined listener. This is
  the recommended deployment when the cache sits behind a webserver
  that already terminates client connections.

The Caddy-module path is the one that gets the most out of the
cache: the webserver answers cache hits directly from memory
instead of forwarding the request to PHP/Python/etc., querying a
separate cache like Redis, and serialising the response back.
Benchmarks measured roughly 5× the throughput of an
equivalent webserver → application → Redis → response path serving
the same bytes — even when the application path is FrankenPHP in
worker mode.

When scopecache runs as a Caddy module it lives in the same Go
process as Caddy itself — no extra hop over a Unix socket, no
serialisation between processes. That in-process call is where the
largest performance gain comes from.

### 1.3 What scopecache is not

The core does not implement:

- a database, search engine, analytics store, or query language
- payload inspection, joins, or filtering beyond `scope`/`id`/`seq`
- TTL, eviction, schedulers, drains, or any background policy
- authentication, authorization, tenant management, or rate limiting
- business workflows of any kind

Anything in this list that you need is operator policy or addon
territory. See §1.4.

### 1.4 The boundary rule

The core has no business logic and no policy logic. It owns:

- memory and capacity enforcement
- `scope`/`id`/`seq` addressing
- write, read, delete, and bulk primitives
- raw payload rendering (`/render`)
- operational stats and lightweight read-heat metadata
- a public, validated Go API for in-process callers

It does not own anything that requires request context (who is
calling, what tenant, what permission). Those concerns live in
**addons**: separate Go sub-packages built on top of the core's
public Go API, each with its own RFC.

The current set of standard addons covers multi-tenant gateways,
batch dispatch, write-only ingestion, operator-elevated dispatch,
and eviction-hint queries. Their RFCs live alongside this one in
`docs/`. Third-party addons follow the same pattern.

### 1.5 Modular architecture

The core is the foundation: a small set of building blocks — the
data model, the capacity rules, the address primitives, and the
public Go API. Anything beyond that comes from **addons**: separate
Go sub-packages built on top of the public Go API. Some addons ship
with `scopecache` as part of the standard distribution
(multi-tenant gateways, batch dispatch, write-only ingestion, …);
anyone can build their own addons against the same public interface
without touching the core.

The result is a clean separation of concerns:

- The core does one job — store and address items, enforce
  capacity, expose the primitives — and stays small, fast, and
  heavily tested.
- Addons add request-context-aware behaviour (auth, tenants, batch
  composition, custom ingestion shapes) without ever needing
  privileged access to core internals.

This separation is what allows the core to remain stable under
heavy testing and benchmarking while addons can evolve, be added,
or be removed without risk to the cache itself.

### 1.6 Status

Pre-v1.0. Core HTTP and Go API surfaces are subject to breaking
change between minor versions. After v1.0 the core becomes
semver-stable; addon RFCs version independently of the core RFC.

---

## 2. Data model

### 2.1 Item

The cache stores **items**. An item has the following fields:

| field     | type           | required on writes | owner  | searchable |
|-----------|----------------|--------------------|--------|------------|
| `scope`   | string         | yes                | client | yes        |
| `id`      | string         | no                 | client | yes        |
| `seq`     | uint64         | n/a                | cache  | yes        |
| `ts`      | int64          | n/a                | cache  | no         |
| `payload` | any JSON value | yes                | client | no         |

- **`scope`** — required on every operation. Free-form, ≤ 256 bytes.
  Items inside the same scope share the same per-scope buffer (and
  its mutex).
- **`id`** — optional on writes. When present, must be unique within
  its scope. Free-form, ≤ 256 bytes.
- **`seq`** — cache-assigned monotonic counter, scoped per buffer.
  Clients **must not** send `seq` on write endpoints; reads accept
  `seq` as an addressing key.
- **`ts`** — cache-assigned microsecond Unix timestamp
  (`time.Now().UnixMicro()`), refreshed on every write that touches
  the item. Observability only — not searchable, not indexed, not
  used for ordering.
- **`payload`** — required, any valid JSON value (object, array,
  string, number, boolean). Literal `null` is rejected. The cache
  treats payload bytes as opaque; nothing inspects, parses, or
  searches inside them.

### 2.2 Addressing

Items are addressed via `scope`, `scope`+`id`, or `scope`+`seq`.
There is no global index, no secondary lookup by payload contents,
and no range query other than a `seq`-prefix drain
(`/delete_up_to`) and a sequential read (`/tail`, `/head`).

Within a scope, items appear in `seq` order. `seq` starts at 1 for
the first write into a scope and increases monotonically per scope.
`seq` numbers are not reused after deletion.

### 2.3 Scopes are opaque strings

The cache imposes no structure on scope names beyond size and
encoding limits. Scope names like `thread:42`, `user:alice:inbox`,
or HMAC-derived prefixes are equally valid; the cache does not
parse them, does not split on `:`, and does not interpret leading
underscores as reserved.

The `_` prefix is a **social convention** for state managed by
addons (`_tokens`, `_counters_*`, addon-internal scopes). The core
does not enforce it. If an addon wants its state protected from
public writes, that protection must come from the operator (gating
which endpoints are publicly reachable) or from the addon itself
(signed payloads, unguessable scope names) — see §1.3.

---

## 3. Capacity and limits

The cache enforces the following capacity limits on every write
path. All three are configurable via `Config` (Go API), env-vars
(standalone binary), or Caddyfile directives (Caddy module); see the
adapter docs for exact knob names.

| limit             | scope        | default   | exceeded → |
|-------------------|--------------|-----------|------------|
| `ScopeMaxItems`   | per-scope    | 100,000   | 507        |
| `MaxItemBytes`    | per-item     | 1 MiB     | 400        |
| `MaxStoreBytes`   | store-wide   | 100 MiB   | 507        |

### 3.1 Per-scope item cap

Writes that would push the per-scope item count past
`ScopeMaxItems` are rejected with `507 Insufficient Storage`. The
response body identifies the offending scope and its current count:

```json
{
  "ok": false,
  "error": "scope is at capacity",
  "scopes": [{"scope": "...", "count": 100000, "cap": 100000}]
}
```

The cache never auto-evicts. Clients free space by deleting items
(`/delete_up_to`, `/delete`) or replacing the scope contents
(`/warm`).

### 3.2 Per-item byte cap

The size of an item is the sum of its scope, id, fixed-overhead, and
payload bytes (see `approxItemSize` in code). Writes whose item
exceeds `MaxItemBytes` are rejected with `400 Bad Request` — this
is a request-shape error, not capacity exhaustion.

### 3.3 Store-wide byte cap

Writes that would push the aggregate stored-item bytes past
`MaxStoreBytes` are rejected with `507 Insufficient Storage`. The
response body reports current usage, the attempted addition, and the
cap:

```json
{
  "ok": false,
  "error": "store is at byte capacity",
  "approx_store_mb": 99.9,
  "added_mb": 0.1,
  "max_store_mb": 100.0
}
```

This is the cache-wide equivalent of `ScopeMaxItems`. Free space by
deletion as before.

### 3.4 No automatic eviction

The cache never evicts. There is no LRU, no LFU, no TTL, no
background sweeper. Whatever you write stays until you delete it
(or until process restart, which clears the entire cache by
definition — the cache is in-memory only).

Operator tools to manage capacity:

- `/delete_up_to` — drain a `seq`-prefix in one call (write-buffer
  pattern)
- `/delete_scope` — remove a whole scope
- `/wipe` — clear every scope, every item, every byte
- `/warm` — atomically replace a scope's contents (frees the
  previous contents' bytes in the same call)
- `/rebuild` — atomically replace the entire store

Read-heat metadata (§8) helps operators identify which scopes are
cold enough to evict, but the cache itself never decides.

---

## 4. Validation

All write paths share the same validation pass before the request
reaches the store. Errors are returned as `400 Bad Request` with a
JSON body — see §5.2.

### 4.1 Field-shape rules

Validation rules for the fields that appear across the API:

| field       | type            | shape rule                                               |
|-------------|-----------------|----------------------------------------------------------|
| `scope`     | string          | required; ≤ 256 bytes; no leading/trailing whitespace; no control characters (0x00–0x1F, 0x7F) |
| `id`        | string          | optional or required (per endpoint); same shape as `scope` when present |
| `payload`   | any JSON value  | required; literal `null` is rejected; bytes are opaque to the cache |
| `seq`       | uint64          | cache-assigned; clients must omit on every write; reads accept it as an addressing key |
| `ts`        | int64           | cache-assigned; clients must omit on every write |
| `by`        | int64           | required for `/counter_add`; non-zero; within ±(2^53 − 1) |
| `max_seq`   | uint64          | required for `/delete_up_to`; must be > 0 |

Per-item byte size (the sum of `scope`, `id`, fixed overhead, and
payload bytes — see `approxItemSize` in code) is checked against
`MaxItemBytes` after field-shape validation. An over-cap item is
rejected with `400 Bad Request`. See §3.2.

### 4.2 Query parameters

Read endpoints accept query parameters with the following rules:

| parameter   | type    | default | rule                                          |
|-------------|---------|---------|-----------------------------------------------|
| `scope`     | string  | —       | same shape rules as the body field            |
| `id`        | string  | —       | same shape rules as the body field            |
| `seq`       | uint64  | —       | parsed as unsigned integer                    |
| `limit`     | int     | 1000    | must be > 0; values above 10000 are clamped, not rejected |
| `offset`    | int     | 0       | must be ≥ 0                                   |
| `after_seq` | uint64  | 0       | parsed as unsigned integer; 0 means "from the start" |

Single-item read endpoints (`/get`, `/render`) require exactly one
of `id` or `seq`. Supplying both, or neither, is rejected with
`400 Bad Request`.

### 4.3 Why the cache rejects client-supplied `seq` and `ts`

Both fields are owned by the cache and stamped on every write that
touches an item. Accepting client-supplied values would silently
break the invariant that `seq` is monotonic per scope and that `ts`
reflects the cache's own write time. The validator rejects them
with an explicit error rather than overwriting silently — clients
that need a "client timestamp" can carry it inside `payload`, where
the cache stays opaque.

---

## 5. HTTP contract

### 5.1 Response envelope

Successful responses use a JSON envelope. The shape of the envelope
varies per endpoint, but two fields are universal:

- `ok` — boolean, always present, `true` on success and `false` on
  error.
- `duration_us` — integer, always present, the handler's internal
  duration in microseconds (measured from the start of request
  processing to the start of response write).

Read endpoints whose response size scales with the request (`/get`,
`/head`, `/tail`) additionally include:

- `approx_response_mb` — number, the approximate marshalled size
  of the response body in MiB (4-decimal precision).

Two endpoints break the JSON-envelope rule by design:

- **`/render`** — returns raw payload bytes (or empty body on
  miss); see §6.4.
- **`/help`** — returns `text/plain`; see §6.5.

### 5.2 Error envelope

Error responses use the same JSON envelope shape with `ok: false`
and a string `error` field describing the failure. Capacity errors
(`507`) include additional structured fields naming the offending
scope or store-wide totals — see §3.

```json
{
  "ok": false,
  "error": "the 'scope' field is required for the '/append' endpoint",
  "duration_us": 12
}
```

### 5.3 Status codes

The cache uses a small, deterministic set of HTTP status codes:

| status | meaning                                     | examples                                |
|--------|---------------------------------------------|-----------------------------------------|
| 200    | success                                     | every successful operation              |
| 400    | request-shape error (validation, parse)     | missing field, oversize item, malformed |
| 404    | resource not found (raw-bytes endpoint only)| `/render` miss                          |
| 405    | method not allowed                          | `GET /append`, `POST /get`              |
| 409    | scope detached mid-flight                   | concurrent `/wipe` or `/delete_scope`   |
| 507    | capacity exceeded                           | per-scope or store-wide cap reached     |

The JSON-envelope reads (`/get`, `/head`, `/tail`) deliberately do
**not** use 404 for misses. A miss is a successful query that
happened to find nothing; the envelope carries `hit: false` instead.
This keeps client error-handling on read paths simple — only network
failures and 4xx-as-validation-errors need attention; misses are
ordinary results.

### 5.4 Content types

| endpoint            | response content-type                      |
|---------------------|--------------------------------------------|
| every JSON endpoint | `application/json; charset=utf-8`          |
| `/render`           | `application/octet-stream`                 |
| `/help`             | `text/plain; charset=utf-8`                |

`/render` deliberately uses `application/octet-stream` — a neutral
default that the fronting proxy is expected to override per-route
(`header Content-Type text/html`, etc.). The cache does not sniff
content or guess the real MIME type.

### 5.5 Method matching

Every endpoint accepts exactly one HTTP method (`GET` for reads
and observability, `POST` for writes and bulk operations). Calling
an endpoint with the wrong method returns `405 Method Not Allowed`
with the standard error envelope. The exact method per endpoint is
listed in §6.

---

## 6. Endpoints

scopecache exposes the following endpoints, grouped by purpose:

| group                       | endpoints                                                  |
|-----------------------------|------------------------------------------------------------|
| §6.1 Single-item writes     | `/append`, `/upsert`, `/update`, `/counter_add`            |
| §6.2 Deletes                | `/delete`, `/delete_up_to`, `/delete_scope`, `/wipe`       |
| §6.3 Bulk                   | `/warm`, `/rebuild`                                         |
| §6.4 Reads                  | `/get`, `/render`, `/head`, `/tail`                         |
| §6.5 Observability          | `/stats`, `/help`                                           |

### Conventions used in this section

To keep per-endpoint sections compact, the following errors are
implicit on every endpoint and are not repeated:

- **`405 Method Not Allowed`** — wrong HTTP method (§5.5).
- **`400 Bad Request`** — request-shape errors from §4 (missing
  required field, oversized item, malformed JSON, client-supplied
  `seq` or `ts`, etc.).

In addition, the following errors are implicit on every **write**
endpoint (§6.1, §6.2, §6.3):

- **`409 Conflict`** — `scope was deleted while the request was in
  flight; please retry`. Fires when a concurrent `/wipe`,
  `/delete_scope`, or `/rebuild` detached the scope between the
  handler's lookup and its mutation.
- **`507 Insufficient Storage`** — per-scope or store-wide capacity
  exceeded (§3.1, §3.3).

Each endpoint section lists only the errors that are specific to
it (typically: the body fields it requires, and any
endpoint-specific 4xx that does not fit the universal patterns
above).

### 6.1 Single-item writes

#### `POST /append`

Insert a new item into a scope. Rejects on duplicate `id`-in-scope.

**Request body**

| field     | type           | required | notes                                |
|-----------|----------------|----------|--------------------------------------|
| `scope`   | string         | yes      | shape per §4.1                       |
| `id`      | string         | no       | shape per §4.1; cache assigns if absent |
| `payload` | any JSON value | yes      | not literal `null`                   |

**Response (200)**

```json
{
  "ok": true,
  "item": {"scope": "events", "id": "e1", "seq": 1, "ts": 1700000000000000},
  "duration_us": 42
}
```

The `item` object echoes the stored `scope`, `id`, `seq`, and `ts`.
Payload bytes are not echoed (they doubled the wire cost on the
write path that just delivered them).

**Endpoint-specific errors**

| status | error                                  | when                          |
|--------|----------------------------------------|-------------------------------|
| 409    | `the 'id' is already in use`           | duplicate `id` within `scope` |

**Example**

```bash
curl -s -X POST http://localhost:8080/append \
  -H 'Content-Type: application/json' \
  -d '{"scope":"events","id":"e1","payload":{"v":1}}'
# → {"ok":true,"item":{"scope":"events","id":"e1","seq":1,"ts":...},"duration_us":42}
```

---

#### `POST /upsert`

Insert a new item, or replace an existing one with the same
`scope`+`id`. Always succeeds (within capacity); the response
distinguishes create from replace via `created`.

**Request body**

| field     | type           | required | notes                                |
|-----------|----------------|----------|--------------------------------------|
| `scope`   | string         | yes      | shape per §4.1                       |
| `id`      | string         | yes      | shape per §4.1                       |
| `payload` | any JSON value | yes      | not literal `null`                   |

**Response (200)**

```json
{
  "ok": true,
  "created": false,
  "item": {"scope": "events", "id": "e1", "seq": 5, "ts": 1700000000000000},
  "duration_us": 42
}
```

`created` is `true` when the item did not exist before this call,
`false` when an existing item was replaced. On replace, `seq`
keeps its original value; on create, `seq` is freshly assigned.
`ts` is always refreshed.

**Example**

```bash
curl -s -X POST http://localhost:8080/upsert \
  -H 'Content-Type: application/json' \
  -d '{"scope":"events","id":"e1","payload":{"v":2}}'
# → {"ok":true,"created":false,"item":{"scope":"events","id":"e1","seq":5,"ts":...},"duration_us":42}
```

---

#### `POST /update`

Modify the payload of an existing item, addressed by `scope`+`id`
or `scope`+`seq`. Soft-misses on a non-existent item (returns 200
with `hit: false`).

**Request body**

| field     | type           | required               | notes                                |
|-----------|----------------|------------------------|--------------------------------------|
| `scope`   | string         | yes                    | shape per §4.1                       |
| `id`      | string         | exactly one of id/seq  | shape per §4.1 when present          |
| `seq`     | uint64         | exactly one of id/seq  | parsed as unsigned integer           |
| `payload` | any JSON value | yes                    | not literal `null`                   |

**Response (200)**

```json
{"ok": true, "hit": true, "updated_count": 1, "duration_us": 42}
```

- `hit` — whether an item was found and updated (`updated_count > 0`).
- `updated_count` — number of items modified (always 0 or 1 since
  `id`/`seq` is unique-in-scope).

**Side effects**

`ts` is refreshed on a hit. `seq` is preserved. If the new payload
is larger than the old one and would push the store past
`MaxStoreBytes`, the request is rejected with 507 and no change is
applied.

**Example**

```bash
curl -s -X POST http://localhost:8080/update \
  -H 'Content-Type: application/json' \
  -d '{"scope":"events","id":"e1","payload":{"v":3}}'
# → {"ok":true,"hit":true,"updated_count":1,"duration_us":42}
```

---

#### `POST /counter_add`

Atomically increment (or create) a numeric counter at `scope`+`id`
by `by`. The only endpoint that reads or mutates a payload as a
typed value — every other write path treats payloads as opaque
bytes.

**Request body**

| field   | type    | required | notes                                                      |
|---------|---------|----------|------------------------------------------------------------|
| `scope` | string  | yes      | shape per §4.1                                             |
| `id`    | string  | yes      | shape per §4.1                                             |
| `by`    | int64   | yes      | non-zero; within ±(2^53 − 1)                               |

**Response (200)**

```json
{"ok": true, "created": false, "value": 7, "duration_us": 42}
```

- `created` — `true` when the counter did not exist (item created
  with payload `by`); `false` when an existing counter was
  incremented.
- `value` — the post-increment counter value.

**Endpoint-specific errors**

| status | error                                                      | when                                              |
|--------|------------------------------------------------------------|---------------------------------------------------|
| 400    | `the counter operation would exceed the allowed range of ±(2^53-1)` | result would overflow the JS-safe integer range |
| 409    | `payload is not a JSON integer` (or similar)               | existing item's payload is not a valid integer    |

**Side effects**

The payload is read, parsed as an integer, incremented by `by`, and
written back as a JSON integer. The counter's underlying byte size
may change (e.g. `99` → `100` is one byte longer); the per-item and
store-wide caps are evaluated against the new size.

**Example**

```bash
curl -s -X POST http://localhost:8080/counter_add \
  -H 'Content-Type: application/json' \
  -d '{"scope":"hits","id":"page-42","by":1}'
# → {"ok":true,"created":false,"value":7,"duration_us":42}
```

### 6.2 Deletes

#### `POST /delete`

Delete a single item by `scope`+`id` or `scope`+`seq`. Soft-misses
on a non-existent item.

**Request body**

| field   | type   | required              | notes                       |
|---------|--------|-----------------------|-----------------------------|
| `scope` | string | yes                   | shape per §4.1              |
| `id`    | string | exactly one of id/seq | shape per §4.1 when present |
| `seq`   | uint64 | exactly one of id/seq | parsed as unsigned integer  |

**Response (200)**

```json
{"ok": true, "hit": true, "deleted_count": 1, "duration_us": 42}
```

**Example**

```bash
curl -s -X POST http://localhost:8080/delete \
  -H 'Content-Type: application/json' \
  -d '{"scope":"events","id":"e1"}'
# → {"ok":true,"hit":true,"deleted_count":1,"duration_us":42}
```

---

#### `POST /delete_up_to`

Drain a `seq`-prefix from a scope: removes every item with
`seq ≤ max_seq`. The write-buffer drain primitive — pair with
`/tail` to read items, then `/delete_up_to` with the last drained
`seq` to release the buffer.

**Request body**

| field      | type   | required | notes                          |
|------------|--------|----------|--------------------------------|
| `scope`    | string | yes      | shape per §4.1                 |
| `max_seq`  | uint64 | yes      | must be > 0                    |

**Response (200)**

```json
{"ok": true, "hit": true, "deleted_count": 100, "duration_us": 42}
```

`deleted_count` is the number of items actually removed (may be 0
if no items had `seq ≤ max_seq`).

**Example**

```bash
curl -s -X POST http://localhost:8080/delete_up_to \
  -H 'Content-Type: application/json' \
  -d '{"scope":"events","max_seq":100}'
# → {"ok":true,"hit":true,"deleted_count":100,"duration_us":42}
```

---

#### `POST /delete_scope`

Remove a whole scope, including its buffer and all its items.
Soft-misses on a non-existent scope.

**Request body**

| field   | type   | required | notes          |
|---------|--------|----------|----------------|
| `scope` | string | yes      | shape per §4.1 |

**Response (200)**

```json
{"ok": true, "hit": true, "deleted_scope": true, "deleted_items": 42, "duration_us": 42}
```

- `hit` / `deleted_scope` — `true` when the scope existed and was
  removed; `false` when the scope did not exist.
- `deleted_items` — the number of items the scope held at deletion
  time.

**Side effects**

In-flight writes against the scope detach (return 409 to their
callers); the scope's bytes are released back to the store-wide
budget.

**Example**

```bash
curl -s -X POST http://localhost:8080/delete_scope \
  -H 'Content-Type: application/json' \
  -d '{"scope":"events"}'
# → {"ok":true,"hit":true,"deleted_scope":true,"deleted_items":42,"duration_us":42}
```

---

#### `POST /wipe`

Clear every scope, every item, and every byte reservation in one
call. The store-wide complement of `/delete_scope` — a single
operation rather than N per-scope calls.

**Request body**

None. A non-empty body is silently ignored. The cache caps the body
at 1 KiB to prevent memory abuse.

**Response (200)**

```json
{"ok": true, "deleted_scopes": 12, "deleted_items": 5400, "freed_mb": 12.3456, "duration_us": 42}
```

**Side effects**

In-flight writes against any scope detach (409). After the call,
`approx_store_mb` reads as 0 in `/stats`.

This is **not** an eviction policy: the cache never wipes itself.
`/wipe` exists so operators can clear-and-rebuild atomically rather
than coordinating N delete calls.

**Example**

```bash
curl -s -X POST http://localhost:8080/wipe
# → {"ok":true,"deleted_scopes":12,"deleted_items":5400,"freed_mb":12.3456,"duration_us":42}
```

### 6.3 Bulk

#### `POST /warm`

Atomically replace the contents of one or more scopes with the
items in the request body. Scopes not mentioned in the body are
left alone. Old contents are released in the same call (the
freed bytes can be reused by the new contents within the same
capacity check).

**Request body**

```json
{"items": [{"scope":"...","id":"...","payload":...}, ...]}
```

Each item validates against the same rules as `/append` (§4.1).
Items are grouped by `scope` server-side; every scope mentioned in
the batch is replaced as a unit.

**Response (200)**

```json
{"ok": true, "count": 100, "replaced_scopes": 3, "duration_us": 42}
```

- `count` — total number of items written.
- `replaced_scopes` — number of distinct scopes touched by the
  replacement.

**Side effects**

The request body cap for `/warm` is much larger than for
single-item writes — large enough that a fully-loaded store can
always be expressed as one bulk call. In-flight writes against any
replaced scope detach (409).

**Example**

```bash
curl -s -X POST http://localhost:8080/warm \
  -H 'Content-Type: application/json' \
  -d '{"items":[{"scope":"events","id":"e1","payload":{"v":1}}]}'
# → {"ok":true,"count":1,"replaced_scopes":1,"duration_us":42}
```

---

#### `POST /rebuild`

Atomically replace the entire store. Every scope and every item is
discarded; the request body becomes the new state. Equivalent to
`/wipe` immediately followed by `/warm` for every scope, but in a
single atomic operation.

**Request body**

```json
{"items": [{"scope":"...","id":"...","payload":...}, ...]}
```

**Endpoint-specific errors**

| status | error                                              | when                  |
|--------|----------------------------------------------------|-----------------------|
| 400    | `the 'items' array must not be empty for the '/rebuild' endpoint` | empty `items[]`       |

An empty `items[]` is rejected explicitly because it would silently
wipe the store — almost always a client bug. Operators that really
want to clear the store should call `/wipe`.

**Response (200)**

```json
{"ok": true, "count": 100, "rebuilt_scopes": 3, "rebuilt_items": 100, "duration_us": 42}
```

**Example**

```bash
curl -s -X POST http://localhost:8080/rebuild \
  -H 'Content-Type: application/json' \
  -d '{"items":[{"scope":"events","id":"e1","payload":{"v":1}}]}'
# → {"ok":true,"count":1,"rebuilt_scopes":1,"rebuilt_items":1,"duration_us":42}
```

### 6.4 Reads

#### `GET /get`

Look up a single item by `scope`+`id` or `scope`+`seq`. Returns
200 in both the hit and miss case; the response carries `hit:
true|false`. See §5.3 for why misses are not 404.

**Query parameters**

| parameter | type   | required              |
|-----------|--------|-----------------------|
| `scope`   | string | yes                   |
| `id`      | string | exactly one of id/seq |
| `seq`     | uint64 | exactly one of id/seq |

**Response (200, hit)**

```json
{
  "ok": true,
  "hit": true,
  "count": 1,
  "item": {"scope":"events","id":"e1","seq":1,"ts":1700000000000000,"payload":{"v":1}},
  "duration_us": 42,
  "approx_response_mb": 0.0001
}
```

**Response (200, miss)**

```json
{"ok": true, "hit": false, "count": 0, "item": null, "duration_us": 42, "approx_response_mb": 0.0001}
```

**Side effects**

A successful hit increments the per-scope read-heat counters
(§8) unless `DisableReadHeat` is set.

**Example**

```bash
curl -s 'http://localhost:8080/get?scope=events&id=e1'
# → {"ok":true,"hit":true,"count":1,"item":{...},"duration_us":42,"approx_response_mb":0.0001}
```

---

#### `GET /render`

Serve a single item's payload as raw bytes, with no JSON envelope.
Designed for fronting proxies (Caddy, nginx, apache) to pipe cached
HTML, JSON, XML, or text fragments straight to the client without
an application layer in between.

**Query parameters**

Same as `/get`: `scope`, plus exactly one of `id` or `seq`.

**Response (200, hit)**

- `Content-Type: application/octet-stream` (override at the proxy)
- Body: raw payload bytes, with one layer of JSON-string decoding
  if the stored payload is a JSON string. Other JSON values
  (object, array, number, boolean) are written verbatim.

**Response (404, miss)**

- `Content-Type: application/octet-stream`
- Empty body. `/render` is the **only** endpoint that uses 404 for
  a not-found resource — the use case (proxy-fronted byte
  streaming) does not benefit from a JSON envelope, so a status
  code is the lowest-friction signal.

**Side effects**

A successful hit increments the per-scope read-heat counters (§8).

**Example**

```bash
curl -i 'http://localhost:8080/render?scope=html&id=page-1'
# HTTP/1.1 200 OK
# Content-Type: application/octet-stream
#
# <html>...</html>
```

---

#### `GET /head`

Return the oldest items in a scope, optionally cursoring past a
given `after_seq`. Returns 200 in both the hit and miss case
(empty scope or unknown scope yields `hit: false`).

**Query parameters**

| parameter   | type   | default | notes                                       |
|-------------|--------|---------|---------------------------------------------|
| `scope`     | string | —       | required, shape per §4.1                    |
| `limit`     | int    | 1000    | clamped to ≤ 10000                          |
| `after_seq` | uint64 | 0       | return items with `seq > after_seq`         |

`offset` is **not** supported on `/head` — use `after_seq` for
cursor-based forward paging (stable under `/delete_up_to`), or
`/tail` for position-based paging.

**Response (200, hit)**

```json
{
  "ok": true,
  "hit": true,
  "count": 10,
  "truncated": false,
  "items": [{"scope":"events","id":"e1","seq":1,"ts":...,"payload":{"v":1}}, ...],
  "duration_us": 42,
  "approx_response_mb": 0.0042
}
```

`truncated` is `true` when more items exist beyond the returned
`limit` window.

**Response (200, miss)** — empty scope or unknown scope:

```json
{"ok": true, "hit": false, "count": 0, "truncated": false, "items": [], "duration_us": 42, "approx_response_mb": 0.0001}
```

**Endpoint-specific errors**

| status | error                                                     | when                       |
|--------|-----------------------------------------------------------|----------------------------|
| 507    | `the response would exceed the maximum allowed size`      | response > `MaxResponseBytes` |

**Side effects**

A successful hit increments the per-scope read-heat counters (§8).

**Example**

```bash
curl -s 'http://localhost:8080/head?scope=events&limit=10'
# → {"ok":true,"hit":true,"count":10,"truncated":false,"items":[...],"duration_us":42,...}
```

---

#### `GET /tail`

Return the newest items in a scope, optionally offset back from the
tail. Returns 200 in both the hit and miss case.

**Query parameters**

| parameter | type   | default | notes                                       |
|-----------|--------|---------|---------------------------------------------|
| `scope`   | string | —       | required, shape per §4.1                    |
| `limit`   | int    | 1000    | clamped to ≤ 10000                          |
| `offset`  | int    | 0       | skip this many items from the tail before reading |

**Response (200, hit)**

```json
{
  "ok": true,
  "hit": true,
  "count": 10,
  "offset": 0,
  "truncated": true,
  "items": [...],
  "duration_us": 42,
  "approx_response_mb": 0.0042
}
```

**Endpoint-specific errors**

| status | error                                                     | when                       |
|--------|-----------------------------------------------------------|----------------------------|
| 507    | `the response would exceed the maximum allowed size`      | response > `MaxResponseBytes` |

**Side effects**

A successful hit increments the per-scope read-heat counters (§8).

**Example**

```bash
curl -s 'http://localhost:8080/tail?scope=events&limit=10'
# → {"ok":true,"hit":true,"count":10,"offset":0,"truncated":true,"items":[...],"duration_us":42,...}
```

### 6.5 Observability

#### `GET /stats`

Return a store-wide snapshot: scope count, total item count,
approximate stored bytes, and per-scope counts and metadata
(including read-heat — see §8).

**Query parameters**

None.

**Response (200)**

```json
{
  "ok": true,
  "scope_count": 12,
  "total_items": 5400,
  "approx_store_mb": 12.3456,
  "max_store_mb": 100.0,
  "scopes": {
    "events": {
      "item_count": 100,
      "last_seq": 100,
      "approx_scope_mb": 0.0123,
      "created_ts": 1700000000000000,
      "last_write_ts": 1700000000000000,
      "last_access_ts": 1700000000000000,
      "read_count_total": 4242,
      "last_7d_read_count": 1337
    }
  },
  "duration_us": 42
}
```

`scopes` is keyed by scope name; outer-map keys serialise in
alphabetical order. Per-scope fields emit in the order shown.

**Side effects**

`/stats` enumerates every scope name in the store. In multi-tenant
deployments where addons keep state in `_*` scopes, those names
will surface here. The cache draws no line; gating `/stats` at the
transport layer is the operator's responsibility (see §1.3).

**Example**

```bash
curl -s 'http://localhost:8080/stats'
# → {"ok":true,"scope_count":12,"total_items":5400,...}
```

---

#### `GET /help`

Return a one-line plain-text pointer to the canonical RFC. Intended
as a low-maintenance self-documentation hook; rich endpoint listings
live in this RFC, not in `/help`.

**Query parameters**

None.

**Response (200)**

- `Content-Type: text/plain; charset=utf-8`
- Body: a single line of text, currently:

```
scopecache — see instructions at https://github.com/VeloxCoding/scopecache/blob/main/docs/scopecache-core-rfc.md
```

**Example**

```bash
curl -s 'http://localhost:8080/help'
# → scopecache — see instructions at https://...
```

---

## 7. Operational model

### 7.1 Locking

The cache uses three concentric layers of locks:

- **Shard locks** — the scope registry is split into independently
  locked shards. Scope creation and lookup take only the relevant
  shard's lock, so unrelated scopes parallelise across shards.
- **Per-scope locks** — every scope has its own `sync.RWMutex`.
  Single-item writes take the shard lock to look up the scope,
  then operate at scope level under the scope's mutex. Concurrent
  writes to the same scope serialise on this mutex; writes to
  different scopes do not.
- **Multi-shard locks** — store-wide mutations (`/wipe`,
  `/rebuild`, multi-scope `/warm`) acquire shard locks in
  ascending shard-index order to prevent deadlock between
  concurrent multi-shard operations.

The byte-budget counter (used by `MaxStoreBytes` admission) is a
single atomic value, modified via compare-and-swap. Writes reserve
their net byte delta on this counter before taking the per-scope
lock, so an over-cap write fails fast without acquiring the scope
mutex.

### 7.2 Durability contract

A `200 OK` response from any write endpoint confirms the write was
applied to the cache at the moment of commit. It is **not** a
persistence guarantee:

- The cache is in-memory only. Process restart clears everything.
- A concurrent `/rebuild` or `/wipe` replaces or clears the entire
  store and erases writes that committed moments earlier. This is
  intentional: both endpoints express "the source of truth says
  this is the new state," and the cache is explicitly subordinate
  to that source.
- `/delete_scope` and `/delete_up_to` erase earlier writes within
  their scope by design. Appends that committed just before a
  delete can vanish from the cache (but not from the source of
  truth, which is where they came from).
- The orphan-detach mechanism (returning `409` when a scope is
  unlinked mid-write) protects the store's internal accounting —
  the byte counter cannot be corrupted by a write committing into
  a buffer unlinked by a concurrent swap — but it does not, and
  cannot, retroactively preserve the write itself.

Clients MUST be idempotent against cache loss (items re-fetchable
or re-derivable from the source of truth) and MUST NOT treat a
`200 OK` as a durable acknowledgement.

### 7.3 Read consistency

Read endpoints answer under per-scope read locks: the contents of
any single scope returned by `/get`, `/head`, `/tail`, or `/render`
are internally consistent at the moment of the read.

`/stats` is an **advisory snapshot**, not a transaction:

- Each scope is read under its own lock, so per-scope values are
  internally consistent.
- The response as a whole is not a global atomic snapshot — writes
  committing in other scopes between per-scope reads are visible
  in the same response.
- Store-wide totals (`approx_store_mb`, `total_items`) come from
  the atomic byte counter and may briefly skew against the sum of
  per-scope values under concurrent writes. The
  `Σ scope.bytes == total_bytes` invariant holds at quiesce, not
  at every observation.

Operators using `/stats` to drive decisions (capacity planning,
eviction-candidate selection) should treat its output as
approximate rather than transactional.

---

## 8. Read-heat tracking

The cache maintains lightweight per-scope read-heat metadata so
addons (and operators) can identify cold scopes for eviction
without polling every scope.

### 8.1 Tracked fields

Every scope carries the following read-heat metadata, surfaced via
`/stats` (per-scope sub-object):

| field                | type    | meaning                                            |
|----------------------|---------|----------------------------------------------------|
| `last_access_ts`     | int64   | microsecond timestamp of the most recent read      |
| `read_count_total`   | uint64  | lifetime read count (since process start or last reset) |
| `last_7d_read_count` | uint64  | rolling 7-day read count                           |

`last_7d_read_count` is computed via a per-scope ring buffer of
day-bucket counters. The bucket for the current day is updated on
each read; buckets older than 7 days are recycled lazily on the
next read into that scope.

### 8.2 What counts as a read

The following endpoints stamp `last_access_ts` and increment the
counters on a successful hit:

- `GET /get`
- `GET /render`
- `GET /head`
- `GET /tail`

Misses (no scope, no item, empty result) do **not** count. `/stats`
itself is observability and does not count.

### 8.3 Concurrency

Read-heat updates are lock-free: timestamps and counters are
modified via compare-and-swap on atomic 64-bit values. The hot
read path does not take the scope's write lock to update heat,
so concurrent readers do not serialise on heat updates.

### 8.4 Disabling read-heat

Heat tracking can be turned off via the `DisableReadHeat`
configuration knob (env-var `SCOPECACHE_DISABLE_READ_HEAT` on the
standalone binary; `disable_read_heat` directive in the Caddy
module). With heat tracking off the four counters above remain
zero in `/stats`. Disabling saves the per-read `time.Now()` call
plus the atomic CAS work — useful when the operator does not need
heat-driven eviction.

### 8.5 Read-heat as a building block

Read-heat is exposed as a primitive. The cache itself never uses
it to decide anything; eviction decisions live in addons (or in
operator-side logic that polls `/stats`). For example, an addon
that ranks scopes for eviction by ascending `last_access_ts`
builds its query on top of `/stats` rather than asking the core
to sort.

---

## 9. Out of scope

The following are **deliberately not** core features. Anything
listed here either lives in operator policy (gating, networking,
deployment), in addon sub-packages (per-addon RFCs), or is a
non-goal entirely.

### 9.1 Not in core: deferred to addons

- **Authentication and authorization.** Tokens, signatures, mTLS,
  bearer headers, capability checks — none of this is core. Addons
  (e.g. a tenant-gateway addon, an operator-elevated dispatcher
  addon) add request-context-aware auth on top of the public Go
  API.
- **Multi-tenancy.** Per-tenant scope isolation, prefix rewrites,
  scope-name conventions tied to tenant identity. Live in addons.
- **Batch dispatch.** Combining N sub-calls into one HTTP roundtrip
  (`/multi_call`-shaped). Lives in an addon.
- **Write-only ingestion shapes.** Cache-assigned IDs, fire-and-
  forget append patterns, payload-cap variations — addon territory.
- **Eviction-hint queries.** Sorting scopes by read-heat to
  recommend which to drop — addon, built on `/stats` and §8 data.

### 9.2 Not in core: operator policy

- **Access control.** Which clients reach which endpoints is a
  transport-layer decision. Caddyfile route guards (or nginx /
  apache equivalents), Unix-socket filesystem permissions,
  separate listeners per trust level — all lives outside the
  cache.
- **Reserved-scope enforcement.** The `_*` prefix is a social
  convention (§2.3), not a core check. Addons that want their
  state-scopes protected from public writes rely on operator
  gating, naming convention, or payload-side validation.
- **Network exposure.** The cache speaks HTTP on whatever the
  adapter mounts it on (Unix socket for the standalone binary,
  Caddy listener for the module). The adapter and the operator
  jointly decide what's reachable from where.

### 9.3 Not in core: non-goals

- **Persistence.** The core is in-memory only. Process restart
  clears everything; rebuild from the source of truth. Addons
  could be built to periodically drain to disk, a database, or a
  remote store — but that lives outside the core.
- **TTL or background eviction.** No scheduler, no LRU, no LFU,
  no time-based expiration. Whatever you write stays until you
  delete it.
- **Payload-content filters.** No queries against payload contents,
  no indexes on payload fields, no joins. Anything beyond
  `scope`/`id`/`seq` belongs in the source-of-truth or in an
  application layer above the cache.
- **Cross-instance replication or coordination.** Each scopecache
  process is independent. Multi-instance deployments coordinate at
  the source-of-truth layer, not in the cache.
