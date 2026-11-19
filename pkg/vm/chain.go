package vm

import (
	"context"
	"errors"
	"fmt"
	"strings"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// layer is one route(matcher, fn) registration made while a module was
// evaluated. It belongs to that module and runs before the module's default
// export.
type layer struct {
	matcher any
	fn      quickjs.Value
}

// route registers a JavaScript layer for the module being evaluated. It is
// installed on the global object and is only legal at module scope.
func (v *VM) route(matcher, fn quickjs.Value) error {
	script := v.currentScript
	if script == "" {
		return errors.New("javascript: route() may only be called at module scope")
	}
	if !fn.IsFunction() {
		return errors.New("javascript: route() requires a function")
	}

	normalized, err := normalizeMatcher(matcher)
	if err != nil {
		return err
	}

	v.mu.Lock()
	v.layers[script] = append(v.layers[script], layer{matcher: normalized, fn: fn})
	v.mu.Unlock()
	return nil
}

func normalizeMatcher(matcher quickjs.Value) (any, error) {
	switch {
	case matcher.IsUndefined(), matcher.IsNull():
		return nil, nil
	case matcher.IsString():
		return matcher.String(), nil
	case matcher.IsFunction():
		return matcher, nil
	default:
		return nil, fmt.Errorf("javascript: route() matcher must be a path pattern or a function")
	}
}

// runChain executes the layers registered for one module in registration
// order. A layer that returns without responding continues to the next layer;
// a response stops the chain so the entry module does not run. It reports
// whether the chain produced a response.
func (v *VM) runChain(ctx context.Context, st *requestState, http quickjs.Value, layers []layer) (bool, error) {
	for _, l := range layers {
		if st.resp.responded() {
			return true, nil
		}

		matches, err := matchLayer(ctx, st, l.matcher, http)
		if err != nil {
			return true, err
		}
		if !matches {
			continue
		}

		if _, err := l.fn.CallContext(ctx, http); err != nil {
			return true, err
		}
	}
	return st.resp.responded(), nil
}

func matchLayer(ctx context.Context, st *requestState, matcher any, http quickjs.Value) (bool, error) {
	switch m := matcher.(type) {
	case nil:
		return true, nil
	case string:
		return matchPath(m, st.currentPath()), nil
	case quickjs.Value:
		result, err := m.CallContext(ctx, http)
		if err != nil {
			return false, err
		}
		return result.Bool(), nil
	default:
		return false, fmt.Errorf("javascript: unsupported route matcher %T", matcher)
	}
}

func matchPath(pattern, path string) bool {
	if pattern == "*" {
		return true
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(path, strings.TrimSuffix(pattern, "*"))
	}
	return path == pattern
}
