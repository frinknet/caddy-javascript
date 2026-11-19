package bridge

import (
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/frinknet/caddy-javascript/pkg/files"
)

type failingScheduler struct {
	calls int32
}

func (f *failingScheduler) Submit(func()) bool {
	atomic.AddInt32(&f.calls, 1)
	return false
}

func newHardeningProgram(t *testing.T, cfg Config, sched Scheduler) *Program {
	t.Helper()
	fsys, err := files.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fsys.Close() })
	return NewProgram(cfg, fsys, sched, nil)
}

func serveOnce(t *testing.T, p *Program, method, path string) (*httptest.ResponseRecorder, *Exchange) {
	t.Helper()
	rec := httptest.NewRecorder()
	w := NewResponseWriter(rec)
	ex := NewExchange(w)
	layers, params := p.Match(method, path)
	if len(layers) == 0 {
		t.Fatalf("no layers matched %s %s", method, path)
	}
	reqObj := p.BuildRequest(&RequestContext{Method: method, Path: path}, params)
	resObj := p.BuildResponse(w, reqObj)
	if err := p.Serve(ex, time.Second, func() error {
		return p.RunChain(ex, layers, reqObj, resObj)
	}); err != nil {
		t.Fatalf("serve: %v", err)
	}
	return rec, ex
}

func waitDone(t *testing.T, ex *Exchange) {
	t.Helper()
	select {
	case <-ex.Done:
	case <-time.After(2 * time.Second):
		t.Fatal("exchange did not finish")
	}
}

func TestDroppedAsyncCallbackFailsExchange(t *testing.T) {
	p := newHardeningProgram(t, Config{Timeout: time.Second, Quit: make(chan struct{})}, &failingScheduler{})
	if err := p.Load([]byte(`
		app.get('/x', function(req, res) {
			fs.readFile('missing.txt', function(err, data) { res.send('ok') })
		})
	`)); err != nil {
		t.Fatal(err)
	}

	rec, ex := serveOnce(t, p, "GET", "/x")
	waitDone(t, ex)
	if !errors.Is(ex.Err(), ErrQueueFull) {
		t.Fatalf("err = %v, want ErrQueueFull", ex.Err())
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("body = %q, want empty", rec.Body.String())
	}
}

func TestDroppedTimerCallbackFailsExchange(t *testing.T) {
	p := newHardeningProgram(t, Config{Timeout: time.Second, Quit: make(chan struct{})}, &failingScheduler{})
	if err := p.Load([]byte(`
		app.get('/x', function(req, res) {
			setTimeout(function() { res.send('ok') }, 0)
		})
	`)); err != nil {
		t.Fatal(err)
	}

	_, ex := serveOnce(t, p, "GET", "/x")
	waitDone(t, ex)
	if !errors.Is(ex.Err(), ErrQueueFull) {
		t.Fatalf("err = %v, want ErrQueueFull", ex.Err())
	}
}

func TestFSCallbackReceivesErrorObject(t *testing.T) {
	rec, err := serveScript(t, `
		app.get('/x', function(req, res) {
			fs.readFile('missing.txt', function(err, data) {
				res.json({
					message: err.message,
					code: err.code,
					name: err.name,
					isError: err instanceof Error
				})
			})
		})
	`, "GET", "/x", &fakeScheduler{})
	if err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	for _, want := range []string{`"code":"ENOENT"`, `"name":"Error"`, `"isError":true`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body = %s, want %s", body, want)
		}
	}
	if strings.Contains(body, `"message":""`) {
		t.Fatalf("body = %s, want a message", body)
	}
}

func TestFetchCallbackReceivesErrorObject(t *testing.T) {
	rec, err := serveFetch(t, `
		app.get('/x', function(req, res) {
			fetch('http://example.invalid/', function(err) {
				res.json({
					message: err.message,
					code: err.code,
					isError: err instanceof Error
				})
			})
		})
	`, nil, "/x")
	if err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	for _, want := range []string{`"code":"ERR_FETCH_FAILED"`, `"isError":true`, "fetch_allow"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body = %s, want %s", body, want)
		}
	}
}

func TestGoIOBoundsConcurrentWork(t *testing.T) {
	p := newTestProgram(t, time.Second)
	p.ioLimit = 1

	first := make(chan struct{})
	releaseFirst := make(chan struct{})
	second := make(chan struct{})
	var running int32

	p.goIO(func() {
		atomic.AddInt32(&running, 1)
		close(first)
		<-releaseFirst
		atomic.AddInt32(&running, -1)
	})
	select {
	case <-first:
	case <-time.After(2 * time.Second):
		t.Fatal("first task did not start")
	}

	p.goIO(func() {
		if got := atomic.LoadInt32(&running); got != 0 {
			t.Errorf("second task ran while %d tasks were active", got)
		}
		close(second)
	})
	select {
	case <-second:
		t.Fatal("second task ran before a slot was free")
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseFirst)
	select {
	case <-second:
	case <-time.After(2 * time.Second):
		t.Fatal("queued task did not start after a slot was free")
	}
}
