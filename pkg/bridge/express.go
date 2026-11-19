package bridge

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/grafana/sobek"

	"github.com/frinknet/caddy-javascript/pkg/router"
)

var routeMethods = []string{"GET", "POST", "PUT", "DELETE", "PATCH", "HEAD", "OPTIONS"}

func (p *Program) registerApp() {
	app := p.rt.NewObject()
	settings := p.rt.NewObject()
	locals := p.rt.NewObject()
	env := os.Getenv("NODE_ENV")
	if env == "" {
		env = "development"
	}
	_ = settings.Set("env", env)
	p.installRoutes(app, p.routes, "app")
	routeGet := app.Get("get")
	_ = app.Set("get", func(call sobek.FunctionCall) sobek.Value {
		if len(call.Arguments) == 1 {
			value := settings.Get(call.Argument(0).String())
			if value == nil {
				return sobek.Undefined()
			}
			return value
		}
		fn, ok := sobek.AssertFunction(routeGet)
		if !ok {
			return sobek.Undefined()
		}
		value, _ := fn(app, call.Arguments...)
		return value
	})
	set := func(call sobek.FunctionCall) sobek.Value {
		_ = settings.Set(call.Argument(0).String(), call.Argument(1))
		return app
	}
	_ = app.Set("set", set)
	_ = app.Set("enable", func(call sobek.FunctionCall) sobek.Value {
		_ = settings.Set(call.Argument(0).String(), p.rt.ToValue(true))
		return app
	})
	_ = app.Set("disable", func(call sobek.FunctionCall) sobek.Value {
		_ = settings.Set(call.Argument(0).String(), p.rt.ToValue(false))
		return app
	})
	_ = app.Set("enabled", func(call sobek.FunctionCall) sobek.Value {
		value := settings.Get(call.Argument(0).String())
		return p.rt.ToValue(value != nil && value.ToBoolean())
	})
	_ = app.Set("disabled", func(call sobek.FunctionCall) sobek.Value {
		value := settings.Get(call.Argument(0).String())
		return p.rt.ToValue(value == nil || !value.ToBoolean())
	})
	_ = app.Set("locals", locals)
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
	for _, name := range []string{"json", "urlencoded", "text", "raw"} {
		_ = expressObj.Set(name, func(call sobek.FunctionCall) sobek.Value {
			return p.bodyParserMiddleware()
		})
	}
	p.expressObj = expressObj
}

func (p *Program) bodyParserMiddleware() sobek.Value {
	return p.rt.ToValue(func(call sobek.FunctionCall) sobek.Value {
		if next, ok := sobek.AssertFunction(call.Argument(2)); ok {
			_, _ = next(sobek.Undefined())
		}
		return sobek.Undefined()
	})
}

// installRoutes gives an object the route registration surface. The app and
// every router share it; only the table they write to differs.
func (p *Program) installRoutes(obj *sobek.Object, table *router.Table, label string) {
	for _, method := range routeMethods {
		_ = obj.Set(strings.ToLower(method), p.routeSetter(table, method, label))
	}
	_ = obj.Set("all", p.routeSetter(table, "", label))
	_ = obj.Set("use", p.useSetter(table, label))
	_ = obj.Set("route", p.routeChainSetter(table, label))
	_ = obj.Set("param", func(call sobek.FunctionCall) sobek.Value {
		name := call.Argument(0).String()
		fn, ok := sobek.AssertFunction(call.Argument(1))
		if !ok {
			panic(p.rt.NewTypeError("%s.param requires a function", label))
		}
		table.Param(name, fn, functionArity(p.rt, call.Argument(1)))
		return sobek.Undefined()
	})
}

func (p *Program) routeChainSetter(table *router.Table, label string) func(sobek.FunctionCall) sobek.Value {
	return func(call sobek.FunctionCall) sobek.Value {
		path := call.Argument(0).String()
		group := table.NextGroup()
		route := p.rt.NewObject()
		for _, method := range routeMethods {
			method := method
			name := label + ".route(...)." + strings.ToLower(method)
			_ = route.Set(strings.ToLower(method), func(call sobek.FunctionCall) sobek.Value {
				for _, h := range p.handlers(call, 0, name) {
					table.AddGroup(method, path, h.fn, h.arity, group)
				}
				return route
			})
		}
		_ = route.Set("all", func(call sobek.FunctionCall) sobek.Value {
			for _, h := range p.handlers(call, 0, label+".route(...).all") {
				table.AddGroup("", path, h.fn, h.arity, group)
			}
			return route
		})
		return route
	}
}

func (p *Program) routeSetter(table *router.Table, method, label string) func(sobek.FunctionCall) sobek.Value {
	name := label + "." + strings.ToLower(method)
	if method == "" {
		name = label + ".all"
	}
	return func(call sobek.FunctionCall) sobek.Value {
		path := call.Argument(0).String()
		group := table.NextGroup()
		for _, h := range p.handlers(call, 1, name) {
			table.AddGroup(method, path, h.fn, h.arity, group)
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
		var add func(sobek.Value)
		add = func(value sobek.Value) {
			if obj := value.ToObject(p.rt); obj != nil && obj.ClassName() == "Array" {
				length := int(obj.Get("length").ToInteger())
				for i := 0; i < length; i++ {
					add(obj.Get(fmt.Sprint(i)))
				}
				return
			}
			if sub, ok := p.routerTable(value); ok {
				table.Mount(path, sub)
				return
			}
			if cfg, ok := p.staticConfig(value); ok {
				table.UseStatic(path, cfg)
				return
			}
			fn, ok := sobek.AssertFunction(value)
			if !ok {
				panic(p.rt.NewTypeError("%s requires a function", name))
			}
			table.Use(path, fn, functionArity(p.rt, value))
		}
		for i := start; i < len(call.Arguments); i++ {
			add(call.Argument(i))
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
		ETag:        true,
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
	if v := obj.Get("maxAge"); !isNilValue(v) {
		cfg.MaxAgeSet = true
		cfg.MaxAge = time.Duration(v.ToInteger()) * time.Millisecond
	}
	if v := obj.Get("etag"); !isNilValue(v) {
		if enabled, ok := v.Export().(bool); ok {
			cfg.ETag = enabled
		}
	}
	if v := obj.Get("extensions"); !isNilValue(v) {
		cfg.Extensions = nil
		if items, ok := v.Export().([]interface{}); ok {
			for _, item := range items {
				cfg.Extensions = append(cfg.Extensions, strings.TrimPrefix(fmt.Sprint(item), "."))
			}
		} else if s, ok := v.Export().(string); ok {
			for _, part := range strings.Split(s, ",") {
				part = strings.TrimSpace(part)
				if part != "" {
					cfg.Extensions = append(cfg.Extensions, strings.TrimPrefix(part, "."))
				}
			}
		}
	}
}

func (p *Program) expandParams(layers []*router.Layer) []*router.Layer {
	seen := make(map[*router.Param]bool)
	out := make([]*router.Layer, 0, len(layers))
	for _, layer := range layers {
		for _, binding := range layer.Params {
			if seen[binding.Param] {
				continue
			}
			seen[binding.Param] = true
			out = append(out, p.paramLayer(binding))
		}
		out = append(out, layer)
	}
	return out
}

func (p *Program) paramLayer(binding router.ParamBinding) *router.Layer {
	param := binding.Param
	value := binding.Value
	fnValue := p.rt.ToValue(func(call sobek.FunctionCall) sobek.Value {
		_, _ = param.Fn(sobek.Undefined(), call.Argument(0), call.Argument(1), call.Argument(2), p.rt.ToValue(value), p.rt.ToValue(param.Name))
		return sobek.Undefined()
	})
	fn, _ := sobek.AssertFunction(fnValue)
	return &router.Layer{Use: true, Fn: fn, Arity: 3}
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
