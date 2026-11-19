package bridge

import (
	"errors"
	"net/http"
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
	reqObj := p.BuildRequest(&RequestContext{Method: "GET", Path: "/x"}, nil)
	resObj := p.BuildResponse(w)

	layers, _ := p.Match("GET", "/x")
	if len(layers) == 0 {
		t.Fatal("route not registered")
	}
	if err := p.Serve(ex, time.Second, func() error {
		return p.RunChain(ex, layers, reqObj, resObj)
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

func TestClearTimeoutCancelsTimer(t *testing.T) {
	fsys, err := files.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fsys.Close() })

	sched := &fakeScheduler{}
	p := NewProgram(Config{Quit: make(chan struct{})}, fsys, sched, nil)
	if err := p.Load([]byte(`
		var fired = 0
		var t = setTimeout(function() { fired++ }, 5)
		clearTimeout(t)
		clearTimeout()
		clearTimeout(undefined)
	`)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	sched.run()

	v, err := p.rt.RunString("fired")
	if err != nil {
		t.Fatal(err)
	}
	if v.ToInteger() != 0 {
		t.Fatalf("cleared timer fired %d times", v.ToInteger())
	}
	if n := sched.pending(); n != 0 {
		t.Fatalf("%d callbacks queued for a cleared timer", n)
	}
}

func TestTimerIDsAreDistinct(t *testing.T) {
	fsys, err := files.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fsys.Close() })

	sched := &fakeScheduler{}
	p := NewProgram(Config{Quit: make(chan struct{})}, fsys, sched, nil)
	if err := p.Load([]byte(`
		var a = setTimeout(function() {}, 1000)
		var b = setTimeout(function() {}, 1000)
		var c = setInterval(function() {}, 1000)
		clearInterval(c)
		clearTimeout(a)
		clearTimeout(b)
	`)); err != nil {
		t.Fatal(err)
	}
	v, err := p.rt.RunString("[a, b].join(',') + ':' + String(a !== b)")
	if err != nil {
		t.Fatal(err)
	}
	if v.String() != "1,2:true" {
		t.Fatalf("timer ids = %q", v.String())
	}
}

func TestClearIntervalStopsRepeating(t *testing.T) {
	sched := &fakeScheduler{}
	rec, err := serveScript(t, `
		app.get('/iv', function(req, res) {
			var n = 0
			var id = setInterval(function() {
				n++
				if (n === 3) {
					clearInterval(id)
					res.send('ticks=' + n)
				}
			}, 1)
		})
	`, "GET", "/iv", sched)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != "ticks=3" {
		t.Fatalf("body = %q, want ticks=3", got)
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

	obj := p.BuildRequest(rc, nil)
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

func TestParseBodyURLEncodedRepeatedField(t *testing.T) {
	got := parseBody([]byte("a=1&a=2&b=3"), "application/x-www-form-urlencoded")
	m, ok := got.(map[string]interface{})
	if !ok {
		t.Fatalf("got %T, want map", got)
	}
	values, ok := m["a"].([]interface{})
	if !ok {
		t.Fatalf("a = %T, want []interface{}", m["a"])
	}
	if len(values) != 2 || values[0] != "1" || values[1] != "2" {
		t.Fatalf("a = %v, want [1 2]", values)
	}
	if m["b"] != "3" {
		t.Fatalf("b = %v, want single string", m["b"])
	}
}

func TestParseBodyMultipart(t *testing.T) {
	body := "--X\r\n" +
		"Content-Disposition: form-data; name=\"a\"\r\n\r\n" +
		"1\r\n" +
		"--X\r\n" +
		"Content-Disposition: form-data; name=\"a\"\r\n\r\n" +
		"2\r\n" +
		"--X\r\n" +
		"Content-Disposition: form-data; name=\"upload\"; filename=\"x.txt\"\r\n" +
		"Content-Type: text/plain\r\n\r\n" +
		"ignored\r\n" +
		"--X--\r\n"
	got := parseBody([]byte(body), "multipart/form-data; boundary=X")
	m, ok := got.(map[string]interface{})
	if !ok {
		t.Fatalf("got %T, want map", got)
	}
	if m["a"] != "1" {
		t.Fatalf("a = %v, want first value", m["a"])
	}
	if _, present := m["upload"]; present {
		t.Fatalf("file part leaked into body: %v", m)
	}
}

func TestParseBodyMultipartMissingBoundary(t *testing.T) {
	got := parseBody([]byte("raw"), "multipart/form-data")
	if got != "raw" {
		t.Fatalf("got %v, want raw string", got)
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

func serveScript(t *testing.T, script, method, path string, sched *fakeScheduler, headers ...http.Header) (*httptest.ResponseRecorder, error) {
	t.Helper()
	return serveScriptIn(t, t.TempDir(), script, method, path, sched, headers...)
}

func serveScriptIn(t *testing.T, dir, script, method, path string, sched *fakeScheduler, headers ...http.Header) (*httptest.ResponseRecorder, error) {
	t.Helper()
	fsys, err := files.NewFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fsys.Close() })

	p := NewProgram(Config{Quit: make(chan struct{})}, fsys, sched, nil)
	if err := p.Load([]byte(script)); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	w := NewResponseWriter(rec)
	ex := NewExchange(w)
	layers, params := p.Match(method, path)
	if len(layers) == 0 {
		t.Fatalf("no layers matched %s %s", method, path)
	}
	var hdrs http.Header
	if len(headers) > 0 {
		hdrs = headers[0]
	}
	reqObj := p.BuildRequest(&RequestContext{Method: method, Path: path, Headers: hdrs}, params)
	resObj := p.BuildResponse(w)

	serveErr := p.Serve(ex, time.Second, func() error {
		return p.RunChain(ex, layers, reqObj, resObj)
	})
	if serveErr != nil {
		t.Fatalf("serve: %v", serveErr)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if sched != nil {
			sched.run()
		}
		select {
		case <-ex.Done:
			return rec, ex.Err()
		default:
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("exchange did not finish")
	return nil, nil
}

func TestChainRunsMiddlewareInOrder(t *testing.T) {
	rec, err := serveScript(t, `
		var order = []
		app.use(function(req, res, next) { order.push('a'); next() })
		app.use(function(req, res, next) { order.push('b'); next() })
		app.get('/x', function(req, res) { res.send(order.join(',')) })
	`, "GET", "/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != "a,b" {
		t.Fatalf("body = %q, want a,b", got)
	}
}

func TestRouteMiddlewareRunsBeforeHandler(t *testing.T) {
	rec, err := serveScript(t, `
		app.get('/y',
			function(req, res, next) { res.set('X-MW', 'yes'); next() },
			function(req, res) { res.send('done') })
	`, "GET", "/y", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Header().Get("X-MW"); got != "yes" {
		t.Fatalf("X-MW = %q, want yes", got)
	}
	if got := rec.Body.String(); got != "done" {
		t.Fatalf("body = %q, want done", got)
	}
}

func TestNextErrorReachesErrorHandler(t *testing.T) {
	rec, err := serveScript(t, `
		app.get('/boom', function(req, res, next) { next(new Error('kaboom')) })
		app.use(function(err, req, res, next) { res.status(503).send('handled: ' + err.message) })
	`, "GET", "/boom", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Code; got != 503 {
		t.Fatalf("status = %d, want 503", got)
	}
	if got := rec.Body.String(); got != "handled: kaboom" {
		t.Fatalf("body = %q", got)
	}
}

func TestThrownErrorReachesErrorHandler(t *testing.T) {
	rec, err := serveScript(t, `
		app.get('/throw', function(req, res) { throw new Error('thrown') })
		app.use(function(err, req, res, next) { res.status(500).send('caught ' + err.message) })
	`, "GET", "/throw", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != "caught thrown" {
		t.Fatalf("body = %q", got)
	}
}

func TestUnhandledErrorIsReportedOnExchange(t *testing.T) {
	rec, err := serveScript(t, `
		app.get('/unhandled', function(req, res) { throw new Error('nope') })
	`, "GET", "/unhandled", nil)
	if err == nil {
		t.Fatal("expected an error on the exchange")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("body = %q, want empty", rec.Body.String())
	}
}

func TestParamsAndWildcard(t *testing.T) {
	script := `
		app.get('/users/:id', function(req, res) { res.json(req.params) })
		app.get('/files/*', function(req, res) { res.send(String(req.params[0])) })
	`
	rec, err := serveScript(t, script, "GET", "/users/42", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != `{"id":"42"}` {
		t.Fatalf("params body = %q", got)
	}

	rec, err = serveScript(t, script, "GET", "/files/a/b.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != "a/b.txt" {
		t.Fatalf("wildcard body = %q", got)
	}
}

func TestExpressShimAndExtraMethods(t *testing.T) {
	script := `
		const express = require('express')
		const a = express()
		a.put('/p', function(req, res) { res.send('put') })
		a.delete('/d', function(req, res) { res.send('delete') })
		a.all('/any', function(req, res) { res.send(req.method) })
		a.listen(3000)
	`
	rec, err := serveScript(t, script, "PUT", "/p", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != "put" {
		t.Fatalf("put body = %q", got)
	}

	rec, err = serveScript(t, script, "DELETE", "/d", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != "delete" {
		t.Fatalf("delete body = %q", got)
	}

	rec, err = serveScript(t, script, "GET", "/any", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != "GET" {
		t.Fatalf("all body = %q", got)
	}
}

func TestUnknownAppMethodFailsLoudly(t *testing.T) {
	fsys, err := files.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fsys.Close() })

	p := NewProgram(Config{Quit: make(chan struct{})}, fsys, nil, nil)
	if err := p.Load([]byte(`app.nope()`)); err == nil {
		t.Fatal("expected app.nope() to throw")
	}
}

func TestChainReportsInterrupt(t *testing.T) {
	fsys, err := files.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fsys.Close() })

	p := NewProgram(Config{Quit: make(chan struct{})}, fsys, nil, nil)
	if err := p.Load([]byte(`app.get('/spin', function(req, res) { while (true) {} })`)); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	w := NewResponseWriter(rec)
	ex := NewExchange(w)
	layers, params := p.Match("GET", "/spin")
	if len(layers) == 0 {
		t.Fatal("route not registered")
	}
	reqObj := p.BuildRequest(&RequestContext{Method: "GET", Path: "/spin"}, params)
	resObj := p.BuildResponse(w)

	err = p.Serve(ex, 50*time.Millisecond, func() error {
		return p.RunChain(ex, layers, reqObj, resObj)
	})
	var interrupted *sobek.InterruptedError
	if !errors.As(err, &interrupted) {
		t.Fatalf("err = %v, want an InterruptedError", err)
	}
	if w.Written() {
		t.Fatal("an interrupted handler should not write")
	}
	if ex.Err() != nil {
		t.Fatalf("interrupt reported as a handler error: %v", ex.Err())
	}
}

func TestAsyncNextAdvancesChain(t *testing.T) {
	sched := &fakeScheduler{}
	rec, err := serveScript(t, `
		app.get('/async-next', function(req, res, next) { setTimeout(next, 0) })
		app.get('/async-next', function(req, res) { res.send('after') })
	`, "GET", "/async-next", sched)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != "after" {
		t.Fatalf("body = %q, want after", got)
	}
}
