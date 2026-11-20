package vm

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileSystemRejectsSymlinkEscape(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "root")
	outside := filepath.Join(parent, "outside")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}

	entry := writeModule(t, dir, "main.js", `import fs from "fs"
const text = fs.readFileSync("/link/secret.txt", "utf8")
export default function handler(http) { http.respond(200, {}, text) }
`)
	if _, err := New(Config{ScriptPath: entry, RootDir: dir, RequireHandler: true}); err == nil {
		t.Fatal("module evaluation followed a symlink outside the root")
	}
}

func TestFileSystemRejectsAbsoluteHostPaths(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "root")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(parent, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	entry := writeModule(t, dir, "main.js", `import fs from "fs"
const text = fs.readFileSync(`+quoteJS(secret)+`, "utf8")
export default function handler(http) { http.respond(200, {}, text) }
`)
	if _, err := New(Config{ScriptPath: entry, RootDir: dir, RequireHandler: true}); err == nil {
		t.Fatal("module evaluation read an absolute host path")
	}
}

func TestEnvAllowlistIsActive(t *testing.T) {
	t.Setenv("CJ_TEST_SECRET", "s3cret")
	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", `const value = _.env("CJ_TEST_SECRET")
export default function handler(http) { http.respond(200, {}, value) }
`)
	v, err := New(Config{
		ScriptPath:     entry,
		RootDir:        dir,
		RequireHandler: true,
		EnvAllow:       []string{"CJ_TEST_SECRET"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer v.Close()

	resp, err := v.Handle(context.Background(), entry, testRequest(http.MethodGet, "/", nil))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := string(resp.Body); got != "s3cret" {
		t.Errorf("env_allow value = %q, want %q", got, "s3cret")
	}
}

func TestMemoryLimitStopsTheModule(t *testing.T) {
	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", `let held = []
for (let i = 0; i < 100000; i++) held.push("x".repeat(1024))
export default function handler(http) { http.respond(200, {}, "unreachable") }
`)
	_, err := New(Config{
		ScriptPath:     entry,
		RootDir:        dir,
		RequireHandler: true,
		Memory:         4 << 20,
	})
	if err == nil {
		t.Fatal("the module exceeded the memory limit without an error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "memory") {
		t.Errorf("memory error = %v, want a memory error", err)
	}
}

func TestStackLimitStopsRunawayRecursion(t *testing.T) {
	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", `function down(n) { return 1 + down(n + 1) }
export default function handler(http) { down(0) }
`)
	v, err := New(Config{ScriptPath: entry, RootDir: dir, RequireHandler: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer v.Close()

	if _, err := v.Handle(context.Background(), entry, testRequest(http.MethodGet, "/", nil)); err == nil {
		t.Fatal("runaway recursion did not fail")
	}
	if got := v.Stats().Failures; got == 0 {
		t.Error("the failure counter did not move")
	}
}

func TestCallbackBudgetStopsARequest(t *testing.T) {
	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", `export default function handler(http) {
	for (let i = 0; i < 100; i++) http.headers["x-probe"]
	http.respond(200, {}, "unreachable")
}
`)
	v, err := New(Config{ScriptPath: entry, RootDir: dir, RequireHandler: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer v.Close()

	req := testRequest(http.MethodGet, "/", nil)
	req.MaxCallbacks = 8
	_, err = v.Handle(context.Background(), entry, req)
	if err == nil || !strings.Contains(err.Error(), "callback budget") {
		t.Fatalf("Handle error = %v, want the callback budget", err)
	}
}

func quoteJS(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + replacer.Replace(value) + `"`
}
