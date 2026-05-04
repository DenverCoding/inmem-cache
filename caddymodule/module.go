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
// SCOPECACHE_EVENTS_MODE). Zero / empty values fall back to the
// compile-time defaults declared in the core package.
//
// MaxStoreMB and MaxItemMB are MiB-facing (matching the env-var
// convention); InboxMaxItemKB is KiB-facing because its default
// (64 KiB) reads awkwardly as MiB. EventsMode is a string enum
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
	// EventsMode controls auto-populate of the reserved `_events`
	// scope. Valid values: "off" (default), "notify" (events without
	// payload), "full" (events with payload). Empty string = "off".
	EventsMode string `json:"events_mode,omitempty"`
	// SubscriberCommand is the absolute path to an executable invoked
	// by the in-core subscriber bridge on every wake-up from the
	// reserved scopes (`_events`, `_inbox`). Empty (default) = no
	// subscriber spawned. Set = one subscriber goroutine per reserved
	// scope, each invoking the command with SCOPECACHE_SCOPE set to
	// `_events` or `_inbox` so a single command can branch on which
	// scope fired. The "command" can be a shell script, a compiled
	// binary, or anything else exec.Command can run. See
	// scopecache.Gateway.StartSubscriber for the contract.
	SubscriberCommand string `json:"subscriber_command,omitempty"`

	api             *scopecache.API
	mux             *http.ServeMux
	stopSubscribers func()
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
	mode, err := scopecache.ParseEventsMode(h.EventsMode)
	if err != nil {
		return err
	}
	gw := scopecache.NewGateway(scopecache.Config{
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
	h.api = scopecache.NewAPI(gw, scopecache.APIConfig{})
	h.mux = http.NewServeMux()
	h.api.RegisterRoutes(h.mux)
	h.stopSubscribers = startSubscribers(gw, h.SubscriberCommand)
	return nil
}

// Cleanup is called by Caddy when the module is being torn down (config
// reload, server shutdown). Stops any active subscriber goroutines so
// in-flight script invocations can complete and the scope-subscription
// slots release before Caddy spins up the next Handler instance.
func (h *Handler) Cleanup() error {
	if h.stopSubscribers != nil {
		h.stopSubscribers()
		h.stopSubscribers = nil
	}
	return nil
}

// startSubscribers wires the in-core subscriber bridge to both
// reserved scopes when command is non-empty. Mirrors the
// standalone-binary helper in cmd/scopecache/main.go: a half-wired
// subscriber (e.g. _events succeeded, _inbox failed) is allowed; the
// function logs to stderr and continues so a single misconfiguration
// doesn't take the Caddy module offline. The returned stop func
// tears down every successful subscribe in reverse order.
func startSubscribers(gw *scopecache.Gateway, command string) func() {
	if command == "" {
		return func() {}
	}
	stops := []func(){}
	for _, scope := range []string{scopecache.EventsScopeName, scopecache.InboxScopeName} {
		stop, err := gw.StartSubscriber(scope, command)
		if err != nil {
			caddy.Log().Named("scopecache.subscriber").Sugar().Warnf(
				"subscriber: failed to subscribe to %s: %v", scope, err)
			continue
		}
		stops = append(stops, stop)
	}
	caddy.Log().Named("scopecache.subscriber").Sugar().Infof(
		"subscriber: %d subscriber(s) active, command=%s", len(stops), command)
	return func() {
		for i := len(stops) - 1; i >= 0; i-- {
			stops[i]()
		}
	}
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
//	    events_mode         off
//	    subscriber_command  /usr/local/bin/drain.sh
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
			if key == "events_mode" {
				if _, err := scopecache.ParseEventsMode(value); err != nil {
					return d.Err(err.Error())
				}
				h.EventsMode = value
				continue
			}
			if key == "subscriber_command" {
				h.SubscriberCommand = value
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
// events_mode strings).
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
	if _, err := scopecache.ParseEventsMode(h.EventsMode); err != nil {
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
	_ caddy.CleanerUpper          = (*Handler)(nil)
	_ caddyhttp.MiddlewareHandler = (*Handler)(nil)
	_ caddyfile.Unmarshaler       = (*Handler)(nil)
)
