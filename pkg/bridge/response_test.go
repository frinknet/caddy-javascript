package bridge

import (
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestSendStatus(t *testing.T) {
	rec, err := serveScript(t, `
		app.get('/x', function(req, res) { res.sendStatus(404) })
	`, "GET", "/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if got := rec.Body.String(); got != "Not Found" {
		t.Fatalf("body = %q, want Not Found", got)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q, want text/plain", ct)
	}
}

func TestSendStatusUnknownCode(t *testing.T) {
	rec, err := serveScript(t, `
		app.get('/x', function(req, res) { res.sendStatus(599) })
	`, "GET", "/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != 599 {
		t.Fatalf("status = %d, want 599", rec.Code)
	}
	if got := rec.Body.String(); got != "599" {
		t.Fatalf("body = %q, want 599", got)
	}
}

func TestSendStatusNoContentHasNoBody(t *testing.T) {
	rec, err := serveScript(t, `
		app.get('/x', function(req, res) { res.sendStatus(204) })
	`, "GET", "/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("body = %q, want empty", rec.Body.String())
	}
}

func TestAppendHeader(t *testing.T) {
	rec, err := serveScript(t, `
		app.get('/x', function(req, res) {
			res.append('X-Multi', 'one').append('X-Multi', 'two').send('ok')
		})
	`, "GET", "/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := rec.Header().Values("X-Multi")
	if len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Fatalf("X-Multi = %v, want [one two]", got)
	}
}

func TestAppendHeaderArray(t *testing.T) {
	rec, err := serveScript(t, `
		app.get('/x', function(req, res) { res.append('X-Multi', ['a', 'b']).send('ok') })
	`, "GET", "/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := rec.Header().Values("X-Multi")
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("X-Multi = %v, want [a b]", got)
	}
}

func TestLocation(t *testing.T) {
	rec, err := serveScript(t, `
		app.get('/x', function(req, res) { res.location('/next').status(201).send('ok') })
	`, "GET", "/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Header().Get("Location"); got != "/next" {
		t.Fatalf("Location = %q, want /next", got)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
}

func TestLocationBack(t *testing.T) {
	headers := http.Header{"Referer": []string{"/from"}}
	rec, err := serveScript(t, `
		app.get('/x', function(req, res) { res.location('back').status(303).send('ok') })
	`, "GET", "/x", nil, headers)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Header().Get("Location"); got != "/from" {
		t.Fatalf("Location = %q, want /from", got)
	}
}

func TestLocationBackWithoutReferer(t *testing.T) {
	rec, err := serveScript(t, `
		app.get('/x', function(req, res) { res.location('back').status(303).send('ok') })
	`, "GET", "/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Header().Get("Location"); got != "/" {
		t.Fatalf("Location = %q, want /", got)
	}
}

func TestCookie(t *testing.T) {
	rec, err := serveScript(t, `
		app.get('/x', function(req, res) {
			res.cookie('sid', 'a b', {
				path: '/app',
				maxAge: 60000,
				httpOnly: true,
				secure: true,
				sameSite: 'lax'
			}).send('ok')
		})
	`, "GET", "/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %v, want one", cookies)
	}
	cookie := cookies[0]
	if cookie.Name != "sid" || cookie.Value != "a%20b" {
		t.Fatalf("cookie = %q=%q, want sid=a%%20b", cookie.Name, cookie.Value)
	}
	if cookie.Path != "/app" {
		t.Fatalf("path = %q, want /app", cookie.Path)
	}
	if cookie.MaxAge != 60 {
		t.Fatalf("maxAge = %d, want 60", cookie.MaxAge)
	}
	if !cookie.HttpOnly || !cookie.Secure {
		t.Fatalf("httpOnly = %v secure = %v, want true true", cookie.HttpOnly, cookie.Secure)
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("sameSite = %v, want lax", cookie.SameSite)
	}
}

func TestCookieDefaultPathAndEncoding(t *testing.T) {
	rec, err := serveScript(t, `
		app.get('/x', function(req, res) { res.cookie('a', 'x=y;z').send('ok') })
	`, "GET", "/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %v, want one", cookies)
	}
	if cookies[0].Value != "x%3Dy%3Bz" {
		t.Fatalf("value = %q, want x%%3Dy%%3Bz", cookies[0].Value)
	}
	if cookies[0].Path != "/" {
		t.Fatalf("path = %q, want /", cookies[0].Path)
	}
}

func sendFileScript(body string) string {
	return `app.get('/f', function(req, res) { ` + body + ` })`
}

func headFileScript(body string) string {
	return `app.head('/f', function(req, res) { ` + body + ` })`
}

func TestSendFile(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "public/hello.txt", "hi there")

	rec, err := serveScriptIn(t, dir, sendFileScript(`res.sendFile('public/hello.txt')`), "GET", "/f", &fakeScheduler{})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "hi there" {
		t.Fatalf("body = %q, want hi there", got)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q, want text/plain", ct)
	}
	if cl := rec.Header().Get("Content-Length"); cl != "8" {
		t.Fatalf("Content-Length = %q, want 8", cl)
	}
}

func TestSendFileRootAndHeaders(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "public/hello.txt", "hi")

	script := sendFileScript(`res.sendFile('hello.txt', {
		root: 'public',
		maxAge: 120000,
		headers: { 'X-Extra': 'yes' },
		dotfiles: 'deny'
	})`)
	rec, err := serveScriptIn(t, dir, script, "GET", "/f", &fakeScheduler{})
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != "hi" {
		t.Fatalf("body = %q, want hi", got)
	}
	if got := rec.Header().Get("X-Extra"); got != "yes" {
		t.Fatalf("X-Extra = %q, want yes", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=120" {
		t.Fatalf("Cache-Control = %q, want public, max-age=120", got)
	}
}

func TestSendFileHeadHasNoBody(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "public/hello.txt", "hi")

	rec, err := serveScriptIn(t, dir, headFileScript(`res.sendFile('public/hello.txt')`), "HEAD", "/f", &fakeScheduler{})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("body = %q, want empty", rec.Body.String())
	}
	if cl := rec.Header().Get("Content-Length"); cl != "2" {
		t.Fatalf("Content-Length = %q, want 2", cl)
	}
}

func TestSendFileMissingIs404(t *testing.T) {
	dir := t.TempDir()
	rec, err := serveScriptIn(t, dir, sendFileScript(`res.sendFile('nope.txt')`), "GET", "/f", &fakeScheduler{})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestSendFileMissingInvokesCallback(t *testing.T) {
	dir := t.TempDir()
	script := sendFileScript(`res.sendFile('nope.txt', function(err) {
		if (!err) return res.send('served')
		res.status(418).send('failed: ' + err.message)
	})`)
	rec, err := serveScriptIn(t, dir, script, "GET", "/f", &fakeScheduler{})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want 418", rec.Code)
	}
	if !strings.HasPrefix(rec.Body.String(), "failed: ") {
		t.Fatalf("body = %q, want callback error", rec.Body.String())
	}
}

func TestSendFileDoesNotEscapeRoot(t *testing.T) {
	parent := t.TempDir()
	dir := parent + "/root"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeModule(t, parent, "secret.txt", "secret")

	rec, err := serveScriptIn(t, dir, sendFileScript(`res.sendFile('../secret.txt')`), "GET", "/f", &fakeScheduler{})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("served outside the sandbox: %q", rec.Body.String())
	}
}

func TestSendFileDoesNotEscapeConfiguredRoot(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "public/hello.txt", "hi")
	writeModule(t, dir, "secret.txt", "SECRET")

	script := sendFileScript(`res.sendFile('../secret.txt', { root: 'public' })`)
	rec, err := serveScriptIn(t, dir, script, "GET", "/f", &fakeScheduler{})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "SECRET") {
		t.Fatalf("served outside the configured root: %q", rec.Body.String())
	}
}

func TestSendFileDotfileDenied(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "public/.env", "TOKEN=1")

	script := sendFileScript(`res.sendFile('public/.env', { dotfiles: 'deny' })`)
	rec, err := serveScriptIn(t, dir, script, "GET", "/f", &fakeScheduler{})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestSendFileDotfileIgnoredByDefault(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "public/.env", "TOKEN=1")

	rec, err := serveScriptIn(t, dir, sendFileScript(`res.sendFile('public/.env')`), "GET", "/f", &fakeScheduler{})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "TOKEN") {
		t.Fatalf("served a dotfile: %q", rec.Body.String())
	}
}
