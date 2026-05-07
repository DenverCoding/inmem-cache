package caddymodule

import (
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
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

// validateConfig must reject values whose `int64(value) << 20` (MiB→
// bytes) or `<< 10` (KiB→bytes) would silently overflow int64. Pre-fix
// a huge max_store_mb wrapped to a tiny or negative cap, falling back
// to defaults or producing a misleading "small" cap. Post-fix the
// shift is gated by an explicit upper bound and the operator gets a
// loud configuration error.
func TestValidateConfig_RejectsOverflowOnUnitShift(t *testing.T) {
	cases := []struct {
		name      string
		set       func(*Handler)
		wantField string
	}{
		{
			name:      "max_store_mb above MiB shift bound",
			set:       func(h *Handler) { h.MaxStoreMB = maxConfigMB + 1 },
			wantField: "max_store_mb",
		},
		{
			name:      "max_item_mb above MiB shift bound",
			set:       func(h *Handler) { h.MaxItemMB = maxConfigMB + 1 },
			wantField: "max_item_mb",
		},
		{
			name:      "inbox_max_item_kb above KiB shift bound",
			set:       func(h *Handler) { h.InboxMaxItemKB = maxConfigKB + 1 },
			wantField: "inbox_max_item_kb",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{}
			tc.set(h)
			err := h.validateConfig()
			if err == nil {
				t.Fatalf("expected overflow rejection for %s; got nil", tc.wantField)
			}
			if !strings.Contains(err.Error(), tc.wantField) {
				t.Errorf("error %q does not name the offending key %q", err.Error(), tc.wantField)
			}
			if !strings.Contains(err.Error(), "overflow") {
				t.Errorf("error %q does not mention overflow", err.Error())
			}
		})
	}
}

// The values JUST AT the upper bound must be accepted — the fix is
// "> upper", not ">= upper". Confirms the boundary is inclusive on the
// safe side.
func TestValidateConfig_AcceptsValuesAtUpperBound(t *testing.T) {
	h := &Handler{
		MaxStoreMB:     maxConfigMB,
		MaxItemMB:      maxConfigMB,
		InboxMaxItemKB: maxConfigKB,
	}
	if err := h.validateConfig(); err != nil {
		t.Errorf("at-bound config rejected: %v", err)
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
		EventsMode:     "full",
	}
	if err := h.validateConfig(); err != nil {
		t.Errorf("positive config rejected: %v", err)
	}
}

// subscriber_command lands on the SubscriberCommand field as-is. We
// do no path validation in the adapter — operators can deploy
// executables after Caddy starts, and the in-core bridge logs
// per-invocation if the file is missing. The test pins the parser
// shape so a future refactor doesn't silently misroute the value to
// a different field.
func TestUnmarshalCaddyfile_SubscriberCommand(t *testing.T) {
	t.Run("set", func(t *testing.T) {
		input := `scopecache {
			subscriber_command /usr/local/bin/drain.sh
		}`
		var h Handler
		if err := h.UnmarshalCaddyfile(caddyfile.NewTestDispenser(input)); err != nil {
			t.Fatalf("UnmarshalCaddyfile: %v", err)
		}
		if h.SubscriberCommand != "/usr/local/bin/drain.sh" {
			t.Errorf("SubscriberCommand=%q, want /usr/local/bin/drain.sh", h.SubscriberCommand)
		}
	})

	t.Run("unset defaults to empty", func(t *testing.T) {
		input := `scopecache {
			max_store_mb 100
		}`
		var h Handler
		if err := h.UnmarshalCaddyfile(caddyfile.NewTestDispenser(input)); err != nil {
			t.Fatalf("UnmarshalCaddyfile: %v", err)
		}
		if h.SubscriberCommand != "" {
			t.Errorf("SubscriberCommand=%q, want empty when not set", h.SubscriberCommand)
		}
	})
}

// init_command lands on the InitCommand field as-is. Same parser
// shape as subscriber_command; the test pins the routing so a typo
// or refactor doesn't silently drop the value or misroute it onto
// another field.
func TestUnmarshalCaddyfile_InitCommand(t *testing.T) {
	t.Run("set", func(t *testing.T) {
		input := `scopecache {
			init_command /usr/local/bin/rebuild.sh
		}`
		var h Handler
		if err := h.UnmarshalCaddyfile(caddyfile.NewTestDispenser(input)); err != nil {
			t.Fatalf("UnmarshalCaddyfile: %v", err)
		}
		if h.InitCommand != "/usr/local/bin/rebuild.sh" {
			t.Errorf("InitCommand=%q, want /usr/local/bin/rebuild.sh", h.InitCommand)
		}
	})

	t.Run("unset defaults to empty", func(t *testing.T) {
		input := `scopecache {
			max_store_mb 100
		}`
		var h Handler
		if err := h.UnmarshalCaddyfile(caddyfile.NewTestDispenser(input)); err != nil {
			t.Fatalf("UnmarshalCaddyfile: %v", err)
		}
		if h.InitCommand != "" {
			t.Errorf("InitCommand=%q, want empty when not set", h.InitCommand)
		}
	})

	t.Run("set alongside subscriber_command", func(t *testing.T) {
		input := `scopecache {
			subscriber_command /usr/local/bin/drain.sh
			init_command       /usr/local/bin/rebuild.sh
		}`
		var h Handler
		if err := h.UnmarshalCaddyfile(caddyfile.NewTestDispenser(input)); err != nil {
			t.Fatalf("UnmarshalCaddyfile: %v", err)
		}
		if h.SubscriberCommand != "/usr/local/bin/drain.sh" {
			t.Errorf("SubscriberCommand=%q", h.SubscriberCommand)
		}
		if h.InitCommand != "/usr/local/bin/rebuild.sh" {
			t.Errorf("InitCommand=%q", h.InitCommand)
		}
	})
}

// init_timeout_sec parses as a non-negative integer onto
// InitTimeoutSec. 0 (default) = no timeout. Used by Provision to
// wrap the init context with a hard deadline before exec.
func TestUnmarshalCaddyfile_InitTimeoutSec(t *testing.T) {
	t.Run("set", func(t *testing.T) {
		input := `scopecache {
			init_timeout_sec 600
		}`
		var h Handler
		if err := h.UnmarshalCaddyfile(caddyfile.NewTestDispenser(input)); err != nil {
			t.Fatalf("UnmarshalCaddyfile: %v", err)
		}
		if h.InitTimeoutSec != 600 {
			t.Errorf("InitTimeoutSec=%d, want 600", h.InitTimeoutSec)
		}
	})

	t.Run("rejects negative", func(t *testing.T) {
		h := &Handler{InitTimeoutSec: -1}
		if err := h.validateConfig(); err == nil {
			t.Fatal("expected error for negative init_timeout_sec; got nil")
		}
	})
}

// events_mode must accept "", "off", "notify", "full" and reject
// anything else. The empty string is the documented sentinel for
// "use the compile-time default" (= off), same shape as the integer
// knobs accepting zero.
func TestValidateConfig_EventsMode(t *testing.T) {
	t.Run("valid values", func(t *testing.T) {
		for _, mode := range []string{"", "off", "notify", "full"} {
			h := &Handler{EventsMode: mode}
			if err := h.validateConfig(); err != nil {
				t.Errorf("events_mode=%q rejected: %v", mode, err)
			}
		}
	})

	t.Run("invalid string rejected", func(t *testing.T) {
		h := &Handler{EventsMode: "verbose"}
		err := h.validateConfig()
		if err == nil {
			t.Fatal("expected error for events_mode=verbose; got nil")
		}
		if !strings.Contains(err.Error(), "events_mode") {
			t.Errorf("error %q does not name the offending key", err.Error())
		}
	})
}
