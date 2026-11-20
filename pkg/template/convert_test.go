package template

import (
	"net/http"
	"os"
	"testing"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
)

type mockHost struct {
	req        *http.Request
	root       *os.Root
	rootDir    string
	envAllow   map[string]bool
	fetchAllow map[string]bool
}

func (h *mockHost) Request() *http.Request { return h.req }

func (h *mockHost) Placeholder(string) (any, bool) { return nil, false }

func (h *mockHost) Root() *os.Root { return h.root }

func (h *mockHost) RootDir() string { return h.rootDir }

func (h *mockHost) EnvAllowed(name string) bool { return h.envAllow[name] }

func (h *mockHost) FetchAllowed(name string) bool { return h.fetchAllow[name] }

func newRuntime(t *testing.T) *quickjs.Runtime {
	t.Helper()
	rt := quickjs.New()
	t.Cleanup(func() { _ = rt.Close() })
	return rt
}

func eval(t *testing.T, rt *quickjs.Runtime, src string) quickjs.Value {
	t.Helper()
	value, err := rt.Eval(src)
	if err != nil {
		t.Fatalf("eval %q: %v", src, err)
	}
	return value
}

func TestRegistryCallString(t *testing.T) {
	rt := newRuntime(t)
	reg := NewRegistry()
	if err := reg.Add("echo", func(s string) string { return s }, false); err != nil {
		t.Fatalf("Add: %v", err)
	}
	result, err := reg.Call(rt, &mockHost{}, "echo", []quickjs.Value{eval(t, rt, `"hello"`)})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if result.Kind() != quickjs.KindString || result.String() != "hello" {
		t.Fatalf("result = %v (%v), want hello", result.String(), result.Kind())
	}
}

func TestRegistryCallDuration(t *testing.T) {
	rt := newRuntime(t)
	reg := NewRegistry()
	if err := reg.Add("wait", func(d time.Duration) time.Duration { return d }, false); err != nil {
		t.Fatalf("Add: %v", err)
	}
	result, err := reg.Call(rt, &mockHost{}, "wait", []quickjs.Value{eval(t, rt, `"1s"`)})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if result.Kind() != quickjs.KindString || result.String() != "1s" {
		t.Fatalf("result = %v (%v), want 1s", result.String(), result.Kind())
	}
}

func TestRegistryCallTime(t *testing.T) {
	rt := newRuntime(t)
	reg := NewRegistry()
	if err := reg.Add("at", func(t time.Time) time.Time { return t }, false); err != nil {
		t.Fatalf("Add: %v", err)
	}
	result, err := reg.Call(rt, &mockHost{}, "at", []quickjs.Value{eval(t, rt, `"2020-01-02T03:04:05Z"`)})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if result.Kind() != quickjs.KindString || result.String() != "2020-01-02T03:04:05Z" {
		t.Fatalf("result = %v (%v), want 2020-01-02T03:04:05Z", result.String(), result.Kind())
	}
}

func TestRegistryCallRequestBound(t *testing.T) {
	rt := newRuntime(t)
	reg := NewRegistry()
	if err := reg.Add("needsRequest", func() string { return "ok" }, true); err != nil {
		t.Fatalf("Add: %v", err)
	}
	_, err := reg.Call(rt, &mockHost{}, "needsRequest", nil)
	if err == nil {
		t.Fatal("Call succeeded without a request")
	}
}

func TestRegistryCallUnknown(t *testing.T) {
	rt := newRuntime(t)
	reg := NewRegistry()
	_, err := reg.Call(rt, &mockHost{}, "missing", nil)
	if err == nil {
		t.Fatal("Call succeeded for an unknown function")
	}
}

func TestAddSprigDeniesDangerousFunctions(t *testing.T) {
	reg := NewRegistry()
	if err := reg.AddSprig(); err != nil {
		t.Fatalf("AddSprig: %v", err)
	}
	for _, name := range []string{"env", "expandenv", "getHostByName"} {
		if _, err := reg.Call(nil, &mockHost{}, name, nil); err == nil {
			t.Errorf("%s is registered", name)
		}
	}
}
