#!/usr/bin/env sh
# Container-side half of the PostToolUse gofmt hook. Reads the hook
# payload JSON on stdin, extracts the file path, filters .go, maps the
# host path to /src/, runs gofmt -w. See .claude/gofmt-on-write.sh for
# the host wrapper that pipes the hook payload through `docker compose
# exec` into this script.

f=$(jq -r '.tool_response.filePath // .tool_input.file_path')

case "$f" in
    *.go) ;;
    *) exit 0 ;;
esac

# Convert host path (e:\dockerprojecten\caddy_module\foo.go or any
# mixed-slash form) to the container's /src/ mount.
cpath=$(printf '%s' "$f" | sed -e 's|\\|/|g' -e 's|^[eE]:/dockerprojecten/caddy_module/|/src/|')

if [ -f "$cpath" ]; then
    gofmt -w "$cpath"
fi
