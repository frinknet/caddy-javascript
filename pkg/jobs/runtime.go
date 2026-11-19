// Package jobs hosts the resident background runtime: a second bridge.Program
// loaded once during provisioning and kept apart from request handling.
package jobs

import (
	"fmt"
	"sync"

	"go.uber.org/zap"

	"github.com/frinknet/caddy-javascript/pkg/bridge"
	"github.com/frinknet/caddy-javascript/pkg/files"
	"github.com/frinknet/caddy-javascript/pkg/metric"
	"github.com/frinknet/caddy-javascript/pkg/sched"
)

// Hooks observes resident runtime lifecycle events. Tests use them to prove
// that the program loads and unloads; production leaves them nil.
type Hooks struct {
	Loaded   func(*bridge.Program)
	Unloaded func()
}

// Executor runs one job on the resident event loop. It is called only from
// that goroutine. A nil executor consumes jobs without running JavaScript.
type Executor func(*bridge.Program, Job)

// Runtime is the resident background runtime. Its event loop goroutine is the
// only goroutine that touches the Program.
type Runtime struct {
	loop   *sched.Loop
	prog   *bridge.Program
	queue  *Queue
	exec   Executor
	logger *zap.Logger
	hooks  Hooks

	workerDone chan struct{}
	stopOnce   sync.Once
}

// Start loads src into a Program owned by a new goroutine and returns once the
// load has finished. The runtime idles after that until Stop.
func Start(src []byte, cfg bridge.Config, fsys *files.FS, logger *zap.Logger, queueSize int, exec Executor, hooks Hooks) (*Runtime, error) {
	queue, err := NewQueue(queueSize)
	if err != nil {
		return nil, err
	}

	r := &Runtime{
		loop:       sched.New(queueSize),
		queue:      queue,
		exec:       exec,
		logger:     logger,
		hooks:      hooks,
		workerDone: make(chan struct{}),
	}
	r.loop.OnPanic = func(recovered any) {
		if r.logger != nil {
			r.logger.Error("javascript: background runtime recovered from panic", zap.Any("panic", recovered))
		}
	}
	cfg.Quit = r.loop.Quit()

	loaded := make(chan error, 1)
	go r.loop.Run(nil)
	if !r.loop.Submit(func() {
		prog := bridge.NewProgram(cfg, fsys, r.loop, r.logger)
		if err := prog.Load(src); err != nil {
			loaded <- err
			return
		}
		r.prog = prog
		if r.hooks.Loaded != nil {
			r.hooks.Loaded(prog)
		}
		loaded <- nil
	}) {
		r.loop.Stop()
		return nil, fmt.Errorf("background runtime queue is full")
	}

	if err := <-loaded; err != nil {
		r.loop.Stop()
		return nil, err
	}

	go r.work()
	return r, nil
}

// Enqueue admits a job for the resident runtime. It never blocks and reports
// false when the bounded queue is full or shutting down.
func (r *Runtime) Enqueue(job Job) bool {
	return r.queue.Admit(job)
}

// work is the supervisor goroutine: it pulls one job at a time and re-enters
// the runtime through the scheduler, waiting for each callback so the event
// loop always gets a chance to run other work.
func (r *Runtime) work() {
	defer close(r.workerDone)
	for {
		job, ok := r.queue.Next()
		if !ok {
			return
		}
		r.dispatch(job)
	}
}

func (r *Runtime) dispatch(job Job) {
	done := make(chan struct{})
	if !r.loop.Submit(func() {
		defer close(done)
		if r.exec == nil {
			return
		}
		r.exec(r.prog, job)
	}) {
		metric.QueueFull.Inc()
		return
	}
	<-done
}

// Stop unloads the resident program and stops its event loop, draining work
// that is already queued.
func (r *Runtime) Stop() {
	r.stopOnce.Do(func() {
		r.queue.Close()
		<-r.workerDone
		r.loop.Stop()
		r.prog = nil
		if r.hooks.Unloaded != nil {
			r.hooks.Unloaded()
		}
	})
}
