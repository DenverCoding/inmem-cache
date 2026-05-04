#!/bin/sh
# e2e_subscriber.sh — end-to-end test for the in-core subscriber bridge
# (Gateway.StartSubscriber) plus the reference drain_events.sh.
#
# Wires up the smallest realistic loop:
#
#   1. Build the standalone scopecache binary.
#   2. Boot it with events_mode=full and SUBSCRIBER_COMMAND pointing at
#      scripts/drain_events.sh.
#   3. POST 10 /append calls to a user scope (auto-populates _events).
#   4. Wait until the cache reports _events is empty (drain script
#      ran, fetched events, wrote JSONL, deleted from cache).
#   5. Assert: JSONL file(s) exist, contain 10 lines total, and each
#      line names scope=trigger inside its payload.
#
# Runs entirely inside the dev container (go + curl + jq required).
#
# Usage:
#   docker compose exec dev sh /src/scripts/e2e_subscriber.sh
#
# Exits 0 on success, non-zero with a "FAIL: ..." line on the first
# failed assertion. The trap cleans up the temp dir and kills the
# background binary regardless.

set -eu

REPO_ROOT=$(cd "$(dirname "$0")/.." && pwd)
TMP=$(mktemp -d)
SOCK="$TMP/sc.sock"
OUTPUT_DIR="$TMP/output"
mkdir -p "$OUTPUT_DIR"

PID=""
cleanup() {
    if [ -n "$PID" ]; then
        kill "$PID" 2>/dev/null || true
        wait "$PID" 2>/dev/null || true
    fi
    rm -rf "$TMP"
}
trap cleanup EXIT INT TERM

echo "== build =="
go build -o "$TMP/scopecache" ./cmd/scopecache

echo "== start standalone =="
SCOPECACHE_SOCKET_PATH="$SOCK" \
SCOPECACHE_EVENTS_MODE=full \
SCOPECACHE_SUBSCRIBER_COMMAND="$REPO_ROOT/scripts/drain_events.sh" \
SCOPECACHE_OUTPUT_DIR="$OUTPUT_DIR" \
"$TMP/scopecache" &
PID=$!

# Wait for socket up — up to 2s. The binary creates /run/scopecache.sock
# (or wherever SCOPECACHE_SOCKET_PATH points) once it's accepting.
for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
    [ -S "$SOCK" ] && break
    sleep 0.1
done
[ -S "$SOCK" ] || { echo "FAIL: socket $SOCK never appeared"; exit 1; }
echo "ok   socket up"

echo "== 10 writes to scope=trigger =="
for i in 1 2 3 4 5 6 7 8 9 10; do
    curl -fsS --unix-socket "$SOCK" -X POST \
        -H "Content-Type: application/json" \
        -d "{\"scope\":\"trigger\",\"id\":\"item-$i\",\"payload\":{\"n\":$i}}" \
        "http://localhost/append" > /dev/null
done
echo "ok   10 appends accepted"

echo "== wait for drain =="
# Poll /head?scope=_events until count=0 (drain finished). Cap at 8s
# (drain_events.sh sleeps 0.5s + curl roundtrips; the bridge may run
# the script multiple times for batches that didn't fully coalesce).
deadline=$(( $(date +%s) + 8 ))
drained=0
while [ "$(date +%s)" -lt "$deadline" ]; do
    count=$(curl -fsS --unix-socket "$SOCK" \
        "http://localhost/head?scope=_events&limit=10" | jq -r '.count // 0')
    if [ "$count" = "0" ]; then
        drained=1
        break
    fi
    sleep 0.2
done
if [ "$drained" -ne 1 ]; then
    echo "FAIL: _events never drained (count=$count after timeout)"
    exit 1
fi
echo "ok   _events drained from cache"

echo "== assertions =="

# A: JSONL files exist
file_count=$(find "$OUTPUT_DIR" -name '_events-*.jsonl' | wc -l | tr -d ' ')
if [ "$file_count" -lt 1 ]; then
    echo "FAIL: no JSONL files in $OUTPUT_DIR"
    ls -la "$OUTPUT_DIR" || true
    exit 1
fi
echo "ok   $file_count JSONL file(s) created"

# B: total events equals 10
total_lines=$(cat "$OUTPUT_DIR"/_events-*.jsonl | wc -l | tr -d ' ')
if [ "$total_lines" -ne 10 ]; then
    echo "FAIL: expected exactly 10 events, got $total_lines"
    cat "$OUTPUT_DIR"/_events-*.jsonl
    exit 1
fi
echo "ok   $total_lines events persisted"

# C: each event's inner payload references scope=trigger. The _events
# auto-populate wraps the user-write — scope at top level is "_events"
# (the event-stream scope), and .payload.scope is the user scope.
trigger_count=$(cat "$OUTPUT_DIR"/_events-*.jsonl | jq -r '.payload.scope' | grep -c '^trigger$' || true)
if [ "$trigger_count" -ne 10 ]; then
    echo "FAIL: expected 10 events for scope=trigger, got $trigger_count"
    cat "$OUTPUT_DIR"/_events-*.jsonl
    exit 1
fi
echo "ok   all 10 events reference scope=trigger"

# D: each event has the original id (item-1 .. item-10) — round-trip
# check that the auto-populate captured the caller's input.
id_count=$(cat "$OUTPUT_DIR"/_events-*.jsonl | jq -r '.payload.id' | grep -cE '^item-([1-9]|10)$' || true)
if [ "$id_count" -ne 10 ]; then
    echo "FAIL: expected 10 ids matching item-1..item-10, got $id_count"
    exit 1
fi
echo "ok   ids item-1..item-10 round-tripped"

echo ""
echo "== summary =="
echo "PASS — 10 writes -> $file_count file(s), $total_lines events, _events empty"
