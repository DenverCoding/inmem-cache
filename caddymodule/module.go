// Package caddymodule exposes scopecache as a Caddy HTTP handler.
//
// The core scopecache package stays stdlib-only (no Caddy imports)
// and owns every cache semantic. This adapter:
//
//   - translates Caddy's JSON / Caddyfile config into scopecache.Config,
//   - wires the core's route table onto an internal http.ServeMux, and
//   - delegates each matched request to that mux from ServeHTTP.
//
// Cross-cutting concerns that require request context — auth,
// identity, per-tenant scope-prefix rewrites, rate-limit hooks —
// belong in this adapter layer or in addon sub-packages built on
// top of the public *scopecache.API surface, not in core.
package caddymodule

import (
	"context"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

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
	// InitCommand is the absolute path to an executable invoked once
	// at Provision time, after the cache has been constructed and the
	// reserved scopes (`_events`, `_inbox`) are pre-created. Empty
	// (default) = no init command. Set = the helper runs sync inside
	// Provision, blocking the rest of Caddy's start-up until the
	// command exits.
	//
	// The script is given SCOPECACHE_SOCKET_PATH pointing to a
	// per-instance private Unix socket that the adapter binds for the
	// duration of init. Inside the script, write into THIS Caddy
	// instance's cache with `curl --unix-socket "$SCOPECACHE_SOCKET_PATH"`
	// — Caddy's normal HTTP listeners aren't bound yet during
	// Provision, but the private socket is. After the script exits the
	// private socket is torn down and Caddy starts listening normally.
	// Use case: rebuild the cache from a source of truth at boot. See
	// scopecache.Gateway.RunInitCommand for the full contract.
	InitCommand string `json:"init_command,omitempty"`
	// InitTimeoutSec caps how long the init command may run before
	// the helper SIGKILLs its process group. 0 (default) = no
	// timeout — only Caddy reload / shutdown cancels the script.
	// Recommended for production: a value high enough for your
	// largest realistic source-of-truth rebuild, plus margin.
	InitTimeoutSec int `json:"init_timeout_sec,omitempty"`

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
func (h *Handler) Provision(ctx caddy.Context) error {
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

	// Boot-time init command (private-socket adapter contract +
	// subscribers-AFTER-init rationale live in init_command.go's
	// file-header). Provision blocks until the script exits — Caddy
	// keeps the previous instance serving during reload, so a long
	// init does not interrupt service. ctx cancellation on reload /
	// shutdown SIGKILLs the script's whole process group; a failure
	// is logged and ignored (empty cache is still a working cache).
	if h.InitCommand != "" {
		if err := h.runInitWithPrivateSocket(ctx, gw); err != nil {
			caddy.Log().Named("scopecache.init").Sugar().Warnf("init: %v", err)
		}
	}

	// Subscribers start AFTER init — RunInitCommand wipes `_events`
	// itself, and `_inbox` was unreachable to external writers during
	// init, so subscribers register against an empty stream.
	h.stopSubscribers = gw.StartReservedSubscribers(
		h.SubscriberCommand,
		caddy.Log().Named("scopecache.subscriber").Sugar().Infof,
	)
	return nil
}

// runInitWithPrivateSocket implements the standalone-symmetric
// init contract (init_command.go file-header): bind a per-instance
// temp Unix socket serving the cache routes, run the init command
// with SCOPECACHE_SOCKET_PATH pointing at it, tear the socket down.
//
// ctx propagates into RunInitCommand so Caddy reload / shutdown
// SIGKILLs a hung script's whole process group. h.InitTimeoutSec
// > 0 wraps ctx with an additional hard deadline. Temp dir is
// 0o700 (MkdirTemp default) so other users on the host can't reach
// the socket.
func (h *Handler) runInitWithPrivateSocket(ctx context.Context, gw *scopecache.Gateway) error {
	logf := caddy.Log().Named("scopecache.init").Sugar().Infof
	logger := caddy.Log().Named("scopecache.init").Sugar()

	dir, err := os.MkdirTemp("", "scopecache-init-")
	if err != nil {
		return fmt.Errorf("create temp dir for init socket: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			logger.Warnf("remove temp dir %s: %v", dir, err)
		}
	}()

	sockPath := filepath.Join(dir, "init.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("listen on init socket %s: %w", sockPath, err)
	}

	server := &http.Server{Handler: h.mux}
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = server.Serve(ln)
	}()

	runCtx := ctx
	if h.InitTimeoutSec > 0 {
		var cancelTimeout context.CancelFunc
		runCtx, cancelTimeout = context.WithTimeout(ctx, time.Duration(h.InitTimeoutSec)*time.Second)
		defer cancelTimeout()
	}

	runErr := gw.RunInitCommand(
		runCtx,
		h.InitCommand,
		[]string{"SCOPECACHE_SOCKET_PATH=" + sockPath},
		logf,
	)

	// Tear the socket down before Provision returns. Shutdown
	// gracefully closes idle connections; in-flight curls from the
	// init script have already completed (RunInitCommand is sync).
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Warnf("shutdown init socket: %v", err)
	}
	<-serveDone

	return runErr
}

// Cleanup is called by Caddy when the module is being torn down (config
// reload, server shutdown). Stops any active subscriber goroutines so
// the scope-subscription slots release before Caddy spins up the next
// Handler instance.
//
// Stop is abortive, not graceful: each goroutine's context is cancelled,
// which SIGKILLs the in-flight drain command (whole process group, see
// configureProcessGroup) instead of waiting for it to exit voluntarily.
// This bounds the reload latency by OS kill time rather than letting a
// stuck script tarpit a Caddy reload; see the StartSubscriber comment
// in subscriber_command.go for the full trade-off rationale.
func (h *Handler) Cleanup() error {
	if h.stopSubscribers != nil {
		h.stopSubscribers()
		h.stopSubscribers = nil
	}
	return nil
}

// ServeHTTP dispatches to the scopecache mux. Any path the mux does not
// recognise falls through to the next Caddy handler — this lets operators
// mount scopecache under a path prefix (`handle /cache/*`) alongside other
// handlers without scopecache swallowing unrelated traffic.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	if handler, pattern := h.mux.Handler(r); pattern != "" {
		handler.ServeHTTP(w, r)
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
//	    init_command        /usr/local/bin/rebuild.sh
//	    init_timeout_sec    600
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
			if key == "init_command" {
				h.InitCommand = value
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
			case "init_timeout_sec":
				h.InitTimeoutSec = n
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
// maxConfigMB / maxConfigKB / maxConfigSec are the upper bounds
// beyond which a later unit conversion in Provision would silently
// overflow int64:
//
//   - MaxStoreMB / MaxItemMB:    `int64(value) << 20` (MiB→bytes).
//   - InboxMaxItemKB:            `int64(value) << 10` (KiB→bytes).
//   - InitTimeoutSec:            `time.Duration(value) * time.Second`
//     (seconds → nanoseconds).
//
// Operationally absurd values, but rejecting them turns "silent
// wrong cap" into "loud configuration error" — the cheaper failure
// mode.
const (
	maxConfigMB  = math.MaxInt64 >> 20                // ~8.79 trillion MiB
	maxConfigKB  = math.MaxInt64 >> 10                // ~9 quadrillion KiB
	maxConfigSec = math.MaxInt64 / int64(time.Second) // ~292 years
)

func (h *Handler) validateConfig() error {
	for _, e := range []struct {
		key      string
		value    int
		upper    int
		upperFmt string
	}{
		// scope_max_items / inbox_max_items are plain int counts —
		// no unit conversion, no upper bound check beyond
		// non-negative. upper == 0 disables the bound check for
		// those rows.
		{"scope_max_items", h.ScopeMaxItems, 0, ""},
		{"max_store_mb", h.MaxStoreMB, maxConfigMB, "MiB"},
		{"max_item_mb", h.MaxItemMB, maxConfigMB, "MiB"},
		{"inbox_max_items", h.InboxMaxItems, 0, ""},
		{"inbox_max_item_kb", h.InboxMaxItemKB, maxConfigKB, "KiB"},
		{"init_timeout_sec", h.InitTimeoutSec, int(maxConfigSec), "seconds"},
	} {
		if e.value < 0 {
			return fmt.Errorf("%s must be zero or a positive integer (got %d); 0 falls back to the compile-time default", e.key, e.value)
		}
		if e.upper > 0 && e.value > e.upper {
			return fmt.Errorf("%s=%d exceeds the maximum (%d %s); larger values would overflow int64 after unit conversion",
				e.key, e.value, e.upper, e.upperFmt)
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
