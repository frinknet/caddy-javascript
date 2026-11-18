package router

import (
	"reflect"
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

func TestExactMatch(t *testing.T) {
	rt := sobek.New()
	table := New()
	table.Add("GET", "/a", fn(rt, "return 'get'"), 2)

	if got, _ := table.Match("GET", "/a"); len(got) != 1 {
		t.Fatalf("GET /a matched %d layers, want 1", len(got))
	}
	if got, _ := table.Match("POST", "/a"); len(got) != 0 {
		t.Fatalf("POST /a matched %d layers, want 0", len(got))
	}
	if got, _ := table.Match("GET", "/b"); len(got) != 0 {
		t.Fatalf("GET /b matched %d layers, want 0", len(got))
	}
}

func TestMatchIsNonStrictAndCaseInsensitive(t *testing.T) {
	rt := sobek.New()
	table := New()
	table.Add("GET", "/Hello/", fn(rt, ""), 2)

	for _, path := range []string{"/Hello", "/hello", "/HELLO/", "/hello"} {
		if got, _ := table.Match("GET", path); len(got) != 1 {
			t.Fatalf("%s matched %d layers, want 1", path, len(got))
		}
	}
}

func TestPathParameters(t *testing.T) {
	rt := sobek.New()
	table := New()
	table.Add("GET", "/users/:id/posts/:post", fn(rt, ""), 2)

	layers, params := table.Match("GET", "/users/7/posts/9")
	if len(layers) != 1 {
		t.Fatalf("matched %d layers, want 1", len(layers))
	}
	want := map[string]string{"id": "7", "post": "9"}
	if !reflect.DeepEqual(params, want) {
		t.Fatalf("params = %v, want %v", params, want)
	}

	if _, params := table.Match("GET", "/users/7/posts"); params != nil {
		t.Fatalf("short path should not match, got %v", params)
	}
	if _, params := table.Match("GET", "/users/7/posts/9/extra"); params != nil {
		t.Fatalf("long path should not match, got %v", params)
	}
}

func TestOptionalParameterAndWildcard(t *testing.T) {
	rt := sobek.New()
	table := New()
	table.Add("GET", "/opt/:name?", fn(rt, ""), 2)
	table.Add("GET", "/files/*", fn(rt, ""), 2)

	if _, params := table.Match("GET", "/opt"); params != nil {
		t.Fatalf("optional segment should match without a value, got %v", params)
	}
	if _, params := table.Match("GET", "/opt/x"); params["name"] != "x" {
		t.Fatalf("optional segment = %v, want x", params)
	}

	_, params := table.Match("GET", "/files/a/b/c.txt")
	if params["0"] != "a/b/c.txt" {
		t.Fatalf("wildcard = %v, want a/b/c.txt", params)
	}
	if _, params := table.Match("GET", "/files"); params["0"] != "" {
		t.Fatalf("wildcard should match the empty rest, got %v", params)
	}
}

func TestUseMatchesPrefixAndEveryMethod(t *testing.T) {
	rt := sobek.New()
	table := New()
	table.Use("/api", fn(rt, ""), 3)
	table.Use("", fn(rt, ""), 2)

	if got, _ := table.Match("POST", "/api/users"); len(got) != 2 {
		t.Fatalf("/api/users matched %d layers, want 2", len(got))
	}
	if got, _ := table.Match("POST", "/api"); len(got) != 2 {
		t.Fatalf("/api matched %d layers, want 2", len(got))
	}
	if got, _ := table.Match("POST", "/apix"); len(got) != 1 {
		t.Fatalf("/apix matched %d layers, want 1", len(got))
	}
	if got, _ := table.Match("POST", "/other"); len(got) != 1 {
		t.Fatalf("/other matched %d layers, want 1", len(got))
	}
}

func TestMatchPreservesRegistrationOrder(t *testing.T) {
	rt := sobek.New()
	table := New()
	first := fn(rt, "return 1")
	second := fn(rt, "return 2")
	table.Use("", first, 2)
	table.Add("GET", "/x", second, 2)
	table.Use("", fn(rt, "return 3"), 2)

	layers, _ := table.Match("GET", "/x")
	if len(layers) != 3 {
		t.Fatalf("matched %d layers, want 3", len(layers))
	}
	if layers[0].Fn == nil || layers[1].Fn == nil {
		t.Fatal("expected callables")
	}
	v, err := layers[0].Fn(sobek.Undefined())
	if err != nil || v.ToInteger() != 1 {
		t.Fatalf("first layer = %v, %v", v, err)
	}
	v, err = layers[1].Fn(sobek.Undefined())
	if err != nil || v.ToInteger() != 2 {
		t.Fatalf("second layer = %v, %v", v, err)
	}
}

func TestNoLayersMatchesNothing(t *testing.T) {
	table := New()
	if got, _ := table.Match("GET", "/"); len(got) != 0 {
		t.Fatalf("empty table matched %d layers", len(got))
	}
}

func TestMountExpandsRelativeRoutes(t *testing.T) {
	rt := sobek.New()
	root := New()
	sub := New()
	sub.Add("GET", "/users/:id", fn(rt, "return 1"), 2)
	root.Mount("/api", sub)

	layers, params := root.Match("GET", "/api/users/7")
	if len(layers) != 1 {
		t.Fatalf("matched %d layers, want 1", len(layers))
	}
	if params["id"] != "7" {
		t.Fatalf("id = %q, want 7", params["id"])
	}
	if got, _ := root.Match("GET", "/other/users/7"); len(got) != 0 {
		t.Fatalf("outside the mount matched %d layers, want 0", len(got))
	}
}

func TestMountSeesRoutesAddedLater(t *testing.T) {
	rt := sobek.New()
	root := New()
	sub := New()
	root.Mount("/api", sub)
	sub.Add("GET", "/late", fn(rt, "return 1"), 2)

	if got, _ := root.Match("GET", "/api/late"); len(got) != 1 {
		t.Fatalf("late route matched %d layers, want 1", len(got))
	}
}

func TestNestedMounts(t *testing.T) {
	rt := sobek.New()
	root := New()
	mid := New()
	inner := New()
	inner.Add("GET", "/x", fn(rt, "return 1"), 2)
	mid.Mount("/v1", inner)
	root.Mount("/api", mid)

	if got, _ := root.Match("GET", "/api/v1/x"); len(got) != 1 {
		t.Fatalf("nested route matched %d layers, want 1", len(got))
	}
	if got, _ := root.Match("GET", "/api/x"); len(got) != 0 {
		t.Fatalf("missing mount segment matched %d layers, want 0", len(got))
	}
}

func TestMountParamsMergeWithInnerParams(t *testing.T) {
	rt := sobek.New()
	root := New()
	sub := New()
	sub.Add("GET", "/posts/:post", fn(rt, "return 1"), 2)
	root.Mount("/user/:id", sub)

	_, params := root.Match("GET", "/user/7/posts/9")
	want := map[string]string{"id": "7", "post": "9"}
	if !reflect.DeepEqual(params, want) {
		t.Fatalf("params = %v, want %v", params, want)
	}
}

func TestStaticLayerRelativeToMounts(t *testing.T) {
	root := New()
	sub := New()
	sub.UseStatic("/static", &Static{Root: "public"})
	root.Mount("/api/:space", sub)

	layers, params := root.Match("GET", "/api/acme/static/app.js")
	if len(layers) != 1 {
		t.Fatalf("matched %d layers, want 1", len(layers))
	}
	if params["space"] != "acme" {
		t.Fatalf("space = %q, want acme", params["space"])
	}
	rest, ok := layers[0].Relative("/api/acme/static/app.js")
	if !ok || rest != "/app.js" {
		t.Fatalf("rest = %q, %v, want /app.js", rest, ok)
	}
}

func TestRestHelper(t *testing.T) {
	if rest, ok := Rest("/static", "/static/a/b.txt"); !ok || rest != "/a/b.txt" {
		t.Fatalf("rest = %q, %v", rest, ok)
	}
	if rest, ok := Rest("", "/a"); !ok || rest != "/a" {
		t.Fatalf("root rest = %q, %v", rest, ok)
	}
	if _, ok := Rest("/other", "/static/a"); ok {
		t.Fatal("mismatched prefix should not match")
	}
}
