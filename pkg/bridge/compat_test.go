package bridge

import (
	"net/http"
	"strings"
	"testing"
)

func TestExpressBodyParserMiddleware(t *testing.T) {
	rec, err := serveScript(t, `
		const express = require('express')
		const bodyParser = require('body-parser')
		app.use(express.json())
		app.use(express.urlencoded({ extended: true }))
		app.use(bodyParser.text())
		app.post('/x', function(req, res) { res.json({ ok: true }) })
	`, "POST", "/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != `{"ok":true}` {
		t.Fatalf("body = %q", got)
	}
}

func TestExpressSettings(t *testing.T) {
	rec, err := serveScript(t, `
		const express = require('express')
		const app2 = express()
		app2.set('name', 'caddy')
		app2.enable('trust proxy')
		app2.get('/settings', function(req, res) {
			res.json({
				name: app2.get('name'),
				enabled: app2.enabled('trust proxy'),
				disabled: app2.disabled('missing')
			})
		})
	`, "GET", "/settings", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != `{"disabled":true,"enabled":true,"name":"caddy"}` {
		t.Fatalf("body = %q", got)
	}
}

func TestExpressResponseHeaderAndLocalHelpers(t *testing.T) {
	rec, err := serveScript(t, `
		app.get('/x', function(req, res) {
			res.set({ 'X-One': '1', 'X-Two': ['a', 'b'] })
			res.locals.answer = 42
			res.type('json').send({ answer: res.locals.answer, header: res.get('X-One') })
		})
	`, "GET", "/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != `{"answer":42,"header":"1"}` {
		t.Fatalf("body = %q", got)
	}
	if values := rec.Header().Values("X-Two"); len(values) != 2 || values[0] != "a" || values[1] != "b" {
		t.Fatalf("X-Two = %v", values)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q", ct)
	}
}

func TestExpressSendBuffer(t *testing.T) {
	rec, err := serveScript(t, `
		app.get('/x', function(req, res) {
			res.send(Buffer.from('hi there'))
		})
	`, "GET", "/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != "hi there" {
		t.Fatalf("body = %q", got)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
}

func TestExpressRedirectBack(t *testing.T) {
	rec, err := serveScript(t, `
		app.get('/x', function(req, res) { res.redirect('back') })
	`, "GET", "/x", nil, http.Header{"Referer": []string{"/from"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Header().Get("Location"); got != "/from" {
		t.Fatalf("Location = %q", got)
	}
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
}

func TestExpressNextRouteSkipsRemainingHandlers(t *testing.T) {
	rec, err := serveScript(t, `
		app.get('/x',
			function(req, res, next) { next('route') },
			function(req, res) { res.status(500).send('skipped') })
		app.get('/x', function(req, res) { res.send('next route') })
	`, "GET", "/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "next route" {
		t.Fatalf("body = %q", got)
	}
}

func TestExpressAsyncHandlerCompletesResponse(t *testing.T) {
	rec, err := serveScript(t, `
		app.get('/x', async function(req, res) {
			await Promise.resolve()
			res.send('async')
		})
	`, "GET", "/x", &fakeScheduler{})
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != "async" {
		t.Fatalf("body = %q", got)
	}
}

func TestResponseSendFileStringRoot(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "public/hello.txt", "file body")
	rec, err := serveScriptIn(t, dir, `
		app.get('/x', function(req, res) { res.sendFile('hello.txt', 'public') })
	`, "GET", "/x", &fakeScheduler{})
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != "file body" {
		t.Fatalf("body = %q", got)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q", ct)
	}
}

func TestExpressCookiesAndFormat(t *testing.T) {
	rec, err := serveScript(t, `
		app.get('/x', function(req, res) {
			res.format({
				'text/plain': function() { res.send(req.cookies.sid) },
				'application/json': function() { res.json({ sid: req.cookies.sid }) }
			})
		})
	`, "GET", "/x", nil,
		http.Header{"Accept": {"text/plain"}, "Cookie": {"sid=a%20b"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != "a b" {
		t.Fatalf("body = %q", got)
	}
}

func TestExpressDownloadAndVary(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "hello.txt", "download body")
	rec, err := serveScriptIn(t, dir, `
		app.get('/x', function(req, res) {
			res.vary('Accept')
			res.download('hello.txt', 'renamed.txt')
		})
	`, "GET", "/x", &fakeScheduler{})
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != "download body" {
		t.Fatalf("body = %q", got)
	}
	if got := rec.Header().Get("Content-Disposition"); got != `attachment; filename=renamed.txt` {
		t.Fatalf("Content-Disposition = %q", got)
	}
	if got := rec.Header().Get("Vary"); got != "Accept" {
		t.Fatalf("Vary = %q", got)
	}
}

func TestExpressClearCookie(t *testing.T) {
	rec, err := serveScript(t, `
		app.get('/x', function(req, res) {
			res.clearCookie('sid', { path: '/app' }).send('ok')
		})
	`, "GET", "/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	cookie := rec.Header().Get("Set-Cookie")
	if !strings.Contains(cookie, "sid=") || !strings.Contains(cookie, "Path=/app") {
		t.Fatalf("Set-Cookie = %q", cookie)
	}
}

func TestExpressAppLocalsAndUseArray(t *testing.T) {
	rec, err := serveScript(t, `
		const one = function(req, res, next) { res.set('X-One', '1'); next() }
		const two = function(req, res, next) { res.set('X-Two', '2'); next() }
		app.locals.answer = 42
		app.use([one, two])
		app.get('/x', function(req, res) { res.json({ answer: app.locals.answer }) })
	`, "GET", "/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Header().Get("X-One"); got != "1" {
		t.Fatalf("X-One = %q", got)
	}
	if got := rec.Header().Get("X-Two"); got != "2" {
		t.Fatalf("X-Two = %q", got)
	}
	if got := rec.Body.String(); got != `{"answer":42}` {
		t.Fatalf("body = %q", got)
	}
}
