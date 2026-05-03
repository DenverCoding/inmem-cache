#!/bin/sh
# End-to-end test suite: exercises every scopecache endpoint over the real
# transport. Supports two modes, chosen by env:
#
#   unix socket (default) — standalone scopecache service:
#     SOCK=/run/scopecache.sock BASE=http://localhost
#     docker compose up -d --build scopecache
#     docker compose exec dev sh /src/e2e_test.sh
#
#   tcp — Caddy module wrapping the core (caddymodule/):
#     SOCK= BASE=http://caddyscope:8080
#     docker compose up -d --build caddyscope
#     docker compose exec -e SOCK= -e BASE=http://caddyscope:8080 dev \
#         sh /src/e2e_test.sh
#
# Empty SOCK disables --unix-socket and the script falls through to plain TCP.
# Every assertion is transport-agnostic — passing on both modes proves the
# standalone adapter and the Caddy adapter behave identically.
#
# The script does NOT fail fast. Every assertion runs, every failure is
# logged with its label and observed body, and the trailing summary
# reports `pass: N / fail: M`. The process exits 1 when at least one
# assertion failed, otherwise 0. This is deliberate: e2e regressions
# often cluster (a route change cascades into multiple shape checks),
# and a single run that surfaces the whole set is more useful than
# fixing one error, rerunning, and finding the next.

set -eu

# Required commands. Failing here gives a clear "missing X" message
# up-front instead of a cryptic curl/jq/openssl error halfway
# through the run. curl drives every HTTP call; jq parses the
# /admin and /guarded slot envelopes and powers the precise-shape
# json_assert helper; openssl computes capability_ids for the
# /guarded test setup.
need_cmd() {
    command -v "$1" >/dev/null 2>&1 || {
        printf 'missing required command: %s\n' "$1" >&2
        exit 127
    }
}
need_cmd curl
need_cmd jq
need_cmd openssl

SOCK=${SOCK-/run/scopecache.sock}
BASE=${BASE:-http://localhost}

# Echo the chosen transport so the run output is self-describing.
# A surprise mismatch (operator thinks they're testing the Caddy
# adapter but SOCK still points at the standalone Unix socket)
# shows up at the top of the log instead of being inferred from
# the URL pattern in failing assertions.
if [ -n "$SOCK" ]; then
    printf 'transport: unix-socket %s, BASE=%s\n' "$SOCK" "$BASE"
else
    printf 'transport: TCP, BASE=%s\n' "$BASE"
fi
printf 'NOTE: this script destroys cache state (multiple /wipe calls). Do not run it against production.\n\n'

# Per-process scratch file for the most recent curl response body.
# Originally a hardcoded /tmp/body, which two parallel runs of this
# script would race on (and which leaves stale bytes behind on a
# crashed run that the next invocation could read by mistake).
# mktemp gives us an isolated path; the EXIT trap cleans up on any
# exit, success or failure.
RESP_BODY=$(mktemp)
trap 'rm -f "$RESP_BODY"' EXIT INT TERM

pass=0
fail=0

say()   { printf '%s\n' "$*"; }
okmsg() { pass=$((pass+1)); printf '  ok   %s\n' "$*"; }
bad()   { fail=$((fail+1)); printf '  FAIL %s\n' "$*"; }

# SOCK="" disables the unix-socket flag so the same curl line works for both
# the standalone binary (AF_UNIX) and the Caddy module (TCP).
if [ -n "$SOCK" ]; then
    _sockargs="--unix-socket $SOCK"
else
    _sockargs=""
fi

# req METHOD PATH [BODY]
# Prints "<status>\n<body>" so callers can `read` them separately.
req() {
    _method=$1; _path=$2; _body=${3:-}
    if [ -n "$_body" ]; then
        # shellcheck disable=SC2086
        curl -s -o "$RESP_BODY" -w '%{http_code}' $_sockargs \
             -X "$_method" -H 'Content-Type: application/json' \
             -d "$_body" "$BASE$_path"
    else
        # shellcheck disable=SC2086
        curl -s -o "$RESP_BODY" -w '%{http_code}' $_sockargs \
             -X "$_method" "$BASE$_path"
    fi
    printf '\n'
    cat "$RESP_BODY"
}

expect() {
    _label=$1; _want=$2; _got=$3; _body=$4
    if [ "$_want" = "$_got" ]; then
        okmsg "$_label -> $_got"
    else
        bad "$_label: want $_want got $_got"
        printf '       body: %s\n' "$_body"
    fi
}

call() {
    _label=$1; _want=$2; _method=$3; _path=$4; _body=${5:-}
    _out=$(req "$_method" "$_path" "$_body")
    _status=$(printf '%s' "$_out" | head -n1)
    _bod=$(printf '%s' "$_out" | tail -n +2)
    expect "$_label" "$_want" "$_status" "$_bod"
    LAST_BODY=$_bod
}

# quiet_call: like call() but does not emit an `ok ...` line on
# success — only fails are logged. Use for tight loops of muterende
# calls where every iteration on the happy path adds line noise but
# a single failed iteration in the middle would otherwise be
# invisible until the eventual end-state assertion. The pass
# counter is still incremented per success so the trailing
# pass/fail summary stays accurate.
# json_assert: precise JSON-shape check on $LAST_BODY using jq -e.
# Use for invariants where substring matching could give false
# positives (e.g. `"payload":7` would also match `"payload":70`,
# `"id":"t1"` would also match `"id":"t10"` or `"id":"t12"`).
#
# Argument is a jq filter that must evaluate to a truthy value
# (true, non-zero number, non-empty string/array). On success the
# pass counter increments; on miss or jq error the body is logged
# under the label.
#
# Substring `case $LAST_BODY in` checks are fine for presence
# assertions ("body mentions deleted_scopes") and error-string
# matching ("error contains 'exceed'"); save json_assert for
# numeric equality, id-list equality, and exact-count checks.
json_assert() {
    _label=$1; _expr=$2
    if printf '%s' "$LAST_BODY" | jq -e "$_expr" >/dev/null 2>&1; then
        okmsg "$_label"
    else
        bad "$_label: $LAST_BODY"
    fi
}

quiet_call() {
    _label=$1; _want=$2; _method=$3; _path=$4; _body=${5:-}
    _out=$(req "$_method" "$_path" "$_body")
    _status=$(printf '%s' "$_out" | head -n1)
    if [ "$_status" = "$_want" ]; then
        pass=$((pass+1))
    else
        _bod=$(printf '%s' "$_out" | tail -n +2)
        bad "$_label: want $_want got $_status"
        printf '       body: %s\n' "$_bod"
    fi
}

# admin_call: dispatches a single sub-call through /admin's envelope and
# returns the SLOT's status + body. Used for /wipe, /warm, /rebuild,
# /delete_scope which are no longer reachable on the public mux. The
# envelope itself always returns 200; what the test cares about is the
# slot result. See guardedflow.md §J, §K.
admin_call() {
    _label=$1; _want=$2; _path=$3; _subbody=${4:-}
    if [ -n "$_subbody" ]; then
        _envelope="{\"calls\":[{\"path\":\"$_path\",\"body\":$_subbody}]}"
    else
        _envelope="{\"calls\":[{\"path\":\"$_path\"}]}"
    fi
    _out=$(req POST /admin "$_envelope")
    _status=$(printf '%s' "$_out" | head -n1)
    _bod=$(printf '%s' "$_out" | tail -n +2)
    if [ "$_status" != "200" ]; then
        bad "$_label admin envelope failed: $_status"
        printf '       body: %s\n' "$_bod"
        return
    fi
    _slotStatus=$(printf '%s' "$_bod" | jq -r '.results[0].status')
    _slotBody=$(printf '%s' "$_bod" | jq -c '.results[0].body')
    expect "$_label" "$_want" "$_slotStatus" "$_slotBody"
    LAST_BODY=$_slotBody
}

# guarded_call: dispatches a single sub-call through /guarded's envelope.
# Same shape as admin_call but with a token in the request body.
guarded_call() {
    _label=$1; _want=$2; _token=$3; _path=$4; _subbody=${5:-}
    if [ -n "$_subbody" ]; then
        _envelope="{\"token\":\"$_token\",\"calls\":[{\"path\":\"$_path\",\"body\":$_subbody}]}"
    else
        _envelope="{\"token\":\"$_token\",\"calls\":[{\"path\":\"$_path\"}]}"
    fi
    _out=$(req POST /guarded "$_envelope")
    _status=$(printf '%s' "$_out" | head -n1)
    _bod=$(printf '%s' "$_out" | tail -n +2)
    if [ "$_status" != "200" ]; then
        # Envelope-level failure (e.g. 401 missing_token, 400
        # scope_not_provisioned). Treat the envelope status as the
        # comparable.
        expect "$_label" "$_want" "$_status" "$_bod"
        LAST_BODY=$_bod
        return
    fi
    _slotStatus=$(printf '%s' "$_bod" | jq -r '.results[0].status')
    _slotBody=$(printf '%s' "$_bod" | jq -c '.results[0].body')
    expect "$_label" "$_want" "$_slotStatus" "$_slotBody"
    LAST_BODY=$_slotBody
}

# admin_call_query: like admin_call but for GET-style sub-calls
# (head, tail, get, stats, delete_scope_candidates) that
# take a query map instead of a body. The query argument is a JSON
# object string. /render is intentionally NOT in /admin's whitelist
# (raw payload bytes don't fit a JSON results array cleanly), so it
# is unreachable through this helper — operators reach /render
# directly on the public mux.
admin_call_query() {
    _label=$1; _want=$2; _path=$3; _query=$4
    _envelope="{\"calls\":[{\"path\":\"$_path\",\"query\":$_query}]}"
    _out=$(req POST /admin "$_envelope")
    _status=$(printf '%s' "$_out" | head -n1)
    _bod=$(printf '%s' "$_out" | tail -n +2)
    if [ "$_status" != "200" ]; then
        bad "$_label admin envelope failed: $_status"
        printf '       body: %s\n' "$_bod"
        return
    fi
    _slotStatus=$(printf '%s' "$_bod" | jq -r '.results[0].status')
    _slotBody=$(printf '%s' "$_bod" | jq -c '.results[0].body')
    expect "$_label" "$_want" "$_slotStatus" "$_slotBody"
    LAST_BODY=$_slotBody
}

# guarded_call_query: like guarded_call but for GET-style sub-calls.
guarded_call_query() {
    _label=$1; _want=$2; _token=$3; _path=$4; _query=$5
    _envelope="{\"token\":\"$_token\",\"calls\":[{\"path\":\"$_path\",\"query\":$_query}]}"
    _out=$(req POST /guarded "$_envelope")
    _status=$(printf '%s' "$_out" | head -n1)
    _bod=$(printf '%s' "$_out" | tail -n +2)
    if [ "$_status" != "200" ]; then
        expect "$_label" "$_want" "$_status" "$_bod"
        LAST_BODY=$_bod
        return
    fi
    _slotStatus=$(printf '%s' "$_bod" | jq -r '.results[0].status')
    _slotBody=$(printf '%s' "$_bod" | jq -c '.results[0].body')
    expect "$_label" "$_want" "$_slotStatus" "$_slotBody"
    LAST_BODY=$_slotBody
}

# --- start clean ---------------------------------------------------------------
say '== wipe for clean slate =='
admin_call 'wipe initial'               200 /wipe

# --- help / stats / unknown routes --------------------------------------------
say '== introspection =='
call 'help'                             200 GET    /help
call 'stats empty'                      200 GET    /stats
call 'unknown route'                    404 GET    /nope
call 'wrong method on /help'            405 POST   /help

# --- /stats aggregate counters ------------------------------------------------
# /stats is aggregate-only since the 100k-scope DoS observation: four
# atomic counters (scope_count, total_items, approx_store_mb,
# last_write_ts) plus the static cap, no per-scope map. This block
# proves the four counters stay in lockstep with the actual store
# contents across a deterministic wipe → appends sequence:
#
#   - 5 appends to scope "stat_a" (no id, no upsert, no replace)
#   - 3 appends to scope "stat_b" — 1 plain, 2 upserts on the same id so
#     net items added = 2 (idempotency check).
#
# Expected post-state: scope_count=2, total_items=7, approx_store_mb
# strictly between 0 and the configured cap, and last_write_ts strictly
# greater than the value /wipe stamped (proving every subsequent write
# advanced the freshness tick). A regression on any write/delete path
# that forgets to update one of the atomics shows up here as a clean
# numeric mismatch — no per-scope walk needed.
say '== /stats aggregate counters =='
call 'stats agg: wipe'                  200 POST   /wipe
call 'stats agg: stats after wipe'      200 GET    /stats
json_assert 'stats agg: scope_count == 0 after wipe' '.scope_count == 0'
json_assert 'stats agg: total_items == 0 after wipe' '.total_items == 0'
json_assert 'stats agg: approx_store_mb == 0 after wipe' '.approx_store_mb == 0'
# /wipe is itself a state-changing event, so last_write_ts must be
# strictly greater than 0 right after — even though scope_count is 0.
# This is the contract that lets a polling client distinguish "cache
# was wiped" from "cache was never written to" (the latter reports 0).
json_assert 'stats agg: last_write_ts > 0 after wipe' '.last_write_ts > 0'
LAST_WRITE_TS_AFTER_WIPE=$(printf '%s' "$LAST_BODY" | jq '.last_write_ts')

i=1; while [ $i -le 5 ]; do
    quiet_call "stats agg: append stat_a #$i" 200 POST /append \
        "{\"scope\":\"stat_a\",\"payload\":$i}"
    i=$((i+1))
done

# Plain append into stat_b — net +1 item.
quiet_call 'stats agg: append stat_b plain' 200 POST /append \
    '{"scope":"stat_b","payload":"first"}'
# Two upserts on the SAME id — net +1 item (idempotent on the second call).
quiet_call 'stats agg: upsert stat_b id=u1 (create)' 200 POST /upsert \
    '{"scope":"stat_b","id":"u1","payload":"v1"}'
quiet_call 'stats agg: upsert stat_b id=u1 (replace)' 200 POST /upsert \
    '{"scope":"stat_b","id":"u1","payload":"v2"}'

call 'stats agg: stats after appends'   200 GET    /stats
json_assert 'stats agg: scope_count == 2' '.scope_count == 2'
json_assert 'stats agg: total_items == 7' '.total_items == 7'
json_assert 'stats agg: approx_store_mb > 0' '.approx_store_mb > 0'
json_assert 'stats agg: approx_store_mb < max_store_mb' '.approx_store_mb < .max_store_mb'
# last_write_ts must have strictly advanced past the post-wipe value —
# every successful write/upsert above bumps the freshness tick via
# CAS-max. This is the polling-pattern contract: clients refetching
# only on tick-change are guaranteed to see every state mutation.
json_assert 'stats agg: last_write_ts strictly advanced past wipe' \
    ".last_write_ts > $LAST_WRITE_TS_AFTER_WIPE"
# Regression guard: /stats must NOT carry per-scope detail (moved to
# /scopelist). Re-introducing it would silently re-create the 100k-scope
# DoS that motivated this strip.
json_assert 'stats agg: no scopes key' '(.scopes // null) == null'

# --- writes: append / upsert / update / counter_add ---------------------------
say '== writes =='
call 'append'                           200 POST   /append   '{"scope":"s","id":"a","payload":{"v":1}}'
call 'append (no id)'                   200 POST   /append   '{"scope":"s","payload":"raw"}'
call 'append scope 2'                   200 POST   /append   '{"scope":"t","id":"x","payload":42}'

call 'upsert create'                    200 POST   /upsert   '{"scope":"s","id":"new","payload":[1,2,3]}'
call 'upsert replace'                   200 POST   /upsert   '{"scope":"s","id":"new","payload":[4,5,6]}'

call 'update by id'                     200 POST   /update   '{"scope":"s","id":"a","payload":{"v":2}}'

call 'counter_add create'               200 POST   /counter_add '{"scope":"c","id":"hits","by":1}'
call 'counter_add inc'                  200 POST   /counter_add '{"scope":"c","id":"hits","by":5}'
call 'counter_add dec'                  200 POST   /counter_add '{"scope":"c","id":"hits","by":-2}'

# --- reads: head / tail / get / render ----------------------------------------
say '== reads =='
call 'head'                             200 GET    '/head?scope=s'
call 'tail'                             200 GET    '/tail?scope=s'
call 'get by id'                        200 GET    '/get?scope=s&id=a'
# /get returns 200 with "hit":false on miss (envelope pattern), unlike /render
# which returns a real 404. Assert both the status AND the miss flag.
call 'get by id miss'                   200 GET    '/get?scope=s&id=missing'
case $LAST_BODY in
    *'"hit":false'*) okmsg 'get miss has "hit":false' ;;
    *) bad "get miss body: $LAST_BODY" ;;
esac
call 'render by id (JSON object)'       200 GET    '/render?scope=s&id=a'
call 'render by id (JSON string)'       200 GET    '/render?scope=s&id=new'
call 'render miss'                      404 GET    '/render?scope=s&id=missing'

# --- warm / rebuild -----------------------------------------------------------
say '== bulk =='
admin_call 'warm'    200 /warm    '{"items":[{"scope":"warm1","id":"a","payload":"A"},{"scope":"warm1","id":"b","payload":"B"},{"scope":"warm2","payload":1}]}'
admin_call 'rebuild' 200 /rebuild '{"items":[{"scope":"only","id":"one","payload":{"k":"v"}}]}'

# After /rebuild the previous scopes are gone. /get still envelopes misses in
# a 200 with "hit":false (only /render returns 404).
call 'post-rebuild: old scope gone'     200 GET    '/get?scope=s&id=a'
case $LAST_BODY in
    *'"hit":false'*) okmsg 'post-rebuild old scope: "hit":false' ;;
    *) bad "post-rebuild old scope body: $LAST_BODY" ;;
esac
call 'post-rebuild: new scope reads'    200 GET    '/get?scope=only&id=one'

# --- candidates / delete_up_to / delete / delete_scope ------------------------
say '== deletes =='
call 'append bulk for trim'  200 POST /append '{"scope":"trim","id":"a","payload":1}'
call 'append bulk for trim'  200 POST /append '{"scope":"trim","id":"b","payload":2}'
call 'append bulk for trim'  200 POST /append '{"scope":"trim","id":"c","payload":3}'

# After three /append calls to a fresh "trim" scope the seqs are 1,2,3.
# Trimming up to seq 2 should leave a single item behind.
call 'delete_up_to (trims oldest)'      200 POST   /delete_up_to '{"scope":"trim","max_seq":2}'

call 'delete by id'                     200 POST   /delete   '{"scope":"only","id":"one"}'
# /delete on a non-existent id returns 200 with "hit":false (same envelope
# pattern as /get). Only /render returns real 404s.
call 'delete miss'                      200 POST   /delete   '{"scope":"only","id":"ghost"}'
case $LAST_BODY in
    *'"hit":false'*) okmsg 'delete miss has "hit":false' ;;
    *) bad "delete miss body: $LAST_BODY" ;;
esac
admin_call 'delete_scope'               200 /delete_scope '{"scope":"trim"}'
admin_call 'delete_scope_candidates'    200 /delete_scope_candidates

# --- validation errors (400) --------------------------------------------------
say '== validation =='
call 'append: missing scope'            400 POST   /append   '{"payload":{}}'
call 'append: missing payload'          400 POST   /append   '{"scope":"s"}'
call 'append: null payload'             400 POST   /append   '{"scope":"s","payload":null}'
call 'append: client seq'               400 POST   /append   '{"scope":"s","payload":{},"seq":5}'
call 'append: bad JSON'                 400 POST   /append   '{not-json'
call 'counter_add: zero by'             400 POST   /counter_add '{"scope":"c","id":"hits","by":0}'
call 'counter_add: missing by'          400 POST   /counter_add '{"scope":"c","id":"hits"}'
call 'update: both id and seq'          400 POST   /update   '{"scope":"s","id":"a","seq":1,"payload":{}}'
call 'delete_up_to: missing max_seq'    400 POST   /delete_up_to '{"scope":"x"}'

# --- counter-on-non-integer (409) ---------------------------------------------
say '== counter conflict =='
call 'upsert non-int'                   200 POST   /upsert   '{"scope":"c","id":"str","payload":"not a number"}'
call 'counter_add on non-int'           409 POST   /counter_add '{"scope":"c","id":"str","by":1}'

# --- arithmetic sanity --------------------------------------------------------
# These four blocks each loop a small number of mutating calls and then assert
# the aggregate state with a single read — they catch regressions where an
# individual op returns 200 but the cache ends up in the wrong shape.
say '== arithmetic sanity =='

# counter: 10x(+1) + 3x(-1) must land at exactly 7
i=0; while [ $i -lt 10 ]; do
    quiet_call "counter +1 #$i" 200 POST /counter_add '{"scope":"cmath","id":"n","by":1}'
    i=$((i+1))
done
i=0; while [ $i -lt 3 ]; do
    quiet_call "counter -1 #$i" 200 POST /counter_add '{"scope":"cmath","id":"n","by":-1}'
    i=$((i+1))
done
call 'counter: read final'              200 GET    '/get?scope=cmath&id=n'
json_assert 'counter 10x(+1) + 3x(-1) == 7' '.item.payload == 7'

# delete_up_to: 10 appends, trim up to seq 6, only t7..t10 must survive
i=1; while [ $i -le 10 ]; do
    quiet_call "tmath /append #$i" 200 POST /append "{\"scope\":\"tmath\",\"id\":\"t$i\",\"payload\":$i}"
    i=$((i+1))
done
call 'trim: delete_up_to seq=6'         200 POST   /delete_up_to '{"scope":"tmath","max_seq":6}'
call 'trim: head after trim'            200 GET    '/head?scope=tmath'
# Exact id-list invariant: only t7..t10 remain, in seq order.
# Substring matching is too weak here — `"id":"t1"` would also
# match "t10", "t11"; `"id":"t7"` matches "t7" but says nothing
# about whether t1..t6 are also still there.
json_assert 'trim: items[].id == [t7,t8,t9,t10]' '[.items[].id] == ["t7","t8","t9","t10"]'

# upsert idempotency: 5 upserts on the same id must leave exactly 1 item,
# and the surviving payload must be the last one written (4). The
# previous version also probed /stats for `.scopes.uidem.item_count == 1`,
# but /stats is now aggregate-only — per-scope detail moved to the
# (future) /scopelist endpoint, so the per-scope item count is verified
# by reading the surviving item back via /get.
i=0; while [ $i -lt 5 ]; do
    quiet_call "uidem /upsert #$i" 200 POST /upsert "{\"scope\":\"uidem\",\"id\":\"only\",\"payload\":$i}"
    i=$((i+1))
done
call 'upsert idem: final value'         200 GET    '/get?scope=uidem&id=only'
json_assert 'upsert idem: final payload is 4' '.item.payload == 4'

# tail windowing: 10 appends (seq 1..10). limit=5 is the newest slice (t6..t10);
# limit=5&offset=5 skips that newest slice and returns the previous one (t1..t5).
i=1; while [ $i -le 10 ]; do
    quiet_call "tail10 /append #$i" 200 POST /append "{\"scope\":\"tail10\",\"id\":\"t$i\",\"payload\":$i}"
    i=$((i+1))
done
call 'tail limit=5 (newest)'            200 GET    '/tail?scope=tail10&limit=5'
# Single strict equality replaces three substring checks
# (t6..t10 present + t1..t5 absent + seq 6..10 present): one
# array-equality covers all three invariants in one assertion.
# /tail returns newest-first, so the order is [t10, t9, ..., t6].
# /tail returns the selected window in seq-ascending order (oldest
# first within the slice), so limit=5 is t6..t10.
json_assert 'tail newest: items[].id == [t6..t10]' '[.items[].id] == ["t6","t7","t8","t9","t10"]'
json_assert 'tail newest: items[].seq == [6..10]' '[.items[].seq] == [6,7,8,9,10]'

call 'tail limit=5 offset=5 (oldest)'   200 GET    '/tail?scope=tail10&limit=5&offset=5'
# offset=5 skips the newest 5 (t6..t10) and returns t1..t5 (older slice).
json_assert 'tail offset=5: items[].id == [t1..t5]' '[.items[].id] == ["t1","t2","t3","t4","t5"]'
json_assert 'tail offset=5: items[].seq == [1..5]' '[.items[].seq] == [1,2,3,4,5]'

# Cross-field consistency on a fresh scope: 10 appends produce
# count=10 and three parallel arrays (id, seq, payload) that all
# agree on the same row index. The bestaande appn-test only checks
# item_count via /stats — that would still pass if seq numbering
# rewound, if a row was dropped and another duplicated, or if
# payload landed against the wrong id. This jq filter pins all four
# invariants in one assertion: a single regression on any of them
# fails the line.
i=1
while [ $i -le 10 ]; do
    quiet_call "exact10 /append #$i" 200 POST /append "{\"scope\":\"exact10\",\"id\":\"a$i\",\"payload\":$i}"
    i=$((i+1))
done
call 'exact10: head limit=10' 200 GET '/head?scope=exact10&limit=10'
json_assert 'exact10: count + ids + seqs + payloads all align' '
    .count == 10 and
    [.items[].id] == ["a1","a2","a3","a4","a5","a6","a7","a8","a9","a10"] and
    [.items[].seq] == [1,2,3,4,5,6,7,8,9,10] and
    [.items[].payload] == [1,2,3,4,5,6,7,8,9,10]
'

# --- cache-owned ts -----------------------------------------------------------
# `ts` is a microsecond unix timestamp set by the cache on every write
# that touches an item: /append, /upsert (create + replace), /update,
# /counter_add (create + increment), /warm, /rebuild, /inbox. Clients
# MUST NOT supply ts on any write — every write rejects a non-zero
# client `ts` with 400. The field is observability only (not searchable,
# not indexed, not used for ordering); /ts_range no longer exists.
say '== cache-owned ts =='

# Every write auto-stamps ts. Read it back and confirm it's non-zero.
call 'ts auto-stamped on /append'       200 POST   /append '{"scope":"tsv","id":"a","payload":1}'
case $LAST_BODY in
    *'"ts":0'*) bad "ts not auto-stamped on /append (got ts:0): $LAST_BODY" ;;
    *'"ts":'[1-9]*) okmsg 'ts auto-stamped on /append (non-zero)' ;;
    *) bad "ts missing from /append response: $LAST_BODY" ;;
esac

# Client-supplied ts is rejected by the validator (400) on every write.
call 'ts forbidden on /append'          400 POST   /append '{"scope":"tsv","id":"x","payload":1,"ts":1000}'
call 'ts forbidden on /upsert'          400 POST   /upsert '{"scope":"tsv","id":"a","payload":1,"ts":1000}'
call 'ts forbidden on /update'          400 POST   /update '{"scope":"tsv","id":"a","payload":1,"ts":1000}'

# Refresh-on-write: /upsert replace must produce a strictly-newer ts.
call 'baseline ts (read after first append)' 200 GET '/get?scope=tsv&id=a'
case $LAST_BODY in
    *'"ts":'*) okmsg 'baseline ts present in /get response' ;;
    *) bad "ts missing on /get: $LAST_BODY" ;;
esac
sleep 1
call 'upsert replace refreshes ts'      200 POST   /upsert '{"scope":"tsv","id":"a","payload":2}'
case $LAST_BODY in
    *'"created":false'*) okmsg 'upsert hit replace path' ;;
    *) bad "upsert did not replace: $LAST_BODY" ;;
esac

# /counter_add stamps the per-item `ts` on create AND refreshes it on
# every increment — that gives counters a free "last activity"
# timestamp consumers can poll via /get. The scope-level / store-wide
# freshness signal (last_write_ts on /stats) is intentionally NOT bumped
# by counter_add — see the next block for the contract and rationale.
call 'counter_add create stamps ts'     200 POST   /counter_add '{"scope":"tsv","id":"c","by":5}'
call 'verify counter ts present'        200 GET    '/get?scope=tsv&id=c'
case $LAST_BODY in
    *'"ts":'[1-9]*'"payload":5'*) okmsg 'counter_add create: ts and payload present' ;;
    *) bad "counter_add create response: $LAST_BODY" ;;
esac
sleep 1
call 'counter_add increment refreshes ts' 200 POST /counter_add '{"scope":"tsv","id":"c","by":3}'
call 'verify counter ts refreshed'      200 GET    '/get?scope=tsv&id=c'
case $LAST_BODY in
    *'"ts":'[1-9]*'"payload":8'*) okmsg 'counter_add increment: ts and updated payload present' ;;
    *) bad "counter_add increment response: $LAST_BODY" ;;
esac

# /counter_add MUST NOT bump /stats.last_write_ts on create or increment
# — counter activity is read-driven by design (view-counter +1 on every
# topic hit) and bumping the store-wide freshness tick on each call
# would degrade it into a heartbeat, breaking the polling contract that
# tells consumers "skip refetch when nothing changed". The per-counter
# ts (verified above) remains the granularity at which counter activity
# IS observable.
#
# The test seeds the scope via /append first so the scope-creation bump
# (which DOES legitimately fire when /counter_add hits a brand-new
# scope) is out of the way before we measure counter behaviour. The
# baseline /stats snapshot is then captured AFTER the seed, and we
# assert last_write_ts is unchanged after both create and increment.
call 'ts-stats: seed scope so creation bump is out of the way' 200 POST /append \
    '{"scope":"tsv_stats","id":"seed","payload":"v"}'
call 'ts-stats: baseline /stats'        200 GET    /stats
LAST_WRITE_TS_BEFORE_COUNTER=$(printf '%s' "$LAST_BODY" | jq '.last_write_ts')
sleep 1
call 'ts-stats: counter_add create on existing scope' 200 POST /counter_add \
    '{"scope":"tsv_stats","id":"views","by":1}'
call 'ts-stats: /stats after counter create' 200 GET /stats
json_assert 'ts-stats: last_write_ts unchanged after counter create' \
    ".last_write_ts == $LAST_WRITE_TS_BEFORE_COUNTER"
sleep 1
call 'ts-stats: counter_add increment' 200 POST /counter_add \
    '{"scope":"tsv_stats","id":"views","by":1}'
call 'ts-stats: /stats after counter increment' 200 GET /stats
json_assert 'ts-stats: last_write_ts unchanged after counter increment' \
    ".last_write_ts == $LAST_WRITE_TS_BEFORE_COUNTER"
# But the per-counter ts MUST advance — verify both increments are
# observable on the item itself, just not on the store-wide signal.
call 'ts-stats: read counter to confirm per-item ts advanced' 200 GET \
    '/get?scope=tsv_stats&id=views'
json_assert 'ts-stats: counter item value == 2 (both increments landed)' \
    '.item.payload == 2'
json_assert 'ts-stats: counter item ts is fresh (> baseline last_write_ts)' \
    ".item.ts > $LAST_WRITE_TS_BEFORE_COUNTER"

# Empty/missing ts in body is treated as "absent" (zero value) and passes
# validation; the cache stamps now() on its own. ts:0 from a client is the
# JSON-decode default for an absent field, so it must not be rejected.
call 'absent ts on /append OK'          200 POST   /append '{"scope":"tsv","id":"d","payload":4}'
case $LAST_BODY in
    *'"ts":'[1-9]*) okmsg 'absent ts: cache stamps anyway' ;;
    *) bad "absent ts handling: $LAST_BODY" ;;
esac

# /ts_range is gone — the route is unregistered on every adapter.
call '/ts_range removed (404)'          404 GET    '/ts_range?scope=tsv'

# --- /multi_call --------------------------------------------------------------
# Sequential dispatch of N self-contained sub-calls in one HTTP roundtrip.
# Each slot reflects what the standalone endpoint would have returned, so the
# inner shapes are the same as their dedicated tests above — what we verify
# here is the dispatcher: ordering, whitelist enforcement, count/body caps,
# and the no-cross-call-atomicity contract.
say '== multi_call =='

# Wipe so /stats inside the batch sees a deterministic shape.
admin_call 'mc: wipe for clean slate'   200 /wipe

# Happy path: write, then read it back inside the same batch, then aggregate.
# The /get at index 1 must see the /append from index 0 — proves sequential
# dispatch (not parallel, not snapshot-isolated). /stats was previously
# the third read here; it moved to /admin in v0.5.17, so the batch ends
# with /tail which observes the just-appended item.
call 'mc: mixed read/write/read'        200 POST   /multi_call \
    '{"calls":[{"path":"/append","body":{"scope":"mc","id":"a","payload":{"v":1}}},{"path":"/get","query":{"scope":"mc","id":"a"}},{"path":"/tail","query":{"scope":"mc","limit":10}}]}'
case $LAST_BODY in
    *'"ok":true'*'"count":3'*) okmsg 'mc: outer ok=true, count=3' ;;
    *) bad "mc happy outer: $LAST_BODY" ;;
esac
case $LAST_BODY in
    *'"approx_response_mb"'*'"duration_us"'*) okmsg 'mc: outer envelope carries approx_response_mb + duration_us' ;;
    *) bad "mc outer envelope shape: $LAST_BODY" ;;
esac
# Sub-call /get at index 1 must see the just-appended item — sequential dispatch.
case $LAST_BODY in
    *'"status":200'*'"hit":true'*'"seq":1'*) okmsg 'mc: /get inside batch saw the prior /append (sequential)' ;;
    *) bad "mc sequential dispatch: $LAST_BODY" ;;
esac
# /tail at index 2 must reflect the in-batch /append: items array length 1
# with the just-written id. Confirms writes propagate across sequential slots.
case $LAST_BODY in
    *'"items":[{'*'"id":"a"'*) okmsg 'mc: /tail slot reflects in-batch write' ;;
    *) bad "mc tail slot: $LAST_BODY" ;;
esac

# Empty calls array: 200 with count:0, results empty. N=0 calls produces
# N=0 results — not an error.
call 'mc: empty calls array'            200 POST   /multi_call '{"calls":[]}'
case $LAST_BODY in
    *'"ok":true'*'"count":0'*'"results":[]'*) okmsg 'mc: empty calls -> count=0, results=[]' ;;
    *) bad "mc empty: $LAST_BODY" ;;
esac

# Missing 'calls' field is malformed input — distinct from explicitly empty.
call 'mc: missing calls field'          400 POST   /multi_call '{}'
call 'mc: malformed JSON'               400 POST   /multi_call '{"calls":['
call 'mc: GET rejected'                 405 GET    /multi_call

# Whitelist: /wipe is excluded (store-wide lock, doesn't compose with a
# sequential dispatcher). The /get at index 0 must NOT execute — the whole
# batch is rejected up-front.
call 'mc: whitelist reject /wipe'       400 POST   /multi_call \
    '{"calls":[{"path":"/get","query":{"scope":"mc","id":"a"}},{"path":"/wipe"}]}'
case $LAST_BODY in
    *"calls[1]"*) okmsg 'mc: whitelist error names offending index' ;;
    *) bad "mc whitelist error shape: $LAST_BODY" ;;
esac

# Each excluded endpoint individually: /warm, /rebuild, /wipe,
# /delete_scope (admin-only), /render, /help, /multi_call self-reference
# — all 400.
call 'mc: exclude /warm'                400 POST   /multi_call '{"calls":[{"path":"/warm","body":{"items":[]}}]}'
call 'mc: exclude /rebuild'             400 POST   /multi_call '{"calls":[{"path":"/rebuild","body":{"items":[]}}]}'
call 'mc: exclude /delete_scope'        400 POST   /multi_call '{"calls":[{"path":"/delete_scope","body":{"scope":"mc"}}]}'
call 'mc: exclude /render'              400 POST   /multi_call '{"calls":[{"path":"/render","query":{"scope":"mc","id":"a"}}]}'
call 'mc: exclude /help'                400 POST   /multi_call '{"calls":[{"path":"/help"}]}'
call 'mc: exclude self /multi_call'     400 POST   /multi_call '{"calls":[{"path":"/multi_call","body":{"calls":[]}}]}'
call 'mc: unknown path'                 400 POST   /multi_call '{"calls":[{"path":"/does-not-exist"}]}'

# Count overflow: default cap is 10 sub-calls per batch. 11 must 400 with
# the configured maximum visible in the error message.
call 'mc: count overflow (11 > 10)'     400 POST   /multi_call \
    '{"calls":[{"path":"/get","query":{"scope":"mc","id":"a"}},{"path":"/get","query":{"scope":"mc","id":"a"}},{"path":"/get","query":{"scope":"mc","id":"a"}},{"path":"/get","query":{"scope":"mc","id":"a"}},{"path":"/get","query":{"scope":"mc","id":"a"}},{"path":"/get","query":{"scope":"mc","id":"a"}},{"path":"/get","query":{"scope":"mc","id":"a"}},{"path":"/get","query":{"scope":"mc","id":"a"}},{"path":"/get","query":{"scope":"mc","id":"a"}},{"path":"/get","query":{"scope":"mc","id":"a"}},{"path":"/get","query":{"scope":"mc","id":"a"}}]}'
case $LAST_BODY in
    *'maximum is 10'*) okmsg 'mc: count overflow names the cap' ;;
    *) bad "mc count error shape: $LAST_BODY" ;;
esac

# Side-effect non-rollback: an /append at slot 0 succeeds, an invalid /get
# (missing id/seq) at slot 1 lands a 400 in its own slot. The batch keeps
# running — there is no rollback. Verify both slot statuses *and* the post-
# batch state.
call 'mc: partial failure does not roll back'  200 POST   /multi_call \
    '{"calls":[{"path":"/append","body":{"scope":"mcsticky","id":"alive","payload":{"v":1}}},{"path":"/get","query":{"scope":"mcsticky"}}]}'
case $LAST_BODY in
    *'"status":200'*'"status":400'*) okmsg 'mc: slot0=200 (write), slot1=400 (bad get)' ;;
    *) bad "mc partial slots: $LAST_BODY" ;;
esac
# Post-batch the write must still be present.
call 'mc: post-batch /get sees the write' 200 GET '/get?scope=mcsticky&id=alive'
case $LAST_BODY in
    *'"hit":true'*) okmsg 'mc: slot0 write survived the slot1 failure' ;;
    *) bad "mc rollback check: $LAST_BODY" ;;
esac

# Query coercion: numbers and booleans in the query map are passed through as
# raw URL-query values. limit:2 must actually cap the /tail to 2 items.
i=1; while [ $i -le 5 ]; do
    quiet_call "mcq /append #$i" 200 POST /append "{\"scope\":\"mcq\",\"id\":\"q$i\",\"payload\":$i}"
    i=$((i+1))
done
call 'mc: number query value (limit:2)' 200 POST   /multi_call \
    '{"calls":[{"path":"/tail","query":{"scope":"mcq","limit":2}}]}'
case $LAST_BODY in
    *'"status":200'*'"count":2'*'"truncated":true'*) okmsg 'mc: number query coerced -> /tail limit=2 honoured' ;;
    *) bad "mc query coercion: $LAST_BODY" ;;
esac

# Nested object in a query value would silently lose shape when flattened
# to a URL query string — rejected for the whole batch.
call 'mc: nested query value rejected'  400 POST   /multi_call \
    '{"calls":[{"path":"/get","query":{"scope":{"nested":true}}}]}'

# --- reserved-scope boundary (public mux) -------------------------------------
# Every scope-bearing public handler must reject scopes starting with
# `_` (the reserved prefix). The block is enforced in-handler via
# rejectReservedScope; this section pins that contract on the wire so
# accidentally removing the helper from any one handler shows up as a
# 200 (or no longer 400) on these checks. The companion expectation —
# /admin CAN reach reserved scopes — is tested in the /admin section
# below (admin: register tenant in _tokens, etc.).
say '== reserved-scope boundary =='
call 'public /get on _* blocked'      400 GET    '/get?scope=_tokens&id=x'
call 'public /head on _* blocked'     400 GET    '/head?scope=_tokens'
call 'public /tail on _* blocked'     400 GET    '/tail?scope=_tokens'
call 'public /render on _* blocked'   400 GET    '/render?scope=_tokens&id=x'
call 'public /append on _* blocked'   400 POST   /append       '{"scope":"_tokens","id":"x","payload":1}'
call 'public /upsert on _* blocked'   400 POST   /upsert       '{"scope":"_tokens","id":"x","payload":1}'
call 'public /update on _* blocked'   400 POST   /update       '{"scope":"_tokens","id":"x","payload":1}'
call 'public /counter_add on _* blocked' 400 POST /counter_add '{"scope":"_tokens","id":"x","by":1}'
call 'public /delete on _* blocked'   400 POST   /delete       '{"scope":"_tokens","id":"x"}'
call 'public /delete_up_to on _* blocked' 400 POST /delete_up_to '{"scope":"_tokens","max_seq":1}'

# --- /admin -------------------------------------------------------------------
# Operator-elevated multi-call gateway. No body-level auth — gated by
# socket access + Caddyfile (the e2e harness reaches the listener
# directly, mimicking PHP/cron). Public counterparts of /wipe, /warm,
# /rebuild, /delete_scope are 404; /admin reaches the same handler
# functions through its dispatcher. See guardedflow.md §J, §K.
say '== admin =='

# Public route to admin-only paths returns 404.
call 'admin: public /wipe is 404'        404 POST   /wipe
call 'admin: public /warm is 404'        404 POST   /warm '{"items":[]}'
call 'admin: public /rebuild is 404'     404 POST   /rebuild '{"items":[{"scope":"x","payload":1}]}'
call 'admin: public /delete_scope 404'   404 POST   /delete_scope '{"scope":"x"}'

# /admin can write to reserved scopes — used here to set up the
# auth-gate: register a tenant's capability_id in _tokens.
admin_call 'admin: register tenant in _tokens' 200 /upsert '{"scope":"_tokens","id":"capX","payload":{"issued_at":"test"}}'

# NOTE: the previous test probed /stats for the literal string "_tokens"
# to confirm /admin saw the reserved scope. /stats is aggregate-only
# now (no per-scope name surface), so this assertion is dropped. When
# /scopelist lands, an equivalent reserved-scope-visibility check
# belongs there.

# /admin's whitelist excludes self-reference, /multi_call, /guarded,
# /help, and /render (raw bytes don't fit a JSON results array).
call 'admin: rejects /admin in calls' 400 POST /admin '{"calls":[{"path":"/admin"}]}'
call 'admin: rejects /multi_call'     400 POST /admin '{"calls":[{"path":"/multi_call","body":{"calls":[]}}]}'
call 'admin: rejects /guarded'        400 POST /admin '{"calls":[{"path":"/guarded","body":{"token":"x","calls":[]}}]}'
call 'admin: rejects /help'           400 POST /admin '{"calls":[{"path":"/help"}]}'
call 'admin: rejects /render'         400 POST /admin '{"calls":[{"path":"/render","query":{"scope":"x","id":"y"}}]}'

# Malformed admin envelope → 400.
call 'admin: malformed body'          400 POST /admin '{not-json'
call 'admin: missing calls field'     400 POST /admin '{}'

# /admin GET → 405.
call 'admin: GET rejected'            405 GET  /admin

# /admin /wipe with `include_reserved=true` query is irrelevant in this
# model (admin /wipe always wipes everything). Confirm it succeeds.
admin_call 'admin: full wipe'          200 /wipe

# --- /guarded -----------------------------------------------------------------
# Tenant-facing gateway. Token in body derives capability_id; sub-calls
# operate only on operator-provisioned `_guarded:<capId>:*` scopes.
# See guardedflow.md §F.
say '== guarded =='

# Reset and provision two tenants. capability_id values below are
# precomputed via:  hex(HMAC_SHA256(server_secret, token)).
# The standalone test binary uses SCOPECACHE_SERVER_SECRET="test-secret".
TENANT_A_TOKEN="tenant-A-token"
TENANT_B_TOKEN="tenant-B-token"
TENANT_A_CAP=$(printf '%s' "$TENANT_A_TOKEN" | openssl dgst -sha256 -hmac "test-secret" -hex | sed 's/^.*= //')
TENANT_B_CAP=$(printf '%s' "$TENANT_B_TOKEN" | openssl dgst -sha256 -hmac "test-secret" -hex | sed 's/^.*= //')

admin_call 'gd: register tenant A in _tokens' 200 /upsert "{\"scope\":\"_tokens\",\"id\":\"${TENANT_A_CAP}\",\"payload\":{\"issued_at\":\"e2e\"}}"
admin_call 'gd: register tenant B in _tokens' 200 /upsert "{\"scope\":\"_tokens\",\"id\":\"${TENANT_B_CAP}\",\"payload\":{\"issued_at\":\"e2e\"}}"

# Happy path: tenant A appends to their own events scope.
guarded_call 'gd: tenant A append'        200 "$TENANT_A_TOKEN" /append '{"scope":"events","id":"e1","payload":{"v":1}}'

# Response stripping: client sees `scope: "events"`, never the
# rewritten `_guarded:<capId>:events` form.
guarded_call_query 'gd: tenant A read back' 200 "$TENANT_A_TOKEN" /get '{"scope":"events","id":"e1"}'
case $LAST_BODY in
    *'"scope":"events"'*) okmsg 'gd: response prefix stripped (client sees "events")' ;;
    *) bad "gd: prefix not stripped: $LAST_BODY" ;;
esac
case $LAST_BODY in
    *'_guarded:'*) bad "gd: response leaked internal prefix: $LAST_BODY" ;;
    *) okmsg 'gd: no _guarded: prefix in response body' ;;
esac

# Random-token attack: forged token has no _tokens entry → whole-batch
# reject with tenant_not_provisioned, no side effect.
ROGUE_CAP=$(printf '%s' 'random-attacker' | openssl dgst -sha256 -hmac "test-secret" -hex | sed 's/^.*= //')
call 'gd: random token rejected'         400 POST /guarded \
    '{"token":"random-attacker","calls":[{"path":"/append","body":{"scope":"events","payload":"junk"}}]}'
case $LAST_BODY in
    *'tenant_not_provisioned'*) okmsg 'gd: tenant_not_provisioned error returned' ;;
    *) bad "gd: expected tenant_not_provisioned: $LAST_BODY" ;;
esac
# Side-effect-free: the rogue's would-be scope must not exist in the
# store. Pre-v0.5.12 this property fell out of the per-scope existence
# check; post-v0.5.12 it depends on the auth-gate firing before any
# /append handler runs, which is the load-bearing line for the
# empty-scope-spam DoS class.
admin_call_query 'gd: rogue scope still absent' 200 /get \
    "{\"scope\":\"_guarded:${ROGUE_CAP}:events\",\"id\":\"x\"}"
case $LAST_BODY in
    *'"hit":false'*) okmsg 'gd: rejected /append did not lazily create the scope' ;;
    *) bad "gd: rejected /append leaked a side effect: $LAST_BODY" ;;
esac

# Tenant with valid token writes to a scope the operator never touched
# — auto-creates within the tenant's prefix. Replaces the pre-v0.5.12
# "scope_not_provisioned" rejection on legit tokens.
guarded_call 'gd: tenant auto-creates fresh scope' 200 "$TENANT_A_TOKEN" /append \
    '{"scope":"ghosts","id":"x","payload":1}'
admin_call_query 'gd: auto-created scope is in store' 200 /get \
    "{\"scope\":\"_guarded:${TENANT_A_CAP}:ghosts\",\"id\":\"x\"}"
case $LAST_BODY in
    *'"hit":true'*) okmsg 'gd: tenant self-organized scope visible to operator' ;;
    *) bad "gd: auto-created scope missing from store: $LAST_BODY" ;;
esac

# Cross-tenant smuggle attempt: tenant A token + body.scope=own +
# query.scope=tenant B's reserved scope. Pre-fix this read tenant B's
# data because rewriteCallScope returned early after rewriting body
# and the GET dispatcher built the URL from un-rewritten query.scope.
# Now /guarded rejects when both body and query carry `scope`.
admin_call 'gd: seed B secret for smuggle test' 200 /append \
    "{\"scope\":\"_guarded:${TENANT_B_CAP}:events\",\"id\":\"smuggle-secret\",\"payload\":{\"do\":\"not-leak\"}}"

call 'gd: smuggle body+query rejected' 400 POST /guarded \
    "{\"token\":\"${TENANT_A_TOKEN}\",\"calls\":[{\"path\":\"/get\",\"body\":{\"scope\":\"events\",\"id\":\"x\"},\"query\":{\"scope\":\"_guarded:${TENANT_B_CAP}:events\",\"id\":\"smuggle-secret\"}}]}"
case $LAST_BODY in
    *'must be in body OR query'*) okmsg 'gd: smuggle rejected with both-carriers error' ;;
    *) bad "gd: expected both-carriers error: $LAST_BODY" ;;
esac
case $LAST_BODY in
    *'do-not-leak'*|*'sensitive'*) bad "gd: smuggle response leaked B's payload: $LAST_BODY" ;;
    *) okmsg 'gd: smuggle response did not leak B data' ;;
esac

# Whitelist enforcement: /wipe inside /guarded is rejected.
call 'gd: rejects /wipe sub-call'         400 POST /guarded \
    "{\"token\":\"${TENANT_A_TOKEN}\",\"calls\":[{\"path\":\"/wipe\"}]}"

# Whitelist enforcement: /delete_scope inside /guarded is rejected.
call 'gd: rejects /delete_scope sub-call' 400 POST /guarded \
    "{\"token\":\"${TENANT_A_TOKEN}\",\"calls\":[{\"path\":\"/delete_scope\",\"body\":{\"scope\":\"events\"}}]}"

# Whitelist enforcement: /render inside /guarded is rejected — raw-byte
# response is a category mismatch with the JSON envelope.
call 'gd: rejects /render sub-call'       400 POST /guarded \
    "{\"token\":\"${TENANT_A_TOKEN}\",\"calls\":[{\"path\":\"/render\",\"query\":{\"scope\":\"events\",\"id\":\"e1\"}}]}"

# Method-aware scope-placement (rewriteCallScope, v0.5.20): GET
# sub-calls must carry scope in query — body.scope is ignored by
# the inner handler. The /guarded dispatcher rejects up-front
# with a clear, method-named error instead of letting the call
# fall through and 400 with a vague "missing scope" downstream.
call 'gd: GET with body.scope rejected before dispatch' 400 POST /guarded \
    "{\"token\":\"${TENANT_A_TOKEN}\",\"calls\":[{\"path\":\"/get\",\"body\":{\"scope\":\"events\",\"id\":\"e1\"}}]}"
case $LAST_BODY in
    *'must be in query for GET'*) okmsg 'gd: GET body.scope rejected with clear method-aware error' ;;
    *) bad "gd: expected GET body.scope method-aware error: $LAST_BODY" ;;
esac

# Method-aware scope-placement, POST counterpart: POST sub-calls
# must carry scope in body — query.scope is ignored by the inner
# handler. Plus a side-effect check via /admin /get on the
# rewritten scope: a properly up-front-rejected POST must not have
# committed the item, even though the body had a valid id+payload.
call 'gd: POST with query.scope rejected before dispatch' 400 POST /guarded \
    "{\"token\":\"${TENANT_A_TOKEN}\",\"calls\":[{\"path\":\"/append\",\"query\":{\"scope\":\"events\"},\"body\":{\"id\":\"bad-post-query-scope\",\"payload\":1}}]}"
case $LAST_BODY in
    *'must be in body for POST'*) okmsg 'gd: POST query.scope rejected with clear method-aware error' ;;
    *) bad "gd: expected POST query.scope method-aware error: $LAST_BODY" ;;
esac
admin_call_query 'gd: POST query.scope reject created no item' 200 /get \
    "{\"scope\":\"_guarded:${TENANT_A_CAP}:events\",\"id\":\"bad-post-query-scope\"}"
case $LAST_BODY in
    *'"hit":false'*) okmsg 'gd: POST query.scope reject had no side effect' ;;
    *) bad "gd: POST query.scope reject created data: $LAST_BODY" ;;
esac

# Whole-batch prevalidation: rewriteCallScope walks every call
# before the dispatch loop, so a malformed later call must reject
# the entire batch and leave the earlier valid /append unapplied.
# Different error path than #1/#2: query.scope here is a JSON
# object, not a string, so the failure point is the
# json.Unmarshal-to-string inside rewriteCallScope.
call 'gd: malformed later query rejects whole batch' 400 POST /guarded \
    "{\"token\":\"${TENANT_A_TOKEN}\",\"calls\":[{\"path\":\"/append\",\"body\":{\"scope\":\"events\",\"id\":\"prevalid-should-not-land\",\"payload\":{\"v\":1}}},{\"path\":\"/get\",\"query\":{\"scope\":{\"bad\":\"object\"},\"id\":\"e1\"}}]}"
case $LAST_BODY in
    *'in query is not a string'*) okmsg 'gd: malformed later query rejected before dispatch' ;;
    *) bad "gd: expected malformed-query-scope error: $LAST_BODY" ;;
esac
admin_call_query 'gd: prevalidation prevented earlier append' 200 /get \
    "{\"scope\":\"_guarded:${TENANT_A_CAP}:events\",\"id\":\"prevalid-should-not-land\"}"
case $LAST_BODY in
    *'"hit":false'*) okmsg 'gd: no partial side effect before malformed later call' ;;
    *) bad "gd: malformed later call allowed partial append: $LAST_BODY" ;;
esac

# Token revocation flow: deleting the _tokens item for a
# capabilityID disables that token immediately on the next
# /guarded request. Distinct from the random-token reject above —
# random-token never had a _tokens entry; this test proves the
# transition from valid → invalid for a token that *was* working.
# Re-provision at the end so later tests in this section still see
# tenant A as a valid token.
admin_call 'gd: revoke tenant A token' 200 /delete \
    "{\"scope\":\"_tokens\",\"id\":\"${TENANT_A_CAP}\"}"
call 'gd: revoked tenant A token rejected' 400 POST /guarded \
    "{\"token\":\"${TENANT_A_TOKEN}\",\"calls\":[{\"path\":\"/append\",\"body\":{\"scope\":\"events\",\"id\":\"after-revoke\",\"payload\":1}}]}"
case $LAST_BODY in
    *'tenant_not_provisioned'*) okmsg 'gd: revoked token gets tenant_not_provisioned' ;;
    *) bad "gd: expected tenant_not_provisioned after revoke: $LAST_BODY" ;;
esac
admin_call_query 'gd: revoked token created no item' 200 /get \
    "{\"scope\":\"_guarded:${TENANT_A_CAP}:events\",\"id\":\"after-revoke\"}"
case $LAST_BODY in
    *'"hit":false'*) okmsg 'gd: revoked token had no side effect' ;;
    *) bad "gd: revoked token created data: $LAST_BODY" ;;
esac
admin_call 'gd: re-provision tenant A after revoke test' 200 /upsert \
    "{\"scope\":\"_tokens\",\"id\":\"${TENANT_A_CAP}\",\"payload\":{\"tenant\":\"A\",\"reissued\":true}}"

# Cross-tenant delete isolation. /guarded permits /delete and
# /delete_up_to within the tenant's own _guarded:<capID>:* prefix;
# this proves the scope-rewrite isolates deletes the same way it
# isolates reads. Both tenants append the same logical id "deliso"
# (which lives in physically different scopes after rewrite); A
# deletes; A's row is gone, B's row survives unchanged.
guarded_call 'gd: tenant A append delete-isolation item' 200 "$TENANT_A_TOKEN" /append \
    '{"scope":"events","id":"deliso","payload":{"tenant":"A"}}'
guarded_call 'gd: tenant B append delete-isolation item' 200 "$TENANT_B_TOKEN" /append \
    '{"scope":"events","id":"deliso","payload":{"tenant":"B"}}'
guarded_call 'gd: tenant A deletes own deliso' 200 "$TENANT_A_TOKEN" /delete \
    '{"scope":"events","id":"deliso"}'
guarded_call_query 'gd: tenant A deliso gone' 200 "$TENANT_A_TOKEN" /get \
    '{"scope":"events","id":"deliso"}'
case $LAST_BODY in
    *'"hit":false'*) okmsg 'gd: tenant A delete removed only A item' ;;
    *) bad "gd: tenant A deliso still exists: $LAST_BODY" ;;
esac
guarded_call_query 'gd: tenant B deliso still present' 200 "$TENANT_B_TOKEN" /get \
    '{"scope":"events","id":"deliso"}'
case $LAST_BODY in
    *'"tenant":"B"'*) okmsg 'gd: tenant B item survived tenant A delete' ;;
    *) bad "gd: tenant B item missing after tenant A delete: $LAST_BODY" ;;
esac

# Counter self-healing. The three internal counter scopes
# (_counters_count_calls, _counters_count_kb_in,
# _counters_count_kb_out) are auto-provisioned by ensureScope on the
# first /guarded call after they go missing. Documented in
# guardedflow.md §M as the recovery path after a /wipe; this test
# exercises the same path via /admin /delete_scope so a regression
# in either ensureScope (commit 165628b made it overhead-aware) or
# guardedIncrementCounters' nil-tolerant skip would surface here.
# Only the calls counter is checked after re-provisioning because
# the kb counters only fire when the request/response crosses 1 KiB,
# and a single /get stays well below that on both sides.
admin_call 'gd: delete calls counter scope' 200 /delete_scope \
    '{"scope":"_counters_count_calls"}'
admin_call 'gd: delete kb_in counter scope' 200 /delete_scope \
    '{"scope":"_counters_count_kb_in"}'
admin_call 'gd: delete kb_out counter scope' 200 /delete_scope \
    '{"scope":"_counters_count_kb_out"}'
guarded_call_query 'gd: guarded still works after counter scope delete' 200 "$TENANT_A_TOKEN" /get \
    '{"scope":"events","id":"e1"}'
admin_call_query 'gd: calls counter scope recreated' 200 /get \
    "{\"scope\":\"_counters_count_calls\",\"id\":\"${TENANT_A_CAP}\"}"
case $LAST_BODY in
    *'"hit":true'*) okmsg 'gd: calls counter recreated after delete_scope' ;;
    *) bad "gd: calls counter not recreated: $LAST_BODY" ;;
esac

# /guarded count-overflow with side-effect check. Default
# maxMultiCallCount is 10; 11 sub-calls must reject before
# dispatch — the check sits in step 3 of handleGuarded, well
# before rewriteCallScope (step 7) and the dispatch loop (step
# 8). The side-effect probe verifies the first /append never
# committed, proving the rejection is pre-dispatch and not
# "first call applied, then reject". Mirrors the existing
# /multi_call count-overflow test but on the /guarded path.
gd_11="{\"token\":\"${TENANT_A_TOKEN}\",\"calls\":["
i=1
while [ $i -le 11 ]; do
    if [ $i -gt 1 ]; then
        gd_11="${gd_11},"
    fi
    gd_11="${gd_11}{\"path\":\"/append\",\"body\":{\"scope\":\"events\",\"id\":\"too-many-$i\",\"payload\":$i}}"
    i=$((i+1))
done
gd_11="${gd_11}]}"
call 'gd: rejects over max call count' 400 POST /guarded "$gd_11"
case $LAST_BODY in
    *'maximum is 10'*) okmsg 'gd: over max call count names the cap' ;;
    *) bad "gd: expected 'maximum is 10' in error: $LAST_BODY" ;;
esac
admin_call_query 'gd: over max count created no first item' 200 /get \
    "{\"scope\":\"_guarded:${TENANT_A_CAP}:events\",\"id\":\"too-many-1\"}"
case $LAST_BODY in
    *'"hit":false'*) okmsg 'gd: over max call count had no side effect' ;;
    *) bad "gd: over max call count created data: $LAST_BODY" ;;
esac

# Two-tenant isolation: tenant B reads events and sees nothing of A's data.
guarded_call_query 'gd: tenant B isolated read' 200 "$TENANT_B_TOKEN" /get '{"scope":"events","id":"e1"}'
case $LAST_BODY in
    *'"hit":false'*) okmsg 'gd: tenant B sees no leaked tenant A data' ;;
    *) bad "gd: tenant isolation broken: $LAST_BODY" ;;
esac

# Counter auto-create: after the appends above, _counters_count_calls
# should have at least one item per active capability_id.
admin_call_query 'gd: counters auto-created' 200 /tail '{"scope":"_counters_count_calls"}'
case $LAST_BODY in
    *"\"id\":\"${TENANT_A_CAP}\""*) okmsg 'gd: tenant A counter created' ;;
    *) bad "gd: tenant A counter missing: $LAST_BODY" ;;
esac

# Counter increments per sub-call, not per HTTP request. Tenant A has so
# far made 2 single-sub-call /guarded requests (append + read back), so
# the counter sits at 2. Send one batch with 5 sub-calls plus one
# helper-style single call and verify the counter advances by exactly 6
# — proving the counter measures cache work, not HTTP request count.
admin_call_query 'gd: read counter A (before batch)' 200 /get \
    "{\"scope\":\"_counters_count_calls\",\"id\":\"${TENANT_A_CAP}\"}"
before=$(printf '%s' "$LAST_BODY" | jq -r '.item.payload')
say "  -- counter A before 5-sub-call batch: ${before}"

guarded_call 'gd: 5-sub-call batch (single)' 200 "$TENANT_A_TOKEN" /append '{"scope":"events","id":"b1","payload":1}'
# Single-call helpers send one sub-call. For a 5-sub-call batch we drop
# down to a raw POST. Quote-and-concat to keep this POSIX sh.
batch5="{\"token\":\"${TENANT_A_TOKEN}\",\"calls\":["
batch5="${batch5}{\"path\":\"/append\",\"body\":{\"scope\":\"events\",\"id\":\"b2\",\"payload\":2}},"
batch5="${batch5}{\"path\":\"/append\",\"body\":{\"scope\":\"events\",\"id\":\"b3\",\"payload\":3}},"
batch5="${batch5}{\"path\":\"/append\",\"body\":{\"scope\":\"events\",\"id\":\"b4\",\"payload\":4}},"
batch5="${batch5}{\"path\":\"/append\",\"body\":{\"scope\":\"events\",\"id\":\"b5\",\"payload\":5}},"
batch5="${batch5}{\"path\":\"/append\",\"body\":{\"scope\":\"events\",\"id\":\"b6\",\"payload\":6}}"
batch5="${batch5}]}"
call 'gd: explicit 5-sub-call batch'      200 POST /guarded "$batch5"

admin_call_query 'gd: read counter A (after batch)' 200 /get \
    "{\"scope\":\"_counters_count_calls\",\"id\":\"${TENANT_A_CAP}\"}"
after=$(printf '%s' "$LAST_BODY" | jq -r '.item.payload')
say "  -- counter A after 5-sub-call batch: ${after}"

# Reads happen via /admin (separate counter scope) so they do NOT bump
# /guarded's _counters_count_calls. Delta is therefore exactly 1 (helper
# single sub-call) + 5 (explicit batch) = 6.
expected_delta=6
actual_delta=$((after - before))
if [ "$actual_delta" -eq "$expected_delta" ]; then
    okmsg "gd: counter delta = ${actual_delta} (1 single + 5 batch sub-calls)"
else
    bad "gd: counter delta = ${actual_delta}, want ${expected_delta} (before=${before} after=${after})"
fi

# Whole-batch rejects don't increment. Send a /guarded whose whitelist
# fails (one slot is /wipe) and confirm the counter is unchanged.
admin_call_query 'gd: read counter A (before reject)' 200 /get \
    "{\"scope\":\"_counters_count_calls\",\"id\":\"${TENANT_A_CAP}\"}"
before_reject=$(printf '%s' "$LAST_BODY" | jq -r '.item.payload')
call 'gd: rejected batch (no increment)' 400 POST /guarded \
    "{\"token\":\"${TENANT_A_TOKEN}\",\"calls\":[{\"path\":\"/append\",\"body\":{\"scope\":\"events\",\"payload\":1}},{\"path\":\"/wipe\"}]}"
admin_call_query 'gd: read counter A (after reject)' 200 /get \
    "{\"scope\":\"_counters_count_calls\",\"id\":\"${TENANT_A_CAP}\"}"
after_reject=$(printf '%s' "$LAST_BODY" | jq -r '.item.payload')
if [ "$before_reject" = "$after_reject" ]; then
    okmsg "gd: whole-batch reject did not increment counter (still ${after_reject})"
else
    bad "gd: rejected batch leaked an increment: ${before_reject} -> ${after_reject}"
fi

# Missing token → 401.
call 'gd: missing token'                 401 POST /guarded \
    '{"calls":[{"path":"/get","query":{"scope":"events","id":"x"}}]}'

# GET /guarded → 405.
call 'gd: GET rejected'                  405 GET /guarded

# Malformed body → 400.
call 'gd: malformed body'                400 POST /guarded '{not-json'

# --- /delete_guarded ----------------------------------------------------------
# Operator-only namespace cleanup. Takes a capability_id (validated as
# 64 lowercase hex chars) and drops every scope under
# `_guarded:<capability_id>:*` in one atomic call. Reachable only via
# /admin — not on the public mux, not in /multi_call's whitelist, not
# in /guarded's whitelist. See scopecache-rfc.md §6.2.
#
# Strategy: two FRESH tenants (DG_A, DG_B) so the deleted_scopes /
# deleted_items counts are deterministic regardless of the state the
# /guarded section above accumulated under TENANT_A / TENANT_B.
say '== delete_guarded =='

DG_A_TOKEN="dg-tenant-A"
DG_B_TOKEN="dg-tenant-B"
DG_A_CAP=$(printf '%s' "$DG_A_TOKEN" | openssl dgst -sha256 -hmac "test-secret" -hex | sed 's/^.*= //')
DG_B_CAP=$(printf '%s' "$DG_B_TOKEN" | openssl dgst -sha256 -hmac "test-secret" -hex | sed 's/^.*= //')

admin_call 'dg: register tenant A in _tokens' 200 /upsert \
    "{\"scope\":\"_tokens\",\"id\":\"${DG_A_CAP}\",\"payload\":{\"u\":1}}"
admin_call 'dg: register tenant B in _tokens' 200 /upsert \
    "{\"scope\":\"_tokens\",\"id\":\"${DG_B_CAP}\",\"payload\":{\"u\":2}}"

# Tenant A appends to two scopes via /guarded → creates
# _guarded:DG_A_CAP:data and _guarded:DG_A_CAP:logs.
guarded_call 'dg: A appends to scope data' 200 "$DG_A_TOKEN" /append \
    '{"scope":"data","id":"x","payload":{"v":1}}'
guarded_call 'dg: A appends to scope logs' 200 "$DG_A_TOKEN" /append \
    '{"scope":"logs","id":"y","payload":{"v":2}}'

# Tenant B appends to a scope of the same NAME → creates
# _guarded:DG_B_CAP:data, which is a different store-space scope than
# A's _guarded:DG_A_CAP:data despite the visible name collision.
guarded_call 'dg: B appends to scope data' 200 "$DG_B_TOKEN" /append \
    '{"scope":"data","id":"x","payload":{"v":3}}'

# Pre-delete sanity: all three rewritten scopes are visible via
# /admin /get. The hit:true here is the baseline that the post-delete
# hit:false assertions below are measured against.
admin_call_query 'dg: A.data exists pre-delete' 200 /get \
    "{\"scope\":\"_guarded:${DG_A_CAP}:data\",\"id\":\"x\"}"
json_assert 'dg: A.data hit pre-delete' '.hit == true'
admin_call_query 'dg: A.logs exists pre-delete' 200 /get \
    "{\"scope\":\"_guarded:${DG_A_CAP}:logs\",\"id\":\"y\"}"
json_assert 'dg: A.logs hit pre-delete' '.hit == true'
admin_call_query 'dg: B.data exists pre-delete' 200 /get \
    "{\"scope\":\"_guarded:${DG_B_CAP}:data\",\"id\":\"x\"}"
json_assert 'dg: B.data hit pre-delete' '.hit == true'

# Drop tenant A's entire namespace in one /admin call.
admin_call 'dg: delete A namespace' 200 /delete_guarded \
    "{\"capability_id\":\"${DG_A_CAP}\"}"
json_assert 'dg: deleted_scopes == 2' '.deleted_scopes == 2'
json_assert 'dg: deleted_items  == 2' '.deleted_items  == 2'
json_assert 'dg: response carries freed_mb' 'has("freed_mb")'

# Post-delete: both A scopes are gone.
admin_call_query 'dg: A.data hit:false post-delete' 200 /get \
    "{\"scope\":\"_guarded:${DG_A_CAP}:data\",\"id\":\"x\"}"
json_assert 'dg: A.data miss post-delete' '.hit == false'
admin_call_query 'dg: A.logs hit:false post-delete' 200 /get \
    "{\"scope\":\"_guarded:${DG_A_CAP}:logs\",\"id\":\"y\"}"
json_assert 'dg: A.logs miss post-delete' '.hit == false'

# Critical assertion (the user's requested test): B's namespace is
# untouched even though A and B used identical scope names.
admin_call_query 'dg: B.data still exists post-delete' 200 /get \
    "{\"scope\":\"_guarded:${DG_B_CAP}:data\",\"id\":\"x\"}"
json_assert 'dg: B.data still hits' '.hit == true'
json_assert 'dg: B.data payload preserved' '.item.payload.v == 3'

# Idempotency: re-running /delete_guarded for an already-cleaned
# tenant returns deleted_scopes=0 (success, not error). Operators run
# the four-call revocation batch unconditionally; a no-op on the
# fourth call must not fail the batch.
admin_call 'dg: re-delete A is idempotent' 200 /delete_guarded \
    "{\"capability_id\":\"${DG_A_CAP}\"}"
json_assert 'dg: deleted_scopes == 0 on second run' '.deleted_scopes == 0'

# --- validation ---
admin_call 'dg: missing capability_id rejected' 400 /delete_guarded '{}'
case $LAST_BODY in
    *required*) okmsg 'dg: missing-id error mentions required' ;;
    *) bad "dg: expected 'required' error: $LAST_BODY" ;;
esac

admin_call 'dg: short capability_id rejected' 400 /delete_guarded \
    '{"capability_id":"abc"}'
case $LAST_BODY in
    *'exactly 64'*) okmsg 'dg: short-id error mentions exactly 64' ;;
    *) bad "dg: expected 'exactly 64' error: $LAST_BODY" ;;
esac

# 64 chars but uppercase. HMAC-SHA256 hex output is lowercase by
# convention, and the validator pins that — uppercase id rejected.
admin_call 'dg: uppercase capability_id rejected' 400 /delete_guarded \
    '{"capability_id":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}'
case $LAST_BODY in
    *'lowercase hex'*) okmsg 'dg: uppercase-id error mentions lowercase hex' ;;
    *) bad "dg: expected 'lowercase hex' error: $LAST_BODY" ;;
esac

# 64 chars but with one non-hex char (`g`) at position 63.
admin_call 'dg: non-hex capability_id rejected' 400 /delete_guarded \
    '{"capability_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaag"}'
case $LAST_BODY in
    *'lowercase hex'*) okmsg 'dg: non-hex-id error mentions lowercase hex' ;;
    *) bad "dg: expected 'lowercase hex' error: $LAST_BODY" ;;
esac

# --- surface enforcement: admin-only ---

# /delete_guarded MUST NOT exist on the public mux.
call 'dg: not on public mux' 404 POST /delete_guarded \
    "{\"capability_id\":\"${DG_B_CAP}\"}"

# /delete_guarded MUST NOT be in /multi_call's whitelist (would let
# any caller wipe any tenant's data without auth).
call 'dg: not in /multi_call whitelist' 400 POST /multi_call \
    "{\"calls\":[{\"path\":\"/delete_guarded\",\"body\":{\"capability_id\":\"${DG_B_CAP}\"}}]}"
case $LAST_BODY in
    *'not allowed'*) okmsg 'dg: /multi_call rejection mentions not allowed' ;;
    *) bad "dg: expected 'not allowed' error: $LAST_BODY" ;;
esac

# /delete_guarded MUST NOT be in /guarded's whitelist (a tenant
# nuking their own or anyone else's namespace is operator-only).
call 'dg: not in /guarded whitelist' 400 POST /guarded \
    "{\"token\":\"${DG_B_TOKEN}\",\"calls\":[{\"path\":\"/delete_guarded\",\"body\":{\"capability_id\":\"${DG_B_CAP}\"}}]}"
case $LAST_BODY in
    *'not allowed'*) okmsg 'dg: /guarded rejection mentions not allowed' ;;
    *) bad "dg: expected 'not allowed' error: $LAST_BODY" ;;
esac

# --- /inbox -------------------------------------------------------------------
# Shared write-only ingestion. Operator-configured scope allowlist
# (SCOPECACHE_INBOX_SCOPES = "_inbox\naudit_log"). Tenants /append into
# one shared scope; cache assigns id=<capId>:<random> and ts=now().
# Reads happen via /admin only; tenants cannot see any item.
say '== inbox =='

# Happy path: tenant A appends to _inbox.
call 'ib: append happy path'          200 POST /inbox \
    "{\"token\":\"${TENANT_A_TOKEN}\",\"scope\":\"_inbox\",\"payload\":{\"event\":\"signup\"}}"
case $LAST_BODY in
    *'"ok":true'*'"ts":'*) okmsg 'ib: response shape (ok+ts)' ;;
    *) bad "ib: response shape: $LAST_BODY" ;;
esac
case $LAST_BODY in
    *'"id":'*|*'"seq":'*|*'"scope":'*|*'"item":'*) bad "ib: response leaks identity field: $LAST_BODY" ;;
    *) okmsg 'ib: response is minimal (no id/seq/scope/item)' ;;
esac

# Drain via /admin: item exists, id starts with capA, ts populated.
admin_call_query 'ib: drain via admin /tail'    200 /tail '{"scope":"_inbox","limit":10}'
case $LAST_BODY in
    *"\"id\":\"${TENANT_A_CAP}:"*) okmsg "ib: stored id starts with tenant A's capId" ;;
    *) bad "ib: stored id missing capA prefix: $LAST_BODY" ;;
esac

# Forbidden fields rejected.
call 'ib: forbidden id'               400 POST /inbox \
    "{\"token\":\"${TENANT_A_TOKEN}\",\"scope\":\"_inbox\",\"payload\":1,\"id\":\"x\"}"
call 'ib: forbidden seq'              400 POST /inbox \
    "{\"token\":\"${TENANT_A_TOKEN}\",\"scope\":\"_inbox\",\"payload\":1,\"seq\":42}"
call 'ib: forbidden ts'               400 POST /inbox \
    "{\"token\":\"${TENANT_A_TOKEN}\",\"scope\":\"_inbox\",\"payload\":1,\"ts\":1745236800000}"

# Missing fields.
call 'ib: missing token (401)'        401 POST /inbox '{"scope":"_inbox","payload":1}'
call 'ib: missing scope (400)'        400 POST /inbox \
    "{\"token\":\"${TENANT_A_TOKEN}\",\"payload\":1}"
call 'ib: missing payload (400)'      400 POST /inbox \
    "{\"token\":\"${TENANT_A_TOKEN}\",\"scope\":\"_inbox\"}"
call 'ib: null payload (400)'         400 POST /inbox \
    "{\"token\":\"${TENANT_A_TOKEN}\",\"scope\":\"_inbox\",\"payload\":null}"

# Scope not in allowlist.
call 'ib: scope not in allowlist'     400 POST /inbox \
    "{\"token\":\"${TENANT_A_TOKEN}\",\"scope\":\"random_scope\",\"payload\":1}"

# Multiple allowed scopes — audit_log also accepts.
call 'ib: second allowed scope'       200 POST /inbox \
    "{\"token\":\"${TENANT_A_TOKEN}\",\"scope\":\"audit_log\",\"payload\":{\"event\":\"login\"}}"

# Rogue token rejected.
call 'ib: rogue token rejected'       400 POST /inbox \
    '{"token":"random-attacker","scope":"_inbox","payload":1}'
case $LAST_BODY in
    *'tenant_not_provisioned'*) okmsg 'ib: rogue token gets tenant_not_provisioned' ;;
    *) bad "ib: expected tenant_not_provisioned: $LAST_BODY" ;;
esac

# GET /inbox → 405.
call 'ib: GET rejected'               405 GET  /inbox

# Tenant cannot read what they wrote — /guarded /tail rewrites scope
# to `_guarded:<capId>:_inbox` (different scope), returns hit:false.
guarded_call_query 'ib: tenant cannot read inbox via /guarded' 200 "$TENANT_A_TOKEN" /tail '{"scope":"_inbox"}'
case $LAST_BODY in
    *'"items":[]'*|*'"items":null'*|*'"hit":false'*'"items"'*) okmsg 'ib: /guarded read of inbox empty' ;;
    *'event'*|*'signup'*|*'login'*) bad "ib: /guarded leaked inbox content: $LAST_BODY" ;;
    *) okmsg 'ib: /guarded read of inbox carries no inbox payload' ;;
esac

# --- /inbox payload cap -------------------------------------------------------
# /inbox has its own per-call payload cap (default 64 KiB,
# SCOPECACHE_MAX_INBOX_KB), tighter than the generic per-item cap.
# Reasoning lives in scopecache-rfc.md §6.4. Tests:
#   - 64 KiB (at default cap) succeeds
#   - 65 KiB (one KiB over default cap) fails 400 with cap-naming error
# The container in the e2e harness runs with the default 64 KiB cap;
# raising the cap is an env-var change at deploy time, not testable
# without a separate container.
say '== inbox payload cap =='

# JSON-encoded string payload of length 65536 = `"<65534 chars>"`.
# Build with awk for portability — POSIX printf has no `%*s`-style
# repeat, and shell-loop concatenation of 65k chars is too slow.
ib_at_cap_chars=$(awk 'BEGIN{for(i=0;i<65534;i++)printf "a"}')
ib_at_cap_payload="\"${ib_at_cap_chars}\""
quiet_call 'ib: at-cap (64 KiB) accepted' 200 POST /inbox \
    "{\"token\":\"${TENANT_A_TOKEN}\",\"scope\":\"_inbox\",\"payload\":${ib_at_cap_payload}}"

# 65 KiB payload (1 KiB over the 64 KiB default cap).
ib_over_cap_chars=$(awk 'BEGIN{for(i=0;i<66558;i++)printf "a"}')
ib_over_cap_payload="\"${ib_over_cap_chars}\""
call 'ib: over-cap (65 KiB) rejected' 400 POST /inbox \
    "{\"token\":\"${TENANT_A_TOKEN}\",\"scope\":\"_inbox\",\"payload\":${ib_over_cap_payload}}"
case $LAST_BODY in
    *'/inbox cap'*) okmsg 'ib: over-cap error names /inbox cap' ;;
    *) bad "ib: expected '/inbox cap' in error: $LAST_BODY" ;;
esac
case $LAST_BODY in
    *65536*) okmsg 'ib: over-cap error names cap value (65536)' ;;
    *) bad "ib: expected 65536 in error: $LAST_BODY" ;;
esac

# Cap-vs-allowlist ordering: an oversized payload aimed at a scope
# that is NOT in the operator's inbox-scope allowlist must produce
# the scope-misconfigured error, not the cap error. The cap check
# runs *after* the allowlist by design (so a wrong scope name gets
# the most informative error). Pre-fix or after a re-ordering
# regression this would surface "/inbox cap" instead.
call 'ib: over-cap + wrong scope -> allowlist error wins' 400 POST /inbox \
    "{\"token\":\"${TENANT_A_TOKEN}\",\"scope\":\"random_scope_not_in_allowlist\",\"payload\":${ib_over_cap_payload}}"
case $LAST_BODY in
    *'not configured as an inbox scope'*) okmsg 'ib: cap-vs-allowlist: allowlist fires first (correct)' ;;
    *'/inbox cap'*) bad "ib: cap-vs-allowlist regression: cap fired before allowlist: $LAST_BODY" ;;
    *) bad "ib: unexpected error: $LAST_BODY" ;;
esac

# Cap-vs-auth ordering: an oversized payload from a tenant whose
# token is NOT in _tokens must produce the cap error, not
# tenant_not_provisioned. The cap check runs *before* the auth
# gate by design (mirroring /guarded's pre-flight cap check) so a
# misconfigured tenant learns the real issue is request size, not
# auth. Pre-fix or after a re-ordering regression this would
# surface "tenant_not_provisioned" instead.
call 'ib: over-cap + bad token -> cap error wins' 400 POST /inbox \
    "{\"token\":\"never-provisioned-token\",\"scope\":\"_inbox\",\"payload\":${ib_over_cap_payload}}"
case $LAST_BODY in
    *'/inbox cap'*) okmsg 'ib: cap-vs-auth: cap fires first (correct)' ;;
    *'tenant_not_provisioned'*) bad "ib: cap-vs-auth regression: auth fired before cap: $LAST_BODY" ;;
    *) bad "ib: unexpected error: $LAST_BODY" ;;
esac

# --- mega deterministic state-machine test ------------------------------------
# One large end-to-end invariant test. Drives a full sequence:
#   wipe → /rebuild → 50 appends → updates → 50 second-scope appends →
#   updates → explicit deletes → extra appends → delete_up_to → range
#   delete by id list → /multi_call read-back of the entire final state →
#   /stats verification.
#
# Every step is deterministic in seq numbering, so the trailing
# json_assert can compare against fully-populated expected arrays
# instead of just count/presence. Catches regressions that:
#   - rewind seq under update/delete cycles
#   - drop one row and silently insert another
#   - leave per-scope counters out of sync with item-count
#   - leak items across scopes
#   - return mis-ordered slices from /head
#
# Uses the global quiet_call helper (defined near the top of the
# file). Earlier draft of this block redefined quiet_call inline
# without the pass=$((pass+1)) increment — leaving it in would
# silently undercount this whole section and shadow the helper for
# anything after it.
say '== mega state-machine =='

say '== mega: clean slate + rebuild =='
admin_call 'mega: wipe before state-machine' 200 /wipe

mega_rebuild_body='{"items":['
mega_rebuild_body="${mega_rebuild_body}"'{"scope":"mega_data","id":"seed1","payload":{"phase":"rebuild","n":1}},'
mega_rebuild_body="${mega_rebuild_body}"'{"scope":"mega_data","id":"seed2","payload":{"phase":"rebuild","n":2}},'
mega_rebuild_body="${mega_rebuild_body}"'{"scope":"mega_data","id":"seed3","payload":{"phase":"rebuild","n":3}},'
mega_rebuild_body="${mega_rebuild_body}"'{"scope":"mega_extra","id":"x_seed1","payload":{"phase":"rebuild","n":1}},'
mega_rebuild_body="${mega_rebuild_body}"'{"scope":"mega_extra","id":"x_seed2","payload":{"phase":"rebuild","n":2}},'
mega_rebuild_body="${mega_rebuild_body}"'{"scope":"mega_other","id":"o1","payload":{"kind":"other","n":1}},'
mega_rebuild_body="${mega_rebuild_body}"'{"scope":"mega_other","id":"o2","payload":{"kind":"other","n":2}}'
mega_rebuild_body="${mega_rebuild_body}"']}'
admin_call 'mega: rebuild seed data' 200 /rebuild "$mega_rebuild_body"

# mega_data after rebuild: seed1=1, seed2=2, seed3=3.
# Append d1..d50 → d_i = seq (3+i).
say '== mega: append 50 normal items =='
i=1
while [ $i -le 50 ]; do
    quiet_call "mega: append mega_data d$i" 200 POST /append \
        "{\"scope\":\"mega_data\",\"id\":\"d$i\",\"payload\":{\"kind\":\"data\",\"v\":$i}}"
    i=$((i+1))
done

# /update preserves seq: d10 stays seq=13, d20 stays seq=23.
say '== mega: updates on normal items =='
quiet_call 'mega: update d10' 200 POST /update \
    '{"scope":"mega_data","id":"d10","payload":{"kind":"data","v":1000,"updated":true}}'
quiet_call 'mega: update d20' 200 POST /update \
    '{"scope":"mega_data","id":"d20","payload":{"kind":"data","v":2000,"updated":true}}'

# mega_extra after rebuild: x_seed1=1, x_seed2=2.
# Append e1..e50 → e_i = seq (2+i).
say '== mega: append 50 items to second scope =='
i=1
while [ $i -le 50 ]; do
    quiet_call "mega: append mega_extra e$i" 200 POST /append \
        "{\"scope\":\"mega_extra\",\"id\":\"e$i\",\"payload\":{\"kind\":\"extra\",\"v\":$i}}"
    i=$((i+1))
done

# e20 keeps seq=22; payload overwritten by /update.
quiet_call 'mega: update e20 payload' 200 POST /update \
    '{"scope":"mega_extra","id":"e20","payload":{"kind":"extra","v":2000,"updated":true}}'

say '== mega: explicit deletes =='
quiet_call 'mega: delete mega_data d3' 200 POST /delete '{"scope":"mega_data","id":"d3"}'
quiet_call 'mega: delete mega_data d49' 200 POST /delete '{"scope":"mega_data","id":"d49"}'
quiet_call 'mega: delete mega_extra e5' 200 POST /delete '{"scope":"mega_extra","id":"e5"}'
quiet_call 'mega: delete mega_extra e49' 200 POST /delete '{"scope":"mega_extra","id":"e49"}'

# Extra appends. last_seq mega_data was 53, mega_extra was 52
# (delete + update don't bump last_seq).
say '== mega: extra appends after deletes =='
quiet_call 'mega: append mega_data d51' 200 POST /append \
    '{"scope":"mega_data","id":"d51","payload":{"kind":"data","v":51,"late":true}}'
quiet_call 'mega: append mega_data d52' 200 POST /append \
    '{"scope":"mega_data","id":"d52","payload":{"kind":"data","v":52,"late":true}}'
quiet_call 'mega: append mega_data d53' 200 POST /append \
    '{"scope":"mega_data","id":"d53","payload":{"kind":"data","v":53,"late":true}}'
quiet_call 'mega: append mega_extra e51' 200 POST /append \
    '{"scope":"mega_extra","id":"e51","payload":{"kind":"extra","v":51,"late":true}}'
quiet_call 'mega: append mega_extra e52' 200 POST /append \
    '{"scope":"mega_extra","id":"e52","payload":{"kind":"extra","v":52,"late":true}}'

# delete_up_to:
#   mega_data max_seq=5 → seed1, seed2, seed3, d1, d2 gone (d3 already gone).
#   mega_extra max_seq=4 → x_seed1, x_seed2, e1, e2 gone.
say '== mega: delete_up_to =='
quiet_call 'mega: delete_up_to mega_data seq<=5' 200 POST /delete_up_to \
    '{"scope":"mega_data","max_seq":5}'
quiet_call 'mega: delete_up_to mega_extra seq<=4' 200 POST /delete_up_to \
    '{"scope":"mega_extra","max_seq":4}'

# Range delete by id list: e12..e16 survive every prior delete; remove
# them one by one to leave a contiguous mid-range hole and confirm the
# final state is the union of (delete_up_to head) + (id-list mid hole).
say '== mega: id-list range delete =='
for id in e12 e13 e14 e15 e16; do
    quiet_call "mega: delete mid-range $id" 200 POST /delete \
        "{\"scope\":\"mega_extra\",\"id\":\"$id\"}"
done

# Expected final state, computed with jq so the assertion text
# below is one line per scope rather than three pages of literals.
mega_data_ids=$(jq -nc '[range(4;49) | "d"+tostring] + ["d50","d51","d52","d53"]')
mega_data_seqs=$(jq -nc '[range(7;52)] + [53,54,55,56]')
mega_extra_ids=$(jq -nc '
    [range(3;53)
     | select(. != 5 and . != 12 and . != 13 and . != 14 and . != 15 and . != 16 and . != 49)
     | "e"+tostring]
')
mega_extra_seqs=$(jq -nc '
    [range(3;53)
     | select(. != 5 and . != 12 and . != 13 and . != 14 and . != 15 and . != 16 and . != 49)
     | . + 2]
')

say '== mega: multi-get final state =='
mega_multi='{"calls":['
mega_multi="${mega_multi}"'{"path":"/head","query":{"scope":"mega_data","limit":100}},'
mega_multi="${mega_multi}"'{"path":"/head","query":{"scope":"mega_extra","limit":100}},'
mega_multi="${mega_multi}"'{"path":"/head","query":{"scope":"mega_other","limit":10}}'
mega_multi="${mega_multi}"']}'
call 'mega: multi-get all final scopes' 200 POST /multi_call "$mega_multi"

json_assert 'mega: multi-get outer envelope and statuses are correct' '
    .ok == true and
    .count == 3 and
    ([.results[].status] == [200,200,200])
'

# Slot 0 — /head mega_data: full id+seq array equality, plus spot
# checks on the three updated/late rows and absence of every
# explicitly-removed id.
if printf '%s' "$LAST_BODY" | jq -e \
    --argjson ids "$mega_data_ids" \
    --argjson seqs "$mega_data_seqs" '
    .results[0].body.count == 49 and
    .results[0].body.truncated == false and
    ([.results[0].body.items[].id] == $ids) and
    ([.results[0].body.items[].seq] == $seqs) and
    (all(.results[0].body.items[]; .scope == "mega_data")) and
    (all(.results[0].body.items[]; .ts > 0)) and
    (any(.results[0].body.items[]; .id == "d10" and .seq == 13 and .payload.v == 1000 and .payload.updated == true)) and
    (any(.results[0].body.items[]; .id == "d20" and .seq == 23 and .payload.v == 2000 and .payload.updated == true)) and
    (any(.results[0].body.items[]; .id == "d51" and .seq == 54 and .payload.late == true)) and
    ([.results[0].body.items[].id] | index("seed1") == null and index("seed2") == null and index("seed3") == null and index("d1") == null and index("d2") == null and index("d3") == null and index("d49") == null)
' >/dev/null; then
    okmsg 'mega: mega_data final ids, seqs, payloads, ts, deletes are exact'
else
    bad "mega: mega_data final-state mismatch: $LAST_BODY"
fi

# Slot 1 — /head mega_extra: same shape, plus the e20 update and the
# late e51/e52 entries.
if printf '%s' "$LAST_BODY" | jq -e \
    --argjson ids "$mega_extra_ids" \
    --argjson seqs "$mega_extra_seqs" '
    .results[1].body.count == 43 and
    .results[1].body.truncated == false and
    ([.results[1].body.items[].id] == $ids) and
    ([.results[1].body.items[].seq] == $seqs) and
    (all(.results[1].body.items[]; .scope == "mega_extra")) and
    (all(.results[1].body.items[]; .ts > 0)) and
    (any(.results[1].body.items[]; .id == "e20" and .seq == 22 and .payload.v == 2000 and .payload.updated == true)) and
    (any(.results[1].body.items[]; .id == "e51" and .seq == 53 and .payload.late == true)) and
    (any(.results[1].body.items[]; .id == "e52" and .seq == 54 and .payload.late == true)) and
    ([.results[1].body.items[].id] | index("x_seed1") == null and index("x_seed2") == null and index("e1") == null and index("e2") == null and index("e5") == null and index("e12") == null and index("e13") == null and index("e14") == null and index("e15") == null and index("e16") == null and index("e49") == null)
' >/dev/null; then
    okmsg 'mega: mega_extra final ids, seqs, payloads, ts, deletes are exact'
else
    bad "mega: mega_extra final-state mismatch: $LAST_BODY"
fi

# Slot 2 — /head mega_other: untouched throughout.
if printf '%s' "$LAST_BODY" | jq -e '
    .results[2].body.count == 2 and
    .results[2].body.truncated == false and
    ([.results[2].body.items[].id] == ["o1","o2"]) and
    ([.results[2].body.items[].seq] == [1,2]) and
    (all(.results[2].body.items[]; .scope == "mega_other")) and
    (all(.results[2].body.items[]; .ts > 0)) and
    ([.results[2].body.items[].payload.n] == [1,2])
' >/dev/null; then
    okmsg 'mega: mega_other untouched after whole state-machine'
else
    bad "mega: mega_other final-state mismatch: $LAST_BODY"
fi

# /stats final invariant: scope_count, total_items, and per-scope
# item_count + last_seq must all line up with the arithmetic above.
say '== mega: stats final state =='
admin_call 'mega: stats final' 200 /stats
json_assert 'mega: /stats scope_count, total_items, item_count and last_seq are exact' '
    .ok == true and
    .scope_count == 3 and
    .total_items == 94 and
    .scopes.mega_data.item_count == 49 and
    .scopes.mega_data.last_seq == 56 and
    .scopes.mega_extra.item_count == 43 and
    .scopes.mega_extra.last_seq == 54 and
    .scopes.mega_other.item_count == 2 and
    .scopes.mega_other.last_seq == 2
'

# --- wipe at end --------------------------------------------------------------
say '== final wipe =='
admin_call 'wipe'                       200 /wipe
# Body should report the scopes and items that existed just before wipe.
if printf '%s' "$LAST_BODY" | grep -q '"deleted_scopes"'; then
    okmsg 'wipe body has deleted_scopes'
else
    bad "wipe body missing deleted_scopes: $LAST_BODY"
fi
admin_call 'stats after wipe'           200 /stats
if printf '%s' "$LAST_BODY" | grep -q '"scope_count":0'; then
    okmsg 'stats shows empty store'
else
    bad "stats post-wipe: $LAST_BODY"
fi

# --- summary ------------------------------------------------------------------
printf '\n== summary ==\n'
printf 'pass: %d\nfail: %d\n' "$pass" "$fail"
[ "$fail" -eq 0 ] || exit 1
