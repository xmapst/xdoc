package xjson

import (
	"encoding/base64"
	"io"
	"math"
	"math/big"
	"strconv"
	"time"
	"unicode"
	"unicode/utf16"

	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xerr"
)

// indentWidth 是排版模式下每层缩进几个空格。
const indentWidth = 4

// maxDepth 是编解码时允许的最大嵌套层数。
//
// 编码这一侧同时也是**环检测**：值之间可以互相引用，
// 没有这个上限就会一路递归下去直到栈溢出。
const maxDepth = 1000

// Marshal 编成紧凑的一行。
func Marshal(v *xbson.Value) ([]byte, error) { return marshal(v, false, 0) }

// MarshalIndent 编成带缩进的多行。
func MarshalIndent(v *xbson.Value) ([]byte, error) { return marshal(v, true, indentWidth) }

// MarshalIndentWidth 编成带缩进的多行，缩进宽度自定。
func MarshalIndentWidth(v *xbson.Value, width int) ([]byte, error) {
	return marshal(v, true, max(width, 0))
}

// Writer 把值编码后直接写进一个流。
type Writer struct {
	// Indent 是缩进宽度，Pretty 决定要不要换行缩进。
	Indent int

	Pretty bool

	w io.Writer
}

// NewWriter 建一个写出器，默认缩进宽度。
func NewWriter(w io.Writer) *Writer { return &Writer{Indent: indentWidth, w: w} }

// Write 把一个值编码后写进流。
//
// 编码过程中缓冲攒到一定大小就先写出去一批，见 [encoder.spill]：
// 导出一份大集合时，内存里不必同时放下整篇文本。
func (w *Writer) Write(v *xbson.Value) error {
	e := &encoder{out: w.w, pretty: w.Pretty, width: max(w.Indent, 0)}
	if err := e.value(v); err != nil {
		return err
	}
	_, err := w.w.Write(e.buf)
	return err
}

// marshal 把一个值编成字节。
func marshal(v *xbson.Value, pretty bool, width int) ([]byte, error) {
	e := &encoder{pretty: pretty, width: width}
	if err := e.value(v); err != nil {
		return nil, err
	}
	return e.buf, nil
}

// encoder 是一次编码的状态。
type encoder struct {
	// buf 是攒着的输出；out 非空时攒够一批就写出去。
	buf []byte

	out io.Writer

	// pretty 与 width 决定排版；level 是当前缩进层数，depth 是嵌套层数。
	pretty bool
	width  int

	level int
	depth int
}

// value 编码一个值。
//
// JSON 本身只有几种类型，其余的用 `{"$oid": "..."}` 这样的扩展写法表达，
// 读的一侧认得回来。
//
// 向量编成普通数组：那样任何 JSON 读取方都看得懂，代价是读回来
// 不再是向量类型。
//
// NaN 与无穷编成 null——JSON 里没有它们，见 [encoder.appendDouble]。
func (e *encoder) value(v *xbson.Value) error {
	if v == nil {
		e.buf = append(e.buf, "null"...)
		return nil
	}
	switch v.Type() {
	case xbson.TypeNull:
		e.buf = append(e.buf, "null"...)
	case xbson.TypeBoolean:
		if b, _ := v.AsBoolean(); b {
			e.buf = append(e.buf, "true"...)
		} else {
			e.buf = append(e.buf, "false"...)
		}
	case xbson.TypeInt32:
		n, _ := v.AsInt32()
		e.buf = strconv.AppendInt(e.buf, int64(n), 10)
	case xbson.TypeDouble:
		f, _ := v.AsDouble()
		e.appendDouble(f)
	case xbson.TypeString:
		s, _ := v.AsString()
		e.appendQuoted(s)
	case xbson.TypeDocument:
		d, _ := v.AsDocument()
		return e.object(d)
	case xbson.TypeArray:
		a, _ := v.AsArray()
		return e.array(a.Items())
	case xbson.TypeVector:
		f, _ := v.AsVector()
		items := make([]*xbson.Value, len(f))
		for i, x := range f {
			items[i] = xbson.Double(float64(x))
		}
		return e.array(items)
	case xbson.TypeInt64:
		n, _ := v.AsInt64()
		e.ext("$numberLong", strconv.FormatInt(n, 10))
	case xbson.TypeDecimal:
		d, _ := v.AsDecimal()
		e.ext("$numberDecimal", d.String())
	case xbson.TypeBinary:
		b, _ := v.AsBinary()
		e.ext("$binary", base64.StdEncoding.EncodeToString(b))
	case xbson.TypeObjectID:
		id, _ := v.AsObjectID()
		e.ext("$oid", id.String())
	case xbson.TypeGUID:
		g, _ := v.AsGUID()
		e.ext("$guid", g.String())
	case xbson.TypeDateTime:
		t, ok := v.AsTime()
		if !ok {
			return xerr.InvalidDataType.Newf("date value cannot be restored to an instant")
		}
		e.ext("$date", formatDate(t))
	case xbson.TypeMinValue:
		e.ext("$minValue", "1")
	case xbson.TypeMaxValue:
		e.ext("$maxValue", "1")
	default:
		return xerr.InvalidDataType.Newf("unknown value type %d", int(v.Type()))
	}
	return nil
}

// object 编码一篇文档。
func (e *encoder) object(d *xbson.Document) error {
	if err := e.enter(); err != nil {
		return err
	}
	defer e.leave()

	keys, vals := orderedElements(d)
	e.startBlock('{', len(keys) > 0)
	for i, k := range keys {
		if err := e.keyValue(k, vals[i], i < len(keys)-1); err != nil {
			return err
		}
		if err := e.spill(); err != nil {
			return err
		}
	}
	e.endBlock('}', len(keys) > 0)
	return nil
}

// idKey 是主键字段名。
const idKey = "_id"

// orderedElements 取出全部键值，**主键挪到最前**，其余保持原有相对次序。
//
// 挪动用两次 copy 而不是删了再插：那样只搬一段，不必重排整个切片。
func orderedElements(d *xbson.Document) ([]string, []*xbson.Value) {
	keys := make([]string, 0, d.Len())
	vals := make([]*xbson.Value, 0, d.Len())
	id := -1
	for k, v := range d.Elements() {
		if id < 0 && len(k) == len(idKey) && foldEqual(k, idKey) {
			id = len(keys)
		}
		keys = append(keys, k)
		vals = append(vals, v)
	}
	if id > 0 {
		k, v := keys[id], vals[id]
		copy(keys[1:id+1], keys[:id])
		copy(vals[1:id+1], vals[:id])
		keys[0], vals[0] = k, v
	}
	return keys, vals
}

// foldEqual 按 ASCII 大小写不敏感地比较两个**等长**的串。
//
// 长度由调用方先比过，所以这里不再检查。
func foldEqual(a, b string) bool {
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if x >= 'A' && x <= 'Z' {
			x += 'a' - 'A'
		}
		if y >= 'A' && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// array 编码一列值。
func (e *encoder) array(items []*xbson.Value) error {
	if err := e.enter(); err != nil {
		return err
	}
	defer e.leave()

	e.startBlock('[', len(items) > 0)
	for i, it := range items {
		if e.pretty && !isFilledContainer(it) {
			e.indent()
		}
		if err := e.value(it); err != nil {
			return err
		}
		if i < len(items)-1 {
			e.buf = append(e.buf, ',')
		}
		e.newline()
		if err := e.spill(); err != nil {
			return err
		}
	}
	e.endBlock(']', len(items) > 0)
	return nil
}

// keyValue 编码一对键值。
//
// 排版模式下，值是非空的容器时先换行再写：那样嵌套的括号各占一行，
// 而空容器 `{}` 就地写完，不必浪费两行。
func (e *encoder) keyValue(key string, v *xbson.Value, comma bool) error {
	e.indent()
	e.appendQuoted(key)
	e.buf = append(e.buf, ':')
	if e.pretty {
		e.buf = append(e.buf, ' ')
		if isFilledContainer(v) {
			e.newline()
		}
	}
	if err := e.value(v); err != nil {
		return err
	}
	if comma {
		e.buf = append(e.buf, ',')
	}
	e.newline()
	return nil
}

// ext 写出一个扩展写法的对象，载荷一律当字符串，不再转义。
func (e *encoder) ext(key, payload string) {
	e.buf = append(e.buf, '{', '"')
	e.buf = append(e.buf, key...)
	e.buf = append(e.buf, '"', ':')
	if e.pretty {
		e.buf = append(e.buf, ' ')
	}
	e.buf = append(e.buf, '"')
	e.buf = append(e.buf, payload...)
	e.buf = append(e.buf, '"', '}')
}

// startBlock 写出容器的开括号，空容器不换行也不加缩进。
func (e *encoder) startBlock(c byte, hasData bool) {
	if !hasData {
		e.buf = append(e.buf, c)
		return
	}
	e.indent()
	e.buf = append(e.buf, c)
	e.newline()
	e.level++
}

// endBlock 写出容器的闭括号。
func (e *encoder) endBlock(c byte, hasData bool) {
	if !hasData {
		e.buf = append(e.buf, c)
		return
	}
	e.level--
	e.indent()
	e.buf = append(e.buf, c)
}

// indent 按当前层数写出缩进，非排版模式下什么都不写。
func (e *encoder) indent() {
	if !e.pretty {
		return
	}
	for range e.level * e.width {
		e.buf = append(e.buf, ' ')
	}
}

// newline 换行，非排版模式下什么都不写。
func (e *encoder) newline() {
	if e.pretty {
		e.buf = append(e.buf, '\n')
	}
}

// spillSize 是缓冲攒到多少字节就写出去一批。
const spillSize = 32 << 10

// spill 把攒够的输出写进流。
//
// 只在有流的时候做；纯编成字节那条路一路攒到底。
func (e *encoder) spill() error {
	if e.out == nil || len(e.buf) < spillSize {
		return nil
	}
	if _, err := e.out.Write(e.buf); err != nil {
		return err
	}
	e.buf = e.buf[:0]
	return nil
}

// enter 进入一层嵌套，太深就报错。
//
// 值之间可以互相引用，这个上限同时也是环检测。
func (e *encoder) enter() error {
	e.depth++
	if e.depth > maxDepth {
		return xerr.DocumentMaxDepth.Newf("document nested deeper than %d levels, likely a cycle", maxDepth)
	}
	return nil
}

// leave 退出一层嵌套。
func (e *encoder) leave() { e.depth-- }

// isFilledContainer 报告这是不是一个非空的容器，排版时据此决定要不要换行。
func isFilledContainer(v *xbson.Value) bool {
	if v == nil {
		return false
	}
	if d, ok := v.AsDocument(); ok {
		return d.Len() > 0
	}
	if a, ok := v.AsArray(); ok {
		return a.Len() > 0
	}
	if f, ok := v.AsVector(); ok {
		return len(f) > 0
	}
	return false
}

// formatDate 把时刻写成 UTC 的 ISO-8601 形式，固定七位小数秒。
func formatDate(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.0000000") + "Z"
}

// appendDouble 写出一个浮点数。
//
// NaN 与无穷写成 null：JSON 里没有它们，写成别的什么都不是合法 JSON。
// 代价是这三个值读回来变成空值。
func (e *encoder) appendDouble(f float64) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		e.buf = append(e.buf, "null"...)
		return
	}
	e.buf = append(e.buf, formatDouble(f)...)
}

// formatDouble 把浮点数写成**定点十进制，最多九位小数**。
//
// 不用指数写法：那样产出的文本各实现读法不一。做法是先取 15 位有效数字，
// 再按四舍五入缩放到九位小数。
//
// 两个后果：绝对值小于 5e-10 的数写出来是 0.0；小数点后第十位起的精度丢掉。
// 整数部分不截断，所以很大的数会写成一长串数字。
//
// 输出总带小数点，至少一位小数（如 "1.0"）。
func formatDouble(f float64) string {
	neg := math.Signbit(f)

	s := strconv.FormatFloat(math.Abs(f), 'e', 14, 64)
	epos := len(s) - 1
	for s[epos] != 'e' {
		epos--
	}
	exp, err := strconv.Atoi(s[epos+1:])
	if err != nil {
		return "0.0"
	}
	digits := s[:1] + s[2:epos]

	m, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		return "0.0"
	}
	if shift := exp - 5; shift >= 0 {
		m.Mul(m, pow10(shift))
	} else {
		d := pow10(-shift)
		q, r := new(big.Int).QuoRem(m, d, new(big.Int))
		r.Lsh(r, 1)
		if r.Cmp(d) >= 0 {
			q.Add(q, bigOne)
		}
		m = q
	}

	q := m.String()
	for len(q) < 10 {
		q = "0" + q
	}
	frac := q[len(q)-9:]

	for len(frac) > 1 && frac[len(frac)-1] == '0' {
		frac = frac[:len(frac)-1]
	}
	out := q[:len(q)-9] + "." + frac
	if neg {
		out = "-" + out
	}
	return out
}

// bigOne 是常量 1，避免在舍入路径上反复分配。
var bigOne = big.NewInt(1)

// pow10 返回 10^n。
func pow10(n int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

// literalCategories 是可以原样写出、不必转义的 Unicode 类别。
//
// 字母、数字、空格、标点、符号原样写；其余（控制字符、格式字符、
// 未分配码位、代理项）转义成 \uXXXX。这样输出既可读，
// 又不会夹带在别处显示成乱码的不可见字符。
var literalCategories = []*unicode.RangeTable{
	unicode.Lu, unicode.Ll, unicode.Lt, unicode.Lo,
	unicode.Nd, unicode.Nl, unicode.No,
	unicode.Zs,
	unicode.Pc, unicode.Pd, unicode.Ps, unicode.Pe, unicode.Pi, unicode.Pf, unicode.Po,
	unicode.Sm, unicode.Sc, unicode.Sk, unicode.So,
}

// appendQuoted 写出一个带引号的字符串。
//
// 辅助平面的字符拆成代理对写两个 \uXXXX，而不是原样输出：
// 只处理基本平面的读取方也能读回来。
func (e *encoder) appendQuoted(s string) {
	e.buf = append(e.buf, '"')
	for _, r := range s {
		switch {
		case r == '"':
			e.buf = append(e.buf, '\\', '"')
		case r == '\\':
			e.buf = append(e.buf, '\\', '\\')
		case r == '\b':
			e.buf = append(e.buf, '\\', 'b')
		case r == '\f':
			e.buf = append(e.buf, '\\', 'f')
		case r == '\n':
			e.buf = append(e.buf, '\\', 'n')
		case r == '\r':
			e.buf = append(e.buf, '\\', 'r')
		case r == '\t':
			e.buf = append(e.buf, '\\', 't')
		case r >= 0x20 && r < 0x7F:
			e.buf = append(e.buf, byte(r))
		case r > 0xFFFF:
			hi, lo := utf16.EncodeRune(r)
			e.appendUnicodeEscape(hi)
			e.appendUnicodeEscape(lo)
		case unicode.In(r, literalCategories...):
			e.buf = append(e.buf, string(r)...)
		default:
			e.appendUnicodeEscape(r)
		}
	}
	e.buf = append(e.buf, '"')
}

// hexDigits 是转义时用的小写十六进制字符。
const hexDigits = "0123456789abcdef"

// appendUnicodeEscape 写出一个 \uXXXX 转义。
func (e *encoder) appendUnicodeEscape(r rune) {
	e.buf = append(e.buf, '\\', 'u',
		hexDigits[(r>>12)&0xF], hexDigits[(r>>8)&0xF],
		hexDigits[(r>>4)&0xF], hexDigits[r&0xF])
}
