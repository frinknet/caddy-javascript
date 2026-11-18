package bridge

import (
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/grafana/sobek"

	"github.com/frinknet/caddy-javascript/pkg/files"
)

func newTestProgram(t *testing.T, timeout time.Duration) *Program {
	t.Helper()
	fsys, err := files.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fsys.Close() })
	return NewProgram(Config{Timeout: timeout, Quit: make(chan struct{})}, fsys, nil, nil)
}

type fakeScheduler struct {
	mu    sync.Mutex
	tasks []func()
}

func (f *fakeScheduler) Submit(fn func()) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tasks = append(f.tasks, fn)
	return true
}

func (f *fakeScheduler) pending() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.tasks)
}

func (f *fakeScheduler) run() {
	for {
		f.mu.Lock()
		if len(f.tasks) == 0 {
			f.mu.Unlock()
			return
		}
		task := f.tasks[0]
		f.tasks = f.tasks[1:]
		f.mu.Unlock()
		task()
	}
}

func TestRunInterruptsInfiniteLoop(t *testing.T) {
	p := newTestProgram(t, 50*time.Millisecond)
	if err := p.Load([]byte(`function spin(){ while(true){} }`)); err != nil {
		t.Fatal(err)
	}

	err := p.Run(50*time.Millisecond, func() error {
		_, e := p.rt.RunString("spin()")
		return e
	})
	if err == nil {
		t.Fatal("expected an interrupt error")
	}
}

func TestRuntimeUsableAfterInterrupt(t *testing.T) {
	p := newTestProgram(t, 50*time.Millisecond)
	if err := p.Load([]byte(`function spin(){ while(true){} } function ok(){ return 41 + 1 }`)); err != nil {
		t.Fatal(err)
	}

	if err := p.Run(50*time.Millisecond, func() error {
		_, e := p.rt.RunString("spin()")
		return e
	}); err == nil {
		t.Fatal("expected an interrupt error")
	}

	var out int64
	err := p.Run(time.Second, func() error {
		v, e := p.rt.RunString("ok()")
		if e == nil {
			out = v.ToInteger()
		}
		return e
	})
	if err != nil {
		t.Fatalf("runtime poisoned after interrupt: %v", err)
	}
	if out != 42 {
		t.Fatalf("ok() = %d, want 42", out)
	}
}

func TestExchangeStaysOpenForAsync(t *testing.T) {
	fsys, err := files.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fsys.Close() })

	sched := &fakeScheduler{}
	p := NewProgram(Config{Quit: make(chan struct{})}, fsys, sched, nil)
	if err := p.Load([]byte(`
		app.get('/x', function(req, res) {
			setTimeout(function() { res.send('done') }, 0)
		})
	`)); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	w := NewResponseWriter(rec)
	ex := NewExchange(w)
	reqObj := p.BuildRequest(&RequestContext{Method: "GET", Path: "/x"})
	resObj := p.BuildResponse(w)

	handler := p.Lookup("GET", "/x")
	if handler == nil {
		t.Fatal("route not registered")
	}
	if err := p.Serve(ex, time.Second, func() error {
		_, e := handler(sobek.Undefined(), reqObj, resObj)
		return e
	}); err != nil {
		t.Fatal(err)
	}

	select {
	case <-ex.Done:
		t.Fatal("exchange finished before the async callback ran")
	default:
	}

	deadline := time.Now().Add(time.Second)
	for sched.pending() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	sched.run()

	select {
	case <-ex.Done:
	default:
		t.Fatal("exchange did not finish after the callback")
	}
	if got := rec.Body.String(); got != "done" {
		t.Fatalf("body = %q, want %q", got, "done")
	}
}

func TestCheckFetchTarget(t *testing.T) {
	fsys, err := files.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fsys.Close() })

	allowed := NewProgram(Config{FetchAllow: []string{"example.com"}, Quit: make(chan struct{})}, fsys, nil, nil)
	if err := allowed.checkFetchTarget("example.com"); err != nil {
		t.Fatalf("allowlisted host rejected: %v", err)
	}
	if err := allowed.checkFetchTarget("evil.example"); err == nil {
		t.Fatal("non-allowlisted host should be rejected")
	}

	denied := NewProgram(Config{Quit: make(chan struct{})}, fsys, nil, nil)
	if err := denied.checkFetchTarget("example.com"); err == nil {
		t.Fatal("fetch should be denied by default")
	}
}

func TestEnvAllowlist(t *testing.T) {
	t.Setenv("E2E_SECRET", "s3cr3t")
	t.Setenv("E2E_OTHER", "nope")

	fsys, err := files.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fsys.Close() })

	p := NewProgram(Config{EnvAllow: []string{"E2E_SECRET"}, Quit: make(chan struct{})}, fsys, nil, nil)

	v, err := p.rt.RunString("process.env.E2E_SECRET")
	if err != nil {
		t.Fatal(err)
	}
	if v.String() != "s3cr3t" {
		t.Fatalf("E2E_SECRET = %q", v.String())
	}

	v, err = p.rt.RunString("typeof process.env.E2E_OTHER")
	if err != nil {
		t.Fatal(err)
	}
	if v.String() != "undefined" {
		t.Fatalf("E2E_OTHER leaked as %q", v.String())
	}
}

func TestRequestContextBodyParsing(t *testing.T) {
	p := newTestProgram(t, time.Second)
	rc := &RequestContext{
		Method:  "POST",
		Path:    "/echo",
		Host:    "example.com",
		Headers: map[string][]string{"Content-Type": {"application/json"}},
		Body:    map[string]interface{}{"a": float64(1)},
		Query:   map[string][]string{"x": {"1", "2"}},
	}

	obj := p.BuildRequest(rc)
	if got := obj.Get("method").String(); got != "POST" {
		t.Fatalf("method = %q", got)
	}
	if got := obj.Get("host").String(); got != "example.com" {
		t.Fatalf("host = %q", got)
	}
	body := obj.Get("body").ToObject(p.rt)
	if got := body.Get("a").ToInteger(); got != 1 {
		t.Fatalf("body.a = %d", got)
	}
	query := obj.Get("query").ToObject(p.rt)
	if got := query.Get("x").String(); got != "1" {
		t.Fatalf("query.x = %q", got)
	}
	all := obj.Get("queryAll").ToObject(p.rt)
	if got := all.Get("x").ToObject(p.rt).Get("1").String(); got != "2" {
		t.Fatalf("queryAll.x[1] = %q", got)
	}
}

func TestParseBodyURLEncoded(t *testing.T) {
	got := parseBody([]byte("a=1&b=2"), "application/x-www-form-urlencoded")
	m, ok := got.(map[string]interface{})
	if !ok {
		t.Fatalf("got %T, want map", got)
	}
	if m["a"] != "1" || m["b"] != "2" {
		t.Fatalf("got %v", m)
	}
}

func TestParseBodyJSONSuffix(t *testing.T) {
	got := parseBody([]byte(`{"a":1}`), "application/vnd.api+json")
	m, ok := got.(map[string]interface{})
	if !ok {
		t.Fatalf("got %T, want map", got)
	}
	if m["a"] != float64(1) {
		t.Fatalf("got %v", m)
	}
}

func TestEncodeBody(t *testing.T) {
	p := newTestProgram(t, time.Second)

	body, contentType := encodeBody(p.rt.ToValue("hi"))
	if string(body) != "hi" || contentType != "text/plain; charset=utf-8" {
		t.Fatalf("string: %q %q", body, contentType)
	}

	body, contentType = encodeBody(p.rt.ToValue(map[string]interface{}{"a": 1}))
	if string(body) != `{"a":1}` || contentType != "application/json; charset=utf-8" {
		t.Fatalf("object: %q %q", body, contentType)
	}
}
