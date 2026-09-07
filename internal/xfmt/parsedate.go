package xfmt

import (
	"slices"
	"strings"
	"unicode"
)

// DateParts 是从一个日期串里解出来的各部分，还没有拼成时刻。
//
// 年为 0 表示输入里没写年；[DateParts.NoDate] 表示整个日期部分都没写。
// 分开来给，是因为「补哪一年」「按哪个时区」要由调用方定。
type DateParts struct {
	// 年月日。年为 0 表示输入里没写。
	Year, Month, Day int
	Hour, Min, Sec   int

	// Frac 是秒的小数部分，单位是 100 纳秒，最多七位。
	Frac int

	// Zone 说明输入里带没带时区，OffsetMinutes 是偏移的分钟数。
	Zone ZoneKind

	OffsetMinutes int

	// NoDate 表示输入里只有时间。
	NoDate bool

	// ISO 表示输入走的是 ISO 那条路（中间有个 T）。
	ISO bool

	// Weekday 是输入里写出来的星期几，-1 表示没写。
	Weekday int
}

// ZoneKind 说明输入里带没带时区信息。
type ZoneKind int

const (
	// ZoneNone 没写时区，ZoneUTC 写了 Z 或 GMT，ZoneOffset 写了 ±HH:MM。
	ZoneNone ZoneKind = iota
	ZoneUTC
	ZoneOffset
)

// ParseDate 按这套语言规则解析一个日期串。
//
// 先切成记号，再拼成各部分。收的写法很宽：各语言的月名、星期名、
// 上下午标记、CJK 的年月日、ISO 形式都认。
func (df *DateTimeFormat) ParseDate(s string) (DateParts, bool) {
	toks, ok := df.lexDate(s)
	if !ok {
		return DateParts{}, false
	}
	return df.assembleDate(toks)
}

// ambigSegMax 是尝试消歧时最多处理几段。
const ambigSegMax = 8

// ambigSegs 找出含有歧义记号的段号。
//
// 日期分隔符与时间分隔符相同的语言里，一个分隔符到底算哪种要试；
// 按段分组是因为同一段（两个空白之间）里的分隔符应当作同一种解读。
//
// 目前没有调用点：主路径 [DateTimeFormat.dtAssemble] 已经在状态机里
// 逐个回退处理了歧义。
func ambigSegs(toks []dtok) []int {
	var out []int
	for _, t := range toks {
		if t.ambig && (len(out) == 0 || out[len(out)-1] != t.seg) {
			out = append(out, t.seg)
		}
	}
	return out
}

// assembleAmbig 按位掩码指定的段把歧义记号当作日期分隔符，再试一次拼装。
//
// buf 由调用方提供，好在多次尝试之间复用。目前没有调用点，
// 理由同 [ambigSegs]。
func (df *DateTimeFormat) assembleAmbig(buf, toks []dtok, segs []int, mask int) (DateParts, bool) {
	copy(buf, toks)
	for i := range buf {
		if !buf[i].ambig {
			continue
		}
		bit := slices.Index(segs, buf[i].seg)
		if mask == -1 || mask&(1<<bit) != 0 {
			buf[i].kind = dtDateSep
		}
	}
	return df.assembleDate(buf)
}

// dtokKind 是切出来的记号种类。
type dtokKind int

const (
	// 记号种类。dtMark 是 CJK 的年月日时分秒那类后缀，
	// dtIgnorable 是这种语言模板里出现过、解析时可以跳过的字面词。
	dtNum dtokKind = iota
	dtMonth
	dtDayName
	dtAmPm
	dtEra
	dtDateSep
	dtTimeSep
	dtDash
	dtPlus
	dtDot
	dtComma
	dtSpace
	dtT
	dtZulu

	dtMark

	dtIgnorable
)

// dtok 是一个切出来的记号。
type dtok struct {
	kind dtokKind

	// num 是数值（数字的值、月份序号、星期序号、上下午的 0/1……），
	// digits 是数字的位数，raw 是分隔符的原字符。
	//
	// 位数要留着：三位以上的数字只能是年份，两位以内的才可能是月或日。
	num    int64
	digits int
	raw    rune

	// ambig 表示这个分隔符的含义有歧义（日期与时间分隔符相同）。
	ambig bool

	// seg 是段号，按空白切分，同一段里的记号连在一起。
	seg int
}

// dtoks 是一串记号。
type dtoks []dtok

// lexDate 把日期串切成记号。
//
// 跳过双向排版用的不可见控制字符：它们在从右往左书写的语言里很常见，
// 留着会让后面每一处比对都要绕开它们。
//
// 分隔符只在「刚读过一个数字或月名」的位置才认——不然一个句点会
// 在任何地方都被当成日期分隔符。
//
// `#` 是一种历史包袱：整串必须恰好被一对 `#` 包住，见 [verifyHashes]。
// 末尾的 0 字节也容忍，但后面不能再有别的东西。
//
// 最后按空白给每个记号编段号，供消歧用。
func (df *DateTimeFormat) lexDate(s string) (dtoks, bool) {
	var out dtoks
	r := []rune(s)
	for i := 0; i < len(r); {
		c := r[i]

		if c == '\u200e' || c == '\u200f' || c == '\u061c' {
			i++
			continue
		}

		k, ok := dateMarks[c]
		if !ok && df.LangName == "ko" {
			k, ok = koreanMarks[c]
		}
		if ok && len(out) > 0 && out[len(out)-1].kind == dtNum &&
			(i+1 >= len(r) || !unicode.IsLetter(r[i+1])) {
			out = append(out, dtok{kind: dtMark, num: int64(k)})
			i++
			continue
		}

		sepPos := len(out) > 0 &&
			(out[len(out)-1].kind == dtNum || out[len(out)-1].kind == dtMonth ||
				(out[len(out)-1].kind == dtSpace && len(out) > 1 &&
					(out[len(out)-2].kind == dtNum || out[len(out)-2].kind == dtMonth)))

		if df.FrenchCanadianSuffixes && sepPos {
			if k, n := matchFrCASuffix(r[i:]); n > 0 {
				out = append(out, dtok{kind: dtMark, num: int64(k)})
				i += n
				continue
			}
		}
		if tok, n, ok := df.lexWord(r[i:], sepPos); ok {
			out = append(out, tok)
			i += n
			continue
		}

		if sepPos {
			if n := matchLit(r[i:], df.TimeSeparator); n > 0 {
				out = append(out, dtok{kind: dtTimeSep, raw: sepRune(df.TimeSeparator),
					ambig: df.TimeSeparator == df.DateSeparator})
				i += n
				continue
			}

			if !df.DateSepIgnorable() && df.DateSeparator != "-" {
				n := matchLit(r[i:], df.DateSeparator)
				lit := df.DateSeparator
				if n == 0 {
					lit = strings.TrimSpace(df.DateSeparator)
					n = matchLit(r[i:], lit)
				}
				if n > 0 {
					out = append(out, dtok{kind: dtDateSep, raw: sepRune(lit)})
					i += n
					continue
				}
			}
		}
		switch {
		case unicode.IsSpace(c):
			for i < len(r) && unicode.IsSpace(r[i]) {
				i++
			}
			out = append(out, dtok{kind: dtSpace})
		case c >= '0' && c <= '9':
			n, d := int64(0), 0
			for i < len(r) && r[i] >= '0' && r[i] <= '9' {
				if n < 1<<40 {
					n = n*10 + int64(r[i]-'0')
				}
				d++
				i++
			}
			out = append(out, dtok{kind: dtNum, num: n, digits: d})
		case c == ':':
			i++
			out = append(out, dtok{kind: dtTimeSep, raw: ':'})
		case c == '/':
			i++
			out = append(out, dtok{kind: dtDateSep})
		case c == '-':
			i++
			out = append(out, dtok{kind: dtDash})
		case c == '+':
			i++
			out = append(out, dtok{kind: dtPlus})
		case c == '.':
			i++
			out = append(out, dtok{kind: dtDot, raw: '.'})
		case c == ',':
			i++
			out = append(out, dtok{kind: dtComma, raw: ','})
		case c == '#':
			if !verifyHashes(r) {
				return nil, false
			}
			i++
			out = append(out, dtok{kind: dtSpace})
		case c == 0:
			for _, c := range r[i:] {
				if c != 0 {
					return nil, false
				}
			}
			i = len(r)
		default:
			return nil, false
		}
	}
	seg := 0
	for i := range out {
		if out[i].kind == dtSpace {
			seg++
		}
		out[i].seg = seg
	}
	return out, true
}

// verifyHashes 检查 `#` 的用法：整串必须恰好有两个，中间夹着内容，
// 外面只允许空白与末尾的 0 字节。
func verifyHashes(r []rune) bool {
	first, second := false, false
	for _, c := range r {
		switch {
		case c == '#':
			switch {
			case !first:
				first = true
			case !second:
				second = true
			default:
				return false
			}
		case c == 0:
			if !second {
				return false
			}
		case !unicode.IsSpace(c):
			if !first || second {
				return false
			}
		}
	}
	return second
}

// maxZoneMinutes 是时区偏移的绝对值上限。
const maxZoneMinutes = 14 * 60

// zoneInRange 报告时区偏移在不在允许范围内。
func (p DateParts) zoneInRange() bool {
	return p.Zone != ZoneOffset || (p.OffsetMinutes >= -maxZoneMinutes && p.OffsetMinutes <= maxZoneMinutes)
}

// sepRune 取出单字符分隔符的那个字符，多字符的给 0。
func sepRune(lit string) rune {
	r := []rune(lit)
	if len(r) != 1 {
		return 0
	}
	return r[0]
}

// matchLit 从头精确匹配一个字面量，返回吃掉几个字符，不匹配给 0。
func matchLit(r []rune, lit string) int {
	if lit == "" {
		return 0
	}
	n := 0
	for _, want := range lit {
		if n >= len(r) || r[n] != want {
			return 0
		}
		n++
	}
	return n
}

// dateMarks 是 CJK 的年月日时分秒后缀，值是成分编号。
var dateMarks = map[rune]int{
	'年': 0, '년': 0,
	'月': 1, '월': 1,
	'日': 2, '일': 2,
	'時': 3, '时': 3, '分': 4, '秒': 5,
}

// koreanMarks 是韩语的时分秒后缀。
//
// 单列出来是因为「분」「초」在别的语境下不是时间后缀，
// 只有语言标签是韩语时才这么认。
var koreanMarks = map[rune]int{'시': 3, '분': 4, '초': 5}

// lexWord 试着从开头读出一个词：月名、星期名、上下午标记、纪元名，
// 或者模板里出现过的可跳过字面词。
//
// **取匹配最长的那个**：缩写月名往往是全名的前缀，先匹配到短的就会
// 把后面的字母留在串里。
//
// 匹配结果后面不能紧跟字母：不然 "Marches" 会被读成三月加一个 "es"。
//
// 除了这种语言自己的名字，英文的月名与星期名也一并认——很多输入
// 就是英文写的。
//
// 数字开头的词只有在这种语言允许「数字前缀」时才当月名，
// 否则判失败：不然 "3rd" 里的数字会被吃进月名。
func (df *DateTimeFormat) lexWord(r []rune, sepPos bool) (dtok, int, bool) {
	text := string(r)

	fits := func(n int) bool {
		return !unicode.IsLetter(r[0]) || n >= len(r) || !unicode.IsLetter(r[n])
	}
	if sepPos {
		best, num := 0, int64(0)

		var dotAM, dotPM string
		if df.LangName == "sq" {
			dotAM, dotPM = "."+df.AMDesignator, "."+df.PMDesignator
		}
		for _, c := range [6]struct {
			name string
			num  int64
		}{{df.AMDesignator, 0}, {df.PMDesignator, 1}, {dotAM, 0}, {dotPM, 1}, {"AM", 0}, {"PM", 1}} {
			if c.name == "" || len(c.name) > len(text) || len([]rune(c.name)) <= best {
				continue
			}
			if df.eqName(text[:len(c.name)], c.name) && fits(len([]rune(c.name))) {
				best, num = len([]rune(c.name)), c.num
			}
		}
		if best > 0 {
			return dtok{kind: dtAmPm, num: num}, best, true
		}

		if (r[0] == 'T' || (r[0] == 't' && !df.OrdinalTokens)) && fits(1) {
			return dtok{kind: dtT}, 1, true
		}
	}

	type cand struct {
		name string
		tok  dtok
	}

	var cands []cand
	cands = append(cands,
		cand{df.AMDesignator, dtok{kind: dtAmPm, num: 0}},
		cand{df.PMDesignator, dtok{kind: dtAmPm, num: 1}})

	for _, sfx := range df.MonthPostfixes() {
		for i := range df.MonthNames {
			cands = append(cands,
				cand{df.MonthNames[i] + sfx, dtok{kind: dtMonth, num: int64(i + 1)}},
				cand{df.AbbreviatedMonthNames[i], dtok{kind: dtMonth, num: int64(i + 1)}})
		}
	}
	for _, w := range df.DateWords() {
		cands = append(cands, cand{w, dtok{kind: dtIgnorable}})

		if df.LangName == "eu" {
			cands = append(cands, cand{"." + w, dtok{kind: dtIgnorable}})
		}
	}
	for i, n := range df.MonthNames {
		cands = append(cands, cand{n, dtok{kind: dtMonth, num: int64(i + 1)}})
	}
	for i, n := range df.AbbreviatedMonthNames {
		cands = append(cands, cand{n, dtok{kind: dtMonth, num: int64(i + 1)}})
	}
	for i, n := range df.MonthGenitiveNames {
		cands = append(cands, cand{n, dtok{kind: dtMonth, num: int64(i + 1)}})
	}
	for i, n := range df.AbbreviatedMonthGenitiveNames {
		cands = append(cands, cand{n, dtok{kind: dtMonth, num: int64(i + 1)}})
	}
	for i, n := range df.DayNames {
		cands = append(cands, cand{n, dtok{kind: dtDayName, num: int64(i)}})
	}
	for i, n := range df.AbbreviatedDayNames {
		cands = append(cands, cand{n, dtok{kind: dtDayName, num: int64(i)}})
	}
	cands = append(cands,
		cand{df.EraName, dtok{kind: dtEra}},
		cand{df.AbbrEraName, dtok{kind: dtEra}},

		cand{"AM", dtok{kind: dtAmPm, num: 0}},
		cand{"PM", dtok{kind: dtAmPm, num: 1}})

	for i, n := range invariantMonthNames {
		cands = append(cands, cand{n, dtok{kind: dtMonth, num: int64(i + 1)}},
			cand{invariantAbbrMonthNames[i], dtok{kind: dtMonth, num: int64(i + 1)}})
	}
	for i, n := range invariantDayNames {
		cands = append(cands, cand{n, dtok{kind: dtDayName, num: int64(i)}},
			cand{invariantAbbrDayNames[i], dtok{kind: dtDayName, num: int64(i)}})
	}

	best, bestTok := 0, dtok{}
	for _, c := range cands {
		if c.name == "" || len([]rune(c.name)) <= best {
			continue
		}
		if df.eqName(text[:min(len(text), len(c.name))], c.name) &&
			fits(len([]rune(c.name))) {
			best, bestTok = len([]rune(c.name)), c.tok
		}
	}
	if best > 0 {
		if bestTok.kind == dtMonth && r[0] >= '0' && r[0] <= '9' &&
			df.FormatFlags&flagDigitPrefix == 0 {
			return dtok{}, 0, false
		}
		return bestTok, best, true
	}

	if r[0] >= '0' && r[0] <= '9' {
		return dtok{}, 0, false
	}

	if len(r) >= 3 && df.eqName(string(r[:3]), "GMT") && fits(3) {
		return dtok{kind: dtZulu, digits: 3}, 3, true
	}
	switch {
	case r[0] == 'T' || (r[0] == 't' && !df.OrdinalTokens):
		return dtok{kind: dtT}, 1, true
	case r[0] == 'Z' || (r[0] == 'z' && !df.OrdinalTokens):
		return dtok{kind: dtZulu}, 1, true
	}
	return dtok{}, 0, false
}

// assembleDate 把记号拼成各部分。
//
// 串里出现过 T 就走 ISO 那条路，否则走状态机那条路——两者的规则
// 差别很大，混在一起会让任何一边都不准。
func (df *DateTimeFormat) assembleDate(toks dtoks) (DateParts, bool) {
	for _, t := range toks {
		if t.kind == dtT {
			return df.assembleISO(toks)
		}
	}
	return df.dtAssemble(toks)
}

// assembleISO 按 ISO 形式拼装：日期、T、时间、时区。
//
// 比状态机那条路严格：时分秒必须两位，分隔符必须是冒号。
// 宽松一点会让 "2024-1-2T3:4" 这种半吊子写法也通过，
// 而它在别的实现里读不出来。
func (df *DateTimeFormat) assembleISO(toks dtoks) (DateParts, bool) {
	b := &dateBuilder{df: df, dayName: -1, ampm: -1, monthPos: -1}
	for i := 0; i < len(toks); {
		t := toks[i]
		switch t.kind {
		case dtSpace, dtComma, dtDateSep, dtDot, dtIgnorable:
			i++
		case dtT:
			if len(b.nums) == 0 || b.marked {
				return DateParts{}, false
			}

			b.isoT = true
			i++
		case dtMark:
			if !b.readMark(int(t.num)) {
				return DateParts{}, false
			}
			i++
		case dtDash, dtPlus:
			if n, ok := b.readZone(toks, i); ok {
				i = n
				continue
			}
			if t.kind == dtPlus {
				return DateParts{}, false
			}
			i++
		case dtEra:
			i++
		case dtDayName:
			if b.dayName >= 0 {
				return DateParts{}, false
			}
			b.dayName = int(t.num)
			i++
		case dtMonth:
			if b.month != 0 {
				return DateParts{}, false
			}
			b.month, b.monthPos = int(t.num), len(b.nums)
			b.digitMonth = t.digits == 1
			i++
		case dtAmPm:
			if b.ampm >= 0 || b.isoT {
				return DateParts{}, false
			}

			if !b.hasTime && len(b.pre) == 0 && len(b.nums) == 1 {
				if b.nums[0].digits > 2 || b.nums[0].num > 23 {
					return DateParts{}, false
				}
				b.hasTime, b.hour = true, int(b.nums[0].num)
				b.nums = b.nums[:0]
			}
			b.ampm = int(t.num)
			i++
		case dtZulu:
			if b.zone != ZoneNone || (b.isoT && t.digits == 3) {
				return DateParts{}, false
			}
			b.zone = ZoneUTC
			i++
		case dtNum:
			if j := toks.skipSpace(i + 1); j < len(toks) && toks[j].kind == dtTimeSep {
				n, ok := b.readTime(toks, i)
				if !ok {
					return DateParts{}, false
				}
				i = n
				continue
			}

			if len(b.nums) >= 4 || t.digits > 8 {
				return DateParts{}, false
			}
			b.nums = append(b.nums, t)
			i++
		default:
			return DateParts{}, false
		}
	}
	return b.finish()
}

// skipSpace 跳过空白记号，返回下一个非空白的位置。
func (toks dtoks) skipSpace(i int) int {
	for i < len(toks) && toks[i].kind == dtSpace {
		i++
	}
	return i
}

// dateBuilder 是 ISO 那条路的拼装状态。
//
// nums 攒着还没定含义的数字，pre 是遇到 CJK 后缀之前攒下的那些，
// cjk 开头的是靠后缀直接定下来的年月日。
type dateBuilder struct {
	df *DateTimeFormat

	// nums 攒着还没定含义的数字；month、monthPos 是月名的值与它出现在第几个数字之前。
	nums     dtoks
	month    int
	monthPos int
	dayName  int

	// hasTime 表示时间部分已经读到，isoT 表示串里出现过 T。
	hasTime bool
	isoT    bool

	// digitMonth 表示月名其实是从数字开头的词里认出来的。
	//
	// 那种月名后面能跟什么，规则更紧。
	digitMonth bool

	// marked 表示用过 CJK 后缀；cjk 开头的是靠后缀直接定下来的成分。
	marked           bool
	cjkY, cjkM, cjkD int
	hour, min, sec   int
	frac             int
	ampm             int

	// pre 是遇到第一个后缀之前攒下的数字；zone、offMin 是时区信息。
	pre    dtoks
	zone   ZoneKind
	offMin int
}

// readMark 处理一个 CJK 后缀：把它前面那个数字直接定到对应成分上。
//
// 更早攒下的数字挪进 pre——「2024 年 3 月」里，后缀之前可能还有别的成分。
func (b *dateBuilder) readMark(k int) bool {
	if b.isoT || len(b.nums) == 0 {
		return false
	}
	n := b.nums[len(b.nums)-1]

	b.pre = append(b.pre, b.nums[:len(b.nums)-1]...)
	b.nums = b.nums[:0]
	b.marked = true
	switch k {
	case 0:
		if b.cjkY != 0 {
			return false
		}
		y, ok := b.yearOf(n)
		b.cjkY = y
		return ok
	case 1:
		b.cjkM = int(n.num)
		return b.cjkM >= 1 && b.cjkM <= 12
	case 2:
		b.cjkD = int(n.num)
		return b.cjkD >= 1 && b.cjkD <= 31
	case 3:
		b.hasTime, b.hour = true, int(n.num)
		return b.hour <= 23
	case 4:
		b.min = int(n.num)
		return b.hasTime && b.min <= 59
	case 5:
		b.sec = int(n.num)
		return b.hasTime && b.sec <= 59
	}
	return false
}

// readTime 读出「时:分[:秒[.小数]]」。
//
// ISO 那条路要求每段恰好两位、分隔符必须是冒号。
//
// 小数点后面没有数字时：逗号可以（那是句读），点不行（那是个残缺的小数）。
func (b *dateBuilder) readTime(toks dtoks, i int) (int, bool) {
	if b.hasTime {
		return 0, false
	}
	b.hasTime = true
	b.hour = int(toks[i].num)
	iso := b.isoT

	if toks[i].digits > 2 || (iso && toks[i].digits != 2) {
		return 0, false
	}
	i = toks.skipSpace(i + 1)
	if i >= len(toks) || toks[i].kind != dtTimeSep || !toks[i].isoTimeSep(iso) {
		return 0, false
	}
	i = toks.skipSpace(i + 1)
	if i >= len(toks) || toks[i].kind != dtNum {
		return 0, false
	}
	b.min = int(toks[i].num)
	if toks[i].digits > 2 || (iso && toks[i].digits != 2) {
		return 0, false
	}
	i++
	if j := toks.skipSpace(i); j < len(toks) && toks[j].kind == dtTimeSep {
		if !toks[j].isoTimeSep(iso) {
			return 0, false
		}
		j = toks.skipSpace(j + 1)
		if j >= len(toks) || toks[j].kind != dtNum {
			return 0, false
		}
		b.sec = int(toks[j].num)
		if toks[j].digits > 2 || (iso && toks[j].digits != 2) {
			return 0, false
		}
		i = j + 1

		if i < len(toks) && (toks[i].raw == '.' || toks[i].raw == ',') {
			if i+1 < len(toks) && toks[i+1].kind == dtNum {
				b.frac = toks[i+1].fracTicks()
				i += 2
			} else if toks[i].raw == '.' {
				return 0, false
			}
		}
	}
	if b.hour > 23 || b.min > 59 || b.sec > 59 {
		return 0, false
	}
	return i, true
}

// fracTicks 把秒的小数部分转成 100 纳秒计数。
//
// 超过八位直接截断；正好八位时**四舍五入**到七位——第八位是能看见的
// 最后一位有效信息，直接截掉会让 .99999995 变成 .9999999。
func (t dtok) fracTicks() int {
	v, d := t.num, t.digits
	for d > 8 {
		v /= 10
		d--
	}
	if d == 8 {
		v = (v + 5) / 10
		d = 7
	}
	for d < 7 {
		v *= 10
		d++
	}

	return int(v)
}

// isoTimeSep 报告一个时间分隔符在 ISO 那条路上能不能接受，只有冒号可以。
func (t dtok) isoTimeSep(iso bool) bool {
	return !iso || t.raw == 0 || t.raw == ':'
}

// readZone 读出 ISO 那条路上的时区偏移，读完之后必须就是串尾。
//
// 四位数字（如 +0800）只在已经读到时间、或数字攒够三个时才拆成时与分：
// 不然 "2024-0800" 里的后半截会被误当成时区。
func (b *dateBuilder) readZone(toks dtoks, i int) (int, bool) {
	if b.zone != ZoneNone {
		return 0, false
	}
	sign := 1
	if toks[i].kind == dtDash {
		sign = -1
	}
	j := toks.skipSpace(i + 1)
	if j >= len(toks) || toks[j].kind != dtNum {
		return 0, false
	}
	hh, mm := toks[j].num, int64(0)
	end := j + 1
	switch {
	case end < len(toks) && toks[end].kind == dtTimeSep &&
		end+1 < len(toks) && toks[end+1].kind == dtNum:
		mm = toks[end+1].num
		end += 2
	case !b.hasTime && len(b.nums) < 3:
		return 0, false
	case toks[j].digits == 4:
		hh, mm = toks[j].num/100, toks[j].num%100
	}
	if hh > 14 || mm > 59 {
		return 0, false
	}

	if toks.skipSpace(end) != len(toks) {
		return 0, false
	}
	b.zone, b.offMin = ZoneOffset, sign*int(hh*60+mm)
	return end, true
}

// readZoneNet 读出状态机那条路上的时区偏移。
//
// 与 [dateBuilder.readZone] 的区别：不要求读完就是串尾——状态机那条路上
// 时区后面还可能跟别的成分。
func (b *dateBuilder) readZoneNet(toks dtoks, i int) (int, bool) {
	if b.zone != ZoneNone {
		return 0, false
	}
	sign := 1
	if toks[i].kind == dtDash {
		sign = -1
	}
	j := i + 1
	if j >= len(toks) || toks[j].kind != dtNum {
		return 0, false
	}
	hh, mm := toks[j].num, int64(0)
	end := j + 1
	switch toks[j].digits {
	case 1, 2:
		if end+1 < len(toks) && toks[end].kind == dtTimeSep && toks[end].raw == ':' &&
			toks[end+1].kind == dtNum && toks[end+1].digits <= 2 {
			mm = toks[end+1].num
			end += 2
		}
	case 3, 4:
		hh, mm = toks[j].num/100, toks[j].num%100
	default:
		return 0, false
	}
	if mm >= 60 {
		return 0, false
	}
	b.zone, b.offMin = ZoneOffset, sign*int(hh*60+mm)
	return end, true
}

// finish 收尾：检查时区范围、处理秒的小数进位、定下日期、应用上下午。
//
// 出现过 T 却没有时间部分判失败：那个 T 就是用来分隔日期与时间的。
func (b *dateBuilder) finish() (DateParts, bool) {
	if b.isoT && !b.hasTime {
		return DateParts{}, false
	}
	out := DateParts{Hour: b.hour, Min: b.min, Sec: b.sec, Frac: b.frac,
		Zone: b.zone, OffsetMinutes: b.offMin, ISO: b.isoT}
	if !out.zoneInRange() {
		return DateParts{}, false
	}

	if out.Frac == 10000000 {
		out.Frac, out.Sec = 0, out.Sec+1
	}
	if !b.resolveDate(&out) {
		return DateParts{}, false
	}

	switch {
	case b.ampm == 0:
		if out.Hour > 12 {
			return DateParts{}, false
		}
		if out.Hour == 12 {
			out.Hour = 0
		}
	case b.ampm == 1:
		if out.Hour > 23 {
			return DateParts{}, false
		}
		if out.Hour < 12 {
			out.Hour += 12
		}
	}
	return out, true
}

// markedOverride 处理「三个数字 + CJK 后缀」这种写法。
//
// 先按普通规则把三个数字排成年月日，再让后缀指定的成分覆盖上去。
func (b *dateBuilder) markedOverride(out *DateParts) bool {
	nums, month := b.nums, b.month
	b.nums, b.month = b.pre, 0
	y, m, d, ok := b.pickYMD()
	b.nums, b.month = nums, month
	if !ok {
		return false
	}
	if b.cjkY != 0 {
		y = b.cjkY
	}
	if b.cjkM != 0 {
		m = b.cjkM
	}
	if b.cjkD != 0 {
		d = b.cjkD
	}
	out.Year, out.Month, out.Day = y, m, d
	return true
}

// resolveDate 定下年月日。
//
// 用过 CJK 后缀时走后缀那套：没被后缀指定的成分补 1，一个都没指定
// 则整个日期部分算没写。
//
// 带星期名却没有日期判失败：光有个「星期三」定不出是哪一天。
func (b *dateBuilder) resolveDate(out *DateParts) bool {
	out.Weekday = b.dayName
	if b.marked {
		if b.month != 0 {
			if b.cjkM != 0 {
				return false
			}
			b.cjkM = b.month
		}

		if len(b.pre) == 3 && len(b.nums) == 0 {
			return b.markedOverride(out)
		}

		if len(b.pre) > 1 {
			return false
		}
		if len(b.pre) == 1 {
			if b.cjkY != 0 {
				return false
			}
			y, ok := b.yearOf(b.pre[0])
			if !ok {
				return false
			}
			b.cjkY = y
		}

		switch len(b.nums) {
		case 0:
		case 1:
			if b.cjkD != 0 {
				return false
			}
			if b.cjkD = int(b.nums[0].num); b.cjkD < 1 || b.cjkD > 31 {
				return false
			}
		case 2:
			if b.hasTime || len(b.pre) != 0 {
				return false
			}
			out.Hour, out.Min = int(b.nums[0].num), int(b.nums[1].num)
			if out.Hour > 23 || out.Min > 59 {
				return false
			}
		default:
			return false
		}

		if b.cjkY == 0 && b.cjkM == 0 && b.cjkD == 0 {
			out.NoDate = true
			return b.dayName < 0
		}
		out.Year, out.Month, out.Day = b.cjkY, max(b.cjkM, 1), max(b.cjkD, 1)
		return true
	}
	if len(b.nums) == 0 && b.month == 0 {
		if !b.hasTime || b.dayName >= 0 {
			return false
		}
		out.NoDate = true
		return true
	}
	y, m, d, ok := b.pickYMD()
	if !ok {
		return false
	}
	out.Year, out.Month, out.Day = y, m, d
	return true
}

// yearOf 把一个年份记号补全。
//
// 两位年按这种语言的百年段上界展开：上界 2049 表示 50..99 归 19xx、
// 00..49 归 20xx。
func (b *dateBuilder) yearOf(t dtok) (int, bool) {
	if t.digits > 4 {
		return 0, false
	}
	y := int(t.num)
	if t.digits <= 2 {
		yearMax := b.df.TwoDigitYearMax
		if yearMax == 0 {
			yearMax = 2049
		}
		if y <= yearMax%100 {
			y += yearMax / 100 * 100
		} else {
			y += yearMax/100*100 - 100
		}
	}
	return y, y >= 1 && y <= 9999
}

// isYearish 报告这个数字只可能是年份——三位以上不可能是月或日。
func (t dtok) isYearish() bool { return t.digits >= 3 }

// yearIndex 返回第一个只可能是年份的数字的下标，没有给 -1。
func (toks dtoks) yearIndex() int {
	for i, n := range toks {
		if n.isYearish() {
			return i
		}
	}
	return -1
}

// pickYMD 把攒下的数字排成年月日。
//
// 有一个数字明显是年份（三位以上）时，剩下两个按这种语言的月日次序排；
// 否则整组按短日期次序排。次序判定不出来就报失败，不猜。
func (b *dateBuilder) pickYMD() (y, m, d int, ok bool) {
	if b.month != 0 {
		return b.pickWithMonthName()
	}
	switch len(b.nums) {
	case 3:
		if k := b.nums.yearIndex(); k >= 0 {
			order, known := b.df.shortDateOrder()
			var md dateOrder
			switch {
			case k == 0:
				md = "Md"
				if known && order == "ydM" {
					md = "dM"
				}
			case !known:
				return 0, 0, 0, false
			case order == "Mdy" || order == "yMd":
				md = "Md"
			default:
				md = "dM"
			}
			f := map[byte]dtok{'y': b.nums[k]}
			rest := 0
			for i, n := range b.nums {
				if i == k {
					continue
				}
				f[md[rest]] = n
				rest++
			}
			return b.assign(f)
		}
		order, known := b.df.shortDateOrder()
		if !known {
			return 0, 0, 0, false
		}
		return b.assign(map[byte]dtok{
			order[0]: b.nums[0], order[1]: b.nums[1], order[2]: b.nums[2]})
	case 2:
		switch {
		case b.nums[0].isYearish():
			return b.assign(map[byte]dtok{'y': b.nums[0], 'M': b.nums[1]})
		case b.nums[1].isYearish():
			return b.assign(map[byte]dtok{'M': b.nums[0], 'y': b.nums[1]})
		}

		md, known := b.df.monthDayOrder()
		if !known {
			return 0, 0, 0, false
		}
		return b.assign(map[byte]dtok{md[0]: b.nums[0], md[1]: b.nums[1]})
	}
	return 0, 0, 0, false
}

// pickWithMonthName 有月名时把剩下的数字排成年与日。
//
// 两个数字时先按次序试一种，不成立再换过来试：
// "March 5 2024" 与 "March 2024 5" 通常只有一种解读成立。
func (b *dateBuilder) pickWithMonthName() (y, m, d int, ok bool) {
	m = b.month
	switch len(b.nums) {
	case 0:
		return 0, 0, 0, false
	case 1:
		return b.monthAndOne(m)
	case 2:
		if b.digitMonth {
			return 0, 0, 0, false
		}
		order, known := b.df.shortDateOrder()
		if !known {
			return 0, 0, 0, false
		}
		var md dateOrder
		switch order {
		case "yMd":
			md = "yd"
		case "Mdy", "dMy":
			md = "dy"
		default:
			return 0, 0, 0, false
		}
		for _, o := range [2]dateOrder{md, md.swap()} {
			if y, _, d, ok = b.assign(map[byte]dtok{
				o[0]: b.nums[0], o[1]: b.nums[1]}); ok {
				return y, m, d, true
			}
		}
		return 0, 0, 0, false
	}
	return 0, 0, 0, false
}

// monthAndOne 有月名加一个数字时，判断那个数字是日还是年。
//
// 判据是这种语言的月日次序与年月次序：数字出现在月名之前还是之后，
// 决定要拿哪一对次序来比。三位以上的数字一律是年。
func (b *dateBuilder) monthAndOne(m int) (y, mm, d int, ok bool) {
	n := b.nums[0]
	md, known := b.df.monthDayOrder()
	if !known {
		return 0, 0, 0, false
	}

	trigger, want := dateOrder("dM"), dateOrder("My")
	if b.monthPos != 0 {
		trigger, want = "Md", "yM"
	}
	asDay := true
	if md == trigger {
		ym, ok := b.df.yearMonthOrder()
		if !ok {
			return 0, 0, 0, false
		}
		asDay = ym != want
	}
	if !asDay {
		y, ok = b.yearOf(n)
		return y, m, 1, ok
	}

	if n.isYearish() {
		if b.digitMonth {
			return 0, 0, 0, false
		}
		y, ok = b.yearOf(n)
		return y, m, 1, ok
	}
	return 0, m, int(n.num), n.num >= 1 && n.num <= 31
}

// assign 按指定的角色（y/M/d）取值并做范围检查。
//
// 缺的成分补 1；年缺则留 0，让上层知道输入里没写年。
func (b *dateBuilder) assign(f map[byte]dtok) (y, m, d int, ok bool) {
	m, d = 1, 1
	if t, has := f['y']; has {
		if y, ok = b.yearOf(t); !ok {
			return 0, 0, 0, false
		}
	}
	if t, has := f['M']; has {
		if t.num < 1 || t.num > 12 {
			return 0, 0, 0, false
		}
		m = int(t.num)
	}
	if t, has := f['d']; has {
		if t.num < 1 || t.num > 31 {
			return 0, 0, 0, false
		}
		d = int(t.num)
	}
	return y, m, d, true
}

// dateOrder 是年月日的排列次序，比如 "yMd"、"dMy"。
type dateOrder string

// without 去掉一个成分，得到剩下两个的次序。
func (o dateOrder) without(c byte) dateOrder {
	var out []byte
	for i := 0; i < len(o); i++ {
		if o[i] != c {
			out = append(out, o[i])
		}
	}
	return dateOrder(out)
}

// String 返回次序串本身。
func (o dateOrder) String() string { return string(o) }

// patOrder 从一个日期模板里看出各成分的先后位次，没出现的给 -1。
//
// 跳过转义与引号里的内容：那些是字面量，不是字段。
//
// 三个以上的 d 是星期名而不是日，不计入。
func patOrder(pattern, fields string) (y, m, d int) {
	y, m, d = -1, -1, -1
	at := func(c byte) *int {
		switch c {
		case 'y':
			return &y
		case 'M':
			return &m
		}
		return &d
	}
	count, quote := 0, false
	for i := 0; i < len(pattern) && count < len(fields); i++ {
		c := pattern[i]
		if c == '\\' || c == '%' {
			i++
			continue
		}
		if c == '\'' || c == '"' {
			quote = !quote
		}
		if quote || strings.IndexByte(fields, c) < 0 {
			continue
		}
		run := 1
		for ; i+1 < len(pattern) && pattern[i+1] == c; i++ {
			run++
		}

		if c == 'd' && run > 2 {
			continue
		}
		*at(c) = count
		count++
	}
	return y, m, d
}

// shortDateOrder 从短日期模板里看出年月日的次序。
//
// 四种排列之外的（比如模板里少了一个成分）报 false，
// 调用方据此放弃而不是猜一个。
func (df *DateTimeFormat) shortDateOrder() (dateOrder, bool) {
	pattern := df.ShortDatePattern
	y, m, d := patOrder(pattern, "yMd")
	switch {
	case y == 0 && m == 1 && d == 2:
		return "yMd", true
	case m == 0 && d == 1 && y == 2:
		return "Mdy", true
	case d == 0 && m == 1 && y == 2:
		return "dMy", true
	case y == 0 && d == 1 && m == 2:
		return "ydM", true
	}
	return "", false
}

// yearMonthOrder 从年月模板里看出年与月的次序。
func (df *DateTimeFormat) yearMonthOrder() (dateOrder, bool) {
	pattern := df.YearMonthPattern
	y, m, _ := patOrder(pattern, "yM")
	switch {
	case y == 0 && m == 1:
		return "yM", true
	case m == 0 && y == 1:
		return "My", true
	}
	return "", false
}

// monthDayOrder 从月日模板里看出月与日的次序。
func (df *DateTimeFormat) monthDayOrder() (dateOrder, bool) {
	pattern := df.MonthDayPattern
	_, m, d := patOrder(pattern, "Md")
	switch {
	case m == 0 && d == 1:
		return "Md", true
	case d == 0 && m == 1:
		return "dM", true
	}
	return "", false
}

// swap 交换两个成分的次序。
func (o dateOrder) swap() dateOrder {
	if len(o) != 2 {
		return o
	}
	return dateOrder([]byte{o[1], o[0]})
}

var (
	// 英文的月名与星期名。
	//
	// 不管当前是哪种语言都一并认：很多输入就是英文写的。
	invariantMonthNames = [12]string{"January", "February", "March", "April", "May", "June",
		"July", "August", "September", "October", "November", "December"}
	invariantAbbrMonthNames = [12]string{"Jan", "Feb", "Mar", "Apr", "May", "Jun",
		"Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}
	invariantDayNames = [7]string{"Sunday", "Monday", "Tuesday", "Wednesday",
		"Thursday", "Friday", "Saturday"}
	invariantAbbrDayNames = [7]string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}
)

// matchFrCASuffix 匹配加拿大法语的时间后缀（h、min、s）。
//
// 带空格的写法排在前面，好让最长匹配优先。
func matchFrCASuffix(r []rune) (int, int) {
	for _, c := range [6]struct {
		lit  string
		mark int
	}{{" h ", 3}, {" min ", 4}, {" s ", 5}, {" h", 3}, {" min", 4}, {" s", 5}} {
		n := len([]rune(c.lit))
		if len(r) >= n && strings.EqualFold(string(r[:n]), c.lit) {
			return c.mark, n
		}
	}
	return 0, 0
}
