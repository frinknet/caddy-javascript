package bridge

import (
	"net/http"
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

func TestExpressRouteChain(t *testing.T) {
	script := `
		const express = require('express')
		const router = express.Router()
		router.route('/thing')
			.get(function(req, res) { res.send('get') })
			.post(function(req, res) { res.send('post') })
		app.use('/api', router)
	`
	rec, err := serveScript(t, script, "GET", "/api/thing", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != "get" {
		t.Fatalf("GET body = %q, want get", got)
	}
	rec, err = serveScript(t, script, "POST", "/api/thing", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != "post" {
		t.Fatalf("POST body = %q, want post", got)
	}
}

func TestExpressParam(t *testing.T) {
	rec, err := serveScript(t, `
		const express = require('express')
		const router = express.Router()
		router.param('id', function(req, res, next, value, name) {
			req.params[name] = 'v' + value
			next()
		})
		router.get('/item/:id', function(req, res) { res.send(req.params.id) })
		app.use(router)
	`, "GET", "/item/7", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != "v7" {
		t.Fatalf("body = %q, want v7", got)
	}
}

func TestExpressParamSkipsUnmatchedRoute(t *testing.T) {
	rec, err := serveScript(t, `
		const express = require('express')
		const router = express.Router()
		router.param('id', function(req, res, next) { res.status(500).send('param') })
		router.get('/item/:id', function(req, res) { res.send('item') })
		app.use(router)
		app.use(function(req, res) { res.status(404).send('miss') })
	`, "GET", "/other", nil)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != 404 || rec.Body.String() != "miss" {
		t.Fatalf("status = %d body = %q, want 404 miss", rec.Code, rec.Body.String())
	}
}

func TestExpressStaticDirectoryRedirect(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "public/dir/index.html", "home")

	rec, err := serveScriptIn(t, dir, staticScript("'/static'", ""), "GET", "/static/dir", &fakeScheduler{})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != 301 {
		t.Fatalf("status = %d, want 301", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/static/dir/" {
		t.Fatalf("Location = %q, want /static/dir/", got)
	}
}

func TestExpressStaticRange(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "public/hello.txt", "0123456789")

	rec, err := serveScriptIn(t, dir, staticScript("'/static'", ""), "GET", "/static/hello.txt", &fakeScheduler{}, http.Header{"Range": {"bytes=2-5"}})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != 206 {
		t.Fatalf("status = %d, want 206", rec.Code)
	}
	if got := rec.Body.String(); got != "2345" {
		t.Fatalf("body = %q, want 2345", got)
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes 2-5/10" {
		t.Fatalf("Content-Range = %q, want bytes 2-5/10", got)
	}
}

func TestExpressStaticConditional304(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "public/hello.txt", "hi")

	rec, err := serveScriptIn(t, dir, staticScript("'/static'", ""), "GET", "/static/hello.txt", &fakeScheduler{})
	if err != nil {
		t.Fatal(err)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("missing ETag")
	}

	rec, err = serveScriptIn(t, dir, staticScript("'/static'", ""), "GET", "/static/hello.txt", &fakeScheduler{}, http.Header{"If-None-Match": {etag}})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != 304 {
		t.Fatalf("status = %d, want 304", rec.Code)
	}
}

func TestExpressStaticMaxAge(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "public/hello.txt", "hi")

	rec, err := serveScriptIn(t, dir, staticScript("'/static'", "{ maxAge: 60000 }"), "GET", "/static/hello.txt", &fakeScheduler{})
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=60" {
		t.Fatalf("Cache-Control = %q, want public, max-age=60", got)
	}
}

func TestExpressStaticETagDisabled(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "public/hello.txt", "hi")

	rec, err := serveScriptIn(t, dir, staticScript("'/static'", "{ etag: false }"), "GET", "/static/hello.txt", &fakeScheduler{})
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Header().Get("ETag"); got != "" {
		t.Fatalf("ETag = %q, want empty", got)
	}
}

func TestExpressStaticExtensions(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "public/about.html", "about")

	rec, err := serveScriptIn(t, dir, staticScript("'/static'", "{ extensions: ['html'] }"), "GET", "/static/about", &fakeScheduler{})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 || rec.Body.String() != "about" {
		t.Fatalf("status = %d body = %q, want 200 about", rec.Code, rec.Body.String())
	}
}

func TestExpressNextRouterLeavesRouter(t *testing.T) {
	rec, err := serveScript(t, `
		const express = require('express')
		const router = express.Router()
		router.get('/skip',
			function(req, res, next) { next('router') },
			function(req, res) { res.send('inner') })
		app.use('/api', router)
		app.use(function(req, res) { res.send('parent ' + req.baseUrl) })
	`, "GET", "/api/skip", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != "parent " {
		t.Fatalf("body = %q, want parent", got)
	}
}

func TestExpressNextRouterLeavesNestedRouter(t *testing.T) {
	rec, err := serveScript(t, `
		const express = require('express')
		const inner = express.Router()
		inner.get('/skip', function(req, res, next) { next('router') })
		const outer = express.Router()
		outer.use('/v1', inner)
		outer.use(function(req, res) { res.send('outer ' + req.baseUrl) })
		app.use('/api', outer)
	`, "GET", "/api/v1/skip", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != "outer /api" {
		t.Fatalf("body = %q, want outer /api", got)
	}
}

func TestExpressBaseUrlAndRouteFollowMounts(t *testing.T) {
	rec, err := serveScript(t, `
		const express = require('express')
		const inner = express.Router()
		inner.get('/item/:id', function(req, res) {
			res.json({ baseUrl: req.baseUrl, path: req.route.path, method: req.route.method })
		})
		const outer = express.Router()
		outer.use('/v1', inner)
		app.use('/api/:space', outer)
	`, "GET", "/api/acme/v1/item/7", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"baseUrl":"/api/acme/v1","method":"GET","path":"/item/:id"}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body = %q, want %s", got, want)
	}
}

func TestExpressRouteStaysSetThroughMiddleware(t *testing.T) {
	rec, err := serveScript(t, `
		app.get('/x', function(req, res, next) { next() })
		app.use(function(req, res) { res.send('route ' + req.route.path) })
	`, "GET", "/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != "route /x" {
		t.Fatalf("body = %q, want route /x", got)
	}
}

func TestExpressFormatCallbackReceivesNext(t *testing.T) {
	rec, err := serveScript(t, `
		app.get('/x', function(req, res) {
			res.format({ 'text/plain': function(req, res, next) { next() } })
		})
		app.use(function(req, res) { res.send('after format') })
	`, "GET", "/x", nil, http.Header{"Accept": {"text/plain"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != "after format" {
		t.Fatalf("body = %q, want after format", got)
	}
}
