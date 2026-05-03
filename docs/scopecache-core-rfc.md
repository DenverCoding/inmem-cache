# scopecache — Core RFC

> **Status: in progress.** Pass 1 (§1–§3) has landed. Passes 2–4
> follow. Until the document is complete,
> [scopecache-rfc-old.md](scopecache-rfc-old.md) remains the most
> complete reference for the cache's contract — but with the
> understanding that its §6.3, §6.4, §13.17, and §13.19–§13.23
> describe endpoints that have left the core in v0.7.17.

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
