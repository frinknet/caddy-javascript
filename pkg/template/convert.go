package template

import (
	"fmt"
	"reflect"
	"time"

	quickjs "github.com/go-quickjs/go-quickjs"
)

var (
	errorType    = reflect.TypeOf((*error)(nil)).Elem()
	durationType = reflect.TypeOf(time.Duration(0))
	timeType     = reflect.TypeOf(time.Time{})
	valueType    = reflect.TypeOf(quickjs.Value{})
)

func call(rt *quickjs.Runtime, host Host, fn reflect.Value, args []quickjs.Value) (quickjs.Value, error) {
	t := fn.Type()
	numIn := t.NumIn()
	variadic := t.IsVariadic()

	fixed := numIn
	if variadic {
		fixed--
	}
	if len(args) < fixed {
		return quickjs.Value{}, fmt.Errorf("javascript: template function expects at least %d arguments, got %d", fixed, len(args))
	}
	if !variadic && len(args) > numIn {
		return quickjs.Value{}, fmt.Errorf("javascript: template function expects at most %d arguments, got %d", numIn, len(args))
	}

	in := make([]reflect.Value, 0, numIn)
	for i := 0; i < fixed; i++ {
		val, err := convertValue(rt, host, args[i], t.In(i))
		if err != nil {
			return quickjs.Value{}, fmt.Errorf("javascript: argument %d: %w", i+1, err)
		}
		in = append(in, val)
	}
	if variadic {
		elem := t.In(numIn - 1).Elem()
		slice := reflect.MakeSlice(t.In(numIn-1), 0, len(args)-fixed)
		for i := fixed; i < len(args); i++ {
			val, err := convertValue(rt, host, args[i], elem)
			if err != nil {
				return quickjs.Value{}, fmt.Errorf("javascript: argument %d: %w", i+1, err)
			}
			slice = reflect.Append(slice, val)
		}
		in = append(in, slice)
	}

	out := fn.Call(in)
	if len(out) > 0 && out[len(out)-1].Type().Implements(errorType) {
		if !out[len(out)-1].IsNil() {
			return quickjs.Value{}, out[len(out)-1].Interface().(error)
		}
		out = out[:len(out)-1]
	}
	if len(out) == 0 {
		return undefined(rt)
	}
	if len(out) > 1 {
		return quickjs.Value{}, fmt.Errorf("javascript: template function returned %d values", len(out))
	}
	return encodeValue(rt, out[0])
}

func undefined(rt *quickjs.Runtime) (quickjs.Value, error) {
	return rt.Get("undefined")
}

func encodeValue(rt *quickjs.Runtime, v reflect.Value) (quickjs.Value, error) {
	if !v.IsValid() {
		return undefined(rt)
	}
	if v.Type() == durationType {
		return rt.Encode(v.Interface().(time.Duration).String())
	}
	if v.Type() == timeType {
		return rt.Encode(v.Interface().(time.Time).Format(time.RFC3339))
	}
	return rt.Encode(v.Interface())
}

func convertValue(rt *quickjs.Runtime, host Host, v quickjs.Value, target reflect.Type) (reflect.Value, error) {
	if target == valueType {
		return reflect.ValueOf(v), nil
	}
	if target == durationType {
		return convertDuration(v)
	}
	if target == timeType {
		return convertTime(v)
	}
	out := reflect.New(target)
	if err := v.Decode(out.Interface()); err != nil {
		return reflect.Value{}, err
	}
	return out.Elem(), nil
}

func convertDuration(v quickjs.Value) (reflect.Value, error) {
	switch v.Kind() {
	case quickjs.KindString:
		d, err := time.ParseDuration(v.String())
		if err != nil {
			return reflect.Value{}, err
		}
		return reflect.ValueOf(d), nil
	case quickjs.KindNumber:
		return reflect.ValueOf(time.Duration(v.Int())), nil
	default:
		return reflect.Value{}, fmt.Errorf("expected a duration string or nanoseconds, got %s", v.Kind())
	}
}

func convertTime(v quickjs.Value) (reflect.Value, error) {
	switch v.Kind() {
	case quickjs.KindString:
		t, err := time.Parse(time.RFC3339, v.String())
		if err != nil {
			return reflect.Value{}, err
		}
		return reflect.ValueOf(t), nil
	case quickjs.KindObject:
		getTime, err := v.Get("getTime")
		if err != nil {
			return reflect.Value{}, err
		}
		if !getTime.IsFunction() {
			return reflect.Value{}, fmt.Errorf("expected a Date or RFC 3339 string")
		}
		ms, err := getTime.CallWithThis(v)
		if err != nil {
			return reflect.Value{}, err
		}
		if ms.Kind() != quickjs.KindNumber {
			return reflect.Value{}, fmt.Errorf("expected a Date or RFC 3339 string")
		}
		return reflect.ValueOf(time.UnixMilli(int64(ms.Int()))), nil
	default:
		return reflect.Value{}, fmt.Errorf("expected a Date or RFC 3339 string, got %s", v.Kind())
	}
}
