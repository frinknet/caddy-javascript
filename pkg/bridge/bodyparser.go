package bridge

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"

	"github.com/grafana/sobek"
)

// bodyKind is which of body-parser's middleware a call asked for. It selects
// the default content type and how the bytes turn into req.body.
type bodyKind int

const (
	bodyJSON bodyKind = iota
	bodyURLEncoded
	bodyText
	bodyRaw
)

// defaultBodyLimit matches body-parser's 100kb default.
const defaultBodyLimit = 100 * 1024

type bodyParserConfig struct {
	kind      bodyKind
	limit     int64
	typeOpt   sobek.Value
	strict    bool
	verify    sobek.Callable
	hasVerify bool
}

// bodyParserOptions reads the options object once, when the middleware is
// created, the way body-parser does.
func (p *Program) bodyParserOptions(kind bodyKind, opts sobek.Value) bodyParserConfig {
	cfg := bodyParserConfig{kind: kind, limit: defaultBodyLimit, strict: true}
	if isNilValue(opts) {
		return cfg
	}
	obj := opts.ToObject(p.rt)
	if obj == nil {
		panic(p.rt.NewTypeError("body parser options must be an object"))
	}
	if _, isFn := sobek.AssertFunction(opts); isFn {
		return cfg
	}
	if v := obj.Get("limit"); !isNilValue(v) {
		limit, ok := parseBodyLimit(v)
		if !ok {
			panic(p.rt.NewTypeError("body parser limit must be a byte count or a size string like '100kb'"))
		}
		cfg.limit = limit
	}
	if v := obj.Get("type"); !isNilValue(v) {
		cfg.typeOpt = v
	}
	if v := obj.Get("strict"); !isNilValue(v) {
		cfg.strict = v.ToBoolean()
	}
	if v := obj.Get("verify"); !isNilValue(v) {
		fn, ok := sobek.AssertFunction(v)
		if !ok {
			panic(p.rt.NewTypeError("body parser verify must be a function"))
		}
		cfg.verify = fn
		cfg.hasVerify = true
	}
	return cfg
}

func (p *Program) bodyParserMiddleware(kind bodyKind, opts sobek.Value) sobek.Value {
	cfg := p.bodyParserOptions(kind, opts)
	return p.rt.ToValue(func(call sobek.FunctionCall) sobek.Value {
		next, ok := sobek.AssertFunction(call.Argument(2))
		if !ok {
			panic(p.rt.NewTypeError("body parser middleware requires next"))
		}
		errVal := p.runBodyParser(call.Argument(0), call.Argument(1), cfg)
		_, _ = next(sobek.Undefined(), errVal)
		return sobek.Undefined()
	})
}

// requestContext walks the request object back to the Go context that carries
// the raw body. A request built without one is left untouched.
func (p *Program) requestContext(req *sobek.Object) *RequestContext {
	if req == nil || p.reqSymbol == nil {
		return nil
	}
	v := req.GetSymbol(p.reqSymbol)
	if isNilValue(v) {
		return nil
	}
	rc, _ := v.Export().(*RequestContext)
	return rc
}

// runBodyParser validates the body against the middleware options and replaces
// req.body when the content type matches. It returns the value to hand to
// next: undefined to continue, an Error with a status to fail the request.
func (p *Program) runBodyParser(reqVal, resVal sobek.Value, cfg bodyParserConfig) sobek.Value {
	if isNilValue(reqVal) {
		return sobek.Undefined()
	}
	req := reqVal.ToObject(p.rt)
	if req == nil {
		return sobek.Undefined()
	}
	rc := p.requestContext(req)
	if rc == nil {
		return sobek.Undefined()
	}
	mediaType := mediaTypeOf(rc.ContentType)
	if !p.bodyTypeMatches(cfg, req, mediaType) {
		return sobek.Undefined()
	}
	if int64(len(rc.RawBody)) > cfg.limit {
		return p.bodyParserError(413, "entity.too.large", "request entity too large")
	}
	if cfg.hasVerify {
		if _, err := cfg.verify(sobek.Undefined(), reqVal, resVal, p.newBuffer(rc.RawBody)); err != nil {
			return p.bodyParserThrown(err)
		}
	}
	if len(rc.RawBody) == 0 {
		switch cfg.kind {
		case bodyText, bodyRaw:
			_ = req.Set("body", "")
		default:
			_ = req.Set("body", p.rt.NewObject())
		}
		return sobek.Undefined()
	}
	switch cfg.kind {
	case bodyJSON:
		if cfg.strict {
			trimmed := bytes.TrimLeft(rc.RawBody, " \t\r\n")
			if len(trimmed) > 0 && trimmed[0] != '{' && trimmed[0] != '[' {
				return p.bodyParserError(400, "entity.parse.failed", "strict mode only accepts objects and arrays")
			}
		}
		var v interface{}
		if err := json.Unmarshal(rc.RawBody, &v); err != nil {
			return p.bodyParserError(400, "entity.parse.failed", err.Error())
		}
		_ = req.Set("body", p.toJSValue(v))
	case bodyURLEncoded:
		values, err := url.ParseQuery(string(rc.RawBody))
		if err != nil {
			return p.bodyParserError(400, "entity.parse.failed", err.Error())
		}
		_ = req.Set("body", p.toJSValue(urlValues(values)))
	default:
		_ = req.Set("body", string(rc.RawBody))
	}
	return sobek.Undefined()
}

func (p *Program) bodyParserError(status int, kind, message string) sobek.Value {
	err := p.rt.NewGoError(errors.New(message))
	_ = err.Set("status", status)
	_ = err.Set("statusCode", status)
	_ = err.Set("type", kind)
	_ = err.Set("expose", true)
	return err
}

// bodyParserThrown turns a throwing verify callback into the error handed to
// next. body-parser reports a rejected body as 403; an error that already
// carries a status keeps it.
func (p *Program) bodyParserThrown(err error) sobek.Value {
	var exc *sobek.Exception
	if errors.As(err, &exc) && exc.Value() != nil {
		v := exc.Value()
		if obj := v.ToObject(p.rt); obj != nil && obj.ClassName() == "Error" {
			if isNilValue(obj.Get("status")) && isNilValue(obj.Get("statusCode")) {
				_ = obj.Set("status", 403)
				_ = obj.Set("statusCode", 403)
			}
		}
		return v
	}
	return p.rt.ToValue(err.Error())
}

func (p *Program) bodyTypeMatches(cfg bodyParserConfig, req *sobek.Object, mediaType string) bool {
	if mediaType == "" {
		return false
	}
	if isNilValue(cfg.typeOpt) {
		return contentTypeMatches(defaultBodyType(cfg.kind), mediaType)
	}
	return p.typeOptionMatches(cfg.typeOpt, req, mediaType)
}

func (p *Program) typeOptionMatches(option sobek.Value, req *sobek.Object, mediaType string) bool {
	if isNilValue(option) {
		return false
	}
	if fn, ok := sobek.AssertFunction(option); ok {
		ret, err := fn(sobek.Undefined(), req)
		if err != nil {
			return false
		}
		return !isNilValue(ret) && ret.ToBoolean()
	}
	if obj := option.ToObject(p.rt); obj != nil && obj.ClassName() == "Array" {
		length := int(obj.Get("length").ToInteger())
		for i := 0; i < length; i++ {
			if p.typeOptionMatches(obj.Get(strconv.Itoa(i)), req, mediaType) {
				return true
			}
		}
		return false
	}
	return contentTypeMatches(option.String(), mediaType)
}

func defaultBodyType(kind bodyKind) string {
	switch kind {
	case bodyJSON:
		return "application/json"
	case bodyURLEncoded:
		return "application/x-www-form-urlencoded"
	case bodyText:
		return "text/plain"
	default:
		return "application/octet-stream"
	}
}

// mediaTypeOf strips parameters and lowercases a Content-Type value.
func mediaTypeOf(contentType string) string {
	mediaType, _, _ := strings.Cut(contentType, ";")
	return strings.ToLower(strings.TrimSpace(mediaType))
}

// contentTypeMatches applies type-is style matching: a wildcard matches any
// segment, a bare name matches the subtype, and the json subtype also matches
// structured suffixes such as application/vnd.api+json.
func contentTypeMatches(pattern, mediaType string) bool {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	mediaType = mediaTypeOf(mediaType)
	if pattern == "" || mediaType == "" {
		return false
	}
	if !strings.Contains(pattern, "/") {
		pattern = "*/" + pattern
	}
	patternType, patternSub, ok := strings.Cut(pattern, "/")
	if !ok {
		return false
	}
	actualType, actualSub, ok := strings.Cut(mediaType, "/")
	if !ok {
		return false
	}
	if patternType != "*" && patternType != actualType {
		return false
	}
	if patternSub == "*" || patternSub == actualSub {
		return true
	}
	if strings.HasPrefix(patternSub, "+") {
		return strings.HasSuffix(actualSub, patternSub)
	}
	return strings.HasSuffix(actualSub, "+"+patternSub)
}

// parseBodyLimit accepts a byte count or a size string such as '100kb'.
func parseBodyLimit(v sobek.Value) (int64, bool) {
	switch v.Export().(type) {
	case int64, float64, int:
		n := v.ToInteger()
		if n < 0 {
			return 0, false
		}
		return n, true
	}
	s := strings.ToLower(strings.TrimSpace(v.String()))
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.' || s[i] == ' ') {
		i++
	}
	numberText := strings.TrimSpace(s[:i])
	unit := strings.TrimSpace(s[i:])
	if numberText == "" {
		return 0, false
	}
	size, err := strconv.ParseFloat(numberText, 64)
	if err != nil || size < 0 {
		return 0, false
	}
	var scale int64
	switch unit {
	case "", "b", "byte", "bytes":
		scale = 1
	case "kb", "kib":
		scale = 1 << 10
	case "mb", "mib":
		scale = 1 << 20
	case "gb", "gib":
		scale = 1 << 30
	case "tb", "tib":
		scale = 1 << 40
	default:
		return 0, false
	}
	return int64(size * float64(scale)), true
}

// statusOf reads the HTTP status carried by a body-parser style error. A value
// without one, or one outside the client and server error range, returns 0.
func statusOf(v sobek.Value, rt *sobek.Runtime) int {
	if isNilValue(v) {
		return 0
	}
	obj := v.ToObject(rt)
	if obj == nil {
		return 0
	}
	for _, name := range []string{"status", "statusCode"} {
		code := obj.Get(name)
		if isNilValue(code) {
			continue
		}
		status := int(code.ToInteger())
		if status >= 400 && status <= 599 {
			return status
		}
	}
	return 0
}
