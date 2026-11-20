package vm

import (
	"net/http"
	"os"
	"strings"

	quickjs "github.com/go-quickjs/go-quickjs"

	"github.com/frinknet/caddy-javascript/pkg/template"
)

// Request returns the request being dispatched, or nil when no request is
// active. It is called on the VM goroutine during a template function call.
func (v *VM) Request() *http.Request {
	if v.active == nil {
		return nil
	}
	return v.active.req
}

// Placeholder resolves a Caddy placeholder for the active request.
func (v *VM) Placeholder(name string) (any, bool) {
	if v.active == nil {
		return nil, false
	}
	return v.active.placeholder(name)
}

// Root returns the sandbox root.
func (v *VM) Root() *os.Root {
	return v.root
}

// RootDir returns the sandbox directory.
func (v *VM) RootDir() string {
	return v.sandbox.dir
}

// FetchAllowed reports whether a host is allowed by fetch_allow.
func (v *VM) FetchAllowed(host string) bool {
	for _, pattern := range v.fetchAllow {
		if pattern == "*" || pattern == host {
			return true
		}
		if strings.HasPrefix(pattern, "*.") && strings.HasSuffix(host, pattern[1:]) {
			return true
		}
	}
	return false
}

// EnvAllowed reports whether an environment name is allowed by env_allow.
func (v *VM) EnvAllowed(name string) bool {
	for _, allowed := range v.envAllow {
		if allowed == "*" || allowed == name {
			return true
		}
	}
	return false
}

// installTemplate builds the `_` registry and installs it as a global. It runs
// on the VM goroutine before the entry module is evaluated.
func (v *VM) installTemplate() error {
	reg := template.NewRegistry()
	if err := reg.AddSprig(); err != nil {
		return err
	}
	if err := reg.AddCaddyBuiltins(v); err != nil {
		return err
	}
	for name, fn := range v.templateFuncs {
		if err := reg.Set(name, fn, false); err != nil {
			return err
		}
	}
	v.registry = reg

	obj := v.rt.NewObject()
	for _, name := range reg.Names() {
		name := name
		if err := obj.Set(name, func(args ...quickjs.Value) (quickjs.Value, error) {
			return reg.Call(v.rt, v, name, args)
		}); err != nil {
			return err
		}
	}
	return v.rt.Set("_", obj)
}
