# scopecache

A small, local, in-memory cache and write buffer written in Go — stdlib only, served over a Unix domain socket.
Tuned for modest VPS footprints, delivering around 10,000 HTTP requests per second per core and well over 100,000 per second aggregate under concurrent load.
Data lives in scopes (namespaces) and is addressable only by scope, id, or seq; the entire cache is disposable and can be wiped and rebuilt from the source of truth at any time.
Payloads can also be served directly via `/render`, allowing Caddy, nginx, or Apache to send cached HTML, JSON, or XML straight to the client without an application layer in between. This can substantially reduce per-request overhead and allow a single server to handle far more traffic on cacheable paths.

## What it is

- A scope-first hot-window cache / write-buffer that sits in front of your real data store. (A *scope* is what other systems call a **namespace** or **bucket** — conceptually comparable to a **table** in SQL terms: every item lives inside exactly one.)
- Typical use: keep a hot slice of your data in RAM so it does not have to be re-queried from the database on every request. For example, the replies to a forum topic, the recent chat messages of a given user, or a rolling feed per session — each lives in its own scope and can be served directly from memory.
- Also usable as a write-buffer: append high-frequency events (analytics hits, log lines, chat messages) to a scope, and let a background worker drain the buffer every few seconds with a single bulk insert against the database. This flattens write spikes and keeps the DB from being hammered on every request. `/delete-up-to` exists specifically for this drain pattern — trim the cache up to the last seq you committed.
- Wipeable and rebuildable at any time — the source of truth lives elsewhere (a database, a JSON file, data built in code, anything).
- Tuned for modest VPS footprints (~1 GB RAM alongside DB + app), with a 100 MiB default store cap.
- **Extremely fast**: around 10,000 HTTP requests per second per core over the Unix socket and over 100,000 per second aggregate under concurrent load, backed by a cache core that answers in-process lookups in sub-50 nanoseconds (see [Performance](#performance)).
- **It can serve cached content directly to the HTTP edge.** The `/render` endpoint returns the raw payload bytes — no JSON envelope, no wrapper — so a fronting proxy (Caddy, nginx, apache) can pipe cached HTML pages, XML documents, JSON API responses or text fragments straight to the browser or API client. No application layer in between, no deserialize-and-reserialize round-trip. With Redis/memcached-style caches an application always has to sit in the middle to translate their reply into HTTP. 
- scopecache is intentionally simple. Filtering and addressing are deliberately limited to three fields — `scope`, `id`, `seq` — and that limitation is the whole point: it is what keeps the cache fast and easy to reason about. There is no rich query language, but because `scope` and `id` are free-form strings the client fully controls, a surprisingly wide set of access patterns can be modeled on top of them.
- scopecache is small enough to stay fully comprehensible, yet rich enough to carry a wide range of useful patterns. Its strength lies in the combination of simplicity, practical usefulness, and ease of deployment: a small, local, in-memory scope-first cache and write-buffer that is intentionally limited in surface area, yet flexible enough to support hot-read caching, lightweight write-buffer workflows, and direct response serving — all built from the same small set of core primitives. Cached HTML, XML, or JSON can be rendered straight to the wire, allowing a fronting webserver to present content directly from the cache without any intermediate application layer.
- scopecache is deliberately limited in capability, yet flexible enough to cover a wide range of real-world use cases. Proposals to expand the query surface or make the cache "smarter" — automatic TTL, eviction, background policies — are scrutinised hard: anything added here competes directly with the simplicity and predictability that make the cache fast and easy to reason about.

## What it is not

- A database, search engine, analytics store, or generic query engine.
- A business-logic layer.
- Payloads are opaque JSON — the cache never inspects, parses, or searches inside them.

## Architecture

Three layers with clear boundaries:

- **Core** — `package scopecache`. The cache engine itself. Stdlib-only, framework-agnostic, caller-anonymous: it registers HTTP routes on a standard `*http.ServeMux` and knows nothing about auth, identity, or who is calling. This is what the [spec](scopecache-rfc.md) describes.
- **Standalone adapter** — `cmd/scopecache/`. Thin binary that reads env vars, opens a Unix socket, and serves the core. What you use if you're running behind nginx/apache, or with no reverse proxy at all.
- **Caddy-module adapter** — `caddymodule/` (Phase 3, planned). Wraps the core as a Caddy module. Also the home for cross-cutting concerns that require request context: auth enforcement, identity-to-scope mapping, per-tenant logging and metrics.

The rule: new **cache features** go into the core. **Cross-cutting concerns** (auth, identity, per-tenant policy) go into an adapter. This keeps the core small and refactorable, keeps both adapters symmetrical, and means cache semantics cannot drift between standalone and Caddy deployments.

## Status

Phase 2 — core logic lives in `package scopecache` at the repo root; the standalone binary is in `cmd/scopecache/`. A Caddy-module wrapper (Phase 3) is planned.

## Quickstart (Docker)

```bash
docker compose up --build scopecache
```

The service listens on `/run/scopecache.sock` inside the container (mounted to the host volume defined in `docker-compose.yml`).

## Usage

Every request hits the Unix socket, so `curl` needs `--unix-socket` and a dummy `http://localhost` host.

### Append an item

```bash
curl -s --unix-socket /run/scopecache.sock -X POST http://localhost/append \
  -H "Content-Type: application/json" \
  -d '{
    "scope": "thread:900",
    "id": "post_1",
    "payload": { "text": "hello" }
  }'
```

Response:

```json
{
  "ok": true,
  "item": {
    "scope": "thread:900",
    "id": "post_1",
    "seq": 1,
    "payload": { "text": "hello" }
  }
}
```

`seq` is assigned by the cache — clients never send it on writes.

### Get it back

By `id`:

```bash
curl -s --unix-socket /run/scopecache.sock \
  "http://localhost/get?scope=thread:900&id=post_1"
```

Or by `seq`:

```bash
curl -s --unix-socket /run/scopecache.sock \
  "http://localhost/get?scope=thread:900&seq=1"
```

Hit response:

```json
{
  "ok": true,
  "hit": true,
  "item": {
    "scope": "thread:900",
    "id": "post_1",
    "seq": 1,
    "payload": { "text": "hello" }
  }
}
```

Miss response:

```json
{ "ok": true, "hit": false, "item": null }
```

### Render one item (raw payload, no envelope)

`/render` serves a single item's payload as raw bytes — no JSON wrapper around it. Use it to serve cached HTML, XML, JSON or text fragments directly from the cache (typically fronted by Caddy, nginx or apache, which sets the real `Content-Type`).

Store an HTML fragment as a JSON string:

```bash
curl -s --unix-socket /run/scopecache.sock -X POST http://localhost/append \
  -H "Content-Type: application/json" \
  -d '{
    "scope": "pages",
    "id": "home",
    "payload": "<html><body>Hi</body></html>"
  }'
```

Serve it raw:

```bash
curl -s --unix-socket /run/scopecache.sock "http://localhost/render?scope=pages&id=home"
# → <html><body>Hi</body></html>
```

Contract: hit returns `200` with the raw payload bytes; miss returns `404` with an empty body. Both use `Content-Type: application/octet-stream` — the fronting proxy is expected to override it for browser-facing deployments. When the payload is a JSON string (HTML/XML/text), one layer of JSON encoding is peeled; other JSON values (object, array, number, bool) are written raw.

### Other endpoints

`/head`, `/tail`, `/warm`, `/rebuild`, `/update`, `/upsert`, `/counter_add`, `/delete`, `/delete-up-to`, `/delete-scope`, `/wipe`, `/delete-scope-candidates`, `/stats`, `/help` — see section 12 of the [spec](scopecache-rfc.md) for full examples.

`/upsert` creates a new item or replaces an existing one by `scope` + `id`. It is the idempotent, retry-safe write path: unlike `/append` (which rejects duplicate ids) or `/update` (which soft-misses on absent items), `/upsert` always writes. `seq` is preserved on replace and freshly assigned on create. The response includes `"created": true` for a fresh item and `false` for a replace.

`/counter_add` atomically adds a signed int64 `by` to the integer counter at `scope` + `id`, auto-creating the counter with starting value `by` if it does not exist. Both paths run under a single scope write-lock — no client-side read-modify-write, so concurrent increments do not lose updates. The stored payload is a bare JSON integer (e.g. `42`), so `/get`, `/render`, `/upsert` and `/update` all see the same value. `by` is required and non-zero; both `by` and the result must lie within ±(2^53−1) (the JavaScript safe-integer range, so values round-trip through every client without precision loss). Reads and absolute sets go through the normal `/get`, `/upsert` and `/update` endpoints — only `/counter_add` parses the payload as an integer. Responses carry `{"ok", "created", "value"}`. If the existing item at `scope` + `id` is *not* a JSON integer within the allowed range, the request is rejected with `409 Conflict` — counters do not silently overwrite other payload types.

`/wipe` clears the entire store in one atomic call: every scope, every item, every byte reservation. It takes no request body. The response carries `{"ok", "deleted_scopes", "deleted_items", "freed_mb"}` so a client can verify what was released. The store-wide complement of `/delete-scope` — useful for test teardown, emergency reset, or preparing a fresh slate before a `/rebuild`. The cache never wipes on its own; this is explicitly a client-initiated action.

## Configuration

All overrides via environment variables:

| Variable                       | Default                  | Purpose                               |
|--------------------------------|--------------------------|---------------------------------------|
| `SCOPECACHE_SOCKET_PATH`       | `/run/scopecache.sock`   | Listening socket path                 |
| `SCOPECACHE_SCOPE_MAX_ITEMS`   | `100000`                 | Max items per scope                   |
| `SCOPECACHE_MAX_STORE_MB`      | `100`                    | Store-wide byte cap (integer MiB)     |

## Limits

Two independent caps apply, either violation returns **HTTP 507 Insufficient Storage** — the cache never evicts on its own. Clients free capacity via `/delete-up-to`, `/delete-scope`, or a fitting `/warm`/`/rebuild`.

- **Per-scope item cap** — 100,000 items (default).
- **Store-wide byte cap** — 100 MiB aggregate (default).
- **Per-item cap** — 1 MiB (enforced on the approximate item size: overhead + scope + id + payload).

## Performance

Two distinct numbers are worth reporting: the *core ceiling* (how fast the cache itself is when called in-process) and the *end-to-end throughput* a real client sees over the Unix socket. Both are measured on a populated 100 scope × 1000 item × ~580 B/item dataset (~57 MiB).

### In-process lookups (cache core)

Single-item read, direct function call against the store — no HTTP layer:

| Benchmark                        | Time/op | Allocs/op |
|----------------------------------|---------|-----------|
| `GetByID`                        | ~32 ns  | 0         |
| `GetBySeq`                       | ~27 ns  | 0         |
| `GetByID` (parallel, 32 cores)   | ~29 ns  | 0         |

That is roughly **30 million reads per second per core**. The scope-level `RWMutex` does not serialize readers, so throughput scales with cores. These numbers describe the ceiling of the cache core itself, not the rate a client sees over the socket.

### HTTP over Unix socket (end-to-end)

What an actual client pays: the full request path through `net/http`, the Unix-socket syscalls, JSON encoding/decoding where applicable, and the same scope-lock acquisition as above. Keep-alive connections are reused (standard `http.Client` pool behaviour).

| Benchmark                              | Time/op  | Throughput                   |
|----------------------------------------|----------|------------------------------|
| `HTTP_GetByID` (serial)                | ~99 µs   | ~10,000 req/s per core       |
| `HTTP_GetByID` (parallel, 32 cores)    | ~8.8 µs  | ~114,000 req/s aggregate     |
| `HTTP_RenderByID` (serial)             | ~103 µs  | ~9,700 req/s per core        |
| `HTTP_RenderByID` (parallel, 32 cores) | ~8.1 µs  | ~124,000 req/s aggregate     |
| `HTTP_Head10` (serial, `limit=10`)     | ~168 µs  | ~6,000 req/s per core        |
| `HTTP_Append` (serial)                 | ~142 µs  | ~7,000 req/s per core        |

A few things fall out of these numbers:

- **HTTP framing, not the cache, sets the floor.** Per-request cost is dominated by `net/http` + syscall overhead. The cache work itself is still in the tens of nanoseconds.
- **`/render` vs `/get` are near-identical at this payload size.** The JSON envelope costs ~4 µs serial at ~580 B payloads — negligible relative to the ~100 µs total. The gap widens for larger payloads where the envelope's per-byte marshalling cost grows.
- **Parallel scaling is ~12-15×** on 32 cores, not linear. The listener's accept serialization, connection-pool coordination, and GC cycles account for the gap. Throughput does scale — just not in proportion to cores.
- **Writes are ~1.5× the cost of reads**, which matches expectations: `/append` takes a scope write-lock plus an atomic store-bytes CAS, so it cannot run concurrently against the same scope the way reads can.

Measured with `go test -bench=. -benchtime=3s` on an AMD Ryzen AI Max+ 395 (Linux, Go 1.23).

Reproduce with:

```bash
go test -bench=. -benchmem -benchtime=3s -run=^$ ./...
```

## Building from source

```bash
go build -o scopecache ./cmd/scopecache
go test ./...
```

Module path: `github.com/DenverCoding/scopecache`. Stdlib only.

## Spec

The full design and endpoint contract lives in [scopecache-rfc.md](scopecache-rfc.md).

## License

Licensed under the [Apache License, Version 2.0](LICENSE). See the `LICENSE` file for the full text.

Copyright 2026 DenverCoding.
