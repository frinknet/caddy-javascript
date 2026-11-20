package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/stdlib"
	"go.uber.org/zap"

	"github.com/frinknet/caddy-javascript/pkg/template"
)

const (
	DefaultMemory           = int64(64 << 20)
	DefaultQueueSize        = 256
	DefaultProvisionTimeout = 10 * time.Second
)

var (
	ErrQueueFull = errors.New("javascript: vm queue full")
	ErrClosed    = errors.New("javascript: vm closed")
	ErrStopped   = errors.New("javascript: vm stopped")
)

type Config struct {
	ScriptPath       string
	RootDir          string
	Memory           int64
	QueueSize        int
	RequireHandler   bool
	ProvisionTimeout time.Duration
	Logger           *zap.Logger
	Caddy            *CaddyConfig
	EnvAllow         []string
	FetchAllow       []string
	TemplateFuncs    map[string]any
}

// Request is one dispatch into the VM. Req carries the live request: scripts
// see its method, URL, headers, cookies, and body, and rewrite() mutates it for
// downstream Caddy handlers. Vars is the request's variable table; nil means a
// private table only the script can see. Lookup, when set, resolves names the
// table does not hold, such as placeholders produced by Caddy's map directive.
type Request struct {
	Req     *http.Request
	Vars    map[string]any
	Lookup  func(name string) (any, bool)
	Set     func(name string, value any)
	Delete  func(name string)
	MaxBody int64
}

type Response struct {
	Status  int
	Headers map[string]string
	Body    []byte
}

type VM struct {
	rt      *quickjs.Runtime
	loop    *stdlib.Loop
	root    *os.Root
	sandbox *sandbox

	mu             sync.RWMutex
	entries        map[string]quickjs.Value
	layers         map[string][]layer
	requireHandler bool

	currentScript string
	varsFactory   quickjs.Value
	headerFactory quickjs.Value
	active        *requestState
	registry      *template.Registry
	templateFuncs map[string]any
	envAllow      []string
	fetchAllow    []string

	config     quickjs.Value
	stringify  quickjs.Value
	deleteProp quickjs.Value
	hasConfig  bool
	reloading  bool
	apply      func(doc []byte) error
	applied    []byte

	slots  chan struct{}
	closed chan struct{}
	done   chan struct{}

	ctx    context.Context
	cancel context.CancelFunc

	closeOnce sync.Once
	errMu     sync.Mutex
	runErr    error

	logger *zap.Logger
}

func New(cfg Config) (*VM, error) {
	if cfg.Memory <= 0 {
		cfg.Memory = DefaultMemory
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = DefaultQueueSize
	}
	if cfg.ScriptPath == "" {
		return nil, fmt.Errorf("javascript: script path is required")
	}
	if cfg.RootDir == "" {
		cfg.RootDir = filepath.Dir(cfg.ScriptPath)
	}

	script, err := filepath.Abs(cfg.ScriptPath)
	if err != nil {
		return nil, fmt.Errorf("javascript: script path: %w", err)
	}
	dir, err := filepath.Abs(cfg.RootDir)
	if err != nil {
		return nil, fmt.Errorf("javascript: root: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("javascript: root %q: %w", dir, err)
	}

	sb := &sandbox{root: root, dir: dir}
	entryName, err := sb.name(script)
	if err != nil {
		root.Close()
		return nil, err
	}
	src, err := sb.read(entryName)
	if err != nil {
		root.Close()
		return nil, err
	}

	rt := quickjs.New(
		quickjs.WithMemoryLimit(cfg.Memory),
		quickjs.WithStackSize(1<<16),
		quickjs.WithMaxCallDepth(1000),
		quickjs.WithLocale("en-US"),
	)
	rt.SetTimeZone(time.UTC)

	loop := stdlib.NewLoop(rt)
	if err := stdlib.Install(rt, stdlib.Config{Loop: loop, NoWebAPIs: true}); err != nil {
		rt.Close()
		root.Close()
		return nil, fmt.Errorf("javascript: install runtime: %w", err)
	}

	rt.SetModuleLoader(func(specifier, referrer string) (string, string, error) {
		name, err := sb.resolve(specifier, referrer)
		if err != nil {
			return "", "", err
		}
		src, err := sb.read(name)
		if err != nil {
			return "", "", err
		}
		return string(src), name, nil
	})

	provisionTimeout := cfg.ProvisionTimeout
	if provisionTimeout <= 0 {
		provisionTimeout = DefaultProvisionTimeout
	}
	provisionCtx, cancelProvision := context.WithTimeout(context.Background(), provisionTimeout)
	defer cancelProvision()

	v := &VM{
		rt:             rt,
		loop:           loop,
		root:           root,
		sandbox:        sb,
		entries:        map[string]quickjs.Value{},
		layers:         map[string][]layer{},
		requireHandler: cfg.RequireHandler,
		slots:          make(chan struct{}, cfg.QueueSize),
		closed:         make(chan struct{}),
		done:           make(chan struct{}),
		logger:         cfg.Logger,
		envAllow:       cfg.EnvAllow,
		fetchAllow:     cfg.FetchAllow,
		templateFuncs:  cfg.TemplateFuncs,
	}
	v.ctx, v.cancel = context.WithCancel(context.Background())

	factory, err := rt.Eval(varsFactorySource)
	if err != nil {
		v.cleanupNew()
		return nil, fmt.Errorf("javascript: install runtime bridge: %w", err)
	}
	v.varsFactory = factory
	headerFactory, err := rt.Eval(headersFactorySource)
	if err != nil {
		v.cleanupNew()
		return nil, fmt.Errorf("javascript: install headers bridge: %w", err)
	}
	v.headerFactory = headerFactory
	proto, err := rt.Eval("Object.getPrototypeOf(globalThis)")
	if err != nil {
		v.cleanupNew()
		return nil, fmt.Errorf("javascript: read global prototype: %w", err)
	}
	installGlobals, err := rt.Eval(globalsFactorySource)
	if err != nil {
		v.cleanupNew()
		return nil, fmt.Errorf("javascript: install globals bridge: %w", err)
	}
	if _, err := installGlobals.Call(proto, v.globalsLive, v.globalsKnown, v.globalsLookup, v.globalsSet); err != nil {
		v.cleanupNew()
		return nil, fmt.Errorf("javascript: install globals bridge: %w", err)
	}
	if err := rt.Set("route", v.route); err != nil {
		v.cleanupNew()
		return nil, fmt.Errorf("javascript: install route(): %w", err)
	}
	if err := v.configureConfig(cfg.Caddy); err != nil {
		v.cleanupNew()
		return nil, fmt.Errorf("javascript: install config: %w", err)
	}
	if err := v.installTemplate(); err != nil {
		v.cleanupNew()
		return nil, fmt.Errorf("javascript: install template: %w", err)
	}

	v.currentScript = script
	ns, err := rt.EvalModuleContext(provisionCtx, entryName, string(src))
	v.currentScript = ""
	if err != nil {
		v.cleanupNew()
		return nil, fmt.Errorf("javascript: evaluate %s: %w", entryName, err)
	}

	if edited, err := v.configEdited(); err != nil {
		v.cleanupNew()
		return nil, fmt.Errorf("javascript: check config: %w", err)
	} else if edited {
		v.cleanupNew()
		return nil, errors.New("javascript: config changed during module evaluation; edit it from a message or timer")
	}

	entry, err := ns.Get("default")
	if err != nil {
		v.cleanupNew()
		return nil, fmt.Errorf("javascript: read default export: %w", err)
	}
	if cfg.RequireHandler && !entry.IsFunction() {
		v.cleanupNew()
		return nil, fmt.Errorf("javascript: %s must export a default function", entryName)
	}
	v.entries[script] = entry

	loop.Begin()
	go v.run()
	return v, nil
}

// cleanupNew releases a VM whose entry module never finished provisioning. The
// loop has not started yet, so the runtime is closed directly.
func (v *VM) cleanupNew() {
	v.cancel()
	v.rt.Close()
	_ = v.root.Close()
}

func (v *VM) run() {
	defer close(v.done)
	if err := v.loop.Run(v.ctx); err != nil && !errors.Is(err, context.Canceled) {
		v.setErr(fmt.Errorf("javascript: vm loop: %w", err))
		if v.logger != nil {
			v.logger.Error("javascript: vm loop stopped", zap.Error(err))
		}
	}
	v.rt.Close()
	_ = v.root.Close()
}

func (v *VM) Handle(ctx context.Context, scriptPath string, req Request) (*Response, error) {
	if err := v.err(); err != nil {
		return nil, err
	}

	v.mu.RLock()
	entry, ok := v.entries[scriptPath]
	layers := append([]layer(nil), v.layers[scriptPath]...)
	v.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("javascript: module %q is not loaded", scriptPath)
	}

	select {
	case v.slots <- struct{}{}:
	default:
		return nil, ErrQueueFull
	}
	defer func() { <-v.slots }()

	type result struct {
		resp *Response
		err  error
	}
	ch := make(chan result, 1)
	resp := &response{}
	st, err := newRequestState(req, resp)
	if err != nil {
		return nil, err
	}

	v.loop.Post(func() {
		v.active = st
		defer func() { v.active = nil }()

		httpObj, err := v.newHTTP(ctx, st, resp)
		if err == nil {
			var responded bool
			responded, err = v.runChain(ctx, st, httpObj, layers)
			if err == nil && !responded && !resp.responded() {
				_, err = entry.CallContext(ctx, httpObj)
			}
		}
		st.finish()
		ch <- result{resp: resp.finish(), err: portableError(err)}
	})

	select {
	case r := <-ch:
		return r.resp, r.err
	case <-ctx.Done():
		st.finish()
		return nil, ctx.Err()
	case <-v.closed:
		st.finish()
		return nil, ErrClosed
	case <-v.done:
		st.finish()
		if err := v.runError(); err != nil {
			return nil, err
		}
		return nil, ErrClosed
	}
}

// globalsLive reports whether a request dispatch is on the VM goroutine. Only
// then does the global space resolve to the request's Caddy vars.
func (v *VM) globalsLive() bool {
	return v.active != nil
}

// globalsKnown reports whether a bare identifier resolves to a Caddy
// placeholder. It is called on the VM goroutine while a dispatch is active.
func (v *VM) globalsKnown(name string) (bool, error) {
	if v.active == nil {
		return false, nil
	}
	_, ok := v.active.placeholder(name)
	return ok, nil
}

// globalsLookup returns the value behind a bare identifier. The JS side calls
// it only after globalsKnown says the name is known.
func (v *VM) globalsLookup(name string) (any, error) {
	if v.active == nil {
		return nil, nil
	}
	value, _ := v.active.placeholder(name)
	return value, nil
}

// globalsSet writes a bare identifier assignment back to the request. A name
// that came from the replacer is overridden there too, so a downstream Caddy
// placeholder such as {site} sees the new value.
func (v *VM) globalsSet(name string, value any) error {
	if v.active == nil {
		return errors.New("javascript: no request is active")
	}
	return v.active.globalSet(name, value)
}

// portableError detaches an error from the runtime. The caller formats it after
// the VM goroutine has moved on, and a *quickjs.Error reads its live value when
// formatted.
func portableError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, ErrClosed):
		return ErrClosed
	}
	var httpErr *template.HTTPError
	if errors.As(err, &httpErr) {
		return httpErr
	}
	return errors.New(err.Error())
}

func (v *VM) Load(ctx context.Context, scriptPath string) error {
	return v.join(ctx, Config{ScriptPath: scriptPath})
}

// Join loads the module in cfg and refreshes the VM's site block. It is used
// when a new configuration generation adopts a running site VM.
func (v *VM) Join(ctx context.Context, cfg Config) error {
	return v.join(ctx, cfg)
}

// join evaluates another module in the VM and refreshes the worker's config
// reference. It also runs when the module is already loaded, so a new
// generation can reuse the VM.
func (v *VM) join(ctx context.Context, cfg Config) error {
	script, err := filepath.Abs(cfg.ScriptPath)
	if err != nil {
		return fmt.Errorf("javascript: script path: %w", err)
	}

	v.mu.RLock()
	_, loaded := v.entries[script]
	v.mu.RUnlock()

	if err := v.err(); err != nil {
		return err
	}

	var name string
	var src []byte
	if !loaded {
		name, err = v.sandbox.name(script)
		if err != nil {
			return err
		}
		src, err = v.sandbox.read(name)
		if err != nil {
			return err
		}
	}

	ch := make(chan error, 1)
	v.loop.Post(func() {
		v.envAllow = cfg.EnvAllow
		v.fetchAllow = cfg.FetchAllow
		v.templateFuncs = cfg.TemplateFuncs
		if err := v.installTemplate(); err != nil {
			ch <- fmt.Errorf("javascript: install template: %w", err)
			return
		}
		if err := v.configureConfig(cfg.Caddy); err != nil {
			ch <- fmt.Errorf("javascript: install config: %w", err)
			return
		}
		if loaded {
			ch <- nil
			return
		}
		v.currentScript = script
		ns, err := v.rt.EvalModuleContext(ctx, name, string(src))
		v.currentScript = ""
		if err != nil {
			ch <- fmt.Errorf("javascript: evaluate %s: %w", name, err)
			return
		}
		if edited, err := v.configEdited(); err != nil {
			ch <- fmt.Errorf("javascript: check config: %w", err)
			return
		} else if edited {
			ch <- errors.New("javascript: config changed during module evaluation; edit it from a message or timer")
			return
		}
		entry, err := ns.Get("default")
		if err != nil {
			ch <- fmt.Errorf("javascript: read default export: %w", err)
			return
		}
		if v.requireHandler && !entry.IsFunction() {
			ch <- fmt.Errorf("javascript: %s must export a default function", name)
			return
		}
		v.mu.Lock()
		v.entries[script] = entry
		v.mu.Unlock()
		ch <- nil
	})

	select {
	case err := <-ch:
		return err
	case <-v.done:
		if err := v.runError(); err != nil {
			return err
		}
		return ErrClosed
	}
}

func (v *VM) Close() error {
	v.closeOnce.Do(func() {
		close(v.closed)
		v.loop.Close()
		v.cancel()
	})
	<-v.done
	return v.runError()
}

func (v *VM) runError() error {
	v.errMu.Lock()
	defer v.errMu.Unlock()
	return v.runErr
}

func (v *VM) setErr(err error) {
	v.errMu.Lock()
	if v.runErr == nil {
		v.runErr = err
	}
	v.errMu.Unlock()
}

func (v *VM) err() error {
	v.errMu.Lock()
	defer v.errMu.Unlock()
	if v.runErr != nil {
		return v.runErr
	}
	select {
	case <-v.closed:
		return ErrClosed
	default:
		return nil
	}
}

type response struct {
	mu       sync.Mutex
	called   bool
	finished bool
	status   int
	headers  map[string]string
	body     []byte
}

func (r *response) respond(status int, headers map[string]any, body any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished {
		return errors.New("javascript: response already finished")
	}
	if r.called {
		return errors.New("javascript: response already sent")
	}
	if status < 100 || status > 999 {
		return fmt.Errorf("javascript: invalid status %d", status)
	}
	r.called = true
	r.status = status
	r.headers = make(map[string]string, len(headers))
	for k, val := range headers {
		if val == nil {
			continue
		}
		r.headers[k] = headerValue(val)
	}
	switch b := body.(type) {
	case nil:
		r.body = nil
	case string:
		r.body = []byte(b)
	case []byte:
		r.body = append([]byte(nil), b...)
	default:
		r.body = []byte(fmt.Sprint(b))
	}
	return nil
}

func (r *response) finish() *Response {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finished = true
	if !r.called {
		return nil
	}
	return &Response{
		Status:  r.status,
		Headers: r.headers,
		Body:    r.body,
	}
}

func (r *response) responded() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.called
}

type sandbox struct {
	root *os.Root
	dir  string
}

func (s *sandbox) name(abs string) (string, error) {
	rel, err := filepath.Rel(s.dir, abs)
	if err != nil {
		return "", fmt.Errorf("javascript: resolve %q: %w", abs, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("javascript: %q is outside the root %q", abs, s.dir)
	}
	return path.Join("/", filepath.ToSlash(rel)), nil
}

func (s *sandbox) resolve(specifier, referrer string) (string, error) {
	if specifier == "" {
		return "", fmt.Errorf("javascript: empty module specifier")
	}
	if !strings.HasPrefix(specifier, ".") && !strings.HasPrefix(specifier, "/") && !filepath.IsAbs(specifier) {
		return "", fmt.Errorf("javascript: bare module specifier %q is not supported", specifier)
	}

	var name string
	if strings.HasPrefix(specifier, "/") {
		name = path.Clean(filepath.ToSlash(specifier))
	} else {
		base := "/"
		if referrer != "" {
			base = path.Dir(referrer)
		}
		name = path.Join(base, filepath.ToSlash(specifier))
	}

	abs := filepath.Join(s.dir, filepath.FromSlash(strings.TrimPrefix(name, "/")))
	rel, err := filepath.Rel(s.dir, abs)
	if err != nil {
		return "", fmt.Errorf("javascript: resolve %q: %w", specifier, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("javascript: module %q escapes the root", specifier)
	}
	return path.Join("/", filepath.ToSlash(rel)), nil
}

func (s *sandbox) read(name string) ([]byte, error) {
	rel := strings.TrimPrefix(path.Clean(name), "/")
	f, err := s.root.Open(filepath.FromSlash(rel))
	if err != nil {
		return nil, fmt.Errorf("javascript: read %q: %w", name, err)
	}
	defer f.Close()
	return io.ReadAll(f)
}

// headerValue renders a JavaScript header value. An array, which a script reads
// when a request carries repeated headers, joins the way HTTP repeats a field.
func headerValue(value any) string {
	switch v := value.(type) {
	case []string:
		return strings.Join(v, ", ")
	case []any:
		parts := make([]string, len(v))
		for i, item := range v {
			parts[i] = fmt.Sprint(item)
		}
		return strings.Join(parts, ", ")
	default:
		return fmt.Sprint(value)
	}
}
