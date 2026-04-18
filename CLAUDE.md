# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

**inmem-cache** — a Caddy module providing a local, rebuildable, disposable in-memory cache.

The DB is always the source of truth. This cache is NOT a database, search engine, analytics store, or business-logic layer. It can be wiped and rebuilt at any time.

### Item Model
- **scope**: required partition key. Max 128 bytes; no surrounding whitespace; no control characters.
- **id**: optional, unique within scope. Same shape rules as scope (max 128 bytes, no surrounding whitespace, no control characters).
- **seq**: cache-local cursor, generated only by cache (clients never send this on writes)
- **payload**: required. Must be a valid JSON value (object, array, string, number, bool) — that's a transport requirement, because the request body is JSON. Its *contents* are opaque to the cache: never inspected, never searched. Literal `null` is treated as missing. Per-item cap 1 MiB.

Note: items do NOT carry a server-level `ts`. Clients that need a DB/business
timestamp put it in the payload and filter client-side. Scope-level time
metadata (`created_ts`, `last_access_ts`) is cache-owned and drives
`/delete-scope-candidates`.

### Operations
- Reads: head (oldest-first, optional `after_seq` cursor), tail (most recent, optional `offset`), get (by id or seq)
- Writes: append, warm, rebuild, update (by id or seq), delete (by id or seq), delete-up-to, delete-scope
- Filtering/addressing only on: scope, id, seq

`/delete-up-to` removes every item in a scope with `seq <= max_seq`. It exists
to support write-buffer patterns: client reads a batch, commits to the DB, then
trims the cache up to the last committed seq.

### Capacity
Two independent caps apply. Either violation returns **HTTP 507 Insufficient Storage** — the cache never evicts on its own. Client frees capacity via `/delete-up-to`, `/delete-scope`, or a fitting `/warm`/`/rebuild`.

- **Per-scope item cap** — 100,000 items (default), overridable with `INMEM_SCOPE_MAX_ITEMS`. `/append` returns 507 with the one offending scope; `/warm` and `/rebuild` are atomic and reject the whole batch with the full offender list.
- **Store-wide byte cap** — 100 MiB aggregate `approxItemSize` (default), overridable with `INMEM_MAX_STORE_MB` (integer MiB). Tuned for ~1 GB VPS footprints. Tracked via an atomic counter on the hot path and a fresh-delta check at batch commit. The 507 response carries `tracked_store_mb`, `added_mb`, and `max_store_mb`.
- **Bulk request cap** — per-request body cap for `/warm` and `/rebuild` is **derived from the store cap** at startup (`bulkRequestBytesFor` in [types.go](types.go): store + 10% + 16 MiB). This guarantees a fully-loaded cache always fits into a single bulk request. Single-item endpoints keep a fixed 2 MiB cap (`MaxSingleRequestBytes`).

All byte-ish JSON fields (`tracked_store_mb`, `max_store_mb`, `approx_scope_mb`, `added_mb`) are serialized as MiB with 4 decimals via the `MB` helper type in [types.go](types.go) — one unit across `/stats`, `/delete-scope-candidates` and 507 responses. Internal size math (atomic counter, `approxItemSize`, per-item cap) stays in bytes.

### Access
- Local-only via Unix domain socket

## Development Phase

**Currently in Phase 1: standalone.** The code is a plain `package main` HTTP server listening on a Unix socket. Phase 3 will convert it into a Caddy module (`package inmemcache` with `caddy.RegisterModule()`).

Until the standalone version is validated by tests, do **not** add Caddy-specific code or imports.

## Build & Development

Module path: `github.com/DenverCoding/inmem-cache`. Stdlib only — no external deps.

```bash
# Build and run the service
docker compose up --build inmem-cache

# Interactive dev shell (Go + curl, shares the /run volume)
docker compose up -d dev
docker compose exec dev sh

# Inside dev shell:
go build -o /tmp/inmem-cache .
go test ./...
go test -run TestName ./...
go vet ./...

# Hit the socket from dev container:
curl --unix-socket /run/inmem.sock http://localhost/help
```

## File Layout

- [main.go](main.go) — `main()`, Unix socket listener, mux wiring
- [handlers.go](handlers.go) — HTTP handlers + `registerRoutes()`; stable API surface
- [store.go](store.go) — `Store` + `ScopeBuffer`; all cache logic
- [validation.go](validation.go) — input validation + query param normalization
- [types.go](types.go) — item model, constants, size estimators

## Architecture

Caddy modules follow a strict lifecycle:

1. `init()` registers the module via `caddy.RegisterModule()`
2. `CaddyModule()` returns module ID and constructor (implements `caddy.Module`)
3. `Provision(ctx caddy.Context)` initializes the module (implements `caddy.Provisioner`)
4. `Validate()` validates configuration (implements `caddy.Validator`)
5. `Cleanup()` tears down resources (implements `caddy.CleanerUpper`)

Module IDs use dot-separated namespaces (e.g., `http.handlers.my_handler`). Struct fields use `json:"field_name,omitempty"` tags for Caddy's JSON config.

## Conventions

- Use interface guards to verify interface compliance at compile time:
  ```go
  var _ caddyhttp.MiddlewareHandler = (*MyModule)(nil)
  ```
- Module names use snake_case in their ID
- Configuration fields use snake_case in JSON tags
