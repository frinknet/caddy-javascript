package template

import (
	"fmt"
	"net/http"
	"os"
	"reflect"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// Host is the request and sandbox view a template function sees. The VM
// implements it. Request is nil when no request is being dispatched.
type Host interface {
	Request() *http.Request
	Placeholder(name string) (any, bool)
	Root() *os.Root
	RootDir() string
	FetchAllowed(host string) bool
	EnvAllowed(name string) bool
}

// Registry holds the template functions exposed as `_`.
type Registry struct {
	funcs        map[string]reflect.Value
	requestBound map[string]bool
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		funcs:        make(map[string]reflect.Value),
		requestBound: make(map[string]bool),
	}
}

// Add registers a function. requestBound marks a function that needs an active
// request.
func (r *Registry) Add(name string, fn any, requestBound bool) error {
	value := reflect.ValueOf(fn)
	if value.Kind() != reflect.Func {
		return fmt.Errorf("javascript: template function %q is not a function", name)
	}
	if _, ok := r.funcs[name]; ok {
		return fmt.Errorf("javascript: template function %q is already registered", name)
	}
	r.funcs[name] = value
	if requestBound {
		r.requestBound[name] = true
	}
	return nil
}

// Set registers a function, replacing one with the same name. Site template
// extensions are applied last so they can shadow a built-in.
func (r *Registry) Set(name string, fn any, requestBound bool) error {
	value := reflect.ValueOf(fn)
	if value.Kind() != reflect.Func {
		return fmt.Errorf("javascript: template function %q is not a function", name)
	}
	r.funcs[name] = value
	if requestBound {
		r.requestBound[name] = true
	} else {
		delete(r.requestBound, name)
	}
	return nil
}

// Names returns the registered function names.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.funcs))
	for name := range r.funcs {
		names = append(names, name)
	}
	return names
}

// Call invokes a registered function with JavaScript arguments.
func (r *Registry) Call(rt *quickjs.Runtime, host Host, name string, args []quickjs.Value) (quickjs.Value, error) {
	fn, ok := r.funcs[name]
	if !ok {
		return quickjs.Value{}, fmt.Errorf("javascript: unknown template function %q", name)
	}
	if r.requestBound[name] && host.Request() == nil {
		return quickjs.Value{}, fmt.Errorf("javascript: template function %q requires a request", name)
	}
	return call(rt, host, fn, args)
}
