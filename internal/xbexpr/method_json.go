package xbexpr

import (
	"encoding/base64"
	"math"
	"math/big"
	"strconv"
	"strings"
	"unicode"

	"github.com/xmapst/xdoc/internal/xbin"
	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xfmt"
)

// jsonWriter 往一个字符串构建器上写 JSON。
type jsonWriter struct {
	*strings.Builder
}

// jsonText 把一个值写成 JSON 文本。
//
// 没有 JSON 原生写法的类型走扩展写法：一个只有一个 $ 开头键的对象，
// 比如 {"$oid":"..."}。[jsonParse] 认得这套写法，能原样读回来。
func jsonText(v *xbson.Value) string {
	w := jsonWriter{new(strings.Builder)}
	w.value(v)
	return w.String()
}

// value 按类型写出一个值。
//
// 几处约定：64 位整数不写成裸数字而走扩展写法，免得读的一方按双精度收；
// NaN 与无穷写成 null——JSON 没有它们；时间一律先换成 UTC 再写。
// 向量写成普通的数字数组，读回来就成了数组，类型不保。
func (w jsonWriter) value(v *xbson.Value) {
	switch v.Type() {
	case xbson.TypeNull:
		w.WriteString("null")
	case xbson.TypeMinValue:
		w.ext("$minValue", "1")
	case xbson.TypeMaxValue:
		w.ext("$maxValue", "1")
	case xbson.TypeBoolean:
		x, _ := v.AsBoolean()
		if x {
			w.WriteString("true")
		} else {
			w.WriteString("false")
		}
	case xbson.TypeInt32:
		n, _ := v.AsInt32()
		w.WriteString(strconv.FormatInt(int64(n), 10))
	case xbson.TypeInt64:
		n, _ := v.AsInt64()
		w.ext("$numberLong", strconv.FormatInt(n, 10))
	case xbson.TypeDouble:
		f, _ := v.AsDouble()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			w.WriteString("null")
			break
		}
		w.WriteString(doubleText(f))
	case xbson.TypeDecimal:
		d, _ := v.AsDecimal()
		w.ext("$numberDecimal", d.String())
	case xbson.TypeString:
		s, _ := v.AsString()
		w.quoted(s)
	case xbson.TypeBinary:
		bs, _ := v.AsBinary()
		w.ext("$binary", base64.StdEncoding.EncodeToString(bs))
	case xbson.TypeObjectID:
		id, _ := v.AsObjectID()
		w.ext("$oid", id.String())
	case xbson.TypeGUID:
		g, _ := v.AsGUID()
		w.ext("$guid", g.String())
	case xbson.TypeDateTime:
		t, ok := v.AsTime()
		if !ok {
			w.WriteString("null")
			break
		}

		w.ext("$date", t.UTC().Format("2006-01-02T15:04:05.0000000Z"))
	case xbson.TypeDocument:
		d, _ := v.AsDocument()
		w.WriteByte('{')
		first := true
		for k, val := range d.Elements() {
			if !first {
				w.WriteByte(',')
			}
			first = false
			w.WriteByte('"')
			w.WriteString(k)
			w.WriteString("\":")
			w.value(val)
		}
		w.WriteByte('}')
	case xbson.TypeArray:
		a, _ := v.AsArray()
		w.WriteByte('[')
		for i, it := range a.Items() {
			if i > 0 {
				w.WriteByte(',')
			}
			w.value(it)
		}
		w.WriteByte(']')
	case xbson.TypeVector:
		f, _ := v.AsVector()
		w.WriteByte('[')
		for i, x := range f {
			if i > 0 {
				w.WriteByte(',')
			}
			w.WriteString(doubleText(float64(x)))
		}
		w.WriteByte(']')
	}
}

// ext 写一个扩展写法的对象。文本直接嵌进去，不做转义。
func (w jsonWriter) ext(kind, text string) {
	w.WriteString(`{"`)
	w.WriteString(kind)
	w.WriteString(`":"`)
	w.WriteString(text)
	w.WriteString(`"}`)
}

// doubleText 把双精度写成定点小数，**最多九位小数**，且一定带小数点。
//
// 先取十五位有效数字，超出九位小数的部分四舍五入。因此绝对值小于
// 五乘十的负十次方的数会写成 0.0，正负零分别写成 0.0 和 -0.0。
func doubleText(f float64) string {
	if f == 0 {
		if math.Signbit(f) {
			return "-0.0"
		}
		return "0.0"
	}
	mant, exp, ok := splitSci(strconv.FormatFloat(f, 'e', 14, 64))
	if !ok {
		return "0.0"
	}
	m, ok := new(big.Int).SetString(mant, 10)
	if !ok {
		return "0.0"
	}
	neg := m.Sign() < 0
	m.Abs(m)

	scale := -exp
	if scale < 0 {
		m.Mul(m, pow10(-scale))
		scale = 0
	}
	if scale > 9 {
		div := pow10(scale - 9)
		q, r := new(big.Int).QuoRem(m, div, new(big.Int))
		if new(big.Int).Lsh(r, 1).Cmp(div) >= 0 {
			q.Add(q, big.NewInt(1))
		}
		m, scale = q, 9
	}
	digits := m.String()
	var intPart, fracPart string
	if scale == 0 {
		intPart, fracPart = digits, ""
	} else if len(digits) > scale {
		intPart, fracPart = digits[:len(digits)-scale], digits[len(digits)-scale:]
	} else {
		intPart = "0"
		fracPart = strings.Repeat("0", scale-len(digits)) + digits
	}
	fracPart = strings.TrimRight(fracPart, "0")
	if fracPart == "" {
		fracPart = "0"
	}
	out := intPart + "." + fracPart
	if neg {
		out = "-" + out
	}
	return out
}

// jsonSafe 是可以直接写进 JSON 字符串的 Unicode 分类，其余转义成 \u 形式。
var jsonSafe = []*unicode.RangeTable{
	unicode.Lu, unicode.Ll, unicode.Lt, unicode.Lo,
	unicode.Nd, unicode.Nl, unicode.No,
	unicode.Zs,
	unicode.Pc, unicode.Pd, unicode.Ps, unicode.Pe, unicode.Pi, unicode.Pf, unicode.Po,
	unicode.Sm, unicode.Sc, unicode.Sk, unicode.So,
}

// quoted 写一个带引号的 JSON 字符串。
//
// 按 UTF-16 码元逐个处理，代理对不在安全分类里，因此**总是**写成两个 \u 转义。
func (w jsonWriter) quoted(s string) {
	w.WriteByte('"')
	for _, c := range utf16Of(s) {
		switch c {
		case '"':
			w.WriteString(`\"`)
		case '\\':
			w.WriteString(`\\`)
		case '\b':
			w.WriteString(`\b`)
		case '\f':
			w.WriteString(`\f`)
		case '\n':
			w.WriteString(`\n`)
		case '\r':
			w.WriteString(`\r`)
		case '\t':
			w.WriteString(`\t`)
		default:
			if !isSurrogateUnit(c) && unicode.In(rune(c), jsonSafe...) {
				w.WriteRune(rune(c))
			} else {
				w.WriteString(`\u`)
				h := strconv.FormatUint(uint64(c), 16)
				for i := len(h); i < 4; i++ {
					w.WriteByte('0')
				}
				w.WriteString(h)
			}
		}
	}
	w.WriteByte('"')
}

// jsonParse 解析一段 JSON 文本。
//
// 三个返回值分工：fatal 是「确实是 JSON，但里面的值坏了」，比如 $oid 解不出来；
// ok 为 false 且 fatal 为空则是普通的语法错误，调用方一般当成 Null。
// 空文本解析成 Null。
func jsonParse(s string) (v *xbson.Value, fatal error, ok bool) {
	p := &jsonParser{src: utf16Of(s)}
	tok := p.next()
	if tok.kind == jtEOF {
		return xbson.Null, nil, true
	}
	val, err := p.value(tok)
	if err != nil {
		if fe, is := err.(jsonFatal); is {
			return nil, fe.err, false
		}
		return nil, nil, false
	}

	return val, nil, true
}

// jsonFatal 把一个错误标成硬错误，与普通语法错误区分开。
type jsonFatal struct{ err error }

// Error 转发底下那个错误的文本。
func (e jsonFatal) Error() string { return e.err.Error() }

// errSyntax 是普通语法错误，调用方据此返回 Null 而不是报错。
var errSyntax = errf("unexpected token")

// jtKind 是 JSON 记号的类别。
type jtKind uint8

const (
	jtEOF jtKind = iota
	jtOpenBrace
	jtCloseBrace
	jtOpenBracket
	jtCloseBracket
	jtColon
	jtComma
	jtMinus
	jtString
	jtInt
	jtDouble
	jtWord
	jtUnknown
)

// jtoken 是一个 JSON 记号。
type jtoken struct {
	kind jtKind
	val  string
}

// jsonParser 在 UTF-16 码元上做一遍扫描，边扫边解析。
type jsonParser struct {
	src []uint16
	pos int
}

// at 取第 i 个码元；越界返回 false。
func (p *jsonParser) at(i int) (uint16, bool) {
	if i < 0 || i >= len(p.src) {
		return 0, false
	}
	return p.src[i], true
}

// next 切出下一个记号。
//
// 比标准 JSON 宽松：单引号也能括字符串，两个减号起头到行尾算注释。
func (p *jsonParser) next() jtoken {
	for {
		c, ok := p.at(p.pos)
		if !ok {
			return jtoken{kind: jtEOF}
		}
		if unicode.IsSpace(rune(c)) {
			p.pos++
			continue
		}
		switch c {
		case '{':
			p.pos++
			return jtoken{kind: jtOpenBrace}
		case '}':
			p.pos++
			return jtoken{kind: jtCloseBrace}
		case '[':
			p.pos++
			return jtoken{kind: jtOpenBracket}
		case ']':
			p.pos++
			return jtoken{kind: jtCloseBracket}
		case ':':
			p.pos++
			return jtoken{kind: jtColon}
		case ',':
			p.pos++
			return jtoken{kind: jtComma}
		case '"', '\'':
			p.pos++
			return jtoken{kind: jtString, val: p.readString(c)}
		case '-':
			if n, ok := p.at(p.pos + 1); ok && n == '-' {
				p.pos += 2
				for {
					ch, ok := p.at(p.pos)
					if !ok {
						break
					}
					p.pos++
					if ch == '\n' {
						break
					}
				}
				continue
			}
			p.pos++
			return jtoken{kind: jtMinus}
		}
		if c >= '0' && c <= '9' {
			return p.readNumber()
		}
		if isWordStart(c) {
			return p.readWord()
		}
		p.pos++
		return jtoken{kind: jtUnknown}
	}
}

// isWordStart 判断能不能作裸词的首字符：字母、下划线或美元号。
func isWordStart(c uint16) bool {
	return unicode.IsLetter(rune(c)) || c == '_' || c == '$'
}

// isWordPart 判断能不能作裸词的后续字符。
func isWordPart(c uint16) bool {
	return unicode.IsLetter(rune(c)) || unicode.IsDigit(rune(c)) || c == '_' || c == '$'
}

// readWord 读一个裸词，用来认 true、false、null 和扩展写法的键。
//
// 单独一个美元号后面不接字母时切成未知记号。
func (p *jsonParser) readWord() jtoken {
	start := p.pos
	if c, _ := p.at(p.pos); c == '$' {
		p.pos++
		if n, ok := p.at(p.pos); !ok || !isWordStart(n) {
			return jtoken{kind: jtUnknown, val: "$"}
		}
	}
	for {
		c, ok := p.at(p.pos)
		if !ok || !isWordPart(c) {
			break
		}
		p.pos++
	}
	return jtoken{kind: jtWord, val: utf16Str(p.src[start:p.pos])}
}

// readNumber 读一个数，区分整数和浮点。读到不合规的字符就停下。
func (p *jsonParser) readNumber() jtoken {
	start := p.pos
	canDot, canE, canSign, isDouble := true, true, false, false
	for {
		c, ok := p.at(p.pos)
		if !ok {
			break
		}
		switch {
		case c >= '0' && c <= '9':
			canSign = false
		case c == '.' && canDot:
			canDot, isDouble, canSign = false, true, false
		case (c == 'e' || c == 'E') && canE:
			canE, isDouble, canSign = false, true, true
		case (c == '+' || c == '-') && canSign:
			canSign = false
		default:
			goto done
		}
		p.pos++
	}
done:
	text := utf16Str(p.src[start:p.pos])
	if isDouble {
		return jtoken{kind: jtDouble, val: text}
	}
	return jtoken{kind: jtInt, val: text}
}

// readString 读一个带引号的字符串，返回解转义之后的内容。
//
// 认不出的转义序列被整个丢掉。没有收尾引号时读到末尾为止，不报错。
func (p *jsonParser) readString(quote uint16) string {
	var out []uint16
	for {
		c, ok := p.at(p.pos)
		if !ok || c == quote {
			if ok {
				p.pos++
			}
			return utf16Str(out)
		}
		p.pos++
		if c != '\\' {
			out = append(out, c)
			continue
		}
		e, ok := p.at(p.pos)
		if !ok {
			return utf16Str(out)
		}
		p.pos++
		switch e {
		case quote:
			out = append(out, quote)
		case '\\':
			out = append(out, '\\')
		case '/':
			out = append(out, '/')
		case 'b':
			out = append(out, 0x08)
		case 'f':
			out = append(out, 0x0C)
		case 'n':
			out = append(out, 0x0A)
		case 'r':
			out = append(out, 0x0D)
		case 't':
			out = append(out, 0x09)
		case 'u':
			var v uint16
			for range 4 {
				h, ok := p.at(p.pos)
				if !ok {
					break
				}
				p.pos++
				v = v<<4 | uint16(hexVal(h))
			}
			out = append(out, v)
		default:
		}
	}
}

// hexVal 把一个十六进制字符转成数值；不是十六进制字符时当 0。
func hexVal(c uint16) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	default:
		return 0
	}
}

// value 由一个记号起头解析出一个值。
//
// true、false、null 三个词不分大小写。负号是单独的记号，后面必须接数字。
func (p *jsonParser) value(t jtoken) (*xbson.Value, error) {
	switch t.kind {
	case jtString:
		return xbson.String(t.val), nil
	case jtOpenBrace:
		return p.object()
	case jtOpenBracket:
		return p.array()
	case jtMinus:
		n := p.next()
		if n.kind != jtInt && n.kind != jtDouble {
			return nil, errSyntax
		}
		return n.kind.numberValue("-" + n.val)
	case jtInt, jtDouble:
		return t.kind.numberValue(t.val)
	case jtWord:
		switch strings.ToLower(t.val) {
		case "null":
			return xbson.Null, nil
		case "true":
			return xbson.True, nil
		case "false":
			return xbson.False, nil
		}
	default:
	}
	return nil, errSyntax
}

// numberValue 把数字文本转成值。
//
// 整数先试 32 位再试 64 位，都装不下就是硬错误。浮点数溢出到无穷不算错，
// 按无穷收下。
func (kind jtKind) numberValue(text string) (*xbson.Value, error) {
	if kind == jtInt {
		if n, err := strconv.ParseInt(text, 10, 32); err == nil {
			return xbson.Int32(int32(n)), nil
		}
		n, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return nil, jsonFatal{errf("integer %q is out of range", text)}
		}
		return xbson.Int64(n), nil
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil {
		if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
			return xbson.Double(f), nil
		}
		return nil, jsonFatal{errf("invalid number %q", text)}
	}
	return xbson.Double(f), nil
}

// object 解析一个对象。
//
// 键可以是字符串，也可以是裸词。**只有第一个键**才会被当作扩展写法来试；
// 认出来之后整个对象就变成那一个值，后面必须紧跟右花括号。
// 逗号可有可无——缺了也照样接着读下一个键。
func (p *jsonParser) object() (*xbson.Value, error) {
	d := xbson.NewDocument()
	t := p.next()
	for t.kind != jtCloseBrace {
		if t.kind != jtString && t.kind != jtWord {
			return nil, errSyntax
		}
		k := t.val
		if t = p.next(); t.kind != jtColon {
			return nil, errSyntax
		}
		t = p.next()
		if d.Len() == 0 {
			if k == "" {
				return nil, jsonFatal{errf("empty key in json object")}
			}
			if k[0] == '$' {
				v, handled, err := extendedValue(k, t.val)
				if err != nil {
					return nil, err
				}
				if handled {
					if e := p.next(); e.kind != jtCloseBrace {
						return nil, errSyntax
					}
					return v, nil
				}
			}
		}
		v, err := p.value(t)
		if err != nil {
			return nil, err
		}
		d.Set(k, v)
		t = p.next()
		if t.kind == jtComma {
			t = p.next()
		}
	}
	return d.Value(), nil
}

// array 解析一个数组。逗号同样可有可无；读到末尾还没见右方括号才算语法错误。
func (p *jsonParser) array() (*xbson.Value, error) {
	a := xbson.NewArray()
	t := p.next()
	for t.kind != jtCloseBracket {
		if t.kind == jtEOF {
			return nil, errSyntax
		}
		v, err := p.value(t)
		if err != nil {
			return nil, err
		}
		a.Append(v)
		t = p.next()
		if t.kind == jtComma {
			t = p.next()
		}
	}
	return a.Value(), nil
}

// extendedValue 按 $ 开头的键把文本解成对应类型的值。
//
// 第二个返回值说明这个键认不认得；认得但内容坏了一律算硬错误，
// 好让调用方报出具体原因而不是笼统地返回 Null。
func extendedValue(kind, text string) (v *xbson.Value, handled bool, err error) {
	switch kind {
	case "$minValue":
		return xbson.MinValue, true, nil
	case "$maxValue":
		return xbson.MaxValue, true, nil
	case "$binary":
		b, e := base64.StdEncoding.DecodeString(text)
		if e != nil {
			return nil, true, jsonFatal{errf("invalid base64 in $binary: %v", e)}
		}
		return xbson.Binary(b), true, nil
	case "$oid":
		id, e := xbson.ParseObjectID(text)
		if e != nil {
			return nil, true, jsonFatal{e}
		}
		return xbson.OID(id), true, nil
	case "$guid":
		g, e := parseGuidLoose(text)
		if e != nil {
			return nil, true, jsonFatal{e}
		}
		return xbson.GUID(g), true, nil
	case "$date":
		t, hadZone, e := parseDateText(text, xfmt.CurrentDate())
		if e != nil {
			return nil, true, jsonFatal{e}
		}
		dv, e := xbson.DateTime(t)
		if e != nil {
			return nil, true, jsonFatal{e}
		}
		if !hadZone {
			dv = dv.AsUnspecified()
		}
		return dv, true, nil
	case "$numberLong":
		n, e := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
		if e != nil {
			return nil, true, jsonFatal{errf("invalid $numberLong %q", text)}
		}
		return xbson.Int64(n), true, nil
	case "$numberDecimal":
		d, ok := decParse(text)
		if !ok {
			return nil, true, jsonFatal{errf("invalid $numberDecimal %q", text)}
		}
		x, e := d.toXbin()
		if e != nil {
			return nil, true, jsonFatal{e}
		}
		return xbson.Decimal(x), true, nil
	}
	return nil, false, nil
}

// parseGuidLoose 按宽松写法解析 GUID。
//
// 容许前后空白、外面套一对花括号或圆括号，以及不带连字符的三十二位写法。
func parseGuidLoose(s string) (xbin.Guid, error) {
	t := strings.TrimSpace(s)
	if len(t) >= 2 {
		if (t[0] == '{' && t[len(t)-1] == '}') || (t[0] == '(' && t[len(t)-1] == ')') {
			t = t[1 : len(t)-1]
		}
	}
	if len(t) == 32 && !strings.Contains(t, "-") {
		t = t[0:8] + "-" + t[8:12] + "-" + t[12:16] + "-" + t[16:20] + "-" + t[20:32]
	}
	return xbin.ParseGuid(strings.ToLower(t))
}
