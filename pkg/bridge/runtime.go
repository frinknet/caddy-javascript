package bridge

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/grafana/sobek"
	"go.uber.org/zap"

	"github.com/frinknet/caddy-javascript/pkg/files"
	"github.com/frinknet/caddy-javascript/pkg/metric"
	"github.com/frinknet/caddy-javascript/pkg/router"
)

type Scheduler interface {
	Submit(func()) bool
}

const DefaultIOConcurrency = 64

type Config struct {
	Timeout       time.Duration
	EnvAllow      []string
	FetchAllow    []string
	MaxBody       int64
	AsyncBudget   int
	IOConcurrency int
	Quit          <-chan struct{}
	// EnqueueJob admits a serialized job to the resident background runtime.
	// It reports false when the queue is full or the runtime is unavailable.
	EnqueueJob func(name string, payload []byte, opts JobOptions) (string, bool)
}

// JobOptions carries the retry policy a producer can attach to a job.
type JobOptions struct {
	MaxAttempts int
	Backoff     time.Duration
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

	ioLimit  int
	ioMu     sync.Mutex
	ioActive int
	ioQueue  []func()

	fsObj          *sobek.Object
	pathObj        *sobek.Object
	appObj         *sobek.Object
	settings       *sobek.Object
	engines        *sobek.Object
	expressObj     *sobek.Object
	bufferObj      *sobek.Object
	urlObj         *sobek.Object
	querystringObj *sobek.Object
	utilObj        *sobek.Object
	eventsVal      sobek.Value
	jobsObj        *sobek.Object

	modules map[string]*moduleRecord
	routers map[*sobek.Object]*router.Table
	statics map[*sobek.Object]*router.Static
	workers map[string]sobek.Value

	reqSymbol *sobek.Symbol

	esmMu    sync.Mutex
	esmPaths map[sobek.ModuleRecord]string
	esmCache map[string]sobek.ModuleRecord
}

func NewProgram(cfg Config, fsys *files.FS, sched Scheduler, logger *zap.Logger) *Program {
	if cfg.AsyncBudget <= 0 {
		cfg.AsyncBudget = 64
	}
	if cfg.IOConcurrency <= 0 {
		cfg.IOConcurrency = DefaultIOConcurrency
	}
	p := &Program{
		rt:       sobek.New(),
		routes:   router.New(),
		fs:       fsys,
		sched:    sched,
		logger:   logger,
		cfg:      cfg,
		ioLimit:  cfg.IOConcurrency,
		timers:   make(map[int64]*jsTimer),
		modules:  make(map[string]*moduleRecord),
		routers:  make(map[*sobek.Object]*router.Table),
		statics:  make(map[*sobek.Object]*router.Static),
		workers:  make(map[string]sobek.Value),
		esmPaths: make(map[sobek.ModuleRecord]string),
		esmCache: make(map[string]sobek.ModuleRecord),
	}
	p.reqSymbol = sobek.NewSymbol("requestContext")
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
	metric.QueueFull.Inc()
	p.failAsync(ex, ErrQueueFull)
	if ex != nil {
		ex.pendingDone()
	}
	if p.logger != nil {
		p.logger.Warn("javascript: event queue full, dropping async callback")
	}
}

// ErrQueueFull is recorded on an exchange whose callback could not be queued.
// The request answers 503 instead of waiting for a response that will never
// arrive.
var ErrQueueFull = errors.New("javascript: event queue full")

// failAsync abandons the outstanding work of an exchange after a callback was
// dropped, so the handler stops waiting and reports the failure to the client.
func (p *Program) failAsync(ex *Exchange, err error) {
	if ex == nil {
		return
	}
	if ex.Err() == nil {
		ex.SetError(err)
	}
	p.cancelTimers(ex)
}

// goIO runs fn off the event loop while keeping the number of concurrently
// running I/O goroutines bounded. Work over the limit waits for a running slot
// instead of spawning another goroutine.
func (p *Program) goIO(fn func()) {
	p.ioMu.Lock()
	if p.ioActive < p.ioLimit {
		p.ioActive++
		p.ioMu.Unlock()
		go p.runIO(fn)
		return
	}
	p.ioQueue = append(p.ioQueue, fn)
	p.ioMu.Unlock()
}

func (p *Program) runIO(fn func()) {
	defer func() {
		p.ioMu.Lock()
		if len(p.ioQueue) > 0 {
			next := p.ioQueue[0]
			p.ioQueue = p.ioQueue[1:]
			p.ioMu.Unlock()
			go p.runIO(next)
			return
		}
		p.ioActive--
		p.ioMu.Unlock()
	}()
	fn()
}

// newFSError builds the Error object an fs callback receives. Node callers
// inspect err.message and err.code, so both are always present.
func (p *Program) newFSError(err error) *sobek.Object {
	obj := p.rt.NewGoError(err)
	_ = obj.Set("name", "Error")
	_ = obj.Set("code", fsErrorCode(err))
	return obj
}

func fsErrorCode(err error) string {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "ENOENT"
	case errors.Is(err, os.ErrPermission):
		return "EACCES"
	case errors.Is(err, os.ErrExist):
		return "EEXIST"
	default:
		return "EIO"
	}
}

// newFetchError builds the Error object a fetch callback receives.
func (p *Program) newFetchError(err error) *sobek.Object {
	obj := p.rt.NewGoError(err)
	_ = obj.Set("name", "Error")
	_ = obj.Set("code", fetchErrorCode(err))
	return obj
}

func fetchErrorCode(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "ABORT_ERR"
	case errors.Is(err, context.DeadlineExceeded):
		return "ETIMEDOUT"
	default:
		return "ERR_FETCH_FAILED"
	}
}
