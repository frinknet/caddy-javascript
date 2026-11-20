package vm

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWatchReportsChanges(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "watched"), 0o700); err != nil {
		t.Fatal(err)
	}
	entry := writeModule(t, dir, "main.js", `import fs from "fs"
let stop
stop = watch("/watched", (names) => {
	fs.writeFileSync("/seen.txt", names.join(","))
	if (stop) stop()
})
`)
	v, err := New(Config{ScriptPath: entry, RootDir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer v.Close()

	if err := os.WriteFile(filepath.Join(dir, "watched", "probe.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFile(t, filepath.Join(dir, "seen.txt"), "/watched/probe.txt")

	if err := os.WriteFile(filepath.Join(dir, "watched", "after.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	data, err := os.ReadFile(filepath.Join(dir, "seen.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != "/watched/probe.txt" {
		t.Errorf("watch kept calling after stop, got %q", got)
	}
}

func TestWatchRejectsPathsOutsideTheRoot(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "root")
	if err := os.MkdirAll(filepath.Join(dir, "watched"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(parent, "outside"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(parent, "outside"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	entry := writeModule(t, dir, "main.js", `import fs from "fs"
try {
	watch("/link", () => {})
	fs.writeFileSync("/result.txt", "ok")
} catch (e) {
	fs.writeFileSync("/result.txt", "err:" + e.message)
}
`)
	v, err := New(Config{ScriptPath: entry, RootDir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer v.Close()

	data, err := os.ReadFile(filepath.Join(dir, "result.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got[:4] != "err:" {
		t.Errorf("watch outside the root = %q, want an error", got)
	}
}
