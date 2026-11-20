package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyevents"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"

	"github.com/frinknet/caddy-javascript/pkg/config"
	"github.com/frinknet/caddy-javascript/pkg/template"
	"github.com/frinknet/caddy-javascript/pkg/vm"
)

func init() {
	caddy.RegisterModule(&App{})
	caddy.RegisterModule(Handler{})
	httpcaddyfile.RegisterGlobalOption("javascript", parseGlobal)
	httpcaddyfile.RegisterDirective("javascript", parseDirective)
	httpcaddyfile.RegisterDirectiveOrder("javascript", httpcaddyfile.Before, "file_server")
}

type App struct {
	config.Options

	vm          *vm.VM
	pool        string
	logger      *zap.Logger
	releaseOnce sync.Once
}

func (*App) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "javascript",
		New: func() caddy.Module { return new(App) },
	}
}

func (a *App) Provision(ctx caddy.Context) error {
	a.logger = ctx.Logger()
	a.Options.ApplyDefaults()
	if err := a.Options.Validate(); err != nil {
		return err
	}
	script, root, err := resolve(a.Options)
	if err != nil {
		return err
	}

	cfg := vm.Config{
		ScriptPath:     script,
		RootDir:        root,
		Memory:         a.Memory,
		QueueSize:      a.QueueSize,
		RequireHandler: false,
		Logger:         a.logger,
		Caddy:          &vm.CaddyConfig{Apply: applyCaddyConfig},
		EnvAllow:       a.EnvAllow,
		FetchAllow:     a.FetchAllow,
		Events:         true,
	}
	provisionCtx, cancel := context.WithTimeout(context.Background(), vm.DefaultProvisionTimeout)
	defer cancel()
	v, key, reused, err := acquireApp(provisionCtx, cfg)
	if err != nil {
		return err
	}
	a.vm = v
	a.pool = key
	if err := a.bindEvents(ctx, v); err != nil {
		_ = releaseApp(a.pool)
		a.pool = ""
		return err
	}
	if !reused || !v.HasConfig() {
		a.seedConfig(ctx, v)
	}
	return nil
}

// bindEvents subscribes the worker's listeners for this configuration
// generation. Caddy keeps subscriptions open only until the events app starts,
// so every reload binds the names the module registered again.
func (a *App) bindEvents(ctx caddy.Context, v *vm.VM) error {
	if ctx.Context == nil {
		v.MarkEventBound()
		return nil
	}
	appIface, err := ctx.App("events")
	if err != nil {
		return fmt.Errorf("javascript: events app: %w", err)
	}
	eventsApp, ok := appIface.(*caddyevents.App)
	if !ok {
		return fmt.Errorf("javascript: events app has unexpected type %T", appIface)
	}
	for _, name := range v.EventNames() {
		if err := eventsApp.On(name, eventSink{v: v}); err != nil {
			return fmt.Errorf("javascript: subscribe %q: %w", name, err)
		}
	}
	v.MarkEventBound()
	return nil
}

// eventSink copies one Caddy event into the worker VM.
type eventSink struct {
	v *vm.VM
}

func (s eventSink) Handle(_ context.Context, event caddy.Event) error {
	if s.v == nil {
		return nil
	}
	origin := "caddy"
	if module := event.Origin(); module != nil {
		origin = string(module.CaddyModule().ID)
	}
	s.v.DispatchEvent(vm.Event{
		ID:     event.ID().String(),
		Name:   event.Name(),
		Origin: origin,
		Time:   event.Timestamp(),
		Data:   event.Data,
	})
	return nil
}

// seedConfig installs the worker's config object once the admin endpoint is
// reachable. On the first load it is started after the app, so the fetch is
// retried for a bounded time.
func (a *App) seedConfig(ctx caddy.Context, v *vm.VM) {
	cctx := ctx.Context
	if cctx == nil {
		cctx = context.Background()
	}
	go func() {
		deadline := time.NewTimer(30 * time.Second)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer deadline.Stop()
		defer ticker.Stop()

		attempt := func() bool {
			value, err := readAdminConfig(2 * time.Second)
			if err != nil {
				return false
			}
			if err := v.SetConfig(cctx, &vm.CaddyConfig{Value: value, Apply: applyCaddyConfig}); err != nil {
				return true
			}
			return true
		}
		if attempt() {
			return
		}
		for {
			select {
			case <-cctx.Done():
				return
			case <-deadline.C:
				if a.logger != nil {
					a.logger.Warn("javascript: config object was not seeded; admin endpoint unavailable")
				}
				return
			case <-ticker.C:
				if attempt() {
					return
				}
			}
		}
	}()
}

func (a *App) Start() error { return nil }

func (a *App) Stop() error { return a.release() }

func (a *App) Cleanup() error { return a.release() }

func (a *App) release() error {
	var err error
	a.releaseOnce.Do(func() {
		err = releaseApp(a.pool)
	})
	return err
}

type Handler struct {
	config.Options

	vm      *vm.VM
	site    *siteRef
	script  string
	logger  *zap.Logger
	timeout time.Duration
}

func (Handler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.javascript",
		New: func() caddy.Module { return new(Handler) },
	}
}

func (h *Handler) Provision(ctx caddy.Context) error {
	h.logger = ctx.Logger()
	h.Options.ApplyDefaults()
	if err := h.Options.Validate(); err != nil {
		return err
	}
	script, root, err := resolve(h.Options)
	if err != nil {
		return err
	}
	h.script = script
	h.timeout = time.Duration(h.Timeout)

	cfg := vm.Config{
		ScriptPath:       script,
		RootDir:          root,
		Memory:           h.Memory,
		QueueSize:        h.QueueSize,
		RequireHandler:   true,
		ProvisionTimeout: vm.DefaultProvisionTimeout,
		Logger:           h.logger,
		EnvAllow:         h.EnvAllow,
		FetchAllow:       h.FetchAllow,
	}

	if site, ok := h.bindSite(ctx); ok {
		funcs, err := templateFuncs(ctx, site.route)
		if err != nil {
			site.gen.discard(site.srv)
			return err
		}
		cfg.TemplateFuncs = funcs
		provisionCtx, cancel := context.WithTimeout(context.Background(), vm.DefaultProvisionTimeout)
		defer cancel()
		v, key, err := acquireSite(provisionCtx, site, cfg)
		if err != nil {
			site.gen.discard(site.srv)
			return err
		}
		site.pool = key
		site.gen.hold()
		h.vm = v
		h.site = &site
		return nil
	}

	v, err := vm.New(cfg)
	if err != nil {
		return err
	}
	h.vm = v
	return nil
}

func (h *Handler) Cleanup() error {
	if h.site != nil {
		site := h.site
		h.site = nil
		h.vm = nil
		err := releaseSite(site.pool)
		site.gen.drop(site.srv)
		return err
	}
	v := h.vm
	h.vm = nil
	if v == nil {
		return nil
	}
	return v.Close()
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	if h.vm == nil {
		http.Error(w, "javascript: runtime unavailable", http.StatusServiceUnavailable)
		return nil
	}

	ctx, cancel := context.WithTimeout(r.Context(), h.timeout)
	defer cancel()

	vars, _ := r.Context().Value(caddyhttp.VarsCtxKey).(map[string]any)
	var lookup func(string) (any, bool)
	var setVar func(string, any)
	var deleteVar func(string)
	if repl, ok := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer); ok && repl != nil {
		lookup = placeholderLookup(repl)
		setVar = repl.Set
		deleteVar = repl.Delete
	}
	resp, err := h.vm.Handle(ctx, h.script, vm.Request{
		Req:    r,
		Vars:   vars,
		Lookup: lookup,
		Set:    setVar,
		Delete: deleteVar,
	})
	if err != nil {
		if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
			return r.Context().Err()
		}
		status := http.StatusInternalServerError
		var httpErr *template.HTTPError
		switch {
		case errors.As(err, &httpErr):
			status = httpErr.Status
		case errors.Is(err, vm.ErrQueueFull), errors.Is(err, vm.ErrClosed):
			status = http.StatusServiceUnavailable
		case errors.Is(err, context.DeadlineExceeded):
			status = http.StatusGatewayTimeout
		}
		if h.logger != nil {
			h.logger.Error("javascript: handler error",
				zap.Error(err),
				zap.String("method", r.Method),
				zap.String("path", r.URL.Path),
			)
		}
		http.Error(w, http.StatusText(status), status)
		return nil
	}

	if resp == nil {
		return next.ServeHTTP(w, r)
	}

	for key, value := range resp.Headers {
		w.Header().Set(key, value)
	}
	status := resp.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(resp.Body)
	return nil
}

// shortPlaceholders expands the Caddyfile shorthands, so a script can read
// http.vars.host the same way a Caddyfile writes {host}.
var shortPlaceholders = httpcaddyfile.NewShorthandReplacer()

// placeholderLookup resolves a name a script reads from http.vars but the vars
// table does not hold. The replacer answers map destinations and other lazy
// placeholders; its Caddyfile shorthands are expanded on demand.
func placeholderLookup(repl *caddy.Replacer) func(string) (any, bool) {
	return func(name string) (any, bool) {
		if value, ok := repl.Get(name); ok {
			return value, true
		}
		if name == "" || strings.ContainsAny(name, "{} \t\r\n") {
			return nil, false
		}
		segment := caddyfile.Segment{{Text: "{" + name + "}"}}
		shortPlaceholders.ApplyToSegment(&segment)
		expanded := segment[0].Text
		if expanded == "{"+name+"}" {
			return nil, false
		}
		return repl.Get(strings.TrimSuffix(strings.TrimPrefix(expanded, "{"), "}"))
	}
}

func resolve(o config.Options) (string, string, error) {
	script, err := filepath.Abs(o.ScriptPath)
	if err != nil {
		return "", "", fmt.Errorf("javascript: script path: %w", err)
	}
	info, err := os.Stat(script)
	if err != nil {
		return "", "", fmt.Errorf("javascript: script %q: %w", script, err)
	}
	if info.IsDir() {
		return "", "", fmt.Errorf("javascript: script %q is a directory", script)
	}

	root := o.RootDir
	if root == "" {
		root = filepath.Dir(script)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return "", "", fmt.Errorf("javascript: root: %w", err)
	}
	info, err = os.Stat(root)
	if err != nil {
		return "", "", fmt.Errorf("javascript: root %q: %w", root, err)
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("javascript: root %q is not a directory", root)
	}
	return script, root, nil
}

func parseDirective(h httpcaddyfile.Helper) ([]httpcaddyfile.ConfigValue, error) {
	var opts config.Options
	opts.ApplyDefaults()
	if err := config.FromDispenser(h.Dispenser, &opts); err != nil {
		return nil, err
	}
	return h.NewRoute(nil, &Handler{Options: opts}), nil
}

func parseGlobal(d *caddyfile.Dispenser, existing any) (any, error) {
	if existing != nil {
		return nil, fmt.Errorf("javascript: global option may only be set once")
	}
	var opts config.Options
	opts.ApplyDefaults()
	if err := config.FromDispenser(d, &opts); err != nil {
		return nil, err
	}
	return httpcaddyfile.App{
		Name:  "javascript",
		Value: caddyconfig.JSON(&App{Options: opts}, nil),
	}, nil
}

var (
	_ caddy.App                   = (*App)(nil)
	_ caddy.Provisioner           = (*App)(nil)
	_ caddy.CleanerUpper          = (*App)(nil)
	_ caddy.Provisioner           = (*Handler)(nil)
	_ caddy.CleanerUpper          = (*Handler)(nil)
	_ caddyhttp.MiddlewareHandler = (*Handler)(nil)
)
