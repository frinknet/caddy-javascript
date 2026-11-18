package bridge

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/grafana/sobek"
)

var ErrBodyTooLarge = errors.New("request body too large")

type RequestContext struct {
	Method  string
	Path    string
	Host    string
	Headers map[string][]string
	Body    interface{}
	Query   map[string][]string
}

func NewRequestContext(r *http.Request, maxBody int64) (*RequestContext, error) {
	body, err := readBody(r, maxBody)
	if err != nil {
		return nil, err
	}
	return &RequestContext{
		Method:  r.Method,
		Path:    r.URL.Path,
		Host:    r.Host,
		Headers: r.Header,
		Body:    parseBody(body, r.Header.Get("Content-Type")),
		Query:   r.URL.Query(),
	}, nil
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
	if mt, _, err := mime.ParseMediaType(contentType); err == nil {
		mediaType = mt
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
	}
	return string(data)
}

func urlValues(values url.Values) map[string]interface{} {
	out := make(map[string]interface{}, len(values))
	for k, v := range values {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out
}

func (p *Program) BuildRequest(rc *RequestContext) *sobek.Object {
	req := p.rt.NewObject()
	_ = req.Set("path", rc.Path)
	_ = req.Set("method", rc.Method)
	_ = req.Set("host", rc.Host)
	_ = req.Set("headers", p.stringsObject(firstValues(rc.Headers)))
	_ = req.Set("query", p.stringsObject(firstValues(rc.Query)))
	_ = req.Set("queryAll", p.arraysObject(rc.Query))
	_ = req.Set("body", p.toJSValue(rc.Body))

	_ = req.Set("header", func(call sobek.FunctionCall) sobek.Value {
		values, ok := rc.Headers[http.CanonicalHeaderKey(call.Argument(0).String())]
		if !ok || len(values) == 0 {
			return sobek.Undefined()
		}
		return p.rt.ToValue(values[0])
	})

	return req
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
