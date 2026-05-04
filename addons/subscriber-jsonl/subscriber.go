// Package subscriberjsonl is a minimal reference subscriber addon: it
// subscribes to one of the cache's reserved scopes (`_events` or
// `_inbox`), waits a coalescing delay after each wake-up, drains the
// scope via Head + DeleteUpTo, and writes each batch to a timestamped
// JSONL file.
//
// This is the simplest possible subscriber that's actually useful —
// no filtering, no transformation, no fsync, no retry policy beyond
// "skip the batch and try again on the next wake-up". Production
// deployments needing those features should write their own
// subscriber against the *scopecache.Gateway API; this one exists as
// a reference + as a stdlib-only default that operators can drop in
// without committing to a sink choice (file format, DB driver, HTTP
// endpoint).
//
// Usage:
//
//	gw := scopecache.NewGateway(scopecache.Config{
//	    Events: scopecache.EventsConfig{Mode: scopecache.EventsModeFull},
//	    // ... caps
//	})
//	stop, err := subscriberjsonl.Start(gw, subscriberjsonl.Config{
//	    Scope: scopecache.EventsScopeName,
//	    Dir:   "/var/log/scopecache",
//	})
//	if err != nil { log.Fatal(err) }
//	defer stop()
//
// Files are named `<scope>-<unix-microseconds>.jsonl`, sortable by
// time, one batch per file. JSONL means one JSON-encoded Item per
// line, terminated by '\n' (encoding/json's Encoder default).
package subscriberjsonl

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/VeloxCoding/scopecache"
)

// Config bundles the operator-tunable knobs. Scope and Dir are
// required; the other fields default to sensible values.
type Config struct {
	// Scope to subscribe to. Must be a reserved scope (the cache only
	// allows Subscribe on `_events` and `_inbox`); anything else is
	// rejected by Gateway.Subscribe with ErrInvalidSubscribeScope.
	Scope string

	// Dir is the output directory for JSONL files. Created if it does
	// not exist; permission errors are returned from Start.
	Dir string

	// DrainDelay is how long to wait after each wake-up before
	// draining. Bursts of writes within this window coalesce into one
	// batch / one file. Default 1s if zero.
	DrainDelay time.Duration

	// BatchLimit caps each Head request. The drain loop calls Head in
	// a loop until empty, so larger pages mean fewer round-trips +
	// fewer files; smaller pages mean more files + better recovery
	// granularity. Default 1000 if zero or negative.
	BatchLimit int
}

// Start spawns a subscriber goroutine that runs until the returned
// stop function is called. The goroutine subscribes once at start,
// drains any pending items pre-subscribe, then loops on wake-ups.
//
// stop() closes the subscription, which closes the wake-up channel,
// which exits the goroutine's `for range` loop. After stop() returns
// it is safe to assume no more writes will happen to Dir from this
// subscriber. (An in-flight drain that started before stop() may
// still complete a few file writes — stop is graceful, not abortive.)
//
// Errors returned from Start come from one of:
//   - Config validation (Scope or Dir empty).
//   - Dir mkdir failure.
//   - Gateway.Subscribe failure (ErrInvalidSubscribeScope on a
//     non-reserved scope, ErrAlreadySubscribed on a duplicate).
func Start(gw *scopecache.Gateway, cfg Config) (stop func(), err error) {
	if gw == nil {
		return nil, errors.New("subscriberjsonl: gateway is nil")
	}
	if cfg.Scope == "" {
		return nil, errors.New("subscriberjsonl: Scope is required")
	}
	if cfg.Dir == "" {
		return nil, errors.New("subscriberjsonl: Dir is required")
	}
	if cfg.DrainDelay == 0 {
		cfg.DrainDelay = time.Second
	}
	if cfg.BatchLimit <= 0 {
		cfg.BatchLimit = 1000
	}

	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("subscriberjsonl: mkdir %s: %w", cfg.Dir, err)
	}

	ch, unsub, err := gw.Subscribe(cfg.Scope)
	if err != nil {
		return nil, fmt.Errorf("subscriberjsonl: subscribe %s: %w", cfg.Scope, err)
	}

	go func() {
		// Initial drain — items might have accumulated before Subscribe
		// was called, so we don't wait for the first wake-up.
		drainAll(gw, cfg)

		// Wake-up loop. The DrainDelay sleep is the coalescing window:
		// a burst of N writes within DrainDelay collapses into one
		// drain + one file (the wake-up channel is single-slot
		// non-blocking, so subsequent writes during the sleep are
		// folded into the slot we already have).
		for range ch {
			time.Sleep(cfg.DrainDelay)
			drainAll(gw, cfg)
		}
	}()

	return unsub, nil
}

// drainAll loops Head + writeBatch + DeleteUpTo until the scope is
// empty. Each iteration is one file. On any error it returns early
// and waits for the next wake-up to retry — no in-loop retry, no
// exponential backoff, no logging. Intentional simplicity.
func drainAll(gw *scopecache.Gateway, cfg Config) {
	for {
		items, _, _ := gw.Head(cfg.Scope, 0, cfg.BatchLimit)
		if len(items) == 0 {
			return
		}
		if err := writeBatch(items, cfg); err != nil {
			// File-create / write-line / close error — leave items in
			// scope, retry on next wake-up. The next file gets a fresh
			// timestamp so partial-write artifacts (if any) don't
			// collide.
			return
		}
		// DeleteUpTo's `max_seq` is the last item's seq we just wrote.
		// Items with greater seqs (added after our Head call) stay
		// for the next loop iteration.
		if _, err := gw.DeleteUpTo(cfg.Scope, items[len(items)-1].Seq); err != nil {
			return
		}
	}
}

// writeBatch creates one file and encodes items as JSONL. Each item
// becomes one line: `{"scope":...,"id":...,"seq":...,"ts":...,"payload":...}\n`.
// json.Encoder appends '\n' after each value by default, so the
// output is line-delimited without manual separator handling.
func writeBatch(items []scopecache.Item, cfg Config) error {
	fname := filepath.Join(cfg.Dir,
		fmt.Sprintf("%s-%d.jsonl", cfg.Scope, time.Now().UnixMicro()))
	f, err := os.Create(fname)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	for i := range items {
		if err := enc.Encode(items[i]); err != nil {
			_ = f.Close()
			return err
		}
	}
	return f.Close()
}
