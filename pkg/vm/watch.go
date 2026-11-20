package vm

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	quickjs "github.com/go-quickjs/go-quickjs"
	"go.uber.org/zap"
)

// watchDebounce coalesces a burst of filesystem events into one callback.
const watchDebounce = 100 * time.Millisecond

type watchEntry struct {
	watcher *fsnotify.Watcher
	done    chan struct{}
	once    sync.Once
}

func (w *watchEntry) stop() {
	w.once.Do(func() {
		close(w.done)
		_ = w.watcher.Close()
	})
}

// installWatch creates the `watch` global. It runs on the VM goroutine before
// the entry module is evaluated.
func (v *VM) installWatch() error {
	v.watchers = make(map[int]*watchEntry)
	return v.rt.Set("watch", v.watch)
}

// watch is watch(dir, fn). It reports changes under one sandbox directory,
// without recursing, and returns a function that stops the watch. The callback
// receives the changed paths as sandbox paths, debounced and sorted.
func (v *VM) watch(dir string, fn quickjs.Value) (func(), error) {
	if !fn.IsFunction() {
		return nil, errors.New("javascript: watch requires a function")
	}
	abs, err := v.watchDir(dir)
	if err != nil {
		return nil, err
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("javascript: watch %q: %w", dir, err)
	}
	if err := watcher.Add(abs); err != nil {
		_ = watcher.Close()
		return nil, fmt.Errorf("javascript: watch %q: %w", dir, err)
	}

	entry := &watchEntry{watcher: watcher, done: make(chan struct{})}
	v.watchMu.Lock()
	id := v.watchSeq
	v.watchSeq++
	v.watchers[id] = entry
	v.watchMu.Unlock()

	go v.watchLoop(id, fn, entry)
	return func() { v.removeWatcher(id) }, nil
}

// watchDir resolves a sandbox path to an existing directory.
func (v *VM) watchDir(dir string) (string, error) {
	if !strings.HasPrefix(dir, "/") {
		return "", fmt.Errorf("javascript: watch path %q must start with /", dir)
	}
	name := path.Join("/", filepath.ToSlash(dir))
	rel := strings.TrimPrefix(name, "/")
	info, err := v.root.Stat(filepath.FromSlash(rel))
	if err != nil {
		return "", fmt.Errorf("javascript: watch %q: %w", dir, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("javascript: watch %q is not a directory", dir)
	}

	abs := filepath.Join(v.sandbox.dir, filepath.FromSlash(rel))
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("javascript: watch %q: %w", dir, err)
	}
	target, err := filepath.Rel(v.sandbox.dir, resolved)
	if err != nil {
		return "", fmt.Errorf("javascript: watch %q: %w", dir, err)
	}
	if target == ".." || strings.HasPrefix(target, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("javascript: watch %q escapes the root", dir)
	}
	return resolved, nil
}

// watchLoop reads filesystem events, debounces them, and posts one callback to
// the VM goroutine. It exits when the watcher is stopped or the VM closes.
func (v *VM) watchLoop(id int, fn quickjs.Value, entry *watchEntry) {
	var timer *time.Timer
	var timerC <-chan time.Time
	pending := make(map[string]struct{})

	flush := func() {
		names := make([]string, 0, len(pending))
		for name := range pending {
			names = append(names, name)
		}
		pending = make(map[string]struct{})
		if len(names) == 0 {
			return
		}
		sort.Strings(names)
		v.loop.Post(func() { v.callWatcher(id, fn, names) })
	}

	for {
		select {
		case <-entry.done:
			if timer != nil {
				timer.Stop()
			}
			return
		case event, ok := <-entry.watcher.Events:
			if !ok {
				return
			}
			name, err := v.sandbox.name(event.Name)
			if err != nil {
				continue
			}
			pending[name] = struct{}{}
			if timer == nil {
				timer = time.NewTimer(watchDebounce)
				timerC = timer.C
			} else {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(watchDebounce)
			}
		case <-timerC:
			flush()
			timer = nil
			timerC = nil
		case err, ok := <-entry.watcher.Errors:
			if !ok {
				return
			}
			if v.logger != nil {
				v.logger.Warn("javascript: watch error", zap.Error(err))
			}
		}
	}
}

// callWatcher runs one watcher callback on the VM goroutine, unless the watch
// was stopped in the meantime.
func (v *VM) callWatcher(id int, fn quickjs.Value, names []string) {
	v.watchMu.Lock()
	_, active := v.watchers[id]
	v.watchMu.Unlock()
	if !active {
		return
	}
	if _, err := fn.CallContext(v.loop.Context(), names); err != nil && v.logger != nil {
		v.logger.Error("javascript: watch callback failed", zap.Error(err))
	}
}

func (v *VM) removeWatcher(id int) {
	v.watchMu.Lock()
	entry := v.watchers[id]
	delete(v.watchers, id)
	v.watchMu.Unlock()
	if entry != nil {
		entry.stop()
	}
}

// stopAllWatchers cancels every watch the module registered. It runs when the
// module is replaced and when the VM closes.
func (v *VM) stopAllWatchers() {
	v.watchMu.Lock()
	entries := v.watchers
	v.watchers = make(map[int]*watchEntry)
	v.watchMu.Unlock()
	for _, entry := range entries {
		entry.stop()
	}
}
