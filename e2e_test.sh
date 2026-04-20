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
