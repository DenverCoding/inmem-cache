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
# The script fails fast on the first unexpected status code or body shape.

set -eu

SOCK=${SOCK-/run/scopecache.sock}
BASE=${BASE:-http://localhost}

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
        curl -s -o /tmp/body -w '%{http_code}' $_sockargs \
             -X "$_method" -H 'Content-Type: application/json' \
             -d "$_body" "$BASE$_path"
    else
        # shellcheck disable=SC2086
        curl -s -o /tmp/body -w '%{http_code}' $_sockargs \
             -X "$_method" "$BASE$_path"
    fi
    printf '\n'
    cat /tmp/body
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

# admin_call_query: like admin_call but for GET-style sub-calls (head,
# tail, get, ts_range, render, stats) that take a query map instead of
# a body. The query argument is a JSON object string.
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
call 'delete_scope_candidates'          200 GET    /delete_scope_candidates

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
    req POST /counter_add '{"scope":"cmath","id":"n","by":1}'  >/dev/null
    i=$((i+1))
done
i=0; while [ $i -lt 3 ]; do
    req POST /counter_add '{"scope":"cmath","id":"n","by":-1}' >/dev/null
    i=$((i+1))
done
call 'counter: read final'              200 GET    '/get?scope=cmath&id=n'
case $LAST_BODY in
    *'"payload":7'*) okmsg 'counter 10x(+1) + 3x(-1) == 7' ;;
    *) bad "counter math body: $LAST_BODY" ;;
esac

# delete_up_to: 10 appends, trim up to seq 6, only t7..t10 must survive
i=1; while [ $i -le 10 ]; do
    req POST /append "{\"scope\":\"tmath\",\"id\":\"t$i\",\"payload\":$i}" >/dev/null
    i=$((i+1))
done
call 'trim: delete_up_to seq=6'         200 POST   /delete_up_to '{"scope":"tmath","max_seq":6}'
call 'trim: head after trim'            200 GET    '/head?scope=tmath'
case $LAST_BODY in
    *'"id":"t7"'*'"id":"t10"'*) okmsg 'trim: t7..t10 still present' ;;
    *) bad "trim: expected t7..t10 in body: $LAST_BODY" ;;
esac
case $LAST_BODY in
    *'"id":"t1"'*|*'"id":"t6"'*) bad "trim: stale ids leaked: $LAST_BODY" ;;
    *) okmsg 'trim: t1..t6 are gone' ;;
esac

# append count: 10 appends to a fresh scope; /stats must report item_count:10
i=1; while [ $i -le 10 ]; do
    req POST /append "{\"scope\":\"appn\",\"id\":\"a$i\",\"payload\":$i}" >/dev/null
    i=$((i+1))
done
call 'append count: stats'              200 GET    /stats
case $LAST_BODY in
    *'"appn"'*'"item_count":10'*) okmsg 'stats: appn has 10 items' ;;
    *) bad "append count stats: $LAST_BODY" ;;
esac

# upsert idempotency: 5 upserts on the same id must leave exactly 1 item,
# and the surviving payload must be the last one written (4).
i=0; while [ $i -lt 5 ]; do
    req POST /upsert "{\"scope\":\"uidem\",\"id\":\"only\",\"payload\":$i}" >/dev/null
    i=$((i+1))
done
call 'upsert idem: stats'               200 GET    /stats
case $LAST_BODY in
    *'"uidem"'*'"item_count":1'*) okmsg 'stats: uidem has 1 item after 5 upserts' ;;
    *) bad "upsert idem stats: $LAST_BODY" ;;
esac
call 'upsert idem: final value'         200 GET    '/get?scope=uidem&id=only'
case $LAST_BODY in
    *'"payload":4'*) okmsg 'upsert idem: final payload is 4' ;;
    *) bad "upsert idem final: $LAST_BODY" ;;
esac

# tail windowing: 10 appends (seq 1..10). limit=5 is the newest slice (t6..t10);
# limit=5&offset=5 skips that newest slice and returns the previous one (t1..t5).
i=1; while [ $i -le 10 ]; do
    req POST /append "{\"scope\":\"tail10\",\"id\":\"t$i\",\"payload\":$i}" >/dev/null
    i=$((i+1))
done
call 'tail limit=5 (newest)'            200 GET    '/tail?scope=tail10&limit=5'
case $LAST_BODY in
    *'"id":"t6"'*'"id":"t10"'*) okmsg 'tail newest: t6..t10 present (ids)' ;;
    *) bad "tail newest: $LAST_BODY" ;;
esac
case $LAST_BODY in
    *'"id":"t1"'*|*'"id":"t5"'*) bad "tail newest leaked older ids: $LAST_BODY" ;;
    *) okmsg 'tail newest: t1..t5 absent (ids)' ;;
esac
# seq check: trailing comma prevents "seq":1 from matching "seq":10 etc.
case $LAST_BODY in
    *'"seq":6,'*'"seq":10,'*) okmsg 'tail newest: seq 6..10 present' ;;
    *) bad "tail newest seq: $LAST_BODY" ;;
esac
case $LAST_BODY in
    *'"seq":1,'*|*'"seq":5,'*) bad "tail newest leaked older seqs: $LAST_BODY" ;;
    *) okmsg 'tail newest: seq 1..5 absent' ;;
esac

call 'tail limit=5 offset=5 (oldest)'   200 GET    '/tail?scope=tail10&limit=5&offset=5'
case $LAST_BODY in
    *'"id":"t1"'*'"id":"t5"'*) okmsg 'tail offset=5: t1..t5 present (ids)' ;;
    *) bad "tail offset=5: $LAST_BODY" ;;
esac
case $LAST_BODY in
    *'"id":"t6"'*|*'"id":"t10"'*) bad "tail offset=5 leaked newer ids: $LAST_BODY" ;;
    *) okmsg 'tail offset=5: t6..t10 absent (ids)' ;;
esac
case $LAST_BODY in
    *'"seq":1,'*'"seq":5,'*) okmsg 'tail offset=5: seq 1..5 present' ;;
    *) bad "tail offset=5 seq: $LAST_BODY" ;;
esac
case $LAST_BODY in
    *'"seq":6,'*|*'"seq":10,'*) bad "tail offset=5 leaked newer seqs: $LAST_BODY" ;;
    *) okmsg 'tail offset=5: seq 6..10 absent' ;;
esac

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
# dispatch (not parallel, not snapshot-isolated).
call 'mc: mixed read/write/read'        200 POST   /multi_call \
    '{"calls":[{"path":"/append","body":{"scope":"mc","id":"a","payload":{"v":1}}},{"path":"/get","query":{"scope":"mc","id":"a"}},{"path":"/tail","query":{"scope":"mc","limit":10}},{"path":"/stats"}]}'
case $LAST_BODY in
    *'"ok":true'*'"count":4'*) okmsg 'mc: outer ok=true, count=4' ;;
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
# /stats sub-call must see the new scope as item_count:1 — proves writes
# in earlier slots are visible to /stats in a later slot.
case $LAST_BODY in
    *'"mc"'*'"item_count":1'*) okmsg 'mc: /stats slot reflects in-batch write' ;;
    *) bad "mc stats slot: $LAST_BODY" ;;
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

# Each excluded endpoint individually: /warm, /rebuild, /wipe, /render,
# /help, /multi_call self-reference — all 400.
call 'mc: exclude /warm'                400 POST   /multi_call '{"calls":[{"path":"/warm","body":{"items":[]}}]}'
call 'mc: exclude /rebuild'             400 POST   /multi_call '{"calls":[{"path":"/rebuild","body":{"items":[]}}]}'
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
    req POST /append "{\"scope\":\"mcq\",\"id\":\"q$i\",\"payload\":$i}" >/dev/null
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

# /admin can write to reserved scopes (provisions a tenant namespace).
admin_call 'admin: provision tenant scope' 200 /upsert '{"scope":"_guarded:capX:events","id":"_provisioned","payload":{"t":1}}'

# /admin /stats sees the provisioned scope.
admin_call 'admin: stats sees provisioned' 200 /stats
case $LAST_BODY in
    *'_guarded:capX:events'*) okmsg 'admin: provisioned scope visible in stats' ;;
    *) bad "admin stats body missing scope: $LAST_BODY" ;;
esac

# /admin's whitelist excludes self-reference, /multi_call, /guarded, /help.
call 'admin: rejects /admin in calls' 400 POST /admin '{"calls":[{"path":"/admin"}]}'
call 'admin: rejects /multi_call'     400 POST /admin '{"calls":[{"path":"/multi_call","body":{"calls":[]}}]}'
call 'admin: rejects /guarded'        400 POST /admin '{"calls":[{"path":"/guarded","body":{"token":"x","calls":[]}}]}'
call 'admin: rejects /help'           400 POST /admin '{"calls":[{"path":"/help"}]}'

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

admin_call 'gd: provision tenant A scope' 200 /upsert "{\"scope\":\"_guarded:${TENANT_A_CAP}:events\",\"id\":\"_provisioned\",\"payload\":{\"t\":1}}"
admin_call 'gd: provision tenant B scope' 200 /upsert "{\"scope\":\"_guarded:${TENANT_B_CAP}:events\",\"id\":\"_provisioned\",\"payload\":{\"t\":1}}"

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

# Random-token attack: forged token's HMAC names a non-existent scope →
# whole-batch reject with scope_not_provisioned.
call 'gd: random token rejected'         400 POST /guarded \
    '{"token":"random-attacker","calls":[{"path":"/append","body":{"scope":"events","payload":"junk"}}]}'
case $LAST_BODY in
    *'not provisioned'*) okmsg 'gd: scope_not_provisioned error returned' ;;
    *) bad "gd: expected scope_not_provisioned: $LAST_BODY" ;;
esac

# Side-effect-free reject: a legit tenant trying to write to a scope
# they never had provisioned must NOT cause the cache to lazily create
# the scope. This guards against a future regression that swaps the
# pre-dispatch existence check from getScope() (read-only) to
# ensureScope() (creates) — that swap would let any token holder fill
# the store with empty buffers by spamming non-provisioned scope names.
guarded_call 'gd: write to unprovisioned scope rejected' 400 "$TENANT_A_TOKEN" /append \
    '{"scope":"ghosts","id":"x","payload":1}'
case $LAST_BODY in
    *'not provisioned'*) okmsg 'gd: ghosts scope rejected with scope_not_provisioned' ;;
    *) bad "gd: expected scope_not_provisioned for ghosts: $LAST_BODY" ;;
esac
admin_call_query 'gd: post-reject scope still absent' 200 /get \
    "{\"scope\":\"_guarded:${TENANT_A_CAP}:ghosts\",\"id\":\"x\"}"
case $LAST_BODY in
    *'"hit":false'*) okmsg 'gd: rejected /append did not lazily create the scope' ;;
    *) bad "gd: rejected /append leaked a side effect: $LAST_BODY" ;;
esac

# Whitelist enforcement: /wipe inside /guarded is rejected.
call 'gd: rejects /wipe sub-call'         400 POST /guarded \
    "{\"token\":\"${TENANT_A_TOKEN}\",\"calls\":[{\"path\":\"/wipe\"}]}"

# Whitelist enforcement: /delete_scope inside /guarded is rejected.
call 'gd: rejects /delete_scope sub-call' 400 POST /guarded \
    "{\"token\":\"${TENANT_A_TOKEN}\",\"calls\":[{\"path\":\"/delete_scope\",\"body\":{\"scope\":\"events\"}}]}"

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

# --- wipe at end --------------------------------------------------------------
say '== final wipe =='
admin_call 'wipe'                       200 /wipe
# Body should report the scopes and items that existed just before wipe.
if printf '%s' "$LAST_BODY" | grep -q '"deleted_scopes"'; then
    okmsg 'wipe body has deleted_scopes'; pass=$((pass+1))
else
    bad "wipe body missing deleted_scopes: $LAST_BODY"
fi
call 'stats after wipe'                 200 GET    /stats
if printf '%s' "$LAST_BODY" | grep -q '"scope_count":0'; then
    okmsg 'stats shows empty store'; pass=$((pass+1))
else
    bad "stats post-wipe: $LAST_BODY"
fi

# --- summary ------------------------------------------------------------------
printf '\n== summary ==\n'
printf 'pass: %d\nfail: %d\n' "$pass" "$fail"
[ "$fail" -eq 0 ] || exit 1
