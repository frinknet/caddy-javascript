package vm

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testConfig(entry string) map[string]any {
	return map[string]any{
		"apps": map[string]any{
			"javascript": map[string]any{"script_path": entry},
			"http": map[string]any{
				"servers": map[string]any{
					"srv0": map[string]any{"listen": []any{":80"}},
				},
			},
		},
	}
}

func handle(t *testing.T, v *VM, script, method, target string) *Response {
	t.Helper()
	resp, err := v.Handle(context.Background(), script, testRequest(method, target, nil))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	return resp
}

func configVM(t *testing.T, dir, entry string, value map[string]any, apply func([]byte) error) *VM {
	t.Helper()
	v, err := New(Config{
		ScriptPath:     entry,
		RootDir:        dir,
		RequireHandler: true,
		Caddy:          &CaddyConfig{Value: value, Apply: apply},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { v.Close() })
	return v
}

func TestConfigIsSeededAndMutable(t *testing.T) {
	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", `export default function handler(http) {
	http.respond(200, {}, typeof config + ":" + config.apps.javascript.script_path)
}
`)
	v := configVM(t, dir, entry, testConfig(entry), nil)
	if !v.HasConfig() {
		t.Fatal("config object was not installed")
	}
	resp := handle(t, v, entry, http.MethodGet, "/")
	if resp == nil || !strings.HasPrefix(string(resp.Body), "object:") {
		t.Fatalf("response = %+v", resp)
	}
}

func TestConfigTopLevelEditRejected(t *testing.T) {
	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", `config.apps.http.servers.srv0.listen = [":99"]
export default function handler(http) {}
`)
	_, err := New(Config{
		ScriptPath:     entry,
		RootDir:        dir,
		RequireHandler: true,
		Caddy:          &CaddyConfig{Value: testConfig(entry)},
	})
	if err == nil {
		t.Fatal("New accepted a top-level config edit")
	}
	if !strings.Contains(err.Error(), "during module evaluation") {
		t.Fatalf("error = %v", err)
	}
}

func TestConfigApplySkipsUnchanged(t *testing.T) {
	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", `export default function handler(http) {
	config.apply()
	http.respond(200, {}, "same")
}
`)
	var calls int
	v := configVM(t, dir, entry, testConfig(entry), func([]byte) error {
		calls++
		return nil
	})
	handle(t, v, entry, http.MethodGet, "/")
	handle(t, v, entry, http.MethodGet, "/")
	time.Sleep(50 * time.Millisecond)
	if calls != 0 {
		t.Fatalf("an unchanged config committed %d times", calls)
	}
}

func TestConfigApplyCommitsChanged(t *testing.T) {
	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", `export default function handler(http) {
	config.apps.http.servers.srv0.listen = [":81"]
	config.apply()
	http.respond(200, {}, "changed")
}
`)
	applied := make(chan []byte, 1)
	v := configVM(t, dir, entry, testConfig(entry), func(doc []byte) error {
		applied <- append([]byte(nil), doc...)
		return nil
	})
	handle(t, v, entry, http.MethodGet, "/")
	select {
	case doc := <-applied:
		if !strings.Contains(string(doc), ":81") {
			t.Fatalf("applied doc = %s", doc)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("config.apply did not reach the handler")
	}
}

func TestConfigApplyRejectsRemovingWorker(t *testing.T) {
	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", `export default function handler(http) {
	delete config.apps.javascript
	config.apply()
	http.respond(200, {}, "unreachable")
}
`)
	v := configVM(t, dir, entry, testConfig(entry), func([]byte) error { return nil })
	_, err := v.Handle(context.Background(), entry, testRequest(http.MethodGet, "/", nil))
	if err == nil || !strings.Contains(err.Error(), "apps.javascript") {
		t.Fatalf("error = %v", err)
	}
}

func TestConfigCaddyfileAdaptsInsideSandbox(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "extra.Caddyfile")
	if err := os.WriteFile(path, []byte(":8080 {\n\trespond \"ok\"\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	entry := writeModule(t, dir, "main.js", fmt.Sprintf(`export default function handler(http) {
	const doc = config.caddyfile(%q)
	http.respond(200, {}, JSON.stringify(doc.apps.http.servers.srv0.listen))
}
`, path))
	v := configVM(t, dir, entry, testConfig(entry), nil)
	resp := handle(t, v, entry, http.MethodGet, "/")
	if resp == nil || string(resp.Body) != `[":8080"]` {
		t.Fatalf("response = %+v", resp)
	}
}

func TestConfigCaddyfileRejectsOutsideSandbox(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.Caddyfile")
	if err := os.WriteFile(outside, []byte(":8080 {\n\trespond \"ok\"\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	entry := writeModule(t, dir, "main.js", fmt.Sprintf(`export default function handler(http) {
	config.caddyfile(%q)
	http.respond(200, {}, "unreachable")
}
`, outside))
	v := configVM(t, dir, entry, testConfig(entry), nil)
	_, err := v.Handle(context.Background(), entry, testRequest(http.MethodGet, "/", nil))
	if err == nil || !strings.Contains(err.Error(), "outside the root") {
		t.Fatalf("error = %v", err)
	}
}
