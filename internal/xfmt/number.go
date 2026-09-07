// Package xfmt 是数字与日期时间的排版和解析。
//
// 两条主线：按格式串把值排成文本（标准写法与自定义模板各一套），
// 以及反过来把文本读回值。各语言的规则（分隔符、月名、次序、历法）
// 存在编译期嵌进来的几张表里，按语言标签逐级回退查找。
//
// 需要产出可交换文本的地方一律用「不随语言变」的那一套规则，
// 见 [Invariant] 与 [InvariantDate]：同一个值在任何机器上排出同样的串。
package xfmt

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// Kind 是数字的底层类型，决定按哪条路取值与舍入。
type Kind uint8

const (
	// Int32、Int64 是整数。
	Int32 Kind = iota

	Int64

	// Double 是双精度浮点。
	Double

	// Decimal 是十进制定点数。
	Decimal
)

// Num 是待排版的数字，四种底层类型合成一个。
//
// 排版要反复取整数部分、小数部分、有效位，各类型的取法不同；
// 合成一个类型之后，上层的排版逻辑只写一遍。
type Num struct {
	// Abs 是十进制数的尾数（绝对值），Sc 是它的小数位数。
	Abs *big.Int
	// I 是整数值，F 是浮点值，各自只在对应的 Kind 下有意义。
	I    int64
	F    float64
	Sc   int
	Kind Kind
	// Neg 是十进制数的符号位；其余类型的符号从值本身看。
	Neg bool
}

// IntNum 造一个整数。
func (k Kind) IntNum(v int64) Num { return Num{Kind: k, I: v} }

// FloatNum 造一个浮点数。
func FloatNum(v float64) Num { return Num{Kind: Double, F: v} }

// DecNum 造一个十进制数。
func DecNum(abs *big.Int, neg bool, scale int) Num {
	return Num{Kind: Decimal, Abs: abs, Neg: neg, Sc: scale}
}

// special 处理 NaN 与正负无穷，它们直接出符号串，不走排版。
func (n Num) special(nf *NumberFormat) (string, bool) {
	if n.Kind != Double {
		return "", false
	}
	switch {
	case math.IsNaN(n.F):
		return nf.NaNSymbol, true
	case math.IsInf(n.F, 1):
		return nf.PositiveInfinitySymbol, true
	case math.IsInf(n.F, -1):
		return nf.NegativeInfinitySymbol, true
	}
	return "", false
}

// negative 报告符号位。
//
// 浮点看符号位而不是比零：-0.0 要排成带负号的零。
func (n Num) negative() bool {
	switch n.Kind {
	case Double:
		return math.Signbit(n.F)
	case Decimal:
		return n.Neg
	default:
		return n.I < 0
	}
}

// absBig 把整数取成大整数的绝对值。
func (n Num) absBig() *big.Int {
	return new(big.Int).Abs(big.NewInt(n.I))
}

// fixed 按定点写法取出整数部分与恰好 frac 位的小数部分。
func (n Num) fixed(frac int) (ip, fp string) {
	switch n.Kind {
	case Double:
		s := strconv.FormatFloat(math.Abs(n.F), 'f', frac, 64)
		ip, fp, _ = strings.Cut(s, ".")
		return ip, fp
	case Decimal:
		return n.roundDec(frac)
	default:
		return n.absBig().String(), strings.Repeat("0", frac)
	}
}

// roundDec 把十进制数舍入到 frac 位小数。
//
// frac 比原有位数多就补零；少则做除法并按**四舍五入**进位
// （余数的两倍不小于除数就进）——不是银行家舍入，排版与算术在这一点上不同。
func (n Num) roundDec(frac int) (ip, fp string) {
	abs, scale := n.Abs, n.Sc
	q := new(big.Int).Set(abs)
	switch {
	case frac >= scale:
		q.Mul(q, pow10(frac-scale))
	default:
		d := pow10(scale - frac)
		r := new(big.Int)
		q.QuoRem(q, d, r)

		if r.Lsh(r, 1).Cmp(d) >= 0 {
			q.Add(q, big.NewInt(1))
		}
	}
	s := q.String()
	if frac == 0 {
		return s, ""
	}
	for len(s) <= frac {
		s = "0" + s
	}
	return s[:len(s)-frac], s[len(s)-frac:]
}

// pow10 返回 10^n。
func pow10(n int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

// digitsExp 取出有效数字串与十进制指数，满足 值 = 0.digits × 10^exp。
//
// 浮点先按 17 位有效数字展开：那是双精度能区分出的位数，再多就是
// 二进制近似泄漏出来的噪声。
func (n Num) digitsExp() (digits string, exp int) {
	switch n.Kind {
	case Double:
		if n.F == 0 {
			return "", 0
		}

		s := strconv.FormatFloat(math.Abs(n.F), 'e', 17, 64)
		mant, e, _ := strings.Cut(s, "e")
		ev, _ := strconv.Atoi(e)
		mant = strings.Replace(mant, ".", "", 1)
		mant = strings.TrimRight(mant, "0")
		if mant == "" {
			return "", 0
		}
		return mant, ev + 1
	case Decimal:
		return trimDigits(n.Abs.String(), -n.Sc)
	default:
		return trimDigits(n.absBig().String(), 0)
	}
}

// trimDigits 去掉首尾的零并算出指数。
func trimDigits(s string, shift int) (string, int) {
	s = strings.TrimLeft(s, "0")
	if s == "" {
		return "", 0
	}
	exp := len(s) + shift
	return strings.TrimRight(s, "0"), exp
}

// shortest 取出能唯一还原这个浮点值的最短数字串。
//
// 与 [Num.digitsExp] 的区别在这里：那个固定 17 位，这个交给标准库挑最短的，
// 所以 0.1 排出来是 "1" 而不是 "10000000000000001"。
func (n Num) shortest() (digits string, exp int) {
	if n.Kind != Double {
		return n.digitsExp()
	}
	if n.F == 0 {
		return "", 0
	}
	s := strconv.FormatFloat(math.Abs(n.F), 'e', -1, 64)
	mant, e, _ := strings.Cut(s, "e")
	ev, _ := strconv.Atoi(e)
	mant = strings.Replace(mant, ".", "", 1)
	mant = strings.TrimRight(mant, "0")
	if mant == "" {
		return "", 0
	}
	return mant, ev + 1
}

// sigDigits 是一串有效数字，只为挂舍入方法。
type sigDigits string

// roundSig 舍入到 sig 位有效数字，按四舍五入。
//
// 全是 9 时进位会多出一位，指数要跟着加一。
func (d sigDigits) roundSig(exp, sig int) (string, int) {
	if sig <= 0 || len(d) <= sig {
		return string(d), exp
	}
	keep := string(d[:sig])
	if d[sig] >= '5' {
		b := []byte(keep)
		i := len(b) - 1
		for ; i >= 0; i-- {
			if b[i] < '9' {
				b[i]++
				break
			}
			b[i] = '0'
		}
		if i < 0 {
			return "1" + string(b[:len(b)-1]), exp + 1
		}
		keep = string(b)
	}
	return strings.TrimRight(keep, "0"), exp
}

// sigRound 舍入到 sig 位有效数字。
func (n Num) sigRound(sig int) (digits string, exp int) {
	if n.Kind == Double {
		if n.F == 0 {
			return "", 0
		}
		s := strconv.FormatFloat(math.Abs(n.F), 'e', sig-1, 64)
		mant, e, _ := strings.Cut(s, "e")
		ev, _ := strconv.Atoi(e)
		mant = strings.Replace(mant, ".", "", 1)
		mant = strings.TrimRight(mant, "0")
		if mant == "" {
			return "", 0
		}
		return mant, ev + 1
	}
	d, e := n.digitsExp()
	return sigDigits(d).roundSig(e, sig)
}

// forCustom 把浮点先转成十进制数再交给自定义模板排版。
//
// 先舍到 15 位有效数字：模板要按位摆放数字，直接用浮点会把二进制近似的
// 尾巴摆出来。15 位是双精度能保证往返一致的位数。
func (n Num) forCustom() Num {
	if n.Kind != Double {
		return n
	}
	d, e := n.sigRound(15)
	abs, ok := new(big.Int).SetString(d, 10)
	if !ok {
		abs = new(big.Int)
	}
	return DecNum(abs, math.Signbit(n.F), len(d)-e)
}

// percentValue 把数乘以 100，得到百分号写法要显示的数。
//
// 浮点这条路不做乘法：按 frac+2 位小数展开之后，把小数点右移两位当成
// scale=frac 的十进制数——乘 100 会引入一次额外的浮点误差，而这里
// 只是挪一下小数点。
func (n Num) percentValue(frac int) Num {
	if n.Kind != Double {
		return n.times100()
	}
	s := strconv.FormatFloat(math.Abs(n.F), 'f', frac+2, 64)
	ip, fp, _ := strings.Cut(s, ".")
	abs, ok := new(big.Int).SetString(ip+fp, 10)
	if !ok {
		abs = new(big.Int)
	}
	return DecNum(abs, math.Signbit(n.F), frac)
}

// isInteger 报告底层类型是不是整数。
func (n Num) isInteger() bool { return n.Kind == Int32 || n.Kind == Int64 }

// group 从右往左按 sizes 给数字分组，中间插入分隔符。
//
// sizes 用完之后一直沿用最后一个；碰到 0 就停下，剩下的部分整块留在最左边——
// 那正是「前三位一组，之后不再分组」这类规则的表达方式。
func group(s string, sizes []int, sep string) string {
	if len(sizes) == 0 || sep == "" {
		return s
	}
	var parts []string
	i := len(s)
	for g := 0; i > 0; g++ {
		size := sizes[min(g, len(sizes)-1)]
		if size <= 0 {
			break
		}
		lo := max(i-size, 0)
		parts = append(parts, s[lo:i])
		i = lo
	}
	if i > 0 {
		parts = append(parts, s[:i])
	}

	for l, r := 0, len(parts)-1; l < r; l, r = l+1, r-1 {
		parts[l], parts[r] = parts[r], parts[l]
	}
	return strings.Join(parts, sep)
}

// errBadFormat 表示格式串不认识。
var errBadFormat = fmt.Errorf("xfmt: format specifier was invalid")
