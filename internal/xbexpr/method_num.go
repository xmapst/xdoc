package xbexpr

import (
	"math"
	"math/big"
	"strconv"
	"strings"

	"github.com/xmapst/xdoc/internal/xbin"
	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xfmt"
)

// dec 是十进制值在算术过程中的形态：一个大整数尾数配一个十进制标度。
//
// 值等于 m 乘以 10 的负 scale 次方。用大整数是为了中间结果能暂时越界，
// 最后再收回到能存下的范围（见 [dec.fit]）。
type dec struct {
	// m 是尾数，带符号。
	m *big.Int
	// scale 是小数位数。
	scale int32

	// negZero 记住零的符号。
	//
	// 尾数为零时看不出正负，可这个类型区分正零和负零，只能另存一位。
	// 尾数非零时这一位不作数，符号以尾数为准。
	negZero bool
}

// negative 判断是不是负数，零看符号位。
func (d dec) negative() bool {
	if d.m != nil && d.m.Sign() != 0 {
		return d.m.Sign() < 0
	}
	return d.negZero
}

// decMaxScale 是能存下的最大小数位数。
const decMaxScale = 28

// decMaxInt64Scale 是 64 位整数的十进制位数，乘法判溢出时用它放宽一档。
const decMaxInt64Scale = 19

// decMaxDigits 是能存下的最大十进制位数。
const decMaxDigits = 29

// decMaxMantissa 是尾数上限：96 位全一。存储格式给尾数的就是这 96 位。
var decMaxMantissa = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 96), big.NewInt(1))

// bigTen 是常用的大整数 10。
var bigTen = big.NewInt(10)

// bigWord32 是取低 32 位用的掩码。
var bigWord32 = big.NewInt(0xFFFFFFFF)

// fitsIn32 判断尾数是否不超过 32 位。
func fitsIn32(m *big.Int) bool { return m.BitLen() <= 32 }

// decZero 返回正零。
func decZero() dec { return dec{m: new(big.Int)} }

// decFromXbin 把存储形态转成算术形态。
//
// 存储里的 96 位尾数按小端存放，这里要翻成大端才喂得进大整数。
// 符号位单独取出来，好保住负零。
func decFromXbin(d xbin.Decimal) dec {
	b := d.Bytes()

	be := [12]byte{
		b[11], b[10], b[9], b[8],
		b[7], b[6], b[5], b[4],
		b[3], b[2], b[1], b[0],
	}
	m := new(big.Int).SetBytes(be[:])
	if d.Signum() < 0 {
		m.Neg(m)
	}

	return dec{m: m, scale: int32(d.Scale()), negZero: b[15]&0x80 != 0}
}

// toXbin 转回存储形态；尾数超过 96 位或标度越界就报溢出。
func (d dec) toXbin() (xbin.Decimal, error) {
	if d.m == nil {
		d.m = new(big.Int)
	}
	abs := new(big.Int).Abs(d.m)
	if abs.Cmp(decMaxMantissa) > 0 || d.scale < 0 || d.scale > decMaxScale {
		return xbin.Decimal{}, errf("decimal overflow")
	}
	var buf [12]byte
	abs.FillBytes(buf[:])
	lo := uint32(buf[11]) | uint32(buf[10])<<8 | uint32(buf[9])<<16 | uint32(buf[8])<<24
	mid := uint32(buf[7]) | uint32(buf[6])<<8 | uint32(buf[5])<<16 | uint32(buf[4])<<24
	hi := uint32(buf[3]) | uint32(buf[2])<<8 | uint32(buf[1])<<16 | uint32(buf[0])<<24
	return xbin.DecimalFromParts(lo, mid, hi, uint8(d.scale), d.negative())
}

// toValue 转成一个十进制值。
func (d dec) toValue() (*xbson.Value, error) {
	x, err := d.toXbin()
	if err != nil {
		return nil, err
	}
	return xbson.Decimal(x), nil
}

// decFromInt64 由整数造一个标度为零的十进制值。
func decFromInt64(n int64) dec { return dec{m: big.NewInt(n)} }

// decFromFloat64 把双精度转成十进制。
//
// 走**十五位有效数字**的科学计数法文本中转，而不是取二进制的精确值——
// 后者会把 0.1 这种数展开成一长串。NaN 和无穷报错。
func decFromFloat64(f float64) (dec, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return dec{}, errf("cannot convert %v to decimal", f)
	}

	s := strconv.FormatFloat(f, 'e', 14, 64)
	mant, exp, ok := splitSci(s)
	if !ok {
		return dec{}, errf("cannot convert %v to decimal", f)
	}

	mant, exp = stripTrailingZeros(mant, exp)
	d, err := decFromDigits(mant, exp)
	if err != nil {
		return dec{}, err
	}
	if d.m.Sign() == 0 {
		return decZero(), nil
	}
	return d, nil
}

// stripTrailingZeros 去掉尾数末尾的零，指数相应增大。全零时归成标准的零。
func stripTrailingZeros(mant string, exp int) (string, int) {
	neg := strings.HasPrefix(mant, "-")
	body := strings.TrimPrefix(mant, "-")
	for len(body) > 1 && strings.HasSuffix(body, "0") {
		body = body[:len(body)-1]
		exp++
	}
	if body == "0" {
		return "0", 0
	}
	if neg {
		body = "-" + body
	}
	return body, exp
}

// splitSci 把科学计数法文本拆成整数尾数与十进制指数，小数点的位数并进指数。
func splitSci(s string) (mant string, exp int, ok bool) {
	i := strings.IndexAny(s, "eE")
	if i < 0 {
		return "", 0, false
	}
	e, err := strconv.Atoi(s[i+1:])
	if err != nil {
		return "", 0, false
	}
	head := s[:i]
	neg := strings.HasPrefix(head, "-")
	head = strings.TrimLeft(head, "+-")
	if j := strings.IndexByte(head, '.'); j >= 0 {
		frac := head[j+1:]
		head = head[:j] + frac
		e -= len(frac)
	}
	if neg {
		head = "-" + head
	}
	return head, e, true
}

// decFromDigits 由数字串与十进制指数造一个十进制值。
//
// 零单独处理，为的是保住符号和标度。指数为正时把尾数乘上去，越界报溢出；
// 指数负得太多时结果小到表示不出来，归成标度拉满的零；其余交给 [fitDigits]。
func decFromDigits(mant string, exp int) (dec, error) {
	m, ok := new(big.Int).SetString(mant, 10)
	if !ok {
		return dec{}, errf("invalid decimal digits %q", mant)
	}

	if m.Sign() == 0 {
		neg := strings.HasPrefix(mant, "-")
		switch {
		case exp >= 0:
			return dec{m: m, negZero: neg}, nil
		case -exp <= decMaxScale:
			return dec{m: m, scale: int32(-exp), negZero: neg}, nil
		default:
			return dec{m: m, scale: decMaxScale, negZero: neg}, nil
		}
	}
	if exp > decMaxDigits {
		return dec{}, errf("decimal overflow")
	}
	switch {
	case exp >= 0:
		m.Mul(m, pow10(exp))
		if new(big.Int).Abs(m).Cmp(decMaxMantissa) > 0 {
			return dec{}, errf("decimal overflow")
		}
		return dec{m: m}, nil
	case exp < -(decMaxScale + len(mant) + 1):
		return dec{m: new(big.Int), scale: decMaxScale, negZero: m.Sign() < 0}, nil
	default:
		return fitDigits(m, int32(-exp))
	}
}

// fitDigits 逐档降低标度，直到尾数装得进 96 位。
//
// 从原标度往下试，每降一档就四舍六入五成双一次；一路降到零仍装不下才报溢出。
func fitDigits(m *big.Int, scale int32) (dec, error) {
	d := dec{m: m, scale: scale}
	for k := int32(0); k <= scale; k++ {
		target := scale - k
		if target > decMaxScale {
			continue
		}
		r := d.rescale(target)
		if new(big.Int).Abs(r.m).Cmp(decMaxMantissa) <= 0 {
			return r, nil
		}
	}
	return dec{}, errf("decimal overflow")
}

// pow10 求 10 的 n 次方；n 不为正时返回 1。
func pow10(n int) *big.Int {
	if n <= 0 {
		return big.NewInt(1)
	}
	return new(big.Int).Exp(bigTen, big.NewInt(int64(n)), nil)
}

// fit 反复降标度直到尾数装得下；标度已经到零还装不下就报溢出。
func (d dec) fit() (dec, error) {
	for d.scale > 0 && new(big.Int).Abs(d.m).Cmp(decMaxMantissa) > 0 {
		d = d.rescale(d.scale - 1)
	}
	if new(big.Int).Abs(d.m).Cmp(decMaxMantissa) > 0 {
		return dec{}, errf("decimal overflow")
	}
	return d, nil
}

// decModOverflows 预判求余的中间结果会不会撑破 96 位。
//
// 按 32 位一段做长除法的模拟：把除数左移到顶满 96 位，再逐段把被除数移进余数
// 并取模，一旦余数的高位越过除数的高 32 位就说明会溢出。
// 除数不足 65 位或超过 96 位时不可能溢出，直接放行。
func decModOverflows(n, d *big.Int) bool {
	db := d.BitLen()

	if db <= 64 || db > 96 {
		return false
	}

	if n.BitLen()-db < 32 {
		return false
	}

	sh := uint(96 - db)
	dn := new(big.Int).Lsh(d, sh)
	nn := new(big.Int).Lsh(n, sh)
	top := new(big.Int).Rsh(dn, 64)

	r := new(big.Int)
	hi := new(big.Int)
	word := new(big.Int)
	for i := (nn.BitLen()+31)/32 - 1; i >= 0; i-- {
		if hi.Rsh(r, 64).Cmp(top) >= 0 {
			return true
		}
		word.And(word.Rsh(nn, uint(32*i)), bigWord32)
		r.Or(r.Lsh(r, 32), word)
		r.Mod(r, dn)
	}
	return false
}

// rescale 把标度改成 target。
//
// 放大标度是乘幂，不丢精度；缩小标度要舍入，取**四舍六入五成双**。
func (d dec) rescale(target int32) dec {
	if target >= d.scale {
		m := new(big.Int).Mul(d.m, pow10(int(target-d.scale)))
		return dec{m: m, scale: target, negZero: d.negative()}
	}
	div := pow10(int(d.scale - target))
	q, r := new(big.Int).QuoRem(d.m, div, new(big.Int))
	if r.Sign() != 0 {
		twice := new(big.Int).Abs(r)
		twice.Lsh(twice, 1)
		c := twice.Cmp(div)
		up := c > 0 || (c == 0 && q.Bit(0) == 1)
		if up {
			if d.m.Sign() < 0 {
				q.Sub(q, big.NewInt(1))
			} else {
				q.Add(q, big.NewInt(1))
			}
		}
	}

	return dec{m: q, scale: target, negZero: d.negative()}
}

// add 相加：先对齐到较大的那个标度，再相加，最后收回可存范围。
func (d dec) add(b dec) (dec, error) {
	s := max(d.scale, b.scale)
	x := d.rescale(s)
	y := b.rescale(s)
	return dec{m: new(big.Int).Add(x.m, y.m), scale: s, negZero: d.addZeroSign(b)}.fit()
}

// addZeroSign 定出相加结果为零时该记哪个符号。
//
// 标度小的那边是零时跟另一边走；否则跟被加数走。
func (d dec) addZeroSign(b dec) bool {
	switch {
	case d.scale < b.scale && d.m.Sign() == 0:
		return b.negative()
	case d.scale > b.scale && b.m.Sign() != 0:
		return b.negative()
	default:
		return d.negative()
	}
}

// div 相除。
//
// 商的标度从两者之差起步，除不尽就一位一位往下算，直到标度到顶或者尾数装不下。
// 最后的余数按四舍六入五成双进位。除不尽的结果会去掉末尾多余的零
// （见 [dec.unscale]），除得尽的则保留标度。
func (d dec) div(b dec) (dec, error) {
	if b.m.Sign() == 0 {
		return dec{}, errf("attempted to divide by zero")
	}
	s := max(d.scale-b.scale, 0)

	num := new(big.Int).Abs(d.m)
	den := new(big.Int).Abs(b.m)
	shift := int(b.scale+s) - int(d.scale)
	if shift >= 0 {
		num.Mul(num, pow10(shift))
	} else {
		den.Mul(den, pow10(-shift))
	}
	q, r := new(big.Int).QuoRem(num, den, new(big.Int))
	exact := r.Sign() == 0
	for r.Sign() != 0 && s < decMaxScale {
		next := new(big.Int).Mul(q, bigTen)

		scaled := new(big.Int).Mul(r, bigTen)
		add, rem := new(big.Int).QuoRem(scaled, den, new(big.Int))
		next.Add(next, add)
		if next.Cmp(decMaxMantissa) > 0 {
			break
		}
		q, r = next, rem
		s++
	}

	if r.Sign() != 0 {
		twice := new(big.Int).Lsh(r, 1)
		c := twice.Cmp(den)
		if c > 0 || (c == 0 && q.Bit(0) == 1) {
			q.Add(q, big.NewInt(1))
		}
	}
	if d.m.Sign() < 0 != (b.m.Sign() < 0) {
		q.Neg(q)
	}

	out, err := dec{m: q, scale: s, negZero: d.negative() != b.negative()}.fit()
	if err != nil || exact {
		return out, err
	}
	return out.unscale(), nil
}

// unscale 去掉末尾的零，相应降低标度；零归成标度为零，符号保留。
func (d dec) unscale() dec {
	if d.m.Sign() == 0 {
		return dec{m: new(big.Int), negZero: d.negZero}
	}
	m, r := new(big.Int).Set(d.m), new(big.Int)
	for d.scale > 0 {
		q := new(big.Int)
		q.QuoRem(m, bigTen, r)
		if r.Sign() != 0 {
			break
		}
		m, d.scale = q, d.scale-1
	}
	return dec{m: m, scale: d.scale, negZero: d.negZero}
}

// abs 取绝对值，顺带清掉负零的符号位。
func (d dec) abs() dec { return dec{m: new(big.Int).Abs(d.m), scale: d.scale} }

// round 舍到指定小数位数；位数为负或已经不多于当前标度时原样返回。
func (d dec) round(digits int) dec {
	if digits < 0 || int32(digits) >= d.scale {
		return d
	}
	return d.rescale(int32(digits))
}

// toFloat64 转成双精度，中间用高精度有理数过渡以免二次舍入。
func (d dec) toFloat64() float64 {
	f, _ := new(big.Float).SetPrec(200).SetRat(d.toRat()).Float64()
	return f
}

// toRat 转成精确的有理数。
func (d dec) toRat() *big.Rat {
	return new(big.Rat).SetFrac(new(big.Int).Set(d.m), pow10(int(d.scale)))
}

// toInt64 舍到整数再转成 64 位整数，越界报错。舍入取四舍六入五成双。
func (d dec) toInt64() (int64, error) {
	r := d.rescale(0)
	if !r.m.IsInt64() {
		return 0, errf("value is out of range for a 64-bit integer")
	}
	return r.m.Int64(), nil
}

// decParse 按不变语言环境解析一个十进制字面量。
func decParse(s string) (dec, bool) {
	return decParseIn(s, xfmt.Invariant())
}

// decParseIn 按指定数字格式解析十进制文本，解析不出或超出范围都返回 false。
func decParseIn(s string, nf *xfmt.NumberFormat) (dec, bool) {
	mant, exp, ok := nf.ParseAny(s)
	if !ok {
		return dec{}, false
	}
	d, err := decFromDigits(mant, exp)
	if err != nil {
		return dec{}, false
	}
	return d, true
}

// allDigits 判断串是否全为 ASCII 数字；空串算真。
func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// parseFloatAny 按指定数字格式解析一个浮点数。
func parseFloatAny(s string, nf *xfmt.NumberFormat) (float64, bool) {
	return nf.ParseFloatAny(s)
}

// parseIntegerText 解析一个纯整数文本，允许前后空白和正负号。
//
// 不认小数点、指数、千位分隔符——那些交给别的解析路径。
func parseIntegerText(s string) (*big.Int, bool) {
	t := strings.TrimSpace(s)
	t = strings.TrimPrefix(t, "+")
	digits := strings.TrimPrefix(t, "-")
	if digits == "" || !allDigits(digits) {
		return nil, false
	}
	n, ok := new(big.Int).SetString(t, 10)
	return n, ok
}

// floatToInt64 把双精度舍成 64 位整数，取四舍六入五成双；NaN、无穷、越界都报错。
func floatToInt64(f float64) (int64, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, errf("cannot convert %v to an integer", f)
	}
	r := math.RoundToEven(f)
	if r < -9223372036854775808 || r >= 9223372036854775808 {
		return 0, errf("value %v is out of range for a 64-bit integer", f)
	}
	return int64(r), nil
}

// numDiv 求商。
//
// 任一边是十进制就走精确的十进制除法，除数为零时报错；
// 否则一律转成双精度相除，此时除以零得到无穷或 NaN 而不报错。
func numDiv(a, b *xbson.Value) (*xbson.Value, error) {
	if a.Type() == xbson.TypeDecimal || b.Type() == xbson.TypeDecimal {
		da, err := toDec(a)
		if err != nil {
			return nil, err
		}
		db, err := toDec(b)
		if err != nil {
			return nil, err
		}
		q, err := da.div(db)
		if err != nil {
			return nil, err
		}
		return q.toValue()
	}
	x, ok := float64Of(a)
	y, ok2 := float64Of(b)
	if !ok || !ok2 {
		return xbson.Null, nil
	}
	return xbson.Double(x / y), nil
}

// toDec 把任意数值转成十进制形态；不是数就报错。
func toDec(v *xbson.Value) (dec, error) {
	switch v.Type() {
	case xbson.TypeInt32:
		n, _ := v.AsInt32()
		return decFromInt64(int64(n)), nil
	case xbson.TypeInt64:
		n, _ := v.AsInt64()
		return decFromInt64(n), nil
	case xbson.TypeDouble:
		f, _ := v.AsDouble()
		return decFromFloat64(f)
	case xbson.TypeDecimal:
		d, _ := v.AsDecimal()
		return decFromXbin(d), nil
	default:
		return dec{}, errf("value of type %s is not a number", v.Type())
	}
}

// demote 把十进制结果降回类型 t。
//
// t 取的是两个操作数里较宽的那个——类型编号本身就是宽窄次序，
// Int32 到 Decimal 依次变宽。降到整数时越界会报错。
func (d dec) demote(t xbson.Type) (*xbson.Value, error) {
	switch t {
	case xbson.TypeInt64:
		n, err := d.toInt64()
		if err != nil {
			return nil, err
		}
		return xbson.Int64(n), nil
	case xbson.TypeDouble:
		return xbson.Double(d.toFloat64()), nil
	default:
		return d.toValue()
	}
}
