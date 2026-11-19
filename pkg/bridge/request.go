package bridge

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/grafana/sobek"
)

var ErrBodyTooLarge = errors.New("request body too large")

type RequestContext struct {
	Method      string
	Path        string
	Host        string
	URL         string
	OriginalURL string
	IP          string
	Protocol    string
	Secure      bool
	Headers     map[string][]string
	Body        interface{}
	Query       map[string][]string
}

func NewRequestContext(r *http.Request, maxBody int64) (*RequestContext, error) {
	body, err := readBody(r, maxBody)
	if err != nil {
		return nil, err
	}
	requestURL := r.URL.RequestURI()
	secure := r.TLS != nil
	protocol := "http"
	if secure {
		protocol = "https"
	}
	return &RequestContext{
		Method:      r.Method,
		Path:        r.URL.Path,
		Host:        r.Host,
		URL:         requestURL,
		OriginalURL: requestURL,
		IP:          remoteIP(r.RemoteAddr),
		Protocol:    protocol,
		Secure:      secure,
		Headers:     r.Header,
		Body:        parseBody(body, r.Header.Get("Content-Type")),
		Query:       r.URL.Query(),
	}, nil
}

// remoteIP strips the port from RemoteAddr. A value that is not host:port is
// returned unchanged, so a bare address still reaches the script.
func remoteIP(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

func readBody(r *http.Request, maxBody int64) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	limit := maxBody
	if limit <= 0 {
		limit = 1 << 20
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, ErrBodyTooLarge
	}
	return data, nil
}

func parseBody(data []byte, contentType string) interface{} {
	if len(data) == 0 {
		return nil
	}
	mediaType := contentType
	params := map[string]string{}
	if mt, p, err := mime.ParseMediaType(contentType); err == nil {
		mediaType = mt
		params = p
	}
	switch {
	case mediaType == "application/json" || strings.HasSuffix(mediaType, "+json"):
		var v interface{}
		if err := json.Unmarshal(data, &v); err == nil {
			return v
		}
	case mediaType == "application/x-www-form-urlencoded":
		if values, err := url.ParseQuery(string(data)); err == nil {
			return urlValues(values)
		}
	case mediaType == "multipart/form-data":
		if fields, ok := multipartValues(data, params["boundary"]); ok {
			return fields
		}
	}
	return string(data)
}

func multipartValues(data []byte, boundary string) (map[string]interface{}, bool) {
	if boundary == "" {
		return nil, false
	}
	out := make(map[string]interface{})
	reader := multipart.NewReader(bytes.NewReader(data), boundary)
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			return out, true
		}
		if err != nil {
			return nil, false
		}
		name := part.FormName()
		if name == "" || part.FileName() != "" {
			_ = part.Close()
			continue
		}
		value, err := io.ReadAll(part)
		_ = part.Close()
		if err != nil {
			return nil, false
		}
		if _, seen := out[name]; !seen {
			out[name] = string(value)
		}
	}
}

func urlValues(values url.Values) map[string]interface{} {
	out := make(map[string]interface{}, len(values))
	for k, v := range values {
		switch len(v) {
		case 0:
		case 1:
			out[k] = v[0]
		default:
			items := make([]interface{}, len(v))
			for i, item := range v {
				items[i] = item
			}
			out[k] = items
		}
	}
	return out
}

func (p *Program) BuildRequest(rc *RequestContext, params map[string]string) *sobek.Object {
	req := p.rt.NewObject()
	_ = req.Set("path", rc.Path)
	_ = req.Set("method", rc.Method)
	_ = req.Set("host", rc.Host)
	_ = req.Set("hostname", hostname(rc.Host))
	_ = req.Set("url", rc.URL)
	_ = req.Set("originalUrl", rc.OriginalURL)
	_ = req.Set("baseUrl", "")
	_ = req.Set("ip", rc.IP)
	_ = req.Set("protocol", rc.Protocol)
	_ = req.Set("secure", rc.Secure)
	_ = req.Set("xhr", strings.EqualFold(firstHeader(rc.Headers, "X-Requested-With"), "XMLHttpRequest"))
	_ = req.Set("app", p.appObj)
	_ = req.Set("params", p.stringsObject(params))
	_ = req.Set("headers", p.stringsObject(firstValues(rc.Headers)))
	_ = req.Set("query", p.stringsObject(firstValues(rc.Query)))
	_ = req.Set("queryAll", p.arraysObject(rc.Query))
	_ = req.Set("body", p.toJSValue(rc.Body))
	_ = req.Set("cookies", p.stringsObject(parseCookies(firstHeader(rc.Headers, "Cookie"))))

	header := func(call sobek.FunctionCall) sobek.Value {
		value, ok := headerValue(rc.Headers, call.Argument(0).String())
		if !ok {
			return sobek.Undefined()
		}
		return p.rt.ToValue(value)
	}
	_ = req.Set("header", header)
	_ = req.Set("get", header)
	_ = req.Set("param", func(call sobek.FunctionCall) sobek.Value {
		name := call.Argument(0).String()
		if value, ok := params[name]; ok {
			return p.rt.ToValue(value)
		}
		if body, ok := rc.Body.(map[string]interface{}); ok {
			if value, ok := body[name]; ok {
				return p.toJSValue(value)
			}
		}
		if values, ok := rc.Query[name]; ok && len(values) > 0 {
			return p.rt.ToValue(values[0])
		}
		return call.Argument(1)
	})
	_ = req.Set("accepts", func(call sobek.FunctionCall) sobek.Value {
		return p.accepts(rc, call.Arguments)
	})
	_ = req.Set("is", func(call sobek.FunctionCall) sobek.Value {
		return p.requestIs(rc, call.Arguments)
	})

	return req
}

func hostname(host string) string {
	if name, _, err := net.SplitHostPort(host); err == nil {
		return name
	}
	return strings.Trim(host, "[]")
}

func (p *Program) accepts(rc *RequestContext, args []sobek.Value) sobek.Value {
	wanted := flattenMediaTypes(args)
	if len(wanted) == 0 {
		return p.rt.ToValue(false)
	}
	accept, _ := headerValue(rc.Headers, "Accept")
	if accept == "" {
		return p.rt.ToValue(wanted[0])
	}
	for _, candidate := range wanted {
		for _, part := range strings.Split(accept, ",") {
			mediaType := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
			if matchesMediaType(mediaType, candidate) {
				return p.rt.ToValue(candidate)
			}
		}
	}
	return p.rt.ToValue(false)
}

func (p *Program) requestIs(rc *RequestContext, args []sobek.Value) sobek.Value {
	contentType, ok := headerValue(rc.Headers, "Content-Type")
	if !ok || contentType == "" {
		return p.rt.ToValue(false)
	}
	actual, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return p.rt.ToValue(false)
	}
	for _, candidate := range flattenMediaTypes(args) {
		if !strings.Contains(candidate, "/") {
			if mapped := mime.TypeByExtension("." + strings.TrimPrefix(candidate, ".")); mapped != "" {
				candidate = strings.SplitN(mapped, ";", 2)[0]
			}
		}
		if matchesMediaType(actual, candidate) {
			return p.rt.ToValue(candidate)
		}
	}
	return p.rt.ToValue(false)
}

func flattenMediaTypes(args []sobek.Value) []string {
	out := make([]string, 0, len(args))
	for _, arg := range args {
		if items, ok := arg.Export().([]interface{}); ok {
			for _, item := range items {
				out = append(out, fmt.Sprint(item))
			}
			continue
		}
		if !isNilValue(arg) {
			out = append(out, arg.String())
		}
	}
	return out
}

func matchesMediaType(actual, wanted string) bool {
	if wanted == "" || wanted == "*/*" || wanted == actual {
		return true
	}
	if strings.HasSuffix(wanted, "/*") {
		return strings.HasPrefix(actual, strings.TrimSuffix(wanted, "*"))
	}
	return false
}

func firstHeader(headers map[string][]string, name string) string {
	value, _ := headerValue(headers, name)
	return value
}

func parseCookies(header string) map[string]string {
	out := make(map[string]string)
	for _, part := range strings.Split(header, ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || name == "" {
			continue
		}
		if decoded, err := url.QueryUnescape(value); err == nil {
			value = decoded
		}
		out[name] = value
	}
	return out
}

// headerValue looks a header up case-insensitively, returning the first value.
func headerValue(headers map[string][]string, name string) (string, bool) {
	if values, ok := headers[http.CanonicalHeaderKey(name)]; ok {
		if len(values) == 0 {
			return "", false
		}
		return values[0], true
	}
	for key, values := range headers {
		if len(values) > 0 && strings.EqualFold(key, name) {
			return values[0], true
		}
	}
	return "", false
}

func firstValues(m map[string][]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out
}

func (p *Program) stringsObject(m map[string]string) *sobek.Object {
	obj := p.rt.NewObject()
	for k, v := range m {
		_ = obj.Set(k, v)
	}
	return obj
}

func (p *Program) arraysObject(m map[string][]string) *sobek.Object {
	obj := p.rt.NewObject()
	for k, v := range m {
		values := make([]interface{}, len(v))
		for i, item := range v {
			values[i] = item
		}
		_ = obj.Set(k, p.rt.NewArray(values...))
	}
	return obj
}

func (p *Program) toJSValue(v interface{}) sobek.Value {
	switch t := v.(type) {
	case nil:
		return sobek.Null()
	case map[string]interface{}:
		obj := p.rt.NewObject()
		for k, val := range t {
			_ = obj.Set(k, p.toJSValue(val))
		}
		return obj
	case []interface{}:
		vals := make([]interface{}, len(t))
		for i, val := range t {
			vals[i] = p.toJSValue(val)
		}
		return p.rt.NewArray(vals...)
	default:
		return p.rt.ToValue(v)
	}
}
