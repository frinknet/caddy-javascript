package vm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestCloseDrainsAnInflightRequest(t *testing.T) {
	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", `export default function handler(http) {
	const end = Date.now() + 80
	while (Date.now() < end) {}
	http.respond(200, {}, "ok")
}
`)
	v, err := New(Config{ScriptPath: entry, RootDir: dir, RequireHandler: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	type result struct {
		resp *Response
		err  error
	}
	handled := make(chan result, 1)
	go func() {
		resp, err := v.Handle(context.Background(), entry, testRequest(http.MethodGet, "/", nil))
		handled <- result{resp: resp, err: err}
	}()

	deadline := time.Now().Add(2 * time.Second)
	for v.Stats().Inflight == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the request never became inflight")
		}
		time.Sleep(time.Millisecond)
	}

	closed := make(chan error, 1)
	go func() { closed <- v.Close() }()

	select {
	case got := <-handled:
		if got.err != nil {
			t.Fatalf("in-flight request failed during shutdown: %v", got.err)
		}
		if got.resp == nil || string(got.resp.Body) != "ok" {
			t.Fatalf("in-flight response = %+v", got.resp)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not let the in-flight request finish")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return after draining")
	}
}

func TestCloseRefusesNewRequests(t *testing.T) {
	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", `export default function handler(http) {
	http.respond(200, {}, "ok")
}
`)
	v, err := New(Config{ScriptPath: entry, RootDir: dir, RequireHandler: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := v.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := v.Handle(context.Background(), entry, testRequest(http.MethodGet, "/", nil)); !errors.Is(err, ErrClosed) {
		t.Fatalf("Handle after Close = %v, want ErrClosed", err)
	}
	if got := v.Stats().Closed; got == 0 {
		t.Error("the closed counter did not move")
	}
}

func TestQueueFullIsCounted(t *testing.T) {
	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", `export default function handler(http) {
	const end = Date.now() + 100
	while (Date.now() < end) {}
	http.respond(200, {}, "ok")
}
`)
	v, err := New(Config{ScriptPath: entry, RootDir: dir, RequireHandler: true, QueueSize: 1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer v.Close()

	done := make(chan error, 1)
	go func() {
		_, err := v.Handle(context.Background(), entry, testRequest(http.MethodGet, "/", nil))
		done <- err
	}()

	deadline := time.Now().Add(2 * time.Second)
	for v.QueueDepth() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the first request never occupied the queue")
		}
		time.Sleep(time.Millisecond)
	}

	if _, err := v.Handle(context.Background(), entry, testRequest(http.MethodGet, "/", nil)); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("second request = %v, want ErrQueueFull", err)
	}
	if got := v.Stats().QueueFull; got != 1 {
		t.Errorf("queue full counter = %d, want 1", got)
	}
	if err := <-done; err != nil {
		t.Fatalf("first request: %v", err)
	}
}

func TestConcurrentRequestsStayIndependent(t *testing.T) {
	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", `let hits = 0
const ch = new BroadcastChannel("stress")
let heard = 0
ch.onmessage = () => { heard++ }
setInterval(() => ch.postMessage("tick"), 1)
export default function handler(http) {
	hits++
	http.vars.seen = http.path
	http.respond(200, {}, http.path + "|" + hits + "|" + (heard > 0))
}
`)
	v, err := New(Config{ScriptPath: entry, RootDir: dir, RequireHandler: true, QueueSize: 64})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer v.Close()

	const goroutines = 16
	const perGoroutine = 20
	seen := make(chan string, goroutines*perGoroutine)
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				path := fmt.Sprintf("/g%d-%d", id, j)
				resp, err := v.Handle(context.Background(), entry, testRequest(http.MethodGet, path, nil))
				if err != nil {
					errs <- err
					return
				}
				seen <- string(resp.Body)
			}
		}(i)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent requests did not finish")
	}
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent request: %v", err)
	}
	close(seen)

	counts := make(map[string]int)
	total := 0
	for body := range seen {
		counts[body]++
		total++
	}
	if total != goroutines*perGoroutine {
		t.Fatalf("responses = %d, want %d", total, goroutines*perGoroutine)
	}
	for body, count := range counts {
		if count != 1 {
			t.Errorf("body %q repeated %d times", body, count)
		}
	}
	stats := v.Stats()
	if stats.Requests != goroutines*perGoroutine {
		t.Errorf("requests = %d, want %d", stats.Requests, goroutines*perGoroutine)
	}
	if stats.Failures != 0 {
		t.Errorf("failures = %d, want 0", stats.Failures)
	}
}

func TestConcurrentVMsDoNotShareRequests(t *testing.T) {
	dirs := make([]string, 2)
	entries := make([]string, 2)
	for i := range dirs {
		dirs[i] = t.TempDir()
		entries[i] = writeModule(t, dirs[i], "main.js", fmt.Sprintf(`export default function handler(http) {
	http.respond(200, {}, %q + ":" + http.path)
}
`, fmt.Sprintf("vm%d", i)))
	}

	vms := make([]*VM, len(dirs))
	for i := range dirs {
		v, err := New(Config{ScriptPath: entries[i], RootDir: dirs[i], RequireHandler: true, QueueSize: 64})
		if err != nil {
			t.Fatalf("New %d: %v", i, err)
		}
		vms[i] = v
		defer v.Close()
	}

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			vmIndex := id % len(vms)
			path := fmt.Sprintf("/req-%d", id)
			resp, err := vms[vmIndex].Handle(context.Background(), entries[vmIndex], testRequest(http.MethodGet, path, nil))
			if err != nil {
				errs <- err
				return
			}
			want := fmt.Sprintf("vm%d:%s", vmIndex, path)
			if got := string(resp.Body); got != want {
				errs <- fmt.Errorf("body = %q, want %q", got, want)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent VM request: %v", err)
	}
}
