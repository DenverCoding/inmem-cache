#!/usr/bin/env bash
# bench.sh — repeatable single-version benchmark for scopecache.
#
# For a given git tag (or branch), spins up a caddyscope container
# (Caddy + scopecache module via xcaddy) with generous capacity caps,
# then runs one of four canned workloads 5 times
# and reports per-run rps + latency percentiles, error counts, and
# the median across runs.
#
# Quickstart:
#   ./bench.sh                                  # main, all 6 tests
#   ./bench.sh v0.8.0                           # v0.8.0, all 6 tests
#   ./bench.sh main get-seq                     # single test
#   ./bench.sh main append-same e-full          # single test, events full
#
# Modes:
#   all                  Default if no mode given. Runs the 6-test sweep:
#                        get-seq, get, append-unique, append-same,
#                        append-same-e-not, append-same-e-full. Single
#                        combined summary table at the end.
#   get                  GET /get cycling through ids (item-1..item-50000)
#                        in a 50k-item scope. Hits b.byID map.
#   get-seq              GET /get on a RANDOM seq in 1..50000 in the same
#                        50k-item scope. Hits b.bySeq map. Diff against
#                        `get` shows whether id-vs-seq lookup costs differ
#                        on the host's cache hierarchy.
#   append-unique        POST /append with a fresh unique scope per request.
#                        Stresses scope-create + lazy-init paths.
#   append-same          POST /append all into one scope ("single"). Stresses
#                        the per-scope buf.mu under high write concurrency.
#   append-same-e-not    Like append-same, but with events_mode=notify
#                        baked in. Measures the wakeup-only events overhead
#                        on the most contended write path.
#   append-same-e-full   Like append-same, but with events_mode=full baked
#                        in. Measures the full _events auto-populate cost
#                        on the same write path.
#
# wrk load (hardcoded; saturation regime matched to the VPS-shaped server):
#   -t50 -c1000 -d3s --latency --timeout 2s
#   Per-knob env-var overrides exist for one-off experiments:
#   WRK_THREADS, WRK_CONNECTIONS, WRK_DURATION, RUNS.
#
# Events (optional last arg; e-off if omitted):
#   e-off     events_mode off    (Events.Mode zero-value)
#   e-not     events_mode notify (wakeup-only)
#   e-full    events_mode full   (auto-populate _events with envelopes)
#   The events arg is IGNORED for *-e-not / *-e-full modes — those carry
#   their own events_mode in the mode name.
#
# Version resolution:
#   <version> can be any git ref (tag, branch, sha). For directory
#   names the script resolves it to the closest reachable tag, so
#   `main` becomes `v0.8.0` (or whatever the latest tag is) — the
#   filename always identifies the actual release, not the branch.
#
# Server runs on cpuset 4-12 (~9 cores), wrk on cpuset 0-3,
# server memory capped at 16 GiB. Hardcoded; this combo gave the
# best throughput on the dev host (Windows + WSL2 + Docker Desktop)
# during pre-v1.0 hardening. Adjust SERVER_CPUSET below if your
# host has different topology.
#
# CPU pinning rationale: putting wrk and the server on disjoint cores
# avoids the load generator stealing cycles from the system under
# test. On a 32-core host this leaves wrk 4 cores and the server 28
# cores. Adjust the pinning below if your machine has a different
# core count. NOTE: other unpinned containers on the host can still
# land on either set, so this is internal isolation, not host-wide
# isolation.
#
# Server config (the in-script Caddyfile):
#   scope_max_items 10_000_000
#   max_store_mb    8192     (8 GiB)
#   max_item_mb     16
#
# Output:
#   - Per-run line: rps, p50, p99, non-2xx count, timeout count, and
#     a breakdown of any non-200 status codes (from a Lua hook).
#   - Per-test summary row in markdown-table shape with the columns:
#         | Mode | RPS | p50 | p99 | Timeouts | Errors |
#     RPS / p50 / p99 are medians across the 5 runs. Timeouts is the
#     total count over all runs (each is a wrk request that exceeded
#     the 2s timeout). Errors is the total non-2xx responses + wrk
#     socket-errors (connect / read / write) over all runs. The row
#     is paste-ready into _phase4/CLAUDE.md per-version notes.
#   - In `all` mode: each of the 6 sub-runs prints its own summary,
#     followed by a single combined 6-row table at the end.
#   - CSV per sub-run at
#     bench-results/<resolved-version>-<mode>[-<events>]-<ts>/results.csv
#
# Requirements:
#   - bash, git, curl, awk, sed
#   - Docker (with `docker compose build`)
#   - On Windows: Git-Bash (the script is POSIX-clean except for one
#     MSYS_NO_PATHCONV guard on Docker volume mounts).
#
# Path note (Windows Docker Desktop): wrk Lua scripts MUST live under
# the repo tree (not /tmp) because Git-Bash + MSYS_NO_PATHCONV path
# translation breaks /tmp mounts (wrk falls back to default GET = 405
# without warning). The script writes the Lua scripts under the
# results dir for safety.

set -uo pipefail

# --- arg parsing ---
case "${1:-}" in
    -h|--help)
        cat <<USAGE
usage: $0 [version] [mode] [events]
  [version]   git tag/branch (default: main)
  [mode]      all | get | get-seq | append-unique | append-same
              | append-same-e-not | append-same-e-full
              (default: all = sweep of 6 tests in one shot)
  [events]    e-off | e-not | e-full (default: e-off; ignored for
              *-e-not / *-e-full modes which carry their own)

  wrk load is hardcoded to -t50 -c1000 -d3s (saturation regime,
  matched to the VPS-shaped server cpuset 4-12). Per-knob env-var
  overrides still work: WRK_THREADS, WRK_CONNECTIONS, WRK_DURATION,
  RUNS.

  Examples:
    $0                                    # main, all 6 tests
    $0 v0.8.0                             # v0.8.0, all 6 tests
    $0 main get-seq                       # single test
    $0 main append-same e-full            # single test, events full
USAGE
        exit 0
        ;;
esac

VERSION="${1:-main}"
shift || true

# Mode defaults to "all" (the 6-test sweep).
MODE="${1:-all}"
shift || true

# Optional events arg (e-off | e-not | e-full). Default e-off.
EVENTS_ARG="${1:-e-off}"

case "$MODE" in
    all|get|get-seq|append-unique|append-same|append-same-e-not|append-same-e-full) ;;
    *) echo "invalid mode: $MODE"; exit 2 ;;
esac

# For modes with events suffix, force EVENTS_MODE/EVENTS_ARG and
# strip the suffix into BASE_MODE for the Lua/URL picker. For plain
# modes, BASE_MODE == MODE and events stays from the positional arg.
case "$MODE" in
    *-e-not)
        EVENTS_MODE="notify"
        EVENTS_ARG="e-not"
        BASE_MODE="${MODE%-e-not}"
        ;;
    *-e-full)
        EVENTS_MODE="full"
        EVENTS_ARG="e-full"
        BASE_MODE="${MODE%-e-full}"
        ;;
    *)
        BASE_MODE="$MODE"
        # Translate events arg to canonical Caddyfile value.
        case "$EVENTS_ARG" in
            e-off)  EVENTS_MODE="off" ;;
            e-not)  EVENTS_MODE="notify" ;;
            e-full) EVENTS_MODE="full" ;;
            *) echo "invalid events arg: $EVENTS_ARG (expected: e-off | e-not | e-full)"; exit 2 ;;
        esac
        ;;
esac

# wrk load is hardcoded to saturate the VPS-shaped server (cpuset
# 4-12, ~9 cores). Per-knob env-var overrides still win
# (RUNS / WRK_THREADS / WRK_CONNECTIONS / WRK_DURATION).
PRESET_THREADS=50
PRESET_CONNECTIONS=1000
PRESET_DURATION=3s

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TS="$(date +%Y%m%d-%H%M%S)"

# Resolve <version> to the closest reachable git tag so dir names
# always identify the actual release ("main" → "v0.8.0", etc.).
# Fallback to the input string if no tag is reachable (e.g. detached
# branch with no ancestor tag).
TARGET_SHA="$(git -C "$REPO_ROOT" rev-parse "$VERSION" 2>/dev/null || true)"
if [[ -n "$TARGET_SHA" ]]; then
    RESOLVED_VERSION="$(git -C "$REPO_ROOT" describe --tags --abbrev=0 "$TARGET_SHA" 2>/dev/null || echo "$VERSION")"
else
    RESOLVED_VERSION="$VERSION"
fi

# --- all-sweep handler ---
# When MODE=all, sub-invoke this script for each of the 6 canonical
# tests, collect summary rows via a shared file, then print one
# combined table. Sub-invocations also stream their own per-run
# output so progress is visible.
if [[ "$MODE" == "all" ]]; then
    SWEEP_MODES=(get-seq get append-unique append-same append-same-e-not append-same-e-full)
    SWEEP_TMP="$(mktemp -d)"
    SWEEP_FILE="$SWEEP_TMP/summary.txt"
    : > "$SWEEP_FILE"

    for m in "${SWEEP_MODES[@]}"; do
        ALL_SWEEP_SUMMARY="$SWEEP_FILE" bash "$0" "$VERSION" "$m"
    done

    echo
    echo "================ combined sweep summary ================"
    printf '  | %-19s | %-8s | %-8s | %-8s | %-8s | %-6s |\n' \
        "Mode" "RPS" "p50" "p99" "Timeouts" "Errors"
    printf '  |%s|%s|%s|%s|%s|%s|\n' \
        "---------------------" "----------" "----------" "----------" "----------" "--------"
    cat "$SWEEP_FILE"
    echo
    echo "  version: $RESOLVED_VERSION (input: $VERSION)"
    echo "  config:  wrk -t50 -c1000 -d3s, server cpuset 4-12, --memory=16g"
    rm -rf "$SWEEP_TMP"
    exit 0
fi

# Mode segment in dir name (events bakes in via -e-not/-e-full
# suffixed modes; for plain modes append events arg only when
# non-default to avoid noisy dir names).
case "$MODE" in
    *-e-not|*-e-full) DIR_EVENTS="" ;;
    *)
        if [[ "$EVENTS_ARG" == "e-off" ]]; then DIR_EVENTS="";
        else DIR_EVENTS="-$EVENTS_ARG"; fi
        ;;
esac
RESULTS_DIR="$REPO_ROOT/bench-results/$RESOLVED_VERSION-$MODE$DIR_EVENTS-$TS"
mkdir -p "$RESULTS_DIR"

CONTAINER="scopecache-bench"
NETWORK="scopecache-bench"
PORT=8084
WRK_CPUSET="0-3"
SERVER_CPUSET="4-12"
RUNS="${RUNS:-5}"
WRK_THREADS="${WRK_THREADS:-$PRESET_THREADS}"
WRK_CONNECTIONS="${WRK_CONNECTIONS:-$PRESET_CONNECTIONS}"
WRK_DURATION="${WRK_DURATION:-$PRESET_DURATION}"

# --- ensure dedicated docker network exists ---
docker network inspect "$NETWORK" >/dev/null 2>&1 \
    || docker network create "$NETWORK" >/dev/null

# --- save state, restore on exit ---
ORIG_REF="$(git -C "$REPO_ROOT" rev-parse --abbrev-ref HEAD)"
[[ "$ORIG_REF" == "HEAD" ]] && ORIG_REF="$(git -C "$REPO_ROOT" rev-parse HEAD)"

restore_state() {
    docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
    if [[ -n "${CADDYFILE_BACKUP:-}" && -f "$CADDYFILE_BACKUP" ]]; then
        mv -f "$CADDYFILE_BACKUP" "$CADDYFILE_PATH"
    fi
    git -C "$REPO_ROOT" checkout -q "$ORIG_REF" 2>/dev/null || true
}
trap restore_state EXIT

# --- checkout target ---
echo "[setup] checkout $VERSION"
if ! git -C "$REPO_ROOT" checkout -q "$VERSION" 2>&1; then
    echo "[setup] checkout failed"
    exit 1
fi

# --- detect Caddyfile location ---
# Post-reorg tags (v0.7.18+) keep Caddyfile.caddyscope under deploy/;
# pre-reorg tags have it at the repo root. Detect after checkout so
# benchmarks against historical versions still work.
if [[ -f "$REPO_ROOT/deploy/Caddyfile.caddyscope" ]]; then
    CADDYFILE_PATH="$REPO_ROOT/deploy/Caddyfile.caddyscope"
else
    CADDYFILE_PATH="$REPO_ROOT/Caddyfile.caddyscope"
fi
CADDYFILE_BACKUP="${CADDYFILE_PATH}.bench-backup"

# --- generous Caddyfile ---
[[ ! -f "$CADDYFILE_BACKUP" ]] && \
    cp "$CADDYFILE_PATH" "$CADDYFILE_BACKUP"

# EVENTS_MODE is always set (off | notify | full) — write the line
# unconditionally. `off` is Events.Mode's zero-value so this is
# functionally identical to omitting the line.
EVENTS_LINE="        events_mode     $EVENTS_MODE"

cat > "$CADDYFILE_PATH" <<CADDYFILE
{
    admin off
    log {
        output stdout
        format console
    }
}

:8080 {
    @html_render {
        path /render
        query scope=html
    }
    header @html_render Content-Type "text/html; charset=utf-8" {
        defer
    }
    scopecache {
        scope_max_items 10000000
        max_store_mb    8192
        max_item_mb     16
$EVENTS_LINE
    }
}
CADDYFILE

# --- build + run ---
echo "[setup] building caddyscope image..."
(cd "$REPO_ROOT" && docker compose build caddyscope) 2>&1 | tail -2 \
    || { echo "[setup] build failed"; exit 1; }

docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
MSYS_NO_PATHCONV=1 docker run -d \
    --name "$CONTAINER" \
    --network "$NETWORK" \
    --cpuset-cpus="$SERVER_CPUSET" \
    --memory=16g \
    --memory-swap=16g \
    -p "$PORT:8080" \
    -v "$CADDYFILE_PATH:/etc/caddy/Caddyfile:ro" \
    -e SCOPECACHE_SERVER_SECRET=test-secret \
    caddy_module-caddyscope \
    caddy run --config /etc/caddy/Caddyfile --adapter caddyfile \
    >/dev/null \
    || { echo "[setup] container start failed"; exit 1; }

sleep 4

if ! curl -sfm 5 -X POST -H 'Content-Type: application/json' \
    -d '{"scope":"warmup","payload":{"x":1}}' \
    "http://localhost:$PORT/append" >/dev/null 2>&1; then
    echo "[setup] /append warmup FAILED"
    docker logs "$CONTAINER" 2>&1 | tail -10
    exit 1
fi

# --- Lua scripts (in repo path, NOT /tmp) ---
cat > "$RESULTS_DIR/append-unique.lua" <<'LUA'
local thread_count = 0
function setup(thread)
    thread:set("tid", thread_count)
    thread_count = thread_count + 1
end
function init(args)
    counter = 0
    base = os.time()
    headers = { ["Content-Type"] = "application/json" }
    bad_status = {}
end
function request()
    counter = counter + 1
    local scope = string.format("wrk-%d-%d-%d", base, tid, counter)
    local body = string.format('{"scope":"%s","payload":{"i":%d}}', scope, counter)
    return wrk.format("POST", "/append", headers, body)
end
function response(status)
    if status ~= 200 then
        bad_status[status] = (bad_status[status] or 0) + 1
    end
end
function done(summary, latency, requests)
    for s, n in pairs(bad_status) do
        io.stderr:write("BADSTATUS " .. s .. " " .. n .. "\n")
    end
end
LUA

cat > "$RESULTS_DIR/append-same.lua" <<'LUA'
function init(args)
    counter = 0
    headers = { ["Content-Type"] = "application/json" }
    bad_status = {}
end
function request()
    counter = counter + 1
    local body = string.format('{"scope":"single","payload":{"i":%d}}', counter)
    return wrk.format("POST", "/append", headers, body)
end
function response(status)
    if status ~= 200 then
        bad_status[status] = (bad_status[status] or 0) + 1
    end
end
function done(summary, latency, requests)
    for s, n in pairs(bad_status) do
        io.stderr:write("BADSTATUS " .. s .. " " .. n .. "\n")
    end
end
LUA

cat > "$RESULTS_DIR/get.lua" <<'LUA'
function init(args)
    counter = 0
    bad_status = {}
end
function request()
    counter = counter + 1
    local id = (counter % 50000) + 1
    return wrk.format("GET", "/get?scope=bench&id=item-" .. id)
end
function response(status)
    if status ~= 200 then
        bad_status[status] = (bad_status[status] or 0) + 1
    end
end
function done(summary, latency, requests)
    for s, n in pairs(bad_status) do
        io.stderr:write("BADSTATUS " .. s .. " " .. n .. "\n")
    end
end
LUA

cat > "$RESULTS_DIR/get-seq.lua" <<'LUA'
local thread_count = 0
function setup(thread)
    thread:set("tid", thread_count)
    thread_count = thread_count + 1
end
function init(args)
    -- Each thread seeds its RNG independently so the two threads do
    -- not march in lock-step through the same sequence.
    math.randomseed(os.time() + (tid or 0) * 1000)
    bad_status = {}
end
function request()
    local seq = math.random(1, 50000)
    return wrk.format("GET", "/get?scope=bench&seq=" .. seq)
end
function response(status)
    if status ~= 200 then
        bad_status[status] = (bad_status[status] or 0) + 1
    end
end
function done(summary, latency, requests)
    for s, n in pairs(bad_status) do
        io.stderr:write("BADSTATUS " .. s .. " " .. n .. "\n")
    end
end
LUA

# Seed Lua: pump as many item-N entries as possible into scope "bench"
# in 2 seconds via wrk. Threaded counter avoids id collisions.
cat > "$RESULTS_DIR/seed.lua" <<'LUA'
local thread_count = 0
function setup(thread)
    thread:set("tid", thread_count)
    thread_count = thread_count + 1
end
function init(args)
    counter = 0
    headers = { ["Content-Type"] = "application/json" }
end
function request()
    counter = counter + 1
    -- 50_000 ids spread across threads: thread 0 owns 1..25_000,
    -- thread 1 owns 25_001..50_000.
    local id = (tid * 25000) + counter
    if id > 50000 then return nil end
    local body = string.format('{"scope":"bench","id":"item-%d","payload":{"v":%d}}', id, id)
    return wrk.format("POST", "/append", headers, body)
end
LUA

# --- helpers ---
wipe() {
    curl -s -X POST "http://localhost:$PORT/wipe" >/dev/null
}

seed_for_get() {
    # Seed via wrk: 2 threads, 8 connections, 2 seconds. Lua script
    # caps each thread's counter at 25_000 ids so the union covers
    # 1..50_000 with no duplicates. wrk stops issuing requests once
    # both threads return nil from request().
    MSYS_NO_PATHCONV=1 docker run --rm --network "$NETWORK" \
        --cpuset-cpus="$WRK_CPUSET" \
        -v "$RESULTS_DIR/seed.lua:/script.lua:ro" \
        williamyeh/wrk -t2 -c8 -d2s --timeout 2s -s /script.lua \
        "http://$CONTAINER:8080/append" >/dev/null 2>&1 || true
}

run_wrk() {
    local lua="$1" url="$2"
    MSYS_NO_PATHCONV=1 docker run --rm --network "$NETWORK" \
        --cpuset-cpus="$WRK_CPUSET" \
        -v "$lua:/script.lua:ro" \
        williamyeh/wrk -t"$WRK_THREADS" -c"$WRK_CONNECTIONS" -d"$WRK_DURATION" --latency --timeout 2s -s /script.lua \
        "$url" 2>&1
}

# Convert wrk latency strings (123us / 4.5ms / 1.2s) to microseconds (integer).
to_us() {
    awk -v v="${1:-0}" 'BEGIN {
        if (v ~ /us$/)      { sub(/us$/, "", v); printf "%d", v + 0 }
        else if (v ~ /ms$/) { sub(/ms$/, "", v); printf "%d", (v + 0) * 1000 }
        else if (v ~ /s$/)  { sub(/s$/, "", v);  printf "%d", (v + 0) * 1000000 }
        else                { printf "%d", v + 0 }
    }'
}

# Format a microsecond integer to a wrk-style display string.
fmt_us() {
    awk -v v="${1:-0}" 'BEGIN {
        if (v >= 1000000) printf "%.2f s", v/1000000
        else if (v >= 1000) printf "%.2f ms", v/1000
        else printf "%d us", v
    }'
}

# Sorted-median of a space-separated list of integers.
median_int() {
    printf '%s\n' "$@" | awk '
        { vals[NR] = $1 + 0 }
        END {
            n = NR
            for (i = 1; i <= n; i++)
                for (j = i + 1; j <= n; j++)
                    if (vals[i] > vals[j]) { t = vals[i]; vals[i] = vals[j]; vals[j] = t }
            print int(vals[int((n + 1) / 2)])
        }
    '
}

# --- pick mode ---
# Lua/URL pick uses BASE_MODE so events-suffixed modes
# (append-same-e-not / -e-full) reuse the plain Lua script.
case "$BASE_MODE" in
    get)            URLPATH=get;    LUA="$RESULTS_DIR/get.lua" ;;
    get-seq)        URLPATH=get;    LUA="$RESULTS_DIR/get-seq.lua" ;;
    append-unique)  URLPATH=append; LUA="$RESULTS_DIR/append-unique.lua" ;;
    append-same)    URLPATH=append; LUA="$RESULTS_DIR/append-same.lua" ;;
esac
URL="http://$CONTAINER:8080/$URLPATH"

# --- header ---
VERSION_DISPLAY="$RESOLVED_VERSION"
[[ "$RESOLVED_VERSION" != "$VERSION" ]] && VERSION_DISPLAY="$RESOLVED_VERSION (input: $VERSION)"

echo
echo "================================================================"
echo "  scopecache bench"
echo "  version: $VERSION_DISPLAY"
echo "  mode:    $MODE"
echo "  events:  $EVENTS_ARG ($EVENTS_MODE)"
echo "  wrk:     -t$WRK_THREADS -c$WRK_CONNECTIONS -d$WRK_DURATION --latency --timeout 2s, cpuset $WRK_CPUSET"
echo "  server:  caddyscope, cpuset $SERVER_CPUSET, --memory=16g"
echo "  config:  scope_max_items=10M, max_store_mb=8192, max_item_mb=16"
echo "================================================================"
echo

# --- runs ---
RESULTS_CSV="$RESULTS_DIR/results.csv"
echo "run,rps,p50_us,p99_us,non2xx,timeouts,status_breakdown" > "$RESULTS_CSV"

declare -a rps_arr p50_arr p99_arr
total_non2xx=0
total_timeouts=0
total_socket_errors=0

printf '%s\n' "  run | rps      | p50      | p99       | non-2xx | timeouts | status detail"
printf '%s\n' "  ----+----------+----------+-----------+---------+----------+--------------"

# /get and /get-seq are read-modes: seed ONCE up front, then no /wipe
# between runs (a wipe would erase the items we just read). For
# append-* modes the /wipe-per-run pattern stays — those measure
# scope-create + write throughput from a clean state.
if [[ "$BASE_MODE" == "get" || "$BASE_MODE" == "get-seq" ]]; then
    wipe
    sleep 1
    seed_for_get
fi

for run in $(seq 1 "$RUNS"); do
    if [[ "$BASE_MODE" != "get" && "$BASE_MODE" != "get-seq" ]]; then
        wipe
        sleep 1
    fi

    out="$(run_wrk "$LUA" "$URL")"

    rps=$(echo "$out" | awk '/Requests\/sec/ {print $2; exit}')
    p50_raw=$(echo "$out" | awk '/^[[:space:]]+50%/ {print $2; exit}')
    p99_raw=$(echo "$out" | awk '/^[[:space:]]+99%/ {print $2; exit}')
    non2xx=$(echo "$out" | awk '/Non-2xx or 3xx/ {print $4; exit}')

    socket_line=$(echo "$out" | grep -E "Socket errors:" | head -1)
    if [[ -n "$socket_line" ]]; then
        connect=$(echo "$socket_line" | sed -nE 's/.*connect ([0-9]+).*/\1/p')
        read_e=$(echo  "$socket_line" | sed -nE 's/.*read ([0-9]+).*/\1/p')
        write_e=$(echo "$socket_line" | sed -nE 's/.*write ([0-9]+).*/\1/p')
        timeouts=$(echo "$socket_line" | sed -nE 's/.*timeout ([0-9]+).*/\1/p')
        socket_total=$(( ${connect:-0} + ${read_e:-0} + ${write_e:-0} ))
    else
        timeouts=0
        socket_total=0
    fi
    non2xx="${non2xx:-0}"
    timeouts="${timeouts:-0}"
    socket_total="${socket_total:-0}"

    bad_detail=$(echo "$out" | grep '^BADSTATUS' \
        | awk '{printf "%s:%s ", $2, $3}')
    bad_detail="${bad_detail:--}"

    p50_us=$(to_us "$p50_raw")
    p99_us=$(to_us "$p99_raw")

    rps_arr+=("$rps")
    p50_arr+=("$p50_us")
    p99_arr+=("$p99_us")
    total_non2xx=$((total_non2xx + non2xx))
    total_timeouts=$((total_timeouts + timeouts))
    total_socket_errors=$((total_socket_errors + socket_total))

    printf '  %-3s | %-8s | %-8s | %-9s | %-7s | %-8s | %s\n' \
        "$run" "$rps" "$p50_raw" "$p99_raw" "$non2xx" "$timeouts" "$bad_detail"

    echo "$run,$rps,$p50_us,$p99_us,$non2xx,$timeouts,\"$bad_detail\"" >> "$RESULTS_CSV"
done

# --- summary row ---
# Single tabular line in `Preset RPS p50 p99 Timeouts Errors` shape,
# paste-ready into _phase4/CLAUDE.md. Errors = non-2xx + socket
# errors combined (timeouts is its own column).
median_pos=$(( (RUNS + 1) / 2 ))
median_rps_int=$(printf '%s\n' "${rps_arr[@]}" | awk '{print int($1+0)}' \
    | sort -n | awk -v pos="$median_pos" 'NR==pos{print}')
median_p50=$(median_int "${p50_arr[@]}")
median_p99=$(median_int "${p99_arr[@]}")
total_errors=$(( total_non2xx + total_socket_errors ))

SUMMARY_ROW=$(printf '  | %-19s | %-8s | %-8s | %-8s | %-8s | %-6s |' \
    "$MODE" "$median_rps_int" "$(fmt_us "$median_p50")" "$(fmt_us "$median_p99")" \
    "$total_timeouts" "$total_errors")

echo
printf '  | %-19s | %-8s | %-8s | %-8s | %-8s | %-6s |\n' \
    "Mode" "RPS" "p50" "p99" "Timeouts" "Errors"
printf '  |%s|%s|%s|%s|%s|%s|\n' \
    "---------------------" "----------" "----------" "----------" "----------" "--------"
echo "$SUMMARY_ROW"

# When called as part of an all-sweep (parent sets ALL_SWEEP_SUMMARY),
# also append this row to the shared file so the parent can build the
# combined table after all 6 sub-runs finish.
[[ -n "${ALL_SWEEP_SUMMARY:-}" ]] && echo "$SUMMARY_ROW" >> "$ALL_SWEEP_SUMMARY"

echo
echo "  results: $RESULTS_CSV"
echo
