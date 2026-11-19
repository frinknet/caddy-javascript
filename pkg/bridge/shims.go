package bridge

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/grafana/sobek"
)

func (p *Program) registerShims() {
	p.registerBuffer()
	p.registerEvents()
	p.registerURL()
	p.registerQuerystring()
	p.registerUtil()
}

func (p *Program) registerEvents() {
	src := `(function () {
  function EventEmitter() {
    this._events = Object.create(null);
    this._maxListeners = 10;
  }
  EventEmitter.prototype.on = function (name, fn) {
    if (!this._events[name]) this._events[name] = [];
    this._events[name].push(fn);
    return this;
  };
  EventEmitter.prototype.addListener = EventEmitter.prototype.on;
  EventEmitter.prototype.prependListener = function (name, fn) {
    if (!this._events[name]) this._events[name] = [];
    this._events[name].unshift(fn);
    return this;
  };
  EventEmitter.prototype.once = function (name, fn) {
    var self = this;
    function wrapper() {
      self.off(name, wrapper);
      return fn.apply(self, arguments);
    }
    wrapper.listener = fn;
    return this.on(name, wrapper);
  };
  EventEmitter.prototype.prependOnceListener = function (name, fn) {
    var self = this;
    function wrapper() {
      self.off(name, wrapper);
      return fn.apply(self, arguments);
    }
    wrapper.listener = fn;
    return this.prependListener(name, wrapper);
  };
  EventEmitter.prototype.off = function (name, fn) {
    var list = this._events[name];
    if (!list) return this;
    for (var i = 0; i < list.length; i++) {
      if (list[i] === fn || list[i].listener === fn) {
        list.splice(i, 1);
        break;
      }
    }
    if (list.length === 0) delete this._events[name];
    return this;
  };
  EventEmitter.prototype.removeListener = EventEmitter.prototype.off;
  EventEmitter.prototype.removeAllListeners = function (name) {
    if (name === undefined) {
      this._events = Object.create(null);
    } else {
      delete this._events[name];
    }
    return this;
  };
  EventEmitter.prototype.emit = function (name) {
    var list = this._events[name];
    if (!list || list.length === 0) {
      if (name === 'error') {
        var err = arguments[1];
        if (err instanceof Error) throw err;
        throw new Error(String(err));
      }
      return false;
    }
    var args = Array.prototype.slice.call(arguments, 1);
    var copy = list.slice();
    for (var i = 0; i < copy.length; i++) {
      copy[i].apply(this, args);
    }
    return true;
  };
  EventEmitter.prototype.listeners = function (name) {
    var list = this._events[name];
    if (!list) return [];
    return list.map(function (fn) { return fn.listener || fn; });
  };
  EventEmitter.prototype.rawListeners = function (name) {
    return (this._events[name] || []).slice();
  };
  EventEmitter.prototype.listenerCount = function (name) {
    var list = this._events[name];
    return list ? list.length : 0;
  };
  EventEmitter.prototype.eventNames = function () {
    return Object.keys(this._events);
  };
  EventEmitter.prototype.setMaxListeners = function (n) {
    this._maxListeners = n;
    return this;
  };
  EventEmitter.prototype.getMaxListeners = function () {
    return this._maxListeners;
  };
  EventEmitter.EventEmitter = EventEmitter;
  EventEmitter.default = EventEmitter;
  return EventEmitter;
})()`
	v, err := p.rt.RunString(src)
	if err != nil {
		panic(fmt.Errorf("javascript: events shim: %w", err))
	}
	p.eventsVal = v
}

func (p *Program) registerUtil() {
	src := `(function () {
  var util = {};
  util.inherits = function (ctor, superCtor) {
    ctor.super_ = superCtor;
    ctor.prototype = Object.create(superCtor.prototype, {
      constructor: { value: ctor, enumerable: false, writable: true, configurable: true }
    });
  };
  util.format = function (f) {
    var args = Array.prototype.slice.call(arguments);
    var i = 1;
    var str = String(f).replace(/%[sdjifoO%]/g, function (tok) {
      if (tok === '%%') return '%';
      if (i >= args.length) return tok;
      var arg = args[i++];
      switch (tok) {
        case '%s': return String(arg);
        case '%d': return String(Number(arg));
        case '%i': return String(parseInt(arg, 10));
        case '%f': return String(parseFloat(arg));
        case '%j': try { return JSON.stringify(arg); } catch (e) { return '[Circular]'; }
        case '%o': case '%O': return util.inspect(arg);
        default: return tok;
      }
    });
    while (i < args.length) str += ' ' + String(args[i++]);
    return str;
  };
  util.inspect = function (obj) {
    if (obj === null) return 'null';
    if (obj === undefined) return 'undefined';
    if (typeof obj === 'string') return JSON.stringify(obj);
    if (typeof obj !== 'object') return String(obj);
    try { return JSON.stringify(obj); } catch (e) { return String(obj); }
  };
  util.isDeepStrictEqual = function (a, b) {
    return JSON.stringify(a) === JSON.stringify(b);
  };
  util.deprecate = function (fn, msg) {
    var warned = false;
    return function () {
      if (!warned) {
        warned = true;
        if (typeof process !== 'undefined' && process.emitWarning) {
          process.emitWarning(msg, 'DeprecationWarning');
        }
      }
      return fn.apply(this, arguments);
    };
  };
  util.promisify = function (fn) {
    return function () {
      var args = Array.prototype.slice.call(arguments);
      var self = this;
      return new Promise(function (resolve, reject) {
        args.push(function (err, value) {
          if (err) reject(err); else resolve(value);
        });
        fn.apply(self, args);
      });
    };
  };
  util.callbackify = function (fn) {
    return function () {
      var args = Array.prototype.slice.call(arguments);
      var cb = args.pop();
      fn.apply(this, args).then(function (value) { cb(null, value); }, function (err) { cb(err); });
    };
  };
  util.types = {};
  return util;
})()`
	v, err := p.rt.RunString(src)
	if err != nil {
		panic(fmt.Errorf("javascript: util shim: %w", err))
	}
	p.utilObj = v.ToObject(p.rt)
}

func (p *Program) registerURL() {
	obj := p.rt.NewObject()
	obj.Set("parse", func(call sobek.FunctionCall) sobek.Value {
		raw := call.Argument(0).String()
		parseQuery := false
		if len(call.Arguments) > 1 && !sobek.IsUndefined(call.Argument(1)) {
			parseQuery = call.Argument(1).ToBoolean()
		}
		u, err := url.Parse(raw)
		if err != nil {
			return sobek.Null()
		}
		out := p.rt.NewObject()
		out.Set("href", u.String())
		out.Set("protocol", u.Scheme+":")
		out.Set("slashes", u.Host != "")
		if u.User != nil {
			out.Set("auth", u.User.String())
		}
		out.Set("host", u.Host)
		out.Set("hostname", u.Hostname())
		out.Set("port", u.Port())
		out.Set("pathname", u.Path)
		if u.RawQuery != "" {
			out.Set("search", "?"+u.RawQuery)
		} else {
			out.Set("search", "")
		}
		if parseQuery {
			out.Set("query", p.queryObject(u.Query()))
		} else {
			out.Set("query", u.RawQuery)
		}
		if u.Fragment != "" {
			out.Set("hash", "#"+u.Fragment)
		} else {
			out.Set("hash", "")
		}
		if u.Path != "" || u.RawQuery != "" {
			out.Set("path", u.Path+"?"+u.RawQuery)
		}
		return out
	})
	obj.Set("format", func(call sobek.FunctionCall) sobek.Value {
		arg := call.Argument(0)
		if arg == nil || sobek.IsUndefined(arg) || sobek.IsNull(arg) {
			return p.rt.ToValue("")
		}
		o := arg.ToObject(p.rt)
		u := &url.URL{}
		if v := o.Get("protocol"); v != nil && !sobek.IsUndefined(v) {
			u.Scheme = strings.TrimSuffix(v.String(), ":")
		}
		if v := o.Get("host"); v != nil && !sobek.IsUndefined(v) {
			u.Host = v.String()
		} else {
			hostname := ""
			if v := o.Get("hostname"); v != nil && !sobek.IsUndefined(v) {
				hostname = v.String()
			}
			port := ""
			if v := o.Get("port"); v != nil && !sobek.IsUndefined(v) {
				port = v.String()
			}
			if port != "" {
				u.Host = hostname + ":" + port
			} else {
				u.Host = hostname
			}
		}
		if v := o.Get("pathname"); v != nil && !sobek.IsUndefined(v) {
			u.Path = v.String()
		}
		if v := o.Get("search"); v != nil && !sobek.IsUndefined(v) {
			u.RawQuery = strings.TrimPrefix(v.String(), "?")
		} else if v := o.Get("query"); v != nil && !sobek.IsUndefined(v) {
			if _, ok := v.Export().(string); ok {
				u.RawQuery = v.String()
			} else {
				u.RawQuery = p.queryString(v.ToObject(p.rt))
			}
		}
		if v := o.Get("hash"); v != nil && !sobek.IsUndefined(v) {
			u.Fragment = strings.TrimPrefix(v.String(), "#")
		}
		if v := o.Get("auth"); v != nil && !sobek.IsUndefined(v) {
			auth := v.String()
			if auth != "" {
				if idx := strings.Index(auth, ":"); idx >= 0 {
					u.User = url.UserPassword(auth[:idx], auth[idx+1:])
				} else {
					u.User = url.User(auth)
				}
			}
		}
		return p.rt.ToValue(u.String())
	})
	obj.Set("resolve", func(call sobek.FunctionCall) sobek.Value {
		from := call.Argument(0).String()
		to := call.Argument(1).String()
		base, err := url.Parse(from)
		if err != nil {
			return p.rt.ToValue(to)
		}
		ref, err := url.Parse(to)
		if err != nil {
			return p.rt.ToValue(to)
		}
		return p.rt.ToValue(base.ResolveReference(ref).String())
	})
	obj.Set("resolveObject", func(call sobek.FunctionCall) sobek.Value {
		from := call.Argument(0).String()
		to := call.Argument(1).String()
		base, err := url.Parse(from)
		if err != nil {
			return sobek.Null()
		}
		ref, err := url.Parse(to)
		if err != nil {
			return sobek.Null()
		}
		resolved := base.ResolveReference(ref)
		out := p.rt.NewObject()
		out.Set("href", resolved.String())
		return out
	})
	p.urlObj = obj
}

func (p *Program) registerQuerystring() {
	obj := p.rt.NewObject()
	obj.Set("parse", func(call sobek.FunctionCall) sobek.Value {
		s := call.Argument(0).String()
		values, err := url.ParseQuery(s)
		if err != nil {
			return p.rt.NewObject()
		}
		return p.queryObject(values)
	})
	obj.Set("stringify", func(call sobek.FunctionCall) sobek.Value {
		arg := call.Argument(0)
		if arg == nil || sobek.IsUndefined(arg) || sobek.IsNull(arg) {
			return p.rt.ToValue("")
		}
		return p.rt.ToValue(p.queryString(arg.ToObject(p.rt)))
	})
	obj.Set("escape", func(call sobek.FunctionCall) sobek.Value {
		return p.rt.ToValue(url.QueryEscape(call.Argument(0).String()))
	})
	obj.Set("unescape", func(call sobek.FunctionCall) sobek.Value {
		s, err := url.QueryUnescape(call.Argument(0).String())
		if err != nil {
			return p.rt.ToValue(call.Argument(0).String())
		}
		return p.rt.ToValue(s)
	})
	obj.Set("encode", obj.Get("escape"))
	obj.Set("decode", obj.Get("unescape"))
	p.querystringObj = obj
}

func (p *Program) registerBuffer() {
	obj := p.rt.NewObject()
	obj.Set("from", func(call sobek.FunctionCall) sobek.Value {
		arg := call.Argument(0)
		encoding := "utf8"
		if len(call.Arguments) > 1 && !sobek.IsUndefined(call.Argument(1)) {
			encoding = call.Argument(1).String()
		}
		return p.newBuffer(p.bufferFromValue(arg, encoding))
	})
	obj.Set("alloc", func(call sobek.FunctionCall) sobek.Value {
		size := int(call.Argument(0).ToInteger())
		fill := byte(0)
		if len(call.Arguments) > 1 {
			fill = p.bufferFillByte(call.Argument(1))
		}
		data := make([]byte, size)
		for i := range data {
			data[i] = fill
		}
		return p.newBuffer(data)
	})
	obj.Set("allocUnsafe", func(call sobek.FunctionCall) sobek.Value {
		size := int(call.Argument(0).ToInteger())
		return p.newBuffer(make([]byte, size))
	})
	obj.Set("isBuffer", func(call sobek.FunctionCall) sobek.Value {
		arg := call.Argument(0)
		if arg == nil || sobek.IsUndefined(arg) || sobek.IsNull(arg) {
			return p.rt.ToValue(false)
		}
		o := arg.ToObject(p.rt)
		v := o.Get("_isBuffer")
		return p.rt.ToValue(v != nil && !sobek.IsUndefined(v) && v.ToBoolean())
	})
	obj.Set("concat", func(call sobek.FunctionCall) sobek.Value {
		list := call.Argument(0).ToObject(p.rt)
		total := 0
		var bufs [][]byte
		length := int(list.Get("length").ToInteger())
		for i := 0; i < length; i++ {
			item := list.Get(strconv.Itoa(i))
			if item == nil || sobek.IsUndefined(item) || sobek.IsNull(item) {
				continue
			}
			b := bufferBytes(item.ToObject(p.rt))
			bufs = append(bufs, b)
			total += len(b)
		}
		if len(call.Arguments) > 1 && !sobek.IsUndefined(call.Argument(1)) {
			total = int(call.Argument(1).ToInteger())
		}
		data := make([]byte, 0, total)
		for _, b := range bufs {
			data = append(data, b...)
		}
		if len(data) > total {
			data = data[:total]
		}
		return p.newBuffer(data)
	})
	obj.Set("byteLength", func(call sobek.FunctionCall) sobek.Value {
		s := call.Argument(0).String()
		encoding := "utf8"
		if len(call.Arguments) > 1 && !sobek.IsUndefined(call.Argument(1)) {
			encoding = call.Argument(1).String()
		}
		return p.rt.ToValue(int64(len(encodeString(s, encoding))))
	})
	obj.Set("compare", func(call sobek.FunctionCall) sobek.Value {
		a := bufferBytes(call.Argument(0).ToObject(p.rt))
		b := bufferBytes(call.Argument(1).ToObject(p.rt))
		return p.rt.ToValue(int64(bytes.Compare(a, b)))
	})
	obj.Set("Buffer", obj)
	p.bufferObj = obj
	p.rt.Set("Buffer", obj)
}

func (p *Program) newBuffer(data []byte) *sobek.Object {
	obj := p.rt.NewObject()
	ab := p.rt.NewArrayBuffer(data)
	obj.Set("_ab", p.rt.ToValue(ab))
	obj.Set("_isBuffer", true)
	obj.Set("length", len(data))
	obj.Set("toString", func(call sobek.FunctionCall) sobek.Value {
		encoding := "utf8"
		if len(call.Arguments) > 0 && !sobek.IsUndefined(call.Argument(0)) {
			encoding = call.Argument(0).String()
		}
		start := 0
		end := len(data)
		if len(call.Arguments) > 1 && !sobek.IsUndefined(call.Argument(1)) {
			start = int(call.Argument(1).ToInteger())
		}
		if len(call.Arguments) > 2 && !sobek.IsUndefined(call.Argument(2)) {
			end = int(call.Argument(2).ToInteger())
		}
		if start < 0 {
			start = 0
		}
		if end > len(data) {
			end = len(data)
		}
		if start > end {
			start = end
		}
		return p.rt.ToValue(decodeString(data[start:end], encoding))
	})
	obj.Set("toJSON", func(call sobek.FunctionCall) sobek.Value {
		out := p.rt.NewObject()
		out.Set("type", "Buffer")
		arr := p.rt.NewArray()
		for i, b := range data {
			arr.Set(strconv.Itoa(i), p.rt.ToValue(int64(b)))
		}
		out.Set("data", arr)
		return out
	})
	obj.Set("equals", func(call sobek.FunctionCall) sobek.Value {
		other := call.Argument(0)
		if other == nil || sobek.IsUndefined(other) || sobek.IsNull(other) {
			return p.rt.ToValue(false)
		}
		return p.rt.ToValue(bytes.Equal(data, bufferBytes(other.ToObject(p.rt))))
	})
	obj.Set("slice", func(call sobek.FunctionCall) sobek.Value {
		start := 0
		end := len(data)
		if len(call.Arguments) > 0 && !sobek.IsUndefined(call.Argument(0)) {
			start = int(call.Argument(0).ToInteger())
		}
		if len(call.Arguments) > 1 && !sobek.IsUndefined(call.Argument(1)) {
			end = int(call.Argument(1).ToInteger())
		}
		if start < 0 {
			start = len(data) + start
		}
		if end < 0 {
			end = len(data) + end
		}
		if start < 0 {
			start = 0
		}
		if end > len(data) {
			end = len(data)
		}
		if start > end {
			start = end
		}
		return p.newBuffer(data[start:end])
	})
	obj.Set("subarray", obj.Get("slice"))
	obj.Set("copy", func(call sobek.FunctionCall) sobek.Value {
		target := call.Argument(0).ToObject(p.rt)
		targetStart := 0
		sourceStart := 0
		sourceEnd := len(data)
		if len(call.Arguments) > 1 && !sobek.IsUndefined(call.Argument(1)) {
			targetStart = int(call.Argument(1).ToInteger())
		}
		if len(call.Arguments) > 2 && !sobek.IsUndefined(call.Argument(2)) {
			sourceStart = int(call.Argument(2).ToInteger())
		}
		if len(call.Arguments) > 3 && !sobek.IsUndefined(call.Argument(3)) {
			sourceEnd = int(call.Argument(3).ToInteger())
		}
		if sourceStart < 0 {
			sourceStart = 0
		}
		if sourceEnd > len(data) {
			sourceEnd = len(data)
		}
		targetBytes := bufferBytes(target)
		n := copy(targetBytes[targetStart:], data[sourceStart:sourceEnd])
		return p.rt.ToValue(int64(n))
	})
	obj.Set("write", func(call sobek.FunctionCall) sobek.Value {
		s := call.Argument(0).String()
		offset := 0
		encoding := "utf8"
		if len(call.Arguments) > 1 && !sobek.IsUndefined(call.Argument(1)) {
			offset = int(call.Argument(1).ToInteger())
		}
		if len(call.Arguments) > 2 && !sobek.IsUndefined(call.Argument(2)) {
			encoding = call.Argument(2).String()
		}
		b := encodeString(s, encoding)
		n := copy(data[offset:], b)
		return p.rt.ToValue(int64(n))
	})
	obj.Set("fill", func(call sobek.FunctionCall) sobek.Value {
		fill := p.bufferFillByte(call.Argument(0))
		for i := range data {
			data[i] = fill
		}
		return obj
	})
	obj.Set("indexOf", func(call sobek.FunctionCall) sobek.Value {
		needle := p.bufferFromValue(call.Argument(0), "utf8")
		return p.rt.ToValue(int64(bytes.Index(data, needle)))
	})
	obj.Set("includes", func(call sobek.FunctionCall) sobek.Value {
		needle := p.bufferFromValue(call.Argument(0), "utf8")
		return p.rt.ToValue(bytes.Contains(data, needle))
	})
	obj.Set("readUInt8", func(call sobek.FunctionCall) sobek.Value {
		offset := int(call.Argument(0).ToInteger())
		if offset < 0 || offset >= len(data) {
			return p.rt.ToValue(int64(0))
		}
		return p.rt.ToValue(int64(data[offset]))
	})
	obj.Set("writeUInt8", func(call sobek.FunctionCall) sobek.Value {
		offset := int(call.Argument(0).ToInteger())
		value := byte(call.Argument(1).ToInteger())
		if offset >= 0 && offset < len(data) {
			data[offset] = value
		}
		return p.rt.ToValue(int64(offset + 1))
	})
	obj.Set("readUInt16LE", func(call sobek.FunctionCall) sobek.Value {
		offset := int(call.Argument(0).ToInteger())
		if offset+2 > len(data) {
			return p.rt.ToValue(int64(0))
		}
		return p.rt.ToValue(int64(binary.LittleEndian.Uint16(data[offset:])))
	})
	obj.Set("writeUInt16LE", func(call sobek.FunctionCall) sobek.Value {
		offset := int(call.Argument(0).ToInteger())
		value := uint16(call.Argument(1).ToInteger())
		if offset+2 <= len(data) {
			binary.LittleEndian.PutUint16(data[offset:], value)
		}
		return p.rt.ToValue(int64(offset + 2))
	})
	obj.Set("readUInt32LE", func(call sobek.FunctionCall) sobek.Value {
		offset := int(call.Argument(0).ToInteger())
		if offset+4 > len(data) {
			return p.rt.ToValue(int64(0))
		}
		return p.rt.ToValue(int64(binary.LittleEndian.Uint32(data[offset:])))
	})
	obj.Set("writeUInt32LE", func(call sobek.FunctionCall) sobek.Value {
		offset := int(call.Argument(0).ToInteger())
		value := uint32(call.Argument(1).ToInteger())
		if offset+4 <= len(data) {
			binary.LittleEndian.PutUint32(data[offset:], value)
		}
		return p.rt.ToValue(int64(offset + 4))
	})
	return obj
}

func bufferBytes(obj *sobek.Object) []byte {
	if obj == nil {
		return nil
	}
	v := obj.Get("_ab")
	if v == nil || sobek.IsUndefined(v) || sobek.IsNull(v) {
		return nil
	}
	ab, ok := v.Export().(sobek.ArrayBuffer)
	if !ok {
		return nil
	}
	return ab.Bytes()
}

func (p *Program) bufferFromValue(arg sobek.Value, encoding string) []byte {
	if arg == nil || sobek.IsUndefined(arg) || sobek.IsNull(arg) {
		return nil
	}
	switch v := arg.Export().(type) {
	case string:
		return encodeString(v, encoding)
	case []byte:
		return append([]byte(nil), v...)
	case sobek.ArrayBuffer:
		return append([]byte(nil), v.Bytes()...)
	case []interface{}:
		out := make([]byte, len(v))
		for i, item := range v {
			out[i] = byte(toInt64(item))
		}
		return out
	default:
		if obj := arg.ToObject(p.rt); obj != nil {
			if b := bufferBytes(obj); b != nil {
				return b
			}
		}
		return nil
	}
}

func toInt64(v interface{}) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	case uint64:
		return int64(n)
	default:
		return 0
	}
}

func (p *Program) bufferFillByte(v sobek.Value) byte {
	if v == nil || sobek.IsUndefined(v) || sobek.IsNull(v) {
		return 0
	}
	switch x := v.Export().(type) {
	case string:
		if len(x) > 0 {
			return x[0]
		}
	case []interface{}:
		if len(x) > 0 {
			return byte(toInt64(x[0]))
		}
	default:
		return byte(v.ToInteger())
	}
	return 0
}

func encodeString(s, encoding string) []byte {
	switch strings.ToLower(encoding) {
	case "hex":
		b, err := hex.DecodeString(s)
		if err != nil {
			return []byte(s)
		}
		return b
	case "base64":
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return []byte(s)
		}
		return b
	case "ascii", "latin1", "binary":
		return []byte(s)
	case "ucs2", "ucs-2", "utf16le", "utf-16le":
		u := utf16.Encode([]rune(s))
		out := make([]byte, len(u)*2)
		for i, r := range u {
			binary.LittleEndian.PutUint16(out[i*2:], r)
		}
		return out
	default:
		return []byte(s)
	}
}

func decodeString(b []byte, encoding string) string {
	switch strings.ToLower(encoding) {
	case "hex":
		return hex.EncodeToString(b)
	case "base64":
		return base64.StdEncoding.EncodeToString(b)
	case "ascii", "latin1", "binary":
		return string(b)
	case "ucs2", "ucs-2", "utf16le", "utf-16le":
		u := make([]uint16, len(b)/2)
		for i := range u {
			u[i] = binary.LittleEndian.Uint16(b[i*2:])
		}
		return string(utf16.Decode(u))
	default:
		return string(b)
	}
}

func (p *Program) queryObject(values url.Values) *sobek.Object {
	obj := p.rt.NewObject()
	for k, vs := range values {
		if len(vs) == 1 {
			obj.Set(k, p.rt.ToValue(vs[0]))
			continue
		}
		arr := p.rt.NewArray()
		for i, v := range vs {
			arr.Set(strconv.Itoa(i), p.rt.ToValue(v))
		}
		obj.Set(k, arr)
	}
	return obj
}

func (p *Program) queryString(obj *sobek.Object) string {
	values := url.Values{}
	for _, k := range obj.Keys() {
		v := obj.Get(k)
		if v == nil || sobek.IsUndefined(v) || sobek.IsNull(v) {
			continue
		}
		if arr, ok := v.Export().([]interface{}); ok {
			for _, item := range arr {
				values.Add(k, fmt.Sprint(item))
			}
			continue
		}
		values.Add(k, v.String())
	}
	return values.Encode()
}
