package router

import (
	"strings"
	"time"

	"github.com/grafana/sobek"
)

// Static describes a static file middleware mounted at a path. The bridge
// package resolves and serves the files; the table only carries the config.
type Static struct {
	// Root is the sandbox-relative directory files are served from.
	Root string
	// Index is the file served for a directory request. Empty disables it.
	Index string
	// Fallthrough continues the chain when the file is missing.
	Fallthrough bool
	// Dotfiles is "allow", "ignore", or "deny".
	Dotfiles string
	// MaxAge sets Cache-Control max-age. MaxAgeSet distinguishes zero from
	// unset.
	MaxAge    time.Duration
	MaxAgeSet bool
	// ETag enables ETag generation and If-None-Match handling.
	ETag bool
	// Extensions are tried in order when the file is not found.
	Extensions []string
}

// Param is a router.param callback bound to a captured path parameter.
type Param struct {
	Name  string
	Fn    sobek.Callable
	Arity int
}

// ParamBinding pairs a Param with the value captured for one request.
type ParamBinding struct {
	Param *Param
	Value string
}

// Layer is one registered route or middleware. An empty Method matches every
// method, which is what app.all and app.use register. Middleware (Use) treats
// Path as a prefix; routes match it exactly.
//
// A mount layer (Router set) expands its sub-table against the rest of the path
// when the request matches the mount prefix. A static layer (Static set) is a
// prefix middleware served by the bridge.
type Layer struct {
	Method     string
	Path       string
	Use        bool
	Fn         sobek.Callable
	Arity      int
	RouteGroup uint64

	// Base is the actual path prefix consumed by enclosing mounts, root first.
	// It becomes req.baseUrl and, for static layers, locates the directory.
	// It is only set on the per-request copy of a layer.
	Base string

	// RouterPath is the chain of routers that expanded this layer, root first.
	// The bridge uses it to implement next('router'). It is only set on the
	// per-request copy of a layer.
	RouterPath []*Table

	Router *Table
	Static *Static

	// Params are the router.param callbacks to run before Fn, bound to the
	// values captured for this request.
	Params []ParamBinding

	segments []segment
}

type segment struct {
	literal  string
	param    string
	wildcard bool
	optional bool
}

// Table holds the registered layers in registration order, which is the order
// they are tried in.
type Table struct {
	layers    []*Layer
	params    map[string][]*Param
	nextGroup uint64
}

func New() *Table {
	return &Table{params: make(map[string][]*Param)}
}

// Param registers a callback run before any route that captures name.
func (t *Table) Param(name string, fn sobek.Callable, arity int) {
	if t.params == nil {
		t.params = make(map[string][]*Param)
	}
	t.params[name] = append(t.params[name], &Param{Name: name, Fn: fn, Arity: arity})
}

// Add registers a route for one method and path. Method may be empty to match
// every method.
func (t *Table) Add(method, path string, fn sobek.Callable, arity int) {
	t.AddGroup(method, path, fn, arity, 0)
}

func (t *Table) AddGroup(method, path string, fn sobek.Callable, arity int, group uint64) {
	layer := newLayer(method, path, false, fn, arity)
	layer.RouteGroup = group
	t.layers = append(t.layers, layer)
}

func (t *Table) NextGroup() uint64 {
	t.nextGroup++
	return t.nextGroup
}

// Use registers middleware. An empty path matches everything, otherwise the
// path is a prefix.
func (t *Table) Use(path string, fn sobek.Callable, arity int) {
	t.layers = append(t.layers, newLayer("", path, true, fn, arity))
}

// Mount registers a router at a path. Any request under the path is matched
// against the router's own table with the prefix removed.
func (t *Table) Mount(path string, sub *Table) {
	t.layers = append(t.layers, &Layer{
		Path:     path,
		Use:      true,
		Router:   sub,
		segments: parse(path),
	})
}

// UseStatic registers a static file middleware at a path.
func (t *Table) UseStatic(path string, cfg *Static) {
	t.layers = append(t.layers, &Layer{
		Path:     path,
		Use:      true,
		Static:   cfg,
		segments: parse(path),
	})
}

// Match returns every layer that matches the method and path, in registration
// order, along with the parameters captured from the path. Mounted routers are
// expanded in place, so their layers keep their position in the chain.
// Parameters from a later matching layer overwrite earlier ones.
func (t *Table) Match(method, path string) ([]*Layer, map[string]string) {
	return t.match(method, splitPath(path), "", nil)
}

func (t *Table) match(method string, parts []string, base string, routers []*Table) ([]*Layer, map[string]string) {
	var (
		matched []*Layer
		params  map[string]string
	)
	routers = appendRouter(routers, t)
	for _, layer := range t.layers {
		if layer.Method != "" && layer.Method != method {
			continue
		}
		if layer.Router != nil {
			consumed, captured, ok := layer.prefixParts(parts)
			if !ok {
				continue
			}
			subLayers, subParams := layer.Router.match(method, parts[consumed:], joinBase(base, parts[:consumed]), routers)
			if len(subLayers) == 0 {
				continue
			}
			matched = append(matched, subLayers...)
			mergeParams(&params, captured)
			mergeParams(&params, subParams)
			continue
		}
		captured, ok := layer.matchParts(parts)
		if !ok {
			continue
		}
		var bindings []ParamBinding
		if layer.Static == nil {
			bindings = t.bindParams(layer, captured)
		}
		copied := *layer
		copied.Base = base
		copied.RouterPath = routers
		copied.Params = bindings
		matched = append(matched, &copied)
		mergeParams(&params, captured)
	}
	return matched, params
}

func appendRouter(routers []*Table, t *Table) []*Table {
	out := make([]*Table, len(routers)+1)
	copy(out, routers)
	out[len(routers)] = t
	return out
}

// Relative returns the path after this layer's prefix, for a request at path.
// It accounts for the prefix consumed by enclosing router mounts.
func (l *Layer) Relative(path string) (string, bool) {
	parts := splitPath(path)
	base := splitPath(l.Base)
	if len(parts) < len(base) {
		return "", false
	}
	for i := range base {
		if !strings.EqualFold(base[i], parts[i]) {
			return "", false
		}
	}
	consumed, _, ok := l.prefixParts(parts[len(base):])
	if !ok {
		return "", false
	}
	return "/" + strings.Join(parts[len(base)+consumed:], "/"), true
}

// Rest returns the portion of path after prefix, and whether the prefix
// matches. It is used when a static middleware is called directly rather than
// mounted, so there is no enclosing router to account for.
func Rest(prefix, path string) (string, bool) {
	l := newLayer("", prefix, true, nil, 0)
	parts := splitPath(path)
	consumed, _, ok := l.prefixParts(parts)
	if !ok {
		return "", false
	}
	return "/" + strings.Join(parts[consumed:], "/"), true
}

func newLayer(method, path string, use bool, fn sobek.Callable, arity int) *Layer {
	return &Layer{
		Method:   method,
		Path:     path,
		Use:      use,
		Fn:       fn,
		Arity:    arity,
		segments: parse(path),
	}
}

func parse(path string) []segment {
	parts := splitPath(path)
	if len(parts) == 0 {
		return nil
	}
	segments := make([]segment, 0, len(parts))
	for _, part := range parts {
		switch {
		case part == "*":
			segments = append(segments, segment{wildcard: true})
		case strings.HasPrefix(part, ":"):
			name := part[1:]
			if strings.HasSuffix(name, "*") {
				segments = append(segments, segment{wildcard: true, param: strings.TrimSuffix(name, "*")})
				continue
			}
			optional := strings.HasSuffix(name, "?")
			segments = append(segments, segment{param: strings.TrimSuffix(name, "?"), optional: optional})
		default:
			segments = append(segments, segment{literal: part})
		}
	}
	return segments
}

func (l *Layer) matchParts(parts []string) (map[string]string, bool) {
	var params map[string]string
	index := 0

	for _, seg := range l.segments {
		if seg.wildcard {
			rest := ""
			if index < len(parts) {
				rest = strings.Join(parts[index:], "/")
			}
			params = setParam(params, wildcardName(seg), rest)
			return params, true
		}
		if index >= len(parts) {
			if seg.optional {
				continue
			}
			return nil, false
		}
		if seg.literal != "" {
			if !strings.EqualFold(seg.literal, parts[index]) {
				return nil, false
			}
		} else {
			params = setParam(params, seg.param, parts[index])
		}
		index++
	}

	if !l.Use && index != len(parts) {
		return nil, false
	}
	return params, true
}

// prefixParts matches the layer's path as a prefix and returns how many
// segments it consumed along with any captured parameters.
func (l *Layer) prefixParts(parts []string) (int, map[string]string, bool) {
	var params map[string]string
	index := 0

	for _, seg := range l.segments {
		if seg.wildcard {
			return len(parts), setParam(params, wildcardName(seg), strings.Join(parts[index:], "/")), true
		}
		if index >= len(parts) {
			if seg.optional {
				continue
			}
			return 0, nil, false
		}
		if seg.literal != "" {
			if !strings.EqualFold(seg.literal, parts[index]) {
				return 0, nil, false
			}
		} else {
			params = setParam(params, seg.param, parts[index])
		}
		index++
	}
	return index, params, true
}

func wildcardName(seg segment) string {
	if seg.param == "" {
		return "0"
	}
	return seg.param
}

func setParam(params map[string]string, name, value string) map[string]string {
	if params == nil {
		params = make(map[string]string, 1)
	}
	params[name] = value
	return params
}

func (t *Table) bindParams(layer *Layer, captured map[string]string) []ParamBinding {
	if len(t.params) == 0 || len(captured) == 0 {
		return nil
	}
	var (
		out  []ParamBinding
		seen map[*Param]bool
	)
	for _, seg := range layer.segments {
		name := ""
		switch {
		case seg.wildcard:
			name = wildcardName(seg)
		case seg.param != "":
			name = seg.param
		}
		if name == "" {
			continue
		}
		value, ok := captured[name]
		if !ok {
			continue
		}
		for _, param := range t.params[name] {
			if seen[param] {
				continue
			}
			if seen == nil {
				seen = make(map[*Param]bool)
			}
			seen[param] = true
			out = append(out, ParamBinding{Param: param, Value: value})
		}
	}
	return out
}

func mergeParams(dst *map[string]string, src map[string]string) {
	if len(src) == 0 {
		return
	}
	if *dst == nil {
		*dst = make(map[string]string, len(src))
	}
	for name, value := range src {
		(*dst)[name] = value
	}
}

func joinBase(base string, parts []string) string {
	if len(parts) == 0 {
		return base
	}
	joined := strings.Join(parts, "/")
	if base == "" || base == "/" {
		return "/" + joined
	}
	return strings.TrimSuffix(base, "/") + "/" + joined
}

// splitPath splits a URL path into segments, dropping the leading slash and a
// single trailing slash. Root and empty paths have no segments.
func splitPath(path string) []string {
	if path == "" || path == "/" {
		return nil
	}
	trimmed := strings.TrimPrefix(path, "/")
	trimmed = strings.TrimSuffix(trimmed, "/")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "/")
}
