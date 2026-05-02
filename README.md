# scopecache

A small, local, in-memory cache and write buffer written in Go — stdlib only, served over a Unix domain socket.
Tuned for modest VPS footprints, delivering around 10,000 HTTP requests per second per core and well over 100,000 per second aggregate under concurrent load.
Data lives in scopes (namespaces) and is addressable only by scope, id, or seq; the entire cache is disposable and can be wiped and rebuilt from the source of truth at any time.
Payloads can also be served directly via `/render`, allowing Caddy, nginx, or Apache to send cached HTML, JSON, or XML straight to the client without an application layer in between.
Read endpoints are equally proxyable. Even without `/render`, the regular query endpoints (`/tail`, `/head`, `/get`, `/ts_range`) are plain HTTP returning JSON and can be returned without the need of an application layer. For example, a forum frontend can fetch `http://example.com/tail?scope=thread:900&limit=100` directly to get the last 100 messages of a specific thread. This can substantially reduce per-request overhead and allow a single server to handle far more traffic on cacheable paths.

**No client library required.** scopecache speaks standard HTTP rather than a bespoke wire protocol — any language with an HTTP client in its standard library (Python, PHP, Node, Ruby, Go, …) can call the cache directly, with no driver to install, pin, or keep version-aligned with the server. Redis and memcached by contrast require **two** layers before a cached value ever reaches the wire: a **per-language driver** (`redis-py`, `phpredis`, `node-redis`, …) to speak their custom binary protocol, and an **application layer** in front to translate that driver's reply into an HTTP response. scopecache collapses both into a single HTTP hop — and via `/render`, that hop can end at Caddy/nginx/apache without any application code in the loop at all.

> **Deployment safety — read this before mounting scopecache on a public listener.**
>
> scopecache is **caller-anonymous**: the cache never looks at who you are. Authentication and scope-based authorisation live in the integrator layer (Caddyfile route guards, Unix-socket permissions, the `/guarded` HMAC gateway). That means **never proxy the full scopecache API to the public internet**. Expose only the exact paths you intend to serve.
>
> Three rules of thumb:
>
> 1. `/admin` is the operator-elevated dispatcher and has no body-level auth — exposing it publicly lets any caller `/wipe` the cache. The Caddy module defaults `enable_admin no`; turn it on only on a trusted listener (localhost-bound, mTLS, or `client_ip` matcher). The standalone Unix-socket binary defaults it on because the socket is already permission-gated by the OS.
> 2. Public callers should reach **read paths** (`/get`, `/render`, `/head`, `/tail`, `/ts_range`) and possibly `/multi_call` — never the write paths (`/append`, `/upsert`, `/update`, `/counter_add`, `/delete`, `/delete_up_to`). Writes belong on a trusted/internal listener or behind `/guarded` (token-gated, per-tenant scope rewrite).
> 3. `/stats`, `/delete_scope_candidates`, and `/delete_guarded` are admin-only — they enumerate scope names, surface heat metadata, or atomically wipe a tenant's whole namespace, and would leak `_tokens`, `_guarded:*`, `_counters_*` plus per-scope read-heat in multi-tenant deployments.

## What it is

- A scope-first hot-window cache / write-buffer that sits in front of your real data store. (A *scope* is what other systems call a **namespace** or **bucket** — conceptually comparable to a **table** in SQL terms: every item lives inside exactly one.)
- Typical use: keep a hot slice of your data in RAM so it does not have to be re-queried from the database on every request. For example, the replies to a forum topic, the recent chat messages of a given user, or a rolling feed per session — each lives in its own scope and can be served directly from memory.
- Also usable as a write-buffer: append high-frequency events (analytics hits, log lines, chat messages) to a scope, and let a background worker drain the buffer every few seconds with a single bulk insert against the database. This flattens write spikes and keeps the DB from being hammered on every request. `/delete_up_to` exists specifically for this drain pattern — trim the cache up to the last seq you committed.
- Wipeable and rebuildable at any time — the source of truth lives elsewhere (a database, a JSON file, data built in code, anything).
- Tuned for modest VPS footprints (~1 GB RAM alongside DB + app), with a 100 MiB default store cap.
- **Extremely fast**: around 10,000 HTTP requests per second per core over the Unix socket and over 100,000 per second aggregate under concurrent load, backed by a cache core that answers in-process lookups in sub-50 nanoseconds (see [Performance](#performance)).
- **It can serve cached content directly to the HTTP edge.** The `/render` endpoint returns the raw payload bytes — no JSON envelope, no wrapper — so a fronting proxy (Caddy, nginx, apache) can pipe cached HTML pages, XML documents, JSON API responses or text fragments straight to the browser or API client. No application layer in between, no deserialize-and-reserialize round-trip. With Redis/memcached-style caches an application always has to sit in the middle to translate their reply into HTTP. 
- **Read endpoints are equally proxyable.** Even without `/render`, the regular query endpoints (`/tail`, `/head`, `/get`, `/ts_range`) are plain HTTP returning JSON, so a browser or SPA can call them through the same fronting proxy with no app layer behind it. For example, a forum frontend can fetch `http://example.com/tail?scope=thread:900&limit=100` directly to get the last 100 messages of thread 900 — Caddy/nginx/apache proxies to scopecache's Unix socket, scopecache answers from memory, the JSON reaches the client. No PHP, no Python, no REST-API process in the middle.
- scopecache is intentionally simple. Addressing is limited to `scope`, `id`, and `seq`; filtering adds one more axis, an optional client-supplied `ts` (int64) that `/ts_range` treats as a time-window filter. That deliberate shortness is the whole point: it is what keeps the cache fast and easy to reason about. There is no rich query language, but because `scope` and `id` are free-form strings the client fully controls — and `ts` opens server-side time windows without dragging payloads into the query surface — a surprisingly wide set of access patterns can be modeled on top of them.
- scopecache is small enough to stay fully comprehensible, yet rich enough to carry a wide range of useful patterns. Its strength lies in the combination of simplicity, practical usefulness, and ease of deployment: a small, local, in-memory scope-first cache and write-buffer that is intentionally limited in surface area, yet flexible enough to support hot-read caching, lightweight write-buffer workflows, and direct response serving — all built from the same small set of core primitives. Cached HTML, XML, or JSON can be rendered straight to the wire, allowing a fronting webserver to present content directly from the cache without any intermediate application layer.
- scopecache is deliberately limited in capability, yet flexible enough to cover a wide range of real-world use cases. Proposals to expand the query surface or make the cache "smarter" — automatic TTL, eviction, background policies — are scrutinised hard: anything added here competes directly with the simplicity and predictability that make the cache fast and easy to reason about.

## What it is not

- A database, search engine, analytics store, or generic query engine.
- A business-logic layer.
- Payloads are opaque JSON — the cache never inspects, parses, or searches inside them.
- Not an HTTP response cache. Tools like **Varnish**, nginx `proxy_cache`, Apache `mod_cache`, and Caddy's **souin** key on URL (+ `Vary` headers) and respect HTTP cache semantics transparently for the app. scopecache is a **data cache**: the app explicitly writes and reads fragments by scope/id, and the cache never looks at URLs or `Cache-Control`. The two compose cleanly side by side — scopecache holds warm data, an HTTP cache at the edge caches the rendered response.
- `/render` comes close to an HTTP-cache feel — Caddy/nginx/apache can serve raw payload bytes to clients without an application layer — but it stays **scope/id/seq-keyed, not URL-keyed**, and does not interpret `Cache-Control`, `ETag`, or conditional GETs. As a functional stand-in for an HTTP cache on paths where you control the keys, `/render` is an acceptable substitute; on an in-memory hit it serves in the order of microseconds, within the same ballpark as Varnish or souin.

## Pre-compute + publish

> Preliminary positioning from the phase-4 harness. Subject to change as measurement and workload breadth mature.

scopecache, Varnish, Redis, and nginx static files all live in the "cache bytes for fast serving" neighbourhood, but they differ sharply in **who decides what to cache and when**. That control axis — not throughput — is scopecache's distinctive feature.

| system | model | populated by | invalidation | cold start |
|-|-|-|-|-|
| **nginx static** | file-system driven | file write + atomic rename | file overwrite | disk read |
| **Varnish** | reactive (pull) | cache-miss → origin → fill | `PURGE`/`BAN` + TTL | first request pays the full backend |
| **Redis `GET`** (from app) | raw KV | app explicitly `SET` | TTL or app `DEL` | app still sits in front of every request |
| **scopecache `/render`** | proactive (push) | app explicitly `/warm` | overwrite via `/warm` | pre-warmed — first request costs what the thousandth costs |

`/render` is a *publish target*, not an HTTP response cache in the Varnish sense. The flow is: render offline (cron, worker, build step, after a CMS save), push to scopecache with `/warm`, bytes are atomically live. No user traffic is required to populate the cache. No cold-start for the first visitor after a release. No fsync wall — a full refresh of the hot set is one in-memory map swap.

This is a distinct operational model from on-demand caching (Varnish) or app-driven KV (Redis), with properties that neither provides on its own: atomic multi-item swap, deterministic cache contents, no cold-miss amplification of backend load, no on-disk state to manage. It composes cleanly with those tools rather than replacing them — Varnish in front of scopecache for edge TLS termination; Redis next to scopecache for session state while scopecache holds rendered pages.

### Phase-4 end-to-end bench (2 vCPU, 128 MB cgroup, 170 MB SQLite DB)

Same harness (FrankenPHP + SQLite forum), same output bytes (~4.7 KiB HTML per response), two paths:

| endpoint | ok | req/s | avg ms | p50 | p95 | p99 | max |
|-|-|-|-|-|-|-|-|
| `GET /?page=random` — PHP → PDO → SQLite → render | 3,615 | 1,197 | 7.87 | 4.62 | 44.32 | 47.40 | 48.99 |
| `GET /render?scope=html&id=page-<rand>` — scopecache direct | 26,942 | **11,116** | **0.61** | **0.23** | **0.49** | **0.90** | **55.91** |

~9–10× throughput and ~90× at p95, identical hardware serving identical bytes. Both paths run through the **same Caddy binary** on the same hardware — what differs is everything past the matcher.

SQLite is not the bottleneck here. Instrumenting the PHP path separately showed the script body itself (all queries included) costs 0.3–0.4 ms; the remaining ~4–10 ms of the PHP path is Caddy's matcher chain, FrankenPHP worker dispatch, `php_server` try_files, and PHP interpreter startup — i.e. the **request lifecycle around PHP**, not the DB work inside it. Two independent experiments confirm this: adding a covering index to make the front-page query index-only produced zero measurable change on GET /; raising SQLite's per-connection cache to 256 MiB and enabling 256 MiB mmap — so the entire 170 MB DB fits in the engine's own cache with page derefs instead of read() syscalls — likewise produced zero measurable change (2,127 vs 2,140 req/s, within noise). An all-in-RAM SQLite is already in the numbers above; there is no further optimisation headroom on the DB side.

What `/render` removes is therefore not the DB query but the whole PHP request lifecycle. The comparison is essentially *Caddy-plus-PHP versus Caddy-alone serving the same bytes* — the bytes happened to originate from SQLite in this harness, but the saving is on the serving side.

The preliminary signal: when the same bytes can be served from either path, the pre-compute-and-publish path saves an order of magnitude of CPU per response. That scales directly: a box handling ~1,200 req/s via PHP+SQLite on this harness can handle ~11,000 req/s via `/render` without re-entering the application layer.

### The gap widens dramatically with more hardware

To confirm that the ceiling really is the PHP request lifecycle rather than scarce CPU or cold page cache, the same harness was re-run on the host unconstrained — 32 CPU / 32 GB, DB fully in OS page cache. A second load generator (`wrk`) was added to rule out PHP-side client underestimation on the scopecache path:

| config | SQLite `GET /?page=random` | scopecache `/render` | gap |
|-|-|-|-|
| 2 vCPU / 128 MB, conc=10 (PHP bench) | 1,197 req/s | 11,116 req/s | 9× |
| 32 CPU / 32 GB, conc=50 (PHP bench) | 2,140 req/s | 36,098 req/s | 17× |
| 32 CPU / 32 GB, conc=50 (wrk) | 2,049 req/s | 62,380 req/s | **30×** |
| 32 CPU / 32 GB, conc=1000 (wrk, server ceiling) | 1,938 req/s | **80,962 req/s** | **~42×** |

Two independent ceilings, one in each direction:

**SQLite has a hard server-side ceiling of ~2,100 req/s** and does not cross it regardless of concurrency or load generator. `wrk` at conc=50 lands on 2,049 req/s — within 5% of the PHP bench's 2,140 — proving the PHP client was *not* the bottleneck on this path; the PHP-worker pool is. At conc=10 utilisation is already 89% of Little's-law theoretical, and conc=100 produces *lower* total throughput (1,938 req/s) because queueing latency climbs faster than throughput.

**`/render` scales near-linearly until connection-handling overhead takes over.** Per-request p50 stays at 0.24 ms through conc=50 and climbs smoothly as the box fills: 1.06 ms at conc=100, 10.8 ms at conc=1000. The PHP-based bench client was underestimating scopecache by ~70% at conc=50 (36k measured vs 62k real); `wrk` at conc=1000 finds the actual server ceiling around **~80k req/s on this 32-core host**.

**The gap widens dramatically, not linearly**, as hardware grows — from 9× on 2 vCPU to ~42× on 32 CPU. Adding cores to a PHP+SQLite server buys a little headroom (≈1.8× for 16× more CPU) before hitting the per-request lifecycle wall; adding the same cores to a `/render`-fronted path keeps scaling because every saved PHP invocation compounds. The comparison is not "cache vs database", it is "serving bytes with vs without invoking a dynamic language runtime per request" — and that difference does not get smaller when you give both sides more hardware.

*Harness lives in `phase4/` (git-ignored local testbench); measurement notes, caveats, and known artefacts in `phase4/CLAUDE.md`.*

### Phase-4 `/guarded` overhead and counter-scope contention (32 CPU host)

`/guarded` adds non-trivial work on top of a bare `/get`: HMAC-SHA256 of the token, JSON parse + scope rewrite + re-serialize, scope-existence check, sub-call dispatch, response parsing + prefix strip, and three `counterAdd` writes (`_counters_count_calls`, `_counters_count_kb_in`, `_counters_count_kb_out`). The pre-design fear was that the counter scopes — shared across every tenant — would become a serialisation point at high concurrency: every `/guarded` request takes the same scope-lock for `counterAdd`, so multi-tenant traffic could in principle bottleneck on a write that single-tenant traffic doesn't notice. This bench measures the actual cost on the same harness as the section above, with `wrk` driving four shapes from a sidecar container on the `phase4_default` bridge network (apples-to-apples vs. the wrk numbers above):

| test | conc=50 req/s | conc=200 req/s | p50 (200) | sub-calls/s @ conc=200 |
|------|--------------:|---------------:|----------:|------------------------:|
| 1. `GET /get` baseline (50-byte items, no /guarded)            | 83,537 | **113,360** | 1.6 ms | 113,360 |
| 2. `POST /guarded` 1 sub-call, single tenant                   | 49,990 | **62,796**  | 1.4 ms | 62,796 |
| 3. `POST /guarded` 10 sub-calls, single tenant (batch)         | 11,248 | **12,212**  | 14.7 ms | **122,123** |
| 4. `POST /guarded` 1 sub-call, rotating across 10 tenants      | 57,689 | **66,281**  | 2.4 ms | 66,281 |

Bench setup: 10 tenant scopes pre-provisioned via `/admin /warm` (`_guarded:<capId>:bench-data` × 50 items each) plus one public baseline scope (`bench-data-public` × 50 items). Items are 50-byte JSON objects. The PHP-driver bench page at `phase4/app/public/sc/bench_guarded.php` exists for quick UI feedback but underestimates by the same ~70% pattern the README documented earlier (PHP's `curl_multi` adds overhead the cache itself does not pay) — wrk numbers are the real ceiling.

**Three takeaways:**

1. **`/guarded` overhead = ~45% throughput cost.** 113k → 63k req/s (test 1 vs. test 2). That is the price of HMAC + scope rewrite + existence check + dispatch + response strip + 2× counter write. p50 latency cost is small (+0.07 ms at conc=50, roughly flat at conc=200) — the cost shows up in throughput, not per-request wall time.

2. **Batch amortisation puts `/guarded` *above* the `/get` baseline.** Test 3 runs 12,212 batches/s × 10 sub-calls = **122,123 effective sub-calls/s**, slightly faster than the bare-`/get` baseline (113,360). Inside a batch every sub-call goes through the same handler code as a standalone call but pays no per-sub-call HTTP framing cost — only the outer envelope is parsed and serialised once. Tenants who batch effectively get the multi-call discount on top of `/guarded`'s fixed-cost overhead.

3. **The shared counter scopes are NOT a contention bottleneck.** Test 4 (multi-tenant rotation) is *consistently faster* than test 2 (single tenant) at both concurrency points: 57.7k vs. 50.0k at conc=50, 66.3k vs. 62.8k at conc=200. Spreading reads across 10 tenant scopes parallelises the per-scope `RWMutex` for the read side, while the counter-scope write is short enough (one map update + one atomic int64 add under one lock acquisition) that it disappears into the noise. **Pre-v1.0 design verdict:** `_counters_count_calls`, `_counters_count_kb_in`, and `_counters_count_kb_out` stay shared across tenants — splitting them per-`capability_id` would be a premature optimisation that the data does not justify. (This bench was taken when `/guarded` only wrote two counters; the third counter was added later. The contention conclusion stands: each counter is a separate scope, so the per-counter lock is short-held and uncontended on its own; adding a third short-held lock acquisition does not change the picture.)

### Bandwidth-bound vs. request-handling-bound — why `/get` exceeds the published `/render` ceiling

The `/get` baseline above (113,360 req/s) is *higher* than the `/render` ceiling reported earlier in this section (80,962 req/s on the same hardware). That is not a contradiction — it is the two endpoints running into different physical limits:

| endpoint | typical response size (this harness) | bytes/sec @ ceiling | bound by |
|----------|-------------------------------------:|---------------------:|----------|
| `/render` (5 KB pre-rendered HTML page) | ~5,000 B | ~400 MB/s | **bandwidth** through the loopback / bridge network — every byte serialised over the socket |
| `/get` (50-byte item, JSON envelope) | ~250 B | ~28 MB/s | **request handling** — `net/http` framing, accept loop, goroutine wakeup, ServeMux dispatch |

At ~5 KB per response, throughput hits the network's bytes-per-second wall before the cache can do more work — the floor moves with payload size, not with cache-internal complexity. At ~250 B per response the same ceiling is ~14× higher in raw req/s because there are simply fewer bytes to push. The cache-internal cost per request (map lookup, scope lock acquire/release) is sub-microsecond and never the dominant term in either regime.

What this means in practice:

- **`/guarded` on small payloads** (the 63k req/s number above, on 50-byte items) is pure request-handling territory — adding HMAC + rewrite + counter writes pays a structural ~45% throughput tax.
- **`/guarded` on `/render`-sized payloads** (5 KB HTML pages, not measured here) would hit the same ~400 MB/s bandwidth wall as `/render` does standalone, well before the cache's per-request work becomes visible. The 45% tax above would shrink to single digits — once you are bandwidth-bound, additional CPU work is approximately free.
- **Anything in between** (~1-2 KB items: small JSON documents, rendered fragments) sits in a transition zone where both terms matter. Worth measuring on the actual workload before assuming either ceiling applies.

The published `/render` ceiling is the right number to quote when the question is "how many bytes/sec can scopecache serve through Caddy"; the `/get` and `/guarded` numbers above are the right ones to quote when the question is "how many cache operations/sec can scopecache dispatch". Both are real, both are measured on the same hardware, and they differ by an order of magnitude because they answer different questions.

## Architecture

Three layers with clear boundaries:

- **Core** — `package scopecache`. The cache engine itself. Stdlib-only, framework-agnostic, caller-anonymous: it registers HTTP routes on a standard `*http.ServeMux` and knows nothing about auth, identity, or who is calling. This is what the [spec](scopecache-rfc.md) describes.
- **Standalone adapter** — `cmd/scopecache/`. Thin binary that reads env vars, opens a Unix socket, and serves the core. What you use if you're running behind nginx/apache, or with no reverse proxy at all.
- **Caddy-module adapter** — `caddymodule/`. Published as a separate Go module (`github.com/VeloxCoding/scopecache/caddymodule`) so consumers of the core never pull in Caddy's dep tree. Wraps the core as a Caddy HTTP handler (`http.handlers.scopecache`), exposed in Caddyfile syntax and JSON config. This is also the home for cross-cutting concerns that require request context: auth enforcement, identity-to-scope mapping, per-tenant logging and metrics.

The rule: new **cache features** go into the core. **Cross-cutting concerns** (auth, identity, per-tenant policy) go into an adapter. This keeps the core small and refactorable, keeps both adapters symmetrical, and means cache semantics cannot drift between standalone and Caddy deployments.

## Authorization

Scopecache itself does not interpret authentication or validate credentials — scopes are opaque strings the cache stores by name. Access control is defined by the integrating application or reverse proxy, through naming conventions on scope strings combined with which endpoints are exposed externally. Possession of a scope name (or of a token an integrator translates into a scope prefix) *is* the capability. This pattern is **application-defined capability namespaces** — also called **authorization-by-naming-convention at the integration layer**.

scopecache supports two ways to apply this pattern. They are deployable separately or together; pick whichever fits the workload.

**1. Reverse-proxy rewrite (no `server_secret` needed).** The integrator does the scope rewrite outside the cache. Application server issues a bearer token, client returns it on each request, Caddy/nginx prepends it to the scope before forwarding. A client `GET /render?scope=privatethread:432` carrying `Authorization: Bearer aaa-bbb-ccc` reaches scopecache as `GET /render?scope=aaa-bbb-ccc:privatethread:432`. The cache sees only opaque scope strings; the token never appears in cache logic. The Caddy helper `scopecache_bearer_prefix` ([Helpers](#helpers-optional-caddy-middleware)) automates this rewrite. Fits when you already have a proxy doing token validation, when public reads need to be cacheable at the proxy layer, or when integrators want full control over the prefix shape.

**2. Built-in `/guarded` gateway (requires `server_secret`).** scopecache itself does the rewrite. Tenant POSTs to `/guarded` with `{"token": "...", "calls": [...]}`; the cache derives a deterministic 64-hex `capability_id = HMAC_SHA256(server_secret, token)` and rewrites every sub-call's scope to `_guarded:<capability_id>:<original-scope>`. Auth is gated by a single lookup in the reserved `_tokens` scope: an item with `id = capability_id` must exist there, otherwise the batch rejects with `400 tenant_not_provisioned`. Operator manages `_tokens` membership at token issuance (`/admin /upsert`) and revocation (`/admin /delete`). Within their own `_guarded:<capability_id>:*` namespace tenants self-organize freely — no per-scope operator approval; the underlying scope buffer auto-creates on first /append. Responses get the prefix stripped before the tenant sees them. Fits when no proxy is doing token validation, when revocation must be cache-side immediate (one item delete), or when per-tenant usage tracking is wanted (the cache auto-counts calls and KiB per `capability_id`). See [Multi-tenant gateways](#multi-tenant-gateways) below for curl examples and the [spec §6.4](scopecache-rfc.md) for the full contract.

The two patterns compose: a deployment can let internal services use the proxy-rewrite path (cheap, cacheable) while exposing `/guarded` to external API tenants (revocable, usage-tracked).

## Helpers (optional Caddy middleware)

scopecache's core stays minimal — append, read, address, update, delete, replace. Some patterns sit one layer above that ("read this scope using the bearer token as a prefix", "drain this scope and remove what was read"), and instead of building those into the cache or shipping a second `xcaddy --with` line, they live as **separate Caddy middleware modules in the same `caddymodule/` Go submodule**. Each helper has its own Caddyfile directive, opts in only where you use it, and the main `scopecache` handler stays untouched.

Concrete examples:

- **`scopecache_bearer_prefix`** — reads `Authorization: Bearer <token>` and prepends `<token>:` to the scope, so an authenticated client's `privatethread:432` becomes `aaa-bbb-ccc:privatethread:432` before reaching scopecache (the proxy-rewrite pattern from *Authorization* above). Functionally overlaps with the built-in `/guarded` gateway — pick `bearer_prefix` when you want every standard public endpoint (`/render`, `/tail`, `/get`, …) to be cacheable through the proxy with no envelope; pick `/guarded` when you want HMAC-derived prefixes, scope-existence enforcement, response stripping, or per-tenant usage counters.
- **`scopecache_pop`** — read-and-delete in one Caddyfile directive for write-buffer drain workers, combining `/tail` and `/delete_up_to`.
- **`scopecache_scope_rewrite`** — generic scope templating from Caddy placeholders (`{remote_host}`, cookie values, header values, …); the bearer-prefix helper is a special case.

The same `xcaddy --with github.com/VeloxCoding/scopecache/caddymodule@vX.Y.Z` build pulls everything in; helpers do nothing until their directive appears in your Caddyfile. Helpers are Caddy-only — the standalone binary expects integrators to do request-shaping at their own proxy layer.

## Status

Phase 4 — testing, benchmarking, and real-life validation under realistic workloads. The core (`package scopecache` at the repo root), the standalone binary (`cmd/scopecache/`), and the Caddy adapter (`caddymodule/`, its own Go module so the core stays stdlib-only for non-Caddy consumers) are all shipping on tagged releases. New features are admitted only when measurement exposes a design gap; expect bug fixes, clarity improvements, and benchmarking work before the v1.0 API freeze. See [scopecache-rfc.md §9](scopecache-rfc.md) for the phase model in full.

## Deployment modes

scopecache ships in two forms. Both run the exact same core; what differs is the glue around it.

**Standalone binary (Unix socket).** A separate `scopecache` daemon listens on a Unix socket. Anything that can speak HTTP over that socket can use it — a fronting webserver (nginx/apache/Caddy), but equally a desktop application, a CLI tool, a local daemon, or a background sync process. Pick this if you already run nginx or apache, if multiple apps on the box need to share one cache instance without routing through Caddy, or if there is no webserver in the picture at all (desktop/CLI/daemon scenarios where the cache is just a local data store).

**Caddy module (in-process).** scopecache is compiled into your Caddy binary as an HTTP handler. Pick this if Caddy is already your edge. It gets you:

- **No IPC.** Cache lookup is an in-process function call, not a socket round-trip. No `usermod -aG` dance to grant the proxy user access to the socket.
- **One process, one binary, one systemd unit.** One log stream, one restart.
- **Caddy's edge stack for free.** TLS + auto Let's Encrypt, HTTP/2, HTTP/3 (QUIC), gzip/brotli, access logs, matchers — all inherited by the cache endpoints.
- **Config in one place.** The `scopecache { … }` block sits next to the routes that use it, not split across a systemd unit and a proxy config.
- **Middleware composition.** `basic_auth`, `jwt`, `forward_auth`, rate-limit plugins, `header` directives run *before* scopecache in the chain. This is where request-context-aware policy belongs (see [cross-cutting-concerns.md](cross-cutting-concerns.md)).
- **Per-vhost mounting.** `handle /cache/*` with `uri strip_prefix`, or separate scopecache instances per site with different caps — all expressible in Caddyfile.

**The in-process path — why it matters.** The "No IPC" bullet is the largest structural advantage of the module deployment, and it is worth spelling out because nothing on the client side can substitute for it.

In a standalone-plus-fronting-proxy setup, every client request costs a second HTTP hop *inside the server*: the request hits Caddy, Caddy opens (or reuses) a Unix-socket connection to the scopecache daemon, writes a second HTTP request on it, reads the response back, and forwards it to the client. Both sides parse an HTTP request; the socket round-trip is measured in milliseconds.

The Caddy module folds that hop away entirely. Caddy's matcher chain dispatches to `scopecache.ServeHTTP()` as a direct Go function call — no TCP, no socket, no second HTTP encode/decode. The caddymodule routes through an internal `http.ServeMux` (a map lookup plus a function call, on the order of nanoseconds) to a handler that reads directly from a `*Store` living in the same Go heap. The HTTP request parsed at the outer listener is the only HTTP request the process ever sees.

End-to-end latency for `GET /render?scope=html&id=<id>` at low concurrency on a 32-core host:

```
p50 0.21 ms · p95 0.44 ms · p99 0.72 ms
```

That 0.21 ms p50 breaks down as one TCP roundtrip from the client, one Caddy matcher evaluation, one scopecache map lookup, and one response write — the scopecache step inside the server is roughly 15 µs of Go work. An application layer between the client and scopecache (PHP, Python, Node, Ruby) cannot close this gap: none of those runtimes can call the Go handler in-process, so every cache lookup from application code pays an additional HTTP roundtrip on top of the runtime's own per-request cost. The module path is therefore not just a convenience over the standalone-plus-proxy setup — it is strictly faster than any deployment where another language sits between the edge and the cache.

**Tradeoffs of the module path:**

- **Cache empties on restart *and* on `caddy reload`.** A full restart is obvious, but `caddy reload` is the subtle one: it re-provisions the scopecache module, which creates a fresh `*Store` — same effect as a cold start. Harmless by design (the cache is disposable), but plan for it: a periodic rebuild from your source of truth, or a warm-on-miss in your application code, keeps cold restarts from hammering the backend. A systemd-based option is also common — hang a rebuild script off `caddy.service` via `ExecStartPost=/usr/local/bin/scopecache-rebuild.sh` (or a separate oneshot unit with `ExecStart=…` and `After=caddy.service`) so it fires automatically after every Caddy start.
- **Tied to Caddy's version** — `xcaddy` rebuild needed on upgrades to either side.
- **Can't be shared across apps** without routing everything through the same Caddy.

## Quickstart (Docker)

Standalone, listening on a Unix domain socket:

```bash
docker compose up --build scopecache
```

The service listens on `/run/scopecache.sock` inside the container (mounted to the host volume defined in `docker-compose.yml`).

As a Caddy module, listening on TCP :8081:

```bash
docker compose up --build caddyscope
curl http://localhost:8081/help
```

## Quickstart (Caddy / FrankenPHP via xcaddy)

Build a Caddy binary with scopecache baked in:

```bash
xcaddy build --with github.com/VeloxCoding/scopecache/caddymodule@latest
```

Combine with FrankenPHP in one binary:

```bash
xcaddy build \
    --with github.com/dunglas/frankenphp/caddy \
    --with github.com/VeloxCoding/scopecache/caddymodule@latest
```

Minimal Caddyfile:

```caddyfile
:8080 {
    scopecache {
        scope_max_items 100000
        max_store_mb    100
        max_item_mb     1
        server_secret   {$SCOPECACHE_SERVER_SECRET}
    }
    respond 404
}
```

All `scopecache { ... }` subdirectives are optional; omit any of them to fall back to the compile-time default. `server_secret` enables the `/guarded` tenant gateway (see [Multi-tenant gateways](#multi-tenant-gateways) below); leaving it empty or unset disables `/guarded` entirely. The `{$SCOPECACHE_SERVER_SECRET}` substitution reads the value from the process environment at config-load — recommended over inlining the secret in the Caddyfile.

`/admin` is **not** registered by default on the Caddy module. To enable operator-elevated operations (`/wipe`, `/warm`, `/rebuild`, `/delete_scope`, `/delete_guarded`, `/stats`, `/delete_scope_candidates`), add `enable_admin yes` to the block AND restrict the `/admin` path to a trusted matcher — never expose it on a public listener:

```caddyfile
:8080 {
    @operator client_ip 10.0.0.0/8
    handle /admin {
        scopecache {
            scope_max_items 100000
            enable_admin    yes
            server_secret   {$SCOPECACHE_SERVER_SECRET}
        }
    }
    handle {
        scopecache {
            scope_max_items 100000
            # enable_admin omitted → /admin returns 404 on the public path.
            server_secret   {$SCOPECACHE_SERVER_SECRET}
        }
    }
    respond 404
}
```

See [Caddyfile.caddyscope](Caddyfile.caddyscope) for the working demo and [Dockerfile.caddyscope](Dockerfile.caddyscope) for the xcaddy build recipe.

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
curl -s --unix-socket /run/scopecache.sock -X POST http://localhost/admin \
  -H "Content-Type: application/json" -d '{"calls":[{"path":"/stats"}]}'
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
EnvironmentFile=-/etc/scopecache.env
RuntimeDirectory=scopecache
RuntimeDirectoryMode=0750
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

The four `SCOPECACHE_*` capacity variables are listed explicitly so every knob is visible and tunable in one place — remove any line to fall back to the compile-time default. `EnvironmentFile=-/etc/scopecache.env` (with the leading `-` so the unit still starts when the file is absent) is the recommended way to inject `SCOPECACHE_SERVER_SECRET` for the `/guarded` tenant gateway: keep the secret in `/etc/scopecache.env` (mode `0600`, owned by the `scopecache` user) instead of a world-readable unit file. The socket lives under a systemd-managed `RuntimeDirectory` (`/run/scopecache/`), which is created at service start and cleaned up on stop.

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

`/head`, `/tail`, `/ts_range`, `/update`, `/upsert`, `/counter_add`, `/delete`, `/delete_up_to`, `/multi_call`, `/help` — see section 13 of the [spec](scopecache-rfc.md) for full examples. `/warm`, `/rebuild`, `/delete_scope`, `/wipe`, `/stats`, and `/delete_scope_candidates` are **not** on the public mux; they are reachable only as sub-calls inside `/admin` (see [Multi-tenant gateways](#multi-tenant-gateways) below). `/stats` and `/delete_scope_candidates` are admin-only because they enumerate every scope name in the store and would leak reserved scopes (`_tokens`, `_guarded:*`, `_counters_*`) and per-scope heat metadata in multi-tenant deployments.

`/ts_range` filters a scope by a client-supplied top-level `ts` (signed int64, milliseconds since unix epoch by convention — the cache is opaque to the unit). At least one of `since_ts` / `until_ts` must be provided; both together form an inclusive `[since_ts, until_ts]` window. Items without a `ts` are excluded. Results are returned in ascending `seq` order — `ts` is a filter, not an ordering key. Responses on `/head`, `/tail`, and `/ts_range` carry `{ "ok", "items", "truncated" }`; `truncated: true` means more matching items exist beyond the returned `limit`. `/ts_range` has **no pagination cursor** because `ts` is mutable (via `/update` / `/upsert`) and non-unique — narrow the window or raise `limit` to fetch more. `ts` is optional on `/append`, `/warm`, `/rebuild`, `/update` (absent = preserve) and `/upsert` (absent = clear, matching its whole-item replace semantics).

`/upsert` creates a new item or replaces an existing one by `scope` + `id`. It is the idempotent, retry-safe write path: unlike `/append` (which rejects duplicate ids) or `/update` (which soft-misses on absent items), `/upsert` always writes. `seq` is preserved on replace and freshly assigned on create. The response includes `"created": true` for a fresh item and `false` for a replace.

`/counter_add` atomically adds a signed int64 `by` to the integer counter at `scope` + `id`, auto-creating the counter with starting value `by` if it does not exist. Both paths run under a single scope write-lock — no client-side read-modify-write, so concurrent increments do not lose updates. The stored payload is a bare JSON integer (e.g. `42`), so `/get`, `/render`, `/upsert` and `/update` all see the same value. `by` is required and non-zero; both `by` and the result must lie within ±(2^53−1) (the JavaScript safe-integer range, so values round-trip through every client without precision loss). Reads and absolute sets go through the normal `/get`, `/upsert` and `/update` endpoints — only `/counter_add` parses the payload as an integer. Responses carry `{"ok", "created", "value"}`. If the existing item at `scope` + `id` is *not* a JSON integer within the allowed range, the request is rejected with `409 Conflict` — counters do not silently overwrite other payload types.

`/wipe` clears the entire store in one atomic call: every scope, every item, every byte reservation. It takes no request body. The response carries `{"ok", "deleted_scopes", "deleted_items", "freed_mb"}` so a client can verify what was released. The store-wide complement of `/delete_scope` — useful for test teardown, emergency reset, or preparing a fresh slate before a `/rebuild`. The cache never wipes on its own; this is explicitly a client-initiated action.

`/multi_call` dispatches `N` self-contained sub-calls in a single HTTP roundtrip. The body is `{"calls": [{"path": "/get", "query": {...}}, {"path": "/append", "body": {...}}, ...]}`; the response is `{"ok", "count", "results", "approx_response_mb", "duration_us"}` where each `results[i]` is `{"status", "body"}` carrying literally the JSON the standalone endpoint would have produced. Sub-calls run **strictly sequentially**, so a `/get` at index `k+1` observes everything writes at indices `0..k` committed; there is **no cross-call atomicity**, a write at index 0 stays applied even if a later sub-call errors. The whitelist is closed: `/append`, `/get`, `/head`, `/tail`, `/ts_range`, `/update`, `/upsert`, `/counter_add`, `/delete`, `/delete_up_to`. Store-wide locks (`/warm`, `/rebuild`, `/wipe`), admin-only `/delete_scope`/`/stats`/`/delete_scope_candidates`, raw-byte `/render`, `text/plain` `/help`, and `/multi_call` itself are excluded — a path outside the whitelist rejects the whole batch with `400`. Use case: collapse the call-count tax for small fanouts ("fetch K known ids", "do a small mixed read+write"). On a loopback Unix socket a 3-call batch typically returns in the low hundreds of microseconds vs. ~1 ms for three separate roundtrips; the gap widens on TCP. See section 13.20 of the [spec](scopecache-rfc.md) for the full contract and a captured example response.

## Multi-tenant gateways

`/admin` and `/guarded` are sibling endpoints that wrap `/multi_call`'s envelope shape with two different trust models. Use them to expose the cache to multiple tenants without writing your own dispatcher.

### `/admin` — operator-elevated multi-call

Same `{"calls": [...]}` body and `{"results": [...]}` response as `/multi_call`. Three things make it different:

- **Wider whitelist.** Includes the seven operator-only paths that no longer live on the public mux (`/wipe`, `/warm`, `/rebuild`, `/delete_scope`, `/delete_guarded`, `/stats`, `/delete_scope_candidates`), plus everything `/multi_call` allows.
- **Reaches reserved scopes.** Scope names beginning with `_` are blocked on every public endpoint (and on `/guarded`). `/admin` is the one path that can write them — that is how the operator manages the `_tokens` auth-gate that gates `/guarded` (one item per active tenant, keyed by `capability_id`).
- **No body-level auth.** `/admin` trusts that whoever reached the listener was authorised to do so. The deployment story is socket-permission-based on the standalone binary, and Caddyfile-route-restricted on the module path (`@operator { client_ip 10.0.0.0/8 } handle /admin { ... }` or similar). Treat the `/admin` path the same way you would treat root access to `/etc`: gated outside the cache, by the same boundary that gates the rest of the deployment.

Register a tenant in the auth-gate (one call per token issuance):

```bash
curl -s --unix-socket /run/scopecache.sock -X POST http://localhost/admin \
  -H "Content-Type: application/json" \
  -d '{
    "calls": [
      { "path": "/upsert", "body": {
          "scope":   "_tokens",
          "id":      "14071b366a5fa1d421678c6449fff66329dc10618b0e5785893e2db4ea2712d3",
          "payload": { "user_id": 42, "issued_at": "2026-04-27T..." }
      }}
    ]
  }'
```

Revoke a tenant (one call):

```bash
curl -s --unix-socket /run/scopecache.sock -X POST http://localhost/admin \
  -H "Content-Type: application/json" \
  -d '{"calls":[{"path":"/delete","body":{"scope":"_tokens","id":"14071b…"}}]}'
```

The `id` is the tenant's `capability_id` — `hex(HMAC_SHA256(server_secret, token))`, computed by the operator the same way `/guarded` does at request time. Item payload is opaque to the cache; operators commonly store user metadata there for their own bookkeeping.

Wipe the store (`/wipe` is reachable only here):

```bash
curl -s --unix-socket /run/scopecache.sock -X POST http://localhost/admin \
  -H "Content-Type: application/json" \
  -d '{"calls":[{"path":"/wipe"}]}'
```

Drop a single tenant's entire `_guarded:<capability_id>:*` namespace in one atomic call (the data-cleanup half of token revocation — pair this with `/delete` against `_tokens` and the two `_counters_*` scopes for the full four-call revocation batch; see [scopecache-rfc.md §6.4 *Token lifecycle is application logic*](scopecache-rfc.md)):

```bash
curl -s --unix-socket /run/scopecache.sock -X POST http://localhost/admin \
  -H "Content-Type: application/json" \
  -d '{"calls":[{"path":"/delete_guarded","body":{"capability_id":"14071b…"}}]}'
```

`/delete_guarded` accepts only a strictly-validated `capability_id` (64 lowercase hex chars), not a free-form prefix — the handler builds the `_guarded:<capability_id>:` prefix internally so callers cannot typo it, forget the trailing `:` (cross-tenant collision), or aim a free-form sweep at `_tokens` / `_counters_*` / `_inbox`. Damage radius is bounded by construction. Response shape mirrors `/wipe`: `{"ok", "deleted_scopes", "deleted_items", "freed_mb"}`.

### `/guarded` — tenant-facing multi-call

Body shape:

```json
{ "token": "<opaque token>", "calls": [{ "path": "/append", "body": {...} }, ...] }
```

For each request the cache derives `capability_id = HMAC_SHA256(SCOPECACHE_SERVER_SECRET, token)` (64-char lowercase hex). Auth is then a single lookup in the reserved `_tokens` scope: an item with `id = capability_id` must exist there. If not, the whole batch is rejected with `400 tenant_not_provisioned` and no sub-call runs. Operator manages `_tokens` membership at token issuance and revocation. After the auth-gate passes, every sub-call's `scope` is rewritten to `_guarded:<capability_id>:<original-scope>`; the underlying scope buffer auto-creates on first /append, so tenants self-organize within their own prefix without per-scope operator approval. Response bodies have the prefix stripped before the tenant sees them; the rewritten form never leaves the cache.

Append (after the operator has registered the tenant in `_tokens` — see [§13.22](scopecache-rfc.md) for the operator flow):

```bash
curl -s --unix-socket /run/scopecache.sock -X POST http://localhost/guarded \
  -H "Content-Type: application/json" \
  -d '{
    "token": "tenant-A-token",
    "calls": [
      { "path": "/append", "body": {
          "scope":   "events",
          "id":      "e1",
          "payload": { "text": "hello" }
      }}
    ]
  }'
```

Response (the `scope` in the slot's `item` reads `"events"` — the prefix is stripped):

```json
{
  "ok": true,
  "count": 1,
  "results": [
    {
      "status": 200,
      "body": {
        "ok": true,
        "item": { "scope": "events", "id": "e1", "seq": 2, "payload": { "text": "hello" } },
        "duration_us": 8
      }
    }
  ],
  "duration_us": 144,
  "approx_response_mb": 0.0002
}
```

Whitelist excludes `/delete_scope` and `/delete_guarded` (a tenant cannot deprovision their own namespace), `/stats` and `/delete_scope_candidates` (cross-tenant visibility), and every store-wide op.

After each successful request the cache atomically increments three reserved scopes for per-tenant usage tracking:

- `_counters_count_calls`, item id = `<capability_id>`, `+= len(calls)` — **one unit per sub-call**, not per HTTP request, so a tenant who batches 5 sub-calls into one `/guarded` call counts the same as a tenant making 5 solo calls.
- `_counters_count_kb_in`, item id = `<capability_id>`, `+= ⌊request_content_length / 1024⌋` — outer request envelope, captures ingress bandwidth per tenant, skipped when the rounded KiB value is `0` or `Content-Length` is unknown (chunked encoding).
- `_counters_count_kb_out`, item id = `<capability_id>`, `+= ⌊response_bytes / 1024⌋` — outer response envelope size, skipped when it rounds to `0`.

All three scopes auto-create on first use, survive `/wipe`, and are monotonic (no built-in reset — operator zeroes a tenant via `/admin /upsert` with `payload: 0`). Read one tenant's call count:

```bash
curl -s --unix-socket /run/scopecache.sock -X POST http://localhost/admin \
  -H "Content-Type: application/json" \
  -d '{"calls":[{"path":"/get","query":{
      "scope": "_counters_count_calls",
      "id":    "14071b366a5fa1d421678c6449fff66329dc10618b0e5785893e2db4ea2712d3"
  }}]}'
```

The slot's `body.item.payload` is the total sub-call count for that `capability_id`. Counters are raw signal — scopecache does not enforce quotas, return `429`, or suspend tenants on its own. Rate-limiting and billing decisions live in whatever process polls these counters.

`/guarded` is registered only when `SCOPECACHE_SERVER_SECRET` is set. Unset → the route is not in the mux, `POST /guarded` returns `404`. See [§6.4](scopecache-rfc.md) and [§13.22-13.23](scopecache-rfc.md) of the spec for the full contract and worked examples.

### `/inbox` — shared write-only ingestion

Sister of `/guarded` for the "many producers, one drainer" pattern. Each request is a single `/append` (no multi-call envelope); the cache assigns identity (`id = <capability_id>:<random>`) and time (`ts = now()`); tenants cannot read what they wrote — drains happen via `/admin /tail` + `/admin /delete_up_to`.

```bash
curl -s --unix-socket /run/scopecache.sock -X POST http://localhost/inbox \
  -H "Content-Type: application/json" \
  -d '{
    "token":   "tenant-A-token",
    "scope":   "_inbox",
    "payload": { "event": "signup", "user_id": 42 }
  }'
```

Response is intentionally minimal — no `id`, `seq`, or `scope` echo, since tenants have nothing to address afterwards:

```json
{ "ok": true, "ts": 1745236800000, "duration_us": 35 }
```

`ts` is a server-authoritative timestamp the cache stamped on the item — useful for clients with skewed clocks. Forbidden in the request body (each gets a `400`): `id`, `seq`, `ts`. The cache owns identity and time; clients wanting historical timestamps put them in `payload`.

Operator config:
- `SCOPECACHE_SERVER_SECRET` must be set (HMAC for `capability_id`).
- `SCOPECACHE_INBOX_SCOPES` lists allowed inbox scope names, newline-separated. Caddyfile equivalent: repeatable `inbox_scope <name>` directive.
- Either missing → route not registered, `POST /inbox` returns `404`.

The auth-gate is the same `_tokens` lookup as `/guarded` — one scope-and-revocation primitive across both endpoints. Drain pattern: `/admin /tail _inbox` to read items, parse `id` on first `:` to extract `capability_id` per item, JOIN against your `api_tokens` table to recover the user, then `/admin /delete_up_to` to free the buffer. See [§6.4](scopecache-rfc.md) (`/inbox` subsection) for the full spec.

## Configuration

All overrides via environment variables:

| Variable                          | Default                  | Purpose                                       |
|-----------------------------------|--------------------------|-----------------------------------------------|
| `SCOPECACHE_SOCKET_PATH`          | `/run/scopecache.sock`   | Listening socket path                         |
| `SCOPECACHE_SCOPE_MAX_ITEMS`      | `100000`                 | Max items per scope                           |
| `SCOPECACHE_MAX_STORE_MB`         | `100`                    | Store-wide byte cap (integer MiB)             |
| `SCOPECACHE_MAX_ITEM_MB`          | `1`                      | Per-item size cap (integer MiB)               |
| `SCOPECACHE_MAX_RESPONSE_MB`      | `25`                     | Per-response byte cap (integer MiB)           |
| `SCOPECACHE_MAX_MULTI_CALL_MB`    | `16`                     | `/multi_call` input body cap (integer MiB)    |
| `SCOPECACHE_MAX_MULTI_CALL_COUNT` | `10`                     | `/multi_call` max sub-calls per batch         |
| `SCOPECACHE_MAX_INBOX_KB`         | `64`                     | `/inbox` per-call payload cap (integer KiB; tighter than `MAX_ITEM_MB` by design — see [Limits](#limits)) |
| `SCOPECACHE_SERVER_SECRET`        | *(unset)*                | HMAC key for `/guarded` and `/inbox`; empty/unset disables both routes |
| `SCOPECACHE_INBOX_SCOPES`         | *(unset)*                | Newline-separated list of scope names `/inbox` accepts; empty/unset disables `/inbox` |

## Limits

Several independent caps apply; any violation returns **HTTP 507 Insufficient Storage** (per-scope, store-wide, per-response) or **HTTP 400 Bad Request** (shape rules: per-item, `/multi_call` count and body). The cache never evicts on its own — clients free capacity via `/delete_up_to`, `/delete_scope`, `/wipe`, or a fitting `/warm`/`/rebuild`.

- **Per-scope item cap** — 100,000 items (default).
- **Store-wide byte cap** — 100 MiB aggregate (default).
- **Per-item cap** — 1 MiB (default); enforced on the approximate item size (overhead + scope + id + payload). Raise it via `SCOPECACHE_MAX_ITEM_MB` when the use-case stores larger blobs (rendered HTML, large JSON documents).
- **Per-response cap** — 25 MiB (default); applies uniformly to every HTTP response, including `/multi_call`'s outer envelope. Prevents pathological responses from `/tail?limit=10000` over 1 MiB items, multi-tenant `/stats` enumeration, and accumulated batch aggregates.
- **`/multi_call` caps** — 16 MiB input body and 10 sub-calls per batch (defaults). Both bound dispatcher work per HTTP request; raise them on bigger hardware.
- **`/inbox` payload cap** — 64 KiB (default); KiB-granular because the meaningful range for tenant-pushed events is sub-MiB. Tighter than the per-item cap by design: `/inbox` is fire-and-forget tenant ingestion that the operator drains in batches via `/admin /tail`, and a 1 MiB ceiling lets one rogue tenant fill response budgets very fast. Override via `SCOPECACHE_MAX_INBOX_KB` (env) or `max_inbox_kb` (Caddyfile). Per-request shape rule — overflow returns `400`, not `507`.

## Eviction / TTL

scopecache has no per-item TTL and no background eviction thread. This is deliberate: the cache is rebuildable-from-source by design, so the intended time-based eviction pattern is a **periodic `/warm` or `/rebuild`** — the client queries the source (`WHERE sent_at > now() - 24h`, or whatever window fits) and hands the resulting slice to scopecache in one atomic call. Items that fall outside the window simply disappear at the next refresh. This matches the materialized-view mental model (Postgres matviews, ISR-style static regeneration, build-artifact caches) rather than the Redis/Memcached per-key TTL model. The tradeoff is bursty (rebuilds re-allocate the scope in one shot) where TTL-based eviction is smoother — but in return you get atomic consistency: a scope is always either the previous complete slice or the new one, never half-expired.

Items *can* carry an optional client-supplied `ts` (int64), but it is a **query primitive, not an eviction signal**: `/ts_range` uses it to filter a server-side time window (e.g. "messages for user:x in the last hour"), and the cache itself never reads `ts` to decide when to delete anything. Scope-level cleanup is operator-initiated via `/delete_scope_candidates`, which surfaces idle scopes ranked by cache-owned `last_access_ts`.

**Caveat when a scope is used as a write-buffer.** `/rebuild` and `/warm` replace scope contents wholesale from whatever the client passes in. If the drain worker has not yet committed pending appends to the source of truth, rebuilding from that source will silently erase them — the undrained items are not there yet. Either drain first (trim with `/delete_up_to` after a successful commit, *then* rebuild if needed), or keep write-buffer scopes separate from cache scopes that are periodically rebuilt. The two patterns compose cleanly on *different* scopes; on the same scope they are mutually exclusive.

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
