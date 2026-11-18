package router

import "github.com/grafana/sobek"

type Table struct {
	routes   map[string]sobek.Callable
	fallback sobek.Callable
}

func New() *Table {
	return &Table{routes: make(map[string]sobek.Callable)}
}

func key(method, path string) string {
	return method + " " + path
}

func (t *Table) Add(method, path string, fn sobek.Callable) {
	t.routes[key(method, path)] = fn
}

func (t *Table) SetFallback(fn sobek.Callable) {
	t.fallback = fn
}

func (t *Table) Lookup(method, path string) sobek.Callable {
	return t.routes[key(method, path)]
}

func (t *Table) Fallback() sobek.Callable {
	return t.fallback
}
