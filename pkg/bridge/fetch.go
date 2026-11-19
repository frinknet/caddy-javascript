package bridge

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/grafana/sobek"
)

const (
	fetchTimeout   = 10 * time.Second
	maxFetchBytes  = 1 << 20
	maxRedirects   = 5
	fetchChunkSize = 32 * 1024
)

var (
	fetchAbortSymbol  = sobek.NewSymbol("caddy.fetchAbort")
	fetchBlobSymbol   = sobek.NewSymbol("caddy.fetchBlob")
	fetchFormSymbol   = sobek.NewSymbol("caddy.fetchForm")
	fetchParamsSymbol = sobek.NewSymbol("caddy.fetchParams")
)

type fetchRedirectKey struct{}

func (p *Program) registerFetch() {
	client := &http.Client{
		Timeout: fetchTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			mode, _ := req.Context().Value(fetchRedirectKey{}).(string)
			switch mode {
			case "error":
				return fmt.Errorf("redirect blocked")
			case "manual":
				return http.ErrUseLastResponse
			}
			return p.checkFetchTarget(req.URL.Hostname())
		},
	}

	p.rt.Set("fetch", func(call sobek.FunctionCall) sobek.Value {
		return p.startFetch(client, call)
	})
	p.rt.Set("AbortController", func(call sobek.ConstructorCall) *sobek.Object {
		return p.newAbortController()
	})
	p.rt.Set("AbortSignal", func(call sobek.ConstructorCall) *sobek.Object {
		panic(p.rt.NewTypeError("Illegal constructor"))
	})
	p.rt.Set("Blob", func(call sobek.ConstructorCall) *sobek.Object {
		return p.newBlob(call)
	})
	p.rt.Set("FormData", func(call sobek.ConstructorCall) *sobek.Object {
		return p.newFormData(call)
	})
	p.rt.Set("URLSearchParams", func(call sobek.ConstructorCall) *sobek.Object {
		return p.newURLSearchParams(call)
	})
}

type fetchRequest struct {
	method      string
	headers     http.Header
	host        string
	body        []byte
	hasBody     bool
	redirect    string
	cache       string
	credentials string
	signal      *fetchAbort
}

type fetchAbort struct {
	p      *Program
	signal *sobek.Object

	mu      sync.Mutex
	hooks   map[int]func()
	nextID  int
	reason  sobek.Value
	aborted bool
}

func newFetchAbort(p *Program) *fetchAbort {
	return &fetchAbort{p: p, hooks: make(map[int]func())}
}

func (a *fetchAbort) isAborted() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.aborted
}

func (a *fetchAbort) getReason() sobek.Value {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reason
}

func (a *fetchAbort) addHook(fn func()) int {
	a.mu.Lock()
	if a.aborted {
		a.mu.Unlock()
		fn()
		return 0
	}
	a.nextID++
	id := a.nextID
	a.hooks[id] = fn
	a.mu.Unlock()
	return id
}

func (a *fetchAbort) removeHook(id int) {
	if id == 0 {
		return
	}
	a.mu.Lock()
	delete(a.hooks, id)
	a.mu.Unlock()
}

func (a *fetchAbort) abort(reason sobek.Value) {
	a.mu.Lock()
	if a.aborted {
		a.mu.Unlock()
		return
	}
	a.aborted = true
	a.reason = reason
	hooks := a.hooks
	a.hooks = make(map[int]func())
	a.mu.Unlock()

	for _, fn := range hooks {
		fn()
	}
	if a.signal != nil {
		if fn, ok := sobek.AssertFunction(a.signal.Get("onabort")); ok {
			_, _ = fn(a.signal)
		}
	}
}

func (p *Program) newAbortController() *sobek.Object {
	signal := p.rt.NewObject()
	state := newFetchAbort(p)
	state.signal = signal
	_ = signal.SetSymbol(fetchAbortSymbol, state)
	_ = signal.Set("aborted", false)
	_ = signal.Set("reason", sobek.Undefined())
	_ = signal.Set("onabort", sobek.Null())
	_ = signal.Set("addEventListener", func(call sobek.FunctionCall) sobek.Value {
		if call.Argument(0).String() != "abort" {
			return sobek.Undefined()
		}
		fn, ok := sobek.AssertFunction(call.Argument(1))
		if !ok {
			return sobek.Undefined()
		}
		_ = signal.Set("onabort", fn)
		return sobek.Undefined()
	})
	_ = signal.Set("removeEventListener", func(call sobek.FunctionCall) sobek.Value {
		if call.Argument(0).String() != "abort" {
			return sobek.Undefined()
		}
		_ = signal.Set("onabort", sobek.Null())
		return sobek.Undefined()
	})
	_ = signal.Set("throwIfAborted", func(call sobek.FunctionCall) sobek.Value {
		if state.isAborted() {
			panic(state.getReason())
		}
		return sobek.Undefined()
	})

	controller := p.rt.NewObject()
	_ = controller.Set("signal", signal)
	_ = controller.Set("abort", func(call sobek.FunctionCall) sobek.Value {
		reason := call.Argument(0)
		if isNilValue(reason) {
			reason = p.rt.NewGoError(fmt.Errorf("The operation was aborted"))
			_ = reason.ToObject(p.rt).Set("name", "AbortError")
		}
		state.abort(reason)
		_ = signal.Set("aborted", true)
		_ = signal.Set("reason", reason)
		return sobek.Undefined()
	})
	return controller
}

func (p *Program) newAbortError() sobek.Value {
	err := p.rt.NewGoError(fmt.Errorf("The operation was aborted"))
	_ = err.ToObject(p.rt).Set("name", "AbortError")
	return err
}

func (p *Program) addAbortHook(state *fetchAbort, ctx context.Context, fn func()) int {
	if state == nil {
		return 0
	}
	id := state.addHook(fn)
	if id != 0 && ctx != nil {
		context.AfterFunc(ctx, func() {
			state.removeHook(id)
		})
	}
	return id
}

func (p *Program) startFetch(client *http.Client, call sobek.FunctionCall) sobek.Value {
	rawURL, opts, cb, hasCallback := p.fetchArgs(call)

	var (
		promise *sobek.Promise
		resolve func(interface{}) error
		reject  func(interface{}) error
	)
	if !hasCallback {
		promise, resolve, reject = p.rt.NewPromise()
	}

	fail := func(err error) sobek.Value {
		if hasCallback {
			ex := p.trackAsync()
			p.submitAsync(ex, func() {
				_, _ = cb(sobek.Undefined(), p.rt.ToValue(err.Error()), sobek.Null())
			})
			return sobek.Undefined()
		}
		_ = reject(p.rt.NewGoError(err))
		return p.rt.ToValue(promise)
	}

	options, err := p.fetchOptions(opts)
	if err != nil {
		return fail(err)
	}

	if options.signal != nil && options.signal.isAborted() {
		reason := options.signal.getReason()
		if hasCallback {
			ex := p.trackAsync()
			p.submitAsync(ex, func() {
				_, _ = cb(sobek.Undefined(), reason, sobek.Null())
			})
			return sobek.Undefined()
		}
		_ = reject(reason)
		return p.rt.ToValue(promise)
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fail(fmt.Errorf("invalid url: %w", err))
	}
	if err := p.checkFetchTarget(parsed.Hostname()); err != nil {
		return fail(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	ctx = context.WithValue(ctx, fetchRedirectKey{}, options.redirect)
	req, err := http.NewRequestWithContext(ctx, options.method, rawURL, bytes.NewReader(options.body))
	if err != nil {
		cancel()
		return fail(err)
	}
	if options.host != "" {
		req.Host = options.host
	}
	if len(options.headers) > 0 {
		req.Header = options.headers
	}
	p.addAbortHook(options.signal, ctx, cancel)

	ex := p.trackAsync()
	go func() {
		resp, err := client.Do(req)
		if err != nil {
			cancel()
			p.submitAsync(ex, func() {
				if options.signal != nil && options.signal.isAborted() {
					reason := options.signal.getReason()
					if hasCallback {
						_, _ = cb(sobek.Undefined(), reason, sobek.Null())
						return
					}
					_ = reject(reason)
					return
				}
				if hasCallback {
					_, _ = cb(sobek.Undefined(), p.rt.ToValue(err.Error()), sobek.Null())
					return
				}
				_ = reject(p.rt.NewGoError(err))
			})
			return
		}

		if hasCallback {
			data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxFetchBytes+1))
			_ = resp.Body.Close()
			cancel()
			if readErr == nil && int64(len(data)) > maxFetchBytes {
				readErr = fmt.Errorf("response exceeds %d bytes", maxFetchBytes)
			}
			p.submitAsync(ex, func() {
				if options.signal != nil && options.signal.isAborted() {
					_, _ = cb(sobek.Undefined(), options.signal.getReason(), sobek.Null())
					return
				}
				if readErr != nil {
					_, _ = cb(sobek.Undefined(), p.rt.ToValue(readErr.Error()), sobek.Null())
					return
				}
				_, _ = cb(sobek.Undefined(), sobek.Null(), p.buildLegacyResponse(resp, rawURL, string(data)))
			})
			return
		}

		body := newFetchBody(p, resp.Body, cancel, rawURL, options.signal, ctx)
		p.submitAsync(ex, func() {
			if options.signal != nil && options.signal.isAborted() {
				_ = reject(options.signal.getReason())
				return
			}
			_ = resolve(p.buildFetchResponse(resp, body))
		})
	}()

	if hasCallback {
		return sobek.Undefined()
	}
	return p.rt.ToValue(promise)
}

func (p *Program) fetchArgs(call sobek.FunctionCall) (string, *sobek.Object, sobek.Callable, bool) {
	rawURL := call.Argument(0).String()
	if len(call.Arguments) < 2 {
		return rawURL, nil, nil, false
	}
	if fn, ok := sobek.AssertFunction(call.Argument(1)); ok {
		return rawURL, nil, fn, true
	}
	var opts *sobek.Object
	if v := call.Argument(1); !isNilValue(v) {
		opts = v.ToObject(p.rt)
	}
	if len(call.Arguments) >= 3 {
		if fn, ok := sobek.AssertFunction(call.Argument(2)); ok {
			return rawURL, opts, fn, true
		}
	}
	return rawURL, opts, nil, false
}

func (p *Program) fetchOptions(opts *sobek.Object) (fetchRequest, error) {
	out := fetchRequest{
		method:      http.MethodGet,
		headers:     make(http.Header),
		redirect:    "follow",
		cache:       "default",
		credentials: "same-origin",
	}
	if opts == nil {
		return out, nil
	}

	if v := opts.Get("method"); !isNilValue(v) {
		if method := strings.TrimSpace(v.String()); method != "" {
			out.method = strings.ToUpper(method)
		}
	}

	if v := opts.Get("headers"); !isNilValue(v) {
		if headers := v.ToObject(p.rt); headers != nil {
			for _, name := range headers.Keys() {
				value := headers.Get(name)
				if isNilValue(value) {
					continue
				}
				if items, ok := value.Export().([]interface{}); ok {
					for _, item := range items {
						out.headers.Add(name, fmt.Sprint(item))
					}
					continue
				}
				if strings.EqualFold(name, "Host") {
					out.host = value.String()
					continue
				}
				out.headers.Add(name, value.String())
			}
		}
	}

	if v := opts.Get("redirect"); !isNilValue(v) {
		switch mode := strings.ToLower(v.String()); mode {
		case "follow", "error", "manual":
			out.redirect = mode
		default:
			return out, fmt.Errorf("fetch: invalid redirect mode %q", mode)
		}
	}

	if v := opts.Get("cache"); !isNilValue(v) {
		switch mode := strings.ToLower(v.String()); mode {
		case "default", "no-store", "reload", "no-cache", "force-cache", "only-if-cached":
			out.cache = mode
		default:
			return out, fmt.Errorf("fetch: invalid cache mode %q", mode)
		}
	}

	if v := opts.Get("credentials"); !isNilValue(v) {
		switch mode := strings.ToLower(v.String()); mode {
		case "omit", "same-origin", "include":
			out.credentials = mode
		default:
			return out, fmt.Errorf("fetch: invalid credentials mode %q", mode)
		}
	}

	if v := opts.Get("signal"); !isNilValue(v) {
		if obj := v.ToObject(p.rt); obj != nil {
			if state, ok := obj.GetSymbol(fetchAbortSymbol).Export().(*fetchAbort); ok {
				out.signal = state
			}
		}
	}

	if v := opts.Get("body"); !isNilValue(v) {
		out.hasBody = true
		body, contentType, err := p.readFetchBody(v)
		if err != nil {
			return out, err
		}
		out.body = body
		if contentType != "" {
			out.headers.Set("Content-Type", contentType)
		}
	}

	switch out.cache {
	case "no-store":
		out.headers.Set("Cache-Control", "no-store")
	case "reload":
		out.headers.Set("Cache-Control", "no-cache")
		out.headers.Set("Pragma", "no-cache")
	case "no-cache":
		out.headers.Set("Cache-Control", "no-cache")
	case "only-if-cached":
		out.headers.Set("Cache-Control", "only-if-cached")
	}

	if out.credentials == "omit" {
		out.headers.Del("Cookie")
	}

	if out.hasBody && (out.method == http.MethodGet || out.method == http.MethodHead) {
		return out, fmt.Errorf("fetch: %s request cannot have a body", out.method)
	}
	return out, nil
}

func (p *Program) readFetchBody(v sobek.Value) ([]byte, string, error) {
	if obj := v.ToObject(p.rt); obj != nil {
		if tag := obj.GetSymbol(fetchBlobSymbol); !isNilValue(tag) {
			if blob, ok := tag.Export().(*fetchBlob); ok {
				return blob.data, blob.contentType, nil
			}
		}
		if tag := obj.GetSymbol(fetchFormSymbol); !isNilValue(tag) {
			if form, ok := tag.Export().(*fetchForm); ok {
				return form.encode()
			}
		}
		if tag := obj.GetSymbol(fetchParamsSymbol); !isNilValue(tag) {
			if params, ok := tag.Export().(*fetchParams); ok {
				return []byte(params.toString()), "application/x-www-form-urlencoded;charset=UTF-8", nil
			}
		}
	}
	if ab, ok := v.Export().(sobek.ArrayBuffer); ok {
		return ab.Bytes(), "", nil
	}
	if b, ok := v.Export().([]byte); ok {
		return b, "", nil
	}
	return []byte(v.String()), "", nil
}

func (p *Program) buildFetchResponse(resp *http.Response, body *fetchBody) *sobek.Object {
	res := p.rt.NewObject()
	p.setFetchResponseFields(res, resp, body.originalURL)
	_ = res.Set("body", body.object())
	_ = res.Set("text", func(call sobek.FunctionCall) sobek.Value {
		return body.text()
	})
	_ = res.Set("json", func(call sobek.FunctionCall) sobek.Value {
		return body.json()
	})
	_ = res.Set("arrayBuffer", func(call sobek.FunctionCall) sobek.Value {
		return body.arrayBuffer()
	})
	_ = res.Set("clone", func(call sobek.FunctionCall) sobek.Value {
		clone, err := body.clone()
		if err != nil {
			panic(p.rt.NewTypeError(err.Error()))
		}
		return p.buildFetchResponse(resp, clone)
	})
	return res
}

func (p *Program) buildLegacyResponse(resp *http.Response, originalURL, text string) *sobek.Object {
	res := p.rt.NewObject()
	p.setFetchResponseFields(res, resp, originalURL)
	_ = res.Set("text", text)
	return res
}

func (p *Program) setFetchResponseFields(res *sobek.Object, resp *http.Response, originalURL string) {
	status := resp.StatusCode
	_ = res.Set("status", status)
	_ = res.Set("statusText", responseStatusText(resp))
	_ = res.Set("ok", status >= 200 && status < 300)
	_ = res.Set("url", resp.Request.URL.String())
	_ = res.Set("redirected", resp.Request.URL.String() != originalURL)
	_ = res.Set("headers", p.fetchHeaders(resp.Header))
}

func responseStatusText(resp *http.Response) string {
	if resp.Status == "" {
		return ""
	}
	if i := strings.IndexByte(resp.Status, ' '); i >= 0 {
		return resp.Status[i+1:]
	}
	return resp.Status
}

func (p *Program) fetchHeaders(headers http.Header) *sobek.Object {
	obj := p.rt.NewObject()
	for name, values := range headers {
		_ = obj.Set(strings.ToLower(name), strings.Join(values, ", "))
	}
	_ = obj.Set("get", func(call sobek.FunctionCall) sobek.Value {
		values := headers.Values(call.Argument(0).String())
		if len(values) == 0 {
			return sobek.Null()
		}
		return p.rt.ToValue(strings.Join(values, ", "))
	})
	_ = obj.Set("has", func(call sobek.FunctionCall) sobek.Value {
		return p.rt.ToValue(len(headers.Values(call.Argument(0).String())) > 0)
	})
	_ = obj.Set("forEach", func(call sobek.FunctionCall) sobek.Value {
		fn, ok := sobek.AssertFunction(call.Argument(0))
		if !ok {
			return sobek.Undefined()
		}
		for name, values := range headers {
			value := p.rt.ToValue(strings.Join(values, ", "))
			if _, err := fn(sobek.Undefined(), value, p.rt.ToValue(strings.ToLower(name))); err != nil {
				panic(err)
			}
		}
		return sobek.Undefined()
	})
	return obj
}

type fetchRead struct {
	resolve func(interface{}) error
	reject  func(interface{}) error
}

// fetchBody owns an HTTP response body. Reader and text/json consumption are
// mutually exclusive, and only one read is outstanding at a time so a slow
// script applies backpressure to the network.
type fetchBody struct {
	p           *Program
	src         io.ReadCloser
	cancelCtx   context.CancelFunc
	originalURL string
	signal      *fetchAbort
	ctx         context.Context
	hookID      int

	sourceOnce sync.Once
	closeOnce  sync.Once

	used        bool
	done        bool
	err         error
	abortReason sobek.Value
	reading     bool
	total       int
	queue       []fetchRead
}

func newFetchBody(p *Program, src io.ReadCloser, cancel context.CancelFunc, originalURL string, signal *fetchAbort, ctx context.Context) *fetchBody {
	b := &fetchBody{
		p:           p,
		src:         src,
		cancelCtx:   cancel,
		originalURL: originalURL,
		signal:      signal,
		ctx:         ctx,
	}
	if signal != nil {
		b.hookID = p.addAbortHook(signal, ctx, func() {
			b.abort(signal.getReason())
		})
	}
	return b
}

func (b *fetchBody) object() *sobek.Object {
	obj := b.p.rt.NewObject()
	_ = obj.Set("getReader", func(call sobek.FunctionCall) sobek.Value {
		return b.reader()
	})
	return obj
}

func (b *fetchBody) reader() sobek.Value {
	if b.used {
		panic(b.p.rt.NewTypeError("body stream is locked"))
	}
	b.used = true
	reader := b.p.rt.NewObject()
	_ = reader.Set("read", func(call sobek.FunctionCall) sobek.Value {
		return b.read()
	})
	_ = reader.Set("cancel", func(call sobek.FunctionCall) sobek.Value {
		return b.cancelPromise()
	})
	_ = reader.Set("releaseLock", func(call sobek.FunctionCall) sobek.Value {
		return sobek.Undefined()
	})
	return reader
}

func (b *fetchBody) read() sobek.Value {
	promise, resolve, reject := b.p.rt.NewPromise()
	b.enqueue(resolve, reject)
	return b.p.rt.ToValue(promise)
}

func (b *fetchBody) enqueue(resolve, reject func(interface{}) error) {
	if b.done {
		b.settle(fetchRead{resolve: resolve, reject: reject})
		return
	}
	b.queue = append(b.queue, fetchRead{resolve: resolve, reject: reject})
	b.pump()
}

func (b *fetchBody) settle(req fetchRead) {
	if b.abortReason != nil {
		_ = req.reject(b.abortReason)
		return
	}
	if b.err != nil {
		_ = req.reject(b.p.rt.NewGoError(b.err))
		return
	}
	_ = req.resolve(b.result(nil, true))
}

func (b *fetchBody) pump() {
	for !b.reading && len(b.queue) > 0 {
		req := b.queue[0]
		b.queue = b.queue[1:]
		if b.done {
			b.settle(req)
			continue
		}
		b.startRead(req)
		return
	}
}

func (b *fetchBody) startRead(req fetchRead) {
	b.reading = true
	ex := b.p.trackAsync()
	go func() {
		buf := make([]byte, fetchChunkSize)
		n, err := b.src.Read(buf)
		b.p.submitAsync(ex, func() {
			b.reading = false
			if b.done {
				b.settle(req)
				b.pump()
				return
			}
			switch {
			case n > 0:
				b.total += n
				if b.total > maxFetchBytes {
					b.fail(fmt.Errorf("response exceeds %d bytes", maxFetchBytes))
					b.settle(req)
					return
				}
				_ = req.resolve(b.result(buf[:n], false))
				b.pump()
			case err == io.EOF:
				b.done = true
				b.close()
				_ = req.resolve(b.result(nil, true))
				b.pump()
			case err != nil:
				b.fail(err)
				b.settle(req)
			default:
				b.startRead(req)
			}
		})
	}()
}

func (b *fetchBody) result(data []byte, done bool) sobek.Value {
	obj := b.p.rt.NewObject()
	_ = obj.Set("done", done)
	if done {
		_ = obj.Set("value", sobek.Undefined())
	} else {
		_ = obj.Set("value", b.p.rt.ToValue(string(data)))
	}
	return obj
}

func (b *fetchBody) text() sobek.Value {
	promise, resolve, reject := b.p.rt.NewPromise()
	if b.used {
		_ = reject(b.p.rt.NewTypeError("body already used"))
		return b.p.rt.ToValue(promise)
	}
	b.used = true
	b.done = true

	ex := b.p.trackAsync()
	go func() {
		data, err := io.ReadAll(io.LimitReader(b.src, maxFetchBytes+1))
		b.close()
		b.p.submitAsync(ex, func() {
			if b.abortReason != nil {
				_ = reject(b.abortReason)
				return
			}
			if err != nil {
				_ = reject(b.p.rt.NewGoError(err))
				return
			}
			if int64(len(data)) > maxFetchBytes {
				_ = reject(b.p.rt.NewGoError(fmt.Errorf("response exceeds %d bytes", maxFetchBytes)))
				return
			}
			_ = resolve(b.p.rt.ToValue(string(data)))
		})
	}()
	return b.p.rt.ToValue(promise)
}

func (b *fetchBody) arrayBuffer() sobek.Value {
	promise, resolve, reject := b.p.rt.NewPromise()
	if b.used {
		_ = reject(b.p.rt.NewTypeError("body already used"))
		return b.p.rt.ToValue(promise)
	}
	b.used = true
	b.done = true

	ex := b.p.trackAsync()
	go func() {
		data, err := io.ReadAll(io.LimitReader(b.src, maxFetchBytes+1))
		b.close()
		b.p.submitAsync(ex, func() {
			if b.abortReason != nil {
				_ = reject(b.abortReason)
				return
			}
			if err != nil {
				_ = reject(b.p.rt.NewGoError(err))
				return
			}
			if int64(len(data)) > maxFetchBytes {
				_ = reject(b.p.rt.NewGoError(fmt.Errorf("response exceeds %d bytes", maxFetchBytes)))
				return
			}
			_ = resolve(b.p.rt.ToValue(b.p.rt.NewArrayBuffer(data)))
		})
	}()
	return b.p.rt.ToValue(promise)
}

func (b *fetchBody) json() sobek.Value {
	text := b.text()
	obj := text.ToObject(b.p.rt)
	if obj == nil {
		return text
	}
	then, ok := sobek.AssertFunction(obj.Get("then"))
	if !ok {
		return text
	}
	parse := b.p.rt.Get("JSON").ToObject(b.p.rt).Get("parse")
	result, err := then(obj, parse)
	if err != nil {
		panic(err)
	}
	return result
}

func (b *fetchBody) cancel() {
	if b.done {
		return
	}
	b.done = true
	b.close()
	b.pump()
}

func (b *fetchBody) cancelPromise() sobek.Value {
	promise, resolve, _ := b.p.rt.NewPromise()
	b.cancel()
	_ = resolve(sobek.Undefined())
	return b.p.rt.ToValue(promise)
}

func (b *fetchBody) closeSource() {
	b.sourceOnce.Do(func() {
		_ = b.src.Close()
	})
}

func (b *fetchBody) close() {
	b.closeOnce.Do(func() {
		b.closeSource()
		if b.cancelCtx != nil {
			b.cancelCtx()
			b.cancelCtx = nil
		}
		if b.signal != nil && b.hookID != 0 {
			b.signal.removeHook(b.hookID)
			b.hookID = 0
		}
	})
}

func (b *fetchBody) fail(err error) {
	if b.done {
		return
	}
	b.done = true
	b.err = err
	b.close()
	b.pump()
}

func (b *fetchBody) abort(reason sobek.Value) {
	if b.abortReason != nil {
		return
	}
	b.abortReason = reason
	b.done = true
	b.close()
	b.pump()
}

func (b *fetchBody) buffer() ([]byte, error) {
	if b.used || b.done || b.reading {
		return nil, fmt.Errorf("body already used")
	}
	data, err := io.ReadAll(io.LimitReader(b.src, maxFetchBytes+1))
	if err != nil {
		b.closeSource()
		return nil, err
	}
	if int64(len(data)) > maxFetchBytes {
		b.closeSource()
		return nil, fmt.Errorf("response exceeds %d bytes", maxFetchBytes)
	}
	b.closeSource()
	b.src = io.NopCloser(bytes.NewReader(data))
	return data, nil
}

func (b *fetchBody) clone() (*fetchBody, error) {
	data, err := b.buffer()
	if err != nil {
		return nil, err
	}
	clone := &fetchBody{
		p:           b.p,
		src:         io.NopCloser(bytes.NewReader(data)),
		originalURL: b.originalURL,
		signal:      b.signal,
		ctx:         b.ctx,
	}
	if b.signal != nil {
		clone.hookID = b.p.addAbortHook(b.signal, b.ctx, func() {
			clone.abort(b.signal.getReason())
		})
	}
	return clone, nil
}

func (p *Program) checkFetchTarget(host string) error {
	host = strings.TrimSuffix(host, ".")
	if host == "" {
		return fmt.Errorf("fetch blocked: missing host")
	}

	allowAll := false
	for _, allowed := range p.cfg.FetchAllow {
		if allowed == "*" {
			allowAll = true
			continue
		}
		if strings.EqualFold(allowed, host) {
			return nil
		}
	}

	if !allowAll {
		return fmt.Errorf("fetch blocked: %s is not in fetch_allow", host)
	}

	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("fetch blocked: cannot resolve %s: %w", host, err)
	}
	for _, ip := range ips {
		if !isPublicIP(ip) {
			return fmt.Errorf("fetch blocked: %s resolves to a private address", host)
		}
	}
	return nil
}

func isPublicIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}
	return true
}

type fetchBlob struct {
	data        []byte
	contentType string
}

func (p *Program) newBlob(call sobek.ConstructorCall) *sobek.Object {
	blob := &fetchBlob{}
	if parts := call.Argument(0); !isNilValue(parts) {
		if obj := parts.ToObject(p.rt); obj != nil {
			for _, key := range obj.Keys() {
				_ = key
			}
			length := int(obj.Get("length").ToInteger())
			for i := 0; i < length; i++ {
				part := obj.Get(fmt.Sprint(i))
				if err := p.appendBlobPart(blob, part); err != nil {
					panic(p.rt.NewTypeError(err.Error()))
				}
			}
		}
	}
	if opts := call.Argument(1); !isNilValue(opts) {
		if obj := opts.ToObject(p.rt); obj != nil {
			if v := obj.Get("type"); !isNilValue(v) {
				blob.contentType = v.String()
			}
		}
	}
	return p.buildBlob(blob)
}

func (p *Program) appendBlobPart(blob *fetchBlob, part sobek.Value) error {
	if isNilValue(part) {
		return nil
	}
	if obj := part.ToObject(p.rt); obj != nil {
		if tag := obj.GetSymbol(fetchBlobSymbol); !isNilValue(tag) {
			if other, ok := tag.Export().(*fetchBlob); ok {
				blob.data = append(blob.data, other.data...)
				return nil
			}
		}
	}
	if ab, ok := part.Export().(sobek.ArrayBuffer); ok {
		blob.data = append(blob.data, ab.Bytes()...)
		return nil
	}
	if b, ok := part.Export().([]byte); ok {
		blob.data = append(blob.data, b...)
		return nil
	}
	blob.data = append(blob.data, []byte(part.String())...)
	return nil
}

func (p *Program) buildBlob(blob *fetchBlob) *sobek.Object {
	obj := p.rt.NewObject()
	_ = obj.SetSymbol(fetchBlobSymbol, blob)
	_ = obj.Set("size", len(blob.data))
	_ = obj.Set("type", blob.contentType)
	_ = obj.Set("text", func(call sobek.FunctionCall) sobek.Value {
		promise, resolve, _ := p.rt.NewPromise()
		_ = resolve(p.rt.ToValue(string(blob.data)))
		return p.rt.ToValue(promise)
	})
	_ = obj.Set("arrayBuffer", func(call sobek.FunctionCall) sobek.Value {
		promise, resolve, _ := p.rt.NewPromise()
		_ = resolve(p.rt.ToValue(p.rt.NewArrayBuffer(blob.data)))
		return p.rt.ToValue(promise)
	})
	_ = obj.Set("slice", func(call sobek.FunctionCall) sobek.Value {
		start := int(call.Argument(0).ToInteger())
		end := len(blob.data)
		if v := call.Argument(1); !isNilValue(v) {
			end = int(v.ToInteger())
		}
		if start < 0 {
			start += len(blob.data)
		}
		if end < 0 {
			end += len(blob.data)
		}
		if start < 0 {
			start = 0
		}
		if end > len(blob.data) {
			end = len(blob.data)
		}
		if start > end {
			start = end
		}
		contentType := blob.contentType
		if v := call.Argument(2); !isNilValue(v) {
			contentType = v.String()
		}
		return p.buildBlob(&fetchBlob{data: blob.data[start:end], contentType: contentType})
	})
	return obj
}

type fetchForm struct {
	entries []fetchFormEntry
}

type fetchFormEntry struct {
	name     string
	value    string
	blob     *fetchBlob
	filename string
}

func (p *Program) newFormData(call sobek.ConstructorCall) *sobek.Object {
	return p.buildForm(&fetchForm{})
}

func (p *Program) buildForm(form *fetchForm) *sobek.Object {
	obj := p.rt.NewObject()
	_ = obj.SetSymbol(fetchFormSymbol, form)
	_ = obj.Set("append", func(call sobek.FunctionCall) sobek.Value {
		form.entries = append(form.entries, p.formEntry(call.Argument(0).String(), call.Argument(1), call.Argument(2)))
		return sobek.Undefined()
	})
	_ = obj.Set("set", func(call sobek.FunctionCall) sobek.Value {
		name := call.Argument(0).String()
		entry := p.formEntry(name, call.Argument(1), call.Argument(2))
		for i := range form.entries {
			if form.entries[i].name == name {
				form.entries[i] = entry
				return sobek.Undefined()
			}
		}
		form.entries = append(form.entries, entry)
		return sobek.Undefined()
	})
	_ = obj.Set("get", func(call sobek.FunctionCall) sobek.Value {
		name := call.Argument(0).String()
		for _, entry := range form.entries {
			if entry.name == name {
				if entry.blob != nil {
					return p.buildBlob(entry.blob)
				}
				return p.rt.ToValue(entry.value)
			}
		}
		return sobek.Null()
	})
	_ = obj.Set("getAll", func(call sobek.FunctionCall) sobek.Value {
		name := call.Argument(0).String()
		var out []interface{}
		for _, entry := range form.entries {
			if entry.name != name {
				continue
			}
			if entry.blob != nil {
				out = append(out, p.buildBlob(entry.blob))
			} else {
				out = append(out, entry.value)
			}
		}
		return p.rt.ToValue(out)
	})
	_ = obj.Set("delete", func(call sobek.FunctionCall) sobek.Value {
		name := call.Argument(0).String()
		filtered := form.entries[:0]
		for _, entry := range form.entries {
			if entry.name != name {
				filtered = append(filtered, entry)
			}
		}
		form.entries = filtered
		return sobek.Undefined()
	})
	_ = obj.Set("forEach", func(call sobek.FunctionCall) sobek.Value {
		fn, ok := sobek.AssertFunction(call.Argument(0))
		if !ok {
			return sobek.Undefined()
		}
		for _, entry := range form.entries {
			var value sobek.Value
			if entry.blob != nil {
				value = p.buildBlob(entry.blob)
			} else {
				value = p.rt.ToValue(entry.value)
			}
			if _, err := fn(sobek.Undefined(), value, p.rt.ToValue(entry.name)); err != nil {
				panic(err)
			}
		}
		return sobek.Undefined()
	})
	return obj
}

func (p *Program) formEntry(name string, value sobek.Value, filename sobek.Value) fetchFormEntry {
	entry := fetchFormEntry{name: name}
	if obj := value.ToObject(p.rt); obj != nil {
		if tag := obj.GetSymbol(fetchBlobSymbol); !isNilValue(tag) {
			if blob, ok := tag.Export().(*fetchBlob); ok {
				entry.blob = blob
			}
		}
	}
	if entry.blob == nil {
		entry.value = value.String()
	}
	if !isNilValue(filename) {
		entry.filename = filename.String()
	}
	return entry
}

func (f *fetchForm) encode() ([]byte, string, error) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	for _, entry := range f.entries {
		if entry.blob != nil {
			filename := entry.filename
			if filename == "" {
				filename = "blob"
			}
			part, err := writer.CreateFormFile(entry.name, filename)
			if err != nil {
				return nil, "", err
			}
			if _, err := part.Write(entry.blob.data); err != nil {
				return nil, "", err
			}
			continue
		}
		if err := writer.WriteField(entry.name, entry.value); err != nil {
			return nil, "", err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), writer.FormDataContentType(), nil
}

type fetchParams struct {
	pairs []fetchParam
}

type fetchParam struct {
	name  string
	value string
}

func (p *Program) newURLSearchParams(call sobek.ConstructorCall) *sobek.Object {
	params := &fetchParams{}
	if init := call.Argument(0); !isNilValue(init) {
		if s, ok := init.Export().(string); ok {
			values, err := url.ParseQuery(s)
			if err == nil {
				for name, list := range values {
					for _, value := range list {
						params.pairs = append(params.pairs, fetchParam{name: name, value: value})
					}
				}
			}
		} else if obj := init.ToObject(p.rt); obj != nil {
			if tag := obj.GetSymbol(fetchParamsSymbol); !isNilValue(tag) {
				if other, ok := tag.Export().(*fetchParams); ok {
					params.pairs = append(params.pairs, other.pairs...)
				}
			} else {
				for _, name := range obj.Keys() {
					value := obj.Get(name)
					if isNilValue(value) {
						continue
					}
					if items, ok := value.Export().([]interface{}); ok {
						for _, item := range items {
							params.pairs = append(params.pairs, fetchParam{name: name, value: fmt.Sprint(item)})
						}
						continue
					}
					params.pairs = append(params.pairs, fetchParam{name: name, value: value.String()})
				}
			}
		}
	}
	return p.buildParams(params)
}

func (p *Program) buildParams(params *fetchParams) *sobek.Object {
	obj := p.rt.NewObject()
	_ = obj.SetSymbol(fetchParamsSymbol, params)
	_ = obj.Set("append", func(call sobek.FunctionCall) sobek.Value {
		params.pairs = append(params.pairs, fetchParam{name: call.Argument(0).String(), value: call.Argument(1).String()})
		return sobek.Undefined()
	})
	_ = obj.Set("set", func(call sobek.FunctionCall) sobek.Value {
		name := call.Argument(0).String()
		value := call.Argument(1).String()
		for i := range params.pairs {
			if params.pairs[i].name == name {
				params.pairs[i].value = value
				return sobek.Undefined()
			}
		}
		params.pairs = append(params.pairs, fetchParam{name: name, value: value})
		return sobek.Undefined()
	})
	_ = obj.Set("get", func(call sobek.FunctionCall) sobek.Value {
		name := call.Argument(0).String()
		for _, pair := range params.pairs {
			if pair.name == name {
				return p.rt.ToValue(pair.value)
			}
		}
		return sobek.Null()
	})
	_ = obj.Set("getAll", func(call sobek.FunctionCall) sobek.Value {
		name := call.Argument(0).String()
		var out []interface{}
		for _, pair := range params.pairs {
			if pair.name == name {
				out = append(out, pair.value)
			}
		}
		return p.rt.ToValue(out)
	})
	_ = obj.Set("delete", func(call sobek.FunctionCall) sobek.Value {
		name := call.Argument(0).String()
		filtered := params.pairs[:0]
		for _, pair := range params.pairs {
			if pair.name != name {
				filtered = append(filtered, pair)
			}
		}
		params.pairs = filtered
		return sobek.Undefined()
	})
	_ = obj.Set("toString", func(call sobek.FunctionCall) sobek.Value {
		return p.rt.ToValue(params.toString())
	})
	return obj
}

func (u *fetchParams) toString() string {
	values := url.Values{}
	for _, pair := range u.pairs {
		values.Add(pair.name, pair.value)
	}
	return values.Encode()
}
