# scopecache

A small, disposable, scope-addressed in-memory cache and write buffer
written in Go. Stdlib-only core, served over HTTP — Unix socket
(standalone) or TCP (as a Caddy module).

## What it is

scopecache holds a hot slice of your data in RAM in front of your real
data store. Items live inside *scopes* — what other systems call a
namespace or bucket — and are addressable only by `scope`, `id`, or
`seq`. The entire cache is wipeable and rebuildable from the source of
truth at any time. There is no on-disk state, no TTL, no eviction
policy, no application logic.

Two main use cases:

- **Hot-read cache.** Keep frequently queried fragments in memory so
  they don't hit the database on every request. A fronting proxy
  (Caddy, nginx, apache) can serve cached HTML, JSON, or XML straight
  from `/render` without any application layer in between.
- **Write buffer.** Append high-frequency events (analytics hits, log
  lines, chat messages); a background worker drains the buffer in
  batches via `/tail` + `/delete_up_to`.

The core is intentionally limited to a small set of HTTP endpoints
(read, write, bulk, observe) and three filter axes (`scope`, `id`,
`seq`). No query language, no joins, no payload inspection. Anything
beyond the core — multi-tenant gateways, batch dispatchers, write-only
ingestion, custom auth — is built as separate add-on sub-packages on
top of the core's public Go API.

## Quickstart

**As a Caddy module** (TCP):

```bash
xcaddy build --with github.com/VeloxCoding/scopecache/caddymodule@latest
```

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

**As a standalone binary** (Unix socket, no Caddy):

```bash
docker compose up --build scopecache
curl --unix-socket /run/scopecache.sock http://localhost/help
```

A working Caddy + scopecache demo is in
[deploy/Caddyfile.caddyscope](deploy/Caddyfile.caddyscope) and
[Dockerfile.caddyscope](Dockerfile.caddyscope).

## Performance

Around 75,000 HTTP requests per second on commodity hardware over a
loopback Unix socket; see [scripts/bench.sh](scripts/bench.sh) for the
harness and `bench-results/` for tagged runs.

## Status

Pre-1.0. The core HTTP and Go API surfaces are still subject to
breaking change between minor versions. After v1.0 the core becomes
semver-stable.

## Building from source

```bash
go build -o scopecache ./cmd/scopecache
go test ./...
```

Module path: `github.com/VeloxCoding/scopecache`. Stdlib only.

## Documentation

The full design, endpoint contracts, and architectural rationale live
in [docs/scopecache-core-rfc.md](docs/scopecache-core-rfc.md).

## License

Apache License, Version 2.0. See [LICENSE](LICENSE).

Copyright 2026 VeloxCoding.
