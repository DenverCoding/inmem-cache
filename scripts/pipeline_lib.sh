# pipeline_lib.sh — composable helpers for parallel-runnable pipeline tests.
#
# Source from a scenario script:
#
#   REPO_ROOT=$(cd "$(dirname "$0")/.." && pwd)
#   PIPELINE_NAME="my-scenario"   # uniqueness for work dir + socket
#   BRIDGE_PORT=8088              # uniqueness for socat TCP listener
#   . "$REPO_ROOT/scripts/pipeline_lib.sh"
#
# Parallel-run contract: every scenario script claims a unique
# (PIPELINE_NAME, BRIDGE_PORT) pair so two scenarios can run in the
# same dev container at the same time without colliding on socket
# paths, work directories, or TCP listeners. Running the SAME scenario
# twice in parallel is not supported (would need a RUN_ID extension).
#
# Allocated bridge ports (claim the next free one when adding a
# scenario; keep this list updated):
#
#   8088  test_pipeline_append_under_reads.sh
#   8089  (reserved — update-under-reads, not yet built)
#   8090  (reserved — delete-under-reads)
#   8091  (reserved — mixed-under-reads)
#
# Two scopes by convention:
#   READS_SCOPE    = "reads"   — pre-seeded immutable read-load target
#   WRITE_SCOPE    = "counter" — scenario-controlled mutation target
# Different scopes so wrk hits a stable dataset while the scenario
# mutates its own scope; keeps wrk numbers independent of producer rate.

set -eu

PIPELINE_NAME="${PIPELINE_NAME:-pipeline}"
READS_SCOPE="${READS_SCOPE:-reads}"
WRITE_SCOPE="${WRITE_SCOPE:-counter}"
BRIDGE_PORT="${BRIDGE_PORT:-8088}"

# Filled by pipeline_setup.
WORK=""
SOCK=""
BINARY=""
SERVER_LOG=""
DRAINER=""

# Tracked PIDs for the cleanup trap.
PID_SCOPECACHE=""
PID_BRIDGE=""
PID_WRK=""

# pipeline_setup REPO_ROOT
# Wipes and prepares harness/${PIPELINE_NAME}-test/ and exports
# WORK / SOCK / BINARY / SERVER_LOG / DRAINER. Call once, first.
pipeline_setup() {
    WORK="$1/harness/${PIPELINE_NAME}-test"
    SOCK="$WORK/sc.sock"
    BINARY="$WORK/scopecache"
    SERVER_LOG="$WORK/server.log"
    DRAINER="$WORK/drain.sh"
    rm -rf "$WORK"
    mkdir -p "$WORK"
}

# pipeline_install_deps
# Installs every system tool the pipeline needs (wrk for read-load,
# socat for the TCP→unix bridge). Idempotent — `apk add` is a no-op
# for packages already present, so calling on every run is fine.
pipeline_install_deps() {
    missing=""
    command -v wrk   >/dev/null 2>&1 || missing="$missing wrk"
    command -v socat >/dev/null 2>&1 || missing="$missing socat"
    if [ -n "$missing" ]; then
        apk add --no-cache$missing >/dev/null
    fi
}

# pipeline_build REPO_ROOT
# go build standalone scopecache into $BINARY.
pipeline_build() {
    go build -o "$BINARY" "$1/cmd/scopecache"
}

# pipeline_write_drainer OUTPUT_DIR
# Emits $DRAINER — a per-event drainer script that fetches up to 1000
# events per /head call and persists each as a separate <ts>-<seq>.json
# file. Same pattern as test_subscribe_pipeline.sh's inline drainer.
pipeline_write_drainer() {
    output_dir="$1"
    cat > "$DRAINER" <<DRAINER_EOF
#!/bin/sh
set -eu
sleep 0.2
SCOPE="\${SCOPECACHE_SCOPE:-_events}"
SOCK="\${SCOPECACHE_SOCKET_PATH:-/run/scopecache.sock}"
DIR="\${SCOPECACHE_OUTPUT_DIR:-${output_dir}}"
mkdir -p "\$DIR"
while :; do
    response=\$(curl -fsS --unix-socket "\$SOCK" "http://localhost/head?scope=\${SCOPE}&limit=1000")
    count=\$(printf '%s' "\$response" | jq -r '.count // 0')
    if [ "\$count" = "0" ]; then break; fi
    printf '%s' "\$response" | jq -c '.items[]' | while IFS= read -r line; do
        ts=\$(printf '%s' "\$line"  | jq -r '.ts')
        seq=\$(printf '%s' "\$line" | jq -r '.seq')
        printf '%s\n' "\$line" > "\${DIR}/\${ts}-\${seq}.json"
    done
    last_seq=\$(printf '%s' "\$response" | jq -r '.items[-1].seq')
    curl -fsS --unix-socket "\$SOCK" -X POST \\
        -H "Content-Type: application/json" \\
        -d "{\\"scope\\":\\"\${SCOPE}\\",\\"max_seq\\":\${last_seq}}" \\
        "http://localhost/delete_up_to" > /dev/null
done
DRAINER_EOF
    chmod +x "$DRAINER"
}

# pipeline_boot_scopecache
# Boots the standalone binary in the background and waits up to 3s for
# the unix socket to appear. Caller controls config via env-var prefix:
#
#   SCOPECACHE_EVENTS_MODE=full \
#   SCOPECACHE_SUBSCRIBER_COMMAND="$DRAINER" \
#   pipeline_boot_scopecache
pipeline_boot_scopecache() {
    SCOPECACHE_SOCKET_PATH="$SOCK" \
        "$BINARY" >"$SERVER_LOG" 2>&1 &
    PID_SCOPECACHE=$!
    for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do
        [ -S "$SOCK" ] && return 0
        sleep 0.2
    done
    echo "FAIL: socket $SOCK never appeared"
    return 1
}

# pipeline_seed_scope SCOPE COUNT
# Pre-populates SCOPE with COUNT items (id="r1".."rN", payload={"v":N})
# via a single /warm request. Verifies via /scopelist that COUNT items
# actually landed.
pipeline_seed_scope() {
    scope="$1"; count="$2"
    awk -v scope="$scope" -v n="$count" 'BEGIN {
        printf "{\"items\":["
        for (i=1; i<=n; i++) {
            if (i>1) printf ","
            printf "{\"scope\":\"%s\",\"id\":\"r%d\",\"payload\":{\"v\":%d}}", scope, i, i
        }
        printf "]}"
    }' | curl -fsS --unix-socket "$SOCK" -X POST \
        -H "Content-Type: application/json" --data-binary @- \
        "http://localhost/warm" > /dev/null

    actual=$(curl -fsS --unix-socket "$SOCK" \
        "http://localhost/scopelist?prefix=${scope}" \
        | jq -r --arg s "$scope" '.scopes[] | select(.scope == $s) | .item_count')
    if [ "$actual" != "$count" ]; then
        echo "FAIL: seed expected $count items in scope=$scope, /scopelist reports ${actual:-0}"
        return 1
    fi
}

# pipeline_start_bridge
# Starts socat as a TCP→unix forwarder on $BRIDGE_PORT and waits up to
# 2s for /help to respond through it (verifies end-to-end reachability,
# not just that the listener bound).
pipeline_start_bridge() {
    socat "TCP-LISTEN:${BRIDGE_PORT},fork,reuseaddr" "UNIX-CONNECT:${SOCK}" \
        >"$WORK/bridge.log" 2>&1 &
    PID_BRIDGE=$!
    for _ in 1 2 3 4 5 6 7 8 9 10; do
        if curl -fsS "http://127.0.0.1:${BRIDGE_PORT}/help" >/dev/null 2>&1; then
            return 0
        fi
        sleep 0.2
    done
    echo "FAIL: bridge port ${BRIDGE_PORT} never reachable end-to-end"
    return 1
}

# pipeline_write_wrk_get_seq_lua LUA_PATH SCOPE SEQ_MAX
# Emits a wrk Lua script that fires GET /get?scope=SCOPE&seq=N where
# N is uniformly random in 1..SEQ_MAX. Per-thread RNG seeding so two
# threads do not march in lock-step. Mirrors bench.sh's get-seq Lua
# but parametrises scope and range.
pipeline_write_wrk_get_seq_lua() {
    lua_path="$1"; scope="$2"; seq_max="$3"
    cat > "$lua_path" <<LUA
local thread_count = 0
function setup(thread)
    thread:set("tid", thread_count)
    thread_count = thread_count + 1
end
function init(args)
    math.randomseed(os.time() + (tid or 0) * 1000)
    bad_status = {}
end
function request()
    local seq = math.random(1, ${seq_max})
    return wrk.format("GET", "/get?scope=${scope}&seq=" .. seq)
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
}

# pipeline_start_wrk LUA_PATH DURATION THREADS CONNS OUT_FILE
# Backgrounds wrk against the local TCP bridge. Output (including
# stdout summary + Lua-stderr BADSTATUS lines) goes to OUT_FILE.
pipeline_start_wrk() {
    lua_path="$1"; duration="$2"; threads="$3"; conns="$4"; out="$5"
    wrk -t"$threads" -c"$conns" -d"$duration" -s "$lua_path" \
        "http://127.0.0.1:${BRIDGE_PORT}/" > "$out" 2>&1 &
    PID_WRK=$!
}

# pipeline_wait_wrk
# Blocks until the backgrounded wrk exits.
pipeline_wait_wrk() {
    if [ -n "$PID_WRK" ]; then
        wait "$PID_WRK" 2>/dev/null || true
        PID_WRK=""
    fi
}

# pipeline_wait_drain [SCOPE] [TIMEOUT_SEC]
# Polls /head?scope=SCOPE every 0.5s until count=0 or timeout. Prints
# the last 30 lines of $SERVER_LOG on timeout.
pipeline_wait_drain() {
    scope="${1:-_events}"; timeout="${2:-30}"
    deadline=$(( $(date +%s) + timeout ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        count=$(curl -fsS --unix-socket "$SOCK" \
            "http://localhost/head?scope=${scope}&limit=10" | jq -r '.count // 0')
        if [ "$count" = "0" ]; then return 0; fi
        sleep 0.5
    done
    echo "FAIL: scope=$scope never drained (count=$count after ${timeout}s)"
    [ -f "$SERVER_LOG" ] && tail -30 "$SERVER_LOG" | sed 's/^/  /'
    return 1
}

# pipeline_cleanup
# Best-effort kill of wrk → bridge → scopecache. Reverse-of-start order
# so the bridge can drain in-flight requests before the cache vanishes.
# Idempotent; safe to call from a trap.
pipeline_cleanup() {
    if [ -n "$PID_WRK" ]; then
        kill "$PID_WRK" 2>/dev/null || true
        wait "$PID_WRK" 2>/dev/null || true
        PID_WRK=""
    fi
    if [ -n "$PID_BRIDGE" ]; then
        kill "$PID_BRIDGE" 2>/dev/null || true
        wait "$PID_BRIDGE" 2>/dev/null || true
        PID_BRIDGE=""
    fi
    if [ -n "$PID_SCOPECACHE" ]; then
        kill "$PID_SCOPECACHE" 2>/dev/null || true
        wait "$PID_SCOPECACHE" 2>/dev/null || true
        PID_SCOPECACHE=""
    fi
}

# pipeline_install_trap
# Installs pipeline_cleanup on EXIT/INT/TERM.
pipeline_install_trap() {
    trap pipeline_cleanup EXIT INT TERM
}
