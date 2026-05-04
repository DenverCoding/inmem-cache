package scopecache

import (
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestStoreForSubscribe builds a *Store with generous caps and
// events_mode=full so writes that happen during subscribe tests trigger
// auto-populate to _events (which is what the drainer wakes up on in
// realistic deployments). Inbox-only tests can ignore events_mode.
func newTestStoreForSubscribe(t *testing.T) *Store {
	t.Helper()
	return NewStore(Config{
		ScopeMaxItems: 1000,
		MaxStoreBytes: 100 << 20,
		MaxItemBytes:  1 << 20,
		Events:        EventsConfig{Mode: EventsModeFull},
	})
}

// waitForWakeup reads one signal from ch with a short timeout. Returns
// true if the signal arrived; false on timeout. Used everywhere a test
// expects a wake-up but doesn't want to deadlock the test runner if
// the Subscribe primitive regresses.
func waitForWakeup(t *testing.T, ch <-chan struct{}, timeout time.Duration) bool {
	t.Helper()
	select {
	case <-ch:
		return true
	case <-time.After(timeout):
		return false
	}
}

// expectNoWakeup asserts no signal arrives within `quiet`. Used for
// "drainer should NOT wake up" assertions (e.g. write to a non-watched
// scope, or after unsub).
func expectNoWakeup(t *testing.T, ch <-chan struct{}, quiet time.Duration, label string) {
	t.Helper()
	select {
	case _, ok := <-ch:
		if ok {
			t.Errorf("%s: unexpected wake-up arrived (channel still open and signaled)", label)
		} else {
			t.Errorf("%s: channel was closed unexpectedly", label)
		}
	case <-time.After(quiet):
		// Good — no signal within window.
	}
}

// Subscribe to a non-reserved scope must reject — the cache's
// observability story is "everything goes through _events", subscribing
// directly to user scopes is not supported (decide-doc settled #2).
func TestSubscribe_RestrictedToReservedScopes(t *testing.T) {
	s := newTestStoreForSubscribe(t)

	for _, scope := range []string{"posts", "users:42", "_other_underscore", ""} {
		ch, unsub, err := s.Subscribe(scope)
		if !errors.Is(err, ErrInvalidSubscribeScope) {
			t.Errorf("Subscribe(%q): err=%v want ErrInvalidSubscribeScope", scope, err)
		}
		if ch != nil {
			t.Errorf("Subscribe(%q): non-nil channel returned despite error", scope)
		}
		if unsub != nil {
			t.Errorf("Subscribe(%q): non-nil unsub returned despite error", scope)
		}
	}
}

// Both reserved scopes accept Subscribe. Confirms the constants used
// by isReservedScope match what Subscribe enforces.
func TestSubscribe_AcceptsBothReservedScopes(t *testing.T) {
	s := newTestStoreForSubscribe(t)

	for _, scope := range []string{EventsScopeName, InboxScopeName} {
		ch, unsub, err := s.Subscribe(scope)
		if err != nil {
			t.Fatalf("Subscribe(%q): err=%v want nil", scope, err)
		}
		if ch == nil || unsub == nil {
			t.Fatalf("Subscribe(%q): nil channel or unsub", scope)
		}
		unsub()
	}
}

// Single-subscriber rule: a second Subscribe to the same scope while
// the first is still active returns ErrAlreadySubscribed (settled #20).
// After unsub, a fresh Subscribe succeeds — the slot is reusable.
func TestSubscribe_SingleSubscriberPerScope(t *testing.T) {
	s := newTestStoreForSubscribe(t)

	_, unsub1, err := s.Subscribe(EventsScopeName)
	if err != nil {
		t.Fatalf("first Subscribe: err=%v", err)
	}

	_, _, err = s.Subscribe(EventsScopeName)
	if !errors.Is(err, ErrAlreadySubscribed) {
		t.Errorf("second Subscribe (concurrent): err=%v want ErrAlreadySubscribed", err)
	}

	unsub1()

	// After unsub, the slot is empty again — re-subscribe must work.
	_, unsub2, err := s.Subscribe(EventsScopeName)
	if err != nil {
		t.Errorf("Subscribe after unsub: err=%v want nil (slot must be reusable)", err)
	}
	unsub2()
}

// /append to a user scope (events_mode=full) auto-populates _events,
// which fires a wake-up on the _events subscriber. End-to-end happy
// path: write to user scope → cache emits to _events → drainer wakes.
func TestSubscribe_WakesOnEventsAutoPopulate(t *testing.T) {
	s := newTestStoreForSubscribe(t)
	ch, unsub, err := s.Subscribe(EventsScopeName)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer unsub()

	if _, err := s.appendOne(Item{
		Scope:   "posts",
		ID:      "p-1",
		Payload: json.RawMessage(`{"v":1}`),
	}); err != nil {
		t.Fatalf("appendOne: %v", err)
	}

	if !waitForWakeup(t, ch, 100*time.Millisecond) {
		t.Errorf("expected wake-up after /append (events_mode=full); got timeout")
	}
}

// /append directly to _inbox wakes the _inbox subscriber. Apps push to
// _inbox; this is the fan-in path.
func TestSubscribe_WakesOnInboxWrite(t *testing.T) {
	s := newTestStoreForSubscribe(t)
	ch, unsub, err := s.Subscribe(InboxScopeName)
	if err != nil {
		t.Fatalf("Subscribe(_inbox): %v", err)
	}
	defer unsub()

	if _, err := s.appendOne(Item{
		Scope:   InboxScopeName,
		Payload: json.RawMessage(`{"event":"ping"}`),
	}); err != nil {
		t.Fatalf("appendOne(_inbox): %v", err)
	}

	if !waitForWakeup(t, ch, 100*time.Millisecond) {
		t.Errorf("expected wake-up after /append _inbox; got timeout")
	}
}

// Coalescing single-slot, 1 deep: 1000 writes while the subscriber is
// "asleep" (test reads the channel only AFTER the writes finish)
// produce at most one wake-up signal in the slot. This is the whole
// point of the design — writes don't backpressure on the drainer's
// drain rate.
func TestSubscribe_CoalescesBurstIntoOneWakeup(t *testing.T) {
	s := newTestStoreForSubscribe(t)
	ch, unsub, err := s.Subscribe(EventsScopeName)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer unsub()

	// Hammer 1000 writes to a user scope (each emits to _events under
	// events_mode=full) without reading the channel.
	for i := 0; i < 1000; i++ {
		if _, err := s.appendOne(Item{
			Scope:   "posts",
			Payload: json.RawMessage(`{"v":1}`),
		}); err != nil {
			t.Fatalf("appendOne #%d: %v", i, err)
		}
	}

	// Drain the channel — there must be exactly one signal queued
	// (the slot was filled by the first write, every subsequent write
	// hit the `default` branch).
	if !waitForWakeup(t, ch, 100*time.Millisecond) {
		t.Fatalf("no wake-up arrived after 1000 writes")
	}
	// And no further signal should arrive (slot was 1-deep).
	expectNoWakeup(t, ch, 50*time.Millisecond, "post-burst")
}

// Subscribe survives /wipe transparently: subscriber slot lives at Store
// level keyed by scope name, NOT on *scopeBuffer, so when wipe drops +
// recreates _events the subscriber stays attached and re-points at the
// fresh buffer (settled #21). A write after the wipe wakes the same
// subscriber.
func TestSubscribe_ChannelSurvivesWipe(t *testing.T) {
	s := newTestStoreForSubscribe(t)
	ch, unsub, err := s.Subscribe(EventsScopeName)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer unsub()

	// Pre-wipe write — drain the wake-up so the slot is empty.
	if _, err := s.appendOne(Item{Scope: "posts", Payload: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("pre-wipe append: %v", err)
	}
	<-ch // drain the wake-up

	s.wipe()

	// Channel must still be open (NOT closed by wipe).
	select {
	case _, ok := <-ch:
		if !ok {
			t.Fatalf("channel was closed by wipe (must survive transparently)")
		}
		// Spurious signal — surprising but not a failure on its own;
		// proceed to the post-wipe write check.
	default:
	}

	// Post-wipe write — fresh _events seq=1; subscriber wakes up.
	if _, err := s.appendOne(Item{Scope: "posts", Payload: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("post-wipe append: %v", err)
	}
	if !waitForWakeup(t, ch, 100*time.Millisecond) {
		t.Errorf("no wake-up after post-wipe write; subscription did not survive /wipe")
	}
}

// Same shape as the wipe test, but for /rebuild — also drops + recreates
// the reserved scopes; subscription must survive transparently.
func TestSubscribe_ChannelSurvivesRebuild(t *testing.T) {
	s := newTestStoreForSubscribe(t)
	ch, unsub, err := s.Subscribe(EventsScopeName)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer unsub()

	// Pre-rebuild write — drain the wake-up.
	if _, err := s.appendOne(Item{Scope: "posts", Payload: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("pre-rebuild append: %v", err)
	}
	<-ch

	if _, _, err := s.rebuildAll(map[string][]Item{
		"new": {{Scope: "new", Payload: json.RawMessage(`{}`)}},
	}); err != nil {
		t.Fatalf("rebuildAll: %v", err)
	}

	// Drain any spurious signal from the wipe-then-rebuild path.
	select {
	case <-ch:
	default:
	}

	// Post-rebuild write — subscriber wakes up.
	if _, err := s.appendOne(Item{Scope: "posts", Payload: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("post-rebuild append: %v", err)
	}
	if !waitForWakeup(t, ch, 100*time.Millisecond) {
		t.Errorf("no wake-up after post-rebuild write; subscription did not survive /rebuild")
	}
}

// close-on-unsub semantics: the subscriber loop is `for range ch { … }`,
// which exits naturally when the channel is closed. Verifies that unsub
// closes the channel + the loop terminates within a short timeout.
func TestSubscribe_UnsubClosesChannel(t *testing.T) {
	s := newTestStoreForSubscribe(t)
	ch, unsub, err := s.Subscribe(EventsScopeName)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Run a typical subscriber loop in a goroutine; it must exit when
	// unsub closes the channel.
	done := make(chan struct{})
	go func() {
		for range ch { //nolint:revive
			// drain
		}
		close(done)
	}()

	unsub()

	select {
	case <-done:
		// Good — the loop exited.
	case <-time.After(200 * time.Millisecond):
		t.Errorf("subscriber loop did not exit within 200ms after unsub (close-on-unsub broken?)")
	}
}

// Calling unsub twice is idempotent — common pattern is `defer unsub()`
// plus an explicit unsub() during shutdown. The second call must NOT
// panic on a double-close.
func TestSubscribe_UnsubIsIdempotent(t *testing.T) {
	s := newTestStoreForSubscribe(t)
	_, unsub, err := s.Subscribe(EventsScopeName)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	unsub()
	// Must not panic.
	unsub()
}

// Two subscribers on different reserved scopes work simultaneously.
// /append to a user scope wakes _events; /append _inbox wakes _inbox;
// neither bleeds into the other.
func TestSubscribe_BothReservedScopesIndependent(t *testing.T) {
	s := newTestStoreForSubscribe(t)
	chEvents, unsubE, err := s.Subscribe(EventsScopeName)
	if err != nil {
		t.Fatalf("Subscribe(_events): %v", err)
	}
	defer unsubE()
	chInbox, unsubI, err := s.Subscribe(InboxScopeName)
	if err != nil {
		t.Fatalf("Subscribe(_inbox): %v", err)
	}
	defer unsubI()

	// User-scope write: _events subscriber wakes, _inbox does not.
	if _, err := s.appendOne(Item{Scope: "posts", Payload: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("appendOne posts: %v", err)
	}
	if !waitForWakeup(t, chEvents, 100*time.Millisecond) {
		t.Errorf("_events did not wake on user-scope write")
	}
	expectNoWakeup(t, chInbox, 50*time.Millisecond, "_inbox after posts write")

	// _inbox write: _inbox subscriber wakes, _events also wakes (because
	// auto-populate emits to _events for any non-_events write, including
	// _inbox writes).
	if _, err := s.appendOne(Item{Scope: InboxScopeName, Payload: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("appendOne _inbox: %v", err)
	}
	if !waitForWakeup(t, chInbox, 100*time.Millisecond) {
		t.Errorf("_inbox did not wake on /append _inbox")
	}
	if !waitForWakeup(t, chEvents, 100*time.Millisecond) {
		t.Errorf("_events did not wake on /append _inbox (auto-populate fires)")
	}
}

// Race-test: many goroutines write while another loops Subscribe/unsub
// rapidly. Goal: catch any send-on-closed-channel panic, any deadlock,
// any data race that `go test -race` would flag.
//
// Run with: go test -race -run TestSubscribe_RaceVsUnsub
func TestSubscribe_RaceVsUnsub(t *testing.T) {
	s := newTestStoreForSubscribe(t)

	const writers = 8
	const writesPerWriter = 1000
	const subscribeChurns = 200

	var wg sync.WaitGroup
	stop := atomic.Bool{}

	// Writers: hammer user-scope writes (each emits to _events).
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < writesPerWriter; j++ {
				if stop.Load() {
					return
				}
				_, _ = s.appendOne(Item{
					Scope:   "posts",
					Payload: json.RawMessage(`{}`),
				})
			}
		}()
	}

	// Subscribe/unsub loop: subscribes, optionally drains a few
	// wake-ups, then unsubs. Cycle this rapidly to maximize the
	// race-window between unsub's close and notify's send.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for k := 0; k < subscribeChurns; k++ {
			ch, unsub, err := s.Subscribe(EventsScopeName)
			if err != nil {
				// Could happen briefly between unsub and re-subscribe;
				// retry on next iteration.
				continue
			}
			// Drain a few signals (or skip on empty).
			for i := 0; i < 3; i++ {
				select {
				case <-ch:
				default:
				}
			}
			unsub()
		}
		stop.Store(true)
	}()

	wg.Wait()
}
