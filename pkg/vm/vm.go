package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
	"github.com/go-quickjs/go-quickjs/stdlib"
	"go.uber.org/zap"
)

const (
	DefaultMemory    = int64(64 << 20)
	DefaultQueueSize = 256
)

var (
	ErrQueueFull = errors.New("javascript: vm queue full")
	ErrClosed    = errors.New("javascript: vm closed")
	ErrStopped   = errors.New("javascript: vm stopped")
)

type Config struct {
	ScriptPath     string
	RootDir        string
	Memory         int64
	QueueSize      int
	RequireHandler bool
	Logger         *zap.Logger
}

type Request struct {
	Method string
	Path   string
}

type Response struct {
	Status  int
	Headers map[string]string
	Body    []byte
}

type VM struct {
	rt    *quickjs.Runtime
	loop  *stdlib.Loop
	root  *os.Root
	entry quickjs.Value

	slots  chan struct{}
	closed chan struct{}
	done   chan struct{}

	ctx    context.Context
	cancel context.CancelFunc

	closeOnce sync.Once
	errMu     sync.Mutex
	runErr    error

	logger *zap.Logger
}

func New(cfg Config) (*VM, error) {
	if cfg.Memory <= 0 {
		cfg.Memory = DefaultMemory
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = DefaultQueueSize
	}
	if cfg.ScriptPath == "" {
		return nil, fmt.Errorf("javascript: script path is required")
	}
	if cfg.RootDir == "" {
		cfg.RootDir = filepath.Dir(cfg.ScriptPath)
	}

	script, err := filepath.Abs(cfg.ScriptPath)
	if err != nil {
		return nil, fmt.Errorf("javascript: script path: %w", err)
	}
	dir, err := filepath.Abs(cfg.RootDir)
	if err != nil {
		return nil, fmt.Errorf("javascript: root: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("javascript: root %q: %w", dir, err)
	}

	sb := &sandbox{root: root, dir: dir}
	entryName, err := sb.name(script)
	if err != nil {
		root.Close()
		return nil, err
	}
	src, err := sb.read(entryName)
	if err != nil {
		root.Close()
		return nil, err
	}

	rt := quickjs.New(
		quickjs.WithMemoryLimit(cfg.Memory),
		quickjs.WithStackSize(1<<16),
		quickjs.WithMaxCallDepth(1000),
		quickjs.WithLocale("en-US"),
	)
	rt.SetTimeZone(time.UTC)

	loop := stdlib.NewLoop(rt)
	if err := stdlib.Install(rt, stdlib.Config{Loop: loop, NoWebAPIs: true}); err != nil {
		rt.Close()
		root.Close()
		return nil, fmt.Errorf("javascript: install runtime: %w", err)
	}

	rt.SetModuleLoader(func(specifier, referrer string) (string, string, error) {
		name, err := sb.resolve(specifier, referrer)
		if err != nil {
			return "", "", err
		}
		src, err := sb.read(name)
		if err != nil {
			return "", "", err
		}
		return string(src), name, nil
	})

	ns, err := rt.EvalModule(entryName, string(src))
	if err != nil {
		rt.Close()
		root.Close()
		return nil, fmt.Errorf("javascript: evaluate %s: %w", entryName, err)
	}

	entry, err := ns.Get("default")
	if err != nil {
		rt.Close()
		root.Close()
		return nil, fmt.Errorf("javascript: read default export: %w", err)
	}
	if cfg.RequireHandler && !entry.IsFunction() {
		rt.Close()
		root.Close()
		return nil, fmt.Errorf("javascript: %s must export a default function", entryName)
	}

	v := &VM{
		rt:     rt,
		loop:   loop,
		root:   root,
		entry:  entry,
		slots:  make(chan struct{}, cfg.QueueSize),
		closed: make(chan struct{}),
		done:   make(chan struct{}),
		logger: cfg.Logger,
	}
	v.ctx, v.cancel = context.WithCancel(context.Background())
	loop.Begin()
	go v.run()
	return v, nil
}

func (v *VM) run() {
	defer close(v.done)
	if err := v.loop.Run(v.ctx); err != nil && !errors.Is(err, context.Canceled) {
		v.setErr(fmt.Errorf("javascript: vm loop: %w", err))
		if v.logger != nil {
			v.logger.Error("javascript: vm loop stopped", zap.Error(err))
		}
	}
	v.rt.Close()
	_ = v.root.Close()
}

func (v *VM) Handle(ctx context.Context, req Request) (*Response, error) {
	if err := v.err(); err != nil {
		return nil, err
	}

	select {
	case v.slots <- struct{}{}:
	default:
		return nil, ErrQueueFull
	}
	defer func() { <-v.slots }()

	type result struct {
		resp *Response
		err  error
	}
	ch := make(chan result, 1)

	v.loop.Post(func() {
		resp := &response{}
		http := map[string]any{
			"method":  req.Method,
			"path":    req.Path,
			"respond": resp.respond,
		}
		_, err := v.entry.CallContext(ctx, http)
		ch <- result{resp: resp.finish(), err: err}
	})

	select {
	case r := <-ch:
		return r.resp, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-v.closed:
		return nil, ErrClosed
	case <-v.done:
		if err := v.runError(); err != nil {
			return nil, err
		}
		return nil, ErrClosed
	}
}

func (v *VM) Close() error {
	v.closeOnce.Do(func() {
		close(v.closed)
		v.loop.Close()
		v.cancel()
	})
	<-v.done
	return v.runError()
}

func (v *VM) runError() error {
	v.errMu.Lock()
	defer v.errMu.Unlock()
	return v.runErr
}

func (v *VM) setErr(err error) {
	v.errMu.Lock()
	if v.runErr == nil {
		v.runErr = err
	}
	v.errMu.Unlock()
}

func (v *VM) err() error {
	v.errMu.Lock()
	defer v.errMu.Unlock()
	if v.runErr != nil {
		return v.runErr
	}
	select {
	case <-v.closed:
		return ErrClosed
	default:
		return nil
	}
}

type response struct {
	mu       sync.Mutex
	called   bool
	finished bool
	status   int
	headers  map[string]string
	body     []byte
}

func (r *response) respond(status int, headers map[string]any, body any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished {
		return errors.New("javascript: response already finished")
	}
	if r.called {
		return errors.New("javascript: response already sent")
	}
	if status < 100 || status > 999 {
		return fmt.Errorf("javascript: invalid status %d", status)
	}
	r.called = true
	r.status = status
	r.headers = make(map[string]string, len(headers))
	for k, val := range headers {
		if val == nil {
			continue
		}
		r.headers[k] = fmt.Sprint(val)
	}
	switch b := body.(type) {
	case nil:
		r.body = nil
	case string:
		r.body = []byte(b)
	case []byte:
		r.body = append([]byte(nil), b...)
	default:
		r.body = []byte(fmt.Sprint(b))
	}
	return nil
}

func (r *response) finish() *Response {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finished = true
	if !r.called {
		return nil
	}
	return &Response{
		Status:  r.status,
		Headers: r.headers,
		Body:    r.body,
	}
}

type sandbox struct {
	root *os.Root
	dir  string
}

func (s *sandbox) name(abs string) (string, error) {
	rel, err := filepath.Rel(s.dir, abs)
	if err != nil {
		return "", fmt.Errorf("javascript: resolve %q: %w", abs, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("javascript: %q is outside the root %q", abs, s.dir)
	}
	return path.Join("/", filepath.ToSlash(rel)), nil
}

func (s *sandbox) resolve(specifier, referrer string) (string, error) {
	if specifier == "" {
		return "", fmt.Errorf("javascript: empty module specifier")
	}
	if !strings.HasPrefix(specifier, ".") && !strings.HasPrefix(specifier, "/") && !filepath.IsAbs(specifier) {
		return "", fmt.Errorf("javascript: bare module specifier %q is not supported", specifier)
	}

	var name string
	if strings.HasPrefix(specifier, "/") {
		name = path.Clean(filepath.ToSlash(specifier))
	} else {
		base := "/"
		if referrer != "" {
			base = path.Dir(referrer)
		}
		name = path.Join(base, filepath.ToSlash(specifier))
	}

	abs := filepath.Join(s.dir, filepath.FromSlash(strings.TrimPrefix(name, "/")))
	rel, err := filepath.Rel(s.dir, abs)
	if err != nil {
		return "", fmt.Errorf("javascript: resolve %q: %w", specifier, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("javascript: module %q escapes the root", specifier)
	}
	return path.Join("/", filepath.ToSlash(rel)), nil
}

func (s *sandbox) read(name string) ([]byte, error) {
	rel := strings.TrimPrefix(path.Clean(name), "/")
	f, err := s.root.Open(filepath.FromSlash(rel))
	if err != nil {
		return nil, fmt.Errorf("javascript: read %q: %w", name, err)
	}
	defer f.Close()
	return io.ReadAll(f)
}
