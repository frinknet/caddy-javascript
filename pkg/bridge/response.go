package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/grafana/sobek"
)

var (
	errDotfileDenied  = errors.New("dotfiles are denied")
	errDotfileIgnored = errors.New("dotfile ignored")
	errPathEscape     = errors.New("path escapes the file root")
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

func (p *Program) BuildResponse(w *ResponseWriter, req *sobek.Object) *sobek.Object {
	res := p.rt.NewObject()
	if req != nil {
		_ = res.Set("req", req)
	}

	_ = res.Set("status", func(call sobek.FunctionCall) sobek.Value {
		w.SetStatus(int(call.Argument(0).ToInteger()))
		return res
	})

	_ = res.Set("set", func(call sobek.FunctionCall) sobek.Value {
		w.Header().Set(call.Argument(0).String(), call.Argument(1).String())
		return res
	})

	_ = res.Set("append", func(call sobek.FunctionCall) sobek.Value {
		field := call.Argument(0).String()
		value := call.Argument(1)
		if items, ok := value.Export().([]interface{}); ok {
			for _, item := range items {
				w.Header().Add(field, fmt.Sprint(item))
			}
			return res
		}
		w.Header().Add(field, value.String())
		return res
	})

	_ = res.Set("type", func(call sobek.FunctionCall) sobek.Value {
		w.Header().Set("Content-Type", call.Argument(0).String())
		return res
	})

	_ = res.Set("location", func(call sobek.FunctionCall) sobek.Value {
		location := call.Argument(0).String()
		if location == "back" {
			location = p.requestHeader(req, "Referer")
			if location == "" {
				location = "/"
			}
		}
		w.Header().Set("Location", location)
		return res
	})

	_ = res.Set("cookie", func(call sobek.FunctionCall) sobek.Value {
		cookie, err := p.buildCookie(call)
		if err != nil {
			panic(p.rt.NewGoError(err))
		}
		w.Header().Add("Set-Cookie", cookie)
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

	_ = res.Set("sendStatus", func(call sobek.FunctionCall) sobek.Value {
		code := int(call.Argument(0).ToInteger())
		text := http.StatusText(code)
		if text == "" {
			text = strconv.Itoa(code)
		}
		w.SetStatus(code)
		w.setContentType("text/plain; charset=utf-8")
		var body []byte
		if code != http.StatusNoContent && code != http.StatusNotModified {
			body = []byte(text)
		}
		w.writeStatusThen(body)
		return res
	})

	_ = res.Set("sendFile", func(call sobek.FunctionCall) sobek.Value {
		name := call.Argument(0).String()
		var opts *sobek.Object
		var cb sobek.Callable
		for i := 1; i < len(call.Arguments); i++ {
			value := call.Argument(i)
			if isNilValue(value) {
				continue
			}
			if fn, ok := sobek.AssertFunction(value); ok {
				cb = fn
				continue
			}
			if obj := value.ToObject(p.rt); obj != nil {
				opts = obj
			}
		}
		p.sendFile(w, req, name, opts, cb)
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

func (p *Program) requestHeader(req *sobek.Object, name string) string {
	if req == nil {
		return ""
	}
	headers := req.Get("headers")
	if isNilValue(headers) {
		return ""
	}
	obj := headers.ToObject(p.rt)
	if obj == nil {
		return ""
	}
	value := obj.Get(http.CanonicalHeaderKey(name))
	if isNilValue(value) {
		return ""
	}
	return value.String()
}

func (p *Program) buildCookie(call sobek.FunctionCall) (string, error) {
	name := call.Argument(0).String()
	if name == "" {
		return "", errors.New("res.cookie requires a name")
	}
	cookie := &http.Cookie{Name: name, Value: call.Argument(1).String(), Path: "/"}

	opts := call.Argument(2)
	if isNilValue(opts) {
		cookie.Value = encodeCookieValue(cookie.Value)
		return cookie.String(), nil
	}
	obj := opts.ToObject(p.rt)
	if obj == nil {
		cookie.Value = encodeCookieValue(cookie.Value)
		return cookie.String(), nil
	}

	if encode, ok := sobek.AssertFunction(obj.Get("encode")); ok {
		encoded, err := encode(sobek.Undefined(), p.rt.ToValue(cookie.Value))
		if err != nil {
			return "", err
		}
		cookie.Value = encoded.String()
	} else {
		cookie.Value = encodeCookieValue(cookie.Value)
	}

	if v := obj.Get("maxAge"); !isNilValue(v) {
		cookie.MaxAge = int(v.ToInteger() / 1000)
	}
	if v := obj.Get("expires"); !isNilValue(v) {
		if when, ok := v.Export().(time.Time); ok {
			cookie.Expires = when
		}
	}
	if v := obj.Get("path"); !isNilValue(v) {
		cookie.Path = v.String()
	}
	if v := obj.Get("domain"); !isNilValue(v) {
		cookie.Domain = v.String()
	}
	if v := obj.Get("secure"); !isNilValue(v) {
		cookie.Secure = v.ToBoolean()
	}
	if v := obj.Get("httpOnly"); !isNilValue(v) {
		cookie.HttpOnly = v.ToBoolean()
	}
	if v := obj.Get("sameSite"); !isNilValue(v) {
		switch strings.ToLower(v.String()) {
		case "strict":
			cookie.SameSite = http.SameSiteStrictMode
		case "lax":
			cookie.SameSite = http.SameSiteLaxMode
		case "none":
			cookie.SameSite = http.SameSiteNoneMode
		}
	}
	return cookie.String(), nil
}

// encodeCookieValue mirrors JavaScript's encodeURIComponent, which is what
// Express applies to cookie values by default.
func encodeCookieValue(value string) string {
	return strings.ReplaceAll(url.QueryEscape(value), "+", "%20")
}

// sendFilePath resolves name against root and reports whether the result stays
// inside that root. A name that climbs above root is refused rather than
// silently served from a parent directory.
func sendFilePath(root, name string) (string, bool) {
	base := path.Join("/", root)
	target := path.Join(base, name)
	if base == "/" || target == base || strings.HasPrefix(target, base+"/") {
		return target, true
	}
	return target, false
}

type sendFileOptions struct {
	root      string
	headers   http.Header
	dotfiles  string
	maxAge    time.Duration
	maxAgeSet bool
}

func (p *Program) sendFile(w *ResponseWriter, req *sobek.Object, name string, opts *sobek.Object, cb sobek.Callable) {
	cfg := sendFileOptions{dotfiles: "ignore"}
	if opts != nil {
		if v := opts.Get("root"); !isNilValue(v) {
			cfg.root = v.String()
		}
		if v := opts.Get("dotfiles"); !isNilValue(v) {
			cfg.dotfiles = strings.ToLower(v.String())
		}
		if v := opts.Get("maxAge"); !isNilValue(v) {
			cfg.maxAgeSet = true
			cfg.maxAge = time.Duration(v.ToInteger()) * time.Millisecond
		}
		if v := opts.Get("headers"); !isNilValue(v) {
			cfg.headers = make(http.Header)
			if hobj := v.ToObject(p.rt); hobj != nil {
				for _, key := range hobj.Keys() {
					value := hobj.Get(key)
					if !isNilValue(value) {
						cfg.headers.Set(key, value.String())
					}
				}
			}
		}
	}

	target, insideRoot := sendFilePath(cfg.root, name)
	if !insideRoot {
		p.finishSendFile(w, cb, errPathEscape)
		return
	}
	switch dotfileAction(target, cfg.dotfiles) {
	case dotDeny:
		p.finishSendFile(w, cb, errDotfileDenied)
		return
	case dotIgnore:
		p.finishSendFile(w, cb, errDotfileIgnored)
		return
	}

	ex := p.trackAsync()
	read := func() {
		data, err := p.fs.ReadFile(target)
		p.submitAsync(ex, func() {
			if err != nil {
				p.finishSendFile(w, cb, err)
				return
			}
			p.writeSendFile(w, req, target, data, cfg)
		})
	}
	if ex != nil {
		go read()
		return
	}
	read()
}

func (p *Program) finishSendFile(w *ResponseWriter, cb sobek.Callable, err error) {
	if cb != nil {
		_, _ = cb(sobek.Undefined(), p.rt.NewGoError(err))
		return
	}
	if w.Written() {
		return
	}
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, errDotfileDenied):
		status = http.StatusForbidden
	case errors.Is(err, errDotfileIgnored), errors.Is(err, errPathEscape), errors.Is(err, os.ErrNotExist):
		status = http.StatusNotFound
	}
	http.Error(w, http.StatusText(status), status)
}

func (p *Program) writeSendFile(w *ResponseWriter, req *sobek.Object, name string, data []byte, cfg sendFileOptions) {
	if w.Written() {
		return
	}
	for key, values := range cfg.headers {
		w.Header()[key] = values
	}
	if w.Header().Get("Content-Type") == "" {
		contentType := mime.TypeByExtension(path.Ext(name))
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		w.Header().Set("Content-Type", contentType)
	}
	if cfg.maxAgeSet {
		maxAge := int64(cfg.maxAge / time.Second)
		if maxAge < 0 {
			maxAge = 0
		}
		w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", maxAge))
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	if p.requestMethod(req) != "HEAD" {
		_, _ = w.Write(data)
	}
}

func (p *Program) requestMethod(req *sobek.Object) string {
	if req == nil {
		return ""
	}
	method := req.Get("method")
	if isNilValue(method) {
		return ""
	}
	return strings.ToUpper(method.String())
}
