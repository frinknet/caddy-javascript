package jobs

import (
	"fmt"
	"sync"

	"github.com/frinknet/caddy-javascript/pkg/metric"
)

// Queue is the bounded admission point shared by every job producer. It is the
// only place that decides whether a job enters the supervisor; a full queue is
// counted and reported to the caller.
type Queue struct {
	items  chan *Job
	stop   chan struct{}
	once   sync.Once
	mu     sync.RWMutex
	closed bool
}

// NewQueue returns a queue that accepts at most size jobs.
func NewQueue(size int) (*Queue, error) {
	if size < 1 {
		return nil, fmt.Errorf("job queue size must be at least 1")
	}
	return &Queue{
		items: make(chan *Job, size),
		stop:  make(chan struct{}),
	}, nil
}

// Admit offers a job without blocking. It reports false when the queue is full
// or shutting down; a full queue is counted once per rejected job.
func (q *Queue) Admit(job *Job) bool {
	if job == nil {
		return false
	}
	q.mu.RLock()
	defer q.mu.RUnlock()
	if q.closed {
		return false
	}
	select {
	case q.items <- job:
		return true
	default:
		metric.QueueFull.Inc()
		return false
	}
}

// Next returns the next job in FIFO order. It reports false once the queue is
// closed and drained.
func (q *Queue) Next() (*Job, bool) {
	select {
	case job := <-q.items:
		return job, true
	case <-q.stop:
		select {
		case job := <-q.items:
			return job, true
		default:
			return nil, false
		}
	}
}

// Close stops admission and lets the consumer drain the jobs already queued.
func (q *Queue) Close() {
	q.once.Do(func() {
		q.mu.Lock()
		q.closed = true
		close(q.stop)
		q.mu.Unlock()
	})
}

// Done is closed when the queue stops admitting jobs.
func (q *Queue) Done() <-chan struct{} { return q.stop }

// Closed reports whether Close has been called.
func (q *Queue) Closed() bool {
	q.mu.RLock()
	defer q.mu.RUnlock()
	return q.closed
}

// Len reports the number of jobs waiting, excluding one in flight.
func (q *Queue) Len() int { return len(q.items) }
