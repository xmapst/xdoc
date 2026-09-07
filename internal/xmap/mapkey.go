package xmap

import (
	"encoding"
	"fmt"
	"reflect"
	"strconv"
)

// keyEncoder 造一个把映射键转成字段名的函数，转不了返回 nil。
//
// 优先用 [encoding.TextMarshaler]；类型本身没实现而指针实现了的，
// 会先拷一份到新分配的指针上再调——原值可能取不到地址。
// 否则按字符串或整数种类直接转。
func keyEncoder(t reflect.Type) func(reflect.Value) (string, error) {
	if t.Implements(textMarshalerType) {
		return marshalTextKey
	}

	if reflect.PointerTo(t).Implements(textMarshalerType) {
		return func(rv reflect.Value) (string, error) {
			p := reflect.New(t)
			p.Elem().Set(rv)
			return marshalTextKey(p)
		}
	}
	switch t.Kind() {
	case reflect.String:
		return func(rv reflect.Value) (string, error) { return rv.String(), nil }
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return func(rv reflect.Value) (string, error) { return strconv.FormatInt(rv.Int(), 10), nil }
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return func(rv reflect.Value) (string, error) { return strconv.FormatUint(rv.Uint(), 10), nil }
	default:
	}
	return nil
}

// marshalTextKey 调 MarshalText 取出键的文本。
func marshalTextKey(rv reflect.Value) (string, error) {
	b, err := rv.Interface().(encoding.TextMarshaler).MarshalText()
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// keyDecoder 造一个把字段名转回映射键的函数，转不了返回 nil。
//
// 优先用 [encoding.TextUnmarshaler]，否则按字符串或整数种类解析，
// 整数按目标类型的位宽判越界。
func keyDecoder(t reflect.Type) func(string) (reflect.Value, error) {
	if reflect.PointerTo(t).Implements(textUnmarshalerType) {
		return func(s string) (reflect.Value, error) {
			p := reflect.New(t)
			if err := p.Interface().(encoding.TextUnmarshaler).UnmarshalText([]byte(s)); err != nil {
				return reflect.Value{}, err
			}
			return p.Elem(), nil
		}
	}
	switch t.Kind() {
	case reflect.String:
		return func(s string) (reflect.Value, error) { return reflect.ValueOf(s).Convert(t), nil }
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return func(s string) (reflect.Value, error) {
			n, err := strconv.ParseInt(s, 10, t.Bits())
			if err != nil {
				return reflect.Value{}, fmt.Errorf("cannot parse field name %q as %s: %w", s, t, err)
			}
			v := reflect.New(t).Elem()
			v.SetInt(n)
			return v, nil
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return func(s string) (reflect.Value, error) {
			n, err := strconv.ParseUint(s, 10, t.Bits())
			if err != nil {
				return reflect.Value{}, fmt.Errorf("cannot parse field name %q as %s: %w", s, t, err)
			}
			v := reflect.New(t).Elem()
			v.SetUint(n)
			return v, nil
		}
	default:
	}
	return nil
}

var (
	// 映射键的文本编解码接口。
	textMarshalerType   = reflect.TypeFor[encoding.TextMarshaler]()
	textUnmarshalerType = reflect.TypeFor[encoding.TextUnmarshaler]()
)
