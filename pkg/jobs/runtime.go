// Package jobs hosts the resident background runtime: a second bridge.Program
// loaded once during provisioning and kept apart from request handling.
package jobs

import (
	"fmt"
	"sync"
	"time"

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
// that goroutine. A nil return acks the job; an error fails the attempt and
// schedules a retry when attempts remain. A nil executor consumes jobs as
// acknowledged without running JavaScript.
type Executor func(*bridge.Program, *Job) error

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
func (r *Runtime) Enqueue(job *Job) bool {
	return r.queue.Admit(job)
}

// work is the supervisor goroutine: it pulls one job at a time, runs its
// attempt through the scheduler, and re-admits failed jobs after their backoff.
func (r *Runtime) work() {
	defer close(r.workerDone)
	for {
		job, ok := r.queue.Next()
		if !ok {
			return
		}
		r.run(job)
	}
}

func (r *Runtime) run(job *Job) {
	if job == nil {
		return
	}

	if err := job.Start(); err != nil {
		r.logSkipped(job, err)
		return
	}

	if err := r.dispatch(job); err != nil {
		if _, ferr := job.Fail(err); ferr != nil {
			r.logSkipped(job, ferr)
		}
	} else if err := job.Ack(); err != nil {
		r.logSkipped(job, err)
	}

	if job.State() != StateRetrying {
		return
	}

	if !r.waitRetry(job) {
		job.Cancel()
		return
	}

	if !r.queue.Admit(job) {
		if r.queue.Closed() {
			job.Cancel()
			return
		}
		job.Abandon(ErrJobQueueFull)
	}
}

// dispatch runs one attempt on the event loop and waits for it, so jobs never
// overlap on the resident runtime.
func (r *Runtime) dispatch(job *Job) error {
	if r.exec == nil {
		return nil
	}

	done := make(chan error, 1)
	if !r.loop.Submit(func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				done <- fmt.Errorf("javascript: job %s panicked: %v", job.ID, recovered)
			}
		}()
		done <- r.exec(r.prog, job)
	}) {
		metric.QueueFull.Inc()
		return ErrJobQueueFull
	}
	return <-done
}

// waitRetry sleeps for the job's backoff, shortened so an expired deadline is
// noticed promptly. It reports false when the runtime is shutting down.
func (r *Runtime) waitRetry(job *Job) bool {
	delay := job.RetryDelay()
	if !job.Deadline.IsZero() {
		if remaining := time.Until(job.Deadline); remaining < delay {
			delay = remaining
		}
	}
	if delay <= 0 {
		return true
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-r.queue.Done():
		return false
	}
}

func (r *Runtime) logSkipped(job *Job, err error) {
	if r.logger == nil {
		return
	}
	r.logger.Debug("javascript: job not advanced",
		zap.String("job_id", job.ID),
		zap.String("job", job.Name),
		zap.String("state", job.State().String()),
		zap.Error(err))
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
