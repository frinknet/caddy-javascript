package e2e

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReload(t *testing.T) {
	base := baseURL(t)
	appDir := os.Getenv("CADDY_E2E_APP")
	if appDir == "" {
		t.Skip("CADDY_E2E_APP not set; run tests/e2e/run.sh instead")
	}

	client := &http.Client{Timeout: 5 * time.Second}
	waitReady(t, client, base)

	if got := get(t, client, base+"/version"); got != "v1" {
		t.Fatalf("/version = %q, want %q", got, "v1")
	}

	script := filepath.Join(appDir, "server.js")
	src, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	updated := string(src) + "\napp.get('/version', (req, res) => { res.send('v2') })\n"
	if err := os.WriteFile(script, []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if get(t, client, base+"/version") == "v2" {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("script was not reloaded after the file changed")
}

func get(t *testing.T, client *http.Client, url string) string {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return string(data)
}
