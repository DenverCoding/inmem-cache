// Standalone scopecache binary. Reads operator config from env vars,
// opens an AF_UNIX socket, runs the optional init command behind a
// private temp socket, then serves the cache routes until SIGTERM /
// SIGINT.
//
// Boot sequence (matters for the init/subscriber contract):
//
//  1. Build *Gateway + *API from env-driven Config.
//  2. If SCOPECACHE_INIT_COMMAND is set, runInitWithPrivateSocket
//     binds a private temp socket exposing the cache routes,
//     execs the script with SCOPECACHE_SOCKET_PATH pointing at it,
//     then tears the temp socket down. The PUBLIC socket is NOT
//     bound during this phase — external clients hitting it get
//     connection-refused, not a half-populated cache.
//  3. Start subscribers (StartReservedSubscribers). RunInitCommand
//     has wiped `_events` already, and `_inbox` was unreachable to
//     external writers, so subscribers see an empty stream.
//  4. Bind the public socket and start serving.
//  5. SIGINT/SIGTERM cancels signalCtx → graceful shutdown.
//
// Recognised env vars (operator interface):
//
//	SCOPECACHE_SOCKET_PATH         AF_UNIX socket path (default platform-specific)
//	SCOPECACHE_SCOPE_MAX_ITEMS     per-scope item cap
//	SCOPECACHE_MAX_STORE_MB        store-wide byte cap (MiB)
//	SCOPECACHE_MAX_ITEM_MB         per-item byte cap (MiB)
//	SCOPECACHE_INBOX_MAX_ITEMS     per-scope item cap on `_inbox`
//	SCOPECACHE_INBOX_MAX_ITEM_KB   per-item byte cap on `_inbox` (KiB)
//	SCOPECACHE_EVENTS_MODE         off | notify | full
//	SCOPECACHE_SUBSCRIBER_COMMAND  drain-script path (empty = disabled)
//	SCOPECACHE_INIT_COMMAND        boot-init-script path (empty = disabled)
//	SCOPECACHE_INIT_TIMEOUT_SEC    hard cap on init script (0 = no timeout)
//
// Every parser follows the same shape: empty env var → compile-time
// default; malformed / out-of-range / negative → log warning and fall
// back to the default. A fat-fingered env var never prevents the
// server from starting.

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

// UnixSocketPerm is the mode applied to the listening socket file on
// POSIX systems — operator-group writable, not world-readable. No-op
// on Windows; AF_UNIX access is gated by NTFS ACLs there.
const UnixSocketPerm = 0660

// DefaultSocketPath is the platform-specific default for the listening
// AF_UNIX socket. Linux uses /run/scopecache.sock (tmpfs, vanishes on
// reboot — matches the cache's disposable semantics); other OSes fall
// back to os.TempDir(). Per-platform definitions live in socket_linux.go
// / socket_other.go. Override at runtime via SCOPECACHE_SOCKET_PATH.
var DefaultSocketPath string

const shutdownGracePeriod = 5 * time.Second

// --- Env-var parsers ------------------------------------------------

// scopeMaxItemsFromEnv reads SCOPECACHE_SCOPE_MAX_ITEMS.
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

// socketPathFromEnv reads SCOPECACHE_SOCKET_PATH, defaulting to the
// platform-specific DefaultSocketPath.
func socketPathFromEnv() string {
	if p := os.Getenv("SCOPECACHE_SOCKET_PATH"); p != "" {
		return p
	}
	return DefaultSocketPath
}

// maxStoreBytesFromEnv reads SCOPECACHE_MAX_STORE_MB and converts to
// bytes. Above maxEnvConfigMB the shift would overflow int64; falls
// back to the compile-time default in that case.
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

// maxItemBytesFromEnv reads SCOPECACHE_MAX_ITEM_MB and converts to
// bytes. Same overflow contract as maxStoreBytesFromEnv.
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

// inboxMaxItemsFromEnv reads SCOPECACHE_INBOX_MAX_ITEMS.
// Returns 0 (sentinel for Config.WithDefaults to fall back to
// ScopeMaxItems) on missing / invalid input.
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

// inboxMaxItemBytesFromEnv reads SCOPECACHE_INBOX_MAX_ITEM_KB and
// converts to bytes. KiB rather than MiB because the default (64 KiB)
// reads awkwardly as MiB. Returns 0 (Config.WithDefaults sentinel)
// on missing / invalid / overflow input.
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

// subscriberCommandFromEnv reads SCOPECACHE_SUBSCRIBER_COMMAND.
// Empty = no subscriber spawned.
func subscriberCommandFromEnv() string {
	return os.Getenv("SCOPECACHE_SUBSCRIBER_COMMAND")
}

// initCommandFromEnv reads SCOPECACHE_INIT_COMMAND.
// Empty = no init script.
func initCommandFromEnv() string {
	return os.Getenv("SCOPECACHE_INIT_COMMAND")
}

// initTimeoutFromEnv reads SCOPECACHE_INIT_TIMEOUT_SEC.
// 0 (default) = no timeout, only SIGINT/SIGTERM cancels the script.
// "Rebuild from source of truth at boot" can legitimately take many
// minutes; a surprise default would cut off real workloads. Above
// maxEnvConfigSec the multiplication would overflow int64.
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

// eventsModeFromEnv reads SCOPECACHE_EVENTS_MODE.
// Empty / invalid = EventsModeOff. Valid: off | notify | full.
func eventsModeFromEnv() scopecache.EventsMode {
	raw := os.Getenv("SCOPECACHE_EVENTS_MODE")
	mode, err := scopecache.ParseEventsMode(raw)
	if err != nil {
		log.Printf("SCOPECACHE_EVENTS_MODE=%q is invalid; falling back to off (valid: off | notify | full)", raw)
		return scopecache.EventsModeOff
	}
	return mode
}

// --- Boot helpers ---------------------------------------------------

// runInitWithPrivateSocket binds a private temp Unix socket serving
// the cache routes, runs the init command (told the socket path via
// SCOPECACHE_SOCKET_PATH), then tears the socket down. Mirrors
// caddymodule's runInitWithPrivateSocket so the same script body
// works in both adapters.
//
// ctx is propagated into RunInitCommand: SIGINT/SIGTERM cancels the
// in-flight script via SIGKILL on the whole process group. timeout
// > 0 wraps ctx with a hard deadline.
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

// listenUnixSocket binds an AF_UNIX listener at path with
// UnixSocketPerm, removing a stale socket file from a prior crash if
// one exists. Refuses to remove anything that is not a socket — a
// misconfigured SCOPECACHE_SOCKET_PATH must not become a foot-gun
// against arbitrary system files.
func listenUnixSocket(path string) (net.Listener, error) {
	dir := filepath.Dir(path)

	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}

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

	// HTTP timeouts sized for a local AF_UNIX cache. ReadTimeout
	// covers a worst-case bulk body decode (~15-40s on a 1 GiB store
	// at slow CPU); WriteTimeout must exceed it; IdleTimeout is the
	// standard keep-alive kill.
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      75 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	socketPath := socketPathFromEnv()

	// SIGINT/SIGTERM triggers Shutdown via the goroutine below.
	// log.Fatal would skip deferred cleanup; we route through the
	// signal context instead.
	signalCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	// Boot-time init runs behind a private temp socket BEFORE the
	// public socket is bound. External clients hitting socketPath
	// during init get connection-refused — no half-populated
	// responses leak out. signalCtx propagates to the script so a
	// SIGTERM during a long init kills the whole process group.
	if cmd := initCommandFromEnv(); cmd != "" {
		if err := runInitWithPrivateSocket(signalCtx, gw, mux, cmd, initTimeoutFromEnv()); err != nil {
			log.Printf("scopecache init: %v (continuing with empty cache)", err)
		}
	}

	// Subscribers start AFTER init. RunInitCommand wipes `_events`
	// itself when it returns (init is restoration, not a domain
	// action) and `_inbox` was unreachable to external writers, so
	// subscribers register against an empty stream. The first
	// wake-up fires on the first user-write after the public socket
	// is up.
	stopSubscribers := gw.StartReservedSubscribers(subscriberCommandFromEnv(), log.Printf)
	defer stopSubscribers()

	ln, err := listenUnixSocket(socketPath)
	if err != nil {
		log.Fatal(err)
	}

	// Shutdown order on signal: stop subscribers FIRST while the
	// HTTP server is still up — exec.CommandContext SIGKILLs the
	// whole drain-script process group, bounded by OS-kill latency
	// rather than the script's voluntary completion. Then Shutdown
	// drains in-flight HTTP requests. Both stops are idempotent, so
	// the deferred backstops above are no-ops if this goroutine
	// fires first.
	go func() {
		<-signalCtx.Done()
		log.Print("scopecache shutting down")
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

	if err := <-serveDone; err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("serve error: %v", err)
	}

	_ = ln.Close()
	_ = os.Remove(socketPath)
	log.Print("scopecache stopped")
}
