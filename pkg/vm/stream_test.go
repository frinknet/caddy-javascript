package vm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"net/http"
	"strings"
	"testing"
	"time"
)

type captureStream struct {
	status   int
	headers  map[string]string
	body     []byte
	flushes  int
	headerAt int
}

func (c *captureStream) WriteHeader(status int, headers map[string]string) error {
	c.status = status
	c.headers = headers
	c.headerAt = len(c.body)
	return nil
}

func (c *captureStream) Write(p []byte) error {
	c.body = append(c.body, p...)
	return nil
}

func (c *captureStream) Flush() error {
	c.flushes++
	return nil
}

func streamVM(t *testing.T, source string) (*VM, string) {
	t.Helper()
	dir := t.TempDir()
	entry := writeModule(t, dir, "main.js", source)
	v, err := New(Config{ScriptPath: entry, RootDir: dir, RequireHandler: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return v, entry
}

func TestStreamWritesChunks(t *testing.T) {
	v, entry := streamVM(t, `export default function handler(http) {
	const out = http.stream(201, {"content-type": "text/plain"})
	out.write("one|")
	out.end("two")
}
`)
	defer v.Close()

	w := &captureStream{}
	resp, err := v.Handle(context.Background(), entry, Request{
		Req:    testRequest(http.MethodGet, "/", nil).Req,
		Writer: w,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if resp == nil || !resp.Streamed {
		t.Fatalf("response = %+v, want streamed", resp)
	}
	if resp.Status != 201 {
		t.Errorf("status = %d, want 201", resp.Status)
	}
	if w.status != 201 || w.headers["content-type"] != "text/plain" {
		t.Errorf("writer header = %d %v", w.status, w.headers)
	}
	if got := string(w.body); got != "one|two" {
		t.Errorf("body = %q", got)
	}
	if w.flushes == 0 {
		t.Error("stream was never flushed")
	}
}

func TestStreamBuffersWithoutWriter(t *testing.T) {
	v, entry := streamVM(t, `export default function handler(http) {
	const out = http.stream(200, {})
	out.write("a")
	out.end("b")
}
`)
	defer v.Close()

	resp, err := v.Handle(context.Background(), entry, testRequest(http.MethodGet, "/", nil))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if resp == nil || !resp.Streamed || string(resp.Body) != "ab" {
		t.Fatalf("response = %+v", resp)
	}
}

func TestStreamContinuesAfterDispatch(t *testing.T) {
	v, entry := streamVM(t, `export default function handler(http) {
	const out = http.stream(200, {})
	out.write("first|")
	setTimeout(() => {
		out.write("second|")
		out.end("third")
	}, 5)
}
`)
	defer v.Close()

	w := &captureStream{}
	resp, err := v.Handle(context.Background(), entry, Request{
		Req:           testRequest(http.MethodGet, "/", nil).Req,
		Writer:        w,
		StreamContext: context.Background(),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if resp == nil || !resp.Streamed {
		t.Fatalf("response = %+v, want streamed", resp)
	}
	if got := string(w.body); got != "first|second|third" {
		t.Errorf("body = %q", got)
	}
}

func TestSSEFormatsEvents(t *testing.T) {
	v, entry := streamVM(t, `export default function handler(http) {
	const sse = http.sse()
	sse.send("a")
	sse.send({n: 1}, {event: "update", id: "7"})
	sse.comment("keepalive")
	sse.close()
}
`)
	defer v.Close()

	w := &captureStream{}
	if _, err := v.Handle(context.Background(), entry, Request{
		Req:    testRequest(http.MethodGet, "/", nil).Req,
		Writer: w,
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := w.headers["content-type"]; got != "text/event-stream" {
		t.Errorf("content-type = %q", got)
	}
	want := "data: a\n\nevent: update\nid: 7\ndata: {\"n\":1}\n\n: keepalive\n\n"
	if got := string(w.body); got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func TestRespondAfterStreamIsRejected(t *testing.T) {
	v, entry := streamVM(t, `export default function handler(http) {
	const out = http.stream(200, {})
	try {
		http.respond(200, {}, "late")
		out.end("ok")
	} catch (err) {
		out.end(String(err.message))
	}
}
`)
	defer v.Close()

	w := &captureStream{}
	if _, err := v.Handle(context.Background(), entry, Request{
		Req:    testRequest(http.MethodGet, "/", nil).Req,
		Writer: w,
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !strings.Contains(string(w.body), "response already sent") {
		t.Errorf("body = %q", w.body)
	}
}

func TestStreamStopsOnClientCancellation(t *testing.T) {
	v, entry := streamVM(t, `export default function handler(http) {
	const out = http.stream(200, {})
	out.write("open")
	setInterval(() => out.write("."), 10)
}
`)
	defer v.Close()

	ctx, cancel := context.WithCancel(context.Background())
	w := &captureStream{}
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	_, err := v.Handle(ctx, entry, Request{
		Req:           testRequest(http.MethodGet, "/", nil).Req,
		Writer:        w,
		StreamContext: ctx,
	})
	if err == nil {
		t.Fatal("Handle returned nil error after cancellation")
	}
}

func TestWebSocketHandshakeAccept(t *testing.T) {
	if got := wsAccept("dGhlIHNhbXBsZSBub25jZQ=="); got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Errorf("accept = %q", got)
	}
}

func TestReadFrameUnmasks(t *testing.T) {
	payload := []byte("hello")
	mask := []byte{1, 2, 3, 4}
	frame := []byte{0x81, 0x80 | byte(len(payload))}
	frame = append(frame, mask...)
	for i, b := range payload {
		frame = append(frame, b^mask[i%4])
	}

	fin, opcode, got, err := readFrame(bufio.NewReader(bytes.NewReader(frame)))
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if !fin || opcode != wsOpText || string(got) != "hello" {
		t.Errorf("frame = %v %d %q", fin, opcode, got)
	}
}

func TestReadFrameRejectsUnmasked(t *testing.T) {
	frame := []byte{0x81, 0x05, 'h', 'e', 'l', 'l', 'o'}
	if _, _, _, err := readFrame(bufio.NewReader(bytes.NewReader(frame))); err == nil {
		t.Fatal("readFrame accepted an unmasked client frame")
	}
}

func TestClosePayload(t *testing.T) {
	payload := closePayload(1000, "bye")
	if len(payload) != 5 {
		t.Fatalf("payload length = %d", len(payload))
	}
	if got := binary.BigEndian.Uint16(payload[:2]); got != 1000 {
		t.Errorf("code = %d", got)
	}
	if got := string(payload[2:]); got != "bye" {
		t.Errorf("reason = %q", got)
	}
}

func TestStreamAbortsOnDispatchError(t *testing.T) {
	v, entry := streamVM(t, `export default function handler(http) {
	const out = http.stream(200, {})
	out.write("partial")
	throw new Error("boom")
}
`)
	defer v.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	w := &captureStream{}
	resp, err := v.Handle(ctx, entry, Request{
		Req:    testRequest(http.MethodGet, "/", nil).Req,
		Writer: w,
	})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Handle error = %v", err)
	}
	if resp == nil || !resp.Streamed {
		t.Fatalf("response = %+v, want streamed", resp)
	}
	if got := string(w.body); got != "partial" {
		t.Errorf("body = %q", got)
	}
}

func TestStreamManySynchronousChunks(t *testing.T) {
	v, entry := streamVM(t, `export default function handler(http) {
	const out = http.stream(200, {})
	for (let i = 0; i < 200; i++) out.write("x")
	out.end("!")
}
`)
	defer v.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	w := &captureStream{}
	if _, err := v.Handle(ctx, entry, Request{
		Req:    testRequest(http.MethodGet, "/", nil).Req,
		Writer: w,
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := string(w.body); got != strings.Repeat("x", 200)+"!" {
		t.Errorf("body length = %d", len(w.body))
	}
}
