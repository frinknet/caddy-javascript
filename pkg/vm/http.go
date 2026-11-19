package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// DefaultMaxBody bounds the request body a script may read. A handler that
// asks for more fails instead of buffering it.
const DefaultMaxBody = int64(1 << 20)

var errRequestFinished = errors.New("javascript: request is finished")

// varsFactorySource builds the http.vars proxy. The Go callbacks are bound to
// one request, so each request gets its own proxy over the live variable table.
// globalsFactorySource installs a Proxy as the prototype of globalThis. An
// identifier that is not a real global resolves through it, so a script can
// read a Caddy placeholder such as site or host as a bare name while a request
// is being dispatched.
const globalsFactorySource = `(function (proto, live, known, read, write) {
	Object.setPrototypeOf(globalThis, new Proxy(proto, {
		has(target, key) {
			if (typeof key === "string" && live()) return true
			return key in target
		},
		get(target, key) {
			if (typeof key === "string" && live() && known(key)) return read(key)
			return target[key]
		},
		set(target, key, value, receiver) {
			if (typeof key === "string" && live()) {
				write(key, value)
				return true
			}
			return Reflect.set(target, key, value, receiver)
		},
	}))
})`

const varsFactorySource = `(function (get, set, has, del, keys) {
	return new Proxy(Object.create(null), {
		get(target, key) {
			if (typeof key !== "string" || !has(key)) return undefined
			return get(key)
		},
		set(target, key, value) {
			if (typeof key === "string") set(key, value)
			return true
		},
		has(target, key) {
			return typeof key === "string" && has(key)
		},
		deleteProperty(target, key) {
			if (typeof key === "string") del(key)
			return true
		},
		ownKeys() {
			return keys()
		},
		getOwnPropertyDescriptor(target, key) {
			if (typeof key !== "string" || !has(key)) return undefined
			return { value: get(key), enumerable: true, configurable: true, writable: true }
		},
	})
})`

// headersFactorySource wraps the request headers in a Proxy, so a script reads
// and writes them as ordinary properties.
const headersFactorySource = `(function (get, set, has, del, keys) {
	return new Proxy(Object.create(null), {
		get(target, key) {
			if (typeof key !== "string" || !has(key)) return undefined
			return get(key)
		},
		set(target, key, value) {
			if (typeof key === "string") set(key, String(value))
			return true
		},
		has(target, key) {
			return typeof key === "string" && has(key)
		},
		deleteProperty(target, key) {
			if (typeof key === "string") del(key)
			return true
		},
		ownKeys() {
			return keys()
		},
		getOwnPropertyDescriptor(target, key) {
			if (typeof key !== "string" || !has(key)) return undefined
			return { value: get(key), enumerable: true, configurable: true, writable: true }
		},
	})
})`

// requestState is the Go side of one http object. The VM goroutine owns it for
// the duration of a dispatch; anything a script reaches after the dispatch has
// returned is refused instead of racing the request goroutine.
type requestState struct {
	mu       sync.Mutex
	req      *http.Request
	resp     *response
	vars     map[string]any
	lookup   func(name string) (any, bool)
	set      func(name string, value any)
	delete   func(name string)
	maxBody  int64
	body     []byte
	bodyRead bool
	bodyErr  error
	finished bool
}

func newRequestState(req Request, resp *response) (*requestState, error) {
	if req.Req == nil {
		return nil, errors.New("javascript: request is required")
	}
	vars := req.Vars
	if vars == nil {
		vars = make(map[string]any)
	}
	maxBody := req.MaxBody
	if maxBody <= 0 {
		maxBody = DefaultMaxBody
	}
	return &requestState{
		req:     req.Req,
		resp:    resp,
		vars:    vars,
		lookup:  req.Lookup,
		set:     req.Set,
		delete:  req.Delete,
		maxBody: maxBody,
	}, nil
}

func (s *requestState) finish() {
	s.mu.Lock()
	s.finished = true
	s.mu.Unlock()
}

func (s *requestState) currentPath() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.req.URL.Path
}

func (s *requestState) checkLocked() error {
	if s.finished {
		return errRequestFinished
	}
	return nil
}

func (s *requestState) readBodyLocked() ([]byte, error) {
	if s.bodyRead {
		return s.body, s.bodyErr
	}
	s.bodyRead = true
	if s.req.Body == nil {
		s.body = nil
		return nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(s.req.Body, s.maxBody+1))
	if err != nil {
		s.bodyErr = fmt.Errorf("javascript: read request body: %w", err)
		return nil, s.bodyErr
	}
	if int64(len(body)) > s.maxBody {
		s.bodyErr = fmt.Errorf("javascript: request body exceeds %d bytes", s.maxBody)
		return nil, s.bodyErr
	}
	s.body = body
	return s.body, nil
}

func (s *requestState) bodyText() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkLocked(); err != nil {
		return "", err
	}
	body, err := s.readBodyLocked()
	return string(body), err
}

func (s *requestState) bodyJSON() (any, error) {
	s.mu.Lock()
	if err := s.checkLocked(); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	body, err := s.readBodyLocked()
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return nil, fmt.Errorf("javascript: decode request body: %w", err)
	}
	return value, nil
}

func (s *requestState) bodyBytes(rt *quickjs.Runtime) (quickjs.Value, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkLocked(); err != nil {
		return quickjs.Value{}, err
	}
	body, err := s.readBodyLocked()
	if err != nil {
		return quickjs.Value{}, err
	}
	return rt.NewBytes(body), nil
}

func (s *requestState) headerGet(name string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkLocked(); err != nil {
		return nil, err
	}
	values := s.req.Header.Values(name)
	switch len(values) {
	case 0:
		return nil, nil
	case 1:
		return values[0], nil
	default:
		return values, nil
	}
}

func (s *requestState) headerSet(name, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkLocked(); err != nil {
		return err
	}
	s.req.Header.Set(name, value)
	return nil
}

func (s *requestState) headerHas(name string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkLocked(); err != nil {
		return false, err
	}
	_, ok := s.req.Header[http.CanonicalHeaderKey(name)]
	return ok, nil
}

func (s *requestState) headerDelete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkLocked(); err != nil {
		return err
	}
	s.req.Header.Del(name)
	return nil
}

func (s *requestState) headerKeys() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkLocked(); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(s.req.Header))
	for name := range s.req.Header {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	return keys, nil
}

func (s *requestState) rewrite(fields any) error {
	options, ok := fields.(map[string]any)
	if !ok {
		return errors.New("javascript: rewrite() expects an object")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkLocked(); err != nil {
		return err
	}

	if value, ok := options["method"]; ok {
		method, ok := value.(string)
		if !ok || method == "" {
			return errors.New("javascript: rewrite() method must be a non-empty string")
		}
		s.req.Method = method
	}
	if value, ok := options["path"]; ok {
		path, ok := value.(string)
		if !ok || !strings.HasPrefix(path, "/") {
			return errors.New("javascript: rewrite() path must start with /")
		}
		s.req.URL.Path = path
		s.req.URL.RawPath = ""
		s.req.RequestURI = s.req.URL.RequestURI()
	}
	if value, ok := options["headers"]; ok {
		headers, ok := value.(map[string]any)
		if !ok {
			return errors.New("javascript: rewrite() headers must be an object")
		}
		for key, value := range headers {
			s.req.Header.Set(key, fmt.Sprint(value))
		}
	}
	return nil
}

// varFind resolves a name from the request's variable table. Bare identifiers
// use placeholder instead; the two namespaces do not fall through to each
// other.
func (s *requestState) varFind(name string) (any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return nil, false
	}
	value, ok := s.vars[name]
	return value, ok
}

// placeholder resolves a bare identifier through the replacer.
func (s *requestState) placeholder(name string) (any, bool) {
	if s.lookup == nil {
		return nil, false
	}
	return s.lookup(name)
}

func (s *requestState) varGet(name string) any {
	value, _ := s.varFind(name)
	return value
}

// globalSet assigns a bare identifier. A bare name is a Caddy placeholder, so
// the write goes to the replacer and never to the vars table; http.vars is the
// only path to a variable.
func (s *requestState) globalSet(name string, value any) error {
	if _, ok := value.(quickjs.Value); ok {
		return errors.New("javascript: cannot write a function to a Caddy global")
	}

	s.mu.Lock()
	if err := s.checkLocked(); err != nil {
		s.mu.Unlock()
		return err
	}
	set, del := s.set, s.delete
	s.mu.Unlock()

	if value == nil {
		if del == nil {
			return errors.New("javascript: globals are read-only")
		}
		del(name)
		return nil
	}
	if set == nil {
		return errors.New("javascript: globals are read-only")
	}
	set(name, value)
	return nil
}

func (s *requestState) varSet(name string, value any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkLocked(); err != nil {
		return err
	}
	if value == nil {
		delete(s.vars, name)
		return nil
	}
	s.vars[name] = value
	return nil
}

func (s *requestState) varDelete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkLocked(); err != nil {
		return err
	}
	delete(s.vars, name)
	return nil
}

func (s *requestState) varHas(name string) (bool, error) {
	s.mu.Lock()
	if err := s.checkLocked(); err != nil {
		s.mu.Unlock()
		return false, err
	}
	s.mu.Unlock()
	_, ok := s.varFind(name)
	return ok, nil
}

func (s *requestState) varKeys() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkLocked(); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(s.vars))
	for key := range s.vars {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys, nil
}

// newHTTP assembles the fresh http object handed to the entry module and to
// every route layer.
func (v *VM) newHTTP(ctx context.Context, st *requestState, resp *response) (quickjs.Value, error) {
	query := make(map[string]any)
	for key, values := range st.req.URL.Query() {
		switch len(values) {
		case 0:
		case 1:
			query[key] = values[0]
		default:
			query[key] = values
		}
	}

	cookies := make(map[string]string)
	for _, cookie := range st.req.Cookies() {
		cookies[cookie.Name] = cookie.Value
	}

	headers, err := v.newHeaders(ctx, st)
	if err != nil {
		return quickjs.Value{}, err
	}

	body := v.rt.NewObject()
	for name, fn := range map[string]any{
		"text":  st.bodyText,
		"json":  st.bodyJSON,
		"bytes": func() (quickjs.Value, error) { return st.bodyBytes(v.rt) },
	} {
		if err := body.Set(name, fn); err != nil {
			return quickjs.Value{}, err
		}
	}

	obj := v.rt.NewObject()
	for name, value := range map[string]any{
		"method":   st.req.Method,
		"url":      st.req.URL.String(),
		"path":     st.req.URL.Path,
		"query":    query,
		"cookies":  cookies,
		"host":     st.req.Host,
		"remote":   st.req.RemoteAddr,
		"protocol": st.req.Proto,
		"headers":  headers,
		"body":     body,
		"respond":  resp.respond,
		"rewrite":  st.rewrite,
	} {
		if err := obj.Set(name, value); err != nil {
			return quickjs.Value{}, err
		}
	}

	vars, err := v.newVars(ctx, st)
	if err != nil {
		return quickjs.Value{}, err
	}
	if err := obj.Set("vars", vars); err != nil {
		return quickjs.Value{}, err
	}
	return obj, nil
}

func (v *VM) newVars(ctx context.Context, st *requestState) (quickjs.Value, error) {
	return v.varsFactory.CallContext(ctx,
		st.varGet,
		st.varSet,
		st.varHas,
		st.varDelete,
		st.varKeys,
	)
}

func (v *VM) newHeaders(ctx context.Context, st *requestState) (quickjs.Value, error) {
	return v.headerFactory.CallContext(ctx,
		st.headerGet,
		st.headerSet,
		st.headerHas,
		st.headerDelete,
		st.headerKeys,
	)
}
