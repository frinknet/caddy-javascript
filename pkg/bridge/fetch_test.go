package bridge

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/frinknet/caddy-javascript/pkg/files"
)

func newFetchServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/json":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Test", "yes")
			_, _ = io.WriteString(w, `{"a":1}`)
		case "/stream":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, "abc")
			_, _ = io.WriteString(w, "def")
		case "/echo":
			body, _ := io.ReadAll(r.Body)
			w.Header().Set("X-Method", r.Method)
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, "%s|%s|%s", r.Method, r.Header.Get("X-Custom"), string(body))
		case "/redirect":
			http.Redirect(w, r, "/json", http.StatusFound)
		case "/big":
			_, _ = w.Write(bytes.Repeat([]byte("x"), maxFetchBytes+1))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func fetchHost(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Hostname()
}

func serveFetch(t *testing.T, script string, allow []string, path string) (*httptest.ResponseRecorder, error) {
	t.Helper()
	fsys, err := files.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fsys.Close() })

	sched := &fakeScheduler{}
	p := NewProgram(Config{Quit: make(chan struct{}), FetchAllow: allow}, fsys, sched, nil)
	if err := p.Load([]byte(script)); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	w := NewResponseWriter(rec)
	ex := NewExchange(w)
	layers, params := p.Match("GET", path)
	if len(layers) == 0 {
		t.Fatalf("no layers matched GET %s", path)
	}
	reqObj := p.BuildRequest(&RequestContext{Method: "GET", Path: path}, params)
	resObj := p.BuildResponse(w)

	if err := p.Serve(ex, 2*time.Second, func() error {
		return p.RunChain(ex, layers, reqObj, resObj)
	}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		sched.run()
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

func TestFetchPromiseResponseFields(t *testing.T) {
	srv := newFetchServer(t)
	script := fmt.Sprintf(`
		app.get('/x', function(req, res) {
			fetch(%q).then(function(r) {
				res.json({
					status: r.status,
					ok: r.ok,
					statusText: r.statusText,
					header: r.headers.get('X-Test'),
					missing: r.headers.get('X-Missing'),
					url: r.url
				})
			}).catch(function(e) {
				res.status(500).send(e.message)
			})
		})
	`, srv.URL+"/json")

	rec, err := serveFetch(t, script, []string{fetchHost(t, srv)}, "/x")
	if err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	for _, want := range []string{`"status":200`, `"ok":true`, `"statusText":"OK"`, `"header":"yes"`, `"missing":null`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body = %s, want %s", body, want)
		}
	}
}

func TestFetchPromiseText(t *testing.T) {
	srv := newFetchServer(t)
	script := fmt.Sprintf(`
		app.get('/x', function(req, res) {
			fetch(%q)
				.then(function(r) { return r.text() })
				.then(function(t) { res.send(t) })
				.catch(function(e) { res.status(500).send(e.message) })
		})
	`, srv.URL+"/json")

	rec, err := serveFetch(t, script, []string{fetchHost(t, srv)}, "/x")
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != `{"a":1}` {
		t.Fatalf("body = %q", got)
	}
}

func TestFetchPromiseJSON(t *testing.T) {
	srv := newFetchServer(t)
	script := fmt.Sprintf(`
		app.get('/x', function(req, res) {
			fetch(%q)
				.then(function(r) { return r.json() })
				.then(function(v) { res.json({ a: v.a }) })
				.catch(function(e) { res.status(500).send(e.message) })
		})
	`, srv.URL+"/json")

	rec, err := serveFetch(t, script, []string{fetchHost(t, srv)}, "/x")
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != `{"a":1}` {
		t.Fatalf("body = %q", got)
	}
}

func TestFetchStreamingReader(t *testing.T) {
	srv := newFetchServer(t)
	script := fmt.Sprintf(`
		app.get('/x', function(req, res) {
			fetch(%q).then(function(r) {
				var reader = r.body.getReader()
				var out = ''
				function pump() {
					return reader.read().then(function(step) {
						if (step.done) { res.send(out); return }
						out += step.value
						return pump()
					})
				}
				return pump()
			}).catch(function(e) {
				res.status(500).send(e.message)
			})
		})
	`, srv.URL+"/stream")

	rec, err := serveFetch(t, script, []string{fetchHost(t, srv)}, "/x")
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != "abcdef" {
		t.Fatalf("body = %q, want abcdef", got)
	}
}

func TestFetchOptionsAndCallback(t *testing.T) {
	srv := newFetchServer(t)
	script := fmt.Sprintf(`
		app.get('/x', function(req, res) {
			fetch(%q, {
				method: 'POST',
				headers: { 'X-Custom': 'yes' },
				body: 'hi'
			}, function(err, r) {
				if (err) { res.status(502).send(err); return }
				res.status(r.status).send(r.text + '|' + r.headers.get('X-Method'))
			})
		})
	`, srv.URL+"/echo")

	rec, err := serveFetch(t, script, []string{fetchHost(t, srv)}, "/x")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != 201 {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	if got := rec.Body.String(); got != "POST|yes|hi|POST" {
		t.Fatalf("body = %q", got)
	}
}

func TestFetchCallbackLegacyFields(t *testing.T) {
	srv := newFetchServer(t)
	script := fmt.Sprintf(`
		app.get('/x', function(req, res) {
			fetch(%q, function(err, r) {
				if (err) { res.status(502).send(err); return }
				res.json({
					status: r.status,
					ok: r.ok,
					text: r.text,
					type: r.headers.get('Content-Type')
				})
			})
		})
	`, srv.URL+"/json")

	rec, err := serveFetch(t, script, []string{fetchHost(t, srv)}, "/x")
	if err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	for _, want := range []string{`"status":200`, `"ok":true`, `"text":"{\"a\":1}"`, `"type":"application/json"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body = %s, want %s", body, want)
		}
	}
}

func TestFetchRedirect(t *testing.T) {
	srv := newFetchServer(t)
	script := fmt.Sprintf(`
		app.get('/x', function(req, res) {
			fetch(%q).then(function(r) {
				res.json({ status: r.status, redirected: r.redirected, url: r.url })
			}).catch(function(e) {
				res.status(500).send(e.message)
			})
		})
	`, srv.URL+"/redirect")

	rec, err := serveFetch(t, script, []string{fetchHost(t, srv)}, "/x")
	if err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	for _, want := range []string{`"status":200`, `"redirected":true`, srv.URL + "/json"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body = %s, want %s", body, want)
		}
	}
}

func TestFetchBlockedHostRejectsPromise(t *testing.T) {
	script := `
		app.get('/x', function(req, res) {
			fetch('http://example.invalid/').then(function() {
				res.send('allowed')
			}).catch(function(e) {
				res.status(502).send(e.message)
			})
		})
	`
	rec, err := serveFetch(t, script, nil, "/x")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != 502 {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "not in fetch_allow") {
		t.Fatalf("body = %q", body)
	}
}

func TestFetchBlockedHostReportsToCallback(t *testing.T) {
	script := `
		app.get('/x', function(req, res) {
			fetch('http://example.invalid/', function(err) {
				if (err) { res.status(502).send(err); return }
				res.send('allowed')
			})
		})
	`
	rec, err := serveFetch(t, script, nil, "/x")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != 502 || !strings.Contains(rec.Body.String(), "not in fetch_allow") {
		t.Fatalf("status = %d body = %q", rec.Code, rec.Body.String())
	}
}

func TestFetchRejectsBodyOnGET(t *testing.T) {
	script := `
		app.get('/x', function(req, res) {
			fetch('http://example.invalid/', { body: 'x' }).catch(function(e) {
				res.status(502).send(e.message)
			})
		})
	`
	rec, err := serveFetch(t, script, nil, "/x")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != 502 || !strings.Contains(rec.Body.String(), "cannot have a body") {
		t.Fatalf("status = %d body = %q", rec.Code, rec.Body.String())
	}
}

func TestFetchStreamingCap(t *testing.T) {
	srv := newFetchServer(t)
	script := fmt.Sprintf(`
		app.get('/x', function(req, res) {
			fetch(%q).then(function(r) {
				var reader = r.body.getReader()
				function pump() {
					return reader.read().then(function(step) {
						if (step.done) { res.send('done'); return }
						return pump()
					})
				}
				return pump()
			}).catch(function(e) {
				res.status(502).send(e.message)
			})
		})
	`, srv.URL+"/big")

	rec, err := serveFetch(t, script, []string{fetchHost(t, srv)}, "/x")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != 502 || !strings.Contains(rec.Body.String(), "exceeds") {
		t.Fatalf("status = %d body = %q", rec.Code, rec.Body.String())
	}
}
