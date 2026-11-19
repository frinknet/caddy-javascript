package jobs

import (
	"errors"
	"sync"
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

func waitState(t *testing.T, job *Job, want State) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if job.State() == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("job state = %s, want %s (attempts %d, err %v)", job.State(), want, job.Attempts(), job.Err())
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
	exec := func(_ *bridge.Program, _ *Job) error {
		if calls.Add(1) == 1 {
			started <- struct{}{}
		}
		<-release
		return nil
	}

	rt, err := Start([]byte(``), bridge.Config{}, newTestFS(t), nil, 2, exec, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Stop()
	defer close(release)

	if !rt.Enqueue(NewJob("a", nil)) {
		t.Fatal("first job was rejected")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("worker never started the first job")
	}

	if !rt.Enqueue(NewJob("b", nil)) {
		t.Fatal("second job was rejected")
	}
	if !rt.Enqueue(NewJob("c", nil)) {
		t.Fatal("third job was rejected")
	}
	if rt.Enqueue(NewJob("d", nil)) {
		t.Fatal("job over the bound was admitted")
	}
	if rt.Enqueue(nil) {
		t.Fatal("a nil job was admitted")
	}
}

func TestRuntimeAcksSuccessfulJobs(t *testing.T) {
	var calls atomic.Int32
	exec := func(_ *bridge.Program, _ *Job) error {
		calls.Add(1)
		return nil
	}

	rt, err := Start([]byte(``), bridge.Config{}, newTestFS(t), nil, 8, exec, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Stop()

	job := NewJob("ok", nil)
	if !rt.Enqueue(job) {
		t.Fatal("job was rejected")
	}
	waitState(t, job, StateSucceeded)
	if got := calls.Load(); got != 1 {
		t.Fatalf("executor ran %d times, want 1", got)
	}
	if got := job.Attempts(); got != 1 {
		t.Fatalf("attempts = %d, want 1", got)
	}
	if got := job.Err(); got != nil {
		t.Fatalf("err = %v, want nil after success", got)
	}
}

func TestRuntimeRetriesFailedJobs(t *testing.T) {
	var calls atomic.Int32
	exec := func(_ *bridge.Program, _ *Job) error {
		if calls.Add(1) < 3 {
			return errors.New("boom")
		}
		return nil
	}

	rt, err := Start([]byte(``), bridge.Config{}, newTestFS(t), nil, 8, exec, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Stop()

	job := NewJob("retry", nil)
	job.MaxAttempts = 3
	job.Backoff = time.Millisecond
	if !rt.Enqueue(job) {
		t.Fatal("job was rejected")
	}

	waitState(t, job, StateSucceeded)
	if got := calls.Load(); got != 3 {
		t.Fatalf("executor ran %d times, want 3", got)
	}
	if got := job.Attempts(); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
	if got := job.Err(); got != nil {
		t.Fatalf("err = %v, want nil after the retry succeeded", got)
	}
}

func TestRuntimeFailsExhaustedJobs(t *testing.T) {
	exec := func(_ *bridge.Program, _ *Job) error {
		return errors.New("boom")
	}

	rt, err := Start([]byte(``), bridge.Config{}, newTestFS(t), nil, 8, exec, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Stop()

	job := NewJob("fail", nil)
	job.MaxAttempts = 2
	job.Backoff = time.Millisecond
	if !rt.Enqueue(job) {
		t.Fatal("job was rejected")
	}

	waitState(t, job, StateFailed)
	if got := job.Attempts(); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
	if got := job.Err(); got == nil || got.Error() != "boom" {
		t.Fatalf("err = %v, want boom", got)
	}
}

func TestRuntimeSkipsCancelledJobs(t *testing.T) {
	seen := make(chan string, 2)
	exec := func(_ *bridge.Program, job *Job) error {
		seen <- job.Name
		return nil
	}

	rt, err := Start([]byte(``), bridge.Config{}, newTestFS(t), nil, 8, exec, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Stop()

	cancelled := NewJob("cancelled", nil)
	cancelled.Cancel()
	if !rt.Enqueue(cancelled) || !rt.Enqueue(NewJob("after", nil)) {
		t.Fatal("job was rejected")
	}

	select {
	case name := <-seen:
		if name != "after" {
			t.Fatalf("executor ran %q, want the cancelled job to be skipped", name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the following job never ran")
	}
	if got := cancelled.State(); got != StateCancelled {
		t.Fatalf("cancelled job state = %s, want cancelled", got)
	}
}

func TestRuntimeSkipsExpiredJobs(t *testing.T) {
	seen := make(chan string, 2)
	exec := func(_ *bridge.Program, job *Job) error {
		seen <- job.Name
		return nil
	}

	rt, err := Start([]byte(``), bridge.Config{}, newTestFS(t), nil, 8, exec, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Stop()

	expired := NewJob("expired", nil)
	expired.Deadline = time.Now().Add(-time.Second)
	if !rt.Enqueue(expired) || !rt.Enqueue(NewJob("after", nil)) {
		t.Fatal("job was rejected")
	}

	select {
	case name := <-seen:
		if name != "after" {
			t.Fatalf("executor ran %q, want the expired job to be skipped", name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the following job never ran")
	}
	if got := expired.State(); got != StateFailed {
		t.Fatalf("expired job state = %s, want failed", got)
	}
	if !errors.Is(expired.Err(), ErrJobDeadline) {
		t.Fatalf("expired job err = %v, want the deadline error", expired.Err())
	}
}

func TestRuntimeCancelsRunningJob(t *testing.T) {
	started := make(chan struct{})
	exec := func(_ *bridge.Program, job *Job) error {
		close(started)
		<-job.Context().Done()
		return job.Context().Err()
	}

	rt, err := Start([]byte(``), bridge.Config{}, newTestFS(t), nil, 8, exec, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Stop()

	job := NewJob("cancel", nil)
	if !rt.Enqueue(job) {
		t.Fatal("job was rejected")
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("job never started")
	}
	if !job.Cancel() {
		t.Fatal("cancel reported false for a running job")
	}
	waitState(t, job, StateCancelled)
}

func TestRuntimeStopsDuringBackoff(t *testing.T) {
	first := make(chan struct{}, 1)
	exec := func(_ *bridge.Program, _ *Job) error {
		first <- struct{}{}
		return errors.New("boom")
	}

	rt, err := Start([]byte(``), bridge.Config{}, newTestFS(t), nil, 8, exec, Hooks{})
	if err != nil {
		t.Fatal(err)
	}

	job := NewJob("slow", nil)
	job.MaxAttempts = 5
	job.Backoff = time.Hour
	if !rt.Enqueue(job) {
		t.Fatal("job was rejected")
	}

	select {
	case <-first:
	case <-time.After(2 * time.Second):
		t.Fatal("job never started")
	}
	waitState(t, job, StateRetrying)

	done := make(chan struct{})
	go func() {
		rt.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop blocked during the retry backoff")
	}
	if got := job.State(); got != StateCancelled {
		t.Fatalf("job state = %s, want cancelled", got)
	}
}

func TestRuntimeDeadlineStopsRetries(t *testing.T) {
	exec := func(_ *bridge.Program, _ *Job) error {
		return errors.New("boom")
	}

	rt, err := Start([]byte(``), bridge.Config{}, newTestFS(t), nil, 8, exec, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Stop()

	job := NewJob("late", nil)
	job.MaxAttempts = 5
	job.Backoff = time.Hour
	job.Deadline = time.Now().Add(50 * time.Millisecond)
	if !rt.Enqueue(job) {
		t.Fatal("job was rejected")
	}

	waitState(t, job, StateFailed)
	if got := job.Attempts(); got != 1 {
		t.Fatalf("attempts = %d, want 1", got)
	}
	if !errors.Is(job.Err(), ErrJobDeadline) {
		t.Fatalf("err = %v, want the deadline error", job.Err())
	}
}

func TestRuntimeDrainsQueuedJobsOnStop(t *testing.T) {
	var calls atomic.Int32
	exec := func(_ *bridge.Program, _ *Job) error {
		calls.Add(1)
		return nil
	}

	rt, err := Start([]byte(``), bridge.Config{}, newTestFS(t), nil, 8, exec, Hooks{})
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 4; i++ {
		if !rt.Enqueue(NewJob("job", nil)) {
			t.Fatalf("job %d was rejected", i)
		}
	}
	rt.Stop()

	if got := calls.Load(); got != 4 {
		t.Fatalf("ran %d queued jobs, want 4", got)
	}
}

func TestRuntimeWaitsForAsyncJob(t *testing.T) {
	started := make(chan *Job, 2)
	exec := func(_ *bridge.Program, job *Job) error {
		started <- job
		return bridge.ErrAsyncPending
	}

	rt, err := Start([]byte(``), bridge.Config{}, newTestFS(t), nil, 8, exec, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Stop()

	job := NewJob("async", nil)
	if !rt.Enqueue(job) {
		t.Fatal("job was rejected")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("job never started")
	}

	time.Sleep(10 * time.Millisecond)
	if got := job.State(); got != StateRunning {
		t.Fatalf("state = %s while the promise is pending, want running", got)
	}
	if err := job.Ack(); err != nil {
		t.Fatal(err)
	}
	waitState(t, job, StateSucceeded)

	next := NewJob("next", nil)
	if !rt.Enqueue(next) {
		t.Fatal("second job was rejected")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("second job never started after the async job completed")
	}
	if err := next.Ack(); err != nil {
		t.Fatal(err)
	}
	waitState(t, next, StateSucceeded)
}

func TestRuntimeCancelsPendingAsyncJob(t *testing.T) {
	started := make(chan *Job, 1)
	exec := func(_ *bridge.Program, job *Job) error {
		started <- job
		return bridge.ErrAsyncPending
	}

	rt, err := Start([]byte(``), bridge.Config{}, newTestFS(t), nil, 8, exec, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Stop()

	job := NewJob("async", nil)
	if !rt.Enqueue(job) {
		t.Fatal("job was rejected")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("job never started")
	}

	job.Cancel()
	waitState(t, job, StateCancelled)

	next := NewJob("next", nil)
	if !rt.Enqueue(next) {
		t.Fatal("job rejected after the async job was cancelled")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("runtime did not advance after an async job was cancelled")
	}
	_, _ = next.Fail(errors.New("done"))
	waitState(t, next, StateFailed)
}

func TestRuntimeReloadSwapsProgram(t *testing.T) {
	loaded := make(chan *bridge.Program, 4)
	ranOn := make(chan *bridge.Program, 4)
	exec := func(prog *bridge.Program, _ *Job) error {
		ranOn <- prog
		return nil
	}

	rt, err := Start([]byte(`app.get('/old', function (req, res) { res.send('old') })`),
		bridge.Config{}, newTestFS(t), nil, 8, exec, Hooks{
			Loaded: func(p *bridge.Program) { loaded <- p },
		})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Stop()

	old := <-loaded
	if err := rt.Reload([]byte(`app.get('/new', function (req, res) { res.send('new') })`)); err != nil {
		t.Fatal(err)
	}
	var current *bridge.Program
	select {
	case current = <-loaded:
	case <-time.After(time.Second):
		t.Fatal("reload never loaded a program")
	}
	if current == old {
		t.Fatal("reload kept the previous program")
	}

	job := NewJob("after-reload", nil)
	if !rt.Enqueue(job) {
		t.Fatal("job was rejected")
	}
	waitState(t, job, StateSucceeded)
	if got := <-ranOn; got != current {
		t.Fatal("job ran on a program other than the reloaded one")
	}
	if got := job.Generation; got != 2 {
		t.Fatalf("job generation = %d, want 2", got)
	}
}

func TestRuntimeReloadFencesStaleJobs(t *testing.T) {
	var calls atomic.Int32
	exec := func(_ *bridge.Program, _ *Job) error {
		calls.Add(1)
		return nil
	}

	rt, err := Start([]byte(``), bridge.Config{}, newTestFS(t), nil, 8, exec, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Stop()

	if err := rt.Reload([]byte(``)); err != nil {
		t.Fatal(err)
	}

	stale := NewJob("stale", nil)
	stale.Generation = 1
	if !rt.Enqueue(stale) {
		t.Fatal("stale job was rejected")
	}
	waitState(t, stale, StateCancelled)
	if !errors.Is(stale.Err(), ErrJobStale) {
		t.Fatalf("stale job err = %v, want the stale-generation error", stale.Err())
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("executor ran %d stale jobs, want 0", got)
	}

	fresh := NewJob("fresh", nil)
	if !rt.Enqueue(fresh) {
		t.Fatal("fresh job was rejected")
	}
	waitState(t, fresh, StateSucceeded)
	if got := calls.Load(); got != 1 {
		t.Fatalf("executor ran %d jobs, want 1", got)
	}
	if got := fresh.Generation; got != 2 {
		t.Fatalf("fresh job generation = %d, want 2", got)
	}
}

func TestRuntimeReloadKeepsProgramOnFailure(t *testing.T) {
	loaded := make(chan *bridge.Program, 4)
	ranOn := make(chan *bridge.Program, 4)
	exec := func(prog *bridge.Program, _ *Job) error {
		ranOn <- prog
		return nil
	}

	rt, err := Start([]byte(``), bridge.Config{}, newTestFS(t), nil, 8, exec, Hooks{
		Loaded: func(p *bridge.Program) { loaded <- p },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Stop()

	old := <-loaded
	if err := rt.Reload([]byte(`throw new Error('boom')`)); err == nil {
		t.Fatal("expected the reload to fail")
	}
	select {
	case <-loaded:
		t.Fatal("a failed reload loaded a program")
	default:
	}

	job := NewJob("after-failure", nil)
	if !rt.Enqueue(job) {
		t.Fatal("job was rejected")
	}
	waitState(t, job, StateSucceeded)
	if got := <-ranOn; got != old {
		t.Fatal("job ran on a program other than the previous one")
	}
	if got := job.Generation; got != 1 {
		t.Fatalf("job generation = %d, want 1", got)
	}
}

func TestRuntimeReloadAndEnqueueRace(t *testing.T) {
	src := []byte(`app.get('/x', function (req, res) { res.send('x') })`)
	exec := func(_ *bridge.Program, _ *Job) error { return nil }

	rt, err := Start(src, bridge.Config{}, newTestFS(t), nil, 8, exec, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Stop()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			rt.Enqueue(NewJob("job", nil))
		}()
		go func() {
			defer wg.Done()
			_ = rt.Reload(src)
		}()
	}
	wg.Wait()
}

func TestRuntimeReloadAfterStop(t *testing.T) {
	rt, err := Start([]byte(``), bridge.Config{}, newTestFS(t), nil, 8, nil, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	rt.Stop()

	if err := rt.Reload([]byte(``)); !errors.Is(err, ErrRuntimeStopped) {
		t.Fatalf("reload after stop = %v, want the stopped error", err)
	}
}
