package xbexpr

import (
	"crypto/rand"
	"errors"
	"fmt"
	"unicode/utf16"

	"github.com/xmapst/xdoc/internal/xbin"
	"github.com/xmapst/xdoc/internal/xbson"
)

// ErrExpr 是本包所有错误的根，供 [errors.Is] 一把抓。
var ErrExpr = errors.New("xbexpr")

// errf 造一个包在 [ErrExpr] 下的错误。
func errf(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrExpr}, a...)...)
}

// ErrEmptySequence 表示某个必须有值的序列是空的。
//
// 目前没有代码返回它：聚合方法遇上空序列各有各的空值约定，不走这条错误。
var ErrEmptySequence = fmt.Errorf("%w: sequence is empty", ErrExpr)

// seqOf 把一串值装成数组值，nil 换成 Null。序列形参就是这么传给方法的。
func seqOf(vals []*xbson.Value) *xbson.Value {
	a := xbson.NewArray()
	for _, v := range vals {
		if v == nil {
			v = xbson.Null
		}
		a.Append(v)
	}
	return a.Value()
}

// emptySeq 返回一个空序列。
func emptySeq() *xbson.Value { return xbson.NewArray().Value() }

// items 把一个值摊成若干项：数组摊成各项，其余就是它自己一项。
func items(v *xbson.Value) []*xbson.Value {
	if a, ok := v.AsArray(); ok {
		return a.Items()
	}
	return []*xbson.Value{v}
}

// isNumber 判断这个值是不是数。
func isNumber(v *xbson.Value) bool { return v.Type().IsNumber() }

// isString 判断这个值是不是字符串。
func isString(v *xbson.Value) bool { return v.Type() == xbson.TypeString }

// str 取字符串值。
func str(v *xbson.Value) (string, bool) { return v.AsString() }

// int32Of 把一个数转成 32 位整数，超出范围就报错。
func int32Of(v *xbson.Value) (int32, error) {
	n, err := int64Of(v)
	if err != nil {
		return 0, err
	}
	if n < -2147483648 || n > 2147483647 {
		return 0, errf("value %s is out of range for a 32-bit integer", v.String())
	}
	return int32(n), nil
}

// int64Of 把一个数转成 64 位整数；不是数就报错。
func int64Of(v *xbson.Value) (int64, error) {
	switch v.Type() {
	case xbson.TypeInt32:
		n, _ := v.AsInt32()
		return int64(n), nil
	case xbson.TypeInt64:
		n, _ := v.AsInt64()
		return n, nil
	case xbson.TypeDouble:
		f, _ := v.AsDouble()
		return floatToInt64(f)
	case xbson.TypeDecimal:
		d, _ := v.AsDecimal()
		return decFromXbin(d).toInt64()
	default:
		return 0, errf("value of type %s is not a number", v.Type())
	}
}

// float64Of 把一个数转成双精度；不是数返回 false。转换可能丢精度，但不会报错。
func float64Of(v *xbson.Value) (float64, bool) {
	switch v.Type() {
	case xbson.TypeInt32:
		n, _ := v.AsInt32()
		return float64(n), true
	case xbson.TypeInt64:
		n, _ := v.AsInt64()
		return float64(n), true
	case xbson.TypeDouble:
		return v.AsDouble()
	case xbson.TypeDecimal:
		d, _ := v.AsDecimal()
		return decFromXbin(d).toFloat64(), true
	default:
		return 0, false
	}
}

// utf16Of 把串转成 UTF-16 码元。字符串方法按码元计位置，不按字节也不按字符。
func utf16Of(s string) []uint16 { return utf16.Encode([]rune(s)) }

// utf16Str 把 UTF-16 码元转回串。
func utf16Str(u []uint16) string { return string(utf16.Decode(u)) }

// utf16Len 返回串的 UTF-16 码元数，也就是字符串方法眼里的长度。
func utf16Len(s string) int { return len(utf16Of(s)) }

// newGuidV4 生成一个随机 GUID。
//
// 取满 16 字节随机数后，把版本位置成 4、变体位置成 RFC 4122 那一套。
func newGuidV4() (xbin.Guid, error) {
	var g xbin.Guid
	if _, err := rand.Read(g[:]); err != nil {
		return xbin.GuidNil, errf("cannot generate a guid: %v", err)
	}

	g[6] = g[6]&0x0F | 0x40
	g[8] = g[8]&0x3F | 0x80
	return g, nil
}
