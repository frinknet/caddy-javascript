package jobs

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/frinknet/caddy-javascript/pkg/bridge"
	"github.com/frinknet/caddy-javascript/pkg/files"
)

func newTestFS(t *testing.T) *files.FS {
	t.Helper()
	fsys, err := files.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fsys.Close() })
	return fsys
}

func TestRuntimeLoadsAndUnloads(t *testing.T) {
	loaded := make(chan int, 1)
	var unloaded atomic.Int32
	rt, err := Start([]byte(`
		app.get('/ready', function (req, res) {
			res.send('ok')
		})
	`), bridge.Config{Timeout: time.Second}, newTestFS(t), nil, 8, nil, Hooks{
		Loaded: func(p *bridge.Program) {
			layers, _ := p.Match("GET", "/ready")
			loaded <- len(layers)
		},
		Unloaded: func() { unloaded.Add(1) },
	})
	if err != nil {
		t.Fatal(err)
	}

	select {
	case layers := <-loaded:
		if layers != 1 {
			t.Fatalf("resident program registered %d layers, want 1", layers)
		}
	case <-time.After(time.Second):
		t.Fatal("resident program was never loaded")
	}

	rt.Stop()
	rt.Stop()
	if got := unloaded.Load(); got != 1 {
		t.Fatalf("resident program unloaded %d times, want 1", got)
	}
}

func TestStartRejectsBadScript(t *testing.T) {
	var unloaded atomic.Int32
	_, err := Start([]byte(`throw new Error('boom')`), bridge.Config{}, newTestFS(t), nil, 4, nil, Hooks{
		Unloaded: func() { unloaded.Add(1) },
	})
	if err == nil {
		t.Fatal("expected the load to fail")
	}
	if got := unloaded.Load(); got != 0 {
		t.Fatalf("unloaded %d times after a failed load, want 0", got)
	}
}

func TestStartRejectsEmptyQueue(t *testing.T) {
	_, err := Start([]byte(``), bridge.Config{}, newTestFS(t), nil, 0, nil, Hooks{})
	if err == nil {
		t.Fatal("expected an empty queue to be rejected")
	}
}

func TestRuntimeAdmitsAndRejectsAtBound(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	exec := func(_ *bridge.Program, _ Job) {
		if calls.Add(1) == 1 {
			started <- struct{}{}
		}
		<-release
	}

	rt, err := Start([]byte(``), bridge.Config{}, newTestFS(t), nil, 2, exec, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Stop()
	defer close(release)

	if !rt.Enqueue(Job{Name: "a"}) {
		t.Fatal("first job was rejected")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker never started the first job")
	}

	if !rt.Enqueue(Job{Name: "b"}) {
		t.Fatal("second job was rejected")
	}
	if !rt.Enqueue(Job{Name: "c"}) {
		t.Fatal("third job was rejected")
	}
	if rt.Enqueue(Job{Name: "d"}) {
		t.Fatal("job over the bound was admitted")
	}
}

func TestRuntimeDrainsQueuedJobsOnStop(t *testing.T) {
	var calls atomic.Int32
	exec := func(_ *bridge.Program, _ Job) {
		calls.Add(1)
	}

	rt, err := Start([]byte(``), bridge.Config{}, newTestFS(t), nil, 8, exec, Hooks{})
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 4; i++ {
		if !rt.Enqueue(Job{Name: "job"}) {
			t.Fatalf("job %d was rejected", i)
		}
	}
	rt.Stop()

	if got := calls.Load(); got != 4 {
		t.Fatalf("ran %d queued jobs, want 4", got)
	}
}
