package vm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeModule(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHandleWithImport(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "lib.js", `export function label(method, path) { return method + " " + path }`)
	entry := writeModule(t, dir, "main.js", `import { label } from "./lib.js"
export default function handler(http) {
	http.respond(200, {"content-type": "text/plain"}, label(http.method, http.path))
}
`)
	v, err := New(Config{ScriptPath: entry, RootDir: dir, RequireHandler: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer v.Close()

	resp, err := v.Handle(context.Background(), entry, testRequest(http.MethodGet, "/hello", nil))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if resp == nil {
		t.Fatal("Handle returned no response")
	}
	if resp.Status != 200 {
		t.Errorf("status = %d", resp.Status)
	}
	if got := string(resp.Body); got != "GET /hello" {
		t.Errorf("body = %q", got)
	}
	if got := resp.Headers["content-type"]; got != "text/plain" {
		t.Errorf("content-type = %q", got)
	}
}

func TestHandleWithoutResponse(t *testing.T) {
	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", "export default function handler(http) {}\n")
	v, err := New(Config{ScriptPath: entry, RootDir: dir, RequireHandler: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer v.Close()

	resp, err := v.Handle(context.Background(), entry, testRequest(http.MethodGet, "/", nil))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if resp != nil {
		t.Fatalf("response = %+v, want nil", resp)
	}
}

func TestMissingDefaultHandler(t *testing.T) {
	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", "export default {}\n")
	if _, err := New(Config{ScriptPath: entry, RootDir: dir, RequireHandler: true}); err == nil {
		t.Fatal("New accepted a module without a default function")
	}
}

func TestBareImportRejected(t *testing.T) {
	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", `import "leftpad"`+"\n")
	if _, err := New(Config{ScriptPath: entry, RootDir: dir, RequireHandler: false}); err == nil {
		t.Fatal("New accepted a bare module specifier")
	}
}

func TestHandleTimeout(t *testing.T) {
	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", `export default function handler(http) {
	if (http.path === "/spin") { while (true) {} }
	http.respond(200, {"content-type": "text/plain"}, "ok")
}
`)
	v, err := New(Config{ScriptPath: entry, RootDir: dir, RequireHandler: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer v.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	_, err = v.Handle(ctx, entry, testRequest(http.MethodGet, "/spin", nil))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Handle error = %v, want deadline exceeded", err)
	}

	resp, err := v.Handle(context.Background(), entry, testRequest(http.MethodGet, "/ok", nil))
	if err != nil {
		t.Fatalf("Handle after timeout: %v", err)
	}
	if resp == nil || string(resp.Body) != "ok" {
		t.Fatalf("response after timeout = %+v", resp)
	}
}

func TestLoadSharesRealmAndModules(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "lib.js", `let n = 0
export function next() { return ++n }
`)
	entry := writeModule(t, dir, "site.js", `import { next } from "./lib.js"
globalThis.site = (globalThis.site ?? 0) + 1
export default function handler(http) {
	http.respond(200, {}, "site=" + globalThis.site + " lib=" + next())
}
`)
	api := writeModule(t, dir, "api.js", `import { next } from "./lib.js"
export default function handler(http) {
	http.respond(200, {}, "api=" + globalThis.site + " lib=" + next())
}
`)

	v, err := New(Config{ScriptPath: entry, RootDir: dir, RequireHandler: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer v.Close()

	if err := v.Load(context.Background(), api); err != nil {
		t.Fatalf("Load: %v", err)
	}

	resp, err := v.Handle(context.Background(), entry, testRequest(http.MethodGet, "/", nil))
	if err != nil {
		t.Fatalf("site Handle: %v", err)
	}
	if got := string(resp.Body); got != "site=1 lib=1" {
		t.Errorf("site body = %q", got)
	}

	resp, err = v.Handle(context.Background(), api, testRequest(http.MethodGet, "/api", nil))
	if err != nil {
		t.Fatalf("api Handle: %v", err)
	}
	if got := string(resp.Body); got != "api=1 lib=2" {
		t.Errorf("api body = %q", got)
	}
}

func TestProvisionDeadline(t *testing.T) {
	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", `while (true) {}
export default function handler(http) {}
`)
	start := time.Now()
	_, err := New(Config{
		ScriptPath:       entry,
		RootDir:          dir,
		RequireHandler:   true,
		ProvisionTimeout: 50 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("New accepted a module that never finishes provisioning")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("provision deadline took %s", elapsed)
	}
}

func TestTopLevelAwaitRejectionFailsProvisioning(t *testing.T) {
	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", `await Promise.reject(new Error("nope"))
export default function handler(http) {}
`)
	if _, err := New(Config{ScriptPath: entry, RootDir: dir, RequireHandler: true}); err == nil {
		t.Fatal("New accepted a module whose top-level await rejected")
	}
}

func testRequest(method, target string, body io.Reader) Request {
	return Request{Req: httptest.NewRequest(method, target, body)}
}

func TestHandleExposesRequest(t *testing.T) {
	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", `export default function handler(http) {
	const body = http.body.text()
	http.respond(200, {
		"x-query": http.query.q,
		"x-header": http.headers.get("x-test"),
	}, [
		http.method, http.url, http.path, http.query.q, http.query.multi,
		http.cookies.c, http.headers.get("x-test"), http.headers.has("x-test"),
		http.params.x ?? "-", http.host, http.remote, http.protocol, body,
	].join("|"))
}
`)
	v, err := New(Config{ScriptPath: entry, RootDir: dir, RequireHandler: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer v.Close()

	req := httptest.NewRequest(http.MethodPost, "/echo?q=z&multi=a&multi=b", strings.NewReader("hello"))
	req.Header.Add("x-test", "one")
	req.Header.Add("x-test", "two")
	req.AddCookie(&http.Cookie{Name: "c", Value: "1"})
	req.Host = "example.test"
	req.RemoteAddr = "203.0.113.5:1234"

	resp, err := v.Handle(context.Background(), entry, Request{Req: req})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	want := "POST|/echo?q=z&multi=a&multi=b|/echo|z|a,b|1|one,two|true|-|example.test|203.0.113.5:1234|HTTP/1.1|hello"
	if got := string(resp.Body); got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if got := resp.Headers["x-query"]; got != "z" {
		t.Errorf("x-query = %q", got)
	}
	if got := resp.Headers["x-header"]; got != "one, two" {
		t.Errorf("x-header = %q", got)
	}
}

func TestHandleMutatesRequest(t *testing.T) {
	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", `export default function handler(http) {
	http.headers.set("x-set", "yes")
	http.headers.delete("x-drop")
	http.vars.seen = "yes"
	http.vars.other = 2
	http.rewrite({ method: "PUT", path: "/new", headers: { "x-rewritten": "1" } })
	if (http.vars.seen !== "yes" || http.vars.other !== 2) throw new Error("vars did not round-trip")
}
`)
	v, err := New(Config{ScriptPath: entry, RootDir: dir, RequireHandler: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer v.Close()

	req := httptest.NewRequest(http.MethodGet, "/old?q=1", nil)
	req.Header.Set("x-drop", "gone")
	vars := map[string]any{"before": "kept"}

	resp, err := v.Handle(context.Background(), entry, Request{Req: req, Vars: vars})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if resp != nil {
		t.Fatalf("response = %+v, want nil", resp)
	}
	if req.Method != http.MethodPut {
		t.Errorf("method = %q", req.Method)
	}
	if req.URL.Path != "/new" || req.RequestURI != "/new?q=1" {
		t.Errorf("path = %q, request URI = %q", req.URL.Path, req.RequestURI)
	}
	if got := req.Header.Get("x-set"); got != "yes" {
		t.Errorf("x-set = %q", got)
	}
	if got := req.Header.Get("x-drop"); got != "" {
		t.Errorf("x-drop = %q", got)
	}
	if got := req.Header.Get("x-rewritten"); got != "1" {
		t.Errorf("x-rewritten = %q", got)
	}
	if got := vars["seen"]; got != "yes" {
		t.Errorf("vars[seen] = %v", got)
	}
	if got := vars["other"]; got != float64(2) && got != int64(2) && got != 2 {
		t.Errorf("vars[other] = %v (%T)", got, got)
	}
	if got := vars["before"]; got != "kept" {
		t.Errorf("vars[before] = %v", got)
	}
}

func TestHandleRouteChain(t *testing.T) {
	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", `route("/api/*", (http, next) => {
	http.headers.set("x-first", "1")
	next()
})
route("/api/*", (http, next) => {
	http.headers.set("x-second", "2")
	http.next()
})
route((http) => http.path === "/api/stop", (http) => {
	http.respond(200, {"content-type": "text/plain"}, "stopped")
})
export default function handler(http) {
	const first = http.headers.get("x-first") ?? "-"
	const second = http.headers.get("x-second") ?? "-"
	http.respond(200, {"content-type": "text/plain"}, [http.path, first, second].join("|"))
}
`)
	v, err := New(Config{ScriptPath: entry, RootDir: dir, RequireHandler: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer v.Close()

	resp, err := v.Handle(context.Background(), entry, testRequest(http.MethodGet, "/api/x", nil))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := string(resp.Body); got != "/api/x|1|2" {
		t.Errorf("body = %q", got)
	}

	resp, err = v.Handle(context.Background(), entry, testRequest(http.MethodGet, "/api/stop", nil))
	if err != nil {
		t.Fatalf("Handle stop: %v", err)
	}
	if got := string(resp.Body); got != "stopped" {
		t.Errorf("stop body = %q", got)
	}

	resp, err = v.Handle(context.Background(), entry, testRequest(http.MethodGet, "/other", nil))
	if err != nil {
		t.Fatalf("Handle other: %v", err)
	}
	if got := string(resp.Body); got != "/other|-|-" {
		t.Errorf("other body = %q", got)
	}
}

func TestRouteStopsBeforeEntry(t *testing.T) {
	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", `route("*", (http, next) => {})
export default function handler(http) {
	http.vars.entry = "ran"
	http.respond(200, {}, "entry")
}
`)
	v, err := New(Config{ScriptPath: entry, RootDir: dir, RequireHandler: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer v.Close()

	vars := map[string]any{}
	resp, err := v.Handle(context.Background(), entry, Request{Req: httptest.NewRequest(http.MethodGet, "/", nil), Vars: vars})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if resp != nil {
		t.Fatalf("response = %+v, want nil", resp)
	}
	if _, ok := vars["entry"]; ok {
		t.Error("entry ran after a layer stopped the chain")
	}
}

func TestRouteOutsideModuleScopeFails(t *testing.T) {
	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", `export default function handler(http) {
	route("*", () => {})
	http.respond(200, {}, "no")
}
`)
	v, err := New(Config{ScriptPath: entry, RootDir: dir, RequireHandler: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer v.Close()

	_, err = v.Handle(context.Background(), entry, testRequest(http.MethodGet, "/", nil))
	if err == nil || !strings.Contains(err.Error(), "module scope") {
		t.Fatalf("Handle error = %v, want a module-scope error", err)
	}
}

func TestBodyLimit(t *testing.T) {
	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", `export default function handler(http) {
	http.body.text()
	http.respond(200, {}, "no")
}
`)
	v, err := New(Config{ScriptPath: entry, RootDir: dir, RequireHandler: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer v.Close()

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("hello"))
	_, err = v.Handle(context.Background(), entry, Request{Req: req, MaxBody: 4})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("Handle error = %v, want a body limit error", err)
	}
}

func TestBodyJSON(t *testing.T) {
	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", `export default function handler(http) {
	const body = http.body.json()
	http.respond(200, {}, body.name + ":" + body.n)
}
`)
	v, err := New(Config{ScriptPath: entry, RootDir: dir, RequireHandler: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer v.Close()

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"caddy","n":7}`))
	resp, err := v.Handle(context.Background(), entry, Request{Req: req})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := string(resp.Body); got != "caddy:7" {
		t.Errorf("body = %q", got)
	}
}

func TestBodyBytes(t *testing.T) {
	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", `export default function handler(http) {
	http.respond(200, {}, String(http.body.bytes().length))
}
`)
	v, err := New(Config{ScriptPath: entry, RootDir: dir, RequireHandler: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer v.Close()

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("hello"))
	resp, err := v.Handle(context.Background(), entry, Request{Req: req})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := string(resp.Body); got != "5" {
		t.Errorf("body = %q", got)
	}
}

func TestMapValuedVars(t *testing.T) {
	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", `export default function handler(http) {
	const user = http.vars.user
	http.vars.copy = { name: user.name, admin: user.admin }
	http.respond(200, {}, "no")
}
`)
	v, err := New(Config{ScriptPath: entry, RootDir: dir, RequireHandler: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer v.Close()

	vars := map[string]any{
		"user": map[string]any{"name": "ada", "admin": true},
	}
	_, err = v.Handle(context.Background(), entry, Request{Req: httptest.NewRequest(http.MethodGet, "/", nil), Vars: vars})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	copied, ok := vars["copy"].(map[string]any)
	if !ok {
		t.Fatalf("vars[copy] = %T, want map[string]any", vars["copy"])
	}
	if copied["name"] != "ada" || copied["admin"] != true {
		t.Errorf("vars[copy] = %+v", copied)
	}
}

func TestVarsStayInTable(t *testing.T) {
	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", `export default function handler(http) {
	http.respond(200, {}, [
		http.vars.site ?? "missing",
		http.vars.explicit ?? "missing",
		site ?? "missing",
	].join("|"))
}
`)
	v, err := New(Config{ScriptPath: entry, RootDir: dir, RequireHandler: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer v.Close()

	lookup := func(name string) (any, bool) {
		if name == "site" {
			return "mapped", true
		}
		return nil, false
	}
	resp, err := v.Handle(context.Background(), entry, Request{
		Req:    httptest.NewRequest(http.MethodGet, "/", nil),
		Vars:   map[string]any{"explicit": "table"},
		Lookup: lookup,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := string(resp.Body); got != "missing|table|mapped" {
		t.Errorf("body = %q", got)
	}
}

func TestBareGlobals(t *testing.T) {
	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", `export default function handler(http) {
	const missing = typeof not_a_var
	const site = "local"
	http.respond(200, {}, [site, host, http.vars.explicit, missing].join("|"))
}
`)
	v, err := New(Config{ScriptPath: entry, RootDir: dir, RequireHandler: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer v.Close()

	lookup := func(name string) (any, bool) {
		if name == "host" {
			return "example.test", true
		}
		return nil, false
	}
	vars := map[string]any{"site": "mapped", "explicit": "table"}
	resp, err := v.Handle(context.Background(), entry, Request{
		Req:    httptest.NewRequest(http.MethodGet, "/", nil),
		Vars:   vars,
		Lookup: lookup,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := string(resp.Body); got != "local|example.test|table|undefined" {
		t.Errorf("body = %q", got)
	}
}

func TestBareGlobalsUnknownAtModuleScope(t *testing.T) {
	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", `const value = site
export default function handler(http) {
	http.respond(200, {}, String(value))
}
`)
	if _, err := New(Config{ScriptPath: entry, RootDir: dir, RequireHandler: true}); err == nil {
		t.Fatal("New accepted a placeholder read outside a dispatch")
	}
}

func TestBareGlobalsWriteBack(t *testing.T) {
	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", `export default function handler(http) {
	site = site + "!"
	fresh = 3
	http.respond(200, {}, site + "|" + fresh + "|" + (http.vars.fresh ?? "absent"))
}
`)
	v, err := New(Config{ScriptPath: entry, RootDir: dir, RequireHandler: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer v.Close()

	placeholders := map[string]any{"site": "mapped"}
	lookup := func(name string) (any, bool) {
		value, ok := placeholders[name]
		return value, ok
	}
	set := func(name string, value any) {
		placeholders[name] = value
	}
	vars := map[string]any{"from_caddy": "mapped"}
	resp, err := v.Handle(context.Background(), entry, Request{
		Req:    httptest.NewRequest(http.MethodGet, "/", nil),
		Vars:   vars,
		Lookup: lookup,
		Set:    set,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := string(resp.Body); got != "mapped!|3|absent" {
		t.Errorf("body = %q", got)
	}
	if got := placeholders["site"]; got != "mapped!" {
		t.Errorf("placeholder site = %v", got)
	}
	if got := placeholders["fresh"]; got != float64(3) && got != int64(3) && got != 3 {
		t.Errorf("placeholder fresh = %v (%T)", got, got)
	}
	if _, ok := vars["site"]; ok {
		t.Error("a bare write landed in the vars table")
	}
	if _, ok := vars["fresh"]; ok {
		t.Error("a bare write landed in the vars table")
	}
	if got := vars["from_caddy"]; got != "mapped" {
		t.Errorf("vars[from_caddy] = %v", got)
	}
}

func TestBareGlobalsRejectFunctions(t *testing.T) {
	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", `export default function handler(http) {
	let result = "accepted"
	try {
		site = () => 1
	} catch (e) {
		result = "rejected"
	}
	http.respond(200, {}, result)
}
`)
	v, err := New(Config{ScriptPath: entry, RootDir: dir, RequireHandler: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer v.Close()

	lookup := func(name string) (any, bool) {
		if name == "site" {
			return "mapped", true
		}
		return nil, false
	}
	resp, err := v.Handle(context.Background(), entry, Request{
		Req:    httptest.NewRequest(http.MethodGet, "/", nil),
		Lookup: lookup,
		Set:    func(string, any) {},
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := string(resp.Body); got != "rejected" {
		t.Errorf("body = %q", got)
	}
}
