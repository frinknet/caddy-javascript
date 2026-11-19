package jobs

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestNewJobDefaults(t *testing.T) {
	a := NewJob("a", []byte("payload"))
	b := NewJob("a", nil)

	if a.ID == "" || b.ID == "" {
		t.Fatal("NewJob left the id empty")
	}
	if a.ID == b.ID {
		t.Fatalf("two jobs share the id %q", a.ID)
	}
	if a.Name != "a" {
		t.Fatalf("name = %q, want %q", a.Name, "a")
	}
	if string(a.Payload) != "payload" {
		t.Fatalf("payload = %q, want %q", a.Payload, "payload")
	}
	if got := a.State(); got != StatePending {
		t.Fatalf("state = %s, want pending", got)
	}
	if got := a.Attempts(); got != 0 {
		t.Fatalf("attempts = %d, want 0", got)
	}
	if a.MaxAttempts != DefaultMaxAttempts {
		t.Fatalf("max attempts = %d, want %d", a.MaxAttempts, DefaultMaxAttempts)
	}
	if a.Backoff != DefaultBackoff {
		t.Fatalf("backoff = %v, want %v", a.Backoff, DefaultBackoff)
	}
	if got := a.Err(); got != nil {
		t.Fatalf("err = %v, want nil", got)
	}
	if a.Context() == nil {
		t.Fatal("context is nil")
	}
}

func TestJobStartAck(t *testing.T) {
	j := NewJob("job", nil)
	if err := j.Start(); err != nil {
		t.Fatal(err)
	}
	if got := j.State(); got != StateRunning {
		t.Fatalf("state = %s, want running", got)
	}
	if got := j.Attempts(); got != 1 {
		t.Fatalf("attempts = %d, want 1", got)
	}
	if err := j.Ack(); err != nil {
		t.Fatal(err)
	}
	if got := j.State(); got != StateSucceeded {
		t.Fatalf("state = %s, want succeeded", got)
	}
	if err := j.Ack(); !errors.Is(err, ErrJobInvalidTransition) {
		t.Fatalf("second ack error = %v, want an invalid transition", err)
	}
	if err := j.Start(); !errors.Is(err, ErrJobInvalidTransition) {
		t.Fatalf("start after ack error = %v, want an invalid transition", err)
	}
	if j.Cancel() {
		t.Fatal("cancel of a finished job reported true")
	}
}

func TestJobFailRetriesWithBackoff(t *testing.T) {
	j := NewJob("job", nil)
	j.MaxAttempts = 3
	j.Backoff = 100 * time.Millisecond

	if err := j.Start(); err != nil {
		t.Fatal(err)
	}
	state, err := j.Fail(errors.New("boom"))
	if err != nil {
		t.Fatal(err)
	}
	if state != StateRetrying {
		t.Fatalf("state after first failure = %s, want retrying", state)
	}
	if got := j.RetryDelay(); got != 100*time.Millisecond {
		t.Fatalf("first retry delay = %v, want 100ms", got)
	}
	if got := j.Err(); got == nil || got.Error() != "boom" {
		t.Fatalf("err = %v, want boom", got)
	}

	if err := j.Start(); err != nil {
		t.Fatal(err)
	}
	state, err = j.Fail(errors.New("boom"))
	if err != nil {
		t.Fatal(err)
	}
	if state != StateRetrying {
		t.Fatalf("state after second failure = %s, want retrying", state)
	}
	if got := j.RetryDelay(); got != 200*time.Millisecond {
		t.Fatalf("second retry delay = %v, want 200ms", got)
	}

	if err := j.Start(); err != nil {
		t.Fatal(err)
	}
	state, err = j.Fail(errors.New("boom"))
	if err != nil {
		t.Fatal(err)
	}
	if state != StateFailed {
		t.Fatalf("state after last failure = %s, want failed", state)
	}
	if got := j.Attempts(); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
}

func TestJobFailWithoutCause(t *testing.T) {
	j := NewJob("job", nil)
	if err := j.Start(); err != nil {
		t.Fatal(err)
	}
	state, err := j.Fail(nil)
	if err != nil {
		t.Fatal(err)
	}
	if state != StateFailed {
		t.Fatalf("state = %s, want failed", state)
	}
	if !errors.Is(j.Err(), ErrJobFailed) {
		t.Fatalf("err = %v, want the generic failure", j.Err())
	}
}

func TestJobZeroMaxAttemptsMeansOne(t *testing.T) {
	j := NewJob("job", nil)
	j.MaxAttempts = 0
	if err := j.Start(); err != nil {
		t.Fatal(err)
	}
	state, err := j.Fail(errors.New("boom"))
	if err != nil {
		t.Fatal(err)
	}
	if state != StateFailed {
		t.Fatalf("state = %s, want failed without retries", state)
	}
}

func TestJobRetryDelayCaps(t *testing.T) {
	j := NewJob("job", nil)
	j.MaxAttempts = 100
	j.Backoff = 2 * time.Second
	for i := 0; i < 10; i++ {
		if err := j.Start(); err != nil {
			t.Fatal(err)
		}
		if _, err := j.Fail(errors.New("boom")); err != nil {
			t.Fatal(err)
		}
	}
	if got := j.RetryDelay(); got != MaxBackoff {
		t.Fatalf("retry delay = %v, want the %v cap", got, MaxBackoff)
	}
}

func TestJobCancelBeforeStart(t *testing.T) {
	j := NewJob("job", nil)
	ctx := j.Context()

	if !j.Cancel() {
		t.Fatal("cancel of a pending job reported false")
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("context was not cancelled")
	}
	if got := j.State(); got != StateCancelled {
		t.Fatalf("state = %s, want cancelled", got)
	}
	if !errors.Is(j.Err(), ErrJobCancelled) {
		t.Fatalf("err = %v, want the cancellation error", j.Err())
	}
	if j.Cancel() {
		t.Fatal("second cancel reported true")
	}
	if err := j.Start(); !errors.Is(err, ErrJobCancelled) {
		t.Fatalf("start error = %v, want the cancellation error", err)
	}
	if got := j.State(); got != StateCancelled {
		t.Fatalf("state after start = %s, want cancelled", got)
	}
}

func TestJobCancelWhileRunning(t *testing.T) {
	j := NewJob("job", nil)
	if err := j.Start(); err != nil {
		t.Fatal(err)
	}
	ctx := j.Context()

	if !j.Cancel() {
		t.Fatal("cancel of a running job reported false")
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("context was not cancelled")
	}
	if err := j.Ack(); !errors.Is(err, ErrJobInvalidTransition) {
		t.Fatalf("ack error = %v, want an invalid transition", err)
	}
	state, err := j.Fail(errors.New("boom"))
	if err == nil {
		t.Fatal("fail of a cancelled job was accepted")
	}
	if state != StateCancelled {
		t.Fatalf("state = %s, want cancelled", state)
	}
}

func TestJobDeadlinePreventsStart(t *testing.T) {
	j := NewJob("job", nil)
	j.Deadline = time.Now().Add(-time.Second)

	if err := j.Start(); !errors.Is(err, ErrJobDeadline) {
		t.Fatalf("start error = %v, want the deadline error", err)
	}
	if got := j.State(); got != StateFailed {
		t.Fatalf("state = %s, want failed", got)
	}
	if got := j.Attempts(); got != 0 {
		t.Fatalf("attempts = %d, want 0", got)
	}
	if !errors.Is(j.Err(), ErrJobDeadline) {
		t.Fatalf("err = %v, want the deadline error", j.Err())
	}
}

func TestJobDeadlineCancelsContext(t *testing.T) {
	j := NewJob("job", nil)
	j.Deadline = time.Now().Add(50 * time.Millisecond)
	ctx := j.Context()

	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("context did not expire with the deadline")
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("context error = %v, want deadline exceeded", ctx.Err())
	}
	if err := j.Start(); !errors.Is(err, ErrJobDeadline) {
		t.Fatalf("start error = %v, want the deadline error", err)
	}
}

func TestJobFailAfterDeadlineDoesNotRetry(t *testing.T) {
	j := NewJob("job", nil)
	j.MaxAttempts = 5
	j.Backoff = time.Millisecond
	j.Deadline = time.Now().Add(20 * time.Millisecond)
	if err := j.Start(); err != nil {
		t.Fatal(err)
	}

	time.Sleep(30 * time.Millisecond)
	state, err := j.Fail(errors.New("late"))
	if err != nil {
		t.Fatal(err)
	}
	if state != StateFailed {
		t.Fatalf("state = %s, want failed", state)
	}
	if !errors.Is(j.Err(), ErrJobDeadline) {
		t.Fatalf("err = %v, want the deadline error", j.Err())
	}
}

func TestJobAbandon(t *testing.T) {
	j := NewJob("job", nil)
	if err := j.Start(); err != nil {
		t.Fatal(err)
	}
	j.Abandon(errors.New("no room"))
	if got := j.State(); got != StateFailed {
		t.Fatalf("state = %s, want failed", got)
	}
	if got := j.Err(); got == nil || got.Error() != "no room" {
		t.Fatalf("err = %v, want no room", got)
	}
}
