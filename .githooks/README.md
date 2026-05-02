# Git hooks

Version-controlled hooks shared by every clone of this repo. Activate once
per clone:

```bash
git config core.hooksPath .githooks
```

After that, every `git commit` runs the hooks in this directory. To
deactivate temporarily, `git commit --no-verify`. To deactivate
permanently, `git config --unset core.hooksPath`.

## Hooks

### `pre-commit`

Runs `gofmt -l` on staged `.go` files and rejects the commit if any
need reformatting. Catches the godoc list/code-block formatting that
Go 1.19+ enforces — same gate the CI's `Verify gofmt` step runs, but
fires at commit time so a red CI run is avoided entirely.

If the hook rejects your commit, it prints the exact `gofmt -w` and
`git add` commands needed to fix the staged set; copy-paste those and
re-commit.

The hook is a portable POSIX shell script — works under Git Bash on
Windows, native sh on Linux/macOS, and any shell `git` invokes hooks
through. It needs `gofmt` on `PATH`; if you have Go installed, you
have `gofmt`.
