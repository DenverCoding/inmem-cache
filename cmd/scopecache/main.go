package main

import (
	"context"
	"errors"
	"fmt"
	"log"
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
	return int64(n) << 20
}

// maxItemBytesFromEnv returns SCOPECACHE_MAX_ITEM_MB (in MiB, converted to bytes)
// if set to a positive integer, otherwise the compile-time default.
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

	// Activate the in-core subscriber bridge when
	// SCOPECACHE_SUBSCRIBER_COMMAND is set. Stopped explicitly after
	// server.Serve returns so subscribers are torn down once the HTTP
	// layer is no longer accepting writes — otherwise an in-flight
	// command invocation could outlive the cache.
	stopSubscribers := gw.StartReservedSubscribers(subscriberCommandFromEnv(), log.Printf)
	defer stopSubscribers()

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
	ln, err := listenUnixSocket(socketPath)
	if err != nil {
		log.Fatal(err)
	}

	// SIGINT/SIGTERM triggers Shutdown, which drains in-flight requests.
	// Using log.Fatal here would bypass the shutdown path entirely because it
	// calls os.Exit and skips deferred cleanup.
	signalCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	go func() {
		<-signalCtx.Done()
		log.Print("scopecache shutting down")
		// Stop subscribers FIRST, while the HTTP server is still up.
		// stopSubscribers blocks until each goroutine has finished its
		// current cmd.Run, so any in-flight drain command completes its
		// curl roundtrips against a still-listening cache — no orphan
		// curl ever hits a torn-down socket. unsubscribe is idempotent
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
	if err := server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("serve error: %v", err)
	}

	_ = ln.Close()
	_ = os.Remove(socketPath)
	log.Print("scopecache stopped")
}
