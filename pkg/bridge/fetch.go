package bridge

import (
	"context"
	"fmt"
	"io"
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

func (p *Program) registerFetch() {
	client := &http.Client{
		Timeout: fetchTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			return p.checkFetchTarget(req.URL.Hostname())
		},
	}

	p.rt.Set("fetch", func(call sobek.FunctionCall) sobek.Value {
		return p.startFetch(client, call)
	})
}

type fetchRequest struct {
	method  string
	headers http.Header
	host    string
	body    string
	hasBody bool
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

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fail(fmt.Errorf("invalid url: %w", err))
	}
	if err := p.checkFetchTarget(parsed.Hostname()); err != nil {
		return fail(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	req, err := http.NewRequestWithContext(ctx, options.method, rawURL, strings.NewReader(options.body))
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

	ex := p.trackAsync()
	go func() {
		resp, err := client.Do(req)
		if err != nil {
			cancel()
			p.submitAsync(ex, func() {
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
				if readErr != nil {
					_, _ = cb(sobek.Undefined(), p.rt.ToValue(readErr.Error()), sobek.Null())
					return
				}
				_, _ = cb(sobek.Undefined(), sobek.Null(), p.buildLegacyResponse(resp, rawURL, string(data)))
			})
			return
		}

		body := newFetchBody(p, resp, cancel, rawURL)
		p.submitAsync(ex, func() {
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
	out := fetchRequest{method: http.MethodGet, headers: make(http.Header)}
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

	if v := opts.Get("body"); !isNilValue(v) {
		out.hasBody = true
		out.body = v.String()
	}
	if out.hasBody && (out.method == http.MethodGet || out.method == http.MethodHead) {
		return out, fmt.Errorf("fetch: %s request cannot have a body", out.method)
	}
	return out, nil
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
	resp        *http.Response
	cancelCtx   context.CancelFunc
	originalURL string
	closeOnce   sync.Once

	used    bool
	done    bool
	err     error
	reading bool
	total   int
	queue   []fetchRead
}

func newFetchBody(p *Program, resp *http.Response, cancel context.CancelFunc, originalURL string) *fetchBody {
	return &fetchBody{p: p, resp: resp, cancelCtx: cancel, originalURL: originalURL}
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
		if b.err != nil {
			_ = reject(b.p.rt.NewGoError(b.err))
			return
		}
		_ = resolve(b.result(nil, true))
		return
	}
	b.queue = append(b.queue, fetchRead{resolve: resolve, reject: reject})
	b.pump()
}

func (b *fetchBody) pump() {
	for !b.reading && len(b.queue) > 0 {
		req := b.queue[0]
		b.queue = b.queue[1:]
		if b.done {
			if b.err != nil {
				_ = req.reject(b.p.rt.NewGoError(b.err))
			} else {
				_ = req.resolve(b.result(nil, true))
			}
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
		n, err := b.resp.Body.Read(buf)
		b.p.submitAsync(ex, func() {
			b.reading = false
			if b.done {
				_ = req.resolve(b.result(nil, true))
				b.pump()
				return
			}
			switch {
			case n > 0:
				b.total += n
				if b.total > maxFetchBytes {
					b.fail(fmt.Errorf("response exceeds %d bytes", maxFetchBytes))
					_ = req.reject(b.p.rt.NewGoError(b.err))
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
				_ = req.reject(b.p.rt.NewGoError(err))
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
		data, err := io.ReadAll(io.LimitReader(b.resp.Body, maxFetchBytes+1))
		b.close()
		b.p.submitAsync(ex, func() {
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

func (b *fetchBody) close() {
	b.closeOnce.Do(func() {
		_ = b.resp.Body.Close()
		if b.cancelCtx != nil {
			b.cancelCtx()
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
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsMulticast() {
		return false
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}
	return true
}
