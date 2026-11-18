package router

import (
	"strings"

	"github.com/grafana/sobek"
)

// Layer is one registered route or middleware. An empty Method matches every
// method, which is what app.all and app.use register. Middleware (Use) treats
// Path as a prefix; routes match it exactly.
type Layer struct {
	Method string
	Path   string
	Use    bool
	Fn     sobek.Callable
	Arity  int

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
	layers []*Layer
}

func New() *Table {
	return &Table{}
}

// Add registers a route for one method and path. Method may be empty to match
// every method.
func (t *Table) Add(method, path string, fn sobek.Callable, arity int) {
	t.layers = append(t.layers, newLayer(method, path, false, fn, arity))
}

// Use registers middleware. An empty path matches everything, otherwise the
// path is a prefix.
func (t *Table) Use(path string, fn sobek.Callable, arity int) {
	t.layers = append(t.layers, newLayer("", path, true, fn, arity))
}

// Match returns every layer that matches the method and path, in registration
// order, along with the parameters captured from the path. Parameters from a
// later matching layer overwrite earlier ones.
func (t *Table) Match(method, path string) ([]*Layer, map[string]string) {
	var (
		matched []*Layer
		params  map[string]string
	)
	for _, layer := range t.layers {
		if layer.Method != "" && layer.Method != method {
			continue
		}
		captured, ok := layer.match(path)
		if !ok {
			continue
		}
		matched = append(matched, layer)
		if len(captured) > 0 {
			if params == nil {
				params = make(map[string]string, len(captured))
			}
			for name, value := range captured {
				params[name] = value
			}
		}
	}
	return matched, params
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

func (l *Layer) match(path string) (map[string]string, bool) {
	parts := splitPath(path)
	var params map[string]string

	index := 0
	for _, seg := range l.segments {
		if seg.wildcard {
			name := seg.param
			if name == "" {
				name = "0"
			}
			if params == nil {
				params = make(map[string]string, 1)
			}
			params[name] = strings.Join(parts[index:], "/")
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
			if params == nil {
				params = make(map[string]string, 1)
			}
			params[seg.param] = parts[index]
		}
		index++
	}

	if !l.Use && index != len(parts) {
		return nil, false
	}
	return params, true
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
