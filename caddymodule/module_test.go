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

// EnableAdmin defaults to false on the Caddy module (the typical
// deployment risk: a Caddyfile mounting the handler at a public
// listener root). UnmarshalCaddyfile must accept the documented
// boolean spellings and reject garbage.
func TestUnmarshalCaddyfile_EnableAdmin(t *testing.T) {
	cases := []struct {
		input string
		want  bool
		errOK bool
	}{
		{"scopecache { enable_admin yes }", true, false},
		{"scopecache { enable_admin true }", true, false},
		{"scopecache { enable_admin on }", true, false},
		{"scopecache { enable_admin 1 }", true, false},
		{"scopecache { enable_admin no }", false, false},
		{"scopecache { enable_admin false }", false, false},
		{"scopecache { enable_admin off }", false, false},
		{"scopecache { enable_admin 0 }", false, false},
		{"scopecache { enable_admin maybe }", false, true},
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
			if h.EnableAdmin != tc.want {
				t.Errorf("EnableAdmin=%v want %v after parsing %q", h.EnableAdmin, tc.want, tc.input)
			}
		})
	}
}

// EnableAdmin's zero-value default must be false — the whole point of
// the directive is to make /admin opt-in on the Caddy module, so a
// scopecache block without `enable_admin` should leave the field at
// its safe default.
func TestUnmarshalCaddyfile_EnableAdminDefaultFalse(t *testing.T) {
	h := &Handler{}
	d := caddyfile.NewTestDispenser("scopecache { scope_max_items 10 }")
	if err := h.UnmarshalCaddyfile(d); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if h.EnableAdmin {
		t.Errorf("EnableAdmin=true with no enable_admin directive; want false (safe default)")
	}
}

// disable_read_heat is the same boolean shape as enable_admin: same
// parsing, same yes/no/true/false/on/off/1/0 vocabulary, garbage
// rejected. Default false (heat tracked, /delete_scope_candidates
// works out of the box). Operators who don't use
// /delete_scope_candidates can set yes for ~2× faster reads.
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
// is on by default so /delete_scope_candidates ranks correctly out of
// the box.
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
