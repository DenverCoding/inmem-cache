// Subscriber-command bridge: a thin Go-side wrapper around the
// Subscribe primitive that invokes an operator-supplied executable on
// every wake-up. The cache itself never reads or writes item data —
// the command does the actual fetch (curl /tail), processing, and
// delete (curl /delete_up_to) using the cache's existing HTTP
// endpoints.
//
// One environment variable is set per command invocation:
//
//	SCOPECACHE_SCOPE   the reserved scope that triggered the wake-up
//	                   (_events or _inbox)
//
// Everything else the command needs (cache socket path, HTTP base URL,
// auth headers) is the operator's responsibility — the cache does not
// know how the command reaches itself.
//
// "Command," not "script": the bridge calls `exec.Command(path)`,
// which works for shell scripts, Python/PHP/Ruby with a shebang,
// and compiled binaries (Go, C, Rust, anything). Operators are not
// limited to interpreted scripts.
//
// Concurrency: one StartSubscriber call = one goroutine = one command
// invocation at a time, in strict order. Wake-ups arriving while a
// command is running coalesce in the cache's single-slot wake-up
// channel; the next loop iteration sees a single pending wake-up and
// triggers one more command run. The command never runs concurrently
// with itself.
//
// Lives in core (rather than as an addon) because every realistic
// scopecache deployment needs this exact bridge — the script-runner
// is the standard way to get items out of the cache and into a sink
// of the operator's choosing. The implementation is stdlib-only
// (`os/exec`) and has no dependencies beyond what core already pulls.

package scopecache

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
)

// StartSubscriber spawns a subscriber goroutine that subscribes to
// scope, then invokes command + waits for it to exit on every
// wake-up. Returns a stop function that:
//
//  1. closes the subscription (which closes the wake-up channel,
//     which exits the goroutine's `for range` loop), and
//  2. blocks until the goroutine has fully exited — including any
//     in-flight `cmd.Run` for the currently-executing command.
//
// Step 2 matters for adapter shutdown: a running drain command may
// still be doing HTTP roundtrips against the cache, so the caller
// must wait for those to settle before tearing down the server. With
// the blocking stop the standalone binary can call `stopSubscribers()`
// before `server.Shutdown` and be sure no orphan curl is left
// banging on a closed socket.
//
// stop is idempotent (Subscribe's unsub is, and reading from a
// closed `done` channel returns immediately) so it's safe to wire
// both into a signal-handler AND a `defer` backstop.
//
// Errors at start-up:
//   - empty scope or command -> validation error
//   - non-reserved scope -> wraps ErrInvalidSubscribeScope
//   - duplicate subscribe -> wraps ErrAlreadySubscribed
//
// The command's exit code is logged but not interpreted: the bridge
// never inspects items (the command owns the items via /tail and
// /delete_up_to) so success/failure is irrelevant to the next
// wake-up. Operators who want exponential backoff or alerting on
// repeated failures build that into the command itself — that's
// where the retry context lives anyway.
//
// The command path is not stat'd here. Operators may deploy the
// command later and a missing file just produces a per-invocation
// log line until it shows up.
func (gw *Gateway) StartSubscriber(scope, command string) (stop func(), err error) {
	if scope == "" {
		return nil, errors.New("scopecache: StartSubscriber: scope is required")
	}
	if command == "" {
		return nil, errors.New("scopecache: StartSubscriber: command is required")
	}

	ch, unsub, err := gw.Subscribe(scope)
	if err != nil {
		return nil, fmt.Errorf("scopecache: StartSubscriber: subscribe %s: %w", scope, err)
	}

	done := make(chan struct{})
	go func() {
		// No initial pre-subscribe drain: the command handles all reads
		// itself, and on first wake-up will see whatever has accumulated
		// (the cache only suppresses wake-ups when the channel slot is
		// already filled, not when items already exist). If the cache
		// has pending items at Start time but no new write triggers a
		// wake-up, the command won't run until something writes — that's
		// a deliberate choice: the bridge never reads, never decides,
		// only forwards.
		defer close(done)
		for range ch {
			runSubscriberCommand(scope, command)
		}
	}()

	stop = func() {
		unsub()
		<-done
	}
	return stop, nil
}

// runSubscriberCommand executes the configured command once, blocking
// on its exit. Stderr/stdout are inherited from the parent process so
// operators can see the command's output in their normal log stream.
// SCOPECACHE_SCOPE is passed via env so a single command can serve
// both reserved scopes and branch on which one fired.
func runSubscriberCommand(scope, command string) {
	cmd := exec.Command(command)
	cmd.Env = append(os.Environ(), "SCOPECACHE_SCOPE="+scope)
	cmd.Stdin = nil
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		// A non-zero exit, missing executable, or signal-kill all land
		// here. Log + move on — the next wake-up will retry. Operators
		// who want exponential backoff or alerting on repeated failures
		// build that into their command (it's where the retry context
		// lives anyway).
		log.Printf("scopecache subscriber: %s (scope=%s): %v", command, scope, err)
	}
}
