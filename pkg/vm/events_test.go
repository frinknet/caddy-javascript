package vm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func waitFile(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	last := ""
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			last = string(data)
			if last == want {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s = %q, want %q", filepath.Base(path), last, want)
}

func TestEventDispatch(t *testing.T) {
	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", `import fs from "fs"
events.on("demo", (e) => {
	fs.writeFileSync("/event.txt", [
		e.id, e.name, e.origin, e.time, e.data.msg, e.data.nested.k,
	].join("|"))
})
events.on("*", (e) => {
	if (e.name === "other") fs.writeFileSync("/wild.txt", String(e.data.n))
})
`)
	v, err := New(Config{ScriptPath: entry, RootDir: dir, Events: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer v.Close()

	names := strings.Join(v.EventNames(), ",")
	if names != ",demo" {
		t.Errorf("EventNames = %q, want %q", names, ",demo")
	}

	at := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	v.DispatchEvent(Event{
		ID:     "evt-1",
		Name:   "demo",
		Origin: "test.module",
		Time:   at,
		Data: map[string]any{
			"msg":    "hi",
			"nested": map[string]any{"k": []any{1, 2}},
		},
	})
	v.DispatchEvent(Event{
		ID:   "evt-2",
		Name: "other",
		Time: at,
		Data: map[string]any{"n": 7},
	})

	waitFile(t, filepath.Join(dir, "event.txt"), "evt-1|demo|test.module|2020-01-02T03:04:05Z|hi|1,2")
	waitFile(t, filepath.Join(dir, "wild.txt"), "7")
}

func TestEventsAbsentOutsideWorker(t *testing.T) {
	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", `import fs from "fs"
fs.writeFileSync("/absent.txt", typeof globalThis.events)
`)
	v, err := New(Config{ScriptPath: entry, RootDir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer v.Close()

	waitFile(t, filepath.Join(dir, "absent.txt"), "undefined")
}

func TestEventRegistrationAfterBindFails(t *testing.T) {
	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", `import fs from "fs"
setTimeout(() => {
	try {
		events.on("late", () => {})
		fs.writeFileSync("/late.txt", "ok")
	} catch (e) {
		fs.writeFileSync("/late.txt", "err:" + e.message)
	}
}, 1)
`)
	v, err := New(Config{ScriptPath: entry, RootDir: dir, Events: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer v.Close()
	v.MarkEventBound()

	waitFile(t, filepath.Join(dir, "late.txt"), "err:javascript: events.on must run at module scope")
}
