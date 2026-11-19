package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// DefaultMaxAttempts is the number of attempts a job gets unless the
	// producer asks for more. Retries are opt-in.
	DefaultMaxAttempts = 1
	// DefaultBackoff is the delay before the first retry, doubled for each
	// further retry.
	DefaultBackoff = 250 * time.Millisecond
	// MaxBackoff caps the exponential retry delay.
	MaxBackoff = 30 * time.Second
)

// State is the lifecycle position of a job.
type State uint8

const (
	// StatePending is a job admitted but not yet started.
	StatePending State = iota
	// StateRunning is a job whose current attempt is executing.
	StateRunning
	// StateRetrying is a failed job waiting for its next attempt.
	StateRetrying
	// StateSucceeded is a job acked after a successful attempt.
	StateSucceeded
	// StateFailed is a job that ran out of attempts or missed its deadline.
	StateFailed
	// StateCancelled is a job cancelled before it finished.
	StateCancelled
)

func (s State) String() string {
	switch s {
	case StatePending:
		return "pending"
	case StateRunning:
		return "running"
	case StateRetrying:
		return "retrying"
	case StateSucceeded:
		return "succeeded"
	case StateFailed:
		return "failed"
	case StateCancelled:
		return "cancelled"
	default:
		return fmt.Sprintf("state(%d)", s)
	}
}

func (s State) terminal() bool {
	return s == StateSucceeded || s == StateFailed || s == StateCancelled
}

var (
	// ErrJobCancelled reports a job cancelled before it finished.
	ErrJobCancelled = errors.New("javascript: job cancelled")
	// ErrJobDeadline reports a job that reached its deadline.
	ErrJobDeadline = errors.New("javascript: job deadline exceeded")
	// ErrJobFailed reports a failed job without a specific cause.
	ErrJobFailed = errors.New("javascript: job failed")
	// ErrJobQueueFull reports a job that could not be queued again.
	ErrJobQueueFull = errors.New("javascript: job queue full")
	// ErrJobStale reports a job bound to a superseded runtime generation.
	ErrJobStale = errors.New("javascript: job belongs to a previous runtime generation")
	// ErrJobInvalidTransition reports an operation on a job in the wrong state.
	ErrJobInvalidTransition = errors.New("javascript: invalid job state transition")
)

var jobSeq atomic.Uint64

// Job is one unit of background work. The payload is already serialized so a
// request runtime can hand it over without sharing live objects.
//
// The configuration fields are read by the supervisor without holding the
// lock, so set them before the job is enqueued: ID, Name, Payload, Generation,
// Deadline, AsyncBudget, MaxAttempts, and Backoff.
type Job struct {
	// ID is stable across retries. NewJob assigns a random one when the
	// producer leaves it empty.
	ID string
	// Name selects the registered worker.
	Name string
	// Payload is the serialized input for the worker.
	Payload []byte
	// Generation binds the job to the runtime generation that created it.
	Generation uint64
	// Deadline is the absolute time after which the job must not run. The
	// zero value means no deadline.
	Deadline time.Time
	// AsyncBudget limits concurrent async work inside the job. Zero means
	// the runtime's configured budget.
	AsyncBudget int
	// MaxAttempts is the total number of attempts, including the first.
	MaxAttempts int
	// Backoff is the delay before the first retry.
	Backoff time.Duration

	mu       sync.Mutex
	state    State
	attempts int
	err      error
	ctx      context.Context
	cancel   context.CancelFunc
}

// NewJob returns a pending job with a fresh id and the default retry policy.
func NewJob(name string, payload []byte) *Job {
	return &Job{
		ID:          newJobID(),
		Name:        name,
		Payload:     payload,
		MaxAttempts: DefaultMaxAttempts,
		Backoff:     DefaultBackoff,
	}
}

func newJobID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	return fmt.Sprintf("%x-%x", time.Now().UnixNano(), jobSeq.Add(1))
}

// State reports the current lifecycle state.
func (j *Job) State() State {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.state
}

// Attempts reports how many times the job has started.
func (j *Job) Attempts() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.attempts
}

// Err reports the cause recorded by the last failure, cancellation, or
// deadline. It is nil while a job is pending, running, or succeeded.
func (j *Job) Err() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.err
}

// Context is cancelled when the job is cancelled or its deadline passes. It is
// rooted at context.Background so callers cannot tie job lifetime to the
// request that produced it.
func (j *Job) Context() context.Context {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.initContextLocked()
	return j.ctx
}

func (j *Job) initContextLocked() {
	if j.ctx != nil {
		return
	}
	if j.Deadline.IsZero() {
		j.ctx, j.cancel = context.WithCancel(context.Background())
		return
	}
	j.ctx, j.cancel = context.WithDeadline(context.Background(), j.Deadline)
}

// Start moves a pending or retrying job to running and counts the attempt.
// It refuses cancelled, expired, or terminal jobs.
func (j *Job) Start() error {
	j.mu.Lock()
	defer j.mu.Unlock()

	switch j.state {
	case StatePending, StateRetrying:
	case StateCancelled:
		return ErrJobCancelled
	default:
		return fmt.Errorf("%w: cannot start a %s job", ErrJobInvalidTransition, j.state)
	}

	if !j.Deadline.IsZero() && !time.Now().Before(j.Deadline) {
		j.state = StateFailed
		j.err = ErrJobDeadline
		return ErrJobDeadline
	}

	j.initContextLocked()
	if j.ctx.Err() != nil {
		j.state = StateCancelled
		if j.err == nil {
			j.err = ErrJobCancelled
		}
		return ErrJobCancelled
	}

	j.state = StateRunning
	j.attempts++
	return nil
}

// Ack marks a running job as succeeded.
func (j *Job) Ack() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.state != StateRunning {
		return fmt.Errorf("%w: cannot ack a %s job", ErrJobInvalidTransition, j.state)
	}
	j.state = StateSucceeded
	j.err = nil
	return nil
}

// Fail records a failed attempt. It moves the job to retrying while attempts
// remain and to failed otherwise. A cancelled or expired job is never retried.
// The returned state is the transition target; the error is non-nil only when
// the transition is rejected.
func (j *Job) Fail(cause error) (State, error) {
	j.mu.Lock()
	defer j.mu.Unlock()

	if j.state.terminal() || (j.state != StateRunning && j.state != StateRetrying) {
		return j.state, fmt.Errorf("%w: cannot fail a %s job", ErrJobInvalidTransition, j.state)
	}

	if cause != nil {
		j.err = cause
	} else if j.err == nil {
		j.err = ErrJobFailed
	}

	if !j.Deadline.IsZero() && !time.Now().Before(j.Deadline) {
		j.state = StateFailed
		j.err = ErrJobDeadline
		return j.state, nil
	}

	j.initContextLocked()
	if errors.Is(j.ctx.Err(), context.Canceled) {
		j.state = StateCancelled
		j.err = ErrJobCancelled
		return j.state, nil
	}

	if j.attempts >= j.maxAttemptsLocked() {
		j.state = StateFailed
		return j.state, nil
	}

	j.state = StateRetrying
	return j.state, nil
}

// Abandon moves a non-terminal job straight to failed, bypassing retries. It is
// used when the job can no longer be admitted, such as a full queue.
func (j *Job) Abandon(cause error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.state.terminal() {
		return
	}
	if cause == nil {
		cause = ErrJobFailed
	}
	j.state = StateFailed
	j.err = cause
}

// Fence stops a non-terminal job that belongs to a superseded runtime
// generation. It cancels the job's context like Cancel but records the stale
// generation as the cause, so the job can never reach a newer Program. It
// reports whether the job was still active.
func (j *Job) Fence() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.state.terminal() {
		return false
	}
	j.initContextLocked()
	j.cancel()
	j.state = StateCancelled
	j.err = ErrJobStale
	return true
}

// Cancel stops a non-terminal job and cancels its context. It reports whether
// the job was still active.
func (j *Job) Cancel() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.state.terminal() {
		return false
	}
	j.initContextLocked()
	j.cancel()
	j.state = StateCancelled
	j.err = ErrJobCancelled
	return true
}

// RetryDelay reports the backoff before the next attempt, doubling with each
// attempt already started and capped at MaxBackoff.
func (j *Job) RetryDelay() time.Duration {
	j.mu.Lock()
	defer j.mu.Unlock()

	delay := j.Backoff
	if delay <= 0 {
		delay = DefaultBackoff
	}
	for i := 1; i < j.attempts; i++ {
		if delay >= MaxBackoff/2 {
			return MaxBackoff
		}
		delay *= 2
	}
	if delay > MaxBackoff {
		return MaxBackoff
	}
	return delay
}

func (j *Job) maxAttemptsLocked() int {
	if j.MaxAttempts < 1 {
		return 1
	}
	return j.MaxAttempts
}
