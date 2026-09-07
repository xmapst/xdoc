package xbexpr

import (
	"math/big"
	"strings"
	"time"

	"github.com/xmapst/xdoc/internal/xbin"
	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xfmt"
)

// dateKindOf 判断一个时间值是未指定时区、UTC，还是本地时间。格式化时的时区标记要靠它。
func dateKindOf(v *xbson.Value) xfmt.DateKind {
	switch {
	case v.Unspecified():
		return xfmt.DateUnspecified
	case v.Location() == time.UTC:
		return xfmt.DateUTC
	default:
		return xfmt.DateLocal
	}
}

// mFORMAT 按格式串把一个值格式化成字符串。格式串不是字符串时返回 Null。
func (*Ctx) mFORMAT(args []*xbson.Value) (*xbson.Value, error) {
	format, ok := str(args[1])
	if !ok {
		return xbson.Null, nil
	}
	s, err := formatValue(args[0], format)
	if err != nil {
		return nil, err
	}
	return xbson.String(s), nil
}

const (
	// 二进制、数组、文档没有格式化写法，格式化它们得到的是这三个固定的类型名。
	bytesText = "System.Byte[]"
	listText  = "System.Collections.Generic.List`1[XDoc.BsonValue]"
	dictText  = "System.Collections.Generic.Dictionary`2[System.String,XDoc.BsonValue]"
)

// formatValue 按类型分派到各自的格式化实现。
//
// 格式串里带花括号一律报错——那是复合格式的写法，这里不支持。
// Null 格式化成空串；没有专门写法的类型退回它自己的文本形式。
func formatValue(v *xbson.Value, format string) (string, error) {
	if strings.ContainsAny(format, "{}") {
		return "", errf("format specifier %q is invalid", format)
	}
	nf := xfmt.Current()
	switch v.Type() {
	case xbson.TypeDateTime:
		t, _ := v.AsTime()

		s, err := (xfmt.Date{T: t, Kind: dateKindOf(v)}).Format(format, xfmt.CurrentDate())
		if err != nil {
			return "", errf("format specifier %q is invalid", format)
		}
		return s, nil
	case xbson.TypeInt32:
		n, _ := v.AsInt32()
		return formatNum(xfmt.Int32.IntNum(int64(n)), format, nf)
	case xbson.TypeInt64:
		n, _ := v.AsInt64()
		return formatNum(xfmt.Int64.IntNum(n), format, nf)
	case xbson.TypeDouble:
		f, _ := v.AsDouble()
		return formatNum(xfmt.FloatNum(f), format, nf)
	case xbson.TypeDecimal:
		d, _ := v.AsDecimal()
		return formatNum(decToNum(d), format, nf)
	case xbson.TypeBoolean:
		b, _ := v.AsBoolean()
		if b {
			return "True", nil
		}
		return "False", nil
	case xbson.TypeGUID:
		g, _ := v.AsGUID()
		s, err := xfmt.Guid(g.String()).Format(format)
		if err != nil {
			return "", errf("format specifier %q is invalid", format)
		}
		return s, nil
	case xbson.TypeObjectID:
		id, _ := v.AsObjectID()
		return id.String(), nil
	case xbson.TypeBinary:
		return bytesText, nil
	case xbson.TypeArray:
		return listText, nil
	case xbson.TypeDocument:
		return dictText, nil
	case xbson.TypeNull:
		return "", nil
	default:
		return stringOf(v), nil
	}
}

// formatNum 格式化一个数，并把底层的错误换成统一的「格式串非法」。
func formatNum(n xfmt.Num, format string, nf *xfmt.NumberFormat) (string, error) {
	s, err := n.Format(format, nf)
	if err != nil {
		return "", errf("format specifier %q is invalid", format)
	}
	return s, nil
}

// decToNum 把十进制值转成格式化用的数。
//
// 走文本中转：先取它的十进制写法，再拆成符号、全部数字、小数位数。
// 拆不出整数时当零处理。
func decToNum(d xbin.Decimal) xfmt.Num {
	s := d.String()
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	ip, fp, _ := strings.Cut(s, ".")
	abs, ok := new(big.Int).SetString(ip+fp, 10)
	if !ok {
		abs = new(big.Int)
	}
	return xfmt.DecNum(abs, neg, len(fp))
}
