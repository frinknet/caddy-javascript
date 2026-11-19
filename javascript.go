package caddyjavascript

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/grafana/sobek"
	"go.uber.org/zap"

	"github.com/frinknet/caddy-javascript/pkg/bridge"
	"github.com/frinknet/caddy-javascript/pkg/config"
	"github.com/frinknet/caddy-javascript/pkg/eval"
	"github.com/frinknet/caddy-javascript/pkg/files"
	"github.com/frinknet/caddy-javascript/pkg/jobs"
	"github.com/frinknet/caddy-javascript/pkg/metric"
	"github.com/frinknet/caddy-javascript/pkg/sched"
)

func init() {
	caddy.RegisterModule(&Handler{})
	httpcaddyfile.RegisterDirective("javascript", parseDirective)
	httpcaddyfile.RegisterDirectiveOrder("javascript", httpcaddyfile.Before, "file_server")
}

type Handler struct {
	config.Options

	logger     *zap.Logger
	fs         *files.FS
	loop       *sched.Loop
	prog       *bridge.Program
	background *jobs.Runtime
	watcher    *files.Watcher
	scriptPath string
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

	scriptPath, err := filepath.Abs(h.ScriptPath)
	if err != nil {
		return err
	}

	rootDir := h.RootDir
	if rootDir == "" {
		rootDir = filepath.Dir(scriptPath)
	}
	rootDir, err = filepath.Abs(rootDir)
	if err != nil {
		return err
	}

	h.scriptPath = scriptPath

	src, err := os.ReadFile(h.scriptPath)
	if err != nil {
		return fmt.Errorf("javascript: failed to initialize runtime: %w", err)
	}

	h.fs, err = files.NewFS(rootDir)
	if err != nil {
		return fmt.Errorf("javascript: opening root %q: %w", rootDir, err)
	}

	h.loop = sched.New(h.QueueSize)
	h.loop.OnPanic = func(r any) {
		h.logger.Error("javascript: recovered from panic", zap.Any("panic", r))
	}

	if h.Eval {
		eval.Register(h.scriptPath, h)
	}

	initErr := make(chan error, 1)
	go h.loop.Run(func() { h.reload() })

	scheduled := h.loop.Submit(func() {
		prog := bridge.NewProgram(h.bridgeConfig(), h.fs, h.loop, h.logger)
		if err := prog.Load(src); err != nil {
			initErr <- err
			return
		}
		h.prog = prog
		initErr <- nil
	})
	if !scheduled {
		return fmt.Errorf("javascript: failed to schedule initial script load")
	}

	if err := <-initErr; err != nil {
		return fmt.Errorf("javascript: failed to initialize runtime: %w", err)
	}

	h.background, err = jobs.Start(src, h.bridgeConfig(), h.fs, h.logger, h.QueueSize, jobs.Hooks{})
	if err != nil {
		h.loop.Stop()
		return fmt.Errorf("javascript: failed to initialize background runtime: %w", err)
	}

	if h.Options.Watching() {
		watcher, err := files.NewWatcher(scriptPath, 200*time.Millisecond, h.loop.SignalReload)
		if err != nil {
			h.logger.Warn("javascript: file watching disabled", zap.Error(err))
		} else {
			h.watcher = watcher
		}
	}

	return nil
}

func (h *Handler) reload() {
	src, err := os.ReadFile(h.scriptPath)
	if err != nil {
		h.logger.Error("javascript: reload could not read script", zap.Error(err))
		return
	}

	prog := bridge.NewProgram(h.bridgeConfig(), h.fs, h.loop, h.logger)
	if err := prog.Load(src); err != nil {
		h.logger.Error("javascript: reload failed, keeping previous script", zap.Error(err))
		return
	}

	h.prog = prog
	h.logger.Info("javascript: reloaded script", zap.String("path", h.scriptPath))
	metric.Reloads.Inc()
}

func (h *Handler) Cleanup() error {
	if h.Eval {
		eval.Unregister(h.scriptPath)
	}
	if h.watcher != nil {
		_ = h.watcher.Close()
	}
	if h.background != nil {
		h.background.Stop()
	}
	if h.loop != nil {
		h.loop.Stop()
	}
	if h.fs != nil {
		_ = h.fs.Close()
	}
	return nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	rc, err := bridge.NewRequestContext(r, h.MaxBody)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, bridge.ErrBodyTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		metric.Requests.WithLabelValues(strconv.Itoa(status)).Inc()
		return caddyhttp.Error(status, err)
	}

	srw := bridge.NewResponseWriter(w)
	ex := bridge.NewExchange(srw)

	scheduled := h.loop.Submit(func() {
		h.serve(ex, rc)
	})
	if !scheduled {
		metric.QueueFull.Inc()
		metric.Requests.WithLabelValues(strconv.Itoa(http.StatusServiceUnavailable)).Inc()
		return caddyhttp.Error(http.StatusServiceUnavailable, fmt.Errorf("javascript: event queue full"))
	}

	select {
	case <-ex.Done:
	case <-r.Context().Done():
		return r.Context().Err()
	}

	if !ex.Handled() {
		if h.Fallthrough {
			return next.ServeHTTP(w, r)
		}
		http.Error(w, "Not Found", http.StatusNotFound)
	} else if !srw.Written() {
		if ex.TimedOut() {
			metric.Timeouts.Inc()
			h.logger.Error("javascript: async callback timeout exceeded",
				zap.String("method", rc.Method),
				zap.String("path", rc.Path),
			)
			http.Error(w, http.StatusText(http.StatusGatewayTimeout), http.StatusGatewayTimeout)
		} else if err := ex.Err(); err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, bridge.ErrQueueFull) {
				status = http.StatusServiceUnavailable
			}
			h.logger.Error("javascript: handler error",
				zap.Error(err),
				zap.String("method", rc.Method),
				zap.String("path", rc.Path),
			)
			http.Error(w, http.StatusText(status), status)
		} else {
			http.Error(w, "Not Found", http.StatusNotFound)
		}
	}

	metric.Requests.WithLabelValues(strconv.Itoa(srw.Status())).Inc()
	return nil
}

func (h *Handler) serve(ex *bridge.Exchange, rc *bridge.RequestContext) {
	w := ex.Writer()

	prog := h.prog
	if prog == nil {
		ex.SetHandled(true)
		http.Error(w, "javascript: runtime unavailable", http.StatusInternalServerError)
		ex.Finish()
		return
	}

	layers, params := prog.Match(rc.Method, rc.Path)
	if len(layers) == 0 {
		if h.Fallthrough {
			ex.Finish()
			return
		}
		ex.SetHandled(true)
		http.Error(w, "Not Found", http.StatusNotFound)
		ex.Finish()
		return
	}

	ex.SetHandled(true)
	reqObj := prog.BuildRequest(rc, params)
	resObj := prog.BuildResponse(w, reqObj)

	err := prog.Serve(ex, time.Duration(h.Timeout), func() error {
		return prog.RunChain(ex, layers, reqObj, resObj)
	})

	if err != nil {
		if _, ok := err.(*sobek.InterruptedError); ok {
			metric.Timeouts.Inc()
		}
		if !w.Written() {
			status := http.StatusInternalServerError
			if _, ok := err.(*sobek.InterruptedError); ok {
				status = http.StatusGatewayTimeout
			}
			http.Error(w, http.StatusText(status), status)
		}
		h.logger.Error("javascript: handler error",
			zap.Error(err),
			zap.String("method", rc.Method),
			zap.String("path", rc.Path),
		)
		ex.Finish()
	}
}

func (h *Handler) bridgeConfig() bridge.Config {
	return bridge.Config{
		Timeout:     time.Duration(h.Timeout),
		EnvAllow:    h.EnvAllow,
		FetchAllow:  h.FetchAllow,
		MaxBody:     h.MaxBody,
		AsyncBudget: h.AsyncBudget,
		Quit:        h.loop.Quit(),
	}
}

// EvalCode runs code on the directive's event loop and returns the result. The
// admin endpoint uses it for diagnostics; the directive must opt in with
// `eval`.
func (h *Handler) EvalCode(ctx context.Context, code string) (string, error) {
	if !h.Eval {
		return "", fmt.Errorf("eval is not enabled")
	}
	if h.loop == nil {
		return "", fmt.Errorf("handler is not provisioned")
	}
	type result struct {
		val string
		err error
	}
	ch := make(chan result, 1)
	if !h.loop.Submit(func() {
		prog := h.prog
		if prog == nil {
			ch <- result{err: fmt.Errorf("runtime unavailable")}
			return
		}
		v, err := prog.Runtime().RunString(code)
		if err != nil {
			ch <- result{err: err}
			return
		}
		ch <- result{val: v.String()}
	}) {
		return "", fmt.Errorf("event queue full")
	}
	select {
	case r := <-ch:
		return r.val, r.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func parseDirective(h httpcaddyfile.Helper) ([]httpcaddyfile.ConfigValue, error) {
	opts, err := config.Parse(h)
	if err != nil {
		return nil, err
	}
	return h.NewRoute(nil, &Handler{Options: opts}), nil
}

var (
	_ caddy.Provisioner           = (*Handler)(nil)
	_ caddy.CleanerUpper          = (*Handler)(nil)
	_ caddy.Validator             = (*Handler)(nil)
	_ caddyhttp.MiddlewareHandler = (*Handler)(nil)
)
