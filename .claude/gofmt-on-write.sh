#!/usr/bin/env sh
# PostToolUse hook: silently run gofmt -w on the just-edited Go file.
#
# Forwards the hook payload JSON (on stdin) into the dev container,
# where gofmt-on-write-inner.sh extracts the file path, filters .go,
# maps the host path to /src/, and runs gofmt -w.
#
# MSYS_NO_PATHCONV=1 disables Git Bash's automatic Unix-path-to-Windows
# conversion that would otherwise rewrite "/src/.claude/..." into
# "C:/Program Files/Git/src/..." before docker exec sees it.
#
# Best-effort: any failure (container down, mapping mismatch, gofmt
# error) is swallowed via 2>/dev/null and `|| true`. The git
# pre-commit hook in .githooks/pre-commit is the authoritative gate
# that blocks bad formatting from reaching a commit.

MSYS_NO_PATHCONV=1 docker compose exec -T dev sh /src/.claude/gofmt-on-write-inner.sh 2>/dev/null || true
