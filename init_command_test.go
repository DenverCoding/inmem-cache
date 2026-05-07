// Init-command tests use a real shell script in a temp dir, since
// exec.CommandContext + a real fork/exec is what the helper does in
// production. We can't reasonably mock os/exec without making the
// helper test-shaped instead of operator-shaped, so the tests run on
// Unix only — Windows builds skip via the build tag.

//go:build unix

package scopecache

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
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
	err := gw.RunInitCommand(context.Background(), "", nil, func(string, ...any) { calls++ })
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
		context.Background(),
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
	err := gw.RunInitCommand(context.Background(), cmdPath, nil, func(format string, args ...any) {
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
	err := gw.RunInitCommand(context.Background(), missing, nil, nil)
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
	if err := gw.RunInitCommand(context.Background(), cmdPath, nil, nil); err != nil {
		t.Errorf("RunInitCommand with nil logf: %v", err)
	}
}

// Cancellation via context: a long-running script gets SIGKILL'd
// when the parent context is cancelled, and RunInitCommand returns
// promptly. Pre-fix the helper used exec.Command (no context), so
// SIGINT/SIGTERM during init would leave the parent stuck in
// cmd.Run() until the script exited voluntarily — a hung curl or
// tarpitted DB query could block boot indefinitely.
//
// The asserted bound is generous (3s) so the test cannot flake on
// CI load while still being orders of magnitude below the
// "infinitely-hanging" failure mode the fix targets.
func TestRunInitCommand_ContextCancelStopsHungScript(t *testing.T) {
	dir := t.TempDir()
	cmdPath := filepath.Join(dir, "hang.sh")
	if err := os.WriteFile(cmdPath, []byte("#!/bin/sh\nsleep 60\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	gw := NewGateway(Config{})
	ctx, cancel := context.WithCancel(context.Background())

	// Cancel after 200ms — long enough that the script is definitely
	// running (the sleep has begun) but short enough that the test
	// doesn't drag.
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	t0 := time.Now()
	err := gw.RunInitCommand(ctx, cmdPath, nil, nil)
	elapsed := time.Since(t0)

	if err == nil {
		t.Fatal("expected error after cancel; got nil")
	}
	if elapsed > 3*time.Second {
		t.Errorf("RunInitCommand returned after %v; want < 3s after cancel", elapsed)
	}
}

// Process-group kill: a script that backgrounds children must have
// its whole process group reaped on cancel. Pre-fix only the direct
// child got SIGKILL'd; a script doing `sleep 60 &` would orphan the
// sleep onto PID 1. configureProcessGroup wraps the cmd in a new
// process group so the cancel kills the negative PID (whole group).
//
// Repro: shell that backgrounds `sleep 60`, writes its PID, waits.
// After cancel, assert the PID is no longer running.
func TestRunInitCommand_CancelReapsBackgroundedChildren(t *testing.T) {
	dir := t.TempDir()
	cmdPath := filepath.Join(dir, "fork.sh")
	pidFile := filepath.Join(dir, "child.pid")
	body := "#!/bin/sh\n" +
		"sleep 60 &\n" +
		"echo $! > " + pidFile + "\n" +
		"wait\n"
	if err := os.WriteFile(cmdPath, []byte(body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	gw := NewGateway(Config{})
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- gw.RunInitCommand(ctx, cmdPath, nil, nil)
	}()

	// Wait until the child PID has been written.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(pidFile); err == nil && len(strings.TrimSpace(string(data))) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		cancel()
		t.Fatalf("read pid file: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		cancel()
		t.Fatalf("parse pid %q: %v", string(pidBytes), err)
	}

	if !pidIsRunning(pid) {
		cancel()
		t.Fatalf("backgrounded child pid=%d not running before cancel — test setup did not reproduce orphan scenario", pid)
	}

	cancel()
	<-done

	// After cancel, the group kill should have hit the sleep too.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !pidIsRunning(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Best-effort cleanup so a regressed test doesn't leave a 60s sleep.
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Errorf("backgrounded child pid=%d still running 2s after cancel — process group not reaped", pid)
}
