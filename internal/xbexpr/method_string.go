package xbexpr

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"unicode"

	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xpage"
)

// ErrNonStringMatch 表示 MATCH 的输入或模式不是字符串。
//
// 目前没有代码返回它：MATCH 遇上非字符串直接返回 Null，见 [Ctx.mMATCH]。
var ErrNonStringMatch = fmt.Errorf("%w: MATCH requires string value and pattern", ErrExpr)

// init 登记字符串方法。
//
// 带位置和长度的那几个都按 UTF-16 码元算，不按字节也不按字符。
func init() {
	one := scalars(1)
	two := scalars(2)
	three := scalars(3)
	reg("LOWER", strMap(strings.ToLower), Info{Params: one})
	reg("UPPER", strMap(strings.ToUpper), Info{Params: one})
	reg("LTRIM", strMap(func(s string) string { return strings.TrimLeftFunc(s, unicode.IsSpace) }), Info{Params: one})
	reg("RTRIM", strMap(func(s string) string { return strings.TrimRightFunc(s, unicode.IsSpace) }), Info{Params: one})
	reg("TRIM", strMap(strings.TrimSpace), Info{Params: one})
	reg("INDEXOF", (*Ctx).mINDEXOF2, Info{Params: two})
	reg("INDEXOF", (*Ctx).mINDEXOF3, Info{Params: three})
	reg("SUBSTRING", (*Ctx).mSUBSTRING2, Info{Params: two})
	reg("SUBSTRING", (*Ctx).mSUBSTRING3, Info{Params: three})
	reg("REPLACE", (*Ctx).mREPLACE, Info{Params: three})
	reg("LPAD", mPAD(true), Info{Params: three})
	reg("RPAD", mPAD(false), Info{Params: three})
	reg("SPLIT", (*Ctx).mSPLIT2, Info{Params: two, ReturnsSeq: true})
	reg("SPLIT", (*Ctx).mSPLIT3, Info{Params: three, ReturnsSeq: true})
	reg("FORMAT", (*Ctx).mFORMAT, Info{Params: two})
	reg("JOIN", (*Ctx).mJOIN1, Info{Params: []ParamKind{ParamSeq}})
	reg("JOIN", (*Ctx).mJOIN2, Info{Params: []ParamKind{ParamSeq, ParamScalar}})
	reg("IS_MATCH", (*Ctx).mISMATCH, Info{Params: two})
	reg("MATCH", (*Ctx).mMATCH, Info{Params: three})
}

// strMap 把一个字符串到字符串的函数包成方法；输入不是字符串时返回 Null。
func strMap(f func(string) string) Method {
	return func(_ *Ctx, args []*xbson.Value) (*xbson.Value, error) {
		s, ok := str(args[0])
		if !ok {
			return xbson.Null, nil
		}
		return xbson.String(f(s)), nil
	}
}

// indexOf16 在码元序列里从 from 起找子串，返回下标；找不到返回 -1。
//
// 空的被查找串返回 from 本身。
func indexOf16(hay, needle []uint16, from int) int {
	if len(needle) == 0 {
		return from
	}
	for i := from; i+len(needle) <= len(hay); i++ {
		match := true
		for j := range needle {
			if hay[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// mINDEXOF2 从头找子串的位置，按 UTF-16 码元计。任一边不是字符串就返回 Null。
func (*Ctx) mINDEXOF2(args []*xbson.Value) (*xbson.Value, error) {
	v, ok1 := str(args[0])
	s, ok2 := str(args[1])
	if !ok1 || !ok2 {
		return xbson.Null, nil
	}
	return xbson.Int32(int32(indexOf16(utf16Of(v), utf16Of(s), 0))), nil
}

// mINDEXOF3 从指定位置起找子串。起点越界报错——那与「找不到」是两回事。
func (*Ctx) mINDEXOF3(args []*xbson.Value) (*xbson.Value, error) {
	v, ok1 := str(args[0])
	s, ok2 := str(args[1])
	if !ok1 || !ok2 || !isNumber(args[2]) {
		return xbson.Null, nil
	}
	start, err := int32Of(args[2])
	if err != nil {
		return nil, err
	}
	hay := utf16Of(v)
	if start < 0 || int(start) > len(hay) {
		return nil, errf("INDEXOF: start index %d is outside [0, %d]", start, len(hay))
	}
	return xbson.Int32(int32(indexOf16(hay, utf16Of(s), int(start)))), nil
}

// mSUBSTRING2 取从某位置到末尾的子串，位置按 UTF-16 码元计。越界报错。
func (*Ctx) mSUBSTRING2(args []*xbson.Value) (*xbson.Value, error) {
	v, ok := str(args[0])
	if !ok || !isNumber(args[1]) {
		return xbson.Null, nil
	}
	start, err := int32Of(args[1])
	if err != nil {
		return nil, err
	}
	u := utf16Of(v)
	if start < 0 || int(start) > len(u) {
		return nil, errf("SUBSTRING: start index %d is outside [0, %d]", start, len(u))
	}
	return xbson.String(utf16Str(u[start:])), nil
}

// mSUBSTRING3 取指定位置起、指定长度的子串。
//
// 长度不会自动截断：起点加长度越过末尾就报错。
func (*Ctx) mSUBSTRING3(args []*xbson.Value) (*xbson.Value, error) {
	v, ok := str(args[0])
	if !ok || !isNumber(args[1]) || !isNumber(args[2]) {
		return xbson.Null, nil
	}
	start, err := int32Of(args[1])
	if err != nil {
		return nil, err
	}
	length, err := int32Of(args[2])
	if err != nil {
		return nil, err
	}
	u := utf16Of(v)
	if start < 0 || int(start) > len(u) {
		return nil, errf("SUBSTRING: start index %d is outside [0, %d]", start, len(u))
	}
	if length < 0 || int(start)+int(length) > len(u) {
		return nil, errf("SUBSTRING: length %d starting at %d is outside the string", length, start)
	}
	return xbson.String(utf16Str(u[start : int(start)+int(length)])), nil
}

// mREPLACE 替换全部出现。被替换的串为空时报错——那会替换出无穷多次。
func (*Ctx) mREPLACE(args []*xbson.Value) (*xbson.Value, error) {
	v, ok1 := str(args[0])
	oldS, ok2 := str(args[1])
	newS, ok3 := str(args[2])
	if !ok1 || !ok2 || !ok3 {
		return xbson.Null, nil
	}
	if oldS == "" {
		return nil, errf("REPLACE: the searched value cannot be empty")
	}
	return xbson.String(strings.ReplaceAll(v, oldS, newS)), nil
}

// mPAD 造出 LPAD 或 RPAD。
//
// **只用填充串的第一个码元**重复补齐，后面的字符不参与。原串已经够长时原样返回。
// 宽度为负、超出上限，或者填充串为空，都报错。
func mPAD(left bool) Method {
	name := "RPAD"
	if left {
		name = "LPAD"
	}
	return func(_ *Ctx, args []*xbson.Value) (*xbson.Value, error) {
		v, ok1 := str(args[0])
		padS, ok2 := str(args[2])
		if !ok1 || !ok2 || !isNumber(args[1]) {
			return xbson.Null, nil
		}
		width, err := int32Of(args[1])
		if err != nil {
			return nil, err
		}
		if width < 0 {
			return nil, errf("%s: width %d must not be negative", name, width)
		}

		if int64(width) > maxPadWidth {
			return nil, errf("%s: width %d exceeds the limit of %d", name, width, maxPadWidth)
		}
		pad := utf16Of(padS)
		if len(pad) == 0 {
			return nil, errf("%s: padding string must not be empty", name)
		}
		u := utf16Of(v)
		if int(width) <= len(u) {
			return xbson.String(v), nil
		}
		fill := make([]uint16, int(width)-len(u))
		for i := range fill {
			fill[i] = pad[0]
		}
		if left {
			return xbson.String(utf16Str(append(fill, u...))), nil
		}
		return xbson.String(utf16Str(append(u, fill...))), nil
	}
}

// maxPadWidth 是补齐宽度的上限。补出来的串终归要装进一篇文档，比这更宽没有意义。
const maxPadWidth = xpage.MaxDocumentSize

// mSPLIT2 按分隔串切分。
func (*Ctx) mSPLIT2(args []*xbson.Value) (*xbson.Value, error) {
	return splitPlain(args[0], args[1]), nil
}

// splitPlain 按分隔串切分，**空的片段一律丢掉**。
//
// 分隔串为空时不切，原样产出一项；原串也为空则产出空串。
func splitPlain(valueV, sepV *xbson.Value) *xbson.Value {
	v, ok1 := str(valueV)
	sep, ok2 := str(sepV)
	if !ok1 || !ok2 {
		return emptySeq()
	}
	if sep == "" {
		if v == "" {
			return emptySeq()
		}
		return seqOf([]*xbson.Value{xbson.String(v)})
	}
	var out []*xbson.Value
	for part := range strings.SplitSeq(v, sep) {
		if part == "" {
			continue
		}
		out = append(out, xbson.String(part))
	}
	return seqOf(out)
}

// mSPLIT3 切分，第三个实参为真时把分隔串当正则。
//
// 正则切分与普通切分行为不同：空片段会保留，捕获组的内容也会作为片段产出。
func (*Ctx) mSPLIT3(args []*xbson.Value) (*xbson.Value, error) {
	useRegex, ok := args[2].AsBoolean()
	if !ok || !useRegex {
		return splitPlain(args[0], args[1]), nil
	}
	v, ok1 := str(args[0])
	pattern, ok2 := str(args[1])
	if !ok1 || !ok2 {
		return emptySeq(), nil
	}
	re, err := compileRegex(pattern)
	if err != nil {
		return nil, err
	}
	var out []*xbson.Value
	last := 0
	for _, m := range re.FindAllStringSubmatchIndex(v, -1) {
		out = append(out, xbson.String(v[last:m[0]]))
		for g := 1; g*2 < len(m); g++ {
			if m[g*2] < 0 {
				continue
			}
			out = append(out, xbson.String(v[m[g*2]:m[g*2+1]]))
		}
		last = m[1]
	}
	out = append(out, xbson.String(v[last:]))
	return seqOf(out), nil
}

// mJOIN1 把一串值直接拼起来，不加分隔。
func (*Ctx) mJOIN1(args []*xbson.Value) (*xbson.Value, error) {
	return joinValues(items(args[0]), ""), nil
}

// mJOIN2 用指定分隔串把一串值拼起来。分隔串不是字符串时返回 Null。
func (*Ctx) mJOIN2(args []*xbson.Value) (*xbson.Value, error) {
	sep, ok := str(args[1])
	if !ok {
		return xbson.Null, nil
	}
	return joinValues(items(args[0]), sep), nil
}

// joinValues 把各项转成文本再拼接。
func joinValues(vals []*xbson.Value, sep string) *xbson.Value {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = stringOf(v)
	}
	return xbson.String(strings.Join(parts, sep))
}

// mISMATCH 判断字符串是否匹配正则。有一边不是字符串就是假；正则编不出来才报错。
func (*Ctx) mISMATCH(args []*xbson.Value) (*xbson.Value, error) {
	v, ok1 := str(args[0])
	pattern, ok2 := str(args[1])
	if !ok1 || !ok2 {
		return xbson.False, nil
	}
	re, err := compileRegex(pattern)
	if err != nil {
		return nil, err
	}
	return xbson.Boolean(re.MatchString(v)), nil
}

// mMATCH 取正则匹配里某个捕获组的内容。
//
// 组可以按序号给，也可以按名字给。整串匹配不上返回 Null；
// 匹配上了但那个组不存在，返回空串。
func (*Ctx) mMATCH(args []*xbson.Value) (*xbson.Value, error) {
	v, ok1 := str(args[0])
	pattern, ok2 := str(args[1])
	if !ok1 || !ok2 {
		return xbson.Null, nil
	}
	re, err := compileRegex(pattern)
	if err != nil {
		return nil, err
	}
	m := re.FindStringSubmatch(v)
	if m == nil {
		return xbson.Null, nil
	}
	group := args[2]
	switch {
	case isNumber(group):
		i, err := int32Of(group)
		if err != nil {
			return nil, err
		}
		if i < 0 || int(i) >= len(m) {
			return xbson.String(""), nil
		}
		return xbson.String(m[i]), nil
	case isString(group):
		name, _ := group.AsString()
		for i, n := range re.SubexpNames() {
			if n == name && i < len(m) {
				return xbson.String(m[i]), nil
			}
		}
		return xbson.String(""), nil
	default:
		return xbson.Null, nil
	}
}

// regexCacheMax 是正则缓存最多留的条数。
//
// 模式串多半是查询里的常量，全表扫描时每行都拿同一个串来编译；
// 设上限是防着参数化的模式源源不断地往里塞。
const regexCacheMax = 256

// regexCache 按原始模式串缓存编译结果。[regexp.Regexp] 本身可以并发用。
//
// 改写具名组是纯函数，按原串作键与按改写后的串作键等价，还省掉命中时的一次改写。
// 编不出来的模式不缓存，每次照常报错。
var regexCache struct {
	sync.RWMutex
	m map[string]*regexp.Regexp
}

// compileRegex 编译一个正则，先把具名捕获组换成本地的写法。
func compileRegex(pattern string) (*regexp.Regexp, error) {
	regexCache.RLock()
	re, ok := regexCache.m[pattern]
	regexCache.RUnlock()
	if ok {
		return re, nil
	}

	re, err := regexp.Compile(rewriteNamedGroups(pattern))
	if err != nil {
		return nil, errf("invalid regular expression %q: %v", pattern, err)
	}

	regexCache.Lock()
	defer regexCache.Unlock()
	if regexCache.m == nil {
		regexCache.m = make(map[string]*regexp.Regexp, regexCacheMax)
	}
	if _, dup := regexCache.m[pattern]; !dup && len(regexCache.m) >= regexCacheMax {
		// 满了随手扔掉一条，map 的遍历顺序本来就是乱的。
		for k := range regexCache.m {
			delete(regexCache.m, k)
			break
		}
	}
	regexCache.m[pattern] = re
	return re, nil
}

// rewriteNamedGroups 把 (?<名字> 改写成 (?P<名字>。
//
// 后顾断言 (?<= 和 (?<! 不能碰，它们只是恰好也以 (?< 开头。
func rewriteNamedGroups(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		if strings.HasPrefix(p[i:], "(?<") && !strings.HasPrefix(p[i:], "(?<=") && !strings.HasPrefix(p[i:], "(?<!") {
			b.WriteString("(?P<")
			i += 2
			continue
		}
		b.WriteByte(p[i])
	}
	return b.String()
}
