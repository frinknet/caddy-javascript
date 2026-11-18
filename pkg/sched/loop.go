package sched

import (
	"sync"
	"time"
)

type Task = func()

type Loop struct {
	queue     chan Task
	reload    chan struct{}
	quit      chan struct{}
	done      chan struct{}
	stopOnce  sync.Once
	OnPanic   func(recovered any)
	drainWait time.Duration
}

func New(queueSize int) *Loop {
	return &Loop{
		queue:     make(chan Task, queueSize),
		reload:    make(chan struct{}, 1),
		quit:      make(chan struct{}),
		done:      make(chan struct{}),
		drainWait: 5 * time.Second,
	}
}

func (l *Loop) Quit() <-chan struct{} {
	return l.quit
}

func (l *Loop) Submit(t Task) bool {
	select {
	case <-l.quit:
		return false
	default:
	}
	select {
	case l.queue <- t:
		return true
	default:
		return false
	}
}

func (l *Loop) SignalReload() {
	select {
	case l.reload <- struct{}{}:
	default:
	}
}

func (l *Loop) Run(onReload func()) {
	defer close(l.done)
	for {
		select {
		case t := <-l.queue:
			l.runTask(t)
		case <-l.reload:
			if onReload != nil {
				l.runTask(func() { onReload() })
			}
		case <-l.quit:
			l.drain()
			return
		}
	}
}

func (l *Loop) Stop() {
	l.stopOnce.Do(func() { close(l.quit) })
	<-l.done
}

func (l *Loop) runTask(t Task) {
	defer func() {
		if r := recover(); r != nil && l.OnPanic != nil {
			l.OnPanic(r)
		}
	}()
	t()
}

func (l *Loop) drain() {
	deadline := time.NewTimer(l.drainWait)
	defer deadline.Stop()
	for {
		select {
		case t := <-l.queue:
			l.runTask(t)
		case <-deadline.C:
			return
		default:
			return
		}
	}
}
