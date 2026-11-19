package bridge

import (
	"errors"
	"fmt"
	"time"

	"github.com/grafana/sobek"
)

// ErrAsyncPending reports a job whose worker returned a promise that is not
// settled yet. The caller must wait for the completion callback instead of
// acking the job immediately.
var ErrAsyncPending = errors.New("javascript: job running asynchronously")

// errJobFailed is the fallback cause for a rejected promise without a reason.
var errJobFailed = errors.New("javascript: job failed")

func (p *Program) registerJobs() {
	jobsObj := p.rt.NewObject()

	jobsObj.Set("process", func(call sobek.FunctionCall) sobek.Value {
		name := call.Argument(0).String()
		if name == "" {
			panic(p.rt.NewTypeError("jobs.process requires a worker name"))
		}
		fn := call.Argument(1)
		if _, ok := sobek.AssertFunction(fn); !ok {
			panic(p.rt.NewTypeError("jobs.process requires a function"))
		}
		if _, exists := p.workers[name]; exists {
			panic(p.rt.NewTypeError("jobs.process: worker " + name + " is already registered"))
		}
		p.workers[name] = fn
		return jobsObj
	})

	jobsObj.Set("enqueue", func(call sobek.FunctionCall) sobek.Value {
		name := call.Argument(0).String()
		if name == "" {
			panic(p.rt.NewTypeError("jobs.enqueue requires a worker name"))
		}
		payload, err := p.serializeJobPayload(call.Argument(1))
		if err != nil {
			panic(p.rt.NewTypeError("jobs.enqueue: " + err.Error()))
		}
		opts, err := p.jobOptions(call.Argument(2))
		if err != nil {
			panic(p.rt.NewTypeError("jobs.enqueue: " + err.Error()))
		}
		if p.cfg.EnqueueJob == nil {
			panic(p.jobError("JOBS_UNAVAILABLE", "javascript: background runtime unavailable"))
		}
		id, ok := p.cfg.EnqueueJob(name, payload, opts)
		if !ok {
			panic(p.jobError("QUEUE_FULL", "javascript: job queue full"))
		}
		return p.rt.ToValue(id)
	})

	p.jobsObj = jobsObj
	_ = p.rt.Set("jobs", jobsObj)
}

// RunJob invokes the worker registered under name. A synchronous worker
// returns nil or its error. A worker that returns a pending promise gets the
// ErrAsyncPending sentinel, and done is called when that promise settles.
func (p *Program) RunJob(id, name string, payload []byte, done func(error)) error {
	run := func() error {
		return p.invokeJob(id, name, payload, done)
	}
	if p.cfg.Timeout <= 0 {
		return run()
	}
	var jobErr error
	runErr := p.Run(p.cfg.Timeout, func() error {
		jobErr = run()
		return jobErr
	})
	if runErr != nil {
		return runErr
	}
	return jobErr
}

func (p *Program) invokeJob(id, name string, payload []byte, done func(error)) error {
	worker, ok := p.workers[name]
	if !ok {
		return fmt.Errorf("javascript: no worker registered for job %q", name)
	}
	fn, ok := sobek.AssertFunction(worker)
	if !ok {
		return fmt.Errorf("javascript: worker %q is not callable", name)
	}

	job := p.rt.NewObject()
	_ = job.Set("id", id)
	_ = job.Set("name", name)
	if len(payload) > 0 {
		value, err := p.parseJSON(payload)
		if err != nil {
			return fmt.Errorf("javascript: job %s payload: %w", id, err)
		}
		_ = job.Set("payload", value)
	} else {
		_ = job.Set("payload", sobek.Null())
	}

	ret, err := fn(sobek.Undefined(), job)
	if err != nil {
		return err
	}
	return p.settleJob(ret, done)
}

func (p *Program) settleJob(ret sobek.Value, done func(error)) error {
	if isNilValue(ret) {
		return nil
	}
	obj := ret.ToObject(p.rt)
	if obj == nil {
		return nil
	}
	then, ok := sobek.AssertFunction(obj.Get("then"))
	if !ok {
		return nil
	}
	if promise, ok := obj.Export().(*sobek.Promise); ok {
		switch promise.State() {
		case sobek.PromiseStateFulfilled:
			return nil
		case sobek.PromiseStateRejected:
			return p.promiseError(promise.Result())
		}
	}

	onFulfilled := func(call sobek.FunctionCall) sobek.Value {
		if done != nil {
			done(nil)
		}
		return sobek.Undefined()
	}
	onRejected := func(call sobek.FunctionCall) sobek.Value {
		if done != nil {
			done(p.promiseError(call.Argument(0)))
		}
		return sobek.Undefined()
	}
	if _, err := then(obj, p.rt.ToValue(onFulfilled), p.rt.ToValue(onRejected)); err != nil {
		return err
	}
	return ErrAsyncPending
}

func (p *Program) serializeJobPayload(value sobek.Value) ([]byte, error) {
	if isNilValue(value) {
		return []byte("null"), nil
	}
	jsonObj := p.rt.Get("JSON").ToObject(p.rt)
	stringify, ok := sobek.AssertFunction(jsonObj.Get("stringify"))
	if !ok {
		return nil, errors.New("JSON.stringify is unavailable")
	}
	out, err := stringify(sobek.Undefined(), value)
	if err != nil {
		return nil, err
	}
	if sobek.IsUndefined(out) {
		return nil, errors.New("payload must be JSON-serializable")
	}
	return []byte(out.String()), nil
}

func (p *Program) jobOptions(value sobek.Value) (JobOptions, error) {
	opts := JobOptions{MaxAttempts: 1}
	if isNilValue(value) {
		return opts, nil
	}
	obj := value.ToObject(p.rt)
	if obj == nil {
		return opts, errors.New("options must be an object")
	}
	if attempts := obj.Get("attempts"); !isNilValue(attempts) {
		n := int(attempts.ToInteger())
		if n < 1 {
			return opts, errors.New("attempts must be at least 1")
		}
		opts.MaxAttempts = n
	}
	if backoff := obj.Get("backoff"); !isNilValue(backoff) {
		ms := backoff.ToInteger()
		if ms < 0 {
			return opts, errors.New("backoff must not be negative")
		}
		opts.Backoff = time.Duration(ms) * time.Millisecond
	}
	return opts, nil
}

func (p *Program) promiseError(value sobek.Value) error {
	if isNilValue(value) {
		return errJobFailed
	}
	if obj := value.ToObject(p.rt); obj != nil {
		if message := obj.Get("message"); !isNilValue(message) {
			return errors.New(message.String())
		}
	}
	return errors.New(value.String())
}

func (p *Program) jobError(code, message string) *sobek.Object {
	obj := p.rt.NewGoError(errors.New(message))
	_ = obj.Set("name", "Error")
	_ = obj.Set("code", code)
	return obj
}
