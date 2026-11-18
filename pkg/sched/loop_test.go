package sched

import (
	"sync"
	"testing"
	"time"
)

func TestLoopDrainsOnStop(t *testing.T) {
	l := New(16)

	release := make(chan struct{})
	if !l.Submit(func() { <-release }) {
		t.Fatal("failed to submit blocker")
	}

	go l.Run(nil)
	time.Sleep(50 * time.Millisecond)

	var mu sync.Mutex
	ran := 0
	for i := 0; i < 5; i++ {
		if !l.Submit(func() {
			mu.Lock()
			ran++
			mu.Unlock()
		}) {
			t.Fatalf("failed to submit task %d", i)
		}
	}

	close(release)
	l.Stop()

	mu.Lock()
	defer mu.Unlock()
	if ran != 5 {
		t.Fatalf("drained %d tasks, want 5", ran)
	}
}

func TestSubmitAfterStopFails(t *testing.T) {
	l := New(1)
	go l.Run(nil)
	l.Stop()

	if l.Submit(func() {}) {
		t.Fatal("submit after stop should fail")
	}
}

func TestSubmitFailsWhenFull(t *testing.T) {
	l := New(1)

	if !l.Submit(func() {}) {
		t.Fatal("failed to fill queue")
	}
	if l.Submit(func() {}) {
		t.Fatal("submit should fail when queue is full")
	}
}

func TestPanicRecovered(t *testing.T) {
	l := New(4)

	var mu sync.Mutex
	var recovered any
	l.OnPanic = func(r any) {
		mu.Lock()
		recovered = r
		mu.Unlock()
	}

	done := make(chan struct{})
	l.Submit(func() { panic("boom") })
	l.Submit(func() { close(done) })

	go l.Run(nil)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("loop did not continue after panic")
	}
	l.Stop()

	mu.Lock()
	defer mu.Unlock()
	if recovered != "boom" {
		t.Fatalf("recovered = %v, want boom", recovered)
	}
}
