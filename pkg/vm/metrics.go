package vm

import "sync/atomic"

// Stats is a snapshot of one VM's counters. They are cheap to read and let a
// host log or export a steady stream without touching the runtime.
type Stats struct {
	Requests   uint64
	QueueFull  uint64
	Timeouts   uint64
	Failures   uint64
	Closed     uint64
	Reloads    uint64
	QueueDepth int
	Inflight   int64
}

type counters struct {
	requests  atomic.Uint64
	queueFull atomic.Uint64
	timeouts  atomic.Uint64
	failures  atomic.Uint64
	closed    atomic.Uint64
	reloads   atomic.Uint64
}

// Stats returns the current counters and queue depth.
func (v *VM) Stats() Stats {
	return Stats{
		Requests:   v.metrics.requests.Load(),
		QueueFull:  v.metrics.queueFull.Load(),
		Timeouts:   v.metrics.timeouts.Load(),
		Failures:   v.metrics.failures.Load(),
		Closed:     v.metrics.closed.Load(),
		Reloads:    v.metrics.reloads.Load(),
		QueueDepth: v.QueueDepth(),
		Inflight:   v.inflight.Load(),
	}
}

// QueueDepth reports how many admission slots are occupied.
func (v *VM) QueueDepth() int {
	return len(v.slots)
}
