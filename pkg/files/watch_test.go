package files

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWatcherFiresOnChange(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "server.js")
	if err := os.WriteFile(script, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}

	fired := make(chan struct{}, 4)
	watcher, err := NewWatcher(script, 10*time.Millisecond, func() {
		select {
		case fired <- struct{}{}:
		default:
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()

	if err := os.WriteFile(script, []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case <-fired:
	case <-time.After(3 * time.Second):
		t.Fatal("watcher did not fire on change")
	}
}

func TestWatcherIgnoresOtherFiles(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "server.js")
	other := filepath.Join(dir, "other.js")
	if err := os.WriteFile(script, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	fired := make(chan struct{}, 4)
	watcher, err := NewWatcher(script, 10*time.Millisecond, func() {
		select {
		case fired <- struct{}{}:
		default:
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()

	if err := os.WriteFile(other, []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case <-fired:
		t.Fatal("watcher fired for an unrelated file")
	case <-time.After(300 * time.Millisecond):
	}
}
