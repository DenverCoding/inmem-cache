package subscriberjsonl

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/VeloxCoding/scopecache"
)

// newGW builds a *Gateway with events_mode=full so user-scope writes
// produce events into _events that the subscriber drains.
func newGW(t *testing.T) *scopecache.Gateway {
	t.Helper()
	return scopecache.NewGateway(scopecache.Config{
		ScopeMaxItems: 1000,
		MaxStoreBytes: 100 << 20,
		MaxItemBytes:  1 << 20,
		Events:        scopecache.EventsConfig{Mode: scopecache.EventsModeFull},
	})
}

// readAllJSONL reads every .jsonl file in dir, splits on newline,
// and returns the lines (one per item written). Used to count + read
// what the subscriber wrote without depending on file-name details.
func readAllJSONL(t *testing.T, dir string) [][]byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var lines [][]byte
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("ReadFile %s: %v", e.Name(), err)
		}
		// Split on '\n'; trailing empty line is the file-final newline.
		for _, line := range bytes.Split(data, []byte{'\n'}) {
			if len(line) == 0 {
				continue
			}
			lines = append(lines, line)
		}
	}
	return lines
}

// Happy path: subscribe to _events, do 3 user-scope appends (each
// auto-populates one event), wait for the coalesce + drain, verify
// one or more JSONL files were written containing exactly 3 events.
func TestStart_DrainsToJSONL(t *testing.T) {
	tmp := t.TempDir()
	gw := newGW(t)

	stop, err := Start(gw, Config{
		Scope:      scopecache.EventsScopeName,
		Dir:        tmp,
		DrainDelay: 20 * time.Millisecond, // tight coalesce for tests
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer stop()

	for i := 0; i < 3; i++ {
		if _, err := gw.Append(scopecache.Item{
			Scope:   "posts",
			ID:      "p-" + string(rune('0'+i)),
			Payload: json.RawMessage(`{"v":1}`),
		}); err != nil {
			t.Fatalf("Append #%d: %v", i, err)
		}
	}

	// Wait for the drain cycle (wake-up + DrainDelay + Head + write +
	// DeleteUpTo). 200ms is comfortably above DrainDelay=20ms plus
	// drain work on a quiet test host.
	time.Sleep(200 * time.Millisecond)

	lines := readAllJSONL(t, tmp)
	if len(lines) != 3 {
		t.Fatalf("got %d JSONL lines, want 3 (one per user-scope /append)", len(lines))
	}

	// Verify each line is a valid Item with the expected envelope-shape.
	for i, line := range lines {
		var item scopecache.Item
		if err := json.Unmarshal(line, &item); err != nil {
			t.Errorf("line %d not valid Item JSON: %v\n%s", i, err, line)
			continue
		}
		if item.Scope != scopecache.EventsScopeName {
			t.Errorf("line %d scope=%q want %q", i, item.Scope, scopecache.EventsScopeName)
		}
		if item.Seq == 0 {
			t.Errorf("line %d seq=0 (events_mode=full should populate seq)", i)
		}
	}

	// Items must have been deleted from _events post-drain.
	remaining, _, _ := gw.Tail(scopecache.EventsScopeName, 100, 0)
	if len(remaining) != 0 {
		t.Errorf("_events still holds %d items post-drain; want 0", len(remaining))
	}
}

// File names must start with the scope name and contain a unix-
// microsecond timestamp. This is the operator-visible naming
// convention — sortable by time, scope-prefixed for grep/ls
// filtering.
func TestStart_FileNameFormat(t *testing.T) {
	tmp := t.TempDir()
	gw := newGW(t)

	stop, err := Start(gw, Config{
		Scope:      scopecache.InboxScopeName,
		Dir:        tmp,
		DrainDelay: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer stop()

	if _, err := gw.Append(scopecache.Item{
		Scope:   scopecache.InboxScopeName,
		Payload: json.RawMessage(`{"event":"ping"}`),
	}); err != nil {
		t.Fatalf("Append _inbox: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatalf("no files written to %s", tmp)
	}

	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, scopecache.InboxScopeName+"-") {
			t.Errorf("file %q must start with %q-", name, scopecache.InboxScopeName)
		}
		if !strings.HasSuffix(name, ".jsonl") {
			t.Errorf("file %q must end with .jsonl", name)
		}
	}
}

// Coalescing: a burst of writes within DrainDelay should produce one
// file (one drain cycle) regardless of how many items the burst held.
// Verifies the wake-up coalescing + DrainDelay-sleep behaviour
// end-to-end.
func TestStart_BurstCoalesces(t *testing.T) {
	tmp := t.TempDir()
	gw := newGW(t)

	stop, err := Start(gw, Config{
		Scope:      scopecache.EventsScopeName,
		Dir:        tmp,
		DrainDelay: 100 * time.Millisecond, // wide enough to coalesce 50 writes
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer stop()

	const N = 50
	for i := 0; i < N; i++ {
		if _, err := gw.Append(scopecache.Item{
			Scope:   "burst",
			Payload: json.RawMessage(`{"x":1}`),
		}); err != nil {
			t.Fatalf("Append #%d: %v", i, err)
		}
	}

	// Wait for the single drain cycle: DrainDelay (100ms) + drain
	// work; 400ms is comfortable.
	time.Sleep(400 * time.Millisecond)

	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	jsonl := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jsonl") {
			jsonl++
		}
	}
	// Coalescing-correctness: a 50-write burst must produce SIGNIFICANTLY
	// fewer than 50 files. Exact count varies with host speed and
	// whether race-detector is on (each /append takes longer, so a
	// 100ms DrainDelay window covers fewer writes; bursts can span
	// 3-5 coalescing windows on slow hosts under -race). The test
	// asserts "much less than no-coalescing", not a specific count.
	if jsonl > 10 {
		t.Errorf("burst of %d writes produced %d files; coalescing should keep this well below 10", N, jsonl)
	}
	lines := readAllJSONL(t, tmp)
	if len(lines) != N {
		t.Errorf("got %d events on disk, want %d (every write must land)", len(lines), N)
	}
}

// Validation errors come from Start, not the goroutine.
func TestStart_ValidatesConfig(t *testing.T) {
	gw := newGW(t)

	// Missing Scope.
	if _, err := Start(gw, Config{Dir: t.TempDir()}); err == nil {
		t.Errorf("Start with empty Scope: err=nil; want validation error")
	}
	// Missing Dir.
	if _, err := Start(gw, Config{Scope: scopecache.EventsScopeName}); err == nil {
		t.Errorf("Start with empty Dir: err=nil; want validation error")
	}
	// Non-reserved scope — Gateway.Subscribe rejects.
	_, err := Start(gw, Config{Scope: "posts", Dir: t.TempDir()})
	if err == nil || !errors.Is(err, scopecache.ErrInvalidSubscribeScope) {
		t.Errorf("Start with non-reserved Scope: err=%v want ErrInvalidSubscribeScope", err)
	}
	// nil gateway.
	if _, err := Start(nil, Config{Scope: scopecache.EventsScopeName, Dir: t.TempDir()}); err == nil {
		t.Errorf("Start with nil gateway: err=nil; want validation error")
	}
}

// stop() must close the wake-up channel and let the goroutine exit.
// After stop, no new files appear even when more writes happen.
func TestStart_StopExitsGoroutine(t *testing.T) {
	tmp := t.TempDir()
	gw := newGW(t)

	stop, err := Start(gw, Config{
		Scope:      scopecache.EventsScopeName,
		Dir:        tmp,
		DrainDelay: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// One write to make sure the goroutine has woken up at least once.
	if _, err := gw.Append(scopecache.Item{
		Scope:   "posts",
		Payload: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	stop()

	// Snapshot file count post-stop.
	beforeBefore, _ := os.ReadDir(tmp)
	preCount := len(beforeBefore)

	// Drive more writes; if the goroutine were still running, they'd
	// trigger another drain → another file.
	for i := 0; i < 5; i++ {
		_, _ = gw.Append(scopecache.Item{
			Scope:   "posts",
			Payload: json.RawMessage(`{}`),
		})
	}
	time.Sleep(100 * time.Millisecond)

	after, _ := os.ReadDir(tmp)
	if len(after) != preCount {
		t.Errorf("file count grew after stop(): pre=%d post=%d (goroutine still running?)",
			preCount, len(after))
	}

	// stop() must be idempotent.
	stop()
}
