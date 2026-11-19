package jobs

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/frinknet/caddy-javascript/pkg/metric"
)

func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetCounter().GetValue()
}

func TestQueueAdmitsUpToBound(t *testing.T) {
	q, err := NewQueue(2)
	if err != nil {
		t.Fatal(err)
	}

	if !q.Admit(NewJob("a", nil)) {
		t.Fatal("first job was rejected")
	}
	if !q.Admit(NewJob("b", nil)) {
		t.Fatal("second job was rejected")
	}
	if q.Admit(NewJob("c", nil)) {
		t.Fatal("job over the bound was admitted")
	}
	if q.Admit(nil) {
		t.Fatal("a nil job was admitted")
	}
	if got := q.Len(); got != 2 {
		t.Fatalf("queue length = %d, want 2", got)
	}
}

func TestQueueCountsFullRejection(t *testing.T) {
	q, err := NewQueue(1)
	if err != nil {
		t.Fatal(err)
	}
	if !q.Admit(NewJob("a", nil)) {
		t.Fatal("first job was rejected")
	}

	before := counterValue(t, metric.QueueFull)
	if q.Admit(NewJob("b", nil)) {
		t.Fatal("job over the bound was admitted")
	}
	if after := counterValue(t, metric.QueueFull); after != before+1 {
		t.Fatalf("queue_full_total = %v, want %v", after, before+1)
	}
}

func TestQueueDrainsAfterClose(t *testing.T) {
	q, err := NewQueue(2)
	if err != nil {
		t.Fatal(err)
	}
	if !q.Admit(NewJob("a", nil)) || !q.Admit(NewJob("b", nil)) {
		t.Fatal("failed to fill the queue")
	}

	q.Close()
	if q.Admit(NewJob("c", nil)) {
		t.Fatal("queue admitted a job after close")
	}
	if !q.Closed() {
		t.Fatal("queue reported open after close")
	}
	select {
	case <-q.Done():
	default:
		t.Fatal("Done was not closed after Close")
	}

	seen := 0
	for {
		if _, ok := q.Next(); !ok {
			break
		}
		seen++
	}
	if seen != 2 {
		t.Fatalf("drained %d jobs, want 2", seen)
	}
	if _, ok := q.Next(); ok {
		t.Fatal("closed queue returned another job")
	}
}

func TestQueueRejectsEmptySize(t *testing.T) {
	if _, err := NewQueue(0); err == nil {
		t.Fatal("expected a zero-sized queue to be rejected")
	}
}
