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
	"context"
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
//     which exits the goroutine's `for range` loop),
//  2. cancels the per-subscriber context — the in-flight cmd.Run
//     was started via exec.CommandContext, so cancellation
//     SIGKILLs the running process and cmd.Run returns immediately,
//     and
//  3. blocks until the goroutine has fully exited.
//
// Step 2 is what bounds shutdown latency. Pre-step-2 the contract was
// "stop blocks until the in-flight command finishes" — fine if the
// command always finishes promptly, catastrophic if it doesn't. A
// stuck curl, a tarpitted network endpoint, or a buggy script with
// an infinite loop would block stop forever, blocking standalone
// shutdown before its 5s grace period and stalling Caddy reload via
// Cleanup. Cancel-on-stop trades graceful in-flight completion for
// a hard upper bound: stop returns within OS-process-kill latency,
// regardless of what the command was doing.
//
// The HTTP-roundtrip-orphan concern that motivated the original
// blocking stop is addressed differently: the standalone main()
// still calls stopSubscribers() before server.Shutdown, so the
// cache socket stays open while the goroutine tears down. A killed
// curl mid-request just produces a "connection reset" client-side;
// the cache sees a closed connection and handles gracefully — same
// as any other client crash.
//
// stop is idempotent (Subscribe's unsub is, calling cancel on an
// already-cancelled context is a no-op, and reading from a closed
// `done` channel returns immediately) so it's safe to wire both
// into a signal-handler AND a `defer` backstop.
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

	// In-package caller — go directly to *store.Subscribe instead of
	// the Gateway passthrough. Keeps the rule clean: external callers
	// reach the cache through *Gateway, internal code reaches *store
	// directly. Subscribe carries no payload bytes (returns a wake-up
	// channel), so this bypass also makes explicit that the
	// gateway_clone.go discipline is by design unrelated to this path.
	ch, unsub, err := gw.store.Subscribe(scope)
	if err != nil {
		return nil, fmt.Errorf("scopecache: StartSubscriber: subscribe %s: %w", scope, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
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
			runSubscriberCommand(ctx, scope, command)
		}
	}()

	stop = func() {
		// Cancel before unsub so an in-flight cmd.Run unblocks
		// (exec.CommandContext SIGKILLs the process on cancel) before
		// the goroutine even loops back to check the channel. unsub
		// then closes ch so the for-range exits on the next iteration,
		// and <-done blocks until the goroutine has actually returned.
		cancel()
		unsub()
		<-done
	}
	return stop, nil
}

// runSubscriberCommand executes the configured command once, blocking
// on its exit OR on cancellation of ctx (whichever fires first).
// Stderr/stdout are inherited from the parent process so operators
// can see the command's output in their normal log stream.
// SCOPECACHE_SCOPE is passed via env so a single command can serve
// both reserved scopes and branch on which one fired.
//
// ctx is the per-subscriber context owned by StartSubscriber.
// Cancellation makes the kernel SIGKILL the entire process group
// (configureProcessGroup wires Setpgid + an override on cmd.Cancel
// that targets -pid instead of pid) so a script that backgrounds
// children — `curl ... & wait`, etc. — does not leak orphan
// descendants when stop() fires. cmd.Run returns with a
// "signal: killed" or context-cancelled error; the error is logged
// like any other failure, then the goroutine sees the closed
// wake-up channel and exits — bounded by OS kill latency instead
// of the command's voluntary exit. See subscriber_command_unix.go
// for the rationale; the _other.go fallback degrades to direct-
// child kill on non-Unix builds.
func runSubscriberCommand(ctx context.Context, scope, command string) {
	cmd := exec.CommandContext(ctx, command)
	cmd.Env = append(os.Environ(), "SCOPECACHE_SCOPE="+scope)
	cmd.Stdin = nil
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	configureProcessGroup(cmd)
	if err := cmd.Run(); err != nil {
		// A non-zero exit, missing executable, signal-kill, or
		// context-cancel all land here. Log + move on — the next
		// wake-up will retry. Operators who want exponential backoff
		// or alerting on repeated failures build that into their
		// command (it's where the retry context lives anyway).
		log.Printf("scopecache subscriber: %s (scope=%s): %v", command, scope, err)
	}
}
