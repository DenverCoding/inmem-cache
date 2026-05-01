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
	"strings"
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

// maxItemBytesFromEnv returns SCOPECACHE_MAX_ITEM_MB (in MiB, converted to bytes)
// if set to a positive integer, otherwise the compile-time default. Operators
// raise this when the use-case stores larger blobs (rendered HTML, large JSON
// documents); the single-item HTTP body cap scales with it automatically via
// singleRequestBytesFor.
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

// maxResponseBytesFromEnv returns SCOPECACHE_MAX_RESPONSE_MB (in MiB,
// converted to bytes) if set to a positive integer, otherwise the
// compile-time default. Caps the byte size of /head, /tail, and /ts_range
// responses; values past the cap are rejected with 507 Insufficient
// Storage rather than streamed truncated. Same lenient policy as the
// other env helpers: malformed or non-positive values log a warning and
// the server keeps running on the default.
func maxResponseBytesFromEnv() int64 {
	raw := os.Getenv("SCOPECACHE_MAX_RESPONSE_MB")
	if raw == "" {
		return int64(scopecache.MaxResponseMiB) << 20
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		log.Printf("SCOPECACHE_MAX_RESPONSE_MB=%q is not a positive integer; using default %d MiB", raw, scopecache.MaxResponseMiB)
		return int64(scopecache.MaxResponseMiB) << 20
	}
	return int64(n) << 20
}

// maxMultiCallBytesFromEnv returns SCOPECACHE_MAX_MULTI_CALL_MB (in MiB,
// converted to bytes) if set to a positive integer, otherwise the
// compile-time default. Caps the input body size of /multi_call; the
// outer response is bounded separately by SCOPECACHE_MAX_RESPONSE_MB.
func maxMultiCallBytesFromEnv() int64 {
	raw := os.Getenv("SCOPECACHE_MAX_MULTI_CALL_MB")
	if raw == "" {
		return int64(scopecache.MaxMultiCallMiB) << 20
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		log.Printf("SCOPECACHE_MAX_MULTI_CALL_MB=%q is not a positive integer; using default %d MiB", raw, scopecache.MaxMultiCallMiB)
		return int64(scopecache.MaxMultiCallMiB) << 20
	}
	return int64(n) << 20
}

// maxMultiCallCountFromEnv returns SCOPECACHE_MAX_MULTI_CALL_COUNT if set to
// a positive integer, otherwise the compile-time default. Caps the number of
// sub-calls per /multi_call batch.
func maxMultiCallCountFromEnv() int {
	raw := os.Getenv("SCOPECACHE_MAX_MULTI_CALL_COUNT")
	if raw == "" {
		return scopecache.MaxMultiCallCount
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		log.Printf("SCOPECACHE_MAX_MULTI_CALL_COUNT=%q is not a positive integer; using default %d", raw, scopecache.MaxMultiCallCount)
		return scopecache.MaxMultiCallCount
	}
	return n
}

// disableReadHeatFromEnv reads SCOPECACHE_DISABLE_READ_HEAT. Default
// is false — heat tracking is on, so /delete_scope_candidates ranks
// scopes correctly. Set "1", "true", "yes", or "on" (case-insensitive)
// to skip recordRead on the hot read path; saves the time.Now() call
// plus the heat counters' atomic adds, at the cost of always-zero
// last_access_ts / last_7d_read_count / read_count_total in /stats.
// Useful for deployments that don't use /delete_scope_candidates.
func disableReadHeatFromEnv() bool {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("SCOPECACHE_DISABLE_READ_HEAT")))
	switch raw {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// enableAdminFromEnv reads SCOPECACHE_ENABLE_ADMIN. Default is true:
// the standalone binary listens on a Unix socket, which the operator
// gates with filesystem permissions (`chmod 0660 /run/scopecache.sock`,
// owner = the operator account). The Caddy module defaults this to
// false because a public reverse-proxy without an explicit /admin
// route guard is the common deployment footgun. Recognised "off"
// values: "0", "false", "no", "off" (case-insensitive). Anything else
// (including unset) leaves the default in place.
func enableAdminFromEnv() bool {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("SCOPECACHE_ENABLE_ADMIN")))
	switch raw {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// maxInboxBytesFromEnv returns SCOPECACHE_MAX_INBOX_KB (in KiB,
// converted to bytes) if set to a positive integer, otherwise the
// compile-time default. KiB-granular (not MiB like the other byte
// knobs) because the meaningful range is sub-MiB — /inbox is meant
// for small fire-and-forget event payloads, not blob uploads. Same
// lenient policy as the other env helpers: malformed or non-positive
// values log a warning and the server keeps running on the default.
func maxInboxBytesFromEnv() int64 {
	raw := os.Getenv("SCOPECACHE_MAX_INBOX_KB")
	if raw == "" {
		return int64(scopecache.MaxInboxKiB) << 10
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		log.Printf("SCOPECACHE_MAX_INBOX_KB=%q is not a positive integer; using default %d KiB", raw, scopecache.MaxInboxKiB)
		return int64(scopecache.MaxInboxKiB) << 10
	}
	return int64(n) << 10
}

// inboxScopesFromEnv returns SCOPECACHE_INBOX_SCOPES split on
// newlines (the robust separator — scope names may contain ":" or
// "," but not "\n"). Empty value, missing var, or all-whitespace
// content yields nil, which leaves /inbox unregistered.
func inboxScopesFromEnv() []string {
	raw := os.Getenv("SCOPECACHE_INBOX_SCOPES")
	if raw == "" {
		return nil
	}
	var out []string
	for _, s := range strings.Split(raw, "\n") {
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
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
	}
	apiCfg := scopecache.APIConfig{
		MaxResponseBytes:  maxResponseBytesFromEnv(),
		MaxMultiCallBytes: maxMultiCallBytesFromEnv(),
		MaxMultiCallCount: maxMultiCallCountFromEnv(),
		ServerSecret:      os.Getenv("SCOPECACHE_SERVER_SECRET"),
		InboxScopes:       inboxScopesFromEnv(),
		MaxInboxBytes:     maxInboxBytesFromEnv(),
		EnableAdmin:       enableAdminFromEnv(),
		DisableReadHeat:   disableReadHeatFromEnv(),
	}
	store := scopecache.NewStore(cfg)
	api := scopecache.NewAPI(store, apiCfg)

	adminStatus := "DISABLED (SCOPECACHE_ENABLE_ADMIN=off)"
	if apiCfg.EnableAdmin {
		adminStatus = "enabled (gated by Unix-socket permissions)"
	}
	guardedStatus := "DISABLED (SCOPECACHE_SERVER_SECRET unset)"
	if apiCfg.ServerSecret != "" {
		guardedStatus = "enabled"
	}
	inboxStatus := "DISABLED (SCOPECACHE_INBOX_SCOPES unset)"
	if apiCfg.ServerSecret == "" {
		inboxStatus = "DISABLED (SCOPECACHE_SERVER_SECRET unset)"
	} else if len(apiCfg.InboxScopes) > 0 {
		inboxStatus = "enabled, scopes=" + strings.Join(apiCfg.InboxScopes, ",")
	}
	log.Printf("scopecache capacity: %d items per scope, %d MiB store-wide, %d MiB per item, %d MiB per response, %d MiB per /multi_call body, %d sub-calls per /multi_call batch, %d KiB per /inbox payload", cfg.ScopeMaxItems, cfg.MaxStoreBytes>>20, cfg.MaxItemBytes>>20, apiCfg.MaxResponseBytes>>20, apiCfg.MaxMultiCallBytes>>20, apiCfg.MaxMultiCallCount, apiCfg.MaxInboxBytes>>10)
	heatStatus := "enabled"
	if apiCfg.DisableReadHeat {
		heatStatus = "DISABLED (SCOPECACHE_DISABLE_READ_HEAT=on; /delete_scope_candidates won't have ranking data)"
	}
	log.Printf("scopecache features: /admin %s; /guarded %s; /inbox %s; read-heat %s", adminStatus, guardedStatus, inboxStatus, heatStatus)

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
