#!/bin/sh
# test_subscribe_pipeline.sh — live demo of the subscriber bridge under
# realistic per-message streaming load.
#
# Setup: standalone scopecache with events_mode=full + an inline
# drainer script wired as SCOPECACHE_SUBSCRIBER_COMMAND. The drainer
# fetches every event from `_events` and writes each as its own JSON
# file named `<ts>-<seq>.json`, so the message order is reconstructible
# by sorting filenames lexically.
#
# Producer side: a curl loop POSTs N messages with monotonic counter
# payloads (1..N) to a user scope at ~10 req/sec. With events_mode=full
# every successful append auto-populates `_events`, the bridge fires,
# the drainer wakes up, fetches the new events, persists them, and
# `delete_up_to`s them out of the cache.
#
# Defaults match the user's spec: 1000 messages × 0.1s interval ≈ 100s
# total runtime. Override via env vars:
#
#   COUNT       number of messages to produce (default 1000)
#   INTERVAL    sleep between appends in seconds (default 0.1)
#   SCOPE       user scope to write into (default "counter")
#
# Outputs land in /src/harness/pipeline-test/messages/. The harness
# directory is gitignored, so re-runs leave inspectable artefacts on
# the host without polluting the repo.
#
# Run from the dev container so go + curl + jq are available:
#
#   docker compose exec dev sh /src/scripts/test_subscribe_pipeline.sh
#
# On exit the standalone binary is stopped and the work directory is
# left in place for inspection. The next run wipes it before starting.

set -eu

REPO_ROOT=$(cd "$(dirname "$0")/.." && pwd)

COUNT="${COUNT:-1000}"
INTERVAL="${INTERVAL:-0.1}"
SCOPE="${SCOPE:-counter}"

WORK="$REPO_ROOT/harness/pipeline-test"
SOCK="$WORK/sc.sock"
OUTPUT_DIR="$WORK/messages"
DRAINER="$WORK/drain.sh"
BINARY="$WORK/scopecache"
SERVER_LOG="$WORK/server.log"

# Wipe and prep — keeps the structure deterministic across runs.
rm -rf "$WORK"
mkdir -p "$WORK" "$OUTPUT_DIR"

PID=""
cleanup() {
    if [ -n "$PID" ]; then
        kill "$PID" 2>/dev/null || true
        wait "$PID" 2>/dev/null || true
    fi
}
trap cleanup EXIT INT TERM

echo "== generating drainer =="
# Per-message drainer: one file per event, named <ts>-<seq>.json so
# they sort lexically by event time. The 0.2s coalesce-sleep mirrors
# drain_events.sh — burst writes collapse into bigger /head fetches.
cat > "$DRAINER" <<'DRAINER_EOF'
#!/bin/sh
set -eu
sleep 0.2

SCOPE="${SCOPECACHE_SCOPE:-_events}"
SOCK="${SCOPECACHE_SOCKET_PATH:-/run/scopecache.sock}"
DIR="${SCOPECACHE_OUTPUT_DIR:-/var/log/scopecache}"

mkdir -p "$DIR"

while :; do
    response=$(curl -fsS --unix-socket "$SOCK" \
        "http://localhost/head?scope=${SCOPE}&limit=1000")
    count=$(printf '%s' "$response" | jq -r '.count // 0')
    if [ "$count" = "0" ]; then
        break
    fi

    # One file per event. The ts is the cache-stamped microsecond
    # timestamp of the original /append call (auto-populated into
    # the _events envelope via emitAppendEvent), so filenames sort
    # by produce-order even when multiple writes share a wake-up.
    printf '%s' "$response" | jq -c '.items[]' | while IFS= read -r line; do
        ts=$(printf '%s' "$line"  | jq -r '.ts')
        seq=$(printf '%s' "$line" | jq -r '.seq')
        printf '%s\n' "$line" > "${DIR}/${ts}-${seq}.json"
    done

    last_seq=$(printf '%s' "$response" | jq -r '.items[-1].seq')
    curl -fsS --unix-socket "$SOCK" -X POST \
        -H "Content-Type: application/json" \
        -d "{\"scope\":\"${SCOPE}\",\"max_seq\":${last_seq}}" \
        "http://localhost/delete_up_to" > /dev/null

    persisted=$(printf '%s' "$response" | jq -r '.count')
    printf '[drainer] persisted %s events through seq=%s\n' "$persisted" "$last_seq"
done
DRAINER_EOF
chmod +x "$DRAINER"

echo "== build standalone =="
go build -o "$BINARY" "$REPO_ROOT/cmd/scopecache"

echo "== boot scopecache (events_mode=full + bridge -> drainer) =="
SCOPECACHE_SOCKET_PATH="$SOCK" \
SCOPECACHE_EVENTS_MODE=full \
SCOPECACHE_SUBSCRIBER_COMMAND="$DRAINER" \
SCOPECACHE_OUTPUT_DIR="$OUTPUT_DIR" \
"$BINARY" >"$SERVER_LOG" 2>&1 &
PID=$!

# Wait for the socket to appear — bounded probe so a botched build
# doesn't hang the script.
for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do
    [ -S "$SOCK" ] && break
    sleep 0.2
done
[ -S "$SOCK" ] || { echo "FAIL: socket $SOCK never appeared"; exit 1; }
echo "ok   socket up at $SOCK"

echo ""
echo "== produce $COUNT messages to scope=$SCOPE at ~$(awk "BEGIN { printf \"%.1f\", 1/$INTERVAL }")/sec =="
start=$(date +%s)
i=0
while [ "$i" -lt "$COUNT" ]; do
    i=$((i + 1))
    curl -fsS --unix-socket "$SOCK" -X POST \
        -H "Content-Type: application/json" \
        -d "{\"scope\":\"${SCOPE}\",\"id\":\"msg-${i}\",\"payload\":{\"n\":${i}}}" \
        "http://localhost/append" > /dev/null
    if [ $((i % 100)) -eq 0 ]; then
        elapsed=$(( $(date +%s) - start ))
        printf '  produced %4d / %d  (%ds elapsed)\n' "$i" "$COUNT" "$elapsed"
    fi
    sleep "$INTERVAL"
done
total=$(( $(date +%s) - start ))
echo "ok   $COUNT writes accepted in ${total}s (~$(awk "BEGIN { printf \"%.1f\", $COUNT/$total }")/sec)"

echo ""
echo "== wait for final drain =="
# Poll /head?scope=_events until count=0. Cap at 30s so a stuck
# drainer can't hang the script forever; in practice the last batch
# lands within the 0.2s coalesce-sleep + a few curl roundtrips.
deadline=$(( $(date +%s) + 30 ))
drained=0
while [ "$(date +%s)" -lt "$deadline" ]; do
    count=$(curl -fsS --unix-socket "$SOCK" \
        "http://localhost/head?scope=_events&limit=10" | jq -r '.count // 0')
    if [ "$count" = "0" ]; then
        drained=1
        break
    fi
    sleep 0.5
done
if [ "$drained" -ne 1 ]; then
    echo "FAIL: _events never drained (count=$count after timeout)"
    echo "---- server log tail ----"
    tail -30 "$SERVER_LOG" || true
    exit 1
fi
echo "ok   _events drained from cache"

echo ""
echo "== assertions =="
file_count=$(find "$OUTPUT_DIR" -name '*.json' -type f | wc -l | tr -d ' ')
if [ "$file_count" -ne "$COUNT" ]; then
    echo "FAIL: expected $COUNT files in $OUTPUT_DIR, got $file_count"
    exit 1
fi
echo "ok   $file_count files persisted (one per message)"

# Counter values 1..COUNT must each appear exactly once across all
# files. Reads .payload.payload.n: the outer payload is the _events
# envelope, the nested .payload is the original /append payload.
unique_counters=$(cat "$OUTPUT_DIR"/*.json | jq -r '.payload.payload.n' | sort -n | uniq | wc -l | tr -d ' ')
if [ "$unique_counters" -ne "$COUNT" ]; then
    echo "FAIL: expected $COUNT unique counter values, got $unique_counters"
    exit 1
fi
echo "ok   counter values 1..$COUNT each appear exactly once in files"

# Verify the live scopecache scope still holds all $COUNT items —
# /append writes don't auto-drain, so the producer's scope is the
# source of truth. /head with limit=$COUNT returns everything in
# one shot (MaxLimit accommodates this).
cache_response=$(curl -fsS --unix-socket "$SOCK" \
    "http://localhost/head?scope=${SCOPE}&limit=${COUNT}")
cache_count=$(printf '%s' "$cache_response" | jq -r '.count // 0')
if [ "$cache_count" -ne "$COUNT" ]; then
    echo "FAIL: expected $COUNT items in cache scope=$SCOPE, got $cache_count"
    exit 1
fi
echo "ok   $cache_count items in cache scope=$SCOPE"

cache_unique=$(printf '%s' "$cache_response" | jq -r '.items[].payload.n' | sort -n | uniq | wc -l | tr -d ' ')
if [ "$cache_unique" -ne "$COUNT" ]; then
    echo "FAIL: expected $COUNT unique counter values in cache, got $cache_unique"
    exit 1
fi
echo "ok   counter values 1..$COUNT each appear exactly once in cache"

# Cross-check: the seq stamps in cache must match those in the files.
cache_seqs=$(printf '%s' "$cache_response" | jq -r '.items[].seq' | sort -n | tr '\n' ',')
file_seqs=$(cat "$OUTPUT_DIR"/*.json | jq -r '.payload.seq' | sort -n | tr '\n' ',')
if [ "$cache_seqs" != "$file_seqs" ]; then
    echo "FAIL: cache seqs do not match file seqs"
    exit 1
fi
echo "ok   cache seqs match file seqs (same events on both sides)"

# ts cross-check: each (seq, ts) pair in the cache scope must match
# the (seq, payload.ts) pair recorded in the events envelope. The
# envelope's payload mirrors the original /append item, so payload.ts
# is the counter-scope ts at write-time. The outer .ts in the file
# is the _events scope's own ts (envelope-write time, stamped a few
# µs later) — different by design, not compared here.
cache_seq_ts=$(printf '%s' "$cache_response" | jq -r '.items[] | "\(.seq):\(.ts)"' | sort -n | tr '\n' ',')
file_seq_ts=$(cat "$OUTPUT_DIR"/*.json | jq -r '.payload | "\(.seq):\(.ts)"' | sort -n | tr '\n' ',')
if [ "$cache_seq_ts" != "$file_seq_ts" ]; then
    echo "FAIL: cache (seq,ts) pairs do not match file (seq,payload.ts) pairs"
    exit 1
fi
echo "ok   cache (seq,ts) pairs match file (seq,payload.ts) pairs"

echo ""
echo "== summary =="
echo "PASS — $COUNT messages produced, drained per-message, persisted to:"
echo "  $OUTPUT_DIR"
echo ""
echo "Files left in place for inspection. Re-run wipes them."
echo "Sample filenames (first 3 + last 3):"
ls -1 "$OUTPUT_DIR" | sort | head -3 | sed 's/^/  /'
echo "  ..."
ls -1 "$OUTPUT_DIR" | sort | tail -3 | sed 's/^/  /'
