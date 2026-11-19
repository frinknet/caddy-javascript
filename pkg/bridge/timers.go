package bridge

import (
	"sync"
	"time"

	"github.com/grafana/sobek"
	"go.uber.org/zap"

	"github.com/frinknet/caddy-javascript/pkg/metric"
)

// jsTimer is a cancellable timer owned by the program. A timer holds one
// pending count on its exchange from creation until it fires or is cancelled,
// so the response stays open while a timer is outstanding.
type jsTimer struct {
	p    *Program
	id   int64
	ex   *Exchange
	stop chan struct{}
	once sync.Once

	releaseOnce sync.Once
}

func (t *jsTimer) cancel() {
	t.once.Do(func() { close(t.stop) })
}

// release drops the pending count and removes the timer from the registry. It
// is safe to call more than once.
func (t *jsTimer) release() {
	t.releaseOnce.Do(func() {
		t.p.forgetTimer(t.id)
		if t.ex != nil {
			t.ex.pendingDone()
		}
	})
}

func (t *jsTimer) stopped() bool {
	select {
	case <-t.stop:
		return true
	default:
		return false
	}
}

func (p *Program) newTimer(ex *Exchange) *jsTimer {
	p.timersMu.Lock()
	defer p.timersMu.Unlock()
	p.nextTimerID++
	t := &jsTimer{p: p, id: p.nextTimerID, ex: ex, stop: make(chan struct{})}
	p.timers[t.id] = t
	return t
}

func (p *Program) forgetTimer(id int64) {
	p.timersMu.Lock()
	defer p.timersMu.Unlock()
	delete(p.timers, id)
}

// cancelTimers stops every timer still attached to ex, so an abandoned
// exchange cannot keep scheduling callbacks.
func (p *Program) cancelTimers(ex *Exchange) {
	if ex == nil {
		return
	}
	p.timersMu.Lock()
	ids := make([]int64, 0, len(p.timers))
	for id, t := range p.timers {
		if t.ex == ex {
			ids = append(ids, id)
		}
	}
	p.timersMu.Unlock()
	for _, id := range ids {
		p.clearTimer(id)
	}
}

func (p *Program) clearTimer(id int64) {
	p.timersMu.Lock()
	t := p.timers[id]
	p.timersMu.Unlock()
	if t != nil {
		t.cancel()
	}
}

// submitAs queues fn on the scheduler with ex as the current exchange. Unlike
// submitAsync it does not touch the pending count; timers manage that
// themselves for their whole lifetime.
func (p *Program) submitAs(ex *Exchange, fn func()) bool {
	if ex != nil && ex.TimedOut() {
		return false
	}
	run := func() {
		prev := p.current
		p.current = ex
		defer func() { p.current = prev }()
		if timedOut, err := p.watchAsync(fn); timedOut {
			err = p.asyncAbandoned(ex, err)
			if p.logger != nil {
				p.logger.Error("javascript: async callback error", zap.Error(err))
			}
		}
	}
	if p.sched.Submit(run) {
		return true
	}
	metric.QueueFull.Inc()
	p.failAsync(ex, ErrQueueFull)
	if p.logger != nil {
		p.logger.Warn("javascript: event queue full, dropping timer callback")
	}
	return false
}

func (p *Program) registerTimers() {
	p.rt.Set("setTimeout", func(call sobek.FunctionCall) sobek.Value {
		return p.startTimer(call, false)
	})
	p.rt.Set("setInterval", func(call sobek.FunctionCall) sobek.Value {
		return p.startTimer(call, true)
	})
	p.rt.Set("clearTimeout", func(call sobek.FunctionCall) sobek.Value {
		p.clearTimerArg(call.Argument(0))
		return sobek.Undefined()
	})
	p.rt.Set("clearInterval", func(call sobek.FunctionCall) sobek.Value {
		p.clearTimerArg(call.Argument(0))
		return sobek.Undefined()
	})
}

func (p *Program) clearTimerArg(id sobek.Value) {
	if isNilValue(id) {
		return
	}
	p.clearTimer(id.ToInteger())
}

func (p *Program) startTimer(call sobek.FunctionCall, repeat bool) sobek.Value {
	name := "setTimeout"
	if repeat {
		name = "setInterval"
	}
	fn, ok := sobek.AssertFunction(call.Argument(0))
	if !ok {
		panic(p.rt.NewTypeError("%s requires a function", name))
	}
	ms := call.Argument(1).ToInteger()
	if ms < 0 {
		ms = 0
	}
	delay := time.Duration(ms) * time.Millisecond

	ex, terr := p.trackAsync()
	if terr != nil {
		return p.rt.ToValue(int64(-1))
	}
	t := p.newTimer(ex)
	if repeat && delay < time.Millisecond {
		delay = time.Millisecond
	}

	if repeat {
		go p.runInterval(t, fn, delay)
	} else {
		go p.runTimeout(t, fn, delay)
	}
	return p.rt.ToValue(t.id)
}

func (p *Program) runTimeout(t *jsTimer, fn sobek.Callable, delay time.Duration) {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-timer.C:
		if p.submitAs(t.ex, func() {
			defer t.release()
			if t.ex != nil && t.ex.TimedOut() {
				return
			}
			if t.stopped() {
				return
			}
			_, _ = fn(sobek.Undefined())
		}) {
			return
		}
		t.release()
	case <-t.stop:
		t.release()
	case <-p.cfg.Quit:
		t.release()
	}
}

func (p *Program) runInterval(t *jsTimer, fn sobek.Callable, delay time.Duration) {
	ticker := time.NewTicker(delay)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if t.ex != nil && t.ex.TimedOut() {
				t.release()
				return
			}
			if !p.submitAs(t.ex, func() {
				if t.ex != nil && t.ex.TimedOut() {
					t.cancel()
					return
				}
				if t.stopped() {
					return
				}
				_, _ = fn(sobek.Undefined())
			}) {
				t.release()
				return
			}
		case <-t.stop:
			t.release()
			return
		case <-p.cfg.Quit:
			t.release()
			return
		}
	}
}
