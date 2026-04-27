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
# (head, tail, get, ts_range, stats, delete_scope_candidates) that
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
admin_call 'stats empty'                200 /stats
call 'public /stats blocked'            404 GET    /stats
call 'public /delete_scope_candidates blocked' 404 GET /delete_scope_candidates
call 'unknown route'                    404 GET    /nope
call 'wrong method on /help'            405 POST   /help

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

# append count: 10 appends to a fresh scope; /stats must report item_count:10
i=1; while [ $i -le 10 ]; do
    quiet_call "appn /append #$i" 200 POST /append "{\"scope\":\"appn\",\"id\":\"a$i\",\"payload\":$i}"
    i=$((i+1))
done
admin_call 'append count: stats'        200 /stats
json_assert 'stats: appn has 10 items' '.scopes.appn.item_count == 10'

# upsert idempotency: 5 upserts on the same id must leave exactly 1 item,
# and the surviving payload must be the last one written (4).
i=0; while [ $i -lt 5 ]; do
    quiet_call "uidem /upsert #$i" 200 POST /upsert "{\"scope\":\"uidem\",\"id\":\"only\",\"payload\":$i}"
    i=$((i+1))
done
admin_call 'upsert idem: stats'         200 /stats
json_assert 'stats: uidem has 1 item after 5 upserts' '.scopes.uidem.item_count == 1'
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

# --- ts_range filtering ------------------------------------------------------
# The top-level `ts` is client-supplied (signed int64). Items without `ts`
# must be excluded from every /ts_range response; bounds are inclusive;
# output is seq-ordered (NOT ts-ordered) because ts is mutable and non-unique.
say '== ts_range =='

# Seed a fresh scope. a..e carry ts 1000..5000; f has no ts and must never
# appear in any ts_range result.
call 'ts seed a (ts=1000)'              200 POST   /append '{"scope":"tsr","id":"a","payload":1,"ts":1000}'
call 'ts seed b (ts=2000)'              200 POST   /append '{"scope":"tsr","id":"b","payload":2,"ts":2000}'
call 'ts seed c (ts=3000)'              200 POST   /append '{"scope":"tsr","id":"c","payload":3,"ts":3000}'
call 'ts seed d (ts=4000)'              200 POST   /append '{"scope":"tsr","id":"d","payload":4,"ts":4000}'
call 'ts seed e (ts=5000)'              200 POST   /append '{"scope":"tsr","id":"e","payload":5,"ts":5000}'
call 'ts seed f (no ts)'                200 POST   /append '{"scope":"tsr","id":"f","payload":6}'

# Window [2000, 4000] inclusive: b, c, d in seq order; a, e, f excluded.
call 'ts_range [2000,4000]'             200 GET    '/ts_range?scope=tsr&since_ts=2000&until_ts=4000'
case $LAST_BODY in
    *'"id":"b"'*'"id":"c"'*'"id":"d"'*) okmsg 'ts_range [2000,4000]: b,c,d in seq order' ;;
    *) bad "ts_range window: $LAST_BODY" ;;
esac
case $LAST_BODY in
    *'"id":"a"'*|*'"id":"e"'*|*'"id":"f"'*) bad "ts_range leaked out-of-window ids: $LAST_BODY" ;;
    *) okmsg 'ts_range: a,e,f absent' ;;
esac
case $LAST_BODY in
    *'"count":3'*'"truncated":false'*) okmsg 'ts_range count=3, truncated=false' ;;
    *) bad "ts_range count/truncated: $LAST_BODY" ;;
esac

# Only since_ts: everything from 3000 up (c, d, e). f (no ts) must stay out.
call 'ts_range since_ts=3000'           200 GET    '/ts_range?scope=tsr&since_ts=3000'
case $LAST_BODY in
    *'"id":"c"'*'"id":"d"'*'"id":"e"'*) okmsg 'ts_range since_ts=3000: c,d,e present' ;;
    *) bad "ts_range since_ts only: $LAST_BODY" ;;
esac
case $LAST_BODY in
    *'"id":"f"'*) bad "ts_range since_ts leaked no-ts item f: $LAST_BODY" ;;
    *) okmsg 'ts_range since_ts: no-ts item (f) excluded' ;;
esac

# Only until_ts: everything up to and including 2000 (a, b).
call 'ts_range until_ts=2000'           200 GET    '/ts_range?scope=tsr&until_ts=2000'
# Envelope orders count before items, so check count first, then id order.
case $LAST_BODY in
    *'"count":2'*'"id":"a"'*'"id":"b"'*) okmsg 'ts_range until_ts=2000: a,b (count=2)' ;;
    *) bad "ts_range until_ts only: $LAST_BODY" ;;
esac
case $LAST_BODY in
    *'"id":"c"'*|*'"id":"d"'*|*'"id":"e"'*|*'"id":"f"'*) bad "ts_range until_ts leaked: $LAST_BODY" ;;
    *) okmsg 'ts_range until_ts: c,d,e,f excluded' ;;
esac

# Degenerate window [2000, 2000] — boundary inclusivity check. Must be
# exactly b, and nothing else.
call 'ts_range [2000,2000] (inclusive)' 200 GET    '/ts_range?scope=tsr&since_ts=2000&until_ts=2000'
case $LAST_BODY in
    *'"count":1'*'"id":"b"'*) okmsg 'ts_range [2000,2000]: exactly b (inclusive on both ends)' ;;
    *) bad "ts_range inclusive: $LAST_BODY" ;;
esac

# Truncation: window spans a..e (5 matches) but limit=2 caps at 2 and flags it.
call 'ts_range truncated'               200 GET    '/ts_range?scope=tsr&since_ts=1000&limit=2'
case $LAST_BODY in
    *'"count":2'*'"truncated":true'*) okmsg 'ts_range limit=2: count=2, truncated=true' ;;
    *) bad "ts_range truncated: $LAST_BODY" ;;
esac

# Non-existent scope returns 200 with hit:false, count:0, truncated:false.
call 'ts_range missing scope'           200 GET    '/ts_range?scope=tsr_nope&since_ts=0'
case $LAST_BODY in
    *'"hit":false'*'"count":0'*'"truncated":false'*) okmsg 'ts_range missing scope: empty result envelope' ;;
    *) bad "ts_range missing scope: $LAST_BODY" ;;
esac

# Negative and zero ts are legal int64 values; window must span them correctly.
call 'ts seed neg (ts=-5000)'           200 POST   /append '{"scope":"tsr_neg","id":"n1","payload":1,"ts":-5000}'
call 'ts seed zero (ts=0)'              200 POST   /append '{"scope":"tsr_neg","id":"n2","payload":2,"ts":0}'
call 'ts_range spans negative..0'       200 GET    '/ts_range?scope=tsr_neg&since_ts=-10000&until_ts=0'
case $LAST_BODY in
    *'"count":2'*'"id":"n1"'*'"id":"n2"'*) okmsg 'ts_range: negative and zero ts both present' ;;
    *) bad "ts_range negative: $LAST_BODY" ;;
esac

# /update with an explicit ts overwrites; without a ts field it preserves.
call 'ts /update overwrites ts'         200 POST   /update '{"scope":"tsr","id":"a","payload":1,"ts":9999}'
call 'ts_range sees a at 9999'          200 GET    '/ts_range?scope=tsr&since_ts=9000&until_ts=10000'
case $LAST_BODY in
    *'"id":"a"'*'"ts":9999'*) okmsg 'ts_range: /update ts=9999 overwrote a' ;;
    *) bad "ts /update overwrite: $LAST_BODY" ;;
esac
call 'ts /update without ts preserves'  200 POST   /update '{"scope":"tsr","id":"a","payload":1}'
call 'ts_range still sees a at 9999'    200 GET    '/ts_range?scope=tsr&since_ts=9000&until_ts=10000'
case $LAST_BODY in
    *'"id":"a"'*'"ts":9999'*) okmsg 'ts_range: /update without ts preserved 9999' ;;
    *) bad "ts /update preserve: $LAST_BODY" ;;
esac

# /upsert without ts CLEARS it (whole-item replace semantics) — a must drop
# out of the [9000, 10000] window after the upsert.
call 'ts /upsert without ts clears'     200 POST   /upsert '{"scope":"tsr","id":"a","payload":1}'
call 'ts_range after upsert-no-ts'      200 GET    '/ts_range?scope=tsr&since_ts=9000&until_ts=10000'
case $LAST_BODY in
    *'"id":"a"'*) bad "ts /upsert: a should no longer have ts: $LAST_BODY" ;;
    *) okmsg 'ts_range: /upsert without ts cleared a from window' ;;
esac

# Validation (400)
call 'ts_range: missing both bounds'    400 GET    '/ts_range?scope=tsr'
call 'ts_range: inverted bounds'        400 GET    '/ts_range?scope=tsr&since_ts=5000&until_ts=1000'
call 'ts_range: non-numeric since_ts'   400 GET    '/ts_range?scope=tsr&since_ts=notanumber'
call 'ts_range: non-integer until_ts'   400 GET    '/ts_range?scope=tsr&until_ts=1.5'

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
call 'public /ts_range on _* blocked' 400 GET    '/ts_range?scope=_tokens&since_ts=0'
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

# /admin /stats sees the _tokens scope.
admin_call 'admin: stats sees _tokens' 200 /stats
case $LAST_BODY in
    *'_tokens'*) okmsg 'admin: _tokens scope visible in stats' ;;
    *) bad "admin stats body missing _tokens: $LAST_BODY" ;;
esac

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

# Counter self-healing. The two internal counter scopes
# (_counters_count_calls, _counters_count_kb) are auto-provisioned
# by ensureScope on the first /guarded call after they go missing.
# Documented in guardedflow.md §M as the recovery path after a
# /wipe; this test exercises the same path via /admin
# /delete_scope so a regression in either ensureScope (commit
# 165628b made it overhead-aware) or guardedIncrementCounters'
# nil-tolerant skip would surface here. Only the calls counter is
# checked because the kb counter only fires when the response
# crosses 1 KiB, and a single /get response stays well below that.
admin_call 'gd: delete calls counter scope' 200 /delete_scope \
    '{"scope":"_counters_count_calls"}'
admin_call 'gd: delete kb counter scope' 200 /delete_scope \
    '{"scope":"_counters_count_kb"}'
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

# --- mega deterministic state-machine test ------------------------------------
# One large end-to-end invariant test. Drives a full sequence:
#   wipe → /rebuild → 50 appends → updates → 50 ts-appends → updates →
#   explicit deletes → extra appends → delete_up_to → ts-range read+delete →
#   /multi_call read-back of the entire final state → /stats verification.
#
# Every step is deterministic in seq numbering, so the trailing
# json_assert can compare against fully-populated expected arrays
# instead of just count/presence. Catches regressions that:
#   - rewind seq under update/delete cycles
#   - drop one row and silently insert another
#   - leave per-scope counters out of sync with item-count
#   - fail to refresh ts-index on /update of an existing item
#   - leak items across scopes
#   - return mis-ordered slices from /head or /ts_range
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
mega_rebuild_body="${mega_rebuild_body}"'{"scope":"mega_ts","id":"ts_seed1","payload":{"phase":"rebuild","n":1},"ts":1000},'
mega_rebuild_body="${mega_rebuild_body}"'{"scope":"mega_ts","id":"ts_seed2","payload":{"phase":"rebuild","n":2},"ts":2000},'
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

# mega_ts after rebuild: ts_seed1=1, ts_seed2=2.
# Append t1..t50 → t_i = seq (2+i), ts = 10000 + i*100.
say '== mega: append 50 ts items =='
i=1
while [ $i -le 50 ]; do
    ts=$((10000 + i * 100))
    quiet_call "mega: append mega_ts t$i" 200 POST /append \
        "{\"scope\":\"mega_ts\",\"id\":\"t$i\",\"payload\":{\"kind\":\"ts\",\"v\":$i},\"ts\":$ts}"
    i=$((i+1))
done

# t20 keeps seq=22; payload + ts both overwritten by /update.
quiet_call 'mega: update t20 payload + ts' 200 POST /update \
    '{"scope":"mega_ts","id":"t20","payload":{"kind":"ts","v":2000,"updated":true},"ts":77777}'

say '== mega: explicit deletes =='
quiet_call 'mega: delete mega_data d3' 200 POST /delete '{"scope":"mega_data","id":"d3"}'
quiet_call 'mega: delete mega_data d49' 200 POST /delete '{"scope":"mega_data","id":"d49"}'
quiet_call 'mega: delete mega_ts t5' 200 POST /delete '{"scope":"mega_ts","id":"t5"}'
quiet_call 'mega: delete mega_ts t49' 200 POST /delete '{"scope":"mega_ts","id":"t49"}'

# Extra appends. last_seq mega_data was 53, mega_ts was 52
# (delete + update don't bump last_seq).
say '== mega: extra appends after deletes =='
quiet_call 'mega: append mega_data d51' 200 POST /append \
    '{"scope":"mega_data","id":"d51","payload":{"kind":"data","v":51,"late":true}}'
quiet_call 'mega: append mega_data d52' 200 POST /append \
    '{"scope":"mega_data","id":"d52","payload":{"kind":"data","v":52,"late":true}}'
quiet_call 'mega: append mega_data d53' 200 POST /append \
    '{"scope":"mega_data","id":"d53","payload":{"kind":"data","v":53,"late":true}}'
quiet_call 'mega: append mega_ts t51' 200 POST /append \
    '{"scope":"mega_ts","id":"t51","payload":{"kind":"ts","v":51,"late":true},"ts":20000}'
quiet_call 'mega: append mega_ts t52' 200 POST /append \
    '{"scope":"mega_ts","id":"t52","payload":{"kind":"ts","v":52,"late":true},"ts":20100}'

# delete_up_to:
#   mega_data max_seq=5 → seed1, seed2, seed3, d1, d2 gone (d3 already gone).
#   mega_ts max_seq=4 → ts_seed1, ts_seed2, t1, t2 gone.
say '== mega: delete_up_to =='
quiet_call 'mega: delete_up_to mega_data seq<=5' 200 POST /delete_up_to \
    '{"scope":"mega_data","max_seq":5}'
quiet_call 'mega: delete_up_to mega_ts seq<=4' 200 POST /delete_up_to \
    '{"scope":"mega_ts","max_seq":4}'

# /ts_range range delete: original t12..t16 had ts 11200..11600 and
# survive every prior delete. Read first to assert the exact range
# result, then delete by id (no delete-by-ts endpoint).
say '== mega: delete by deterministic ts_range =='
call 'mega: ts_range delete-window before delete' 200 GET \
    '/ts_range?scope=mega_ts&since_ts=11200&until_ts=11600&limit=100'
json_assert 'mega: ts_range delete-window exactly t12..t16' '
    .count == 5 and
    [.items[].id] == ["t12","t13","t14","t15","t16"] and
    [.items[].seq] == [14,15,16,17,18] and
    [.items[].ts] == [11200,11300,11400,11500,11600]
'
for id in t12 t13 t14 t15 t16; do
    quiet_call "mega: delete ts-window $id" 200 POST /delete \
        "{\"scope\":\"mega_ts\",\"id\":\"$id\"}"
done

# Expected final state, computed with jq so the assertion text
# below is one line per scope rather than three pages of literals.
mega_data_ids=$(jq -nc '[range(4;49) | "d"+tostring] + ["d50","d51","d52","d53"]')
mega_data_seqs=$(jq -nc '[range(7;52)] + [53,54,55,56]')
mega_ts_ids=$(jq -nc '
    [range(3;53)
     | select(. != 5 and . != 12 and . != 13 and . != 14 and . != 15 and . != 16 and . != 49)
     | "t"+tostring]
')
mega_ts_seqs=$(jq -nc '
    [range(3;53)
     | select(. != 5 and . != 12 and . != 13 and . != 14 and . != 15 and . != 16 and . != 49)
     | . + 2]
')

say '== mega: multi-get final state =='
# Slots 5 + 6 prove ts-bounds invariants (no item below 10300, no
# item above 77777). The cache does NOT expose oldest_ts/newest_ts
# on /stats — those are derived properties, not tracked aggregates
# — so two empty /ts_range queries outside the expected window are
# the canonical way to assert it.
mega_multi='{"calls":['
mega_multi="${mega_multi}"'{"path":"/head","query":{"scope":"mega_data","limit":100}},'
mega_multi="${mega_multi}"'{"path":"/head","query":{"scope":"mega_ts","limit":100}},'
mega_multi="${mega_multi}"'{"path":"/head","query":{"scope":"mega_other","limit":10}},'
mega_multi="${mega_multi}"'{"path":"/ts_range","query":{"scope":"mega_ts","since_ts":11200,"until_ts":11600,"limit":100}},'
mega_multi="${mega_multi}"'{"path":"/ts_range","query":{"scope":"mega_ts","since_ts":77777,"until_ts":77777,"limit":10}},'
mega_multi="${mega_multi}"'{"path":"/ts_range","query":{"scope":"mega_ts","since_ts":0,"until_ts":10299,"limit":10}},'
mega_multi="${mega_multi}"'{"path":"/ts_range","query":{"scope":"mega_ts","since_ts":77778,"until_ts":99999999,"limit":10}}'
mega_multi="${mega_multi}"']}'
call 'mega: multi-get all final scopes' 200 POST /multi_call "$mega_multi"

json_assert 'mega: multi-get outer envelope and statuses are correct' '
    .ok == true and
    .count == 7 and
    ([.results[].status] == [200,200,200,200,200,200,200])
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
    (any(.results[0].body.items[]; .id == "d10" and .seq == 13 and .payload.v == 1000 and .payload.updated == true)) and
    (any(.results[0].body.items[]; .id == "d20" and .seq == 23 and .payload.v == 2000 and .payload.updated == true)) and
    (any(.results[0].body.items[]; .id == "d51" and .seq == 54 and .payload.late == true)) and
    ([.results[0].body.items[].id] | index("seed1") == null and index("seed2") == null and index("seed3") == null and index("d1") == null and index("d2") == null and index("d3") == null and index("d49") == null)
' >/dev/null; then
    okmsg 'mega: mega_data final ids, seqs, payloads and deletes are exact'
else
    bad "mega: mega_data final-state mismatch: $LAST_BODY"
fi

# Slot 1 — /head mega_ts: same shape, plus the t20 ts/payload
# overwrite and the late t51/t52 entries.
if printf '%s' "$LAST_BODY" | jq -e \
    --argjson ids "$mega_ts_ids" \
    --argjson seqs "$mega_ts_seqs" '
    .results[1].body.count == 43 and
    .results[1].body.truncated == false and
    ([.results[1].body.items[].id] == $ids) and
    ([.results[1].body.items[].seq] == $seqs) and
    (all(.results[1].body.items[]; .scope == "mega_ts")) and
    (any(.results[1].body.items[]; .id == "t20" and .seq == 22 and .ts == 77777 and .payload.v == 2000 and .payload.updated == true)) and
    (any(.results[1].body.items[]; .id == "t51" and .seq == 53 and .ts == 20000 and .payload.late == true)) and
    (any(.results[1].body.items[]; .id == "t52" and .seq == 54 and .ts == 20100 and .payload.late == true)) and
    ([.results[1].body.items[].id] | index("ts_seed1") == null and index("ts_seed2") == null and index("t1") == null and index("t2") == null and index("t5") == null and index("t12") == null and index("t13") == null and index("t14") == null and index("t15") == null and index("t16") == null and index("t49") == null)
' >/dev/null; then
    okmsg 'mega: mega_ts final ids, seqs, ts, payloads and deletes are exact'
else
    bad "mega: mega_ts final-state mismatch: $LAST_BODY"
fi

# Slot 2 — /head mega_other: untouched throughout.
if printf '%s' "$LAST_BODY" | jq -e '
    .results[2].body.count == 2 and
    .results[2].body.truncated == false and
    ([.results[2].body.items[].id] == ["o1","o2"]) and
    ([.results[2].body.items[].seq] == [1,2]) and
    (all(.results[2].body.items[]; .scope == "mega_other")) and
    ([.results[2].body.items[].payload.n] == [1,2])
' >/dev/null; then
    okmsg 'mega: mega_other untouched after whole state-machine'
else
    bad "mega: mega_other final-state mismatch: $LAST_BODY"
fi

# Slot 3 — /ts_range over the deleted window: must be empty.
if printf '%s' "$LAST_BODY" | jq -e '
    .results[3].body.count == 0 and
    .results[3].body.items == [] and
    .results[3].body.truncated == false
' >/dev/null; then
    okmsg 'mega: deleted ts_range [11200,11600] is now empty'
else
    bad "mega: deleted ts_range still contains data: $LAST_BODY"
fi

# Slot 4 — /ts_range at the t20-overwrite ts: proves /update
# refreshed the ts index, not just the payload.
if printf '%s' "$LAST_BODY" | jq -e '
    .results[4].body.count == 1 and
    .results[4].body.items[0].id == "t20" and
    .results[4].body.items[0].seq == 22 and
    .results[4].body.items[0].ts == 77777 and
    .results[4].body.items[0].payload.updated == true
' >/dev/null; then
    okmsg 'mega: updated t20 is reachable at new ts=77777'
else
    bad "mega: updated t20 ts lookup mismatch: $LAST_BODY"
fi

# Slot 5 — ts-bounds floor: nothing in mega_ts has ts < 10300.
# (delete_up_to removed t1 and t2 with ts 10100/10200; t3 at ts
# 10300 should be the oldest survivor.)
if printf '%s' "$LAST_BODY" | jq -e '
    .results[5].body.count == 0 and
    .results[5].body.items == []
' >/dev/null; then
    okmsg 'mega: no items below ts=10300 (oldest survivor is t3)'
else
    bad "mega: items leaked below ts=10300: $LAST_BODY"
fi

# Slot 6 — ts-bounds ceiling: nothing in mega_ts has ts > 77777.
# (t20's /update bumped its ts to 77777; t52's late append used
# ts=20100; nothing else got a higher ts.)
if printf '%s' "$LAST_BODY" | jq -e '
    .results[6].body.count == 0 and
    .results[6].body.items == []
' >/dev/null; then
    okmsg 'mega: no items above ts=77777 (newest is updated t20)'
else
    bad "mega: items leaked above ts=77777: $LAST_BODY"
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
    .scopes.mega_ts.item_count == 43 and
    .scopes.mega_ts.last_seq == 54 and
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
