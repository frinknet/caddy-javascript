package handler

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"

	"github.com/frinknet/caddy-javascript/pkg/config"
)

func serveHandler(t *testing.T, h *Handler) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = h.ServeHTTP(w, r, caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error {
			return nil
		}))
	}))
}

func TestServeHTTPStreams(t *testing.T) {
	dir := t.TempDir()
	path := writeScript(t, dir, "main.js", `export default function handler(http) {
	const out = http.stream(200, {"content-type": "text/plain"})
	out.write("a")
	setTimeout(() => out.end("b"), 5)
}
`)
	h := &Handler{Options: config.Options{ScriptPath: path}}
	if err := h.Provision(caddy.Context{}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	defer h.Cleanup()

	srv := serveHandler(t, h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK || string(body) != "ab" {
		t.Errorf("response = %d %q", resp.StatusCode, body)
	}
}

func TestServeHTTPWebSocket(t *testing.T) {
	dir := t.TempDir()
	path := writeScript(t, dir, "main.js", `export default function handler(http) {
	const ws = http.websocket()
	ws.onmessage = (data) => ws.send("echo:" + data)
}
`)
	h := &Handler{Options: config.Options{ScriptPath: path}}
	if err := h.Provision(caddy.Context{}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	defer h.Cleanup()

	srv := serveHandler(t, h)
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	request := "GET /ws HTTP/1.1\r\n" +
		"Host: " + addr + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatalf("write handshake: %v", err)
	}

	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if !strings.Contains(status, "101") {
		t.Fatalf("status = %q", status)
	}
	var accept string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read header: %v", err)
		}
		if line == "\r\n" {
			break
		}
		if strings.HasPrefix(strings.ToLower(line), "sec-websocket-accept:") {
			accept = strings.TrimSpace(line[len("sec-websocket-accept:"):])
		}
	}
	if accept != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Errorf("accept = %q", accept)
	}

	if err := writeClientFrame(conn, 0x1, []byte("hello")); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	opcode, payload, err := readServerFrame(reader)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if opcode != 0x1 || string(payload) != "echo:hello" {
		t.Errorf("frame = %d %q", opcode, payload)
	}

	if err := writeClientFrame(conn, 0x8, []byte{0x03, 0xe8}); err != nil {
		t.Fatalf("write close: %v", err)
	}
	if _, _, err := readServerFrame(reader); err != nil {
		t.Fatalf("read close: %v", err)
	}
}

func writeClientFrame(w io.Writer, opcode byte, payload []byte) error {
	header := []byte{0x80 | opcode}
	switch {
	case len(payload) < 126:
		header = append(header, 0x80|byte(len(payload)))
	default:
		header = append(header, 0x80|126, byte(len(payload)>>8), byte(len(payload)))
	}
	mask := []byte{1, 2, 3, 4}
	header = append(header, mask...)
	masked := make([]byte, len(payload))
	for i, b := range payload {
		masked[i] = b ^ mask[i%4]
	}
	if _, err := w.Write(header); err != nil {
		return err
	}
	_, err := w.Write(masked)
	return err
}

func readServerFrame(r *bufio.Reader) (byte, []byte, error) {
	head := make([]byte, 2)
	if _, err := io.ReadFull(r, head); err != nil {
		return 0, nil, err
	}
	opcode := head[0] & 0x0f
	length := int(head[1] & 0x7f)
	switch length {
	case 126:
		size := make([]byte, 2)
		if _, err := io.ReadFull(r, size); err != nil {
			return 0, nil, err
		}
		length = int(binary.BigEndian.Uint16(size))
	case 127:
		size := make([]byte, 8)
		if _, err := io.ReadFull(r, size); err != nil {
			return 0, nil, err
		}
		length = int(binary.BigEndian.Uint64(size))
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return opcode, payload, nil
}

func TestStreamStopsWhenClientDisconnects(t *testing.T) {
	dir := t.TempDir()
	path := writeScript(t, dir, "main.js", `export default function handler(http) {
	const out = http.stream(200, {})
	setInterval(() => out.write("."), 5)
}
`)
	h := &Handler{Options: config.Options{ScriptPath: path}}
	if err := h.Provision(caddy.Context{}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	defer h.Cleanup()

	srv := serveHandler(t, h)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Fatal("stream outlived its client")
	}
}
