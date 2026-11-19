package bridge

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/frinknet/caddy-javascript/pkg/files"
)

func newJobsProgram(t *testing.T, cfg Config) *Program {
	t.Helper()
	fsys, err := files.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fsys.Close() })
	if cfg.Quit == nil {
		cfg.Quit = make(chan struct{})
	}
	return NewProgram(cfg, fsys, nil, nil)
}

func waitScheduled(t *testing.T, sched *fakeScheduler) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for sched.pending() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("timer callback was never scheduled")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestJobsEnqueueSerializesPayloadAndOptions(t *testing.T) {
	var gotName string
	var gotPayload []byte
	var gotOpts JobOptions
	p := newJobsProgram(t, Config{
		EnqueueJob: func(name string, payload []byte, opts JobOptions) (string, bool) {
			gotName = name
			gotPayload = payload
			gotOpts = opts
			return "job-1", true
		},
	})

	_, err := p.rt.RunString(`
		var id = jobs.enqueue('record', {n: 1, s: 'x'}, {attempts: 3, backoff: 25})
		if (id !== 'job-1') throw new Error('unexpected id: ' + id)
	`)
	if err != nil {
		t.Fatal(err)
	}
	if gotName != "record" {
		t.Fatalf("name = %q, want record", gotName)
	}
	if string(gotPayload) != `{"n":1,"s":"x"}` {
		t.Fatalf("payload = %s, want serialized object", gotPayload)
	}
	if gotOpts.MaxAttempts != 3 {
		t.Fatalf("MaxAttempts = %d, want 3", gotOpts.MaxAttempts)
	}
	if gotOpts.Backoff != 25*time.Millisecond {
		t.Fatalf("Backoff = %s, want 25ms", gotOpts.Backoff)
	}
}

func TestJobsEnqueueReportsUnavailableRuntime(t *testing.T) {
	p := newJobsProgram(t, Config{})
	value, err := p.rt.RunString(`
		var code = 'none'
		try { jobs.enqueue('record', {}) } catch (err) { code = err.code }
		code
	`)
	if err != nil {
		t.Fatal(err)
	}
	if got := value.String(); got != "JOBS_UNAVAILABLE" {
		t.Fatalf("code = %q, want JOBS_UNAVAILABLE", got)
	}
}

func TestJobsEnqueueReportsFullQueue(t *testing.T) {
	p := newJobsProgram(t, Config{
		EnqueueJob: func(string, []byte, JobOptions) (string, bool) { return "", false },
	})
	value, err := p.rt.RunString(`
		var code = 'none'
		try { jobs.enqueue('record', {}) } catch (err) { code = err.code }
		code
	`)
	if err != nil {
		t.Fatal(err)
	}
	if got := value.String(); got != "QUEUE_FULL" {
		t.Fatalf("code = %q, want QUEUE_FULL", got)
	}
}

func TestJobsEnqueueRejectsNonSerializablePayload(t *testing.T) {
	p := newJobsProgram(t, Config{
		EnqueueJob: func(string, []byte, JobOptions) (string, bool) { return "id", true },
	})
	value, err := p.rt.RunString(`
		var message = 'none'
		try { jobs.enqueue('record', function () {}) } catch (err) { message = err.message }
		message
	`)
	if err != nil {
		t.Fatal(err)
	}
	if got := value.String(); !strings.Contains(got, "JSON-serializable") {
		t.Fatalf("message = %q, want a serialization error", got)
	}
}

func TestJobsProcessAndRunSynchronousWorker(t *testing.T) {
	fsys, err := files.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fsys.Close() })
	p := NewProgram(Config{Quit: make(chan struct{})}, fsys, nil, nil)

	err = p.Load([]byte(`
		jobs.process('write', function (job) {
			fs.writeFileSync('result.txt', job.id + ':' + job.name + ':' + job.payload.value)
		})
	`))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.RunJob("job-1", "write", []byte(`{"value":"ok"}`), nil); err != nil {
		t.Fatal(err)
	}
	data, err := fsys.ReadFile("result.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "job-1:write:ok" {
		t.Fatalf("result = %q, want job-1:write:ok", data)
	}
}

func TestJobsProcessRejectsDuplicateWorker(t *testing.T) {
	p := newJobsProgram(t, Config{})
	value, err := p.rt.RunString(`
		jobs.process('write', function () {})
		var message = 'none'
		try { jobs.process('write', function () {}) } catch (err) { message = err.message }
		message
	`)
	if err != nil {
		t.Fatal(err)
	}
	if got := value.String(); !strings.Contains(got, "already registered") {
		t.Fatalf("message = %q, want a duplicate worker error", got)
	}
}

func TestJobsAsyncWorkerCompletesAfterPromise(t *testing.T) {
	sched := &fakeScheduler{}
	fsys, err := files.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fsys.Close() })
	p := NewProgram(Config{Quit: make(chan struct{}), Timeout: time.Second}, fsys, sched, nil)

	err = p.Load([]byte(`
		jobs.process('async', function (job) {
			return new Promise(function (resolve) {
				setTimeout(function () { resolve() }, 0)
			})
		})
	`))
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	err = p.RunJob("job-1", "async", []byte("null"), func(err error) { done <- err })
	if !errors.Is(err, ErrAsyncPending) {
		t.Fatalf("RunJob error = %v, want ErrAsyncPending", err)
	}
	waitScheduled(t, sched)
	sched.run()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("completion error = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("async worker never completed")
	}
}

func TestJobsAsyncWorkerReportsRejection(t *testing.T) {
	sched := &fakeScheduler{}
	fsys, err := files.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fsys.Close() })
	p := NewProgram(Config{Quit: make(chan struct{}), Timeout: time.Second}, fsys, sched, nil)

	err = p.Load([]byte(`
		jobs.process('async', function () {
			return new Promise(function (resolve, reject) {
				setTimeout(function () { reject(new Error('worker failed')) }, 0)
			})
		})
	`))
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	err = p.RunJob("job-1", "async", nil, func(err error) { done <- err })
	if !errors.Is(err, ErrAsyncPending) {
		t.Fatalf("RunJob error = %v, want ErrAsyncPending", err)
	}
	waitScheduled(t, sched)
	sched.run()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "worker failed") {
			t.Fatalf("completion error = %v, want worker failure", err)
		}
	case <-time.After(time.Second):
		t.Fatal("async worker never completed")
	}
}

func TestJobsRunUnknownWorker(t *testing.T) {
	p := newJobsProgram(t, Config{})
	err := p.RunJob("job-1", "missing", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "no worker registered") {
		t.Fatalf("error = %v, want an unknown worker error", err)
	}
}
