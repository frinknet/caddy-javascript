package bridge

import (
	"time"

	"github.com/grafana/sobek"
)

func (p *Program) registerTimers() {
	p.rt.Set("setTimeout", func(call sobek.FunctionCall) sobek.Value {
		fn, ok := sobek.AssertFunction(call.Argument(0))
		if !ok {
			panic(p.rt.NewTypeError("setTimeout requires a function"))
		}
		ms := call.Argument(1).ToInteger()
		if ms < 0 {
			ms = 0
		}
		delay := time.Duration(ms) * time.Millisecond

		ex := p.trackAsync()

		go func() {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-timer.C:
				p.submitAsync(ex, func() {
					_, _ = fn(sobek.Undefined())
				})
			case <-p.cfg.Quit:
				if ex != nil {
					ex.pendingDone()
				}
			}
		}()
		return sobek.Undefined()
	})
}
