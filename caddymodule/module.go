// Package caddymodule exposes scopecache as a Caddy HTTP handler module.
//
// The core scopecache package stays stdlib-only (no Caddy imports) and owns
// every cache semantic. This adapter:
//
//   - translates Caddy's JSON config into scopecache.Config,
//   - wires the core's route table onto an internal http.ServeMux, and
//   - delegates each matched request to that mux from ServeHTTP.
//
// Cross-cutting concerns that require request context — auth, identity,
// per-tenant scope-prefix enforcement, rate-limit hooks — belong in this
// adapter layer (see CLAUDE.md "boundary rule") or in addon sub-packages
// built on top of the public *API surface.
package caddymodule

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/VeloxCoding/scopecache"
	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// Handler is the Caddy HTTP handler that embeds scopecache.
//
// JSON config fields map 1:1 to the same capacity knobs the standalone
// binary reads from env vars (SCOPECACHE_SCOPE_MAX_ITEMS, SCOPECACHE_MAX_STORE_MB,
// SCOPECACHE_MAX_ITEM_MB). Zero values fall back to the compile-time defaults
// declared in the core package.
//
// MaxStoreMB and MaxItemMB are MiB-facing (matching the env-var convention);
// they are converted to bytes in Provision before being handed to the core.
type Handler struct {
	// ScopeMaxItems caps items per scope. 0 = use scopecache.ScopeMaxItems.
	ScopeMaxItems int `json:"scope_max_items,omitempty"`
	// MaxStoreMB caps aggregate store size in MiB. 0 = use scopecache.MaxStoreMiB.
	MaxStoreMB int `json:"max_store_mb,omitempty"`
	// MaxItemMB caps a single item's approxItemSize in MiB. 0 = use scopecache.MaxItemBytes.
	MaxItemMB int `json:"max_item_mb,omitempty"`
	// MaxResponseMB caps the byte size of /head and /tail responses
	// in MiB. 0 = use scopecache.MaxResponseMiB.
	MaxResponseMB int `json:"max_response_mb,omitempty"`
	// DisableReadHeat turns off per-scope read-heat tracking on the
	// hot read path (/get, /render, /head, /tail). Default false —
	// heat is tracked. Set to true to skip recordRead and the
	// time.Now() that feeds it; saves ~2× on the hottest paths at
	// the cost of always-zero last_access_ts / last_7d_read_count /
	// read_count_total in /stats.
	DisableReadHeat bool `json:"disable_read_heat,omitempty"`

	api *scopecache.API
	mux *http.ServeMux
}

// CaddyModule returns the Caddy module registration. The ID places this under
// http.handlers.* so it can be used as a `handle` directive in a Caddyfile
// or as a JSON handler entry.
func (Handler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.scopecache",
		New: func() caddy.Module { return new(Handler) },
	}
}

// Provision builds the core Store + API and registers its routes on an
// internal mux. Called once per module instance at Caddy start / config
// reload. Zero-valued numeric directives fall back to the core's
// compile-time defaults via Config.WithDefaults inside NewStore.
func (h *Handler) Provision(_ caddy.Context) error {
	if err := h.validateConfig(); err != nil {
		return err
	}
	store := scopecache.NewStore(scopecache.Config{
		ScopeMaxItems: h.ScopeMaxItems,
		MaxStoreBytes: int64(h.MaxStoreMB) << 20,
		MaxItemBytes:  int64(h.MaxItemMB) << 20,
	})
	h.api = scopecache.NewAPI(store, scopecache.APIConfig{
		MaxResponseBytes: int64(h.MaxResponseMB) << 20,
		DisableReadHeat:  h.DisableReadHeat,
	})
	h.mux = http.NewServeMux()
	h.api.RegisterRoutes(h.mux)
	return nil
}

// ServeHTTP dispatches to the scopecache mux. Any path the mux does not
// recognise falls through to the next Caddy handler — this lets operators
// mount scopecache under a path prefix (`handle /cache/*`) alongside other
// handlers without scopecache swallowing unrelated traffic.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	if _, pattern := h.mux.Handler(r); pattern != "" {
		h.mux.ServeHTTP(w, r)
		return nil
	}
	return next.ServeHTTP(w, r)
}

// UnmarshalCaddyfile parses the `scopecache` handler directive. All
// subdirectives are optional; an unset value falls back to the core default
// inside Provision. Example:
//
//	scopecache {
//	    scope_max_items     100000
//	    max_store_mb        100
//	    max_item_mb         1
//	    max_response_mb     25
//	    disable_read_heat   no
//	}
//
// Numeric capacity knobs are integer; `disable_read_heat` is a boolean
// (yes/no, true/false, on/off, 1/0).
func (h *Handler) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	for d.Next() {
		if d.NextArg() {
			return d.ArgErr()
		}
		for d.NextBlock(0) {
			key := d.Val()
			if !d.NextArg() {
				return d.ArgErr()
			}
			value := d.Val()

			// Boolean-valued directives.
			if key == "disable_read_heat" {
				switch value {
				case "yes", "true", "on", "1":
					h.DisableReadHeat = true
				case "no", "false", "off", "0":
					h.DisableReadHeat = false
				default:
					return d.Errf("disable_read_heat: %q is not a boolean (use yes/no, true/false, on/off, or 1/0)", value)
				}
				continue
			}

			// Integer-valued directives.
			n, err := strconv.Atoi(value)
			if err != nil {
				return d.Errf("%s: %v", key, err)
			}
			switch key {
			case "scope_max_items":
				h.ScopeMaxItems = n
			case "max_store_mb":
				h.MaxStoreMB = n
			case "max_item_mb":
				h.MaxItemMB = n
			case "max_response_mb":
				h.MaxResponseMB = n
			default:
				return d.Errf("unrecognized option: %s", key)
			}
		}
	}
	return nil
}

// validateConfig rejects values the standalone binary's env-var parsers
// would have ignored with a warning (negative integers).
func (h *Handler) validateConfig() error {
	for _, e := range []struct {
		key   string
		value int
	}{
		{"scope_max_items", h.ScopeMaxItems},
		{"max_store_mb", h.MaxStoreMB},
		{"max_item_mb", h.MaxItemMB},
		{"max_response_mb", h.MaxResponseMB},
	} {
		if e.value < 0 {
			return fmt.Errorf("%s must be zero or a positive integer (got %d); 0 falls back to the compile-time default", e.key, e.value)
		}
	}
	return nil
}

// parseCaddyfile is the Caddyfile-syntax entry point registered with
// http.handlers so `scopecache { ... }` is recognised as a handler directive.
func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var m Handler
	if err := m.UnmarshalCaddyfile(h.Dispenser); err != nil {
		return nil, err
	}
	return &m, nil
}

func init() {
	caddy.RegisterModule(Handler{})
	httpcaddyfile.RegisterHandlerDirective("scopecache", parseCaddyfile)
	// Without an explicit order Caddy rejects the Caddyfile directive with
	// "not an ordered HTTP handler". Placing scopecache just before the
	// `respond` catch-all matches how terminal handlers are usually slotted
	// in and means operators never need to write a manual `order` line.
	httpcaddyfile.RegisterDirectiveOrder("scopecache", httpcaddyfile.Before, "respond")
}

var (
	_ caddy.Module                = (*Handler)(nil)
	_ caddy.Provisioner           = (*Handler)(nil)
	_ caddyhttp.MiddlewareHandler = (*Handler)(nil)
	_ caddyfile.Unmarshaler       = (*Handler)(nil)
)
