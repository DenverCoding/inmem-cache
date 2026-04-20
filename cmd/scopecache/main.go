package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/DenverCoding/scopecache"
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
// if set to a positive integer, otherwise the compile-time default. Same
// lenient policy as scopeMaxItemsFromEnv: malformed or non-positive values log
// a warning and the server keeps running on the default.
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

const shutdownGracePeriod = 5 * time.Second

func listenUnixSocket(path string) (net.Listener, error) {
	dir := filepath.Dir(path)

	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}

	// Unix socket files persist after a crash or non-graceful shutdown; a new
	// listen() on the same path would fail with "address already in use", so we
	// remove any stale file first. ErrNotExist is the normal first-boot case.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
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
	maxItems := scopeMaxItemsFromEnv()
	maxStoreBytes := maxStoreBytesFromEnv()
	store := scopecache.NewStore(maxItems, maxStoreBytes)
	api := scopecache.NewAPI(store)
	log.Printf("scopecache capacity: %d items per scope, %d MiB store-wide", maxItems, maxStoreBytes>>20)

	mux := http.NewServeMux()
	api.RegisterRoutes(mux)

	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 3 * time.Second,
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
