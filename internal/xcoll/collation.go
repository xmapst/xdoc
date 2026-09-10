// Package xcoll 定义字符串的比较与排序规则。
//
// 规则是整份文件的属性：索引里的键按它排好，换一套就得重建。默认按编码单元
// 逐个比，不依赖语言数据，因而同一份文件在任何机器上都排成同样的顺序。
package xcoll

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"
)

const (
	// OptNone 不加任何调整。
	OptNone = 0

	// OptIgnoreCase 比较时不分大小写。
	OptIgnoreCase = 1

	// OptIgnoreNonSpace 忽略变音符号一类的非间距记号。
	OptIgnoreNonSpace = 2

	// OptIgnoreSymbols 忽略符号与标点。
	OptIgnoreSymbols = 4

	// OptIgnoreKanaType 不区分平假名与片假名。
	OptIgnoreKanaType = 8

	// OptIgnoreWidth 不区分半角与全角。
	OptIgnoreWidth = 16

	// OptNumericOrdering 把串里的数字段按数值比，于是 a2 排在 a10 前面。
	OptNumericOrdering = 32

	// OptStringSort 让连字符与撇号参与排序，而不是被当作可忽略的记号。
	OptStringSort = 0x20000000

	// OptOrdinal 按编码单元逐个比，不看语言习惯。
	//
	// 这是默认，也是唯一不依赖语言数据的方式：同一份文件在任何机器上排出
	// 同样的顺序，索引因此可以跨机器复用。
	OptOrdinal = 0x40000000

	// OptOrdinalIgnoreCase 按编码单元比，但先统一成大写。
	OptOrdinalIgnoreCase = 0x10000000
)

// LCIDInvariant 是不绑定任何语言的区域标识。
const LCIDInvariant = 127

// Collation 是字符串的比较与排序规则。
//
// 它是整份文件的属性，建库时定下，之后只能靠重建来改：索引里的键就是按它
// 排好的，换一套规则等于所有索引全乱。
type Collation struct {
	// lcid 是区域标识，options 是比较选项的位组合。
	lcid    int
	options int

	// ignoreCase 是从选项里预先算出来的，省得每次比较都再解一遍位。
	ignoreCase bool

	// cmp 为 nil 表示按编码单元比；非 nil 时走语言相关的比较器。
	cmp *collator
}

// Binary 按编码单元逐个比，不看语言习惯。
var Binary = Collation{lcid: LCIDInvariant, options: OptOrdinal}

// Default 是新建库时用的规则，与 [Binary] 相同。
var Default = Collation{lcid: LCIDInvariant, options: OptOrdinal}

// New 建一个排序规则，区域标识必须是认识的。
func New(lcid, options int) (Collation, error) {
	if _, ok := lcidTags[lcid]; !ok {
		return Collation{}, fmt.Errorf("xcoll: invalid LCID code %d", lcid)
	}
	return NewLenient(lcid, options), nil
}

// NewLenient 建一个排序规则，不认识的区域标识也收。
//
// 读一份已有文件时走这条：文件里记着什么就是什么，认不出来也要能打开——
// 比较会退回到语言无关的那条路，但至少数据读得出来。
func NewLenient(lcid, options int) Collation {
	c := Collation{
		lcid:       lcid,
		options:    options,
		ignoreCase: options&(OptIgnoreCase|OptOrdinalIgnoreCase) != 0,
	}
	if options&(OptOrdinal|OptOrdinalIgnoreCase) != 0 {
		return c
	}
	c.cmp = c.collator()
	return c
}

// Parse 解析 "区域/选项" 形式的规则串，比如 "en-US/IgnoreCase"。
//
// 不写斜杠就是不加选项。区域可以写语言标签，也可以直接写数字标识。
func Parse(s string) (Collation, error) {
	s = strings.TrimSpace(s)
	culture, optText, hasSlash := strings.Cut(s, "/")
	if !hasSlash {
		optText = "None"
	}
	culture = strings.TrimSpace(culture)
	id, ok := lcidOf(culture)
	if !ok {
		return Collation{}, fmt.Errorf("xcoll: invalid collation %q: unknown culture %q", s, culture)
	}
	opts, err := parseOptions(optText)
	if err != nil {
		return Collation{}, fmt.Errorf("xcoll: invalid collation %q: %w", s, err)
	}
	return New(id, opts)
}

// lcidOf 把语言标签或数字串转成区域标识。
func lcidOf(culture string) (int, bool) {
	if id, err := strconv.Atoi(culture); err == nil {
		_, ok := lcidTags[id]
		return id, ok
	}
	id, ok := lcidByTag[culture]
	return id, ok
}

// parseOptions 解析选项：可以是一个数字，也可以是逗号分隔的名字。
//
// 名字不区分大小写；认不出来的名字报错而不是忽略——一个拼错的选项名
// 静默变成"无选项"，会让整份库按错误的规则建索引。
func parseOptions(s string) (int, error) {
	if n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 32); err == nil {
		return int(n), nil
	}
	var out int
	for name := range strings.SplitSeq(s, ",") {
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "", "none":
		case "ignorecase":
			out |= OptIgnoreCase
		case "ignorenonspace":
			out |= OptIgnoreNonSpace
		case "ignoresymbols":
			out |= OptIgnoreSymbols
		case "ignorekanatype":
			out |= OptIgnoreKanaType
		case "ignorewidth":
			out |= OptIgnoreWidth
		case "numericordering":
			out |= OptNumericOrdering
		case "stringsort":
			out |= OptStringSort
		case "ordinal":
			out |= OptOrdinal
		case "ordinalignorecase":
			out |= OptOrdinalIgnoreCase
		default:
			return 0, fmt.Errorf("unknown compare option %q", name)
		}
	}
	return out, nil
}

// LCID 返回区域标识。
func (c Collation) LCID() int { return c.lcid }

// Options 返回比较选项的位组合。
func (c Collation) Options() int { return c.options }

// Ordinal 报告是不是按编码单元比。
func (c Collation) Ordinal() bool { return c.cmp == nil }

// SameLengthWhenEqual 报告"两串相等"是否蕴含"字节数相同"。
//
// 按编码单元比且区分大小写时成立。此时长度不同就一定不等，
// 比较可以先看长度直接排除掉一大半。
func (c Collation) SameLengthWhenEqual() bool { return c.cmp == nil && !c.ignoreCase }

// String 写成 "区域/选项" 形式。
//
// 有认不出来的选项位时整个退回数字：写出一份漏掉几位的名字列表，
// 会让人以为那些位不存在。
func (c Collation) String() string {
	var names []string

	var matched int

	for _, it := range [...]struct {
		bit  int
		name string
	}{
		{OptIgnoreCase, "IgnoreCase"},
		{OptIgnoreNonSpace, "IgnoreNonSpace"},
		{OptIgnoreSymbols, "IgnoreSymbols"},
		{OptIgnoreKanaType, "IgnoreKanaType"},
		{OptIgnoreWidth, "IgnoreWidth"},
		{OptNumericOrdering, "NumericOrdering"},
		{OptOrdinalIgnoreCase, "OrdinalIgnoreCase"},
		{OptStringSort, "StringSort"},
		{OptOrdinal, "Ordinal"},
	} {
		if c.options&it.bit != 0 {
			names = append(names, it.name)
			matched |= it.bit
		}
	}
	tag, ok := lcidTags[c.lcid]
	if !ok {
		tag = strconv.Itoa(c.lcid)
	}
	if c.options&^matched != 0 {
		return tag + "/" + strconv.Itoa(c.options)
	}
	if len(names) == 0 {
		names = []string{"None"}
	}
	return tag + "/" + strings.Join(names, ", ")
}

// CultureName 返回区域标识对应的语言标签，认不出来时为空串。
func (c Collation) CultureName() string { return lcidTags[c.lcid] }

// Compare 比较两个串。
func (c Collation) Compare(a, b string) int {
	if c.cmp != nil {
		return c.cmp.compare(a, b)
	}
	if c.ignoreCase {
		return cmpFold(a, b)
	}
	return cmpUnits(a, b)
}

// Equal 判断两个串按这套规则是否相等。
func (c Collation) Equal(a, b string) bool { return c.Compare(a, b) == 0 }

// cmpUnits 按 **UTF-16 编码单元**顺序比较两个 UTF-8 串。
//
// 不是按码点比：辅助平面的字符在 UTF-16 里是一对代理项，它的高位代理落在
// 0xD800..0xDBFF，因而排在 0xE000 以上的基本平面字符**前面**。按码点比会
// 把它们排到后面——那与文件里索引的排列不符。
//
// 非法 UTF-8 字节按原始字节比，不当成替换字符：不然一串不同的坏字节会
// 彼此相等。
func cmpUnits(a, b string) int {
	for len(a) > 0 && len(b) > 0 {
		if a[0] < utf8.RuneSelf && b[0] < utf8.RuneSelf {
			if a[0] != b[0] {
				return cmpByte(a[0], b[0])
			}
			a, b = a[1:], b[1:]
			continue
		}
		ra, na := utf8.DecodeRuneInString(a)
		rb, nb := utf8.DecodeRuneInString(b)
		if (na == 1 && ra == utf8.RuneError) || (nb == 1 && rb == utf8.RuneError) {
			if a[0] != b[0] {
				return cmpByte(a[0], b[0])
			}
			a, b = a[1:], b[1:]
			continue
		}
		if ra != rb {
			return cmpRune(ra, rb)
		}
		a, b = a[na:], b[nb:]
	}
	switch {
	case len(a) > 0:
		return 1
	case len(b) > 0:
		return -1
	}
	return 0
}

// cmpByte 比较两个字节，相等的情形调用方已经排除。
func cmpByte(x, y byte) int {
	if x < y {
		return -1
	}
	return 1
}

// cmpRune 按 UTF-16 编码单元序比较两个不相等的码点。
func cmpRune(x, y rune) int {
	if ux, uy := leadUnit(x), leadUnit(y); ux != uy {
		if ux < uy {
			return -1
		}
		return 1
	}
	if x < y {
		return -1
	}
	return 1
}

// leadUnit 返回一个码点在 UTF-16 里的首个编码单元，辅助平面取高位代理。
func leadUnit(r rune) rune {
	if r >= 0x10000 {
		return 0xD800 + ((r - 0x10000) >> 10)
	}
	return r
}

// cmpFold 忽略大小写比较：结果等同于两边逐字符转大写后再 [cmpUnits]，
// 但边解码边转，不复制整串。
//
// 统一成大写而不是小写：两者的映射不是互逆的，选哪一边决定了哪些字符
// 会被当作相等，这是文件格式的一部分。
//
// 非法字节原样保留、按原始字节比。它和对面的合法字符比时，比的是那个字符
// 转大写后编码的首字节；首字节相同则剩下的尾字节挂起，接着逐字节比。
func cmpFold(a, b string) int {
	ca, cb := foldCursor{s: a}, foldCursor{s: b}
	for !ca.done() && !cb.done() {
		ra, na, rawA := ca.peek()
		rb, nb, rawB := cb.peek()
		if rawA || rawB {
			if x, y := ca.takeByte(ra, na, rawA), cb.takeByte(rb, nb, rawB); x != y {
				return cmpByte(x, y)
			}
			continue
		}
		if ra != rb {
			return cmpRune(ra, rb)
		}
		ca.s, cb.s = ca.s[na:], cb.s[nb:]
	}
	switch {
	case !ca.done():
		return 1
	case !cb.done():
		return -1
	}
	return 0
}

// foldCursor 是 [cmpFold] 在一边串上的读取位置。
//
// tail[lo:hi] 是上一个字符转大写后还没比完的尾字节，全是续字节，
// 单独解码必然非法，所以只会按原始字节比。
type foldCursor struct {
	s      string
	tail   [utf8.UTFMax]byte
	lo, hi int
}

func (c *foldCursor) done() bool { return c.lo == c.hi && len(c.s) == 0 }

// peek 看当前位置的单元，不前进。
//
// raw 为真时 r 是一个原始字节（挂起的尾字节或非法字节）；否则 r 是已转成
// 大写的码点，n 是它在原串里占的字节数。
func (c *foldCursor) peek() (r rune, n int, raw bool) {
	if c.lo < c.hi {
		return rune(c.tail[c.lo]), 1, true
	}
	if x := c.s[0]; x < utf8.RuneSelf {
		if 'a' <= x && x <= 'z' {
			x -= 'a' - 'A'
		}
		return rune(x), 1, false
	}
	r, n = utf8.DecodeRuneInString(c.s)
	if n == 1 && r == utf8.RuneError {
		return rune(c.s[0]), 1, true
	}
	return unicode.ToUpper(r), n, false
}

// takeByte 取出 peek 所得单元在转大写后串里的首字节，并越过这一个字节。
func (c *foldCursor) takeByte(r rune, n int, raw bool) byte {
	switch {
	case !raw:
		m := utf8.EncodeRune(c.tail[:], r)
		c.s = c.s[n:]
		c.lo, c.hi = 1, m
		return c.tail[0]
	case c.lo < c.hi:
		c.lo++
	default:
		c.s = c.s[1:]
	}
	return byte(r)
}

// collator 是语言相关比较器的池。
//
// 比较器本身不能并发用，而建一个的代价不小，所以按规则缓存、按需取用。
type collator struct {
	pool sync.Pool
}

// compare 从池里取一个比较器比一次。
func (c *collator) compare(a, b string) int {
	cl := c.pool.Get().(*collate.Collator)
	n := cl.CompareString(a, b)
	c.pool.Put(cl)
	return n
}

// collators 按区域与选项缓存比较器池，进程内共享。
var collators sync.Map

// collatorKey 是比较器池的缓存键。
type collatorKey struct {
	lcid    int
	options int
}

// collator 取出（或建出）这套规则的比较器池。
//
// 用 LoadOrStore 收尾：两个 goroutine 同时建时只留一个，另一个丢掉——
// 比较器池不带状态，多建一个不会出错，只是浪费。
func (c Collation) collator() *collator {
	k := collatorKey{c.lcid, c.options}
	if v, ok := collators.Load(k); ok {
		return v.(*collator)
	}
	tag := c.tag()
	col := &collator{}

	col.pool.New = func() any { return collate.New(tag, c.collateOptions()...) }
	actual, _ := collators.LoadOrStore(k, col)
	return actual.(*collator)
}

// tag 把区域标识转成语言标签。
//
// 认不出来时退到未定语言，而不是报错：那时比较仍然是确定的，
// 只是不带语言习惯。
func (c Collation) tag() language.Tag {
	name, ok := lcidTags[c.lcid]
	if !ok || name == "" {
		return language.Und
	}
	t, err := language.Parse(name)
	if err != nil {
		return language.Und
	}
	return t
}

// collateOptions 把选项位翻成比较器的参数。
//
// 只有三个位有对应的实现；其余位（忽略符号、假名类型、数字排序、串排序）
// 在这条路上不起作用。
func (c Collation) collateOptions() []collate.Option {
	var out []collate.Option
	if c.options&OptIgnoreCase != 0 {
		out = append(out, collate.IgnoreCase)
	}
	if c.options&OptIgnoreNonSpace != 0 {
		out = append(out, collate.IgnoreDiacritics)
	}
	if c.options&OptIgnoreWidth != 0 {
		out = append(out, collate.IgnoreWidth)
	}
	return out
}
