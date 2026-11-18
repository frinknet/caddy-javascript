package bridge

import (
	"strings"

	"github.com/grafana/sobek"

	"github.com/frinknet/caddy-javascript/pkg/router"
)

var routeMethods = []string{"GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS"}

func (p *Program) registerApp() {
	app := p.rt.NewObject()
	p.installRoutes(app, p.routes, "app")
	_ = app.Set("listen", func(call sobek.FunctionCall) sobek.Value {
		return app
	})
	p.appObj = app
	p.rt.Set("app", app)

	express := p.rt.ToValue(func(call sobek.FunctionCall) sobek.Value {
		return app
	})
	expressObj := express.ToObject(p.rt)
	_ = expressObj.Set("Router", func(call sobek.FunctionCall) sobek.Value {
		return p.newRouter()
	})
	_ = expressObj.Set("static", func(call sobek.FunctionCall) sobek.Value {
		return p.newStatic(call)
	})
	p.expressObj = expressObj
}

// installRoutes gives an object the route registration surface. The app and
// every router share it; only the table they write to differs.
func (p *Program) installRoutes(obj *sobek.Object, table *router.Table, label string) {
	for _, method := range routeMethods {
		_ = obj.Set(strings.ToLower(method), p.routeSetter(table, method, label))
	}
	_ = obj.Set("all", p.routeSetter(table, "", label))
	_ = obj.Set("use", p.useSetter(table, label))
}

func (p *Program) routeSetter(table *router.Table, method, label string) func(sobek.FunctionCall) sobek.Value {
	name := label + "." + strings.ToLower(method)
	if method == "" {
		name = label + ".all"
	}
	return func(call sobek.FunctionCall) sobek.Value {
		path := call.Argument(0).String()
		for _, h := range p.handlers(call, 1, name) {
			table.Add(method, path, h.fn, h.arity)
		}
		return sobek.Undefined()
	}
}

func (p *Program) useSetter(table *router.Table, label string) func(sobek.FunctionCall) sobek.Value {
	name := label + ".use"
	return func(call sobek.FunctionCall) sobek.Value {
		path := ""
		start := 0
		if len(call.Arguments) > 1 {
			if _, ok := sobek.AssertFunction(call.Argument(0)); !ok {
				path = call.Argument(0).String()
				start = 1
			}
		}
		if start >= len(call.Arguments) {
			panic(p.rt.NewTypeError("%s requires a function", name))
		}
		for i := start; i < len(call.Arguments); i++ {
			value := call.Argument(i)
			if sub, ok := p.routerTable(value); ok {
				table.Mount(path, sub)
				continue
			}
			if cfg, ok := p.staticConfig(value); ok {
				table.UseStatic(path, cfg)
				continue
			}
			fn, ok := sobek.AssertFunction(value)
			if !ok {
				panic(p.rt.NewTypeError("%s requires a function", name))
			}
			table.Use(path, fn, functionArity(p.rt, value))
		}
		return sobek.Undefined()
	}
}

func (p *Program) newRouter() sobek.Value {
	table := router.New()
	value := p.rt.ToValue(func(call sobek.FunctionCall) sobek.Value {
		return sobek.Undefined()
	})
	obj := value.ToObject(p.rt)
	p.routers[obj] = table
	p.installRoutes(obj, table, "router")
	return value
}

func (p *Program) newStatic(call sobek.FunctionCall) sobek.Value {
	rootValue := call.Argument(0)
	root, ok := rootValue.Export().(string)
	if !ok || strings.TrimSpace(root) == "" {
		panic(p.rt.NewTypeError("express.static requires a root path"))
	}

	cfg := &router.Static{
		Root:        cleanRel(root),
		Index:       "index.html",
		Fallthrough: true,
		Dotfiles:    "ignore",
	}
	if opts := call.Argument(1); !isNilValue(opts) {
		p.applyStaticOptions(cfg, opts)
	}

	value := p.rt.ToValue(func(call sobek.FunctionCall) sobek.Value {
		p.serveStaticJS(cfg, call)
		return sobek.Undefined()
	})
	p.statics[value.ToObject(p.rt)] = cfg
	return value
}

func (p *Program) applyStaticOptions(cfg *router.Static, opts sobek.Value) {
	obj := opts.ToObject(p.rt)
	if obj == nil {
		return
	}
	if v := obj.Get("index"); !isNilValue(v) {
		if enabled, ok := v.Export().(bool); ok && !enabled {
			cfg.Index = ""
		} else if name, ok := v.Export().(string); ok {
			cfg.Index = name
		}
	}
	if v := obj.Get("fallthrough"); !isNilValue(v) {
		if enabled, ok := v.Export().(bool); ok {
			cfg.Fallthrough = enabled
		}
	}
	if v := obj.Get("dotfiles"); !isNilValue(v) {
		cfg.Dotfiles = strings.ToLower(v.String())
	}
}

func (p *Program) routerTable(v sobek.Value) (*router.Table, bool) {
	if isNilValue(v) {
		return nil, false
	}
	obj := v.ToObject(p.rt)
	if obj == nil {
		return nil, false
	}
	table, ok := p.routers[obj]
	return table, ok
}

func (p *Program) staticConfig(v sobek.Value) (*router.Static, bool) {
	if isNilValue(v) {
		return nil, false
	}
	obj := v.ToObject(p.rt)
	if obj == nil {
		return nil, false
	}
	cfg, ok := p.statics[obj]
	return cfg, ok
}

type jsHandler struct {
	fn    sobek.Callable
	arity int
}

func (p *Program) handlers(call sobek.FunctionCall, start int, name string) []jsHandler {
	out := make([]jsHandler, 0, len(call.Arguments)-start)
	for i := start; i < len(call.Arguments); i++ {
		fn, ok := sobek.AssertFunction(call.Argument(i))
		if !ok {
			panic(p.rt.NewTypeError("%s requires a function", name))
		}
		out = append(out, jsHandler{fn: fn, arity: functionArity(p.rt, call.Argument(i))})
	}
	if len(out) == 0 {
		panic(p.rt.NewTypeError("%s requires a function", name))
	}
	return out
}

func functionArity(rt *sobek.Runtime, v sobek.Value) int {
	obj := v.ToObject(rt)
	if obj == nil {
		return 0
	}
	return int(obj.Get("length").ToInteger())
}
