// Subscriber-command tests use a real shell script in a temp dir,
// since exec.Command + a real fork/exec is what the bridge does in
// production. We can't reasonably mock os/exec without making the
// bridge test-shaped instead of operator-shaped, so the tests run on
// Unix only — Windows builds skip via the build tag.

//go:build unix

package scopecache

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// writeSubscriberCommandHelper creates a `chmod +x` script in dir
// that appends one line per invocation to outFile. The line includes
// the SCOPECACHE_SCOPE env var so tests can verify it was set.
func writeSubscriberCommandHelper(t *testing.T, dir, outFile string) string {
	t.Helper()
	path := filepath.Join(dir, "drain.sh")
	body := "#!/bin/sh\necho \"scope=$SCOPECACHE_SCOPE\" >> " + outFile + "\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return path
}

// readSubscriberCommandLines returns the trimmed, non-empty lines of
// file. Empty file or missing file returns an empty slice (the
// command may not have fired yet).
func readSubscriberCommandLines(t *testing.T, file string) []string {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read %s: %v", file, err)
	}
	out := []string{}
	for _, line := range strings.Split(string(data), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// waitFor polls f every 10ms until it returns true or timeout fires.
// Returns true on success, false on timeout.
func waitForSubscriberCommand(timeout time.Duration, f func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if f() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return f()
}

// --- Validation tests --------------------------------------------------------

func TestStartSubscriber_RequiresScope(t *testing.T) {
	gw := NewGateway(Config{Events: EventsConfig{Mode: EventsModeFull}})
	_, err := gw.StartSubscriber("", "/bin/true")
	if err == nil {
		t.Fatal("expected error on empty scope")
	}
}

func TestStartSubscriber_RequiresCommand(t *testing.T) {
	gw := NewGateway(Config{Events: EventsConfig{Mode: EventsModeFull}})
	_, err := gw.StartSubscriber(EventsScopeName, "")
	if err == nil {
		t.Fatal("expected error on empty command")
	}
}

func TestStartSubscriber_RejectsNonReservedScope(t *testing.T) {
	gw := NewGateway(Config{})
	_, err := gw.StartSubscriber("user-scope", "/bin/true")
	if err == nil {
		t.Fatal("expected error on non-reserved scope")
	}
	if !errors.Is(err, ErrInvalidSubscribeScope) {
		t.Errorf("err = %v, want wraps ErrInvalidSubscribeScope", err)
	}
}

// --- Behaviour tests ---------------------------------------------------------

// Command fires once per wake-up, with SCOPECACHE_SCOPE set.
func TestStartSubscriber_CommandInvokedOnWakeup(t *testing.T) {
	dir := t.TempDir()
	outFile := filepath.Join(dir, "out.log")
	command := writeSubscriberCommandHelper(t, dir, outFile)

	gw := NewGateway(Config{Events: EventsConfig{Mode: EventsModeFull}})
	stop, err := gw.StartSubscriber(EventsScopeName, command)
	if err != nil {
		t.Fatalf("StartSubscriber: %v", err)
	}
	defer stop()

	// Trigger one wake-up by writing into a non-reserved scope (cache
	// auto-populates _events on every write when EventsModeFull).
	if _, err := gw.Append(Item{Scope: "trigger", Payload: []byte(`"x"`)}); err != nil {
		t.Fatalf("append: %v", err)
	}

	if !waitForSubscriberCommand(2*time.Second, func() bool {
		return len(readSubscriberCommandLines(t, outFile)) >= 1
	}) {
		t.Fatal("command never invoked")
	}

	lines := readSubscriberCommandLines(t, outFile)
	if len(lines) == 0 || lines[0] != "scope=_events" {
		t.Errorf("first line = %q, want %q", strings.Join(lines, "\n"), "scope=_events")
	}
}

// A burst of writes coalesces into fewer command invocations than
// individual writes. The exact number depends on host timing — we
// just assert "fewer than the number of writes" to pin the
// coalescing contract without flaking on timing variance.
func TestStartSubscriber_BurstCoalesces(t *testing.T) {
	dir := t.TempDir()
	outFile := filepath.Join(dir, "out.log")
	// Slow command: each invocation sleeps 50ms before writing the line,
	// so a burst of writes lands in the cache while the command is
	// still running and the wake-ups coalesce in the channel.
	cmdPath := filepath.Join(dir, "slow.sh")
	body := "#!/bin/sh\nsleep 0.05\necho \"hit\" >> " + outFile + "\n"
	if err := os.WriteFile(cmdPath, []byte(body), 0o755); err != nil {
		t.Fatalf("write slow command: %v", err)
	}

	gw := NewGateway(Config{Events: EventsConfig{Mode: EventsModeFull}})
	stop, err := gw.StartSubscriber(EventsScopeName, cmdPath)
	if err != nil {
		t.Fatalf("StartSubscriber: %v", err)
	}
	defer stop()

	const writes = 50
	for i := 0; i < writes; i++ {
		if _, err := gw.Append(Item{Scope: "trigger", Payload: []byte(`"x"`)}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	// Wait until the line-count is stable for ~250ms (5 × 50ms with
	// no change = drained). Cap at 5s as worst-case sanity bound.
	stable := 0
	prev := -1
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		cur := len(readSubscriberCommandLines(t, outFile))
		if cur == prev && cur > 0 {
			stable++
			if stable >= 5 {
				break
			}
		} else {
			stable = 0
			prev = cur
		}
		time.Sleep(50 * time.Millisecond)
	}

	lines := readSubscriberCommandLines(t, outFile)
	hits := len(lines)
	if hits == 0 {
		t.Fatal("command never ran")
	}
	if hits >= writes {
		t.Errorf("hits=%d, expected coalescing (< %d writes)", hits, writes)
	}
	t.Logf("burst coalescing: %d writes -> %d command invocations", writes, hits)
}

// stop() makes the goroutine exit. We can detect it by Subscribe-ing
// the same scope after stop() — Gateway returns ErrAlreadySubscribed
// while the bridge's Subscribe is live, and lets us re-subscribe once
// the bridge has unsubscribed.
func TestStartSubscriber_StopExitsGoroutine(t *testing.T) {
	gw := NewGateway(Config{})
	stop, err := gw.StartSubscriber(EventsScopeName, "/bin/true")
	if err != nil {
		t.Fatalf("StartSubscriber: %v", err)
	}

	// While bridge is running, a second subscribe must fail.
	if _, _, err := gw.Subscribe(EventsScopeName); err == nil {
		t.Fatal("expected ErrAlreadySubscribed while bridge is live")
	}

	stop()

	// After stop, re-subscribe should succeed within a reasonable
	// window. The unsubscribe path closes the channel; the goroutine's
	// `for range ch` exits; but the unsub hook itself synchronously
	// removes the subscriber slot, so re-subscribe should be available
	// immediately.
	if !waitForSubscriberCommand(time.Second, func() bool {
		ch, unsub, err := gw.Subscribe(EventsScopeName)
		if err != nil {
			return false
		}
		_ = ch
		unsub()
		return true
	}) {
		t.Fatal("could not re-subscribe after stop()")
	}
}

// A non-existent command path is logged but does not block future
// wake-ups. We test by checking the goroutine is still alive after
// multiple wake-ups (Subscribe slot still occupied).
func TestStartSubscriber_MissingCommandDoesNotStallLoop(t *testing.T) {
	gw := NewGateway(Config{Events: EventsConfig{Mode: EventsModeFull}})

	missing := filepath.Join(t.TempDir(), "nonexistent.sh")
	stop, err := gw.StartSubscriber(EventsScopeName, missing)
	if err != nil {
		t.Fatalf("StartSubscriber: %v", err)
	}
	defer stop()

	for i := 0; i < 10; i++ {
		if _, err := gw.Append(Item{Scope: "t", Payload: []byte(`"x"`)}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if _, _, err := gw.Subscribe(EventsScopeName); err == nil {
		t.Fatal("bridge's Subscribe slot is gone — goroutine exited unexpectedly")
	}
}

// Stress test: many writes + slow command + concurrent Append. Just
// verifies the bridge doesn't deadlock or panic. Counts hits as a
// sanity check.
func TestStartSubscriber_StressManyWrites(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test")
	}

	dir := t.TempDir()
	outFile := filepath.Join(dir, "stress.log")
	command := writeSubscriberCommandHelper(t, dir, outFile)

	gw := NewGateway(Config{Events: EventsConfig{Mode: EventsModeFull}})
	stop, err := gw.StartSubscriber(EventsScopeName, command)
	if err != nil {
		t.Fatalf("StartSubscriber: %v", err)
	}
	defer stop()

	var written int64
	const writers = 4
	const writesPerWriter = 200
	done := make(chan struct{})
	for w := 0; w < writers; w++ {
		go func(id int) {
			for i := 0; i < writesPerWriter; i++ {
				if _, err := gw.Append(Item{
					Scope:   "stress",
					Payload: []byte(`"x"`),
				}); err == nil {
					atomic.AddInt64(&written, 1)
				}
				if i%50 == 0 {
					time.Sleep(time.Millisecond)
				}
			}
			done <- struct{}{}
		}(w)
	}
	for w := 0; w < writers; w++ {
		<-done
	}
	t.Logf("writers committed: %d / %d", atomic.LoadInt64(&written), int64(writers*writesPerWriter))

	// Wait until idle (line count stable for ~500ms).
	stable := 0
	prev := -1
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		cur := len(readSubscriberCommandLines(t, outFile))
		if cur == prev && cur > 0 {
			stable++
			if stable >= 10 {
				break
			}
		} else {
			stable = 0
			prev = cur
		}
		time.Sleep(50 * time.Millisecond)
	}

	hits := len(readSubscriberCommandLines(t, outFile))
	if hits == 0 {
		t.Fatal("command never ran under stress")
	}
	t.Logf("stress: %d writes -> %d command invocations", atomic.LoadInt64(&written), hits)
}
