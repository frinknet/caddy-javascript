package vm

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"

	quickjs "github.com/go-quickjs/go-quickjs"
	"go.uber.org/zap"
)

const (
	wsGUID       = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	wsMaxMessage = 1 << 20
	wsMaxControl = 125

	wsOpContinuation = 0x0
	wsOpText         = 0x1
	wsOpBinary       = 0x2
	wsOpClose        = 0x8
	wsOpPing         = 0x9
	wsOpPong         = 0xA
)

// wsConn is one upgraded connection. The reader runs on its own goroutine and
// posts every message to the VM loop; the loop is the only goroutine that
// touches a JavaScript value.
type wsConn struct {
	v      *VM
	st     *requestState
	conn   net.Conn
	reader *bufio.Reader
	obj    quickjs.Value

	writeMu sync.Mutex

	stateMu sync.Mutex
	closed  bool

	done       chan struct{}
	stop       chan struct{}
	slots      chan struct{}
	finishOnce sync.Once
	loopOnce   sync.Once
}

// openWebSocket upgrades the request and returns the socket object. The
// handshake is written directly to the hijacked connection; the response is
// marked as streamed so the handler does not touch it again.
func (s *requestState) openWebSocket() (quickjs.Value, error) {
	if err := s.charge(); err != nil {
		return quickjs.Value{}, err
	}
	return s.v.upgradeWebSocket(s)
}

func (v *VM) upgradeWebSocket(st *requestState) (quickjs.Value, error) {
	req := st.req
	if req.Method != http.MethodGet {
		return quickjs.Value{}, errors.New("javascript: websocket requires GET")
	}
	if !strings.EqualFold(req.Header.Get("Upgrade"), "websocket") {
		return quickjs.Value{}, errors.New("javascript: missing Upgrade: websocket")
	}
	if !headerHasToken(req.Header, "Connection", "upgrade") {
		return quickjs.Value{}, errors.New("javascript: missing Connection: upgrade")
	}
	if req.Header.Get("Sec-WebSocket-Version") != "13" {
		return quickjs.Value{}, errors.New("javascript: unsupported websocket version")
	}
	key := strings.TrimSpace(req.Header.Get("Sec-WebSocket-Key"))
	raw, err := base64.StdEncoding.DecodeString(key)
	if err != nil || len(raw) != 16 {
		return quickjs.Value{}, errors.New("javascript: invalid Sec-WebSocket-Key")
	}
	if st.hijack == nil {
		return quickjs.Value{}, errors.New("javascript: websocket is not available")
	}
	if st.resp.responded() {
		return quickjs.Value{}, errors.New("javascript: response already sent")
	}

	conn, brw, err := st.hijack()
	if err != nil {
		return quickjs.Value{}, fmt.Errorf("javascript: websocket upgrade: %w", err)
	}

	if err := st.resp.stream(http.StatusSwitchingProtocols, nil); err != nil {
		_ = conn.Close()
		return quickjs.Value{}, err
	}

	accept := wsAccept(key)
	if _, err := fmt.Fprintf(brw,
		"HTTP/1.1 101 Switching Protocols\r\n"+
			"Upgrade: websocket\r\n"+
			"Connection: Upgrade\r\n"+
			"Sec-WebSocket-Accept: %s\r\n\r\n", accept); err != nil {
		_ = conn.Close()
		return quickjs.Value{}, fmt.Errorf("javascript: websocket handshake: %w", err)
	}
	if err := brw.Flush(); err != nil {
		_ = conn.Close()
		return quickjs.Value{}, fmt.Errorf("javascript: websocket handshake: %w", err)
	}

	w := &wsConn{
		v:      v,
		st:     st,
		conn:   conn,
		reader: brw.Reader,
		done:   make(chan struct{}),
		stop:   make(chan struct{}),
		slots:  make(chan struct{}, 256),
	}
	if w.reader == nil {
		w.reader = bufio.NewReader(conn)
	}

	obj := v.rt.NewObject()
	if err := obj.Set("send", w.send); err != nil {
		_ = conn.Close()
		return quickjs.Value{}, err
	}
	if err := obj.Set("close", w.close); err != nil {
		_ = conn.Close()
		return quickjs.Value{}, err
	}
	if err := obj.Set("onmessage", nil); err != nil {
		_ = conn.Close()
		return quickjs.Value{}, err
	}
	if err := obj.Set("onclose", nil); err != nil {
		_ = conn.Close()
		return quickjs.Value{}, err
	}
	w.obj = obj
	st.setWebSocket(w)
	v.loop.Begin()
	go w.readLoop()
	return obj, nil
}

func wsAccept(key string) string {
	sum := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

func headerHasToken(header http.Header, name, token string) bool {
	for _, value := range header.Values(name) {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

func (w *wsConn) send(data any) error {
	if w.isClosed() || w.st.isFinished() {
		return nil
	}
	opcode, payload := wsPayload(data)
	return w.writeFrame(opcode, payload)
}

func wsPayload(data any) (byte, []byte) {
	switch value := data.(type) {
	case string:
		return wsOpText, []byte(value)
	case []byte:
		return wsOpBinary, append([]byte(nil), value...)
	case quickjs.Value:
		if value.IsUndefined() || value.IsNull() {
			return wsOpText, nil
		}
		return wsOpText, []byte(fmt.Sprint(value))
	default:
		return wsOpText, []byte(fmt.Sprint(data))
	}
}

func (w *wsConn) close(args ...any) error {
	if w.isClosed() || w.st.isFinished() {
		return nil
	}
	code := 1000
	reason := ""
	if len(args) > 0 && args[0] != nil {
		if n, ok := numberValue(args[0]); ok {
			code = n
		}
	}
	if len(args) > 1 && args[1] != nil {
		reason = fmt.Sprint(args[1])
	}
	err := w.writeFrame(wsOpClose, closePayload(code, reason))
	w.finish(code, reason)
	return err
}

func numberValue(value any) (int, bool) {
	switch n := value.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}

func closePayload(code int, reason string) []byte {
	if code < 1000 || code > 4999 || code == 1004 || code == 1005 || code == 1006 || (code >= 1015 && code < 3000) {
		code = 1000
	}
	payload := make([]byte, 2, 2+len(reason))
	binary.BigEndian.PutUint16(payload, uint16(code))
	payload = append(payload, []byte(reason)...)
	if len(payload) > wsMaxControl {
		payload = payload[:wsMaxControl]
	}
	return payload
}

func (w *wsConn) writeFrame(opcode byte, payload []byte) error {
	w.stateMu.Lock()
	closed := w.closed
	w.stateMu.Unlock()
	if closed {
		return errors.New("javascript: websocket is closed")
	}

	w.writeMu.Lock()
	defer w.writeMu.Unlock()

	header := make([]byte, 0, 10)
	header = append(header, 0x80|opcode)
	switch {
	case len(payload) < 126:
		header = append(header, byte(len(payload)))
	case len(payload) <= 0xffff:
		header = append(header, 126, byte(len(payload)>>8), byte(len(payload)))
	default:
		header = append(header, 127)
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(payload)))
		header = append(header, size[:]...)
	}
	if _, err := w.conn.Write(header); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.conn.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

func (w *wsConn) readLoop() {
	for {
		fin, opcode, payload, err := readFrame(w.reader)
		if err != nil {
			w.finish(1006, "")
			return
		}
		switch opcode {
		case wsOpText, wsOpBinary:
			message, ok := w.readMessage(fin, opcode, payload)
			if !ok {
				return
			}
			w.dispatchMessage(opcode == wsOpBinary, message)
		case wsOpContinuation:
			_ = w.writeFrame(wsOpClose, closePayload(1002, "unexpected continuation"))
			w.finish(1002, "unexpected continuation")
			return
		case wsOpClose:
			code, reason := parseClose(payload)
			_ = w.writeFrame(wsOpClose, closePayload(code, reason))
			w.finish(code, reason)
			return
		case wsOpPing:
			_ = w.writeFrame(wsOpPong, payload)
		case wsOpPong:
		default:
			_ = w.writeFrame(wsOpClose, closePayload(1002, "unsupported opcode"))
			w.finish(1002, "unsupported opcode")
			return
		}
	}
}

// readMessage collects the fragments of one message. A fragmented control
// frame is rejected by readFrame; a data message may span continuation frames.
func (w *wsConn) readMessage(fin bool, opcode byte, first []byte) ([]byte, bool) {
	if fin {
		if len(first) > wsMaxMessage {
			_ = w.writeFrame(wsOpClose, closePayload(1009, "message too large"))
			w.finish(1009, "message too large")
			return nil, false
		}
		return first, true
	}

	message := append([]byte(nil), first...)
	for {
		fin, nextOpcode, payload, err := readFrame(w.reader)
		if err != nil {
			w.finish(1006, "")
			return nil, false
		}
		switch nextOpcode {
		case wsOpPing:
			_ = w.writeFrame(wsOpPong, payload)
			continue
		case wsOpPong:
			continue
		case wsOpClose:
			code, reason := parseClose(payload)
			_ = w.writeFrame(wsOpClose, closePayload(code, reason))
			w.finish(code, reason)
			return nil, false
		case wsOpContinuation:
		default:
			_ = w.writeFrame(wsOpClose, closePayload(1002, "expected continuation"))
			w.finish(1002, "expected continuation")
			return nil, false
		}
		message = append(message, payload...)
		if len(message) > wsMaxMessage {
			_ = w.writeFrame(wsOpClose, closePayload(1009, "message too large"))
			w.finish(1009, "message too large")
			return nil, false
		}
		if fin {
			return message, true
		}
	}
}

func readFrame(r *bufio.Reader) (bool, byte, []byte, error) {
	var head [2]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return false, 0, nil, err
	}
	fin := head[0]&0x80 != 0
	if head[0]&0x70 != 0 {
		return false, 0, nil, errors.New("websocket: reserved bits set")
	}
	opcode := head[0] & 0x0f
	masked := head[1]&0x80 != 0
	if !masked {
		return false, 0, nil, errors.New("websocket: client frame is not masked")
	}
	length := int64(head[1] & 0x7f)
	switch length {
	case 126:
		var size [2]byte
		if _, err := io.ReadFull(r, size[:]); err != nil {
			return false, 0, nil, err
		}
		length = int64(binary.BigEndian.Uint16(size[:]))
	case 127:
		var size [8]byte
		if _, err := io.ReadFull(r, size[:]); err != nil {
			return false, 0, nil, err
		}
		length = int64(binary.BigEndian.Uint64(size[:]))
	}
	if opcode >= wsOpClose && (!fin || length > wsMaxControl) {
		return false, 0, nil, errors.New("websocket: invalid control frame")
	}
	if length < 0 || length > wsMaxMessage {
		return false, 0, nil, errors.New("websocket: message too large")
	}

	var mask [4]byte
	if _, err := io.ReadFull(r, mask[:]); err != nil {
		return false, 0, nil, err
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return false, 0, nil, err
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return fin, opcode, payload, nil
}

func parseClose(payload []byte) (int, string) {
	if len(payload) < 2 {
		return 1000, ""
	}
	code := int(binary.BigEndian.Uint16(payload[:2]))
	return code, string(payload[2:])
}

func (w *wsConn) dispatchMessage(binary bool, payload []byte) {
	if w.isClosed() {
		return
	}
	select {
	case w.slots <- struct{}{}:
	case <-w.stop:
		return
	}
	w.v.loop.Post(func() {
		defer func() { <-w.slots }()
		if w.isClosed() {
			return
		}
		if err := w.st.charge(); err != nil {
			return
		}
		fn, err := w.obj.Get("onmessage")
		if err != nil || !fn.IsFunction() {
			return
		}
		var arg any = string(payload)
		if binary {
			arg = w.v.rt.NewBytes(payload)
		}
		if _, err := fn.CallContext(w.v.loop.Context(), arg); err != nil {
			w.logError("message handler failed", err)
		}
	})
}

func (w *wsConn) dispatchClose(code int, reason string) {
	if err := w.st.charge(); err != nil {
		return
	}
	fn, err := w.obj.Get("onclose")
	if err != nil || !fn.IsFunction() {
		return
	}
	if _, err := fn.CallContext(w.v.loop.Context(), code, reason); err != nil {
		w.logError("close handler failed", err)
	}
}

func (w *wsConn) finish(code int, reason string) {
	w.finishOnce.Do(func() {
		close(w.stop)
		w.stateMu.Lock()
		w.closed = true
		w.stateMu.Unlock()
		_ = w.conn.Close()
		w.loopOnce.Do(func() { w.v.loop.Done() })
		w.v.loop.Post(func() {
			w.dispatchClose(code, reason)
			close(w.done)
		})
	})
}

func (w *wsConn) isClosed() bool {
	w.stateMu.Lock()
	defer w.stateMu.Unlock()
	return w.closed
}

func (w *wsConn) shutdown() {
	if w.isClosed() {
		return
	}
	w.finish(1006, "request finished")
}

func (w *wsConn) logError(message string, err error) {
	if w.v.logger != nil {
		w.v.logger.Error("javascript: websocket "+message, zap.Error(err))
	}
}
