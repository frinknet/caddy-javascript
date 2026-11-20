package vm

import (
	"crypto/sha256"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"go.uber.org/zap"
)

// treeDebounce coalesces the events one editor save produces.
const treeDebounce = 150 * time.Millisecond

// treeWatcher watches the directories holding the modules the VM loaded.
type treeWatcher struct {
	watcher *fsnotify.Watcher
	done    chan struct{}
	once    sync.Once
}

func (t *treeWatcher) stop() {
	t.once.Do(func() {
		close(t.done)
		_ = t.watcher.Close()
	})
}

// setReload installs the callback an edited module tree triggers and starts or
// stops the watcher. It is called after provisioning and on every join, so a
// generation that disables watch stops the previous watcher.
func (v *VM) setReload(fn func() error, watch bool) {
	v.reloadMu.Lock()
	v.reloadFn = fn
	v.reloadMu.Unlock()
	if watch && fn != nil {
		v.startTreeWatch()
		return
	}
	v.stopTreeWatch()
}

func (v *VM) stopTreeWatch() {
	v.reloadMu.Lock()
	t := v.treeWatch
	v.treeWatch = nil
	v.reloadMu.Unlock()
	if t != nil {
		t.stop()
	}
}

func (v *VM) startTreeWatch() {
	if v.closing.Load() {
		return
	}
	modules := v.sandbox.moduleHashes()
	if len(modules) == 0 {
		return
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		if v.logger != nil {
			v.logger.Warn("javascript: watch module tree", zap.Error(err))
		}
		return
	}
	added := make(map[string]struct{})
	for name := range modules {
		dir := filepath.Dir(v.sandbox.abs(name))
		if _, ok := added[dir]; ok {
			continue
		}
		if err := watcher.Add(dir); err != nil {
			if v.logger != nil {
				v.logger.Warn("javascript: watch module tree",
					zap.String("dir", dir), zap.Error(err))
			}
			continue
		}
		added[dir] = struct{}{}
	}
	if len(added) == 0 {
		_ = watcher.Close()
		return
	}

	t := &treeWatcher{watcher: watcher, done: make(chan struct{})}
	v.reloadMu.Lock()
	old := v.treeWatch
	v.treeWatch = t
	v.reloadMu.Unlock()
	if old != nil {
		old.stop()
	}
	go v.treeWatchLoop(t, modules)
}

func (v *VM) treeWatchLoop(t *treeWatcher, modules map[string][32]byte) {
	var timer *time.Timer
	var timerC <-chan time.Time
	pending := make(map[string]struct{})

	flush := func() {
		names := make([]string, 0, len(pending))
		for name := range pending {
			names = append(names, name)
		}
		pending = make(map[string]struct{})
		sort.Strings(names)
		if len(names) > 0 {
			v.triggerTreeReload(names)
		}
	}

	for {
		select {
		case <-t.done:
			if timer != nil {
				timer.Stop()
			}
			return
		case event, ok := <-t.watcher.Events:
			if !ok {
				return
			}
			name, err := v.sandbox.name(event.Name)
			if err != nil {
				continue
			}
			if _, tracked := modules[name]; !tracked {
				continue
			}
			pending[name] = struct{}{}
			if timer == nil {
				timer = time.NewTimer(treeDebounce)
				timerC = timer.C
			} else {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(treeDebounce)
			}
		case <-timerC:
			flush()
			timer = nil
			timerC = nil
		case err, ok := <-t.watcher.Errors:
			if !ok {
				return
			}
			if v.logger != nil {
				v.logger.Warn("javascript: watch module tree", zap.Error(err))
			}
		}
	}
}

func (v *VM) triggerTreeReload(paths []string) {
	if v.closing.Load() {
		return
	}
	v.reloadMu.Lock()
	fn := v.reloadFn
	v.reloadMu.Unlock()
	if fn == nil {
		return
	}
	v.metrics.reloads.Add(1)
	if v.logger != nil {
		v.logger.Info("javascript: module tree changed; reloading", zap.Strings("paths", paths))
	}
	go func() {
		if err := fn(); err != nil && v.logger != nil {
			v.logger.Warn("javascript: automatic reload rejected; previous generation keeps serving",
				zap.Error(err))
		}
	}()
}

// TreeChanged reports whether a file the loader read differs from what the VM
// evaluated. It lets a reload adopt a fresh VM after an edit while a generation
// that only changed routes keeps its state.
func (v *VM) TreeChanged() bool {
	for name, sum := range v.sandbox.moduleHashes() {
		src, err := v.sandbox.read(name)
		if err != nil || sha256.Sum256(src) != sum {
			return true
		}
	}
	return false
}

// abs resolves a sandbox path to a host path under the root.
func (s *sandbox) abs(name string) string {
	return filepath.Join(s.dir, filepath.FromSlash(strings.TrimPrefix(path.Clean(filepath.ToSlash(name)), "/")))
}
