package xbin

import (
	"errors"
	"fmt"
	"math/bits"
	"strings"
)

// Decimal 是 96 位尾数加十进制小数位数的定点数。
//
// 它算的是十进制，0.1+0.2 恰好等于 0.3——浮点做不到这一点。代价是范围小得多。
//
// **零可以带负号**：符号位与尾数是分开存的，尾数为零时符号位照样落到文件里。
// [Decimal.Signum] 与 [Decimal.String] 都不看它，但 [Decimal.Bytes] 会原样写出来，
// 所以按字节比较时 -0 与 0 是两份不同的东西。
type Decimal struct {
	// lo、mid、hi 拼成 96 位无符号尾数，从低到高。
	lo, mid, hi uint32

	// scale 是小数位数：真实值等于尾数除以 10^scale。
	scale uint8

	// neg 是符号位，与尾数无关，所以零也可以是负的。
	neg bool
}

// maxScale 是允许的最大小数位数。
const maxScale = 28

// maxDecimalDigits 是 96 位尾数最多能有几位十进制数字，用作拼串时的容量预估。
const maxDecimalDigits = 29

// errDecimalFlags 表示标志字里有不该置位的位。
var errDecimalFlags = errors.New("xbin: invalid decimal flags")

// DecimalFromParts 从各部分拼出一个十进制数。
func DecimalFromParts(lo, mid, hi uint32, scale uint8, neg bool) (Decimal, error) {
	if scale > maxScale {
		return Decimal{}, fmt.Errorf("xbin: decimal scale %d out of range 0..%d", scale, maxScale)
	}
	return Decimal{lo: lo, mid: mid, hi: hi, scale: scale, neg: neg}, nil
}

// DecimalFromBytes 从 16 字节读出一个十进制数。
//
// 布局：尾数三段各 4 字节（小端，从低到高），再 4 字节标志字——
// 第 16..23 位是小数位数，最高位是符号，其余位必须为零。
//
// 保留位置位时报错而不是忽略：那说明这不是一个十进制数，或者文件坏了。
func DecimalFromBytes(b []byte) (Decimal, error) {
	if len(b) < 16 {
		return Decimal{}, fmt.Errorf("xbin: decimal needs 16 bytes, got %d", len(b))
	}
	flags := Uint32LE(b[12:])
	if flags&^(0x00FF0000|0x80000000) != 0 {
		return Decimal{}, fmt.Errorf("%w: reserved bits set in %#08x", errDecimalFlags, flags)
	}
	scale := uint8(flags >> 16)
	if scale > maxScale {
		return Decimal{}, fmt.Errorf("%w: scale %d out of range 0..%d", errDecimalFlags, scale, maxScale)
	}
	return Decimal{
		lo:    Uint32LE(b),
		mid:   Uint32LE(b[4:]),
		hi:    Uint32LE(b[8:]),
		scale: scale,
		neg:   flags&0x80000000 != 0,
	}, nil
}

// Bytes 写出 16 字节表示。
func (d Decimal) Bytes() [16]byte {
	var out [16]byte
	d.PutBytes(out[:])
	return out
}

// PutBytes 把 16 字节表示写进 b。
//
// 开头那句下标是边界检查提示：越界要在写第一个字节之前就 panic，
// 而不是写了一半才发现。
func (d Decimal) PutBytes(b []byte) {
	_ = b[15]
	flags := uint32(d.scale) << 16
	if d.neg {
		flags |= 0x80000000
	}
	PutUint32LE(b, d.lo)
	PutUint32LE(b[4:], d.mid)
	PutUint32LE(b[8:], d.hi)
	PutUint32LE(b[12:], flags)
}

// Scale 返回小数位数。
func (d Decimal) Scale() uint8 { return d.scale }

// IsZero 只看尾数，不看符号位，所以 -0 也算零。
func (d Decimal) IsZero() bool { return d.lo == 0 && d.mid == 0 && d.hi == 0 }

// Signum 返回 -1、0、1。零一律给 0，即便符号位是负。
func (d Decimal) Signum() int {
	if d.IsZero() {
		return 0
	}
	if d.neg {
		return -1
	}
	return 1
}

// String 写成十进制字面量，小数位数原样保留。
//
// 0.10 与 0.1 是两个不同的串：小数位数是这个类型的一部分，抹掉它就等于
// 把「量到小数点后两位」和「量到一位」混为一谈。
//
// 零不带负号。
func (d Decimal) String() string {
	digits := d.mantissaDigits()
	var b strings.Builder
	b.Grow(len(digits) + 3)
	if d.neg && !d.IsZero() {
		b.WriteByte('-')
	}
	s := int(d.scale)
	switch {
	case s == 0:
		b.WriteString(digits)
	case len(digits) > s:
		b.WriteString(digits[:len(digits)-s])
		b.WriteByte('.')
		b.WriteString(digits[len(digits)-s:])
	default:
		b.WriteString("0.")
		b.WriteString(strings.Repeat("0", s-len(digits)))
		b.WriteString(digits)
	}
	return b.String()
}

// mantissaDigits 把 96 位尾数转成十进制数字串。
//
// 每次除以 10^9 取一段：一段正好塞进 uint32，段数因此最少，
// 而且每段拼串时补足 9 位就行。
func (d Decimal) mantissaDigits() string {
	if d.IsZero() {
		return "0"
	}

	var segs []uint32
	lo, mid, hi := d.lo, d.mid, d.hi
	for lo != 0 || mid != 0 || hi != 0 {
		var rem uint32
		hi, rem = bits.Div32(0, hi, 1e9)
		mid, rem = bits.Div32(rem, mid, 1e9)
		lo, rem = bits.Div32(rem, lo, 1e9)
		segs = append(segs, rem)
	}
	var b strings.Builder
	b.Grow(maxDecimalDigits)

	_, _ = fmt.Fprintf(&b, "%d", segs[len(segs)-1])
	for i := len(segs) - 2; i >= 0; i-- {
		_, _ = fmt.Fprintf(&b, "%09d", segs[i])
	}
	return b.String()
}

// Compare 按数值比较，小数位数不同也能比。
//
// 先比符号，再比对齐之后的尾数。
func (d Decimal) Compare(o Decimal) int {
	ds, os := d.Signum(), o.Signum()
	if ds != os {
		if ds < os {
			return -1
		}
		return 1
	}
	if ds == 0 {
		return 0
	}
	c := d.cmpScaled(o)
	if ds < 0 {
		return -c
	}
	return c
}

// Equal 按数值判等，所以 1.0 等于 1.00，-0 等于 0。
//
// 要区分这些，比 [Decimal.Bytes]。
func (d Decimal) Equal(o Decimal) bool { return d.Compare(o) == 0 }

// wide192 是 192 位无符号整数，从低位到高位。
//
// 比较时要把小数位数少的那个乘上去对齐，96 位放不下，
// 192 位则足够容纳最大尾数乘 10^28。
type wide192 [6]uint32

// cmpScaled 把两个尾数对齐到同一小数位数再比大小。
//
// 不缩小小数位数多的那个——那要做除法，会丢掉决定大小的末几位。
func (d Decimal) cmpScaled(o Decimal) int {
	x := wide192{d.lo, d.mid, d.hi}
	y := wide192{o.lo, o.mid, o.hi}
	switch {
	case d.scale < o.scale:
		x.mul10n(int(o.scale - d.scale))
	case o.scale < d.scale:
		y.mul10n(int(d.scale - o.scale))
	}
	for i := 5; i >= 0; i-- {
		if x[i] != y[i] {
			if x[i] < y[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// mul10n 原地乘以 10^n。
func (w *wide192) mul10n(n int) {
	for range n {
		var carry uint32
		for i := range w {
			hi, lo := bits.Mul32(w[i], 10)
			sum, c := bits.Add32(lo, carry, 0)
			w[i] = sum
			carry = hi + c
		}
	}
}
