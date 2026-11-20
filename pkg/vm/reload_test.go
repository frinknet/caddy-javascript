package vm

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTreeWatchTriggersReload(t *testing.T) {
	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", `export default function handler(http) {
	http.respond(200, {}, "one")
}
`)
	reloads := make(chan struct{}, 4)
	v, err := New(Config{
		ScriptPath:     entry,
		RootDir:        dir,
		RequireHandler: true,
		Watch:          true,
		Reload: func() error {
			reloads <- struct{}{}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer v.Close()

	writeModule(t, dir, "main.js", `export default function handler(http) {
	http.respond(200, {}, "two")
}
`)
	select {
	case <-reloads:
	case <-time.After(3 * time.Second):
		t.Fatal("editing the entry module did not trigger a reload")
	}
	if !v.TreeChanged() {
		t.Fatal("TreeChanged did not see the edited module")
	}
}

func TestTreeWatchSeesImports(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "lib.js", `export const value = "one"`)
	entry := writeModule(t, dir, "main.js", `import { value } from "./lib.js"
export default function handler(http) { http.respond(200, {}, value) }
`)
	reloads := make(chan struct{}, 4)
	v, err := New(Config{
		ScriptPath:     entry,
		RootDir:        dir,
		RequireHandler: true,
		Watch:          true,
		Reload:         func() error { reloads <- struct{}{}; return nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer v.Close()

	writeModule(t, dir, "lib.js", `export const value = "two"`)
	select {
	case <-reloads:
	case <-time.After(3 * time.Second):
		t.Fatal("editing an imported module did not trigger a reload")
	}
	if !v.TreeChanged() {
		t.Fatal("TreeChanged did not see the edited import")
	}
}

func TestRejectedReloadKeepsTheRunningModule(t *testing.T) {
	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", `export default function handler(http) {
	http.respond(200, {}, "one")
}
`)
	attempted := make(chan error, 4)
	fail := errors.New("syntax error")
	v, err := New(Config{
		ScriptPath:     entry,
		RootDir:        dir,
		RequireHandler: true,
		Watch:          true,
		Reload: func() error {
			attempted <- fail
			return fail
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer v.Close()

	writeModule(t, dir, "main.js", `export default function handler( {`)
	select {
	case <-attempted:
	case <-time.After(3 * time.Second):
		t.Fatal("editing the entry module did not attempt a reload")
	}

	resp, err := v.Handle(context.Background(), entry, testRequest(http.MethodGet, "/", nil))
	if err != nil {
		t.Fatalf("Handle after a rejected reload: %v", err)
	}
	if got := string(resp.Body); got != "one" {
		t.Errorf("running module after a rejected reload = %q, want %q", got, "one")
	}
}

func TestCloseStopsTheTreeWatcher(t *testing.T) {
	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", `export default function handler(http) {
	http.respond(200, {}, "ok")
}
`)
	reloads := make(chan struct{}, 4)
	v, err := New(Config{
		ScriptPath:     entry,
		RootDir:        dir,
		RequireHandler: true,
		Watch:          true,
		Reload:         func() error { reloads <- struct{}{}; return nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := v.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	writeModule(t, dir, "main.js", `export default function handler(http) {
	http.respond(200, {}, "changed")
}
`)
	select {
	case <-reloads:
		t.Fatal("a closed VM still reacted to a module edit")
	case <-time.After(400 * time.Millisecond):
	}
}

func TestWatchDisabledLeavesTheTreeAlone(t *testing.T) {
	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", `export default function handler(http) {
	http.respond(200, {}, "ok")
}
`)
	reloads := make(chan struct{}, 4)
	v, err := New(Config{
		ScriptPath:     entry,
		RootDir:        dir,
		RequireHandler: true,
		Reload:         func() error { reloads <- struct{}{}; return nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer v.Close()

	writeModule(t, dir, "main.js", `export default function handler(http) {
	http.respond(200, {}, "changed")
}
`)
	select {
	case <-reloads:
		t.Fatal("watch was not enabled but the VM reloaded")
	case <-time.After(400 * time.Millisecond):
	}
	if !v.TreeChanged() {
		t.Fatal("TreeChanged should still report the edit")
	}
}

func TestTreeWatchIsRooted(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "root")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	entry := writeModule(t, dir, "main.js", `export default function handler(http) {
	http.respond(200, {}, "ok")
}
`)
	reloads := make(chan struct{}, 4)
	v, err := New(Config{
		ScriptPath:     entry,
		RootDir:        dir,
		RequireHandler: true,
		Watch:          true,
		Reload:         func() error { reloads <- struct{}{}; return nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer v.Close()

	writeModule(t, parent, "outside.js", `export const value = 1`)
	select {
	case <-reloads:
		t.Fatal("a file outside the root triggered a reload")
	case <-time.After(400 * time.Millisecond):
	}
}
