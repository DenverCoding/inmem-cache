package caddymodule

import (
	"strings"
	"testing"
)

// validateConfig must reject negative integer directives. The standalone
// binary's env-var parsers ignore non-positive values with a warning and
// fall back to defaults; the Caddy module historically did neither, so
// `max_store_mb -1` would silently produce a -1 MiB cap and brick the
// cache.
func TestValidateConfig_RejectsNegative(t *testing.T) {
	cases := []struct {
		name string
		set  func(*Handler)
		want string
	}{
		{"scope_max_items", func(h *Handler) { h.ScopeMaxItems = -1 }, "scope_max_items"},
		{"max_store_mb", func(h *Handler) { h.MaxStoreMB = -1 }, "max_store_mb"},
		{"max_item_mb", func(h *Handler) { h.MaxItemMB = -5 }, "max_item_mb"},
		{"max_response_mb", func(h *Handler) { h.MaxResponseMB = -25 }, "max_response_mb"},
		{"max_multi_call_mb", func(h *Handler) { h.MaxMultiCallMB = -16 }, "max_multi_call_mb"},
		{"max_multi_call_count", func(h *Handler) { h.MaxMultiCallCount = -10 }, "max_multi_call_count"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{}
			tc.set(h)
			err := h.validateConfig()
			if err == nil {
				t.Fatalf("expected error for negative %s; got nil", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name the offending key %q", err.Error(), tc.want)
			}
		})
	}
}

// Zero is the documented sentinel for "use compile-time default" — must
// stay accepted.
func TestValidateConfig_AcceptsZero(t *testing.T) {
	h := &Handler{} // all fields zero
	if err := h.validateConfig(); err != nil {
		t.Errorf("zero config rejected: %v", err)
	}
}

func TestValidateConfig_AcceptsPositive(t *testing.T) {
	h := &Handler{
		ScopeMaxItems:     100000,
		MaxStoreMB:        100,
		MaxItemMB:         1,
		MaxResponseMB:     25,
		MaxMultiCallMB:    16,
		MaxMultiCallCount: 10,
		ServerSecret:      "real-secret",
	}
	if err := h.validateConfig(); err != nil {
		t.Errorf("positive config rejected: %v", err)
	}
}

// Empty server_secret is the documented kill-switch — /guarded simply
// isn't registered. Must stay accepted.
func TestValidateConfig_EmptyServerSecretIsKillSwitch(t *testing.T) {
	h := &Handler{ServerSecret: ""}
	if err := h.validateConfig(); err != nil {
		t.Errorf("empty server_secret rejected: %v", err)
	}
}

// Whitespace-only server_secret almost always means the operator wrote
// `{$SCOPECACHE_SERVER_SECRET}` and the env var was set to whitespace
// (or unset, then somehow padded). Either way it's an extremely weak
// HMAC key and almost certainly an accident.
func TestValidateConfig_RejectsWhitespaceOnlyServerSecret(t *testing.T) {
	for _, s := range []string{" ", "\t", "  \n", " \t  "} {
		h := &Handler{ServerSecret: s}
		err := h.validateConfig()
		if err == nil {
			t.Errorf("whitespace-only secret %q accepted; expected error", s)
			continue
		}
		if !strings.Contains(err.Error(), "server_secret") {
			t.Errorf("error %q does not mention server_secret", err.Error())
		}
	}
}
