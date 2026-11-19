// Package jobs hosts the resident background runtime: a second bridge.Program
// loaded once during provisioning and kept apart from request handling.
package jobs

import (
	"fmt"
	"sync"

	"go.uber.org/zap"

	"github.com/frinknet/caddy-javascript/pkg/bridge"
	"github.com/frinknet/caddy-javascript/pkg/files"
	"github.com/frinknet/caddy-javascript/pkg/sched"
)

// Hooks observes resident runtime lifecycle events. Tests use them to prove
// that the program loads and unloads; production leaves them nil.
type Hooks struct {
	Loaded   func(*bridge.Program)
	Unloaded func()
}

// Runtime is the resident background runtime. Its event loop goroutine is the
// only goroutine that touches the Program.
type Runtime struct {
	loop   *sched.Loop
	prog   *bridge.Program
	logger *zap.Logger
	hooks  Hooks

	stopOnce sync.Once
}

// Start loads src into a Program owned by a new goroutine and returns once the
// load has finished. The runtime idles after that until Stop.
func Start(src []byte, cfg bridge.Config, fsys *files.FS, logger *zap.Logger, queueSize int, hooks Hooks) (*Runtime, error) {
	if queueSize < 1 {
		return nil, fmt.Errorf("background runtime queue size must be at least 1")
	}

	r := &Runtime{
		loop:   sched.New(queueSize),
		logger: logger,
		hooks:  hooks,
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
	return r, nil
}

// Stop unloads the resident program and stops its event loop, draining work
// that is already queued.
func (r *Runtime) Stop() {
	r.stopOnce.Do(func() {
		r.loop.Stop()
		r.prog = nil
		if r.hooks.Unloaded != nil {
			r.hooks.Unloaded()
		}
	})
}
