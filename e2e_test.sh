#!/bin/sh
# End-to-end test suite: exercises every scopecache endpoint over the real
# Unix socket. Intended to run inside the `dev` container (which shares the
# /run volume with the `scopecache` service) so the socket path is just
# /run/scopecache.sock.
#
# Usage (from the host):
#   docker compose up -d --build scopecache
#   docker compose exec dev sh /src/e2e_test.sh
#
# The script fails fast on the first unexpected status code or body shape.

set -eu

SOCK=${SOCK:-/run/scopecache.sock}
BASE=http://localhost

pass=0
fail=0

say()   { printf '%s\n' "$*"; }
okmsg() { pass=$((pass+1)); printf '  ok   %s\n' "$*"; }
bad()   { fail=$((fail+1)); printf '  FAIL %s\n' "$*"; }

# req METHOD PATH [BODY]
# Prints "<status>\n<body>" so callers can `read` them separately.
req() {
    _method=$1; _path=$2; _body=${3:-}
    if [ -n "$_body" ]; then
        curl -s -o /tmp/body -w '%{http_code}' --unix-socket "$SOCK" \
             -X "$_method" -H 'Content-Type: application/json' \
             -d "$_body" "$BASE$_path"
    else
        curl -s -o /tmp/body -w '%{http_code}' --unix-socket "$SOCK" \
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

# --- start clean ---------------------------------------------------------------
say '== wipe for clean slate =='
call 'wipe initial'                     200 POST   /wipe

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
call 'warm'  200 POST /warm '{"items":[{"scope":"warm1","id":"a","payload":"A"},{"scope":"warm1","id":"b","payload":"B"},{"scope":"warm2","payload":1}]}'
call 'rebuild' 200 POST /rebuild '{"items":[{"scope":"only","id":"one","payload":{"k":"v"}}]}'

# After /rebuild the previous scopes are gone. /get still envelopes misses in
# a 200 with "hit":false (only /render returns 404).
call 'post-rebuild: old scope gone'     200 GET    '/get?scope=s&id=a'
case $LAST_BODY in
    *'"hit":false'*) okmsg 'post-rebuild old scope: "hit":false' ;;
    *) bad "post-rebuild old scope body: $LAST_BODY" ;;
esac
call 'post-rebuild: new scope reads'    200 GET    '/get?scope=only&id=one'

# --- candidates / delete-up-to / delete / delete-scope ------------------------
say '== deletes =='
call 'append bulk for trim'  200 POST /append '{"scope":"trim","id":"a","payload":1}'
call 'append bulk for trim'  200 POST /append '{"scope":"trim","id":"b","payload":2}'
call 'append bulk for trim'  200 POST /append '{"scope":"trim","id":"c","payload":3}'

# After three /append calls to a fresh "trim" scope the seqs are 1,2,3.
# Trimming up to seq 2 should leave a single item behind.
call 'delete-up-to (trims oldest)'      200 POST   /delete-up-to '{"scope":"trim","max_seq":2}'

call 'delete by id'                     200 POST   /delete   '{"scope":"only","id":"one"}'
# /delete on a non-existent id returns 200 with "hit":false (same envelope
# pattern as /get). Only /render returns real 404s.
call 'delete miss'                      200 POST   /delete   '{"scope":"only","id":"ghost"}'
case $LAST_BODY in
    *'"hit":false'*) okmsg 'delete miss has "hit":false' ;;
    *) bad "delete miss body: $LAST_BODY" ;;
esac
call 'delete-scope'                     200 POST   /delete-scope '{"scope":"trim"}'
call 'delete-scope-candidates'          200 GET    /delete-scope-candidates

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
call 'delete-up-to: missing max_seq'    400 POST   /delete-up-to '{"scope":"x"}'

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

# delete-up-to: 10 appends, trim up to seq 6, only t7..t10 must survive
i=1; while [ $i -le 10 ]; do
    req POST /append "{\"scope\":\"tmath\",\"id\":\"t$i\",\"payload\":$i}" >/dev/null
    i=$((i+1))
done
call 'trim: delete-up-to seq=6'         200 POST   /delete-up-to '{"scope":"tmath","max_seq":6}'
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

# --- wipe at end --------------------------------------------------------------
say '== final wipe =='
call 'wipe'                             200 POST   /wipe
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
