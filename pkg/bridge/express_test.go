package bridge

import (
	"strings"
	"testing"
)

func TestExpressRouterMountsRelativeRoutes(t *testing.T) {
	rec, err := serveScript(t, `
		const express = require('express')
		const router = express.Router()
		router.get('/hello', function(req, res) { res.send('hello ' + req.params.id) })
		app.use('/api/:id', router)
	`, "GET", "/api/7/hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != "hello 7" {
		t.Fatalf("body = %q, want hello 7", got)
	}
}

func TestExpressRouterNestedAndMiddleware(t *testing.T) {
	rec, err := serveScript(t, `
		const express = require('express')
		const inner = express.Router()
		inner.get('/deep', function(req, res) { res.send('deep') })
		const outer = express.Router()
		outer.use(function(req, res, next) { res.set('X-Outer', '1'); next() })
		outer.use('/v1', inner)
		app.use('/api', outer)
	`, "GET", "/api/v1/deep", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != "deep" {
		t.Fatalf("body = %q, want deep", got)
	}
	if got := rec.Header().Get("X-Outer"); got != "1" {
		t.Fatalf("X-Outer = %q, want 1", got)
	}
}

func TestExpressRouterSeesLaterRoutes(t *testing.T) {
	rec, err := serveScript(t, `
		const express = require('express')
		const router = express.Router()
		app.use('/late', router)
		router.get('/x', function(req, res) { res.send('late') })
	`, "GET", "/late/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != "late" {
		t.Fatalf("body = %q, want late", got)
	}
}

func TestExpressRouterUnmatchedFallsThrough(t *testing.T) {
	rec, err := serveScript(t, `
		const express = require('express')
		const router = express.Router()
		router.get('/only', function(req, res) { res.send('only') })
		app.use('/api', router)
		app.use(function(req, res) { res.status(404).send('fallback') })
	`, "GET", "/api/missing", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != "fallback" {
		t.Fatalf("body = %q, want fallback", got)
	}
}

func staticScript(mount, opts string) string {
	options := ""
	if opts != "" {
		options = ", " + opts
	}
	call := "app.use(express.static('public'" + options + "))"
	if mount != "" {
		call = "app.use(" + mount + ", express.static('public'" + options + "))"
	}
	return `
		const express = require('express')
		` + call + `
		app.use(function(req, res) { res.status(404).send('miss') })
	`
}

func TestExpressStaticServesFile(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "public/hello.txt", "hi there")

	rec, err := serveScriptIn(t, dir, staticScript("'/static'", ""), "GET", "/static/hello.txt", &fakeScheduler{})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "hi there" {
		t.Fatalf("body = %q, want hi there", got)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q, want text/plain", ct)
	}
}

func TestExpressStaticServesIndex(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "public/index.html", "<h1>home</h1>")

	rec, err := serveScriptIn(t, dir, staticScript("'/static'", ""), "GET", "/static/", &fakeScheduler{})
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != "<h1>home</h1>" {
		t.Fatalf("body = %q", got)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", ct)
	}
}

func TestExpressStaticMissFallsThrough(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "public/hello.txt", "hi")

	rec, err := serveScriptIn(t, dir, staticScript("'/static'", ""), "GET", "/static/missing", &fakeScheduler{})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != 404 || rec.Body.String() != "miss" {
		t.Fatalf("status = %d body = %q, want 404 miss", rec.Code, rec.Body.String())
	}
}

func TestExpressStaticHeadHasNoBody(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "public/hello.txt", "hi")

	rec, err := serveScriptIn(t, dir, staticScript("'/static'", ""), "HEAD", "/static/hello.txt", &fakeScheduler{})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("body = %q, want empty", rec.Body.String())
	}
	if got := rec.Header().Get("Content-Length"); got != "2" {
		t.Fatalf("Content-Length = %q, want 2", got)
	}
}

func TestExpressStaticSkipsNonGet(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "public/hello.txt", "hi")

	rec, err := serveScriptIn(t, dir, staticScript("'/static'", ""), "POST", "/static/hello.txt", &fakeScheduler{})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != 404 || rec.Body.String() != "miss" {
		t.Fatalf("status = %d body = %q, want 404 miss", rec.Code, rec.Body.String())
	}
}

func TestExpressStaticIgnoresDotfilesByDefault(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "public/.secret", "nope")

	rec, err := serveScriptIn(t, dir, staticScript("'/static'", ""), "GET", "/static/.secret", &fakeScheduler{})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != 404 || rec.Body.String() != "miss" {
		t.Fatalf("status = %d body = %q, want 404 miss", rec.Code, rec.Body.String())
	}
}

func TestExpressStaticDotfilesAllow(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "public/.secret", "nope")

	rec, err := serveScriptIn(t, dir, staticScript("'/static'", "{ dotfiles: 'allow' }"), "GET", "/static/.secret", &fakeScheduler{})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 || rec.Body.String() != "nope" {
		t.Fatalf("status = %d body = %q, want 200 nope", rec.Code, rec.Body.String())
	}
}

func TestExpressStaticFallthroughFalse(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "public/hello.txt", "hi")

	rec, err := serveScriptIn(t, dir, staticScript("'/static'", "{ fallthrough: false }"), "GET", "/static/missing", &fakeScheduler{})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != 404 {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Not Found") {
		t.Fatalf("body = %q, want Not Found", rec.Body.String())
	}
}

func TestExpressStaticIndexDisabled(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "public/index.html", "home")

	rec, err := serveScriptIn(t, dir, staticScript("'/static'", "{ index: false }"), "GET", "/static/", &fakeScheduler{})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != 404 || rec.Body.String() != "miss" {
		t.Fatalf("status = %d body = %q, want 404 miss", rec.Code, rec.Body.String())
	}
}

func TestExpressStaticInsideRouter(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "public/app.js", "console.log(1)")

	script := `
		const express = require('express')
		const router = express.Router()
		router.use('/assets', express.static('public'))
		app.use('/api', router)
		app.use(function(req, res) { res.status(404).send('miss') })
	`
	rec, err := serveScriptIn(t, dir, script, "GET", "/api/assets/app.js", &fakeScheduler{})
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != "console.log(1)" {
		t.Fatalf("body = %q", got)
	}
}

func TestExpressStaticAtRoot(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "public/hello.txt", "root")

	rec, err := serveScriptIn(t, dir, staticScript("", ""), "GET", "/hello.txt", &fakeScheduler{})
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != "root" {
		t.Fatalf("body = %q", got)
	}
}

func TestExpressStaticDotfilesDeny(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "public/.secret", "nope")

	rec, err := serveScriptIn(t, dir, staticScript("'/static'", "{ dotfiles: 'deny' }"), "GET", "/static/.secret", &fakeScheduler{})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != 403 {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestExpressStaticAsRouteHandler(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "public/hello.txt", "direct")

	script := `
		const express = require('express')
		app.get('/hello.txt', express.static('public'))
	`
	rec, err := serveScriptIn(t, dir, script, "GET", "/hello.txt", &fakeScheduler{})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 || rec.Body.String() != "direct" {
		t.Fatalf("status = %d body = %q, want 200 direct", rec.Code, rec.Body.String())
	}
}

func TestExpressRouterMountsAtRoot(t *testing.T) {
	rec, err := serveScript(t, `
		const express = require('express')
		const router = express.Router()
		router.get('/rooted', function(req, res) { res.send('rooted') })
		app.use(router)
	`, "GET", "/rooted", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != "rooted" {
		t.Fatalf("body = %q, want rooted", got)
	}
}
