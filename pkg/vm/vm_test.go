package vm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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

	resp, err := v.Handle(context.Background(), Request{Method: "GET", Path: "/hello"})
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

	resp, err := v.Handle(context.Background(), Request{Method: "GET", Path: "/"})
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
	_, err = v.Handle(ctx, Request{Method: "GET", Path: "/spin"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Handle error = %v, want deadline exceeded", err)
	}

	resp, err := v.Handle(context.Background(), Request{Method: "GET", Path: "/ok"})
	if err != nil {
		t.Fatalf("Handle after timeout: %v", err)
	}
	if resp == nil || string(resp.Body) != "ok" {
		t.Fatalf("response after timeout = %+v", resp)
	}
}
