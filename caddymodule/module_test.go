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
		{"max_response_mb", func(h *Handler) { h.MaxResponseMB = -25 }, "max_response_mb"},
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
		ScopeMaxItems: 100000,
		MaxStoreMB:    100,
		MaxItemMB:     1,
		MaxResponseMB: 25,
	}
	if err := h.validateConfig(); err != nil {
		t.Errorf("positive config rejected: %v", err)
	}
}

// disable_read_heat is a boolean directive with the standard
// yes/no/true/false/on/off/1/0 vocabulary; garbage values are rejected.
// Default false (heat tracked).
func TestUnmarshalCaddyfile_DisableReadHeat(t *testing.T) {
	cases := []struct {
		input string
		want  bool
		errOK bool
	}{
		{"scopecache { disable_read_heat yes }", true, false},
		{"scopecache { disable_read_heat true }", true, false},
		{"scopecache { disable_read_heat on }", true, false},
		{"scopecache { disable_read_heat 1 }", true, false},
		{"scopecache { disable_read_heat no }", false, false},
		{"scopecache { disable_read_heat false }", false, false},
		{"scopecache { disable_read_heat off }", false, false},
		{"scopecache { disable_read_heat 0 }", false, false},
		{"scopecache { disable_read_heat maybe }", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			h := &Handler{}
			d := caddyfile.NewTestDispenser(tc.input)
			err := h.UnmarshalCaddyfile(d)
			if tc.errOK {
				if err == nil {
					t.Errorf("expected error parsing %q; got nil", tc.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error parsing %q: %v", tc.input, err)
			}
			if h.DisableReadHeat != tc.want {
				t.Errorf("DisableReadHeat=%v want %v after parsing %q", h.DisableReadHeat, tc.want, tc.input)
			}
		})
	}
}

// DisableReadHeat's zero-value default must be false — heat tracking
// is on by default.
func TestUnmarshalCaddyfile_DisableReadHeatDefaultFalse(t *testing.T) {
	h := &Handler{}
	d := caddyfile.NewTestDispenser("scopecache { scope_max_items 10 }")
	if err := h.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if h.DisableReadHeat {
		t.Errorf("DisableReadHeat=true with no disable_read_heat directive; want false (default-on heat tracking)")
	}
}
