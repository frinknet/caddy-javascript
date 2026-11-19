package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"

	"github.com/frinknet/caddy-javascript/pkg/config"
	"github.com/frinknet/caddy-javascript/pkg/vm"
)

func init() {
	caddy.RegisterModule(App{})
	caddy.RegisterModule(Handler{})
	httpcaddyfile.RegisterGlobalOption("javascript", parseGlobal)
	httpcaddyfile.RegisterDirective("javascript", parseDirective)
	httpcaddyfile.RegisterDirectiveOrder("javascript", httpcaddyfile.Before, "file_server")
}

type App struct {
	config.Options

	vm     *vm.VM
	logger *zap.Logger
}

func (App) CaddyModule() caddy.ModuleInfo {
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
	v, err := vm.New(vm.Config{
		ScriptPath:     script,
		RootDir:        root,
		Memory:         a.Memory,
		QueueSize:      a.QueueSize,
		RequireHandler: false,
		Logger:         a.logger,
	})
	if err != nil {
		return err
	}
	a.vm = v
	return nil
}

func (a *App) Start() error { return nil }

func (a *App) Stop() error { return a.closeVM() }

func (a *App) Cleanup() error { return a.closeVM() }

func (a *App) closeVM() error {
	if a.vm == nil {
		return nil
	}
	return a.vm.Close()
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
	}

	if site, ok := h.bindSite(ctx); ok {
		provisionCtx, cancel := context.WithTimeout(context.Background(), vm.DefaultProvisionTimeout)
		defer cancel()
		v, err := site.gen.join(provisionCtx, site.key, cfg)
		if err != nil {
			site.gen.discard(site.srv)
			return err
		}
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
		return site.gen.release(site.srv, site.key)
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

	resp, err := h.vm.Handle(ctx, h.script, vm.Request{Method: r.Method, Path: r.URL.Path})
	if err != nil {
		if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
			return r.Context().Err()
		}
		status := http.StatusInternalServerError
		switch {
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
		if h.Fallthrough {
			return next.ServeHTTP(w, r)
		}
		http.NotFound(w, r)
		return nil
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
