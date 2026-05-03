#!/bin/sh
# Orchestrator for the end-to-end test suite. Builds both Docker
# images (scopecache + caddyscope), brings them up alongside the dev
# container, waits for each adapter to answer HTTP, and then runs
# e2e_test.sh against both — first via the standalone Unix socket,
# then via the Caddy module on TCP.
#
# Why this exists: `go test ./...` covers Go-level correctness
# (logic, races, handler shapes via httptest) but does not exercise
# the actual Docker images, the Caddyfile parser, env-var
# resolution, or the wire-level JSON output. The e2e suite catches
# regressions in those layers; this script is the single command
# that wires the whole pipeline together.
#
# Usage:
#   ./run_e2e.sh           rebuild images, run e2e against both
#                          adapters, leave services up afterwards.
#   ./run_e2e.sh --quick   skip the rebuild and run against the
#                          currently-running images. Saves ~25s on
#                          repeat runs when only the test script
#                          itself changed.
#   ./run_e2e.sh --down    'docker compose down' on exit (clean
#                          teardown). Default leaves services up so
#                          back-to-back runs are fast.
#
# Exit codes:
#   0    both adapters: 0 failures.
#   1    one or both adapters reported failures, or a setup step
#        failed. Container logs are dumped on failure.
#   2    bad usage (unknown flag).
#
# This script is POSIX sh — runs identically on Linux, macOS, and
# Git-bash on Windows. The actual test logic lives in e2e_test.sh.

set -eu

# Git-bash on Windows rewrites absolute paths like /src/... into
# C:/Program Files/Git/src/... before passing them to docker. The
# variable below is a documented opt-out; harmless no-op on Linux
# and macOS where MSYS isn't in the picture.
export MSYS_NO_PATHCONV=1

REPO_ROOT=$(cd "$(dirname "$0")/.." && pwd)
QUICK=0
DOWN=0

while [ $# -gt 0 ]; do
    case "$1" in
        --quick) QUICK=1; shift ;;
        --down)  DOWN=1; shift ;;
        -h|--help)
            sed -n '/^# Usage:/,/^$/p' "$0" | sed 's/^# //;s/^#$//'
            exit 0
            ;;
        *)
            printf 'unknown flag: %s\n' "$1" >&2
            printf 'try %s --help\n' "$0" >&2
            exit 2
            ;;
    esac
done

cleanup() {
    rc=$?
    if [ "$DOWN" = "1" ]; then
        printf '\n[cleanup] docker compose down\n'
        docker compose down >/dev/null 2>&1 || true
    fi
    exit "$rc"
}
trap cleanup EXIT INT TERM

cd "$REPO_ROOT"

elapsed() {
    printf '%ss' "$(( $(date +%s) - $1 ))"
}

# --- 1. build -----------------------------------------------------
if [ "$QUICK" = "0" ]; then
    printf '[1/4] Building scopecache + caddyscope images... '
    t0=$(date +%s)
    if ! docker compose build scopecache caddyscope >/tmp/run_e2e_build.log 2>&1; then
        printf 'FAIL\n'
        cat /tmp/run_e2e_build.log
        exit 1
    fi
    printf 'done (%s)\n' "$(elapsed "$t0")"
else
    printf '[1/4] Skipping image build (--quick).\n'
fi

# --- 2. start -----------------------------------------------------
printf '[2/4] Starting services... '
t0=$(date +%s)
if ! docker compose up -d scopecache caddyscope dev >/tmp/run_e2e_up.log 2>&1; then
    printf 'FAIL\n'
    cat /tmp/run_e2e_up.log
    exit 1
fi
printf 'up (%s)\n' "$(elapsed "$t0")"

# --- 3. wait for both adapters to answer -------------------------
printf '[3/4] Waiting for adapters to answer HTTP... '
t0=$(date +%s)
ready_unix=0
ready_tcp=0
i=0
while [ "$i" -lt 30 ]; do
    if [ "$ready_unix" = "0" ] && \
       docker compose exec -T dev sh -c 'curl -fs --unix-socket /run/scopecache.sock http://localhost/help >/dev/null' 2>/dev/null; then
        ready_unix=1
    fi
    if [ "$ready_tcp" = "0" ] && \
       docker compose exec -T dev sh -c 'curl -fs http://caddyscope:8080/help >/dev/null' 2>/dev/null; then
        ready_tcp=1
    fi
    if [ "$ready_unix" = "1" ] && [ "$ready_tcp" = "1" ]; then
        break
    fi
    sleep 1
    i=$((i + 1))
done
if [ "$ready_unix" = "0" ] || [ "$ready_tcp" = "0" ]; then
    printf 'TIMEOUT (unix=%s tcp=%s)\n' "$ready_unix" "$ready_tcp"
    printf '\n--- scopecache logs ---\n'
    docker compose logs --tail=30 scopecache || true
    printf '\n--- caddyscope logs ---\n'
    docker compose logs --tail=30 caddyscope || true
    exit 1
fi
printf 'ready (%s)\n' "$(elapsed "$t0")"

# --- 4. e2e against both adapters ---------------------------------
printf '\n[4/4] e2e against unix-socket scopecache:\n'
t0=$(date +%s)
set +e
docker compose exec -T dev sh /src/scripts/e2e_test.sh
unix_rc=$?
set -e
unix_dur=$(elapsed "$t0")

printf '\n[4/4] e2e against caddyscope (TCP):\n'
t0=$(date +%s)
set +e
docker compose exec -T -e SOCK= -e BASE=http://caddyscope:8080 dev sh /src/scripts/e2e_test.sh
tcp_rc=$?
set -e
tcp_dur=$(elapsed "$t0")

# --- summary ------------------------------------------------------
printf '\n=== summary ===\n'
printf 'unix-socket: rc=%d (%s)\n' "$unix_rc" "$unix_dur"
printf 'TCP        : rc=%d (%s)\n' "$tcp_rc" "$tcp_dur"

if [ "$unix_rc" = "0" ] && [ "$tcp_rc" = "0" ]; then
    printf '\nALL PASS — both adapters symmetric on this commit.\n'
    exit 0
fi

printf '\nFAILURE — at least one adapter reported failures.\n'
printf '\n--- scopecache logs (tail) ---\n'
docker compose logs --tail=20 scopecache || true
printf '\n--- caddyscope logs (tail) ---\n'
docker compose logs --tail=20 caddyscope || true
exit 1
