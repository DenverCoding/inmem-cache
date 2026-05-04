#!/bin/sh
# drain_events.sh — reference subscriber-command for scopecache.
#
# This is the operator-side script invoked by scopecache's in-core
# subscriber bridge (Gateway.StartSubscriber → exec.Command). On every
# wake-up the bridge runs this script and waits for it to exit. The
# bridge passes one environment variable:
#
#   SCOPECACHE_SCOPE   the reserved scope that triggered the wake-up.
#                      One of "_events" or "_inbox". Set per-invocation
#                      so a single script can serve both scopes.
#
# Two more env vars let the operator override deployment-specific
# paths without editing the script (defaults match the standalone
# binary's defaults):
#
#   SCOPECACHE_SOCKET_PATH   Unix socket the cache listens on
#                            (default: /run/scopecache.sock).
#   SCOPECACHE_OUTPUT_DIR    where JSONL batch files land
#                            (default: /var/log/scopecache).
#
# What this script does, per invocation:
#
#   1. Sleep 0.5 s to coalesce burst writes that arrived during the
#      wake-up. The cache's single-slot wake-up channel coalesces
#      multiple writes into one wake-up; the sleep here just makes
#      each batch larger so we issue fewer HTTP roundtrips.
#   2. GET /head?scope=$SCOPECACHE_SCOPE&limit=1000 — fetch a batch.
#   3. Append each item as one JSON line to <DIR>/<scope>-<ts>-<i>.jsonl
#      (one file per batch, sortable by timestamp).
#   4. POST /delete_up_to with the seq of the last item written.
#   5. Repeat 2-4 until the scope returns an empty batch, then exit 0.
#
# Dependencies: curl (for HTTP), jq (for parsing). Both are typically
# already installed on production hosts; if not, install via your
# package manager.
#
# This is a *reference*, not production-grade. Operators almost
# certainly want to:
#   - replace the JSONL sink with their actual destination (DB,
#     webhook, message queue, content-addressed store, …)
#   - add retry logic or alerting if the destination is unreliable
#   - tune the batch limit and sleep to match their write rate
#   - configure log rotation if writing to disk
#
# Wire it up by setting one env var (standalone) or one Caddyfile key:
#
#   SCOPECACHE_SUBSCRIBER_COMMAND=/path/to/drain_events.sh ./scopecache
#
#   # or in a Caddyfile:
#   scopecache {
#       events_mode        full
#       subscriber_command /path/to/drain_events.sh
#   }
#
# Make sure the file is executable: `chmod +x drain_events.sh`.

set -eu

# Coalesce-delay. The cache fires wake-ups on every write; this sleep
# lets a burst of writes collapse into a single batch fetch instead
# of one curl roundtrip per write.
sleep 0.5

SCOPE="${SCOPECACHE_SCOPE:-_events}"
SOCK="${SCOPECACHE_SOCKET_PATH:-/run/scopecache.sock}"
DIR="${SCOPECACHE_OUTPUT_DIR:-/var/log/scopecache}"

mkdir -p "$DIR"

# Drain the scope in a loop. Each iteration: one batch fetched, one
# file written, one delete_up_to. Stop on the first empty batch.
i=0
while :; do
    response=$(curl -fsS --unix-socket "$SOCK" \
        "http://localhost/head?scope=${SCOPE}&limit=1000")

    count=$(printf '%s' "$response" | jq -r '.count // 0')
    if [ "$count" = "0" ]; then
        break
    fi

    # One file per batch, named so files sort lexically by time.
    # Plain `date +%s` (second resolution) plus an iteration counter
    # is enough to keep names unique within one drain loop.
    output_file="${DIR}/${SCOPE}-$(date +%s)-${i}.jsonl"
    printf '%s' "$response" | jq -c '.items[]' >> "$output_file"

    # Pull the seq of the last item we just persisted; everything up
    # to and including that seq is safe to wipe.
    last_seq=$(printf '%s' "$response" | jq -r '.items[-1].seq')

    curl -fsS --unix-socket "$SOCK" -X POST \
        -H "Content-Type: application/json" \
        -d "{\"scope\":\"${SCOPE}\",\"max_seq\":${last_seq}}" \
        "http://localhost/delete_up_to" > /dev/null

    i=$((i + 1))
done
