# inmem-cache

A small, local, rebuildable in-memory cache over a Unix domain socket. Written in Go, stdlib-only, no external dependencies.

## What it is

- A scope-first hot-window cache / write-buffer that sits in front of your real data store.
- Wipeable and rebuildable at any time — the source of truth lives elsewhere (a database, a JSON file, data built in code, anything).
- Tuned for modest VPS footprints (~1 GB RAM alongside DB + app), with a 100 MiB default store cap.
- **Extremely fast**: sub-50-nanosecond reads on a populated store (see [Performance](#performance)).

## What it is not

- A database, search engine, analytics store, or generic query engine.
- A business-logic layer.
- Payloads are opaque JSON — the cache never inspects, parses, or searches inside them.

Filtering and addressing are limited to three fields: `scope`, `id`, `seq`.

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
