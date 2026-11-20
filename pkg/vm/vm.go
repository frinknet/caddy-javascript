package vm

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/stdlib"
	"go.uber.org/zap"

	"github.com/frinknet/caddy-javascript/pkg/template"
)

const (
	DefaultMemory           = int64(64 << 20)
	DefaultQueueSize        = 256
	DefaultMaxWorkers       = 8
	DefaultFetchBodyLimit   = int64(1 << 20)
	DefaultProvisionTimeout = 10 * time.Second
	DefaultDrainTimeout     = 5 * time.Second
)

// vmSeq gives every VM a token for names that are process-wide, such as
// BroadcastChannel, so two VMs never hear each other.
var vmSeq atomic.Uint64

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
	Events           bool
	Watch            bool
	Reload           func() error
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
	// MaxCallbacks bounds host callbacks for this request. Zero uses
	// DefaultMaxCallbacks.
	MaxCallbacks int

	// Writer receives a streaming response. When nil, a stream is buffered
	// into Response.Body instead.
	Writer StreamWriter
	// StreamContext bounds a streaming response after it starts. The
	// dispatch itself is still bounded by the Handle context.
	StreamContext context.Context
	// Hijack takes over the connection for a WebSocket upgrade. A handler
	// supplies it; a nil value makes http.websocket() fail.
	Hijack func() (net.Conn, *bufio.ReadWriter, error)
}

type Response struct {
	Status   int
	Headers  map[string]string
	Body     []byte
	Streamed bool
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

	events         bool
	eventMu        sync.Mutex
	eventNames     map[string]struct{}
	eventListeners map[string][]quickjs.Value
	eventBound     bool
	eventSlots     chan struct{}

	watchMu  sync.Mutex
	watchers map[int]*watchEntry
	watchSeq int

	reloadMu  sync.Mutex
	reloadFn  func() error
	treeWatch *treeWatcher

	closing  atomic.Bool
	inflight atomic.Int64
	metrics  counters

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

	sb := &sandbox{root: root, dir: dir, modules: make(map[string][32]byte)}
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
	sb.record(entryName, src)

	loader := func(specifier, referrer string) (string, string, error) {
		name, err := sb.resolve(specifier, referrer)
		if err != nil {
			return "", "", err
		}
		src, err := sb.read(name)
		if err != nil {
			return "", "", err
		}
		sb.record(name, src)
		return string(src), name, nil
	}

	newRuntime := func() (*quickjs.Runtime, error) {
		worker := quickjs.New(
			quickjs.WithMemoryLimit(cfg.Memory),
			quickjs.WithStackSize(1<<16),
			quickjs.WithMaxCallDepth(1000),
			quickjs.WithLocale("en-US"),
		)
		worker.SetTimeZone(time.UTC)
		worker.SetModuleLoader(loader)
		return worker, nil
	}

	scope := fmt.Sprintf("caddy-js-%d:", vmSeq.Add(1))
	scoped := func(rt *quickjs.Runtime) error {
		return installChannelScope(rt, scope)
	}

	rt, err := newRuntime()
	if err != nil {
		root.Close()
		return nil, fmt.Errorf("javascript: create runtime: %w", err)
	}

	loop := stdlib.NewLoop(rt)
	lib := stdlib.Config{
		Loop: loop,
		FS:   &stdlib.FS{Root: dir},
		Workers: &stdlib.Workers{
			Max:       DefaultMaxWorkers,
			New:       newRuntime,
			Installed: scoped,
			Load: func(specifier string) (string, string, bool, error) {
				name, err := sb.resolve(specifier, entryName)
				if err != nil {
					return "", "", false, err
				}
				src, err := sb.read(name)
				if err != nil {
					return "", "", false, err
				}
				sb.record(name, src)
				return string(src), name, true, nil
			},
		},
	}
	if len(cfg.FetchAllow) > 0 {
		allow := append([]string(nil), cfg.FetchAllow...)
		lib.Fetch = &stdlib.Fetch{
			Allow: func(req *http.Request) error {
				host := req.URL.Hostname()
				if !fetchAllowed(allow, host) {
					return fmt.Errorf("javascript: fetch to %q is not allowed", host)
				}
				return nil
			},
			MaxBodyBytes: DefaultFetchBodyLimit,
		}
	}
	if err := stdlib.Install(rt, lib); err != nil {
		rt.Close()
		root.Close()
		return nil, fmt.Errorf("javascript: install runtime: %w", err)
	}
	if err := scoped(rt); err != nil {
		rt.Close()
		root.Close()
		return nil, fmt.Errorf("javascript: install messaging: %w", err)
	}

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
		reloadFn:       cfg.Reload,
	}
	v.ctx, v.cancel = context.WithCancel(context.Background())

	if cfg.Events {
		if err := v.installEvents(); err != nil {
			v.cleanupNew()
			return nil, fmt.Errorf("javascript: install events: %w", err)
		}
	}
	if err := v.installWatch(); err != nil {
		v.cleanupNew()
		return nil, fmt.Errorf("javascript: install watch: %w", err)
	}

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
	if cfg.Watch && cfg.Reload != nil {
		v.startTreeWatch()
	}
	if v.logger != nil {
		v.logger.Debug("javascript: vm started",
			zap.String("script", script),
			zap.String("root", dir),
			zap.Int64("memory", cfg.Memory),
			zap.Int("queue_size", cfg.QueueSize),
			zap.Int("max_workers", DefaultMaxWorkers),
		)
	}
	return v, nil
}

// cleanupNew releases a VM whose entry module never finished provisioning. The
// loop has not started yet, so the runtime is closed directly.
func (v *VM) cleanupNew() {
	v.cancel()
	v.stopAllWatchers()
	v.stopTreeWatch()
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
	v.stopAllWatchers()
	v.stopTreeWatch()
	v.rt.Close()
	_ = v.root.Close()
}

func (v *VM) Handle(ctx context.Context, scriptPath string, req Request) (*Response, error) {
	v.metrics.requests.Add(1)
	if err := v.err(); err != nil {
		if errors.Is(err, ErrClosed) {
			v.metrics.closed.Add(1)
		}
		return nil, err
	}

	v.mu.RLock()
	entry, ok := v.entries[scriptPath]
	layers := append([]layer(nil), v.layers[scriptPath]...)
	v.mu.RUnlock()
	if !ok {
		v.metrics.failures.Add(1)
		return nil, fmt.Errorf("javascript: module %q is not loaded", scriptPath)
	}

	select {
	case v.slots <- struct{}{}:
	default:
		v.metrics.queueFull.Add(1)
		return nil, ErrQueueFull
	}
	defer func() { <-v.slots }()

	v.inflight.Add(1)
	defer v.inflight.Add(-1)
	if v.closing.Load() {
		v.metrics.closed.Add(1)
		return nil, ErrClosed
	}

	resp := &response{}
	st, err := newRequestState(v, req, resp)
	if err != nil {
		v.metrics.failures.Add(1)
		return nil, err
	}
	st.lifetime = ctx
	if req.StreamContext != nil {
		st.lifetime = req.StreamContext
	}
	defer st.finish()

	ch := make(chan error, 1)
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
		ch <- portableError(err)
	})

	dispatchDone := false
	streamDone := false
	var dispatchErr error
	var wsDone <-chan struct{}
	for {
		streamed := st.streaming()
		if dispatchDone {
			if dispatchErr != nil {
				v.drainStreamEvents(st)
				return st.finishResponse(resp, dispatchErr)
			}
			if !streamed || streamDone {
				return st.finishResponse(resp, dispatchErr)
			}
			if ws := st.webSocket(); ws != nil && ws.isClosed() {
				return st.finishResponse(resp, dispatchErr)
			}
		}

		var events <-chan streamEvent
		if !streamDone {
			events = st.stream.events
		}
		if ws := st.webSocket(); ws != nil && !ws.isClosed() {
			wsDone = ws.done
		} else {
			wsDone = nil
		}
		ctxDone := ctx.Done()
		if streamed {
			ctxDone = st.lifetime.Done()
		}

		select {
		case dispatchErr = <-ch:
			dispatchDone = true
			ch = nil
			if dispatchErr != nil {
				v.metrics.failures.Add(1)
			}
		case ev := <-events:
			if err := v.applyStreamEvent(st, ev); err != nil {
				st.stream.stop()
				v.metrics.failures.Add(1)
				return st.finishResponse(resp, err)
			}
			if ev.end {
				streamDone = true
			}
		case <-wsDone:
			wsDone = nil
		case <-ctxDone:
			cause := ctx.Err()
			if streamed {
				cause = st.lifetime.Err()
			}
			if errors.Is(cause, context.DeadlineExceeded) {
				v.metrics.timeouts.Add(1)
			} else {
				v.metrics.failures.Add(1)
			}
			if streamed {
				return st.finishResponse(resp, cause)
			}
			return nil, cause
		case <-v.closed:
			v.metrics.closed.Add(1)
			if st.streaming() {
				return st.finishResponse(resp, ErrClosed)
			}
			return nil, ErrClosed
		case <-v.done:
			v.metrics.closed.Add(1)
			cause := v.runError()
			if cause == nil {
				cause = ErrClosed
			}
			if st.streaming() {
				return st.finishResponse(resp, cause)
			}
			return nil, cause
		}
	}
}

// finishResponse turns the request state into the response the handler sees.
// A buffered stream keeps its chunks in Body; a handler-owned stream has
// already written them.
func (s *requestState) finishResponse(resp *response, err error) (*Response, error) {
	out := resp.finish()
	if out != nil && out.Streamed {
		out.Body = s.stream.bodyCopy()
	}
	return out, err
}

// drainStreamEvents applies chunks that were queued before a dispatch error
// ended the request, so a partial response is not silently dropped.
func (v *VM) drainStreamEvents(st *requestState) {
	for {
		select {
		case ev := <-st.stream.events:
			if err := v.applyStreamEvent(st, ev); err != nil {
				return
			}
		default:
			return
		}
	}
}

// applyStreamEvent hands one chunk to the handler's writer. Without a writer
// the stream is buffered, which is how a unit test observes it.
func (v *VM) applyStreamEvent(st *requestState, ev streamEvent) error {
	w := st.stream.writer
	switch {
	case ev.start:
		if w == nil {
			return nil
		}
		return w.WriteHeader(ev.status, ev.headers)
	case ev.end:
		if w == nil {
			return nil
		}
		return w.Flush()
	default:
		if w == nil {
			st.stream.appendBody(ev.data)
			return nil
		}
		if err := w.Write(ev.data); err != nil {
			return err
		}
		return w.Flush()
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
		v.sandbox.record(name, src)
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
		v.clearEventListeners()
		v.stopAllWatchers()
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
		if err == nil {
			v.setReload(cfg.Reload, cfg.Watch)
		}
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
		v.closing.Store(true)
		v.stopAllWatchers()
		v.stopTreeWatch()
		v.drain()
		close(v.closed)
		v.loop.Close()
		v.cancel()
	})
	<-v.done
	return v.runError()
}

// drain waits for the requests already admitted to finish before the loop is
// closed. Work that arrives after Close is refused by the closing flag, and a
// request that outlasts the drain window is cut off with the rest of the loop.
func (v *VM) drain() {
	deadline := time.NewTimer(DefaultDrainTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if v.inflight.Load() == 0 {
			return
		}
		select {
		case <-deadline.C:
			if v.logger != nil {
				v.logger.Warn("javascript: shutdown drain timed out",
					zap.Int64("inflight", v.inflight.Load()),
				)
			}
			return
		case <-v.done:
			return
		case <-ticker.C:
		}
	}
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
	streamed bool
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
		Status:   r.status,
		Headers:  r.headers,
		Body:     r.body,
		Streamed: r.streamed,
	}
}

func (r *response) stream(status int, headers map[string]string) error {
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
	r.streamed = true
	r.status = status
	r.headers = headers
	return nil
}

func (r *response) responded() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.called
}

type sandbox struct {
	root *os.Root
	dir  string

	mu      sync.Mutex
	modules map[string][32]byte
}

// record remembers a module the loader read, so the VM can tell whether the
// file was edited and watch its directory.
func (s *sandbox) record(name string, src []byte) {
	s.mu.Lock()
	s.modules[name] = sha256.Sum256(src)
	s.mu.Unlock()
}

func (s *sandbox) moduleHashes() map[string][32]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	modules := make(map[string][32]byte, len(s.modules))
	for name, sum := range s.modules {
		modules[name] = sum
	}
	return modules
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
