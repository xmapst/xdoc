package xbexpr

import (
	"math"

	"github.com/xmapst/xdoc/internal/xbson"
)

// init 登记本文件里的数学方法。
//
// 登记的是方法表达式：方法的接收者正好补上 [Method] 签名里的 ctx 那一位，
// 用不到上下文的方法因此也挂在 [Ctx] 上。
func init() {
	reg("ABS", (*Ctx).mABS, Info{Params: scalars(1)})
	reg("ROUND", (*Ctx).mROUND, Info{Params: scalars(2)})
	reg("POW", (*Ctx).mPOW, Info{Params: scalars(2)})
}

// mABS 求绝对值，结果保持原来的数值类型。
//
// 两种整型的最小值都没有对应的正数，那两个报错。不是数则返回 Null。
func (*Ctx) mABS(args []*xbson.Value) (*xbson.Value, error) {
	v := args[0]
	switch v.Type() {
	case xbson.TypeInt32:
		n, _ := v.AsInt32()
		if n == math.MinInt32 {
			return nil, errf("ABS: %d has no 32-bit absolute value", n)
		}
		if n < 0 {
			n = -n
		}
		return xbson.Int32(n), nil
	case xbson.TypeInt64:
		n, _ := v.AsInt64()
		if n == math.MinInt64 {
			return nil, errf("ABS: %d has no 64-bit absolute value", n)
		}
		if n < 0 {
			n = -n
		}
		return xbson.Int64(n), nil
	case xbson.TypeDouble:
		f, _ := v.AsDouble()
		return xbson.Double(math.Abs(f)), nil
	case xbson.TypeDecimal:
		d, _ := v.AsDecimal()
		return decFromXbin(d).abs().toValue()
	default:
		return xbson.Null, nil
	}
}

// mROUND 四舍五入到指定位数。
//
// 整型原样返回。双精度的位数限 0 到 15，十进制限 0 到最大标度，超出报错。
// 位数本身不是数时返回 Null。
func (*Ctx) mROUND(args []*xbson.Value) (*xbson.Value, error) {
	value, digitsVal := args[0], args[1]
	if !isNumber(digitsVal) {
		return xbson.Null, nil
	}
	switch value.Type() {
	case xbson.TypeInt32, xbson.TypeInt64:
		return value, nil
	case xbson.TypeDouble:
		digits, err := int32Of(digitsVal)
		if err != nil {
			return nil, err
		}
		if digits < 0 || digits > 15 {
			return nil, errf("ROUND: digits must be between 0 and 15, got %d", digits)
		}
		f, _ := value.AsDouble()
		return xbson.Double(roundDouble(f, int(digits))), nil
	case xbson.TypeDecimal:
		digits, err := int32Of(digitsVal)
		if err != nil {
			return nil, err
		}
		if digits < 0 || digits > decMaxScale {
			return nil, errf("ROUND: digits must be between 0 and %d for a decimal, got %d", decMaxScale, digits)
		}
		d, _ := value.AsDecimal()
		return decFromXbin(d).round(int(digits)).toValue()
	default:
		return xbson.Null, nil
	}
}

// roundDouble 把双精度舍到指定位数，取**四舍六入五成双**。
//
// NaN、无穷，以及绝对值大到小数位已经没有意义的数，原样返回。
func roundDouble(f float64, digits int) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) || math.Abs(f) >= 1e16 {
		return f
	}
	p := math.Pow(10, float64(digits))
	return math.RoundToEven(f*p) / p
}

// mPOW 求幂，一律按双精度算。任一边不是数就返回 Null。
func (*Ctx) mPOW(args []*xbson.Value) (*xbson.Value, error) {
	x, ok := float64Of(args[0])
	y, ok2 := float64Of(args[1])
	if !ok || !ok2 {
		return xbson.Null, nil
	}
	return xbson.Double(math.Pow(x, y)), nil
}
