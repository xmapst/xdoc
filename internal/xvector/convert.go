package xvector

import (
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/xmapst/xdoc/internal/xbin"
	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xfmt"
)

// ToDouble 把一个值转成双精度浮点。
//
// 空值与两个哨兵值给 0；布尔给 0 或 1；字符串按当前数字格式解析。
// 其余非数字类型转不出来。
func ToDouble(v *xbson.Value) (float64, bool) {
	switch v.Type() {
	case xbson.TypeNull, xbson.TypeMinValue, xbson.TypeMaxValue:
		return 0, true
	case xbson.TypeBoolean:
		b, _ := v.AsBoolean()
		if b {
			return 1, true
		}
		return 0, true
	case xbson.TypeString:
		s, _ := v.AsString()
		return parseDouble(s)
	default:
		return numericDouble(v)
	}
}

// parseDouble 按当前数字格式解析一个数字串。
//
// 先把分组分隔符去掉、把本地的小数点与正负号换成标准写法，再交给标准解析。
//
// 小数点不是 `.` 时，串里出现 `.` 直接判失败：那时 `.` 是分组分隔符的候选，
// 把它当小数点会把 1.234 读成 1.234 而不是 1234。
//
// 换完之后逐字符筛一遍，只允许数字与 `+-.eE`：标准解析还认十六进制浮点、
// 下划线分隔这些写法，放进来会让「什么串算数字」变得不可预期。
func parseDouble(s string) (float64, bool) {
	nf := xfmt.Current()
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, false
	}

	switch t {
	case nf.NaNSymbol:
		return math.NaN(), true
	case nf.PositiveInfinitySymbol:
		return math.Inf(1), true
	case nf.NegativeInfinitySymbol:
		return math.Inf(-1), true
	}
	if g := nf.NumberGroupSeparator; g != "" {
		t = strings.ReplaceAll(t, g, "")
	}
	if d := nf.NumberDecimalSeparator; d != "" && d != "." {
		if strings.Contains(t, ".") {
			return 0, false
		}
		t = strings.ReplaceAll(t, d, ".")
	}
	if n := nf.NegativeSign; n != "" && n != "-" {
		t = strings.Replace(t, n, "-", 1)
	}
	if p := nf.PositiveSign; p != "" && p != "+" {
		t = strings.Replace(t, p, "+", 1)
	}

	for _, c := range t {
		switch {
		case c >= '0' && c <= '9', c == '+', c == '-', c == '.', c == 'e', c == 'E':
		default:
			return 0, false
		}
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// numericDouble 把数字类型的值转成双精度浮点。
func numericDouble(v *xbson.Value) (float64, bool) {
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
		return decimalToFloat(d), true
	default:
		return 0, false
	}
}

// decimalToFloat 把十进制数转成双精度浮点，转不出来给 NaN。
//
// 走文本中转：十进制的尾数有 96 位，没有直接的位级转换。
func decimalToFloat(d xbin.Decimal) float64 {
	f, err := strconv.ParseFloat(d.String(), 64)
	if err != nil {
		return math.NaN()
	}
	return f
}

// ExtractVector 从一个值里取出定长向量。
//
// 向量类型直接拷一份；数组则逐项转成浮点。长度对不上、或有一项转不出来，
// 整个判失败——半截向量算出来的距离没有意义。
//
// 拷贝而不是共享底层数组：取出来的向量会被存进索引，与原值同生共死会
// 把整篇文档拖住不放。
func ExtractVector(v *xbson.Value, dims uint16) ([]float32, bool) {
	switch v.Type() {
	case xbson.TypeVector:
		f, _ := v.AsVector()
		if len(f) != int(dims) {
			return nil, false
		}
		return slices.Clone(f), true
	case xbson.TypeArray:
		a, _ := v.AsArray()
		if a.Len() != int(dims) {
			return nil, false
		}
		out := make([]float32, 0, a.Len())
		for _, it := range a.Items() {
			f, ok := ToDouble(it)
			if !ok {
				return nil, false
			}
			out = append(out, float32(f))
		}
		return out, true
	default:
		return nil, false
	}
}
