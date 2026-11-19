package config

import "testing"

func TestApplyDefaults(t *testing.T) {
	var o Options
	o.ApplyDefaults()

	if o.Timeout == 0 {
		t.Fatal("timeout default not applied")
	}
	if o.QueueSize != DefaultQueueSize {
		t.Fatalf("queue size = %d, want %d", o.QueueSize, DefaultQueueSize)
	}
	if o.AsyncBudget != DefaultAsyncBudget {
		t.Fatalf("async budget = %d, want %d", o.AsyncBudget, DefaultAsyncBudget)
	}
	if o.MaxBody != DefaultMaxBody {
		t.Fatalf("max body = %d, want %d", o.MaxBody, DefaultMaxBody)
	}
}

func TestValidate(t *testing.T) {
	o := Options{ScriptPath: "/app/server.js"}
	o.ApplyDefaults()
	if err := o.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := (Options{}).Validate(); err == nil {
		t.Fatal("expected error for missing script path")
	}

	bad := Options{ScriptPath: "/app/server.js", AsyncBudget: -1}
	bad.ApplyDefaults()
	if err := bad.Validate(); err == nil {
		t.Fatal("expected error for negative async_budget")
	}
}

func TestWatchingDefaultsOn(t *testing.T) {
	var o Options
	if !o.Watching() {
		t.Fatal("watch should default to on")
	}
	off := false
	o.Watch = &off
	if o.Watching() {
		t.Fatal("watch should be off when explicitly disabled")
	}
}
