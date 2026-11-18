package bridge

import (
	"errors"

	"github.com/grafana/sobek"

	"github.com/frinknet/caddy-javascript/pkg/router"
)

// Chain drives the layers matched for one request with Express-style next()
// semantics: layers run in registration order, next() advances to the next one,
// and next(err) skips ahead to the next four-argument error handler.
type Chain struct {
	p      *Program
	ex     *Exchange
	layers []*router.Layer
	req    *sobek.Object
	res    *sobek.Object
	next   sobek.Value

	index     int
	pending   sobek.Value
	errMsg    string
	nextCalls int
	done      bool
}

// RunChain runs the matched layers. It returns an error only when the runtime
// was interrupted, so the caller can tell a timeout from a handler error; other
// errors are reported on the exchange.
func (p *Program) RunChain(ex *Exchange, layers []*router.Layer, req, res *sobek.Object) error {
	c := &Chain{p: p, ex: ex, layers: layers, req: req, res: res}
	c.next = p.rt.ToValue(func(call sobek.FunctionCall) sobek.Value {
		return c.advance(call.Argument(0))
	})
	return c.step(sobek.Undefined())
}

func (c *Chain) advance(errVal sobek.Value) sobek.Value {
	c.nextCalls++
	_ = c.step(errVal)
	return sobek.Undefined()
}

func (c *Chain) step(errVal sobek.Value) error {
	if c.done {
		return nil
	}
	if !isNilValue(errVal) {
		c.pending = errVal
		c.errMsg = errVal.String()
	}

	for {
		if c.index >= len(c.layers) {
			c.done = true
			c.report()
			return nil
		}

		layer := c.layers[c.index]
		c.index++

		isErrorHandler := layer.Arity >= 4
		if c.pending != nil {
			if !isErrorHandler {
				continue
			}
		} else if isErrorHandler {
			continue
		}

		var args []sobek.Value
		if c.pending != nil {
			args = []sobek.Value{c.pending, c.req, c.res, c.next}
			c.pending = nil
		} else {
			args = []sobek.Value{c.req, c.res, c.next}
		}

		mark := c.nextCalls
		_, err := layer.Fn(sobek.Undefined(), args...)
		if err != nil {
			var interrupted *sobek.InterruptedError
			if errors.As(err, &interrupted) {
				c.done = true
				return err
			}
			c.pending = c.errorValue(err)
			c.errMsg = err.Error()
			continue
		}

		if c.nextCalls > mark {
			return nil
		}
		c.report()
		return nil
	}
}

// errorValue returns the value handed to an error handler: the original
// JavaScript error when the runtime produced one, so err.message and err.stack
// survive, otherwise the Go error message.
func (c *Chain) errorValue(err error) sobek.Value {
	var exc *sobek.Exception
	if errors.As(err, &exc) {
		if v := exc.Value(); v != nil {
			return v
		}
	}
	return c.p.rt.ToValue(err.Error())
}

func (c *Chain) report() {
	if c.ex == nil || c.errMsg == "" {
		return
	}
	if c.ex.Writer().Written() {
		return
	}
	c.ex.SetError(errors.New(c.errMsg))
}

func isNilValue(v sobek.Value) bool {
	return v == nil || sobek.IsUndefined(v) || sobek.IsNull(v)
}
