package template

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanSandboxName(t *testing.T) {
	cases := map[string]string{
		".":             ".",
		"":              ".",
		"/":             ".",
		"lib.js":        "lib.js",
		"/lib.js":       "lib.js",
		"../etc/passwd": "etc/passwd",
		"a/../b":        "b",
	}
	for in, want := range cases {
		if got := cleanSandboxName(in); got != want {
			t.Errorf("cleanSandboxName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuiltinsListFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "lib.js"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	b := &builtins{host: &mockHost{req: &http.Request{}, root: root}}
	names, err := b.listFiles(".")
	if err != nil {
		t.Fatalf("listFiles: %v", err)
	}
	found := false
	for _, name := range names {
		if name == "lib.js" {
			found = true
		}
	}
	if !found {
		t.Fatalf("listFiles = %v, want lib.js", names)
	}
}

func TestBuiltinsFileStat(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "lib.js"), []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	b := &builtins{host: &mockHost{req: &http.Request{}, root: root}}
	info, err := b.fileStat("lib.js")
	if err != nil {
		t.Fatalf("fileStat: %v", err)
	}
	if info.Name != "lib.js" || info.Size != 3 || info.IsDir {
		t.Fatalf("fileStat = %+v, want lib.js, 3 bytes, not a dir", info)
	}
	if info.ModTime == "" {
		t.Fatal("fileStat mod time is empty")
	}
}

func TestBuiltinsHumanize(t *testing.T) {
	b := &builtins{}
	size, err := b.humanize("size", "2048000")
	if err != nil {
		t.Fatalf("humanize size: %v", err)
	}
	if size != "2.0 MB" {
		t.Fatalf("humanize size = %q, want 2.0 MB", size)
	}

	when, err := b.humanize("time:2006-Jan-02", "2020-Jan-02")
	if err != nil {
		t.Fatalf("humanize time: %v", err)
	}
	if !strings.HasSuffix(when, "ago") {
		t.Fatalf("humanize time = %q, want a relative past time", when)
	}

	if _, err := b.humanize("size", "not a number"); err == nil {
		t.Fatal("humanize accepted an invalid size")
	}
	if _, err := b.humanize("bogus", "1"); err == nil {
		t.Fatal("humanize accepted an unknown format")
	}
}

func TestBuiltinsSplitFrontMatter(t *testing.T) {
	b := &builtins{}
	cases := []struct {
		name string
		in   string
		key  string
		want string
	}{
		{"yaml", "---\ntitle: Hi\n---\nBody", "title", "Hi"},
		{"toml", "+++\ntitle = \"Hi\"\n+++\nBody", "title", "Hi"},
		{"json", "{\n\"title\": \"Hi\"\n}\nBody", "title", "Hi"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := b.splitFrontMatter(tc.in)
			if err != nil {
				t.Fatalf("splitFrontMatter: %v", err)
			}
			if got := doc.Meta[tc.key]; got != tc.want {
				t.Fatalf("meta[%q] = %v, want %q", tc.key, got, tc.want)
			}
			if !strings.Contains(doc.Body, "Body") {
				t.Fatalf("body = %q, want it to contain Body", doc.Body)
			}
		})
	}

	doc, err := b.splitFrontMatter("plain text")
	if err != nil {
		t.Fatalf("splitFrontMatter plain: %v", err)
	}
	if doc.Meta != nil || doc.Body != "plain text" {
		t.Fatalf("plain split = %+v, want no meta and the whole body", doc)
	}

	if _, err := b.splitFrontMatter("---\ntitle: Hi"); err == nil {
		t.Fatal("splitFrontMatter accepted unterminated front matter")
	}
}
