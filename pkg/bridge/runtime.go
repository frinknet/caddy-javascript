package bridge

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/grafana/sobek"
	"go.uber.org/zap"

	"github.com/frinknet/caddy-javascript/pkg/files"
	"github.com/frinknet/caddy-javascript/pkg/router"
)

type Scheduler interface {
	Submit(func()) bool
}

type Config struct {
	Timeout     time.Duration
	EnvAllow    []string
	FetchAllow  []string
	MaxBody     int64
	AsyncBudget int
	Quit        <-chan struct{}
}

type Program struct {
	rt     *sobek.Runtime
	routes *router.Table
	fs     *files.FS
	sched  Scheduler
	logger *zap.Logger
	cfg    Config

	current *Exchange

	timersMu    sync.Mutex
	timers      map[int64]*jsTimer
	nextTimerID int64

	fsObj          *sobek.Object
	pathObj        *sobek.Object
	appObj         *sobek.Object
	expressObj     *sobek.Object
	bufferObj      *sobek.Object
	urlObj         *sobek.Object
	querystringObj *sobek.Object
	utilObj        *sobek.Object
	eventsVal      sobek.Value

	modules map[string]*moduleRecord
	routers map[*sobek.Object]*router.Table
	statics map[*sobek.Object]*router.Static

	esmMu    sync.Mutex
	esmPaths map[sobek.ModuleRecord]string
	esmCache map[string]sobek.ModuleRecord
}

func NewProgram(cfg Config, fsys *files.FS, sched Scheduler, logger *zap.Logger) *Program {
	if cfg.AsyncBudget <= 0 {
		cfg.AsyncBudget = 64
	}
	p := &Program{
		rt:       sobek.New(),
		routes:   router.New(),
		fs:       fsys,
		sched:    sched,
		logger:   logger,
		cfg:      cfg,
		timers:   make(map[int64]*jsTimer),
		modules:  make(map[string]*moduleRecord),
		routers:  make(map[*sobek.Object]*router.Table),
		statics:  make(map[*sobek.Object]*router.Static),
		esmPaths: make(map[sobek.ModuleRecord]string),
		esmCache: make(map[string]sobek.ModuleRecord),
	}
	p.registerEnvironment()
	return p
}

func (p *Program) Load(src []byte) error {
	if looksLikeESM(src) {
		return p.LoadModule("entry.mjs", src)
	}
	_, err := p.rt.RunString(string(src))
	return err
}

// Match returns the layers registered for the request, in registration order,
// with the parameters captured from the path.
func (p *Program) Match(method, path string) ([]*router.Layer, map[string]string) {
	layers, params := p.routes.Match(method, path)
	return p.expandParams(layers), params
}

func (p *Program) Runtime() *sobek.Runtime {
	return p.rt
}

// Exchange tracks a single request across its asynchronous callbacks so the
// HTTP handler does not return until the response is actually complete.
type Exchange struct {
	w        *ResponseWriter
	pending  int32
	returned int32
	handled  int32
	timedOut int32
	once     sync.Once
	Done     chan struct{}

	errMu sync.Mutex
	err   error
}

func NewExchange(w *ResponseWriter) *Exchange {
	return &Exchange{w: w, Done: make(chan struct{})}
}

func (e *Exchange) Writer() *ResponseWriter {
	return e.w
}

func (e *Exchange) SetHandled(v bool) {
	if v {
		atomic.StoreInt32(&e.handled, 1)
	}
}

// SetError records a handler error that happened after the initial invocation
// returned, such as one raised from an async callback or from next(err).
func (e *Exchange) SetError(err error) {
	e.errMu.Lock()
	defer e.errMu.Unlock()
	e.err = err
}

func (e *Exchange) Err() error {
	e.errMu.Lock()
	defer e.errMu.Unlock()
	return e.err
}

func (e *Exchange) Handled() bool {
	return atomic.LoadInt32(&e.handled) == 1
}

// SetTimedOut marks the exchange as abandoned because a callback exceeded the
// execution budget. The request is finished immediately so a runaway callback
// cannot hold the slot open.
func (e *Exchange) SetTimedOut() {
	atomic.StoreInt32(&e.timedOut, 1)
}

func (e *Exchange) TimedOut() bool {
	return atomic.LoadInt32(&e.timedOut) == 1
}

func (e *Exchange) Finish() {
	e.once.Do(func() { close(e.Done) })
}

func (e *Exchange) maybeFinish() {
	if e.w.Written() || (atomic.LoadInt32(&e.returned) == 1 && atomic.LoadInt32(&e.pending) == 0) {
		e.Finish()
	}
}

func (e *Exchange) pendingDone() {
	atomic.AddInt32(&e.pending, -1)
	e.maybeFinish()
}

func (p *Program) Serve(ex *Exchange, timeout time.Duration, fn func() error) error {
	p.current = ex
	err := p.Run(timeout, fn)
	p.current = nil
	atomic.StoreInt32(&ex.returned, 1)
	ex.maybeFinish()
	return err
}

func (p *Program) Run(timeout time.Duration, fn func() error) error {
	return p.watch(timeout, fn)
}

// watch runs fn with a deadline and interrupts the runtime when it expires.
// Callers must run it on the scheduler goroutine: the interrupt flag is global
// to the runtime, so two overlapping watchdogs would interfere.
func (p *Program) watch(timeout time.Duration, fn func() error) error {
	_, err := p.watchDeadline(timeout, fn)
	return err
}

// watchDeadline reports whether the deadline expired in addition to returning
// fn's error, because a JS callback can swallow the interrupt it receives.
func (p *Program) watchDeadline(timeout time.Duration, fn func() error) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	var expired int32
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		if ctx.Err() == context.DeadlineExceeded {
			atomic.StoreInt32(&expired, 1)
			p.rt.Interrupt("script execution timeout exceeded")
		}
	}()
	defer func() {
		cancel()
		<-stopped
		p.rt.ClearInterrupt()
	}()

	err := fn()
	return atomic.LoadInt32(&expired) == 1, err
}

// watchAsync runs one async callback under the same watchdog as a handler
// invocation. A callback that spins or blocks in a native call is interrupted
// instead of wedging the shared event loop.
func (p *Program) watchAsync(fn func()) (bool, error) {
	timeout := p.cfg.Timeout
	if timeout <= 0 {
		fn()
		return false, nil
	}
	return p.watchDeadline(timeout, func() error {
		fn()
		return nil
	})
}

// asyncAbandoned records a callback that ran out of its budget and completes
// the exchange, so the request finishes with a timeout instead of waiting on
// callbacks that will never produce a response.
func (p *Program) asyncAbandoned(ex *Exchange, err error) error {
	if err == nil {
		err = errAsyncTimeout
	}
	if ex != nil {
		ex.SetTimedOut()
		ex.SetError(err)
		p.cancelTimers(ex)
		ex.Finish()
	}
	return err
}

var (
	errAsyncTimeout        = errors.New("javascript: async callback timeout exceeded")
	errAsyncBudgetExceeded = errors.New("javascript: async budget exceeded")
)

func (p *Program) trackAsync() (*Exchange, error) {
	ex := p.current
	if ex == nil {
		return nil, nil
	}
	if atomic.AddInt32(&ex.pending, 1) > int32(p.cfg.AsyncBudget) {
		atomic.AddInt32(&ex.pending, -1)
		return nil, errAsyncBudgetExceeded
	}
	return ex, nil
}

func (p *Program) submitAsync(ex *Exchange, fn func()) {
	if ex != nil && ex.TimedOut() {
		ex.pendingDone()
		return
	}
	run := func() {
		prev := p.current
		p.current = ex
		defer func() {
			p.current = prev
			if ex != nil {
				ex.pendingDone()
			}
		}()
		if ex != nil && ex.TimedOut() {
			return
		}
		if timedOut, err := p.watchAsync(fn); timedOut {
			err = p.asyncAbandoned(ex, err)
			if p.logger != nil {
				p.logger.Error("javascript: async callback error", zap.Error(err))
			}
		}
	}

	if p.sched == nil {
		run()
		return
	}
	if p.sched.Submit(run) {
		return
	}
	if ex != nil {
		ex.pendingDone()
	}
	if p.logger != nil {
		p.logger.Warn("javascript: event queue full, dropping async callback")
	}
}
