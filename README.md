# inmem-cache

A small, local, rebuildable in-memory cache over a Unix domain socket. Written in Go, stdlib-only, no external dependencies.

## What it is

- A scope-first hot-window cache / write-buffer that sits in front of your real data store. (A *scope* is what other systems call a **namespace** or **bucket** — conceptually comparable to a **table** in SQL terms: every item lives inside exactly one.)
- Typical use: keep a hot slice of your data in RAM so it does not have to be re-queried from the database on every request. For example, the replies to a forum topic, the recent chat messages of a given user, or a rolling feed per session — each lives in its own scope and can be served directly from memory.
- Also usable as a write-buffer: append high-frequency events (analytics hits, log lines, chat messages) to a scope, and let a background worker drain the buffer every few seconds with a single bulk insert against the database. This flattens write spikes and keeps the DB from being hammered on every request. `/delete-up-to` exists specifically for this drain pattern — trim the cache up to the last seq you committed.
- Wipeable and rebuildable at any time — the source of truth lives elsewhere (a database, a JSON file, data built in code, anything).
- Tuned for modest VPS footprints (~1 GB RAM alongside DB + app), with a 100 MiB default store cap.
- **Extremely fast**: sub-50-nanosecond reads on a populated store (see [Performance](#performance)).
- **It can serve cached content directly to the HTTP edge.** The `/render` endpoint returns the raw payload bytes — no JSON envelope, no wrapper — so a fronting proxy (Caddy, nginx, apache) can pipe cached HTML pages, XML documents, JSON API responses or text fragments straight to the browser or API client. No application layer in between, no deserialize-and-reserialize round-trip. With Redis/memcached-style caches an application always has to sit in the middle to translate their reply into HTTP. 
- inmem-cache is intentionally simple. Filtering and addressing are deliberately limited to three fields — `scope`, `id`, `seq` — and that limitation is the whole point: it is what keeps the cache fast and easy to reason about. There is no rich query language, but because `scope` and `id` are free-form strings the client fully controls, a surprisingly wide set of access patterns can be modeled on top of them.
- inmem-cache is small enough to stay fully comprehensible, yet rich enough to carry a wide range of useful patterns. Its strength lies in the combination of simplicity, practical usefulness, and ease of deployment: a small, local, in-memory scope-first cache and write-buffer that is intentionally limited in surface area, yet flexible enough to support hot-read caching, lightweight write-buffer workflows, and direct response serving — all built from the same small set of core primitives. Cached HTML, XML, or JSON can be rendered straight to the wire, allowing a fronting webserver to present content directly from the cache without any intermediate application layer.
- inmem-cache is deliberately limited in capability, yet flexible enough to cover a wide range of real-world use cases. Proposals to expand the query surface or make the cache "smarter" — automatic TTL, eviction, background policies — are scrutinised hard: anything added here competes directly with the simplicity and predictability that make the cache fast and easy to reason about.

## What it is not

- A database, search engine, analytics store, or generic query engine.
- A business-logic layer.
- Payloads are opaque JSON — the cache never inspects, parses, or searches inside them.

## Architecture

Three layers with clear boundaries:

- **Core** — `package inmemcache`. The cache engine itself. Stdlib-only, framework-agnostic, caller-anonymous: it registers HTTP routes on a standard `*http.ServeMux` and knows nothing about auth, identity, or who is calling. This is what the [spec](inmem-cache-compact-rfc.md) describes.
- **Standalone adapter** — `cmd/inmem-cache/`. Thin binary that reads env vars, opens a Unix socket, and serves the core. What you use if you're running behind nginx/apache, or with no reverse proxy at all.
- **Caddy-module adapter** — `caddymodule/` (Phase 3, planned). Wraps the core as a Caddy module. Also the home for cross-cutting concerns that require request context: auth enforcement, identity-to-scope mapping, per-tenant logging and metrics.

The rule: new **cache features** go into the core. **Cross-cutting concerns** (auth, identity, per-tenant policy) go into an adapter. This keeps the core small and refactorable, keeps both adapters symmetrical, and means cache semantics cannot drift between standalone and Caddy deployments.

## Status

Phase 2 — core logic lives in `package inmemcache` at the repo root; the standalone binary is in `cmd/inmem-cache/`. A Caddy-module wrapper (Phase 3) is planned.

## Quickstart (Docker)

```bash
docker compose up --build inmem-cache
```

The service listens on `/run/inmem.sock` inside the container (mounted to the host volume defined in `docker-compose.yml`).

## Usage

Every request hits the Unix socket, so `curl` needs `--unix-socket` and a dummy `http://localhost` host.

### Append an item

```bash
curl -s --unix-socket /run/inmem.sock -X POST http://localhost/append \
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
curl -s --unix-socket /run/inmem.sock \
  "http://localhost/get?scope=thread:900&id=post_1"
```

Or by `seq`:

```bash
curl -s --unix-socket /run/inmem.sock \
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
curl -s --unix-socket /run/inmem.sock -X POST http://localhost/append \
  -H "Content-Type: application/json" \
  -d '{
    "scope": "pages",
    "id": "home",
    "payload": "<html><body>Hi</body></html>"
  }'
```

Serve it raw:

```bash
curl -s --unix-socket /run/inmem.sock "http://localhost/render?scope=pages&id=home"
# → <html><body>Hi</body></html>
```

Contract: hit returns `200` with the raw payload bytes; miss returns `404` with an empty body. Both use `Content-Type: application/octet-stream` — the fronting proxy is expected to override it for browser-facing deployments. When the payload is a JSON string (HTML/XML/text), one layer of JSON encoding is peeled; other JSON values (object, array, number, bool) are written raw.

### Other endpoints

`/head`, `/tail`, `/warm`, `/rebuild`, `/update`, `/delete`, `/delete-up-to`, `/delete-scope`, `/delete-scope-candidates`, `/stats`, `/help` — see section 12 of the [spec](inmem-cache-compact-rfc.md) for full examples.

## Configuration

All overrides via environment variables:

| Variable                 | Default              | Purpose                               |
|--------------------------|----------------------|---------------------------------------|
| `INMEM_SOCKET_PATH`      | `/run/inmem.sock`    | Listening socket path                 |
| `INMEM_SCOPE_MAX_ITEMS`  | `100000`             | Max items per scope                   |
| `INMEM_MAX_STORE_MB`     | `100`                | Store-wide byte cap (integer MiB)     |

## Limits

Two independent caps apply, either violation returns **HTTP 507 Insufficient Storage** — the cache never evicts on its own. Clients free capacity via `/delete-up-to`, `/delete-scope`, or a fitting `/warm`/`/rebuild`.

- **Per-scope item cap** — 100,000 items (default).
- **Store-wide byte cap** — 100 MiB aggregate (default).
- **Per-item cap** — 1 MiB (enforced on the approximate item size: overhead + scope + id + payload).

## Performance

Single-item read on a ~57 MiB dataset (100 scopes × 1000 items × ~580 B/item):

| Benchmark                        | Time/op | Allocs/op |
|----------------------------------|---------|-----------|
| `GetByID`                        | ~32 ns  | 0         |
| `GetBySeq`                       | ~27 ns  | 0         |
| `GetByID` (parallel, 32 cores)   | ~29 ns  | 0         |

That's roughly **30 million reads per second per core**, and the scope-level `RWMutex` does not serialize readers, so throughput scales with cores.

Measured with `go test -bench=. -benchtime=3s` on an AMD Ryzen AI Max+ 395 (Linux, Go 1.23). Numbers are in-process Go lookups — HTTP and Unix-socket overhead is additional but small (stdlib `net/http` + `net.UnixConn`, no JSON transformation on the read path beyond marshaling the item).

Reproduce with:

```bash
go test -bench=. -benchmem -benchtime=3s -run=^$ ./...
```

## Building from source

```bash
go build -o inmem-cache ./cmd/inmem-cache
go test ./...
```

Module path: `github.com/DenverCoding/inmem-cache`. Stdlib only.

## Spec

The full design and endpoint contract lives in [inmem-cache-compact-rfc.md](inmem-cache-compact-rfc.md).

## License

TBD.
