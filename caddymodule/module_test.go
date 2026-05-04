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
		{"inbox_max_items", func(h *Handler) { h.InboxMaxItems = -1 }, "inbox_max_items"},
		{"inbox_max_item_kb", func(h *Handler) { h.InboxMaxItemKB = -8 }, "inbox_max_item_kb"},
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
		ScopeMaxItems:  100000,
		MaxStoreMB:     100,
		MaxItemMB:      1,
		InboxMaxItems:  50000,
		InboxMaxItemKB: 64,
		EventMode:      "full",
	}
	if err := h.validateConfig(); err != nil {
		t.Errorf("positive config rejected: %v", err)
	}
}

// event_mode must accept "", "off", "notify", "full" and reject
// anything else. The empty string is the documented sentinel for
// "use the compile-time default" (= off), same shape as the integer
// knobs accepting zero.
func TestValidateConfig_EventMode(t *testing.T) {
	t.Run("valid values", func(t *testing.T) {
		for _, mode := range []string{"", "off", "notify", "full"} {
			h := &Handler{EventMode: mode}
			if err := h.validateConfig(); err != nil {
				t.Errorf("event_mode=%q rejected: %v", mode, err)
			}
		}
	})

	t.Run("invalid string rejected", func(t *testing.T) {
		h := &Handler{EventMode: "verbose"}
		err := h.validateConfig()
		if err == nil {
			t.Fatal("expected error for event_mode=verbose; got nil")
		}
		if !strings.Contains(err.Error(), "event_mode") {
			t.Errorf("error %q does not name the offending key", err.Error())
		}
	})
}
