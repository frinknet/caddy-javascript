package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"

	"github.com/frinknet/caddy-javascript/pkg/config"
)

const respondScript = `export default function handler(http) {
	http.respond(200, {"content-type": "text/plain"}, http.method + " " + http.path)
}
`

func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestServeHTTPResponds(t *testing.T) {
	dir := t.TempDir()
	path := writeScript(t, dir, "main.js", respondScript)
	h := &Handler{Options: config.Options{ScriptPath: path}}
	if err := h.Provision(caddy.Context{}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	defer h.Cleanup()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	err := h.ServeHTTP(rec, req, caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error {
		t.Error("next handler was called")
		return nil
	}))
	if err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "GET /x" {
		t.Errorf("body = %q", got)
	}
}

func TestServeHTTPContinuesWhenNoResponse(t *testing.T) {
	dir := t.TempDir()
	path := writeScript(t, dir, "main.js", "export default function handler(http) {}\n")
	h := &Handler{Options: config.Options{ScriptPath: path}}
	if err := h.Provision(caddy.Context{}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	defer h.Cleanup()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	called := false
	err := h.ServeHTTP(rec, req, caddyhttp.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) error {
		called = true
		w.WriteHeader(http.StatusNoContent)
		return nil
	}))
	if err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if !called {
		t.Error("next handler was not called")
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rec.Code)
	}
}

func TestServeHTTPTimeout(t *testing.T) {
	dir := t.TempDir()
	path := writeScript(t, dir, "main.js", "export default function handler(http) { while (true) {} }\n")
	h := &Handler{Options: config.Options{
		ScriptPath: path,
		Timeout:    caddy.Duration(25 * time.Millisecond),
	}}
	if err := h.Provision(caddy.Context{}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	defer h.Cleanup()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	err := h.ServeHTTP(rec, req, caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error {
		return nil
	}))
	if err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if rec.Code != http.StatusGatewayTimeout {
		t.Errorf("status = %d, want 504", rec.Code)
	}
}

func TestProvisionChecksScript(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.js")
	h := &Handler{Options: config.Options{ScriptPath: missing}}
	if err := h.Provision(caddy.Context{}); err == nil {
		t.Fatal("Provision succeeded for a missing script")
	}

	path := writeScript(t, dir, "main.js", respondScript)
	h = &Handler{Options: config.Options{ScriptPath: path}}
	if err := h.Provision(caddy.Context{}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if err := h.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
}

func TestProvisionRejectsDirectory(t *testing.T) {
	h := &Handler{Options: config.Options{ScriptPath: t.TempDir()}}
	if err := h.Provision(caddy.Context{}); err == nil {
		t.Fatal("Provision succeeded for a directory")
	}
}

func TestParseGlobal(t *testing.T) {
	d := caddyfile.NewTestDispenser(`javascript /srv/global.js {
		timeout 1s
	}`)
	v, err := parseGlobal(d, nil)
	if err != nil {
		t.Fatalf("parseGlobal: %v", err)
	}
	app, ok := v.(httpcaddyfile.App)
	if !ok {
		t.Fatalf("parseGlobal returned %T", v)
	}
	if app.Name != "javascript" {
		t.Errorf("app name = %q", app.Name)
	}
	if len(app.Value) == 0 {
		t.Error("app value is empty")
	}

	d = caddyfile.NewTestDispenser(`javascript /srv/global.js`)
	if _, err := parseGlobal(d, app); err == nil {
		t.Fatal("parseGlobal accepted a duplicate option")
	}
}

var javaScriptHandlerRaw = json.RawMessage(`{"handler":"javascript"}`)

func hostMatchers(hosts ...string) caddyhttp.MatcherSets {
	return caddyhttp.MatcherSets{{caddyhttp.MatchHost(hosts)}}
}

func serverContext(srv *caddyhttp.Server) caddy.Context {
	return caddy.Context{Context: context.WithValue(context.Background(), caddyhttp.ServerCtxKey, srv)}
}

func responseBody(t *testing.T, h *Handler) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if err := h.ServeHTTP(rec, req, caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error {
		return nil
	})); err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	return rec.Body.String()
}

func TestSiteHandlersShareOneVM(t *testing.T) {
	dir := t.TempDir()
	writeScript(t, dir, "lib.js", "let n = 0\nexport function next() { return ++n }\n")
	site := writeScript(t, dir, "site.js", `import { next } from "./lib.js"
globalThis.site = (globalThis.site ?? 0) + 1
export default function handler(http) {
	http.respond(200, {}, "site=" + globalThis.site + " lib=" + next())
}
`)
	api := writeScript(t, dir, "api.js", `import { next } from "./lib.js"
export default function handler(http) {
	http.respond(200, {}, "api=" + globalThis.site + " lib=" + next())
}
`)

	srv := &caddyhttp.Server{Routes: caddyhttp.RouteList{
		{
			MatcherSets: hostMatchers("example.com"),
			HandlersRaw: []json.RawMessage{javaScriptHandlerRaw},
		},
		{
			MatcherSets: caddyhttp.MatcherSets{{
				caddyhttp.MatchHost{"example.com"},
				caddyhttp.MatchPath{"/api/*"},
			}},
			HandlersRaw: []json.RawMessage{javaScriptHandlerRaw},
		},
	}}
	ctx := serverContext(srv)

	first := &Handler{Options: config.Options{ScriptPath: site}}
	if err := first.Provision(ctx); err != nil {
		t.Fatalf("Provision site: %v", err)
	}
	defer first.Cleanup()
	srv.Routes[0].Handlers = []caddyhttp.MiddlewareHandler{first}
	srv.Routes[0].HandlersRaw = nil

	second := &Handler{Options: config.Options{ScriptPath: api}}
	if err := second.Provision(ctx); err != nil {
		t.Fatalf("Provision api: %v", err)
	}
	defer second.Cleanup()

	if first.vm != second.vm {
		t.Fatal("site and handle directives own different VMs")
	}
	if got := responseBody(t, first); got != "site=1 lib=1" {
		t.Errorf("site body = %q", got)
	}
	if got := responseBody(t, second); got != "api=1 lib=2" {
		t.Errorf("api body = %q", got)
	}

	if err := second.Cleanup(); err != nil {
		t.Fatalf("cleanup api: %v", err)
	}
	if got := responseBody(t, first); got != "site=1 lib=3" {
		t.Errorf("site body after handle cleanup = %q", got)
	}
}

func TestDifferentHostsGetDifferentVMs(t *testing.T) {
	dir := t.TempDir()
	firstScript := writeScript(t, dir, "first.js", respondScript)
	secondScript := writeScript(t, dir, "second.js", respondScript)

	srv := &caddyhttp.Server{Routes: caddyhttp.RouteList{
		{MatcherSets: hostMatchers("one.example"), HandlersRaw: []json.RawMessage{javaScriptHandlerRaw}},
		{MatcherSets: hostMatchers("two.example"), HandlersRaw: []json.RawMessage{javaScriptHandlerRaw}},
	}}
	ctx := serverContext(srv)

	first := &Handler{Options: config.Options{ScriptPath: firstScript}}
	if err := first.Provision(ctx); err != nil {
		t.Fatalf("Provision first: %v", err)
	}
	defer first.Cleanup()
	srv.Routes[0].Handlers = []caddyhttp.MiddlewareHandler{first}
	srv.Routes[0].HandlersRaw = nil

	second := &Handler{Options: config.Options{ScriptPath: secondScript}}
	if err := second.Provision(ctx); err != nil {
		t.Fatalf("Provision second: %v", err)
	}
	defer second.Cleanup()

	if first.vm == second.vm {
		t.Fatal("handlers on different hosts share one VM")
	}
}

func TestSiteOptionSharesVMs(t *testing.T) {
	dir := t.TempDir()
	firstScript := writeScript(t, dir, "first.js", respondScript)
	secondScript := writeScript(t, dir, "second.js", respondScript)

	srv := &caddyhttp.Server{Routes: caddyhttp.RouteList{
		{MatcherSets: hostMatchers("one.example"), HandlersRaw: []json.RawMessage{javaScriptHandlerRaw}},
		{MatcherSets: hostMatchers("two.example"), HandlersRaw: []json.RawMessage{javaScriptHandlerRaw}},
	}}
	ctx := serverContext(srv)

	first := &Handler{Options: config.Options{ScriptPath: firstScript, Site: "shared"}}
	if err := first.Provision(ctx); err != nil {
		t.Fatalf("Provision first: %v", err)
	}
	defer first.Cleanup()
	srv.Routes[0].Handlers = []caddyhttp.MiddlewareHandler{first}
	srv.Routes[0].HandlersRaw = nil

	second := &Handler{Options: config.Options{ScriptPath: secondScript, Site: "shared"}}
	if err := second.Provision(ctx); err != nil {
		t.Fatalf("Provision second: %v", err)
	}
	defer second.Cleanup()

	if first.vm != second.vm {
		t.Fatal("site override did not share the VM")
	}
}

func TestSiteVMReusedWhenTheScriptChanges(t *testing.T) {
	dir := t.TempDir()
	firstScript := writeScript(t, dir, "first.js", respondScript)
	secondScript := writeScript(t, dir, "second.js", respondScript)

	newServer := func(script string) *caddyhttp.Server {
		return &caddyhttp.Server{
			Listen: []string{":80"},
			Routes: caddyhttp.RouteList{{
				MatcherSetsRaw: caddyhttp.RawMatcherSets{caddy.ModuleMap{
					"host": json.RawMessage(`["example.com"]`),
				}},
				HandlersRaw: []json.RawMessage{javascriptRouteRaw(t, script)},
			}},
		}
	}

	first := &Handler{Options: config.Options{ScriptPath: firstScript}}
	if err := first.Provision(serverContext(newServer(firstScript))); err != nil {
		t.Fatalf("Provision first: %v", err)
	}
	defer first.Cleanup()

	second := &Handler{Options: config.Options{ScriptPath: secondScript}}
	if err := second.Provision(serverContext(newServer(secondScript))); err != nil {
		t.Fatalf("Provision second: %v", err)
	}
	defer second.Cleanup()

	if first.vm != second.vm {
		t.Fatal("a changed script replaced the VM")
	}
}

func TestNestedHandlerJoinsEnclosingSite(t *testing.T) {
	dir := t.TempDir()
	site := writeScript(t, dir, "site.js", respondScript)
	api := writeScript(t, dir, "api.js", respondScript)

	subrouteRaw := json.RawMessage(`{"handler":"subroute","routes":[{"handle":[` + string(javaScriptHandlerRaw) + `]}]}`)
	srv := &caddyhttp.Server{Routes: caddyhttp.RouteList{
		{MatcherSets: hostMatchers("example.com"), HandlersRaw: []json.RawMessage{subrouteRaw}},
		{MatcherSets: hostMatchers("example.com"), HandlersRaw: []json.RawMessage{javaScriptHandlerRaw}},
	}}
	ctx := serverContext(srv)

	nested := &Handler{Options: config.Options{ScriptPath: api}}
	if err := nested.Provision(ctx); err != nil {
		t.Fatalf("Provision nested: %v", err)
	}
	defer nested.Cleanup()
	srv.Routes[0].Handlers = []caddyhttp.MiddlewareHandler{
		&caddyhttp.Subroute{Routes: caddyhttp.RouteList{{Handlers: []caddyhttp.MiddlewareHandler{nested}}}},
	}
	srv.Routes[0].HandlersRaw = nil

	top := &Handler{Options: config.Options{ScriptPath: site}}
	if err := top.Provision(ctx); err != nil {
		t.Fatalf("Provision top: %v", err)
	}
	defer top.Cleanup()

	if nested.vm != top.vm {
		t.Fatal("nested handler did not join the enclosing site VM")
	}
}

func TestServeHTTPSharesVarsAndRewriteWithNextHandler(t *testing.T) {
	dir := t.TempDir()
	path := writeScript(t, dir, "main.js", `route("/down/*", (http) => {
	http.vars.layer = "ran"
})
export default function handler(http) {
	http.vars.seen = "yes"
	http.rewrite({ method: "PUT", path: "/down/changed", headers: { "x-rewritten": "1" } })
}
`)
	h := &Handler{Options: config.Options{ScriptPath: path}}
	if err := h.Provision(caddy.Context{}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	defer h.Cleanup()

	vars := make(map[string]any)
	req := httptest.NewRequest(http.MethodGet, "/down/original", nil)
	req = req.WithContext(context.WithValue(req.Context(), caddyhttp.VarsCtxKey, vars))

	rec := httptest.NewRecorder()
	called := false
	err := h.ServeHTTP(rec, req, caddyhttp.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		called = true
		if r.Method != http.MethodPut {
			t.Errorf("method = %q, want PUT", r.Method)
		}
		if r.URL.Path != "/down/changed" {
			t.Errorf("path = %q, want /down/changed", r.URL.Path)
		}
		if got := r.Header.Get("x-rewritten"); got != "1" {
			t.Errorf("x-rewritten = %q", got)
		}
		if got := vars["seen"]; got != "yes" {
			t.Errorf("vars[seen] = %v", got)
		}
		if got := vars["layer"]; got != "ran" {
			t.Errorf("vars[layer] = %v", got)
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	}))
	if err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if !called {
		t.Fatal("next handler was not called")
	}
}

func TestServeHTTPVarsDoNotSeePlaceholders(t *testing.T) {
	dir := t.TempDir()
	path := writeScript(t, dir, "main.js", `export default function handler(http) {
	http.respond(200, {"content-type": "text/plain"}, (http.vars.site ?? "missing") + "|" + site)
}
`)
	h := &Handler{Options: config.Options{ScriptPath: path}}
	if err := h.Provision(caddy.Context{}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	defer h.Cleanup()

	repl := caddy.NewReplacer()
	repl.Map(func(key string) (any, bool) {
		if key == "site" {
			return "mapped", true
		}
		return nil, false
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(context.WithValue(req.Context(), caddy.ReplacerCtxKey, repl))

	rec := httptest.NewRecorder()
	err := h.ServeHTTP(rec, req, caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error {
		return nil
	}))
	if err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if got := rec.Body.String(); got != "missing|mapped" {
		t.Errorf("body = %q", got)
	}
}

func TestServeHTTPBarePlaceholders(t *testing.T) {
	dir := t.TempDir()
	path := writeScript(t, dir, "main.js", `export default function handler(http) {
	http.respond(200, {"content-type": "text/plain"}, site + "|" + host)
}
`)
	h := &Handler{Options: config.Options{ScriptPath: path}}
	if err := h.Provision(caddy.Context{}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	defer h.Cleanup()

	repl := caddy.NewReplacer()
	repl.Map(func(key string) (any, bool) {
		if key == "site" {
			return "mapped", true
		}
		return nil, false
	})
	repl.Set("http.request.host", "example.test")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(context.WithValue(req.Context(), caddy.ReplacerCtxKey, repl))

	rec := httptest.NewRecorder()
	err := h.ServeHTTP(rec, req, caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error {
		return nil
	}))
	if err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if got := rec.Body.String(); got != "mapped|example.test" {
		t.Errorf("body = %q", got)
	}
}

func TestServeHTTPBarePlaceholderWriteBack(t *testing.T) {
	dir := t.TempDir()
	path := writeScript(t, dir, "main.js", `export default function handler(http) {
	site = "changed"
	fresh = "new"
}
`)
	h := &Handler{Options: config.Options{ScriptPath: path}}
	if err := h.Provision(caddy.Context{}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	defer h.Cleanup()

	repl := caddy.NewReplacer()
	repl.Map(func(key string) (any, bool) {
		if key == "site" {
			return "mapped", true
		}
		return nil, false
	})
	vars := make(map[string]any)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(context.WithValue(req.Context(), caddy.ReplacerCtxKey, repl))
	req = req.WithContext(context.WithValue(req.Context(), caddyhttp.VarsCtxKey, vars))

	rec := httptest.NewRecorder()
	err := h.ServeHTTP(rec, req, caddyhttp.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) error {
		if _, ok := vars["site"]; ok {
			t.Error("a bare write landed in the vars table")
		}
		if _, ok := vars["fresh"]; ok {
			t.Error("a bare write landed in the vars table")
		}
		repl := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer)
		if value, ok := repl.Get("site"); !ok || value != "changed" {
			t.Errorf("replacer site = %v, %v", value, ok)
		}
		if value, ok := repl.Get("fresh"); !ok || value != "new" {
			t.Errorf("replacer fresh = %v, %v", value, ok)
		}
		return nil
	}))
	if err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
}

func TestServeHTTPWritesResponse(t *testing.T) {
	dir := t.TempDir()
	path := writeScript(t, dir, "main.js", `export default function handler(http) {
	http.respond(201, {"content-type": "text/plain", "x-extra": "1"}, "created")
}
`)
	h := &Handler{Options: config.Options{ScriptPath: path}}
	if err := h.Provision(caddy.Context{}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	defer h.Cleanup()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	err := h.ServeHTTP(rec, req, caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error {
		t.Error("next handler was called")
		return nil
	}))
	if err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want 201", rec.Code)
	}
	if got := rec.Header().Get("content-type"); got != "text/plain" {
		t.Errorf("content-type = %q", got)
	}
	if got := rec.Header().Get("x-extra"); got != "1" {
		t.Errorf("x-extra = %q", got)
	}
	if got := rec.Body.String(); got != "created" {
		t.Errorf("body = %q", got)
	}
}

func TestServeHTTPClientCancellation(t *testing.T) {
	dir := t.TempDir()
	path := writeScript(t, dir, "main.js", "export default function handler(http) { while (true) {} }\n")
	h := &Handler{Options: config.Options{ScriptPath: path}}
	if err := h.Provision(caddy.Context{}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	defer h.Cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	timer := time.AfterFunc(25*time.Millisecond, cancel)
	defer timer.Stop()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	err := h.ServeHTTP(rec, req, caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error {
		return nil
	}))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ServeHTTP error = %v, want context.Canceled", err)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want no error page", rec.Body.String())
	}
}

func javascriptRouteRaw(t *testing.T, script string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"handler":     "javascript",
		"script_path": script,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func subrouteRaw(t *testing.T, routes ...any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"handler": "subroute",
		"routes":  routes,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func nestedRouteRaw(t *testing.T, match any, handlers ...json.RawMessage) map[string]any {
	t.Helper()
	route := map[string]any{"handle": handlers}
	if match != nil {
		route["match"] = match
	}
	return route
}

func responseFor(t *testing.T, h *Handler, method, target string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, target, nil)
	req.Host = "example.com"
	if err := h.ServeHTTP(rec, req, caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error {
		return nil
	})); err != nil {
		t.Fatalf("ServeHTTP: %v", err)
	}
	return rec.Body.String()
}

func TestSiteDirectivesShareOneVM(t *testing.T) {
	dir := t.TempDir()
	site := writeScript(t, dir, "site.js", respondScript)
	api := writeScript(t, dir, "api.js", `export default function handler(http) {
	http.respond(200, {}, "api")
}
`)

	subroute := subrouteRaw(t,
		nestedRouteRaw(t,
			[]any{map[string]any{"path": []any{"/api/*"}}},
			subrouteRaw(t, nestedRouteRaw(t, nil, javascriptRouteRaw(t, api)))),
		nestedRouteRaw(t, nil, javascriptRouteRaw(t, site)),
	)
	srv := &caddyhttp.Server{Routes: caddyhttp.RouteList{{
		MatcherSetsRaw: caddyhttp.RawMatcherSets{caddy.ModuleMap{
			"host": json.RawMessage(`["example.com"]`),
		}},
		HandlersRaw: []json.RawMessage{subroute},
	}}}
	ctx := serverContext(srv)

	first := &Handler{Options: config.Options{ScriptPath: site}}
	if err := first.Provision(ctx); err != nil {
		t.Fatalf("Provision site: %v", err)
	}
	defer first.Cleanup()

	second := &Handler{Options: config.Options{ScriptPath: api}}
	if err := second.Provision(ctx); err != nil {
		t.Fatalf("Provision api: %v", err)
	}
	defer second.Cleanup()

	if first.vm != second.vm {
		t.Fatal("site directives own different VMs")
	}
	if got := responseFor(t, first, http.MethodGet, "/"); got != "GET /" {
		t.Errorf("site body = %q", got)
	}
	if got := responseFor(t, second, http.MethodGet, "/anything"); got != "api" {
		t.Errorf("api body = %q", got)
	}
}

func TestSiteAcceptsProgrammaticRoute(t *testing.T) {
	dir := t.TempDir()
	site := writeScript(t, dir, "site.js", respondScript)

	srv := &caddyhttp.Server{Routes: caddyhttp.RouteList{{
		MatcherSetsRaw: caddyhttp.RawMatcherSets{caddy.ModuleMap{
			"host": json.RawMessage(`["example.com"]`),
		}},
		HandlersRaw: []json.RawMessage{javaScriptHandlerRaw},
	}}}
	ctx := serverContext(srv)

	h := &Handler{Options: config.Options{ScriptPath: site}}
	if err := h.Provision(ctx); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	defer h.Cleanup()

	if got := responseFor(t, h, http.MethodGet, "/"); got != "GET /" {
		t.Errorf("body = %q", got)
	}
}

func TestSiteVMReusedAcrossGenerations(t *testing.T) {
	dir := t.TempDir()
	site := writeScript(t, dir, "site.js", `let hits = 0
export default function handler(http) {
	hits++
	http.respond(200, {}, "hits=" + hits)
}
`)

	newServer := func() *caddyhttp.Server {
		return &caddyhttp.Server{
			Listen: []string{":80"},
			Routes: caddyhttp.RouteList{{
				MatcherSetsRaw: caddyhttp.RawMatcherSets{caddy.ModuleMap{
					"host": json.RawMessage(`["example.com"]`),
				}},
				HandlersRaw: []json.RawMessage{javascriptRouteRaw(t, site)},
			}},
		}
	}

	first := &Handler{Options: config.Options{ScriptPath: site}}
	if err := first.Provision(serverContext(newServer())); err != nil {
		t.Fatalf("Provision first: %v", err)
	}
	defer first.Cleanup()
	if got := responseFor(t, first, http.MethodGet, "/"); got != "hits=1" {
		t.Fatalf("first body = %q", got)
	}

	second := &Handler{Options: config.Options{ScriptPath: site}}
	if err := second.Provision(serverContext(newServer())); err != nil {
		t.Fatalf("Provision second: %v", err)
	}
	defer second.Cleanup()

	if first.vm != second.vm {
		t.Fatal("a reload that only changes routes replaced the VM")
	}
	if err := first.Cleanup(); err != nil {
		t.Fatalf("cleanup first: %v", err)
	}
	if got := responseFor(t, second, http.MethodGet, "/"); got != "hits=2" {
		t.Errorf("state did not survive the reload, body = %q", got)
	}
}

func TestSiteVMRecreatedWhenOptionsChange(t *testing.T) {
	dir := t.TempDir()
	site := writeScript(t, dir, "site.js", respondScript)

	newServer := func() *caddyhttp.Server {
		return &caddyhttp.Server{
			Listen: []string{":80"},
			Routes: caddyhttp.RouteList{{
				MatcherSetsRaw: caddyhttp.RawMatcherSets{caddy.ModuleMap{
					"host": json.RawMessage(`["example.com"]`),
				}},
				HandlersRaw: []json.RawMessage{javascriptRouteRaw(t, site)},
			}},
		}
	}

	first := &Handler{Options: config.Options{ScriptPath: site}}
	if err := first.Provision(serverContext(newServer())); err != nil {
		t.Fatalf("Provision first: %v", err)
	}
	defer first.Cleanup()

	second := &Handler{Options: config.Options{ScriptPath: site, Memory: 32 << 20}}
	if err := second.Provision(serverContext(newServer())); err != nil {
		t.Fatalf("Provision second: %v", err)
	}
	defer second.Cleanup()

	if first.vm == second.vm {
		t.Fatal("a changed memory limit reused the VM")
	}
}

func TestSiteVMReusedWhenAMountShiftsTheRoute(t *testing.T) {
	dir := t.TempDir()
	site := writeScript(t, dir, "site.js", `let hits = 0
export default function handler(http) {
	hits++
	http.respond(200, {}, "hits=" + hits)
}
`)
	javascript := &caddyhttp.Route{
		HandlersRaw: []json.RawMessage{javascriptRouteRaw(t, site)},
	}
	static := &caddyhttp.Route{
		HandlersRaw: []json.RawMessage{mustJSON(t, map[string]any{
			"handler": "static_response",
			"body":    "native",
		})},
	}

	oldServer := &caddyhttp.Server{Listen: []string{":80"}, Routes: caddyhttp.RouteList{*javascript}}
	newServer := &caddyhttp.Server{Listen: []string{":80"}, Routes: caddyhttp.RouteList{*static, *javascript}}

	first := &Handler{Options: config.Options{ScriptPath: site}}
	if err := first.Provision(serverContext(oldServer)); err != nil {
		t.Fatalf("Provision first: %v", err)
	}
	defer first.Cleanup()
	responseFor(t, first, http.MethodGet, "/")

	second := &Handler{Options: config.Options{ScriptPath: site}}
	if err := second.Provision(serverContext(newServer)); err != nil {
		t.Fatalf("Provision second: %v", err)
	}
	defer second.Cleanup()

	if first.vm != second.vm {
		t.Fatal("a mount that shifted the site route replaced the VM")
	}
	if got := responseFor(t, second, http.MethodGet, "/"); got != "hits=2" {
		t.Errorf("state did not survive the mount, body = %q", got)
	}
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestAppVMReusedAcrossGenerations(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, "global.js", "export default {}\n")

	first := &App{Options: config.Options{ScriptPath: script}}
	if err := first.Provision(caddy.Context{}); err != nil {
		t.Fatalf("Provision first: %v", err)
	}
	defer first.Cleanup()

	second := &App{Options: config.Options{ScriptPath: script}}
	if err := second.Provision(caddy.Context{}); err != nil {
		t.Fatalf("Provision second: %v", err)
	}
	defer second.Cleanup()

	if first.vm != second.vm {
		t.Fatal("a reload replaced the global worker VM")
	}
	if err := first.Cleanup(); err != nil {
		t.Fatalf("cleanup first: %v", err)
	}
}

func TestAppVMRecreatedWhenOptionsChange(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, "global.js", "export default {}\n")

	first := &App{Options: config.Options{ScriptPath: script}}
	if err := first.Provision(caddy.Context{}); err != nil {
		t.Fatalf("Provision first: %v", err)
	}
	defer first.Cleanup()

	second := &App{Options: config.Options{ScriptPath: script, Memory: 32 << 20}}
	if err := second.Provision(caddy.Context{}); err != nil {
		t.Fatalf("Provision second: %v", err)
	}
	defer second.Cleanup()

	if first.vm == second.vm {
		t.Fatal("a changed memory limit reused the worker VM")
	}
}
