package files

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadWriteWithinRoot(t *testing.T) {
	dir := t.TempDir()
	fsys, err := NewFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer fsys.Close()

	if err := fsys.WriteFile("hello.txt", []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := fsys.ReadFile("/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hi" {
		t.Fatalf("got %q, want %q", data, "hi")
	}
}

func TestDotDotDoesNotEscape(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "secret.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	fsys, err := NewFS(root)
	if err != nil {
		t.Fatal(err)
	}
	defer fsys.Close()

	if data, err := fsys.ReadFile("../secret.txt"); err == nil {
		t.Fatalf("escaped root, read %q", data)
	}
	if err := fsys.WriteFile("../escape.txt", []byte("x"), 0o644); err != nil {
		t.Fatalf("confined write failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(parent, "escape.txt")); err == nil {
		t.Fatal("write escaped root")
	}
}

func TestSymlinkDoesNotEscape(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "root")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(parent, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	fsys, err := NewFS(root)
	if err != nil {
		t.Fatal(err)
	}
	defer fsys.Close()

	if data, err := fsys.ReadFile("link.txt"); err == nil {
		t.Fatalf("followed escaping symlink, read %q", data)
	}
}

func TestJoin(t *testing.T) {
	if got := Join("/a", "b", "..", "c"); got != "/a/c" {
		t.Fatalf("Join = %q, want %q", got, "/a/c")
	}
}
