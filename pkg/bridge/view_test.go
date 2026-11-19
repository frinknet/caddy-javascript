package bridge

import (
	"net/http"
	"strings"
	"testing"
)

func TestExpressRenderMergesLocalsAndSettings(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "views/hello.html", "Hello {{name}} from {{title}}")

	rec, err := serveScriptIn(t, dir, `
		const express = require('express')
		const fs = require('fs')
		app.engine('html', function(file, opts, cb) {
			fs.readFile(file, function(err, text) {
				if (err) return cb(err)
				cb(null, text.replace('{{name}}', opts.name).replace('{{title}}', opts.title))
			})
		})
		app.set('view engine', 'html')
		app.locals.title = 'app'
		app.get('/view', function(req, res) {
			res.locals.name = 'res'
			res.render('hello', { name: 'options' })
		})
	`, "GET", "/view", &fakeScheduler{})
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != "Hello options from app" {
		t.Fatalf("body = %q", got)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type = %q", ct)
	}
}

func TestExpressRenderCallbackLeavesSendingToCaller(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "views/plain.html", "hello {{name}}")

	rec, err := serveScriptIn(t, dir, `
		const express = require('express')
		const fs = require('fs')
		app.engine('.html', function(file, opts, cb) {
			fs.readFile(file, function(err, text) {
				if (err) return cb(err)
				cb(null, text.replace('{{name}}', opts.name))
			})
		})
		app.set('view engine', '.html')
		app.get('/cb', function(req, res) {
			res.render('plain', { name: 'callback' }, function(err, html) {
				if (err) return res.status(500).send(err.message)
				res.type('text/plain').send('rendered:' + html)
			})
		})
	`, "GET", "/cb", &fakeScheduler{})
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != "rendered:hello callback" {
		t.Fatalf("body = %q", got)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type = %q", ct)
	}
}

func TestExpressRenderMissingViewReachesErrorHandler(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "views/hello.html", "hi")

	rec, err := serveScriptIn(t, dir, `
		const express = require('express')
		app.engine('html', function(file, opts, cb) { cb(null, 'hi') })
		app.set('view engine', 'html')
		app.get('/missing', function(req, res) { res.render('missing') })
		app.use(function(err, req, res, next) {
			res.status(500).send('caught ' + err.message)
		})
	`, "GET", "/missing", nil)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if got := rec.Body.String(); !strings.HasPrefix(got, "caught Failed to lookup view") {
		t.Fatalf("body = %q", got)
	}
}

func TestExpressRenderViewsArrayAndAppRender(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "views/item.html", "item {{id}}")

	rec, err := serveScriptIn(t, dir, `
		const express = require('express')
		const fs = require('fs')
		app.engine('html', function(file, opts, cb) {
			fs.readFile(file, function(err, text) {
				if (err) return cb(err)
				cb(null, text.replace('{{id}}', opts.id))
			})
		})
		app.set('views', ['missing', 'views'])
		app.set('view engine', 'html')
		app.get('/app', function(req, res) {
			app.render('item', { id: 7 }, function(err, html) {
				if (err) return res.status(500).send(err.message)
				res.send(html)
			})
		})
	`, "GET", "/app", &fakeScheduler{})
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != "item 7" {
		t.Fatalf("body = %q", got)
	}
}

func TestExpressRenderRequiresRegisteredEngine(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "views/hello.unknown", "hi")

	rec, err := serveScriptIn(t, dir, `
		const express = require('express')
		app.set('view engine', 'unknown')
		app.get('/view', function(req, res) { res.render('hello') })
		app.use(function(err, req, res, next) {
			res.status(500).send(err.message)
		})
	`, "GET", "/view", &fakeScheduler{})
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); !strings.Contains(got, `No engine was registered for the extension ".unknown"`) {
		t.Fatalf("body = %q", got)
	}
}

func TestExpressRenderRequiresEngineModule(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "views/hello.tiny", "tiny {{value}}")
	writeModule(t, dir, "node_modules/tiny/index.js", `
		const fs = require('fs')
		exports.renderFile = function(file, opts, cb) {
			fs.readFile(file, function(err, text) {
				if (err) return cb(err)
				cb(null, text.replace('{{value}}', opts.value))
			})
		}
	`)

	rec, err := serveScriptIn(t, dir, `
		const express = require('express')
		app.set('view engine', 'tiny')
		app.get('/view', function(req, res) { res.render('hello', { value: 'module' }) })
	`, "GET", "/view", &fakeScheduler{})
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != "tiny module" {
		t.Fatalf("body = %q", got)
	}
}
