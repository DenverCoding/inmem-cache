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
// # Adapter contract: both adapters give the script a private socket
//
// Both the standalone and the Caddy adapter wrap RunInitCommand in
// their own runInitWithPrivateSocket helper that:
//
//  1. Creates a temp directory (0o700 perms).
//  2. Binds a temp AF_UNIX socket inside it serving the cache's
//     HTTP routes.
//  3. Sets SCOPECACHE_SOCKET_PATH=<that path> in extraEnv.
//  4. Runs the init script.
//  5. Tears the socket and temp dir down before returning.
//
// Effect: the script ALWAYS reaches the cache via
// `curl --unix-socket "$SCOPECACHE_SOCKET_PATH" http://localhost/...`,
// regardless of whether the operator configured a Unix-socket
// standalone or a TCP-listening Caddy module. The same script body
// works in both deployments.
//
// The PUBLIC listener (the operator-configured Unix socket on the
// standalone, Caddy's HTTP listener on the module) is NOT bound
// during init. External clients hitting it get connection-refused
// instead of a half-populated cache. The public listener binds only
// after RunInitCommand returns.
//
// Subscribers (`SCOPECACHE_SUBSCRIBER_COMMAND` /
// `subscriber_command`) are started AFTER init. Two reasons:
//
//  1. Init is cache-state RESTORATION, not a domain action. The
//     writes it performs auto-populate `_events` with duplicates of
//     state that already exists at the source of truth. Forwarding
//     those through a drain script would loop the data back to
//     where init pulled it from. RunInitCommand wipes `_events`
//     itself when it returns, so subscribers see an empty stream
//     when they activate.
//  2. `_inbox` is untouched during init — no external writers can
//     reach the public listener yet, and init's purpose is
//     restoration, not fan-in.
//
// Net result: when subscribers register there is no backlog. The
// first wake-up fires on the first user-write once the public
// listener is up. No initial-drain dance, no race against the
// listener-bind window.
//
// # Cancellation and process group
//
// ctx is the cancellation handle the caller supplies — typically a
// SIGINT/SIGTERM-aware context for the standalone (signalCtx) or the
// caddy.Context passed into Provision for the module. Cancellation
// causes the kernel to SIGKILL the entire process group (see
// configureProcessGroup), so a script that backgrounds children
// (`curl ... &; wait`) does not leak orphan processes when boot
// gets interrupted. Callers that want a hard timeout wrap ctx with
// context.WithTimeout themselves; the helper itself does not enforce
// a default, since "rebuild from source of truth at boot" can
// legitimately take many minutes on a large dataset.
//
// # Logging and error handling
//
// stdout/stderr are inherited so the operator sees the script's
// output in their normal log stream. logf may be nil — the helper
// then runs without lifecycle logging.
//
// Errors are logged AND returned: the caller decides whether a
// failed init is fatal (abort startup) or recoverable (continue with
// an empty cache). Both adapters currently log + continue — an empty
// cache is still a working cache, and the next operator-triggered
// rebuild (manual /rebuild, scheduled cron, next reload) will fix it.
//
// # Why this lives in core
//
// Same reason subscriber_command.go does: every realistic deployment
// needs this exact bridge — running an operator script at
// well-defined cache lifecycle points is the standard extension
// hook. Stdlib-only (`os/exec`), no new dependencies.

package scopecache

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
)

// RunInitCommand executes command synchronously and blocks until it
// exits or until ctx is cancelled. Returns nil if command is empty
// (sentinel for "not configured") or if cmd.Run reports success;
// otherwise returns the exec error wrapped with the command path.
//
// ctx is the cancellation handle the caller supplies — typically a
// SIGINT/SIGTERM-aware context for the standalone (signalCtx) or the
// caddy.Context passed into Provision for the module. Cancellation
// causes the kernel to SIGKILL the entire process group (see
// configureProcessGroup), so a script that backgrounds children
// (`curl ... &; wait`) does not leak orphan processes when boot
// gets interrupted. Callers that want a hard timeout wrap ctx with
// context.WithTimeout themselves; the helper itself does not enforce
// a default, since "rebuild from source of truth at boot" can
// legitimately take many minutes on a large dataset.
//
// extraEnv is appended to os.Environ(); both adapters use this to
// pass SCOPECACHE_SOCKET_PATH pointing to a per-instance temp Unix
// socket that exposes the cache's HTTP routes for the duration of
// the script. See the file-level comment for the full adapter
// contract.
//
// stdout/stderr are inherited so the operator sees the script's
// output in their normal log stream. logf may be nil — the helper
// then runs without lifecycle logging.
func (g *Gateway) RunInitCommand(ctx context.Context, command string, extraEnv []string, logf func(string, ...any)) error {
	if command == "" {
		return nil
	}
	if logf != nil {
		logf("init: running %s", command)
	}
	cmd := exec.CommandContext(ctx, command)
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.Stdin = nil
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	configureProcessGroup(cmd)
	runErr := cmd.Run()

	// Always wipe `_events` after init, regardless of the script's
	// exit code. Init is cache-state restoration from a source of
	// truth, NOT a domain action — the events its writes auto-
	// populated are duplicates of state that already exists at the
	// source. Forwarding them through a drain script would loop the
	// data back to where init pulled it from. A failure on the wipe
	// is logged but not surfaced; an empty cache plus a stale event
	// stream is still a working cache, and the next operator action
	// will overwrite it.
	if _, delErr := g.DeleteUpTo(EventsScopeName, math.MaxUint64); delErr != nil && logf != nil {
		logf("init: clear %s: %v", EventsScopeName, delErr)
	}

	if runErr != nil {
		if logf != nil {
			logf("init: %s: %v", command, runErr)
		}
		return fmt.Errorf("scopecache init command %s: %w", command, runErr)
	}
	if logf != nil {
		logf("init: %s: completed", command)
	}
	return nil
}
