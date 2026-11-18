package bridge

import (
	"context"
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
	Timeout    time.Duration
	EnvAllow   []string
	FetchAllow []string
	MaxBody    int64
	Quit       <-chan struct{}
}

type Program struct {
	rt     *sobek.Runtime
	routes *router.Table
	fs     *files.FS
	sched  Scheduler
	logger *zap.Logger
	cfg    Config

	current *Exchange

	fsObj   *sobek.Object
	pathObj *sobek.Object
}

func NewProgram(cfg Config, fsys *files.FS, sched Scheduler, logger *zap.Logger) *Program {
	p := &Program{
		rt:     sobek.New(),
		routes: router.New(),
		fs:     fsys,
		sched:  sched,
		logger: logger,
		cfg:    cfg,
	}
	p.registerEnvironment()
	return p
}

func (p *Program) Load(src []byte) error {
	_, err := p.rt.RunString(string(src))
	return err
}

func (p *Program) Lookup(method, path string) sobek.Callable {
	if fn := p.routes.Lookup(method, path); fn != nil {
		return fn
	}
	return p.routes.Fallback()
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
	once     sync.Once
	Done     chan struct{}
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

func (e *Exchange) Handled() bool {
	return atomic.LoadInt32(&e.handled) == 1
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
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		if ctx.Err() == context.DeadlineExceeded {
			p.rt.Interrupt("script execution timeout exceeded")
		}
	}()

	err := fn()
	cancel()
	<-stopped
	p.rt.ClearInterrupt()
	return err
}

func (p *Program) trackAsync() *Exchange {
	ex := p.current
	if ex != nil {
		atomic.AddInt32(&ex.pending, 1)
	}
	return ex
}

func (p *Program) submitAsync(ex *Exchange, fn func()) {
	run := func() {
		prev := p.current
		p.current = ex
		defer func() {
			p.current = prev
			if ex != nil {
				atomic.AddInt32(&ex.pending, -1)
				ex.maybeFinish()
			}
		}()
		fn()
	}

	if p.sched.Submit(run) {
		return
	}
	if ex != nil {
		atomic.AddInt32(&ex.pending, -1)
		ex.maybeFinish()
	}
	if p.logger != nil {
		p.logger.Warn("javascript: event queue full, dropping async callback")
	}
}
