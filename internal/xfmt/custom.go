package xfmt

import (
	"math"
	"math/big"
	"strings"
	"unicode/utf8"
)

const (
	// 自定义模板里的记号种类：原样输出的字面量、整数位、小数位、小数点、
	// 科学计数标记。
	tokLiteral = iota
	tokIntDigit
	tokFracDigit
	tokPoint
	tokSci
)

// token 是模板解析出来的一个记号。
type token struct {
	// text 是字面量的内容。
	text string
	// kind 是记号种类。
	kind int
	// expDigits 是科学计数里指数部分要写几位。
	expDigits int
	// zero 表示这一位写的是 `0`（必留）而不是 `#`（可省）。
	zero bool
	// expPlus 表示正指数也要写加号，expUpper 表示指数标记用大写 E。
	expPlus  bool
	expUpper bool
}

// section 是模板的一节（用 `;` 分开的那种）。
//
// 解析一次算出各位置的下标与整体属性，排版时就不必再扫模板。
type section struct {
	// tokens 是这一节的全部记号，按出现顺序。
	tokens []token
	// ints、fracs 是整数位与小数位在 tokens 里的下标。
	ints  []int
	fracs []int
	// group 表示要按分组分隔符分组，commas 是末尾的逗号个数（每个表示除以一千）。
	group  bool
	commas int
	// exp10 是百分号与千分号带来的十的幂次。
	exp10 int
	// sci 是科学计数记号的下标，-1 表示没有。
	sci int
	// minInt 是整数部分至少要写几位（由最左边那个 `0` 决定）。
	minInt int
}

// custom 按自定义模板排版一个数。
//
// 模板最多四节，依次是正数、负数、零、空值；缺哪一节就退回第一节。
// 负号只有在真的落到第一节时才补——第二节自己就带着负号的写法。
//
// 一个不巧的情形：非零的数按第一节排出来全是零（比如 0.001 遇上 `0.#`），
// 这时改用零那一节重排。不然会得到一个看起来是零、却又带着负号的输出。
func (n Num) custom(format string, nf *NumberFormat) (string, error) {
	secs, err := splitSections(format)
	if err != nil {
		return "", err
	}

	v := n.forCustom()
	neg := v.negative()

	isFloat := n.Kind == Double

	want := 0
	switch {
	case v.isZero():
		want = 2
	case neg:
		want = 1
	}
	idx := findSection(secs, want)
	body, zeroed := parseSection(secs[idx]).render(v.abs(), nf)
	if zeroed && !v.isZero() {
		if j := findSection(secs, 2); j != idx {
			idx = j
			body, _ = parseSection(secs[idx]).render(zeroNum(), nf)
		}
	}

	if neg && idx == 0 && (!zeroed || (isFloat && body != "")) {
		return nf.NegativeSign + body, nil
	}
	return body, nil
}

// zeroNum 返回十进制的零。
func zeroNum() Num { return DecNum(new(big.Int), false, 0) }

// findSection 挑一节，想要的那节不存在或为空时退回第一节。
func findSection(secs []string, want int) int {
	if want <= 0 || want >= len(secs) || secs[want] == "" {
		return 0
	}
	return want
}

// splitSections 按 `;` 把模板拆成几节。
//
// 转义与引号里的 `;` 不算分隔符。超过四节报错：多出来的那节没有含义，
// 静默丢掉会让人以为它生效了。
func splitSections(format string) ([]string, error) {
	var secs []string
	var cur strings.Builder
	for i := 0; i < len(format); i++ {
		c := format[i]
		switch c {
		case '\\':
			cur.WriteByte(c)
			if i+1 < len(format) {
				i++
				cur.WriteByte(format[i])
			}
		case '\'', '"':
			cur.WriteByte(c)
			for i++; i < len(format); i++ {
				cur.WriteByte(format[i])
				if format[i] == c {
					break
				}
			}
		case ';':
			secs = append(secs, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	secs = append(secs, cur.String())
	if len(secs) > 4 {
		return nil, errBadFormat
	}
	return secs, nil
}

// isZero 报告这个数是不是零。
func (n Num) isZero() bool {
	switch n.Kind {
	case Double:
		return n.F == 0
	case Decimal:
		return n.Abs.Sign() == 0
	default:
		return n.I == 0
	}
}

// abs 取绝对值。整数与十进制都转成十进制，好走同一条排版路径。
func (n Num) abs() Num {
	switch n.Kind {
	case Double:
		return FloatNum(math.Abs(n.F))
	case Decimal:
		return DecNum(n.Abs, false, n.Sc)
	default:
		return DecNum(n.absBig(), false, 0)
	}
}

// scaled 乘以 10^k。
//
// 十进制这条路只挪小数位数，不做乘法：小数位数够减时直接减，
// 不够才真的乘，这样不引入舍入误差。
func (n Num) scaled(k int) Num {
	if k == 0 {
		return n
	}
	if n.Kind == Double {
		return FloatNum(n.F * math.Pow(10, float64(k)))
	}
	abs, neg, sc := n.Abs, n.Neg, n.Sc
	if n.Kind != Decimal {
		abs, neg, sc = n.absBig(), n.I < 0, 0
	}
	if k <= sc {
		return DecNum(abs, neg, sc-k)
	}
	return DecNum(new(big.Int).Mul(abs, pow10(k-sc)), neg, 0)
}

// parseSection 解析模板的一节。
//
// 逗号的含义取决于它的位置：出现在整数位之间表示要分组，
// 出现在全部整数位之后表示每个除以一千，出现在最前面则没有意义。
// 所以先把每个逗号左边有几个整数位记下来，扫完再一起判定。
func parseSection(src string) section {
	s := section{sci: -1}
	seenPoint := false

	var commaRuns []int

	add := func(t token) { s.tokens = append(s.tokens, t) }

	for i := 0; i < len(src); i++ {
		c := src[i]
		switch c {
		case '0', '#':
			t := token{kind: tokIntDigit, zero: c == '0'}
			if seenPoint {
				t.kind = tokFracDigit
			}
			add(t)
			idx := len(s.tokens) - 1
			if seenPoint {
				s.fracs = append(s.fracs, idx)
			} else {
				s.ints = append(s.ints, idx)
			}
		case '.':
			if seenPoint {
				continue
			}
			seenPoint = true
			add(token{kind: tokPoint})
		case ',':
			commaRuns = append(commaRuns, len(s.ints))
		case '%':
			s.exp10 += 2
			add(token{kind: tokLiteral, text: "%"})
		case '\\':
			if i+1 < len(src) {
				_, w := utf8.DecodeRuneInString(src[i+1:])
				add(token{kind: tokLiteral, text: src[i+1 : i+1+w]})
				i += w
			}
		case '\'', '"':
			var lit strings.Builder
			for i++; i < len(src) && src[i] != c; i++ {
				lit.WriteByte(src[i])
			}
			add(token{kind: tokLiteral, text: lit.String()})
		case 'E', 'e':
			if t, w, ok := parseSci(src[i:], c == 'E'); ok {
				add(t)
				s.sci = len(s.tokens) - 1
				i += w - 1
				continue
			}
			add(token{kind: tokLiteral, text: string(c)})
		default:
			if strings.HasPrefix(src[i:], "‰") {
				s.exp10 += 3
				add(token{kind: tokLiteral, text: "‰"})
				i += len("‰") - 1
				continue
			}

			_, w := utf8.DecodeRuneInString(src[i:])
			add(token{kind: tokLiteral, text: src[i : i+w]})
			i += w - 1
		}
	}

	for _, before := range commaRuns {
		switch {
		case before == 0:
		case before < len(s.ints):
			s.group = true
		default:
			s.commas++
		}
	}

	for i := range s.ints {
		if s.tokens[s.ints[i]].zero {
			s.minInt = len(s.ints) - i
			break
		}
	}
	return s
}

// parseSci 尝试把 `E+00` 这样的片段读成科学计数标记。
//
// 后面必须跟至少一个 `0`，否则那个 E 只是个普通字符。
func parseSci(src string, upper bool) (token, int, bool) {
	i := 1
	plus := false
	if i < len(src) && (src[i] == '+' || src[i] == '-') {
		plus = src[i] == '+'
		i++
	}
	n := 0
	for i < len(src) && src[i] == '0' {
		i++
		n++
	}
	if n == 0 {
		return token{}, 0, false
	}
	return token{kind: tokSci, expDigits: n, expPlus: plus, expUpper: upper}, i, true
}

// render 按这一节排版，第二个返回值说明排出来的数字位是不是全零。
//
// 小数末尾那些写作 `#` 的位，只有前面还有非零数字时才留：
// 所以 `0.##` 排 1.5 得到 "1.5" 而不是 "1.50"。
//
// 模板里一个整数位都没有时（比如 `.##`），整数部分整个塞在小数点之前，
// 见 [section.intPending]。
func (s section) render(n Num, nf *NumberFormat) (string, bool) {
	if k := s.exp10 - 3*s.commas; k != 0 {
		n = n.scaled(k)
	}
	if s.sci >= 0 {
		return s.renderSci(n, nf)
	}
	ip, fp := n.fixed(len(s.fracs))

	fracOut := make([]string, len(s.fracs))
	lastKept := -1
	for i := range s.fracs {
		d := byte('0')
		if i < len(fp) {
			d = fp[i]
		}
		fracOut[i] = string(d)
		if d != '0' || s.tokens[s.fracs[i]].zero {
			lastKept = i
		}
	}
	for i := lastKept + 1; i < len(fracOut); i++ {
		fracOut[i] = ""
	}

	intOut := s.layoutInt(ip, nf)
	pending := s.intPending(ip, nf)

	var sb strings.Builder
	fi, ii := 0, 0
	for _, t := range s.tokens {
		switch t.kind {
		case tokIntDigit:
			sb.WriteString(intOut[ii])
			ii++
		case tokFracDigit:
			sb.WriteString(fracOut[fi])
			fi++
		case tokPoint:
			sb.WriteString(pending)
			if lastKept >= 0 {
				sb.WriteString(nf.NumberDecimalSeparator)
			}
		default:
			sb.WriteString(t.text)
		}
	}
	return sb.String(), allZero(ip) && allZero(fp)
}

// intPending 处理模板里没有整数位的情形。
//
// 那时整数部分无处安放，只好整块堆到小数点前面；正好是 0 的话就不写。
func (s section) intPending(ip string, nf *NumberFormat) string {
	if len(s.ints) > 0 || ip == "0" {
		return ""
	}
	if s.group {
		return group(ip, nf.NumberGroupSizes, nf.NumberGroupSeparator)
	}
	return ip
}

// allZero 报告一个数字串是不是全零。
func allZero(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '0' {
			return false
		}
	}
	return true
}

// layoutInt 把整数部分从右往左摊到各个整数位上。
//
// 位数不够时，最左边那个整数位吃下剩余的全部数字——所以 `00` 排 12345
// 得到 "12345" 而不是 "45"。位数有余时，只有 `0` 那些位补零。
//
// 整数部分正好是 0 时当作空串：那时补不补零由 minInt 决定，
// `#` 排 0 应当什么都不出。
func (s section) layoutInt(ip string, nf *NumberFormat) []string {
	out := make([]string, len(s.ints))
	if len(s.ints) == 0 {
		return out
	}
	seq := ip
	if s.group {
		seq = group(ip, nf.NumberGroupSizes, nf.NumberGroupSeparator)
	}

	if ip == "0" {
		seq = ""
	}
	si := len(seq) - 1
	for k := len(s.ints) - 1; k >= 0; k-- {
		if si < 0 {
			if k >= len(s.ints)-s.minInt {
				out[k] = "0"
			}
			continue
		}
		start := si
		for start > 0 && !isDigitByte(seq[start-1]) {
			start--
		}
		if k == 0 {
			out[k] = seq[:si+1]
			si = -1
			continue
		}
		out[k] = seq[start : si+1]
		si = start - 1
	}
	return out
}

// isDigitByte 报告一个字节是不是数字。
func isDigitByte(c byte) bool { return c >= '0' && c <= '9' }

// renderSci 按科学计数排版。
//
// 有效位数等于整数位加小数位；指数由「有效数字的十进制指数」减去整数位数
// 得出，这样小数点正好落在模板画的那个位置上。
func (s section) renderSci(n Num, nf *NumberFormat) (string, bool) {
	sig := len(s.ints) + len(s.fracs)
	digits, exp := n.sigRound(max(sig, 1))

	mant := digits

	shift := exp - len(s.ints)
	if digits == "" {
		mant, shift = "", 0
	}
	for len(mant) < sig {
		mant += "0"
	}
	mant = mant[:sig]

	nInt := len(s.ints)
	lastKept := -1
	fracOut := make([]string, len(s.fracs))
	for i := range s.fracs {
		d := byte('0')
		if nInt+i < len(mant) {
			d = mant[nInt+i]
		}
		fracOut[i] = string(d)
		if d != '0' || s.tokens[s.fracs[i]].zero {
			lastKept = i
		}
	}
	for i := lastKept + 1; i < len(fracOut); i++ {
		fracOut[i] = ""
	}

	var sb strings.Builder
	mi, fi := 0, 0
	for i, tk := range s.tokens {
		switch tk.kind {
		case tokIntDigit:
			if mi < len(mant) {
				sb.WriteByte(mant[mi])
			} else {
				sb.WriteByte('0')
			}
			mi++
		case tokFracDigit:
			sb.WriteString(fracOut[fi])
			fi++
			mi++
		case tokPoint:
			if lastKept >= 0 {
				sb.WriteString(nf.NumberDecimalSeparator)
			}
		case tokSci:
			if i != s.sci {
				break
			}
			e := 'e'
			if tk.expUpper {
				e = 'E'
			}
			sb.WriteRune(e)
			ev := shift
			if ev < 0 {
				sb.WriteString(nf.NegativeSign)
				ev = -ev
			} else if tk.expPlus {
				sb.WriteString(nf.PositiveSign)
			}
			es := itoaPad(ev, tk.expDigits)
			sb.WriteString(es)
		default:
			sb.WriteString(tk.text)
		}
	}
	return sb.String(), digits == ""
}

// itoaPad 把非负整数写成至少 width 位，左边补零。
func itoaPad(v, width int) string {
	s := ""
	if v == 0 {
		s = "0"
	}
	for v > 0 {
		s = string(rune('0'+v%10)) + s
		v /= 10
	}
	for len(s) < width {
		s = "0" + s
	}
	return s
}
