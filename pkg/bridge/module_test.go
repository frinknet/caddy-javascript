package bridge

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/frinknet/caddy-javascript/pkg/files"
)

func newModuleProgram(t *testing.T) (*Program, string) {
	t.Helper()
	dir := t.TempDir()
	fsys, err := files.NewFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fsys.Close() })
	return NewProgram(Config{Quit: make(chan struct{})}, fsys, nil, nil), dir
}

func writeModule(t *testing.T, dir, name, content string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func evalString(t *testing.T, p *Program, expr string) string {
	t.Helper()
	v, err := p.rt.RunString(expr)
	if err != nil {
		t.Fatal(err)
	}
	return v.String()
}

func TestRequireRelativeModule(t *testing.T) {
	p, dir := newModuleProgram(t)
	writeModule(t, dir, "lib/helper.js", `
		module.exports = { greet: function(n) { return 'hi ' + n } }
	`)
	if err := p.Load([]byte(`
		var h = require('./lib/helper')
		var out = h.greet('x')
	`)); err != nil {
		t.Fatal(err)
	}
	if got := evalString(t, p, "out"); got != "hi x" {
		t.Fatalf("out = %q, want hi x", got)
	}
}

func TestRequireExtensionlessAndIndex(t *testing.T) {
	p, dir := newModuleProgram(t)
	writeModule(t, dir, "lib/plain.js", `module.exports = 'plain'`)
	writeModule(t, dir, "lib/pkg/index.js", `module.exports = 'index'`)
	if err := p.Load([]byte(`
		var a = require('./lib/plain')
		var b = require('./lib/pkg')
		var out = a + ':' + b
	`)); err != nil {
		t.Fatal(err)
	}
	if got := evalString(t, p, "out"); got != "plain:index" {
		t.Fatalf("out = %q, want plain:index", got)
	}
}

func TestRequireRelativeFromNestedModule(t *testing.T) {
	p, dir := newModuleProgram(t)
	writeModule(t, dir, "lib/deep/leaf.js", `module.exports = 'leaf'`)
	writeModule(t, dir, "lib/mid.js", `
		var leaf = require('./deep/leaf')
		module.exports = 'mid->' + leaf
	`)
	if err := p.Load([]byte(`
		var out = require('./lib/mid')
	`)); err != nil {
		t.Fatal(err)
	}
	if got := evalString(t, p, "out"); got != "mid->leaf" {
		t.Fatalf("out = %q, want mid->leaf", got)
	}
}

func TestRequireNodeModules(t *testing.T) {
	p, dir := newModuleProgram(t)
	writeModule(t, dir, "node_modules/leftpad/index.js", `
		module.exports = function(s) { return '0' + s }
	`)
	if err := p.Load([]byte(`
		var lp = require('leftpad')
		var out = lp('5')
	`)); err != nil {
		t.Fatal(err)
	}
	if got := evalString(t, p, "out"); got != "05" {
		t.Fatalf("out = %q, want 05", got)
	}
}

func TestRequireNodeModulesWalksUp(t *testing.T) {
	p, dir := newModuleProgram(t)
	writeModule(t, dir, "node_modules/top/index.js", `module.exports = 'top'`)
	writeModule(t, dir, "app/nested/entry.js", `
		module.exports = require('top')
	`)
	if err := p.Load([]byte(`
		var out = require('./app/nested/entry')
	`)); err != nil {
		t.Fatal(err)
	}
	if got := evalString(t, p, "out"); got != "top" {
		t.Fatalf("out = %q, want top", got)
	}
}

func TestRequirePackageMain(t *testing.T) {
	p, dir := newModuleProgram(t)
	writeModule(t, dir, "node_modules/pkg/package.json", `{"main": "src/entry.js"}`)
	writeModule(t, dir, "node_modules/pkg/src/entry.js", `module.exports = 'from-main'`)
	if err := p.Load([]byte(`
		var out = require('pkg')
	`)); err != nil {
		t.Fatal(err)
	}
	if got := evalString(t, p, "out"); got != "from-main" {
		t.Fatalf("out = %q, want from-main", got)
	}
}

func TestRequireJSONModule(t *testing.T) {
	p, dir := newModuleProgram(t)
	writeModule(t, dir, "config.json", `{"port": 8080, "name": "app"}`)
	if err := p.Load([]byte(`
		var cfg = require('./config.json')
		var out = cfg.name + ':' + cfg.port
	`)); err != nil {
		t.Fatal(err)
	}
	if got := evalString(t, p, "out"); got != "app:8080" {
		t.Fatalf("out = %q, want app:8080", got)
	}
}

func TestRequireModuleCache(t *testing.T) {
	p, dir := newModuleProgram(t)
	writeModule(t, dir, "cached.js", `
		__loads = __loads + 1
		module.exports = { id: __loads }
	`)
	if err := p.rt.Set("__loads", 0); err != nil {
		t.Fatal(err)
	}
	if err := p.Load([]byte(`
		var a = require('./cached')
		var b = require('./cached')
		var same = (a === b)
		var out = a.id + ':' + same
	`)); err != nil {
		t.Fatal(err)
	}
	if got := evalString(t, p, "out"); got != "1:true" {
		t.Fatalf("out = %q, want 1:true", got)
	}
}

func TestRequireCircularDependencies(t *testing.T) {
	p, dir := newModuleProgram(t)
	writeModule(t, dir, "a.js", `
		exports.name = 'a'
		var b = require('./b')
		exports.fromB = b.name
	`)
	writeModule(t, dir, "b.js", `
		exports.name = 'b'
		var a = require('./a')
		exports.seenA = a.name
	`)
	if err := p.Load([]byte(`
		var a = require('./a')
		var b = require('./b')
		var out = a.name + ':' + a.fromB + ':' + b.seenA
	`)); err != nil {
		t.Fatal(err)
	}
	if got := evalString(t, p, "out"); got != "a:b:a" {
		t.Fatalf("out = %q, want a:b:a", got)
	}
}

func TestRequireModuleSeesDirnameAndFilename(t *testing.T) {
	p, dir := newModuleProgram(t)
	writeModule(t, dir, "lib/whoami.js", `module.exports = __dirname + '|' + __filename`)
	if err := p.Load([]byte(`
		var out = require('./lib/whoami')
	`)); err != nil {
		t.Fatal(err)
	}
	if got := evalString(t, p, "out"); got != "lib|lib/whoami.js" {
		t.Fatalf("out = %q, want lib|lib/whoami.js", got)
	}
}

func TestRequireMissingModuleThrows(t *testing.T) {
	p, _ := newModuleProgram(t)
	if err := p.Load([]byte(`require('./does-not-exist')`)); err == nil {
		t.Fatal("expected require of a missing module to throw")
	}
}

func TestRequireBuiltins(t *testing.T) {
	p, _ := newModuleProgram(t)
	if err := p.Load([]byte(`
		var fs = require('fs')
		var p1 = require('node:path')
		var express = require('express')
		var out = typeof fs.readFileSync + ':' + typeof p1.join + ':' + (express() === app)
	`)); err != nil {
		t.Fatal(err)
	}
	if got := evalString(t, p, "out"); got != "function:function:true" {
		t.Fatalf("out = %q, want function:function:true", got)
	}
}

func TestRequireInsideHandler(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "lib/msg.js", `module.exports = 'from module'`)
	rec, err := serveScriptIn(t, dir, `
		app.get('/m', function(req, res) {
			res.send(require('./lib/msg'))
		})
	`, "GET", "/m", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != "from module" {
		t.Fatalf("body = %q, want from module", got)
	}
}

func TestRequireReloadResetsCache(t *testing.T) {
	dir := t.TempDir()
	fsys, err := files.NewFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fsys.Close() })

	writeModule(t, dir, "lib/value.js", `module.exports = 'one'`)
	first := NewProgram(Config{Quit: make(chan struct{})}, fsys, nil, nil)
	if err := first.Load([]byte(`var out = require('./lib/value')`)); err != nil {
		t.Fatal(err)
	}
	if got := evalString(t, first, "out"); got != "one" {
		t.Fatalf("out = %q, want one", got)
	}

	writeModule(t, dir, "lib/value.js", `module.exports = 'two'`)
	second := NewProgram(Config{Quit: make(chan struct{})}, fsys, nil, nil)
	if err := second.Load([]byte(`var out = require('./lib/value')`)); err != nil {
		t.Fatal(err)
	}
	if got := evalString(t, second, "out"); got != "two" {
		t.Fatalf("out = %q, want two after reload", got)
	}
}
