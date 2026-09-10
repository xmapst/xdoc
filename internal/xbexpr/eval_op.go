package xbexpr

import (
	"math"
	"math/big"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/xmapst/xdoc/internal/xbin"
	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xcoll"
)

// evalBinary 求一次二元运算。
//
// AND/OR 走短路，带 ANY/ALL 的走量词，其余两边都求出来再按基础运算算。
func (n *BinaryNode) evalBinary(e env) (*xbson.Value, error) {
	if n == nil {
		return xbson.Null, nil
	}
	if !n.Op.valid() {
		return nil, errf("unknown operator %d", n.Op)
	}

	switch n.Op {
	case OpAnd, OpOr:
		return n.logical(e)
	default:
	}

	if n.Op.Quantifier() != QuantNone {
		return n.quantified(e)
	}

	left, err := e.evalScalar(n.Left)
	if err != nil {
		return nil, err
	}
	right, err := e.evalScalar(n.Right)
	if err != nil {
		return nil, err
	}
	return n.Op.Base().apply(left, right, e)
}

// logical 求 AND 或 OR，左边已经定胜负时就不求右边了。
//
// 两边都必须是布尔值：不是布尔就报错，而不是按真假性去猜。
func (n *BinaryNode) logical(e env) (*xbson.Value, error) {
	left, err := e.evalScalar(n.Left)
	if err != nil {
		return nil, err
	}
	lb, ok := left.AsBoolean()
	if !ok {
		return nil, errf("`%s` needs boolean operands, left side is %s", n.Op, left.Type())
	}
	if (n.Op == OpAnd && !lb) || (n.Op == OpOr && lb) {
		return xbson.Boolean(lb), nil
	}

	right, err := e.evalScalar(n.Right)
	if err != nil {
		return nil, err
	}
	rb, ok := right.AsBoolean()
	if !ok {
		return nil, errf("`%s` needs boolean operands, right side is %s", n.Op, right.Type())
	}
	return xbson.Boolean(rb), nil
}

// quantified 求带 ANY/ALL 的比较：左边是一串值，逐项与右边比。
//
// ALL 从真开始，撞上一个假就定案；ANY 从假开始，撞上一个真就定案。
// 因此空序列下 ALL 为真、ANY 为假。右边只求一次。
func (n *BinaryNode) quantified(e env) (*xbson.Value, error) {
	right, err := e.evalScalar(n.Right)
	if err != nil {
		return nil, err
	}
	base := n.Op.Base()
	all := n.Op.Quantifier() == QuantAll

	result := all
	for item, err := range e.seqValues(n.Left) {
		if err != nil {
			return nil, err
		}
		v, err := base.apply(item, right, e)
		if err != nil {
			return nil, err
		}
		b, _ := v.AsBoolean()
		if b != all {
			result = b
			break
		}
	}
	return xbson.Boolean(result), nil
}

// apply 按基础运算算出结果。
//
// 算术运算遇上非数一律返回 Null 而不是报错——文档里字段缺失、类型不齐是常态。
// 比较运算走值的比较，用当前排序规则。
func (o Operator) apply(left, right *xbson.Value, e env) (*xbson.Value, error) {
	switch o {
	case OpAdd:
		return opAdd(left, right)
	case OpSub:
		return opSub(left, right)
	case OpMul:
		if !isNumber(left) || !isNumber(right) {
			return xbson.Null, nil
		}
		return opKindMul.numBinary(left, right)
	case OpDiv:
		if !isNumber(left) || !isNumber(right) {
			return xbson.Null, nil
		}
		return numDiv(left, right)
	case OpMod:
		if !isNumber(left) || !isNumber(right) {
			return xbson.Null, nil
		}
		return numMod(left, right)

	case OpEQ:
		return xbson.Boolean(left.Compare(right, e.coll) == 0), nil
	case OpNE:
		return xbson.Boolean(left.Compare(right, e.coll) != 0), nil
	case OpGT:
		return xbson.Boolean(left.Compare(right, e.coll) > 0), nil
	case OpGTE:
		return xbson.Boolean(left.Compare(right, e.coll) >= 0), nil
	case OpLT:
		return xbson.Boolean(left.Compare(right, e.coll) < 0), nil
	case OpLTE:
		return xbson.Boolean(left.Compare(right, e.coll) <= 0), nil
	case OpLike:
		return opLike(left, right, e.coll), nil
	case OpBetween:
		return opBetween(left, right, e.coll)
	case OpIn:
		return opIn(left, right, e.coll), nil
	case OpVectorSim:
		fn := Lookup("VECTOR_SIM", 2)
		if fn == nil {
			return nil, errf("method VECTOR_SIM does not exist or contains invalid parameters")
		}
		return fn(e.ctx(), []*xbson.Value{left, right})
	default:
	}
	return nil, errf("operator `%s` cannot be applied here", o)
}

// opLike 求 LIKE。两边有一个不是字符串就是假。
func opLike(left, right *xbson.Value, coll xcoll.Collation) *xbson.Value {
	s, ok := str(left)
	p, ok2 := str(right)
	if !ok || !ok2 {
		return xbson.False
	}
	return xbson.Boolean(sqlLike(s, p, coll))
}

// opBetween 求 BETWEEN，右边必须是恰好两项的数组，两端都算在内。
func opBetween(left, right *xbson.Value, coll xcoll.Collation) (*xbson.Value, error) {
	a, ok := right.AsArray()
	if !ok || a.Len() != 2 {
		return nil, errf("BETWEEN needs an array with 2 values")
	}
	lo, hi := a.At(0), a.At(1)
	in := left.Compare(lo, coll) >= 0 && left.Compare(hi, coll) <= 0
	return xbson.Boolean(in), nil
}

// opIn 求 IN。右边不是数组时退化成一次相等比较。
func opIn(left, right *xbson.Value, coll xcoll.Collation) *xbson.Value {
	a, ok := right.AsArray()
	if !ok {
		return xbson.Boolean(left.Compare(right, coll) == 0)
	}
	for _, it := range a.Items() {
		if left.Compare(it, coll) == 0 {
			return xbson.True
		}
	}
	return xbson.False
}

// sqlLike 按 SQL 的通配规则匹配：_ 配一个字符，% 配任意多个。
//
// 在 UTF-16 码元上走，配的是码元不是字符。回溯只记最近一个 %，
// 遇到失配就退回去让它多吃一个码元——模式里的 % 不会嵌套，一个回溯点够用。
//
// 大小写是否折叠由排序规则说了算：序数规则直接按码元比（必要时折一次大小写），
// 其余交给排序规则逐个字符比。代理对码元不参与折叠，只按相等比。
//
// 码元是边解 UTF-8 边按需产出的，不整份转成 UTF-16，匹配一次不分配。
func sqlLike(s, pattern string, coll xcoll.Collation) bool {
	fast := coll.Ordinal()
	// 折叠只在序数快路上用得着，而序数规则下它就是“忽略大小写”，不必现比一回。
	fold := fast && !coll.SameLengthWhenEqual()

	var i, j unitPos
	star, retry := unitPos{off: -1}, unitPos{}
	for i.off < len(s) {
		su, ni := nextUnit(s, i)
		pu, nj := uint16(0), j
		hasP := j.off < len(pattern)
		if hasP {
			pu, nj = nextUnit(pattern, j)
		}
		switch {
		case hasP && pu == '_':
			i, j = ni, nj
		case hasP && pu == '%':
			star = j
			retry = i
			j = nj
		case hasP && unitEqual(su, pu, coll, fold, fast):
			i, j = ni, nj
		case star.off >= 0:
			_, retry = nextUnit(s, retry)
			i = retry
			_, j = nextUnit(pattern, star)
		default:
			return false
		}
	}

	// '%' 是单字节；停在低位代理上时那个字节是四字节序列的首字节，不会误认。
	for j.off < len(pattern) && pattern[j.off] == '%' {
		j.off++
	}
	return j.off == len(pattern)
}

// unitPos 是 UTF-8 串里某个 UTF-16 码元的位置。
//
// off 是字符的起始字节；low 为真表示停在辅助平面字符的低位代理上。
type unitPos struct {
	off int
	low bool
}

// nextUnit 取 p 处的码元，并给出下一个码元的位置。p 不能在串尾。
//
// 非法 UTF-8 字节逐个当成 U+FFFD，与 utf16.Encode([]rune(s)) 的结果一致。
func nextUnit(s string, p unitPos) (uint16, unitPos) {
	if c := s[p.off]; c < utf8.RuneSelf {
		return uint16(c), unitPos{off: p.off + 1}
	}
	r, size := utf8.DecodeRuneInString(s[p.off:])
	if r < 0x10000 {
		return uint16(r), unitPos{off: p.off + size}
	}
	hi, lo := utf16.EncodeRune(r)
	if p.low {
		return uint16(lo), unitPos{off: p.off + size}
	}
	return uint16(hi), unitPos{off: p.off, low: true}
}

// unitEqual 比较两个 UTF-16 码元是否相等，fold 决定折不折大小写，fast 走序数快路。
func unitEqual(a, b uint16, coll xcoll.Collation, fold, fast bool) bool {
	if a == b {
		return true
	}
	if isSurrogateUnit(a) || isSurrogateUnit(b) {
		return false
	}
	if fast {
		if !fold {
			return false
		}
		return unicode.ToUpper(rune(a)) == unicode.ToUpper(rune(b))
	}
	return coll.Equal(string(rune(a)), string(rune(b)))
}

// isSurrogateUnit 判断一个码元是不是代理对的一半。
func isSurrogateUnit(u uint16) bool { return u >= 0xD800 && u <= 0xDFFF }

// opKind 区分加减乘三种数值运算，好让它们共用同一套类型提升逻辑。
type opKind uint8

const (
	opKindAdd opKind = iota
	opKindSub
	opKindMul
)

// opAdd 求加法。
//
// 有一边是字符串就变成拼接，另一边按它的文本形式拼上去。
// 时间加数字是加计时刻度。都不是就按数值相加；不是数则返回 Null。
func opAdd(left, right *xbson.Value) (*xbson.Value, error) {
	ls, lok := str(left)
	rs, rok := str(right)
	switch {
	case lok && rok:
		return xbson.String(ls + rs), nil
	case lok || rok:
		return xbson.String(stringOf(left) + stringOf(right)), nil
	}
	if t, n, ok := dateAndNumber(left, right); ok {
		return addTicks(t, n)
	}
	if isNumber(left) && isNumber(right) {
		return opKindAdd.numBinary(left, right)
	}
	return xbson.Null, nil
}

// opSub 求减法。
//
// 时间减数字是减计时刻度。**数字减时间也走同一条路**：结果是时间减去那个数字，
// 而不是反过来。
func opSub(left, right *xbson.Value) (*xbson.Value, error) {
	if left.Type() == xbson.TypeDateTime && isNumber(right) {
		n, err := int64Of(right)
		if err != nil {
			return nil, err
		}
		return addTicks(left, -n)
	}
	if isNumber(left) && right.Type() == xbson.TypeDateTime {
		n, err := int64Of(left)
		if err != nil {
			return nil, err
		}
		return addTicks(right, -n)
	}
	if isNumber(left) && isNumber(right) {
		return opKindSub.numBinary(left, right)
	}
	return xbson.Null, nil
}

// dateAndNumber 认出「一个时间加一个数字」这种组合，不论谁在左边。
func dateAndNumber(a, b *xbson.Value) (*xbson.Value, int64, bool) {
	var t, num *xbson.Value
	switch {
	case a.Type() == xbson.TypeDateTime && isNumber(b):
		t, num = a, b
	case isNumber(a) && b.Type() == xbson.TypeDateTime:
		t, num = b, a
	default:
		return nil, 0, false
	}
	n, err := int64Of(num)
	if err != nil {
		return nil, 0, false
	}
	return t, n, true
}

// addTicks 给一个时间加上 n 个计时刻度。
//
// 加的是**墙上时间**：先按时区偏移换算过去，加完再按原时区重新拼回来。
// 溢出与超出可表示范围都报错，不静默回绕。
func addTicks(v *xbson.Value, n int64) (*xbson.Value, error) {
	ticks, ok := v.Ticks()
	if !ok {
		return xbson.Null, nil
	}
	loc := v.Location()
	wall, err := wallTicks(ticks, loc)
	if err != nil {
		return nil, err
	}
	sum := wall + n

	if (n > 0 && sum < wall) || (n < 0 && sum > wall) {
		return nil, errf("datetime arithmetic overflowed")
	}
	t, err := xbin.TicksToTime(sum)
	if err != nil {
		return nil, errf("datetime arithmetic went out of range: %v", err)
	}

	return xbson.DateTime(time.Date(t.Year(), t.Month(), t.Day(),
		t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), loc))
}

// wallTicks 把绝对刻度换成 loc 上的墙上时间刻度，并夹到可表示的范围内。
func wallTicks(ticks int64, loc *time.Location) (int64, error) {
	t, err := xbin.TicksToTime(ticks)
	if err != nil {
		return 0, errf("datetime arithmetic went out of range: %v", err)
	}
	_, off := t.In(loc).Zone()
	wall := ticks + int64(off)*1000*xbin.TicksPerMillisecond
	return min(max(wall, xbin.MinTicks), xbin.MaxTicks), nil
}

// apply 对同类型的两个数按 k 算一次，加减乘共用一份代码。
func (k opKind) apply[T int32 | int64 | float64](x, y T) T {
	switch k {
	case opKindAdd:
		return x + y
	case opKindSub:
		return x - y
	default:
		return x * y
	}
}

// numBinary 算两个数的加减乘，按类型决定结果类型。
//
// 同类型就原地算，整型会静默回绕。类型不同则一律先升到十进制算，
// 再降回两者中较宽的那个类型——类型编号的大小正好就是宽窄次序。
func (k opKind) numBinary(a, b *xbson.Value) (*xbson.Value, error) {
	ta, tb := a.Type(), b.Type()
	if ta == tb {
		switch ta {
		case xbson.TypeInt32:
			x, _ := a.AsInt32()
			y, _ := b.AsInt32()
			return xbson.Int32(k.apply(x, y)), nil
		case xbson.TypeInt64:
			x, _ := a.AsInt64()
			y, _ := b.AsInt64()
			return xbson.Int64(k.apply(x, y)), nil
		case xbson.TypeDouble:
			x, _ := a.AsDouble()
			y, _ := b.AsDouble()
			return xbson.Double(k.apply(x, y)), nil
		case xbson.TypeDecimal:
			x, _ := a.AsDecimal()
			y, _ := b.AsDecimal()
			r, err := decFromXbin(x).binary(decFromXbin(y), k)
			if err != nil {
				return nil, err
			}
			return r.toValue()
		}
	}

	da, err := toDec(a)
	if err != nil {
		return nil, err
	}
	db, err := toDec(b)
	if err != nil {
		return nil, err
	}
	r, err := da.binary(db, k)
	if err != nil {
		return nil, err
	}
	return r.demote(max(ta, tb))
}

// binary 算两个十进制数的加减乘。
//
// 减法是加上取负。乘法把标度相加，超出上限就四舍五入回上限；
// 两个小数相乘的标度早已越界时直接归零——那个结果已经小到表示不出来。
func (d dec) binary(b dec, k opKind) (dec, error) {
	switch k {
	case opKindAdd:
		return d.add(b)
	case opKindSub:
		return d.add(b.neg())
	default:
		p := dec{m: new(big.Int).Mul(d.m, b.m), scale: d.scale + b.scale, negZero: d.negative() != b.negative()}

		if fitsIn32(d.m) && fitsIn32(b.m) {
			if p.scale > decMaxScale+decMaxInt64Scale {
				return decZero(), nil
			}
		} else if p.m.Sign() == 0 {
			return decZero(), nil
		}
		if p.scale > decMaxScale {
			p = p.rescale(decMaxScale)
		}
		return p.fit()
	}
}

// neg 取负，同时翻转零的符号位——十进制的零分正负。
func (d dec) neg() dec {
	return dec{m: new(big.Int).Neg(d.m), scale: d.scale, negZero: !d.negative()}
}

// numMod 求余。
//
// 同类型的整型和十进制走精确求余，除数为零时报错；其余情况一律转成
// 双精度走浮点求余，此时除数为零得到 NaN 而不报错。
func numMod(a, b *xbson.Value) (*xbson.Value, error) {
	ta, tb := a.Type(), b.Type()
	if ta == tb {
		switch ta {
		case xbson.TypeInt32:
			x, _ := a.AsInt32()
			y, _ := b.AsInt32()
			if y == 0 {
				return nil, errf("attempted to divide by zero")
			}
			return xbson.Int32(x % y), nil
		case xbson.TypeInt64:
			x, _ := a.AsInt64()
			y, _ := b.AsInt64()
			if y == 0 {
				return nil, errf("attempted to divide by zero")
			}
			return xbson.Int64(x % y), nil
		case xbson.TypeDecimal:
			x, _ := a.AsDecimal()
			y, _ := b.AsDecimal()
			r, err := decFromXbin(x).mod(decFromXbin(y))
			if err != nil {
				return nil, err
			}
			return r.toValue()
		}
	}
	x, ok := float64Of(a)
	y, ok2 := float64Of(b)
	if !ok || !ok2 {
		return xbson.Null, nil
	}
	return xbson.Double(math.Mod(x, y)), nil
}

// mod 求两个十进制数的余数，符号跟被除数走。
//
// 先把两边对齐到同一标度。被除数的绝对值更小时直接返回它自己。
func (d dec) mod(b dec) (dec, error) {
	if b.m.Sign() == 0 {
		return dec{}, errf("attempted to divide by zero")
	}

	s := max(d.scale, b.scale)
	x := d.rescale(s)
	y := b.rescale(s)
	nn := new(big.Int).Abs(x.m)
	dd := new(big.Int).Abs(y.m)
	if nn.Cmp(dd) < 0 {
		return d, nil
	}

	if decModOverflows(nn, dd) {
		return dec{}, errf("arithmetic operation resulted in an overflow")
	}

	return dec{m: new(big.Int).Rem(x.m, y.m), scale: s, negZero: d.negative()}.fit()
}
