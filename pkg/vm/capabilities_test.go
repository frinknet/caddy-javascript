package vm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func waitBody(t *testing.T, v *VM, script string, ready func(string) bool) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	last := ""
	for time.Now().Before(deadline) {
		resp, err := v.Handle(context.Background(), script, testRequest(http.MethodGet, "/", nil))
		if err != nil {
			t.Fatalf("Handle: %v", err)
		}
		if resp != nil {
			last = string(resp.Body)
			if ready(last) {
				return last
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for the VM, last response %q", last)
	return last
}

func TestRuntimeCapabilities(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "lib.js", "lib\n")
	entry := writeModule(t, dir, "main.js", `import fs from "fs"
const text = fs.readFileSync("/lib.js", "utf8").trim()
const clone = structuredClone({ a: [1, 2], when: new Date(0) })
export default function handler(http) {
	http.respond(200, {}, [
		typeof fetch, typeof Worker, typeof BroadcastChannel, typeof MessageChannel,
		typeof structuredClone, typeof process, typeof require,
		text, clone.a.join(","), new BroadcastChannel("probe").name,
	].join("|"))
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
	want := "undefined|function|function|function|function|undefined|undefined|lib|1,2|probe"
	if got := string(resp.Body); got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func TestFileSystemIsConfinedToRoot(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "root")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "outside.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	entry := writeModule(t, dir, "main.js", `import fs from "fs"
const text = fs.readFileSync("/../outside.txt", "utf8")
export default function handler(http) { http.respond(200, {}, text) }
`)
	if _, err := New(Config{ScriptPath: entry, RootDir: dir, RequireHandler: true}); err == nil {
		t.Fatal("New read a file outside the sandbox root")
	}
}

func TestFetchNeedsAllowAndStaysInIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("remote"))
	}))
	defer srv.Close()

	fetchScript := func(url string) string {
		return `let result = "pending"
fetch(` + url + `).then((r) => r.text()).then((t) => { result = t }).catch((e) => { result = "error:" + e })
export default function handler(http) { http.respond(200, {}, result) }
`
	}

	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", fetchScript(`"`+srv.URL+`"`))
	v, err := New(Config{ScriptPath: entry, RootDir: dir, RequireHandler: true, FetchAllow: []string{"127.0.0.1"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer v.Close()
	if got := waitBody(t, v, entry, func(body string) bool { return body != "pending" }); got != "remote" {
		t.Errorf("allowed fetch body = %q, want %q", got, "remote")
	}

	dir = t.TempDir()
	entry = writeModule(t, dir, "main.js", fetchScript(`"`+srv.URL+`"`))
	v, err = New(Config{ScriptPath: entry, RootDir: dir, RequireHandler: true, FetchAllow: []string{"example.com"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer v.Close()
	if got := waitBody(t, v, entry, func(body string) bool { return body != "pending" }); !strings.HasPrefix(got, "error:") {
		t.Errorf("denied fetch body = %q, want an error", got)
	}

	dir = t.TempDir()
	entry = writeModule(t, dir, "main.js", `export default function handler(http) {
	http.respond(200, {}, typeof fetch)
}
`)
	v, err = New(Config{ScriptPath: entry, RootDir: dir, RequireHandler: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer v.Close()
	resp, err := v.Handle(context.Background(), entry, testRequest(http.MethodGet, "/", nil))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := string(resp.Body); got != "undefined" {
		t.Errorf("fetch without fetch_allow = %q, want undefined", got)
	}
}

func TestWorkerPostsToItsParent(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "worker.js", `postMessage("ok")
`)
	entry := writeModule(t, dir, "main.js", `let got = "pending"
const worker = new Worker("./worker.js", { type: "module" })
worker.onmessage = (e) => { got = "worker:" + e.data }
export default function handler(http) { http.respond(200, {}, got) }
`)
	v, err := New(Config{ScriptPath: entry, RootDir: dir, RequireHandler: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer v.Close()

	if got := waitBody(t, v, entry, func(body string) bool { return body != "pending" }); got != "worker:ok" {
		t.Errorf("worker body = %q, want %q", got, "worker:ok")
	}
}

func TestWorkerIsTerminatedOnClose(t *testing.T) {
	dir := t.TempDir()
	writeModule(t, dir, "worker.js", `while (true) {}
`)
	entry := writeModule(t, dir, "main.js", `new Worker("/worker.js", { type: "module" })
export default function handler(http) { http.respond(200, {}, "ok") }
`)
	v, err := New(Config{ScriptPath: entry, RootDir: dir, RequireHandler: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	done := make(chan struct{})
	go func() {
		v.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not terminate a running worker")
	}
}

func TestBroadcastChannelWithinVM(t *testing.T) {
	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", `let got = "pending"
const sender = new BroadcastChannel("probe")
const receiver = new BroadcastChannel("probe")
receiver.onmessage = (e) => { got = "got:" + e.data }
setTimeout(() => sender.postMessage("hello"), 5)
export default function handler(http) { http.respond(200, {}, got) }
`)
	v, err := New(Config{ScriptPath: entry, RootDir: dir, RequireHandler: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer v.Close()

	if got := waitBody(t, v, entry, func(body string) bool { return body != "pending" }); got != "got:hello" {
		t.Errorf("broadcast body = %q, want %q", got, "got:hello")
	}
}

func TestBroadcastChannelDoesNotCrossVMs(t *testing.T) {
	dirA := t.TempDir()
	entryA := writeModule(t, dirA, "main.js", `const ch = new BroadcastChannel("probe")
setTimeout(() => ch.postMessage("leak"), 5)
export default function handler(http) { http.respond(200, {}, "sent") }
`)
	a, err := New(Config{ScriptPath: entryA, RootDir: dirA, RequireHandler: true})
	if err != nil {
		t.Fatalf("New A: %v", err)
	}
	defer a.Close()

	dirB := t.TempDir()
	entryB := writeModule(t, dirB, "main.js", `let got = "none"
const ch = new BroadcastChannel("probe")
ch.onmessage = (e) => { got = "got:" + e.data }
export default function handler(http) { http.respond(200, {}, got) }
`)
	b, err := New(Config{ScriptPath: entryB, RootDir: dirB, RequireHandler: true})
	if err != nil {
		t.Fatalf("New B: %v", err)
	}
	defer b.Close()

	time.Sleep(150 * time.Millisecond)
	resp, err := b.Handle(context.Background(), entryB, testRequest(http.MethodGet, "/", nil))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := string(resp.Body); got != "none" {
		t.Errorf("body = %q, want %q; a message crossed VMs", got, "none")
	}
}
