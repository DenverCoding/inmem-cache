#!/bin/sh
# One-off /stats latency comparison helper, intended to be run inside
# the dev container against the caddyscope service. Lives under
# scripts/ for the same reason bench.sh does — kept around for
# reproducibility but not part of the standard test surface.
#
# Args: <label>  — string used in the output banner so two runs
#                  (old vs new) are distinguishable in a single log.
#
# Behaviour:
#   1. /wipe to baseline.
#   2. Populate N scopes (one item each). N defaults to 10000;
#      override via SCOPES env.
#   3. Time M GET /stats calls server-side, reading duration_us
#      from each response (no network jitter).
#   4. Report min / median / p95 / p99 / max / mean in microseconds.

set -eu

LABEL=${1:-stats-bench}
SCOPES=${SCOPES:-10000}
SAMPLES=${SAMPLES:-100}
HOST=${HOST:-http://caddyscope:8080}

echo "================================================================"
echo "  /stats latency bench — $LABEL"
echo "  populate: $SCOPES scopes (1 item each)"
echo "  samples:  $SAMPLES /stats calls"
echo "  host:     $HOST"
echo "================================================================"

until curl -sf "$HOST/help" >/dev/null 2>&1; do sleep 0.2; done

curl -s -X POST "$HOST/wipe" >/dev/null

echo "[populate] starting..."
t0=$(date +%s)
seq 1 "$SCOPES" | xargs -I{} -P 32 curl -s -o /dev/null \
    -X POST -H "Content-Type: application/json" \
    -d "{\"scope\":\"scope_{}\",\"payload\":\"v\"}" \
    "$HOST/append"
t1=$(date +%s)
echo "[populate] done in $((t1 - t0))s"

# Verify the populate landed
verify=$(curl -s "$HOST/stats" | jq -c "{scope_count, total_items, approx_store_mb}")
echo "[verify] $verify"

echo "[bench] timing $SAMPLES GET /stats calls (server-side duration_us)..."
DURATIONS=$(mktemp)
for i in $(seq 1 "$SAMPLES"); do
    curl -s "$HOST/stats" | jq -r ".duration_us"
done | sort -n > "$DURATIONS"

awk '
    {a[NR]=$1; sum+=$1}
    END {
        n=NR
        printf "  count: %d\n", n
        printf "  min:    %8d us\n", a[1]
        printf "  median: %8d us\n", (n%2==1) ? a[int(n/2)+1] : (a[n/2]+a[n/2+1])/2
        printf "  p95:    %8d us\n", a[int(n*0.95)]
        printf "  p99:    %8d us\n", a[int(n*0.99)]
        printf "  max:    %8d us\n", a[n]
        printf "  mean:   %8.0f us\n", sum/n
    }
' "$DURATIONS"

rm -f "$DURATIONS"
