// Subscriber bridge process-group cleanup is Unix-only — see
// subscriber_command_unix.go for the implementation. Non-Unix builds
// fall back to exec.Cmd's default cancellation (SIGKILL the direct
// child only). Operators on non-Unix platforms who run a subscriber
// script that spawns long-lived children must either `exec` the real
// command (no shell wrapper, no children) or accept that stop() may
// leave orphan processes behind.
//
// In practice the standalone scopecache binary targets Linux Docker
// deployments and the Caddy module ships in xcaddy builds whose
// dominant deployment shape is also Linux; this fallback exists
// purely to keep `go build` working on Windows / Plan 9 dev machines.

//go:build !unix

package scopecache

import "os/exec"

func configureProcessGroup(_ *exec.Cmd) {}
