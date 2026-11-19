package bridge

import (
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"

	"github.com/grafana/sobek"
)

var renderNextSymbol = sobek.NewSymbol("caddy.renderNext")

// normalizeViewExt accepts "html", ".html", or "HTML" and returns ".html".
func normalizeViewExt(ext string) string {
	ext = strings.ToLower(strings.TrimSpace(ext))
	if ext == "" {
		return ""
	}
	return "." + strings.TrimPrefix(ext, ".")
}

// renderView resolves a view, runs its engine, and either hands the result to
// the JavaScript callback or sends it. The callback is nil for res.render
// without one, in which case errors go to the chain's next function.
func (p *Program) renderView(w *ResponseWriter, res *sobek.Object, name string, options *sobek.Object, cb sobek.Callable) {
	file, ext, err := p.resolveView(name)
	if err != nil {
		p.renderFailed(w, res, p.rt.NewGoError(err), cb)
		return
	}
	engine, err := p.viewEngine(ext)
	if err != nil {
		p.renderFailed(w, res, p.rt.NewGoError(err), cb)
		return
	}

	opts := p.viewOptions(res, options)
	done := false
	engineCallback := func(call sobek.FunctionCall) sobek.Value {
		if done {
			return sobek.Undefined()
		}
		done = true
		errVal := call.Argument(0)
		if !isNilValue(errVal) {
			p.renderFailed(w, res, errVal, cb)
			return sobek.Undefined()
		}
		if cb != nil {
			_, _ = cb(sobek.Undefined(), sobek.Null(), call.Argument(1))
			return sobek.Undefined()
		}
		p.renderSend(w, call.Argument(1))
		return sobek.Undefined()
	}

	if _, err := engine(sobek.Undefined(), p.rt.ToValue(file), opts, p.rt.ToValue(engineCallback)); err != nil && !done {
		done = true
		p.renderFailed(w, res, p.renderErrorValue(err), cb)
	}
}

func (p *Program) renderErrorValue(err error) sobek.Value {
	var exc *sobek.Exception
	if errors.As(err, &exc) {
		if value := exc.Value(); value != nil {
			return value
		}
	}
	return p.rt.NewGoError(err)
}

func (p *Program) renderFailed(w *ResponseWriter, res *sobek.Object, errVal sobek.Value, cb sobek.Callable) {
	if cb != nil {
		_, _ = cb(sobek.Undefined(), errVal)
		return
	}
	if res != nil {
		if next, ok := sobek.AssertFunction(res.GetSymbol(renderNextSymbol)); ok {
			_, _ = next(sobek.Undefined(), errVal)
			return
		}
	}
	if w != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
	}
}

func (p *Program) renderSend(w *ResponseWriter, html sobek.Value) {
	if w == nil {
		return
	}
	w.setContentType("text/html; charset=utf-8")
	w.writeStatusThen([]byte(html.String()))
}

func (p *Program) resolveView(name string) (string, string, error) {
	if name == "" {
		return "", "", fmt.Errorf("res.render requires a view name")
	}
	view := name
	ext := path.Ext(view)
	if ext == "" {
		engineName := p.settingString("view engine")
		if engineName == "" {
			return "", "", fmt.Errorf("No default engine was specified and no extension was provided")
		}
		ext = normalizeViewExt(engineName)
		view += ext
	} else {
		ext = normalizeViewExt(ext)
	}

	if strings.HasPrefix(name, "/") {
		candidate := cleanRel(name)
		if p.isFile(candidate) {
			return candidate, ext, nil
		}
		return "", "", fmt.Errorf("Failed to lookup view %q", name)
	}

	for _, dir := range p.viewDirs() {
		candidate := cleanRel(path.Join(dir, view))
		if p.isFile(candidate) {
			return candidate, ext, nil
		}
	}
	return "", "", fmt.Errorf("Failed to lookup view %q in views directory", name)
}

func (p *Program) viewDirs() []string {
	if p.settings == nil {
		return []string{"views"}
	}
	value := p.settings.Get("views")
	if isNilValue(value) {
		return []string{"views"}
	}
	if dirs, ok := value.Export().([]interface{}); ok {
		out := make([]string, 0, len(dirs))
		for _, dir := range dirs {
			out = append(out, cleanRel(fmt.Sprint(dir)))
		}
		if len(out) > 0 {
			return out
		}
		return []string{"views"}
	}
	return []string{cleanRel(value.String())}
}

func (p *Program) settingString(name string) string {
	if p.settings == nil {
		return ""
	}
	value := p.settings.Get(name)
	if isNilValue(value) {
		return ""
	}
	return value.String()
}

func (p *Program) viewEngine(ext string) (sobek.Callable, error) {
	if p.engines != nil {
		if fn, ok := sobek.AssertFunction(p.engines.Get(ext)); ok {
			return fn, nil
		}
	}
	module := strings.TrimPrefix(ext, ".")
	value, err := p.requireFrom("", module)
	if err != nil {
		return nil, fmt.Errorf("No engine was registered for the extension %q", ext)
	}
	fn, err := engineFunction(value, p.rt)
	if err != nil {
		return nil, err
	}
	return fn, nil
}

func engineFunction(value sobek.Value, rt *sobek.Runtime) (sobek.Callable, error) {
	obj := value.ToObject(rt)
	if obj != nil {
		if fn, ok := sobek.AssertFunction(obj.Get("__express")); ok {
			return fn, nil
		}
		if fn, ok := sobek.AssertFunction(obj.Get("renderFile")); ok {
			return fn, nil
		}
	}
	if fn, ok := sobek.AssertFunction(value); ok {
		return fn, nil
	}
	return nil, fmt.Errorf("View engine module does not export a render function")
}

func (p *Program) viewOptions(res *sobek.Object, options *sobek.Object) *sobek.Object {
	opts := p.rt.NewObject()
	copyKeys(p, opts, p.appObj.Get("locals"))
	if res != nil {
		copyKeys(p, opts, res.Get("locals"))
	}
	copyKeys(p, opts, options)
	_ = opts.Set("settings", p.settings)
	if res != nil {
		_ = opts.Set("_locals", res.Get("locals"))
	}
	if p.settings != nil {
		if cache := p.settings.Get("view cache"); !isNilValue(cache) {
			_ = opts.Set("cache", cache)
		}
	}
	return opts
}

func copyKeys(p *Program, dst *sobek.Object, src sobek.Value) {
	obj := src.ToObject(p.rt)
	if obj == nil {
		return
	}
	for _, key := range obj.Keys() {
		_ = dst.Set(key, obj.Get(key))
	}
}
