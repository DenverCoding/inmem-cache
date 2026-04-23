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
- scopecache is intentionally simple. Addressing is limited to `scope`, `id`, and `seq`; filtering adds one more axis, an optional client-supplied `ts` (int64) that `/ts_range` treats as a time-window filter. That deliberate shortness is the whole point: it is what keeps the cache fast and easy to reason about. There is no rich query language, but because `scope` and `id` are free-form strings the client fully controls — and `ts` opens server-side time windows without dragging payloads into the query surface — a surprisingly wide set of access patterns can be modeled on top of them.
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
- **Caddy-module adapter** — `caddymodule/` (Phase 3). Published as a separate Go module (`github.com/VeloxCoding/scopecache/caddymodule`) so consumers of the core never pull in Caddy's dep tree. Wraps the core as a Caddy HTTP handler (`http.handlers.scopecache`), exposed in Caddyfile syntax and JSON config. This is also the home for cross-cutting concerns that require request context: auth enforcement, identity-to-scope mapping, per-tenant logging and metrics.

The rule: new **cache features** go into the core. **Cross-cutting concerns** (auth, identity, per-tenant policy) go into an adapter. This keeps the core small and refactorable, keeps both adapters symmetrical, and means cache semantics cannot drift between standalone and Caddy deployments.

## Status

Phase 3 — core in `package scopecache` (repo root), standalone binary in `cmd/scopecache/`, Caddy adapter in `caddymodule/` (its own Go module so the core stays stdlib-only for non-Caddy consumers).

## Quickstart (Docker)

Standalone, listening on a Unix domain socket:

```bash
docker compose up --build scopecache
```

The service listens on `/run/scopecache.sock` inside the container (mounted to the host volume defined in `docker-compose.yml`).

As a Caddy module, listening on TCP :8081:

```bash
docker compose up --build caddyscope
curl http://localhost:8081/stats
```

## Quickstart (Caddy / FrankenPHP via xcaddy)

Build a Caddy binary with scopecache baked in:

```bash
xcaddy build --with github.com/VeloxCoding/scopecache/caddymodule@v0.1.0
```

Combine with FrankenPHP in one binary:

```bash
xcaddy build \
    --with github.com/dunglas/frankenphp/caddy \
    --with github.com/VeloxCoding/scopecache/caddymodule@v0.1.0
```

Minimal Caddyfile:

```caddyfile
:8080 {
    scopecache {
        scope_max_items 100000
        max_store_mb    100
        max_item_mb     1
    }
    respond 404
}
```

All three `scopecache { ... }` subdirectives are optional; omit any of them to fall back to the compile-time default. See [Caddyfile.caddyscope](Caddyfile.caddyscope) for a working example and [Dockerfile.caddyscope](Dockerfile.caddyscope) for the xcaddy build recipe.

## Quickstart (Linux VPS)

Two install routes — pick one, then the **Running**, **systemd**, and **proxy** sections below apply identically to both.

### Install

**Option A — prebuilt binary (recommended).** Statically linked (`CGO_ENABLED=0`), runs on any Linux distro — Debian, Ubuntu, Alpine, scratch containers — with no glibc or Go dependency.

```bash
# Pick amd64 or arm64 to match your machine:
wget https://github.com/VeloxCoding/scopecache/releases/latest/download/scopecache-linux-amd64
chmod +x scopecache-linux-amd64
sudo mv scopecache-linux-amd64 /usr/local/bin/scopecache
```

Verify the download against the published `SHA256SUMS` file on the same [Releases page](https://github.com/VeloxCoding/scopecache/releases).

**Option B — build from source.** Requires **Go 1.25+** — install or upgrade via [go.dev/dl/](https://go.dev/dl/). The core is stdlib-only, so the build has no external dependencies beyond Go and git.

```bash
git clone https://github.com/VeloxCoding/scopecache.git
cd scopecache
go build -o scopecache ./cmd/scopecache
sudo mv scopecache /usr/local/bin/scopecache
```

### Running

As root, the default socket path `/run/scopecache.sock` works out of the box:

```bash
scopecache &
```

As a non-root user, override the socket path to a user-owned location. `$XDG_RUNTIME_DIR` (typically `/run/user/<uid>/`) is the idiomatic choice — tmpfs-backed, user-owned, auto-cleaned on logout:

```bash
SCOPECACHE_SOCKET_PATH="${XDG_RUNTIME_DIR}/scopecache.sock" scopecache &
```

Smoke test:

```bash
curl --unix-socket /run/scopecache.sock http://localhost/help
curl -s --unix-socket /run/scopecache.sock http://localhost/stats
```

Full end-to-end suite (75 assertions over every endpoint):

```bash
bash e2e_test.sh
```

### Running under systemd

For production, run scopecache as its own unprivileged system user under systemd. Create `/etc/systemd/system/scopecache.service`:

```ini
[Unit]
Description=scopecache
After=network.target

[Service]
Type=simple
User=scopecache
Group=scopecache
ExecStart=/usr/local/bin/scopecache
Environment=SCOPECACHE_SOCKET_PATH=/run/scopecache/scopecache.sock
Environment=SCOPECACHE_SCOPE_MAX_ITEMS=100000
Environment=SCOPECACHE_MAX_STORE_MB=100
Environment=SCOPECACHE_MAX_ITEM_MB=1
RuntimeDirectory=scopecache
RuntimeDirectoryMode=0750
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

All four `SCOPECACHE_*` variables are listed explicitly so every capacity knob is visible and tunable in one place — remove any line to fall back to the compile-time default. The socket lives under a systemd-managed `RuntimeDirectory` (`/run/scopecache/`), which is created at service start and cleaned up on stop.

Then:

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin scopecache
sudo systemctl daemon-reload
sudo systemctl enable --now scopecache
```

### Connecting from Caddy / nginx / apache

Two steps: (1) grant the proxy user read access to the socket by adding it to the `scopecache` group, and (2) point the proxy at the Unix socket.

```bash
sudo usermod -aG scopecache caddy      # Caddy
sudo usermod -aG scopecache www-data   # nginx or apache (Debian/Ubuntu)
```

**Caddy** (`Caddyfile`):

```caddyfile
:8080 {
    reverse_proxy unix//run/scopecache/scopecache.sock
}
```

**nginx** (site config):

```nginx
upstream scopecache {
    server unix:/run/scopecache/scopecache.sock;
}

server {
    listen 8080;
    location / {
        proxy_pass http://scopecache;
    }
}
```

**apache** (site config; requires `mod_proxy` and `mod_proxy_http`, apache 2.4.9+):

```apache
ProxyPass        "/" "unix:/run/scopecache/scopecache.sock|http://localhost/"
ProxyPassReverse "/" "unix:/run/scopecache/scopecache.sock|http://localhost/"
```

If you prefer scopecache as a **native Caddy handler** instead of proxying to the standalone binary over a socket, see the [xcaddy quickstart](#quickstart-caddy--frankenphp-via-xcaddy) above — it eliminates the socket hop entirely.

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

`/head`, `/tail`, `/ts_range`, `/warm`, `/rebuild`, `/update`, `/upsert`, `/counter_add`, `/delete`, `/delete-up-to`, `/delete-scope`, `/wipe`, `/delete-scope-candidates`, `/stats`, `/help` — see section 13 of the [spec](scopecache-rfc.md) for full examples.

`/ts_range` filters a scope by a client-supplied top-level `ts` (signed int64, milliseconds since unix epoch by convention — the cache is opaque to the unit). At least one of `since_ts` / `until_ts` must be provided; both together form an inclusive `[since_ts, until_ts]` window. Items without a `ts` are excluded. Results are returned in ascending `seq` order — `ts` is a filter, not an ordering key. Responses on `/head`, `/tail`, and `/ts_range` carry `{ "ok", "items", "truncated" }`; `truncated: true` means more matching items exist beyond the returned `limit`. `/ts_range` has **no pagination cursor** because `ts` is mutable (via `/update` / `/upsert`) and non-unique — narrow the window or raise `limit` to fetch more. `ts` is optional on `/append`, `/warm`, `/rebuild`, `/update` (absent = preserve) and `/upsert` (absent = clear, matching its whole-item replace semantics).

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
| `SCOPECACHE_MAX_ITEM_MB`       | `1`                      | Per-item size cap (integer MiB)       |

## Limits

Three independent caps apply; any violation returns **HTTP 507 Insufficient Storage** (per-scope, store-wide) or **HTTP 400 Bad Request** (per-item shape rule). The cache never evicts on its own — clients free capacity via `/delete-up-to`, `/delete-scope`, `/wipe`, or a fitting `/warm`/`/rebuild`.

- **Per-scope item cap** — 100,000 items (default).
- **Store-wide byte cap** — 100 MiB aggregate (default).
- **Per-item cap** — 1 MiB (default); enforced on the approximate item size (overhead + scope + id + payload). Raise it via `SCOPECACHE_MAX_ITEM_MB` when the use-case stores larger blobs (rendered HTML, large JSON documents).

## Eviction / TTL

scopecache has no per-item TTL and no background eviction thread. This is deliberate: the cache is rebuildable-from-source by design, so the intended time-based eviction pattern is a **periodic `/warm` or `/rebuild`** — the client queries the source (`WHERE sent_at > now() - 24h`, or whatever window fits) and hands the resulting slice to scopecache in one atomic call. Items that fall outside the window simply disappear at the next refresh. This matches the materialized-view mental model (Postgres matviews, ISR-style static regeneration, build-artifact caches) rather than the Redis/Memcached per-key TTL model. The tradeoff is bursty (rebuilds re-allocate the scope in one shot) where TTL-based eviction is smoother — but in return you get atomic consistency: a scope is always either the previous complete slice or the new one, never half-expired.

Items *can* carry an optional client-supplied `ts` (int64), but it is a **query primitive, not an eviction signal**: `/ts_range` uses it to filter a server-side time window (e.g. "messages for user:x in the last hour"), and the cache itself never reads `ts` to decide when to delete anything. Scope-level cleanup is operator-initiated via `/delete-scope-candidates`, which surfaces idle scopes ranked by cache-owned `last_access_ts`.

**Caveat when a scope is used as a write-buffer.** `/rebuild` and `/warm` replace scope contents wholesale from whatever the client passes in. If the drain worker has not yet committed pending appends to the source of truth, rebuilding from that source will silently erase them — the undrained items are not there yet. Either drain first (trim with `/delete-up-to` after a successful commit, *then* rebuild if needed), or keep write-buffer scopes separate from cache scopes that are periodically rebuilt. The two patterns compose cleanly on *different* scopes; on the same scope they are mutually exclusive.

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

### Time-window filtering (`/ts_range`)

`/ts_range` performs an unindexed linear scan over the scope's items — no secondary index on `ts` is maintained. Two shapes worth measuring: a realistic scope where the scan early-exits on `limit`, and a worst-case scope where the filter window forces the scan to traverse every item.

| Benchmark                     | Items in scope | Scan work                       | Time/op  | Allocs/op |
|-------------------------------|----------------|----------------------------------|----------|-----------|
| `TsRange_Realistic`           | 2,000 (all match) | ~1,001 iterations (early-exit) | ~7.3 µs  | 1         |
| `TsRange_FullScope_Worst`     | 100,000 (matches clustered at tail) | full 100,000 iterations | ~119 µs  | 1         |

**Why no `ts` index.** A secondary index would trade sub-millisecond read cost for non-trivial write-path overhead — index maintenance on every `/append`, `/update`, `/upsert`, `/warm`, and `/rebuild`, plus the extra memory to hold it. The scan is already fast enough that this trade is not warranted: even the pathological case on a maxed-out 100,000-item scope completes in ~120 µs, well below the ~100 µs HTTP framing floor (see the end-to-end table below). A client on the Unix socket therefore cannot observe the scan cost on top of the request round-trip. Keeping the read path unindexed keeps the write path cheap and the store small — which is the whole point of the design (§4 of the [spec](scopecache-rfc.md)).

The single allocation per call is the output slice, sized to `limit` (~72 KiB at the default `limit=1000`). The scan loop itself allocates nothing.

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

### Memory footprint

The standalone binary uses ~4.5 MB resident RAM at startup with an empty cache, and stabilises around ~9 MB after serving load (Go runtime + goroutine stacks). Cache data adds `approxItemSize` per item on top. At the default 100 MiB store cap the total RSS ceiling is roughly ~110 MB.

Measured with `/proc/<pid>/status` on linux/amd64, Go 1.21, static binary (`CGO_ENABLED=0`).

## Building from source

```bash
go build -o scopecache ./cmd/scopecache
go test ./...
```

Module path: `github.com/VeloxCoding/scopecache`. Stdlib only.

## Testing

All tests are stdlib-only and run without Docker. A few commands cover the full matrix:

```bash
go test ./...                                                   # unit + integration
go test -race ./...                                             # same, with race detector
go test -cover ./...                                            # coverage summary
go test -run=^$ -bench=. -benchmem ./...                        # benchmarks
go test -run=^$ -fuzz=FuzzValidateWriteItem -fuzztime=30s       # one fuzz target
docker compose exec dev sh //src//e2e_test.sh                   # E2E over the real Unix socket
```

What each file covers:

- [store_test.go](store_test.go) — core store/buffer invariants: append, upsert, counter, delete, rebuild, detach-on-replace.
- [handlers_test.go](handlers_test.go) — HTTP contract: status codes, JSON envelopes, validation errors.
- [validation_test.go](validation_test.go) — input-validation boundary (scope/id shape, payload presence, counter ranges, hours overflow).
- [fuzz_test.go](fuzz_test.go) — property-based coverage of the validators with Go's native fuzzer.
- [stress_test.go](stress_test.go) — 16 goroutines × mixed ops, with a post-run invariant check.
- [bench_test.go](bench_test.go), [bench_http_test.go](bench_http_test.go) — in-process and HTTP throughput benchmarks (see [Performance](#performance)).
- [e2e_test.sh](e2e_test.sh) — end-to-end curl suite against the live Unix socket.

The exact contracts these tests enforce are listed in §10 of the [spec](scopecache-rfc.md).

## Spec

The full design and endpoint contract lives in [scopecache-rfc.md](scopecache-rfc.md).

## License

Licensed under the [Apache License, Version 2.0](LICENSE). See the `LICENSE` file for the full text.

Copyright 2026 VeloxCoding.
