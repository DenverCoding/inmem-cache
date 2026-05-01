package scopecache

import (
	"bytes"
	"net/http"
	"time"
)

// cappedResponseWriter buffers a handler's full response so it can be
// inspected before any byte reaches the client. The wrapping middleware
// uses the buffer to enforce the per-response byte cap: if the handler
// writes more than the cap allows, the wrapper discards the buffer and
// replaces the response with a 507. Otherwise it replays the captured
// status, headers, and body verbatim.
//
// The wrapper is needed because Go HTTP cannot un-send headers — once
// WriteHeader(200) reaches the client, we cannot retroactively swap to
// 507. By intercepting WriteHeader and Write at the wrapper layer the
// handler's output stays in our control until we know whether to commit
// it or reject it.
//
// Memory note: the buffer can hold up to `cap` bytes per in-flight
// request. Above the cap we stop appending (the buffer is reset and
// subsequent writes are accepted-and-discarded), so memory is bounded
// even when a misbehaving handler tries to stream a multi-GB body.
type cappedResponseWriter struct {
	inner      http.ResponseWriter
	cap        int64
	statusCode int
	headers    http.Header
	buf        bytes.Buffer
	written    int64
	overflowed bool
	started    time.Time
}

func newCappedResponseWriter(w http.ResponseWriter, cap int64, started time.Time) *cappedResponseWriter {
	return &cappedResponseWriter{
		inner:   w,
		cap:     cap,
		headers: make(http.Header),
		started: started,
	}
}

func (c *cappedResponseWriter) Header() http.Header { return c.headers }

func (c *cappedResponseWriter) WriteHeader(code int) {
	if c.statusCode == 0 {
		c.statusCode = code
	}
}

func (c *cappedResponseWriter) Write(b []byte) (int, error) {
	if c.statusCode == 0 {
		c.statusCode = http.StatusOK
	}
	n := len(b)
	c.written += int64(n)
	if c.overflowed {
		// Already over cap on a previous write; accept the bytes (handler
		// keeps running, never sees a write error) but drop them so memory
		// stays bounded.
		return n, nil
	}
	if c.written > c.cap {
		c.overflowed = true
		c.buf.Reset()
		return n, nil
	}
	return c.buf.Write(b)
}

// flush replays the buffered response on success, or replaces it with a
// 507 on overflow. Called by capResponse after the handler returns.
func (c *cappedResponseWriter) flush() {
	if c.overflowed {
		responseTooLarge(c.inner, c.started, c.written, c.cap)
		return
	}
	dst := c.inner.Header()
	for k, vs := range c.headers {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
	if c.statusCode == 0 {
		c.statusCode = http.StatusOK
	}
	c.inner.WriteHeader(c.statusCode)
	_, _ = c.inner.Write(c.buf.Bytes())
}

// responseTooLarge writes the 507 envelope used when a response would
// exceed the per-response cap. Body shape mirrors the existing 507
// helpers (storeFull, scopeFull): {ok, error, approx_response_mb,
// max_response_mb, duration_us}.
//
// Side effects already applied by the handler are NOT rolled back. This
// matches every other 507 in the cache: 2xx is not durability, and 507
// does not roll back. In practice the cap-protected endpoints are
// read-only (/head, /tail), so there is nothing to roll back.
func responseTooLarge(w http.ResponseWriter, started time.Time, written, cap int64) {
	writeJSONWithDuration(w, http.StatusInsufficientStorage, orderedFields{
		{"ok", false},
		{"error", "the response would exceed the maximum allowed size"},
		{"approx_response_mb", MB(written)},
		{"max_response_mb", MB(cap)},
	}, started)
}

// cappedResponseWriter is retained for /multi_call's per-sub-call cap
// path (handlers_multi_call.go wraps each sub-call's recorder so an
// oversized sub-call body becomes a per-slot truncation marker without
// aborting the whole batch). The public mux's /head and /tail
// no longer use this wrapper — they enforce the cap inside
// writeJSONWithMetaCap at marshal time, which avoids the second
// body-buffering pass.
