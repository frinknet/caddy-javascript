package bridge

import (
	"fmt"
	"path"
	"strings"

	"github.com/grafana/sobek"
)

// cjsModuleRecord adapts a CommonJS module (or builtin shim) to the ESM
// ModuleRecord interface so ESM files can import it.
type cjsModuleRecord struct {
	rt      *sobek.Runtime
	exports sobek.Value
}

func (m *cjsModuleRecord) GetExportedNames(callback func([]string), _ ...sobek.ModuleRecord) bool {
	names := []string{"default"}
	if obj := m.exports.ToObject(m.rt); obj != nil {
		names = append(names, obj.Keys()...)
	}
	callback(names)
	return true
}

func (m *cjsModuleRecord) ResolveExport(name string, _ ...sobek.ResolveSetElement) (*sobek.ResolvedBinding, bool) {
	if name == "default" {
		return &sobek.ResolvedBinding{Module: m, BindingName: "default"}, false
	}
	if obj := m.exports.ToObject(m.rt); obj != nil {
		if obj.Get(name) != nil {
			return &sobek.ResolvedBinding{Module: m, BindingName: name}, false
		}
	}
	return nil, false
}

func (m *cjsModuleRecord) Link() error { return nil }

func (m *cjsModuleRecord) Evaluate(rt *sobek.Runtime) *sobek.Promise {
	p, resolve, _ := rt.NewPromise()
	_ = resolve(&cjsModuleInstance{rt: rt, exports: m.exports})
	return p
}

type cjsModuleInstance struct {
	rt      *sobek.Runtime
	exports sobek.Value
}

func (i *cjsModuleInstance) GetBindingValue(name string) sobek.Value {
	if name == "default" {
		return i.exports
	}
	obj := i.exports.ToObject(i.rt)
	if obj == nil {
		return sobek.Undefined()
	}
	return obj.Get(name)
}

func (p *Program) newCJSModule(v sobek.Value) sobek.ModuleRecord {
	return &cjsModuleRecord{rt: p.rt, exports: v}
}

// LoadModule evaluates src as an ES module. Imports of CommonJS modules and
// builtins are adapted through cjsModuleRecord.
func (p *Program) LoadModule(name string, src []byte) error {
	rec, err := sobek.ParseModule(name, string(src), p.resolveESM)
	if err != nil {
		return err
	}
	p.esmMu.Lock()
	p.esmPaths[rec] = name
	p.esmCache[name] = rec
	p.esmMu.Unlock()

	if err := rec.Link(); err != nil {
		return err
	}
	promise := p.rt.CyclicModuleRecordEvaluate(rec, p.resolveESM)
	switch promise.State() {
	case sobek.PromiseStateRejected:
		return fmt.Errorf("module evaluation failed: %v", promise.Result())
	case sobek.PromiseStatePending:
		return fmt.Errorf("module evaluation is pending (top-level await is not supported)")
	}
	return nil
}

// resolveESM is the host resolver for ESM imports. It resolves relative and
// bare specifiers against the sandbox and wraps CommonJS modules as module
// records.
func (p *Program) resolveESM(referencing interface{}, specifier string) (sobek.ModuleRecord, error) {
	parent := "."
	if rec, ok := referencing.(sobek.ModuleRecord); ok {
		p.esmMu.Lock()
		if name, ok := p.esmPaths[rec]; ok {
			parent = name
		}
		p.esmMu.Unlock()
	}

	if v, ok := p.builtin(specifier); ok {
		key := "\x00builtin:" + specifier
		p.esmMu.Lock()
		if rec, ok := p.esmCache[key]; ok {
			p.esmMu.Unlock()
			return rec, nil
		}
		p.esmMu.Unlock()
		rec := p.newCJSModule(v)
		p.esmMu.Lock()
		p.esmCache[key] = rec
		p.esmMu.Unlock()
		return rec, nil
	}

	name, err := p.resolve(specifier, path.Dir(parent))
	if err != nil {
		return nil, fmt.Errorf("cannot resolve module %q: %w", specifier, err)
	}

	p.esmMu.Lock()
	if rec, ok := p.esmCache[name]; ok {
		p.esmMu.Unlock()
		return rec, nil
	}
	p.esmMu.Unlock()

	src, err := p.fs.ReadFile(name)
	if err != nil {
		return nil, err
	}

	var rec sobek.ModuleRecord
	if strings.HasSuffix(name, ".json") || looksLikeCJS(src) {
		v, err := p.loadModule(name)
		if err != nil {
			return nil, err
		}
		rec = p.newCJSModule(v)
	} else {
		parsed, err := sobek.ParseModule(name, string(src), p.resolveESM)
		if err != nil {
			return nil, err
		}
		rec = parsed
	}

	p.esmMu.Lock()
	p.esmPaths[rec] = name
	p.esmCache[name] = rec
	p.esmMu.Unlock()
	return rec, nil
}

// looksLikeESM reports whether src appears to use ES module syntax. The check
// is intentionally shallow: a top-level import or export statement.
func looksLikeESM(src []byte) bool {
	for _, line := range strings.Split(string(src), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "import ") || strings.HasPrefix(line, "import{") ||
			strings.HasPrefix(line, "import(") || strings.HasPrefix(line, "export ") ||
			strings.HasPrefix(line, "export{") || strings.HasPrefix(line, "export default") {
			return true
		}
	}
	return false
}

// looksLikeCJS reports whether src appears to use CommonJS module syntax.
func looksLikeCJS(src []byte) bool {
	s := string(src)
	return strings.Contains(s, "module.exports") || strings.Contains(s, "exports.") || strings.Contains(s, "require(")
}
