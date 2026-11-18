package files

import (
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

type Watcher struct {
	w        *fsnotify.Watcher
	onChange func()
	debounce time.Duration
	quit     chan struct{}
	done     chan struct{}
	closeOne sync.Once
}

func NewWatcher(path string, debounce time.Duration, onChange func()) (*Watcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	if err := w.Add(filepath.Dir(path)); err != nil {
		_ = w.Close()
		return nil, err
	}

	wt := &Watcher{
		w:        w,
		onChange: onChange,
		debounce: debounce,
		quit:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go wt.loop(path)
	return wt, nil
}

func (wt *Watcher) loop(path string) {
	defer close(wt.done)

	target := filepath.Clean(path)
	var timer *time.Timer
	var fire <-chan time.Time

	stopTimer := func() {
		if timer != nil {
			timer.Stop()
			timer = nil
			fire = nil
		}
	}
	defer stopTimer()

	for {
		select {
		case event, ok := <-wt.w.Events:
			if !ok {
				return
			}
			if filepath.Clean(event.Name) != target {
				continue
			}
			if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
				continue
			}
			if timer == nil {
				timer = time.NewTimer(wt.debounce)
				fire = timer.C
			} else {
				timer.Reset(wt.debounce)
			}
		case <-fire:
			stopTimer()
			if wt.onChange != nil {
				wt.onChange()
			}
		case _, ok := <-wt.w.Errors:
			if !ok {
				return
			}
		case <-wt.quit:
			return
		}
	}
}

func (wt *Watcher) Close() error {
	wt.closeOne.Do(func() {
		close(wt.quit)
		_ = wt.w.Close()
	})
	<-wt.done
	return nil
}
