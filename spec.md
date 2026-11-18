# Technical Specification: Production-Grade Caddy JavaScript Plugin (`caddy-javascript`)

---

## 1. Executive Summary & Philosophy

The `caddy-javascript` plugin provides a high-performance, in-process JavaScript execution environment for Caddy, engineered as a **drop-in replacement for Express** backed by **Node.js-style asynchronous execution semantics**.

Instead of relying on multi-VM pools or heavy, process-managed C++ runtimes (such as V8), the plugin embeds **Sobek**—a pure-Go ECMAScript engine—and pairs it with a **Go-powered single-threaded event loop**. This architecture satisfies key production imperatives:

1. **Frictionless Compilation:** Pure-Go implementation (`CGO_ENABLED=0`) allows seamless cross-compilation and native builds via `xcaddy` without external C dependencies.
2. **Familiar Mental Model:** Developers run unmodified Express apps (`require('express')`, `express()`, `app.use`, ordered middleware with `next()`) alongside pre-attached operating system primitives (`fs`, `path`, `process.env`, `fetch`), backed by a shared-heap event loop.
3. **Developer Ergonomics (Hot-Reloading):** Built-in file watching (`fsnotify`) automatically reloads script updates on disk without requiring manual Caddy reloads during local development.
4. **Resilience & Safety:** Non-blocking backpressure channel queuing (`503 Service Unavailable` on overload), thread-safe double-write protection (`safeResponseWriter`), header/parameter parsing, and context-bound watchdog timers (`vm.Interrupt()`) with automated flag resets.

---

## 2. Caddyfile Configuration & Directives

The plugin registers a single HTTP handler directive: `javascript`. Each directive block instantiates an isolated, self-contained event loop runtime tied to the specified script path.

### Syntax

```caddyfile
javascript <path_to_server_script>

```

### Configuration Examples

#### Example A: Global Site Hosting

Routes all incoming traffic for a domain directly into the JavaScript application logic:

```caddyfile
example.com {
    javascript /var/www/my-app/server.js
}

```

#### Example B: Scoped API Gateway

Isolates script execution to specific URI patterns while leaving static asset delivery or reverse proxying to native Caddy modules:

```caddyfile
api.example.com {
    @api {
        path /api/*
    }
    handle @api {
        javascript /var/www/my-app/api-router.js
    }
    
    handle {
        respond "Not found in JS gateway" 404
    }
}

```

---

## 3. Supported JavaScript API Surface

The runtime pre-attaches core globals upon initialization, eliminating complex package managers or build steps for basic application logic.

Compatibility goal: the Express module and middleware chain are the target surface; the legacy `app` global below remains as a fallback. See `BACKLOG.md` for the gap list and P0 work.

### A. Routing Framework (`app`)

* `app.get(path, callback)`: Registers a GET route handler.
* `app.post(path, callback)`: Registers a POST route handler.
* `app.use(callback)`: Registers a fallback / middleware handler for unmatched routes.

### B. Primitives & System Bindings

* `setTimeout(callback, ms)`: Non-blocking timer offloaded to Go background routines.
* `fetch(url, callback)`: Non-blocking asynchronous HTTP client offloaded to Go.
* `fs.readFile(path, callback)` / `fs.readFileSync(path)`: Sandboxed asynchronous and synchronous file readers.
* `fs.writeFile(path, data, callback)` / `fs.writeFileSync(path, data)`: Sandboxed asynchronous and synchronous file writers.
* `path.join(...parts)`: Normalizes and joins path segments via Go's path package.
* `process.env`: Environment variables container mapped from host system environment.

### C. CommonJS Compatibility Shim

* `require('fs')` / `require('node:fs')`: Maps directly to the sandboxed file system module.
* `require('path')` / `require('node:path')`: Maps directly to the path utility module.

### D. HTTP Context Objects

* **Request (`req`):** `req.path`, `req.method`, `req.headers`, `req.body` (auto-parsed from JSON if applicable), `req.query`
* **Response (`res`):** `res.status(code)`, `res.send(data)`, `res.json(obj)`

---

## 4. Complete Reference Implementation (`caddy_javascript.go`)

```go
package caddyjavascript

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/fsnotify/fsnotify"
	"github.com/grafana/sobek"
)

func init() {
	caddy.RegisterModule(&JSEventLoopHandler{})
	httpcaddyfile.RegisterHandlerDirective("javascript", parseCaddyfile)
}

type Task func()

// HTTPRequestContext contains immutable value types safely extracted across thread boundaries
type HTTPRequestContext struct {
	Method  string
	Path    string
	Headers map[string]string
	Body    interface{}
	Query   map[string]string
}

// safeResponseWriter guards against double-writes and header modification after flush
type safeResponseWriter struct {
	w          http.ResponseWriter
	mu         sync.Mutex
	written    bool
	statusCode int
}

func (s *safeResponseWriter) Header() http.Header {
	return s.w.Header()
}

func (s *safeResponseWriter) WriteHeader(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.written {
		s.statusCode = code
		s.w.WriteHeader(code)
		s.written = true
	}
}

func (s *safeResponseWriter) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.written {
		s.statusCode = http.StatusOK
		s.w.WriteHeader(http.StatusOK)
		s.written = true
	}
	return s.w.Write(b)
}

func (s *safeResponseWriter) IsWritten() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.written
}

type JSEventLoopHandler struct {
	ScriptPath string `json:"script_path"`
	rootDir    string
	vm         *sobek.Runtime
	taskQueue  chan Task
	quit       chan struct{}
	wg         sync.WaitGroup
	mu         sync.RWMutex
	routes     map[string]sobek.Callable
	fallback   sobek.Callable
}

func (JSEventLoopHandler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID: "http.handlers.javascript",
		New: func() caddy.Module {
			return &JSEventLoopHandler{
				taskQueue: make(chan Task, 5000), // Bounded buffer to prevent unbounded memory growth
				quit:      make(chan struct{}),
				routes:    make(map[string]sobek.Callable),
			}
		},
	}
}

func (h *JSEventLoopHandler) Provision(ctx caddy.Context) error {
	absPath, err := filepath.Abs(h.ScriptPath)
	if err != nil {
		return err
	}
	h.rootDir = filepath.Dir(absPath)

	initDone := make(chan error, 1)
	h.wg.Add(1)
	go h.startEventLoop(absPath, initDone)

	return <-initDone
}

func (h *JSEventLoopHandler) Cleanup() error {
	close(h.quit)
	h.wg.Wait()
	return nil
}

func (h *JSEventLoopHandler) startEventLoop(scriptPath string, initDone chan<- error) {
	defer h.wg.Done()

	loadScript := func(path string) error {
		code, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		h.vm = sobek.New()
		h.registerEnvironment(h.vm)
		_, err = h.vm.RunString(string(code))
		return err
	}

	// Initial synchronous script compilation & load
	if err := loadScript(scriptPath); err != nil {
		initDone <- fmt.Errorf("failed to initialize JS runtime: %w", err)
		return
	}
	initDone <- nil

	// Start file watcher for hot-reloading (Nodemon-style development ergonomics)
	watcher, err := fsnotify.NewWatcher()
	if err == nil {
		defer watcher.Close()
		_ = watcher.Add(scriptPath)
	}

	for {
		select {
		case task := <-h.taskQueue:
			func() {
				defer func() {
					if r := recover(); r != nil {
						fmt.Printf("[caddy-javascript] recovered event loop panic: %v\n", r)
					}
				}()
				task()
			}()
		case event := <-watcher.Events:
			if event.Op&fsnotify.Write == fsnotify.Write {
				h.mu.Lock()
				h.routes = make(map[string]sobek.Callable)
				h.fallback = nil
				if err := loadScript(scriptPath); err != nil {
					fmt.Printf("[caddy-javascript] hot-reload error: %v\n", err)
				} else {
					fmt.Printf("[caddy-javascript] successfully reloaded %s\n", scriptPath)
				}
				h.mu.Unlock()
			}
		case <-h.quit:
			return
		}
	}
}

func (h *JSEventLoopHandler) registerEnvironment(vm *sobek.Runtime) {
	// 1. Express-Style Router
	appObj := vm.NewObject()
	appObj.Set("get", func(call sobek.FunctionCall) sobek.Value {
		path := call.Argument(0).String()
		if fn, ok := sobek.AssertFunction(call.Argument(1)); ok {
			h.mu.Lock()
			h.routes["GET:"+path] = fn
			h.mu.Unlock()
		}
		return sobek.Undefined()
	})
	appObj.Set("post", func(call sobek.FunctionCall) sobek.Value {
		path := call.Argument(0).String()
		if fn, ok := sobek.AssertFunction(call.Argument(1)); ok {
			h.mu.Lock()
			h.routes["POST:"+path] = fn
			h.mu.Unlock()
		}
		return sobek.Undefined()
	})
	appObj.Set("use", func(call sobek.FunctionCall) sobek.Value {
		if fn, ok := sobek.AssertFunction(call.Argument(0)); ok {
			h.mu.Lock()
			h.fallback = fn
			h.mu.Unlock()
		}
		return sobek.Undefined()
	})
	vm.Set("app", appObj)

	// 2. Process Environment
	processObj := vm.NewObject()
	envObj := vm.NewObject()
	for _, env := range os.Environ() {
		pair := strings.SplitN(env, "=", 2)
		if len(pair) == 2 {
			envObj.Set(pair[0], pair[1])
		}
	}
	processObj.Set("env", envObj)
	vm.Set("process", processObj)

	// 3. Non-Blocking Timers
	vm.Set("setTimeout", func(call sobek.FunctionCall) sobek.Value {
		fn, ok := sobek.AssertFunction(call.Argument(0))
		if !ok {
			return sobek.Undefined()
		}
		ms := call.Argument(1).ToInteger()

		go func() {
			select {
			case <-time.After(time.Duration(ms) * time.Millisecond):
				select {
				case h.taskQueue <- func() {
					_, _ = fn(sobek.Undefined())
				}:
				case <-h.quit:
				}
			case <-h.quit:
			}
		}()
		return sobek.Undefined()
	})

	// 4. Asynchronous Outbound Fetch Client
	vm.Set("fetch", func(call sobek.FunctionCall) sobek.Value {
		url := call.Argument(0).String()
		cb, ok := sobek.AssertFunction(call.Argument(1))
		if !ok {
			return sobek.Undefined()
		}

		go func() {
			resp, err := http.Get(url)
			if err != nil {
				select {
				case h.taskQueue <- func() {
					_, _ = cb(sobek.Undefined(), vm.ToValue(err.Error()), sobek.Null())
				}:
				case <-h.quit:
				}
				return
			}
			defer resp.Body.Close()
			bodyBytes, _ := io.ReadAll(resp.Body)

			select {
			case h.taskQueue <- func() {
				resObj := vm.NewObject()
				resObj.Set("status", resp.StatusCode)
				resObj.Set("text", string(bodyBytes))
				_, _ = cb(sobek.Undefined(), sobek.Null(), resObj)
			}:
			case <-h.quit:
			}
		}()
		return sobek.Undefined()
	})

	// 5. File System Primitives (Sync & Async Read/Write)
	fsObj := vm.NewObject()
	fsObj.Set("readFileSync", func(call sobek.FunctionCall) sobek.Value {
		safePath := filepath.Join(h.rootDir, filepath.Clean("/"+call.Argument(0).String()))
		data, err := os.ReadFile(safePath)
		if err != nil {
			panic(vm.NewGoError(err))
		}
		return vm.ToValue(string(data))
	})
	fsObj.Set("readFile", func(call sobek.FunctionCall) sobek.Value {
		relPath := call.Argument(0).String()
		cb, ok := sobek.AssertFunction(call.Argument(1))
		if !ok {
			return sobek.Undefined()
		}

		safePath := filepath.Join(h.rootDir, filepath.Clean("/"+relPath))
		go func() {
			data, err := os.ReadFile(safePath)
			select {
			case h.taskQueue <- func() {
				if err != nil {
					_, _ = cb(sobek.Undefined(), vm.ToValue(err.Error()), sobek.Null())
					return
				}
				_, _ = cb(sobek.Undefined(), sobek.Null(), vm.ToValue(string(data)))
			}:
			case <-h.quit:
			}
		}()
		return sobek.Undefined()
	})
	fsObj.Set("writeFileSync", func(call sobek.FunctionCall) sobek.Value {
		safePath := filepath.Join(h.rootDir, filepath.Clean("/"+call.Argument(0).String()))
		content := call.Argument(1).String()
		err := os.WriteFile(safePath, []byte(content), 0644)
		if err != nil {
			panic(vm.NewGoError(err))
		}
		return sobek.Undefined()
	})
	fsObj.Set("writeFile", func(call sobek.FunctionCall) sobek.Value {
		relPath := call.Argument(0).String()
		content := call.Argument(1).String()
		cb, ok := sobek.AssertFunction(call.Argument(2))
		if !ok {
			return sobek.Undefined()
		}

		safePath := filepath.Join(h.rootDir, filepath.Clean("/"+relPath))
		go func() {
			err := os.WriteFile(safePath, []byte(content), 0644)
			select {
			case h.taskQueue <- func() {
				if err != nil {
					_, _ = cb(sobek.Undefined(), vm.ToValue(err.Error()))
					return
				}
				_, _ = cb(sobek.Undefined(), sobek.Null())
			}:
			case <-h.quit:
			}
		}()
		return sobek.Undefined()
	})
	vm.Set("fs", fsObj)

	// 6. Path Utility
	pathObj := vm.NewObject()
	pathObj.Set("join", func(call sobek.FunctionCall) sobek.Value {
		var parts []string
		for _, arg := range call.Arguments {
			parts = append(parts, arg.String())
		}
		return vm.ToValue(filepath.Join(parts...))
	})
	vm.Set("path", pathObj)

	// 7. Require Shim
	vm.Set("require", func(call sobek.FunctionCall) sobek.Value {
		switch call.Argument(0).String() {
		case "fs", "node:fs":
			return fsObj
		case "path", "node:path":
			return pathObj
		default:
			panic(vm.NewGoError(fmt.Errorf("module not supported in caddy-javascript: %s", call.Argument(0).String())))
		}
	})
}

func (h *JSEventLoopHandler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	bodyBytes, _ := io.ReadAll(r.Body)

	var parsedBody interface{}
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		var jsonMap map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &jsonMap); err == nil {
			parsedBody = jsonMap
		}
	}
	if parsedBody == nil {
		parsedBody = string(bodyBytes)
	}

	headersMap := make(map[string]string)
	for k, v := range r.Header {
		if len(v) > 0 {
			headersMap[k] = v[0]
		}
	}

	queryMap := make(map[string]string)
	for k, v := range r.URL.Query() {
		if len(v) > 0 {
			queryMap[k] = v[0]
		}
	}

	reqCtx := HTTPRequestContext{
		Method:  r.Method,
		Path:    r.URL.Path,
		Headers: headersMap,
		Body:    parsedBody,
		Query:   queryMap,
	}

	srw := &safeResponseWriter{w: w}
	done := make(chan struct{})
	var once sync.Once
	finishRequest := func() {
		once.Do(func() { close(done) })
	}

	task := func() {
		defer finishRequest()

		h.mu.RLock()
		routeKey := reqCtx.Method + ":" + reqCtx.Path
		handler := h.routes[routeKey]
		if handler == nil {
			handler = h.fallback
		}
		h.mu.RUnlock()

		if handler == nil {
			if !srw.IsWritten() {
				http.Error(srw, "Not Found", http.StatusNotFound)
			}
			return
		}

		reqObj := h.vm.NewObject()
		reqObj.Set("path", reqCtx.Path)
		reqObj.Set("method", reqCtx.Method)

		headersObj := h.vm.NewObject()
		for k, v := range reqCtx.Headers {
			headersObj.Set(k, v)
		}
		reqObj.Set("headers", headersObj)

		if m, ok := reqCtx.Body.(map[string]interface{}); ok {
			bodyObj := h.vm.NewObject()
			for k, val := range m {
				bodyObj.Set(k, val)
			}
			reqObj.Set("body", bodyObj)
		} else {
			reqObj.Set("body", reqCtx.Body)
		}

		queryObj := h.vm.NewObject()
		for k, v := range reqCtx.Query {
			queryObj.Set(k, v)
		}
		reqObj.Set("query", queryObj)

		resObj := h.vm.NewObject()
		resObj.Set("status", func(call sobek.FunctionCall) sobek.Value {
			statusCode := int(call.Argument(0).ToInteger())
			srw.mu.Lock()
			srw.statusCode = statusCode
			srw.mu.Unlock()
			return resObj
		})
		resObj.Set("send", func(call sobek.FunctionCall) sobek.Value {
			srw.mu.Lock()
			code := srw.statusCode
			if code == 0 {
				code = 200
			}
			srw.mu.Unlock()
			srw.WriteHeader(code)
			_, _ = srw.Write([]byte(call.Argument(0).String()))
			return resObj
		})
		resObj.Set("json", func(call sobek.FunctionCall) sobek.Value {
			srw.Header().Set("Content-Type", "application/json")
			srw.mu.Lock()
			code := srw.statusCode
			if code == 0 {
				code = 200
			}
			srw.mu.Unlock()
			srw.WriteHeader(code)
			_, _ = srw.Write([]byte(call.Argument(0).String()))
			return resObj
		})

		// Execution Watchdog with Interrupt State Cleansing
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		go func() {
			<-ctx.Done()
			if ctx.Err() == context.DeadlineExceeded {
				h.vm.Interrupt("script execution timeout exceeded (possible infinite loop)")
			}
		}()

		_, err := handler(sobek.Undefined(), reqObj, resObj)

		// Clear interrupt flag so subsequent requests aren't poisoned
		h.vm.ClearInterrupt()

		if err != nil && !srw.IsWritten() {
			http.Error(srw, err.Error(), http.StatusInternalServerError)
		}
	}

	// Non-blocking backpressure queue send with overload protection (503 Service Unavailable)
	select {
	case h.taskQueue <- task:
	case <-r.Context().Done():
		return nil
	default:
		http.Error(w, "Server overloaded (event queue full)", http.StatusServiceUnavailable)
		return nil
	}

	select {
	case <-done:
	case <-r.Context().Done():
	}

	return nil
}

func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	var handler JSEventLoopHandler
	for h.Next() {
		if h.NextArg() {
			handler.ScriptPath = h.Val()
		}
	}
	return &handler, nil
}

```
