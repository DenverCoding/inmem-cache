#!/usr/bin/env bash
# release.sh — single-command release flow for scopecache.
#
# Replaces the older "push tag, then wait for the bot workflow to bump
# caddymodule" dance. The bot lands its commit on origin AFTER your
# push, leaving local main one commit behind, forcing a `git pull
# --rebase` before the next push, and occasionally race-failing on
# proxy.golang.org pickup.
#
# This script does the entire flow locally and atomically:
#   1. Validate version shape + clean tree + remote sync.
#   2. Tag root vX.Y.Z locally.
#   3. Push root tag (so the caddymodule resolver can see it).
#   4. Bump caddymodule/go.mod to require vX.Y.Z.
#   5. go mod tidy in caddymodule (with proxy retries + direct fallback).
#   6. Commit the bump.
#   7. Tag caddymodule/vX.Y.Z locally.
#   8. Push main + caddymodule tag.
#
# After step 8, origin and local are in sync — no surprise bot commit
# arrives, no `pull --rebase` is needed afterwards, and a downstream
# `xcaddy --with .../caddymodule@latest` resolves to the right thing
# the moment the push completes.
#
# The .github/workflows/sync-caddymodule-tag.yml workflow has been
# changed to workflow_dispatch only — it will not auto-fire on tag
# push and therefore cannot race this script. It remains available
# from the Actions tab as a recovery path if a release is ever
# performed without this script (e.g. manual `git push origin
# vX.Y.Z`).
#
# Usage:
#   ./scripts/release.sh v0.8.4
#   ./scripts/release.sh v0.8.4 --dry-run   # show what would run, no changes
#
# Re-running after a partial failure: the script is idempotent in the
# steps that matter — if the root tag is already on origin it skips
# steps 2-3, if caddymodule/go.mod already pins the target version
# step 4 is a no-op, etc. So if push step 8 fails on a transient
# network glitch, just re-run.

set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
REPO_ROOT=$(cd "$SCRIPT_DIR/.." && pwd)
cd "$REPO_ROOT"

VERSION=""
DRY_RUN=0
for arg in "$@"; do
    case "$arg" in
        --dry-run) DRY_RUN=1 ;;
        v*)        VERSION="$arg" ;;
        -h|--help)
            sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//'
            exit 0 ;;
        *)
            echo "unknown argument: $arg" >&2
            echo "usage: $0 vX.Y.Z [--dry-run]" >&2
            exit 2 ;;
    esac
done

if [ -z "$VERSION" ]; then
    echo "usage: $0 vX.Y.Z [--dry-run]" >&2
    exit 2
fi

# Token-only check — full semver validation lives in the workflow's
# regex when the safety-net path is used; here a `v*` prefix is enough
# because everything else flows through `git tag -a`.
case "$VERSION" in
    v*) ;;
    *)  echo "version must start with 'v' (got: $VERSION)" >&2; exit 2 ;;
esac

run() {
    if [ "$DRY_RUN" -eq 1 ]; then
        echo "  [dry-run] $*"
    else
        echo "  + $*"
        eval "$*"
    fi
}

# --- pre-flight ----------------------------------------------------

echo "[pre-flight] working tree clean"
if ! git diff --quiet || ! git diff --cached --quiet; then
    echo "  uncommitted changes present; commit or stash first" >&2
    exit 1
fi

echo "[pre-flight] caddymodule/$VERSION tag is free"
if git rev-parse "refs/tags/caddymodule/$VERSION" >/dev/null 2>&1; then
    echo "  tag caddymodule/$VERSION already exists locally" >&2
    exit 1
fi
if git ls-remote --tags origin "caddymodule/$VERSION" | grep -q "caddymodule/$VERSION"; then
    echo "  tag caddymodule/$VERSION already exists on origin" >&2
    exit 1
fi

echo "[pre-flight] local main vs origin/main"
git fetch origin main >/dev/null
LOCAL=$(git rev-parse main)
REMOTE=$(git rev-parse origin/main)
BASE=$(git merge-base main origin/main 2>/dev/null || echo "")
if [ "$LOCAL" = "$REMOTE" ]; then
    echo "  in sync"
elif [ "$LOCAL" = "$BASE" ]; then
    echo "  local main is BEHIND origin/main; run 'git pull --rebase origin main' first" >&2
    exit 1
elif [ "$REMOTE" = "$BASE" ]; then
    echo "  local main is ahead of origin/main (will push our commits)"
else
    echo "  local main has diverged from origin/main; resolve manually first" >&2
    exit 1
fi

# --- root tag (idempotent: skip if already on origin) --------------

ROOT_TAG_ON_ORIGIN=0
if git ls-remote --tags origin "$VERSION" | grep -q "refs/tags/$VERSION$"; then
    ROOT_TAG_ON_ORIGIN=1
fi

if [ "$ROOT_TAG_ON_ORIGIN" -eq 1 ]; then
    echo "[step 1-2/6] root tag $VERSION already on origin; skipping create+push"
else
    echo "[step 1/6] tag root $VERSION locally"
    run "git tag -a '$VERSION' -m '$VERSION'"

    echo "[step 2/6] push root tag (so caddymodule resolver can see it)"
    run "git push origin '$VERSION'"
fi

# --- caddymodule bump ---------------------------------------------

CURRENT_PIN=$(grep -oE 'github.com/VeloxCoding/scopecache v[^ ]+' caddymodule/go.mod | awk '{print $2}' || echo "")
if [ "$CURRENT_PIN" = "$VERSION" ]; then
    echo "[step 3-5/6] caddymodule/go.mod already requires $VERSION; skipping bump"
else
    echo "[step 3/6] bump caddymodule/go.mod from $CURRENT_PIN to $VERSION"
    run "( cd caddymodule && go mod edit -require=github.com/VeloxCoding/scopecache@$VERSION )"

    echo "[step 4/6] go mod tidy in caddymodule (5x retry, GOPROXY=direct fallback)"
    if [ "$DRY_RUN" -eq 1 ]; then
        echo "  [dry-run] ( cd caddymodule && GOWORK=off go mod tidy )"
        echo "  [dry-run] on persistent failure: GOPROXY=direct go mod tidy"
    else
        SUCCESS=0
        for i in 1 2 3 4 5; do
            if ( cd caddymodule && GOWORK=off go mod tidy ); then
                SUCCESS=1
                break
            fi
            echo "  go mod tidy attempt $i failed; sleeping 20s before retry"
            sleep 20
        done
        if [ "$SUCCESS" != "1" ]; then
            echo "  proxy retries exhausted; falling back to GOPROXY=direct"
            ( cd caddymodule && GOWORK=off GOPROXY=direct go mod tidy )
        fi
    fi

    echo "[step 5/6] commit caddymodule bump"
    if git diff --quiet caddymodule/go.mod caddymodule/go.sum; then
        echo "  no changes to caddymodule/go.mod or go.sum after tidy; nothing to commit"
    else
        run "git add caddymodule/go.mod caddymodule/go.sum"
        run "git commit -m 'build(caddymodule): bump scopecache dep to $VERSION'"
    fi
fi

# --- caddymodule tag + final push ---------------------------------

echo "[step 6/6] tag caddymodule/$VERSION + push main + tag"
run "git tag -a 'caddymodule/$VERSION' -m 'caddymodule/$VERSION'"
run "git push origin main 'caddymodule/$VERSION'"

if [ "$DRY_RUN" -eq 1 ]; then
    echo
    echo "[dry-run] no changes made. Re-run without --dry-run to actually release."
else
    echo
    echo "[done] $VERSION released. local main and origin/main are in sync."
fi
