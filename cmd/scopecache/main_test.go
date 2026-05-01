package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// listenUnixSocket must remove a stale Unix socket left behind by a prior
// crash, but must refuse to remove a regular file at the configured path.
// The latter is a defence against misconfigured SCOPECACHE_SOCKET_PATH —
// the standalone binary often runs as root and would otherwise be a
// blunt instrument against arbitrary system files.

func TestListenUnixSocket_FreshPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "scopecache.sock")

	ln, err := listenUnixSocket(path)
	if err != nil {
		t.Fatalf("listenUnixSocket on fresh path: %v", err)
	}
	defer ln.Close()

	if _, err := os.Stat(path); err != nil {
		t.Errorf("socket was not created at %s: %v", path, err)
	}
}

func TestListenUnixSocket_StaleSocketIsReplaced(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "scopecache.sock")

	// First listener creates the socket file, then we close it without
	// removing the file — same shape as a non-graceful shutdown.
	// Linux's net.UnixListener.Close() unlinks by default; turn that off
	// so the socket file persists like it would after a SIGKILL.
	first, err := listenUnixSocket(path)
	if err != nil {
		t.Fatalf("first listenUnixSocket: %v", err)
	}
	if ul, ok := first.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	first.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected stale socket file at %s: %v", path, err)
	}

	// Second call must replace the stale socket and succeed.
	second, err := listenUnixSocket(path)
	if err != nil {
		t.Fatalf("listenUnixSocket on stale socket: %v", err)
	}
	defer second.Close()
}

func TestListenUnixSocket_RefusesRegularFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "not-a-socket.txt")

	if err := os.WriteFile(path, []byte("important data"), 0644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	ln, err := listenUnixSocket(path)
	if err == nil {
		ln.Close()
		t.Fatal("listenUnixSocket succeeded against a regular file; expected refusal")
	}

	if !strings.Contains(err.Error(), "non-socket") {
		t.Errorf("error message does not mention non-socket file: %v", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error message does not include the path: %v", err)
	}
	if !strings.Contains(err.Error(), "SCOPECACHE_SOCKET_PATH") {
		t.Errorf("error message does not mention the env var: %v", err)
	}

	// File must still be there — the whole point.
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("regular file disappeared: %v", err)
	}
	if string(body) != "important data" {
		t.Errorf("regular file was modified; content=%q", body)
	}
}

func TestListenUnixSocket_RefusesDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir")
	if err := os.Mkdir(path, 0755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	ln, err := listenUnixSocket(path)
	if err == nil {
		ln.Close()
		t.Fatal("listenUnixSocket succeeded against a directory; expected refusal")
	}

	// Directory must still be there.
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		t.Errorf("directory disappeared or is no longer a directory: %v", err)
	}
}
