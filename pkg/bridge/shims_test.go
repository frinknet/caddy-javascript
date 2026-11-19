package bridge

import "testing"

func TestBufferShim(t *testing.T) {
	p, _ := newModuleProgram(t)
	if err := p.Load([]byte(`
		var b = require("buffer").Buffer.from("hello");
		var out = b.toString();
		var hex = require("buffer").Buffer.from("6869", "hex").toString();
		var isBuf = require("buffer").Buffer.isBuffer(b);
		var len = b.length;
	`)); err != nil {
		t.Fatal(err)
	}
	if got := evalString(t, p, "out"); got != "hello" {
		t.Fatalf("buffer toString = %q", got)
	}
	if got := evalString(t, p, "hex"); got != "hi" {
		t.Fatalf("buffer hex = %q", got)
	}
	if got := evalString(t, p, "isBuf"); got != "true" {
		t.Fatalf("isBuffer = %q", got)
	}
	if got := evalString(t, p, "len"); got != "5" {
		t.Fatalf("length = %q", got)
	}
}

func TestEventsShim(t *testing.T) {
	p, _ := newModuleProgram(t)
	if err := p.Load([]byte(`
		var EventEmitter = require("events");
		var e = new EventEmitter();
		var seen = "";
		e.on("ping", function(v) { seen += v; });
		e.once("once", function() { seen += "!"; });
		e.emit("ping", "a");
		e.emit("ping", "b");
		e.emit("once");
		e.emit("once");
	`)); err != nil {
		t.Fatal(err)
	}
	if got := evalString(t, p, "seen"); got != "ab!" {
		t.Fatalf("events = %q", got)
	}
}

func TestURLShim(t *testing.T) {
	p, _ := newModuleProgram(t)
	if err := p.Load([]byte(`
		var url = require("url");
		var u = url.parse("http://example.com/a?b=c");
		var host = u.hostname;
		var query = u.query;
		var built = url.resolve("http://example.com/a/", "b");
	`)); err != nil {
		t.Fatal(err)
	}
	if got := evalString(t, p, "host"); got != "example.com" {
		t.Fatalf("hostname = %q", got)
	}
	if got := evalString(t, p, "query"); got != "b=c" {
		t.Fatalf("query = %q", got)
	}
	if got := evalString(t, p, "built"); got != "http://example.com/a/b" {
		t.Fatalf("resolve = %q", got)
	}
}

func TestQuerystringShim(t *testing.T) {
	p, _ := newModuleProgram(t)
	if err := p.Load([]byte(`
		var qs = require("querystring");
		var parsed = qs.parse("a=1&b=2");
		var a = parsed.a;
		var b = parsed.b;
		var out = qs.stringify({ x: "1", y: "2" });
	`)); err != nil {
		t.Fatal(err)
	}
	if got := evalString(t, p, "a"); got != "1" {
		t.Fatalf("a = %q", got)
	}
	if got := evalString(t, p, "b"); got != "2" {
		t.Fatalf("b = %q", got)
	}
	if got := evalString(t, p, "out"); got != "x=1&y=2" {
		t.Fatalf("stringify = %q", got)
	}
}

func TestUtilShim(t *testing.T) {
	p, _ := newModuleProgram(t)
	if err := p.Load([]byte(`
		var util = require("util");
		var kind = typeof util.inherits;
		var msg = util.format("%s-%d", "x", 2);
	`)); err != nil {
		t.Fatal(err)
	}
	if got := evalString(t, p, "kind"); got != "function" {
		t.Fatalf("inherits type = %q", got)
	}
	if got := evalString(t, p, "msg"); got != "x-2" {
		t.Fatalf("format = %q", got)
	}
}
