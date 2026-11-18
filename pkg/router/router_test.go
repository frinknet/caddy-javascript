package router

import (
	"testing"

	"github.com/grafana/sobek"
)

func fn(rt *sobek.Runtime, body string) sobek.Callable {
	v, err := rt.RunString("(function(){" + body + "})")
	if err != nil {
		panic(err)
	}
	callable, ok := sobek.AssertFunction(v)
	if !ok {
		panic("not a function")
	}
	return callable
}

func TestLookupAndFallback(t *testing.T) {
	rt := sobek.New()
	table := New()

	get := fn(rt, "return 'get'")
	fallback := fn(rt, "return 'fallback'")

	table.Add("GET", "/a", get)
	table.SetFallback(fallback)

	if got := table.Lookup("GET", "/a"); got == nil {
		t.Fatal("expected GET /a to match")
	}
	if got := table.Lookup("POST", "/a"); got != nil {
		t.Fatal("method mismatch should not match")
	}
	if got := table.Lookup("GET", "/b"); got != nil {
		t.Fatal("path mismatch should not match")
	}
	if got := table.Fallback(); got == nil {
		t.Fatal("expected fallback to be set")
	}
}
