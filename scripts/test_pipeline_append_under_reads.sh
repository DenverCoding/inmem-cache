#!/bin/sh
# test_pipeline_append_under_reads.sh — append/drain pipeline under
# concurrent get-seq read load.
#
# Foreground (the thing being tested):
#   COUNT messages produced into WRITE_SCOPE at INTERVAL/sec; subscriber
#   bridge fires the per-event drainer that persists each event as a
#   separate <ts>-<seq>.json file. Same shape as test_subscribe_pipeline.sh.
#
# Background (the disturbance):
#   SEED_COUNT items pre-loaded into READS_SCOPE; wrk drives random
#   /get?scope=READS_SCOPE&seq=N reads against that scope through a
#   socat TCP bridge for WRK_DURATION.
#
# The two scopes are separate by design: wrk hits a stable, immutable
# dataset so its rps/latency numbers are not distorted by the
# producer's growing scope, and the producer's correctness can be
# verified independently of how many reads landed.
#
# Pass/fail signal: the six pipeline assertions (file count, file
# values, cache count, cache values, seq cross-check, ts cross-check).
# wrk numbers are observability data — interesting but not gating.
#
# Parallel-runnable: claims BRIDGE_PORT 8088 and PIPELINE_NAME
# "append-under-reads" — see pipeline_lib.sh for the full port
# allocation list.
#
# Defaults match the user's spec: 1000 messages × 0.1s ≈ 100s producer
# runtime, wrk -t4 -c40 -d100s. Override via env vars:
#
#   COUNT, INTERVAL                producer shape
#   SEED_COUNT                     read-load dataset size (default 50000)
#   WRK_THREADS, WRK_CONNS,        background wrk shape
#   WRK_DURATION                   (default 100s — matches producer)
#
# Run from the dev container:
#
#   docker compose exec dev sh /src/scripts/test_pipeline_append_under_reads.sh

set -eu

REPO_ROOT=$(cd "$(dirname "$0")/.." && pwd)

PIPELINE_NAME="append-under-reads"
BRIDGE_PORT=8088
. "$REPO_ROOT/scripts/pipeline_lib.sh"

COUNT="${COUNT:-1000}"
INTERVAL="${INTERVAL:-0.1}"
SEED_COUNT="${SEED_COUNT:-50000}"
WRK_THREADS="${WRK_THREADS:-4}"
WRK_CONNS="${WRK_CONNS:-40}"
WRK_DURATION="${WRK_DURATION:-100s}"

pipeline_setup "$REPO_ROOT"
pipeline_install_trap

OUTPUT_DIR="$WORK/messages"
WRK_OUTPUT="$WORK/wrk.txt"
WRK_LUA="$WORK/get-seq.lua"
mkdir -p "$OUTPUT_DIR"

echo "== install deps if needed (wrk, socat) =="
pipeline_install_deps

echo "== generate drainer =="
pipeline_write_drainer "$OUTPUT_DIR"

echo "== build standalone =="
pipeline_build "$REPO_ROOT"

echo "== boot scopecache (events_mode=full + bridge -> drainer) =="
SCOPECACHE_EVENTS_MODE=full \
SCOPECACHE_SUBSCRIBER_COMMAND="$DRAINER" \
SCOPECACHE_OUTPUT_DIR="$OUTPUT_DIR" \
pipeline_boot_scopecache
echo "ok   socket up at $SOCK"

echo "== seed read-load scope ($SEED_COUNT items into $READS_SCOPE) =="
pipeline_seed_scope "$READS_SCOPE" "$SEED_COUNT"
echo "ok   $SEED_COUNT items seeded into $READS_SCOPE"

# /warm emits one {op:"warm", ts} envelope into _events; the drainer
# wakes, persists it, and clears _events. Wait for that to happen,
# then wipe the messages dir so the producer's events are the only
# files counted in assertions. Bridge isn't up yet so wrk-side load
# can't interfere here.
echo "== drain seed-event + reset output dir =="
pipeline_wait_drain "_events" 10
rm -f "$OUTPUT_DIR"/*.json
echo "ok   _events drained, $OUTPUT_DIR cleared"

echo "== start tcp bridge on :$BRIDGE_PORT =="
pipeline_start_bridge
echo "ok   bridge up at 127.0.0.1:$BRIDGE_PORT"

echo "== generate wrk lua + start background load =="
pipeline_write_wrk_get_seq_lua "$WRK_LUA" "$READS_SCOPE" "$SEED_COUNT"
pipeline_start_wrk "$WRK_LUA" "$WRK_DURATION" "$WRK_THREADS" "$WRK_CONNS" "$WRK_OUTPUT"
echo "ok   wrk started ($WRK_THREADS threads × $WRK_CONNS conns × $WRK_DURATION on $READS_SCOPE)"

echo ""
echo "== produce $COUNT messages to $WRITE_SCOPE at ~$(awk "BEGIN { printf \"%.1f\", 1/$INTERVAL }")/sec =="
start=$(date +%s)
i=0
while [ "$i" -lt "$COUNT" ]; do
    i=$((i + 1))
    curl -fsS --unix-socket "$SOCK" -X POST \
        -H "Content-Type: application/json" \
        -d "{\"scope\":\"${WRITE_SCOPE}\",\"id\":\"msg-${i}\",\"payload\":{\"n\":${i}}}" \
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
echo "== wait for wrk to finish =="
pipeline_wait_wrk
echo "ok   wrk finished"

echo ""
echo "== wait for final drain =="
pipeline_wait_drain "_events" 30
echo "ok   _events drained from cache"

echo ""
echo "== assertions =="
file_count=$(find "$OUTPUT_DIR" -name '*.json' -type f | wc -l | tr -d ' ')
if [ "$file_count" -ne "$COUNT" ]; then
    echo "FAIL: expected $COUNT files in $OUTPUT_DIR, got $file_count"
    exit 1
fi
echo "ok   $file_count files persisted (one per message)"

unique_counters=$(cat "$OUTPUT_DIR"/*.json | jq -r '.payload.payload.n' | sort -n | uniq | wc -l | tr -d ' ')
if [ "$unique_counters" -ne "$COUNT" ]; then
    echo "FAIL: expected $COUNT unique counter values in files, got $unique_counters"
    exit 1
fi
echo "ok   counter values 1..$COUNT each appear exactly once in files"

cache_response=$(curl -fsS --unix-socket "$SOCK" \
    "http://localhost/head?scope=${WRITE_SCOPE}&limit=${COUNT}")
cache_count=$(printf '%s' "$cache_response" | jq -r '.count // 0')
if [ "$cache_count" -ne "$COUNT" ]; then
    echo "FAIL: expected $COUNT items in cache scope=$WRITE_SCOPE, got $cache_count"
    exit 1
fi
echo "ok   $cache_count items in cache scope=$WRITE_SCOPE"

cache_unique=$(printf '%s' "$cache_response" | jq -r '.items[].payload.n' | sort -n | uniq | wc -l | tr -d ' ')
if [ "$cache_unique" -ne "$COUNT" ]; then
    echo "FAIL: expected $COUNT unique counter values in cache, got $cache_unique"
    exit 1
fi
echo "ok   counter values 1..$COUNT each appear exactly once in cache"

cache_seqs=$(printf '%s' "$cache_response" | jq -r '.items[].seq' | sort -n | tr '\n' ',')
file_seqs=$(cat "$OUTPUT_DIR"/*.json | jq -r '.payload.seq' | sort -n | tr '\n' ',')
if [ "$cache_seqs" != "$file_seqs" ]; then
    echo "FAIL: cache seqs do not match file seqs"
    exit 1
fi
echo "ok   cache seqs match file seqs (same events on both sides)"

cache_seq_ts=$(printf '%s' "$cache_response" | jq -r '.items[] | "\(.seq):\(.ts)"' | sort -n | tr '\n' ',')
file_seq_ts=$(cat "$OUTPUT_DIR"/*.json | jq -r '.payload | "\(.seq):\(.ts)"' | sort -n | tr '\n' ',')
if [ "$cache_seq_ts" != "$file_seq_ts" ]; then
    echo "FAIL: cache (seq,ts) pairs do not match file (seq,payload.ts) pairs"
    exit 1
fi
echo "ok   cache (seq,ts) pairs match file (seq,payload.ts) pairs"

echo ""
echo "== summary =="
echo "PASS — pipeline survived $COUNT appends + concurrent get-seq reads."
echo ""
echo "Foreground: $COUNT files in $OUTPUT_DIR (${total}s producer)"
echo "Background: $WRK_THREADS-thread × $WRK_CONNS-conn × $WRK_DURATION wrk reads on $READS_SCOPE"
echo ""
echo "wrk results:"
grep -E "Requests/sec|Latency *[0-9]|Socket errors|Non-2xx|BADSTATUS" "$WRK_OUTPUT" | sed 's/^/  /' || true
echo ""
echo "Files left in place for inspection. Re-run wipes them."
