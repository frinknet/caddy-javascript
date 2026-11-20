package vm

import (
	"context"
	"net/http"
	"testing"
)

func TestTemplateGlobals(t *testing.T) {
	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", `export default function handler(http) {
	http.respond(200, {}, _.upper("hello") + "|" + _.pathEscape("a b"))
}
`)
	v, err := New(Config{ScriptPath: entry, RootDir: dir, RequireHandler: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer v.Close()

	resp, err := v.Handle(context.Background(), entry, testRequest(http.MethodGet, "/", nil))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if resp == nil || string(resp.Body) != "HELLO|a%20b" {
		t.Fatalf("body = %q", resp.Body)
	}
}

func TestTemplateEnvDenied(t *testing.T) {
	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", `export default function handler(http) {
	_.env("PATH")
}
`)
	v, err := New(Config{ScriptPath: entry, RootDir: dir, RequireHandler: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer v.Close()

	_, err = v.Handle(context.Background(), entry, testRequest(http.MethodGet, "/", nil))
	if err == nil {
		t.Fatal("env was allowed without env_allow")
	}
}
