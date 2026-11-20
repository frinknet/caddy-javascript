package vm

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	quickjs "github.com/go-quickjs/go-quickjs"
)

var (
	errStreamStarted = errors.New("javascript: response already started")
	errStreamClosed  = errors.New("javascript: stream is closed")
)

// StreamWriter is the host side of a streaming response. The handler owns the
// real http.ResponseWriter; the VM hands it one event at a time.
type StreamWriter interface {
	WriteHeader(status int, headers map[string]string) error
	Write(p []byte) error
	Flush() error
}

type streamEvent struct {
	start   bool
	status  int
	headers map[string]string
	data    []byte
	end     bool
}

// streamState carries chunks from the VM goroutine to the handler goroutine.
// The channel is bounded so a slow client applies backpressure to the site VM
// instead of growing an unbounded buffer.
type streamState struct {
	v      *VM
	writer StreamWriter
	events chan streamEvent
	abort  chan struct{}

	abortOnce sync.Once
	doneOnce  sync.Once

	mu      sync.Mutex
	started bool
	ended   bool
	begun   bool
	body    []byte
	status  int
	headers map[string]string
}

func newStreamState(v *VM, writer StreamWriter) *streamState {
	return &streamState{
		v:      v,
		writer: writer,
		events: make(chan streamEvent, 64),
		abort:  make(chan struct{}),
	}
}

func (s *streamState) send(ev streamEvent) error {
	select {
	case s.events <- ev:
		return nil
	case <-s.abort:
		return errStreamClosed
	}
}

func (s *streamState) start(status int, headers map[string]string) error {
	s.mu.Lock()
	if s.started || s.ended {
		s.mu.Unlock()
		return errStreamStarted
	}
	s.started = true
	s.begun = true
	s.status = status
	s.headers = headers
	s.mu.Unlock()

	s.v.loop.Begin()
	if err := s.send(streamEvent{start: true, status: status, headers: headers}); err != nil {
		s.release()
		return err
	}
	return nil
}

func (s *streamState) write(p []byte) error {
	s.mu.Lock()
	if !s.started || s.ended {
		s.mu.Unlock()
		return errStreamClosed
	}
	s.mu.Unlock()
	return s.send(streamEvent{data: p})
}

func (s *streamState) end() error {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return nil
	}
	s.ended = true
	s.mu.Unlock()
	err := s.send(streamEvent{end: true})
	s.release()
	return err
}

// stop ends the stream from the handler side. It unblocks a writer that is
// waiting on a full channel and releases the loop reference exactly once.
func (s *streamState) stop() {
	s.abortOnce.Do(func() {
		close(s.abort)
		s.mu.Lock()
		s.ended = true
		s.mu.Unlock()
		s.release()
	})
}

func (s *streamState) release() {
	s.mu.Lock()
	begun := s.begun
	s.mu.Unlock()
	if !begun {
		return
	}
	s.doneOnce.Do(func() {
		s.v.loop.Done()
	})
}

func (s *streamState) isStarted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
}

func (s *streamState) appendBody(p []byte) {
	s.mu.Lock()
	s.body = append(s.body, p...)
	s.mu.Unlock()
}

func (s *streamState) bodyCopy() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.body...)
}

// chunkValue converts one JavaScript chunk into bytes. A string is written
// verbatim, a Uint8Array crosses as []byte, and anything else is stringified.
func chunkValue(value any) ([]byte, bool) {
	switch b := value.(type) {
	case nil:
		return nil, false
	case string:
		return []byte(b), true
	case []byte:
		return append([]byte(nil), b...), true
	case quickjs.Value:
		if b.IsUndefined() || b.IsNull() {
			return nil, false
		}
		return []byte(fmt.Sprint(b)), true
	default:
		return []byte(fmt.Sprint(value)), true
	}
}

func (s *requestState) charge() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkLocked()
}

// chargeStream is charge for a write on a response that outlives the dispatch.
// A client disconnect ends the request normally, so a late write is a no-op
// instead of an exception that would stop the VM from a timer callback.
func (s *requestState) chargeStream() (bool, error) {
	err := s.charge()
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, errRequestFinished):
		return false, nil
	default:
		return false, err
	}
}

// openStream starts a chunked response and returns the writer object a script
// uses to send chunks.
func (s *requestState) openStream(args ...any) (quickjs.Value, error) {
	if err := s.charge(); err != nil {
		return quickjs.Value{}, err
	}
	status := 200
	if len(args) > 0 && args[0] != nil {
		n, ok := numberValue(args[0])
		if !ok {
			return quickjs.Value{}, errors.New("javascript: stream() status must be a number")
		}
		status = n
	}
	if status < 100 || status > 999 {
		return quickjs.Value{}, fmt.Errorf("javascript: invalid status %d", status)
	}
	var headers map[string]any
	if len(args) > 1 && args[1] != nil {
		m, ok := args[1].(map[string]any)
		if !ok {
			return quickjs.Value{}, errors.New("javascript: stream() headers must be an object")
		}
		headers = m
	}
	hdrs := make(map[string]string, len(headers))
	for key, value := range headers {
		if value == nil {
			continue
		}
		hdrs[key] = headerValue(value)
	}
	if s.resp.responded() {
		return quickjs.Value{}, errors.New("javascript: response already sent")
	}
	if err := s.stream.start(status, hdrs); err != nil {
		return quickjs.Value{}, err
	}
	if err := s.resp.stream(status, hdrs); err != nil {
		s.stream.stop()
		return quickjs.Value{}, err
	}

	obj := s.v.rt.NewObject()
	if err := obj.Set("write", func(value any) error {
		if ok, err := s.chargeStream(); !ok {
			return err
		}
		chunk, ok := chunkValue(value)
		if !ok {
			return nil
		}
		return s.stream.write(chunk)
	}); err != nil {
		return quickjs.Value{}, err
	}
	if err := obj.Set("end", func(value any) error {
		if ok, err := s.chargeStream(); !ok {
			return err
		}
		if chunk, ok := chunkValue(value); ok {
			if err := s.stream.write(chunk); err != nil {
				return err
			}
		}
		return s.stream.end()
	}); err != nil {
		return quickjs.Value{}, err
	}
	if err := obj.Set("close", func() error {
		if ok, err := s.chargeStream(); !ok {
			return err
		}
		return s.stream.end()
	}); err != nil {
		return quickjs.Value{}, err
	}
	return obj, nil
}

// openSSE starts a text/event-stream response and returns helpers that format
// events for the script.
func (s *requestState) openSSE() (quickjs.Value, error) {
	if _, err := s.openStream(200, map[string]any{
		"content-type":      "text/event-stream",
		"cache-control":     "no-cache",
		"x-accel-buffering": "no",
	}); err != nil {
		return quickjs.Value{}, err
	}

	obj := s.v.rt.NewObject()
	if err := obj.Set("send", s.sseSend); err != nil {
		return quickjs.Value{}, err
	}
	if err := obj.Set("comment", func(text string) error {
		if ok, err := s.chargeStream(); !ok {
			return err
		}
		return s.stream.write([]byte(": " + text + "\n\n"))
	}); err != nil {
		return quickjs.Value{}, err
	}
	if err := obj.Set("close", func() error {
		if ok, err := s.chargeStream(); !ok {
			return err
		}
		return s.stream.end()
	}); err != nil {
		return quickjs.Value{}, err
	}
	return obj, nil
}

func (s *requestState) sseSend(data any, options map[string]any) error {
	if ok, err := s.chargeStream(); !ok {
		return err
	}

	var payload string
	switch value := data.(type) {
	case string:
		payload = value
	case []byte:
		payload = string(value)
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("javascript: encode sse data: %w", err)
		}
		payload = string(encoded)
	}

	var b strings.Builder
	if options != nil {
		if event, ok := options["event"]; ok && event != nil {
			fmt.Fprintf(&b, "event: %s\n", fmt.Sprint(event))
		}
		if id, ok := options["id"]; ok && id != nil {
			fmt.Fprintf(&b, "id: %s\n", fmt.Sprint(id))
		}
		if retry, ok := options["retry"]; ok && retry != nil {
			fmt.Fprintf(&b, "retry: %s\n", fmt.Sprint(retry))
		}
	}
	for _, line := range strings.Split(strings.ReplaceAll(payload, "\r\n", "\n"), "\n") {
		b.WriteString("data: ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	return s.stream.write([]byte(b.String()))
}
