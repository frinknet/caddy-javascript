package bridge

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/grafana/sobek"

	"github.com/frinknet/caddy-javascript/pkg/files"
)

// serveBody drives one request through a script, with a body and content type
// so the body parser middleware has something to work on.
func serveBody(t *testing.T, script, method, path, contentType, body string) (*httptest.ResponseRecorder, error) {
	t.Helper()
	fsys, err := files.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fsys.Close() })
	p := NewProgram(Config{Quit: make(chan struct{})}, fsys, nil, nil)
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
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	rc, err := NewRequestContext(r, 0)
	if err != nil {
		t.Fatal(err)
	}
	reqObj := p.BuildRequest(rc, params)
	resObj := p.BuildResponse(w, reqObj)

	if err := p.Serve(ex, time.Second, func() error {
		return p.RunChain(ex, layers, reqObj, resObj)
	}); err != nil {
		t.Fatalf("serve: %v", err)
	}
	return rec, ex.Err()
}

func TestBodyParserJSONParsesBody(t *testing.T) {
	rec, err := serveBody(t, `
		const express = require('express')
		app.post('/x', express.json(), function(req, res) { res.json(req.body) })
	`, "POST", "/x", "application/json", `{"name":"caddy","n":2}`)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != `{"n":2,"name":"caddy"}` {
		t.Fatalf("body = %q", got)
	}
}

func TestBodyParserJSONRejectsMalformedBody(t *testing.T) {
	rec, err := serveBody(t, `
		const express = require('express')
		app.post('/x', express.json(), function(req, res) { res.json(req.body) })
	`, "POST", "/x", "application/json", `{"name":`)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Bad Request") {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestBodyParserJSONStrictOption(t *testing.T) {
	script := `
		const express = require('express')
		app.post('/strict', express.json(), function(req, res) { res.json(req.body) })
		app.post('/loose', express.json({ strict: false }), function(req, res) { res.json(req.body) })
	`
	rec, err := serveBody(t, script, "POST", "/strict", "application/json", `"text"`)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("strict status = %d, want 400", rec.Code)
	}

	rec, err = serveBody(t, script, "POST", "/loose", "application/json", `"text"`)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("loose status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != `"text"` {
		t.Fatalf("loose body = %q", got)
	}
}

func TestBodyParserEmptyJSONBodyIsAnObject(t *testing.T) {
	rec, err := serveBody(t, `
		const express = require('express')
		app.post('/x', express.json(), function(req, res) { res.json(req.body) })
	`, "POST", "/x", "application/json", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != `{}` {
		t.Fatalf("body = %q, want {}", got)
	}
}

func TestBodyParserLimitOption(t *testing.T) {
	script := `
		const express = require('express')
		app.post('/x', express.json({ limit: '10b' }), function(req, res) { res.json(req.body) })
	`
	rec, err := serveBody(t, script, "POST", "/x", "application/json", `{"a":1}`)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	rec, err = serveBody(t, script, "POST", "/x", "application/json", `{"abcdefgh":1}`)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}

func TestBodyParserTypeOption(t *testing.T) {
	script := `
		const express = require('express')
		app.post('/default', express.json(), function(req, res) { res.json({ body: req.body }) })
		app.post('/custom', express.json({ type: 'text/plain' }), function(req, res) { res.json(req.body) })
	`
	rec, err := serveBody(t, script, "POST", "/default", "text/plain", `{"a":1}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != `{"body":"{\"a\":1}"}` {
		t.Fatalf("default body = %q, want the raw string from the eager parse", got)
	}

	rec, err = serveBody(t, script, "POST", "/custom", "text/plain", `{"a":1}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != `{"a":1}` {
		t.Fatalf("custom body = %q", got)
	}
}

func TestBodyParserVerifyOption(t *testing.T) {
	script := `
		const express = require('express')
		app.post('/x', express.json({
			verify: function(req, res, buf) {
				if (buf.toString() !== '{"ok":true}') {
					const err = new Error('rejected body')
					throw err
				}
			}
		}), function(req, res) { res.json(req.body) })
	`
	rec, err := serveBody(t, script, "POST", "/x", "application/json", `{"ok":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	rec, err = serveBody(t, script, "POST", "/x", "application/json", `{"ok":false}`)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("rejected status = %d, want 403", rec.Code)
	}
}

func TestBodyParserErrorReachesErrorHandler(t *testing.T) {
	rec, err := serveBody(t, `
		const express = require('express')
		app.use(express.json())
		app.post('/x', function(req, res) { res.json(req.body) })
		app.use(function(err, req, res, next) {
			res.status(422).json({ status: err.status, type: err.type })
		})
	`, "POST", "/x", "application/json", `{"a":`)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if got := rec.Body.String(); got != `{"status":400,"type":"entity.parse.failed"}` {
		t.Fatalf("body = %q", got)
	}
}

func TestBodyParserTextAndRaw(t *testing.T) {
	script := `
		const express = require('express')
		const bodyParser = require('body-parser')
		app.post('/text', express.text(), function(req, res) { res.json({ value: req.body }) })
		app.post('/raw', bodyParser.raw({ type: 'application/octet-stream' }), function(req, res) { res.json({ value: req.body }) })
	`
	rec, err := serveBody(t, script, "POST", "/text", "text/plain", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != `{"value":"hello"}` {
		t.Fatalf("text body = %q", got)
	}

	rec, err = serveBody(t, script, "POST", "/raw", "application/octet-stream", "bytes")
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != `{"value":"bytes"}` {
		t.Fatalf("raw body = %q", got)
	}
}

func TestBodyParserURLEncodedLimitApplies(t *testing.T) {
	rec, err := serveBody(t, `
		const express = require('express')
		app.post('/x', express.urlencoded({ limit: 4 }), function(req, res) { res.json(req.body) })
	`, "POST", "/x", "application/x-www-form-urlencoded", "a=1&b=2")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}

func TestParseBodyLimit(t *testing.T) {
	rt := sobek.New()
	cases := []struct {
		in   interface{}
		want int64
		ok   bool
	}{
		{1024, 1024, true},
		{"100kb", 102400, true},
		{"1mb", 1 << 20, true},
		{"1.5kb", 1536, true},
		{"512", 512, true},
		{"1gb", 1 << 30, true},
		{"nope", 0, false},
		{"-1kb", 0, false},
	}
	for _, tc := range cases {
		got, ok := parseBodyLimit(rt.ToValue(tc.in))
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("parseBodyLimit(%v) = %d, %v; want %d, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestContentTypeMatches(t *testing.T) {
	cases := []struct {
		pattern, mediaType string
		want               bool
	}{
		{"application/json", "application/json; charset=utf-8", true},
		{"application/json", "application/vnd.api+json", true},
		{"application/json", "text/plain", false},
		{"application/*", "application/xml", true},
		{"*/json", "application/json", true},
		{"*/*", "text/plain", true},
		{"json", "application/json", true},
		{"text/plain", "text/plain", true},
		{"text/plain", "text/html", false},
	}
	for _, tc := range cases {
		if got := contentTypeMatches(tc.pattern, tc.mediaType); got != tc.want {
			t.Errorf("contentTypeMatches(%q, %q) = %v, want %v", tc.pattern, tc.mediaType, got, tc.want)
		}
	}
}
