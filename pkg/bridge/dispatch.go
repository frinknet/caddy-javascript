package bridge

import (
	"errors"
	"net/http"
	"strings"

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
	current   *router.Layer
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
	if !isNilValue(errVal) && c.current != nil {
		switch errVal.String() {
		case "route":
			c.skipRoute(c.current)
			_ = c.step(sobek.Undefined())
			return sobek.Undefined()
		}
	}
	_ = c.step(errVal)
	return sobek.Undefined()
}

func (c *Chain) skipRoute(layer *router.Layer) {
	if layer == nil || layer.Use {
		return
	}
	for c.index < len(c.layers) {
		next := c.layers[c.index]
		if next.Use || next.RouteGroup != layer.RouteGroup {
			return
		}
		c.index++
	}
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

		if layer.Static != nil {
			c.serveStatic(layer)
			return nil
		}

		var args []sobek.Value
		if c.pending != nil {
			args = []sobek.Value{c.pending, c.req, c.res, c.next}
			c.pending = nil
		} else {
			args = []sobek.Value{c.req, c.res, c.next}
		}

		mark := c.nextCalls
		c.current = layer
		if c.req != nil {
			route := c.p.rt.NewObject()
			_ = route.Set("path", layer.Path)
			_ = route.Set("method", layer.Method)
			_ = c.req.Set("route", route)
		}
		ret, err := layer.Fn(sobek.Undefined(), args...)
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

		if c.awaitPromise(ret) {
			return nil
		}

		if c.nextCalls > mark {
			return nil
		}
		c.report()
		return nil
	}
}

func (c *Chain) awaitPromise(value sobek.Value) bool {
	if isNilValue(value) {
		return false
	}
	obj := value.ToObject(c.p.rt)
	if obj == nil {
		return false
	}
	then, ok := sobek.AssertFunction(obj.Get("then"))
	if !ok {
		return false
	}
	ex, err := c.p.trackAsync()
	if err != nil {
		c.pending = c.errorValue(err)
		c.errMsg = err.Error()
		return false
	}
	onFulfilled := func(call sobek.FunctionCall) sobek.Value {
		if ex != nil {
			c.p.submitAsync(ex, func() {})
		}
		return sobek.Undefined()
	}
	onRejected := func(call sobek.FunctionCall) sobek.Value {
		reason := call.Argument(0)
		if ex != nil {
			c.p.submitAsync(ex, func() {
				if !isNilValue(reason) && c.ex != nil && !c.ex.Writer().Written() {
					c.ex.SetError(errors.New(reason.String()))
				}
			})
		}
		return sobek.Undefined()
	}
	if _, err := then(obj, c.p.rt.ToValue(onFulfilled), c.p.rt.ToValue(onRejected)); err != nil {
		if ex != nil {
			c.p.submitAsync(ex, func() {})
		}
		c.pending = c.errorValue(err)
		c.errMsg = err.Error()
		return false
	}
	return true
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

// report finishes the exchange after the chain ends. An error that carries an
// HTTP status, as body-parser produces for a rejected body, answers with that
// status; anything else is reported so the caller can answer 500.
func (c *Chain) report() {
	if c.ex == nil || c.errMsg == "" {
		return
	}
	if c.ex.Writer().Written() {
		return
	}
	if status := statusOf(c.pending, c.p.rt); status != 0 {
		http.Error(c.ex.Writer(), http.StatusText(status), status)
		c.ex.SetHandled(true)
		return
	}
	c.ex.SetError(errors.New(c.errMsg))
}

func isNilValue(v sobek.Value) bool {
	return v == nil || sobek.IsUndefined(v) || sobek.IsNull(v)
}

// serveStatic serves one static middleware layer. It hands control back to the
// chain when the layer does not handle the request.
func (c *Chain) serveStatic(layer *router.Layer) {
	next := func() { c.advance(sobek.Undefined()) }
	fullPath := c.reqString("path")
	rest, ok := layer.Relative(fullPath)
	if !ok {
		next()
		return
	}
	c.p.serveStaticFile(layer.Static, rest, fullPath, strings.ToUpper(c.reqString("method")), c.p.staticHeaders(c.req), next)
}

func (c *Chain) reqString(name string) string {
	if c.req == nil {
		return ""
	}
	v := c.req.Get(name)
	if isNilValue(v) {
		return ""
	}
	return v.String()
}
