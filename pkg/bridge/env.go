package bridge

import (
	"fmt"
	"os"
	"strings"

	"github.com/grafana/sobek"

	"github.com/frinknet/caddy-javascript/pkg/files"
)

func (p *Program) registerEnvironment() {
	p.registerApp()
	p.registerProcess()
	p.registerRequire()
	p.registerFS()
	p.registerPath()
	p.registerTimers()
	p.registerFetch()
}

func (p *Program) registerApp() {
	app := p.rt.NewObject()

	app.Set("get", p.routeSetter("GET"))
	app.Set("post", p.routeSetter("POST"))

	app.Set("use", func(call sobek.FunctionCall) sobek.Value {
		fn, ok := sobek.AssertFunction(call.Argument(0))
		if !ok {
			panic(p.rt.NewTypeError("app.use requires a function"))
		}
		p.routes.SetFallback(fn)
		return sobek.Undefined()
	})

	p.rt.Set("app", app)
}

func (p *Program) routeSetter(method string) func(sobek.FunctionCall) sobek.Value {
	return func(call sobek.FunctionCall) sobek.Value {
		path := call.Argument(0).String()
		fn, ok := sobek.AssertFunction(call.Argument(1))
		if !ok {
			panic(p.rt.NewTypeError("app.%s requires a function", strings.ToLower(method)))
		}
		p.routes.Add(method, path, fn)
		return sobek.Undefined()
	}
}

func (p *Program) registerProcess() {
	process := p.rt.NewObject()
	env := p.rt.NewObject()

	allowAll := false
	allowed := make(map[string]bool, len(p.cfg.EnvAllow))
	for _, name := range p.cfg.EnvAllow {
		if name == "*" {
			allowAll = true
		}
		allowed[name] = true
	}

	for _, entry := range os.Environ() {
		name, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if allowAll || allowed[name] {
			_ = env.Set(name, value)
		}
	}

	process.Set("env", env)
	p.rt.Set("process", process)
}

func (p *Program) registerRequire() {
	p.rt.Set("require", func(call sobek.FunctionCall) sobek.Value {
		switch call.Argument(0).String() {
		case "fs", "node:fs":
			return p.fsObj
		case "path", "node:path":
			return p.pathObj
		default:
			panic(p.rt.NewGoError(fmt.Errorf("module not supported in caddy-javascript: %s", call.Argument(0).String())))
		}
	})
}

func (p *Program) registerFS() {
	fsObj := p.rt.NewObject()

	fsObj.Set("readFileSync", func(call sobek.FunctionCall) sobek.Value {
		data, err := p.fs.ReadFile(call.Argument(0).String())
		if err != nil {
			panic(p.rt.NewGoError(err))
		}
		return p.rt.ToValue(string(data))
	})

	fsObj.Set("writeFileSync", func(call sobek.FunctionCall) sobek.Value {
		name := call.Argument(0).String()
		content := call.Argument(1).String()
		if err := p.fs.WriteFile(name, []byte(content), 0o644); err != nil {
			panic(p.rt.NewGoError(err))
		}
		return sobek.Undefined()
	})

	fsObj.Set("readFile", func(call sobek.FunctionCall) sobek.Value {
		name := call.Argument(0).String()
		cb, ok := sobek.AssertFunction(call.Argument(1))
		if !ok {
			panic(p.rt.NewTypeError("fs.readFile requires a callback"))
		}

		ex := p.trackAsync()
		go func() {
			data, err := p.fs.ReadFile(name)
			p.submitAsync(ex, func() {
				if err != nil {
					_, _ = cb(sobek.Undefined(), p.rt.ToValue(err.Error()), sobek.Null())
					return
				}
				_, _ = cb(sobek.Undefined(), sobek.Null(), p.rt.ToValue(string(data)))
			})
		}()
		return sobek.Undefined()
	})

	fsObj.Set("writeFile", func(call sobek.FunctionCall) sobek.Value {
		name := call.Argument(0).String()
		content := call.Argument(1).String()
		cb, ok := sobek.AssertFunction(call.Argument(2))
		if !ok {
			panic(p.rt.NewTypeError("fs.writeFile requires a callback"))
		}

		ex := p.trackAsync()
		go func() {
			err := p.fs.WriteFile(name, []byte(content), 0o644)
			p.submitAsync(ex, func() {
				if err != nil {
					_, _ = cb(sobek.Undefined(), p.rt.ToValue(err.Error()))
					return
				}
				_, _ = cb(sobek.Undefined(), sobek.Null())
			})
		}()
		return sobek.Undefined()
	})

	p.fsObj = fsObj
	p.rt.Set("fs", fsObj)
}

func (p *Program) registerPath() {
	pathObj := p.rt.NewObject()
	pathObj.Set("join", func(call sobek.FunctionCall) sobek.Value {
		parts := make([]string, len(call.Arguments))
		for i, arg := range call.Arguments {
			parts[i] = arg.String()
		}
		return p.rt.ToValue(files.Join(parts...))
	})
	p.pathObj = pathObj
	p.rt.Set("path", pathObj)
}
