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
// binary reads from env vars (SCOPECACHE_SCOPE_MAX_ITEMS,
// SCOPECACHE_MAX_STORE_MB, SCOPECACHE_MAX_ITEM_MB,
// SCOPECACHE_INBOX_MAX_ITEMS, SCOPECACHE_INBOX_MAX_ITEM_KB,
// SCOPECACHE_EVENT_MODE). Zero / empty values fall back to the
// compile-time defaults declared in the core package.
//
// MaxStoreMB and MaxItemMB are MiB-facing (matching the env-var
// convention); InboxMaxItemKB is KiB-facing because its default
// (64 KiB) reads awkwardly as MiB. EventMode is a string enum
// (off / notify / full). All are converted at the boundary before
// being handed to the core.
type Handler struct {
	// ScopeMaxItems caps items per scope. 0 = use scopecache.ScopeMaxItems.
	ScopeMaxItems int `json:"scope_max_items,omitempty"`
	// MaxStoreMB caps aggregate store size in MiB. 0 = use scopecache.MaxStoreMiB.
	MaxStoreMB int `json:"max_store_mb,omitempty"`
	// MaxItemMB caps a single item's approxItemSize in MiB. 0 = use scopecache.MaxItemBytes.
	MaxItemMB int `json:"max_item_mb,omitempty"`
	// InboxMaxItems caps items in the reserved `_inbox` scope. 0 =
	// fall back to ScopeMaxItems.
	InboxMaxItems int `json:"inbox_max_items,omitempty"`
	// InboxMaxItemKB caps a single `_inbox` item's approxItemSize in
	// KiB. 0 = use scopecache.InboxMaxItemBytes (64 KiB).
	InboxMaxItemKB int `json:"inbox_max_item_kb,omitempty"`
	// EventMode controls auto-populate of the reserved `_events`
	// scope. Valid values: "off" (default), "notify" (events without
	// payload), "full" (events with payload). Empty string = "off".
	EventMode string `json:"event_mode,omitempty"`

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
	mode, err := scopecache.ParseEventMode(h.EventMode)
	if err != nil {
		return err
	}
	store := scopecache.NewStore(scopecache.Config{
		ScopeMaxItems: h.ScopeMaxItems,
		MaxStoreBytes: int64(h.MaxStoreMB) << 20,
		MaxItemBytes:  int64(h.MaxItemMB) << 20,
		Events: scopecache.EventsConfig{
			Mode: mode,
		},
		Inbox: scopecache.InboxConfig{
			MaxItems:     h.InboxMaxItems,
			MaxItemBytes: int64(h.InboxMaxItemKB) << 10,
		},
	})
	h.api = scopecache.NewAPI(store, scopecache.APIConfig{})
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
//	    inbox_max_items     100000
//	    inbox_max_item_kb   64
//	    event_mode          off
//	}
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

			// String-valued directives go first; integer parsing
			// would otherwise fail before the switch saw "off"/etc.
			if key == "event_mode" {
				if _, err := scopecache.ParseEventMode(value); err != nil {
					return d.Err(err.Error())
				}
				h.EventMode = value
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
			case "inbox_max_items":
				h.InboxMaxItems = n
			case "inbox_max_item_kb":
				h.InboxMaxItemKB = n
			default:
				return d.Errf("unrecognized option: %s", key)
			}
		}
	}
	return nil
}

// validateConfig rejects values the standalone binary's env-var parsers
// would have ignored with a warning (negative integers, unknown
// event_mode strings).
func (h *Handler) validateConfig() error {
	for _, e := range []struct {
		key   string
		value int
	}{
		{"scope_max_items", h.ScopeMaxItems},
		{"max_store_mb", h.MaxStoreMB},
		{"max_item_mb", h.MaxItemMB},
		{"inbox_max_items", h.InboxMaxItems},
		{"inbox_max_item_kb", h.InboxMaxItemKB},
	} {
		if e.value < 0 {
			return fmt.Errorf("%s must be zero or a positive integer (got %d); 0 falls back to the compile-time default", e.key, e.value)
		}
	}
	if _, err := scopecache.ParseEventMode(h.EventMode); err != nil {
		return err
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
