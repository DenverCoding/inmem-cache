# scripts/bench.sh

**At a glance:**

- starts a `caddyscope` Docker container on `:8084` (built via `docker compose`); requires Docker running
- bare `bench.sh` (no args) runs the **6-test sweep on `main`**
- wrk load is hardcoded to `-t50 -c1000 -d3s` (saturation regime, matched to the VPS-shaped server)
- server fixed at cpuset `4-12` (~9 cores), `--memory=16g`
- single combined summary table at the end of the sweep, paste-ready into `_phase4/CLAUDE.md`

Examples:

```bash
bench.sh                                # main, all 6 tests
bench.sh v0.8.0                         # v0.8.0 tag, all 6 tests
bench.sh main get-seq                   # single test
bench.sh main append-same e-full        # single test, events full
```

The 6 sweep tests are: `get-seq`, `get`, `append-unique`, `append-same`,
`append-same-e-not`, `append-same-e-full`.

---

Repeatable single-version HTTP benchmark for scopecache. Spins up
a `caddyscope` container (Caddy + scopecache module via xcaddy) on
`:8084`, runs one of seven canned workloads against it 5× via `wrk`,
and reports per-run rps + latency percentiles + error counts plus
the median across runs. The default `all` mode chains the six
canonical tests into one invocation.

The script is also a **regression test**: non-zero error counts in
the output are regressions, not noise.

## Modes

Six canonical modes that stay stable across the v0.8.X series so
per-version comparisons are direct, plus the `all` aggregator.
Don't add knobs to the canonical six mid-series; new modes get new
names.

| Mode | What it measures |
|---|---|
| `all` | **Default if no mode given.** Runs the 6-test sweep below in order. Single combined summary table at the end. |
| `get` | `GET /get?scope=…&id=…` cycling through ids (`item-1` .. `item-50000`) in a 50k-item scope. Hits `b.byID` map. |
| `get-seq` | `GET /get?scope=…&seq=…` on a random seq in 1..50000 in the same scope. Hits `b.bySeq` map. Diff against `get` shows whether id-vs-seq lookup costs differ on the host's cache hierarchy. |
| `append-unique` | `POST /append` with a fresh unique scope per request. Stresses the scope-create + lazy-init paths. |
| `append-same` | `POST /append` all into one scope (`"single"`). Stresses the per-scope `buf.mu` under high write concurrency. |
| `append-same-e-not` | Like `append-same` but with `events_mode=notify` baked in. Measures wakeup-only events overhead on the most contended write path. |
| `append-same-e-full` | Like `append-same` but with `events_mode=full` baked in. Measures the full `_events` auto-populate cost on the same write path. |

Mode names with `-e-not` / `-e-full` suffix carry their own
`events_mode` — the `[events]` positional arg is ignored for
those.

## wrk load (hardcoded)

wrk runs in saturation mode by default — `-t50 -c1000 -d3s` —
matched to the VPS-shaped server (cpuset 4-12, ~9 cores). Pre-1.0
hardening cares about "what does the cache actually do under
production-shaped traffic," not "how fast can we drive 32
connections."

The four wrk knobs are env-var-overridable for one-off
experiments without modifying the script:

| Knob | Default | Override env-var |
|---|---|---|
| wrk threads | 50 | `WRK_THREADS` |
| wrk connections | 1000 | `WRK_CONNECTIONS` |
| wrk duration | 3s | `WRK_DURATION` |
| Runs per mode | 5 | `RUNS` |

Trade-off: saturation produces ~10-15% run-to-run RPS spread, so
small (<10%) regressions may be invisible without bumping `RUNS`.
For fine-grained regression deltas, run `RUNS=10 WRK_DURATION=10s`
once. For the daily sweep, the defaults are fine.

## Events

For modes without an events-suffix, `events_mode` is set via the
last positional arg:

| Arg | Caddyfile value | Meaning |
|---|---|---|
| `e-off` | `events_mode off` | Events.Mode zero-value (no auto-populate) |
| `e-not` | `events_mode notify` | wakeup-only (no `_events` envelopes) |
| `e-full` | `events_mode full` | auto-populate `_events` with action-vector envelopes |

For `append-same-e-not` and `append-same-e-full` the events_mode is
fixed by the mode name; the events arg is ignored.

## Version resolution

`<version>` accepts any git ref (tag, branch, commit sha). For
filenames the script resolves it to the **closest reachable tag**
via `git describe --tags --abbrev=0`, so `main` shows up as
`v0.8.0` (or whatever the latest tag is) in the results dir name.
The bench header echoes both the resolved version and the original
input for clarity:

```
version: v0.8.0 (input: main)
```

If you pass a commit/branch with no ancestor tag, the input string
is used as-is.

## Other defaults

| Knob | Default |
|---|---|
| wrk timeout | 2s |
| wrk cpuset | 0-3 |
| Server cpuset | 4-12 |
| Server memory | 16 GiB |
| Server port | 8084 |

All hardcoded. Adjust in the script if your host has different
topology (see "wrk load" above for env-var overrides on the wrk
side).

## Server config (in-script Caddyfile)

```
scope_max_items 10_000_000
max_store_mb    8192     (8 GiB)
max_item_mb     16
events_mode     <off | notify | full>
```

## Usage

```bash
./scripts/bench.sh [version] [mode] [events]
```

All three positional args are optional:

- `[version]` — git ref. Default: `main`.
- `[mode]` — `all` | one of the 6 canonical modes. Default: `all`.
- `[events]` — `e-off` | `e-not` | `e-full`. Default: `e-off`.
  Ignored for events-suffixed modes.

`./scripts/bench.sh -h` prints the usage block.

### Examples

```bash
# Default everything: main, all 6 tests:
./scripts/bench.sh

# Specific version, all 6 tests:
./scripts/bench.sh v0.8.0

# Single test:
./scripts/bench.sh main get-seq

# Single test with events full:
./scripts/bench.sh main append-same e-full

# Tighter regression measurement (more runs, longer duration):
RUNS=10 WRK_DURATION=10s ./scripts/bench.sh v0.8.0
```

## Output

Per-run line shows: rps, p50, p99, non-2xx count, timeout count,
plus a breakdown of any non-200 status codes (from a Lua hook).

End-of-each-test shows a single summary row in markdown-table shape:

```
  | Mode                | RPS      | p50      | p99      | Timeouts | Errors |
  |---------------------|----------|----------|----------|----------|--------|
  | get-seq             | 198256   | 4.90 ms  | 28.98 ms | 0        | 0      |
```

In `all` mode, after the 6 sub-runs finish, a single combined table
is printed:

```
================ combined sweep summary ================
  | Mode                | RPS      | p50      | p99      | Timeouts | Errors |
  |---------------------|----------|----------|----------|----------|--------|
  | get-seq             | 198256   | 4.90 ms  | 28.98 ms | 0        | 0      |
  | get                 | 224915   | 4.22 ms  | 28.53 ms | 0        | 0      |
  | append-unique       | 145725   | 6.29 ms  | 34.70 ms | 0        | 0      |
  | append-same         | 179139   | 5.16 ms  | 40.67 ms | 121      | 0      |
  | append-same-e-not   | 159724   | 5.57 ms  | 47.24 ms | 0        | 0      |
  | append-same-e-full  | 164545   | 5.52 ms  | 49.97 ms | 0        | 0      |
```

Columns:

- **Mode** — the test mode.
- **RPS** — median requests/sec across the 5 runs.
- **p50** — median p50 latency across the 5 runs.
- **p99** — median p99 latency across the 5 runs.
- **Timeouts** — total wrk timeouts (requests that exceeded the
  2 s timeout) across all 5 runs combined.
- **Errors** — total non-2xx responses + wrk socket-errors
  (connect / read / write) across all 5 runs combined.

The combined table is paste-ready into `_phase4/CLAUDE.md`
per-version bench notes — copy directly under the run's
`### v0.8.X — <date>` header.

Per-run CSV (5 rows + header) persisted per sub-run to:

```
bench-results/<resolved-version>-<mode>[-<events>]-<timestamp>/results.csv
```

`bench-results/` is gitignored; results live locally only.

## Interpretation

**What each mode tells you:**

- `get` vs `get-seq` → cost of `byID` (string-keyed map) vs `bySeq`
  (uint64-keyed map). Diff is small but exists; useful when changing
  either map's allocation or growth strategy.
- `append-unique` → scope-create cost. Hits `getOrCreateScope`,
  `*scopeBuffer` alloc, `byID`/`bySeq` map allocation. Anti-pattern
  workload (see *Scope modeling* in CLAUDE.md), but it's the
  worst-case stress on the create path.
- `append-same` → `buf.mu` contention on the write side. All
  goroutines serialize through one scope's mutex.
- `append-same-e-not` vs `append-same` → wakeup-only events
  overhead on the most-contended write path. Notable on **p99 tail**
  more than median rps.
- `append-same-e-full` vs `append-same-e-not` → cost of the
  `_events` auto-populate envelopes on top of wakeup-only signaling.

**Saturation regime by design.** wrk hammers the server with 1000
concurrent connections, so the rps numbers reflect the cache's
*actual* throughput ceiling under production-shaped traffic, not
the loadgen-limited "how fast can wrk push 32 connections" number.
Trade-off: ~10-15% run-to-run noise on RPS. For tight regression
deltas (<10%), bump `RUNS=10 WRK_DURATION=10s` once.

**What it doesn't catch:**

- Anything past the HTTP path. The Gateway clone discipline
  (entry/exit `clonePayload`) is bypassed because HTTP traffic goes
  `*API → *store` directly (`NewAPI` extracts `gw.store`). For
  Gateway-level perf, see `bench_gateway_test.go` at the repo root.
- Long-tail GC effects (3s isn't long enough). For sustained-load
  perf, raise `WRK_DURATION` and `RUNS`.
- Anything PHP-touching (use `harness/` for that).

**Per-version notes** (deltas, regressions, headline numbers across
`v0.8.X`) live in `_phase4/CLAUDE.md`. Append the combined sweep
table there after each tagged release.

## Requirements

- bash, git, curl, awk, sed
- Docker (with `docker compose build` available)
- On Windows: Git-Bash. The script is POSIX-clean except for one
  `MSYS_NO_PATHCONV` guard on Docker volume mounts.
