// Boot-time init-command bridge: a thin Go-side helper that invokes an
// operator-supplied executable once, synchronously, after the cache is
// fully constructed (NewGateway has pre-created the reserved scopes
// `_events` and `_inbox` via initReservedScopes).
//
// Use case: rebuild the cache from a source of truth at boot. The
// script exec.Run's into existence, queries a database / reads files /
// hits a remote API, and writes the resulting state into the cache via
// the same HTTP endpoints any other client uses (curl /append, /warm,
// /rebuild). The cache itself never reads the source of truth — it
// just provides the "now is the right moment to populate me" hook.
//
// extraEnv is appended to os.Environ() so the operator can tell the
// script how to reach the cache:
//
//	SCOPECACHE_SOCKET_PATH   the standalone passes the AF_UNIX socket
//	                         path so curl --unix-socket works directly.
//
// The Caddy adapter passes nothing extra: Caddy listens on whatever
// the operator configured (port, address, TLS), and the script either
// already knows that endpoint or doesn't need to talk to the cache at
// all (init scripts that prepare external state are fine).
//
// Sync, blocking, one-shot. The helper returns when cmd.Run returns;
// the caller's startup sequence is paused for that duration. This
// matches the documented contract: "rebuild from source of truth at
// boot" implies the cache is fully populated before traffic arrives.
// Operators who want the script to background work do so inside the
// script itself (`&`, `nohup ... &`, `setsid` — same shell tools they
// already know).
//
// Adapter timing notes (caller's responsibility, not the helper's):
//
//   - Standalone: bind the AF_UNIX listener and start server.Serve in
//     a goroutine BEFORE invoking RunInitCommand. The script then
//     reaches the cache via SCOPECACHE_SOCKET_PATH while Serve runs
//     concurrently in the background.
//   - Caddy module: Caddy hasn't bound its HTTP listeners yet when
//     Provision runs, so a script that curls localhost reaches the
//     PREVIOUS Caddy instance during reload (if any) or fails to
//     connect on first boot. Operators who want "rebuild from
//     source-of-truth" semantics on the Caddy adapter should either
//     prepare external state in their script (a separate sink that
//     pushes into the cache after Caddy is up) or run the script
//     externally via systemd ExecStartPost / cron, where Caddy's
//     listener is already up.
//
// Errors are logged AND returned: the caller decides whether a
// failed init is fatal (abort startup) or recoverable (continue with
// an empty cache). Both adapters currently log + continue — an empty
// cache is still a working cache, and the next operator-triggered
// rebuild (manual /rebuild, scheduled cron, next reload) will fix it.
//
// Lives in core (rather than as an addon) for the same reason
// subscriber_command.go does: every realistic deployment needs this
// exact bridge — running an operator script at well-defined cache
// lifecycle points is the standard extension hook. Stdlib-only
// (`os/exec`), no new dependencies.

package scopecache

import (
	"fmt"
	"os"
	"os/exec"
)

// RunInitCommand executes command synchronously and blocks until it
// exits. Returns nil if command is empty (sentinel for "not
// configured") or if cmd.Run reports success; otherwise returns the
// exec error wrapped with the command path.
//
// extraEnv is appended to os.Environ(); typical use is passing the
// cache's reachable address (e.g. SCOPECACHE_SOCKET_PATH for the
// standalone adapter) so the script knows where to send writes.
//
// stdout/stderr are inherited so the operator sees the script's
// output in their normal log stream. logf may be nil — the helper
// then runs without lifecycle logging.
//
// See the file-level comment for the full lifecycle/timing contract,
// in particular the per-adapter notes about when the cache is
// reachable via HTTP.
func (g *Gateway) RunInitCommand(command string, extraEnv []string, logf func(string, ...any)) error {
	if command == "" {
		return nil
	}
	if logf != nil {
		logf("init: running %s", command)
	}
	cmd := exec.Command(command)
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.Stdin = nil
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if logf != nil {
			logf("init: %s: %v", command, err)
		}
		return fmt.Errorf("scopecache init command %s: %w", command, err)
	}
	if logf != nil {
		logf("init: %s: completed", command)
	}
	return nil
}
