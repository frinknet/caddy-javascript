package bridge

import (
	"encoding/json"
	"net/http"
	"sync"

	"github.com/grafana/sobek"
)

type ResponseWriter struct {
	w          http.ResponseWriter
	mu         sync.Mutex
	written    bool
	statusSet  bool
	statusCode int
}

func NewResponseWriter(w http.ResponseWriter) *ResponseWriter {
	return &ResponseWriter{w: w}
}

func (s *ResponseWriter) Header() http.Header {
	return s.w.Header()
}

func (s *ResponseWriter) WriteHeader(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.written {
		return
	}
	s.written = true
	s.statusCode = code
	s.w.WriteHeader(code)
}

func (s *ResponseWriter) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.written {
		s.written = true
		s.statusCode = http.StatusOK
		s.w.WriteHeader(http.StatusOK)
	}
	return s.w.Write(b)
}

func (s *ResponseWriter) Unwrap() http.ResponseWriter {
	return s.w
}

func (s *ResponseWriter) Flush() {
	if f, ok := s.w.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *ResponseWriter) Written() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.written
}

func (s *ResponseWriter) Status() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.written {
		return http.StatusOK
	}
	return s.statusCode
}

func (s *ResponseWriter) SetStatus(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.written {
		return
	}
	s.statusSet = true
	s.statusCode = code
}

func (s *ResponseWriter) setContentType(value string) {
	if s.Header().Get("Content-Type") == "" {
		s.Header().Set("Content-Type", value)
	}
}

func (s *ResponseWriter) writeStatusThen(b []byte) {
	s.mu.Lock()
	code := http.StatusOK
	if s.statusSet {
		code = s.statusCode
	}
	s.mu.Unlock()

	s.WriteHeader(code)
	_, _ = s.Write(b)
}

func (p *Program) BuildResponse(w *ResponseWriter) *sobek.Object {
	res := p.rt.NewObject()

	_ = res.Set("status", func(call sobek.FunctionCall) sobek.Value {
		w.SetStatus(int(call.Argument(0).ToInteger()))
		return res
	})

	_ = res.Set("set", func(call sobek.FunctionCall) sobek.Value {
		w.Header().Set(call.Argument(0).String(), call.Argument(1).String())
		return res
	})

	_ = res.Set("type", func(call sobek.FunctionCall) sobek.Value {
		w.Header().Set("Content-Type", call.Argument(0).String())
		return res
	})

	_ = res.Set("redirect", func(call sobek.FunctionCall) sobek.Value {
		code := http.StatusFound
		location := ""
		switch len(call.Arguments) {
		case 0:
			panic(p.rt.NewTypeError("res.redirect requires a location"))
		case 1:
			location = call.Argument(0).String()
		default:
			code = int(call.Argument(0).ToInteger())
			location = call.Argument(1).String()
		}
		w.Header().Set("Location", location)
		w.SetStatus(code)
		w.writeStatusThen(nil)
		return res
	})

	_ = res.Set("send", func(call sobek.FunctionCall) sobek.Value {
		body, contentType := encodeBody(call.Argument(0))
		w.setContentType(contentType)
		w.writeStatusThen(body)
		return res
	})

	_ = res.Set("json", func(call sobek.FunctionCall) sobek.Value {
		data, err := json.Marshal(call.Argument(0).Export())
		if err != nil {
			panic(p.rt.NewGoError(err))
		}
		w.setContentType("application/json; charset=utf-8")
		w.writeStatusThen(data)
		return res
	})

	_ = res.Set("end", func(call sobek.FunctionCall) sobek.Value {
		if len(call.Arguments) > 0 {
			body, contentType := encodeBody(call.Argument(0))
			w.setContentType(contentType)
			w.writeStatusThen(body)
			return res
		}
		w.writeStatusThen(nil)
		return res
	})

	_ = res.Set("flush", func(call sobek.FunctionCall) sobek.Value {
		w.Flush()
		return res
	})

	return res
}

func encodeBody(v sobek.Value) ([]byte, string) {
	if v == nil || sobek.IsUndefined(v) || sobek.IsNull(v) {
		return nil, "text/plain; charset=utf-8"
	}
	if s, ok := v.Export().(string); ok {
		return []byte(s), "text/plain; charset=utf-8"
	}
	data, err := json.Marshal(v.Export())
	if err != nil {
		return []byte(v.String()), "text/plain; charset=utf-8"
	}
	return data, "application/json; charset=utf-8"
}
