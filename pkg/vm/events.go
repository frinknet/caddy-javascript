package vm

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
	"go.uber.org/zap"
)

// Event is one Caddy event delivered to the global worker. Data is cloned
// before it is posted to the VM, so a handler that keeps mutating the event on
// the emitting goroutine cannot race with a script.
type Event struct {
	ID     string
	Name   string
	Origin string
	Time   time.Time
	Data   map[string]any
}

// eventBacklog bounds the events waiting for the VM goroutine. A burst that
// exceeds it is dropped and logged instead of growing without limit.
const eventBacklog = 1024

// installEvents creates the `events` global. It runs on the VM goroutine
// before the entry module is evaluated, and only in a worker VM.
func (v *VM) installEvents() error {
	obj := v.rt.NewObject()
	if err := obj.Set("on", v.eventOn); err != nil {
		return err
	}
	v.eventNames = make(map[string]struct{})
	v.eventListeners = make(map[string][]quickjs.Value)
	v.eventSlots = make(chan struct{}, eventBacklog)
	v.events = true
	return v.rt.Set("events", obj)
}

// eventOn is events.on(name, fn). An empty name or "*" listens to every event.
// Registration is allowed only while the entry module is evaluated, because
// Caddy accepts event subscriptions only before its apps start.
func (v *VM) eventOn(name string, fn quickjs.Value) error {
	if !fn.IsFunction() {
		return errors.New("javascript: events.on requires a function")
	}
	key := name
	if key == "*" {
		key = ""
	}

	v.eventMu.Lock()
	defer v.eventMu.Unlock()
	if v.eventBound {
		return errors.New("javascript: events.on must run at module scope")
	}
	v.eventNames[key] = struct{}{}
	v.eventListeners[key] = append(v.eventListeners[key], fn)
	return nil
}

// MarkEventBound records that the current configuration generation subscribed
// the VM's listeners. A later registration is a module-scope error.
func (v *VM) MarkEventBound() {
	v.eventMu.Lock()
	v.eventBound = true
	v.eventMu.Unlock()
}

// EventNames returns the event names the loaded module registered, sorted. An
// empty name means every event.
func (v *VM) EventNames() []string {
	v.eventMu.Lock()
	defer v.eventMu.Unlock()
	names := make([]string, 0, len(v.eventNames))
	for name := range v.eventNames {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// clearEventListeners drops the previous module's registrations before a new
// module is evaluated into a reused VM.
func (v *VM) clearEventListeners() {
	v.eventMu.Lock()
	v.eventNames = make(map[string]struct{})
	v.eventListeners = make(map[string][]quickjs.Value)
	v.eventBound = false
	v.eventMu.Unlock()
}

// DispatchEvent queues one event for the VM goroutine. It never blocks: the
// Caddy events app dispatches synchronously on the emitting goroutine, so the
// hook only clones the payload and posts it.
func (v *VM) DispatchEvent(event Event) {
	if !v.events {
		return
	}
	select {
	case v.eventSlots <- struct{}{}:
	default:
		if v.logger != nil {
			v.logger.Warn("javascript: event backlog full; dropping event",
				zap.String("name", event.Name),
			)
		}
		return
	}
	event.Data = cloneEventData(event.Data)
	v.loop.Post(func() {
		defer func() { <-v.eventSlots }()
		v.deliverEvent(event)
	})
}

// deliverEvent invokes the module's listeners on the VM goroutine. The first
// throwable callback does not stop the others; each failure is logged.
func (v *VM) deliverEvent(event Event) {
	v.eventMu.Lock()
	listeners := append([]quickjs.Value(nil), v.eventListeners[event.Name]...)
	listeners = append(listeners, v.eventListeners[""]...)
	v.eventMu.Unlock()
	if len(listeners) == 0 {
		return
	}

	obj := v.rt.NewObject()
	if err := obj.Set("id", event.ID); err != nil {
		v.logEventError(event, err)
		return
	}
	if err := obj.Set("name", event.Name); err != nil {
		v.logEventError(event, err)
		return
	}
	if err := obj.Set("origin", event.Origin); err != nil {
		v.logEventError(event, err)
		return
	}
	if err := obj.Set("time", event.Time.UTC().Format(time.RFC3339Nano)); err != nil {
		v.logEventError(event, err)
		return
	}
	if err := obj.Set("data", event.Data); err != nil {
		v.logEventError(event, err)
		return
	}

	for _, fn := range listeners {
		if _, err := fn.CallContext(v.loop.Context(), obj); err != nil {
			v.logEventError(event, err)
		}
	}
}

func (v *VM) logEventError(event Event, err error) {
	if v.logger != nil {
		v.logger.Error("javascript: event handler failed",
			zap.String("name", event.Name),
			zap.Error(err),
		)
	}
}

// cloneEventData turns event data into plain JSON-shaped values. Structs such
// as the TLS client hello survive the JSON round trip; anything else that does
// not marshal is rendered as a string.
func cloneEventData(data map[string]any) map[string]any {
	if data == nil {
		return nil
	}
	if raw, err := json.Marshal(data); err == nil {
		var plain map[string]any
		if json.Unmarshal(raw, &plain) == nil {
			return plain
		}
	}
	out := make(map[string]any, len(data))
	for key, value := range data {
		out[key] = cloneEventValue(value)
	}
	return out
}

func cloneEventValue(value any) any {
	switch v := value.(type) {
	case nil, bool, string, float64, int, int64, uint64:
		return v
	case time.Time:
		return v.UTC().Format(time.RFC3339Nano)
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = cloneEventValue(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = cloneEventValue(item)
		}
		return out
	}

	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Map:
		out := make(map[string]any, rv.Len())
		iter := rv.MapRange()
		for iter.Next() {
			out[fmt.Sprint(iter.Key().Interface())] = cloneEventValue(iter.Value().Interface())
		}
		return out
	case reflect.Slice, reflect.Array:
		out := make([]any, rv.Len())
		for i := range out {
			out[i] = cloneEventValue(rv.Index(i).Interface())
		}
		return out
	}

	if raw, err := json.Marshal(value); err == nil {
		var plain any
		if json.Unmarshal(raw, &plain) == nil {
			return plain
		}
	}
	return fmt.Sprint(value)
}
