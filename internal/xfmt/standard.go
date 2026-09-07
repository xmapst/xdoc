package xfmt

import (
	"math/big"
	"strconv"
	"strings"
)

// Format 按格式串排版一个数。
//
// 先试标准写法（一个字母加可选位数，如 F2、N0、E3），不是的话按自定义模板处理。
//
// 含 `{` 或 `}` 直接报错：那是复合格式串的括号，这里不接受。
// 单个字母又不是标准写法的（比如 "Q"）也报错，而不是当成一个字面字符输出——
// 那多半是把写法代号记错了。
func (n Num) Format(format string, nf *NumberFormat) (string, error) {
	if strings.ContainsAny(format, "{}") {
		return "", errBadFormat
	}
	if s, ok := n.special(nf); ok {
		return s, nil
	}
	if spec, prec, hasPrec, ok := parseStandard(format); ok {
		return n.standard(spec, prec, hasPrec, nf)
	}

	if len(format) == 1 && isASCIILetter(format[0]) {
		return "", errBadFormat
	}
	return n.custom(format, nf)
}

// parseStandard 解析标准写法：一个代号字母加最多两位数字。
//
// 空串等同 G（通用写法）。
func parseStandard(f string) (spec byte, prec int, hasPrec, ok bool) {
	if f == "" {
		return 'G', 0, false, true
	}
	if len(f) > 3 {
		return 0, 0, false, false
	}
	c := f[0]
	if !isStandardSpec(c) {
		return 0, 0, false, false
	}
	if len(f) == 1 {
		return c, 0, false, true
	}

	for i := 1; i < len(f); i++ {
		if f[i] < '0' || f[i] > '9' {
			return 0, 0, false, false
		}
	}
	p, err := strconv.Atoi(f[1:])
	if err != nil {
		return 0, 0, false, false
	}
	return c, p, true, true
}

// isASCIILetter 报告一个字节是不是 ASCII 字母。
func isASCIILetter(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// isStandardSpec 报告一个字母是不是标准写法的代号。
func isStandardSpec(c byte) bool {
	switch c {
	case 'C', 'c', 'D', 'd', 'E', 'e', 'F', 'f', 'G', 'g', 'N', 'n', 'P', 'p', 'R', 'r', 'X', 'x':
		return true
	}
	return false
}

// standard 按标准写法排版。
//
// D（补零整数）与 X（十六进制）只对整数有意义，用在浮点或十进制数上报错。
func (n Num) standard(spec byte, prec int, hasPrec bool, nf *NumberFormat) (string, error) {
	switch spec {
	case 'C', 'c':
		return n.currency(prec, hasPrec, nf), nil
	case 'D', 'd':
		if !n.isInteger() {
			return "", errBadFormat
		}
		return n.decimalSpec(prec, nf), nil
	case 'E', 'e':
		return n.scientific(prec, hasPrec, spec == 'E', 3, nf), nil
	case 'F', 'f':
		return n.fixedSpec(prec, hasPrec, nf), nil
	case 'G', 'g':
		return n.general(prec, hasPrec, spec == 'G', nf), nil
	case 'N', 'n':
		return n.numberSpec(prec, hasPrec, nf), nil
	case 'P', 'p':
		return n.percent(prec, hasPrec, nf), nil
	case 'R', 'r':
		return n.roundtrip(prec, hasPrec, spec == 'R', nf), nil
	case 'X', 'x':
		if !n.isInteger() {
			return "", errBadFormat
		}
		return n.hex(prec, spec == 'X'), nil
	}
	return "", errBadFormat
}

// body 排出「分组后的整数部分 + 小数点 + 定长小数部分」，不含符号。
func (n Num) body(frac int, sizes []int, gsep, dsep string) string {
	ip, fp := n.fixed(frac)
	ip = group(ip, sizes, gsep)
	if frac == 0 {
		return ip
	}
	return ip + dsep + fp
}

// fixedSpec 是定点写法：固定小数位数，不分组。
func (n Num) fixedSpec(prec int, hasPrec bool, nf *NumberFormat) string {
	frac := prec
	if !hasPrec {
		frac = nf.NumberDecimalDigits
	}
	s := n.body(frac, nil, "", nf.NumberDecimalSeparator)
	if n.negative() {
		return nf.NegativeSign + s
	}
	return s
}

// numberSpec 是数字写法：固定小数位数，按分组分隔符分组。
func (n Num) numberSpec(prec int, hasPrec bool, nf *NumberFormat) string {
	frac := prec
	if !hasPrec {
		frac = nf.NumberDecimalDigits
	}
	s := n.body(frac, nf.NumberGroupSizes, nf.NumberGroupSeparator, nf.NumberDecimalSeparator)
	if !n.negative() {
		return s
	}
	return nf.numberNegative(s)
}

// numberNegative 按这套格式的负数写法给数字加负号。
//
// 写法编号决定负号在前、在后、还是用括号，中间要不要空格。
func (nf *NumberFormat) numberNegative(s string) string {
	pattern, sign := nf.NumberNegativePattern, nf.NegativeSign
	switch pattern {
	case 0:
		return "(" + s + ")"
	case 2:
		return sign + " " + s
	case 3:
		return s + sign
	case 4:
		return s + " " + sign
	default:
		return sign + s
	}
}

// currency 是货币写法：分组、固定小数位数，再按写法编号摆放货币符号。
func (n Num) currency(prec int, hasPrec bool, nf *NumberFormat) string {
	frac := prec
	if !hasPrec {
		frac = nf.CurrencyDecimalDigits
	}
	s := n.body(frac, nf.CurrencyGroupSizes, nf.CurrencyGroupSeparator, nf.CurrencyDecimalSeparator)
	cur := nf.CurrencySymbol
	if !n.negative() {
		switch nf.CurrencyPositivePattern {
		case 1:
			return s + cur
		case 2:
			return cur + " " + s
		case 3:
			return s + " " + cur
		default:
			return cur + s
		}
	}
	return nf.currencyNegative(s, cur)
}

// currencyNegative 按写法编号摆放负号与货币符号。
//
// 十七种排列都要列出来：编号是格式数据里的值，缺一种就会排错。
func (nf *NumberFormat) currencyNegative(s, cur string) string {
	pattern, sign := nf.CurrencyNegativePattern, nf.NegativeSign
	switch pattern {
	case 0:
		return "(" + cur + s + ")"
	case 2:
		return cur + sign + s
	case 3:
		return cur + s + sign
	case 4:
		return "(" + s + cur + ")"
	case 5:
		return sign + s + cur
	case 6:
		return s + sign + cur
	case 7:
		return s + cur + sign
	case 8:
		return sign + s + " " + cur
	case 9:
		return sign + cur + " " + s
	case 10:
		return s + " " + cur + sign
	case 11:
		return cur + " " + s + sign
	case 12:
		return cur + " " + sign + s
	case 13:
		return s + sign + " " + cur
	case 14:
		return "(" + cur + " " + s + ")"
	case 15:
		return "(" + s + " " + cur + ")"
	case 16:
		return cur + sign + " " + s
	default:
		return sign + cur + s
	}
}

// percent 是百分号写法：先乘一百，再按写法编号摆放百分号。
//
// 符号看的是原数而不是乘完的数——乘法不改变符号，但十进制那条路上
// 乘完的值是新造的，符号位不一定跟着走。
func (n Num) percent(prec int, hasPrec bool, nf *NumberFormat) string {
	frac := prec
	if !hasPrec {
		frac = nf.PercentDecimalDigits
	}
	v := n.percentValue(frac)
	s := v.body(frac, nf.PercentGroupSizes, nf.PercentGroupSeparator, nf.PercentDecimalSeparator)
	pct := nf.PercentSymbol
	if !n.negative() {
		switch nf.PercentPositivePattern {
		case 1:
			return s + pct
		case 2:
			return pct + s
		case 3:
			return pct + " " + s
		default:
			return s + " " + pct
		}
	}
	return nf.percentNegative(s, pct)
}

// percentNegative 按写法编号摆放负号与百分号。
func (nf *NumberFormat) percentNegative(s, pct string) string {
	pattern, sign := nf.PercentNegativePattern, nf.NegativeSign
	switch pattern {
	case 1:
		return sign + s + pct
	case 2:
		return sign + pct + s
	case 3:
		return pct + sign + s
	case 4:
		return pct + s + sign
	case 5:
		return s + sign + pct
	case 6:
		return s + pct + sign
	case 7:
		return sign + pct + " " + s
	case 8:
		return s + " " + pct + sign
	case 9:
		return pct + " " + s + sign
	case 10:
		return pct + " " + sign + s
	case 11:
		return s + sign + " " + pct
	default:
		return sign + s + " " + pct
	}
}

// times100 乘以一百。
//
// 十进制这条路优先减小数位数：够减就不做乘法，尾数不变，也就不会溢出。
func (n Num) times100() Num {
	switch n.Kind {
	case Double:
		return FloatNum(n.F * 100)
	case Decimal:
		if n.Sc >= 2 {
			return DecNum(n.Abs, n.Neg, n.Sc-2)
		}
		return DecNum(new(big.Int).Mul(n.Abs, pow10(2-n.Sc)), n.Neg, 0)
	default:
		return DecNum(new(big.Int).Mul(n.absBig(), big.NewInt(100)), n.I < 0, 0)
	}
}

// decimalSpec 是补零整数写法：至少 prec 位，左边补零。
func (n Num) decimalSpec(prec int, nf *NumberFormat) string {
	s := n.absBig().String()
	for len(s) < prec {
		s = "0" + s
	}
	if n.I < 0 {
		return nf.NegativeSign + s
	}
	return s
}

// hex 是十六进制写法，至少 prec 位，左边补零。
//
// 32 位整数按 32 位补码写：-1 排成 ffffffff 而不是 ffffffffffffffff。
func (n Num) hex(prec int, upper bool) string {
	var s string
	if n.Kind == Int32 {
		s = strconv.FormatUint(uint64(uint32(int32(n.I))), 16)
	} else {
		s = strconv.FormatUint(uint64(n.I), 16)
	}
	if upper {
		s = strings.ToUpper(s)
	}
	for len(s) < prec {
		s = "0" + s
	}
	return s
}

// scientific 是科学计数写法，默认六位小数，指数至少三位。
func (n Num) scientific(prec int, hasPrec bool, upper bool, expDigits int, nf *NumberFormat) string {
	if !hasPrec {
		prec = 6
	}
	digits, exp := n.sigRound(prec + 1)
	return nf.assembleSci(digits, exp, prec, upper, expDigits, n.negative())
}

// assembleSci 把有效数字与指数拼成科学计数形式。
//
// 数字串为空（值是零）时写成 0，指数按 1 算，于是排出 0.000000e+000。
func (nf *NumberFormat) assembleSci(digits string, exp, prec int, upper bool, expDigits int,
	neg bool) string {
	var sb strings.Builder
	if neg {
		sb.WriteString(nf.NegativeSign)
	}
	if digits == "" {
		sb.WriteByte('0')
		exp = 1
	} else {
		sb.WriteByte(digits[0])
		digits = digits[1:]
	}
	if prec > 0 {
		sb.WriteString(nf.NumberDecimalSeparator)
		for i := range prec {
			if i < len(digits) {
				sb.WriteByte(digits[i])
			} else {
				sb.WriteByte('0')
			}
		}
	}
	e := 'e'
	if upper {
		e = 'E'
	}
	sb.WriteRune(e)
	ev := exp - 1
	if ev < 0 {
		sb.WriteString(nf.NegativeSign)
		ev = -ev
	} else {
		sb.WriteString(nf.PositiveSign)
	}
	es := strconv.Itoa(ev)
	for len(es) < expDigits {
		es = "0" + es
	}
	sb.WriteString(es)
	return sb.String()
}

// general 是通用写法：定点与科学计数里挑短的那个。
//
// 没指定位数时取能唯一还原这个值的最短数字串，判定阈值则按类型的
// 最大有效位数算。
func (n Num) general(prec int, hasPrec bool, upper bool, nf *NumberFormat) string {
	if n.Kind == Decimal {
		return n.decimalGeneral(prec, hasPrec, upper, nf)
	}
	var digits string
	var exp int
	if hasPrec && prec > 0 {
		digits, exp = n.sigRound(prec)
	} else {
		digits, exp = n.shortest()
		prec = n.defaultPrecision()
	}
	return nf.assembleGeneral(digits, exp, prec, upper, n.negative())
}

// assembleGeneral 在定点与科学计数之间挑一个。
//
// 指数落在 -3 到 prec 之间用定点，否则用科学计数：再往两头走，
// 定点写法的前导零或尾随零就比数字本身还长了。
func (nf *NumberFormat) assembleGeneral(digits string, exp, prec int, upper, neg bool) string {
	if exp <= prec && exp >= -3 {
		return nf.assembleGeneralFixed(digits, exp, neg)
	}
	return nf.assembleSci(digits, exp, max(len(digits)-1, 0), upper, 2, neg)
}

// decimalGeneral 是十进制数的通用写法。
//
// 不指定位数时原样写出，小数位数一位不改——那是这个类型的一部分。
func (n Num) decimalGeneral(prec int, hasPrec, upper bool, nf *NumberFormat) string {
	if !hasPrec {
		return n.decimalPlain(nf)
	}

	if prec < 1 {
		prec = n.decimalDigitCount()
	}
	digits, exp := n.sigRound(prec)
	return nf.assembleGeneral(digits, exp, prec, upper, n.negative())
}

// decimalDigitCount 返回尾数有多少位十进制数字，零给 0。
func (n Num) decimalDigitCount() int {
	if n.Abs.Sign() == 0 {
		return 0
	}
	return len(n.Abs.String())
}

// decimalPlain 把十进制数原样写出，保留全部小数位。
func (n Num) decimalPlain(nf *NumberFormat) string {
	s := n.Abs.String()
	switch {
	case n.Sc <= 0:
		s += strings.Repeat("0", -n.Sc)
	default:
		for len(s) <= n.Sc {
			s = "0" + s
		}
		s = s[:len(s)-n.Sc] + nf.NumberDecimalSeparator + s[len(s)-n.Sc:]
	}
	if n.negative() {
		return nf.NegativeSign + s
	}
	return s
}

// defaultPrecision 返回各类型的最大有效位数。
//
// 通用写法用它决定什么时候改用科学计数。
func (n Num) defaultPrecision() int {
	switch n.Kind {
	case Int32:
		return 10
	case Int64:
		return 19
	default:
		return 17
	}
}

// assembleGeneralFixed 把有效数字与指数拼成定点形式，两头按需补零。
func (nf *NumberFormat) assembleGeneralFixed(digits string, exp int, neg bool) string {
	var sb strings.Builder
	if neg {
		sb.WriteString(nf.NegativeSign)
	}
	switch {
	case digits == "":
		sb.WriteByte('0')
	case exp <= 0:
		sb.WriteByte('0')
		sb.WriteString(nf.NumberDecimalSeparator)
		sb.WriteString(strings.Repeat("0", -exp))
		sb.WriteString(digits)
	case exp >= len(digits):
		sb.WriteString(digits)
		sb.WriteString(strings.Repeat("0", exp-len(digits)))
	default:
		sb.WriteString(digits[:exp])
		sb.WriteString(nf.NumberDecimalSeparator)
		sb.WriteString(digits[exp:])
	}
	return sb.String()
}

// roundtrip 是往返写法：排出来的串再读回去能得到同一个值。
//
// 只对浮点有额外含义——取最短且唯一的数字串。其余类型本来就是精确的，
// 直接走通用写法。
func (n Num) roundtrip(prec int, hasPrec, upper bool, nf *NumberFormat) string {
	if n.Kind != Double {
		return n.general(prec, hasPrec, upper, nf)
	}
	digits, exp := n.shortest()

	return nf.assembleGeneral(digits, exp, n.defaultPrecision(), upper, n.negative())
}
