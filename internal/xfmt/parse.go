package xfmt

import (
	"errors"
	"math"
	"strconv"
	"strings"
)

// ParseAny 按这套数字格式解析一个数字串，返回有效数字与十进制指数。
//
// 宽松地收：前后空白、正负号、括号负数、货币符号、分组分隔符都认，
// 科学计数也认。但整串必须用完，而且至少要有一位数字。
func (nf *NumberFormat) ParseAny(s string) (digits string, exp int, ok bool) {
	p := &anyParser{s: s, nf: nf}
	return p.run()
}

// anyParser 是 [NumberFormat.ParseAny] 的状态机。
//
// 那些 saw 开头的标志记着「这个成分已经出现过」：符号、小数点、货币符号
// 各自只许出现一次，重复出现就该判失败而不是接着读。
type anyParser struct {
	s  string
	nf *NumberFormat
	i  int

	sawSign     bool
	sawParen    bool
	sawDigit    bool
	sawDecimal  bool
	sawCurrency bool
	negative    bool

	currencyUsed bool

	intPart  []byte
	fracPart []byte
}

// run 依次读前缀、数字、指数、后缀。
//
// 收尾时三条都要成立：括号配对了、整串用完了、至少有一位数字。
// 少一条都判失败——留下没读完的尾巴意味着这不是一个数。
func (p *anyParser) run() (string, int, bool) {
	p.leading()
	exp := p.number()
	e, ok := p.exponent()
	if !ok {
		return "", 0, false
	}
	exp += e
	p.trailing()

	if p.sawParen || p.i != len(p.s) || !p.sawDigit {
		return "", 0, false
	}
	d := strings.TrimLeft(string(p.intPart), "0") + string(p.fracPart)
	if d == "" {
		d = "0"
	}
	if p.negative {
		d = "-" + d
	}
	return d, exp, true
}

// leading 读数字之前的空白、符号与货币符号。
//
// 符号之后还允不允许空白，取决于这套格式的负数写法，见 [anyParser.leadingWhiteOK]。
func (p *anyParser) leading() {
	for p.i < len(p.s) {
		switch {
		case isAsciiWhite(p.s[p.i]) && p.leadingWhiteOK():
			p.i++
		case !p.sawSign && p.matchNegative():
			p.sawSign, p.negative = true, true
		case !p.sawSign && p.match(p.nf.PositiveSign):
			p.sawSign = true
		case !p.sawSign && p.s[p.i] == '(':
			p.sawSign, p.sawParen, p.negative = true, true, true
			p.i++
		case !p.currencyUsed && p.match(p.nf.CurrencySymbol):
			p.sawCurrency, p.currencyUsed = true, true
		default:
			return
		}
	}
}

// leadingWhiteOK 报告此刻能不能吃掉一个空白。
//
// 符号之后一般不许再有空白（"- 1" 不是一个数），除非已经见过货币符号，
// 或者这套格式的负数写法本身就是「符号 空格 数字」。
func (p *anyParser) leadingWhiteOK() bool {
	return !p.sawSign || p.sawCurrency || p.nf.NumberNegativePattern == 2
}

// number 读数字、小数点与分组分隔符，返回小数位数的负值当指数。
//
// 分组分隔符只在小数点之前、且已经有过数字时才允许：不然 ",5" 也会被收下。
//
// 见过货币符号之后就只认货币那一套分隔符：两套混用会让 "1.234" 在
// 某些格式下有两种读法。
func (p *anyParser) number() int {
	for p.i < len(p.s) {
		switch c := p.s[p.i]; {
		case c >= '0' && c <= '9':
			p.sawDigit = true
			if p.sawDecimal {
				p.fracPart = append(p.fracPart, c)
			} else {
				p.intPart = append(p.intPart, c)
			}
			p.i++
		case !p.sawDecimal && p.match(p.nf.CurrencyDecimalSeparator):
			p.sawDecimal = true
		case !p.sawDecimal && !p.sawCurrency && p.match(p.nf.NumberDecimalSeparator):
			p.sawDecimal = true
		case p.sawDigit && !p.sawDecimal && p.match(p.nf.CurrencyGroupSeparator):
		case p.sawDigit && !p.sawDecimal && !p.sawCurrency && p.match(p.nf.NumberGroupSeparator):
		default:
			return -len(p.fracPart)
		}
	}
	return -len(p.fracPart)
}

// exponent 读科学计数的指数部分，没有就返回 0。
//
// e 后面不是数字时把位置退回去：那个 e 可能是后缀的一部分，
// 不该在这里吃掉。
//
// 指数累加到 2^20 就不再增大：再大也只会溢出成正负无穷，
// 继续累加反而可能整数回绕。
func (p *anyParser) exponent() (int, bool) {
	if !p.sawDigit || p.i >= len(p.s) || (p.s[p.i] != 'e' && p.s[p.i] != 'E') {
		return 0, true
	}
	save := p.i
	p.i++
	neg := false
	switch {
	case p.matchNegative():
		neg = true
	case p.match(p.nf.PositiveSign):
	}
	if p.i >= len(p.s) || p.s[p.i] < '0' || p.s[p.i] > '9' {
		p.i = save
		return 0, true
	}
	e := 0
	for p.i < len(p.s) && p.s[p.i] >= '0' && p.s[p.i] <= '9' {
		if e < 1<<20 {
			e = e*10 + int(p.s[p.i]-'0')
		}
		p.i++
	}
	if neg {
		e = -e
	}
	return e, true
}

// trailing 读数字之后的空白、符号、右括号与货币符号。
func (p *anyParser) trailing() {
	for p.i < len(p.s) {
		switch {
		case isAsciiWhite(p.s[p.i]):
			p.i++
		case !p.sawSign && p.matchNegative():
			p.sawSign, p.negative = true, true
		case !p.sawSign && p.match(p.nf.PositiveSign):
			p.sawSign = true
		case p.sawParen && p.s[p.i] == ')':
			p.sawParen = false
			p.i++
		case !p.currencyUsed && p.match(p.nf.CurrencySymbol):
			p.sawCurrency, p.currencyUsed = true, true
		default:
			return
		}
	}
}

// match 尝试吃掉一个字面量，成功才前进。
//
// 不间断空格与普通空格互相认：格式数据里用的是哪一个，输入里往往是另一个。
func (p *anyParser) match(want string) bool {
	if want == "" {
		return false
	}
	i := p.i
	for _, r := range want {
		if i >= len(p.s) {
			return false
		}
		var n int
		switch {
		case strings.HasPrefix(p.s[i:], string(r)):
			n = len(string(r))
		case (r == ' ' || r == ' ') && p.s[i] == ' ':
			n = 1
		default:
			return false
		}
		i += n
	}
	p.i = i
	return true
}

// hyphenLike 是各种看起来像减号的字符。
var hyphenLike = map[rune]bool{
	'‒': true, '⁻': true, '₋': true,
	'−': true, '➖': true, '﹣': true, '－': true,
}

// matchNegative 尝试吃掉一个负号。
//
// 这套格式的负号是某个类减号字符时，也接受 ASCII 的 `-`：
// 键盘上打不出那些字符，而输入里出现的多半就是 ASCII 那个。
func (p *anyParser) matchNegative() bool {
	if p.match(p.nf.NegativeSign) {
		return true
	}
	r := []rune(p.nf.NegativeSign)
	if len(r) == 1 && hyphenLike[r[0]] && p.i < len(p.s) && p.s[p.i] == '-' {
		p.i++
		return true
	}
	return false
}

// isAsciiWhite 报告一个字节是不是 ASCII 空白。
func isAsciiWhite(c byte) bool { return c == ' ' || (c >= '\t' && c <= '\r') }

// ParseFloatAny 按这套格式解析成双精度浮点。
//
// 解析不出来时再试 NaN 与正负无穷的符号串。
func (nf *NumberFormat) ParseFloatAny(s string) (float64, bool) {
	digits, exp, ok := nf.ParseAny(s)
	if !ok {
		return nf.specialFloat(s)
	}
	return sigDigits(digits).assemble(exp)
}

// specialFloat 识别 NaN 与正负无穷的符号串，不区分大小写。
//
// 符号串前面还可以带一个正负号：负号加正无穷的符号串读成负无穷，
// 而负号加 NaN 仍然是 NaN。
func (nf *NumberFormat) specialFloat(s string) (float64, bool) {
	t := strings.TrimFunc(s, isUnicodeWhite)
	switch {
	case strings.EqualFold(t, nf.PositiveInfinitySymbol):
		return inf(1), true
	case strings.EqualFold(t, nf.NegativeInfinitySymbol):
		return inf(-1), true
	case strings.EqualFold(t, nf.NaNSymbol):
		return nan(), true
	}
	if rest, cut := strings.CutPrefix(t, nf.PositiveSign); cut && nf.PositiveSign != "" {
		switch {
		case strings.EqualFold(rest, nf.PositiveInfinitySymbol):
			return inf(1), true
		case strings.EqualFold(rest, nf.NaNSymbol):
			return nan(), true
		}
	}
	if rest, cut := strings.CutPrefix(t, nf.NegativeSign); cut && nf.NegativeSign != "" {
		switch {
		case strings.EqualFold(rest, nf.PositiveInfinitySymbol):
			return inf(-1), true
		case strings.EqualFold(rest, nf.NaNSymbol):
			return nan(), true
		}
	}
	return 0, false
}

// isUnicodeWhite 报告一个字符是不是空白，各种宽度的空格都算。
func isUnicodeWhite(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\v', '\f', '\r', 0x85, 0xa0,
		0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// assemble 把有效数字与指数拼成浮点数。
//
// 溢出不算失败：那时标准解析已经给出正负无穷或零，那正是想要的结果。
func (d sigDigits) assemble(exp int) (float64, bool) {
	f, err := strconv.ParseFloat(string(d)+"e"+strconv.Itoa(exp), 64)
	if err != nil {
		var ne *strconv.NumError
		if !errors.As(err, &ne) || ne.Err != strconv.ErrRange {
			return 0, false
		}
	}
	return f, true
}

// inf 返回带符号的无穷。
func inf(sign int) float64 { return math.Inf(sign) }

// nan 返回**符号位为 1** 的 NaN。
//
// 不用 math.NaN()：那个的符号位是 0。两者做算术没有区别，但写进文件的
// 八个字节不同，而 NaN 的字节形态是格式的一部分。
func nan() float64 { return math.Float64frombits(0xFFF8000000000000) }
