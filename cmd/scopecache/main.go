package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/VeloxCoding/scopecache"
)

// maxEnvConfig{MB,KB,Sec} are the upper bounds beyond which a later
// unit conversion would silently overflow int64:
//
//   - MB  (MiB→bytes via `<< 20`)
//   - KB  (KiB→bytes via `<< 10`)
//   - Sec (seconds → nanoseconds via `time.Duration * time.Second`)
//
// Mirrors caddymodule's maxConfig{MB,KB,Sec}; same rationale (loud
// rejection beats silent wrong cap).
const (
	maxEnvConfigMB  = math.MaxInt64 >> 20
	maxEnvConfigKB  = math.MaxInt64 >> 10
	maxEnvConfigSec = math.MaxInt64 / int64(time.Second)
)

// UnixSocketPerm is applied to the listening socket file on POSIX systems
// so the Caddy / scopecache group can connect without the file being
// world-readable. It is a no-op on Windows (Chmod there only toggles the
// read-only attribute), which is harmless: Windows AF_UNIX access is
// already gated by NTFS ACLs on the containing directory.
const UnixSocketPerm = 0660

// DefaultSocketPath is the platform-specific default for the listening
// AF_UNIX socket. Linux uses /run/scopecache.sock (a tmpfs that vanishes on
// reboot, which matches the cache's disposable semantics); other OSes fall
// back to os.TempDir() because /run does not exist or is not user-writable
// there. Per-platform definitions live in socket_linux.go and socket_other.go.
// The value can be overridden at runtime via the SCOPECACHE_SOCKET_PATH env var.
var DefaultSocketPath string

// scopeMaxItemsFromEnv returns SCOPECACHE_SCOPE_MAX_ITEMS if set to a positive
// integer, otherwise the compile-time default. A malformed or non-positive
// value is ignored with a warning — the server still starts rather than
// failing on a fat-fingered env var.
func scopeMaxItemsFromEnv() int {
	raw := os.Getenv("SCOPECACHE_SCOPE_MAX_ITEMS")
	if raw == "" {
		return scopecache.ScopeMaxItems
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		log.Printf("SCOPECACHE_SCOPE_MAX_ITEMS=%q is not a positive integer; using default %d", raw, scopecache.ScopeMaxItems)
		return scopecache.ScopeMaxItems
	}
	return n
}

// socketPathFromEnv returns SCOPECACHE_SOCKET_PATH if set to a non-empty value,
// otherwise the platform-specific DefaultSocketPath (see socket_linux.go /
// socket_other.go). Letting operators override the path keeps the binary
// usable on systems where /run is not writable (macOS dev boxes) or where
// multiple cache instances need distinct sockets.
func socketPathFromEnv() string {
	if p := os.Getenv("SCOPECACHE_SOCKET_PATH"); p != "" {
		return p
	}
	return DefaultSocketPath
}

// maxStoreBytesFromEnv returns SCOPECACHE_MAX_STORE_MB (in MiB, converted to bytes)
// if set to a positive integer, otherwise the compile-time default.
// Values above maxEnvConfigMB would overflow int64 after the shift; we
// log + fall back to the default rather than letting a wrap silently
// produce a tiny or negative cap.
func maxStoreBytesFromEnv() int64 {
	raw := os.Getenv("SCOPECACHE_MAX_STORE_MB")
	if raw == "" {
		return int64(scopecache.MaxStoreMiB) << 20
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		log.Printf("SCOPECACHE_MAX_STORE_MB=%q is not a positive integer; using default %d MiB", raw, scopecache.MaxStoreMiB)
		return int64(scopecache.MaxStoreMiB) << 20
	}
	if int64(n) > maxEnvConfigMB {
		log.Printf("SCOPECACHE_MAX_STORE_MB=%d exceeds the maximum MiB value (%d); using default %d MiB", n, maxEnvConfigMB, scopecache.MaxStoreMiB)
		return int64(scopecache.MaxStoreMiB) << 20
	}
	return int64(n) << 20
}

// maxItemBytesFromEnv returns SCOPECACHE_MAX_ITEM_MB (in MiB, converted to bytes)
// if set to a positive integer, otherwise the compile-time default.
// See maxStoreBytesFromEnv for the upper-bound rationale.
func maxItemBytesFromEnv() int64 {
	raw := os.Getenv("SCOPECACHE_MAX_ITEM_MB")
	if raw == "" {
		return int64(scopecache.MaxItemBytes)
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		log.Printf("SCOPECACHE_MAX_ITEM_MB=%q is not a positive integer; using default %d MiB", raw, scopecache.MaxItemBytes>>20)
		return int64(scopecache.MaxItemBytes)
	}
	if int64(n) > maxEnvConfigMB {
		log.Printf("SCOPECACHE_MAX_ITEM_MB=%d exceeds the maximum MiB value (%d); using default %d MiB", n, maxEnvConfigMB, scopecache.MaxItemBytes>>20)
		return int64(scopecache.MaxItemBytes)
	}
	return int64(n) << 20
}

// inboxMaxItemsFromEnv returns SCOPECACHE_INBOX_MAX_ITEMS if set to a
// positive integer, otherwise 0 — the sentinel that lets
// Config.WithDefaults fall back to ScopeMaxItems for the `_inbox`
// scope. A malformed or non-positive value is ignored with a warning.
func inboxMaxItemsFromEnv() int {
	raw := os.Getenv("SCOPECACHE_INBOX_MAX_ITEMS")
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		log.Printf("SCOPECACHE_INBOX_MAX_ITEMS=%q is not a positive integer; falling back to ScopeMaxItems", raw)
		return 0
	}
	return n
}

// inboxMaxItemBytesFromEnv returns SCOPECACHE_INBOX_MAX_ITEM_KB
// (in KiB, converted to bytes) if set to a positive integer, otherwise
// 0 — the sentinel that lets Config.WithDefaults fall back to
// InboxMaxItemBytes (64 KiB) for the `_inbox` scope. A malformed or
// non-positive value is ignored with a warning. KiB is the configured
// unit because the default (64 KiB) reads awkwardly as MiB (0.0625).
// Values above maxEnvConfigKB would overflow int64 after the shift;
// see maxStoreBytesFromEnv for the upper-bound rationale.
func inboxMaxItemBytesFromEnv() int64 {
	raw := os.Getenv("SCOPECACHE_INBOX_MAX_ITEM_KB")
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		log.Printf("SCOPECACHE_INBOX_MAX_ITEM_KB=%q is not a positive integer; falling back to %d KiB", raw, scopecache.InboxMaxItemBytes>>10)
		return 0
	}
	if int64(n) > maxEnvConfigKB {
		log.Printf("SCOPECACHE_INBOX_MAX_ITEM_KB=%d exceeds the maximum KiB value (%d); falling back to %d KiB", n, maxEnvConfigKB, scopecache.InboxMaxItemBytes>>10)
		return 0
	}
	return int64(n) << 10
}

// subscriberCommandFromEnv returns SCOPECACHE_SUBSCRIBER_COMMAND if
// set to a non-empty value, otherwise the empty string. Empty = no
// subscriber spawned (default). Set = the cache spawns one
// subscriber goroutine per reserved scope (`_events`, `_inbox`),
// each invoking the same command on every wake-up. The command
// branches on SCOPECACHE_SCOPE to know which scope fired.
//
// The path is not stat'd here; missing-executable errors surface
// per-invocation (see subscriber_command.go in the core package).
func subscriberCommandFromEnv() string {
	return os.Getenv("SCOPECACHE_SUBSCRIBER_COMMAND")
}

// initCommandFromEnv returns SCOPECACHE_INIT_COMMAND if set to a
// non-empty value, otherwise the empty string. Empty = no init
// command (default). Set = the cache invokes the command once at
// boot, after the AF_UNIX listener is up and after subscribers have
// started, but before main blocks on the shutdown signal.
// SCOPECACHE_SOCKET_PATH is exported into the script's environment
// so it can curl --unix-socket back into the cache.
//
// The path is not stat'd here; missing-executable errors surface at
// invocation time (see init_command.go in the core package).
func initCommandFromEnv() string {
	return os.Getenv("SCOPECACHE_INIT_COMMAND")
}

// initTimeoutFromEnv returns SCOPECACHE_INIT_TIMEOUT_SEC parsed as a
// positive number of seconds, otherwise 0 (no timeout — only
// SIGINT/SIGTERM cancels the running script). Negative, malformed,
// or above-bound values fall back to 0 with a warning.
//
// 0 is the default because "rebuild from source of truth at boot"
// can legitimately take many minutes on a large dataset; a
// surprise-default would cut off real workloads. Operators who want
// a hard ceiling opt in. Above maxEnvConfigSec the multiplication
// would overflow int64 and silently wrap to a tiny or negative
// timeout — rejected loudly so the operator notices.
func initTimeoutFromEnv() time.Duration {
	raw := os.Getenv("SCOPECACHE_INIT_TIMEOUT_SEC")
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		log.Printf("SCOPECACHE_INIT_TIMEOUT_SEC=%q is not a non-negative integer; using no timeout", raw)
		return 0
	}
	if int64(n) > maxEnvConfigSec {
		log.Printf("SCOPECACHE_INIT_TIMEOUT_SEC=%d exceeds the maximum (%d seconds); using no timeout", n, maxEnvConfigSec)
		return 0
	}
	return time.Duration(n) * time.Second
}

// eventsModeFromEnv returns the SCOPECACHE_EVENTS_MODE setting parsed
// into the typed scopecache.EventsMode. Empty string maps to
// EventsModeOff (the default — auto-populate disabled). Any other
// invalid value logs a warning and falls back to EventsModeOff so a
// fat-fingered env var doesn't prevent the server from starting.
func eventsModeFromEnv() scopecache.EventsMode {
	raw := os.Getenv("SCOPECACHE_EVENTS_MODE")
	mode, err := scopecache.ParseEventsMode(raw)
	if err != nil {
		log.Printf("SCOPECACHE_EVENTS_MODE=%q is invalid; falling back to off (valid: off | notify | full)", raw)
		return scopecache.EventsModeOff
	}
	return mode
}

const shutdownGracePeriod = 5 * time.Second

// runInitWithPrivateSocket binds a private temp Unix socket serving
// the cache routes, runs the init command (which is told the socket
// path via SCOPECACHE_SOCKET_PATH), then tears the socket down. The
// public socketPath stays unbound throughout, so external clients
// hitting it during init get connection-refused instead of hitting
// a partially-populated cache.
//
// ctx is propagated into RunInitCommand: SIGINT/SIGTERM at the
// signal handler cancels the in-flight script via SIGKILL on the
// whole process group. timeout > 0 wraps ctx with a hard deadline.
//
// Mirrors caddymodule's runInitWithPrivateSocket — the standalone
// and Caddy adapters share the same "init runs behind a private
// channel" contract so the same script body works in both.
func runInitWithPrivateSocket(ctx context.Context, gw *scopecache.Gateway, mux *http.ServeMux, command string, timeout time.Duration) error {
	dir, err := os.MkdirTemp("", "scopecache-init-")
	if err != nil {
		return fmt.Errorf("create temp dir for init socket: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			log.Printf("remove temp dir %s: %v", dir, err)
		}
	}()

	sockPath := filepath.Join(dir, "init.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("listen on init socket %s: %w", sockPath, err)
	}

	server := &http.Server{Handler: mux}
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = server.Serve(ln)
	}()

	runCtx := ctx
	if timeout > 0 {
		var cancelTimeout context.CancelFunc
		runCtx, cancelTimeout = context.WithTimeout(ctx, timeout)
		defer cancelTimeout()
	}

	runErr := gw.RunInitCommand(
		runCtx,
		command,
		[]string{"SCOPECACHE_SOCKET_PATH=" + sockPath},
		log.Printf,
	)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown init socket: %v", err)
	}
	<-serveDone

	return runErr
}

func listenUnixSocket(path string) (net.Listener, error) {
	dir := filepath.Dir(path)

	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}

	// Unix socket files persist after a crash or non-graceful shutdown; a new
	// listen() on the same path would fail with "address already in use", so we
	// remove any stale file first. Only socket files are removed: if the path
	// resolves to anything else (regular file, directory, symlink to non-socket)
	// we refuse to start instead of obliterating it. The standalone binary
	// commonly runs as root to write into /run/, so a misconfigured
	// SCOPECACHE_SOCKET_PATH would otherwise be a foot-gun against arbitrary
	// system files. ErrNotExist is the normal first-boot case.
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf(
				"refusing to remove non-socket file at %q: "+
					"set SCOPECACHE_SOCKET_PATH to an unused path "+
					"(expected a stale Unix socket from a prior run, found %s)",
				path, info.Mode().Type())
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}

	if err := os.Chmod(path, UnixSocketPerm); err != nil {
		_ = ln.Close()
		_ = os.Remove(path)
		return nil, err
	}

	return ln, nil
}

func main() {
	cfg := scopecache.Config{
		ScopeMaxItems: scopeMaxItemsFromEnv(),
		MaxStoreBytes: maxStoreBytesFromEnv(),
		MaxItemBytes:  maxItemBytesFromEnv(),
		Events: scopecache.EventsConfig{
			Mode: eventsModeFromEnv(),
		},
		Inbox: scopecache.InboxConfig{
			MaxItems:     inboxMaxItemsFromEnv(),
			MaxItemBytes: inboxMaxItemBytesFromEnv(),
		},
	}
	cfg = cfg.WithDefaults()
	gw := scopecache.NewGateway(cfg)
	api := scopecache.NewAPI(gw, scopecache.APIConfig{})

	log.Printf("scopecache capacity: %d items per scope, %d MiB store-wide, %d MiB per item; inbox %d items, %d KiB per item; events_mode=%s",
		cfg.ScopeMaxItems, cfg.MaxStoreBytes>>20, cfg.MaxItemBytes>>20,
		cfg.Inbox.MaxItems, cfg.Inbox.MaxItemBytes>>10,
		cfg.Events.Mode)

	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	// Timeouts sized for a local AF_UNIX cache. Budgets account for wire
	// transfer AND stream JSON-decode of the bulk body (encoding/json runs
	// ~100-500 MB/s on modern CPU; interleaved with reading so the slower
	// of the two dominates).
	//   ReadTimeout  — accept → body fully read. A 1 GiB store config
	//                  (~1.14 GiB bulk body cap) takes ~15-40s on a slow
	//                  CPU-constrained host; 60s gives comfortable margin.
	//                  Configs beyond ~2 GiB may need tuning.
	//   WriteTimeout — must exceed ReadTimeout; covers body-read + handler
	//                  + response write. Handlers are sub-ms, so the 15s
	//                  overhead is pure slack.
	//   IdleTimeout  — keep-alive idle-kill; standard Go default shape.
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      75 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	socketPath := socketPathFromEnv()

	// SIGINT/SIGTERM triggers Shutdown, which drains in-flight requests.
	// Using log.Fatal here would bypass the shutdown path entirely because it
	// calls os.Exit and skips deferred cleanup.
	signalCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	// Boot-time init command runs BEFORE the public socket is bound,
	// behind a private temp socket. External clients trying to reach
	// the configured socketPath during init get connection-refused —
	// no half-populated responses leak out. Mirrors the Caddy module's
	// runInitWithPrivateSocket pattern. See init_command.go.
	//
	// signalCtx is passed in so SIGINT/SIGTERM during a long init
	// SIGKILLs the script's process group instead of leaving boot
	// hung indefinitely.
	if cmd := initCommandFromEnv(); cmd != "" {
		if err := runInitWithPrivateSocket(signalCtx, gw, mux, cmd, initTimeoutFromEnv()); err != nil {
			log.Printf("scopecache init: %v (continuing with empty cache)", err)
		}
	}

	// Subscribers start AFTER init. Reason: init writes auto-populate
	// `_events` (when EventsMode != off) and may write to `_inbox`
	// directly. If subscribers were already active during init, each
	// of those writes would fire a wake-up — and the operator's drain
	// command, when it tries to reach the cache via the configured
	// socketPath, would hit connection-refused (we haven't bound it
	// yet). The failed run consumes the wake-up, so the backlog sits
	// until a later user-write triggers another wake-up.
	//
	// Starting subscribers AFTER init means no wake-ups fire while
	// the public socket is still down. The matching wake-up that
	// drains init's backlog is fired explicitly via
	// WakeReservedSubscribers below, AFTER server.Serve has bound
	// the public socket — so the drain command's curl reaches a
	// listening cache.
	stopSubscribers := gw.StartReservedSubscribers(subscriberCommandFromEnv(), log.Printf)
	defer stopSubscribers()

	ln, err := listenUnixSocket(socketPath)
	if err != nil {
		log.Fatal(err)
	}

	go func() {
		<-signalCtx.Done()
		log.Print("scopecache shutting down")
		// Stop subscribers FIRST, while the HTTP server is still up.
		// stopSubscribers cancels each goroutine's per-subscriber
		// context, which makes exec.CommandContext SIGKILL the
		// in-flight drain command (the entire process group via
		// configureProcessGroup, so script-spawned children die
		// alongside their parent). The call then blocks until each
		// goroutine has fully exited, bounded by OS kill latency
		// rather than the command's voluntary completion — a stuck
		// curl or tarpitted endpoint cannot stall shutdown past the
		// 5s grace period. The cache socket stays open during the
		// kill window, so any HTTP roundtrip the killed command had
		// in flight produces a "connection reset" client-side rather
		// than hitting a torn-down socket; the cache treats it as
		// any other client crash. unsubscribe is idempotent
		// (subscribe.go), so the deferred stopSubscribers() at the
		// outer scope is a safe no-op backstop.
		stopSubscribers()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGracePeriod)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("graceful shutdown error: %v", err)
		}
	}()

	log.Printf("scopecache listening on unix://%s", socketPath)

	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.Serve(ln)
	}()

	// Block on Serve's exit. The signal goroutine triggers
	// server.Shutdown on SIGINT/SIGTERM, which makes Serve return
	// http.ErrServerClosed; any other exit is logged.
	if err := <-serveDone; err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("serve error: %v", err)
	}

	_ = ln.Close()
	_ = os.Remove(socketPath)
	log.Print("scopecache stopped")
}
