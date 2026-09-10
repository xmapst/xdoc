package xbson

import (
	"bytes"
	"cmp"
	"math"
	"math/big"

	"github.com/xmapst/xdoc/internal/xbin"
	"github.com/xmapst/xdoc/internal/xcoll"
)

// Compare 比较两个值，返回负、零、正。
//
// nil 当作 [Null]。
//
// 类型不同时：都是数字就跨类型按数值比，否则**按类型编号比**——
// 所以任何数字都小于任何字符串，[MinValue] 小于一切、[MaxValue] 大于一切。
// 这个次序是格式的一部分，索引就是按它排的。
func (v *Value) Compare(b *Value, c xcoll.Collation) int {
	if v == nil {
		v = Null
	}
	if b == nil {
		b = Null
	}
	if v.t != b.t {
		if v.t.IsNumber() && b.t.IsNumber() {
			return v.compareNumber(b)
		}
		return cmp.Compare(int(v.t), int(b.t))
	}

	switch v.t {
	case TypeMinValue, TypeNull, TypeMaxValue:
		return 0
	case TypeBoolean:
		return cmp.Compare(v.num, b.num)
	case TypeInt32, TypeInt64, TypeDateTime:
		return cmp.Compare(v.num, b.num)
	case TypeDouble:
		return cmpFloat(v.flt, b.flt)
	case TypeDecimal:
		x, _ := v.AsDecimal()
		y, _ := b.AsDecimal()
		return x.Compare(y)
	case TypeString:
		return c.Compare(v.str, b.str)
	case TypeBinary:
		x, _ := v.AsBinary()
		y, _ := b.AsBinary()
		return bytes.Compare(x, y)
	case TypeObjectID:
		x, _ := v.AsObjectID()
		y, _ := b.AsObjectID()
		return x.Compare(y)
	case TypeGUID:
		x, _ := v.AsGUID()
		y, _ := b.AsGUID()
		return x.Compare(y)
	case TypeVector:
		x, _ := v.AsVector()
		y, _ := b.AsVector()
		return compareVector(x, y)
	case TypeDocument:
		x, _ := v.AsDocument()
		y, _ := b.AsDocument()
		return x.compareDocument(y, c)
	case TypeArray:
		x, _ := v.AsArray()
		y, _ := b.AsArray()
		return x.compareArray(y, c)
	default:
		return 0
	}
}

// Equal 判断两个值是否相等。
func (v *Value) Equal(b *Value, c xcoll.Collation) bool { return v.Compare(b, c) == 0 }

// BinaryKey 是值按 [xcoll.Binary] 判等时的身份，可以当 map 的键用。
//
// 对 ok 为真的两个值，a.Compare(b, xcoll.Binary) == 0 当且仅当两者的 BinaryKey 相等。
type BinaryKey struct {
	t   Type
	num int64
	raw [16]byte
	str string
}

// BinaryKey 算出值按二进制序判等的身份。
//
// 整数与整值浮点跨类型相等，所以都归到 int64 上；NaN 彼此相等，±0 也相等。
// 小数（与其他数字跨类型相等，归一代价太大）、文档、数组、向量没有这样的身份，
// ok 为假，调用方得退回逐个 Compare。
func (v *Value) BinaryKey() (BinaryKey, bool) {
	if v == nil {
		v = Null
	}
	switch v.t {
	case TypeMinValue, TypeNull, TypeMaxValue:
		return BinaryKey{t: v.t}, true
	case TypeBoolean, TypeDateTime:
		return BinaryKey{t: v.t, num: v.num}, true
	case TypeInt32, TypeInt64:
		return BinaryKey{t: TypeInt64, num: v.num}, true
	case TypeDouble:
		const twoPow63 = 9223372036854775808.0
		f := v.flt
		switch {
		case math.IsNaN(f):
			return BinaryKey{t: TypeDouble, num: int64(math.Float64bits(math.NaN()))}, true
		case f == math.Trunc(f) && f >= -twoPow63 && f < twoPow63:
			return BinaryKey{t: TypeInt64, num: int64(f)}, true
		}
		return BinaryKey{t: TypeDouble, num: int64(math.Float64bits(f))}, true
	case TypeString:
		return BinaryKey{t: v.t, str: v.str}, true
	case TypeBinary:
		b, _ := v.AsBinary()
		return BinaryKey{t: v.t, str: string(b)}, true
	case TypeObjectID:
		k := BinaryKey{t: v.t}
		id, _ := v.AsObjectID()
		copy(k.raw[:], id[:])
		return k, true
	case TypeGUID:
		g, _ := v.AsGUID()
		return BinaryKey{t: v.t, raw: g}, true
	}
	return BinaryKey{}, false
}

// compareDocument 逐键比较两篇文档，键少的排前面。
//
// **按左边那篇的键名去右边取值**：右边没有这个键时取到空值。
// 所以这个比较不是对称的——它是格式定下的次序，不是一个通用的相等判定。
//
// 排序规则参数被忽略，一律按二进制比：文档作为索引键时必须有一个
// 与语言无关的确定次序。
func (d *Document) compareDocument(y *Document, _ xcoll.Collation) int {
	// 直接遍历内部切片：比较只读不写，不必像 [Document.Keys] 那样复制一份。
	n := min(len(d.keys), len(y.keys))
	for i, k := range d.keys[:n] {
		if r := d.vals[i].compareElem(y.Get(k), xcoll.Binary); r != 0 {
			return r
		}
	}
	return cmp.Compare(len(d.keys), len(y.keys))
}

// compareElem 比较容器里的一个元素。
//
// 两边都是字符串时按二进制比，不用传进来的排序规则：容器内部的次序
// 不该随语言变。
func (v *Value) compareElem(b *Value, c xcoll.Collation) int {
	if v != nil && b != nil && v.Type() == TypeString && b.Type() == TypeString {
		x, _ := v.AsString()
		y, _ := b.AsString()
		return cmpString(x, y, xcoll.Binary)
	}
	return v.Compare(b, c)
}

// compareArray 逐项比较两个数组，短的排前面。
func (a *Array) compareArray(y *Array, c xcoll.Collation) int {
	n := min(a.Len(), y.Len())
	for i := range n {
		if r := a.At(i).compareElem(y.At(i), c); r != 0 {
			return r
		}
	}
	return cmp.Compare(a.Len(), y.Len())
}

// compareVector 逐项比较两个向量，短的排前面。
func compareVector(x, y []float32) int {
	n := min(len(x), len(y))
	for i := range n {
		if r := cmpFloat(float64(x[i]), float64(y[i])); r != 0 {
			return r
		}
	}
	return cmp.Compare(len(x), len(y))
}

// compareNumber 跨类型比较两个数字。
//
// 有一边是十进制数时走精确比较；一边整数一边浮点时不把整数转成浮点——
// 大整数转浮点会丢精度，让两个不同的数比成相等。
func (v *Value) compareNumber(b *Value) int {
	switch {
	case v.t == TypeDecimal || b.t == TypeDecimal:
		return v.compareExact(b)
	case v.t == TypeDouble && b.t == TypeDouble:
		return cmpFloat(v.flt, b.flt)
	case v.t == TypeDouble:
		return -cmpIntFloat(b.num, v.flt)
	case b.t == TypeDouble:
		return cmpIntFloat(v.num, b.flt)
	default:
		return cmp.Compare(v.num, b.num)
	}
}

// compareExact 精确比较，先处理 NaN 与无穷。
//
// 有理数表示不了那三个值，所以要单独排序。
//
// 没有浮点参与时（十进制数对整数）把整数转成十进制数直接比，不必造有理数；
// 有浮点参与时浮点可能超出十进制数的范围或精度，仍走有理数。
func (v *Value) compareExact(b *Value) int {
	if r, ok := v.compareSpecial(b); ok {
		return r
	}
	if v.t != TypeDouble && b.t != TypeDouble {
		return v.toDecimal().Compare(b.toDecimal())
	}
	return v.toRat().Cmp(b.toRat())
}

// toDecimal 把十进制数或整数值转成十进制数。
//
// 整数的绝对值不超过 2^63，放得进 96 位尾数的低 64 位，小数位数取零，
// 所以转换是精确的。取绝对值用无符号取反：最小的 int64 取反会溢出，
// 转成 uint64 再取反恰好得到 2^63。
func (v *Value) toDecimal() xbin.Decimal {
	if v.t == TypeDecimal {
		d, _ := v.AsDecimal()
		return d
	}
	u := uint64(v.num)
	if v.num < 0 {
		u = -u
	}
	d, _ := xbin.DecimalFromParts(uint32(u), uint32(u>>32), 0, 0, v.num < 0)
	return d
}

// compareSpecial 比较 NaN 与无穷，两边都不是特殊值时报 false。
func (v *Value) compareSpecial(b *Value) (int, bool) {
	ra, rb := v.rankSpecial(), b.rankSpecial()
	if ra == 0 && rb == 0 {
		return 0, false
	}
	return cmp.Compare(ra, rb), true
}

// rankSpecial 给特殊值排个次序：NaN 最小，然后负无穷，正无穷最大。
//
// NaN 排在负无穷之前是个约定——它与谁都比不出大小，总得给它一个
// 确定的位置，否则排序结果不稳定。
func (v *Value) rankSpecial() int {
	if v.t != TypeDouble {
		return 0
	}
	switch {
	case math.IsNaN(v.flt):
		return -2
	case math.IsInf(v.flt, -1):
		return -1
	case math.IsInf(v.flt, 1):
		return 1
	default:
		return 0
	}
}

// toRat 把一个数字值转成精确的有理数。
func (v *Value) toRat() *big.Rat {
	switch v.t {
	case TypeDouble:
		r := new(big.Rat)
		r.SetFloat64(v.flt)
		return r
	case TypeDecimal:
		d, _ := v.AsDecimal()
		r, _ := new(big.Rat).SetString(d.String())
		return r
	default:
		return new(big.Rat).SetInt64(v.num)
	}
}

// cmpIntFloat 精确比较一个整数与一个浮点数。
//
// 不把整数转成浮点：超过 2^53 的整数转过去会丢精度。做法是先按范围
// 排除掉浮点超出整数范围的情形，再比整数部分，最后看小数部分的正负。
func cmpIntFloat(i int64, f float64) int {
	switch {
	case math.IsNaN(f):
		return 1
	case math.IsInf(f, 1):
		return -1
	case math.IsInf(f, -1):
		return 1
	}

	const twoPow63 = 9223372036854775808.0
	if f >= twoPow63 {
		return -1
	}
	if f < -twoPow63 {
		return 1
	}
	t := math.Trunc(f)
	if r := cmp.Compare(i, int64(t)); r != 0 {
		return r
	}

	switch frac := f - t; {
	case frac > 0:
		return -1
	case frac < 0:
		return 1
	default:
		return 0
	}
}

// cmpFloat 比较两个浮点，NaN 排在一切之前。
func cmpFloat(a, b float64) int { return cmp.Compare(a, b) }

// cmpString 按排序规则比较两个串。
func cmpString(a, b string, c xcoll.Collation) int { return c.Compare(a, b) }
