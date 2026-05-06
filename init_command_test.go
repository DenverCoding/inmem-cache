// Init-command tests use a real shell script in a temp dir, since
// exec.Command + a real fork/exec is what the helper does in
// production. We can't reasonably mock os/exec without making the
// helper test-shaped instead of operator-shaped, so the tests run on
// Unix only — Windows builds skip via the build tag.

//go:build unix

package scopecache

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeInitCommandHelper creates a `chmod +x` script in dir that
// records its environment to outFile. One line per invocation; the
// line includes the SCOPECACHE_SOCKET_PATH env var so tests can
// verify extraEnv was forwarded.
func writeInitCommandHelper(t *testing.T, dir, outFile string) string {
	t.Helper()
	path := filepath.Join(dir, "init.sh")
	body := "#!/bin/sh\necho \"sock=$SCOPECACHE_SOCKET_PATH\" >> " + outFile + "\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return path
}

// Empty command is a no-op: returns nil and never invokes logf.
func TestRunInitCommand_EmptyIsNoOp(t *testing.T) {
	gw := NewGateway(Config{})
	calls := 0
	err := gw.RunInitCommand("", nil, func(string, ...any) { calls++ })
	if err != nil {
		t.Errorf("err = %v, want nil for empty command", err)
	}
	if calls != 0 {
		t.Errorf("logf called %d times for empty command, want 0", calls)
	}
}

// Happy path: script runs sync, env vars from extraEnv reach it,
// logf sees the running + completed lines, return value is nil.
func TestRunInitCommand_RunsScriptAndForwardsEnv(t *testing.T) {
	dir := t.TempDir()
	outFile := filepath.Join(dir, "out.log")
	command := writeInitCommandHelper(t, dir, outFile)

	gw := NewGateway(Config{})
	var lines []string
	err := gw.RunInitCommand(
		command,
		[]string{"SCOPECACHE_SOCKET_PATH=/tmp/foo.sock"},
		func(format string, args ...any) {
			lines = append(lines, format)
		},
	)
	if err != nil {
		t.Fatalf("RunInitCommand: %v", err)
	}

	out, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read %s: %v", outFile, err)
	}
	want := "sock=/tmp/foo.sock"
	if !strings.Contains(string(out), want) {
		t.Errorf("script output = %q, want substring %q (extraEnv not forwarded?)", string(out), want)
	}

	// Two log lines on success: "running" + "completed".
	if len(lines) != 2 {
		t.Errorf("logf calls = %d, want 2 (running + completed); got %v", len(lines), lines)
	}
}

// Script that exits non-zero produces an error AND a log line.
func TestRunInitCommand_FailureReturnsError(t *testing.T) {
	dir := t.TempDir()
	cmdPath := filepath.Join(dir, "fail.sh")
	body := "#!/bin/sh\nexit 1\n"
	if err := os.WriteFile(cmdPath, []byte(body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	gw := NewGateway(Config{})
	var lines []string
	err := gw.RunInitCommand(cmdPath, nil, func(format string, args ...any) {
		lines = append(lines, format)
	})
	if err == nil {
		t.Fatal("expected error from failing script; got nil")
	}
	// Two log lines on failure: "running" + the error line.
	if len(lines) != 2 {
		t.Errorf("logf calls = %d, want 2 (running + error); got %v", len(lines), lines)
	}
}

// Missing executable: cmd.Run reports an exec error which the helper
// wraps and returns. Pins the contract that we don't pre-stat the
// path; operators who deploy the script after Caddy starts only see
// the failure on the next reload.
func TestRunInitCommand_MissingPathReturnsError(t *testing.T) {
	gw := NewGateway(Config{})
	missing := filepath.Join(t.TempDir(), "does-not-exist.sh")
	err := gw.RunInitCommand(missing, nil, nil)
	if err == nil {
		t.Fatal("expected error for missing executable; got nil")
	}
}

// Nil logf is a supported shape — both adapters pass a non-nil
// logger today, but the helper must not panic if a future caller
// (or a test) passes nil. The "running"/"completed" lines are simply
// dropped.
func TestRunInitCommand_NilLogfDoesNotPanic(t *testing.T) {
	dir := t.TempDir()
	cmdPath := filepath.Join(dir, "ok.sh")
	if err := os.WriteFile(cmdPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	gw := NewGateway(Config{})
	if err := gw.RunInitCommand(cmdPath, nil, nil); err != nil {
		t.Errorf("RunInitCommand with nil logf: %v", err)
	}
}
