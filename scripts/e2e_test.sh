#!/bin/sh
# End-to-end test suite: exercises every public scopecache endpoint over
# the real transport. Supports two modes, chosen by env:
#
#   unix socket (default) — standalone scopecache service:
#     SOCK=/run/scopecache.sock BASE=http://localhost
#     docker compose up -d --build scopecache
#     docker compose exec dev sh /src/scripts/e2e_test.sh
#
#   tcp — Caddy module wrapping the core (caddymodule/):
#     SOCK= BASE=http://caddyscope:8080
#     docker compose up -d --build caddyscope
#     docker compose exec -e SOCK= -e BASE=http://caddyscope:8080 dev \
#         sh /src/scripts/e2e_test.sh
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
#
# Scope: the **current core** only — every endpoint in handlers.go's
# RegisterRoutes (append, upsert, update, counter_add, delete,
# delete_up_to, head, tail, get, render, wipe, warm, rebuild,
# delete_scope, stats, help). Endpoints that used to live in core but
# moved out (/admin, /guarded, /delete_guarded, /inbox, /multi_call,
# /delete_scope_candidates, /ts_range) are not exercised here — when
# they return as addons each will get its own per-addon e2e_test.sh.

set -eu

# Required commands. Failing here gives a clear "missing X" message
# up-front instead of a cryptic curl/jq error halfway through the run.
# curl drives every HTTP call; jq parses JSON envelopes and powers the
# precise-shape json_assert helper.
need_cmd() {
    command -v "$1" >/dev/null 2>&1 || {
        printf 'missing required command: %s\n' "$1" >&2
        exit 127
    }
}
need_cmd curl
need_cmd jq

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

# --- start clean ---------------------------------------------------------------
say '== wipe for clean slate =='
call 'wipe initial' 200 POST /wipe

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
# the future /scopelist endpoint). Re-introducing it would silently
# re-create the 100k-scope DoS that motivated the strip.
json_assert 'stats agg: no scopes key' '(.scopes // null) == null'

# --- /stats freshness contract: every mutation advances last_write_ts ---------
# Per-operation tightening of the aggregate-counters block above. The
# block above proved the four atomics agree with the actual store
# contents at one snapshot; this block walks every state-changing
# endpoint individually and asserts that:
#
#   1. total_items moves by exactly the right delta on every op
#      (append +1, upsert-create +1, upsert-replace 0, update 0,
#      delete -1, delete_up_to -N, delete_scope -|scope|, wipe -all)
#   2. scope_count moves by exactly the right delta
#   3. last_write_ts strictly advances on every state-changing op —
#      this is the polling contract: any client that re-queries only
#      when last_write_ts has changed is guaranteed to see the new
#      state
#   4. last_write_ts on /stats equals item.ts for the most recent
#      successful single-item write (the store-wide max equals the
#      per-item stamp because the cache uses one nowUs() for both,
#      see buffer_write.go's insertNewItemLocked)
#
# Everything is timestamped via sleep 1 between ops so the
# strict-greater check is reliable even on hosts where two writes
# could otherwise land in the same microsecond.
say '== /stats freshness: every mutation advances last_write_ts =='

call 'fresh: wipe baseline' 200 POST /wipe
call 'fresh: stats post-wipe' 200 GET /stats
FRESH_TS_PREV=$(printf '%s' "$LAST_BODY" | jq '.last_write_ts')
json_assert 'fresh: total_items == 0 post-wipe' '.total_items == 0'
json_assert 'fresh: scope_count == 0 post-wipe' '.scope_count == 0'

# Single /append: /stats.last_write_ts must equal the item.ts that
# came back from the same call. Both are set from one nowUs() inside
# insertNewItemLocked, so the store-wide max equals the per-item
# stamp on a freshly-wiped store with exactly one write.
sleep 1
call 'fresh: single /append' 200 POST /append \
    '{"scope":"fresh","id":"a","payload":1}'
APPEND_ITEM_TS=$(printf '%s' "$LAST_BODY" | jq '.item.ts')
call 'fresh: stats after single /append' 200 GET /stats
json_assert 'fresh: total_items == 1 after one /append' '.total_items == 1'
json_assert 'fresh: scope_count == 1 after one /append' '.scope_count == 1'
json_assert 'fresh: last_write_ts strictly > wipe-stamp' \
    ".last_write_ts > $FRESH_TS_PREV"
json_assert 'fresh: last_write_ts == item.ts of the only write' \
    ".last_write_ts == $APPEND_ITEM_TS"
FRESH_TS_PREV=$(printf '%s' "$LAST_BODY" | jq '.last_write_ts')

# 10 more appends: total_items must land at exactly 11 (1 + 10),
# scope_count stays at 1 (same scope), last_write_ts advances and
# equals the ts of the 10th item.
sleep 1
i=1
while [ $i -le 10 ]; do
    quiet_call "fresh: /append #$i" 200 POST /append \
        "{\"scope\":\"fresh\",\"id\":\"x$i\",\"payload\":$i}"
    i=$((i+1))
done
# Read x10 back so we can compare its ts to /stats.last_write_ts.
# x10 was the most recent /append, so it carries the max ts in the
# store and /stats.last_write_ts must equal it (CAS-max contract).
call 'fresh: read x10 (last appended)' 200 GET '/get?scope=fresh&id=x10'
LAST_ITEM_TS=$(printf '%s' "$LAST_BODY" | jq '.item.ts')
call 'fresh: stats after 10 appends' 200 GET /stats
json_assert 'fresh: total_items == 11 after 1 + 10 appends' '.total_items == 11'
json_assert 'fresh: scope_count still 1 (same scope)' '.scope_count == 1'
json_assert 'fresh: last_write_ts strictly > pre-loop' \
    ".last_write_ts > $FRESH_TS_PREV"
json_assert 'fresh: last_write_ts == latest-item.ts' \
    ".last_write_ts == $LAST_ITEM_TS"
FRESH_TS_PREV=$(printf '%s' "$LAST_BODY" | jq '.last_write_ts')

# Second scope: scope_count must move to 2 on the first append, item
# count by exactly +1.
sleep 1
call 'fresh: append into a second scope' 200 POST /append \
    '{"scope":"fresh2","id":"y1","payload":1}'
SECOND_SCOPE_ITEM_TS=$(printf '%s' "$LAST_BODY" | jq '.item.ts')
call 'fresh: stats after second-scope append' 200 GET /stats
json_assert 'fresh: total_items == 12 after second-scope append' '.total_items == 12'
json_assert 'fresh: scope_count == 2 after second-scope append' '.scope_count == 2'
json_assert 'fresh: last_write_ts == y1.ts (latest write)' \
    ".last_write_ts == $SECOND_SCOPE_ITEM_TS"
FRESH_TS_PREV=$(printf '%s' "$LAST_BODY" | jq '.last_write_ts')

# /update: total_items unchanged, last_write_ts strictly advances.
# /update is the canonical "modify in place, no count change" path —
# any regression that leaks an item creation here surfaces as a
# total_items drift on the very next /stats call. /update's response
# does not echo the item (only {ok, hit, updated_count}), so the
# refreshed ts comes from a /get probe.
sleep 1
call 'fresh: /update existing item' 200 POST /update \
    '{"scope":"fresh","id":"x5","payload":555}'
call 'fresh: read x5 to get refreshed ts' 200 GET '/get?scope=fresh&id=x5'
UPDATE_ITEM_TS=$(printf '%s' "$LAST_BODY" | jq '.item.ts')
call 'fresh: stats after /update' 200 GET /stats
json_assert 'fresh: total_items unchanged after /update' '.total_items == 12'
json_assert 'fresh: scope_count unchanged after /update' '.scope_count == 2'
json_assert 'fresh: last_write_ts strictly > pre-update' \
    ".last_write_ts > $FRESH_TS_PREV"
json_assert 'fresh: last_write_ts == updated-item.ts' \
    ".last_write_ts == $UPDATE_ITEM_TS"
FRESH_TS_PREV=$(printf '%s' "$LAST_BODY" | jq '.last_write_ts')

# /upsert replace: same shape as /update (item already exists).
# total_items unchanged, last_write_ts strictly advances.
sleep 1
call 'fresh: /upsert replace existing' 200 POST /upsert \
    '{"scope":"fresh","id":"x5","payload":5555}'
UPSERT_ITEM_TS=$(printf '%s' "$LAST_BODY" | jq '.item.ts')
call 'fresh: stats after /upsert replace' 200 GET /stats
json_assert 'fresh: total_items unchanged after /upsert replace' '.total_items == 12'
json_assert 'fresh: last_write_ts strictly > pre-upsert' \
    ".last_write_ts > $FRESH_TS_PREV"
json_assert 'fresh: last_write_ts == replaced-item.ts' \
    ".last_write_ts == $UPSERT_ITEM_TS"
FRESH_TS_PREV=$(printf '%s' "$LAST_BODY" | jq '.last_write_ts')

# /delete one item: total_items -1, scope_count unchanged (scope still
# has 10 items left in fresh + 1 in fresh2), last_write_ts advances.
sleep 1
call 'fresh: /delete one item' 200 POST /delete \
    '{"scope":"fresh","id":"x1"}'
call 'fresh: stats after /delete' 200 GET /stats
json_assert 'fresh: total_items == 11 after /delete (-1)' '.total_items == 11'
json_assert 'fresh: scope_count still 2 (fresh still has items)' '.scope_count == 2'
json_assert 'fresh: last_write_ts strictly > pre-delete' \
    ".last_write_ts > $FRESH_TS_PREV"
FRESH_TS_PREV=$(printf '%s' "$LAST_BODY" | jq '.last_write_ts')

# /delete_up_to: trim a defined number of items, last_write_ts advances.
# Seq layout in scope "fresh" before this op (after the deletes/updates
# above): a(1), x1 already deleted, x2(3), x3(4), x4(5), x5(6 — value
# upserted), x6(7), x7(8), x8(9), x9(10), x10(11). Total = 10 items
# in this scope plus 1 in fresh2 = 11 store-wide.
# delete_up_to max_seq=4 in scope "fresh" removes seqs 1, 3, 4 (a, x2,
# x3). x1 is already gone so deleted_count == 3. total_items goes to 8.
sleep 1
call 'fresh: /delete_up_to seq=4' 200 POST /delete_up_to \
    '{"scope":"fresh","max_seq":4}'
json_assert 'fresh: /delete_up_to deleted_count == 3' '.deleted_count == 3'
call 'fresh: stats after /delete_up_to' 200 GET /stats
json_assert 'fresh: total_items == 8 after /delete_up_to (-3)' '.total_items == 8'
json_assert 'fresh: scope_count still 2' '.scope_count == 2'
json_assert 'fresh: last_write_ts strictly > pre-trim' \
    ".last_write_ts > $FRESH_TS_PREV"
FRESH_TS_PREV=$(printf '%s' "$LAST_BODY" | jq '.last_write_ts')

# /delete_scope on "fresh": removes the 7 items still there, scope_count
# goes from 2 to 1, total_items from 8 to 1 (only fresh2's y1 left).
sleep 1
call 'fresh: /delete_scope fresh' 200 POST /delete_scope \
    '{"scope":"fresh"}'
json_assert 'fresh: /delete_scope deleted_items == 7' '.deleted_items == 7'
call 'fresh: stats after /delete_scope' 200 GET /stats
json_assert 'fresh: total_items == 1 after /delete_scope (-7)' '.total_items == 1'
json_assert 'fresh: scope_count == 1 after /delete_scope (-1)' '.scope_count == 1'
json_assert 'fresh: last_write_ts strictly > pre-delete-scope' \
    ".last_write_ts > $FRESH_TS_PREV"
FRESH_TS_PREV=$(printf '%s' "$LAST_BODY" | jq '.last_write_ts')

# /wipe with one scope still alive: every counter goes to 0,
# last_write_ts still advances (wipe is itself a state-changing
# event, see the post-wipe assertion at the top of /stats agg above).
sleep 1
call 'fresh: /wipe (final, after one-scope state)' 200 POST /wipe
call 'fresh: stats after final /wipe' 200 GET /stats
json_assert 'fresh: total_items == 0 after /wipe' '.total_items == 0'
json_assert 'fresh: scope_count == 0 after /wipe' '.scope_count == 0'
json_assert 'fresh: last_write_ts strictly > pre-wipe' \
    ".last_write_ts > $FRESH_TS_PREV"

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
call 'warm'    200 POST /warm    '{"items":[{"scope":"warm1","id":"a","payload":"A"},{"scope":"warm1","id":"b","payload":"B"},{"scope":"warm2","payload":1}]}'
json_assert 'warm: replaced_scopes == 2' '.replaced_scopes == 2'
json_assert 'warm: count == 3' '.count == 3'

call 'rebuild' 200 POST /rebuild '{"items":[{"scope":"only","id":"one","payload":{"k":"v"}}]}'
json_assert 'rebuild: rebuilt_scopes == 1' '.rebuilt_scopes == 1'
json_assert 'rebuild: rebuilt_items == 1' '.rebuilt_items == 1'

# After /rebuild the previous scopes are gone. /get still envelopes misses in
# a 200 with "hit":false (only /render returns 404).
call 'post-rebuild: old scope gone'     200 GET    '/get?scope=s&id=a'
case $LAST_BODY in
    *'"hit":false'*) okmsg 'post-rebuild old scope: "hit":false' ;;
    *) bad "post-rebuild old scope body: $LAST_BODY" ;;
esac
call 'post-rebuild: new scope reads'    200 GET    '/get?scope=only&id=one'

# /rebuild with empty items[] is a client-bug guard, NOT a way to clear
# the cache. Operators wanting a clean slate use /wipe.
call 'rebuild: empty items[] rejected'  400 POST /rebuild '{"items":[]}'

# --- delete / delete_up_to / delete_scope -------------------------------------
say '== deletes =='
call 'append for trim a' 200 POST /append '{"scope":"trim","id":"a","payload":1}'
call 'append for trim b' 200 POST /append '{"scope":"trim","id":"b","payload":2}'
call 'append for trim c' 200 POST /append '{"scope":"trim","id":"c","payload":3}'

# After three /append calls to a fresh "trim" scope the seqs are 1,2,3.
# Trimming up to seq 2 should leave a single item behind.
call 'delete_up_to (trims oldest)'      200 POST   /delete_up_to '{"scope":"trim","max_seq":2}'
json_assert 'delete_up_to: deleted_count == 2' '.deleted_count == 2'

call 'delete by id'                     200 POST   /delete   '{"scope":"only","id":"one"}'
# /delete on a non-existent id returns 200 with "hit":false (same envelope
# pattern as /get). Only /render returns real 404s.
call 'delete miss'                      200 POST   /delete   '{"scope":"only","id":"ghost"}'
case $LAST_BODY in
    *'"hit":false'*) okmsg 'delete miss has "hit":false' ;;
    *) bad "delete miss body: $LAST_BODY" ;;
esac

call 'delete_scope'                     200 POST   /delete_scope '{"scope":"trim"}'
json_assert 'delete_scope: deleted_scope == true' '.deleted_scope == true'
# Re-deleting a now-absent scope reports hit:false, deleted_scope:false.
call 'delete_scope missing'             200 POST   /delete_scope '{"scope":"trim"}'
case $LAST_BODY in
    *'"hit":false'*) okmsg 'delete_scope missing has "hit":false' ;;
    *) bad "delete_scope missing body: $LAST_BODY" ;;
esac

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
# /counter_add (create + increment), /warm, /rebuild. Clients MUST NOT
# supply ts on any write — every write endpoint rejects a non-zero
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

# --- mega deterministic state-machine test ------------------------------------
# One large end-to-end invariant test. Drives a full sequence:
#   wipe → /rebuild → 50 appends → updates → 50 second-scope appends →
#   updates → explicit deletes → extra appends → delete_up_to → range
#   delete by id list → /head read-back of the entire final state →
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
say '== mega state-machine =='

say '== mega: clean slate + rebuild =='
call 'mega: wipe before state-machine' 200 POST /wipe

mega_rebuild_body='{"items":['
mega_rebuild_body="${mega_rebuild_body}"'{"scope":"mega_data","id":"seed1","payload":{"phase":"rebuild","n":1}},'
mega_rebuild_body="${mega_rebuild_body}"'{"scope":"mega_data","id":"seed2","payload":{"phase":"rebuild","n":2}},'
mega_rebuild_body="${mega_rebuild_body}"'{"scope":"mega_data","id":"seed3","payload":{"phase":"rebuild","n":3}},'
mega_rebuild_body="${mega_rebuild_body}"'{"scope":"mega_extra","id":"x_seed1","payload":{"phase":"rebuild","n":1}},'
mega_rebuild_body="${mega_rebuild_body}"'{"scope":"mega_extra","id":"x_seed2","payload":{"phase":"rebuild","n":2}},'
mega_rebuild_body="${mega_rebuild_body}"'{"scope":"mega_other","id":"o1","payload":{"kind":"other","n":1}},'
mega_rebuild_body="${mega_rebuild_body}"'{"scope":"mega_other","id":"o2","payload":{"kind":"other","n":2}}'
mega_rebuild_body="${mega_rebuild_body}"']}'
call 'mega: rebuild seed data' 200 POST /rebuild "$mega_rebuild_body"

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

# Three direct /head calls in place of the previous /multi_call wrapper —
# the dispatcher endpoint moved out of core (see Phase C addons), so we
# probe each scope individually. limit=200 is comfortably above the
# expected count for every scope so truncated must be false on all three.
say '== mega: head each final scope =='

call 'mega: /head mega_data limit=200' 200 GET '/head?scope=mega_data&limit=200'
if printf '%s' "$LAST_BODY" | jq -e \
    --argjson ids "$mega_data_ids" \
    --argjson seqs "$mega_data_seqs" '
    .count == 49 and
    .truncated == false and
    ([.items[].id] == $ids) and
    ([.items[].seq] == $seqs) and
    (all(.items[]; .scope == "mega_data")) and
    (all(.items[]; .ts > 0)) and
    (any(.items[]; .id == "d10" and .seq == 13 and .payload.v == 1000 and .payload.updated == true)) and
    (any(.items[]; .id == "d20" and .seq == 23 and .payload.v == 2000 and .payload.updated == true)) and
    (any(.items[]; .id == "d51" and .seq == 54 and .payload.late == true)) and
    ([.items[].id] | index("seed1") == null and index("seed2") == null and index("seed3") == null and index("d1") == null and index("d2") == null and index("d3") == null and index("d49") == null)
' >/dev/null; then
    okmsg 'mega: mega_data final ids, seqs, payloads, ts, deletes are exact'
else
    bad "mega: mega_data final-state mismatch: $LAST_BODY"
fi

call 'mega: /head mega_extra limit=200' 200 GET '/head?scope=mega_extra&limit=200'
if printf '%s' "$LAST_BODY" | jq -e \
    --argjson ids "$mega_extra_ids" \
    --argjson seqs "$mega_extra_seqs" '
    .count == 43 and
    .truncated == false and
    ([.items[].id] == $ids) and
    ([.items[].seq] == $seqs) and
    (all(.items[]; .scope == "mega_extra")) and
    (all(.items[]; .ts > 0)) and
    (any(.items[]; .id == "e20" and .seq == 22 and .payload.v == 2000 and .payload.updated == true)) and
    (any(.items[]; .id == "e51" and .seq == 53 and .payload.late == true)) and
    (any(.items[]; .id == "e52" and .seq == 54 and .payload.late == true)) and
    ([.items[].id] | index("x_seed1") == null and index("x_seed2") == null and index("e1") == null and index("e2") == null and index("e5") == null and index("e12") == null and index("e13") == null and index("e14") == null and index("e15") == null and index("e16") == null and index("e49") == null)
' >/dev/null; then
    okmsg 'mega: mega_extra final ids, seqs, payloads, ts, deletes are exact'
else
    bad "mega: mega_extra final-state mismatch: $LAST_BODY"
fi

call 'mega: /head mega_other limit=10' 200 GET '/head?scope=mega_other&limit=10'
if printf '%s' "$LAST_BODY" | jq -e '
    .count == 2 and
    .truncated == false and
    ([.items[].id] == ["o1","o2"]) and
    ([.items[].seq] == [1,2]) and
    (all(.items[]; .scope == "mega_other")) and
    (all(.items[]; .ts > 0)) and
    ([.items[].payload.n] == [1,2])
' >/dev/null; then
    okmsg 'mega: mega_other untouched after whole state-machine'
else
    bad "mega: mega_other final-state mismatch: $LAST_BODY"
fi

# /stats final invariant: aggregate-only since the 100k-scope DoS
# observation (see /stats agg block above). scope_count and total_items
# must agree with the arithmetic above; per-scope item_count + last_seq
# are no longer surfaced by /stats and were verified above via the three
# /head probes (count + items[-1].seq fall out of those assertions).
# When /scopelist lands the per-scope numbers can be re-checked here.
say '== mega: stats final state =='
call 'mega: stats final' 200 GET /stats
json_assert 'mega: /stats aggregate counters exact' '
    .ok == true and
    .scope_count == 3 and
    .total_items == 94
'

# --- wipe at end --------------------------------------------------------------
say '== final wipe =='
call 'wipe' 200 POST /wipe
# Body should report the scopes and items that existed just before wipe.
if printf '%s' "$LAST_BODY" | grep -q '"deleted_scopes"'; then
    okmsg 'wipe body has deleted_scopes'
else
    bad "wipe body missing deleted_scopes: $LAST_BODY"
fi
call 'stats after wipe' 200 GET /stats
if printf '%s' "$LAST_BODY" | grep -q '"scope_count":0'; then
    okmsg 'stats shows empty store'
else
    bad "stats post-wipe: $LAST_BODY"
fi

# --- summary ------------------------------------------------------------------
printf '\n== summary ==\n'
printf 'pass: %d\nfail: %d\n' "$pass" "$fail"
[ "$fail" -eq 0 ] || exit 1
