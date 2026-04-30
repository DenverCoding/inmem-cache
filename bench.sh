#!/usr/bin/env bash
# bench.sh — repeatable single-version benchmark for scopecache.
#
# For a given git tag (or branch), spins up a caddyscope container
# (Caddy + scopecache module via xcaddy) with generous capacity caps
# and admin enabled, then runs one of four canned workloads 5 times
# and reports per-run rps + latency percentiles, error counts, and
# the median across runs.
#
# Quickstart:
#   ./bench.sh v0.7.10 append-unique
#
# Modes:
#   get             GET /get cycling through ids (item-1..item-50000)
#                   in a 50k-item scope. Hits b.byID map.
#   get-seq         GET /get on a RANDOM seq in 1..50000 in the same
#                   50k-item scope. Hits b.bySeq map. Diff against
#                   `get` shows whether id-vs-seq lookup costs differ
#                   on the host's cache hierarchy.
#   append-unique   POST /append with a fresh unique scope per request.
#                   Stresses scope-create + lazy-init paths.
#   append-same     POST /append all into one scope ("single"). Stresses
#                   the per-scope buf.mu under high write concurrency.
#
# Each run uses:
#   wrk    -t2 -c32 -d3s --latency --timeout 2s, cpuset 0-3
#   server caddyscope, cpuset 4-31
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
#   enable_admin    yes      (so /wipe via /admin works between runs)
#
# Output:
#   - Per-run line: rps, p50, p99, non-2xx count, timeout count, and
#     a breakdown of any non-200 status codes (from a Lua hook).
#   - Median rps/p50/p99 across the 5 runs.
#   - Totals for non-2xx, timeouts, and socket errors across all runs.
#   - CSV at bench-results/<version>-<mode>-<timestamp>/results.csv
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

VERSION="${1:-}"
MODE="${2:-}"

if [[ -z "$VERSION" || -z "$MODE" ]]; then
    cat <<USAGE
usage: $0 <version> <mode>
  <version>  git tag/branch (e.g. v0.7.10)
  <mode>     get | get-seq | append-unique | append-same
USAGE
    exit 2
fi

case "$MODE" in
    get|get-seq|append-unique|append-same) ;;
    *) echo "invalid mode: $MODE (expected: get | get-seq | append-unique | append-same)"; exit 2 ;;
esac

REPO_ROOT="$(cd "$(dirname "$0")" && pwd)"
TS="$(date +%Y%m%d-%H%M%S)"
RESULTS_DIR="$REPO_ROOT/bench-results/$VERSION-$MODE-$TS"
mkdir -p "$RESULTS_DIR"

CONTAINER="scopecache-bench"
NETWORK="scopecache-bench"
PORT=8084
WRK_CPUSET="0-3"
SERVER_CPUSET="4-31"

# --- ensure dedicated docker network exists ---
docker network inspect "$NETWORK" >/dev/null 2>&1 \
    || docker network create "$NETWORK" >/dev/null

# --- save state, restore on exit ---
ORIG_REF="$(git -C "$REPO_ROOT" rev-parse --abbrev-ref HEAD)"
[[ "$ORIG_REF" == "HEAD" ]] && ORIG_REF="$(git -C "$REPO_ROOT" rev-parse HEAD)"

restore_state() {
    docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
    if [[ -f "$REPO_ROOT/Caddyfile.caddyscope.bench-backup" ]]; then
        mv -f "$REPO_ROOT/Caddyfile.caddyscope.bench-backup" \
              "$REPO_ROOT/Caddyfile.caddyscope"
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

# --- generous Caddyfile ---
[[ ! -f "$REPO_ROOT/Caddyfile.caddyscope.bench-backup" ]] && \
    cp "$REPO_ROOT/Caddyfile.caddyscope" "$REPO_ROOT/Caddyfile.caddyscope.bench-backup"

cat > "$REPO_ROOT/Caddyfile.caddyscope" <<'CADDYFILE'
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
        server_secret   {$SCOPECACHE_SERVER_SECRET}
        enable_admin    yes
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
    -p "$PORT:8080" \
    -v "$REPO_ROOT/Caddyfile.caddyscope:/etc/caddy/Caddyfile:ro" \
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
    curl -s -X POST -H 'Content-Type: application/json' \
        -d '{"calls":[{"path":"/wipe"}]}' \
        "http://localhost:$PORT/admin" >/dev/null
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
        williamyeh/wrk -t2 -c32 -d3s --latency --timeout 2s -s /script.lua \
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
case "$MODE" in
    get)            URLPATH=get;    LUA="$RESULTS_DIR/get.lua" ;;
    get-seq)        URLPATH=get;    LUA="$RESULTS_DIR/get-seq.lua" ;;
    append-unique)  URLPATH=append; LUA="$RESULTS_DIR/append-unique.lua" ;;
    append-same)    URLPATH=append; LUA="$RESULTS_DIR/append-same.lua" ;;
esac
URL="http://$CONTAINER:8080/$URLPATH"

# --- header ---
echo
echo "================================================================"
echo "  scopecache bench"
echo "  version: $VERSION"
echo "  mode:    $MODE"
echo "  wrk:     -t2 -c32 -d3s --latency --timeout 2s, cpuset $WRK_CPUSET"
echo "  server:  caddyscope, cpuset $SERVER_CPUSET"
echo "  config:  scope_max_items=10M, max_store_mb=8192, max_item_mb=16"
echo "================================================================"
echo

# --- 5 runs ---
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
if [[ "$MODE" == "get" || "$MODE" == "get-seq" ]]; then
    wipe
    sleep 1
    seed_for_get
fi

for run in 1 2 3 4 5; do
    if [[ "$MODE" != "get" && "$MODE" != "get-seq" ]]; then
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

# --- median + totals ---
median_rps_int=$(printf '%s\n' "${rps_arr[@]}" | awk '{print int($1+0)}' \
    | sort -n | awk 'NR==3{print}')
median_p50=$(median_int "${p50_arr[@]}")
median_p99=$(median_int "${p99_arr[@]}")

echo
printf '  median: rps=%-8s  p50=%-10s  p99=%-10s\n' \
    "$median_rps_int" "$(fmt_us "$median_p50")" "$(fmt_us "$median_p99")"
echo
printf '  totals over 5 runs: non-2xx=%s timeouts=%s socket-errors=%s\n' \
    "$total_non2xx" "$total_timeouts" "$total_socket_errors"

echo
echo "  results: $RESULTS_CSV"
echo
