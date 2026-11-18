package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type Case struct {
	Name             string            `json:"name"`
	Method           string            `json:"method"`
	Path             string            `json:"path"`
	Body             string            `json:"body"`
	Headers          map[string]string `json:"headers"`
	WantStatus       int               `json:"wantStatus"`
	WantContentType  string            `json:"wantContentType"`
	WantBodyContains string            `json:"wantBodyContains"`
	WantBodyEquals   string            `json:"wantBodyEquals"`
	WantHeaders      map[string]string `json:"wantHeaders"`
}

func baseURL(t *testing.T) string {
	t.Helper()
	v := os.Getenv("CADDY_E2E_ADDR")
	if v == "" {
		t.Skip("CADDY_E2E_ADDR not set; run tests/e2e/run.sh instead")
	}
	return strings.TrimRight(v, "/")
}

func casesDir() string {
	if v := os.Getenv("CADDY_E2E_CASES"); v != "" {
		return v
	}
	return "./cases"
}

func TestE2E(t *testing.T) {
	base := baseURL(t)
	client := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	waitReady(t, client, base)

	files, err := filepath.Glob(filepath.Join(casesDir(), "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("no case files found in %s", casesDir())
	}

	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var cases []Case
		if err := json.Unmarshal(data, &cases); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		group := strings.TrimSuffix(filepath.Base(file), ".json")
		for _, c := range cases {
			c := c
			t.Run(group+"/"+c.Name, func(t *testing.T) {
				runCase(t, client, base, c)
			})
		}
	}
}

func waitReady(t *testing.T, client *http.Client, base string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(base + "/")
		if err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("server at %s never became ready", base)
}

func runCase(t *testing.T, client *http.Client, base string, c Case) {
	t.Helper()

	var body io.Reader
	if c.Body != "" {
		body = strings.NewReader(c.Body)
	}

	req, err := http.NewRequest(c.Method, base+c.Path, body)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range c.Headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	got := string(data)

	if c.WantStatus != 0 && resp.StatusCode != c.WantStatus {
		t.Errorf("status = %d, want %d (body %q)", resp.StatusCode, c.WantStatus, got)
	}
	if c.WantContentType != "" && !strings.Contains(resp.Header.Get("Content-Type"), c.WantContentType) {
		t.Errorf("content-type = %q, want to contain %q", resp.Header.Get("Content-Type"), c.WantContentType)
	}
	if c.WantBodyEquals != "" && got != c.WantBodyEquals {
		t.Errorf("body = %q, want %q", got, c.WantBodyEquals)
	}
	if c.WantBodyContains != "" && !strings.Contains(got, c.WantBodyContains) {
		t.Errorf("body = %q, want to contain %q", got, c.WantBodyContains)
	}
	for name, want := range c.WantHeaders {
		if have := resp.Header.Get(name); have != want {
			t.Errorf("header %s = %q, want %q", name, have, want)
		}
	}
}
