package handler

import (
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

func TestServeHTTPFallthrough(t *testing.T) {
	dir := t.TempDir()
	path := writeScript(t, dir, "main.js", "export default function handler(http) {}\n")
	h := &Handler{Options: config.Options{ScriptPath: path, Fallthrough: true}}
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
