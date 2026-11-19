package bridge

import (
	"fmt"
	"path"
	"strings"

	"github.com/grafana/sobek"
)

// moduleRecord is one entry in the module cache. A JavaScript module keeps its
// live module object so that a circular require observes the exports built so
// far; a JSON module only has a value.
type moduleRecord struct {
	module *sobek.Object
	value  sobek.Value
}

func (r *moduleRecord) exports() sobek.Value {
	if r.module != nil {
		return r.module.Get("exports")
	}
	return r.value
}

// builtin returns the shim for a core module name, if any.
func (p *Program) builtin(name string) (sobek.Value, bool) {
	switch name {
	case "fs", "node:fs":
		return p.fsObj, true
	case "path", "node:path":
		return p.pathObj, true
	case "express", "node:express":
		return p.expressObj, true
	case "body-parser", "node:body-parser":
		return p.expressObj, true
	case "buffer", "node:buffer":
		return p.bufferObj, true
	case "url", "node:url":
		return p.urlObj, true
	case "querystring", "node:querystring":
		return p.querystringObj, true
	case "util", "node:util":
		return p.utilObj, true
	case "events", "node:events":
		return p.eventsVal, true
	case "jobs", "node:jobs":
		return p.jobsObj, true
	default:
		return nil, false
	}
}

// makeRequire builds the `require` function a module (or the entry script) sees.
// dir is the sandbox-relative directory that relative specifiers resolve from.
func (p *Program) makeRequire(dir string) sobek.Value {
	return p.rt.ToValue(func(call sobek.FunctionCall) sobek.Value {
		v, err := p.requireFrom(dir, call.Argument(0).String())
		if err != nil {
			panic(p.rt.NewGoError(err))
		}
		return v
	})
}

func (p *Program) requireFrom(dir, request string) (sobek.Value, error) {
	if v, ok := p.builtin(request); ok {
		return v, nil
	}
	resolved, err := p.resolve(request, dir)
	if err != nil {
		return nil, err
	}
	return p.loadModule(resolved)
}

// resolve turns a require specifier into a sandbox-relative file path. Relative
// and absolute specifiers resolve against dir; bare specifiers walk up the
// directory tree looking for node_modules.
func (p *Program) resolve(request, dir string) (string, error) {
	if request == "" {
		return "", fmt.Errorf("cannot find module ''")
	}

	if isLocalSpecifier(request) {
		base := request
		if !strings.HasPrefix(request, "/") {
			base = path.Join(dir, request)
		}
		if resolved := p.resolveFile(cleanRel(base)); resolved != "" {
			return resolved, nil
		}
		return "", fmt.Errorf("cannot find module '%s'", request)
	}

	for d := cleanRel(dir); ; {
		if resolved := p.resolveFile(path.Join(d, "node_modules", request)); resolved != "" {
			return resolved, nil
		}
		parent := path.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	return "", fmt.Errorf("cannot find module '%s'", request)
}

func isLocalSpecifier(request string) bool {
	return strings.HasPrefix(request, "./") ||
		strings.HasPrefix(request, "../") ||
		strings.HasPrefix(request, "/") ||
		request == "." ||
		request == ".."
}

// resolveFile applies the CommonJS file and directory rules: an exact file, a
// `.js`/`.json` sibling, a directory's package.json main, then its index.
func (p *Program) resolveFile(base string) string {
	if p.isFile(base) {
		return base
	}
	for _, ext := range []string{".js", ".json"} {
		if candidate := base + ext; p.isFile(candidate) {
			return candidate
		}
	}
	if p.isDir(base) {
		if main := p.packageMain(base); main != "" {
			if resolved := p.resolveFile(path.Join(base, main)); resolved != "" {
				return resolved
			}
		}
		for _, index := range []string{"index.js", "index.json"} {
			if candidate := path.Join(base, index); p.isFile(candidate) {
				return candidate
			}
		}
	}
	return ""
}

func (p *Program) isFile(name string) bool {
	info, err := p.fs.Stat(name)
	return err == nil && !info.IsDir()
}

func (p *Program) isDir(name string) bool {
	info, err := p.fs.Stat(name)
	return err == nil && info.IsDir()
}

func (p *Program) packageMain(dir string) string {
	data, err := p.fs.ReadFile(path.Join(dir, "package.json"))
	if err != nil {
		return ""
	}
	v, err := p.parseJSON(data)
	if err != nil {
		return ""
	}
	obj := v.ToObject(p.rt)
	if obj == nil {
		return ""
	}
	main := obj.Get("main")
	if main == nil || sobek.IsUndefined(main) || sobek.IsNull(main) {
		return ""
	}
	return main.String()
}

// loadModule reads, wraps, and evaluates a module, caching its exports.
func (p *Program) loadModule(name string) (sobek.Value, error) {
	name = cleanRel(name)
	if rec, ok := p.modules[name]; ok {
		return rec.exports(), nil
	}

	src, err := p.fs.ReadFile(name)
	if err != nil {
		return nil, err
	}

	if strings.HasSuffix(name, ".json") {
		v, err := p.parseJSON(src)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		p.modules[name] = &moduleRecord{value: v}
		return v, nil
	}

	exports := p.rt.NewObject()
	moduleObj := p.rt.NewObject()
	_ = moduleObj.Set("exports", exports)
	rec := &moduleRecord{module: moduleObj, value: exports}
	p.modules[name] = rec

	dir := path.Dir(name)
	fn, err := p.compileModule(name, string(src))
	if err != nil {
		delete(p.modules, name)
		return nil, err
	}

	_, err = fn(sobek.Undefined(),
		exports,
		p.makeRequire(dir),
		moduleObj,
		p.rt.ToValue(name),
		p.rt.ToValue(dir),
	)
	if err != nil {
		delete(p.modules, name)
		return nil, err
	}

	return moduleObj.Get("exports"), nil
}

func (p *Program) compileModule(name, src string) (sobek.Callable, error) {
	wrapped := "(function (exports, require, module, __filename, __dirname) {\n" + src + "\n})"
	prog, err := sobek.Compile(name, wrapped, false)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	v, err := p.rt.RunProgram(prog)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	fn, ok := sobek.AssertFunction(v)
	if !ok {
		return nil, fmt.Errorf("%s: module did not compile to a function", name)
	}
	return fn, nil
}

func (p *Program) parseJSON(src []byte) (sobek.Value, error) {
	jsonObj := p.rt.Get("JSON").ToObject(p.rt)
	parse, ok := sobek.AssertFunction(jsonObj.Get("parse"))
	if !ok {
		return nil, fmt.Errorf("JSON.parse is unavailable")
	}
	return parse(sobek.Undefined(), p.rt.ToValue(string(src)))
}

// cleanRel normalizes a sandbox-relative path, clamping it to the root the way
// the file system sandbox does.
func cleanRel(name string) string {
	return path.Clean("/" + name)[1:]
}
