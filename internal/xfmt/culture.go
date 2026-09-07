package xfmt

import (
	_ "embed"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

// NumberFormat 是一种语言下数字的排版与解析规则。
//
// 分成数字、货币、百分号三套，各有各的小数位数、分隔符与分组方式——
// 同一种语言里这三者可以不一样。
type NumberFormat struct {
	// 普通数字的排版规则。
	NumberDecimalDigits    int
	NumberDecimalSeparator string
	NumberGroupSeparator   string
	NumberGroupSizes       []int
	NumberNegativePattern  int

	// 货币的排版规则。正负号与符号的摆放位置由两个写法编号决定。
	CurrencySymbol           string
	CurrencyDecimalDigits    int
	CurrencyDecimalSeparator string
	CurrencyGroupSeparator   string
	CurrencyGroupSizes       []int
	CurrencyPositivePattern  int
	CurrencyNegativePattern  int

	// 百分号的排版规则。
	PercentSymbol           string
	PercentDecimalDigits    int
	PercentDecimalSeparator string
	PercentGroupSeparator   string
	PercentGroupSizes       []int
	PercentPositivePattern  int
	PercentNegativePattern  int

	// 正负号与几个特殊值的符号串，三套规则共用。
	NegativeSign           string
	PositiveSign           string
	NaNSymbol              string
	PositiveInfinitySymbol string
	NegativeInfinitySymbol string
	PerMilleSymbol         string
}

// Invariant 返回不随语言变的数字规则。
//
// 需要产出可交换文本的地方用它：同一个数在任何机器上排出同样的串。
func Invariant() *NumberFormat {
	return &NumberFormat{
		NumberDecimalDigits:    2,
		NumberDecimalSeparator: ".",
		NumberGroupSeparator:   ",",
		NumberGroupSizes:       []int{3},
		NumberNegativePattern:  1,

		CurrencySymbol:           "¤",
		CurrencyDecimalDigits:    2,
		CurrencyDecimalSeparator: ".",
		CurrencyGroupSeparator:   ",",
		CurrencyGroupSizes:       []int{3},
		CurrencyPositivePattern:  0,
		CurrencyNegativePattern:  0,

		PercentSymbol:           "%",
		PercentDecimalDigits:    2,
		PercentDecimalSeparator: ".",
		PercentGroupSeparator:   ",",
		PercentGroupSizes:       []int{3},
		PercentPositivePattern:  0,
		PercentNegativePattern:  0,

		NegativeSign:           "-",
		PositiveSign:           "+",
		NaNSymbol:              "NaN",
		PositiveInfinitySymbol: "Infinity",
		NegativeInfinitySymbol: "-Infinity",
		PerMilleSymbol:         "‰",
	}
}

// cultureTable 是各语言的数字规则表，按语言标签排好序，编译期嵌进来。
//
//go:embed cultures.tsv
var cultureTable string

// cultureLines 把规则表切成行，第一次用到时才切。
var cultureLines = sync.OnceValue(func() []string {
	return strings.Split(strings.TrimSuffix(cultureTable, "\n"), "\n")
})

// cultureTag 是小写的语言标签，用作查表的键。
type cultureTag string

// findCulture 在数字规则表里查这个标签。
func (tag cultureTag) findCulture() (string, bool) { return tag.searchTable(cultureLines()) }

// parentTag 去掉最后一段，得到更宽泛的标签。
//
// zh-Hans-CN 找不到时退到 zh-Hans，再退到 zh：具体到国家的规则往往缺失，
// 而语言一级的总是有的。
func (tag cultureTag) parentTag() cultureTag {
	if i := strings.LastIndexAny(string(tag), "-_"); i > 0 {
		return tag[:i]
	}
	return ""
}

// formatCache 按语言标签缓存解析好的规则。
type formatCache sync.Map

// get 取缓存，没有就解析一次存进去。
//
// 两个 goroutine 同时解析同一个标签时会各算一遍、后写的覆盖前面的：
// 结果一样，多算一次比加锁便宜。
func (c *formatCache) get[T any](tag cultureTag, resolve func(cultureTag) T) T {
	m := (*sync.Map)(c)
	if f, ok := m.Load(tag); ok {
		return f.(T)
	}
	f := resolve(tag)
	m.Store(tag, f)
	return f
}

// cultureCache 缓存各语言的数字规则。
var cultureCache formatCache

// Unknown 返回认不出来的语言该用的数字规则。
//
// 不是直接退回 [Invariant]：它在几处上有区别（小数位数、无穷符号、
// 百分号的摆放），这些区别是格式的一部分。
func Unknown() *NumberFormat {
	f := Invariant()
	f.NumberDecimalDigits = 3
	f.CurrencyNegativePattern = 1
	f.PercentDecimalDigits = 3
	f.PercentPositivePattern = 1
	f.PercentNegativePattern = 1
	f.PositiveInfinitySymbol = "\u221e"
	f.NegativeInfinitySymbol = "-\u221e"
	return f
}

// invariantNames 是几个表示「不指定语言」的标签。
var invariantNames = map[cultureTag]bool{"": true, "und": true, "c": true}

// Culture 取某种语言的数字规则，名字不区分大小写。
func Culture(name string) *NumberFormat {
	return cultureCache.get(cultureTag(strings.ToLower(name)), cultureTag.resolveCulture)
}

// resolveCulture 沿标签逐级往上找，都找不到就用认不出来那一套。
func (tag cultureTag) resolveCulture() *NumberFormat {
	if invariantNames[tag] {
		return Invariant()
	}
	for t := tag; t != ""; t = t.parentTag() {
		if line, ok := t.findCulture(); ok {
			return parseNumberFormat(line)
		}
	}
	return Unknown()
}

// numberFields 是数字规则表每行的列数。
const numberFields = 26

// parseNumberFormat 解析规则表的一行。
//
// 列数对不上就退回不随语言变的那一套：表是编译期嵌进来的，
// 列数不对说明表与代码不同步，此时按残缺的行去解只会得到一堆空串。
func parseNumberFormat(line string) *NumberFormat {
	c := strings.Split(line, "\t")
	if len(c) != numberFields {
		return Invariant()
	}
	atoi := func(s string) int { n, _ := strconv.Atoi(s); return n }
	sizes := func(s string) []int {
		parts := strings.Split(s, ",")
		out := make([]int, len(parts))
		for i, p := range parts {
			out[i] = atoi(p)
		}
		return out
	}
	return &NumberFormat{
		NumberDecimalDigits:    atoi(c[1]),
		NumberDecimalSeparator: c[2],
		NumberGroupSeparator:   c[3],
		NumberGroupSizes:       sizes(c[4]),
		NumberNegativePattern:  atoi(c[5]),

		CurrencySymbol:           c[6],
		CurrencyDecimalDigits:    atoi(c[7]),
		CurrencyDecimalSeparator: c[8],
		CurrencyGroupSeparator:   c[9],
		CurrencyGroupSizes:       sizes(c[10]),
		CurrencyPositivePattern:  atoi(c[11]),
		CurrencyNegativePattern:  atoi(c[12]),

		PercentSymbol:           c[13],
		PercentDecimalDigits:    atoi(c[14]),
		PercentDecimalSeparator: c[15],
		PercentGroupSeparator:   c[16],
		PercentGroupSizes:       sizes(c[17]),
		PercentPositivePattern:  atoi(c[18]),
		PercentNegativePattern:  atoi(c[19]),

		NegativeSign:           c[20],
		PositiveSign:           c[21],
		NaNSymbol:              c[22],
		PositiveInfinitySymbol: c[23],
		NegativeInfinitySymbol: c[24],
		PerMilleSymbol:         c[25],
	}
}

// current 是当前环境的数字规则，只解析一次。
var current = sync.OnceValue(func() *NumberFormat {
	return Culture(CurrentCultureName())
})

// Current 返回当前环境的数字规则。
//
// **进程期间固定**：中途改环境变量不会生效。排版结果在一次运行里
// 必须前后一致，否则同一个查询前后两次会得到不同的文本。
func Current() *NumberFormat { return current() }

// CurrentCultureName 从环境变量里读出当前语言标签。
//
// 按 LC_ALL、LC_MESSAGES、LANG 的次序看第一个非空的；去掉编码与修饰后缀，
// 下划线换成连字符。C 与 POSIX 当作「不指定」。
func CurrentCultureName() string {
	for _, k := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		v := os.Getenv(k)
		if v == "" {
			continue
		}

		v, _, _ = strings.Cut(v, ".")
		v, _, _ = strings.Cut(v, "@")
		if v == "C" || v == "POSIX" {
			return ""
		}
		return strings.ReplaceAll(v, "_", "-")
	}
	return ""
}

// DateTimeFormat 是一种语言下日期时间的排版与解析规则。
type DateTimeFormat struct {
	// 上下午标记与日期、时间的分隔符。
	AMDesignator  string
	PMDesignator  string
	DateSeparator string
	TimeSeparator string

	// 各种标准写法对应的模板。
	ShortDatePattern    string
	LongDatePattern     string
	ShortTimePattern    string
	LongTimePattern     string
	FullDateTimePattern string
	MonthDayPattern     string
	YearMonthPattern    string

	// 星期名与月名，全名与缩写各一套。
	DayNames              [7]string
	AbbreviatedDayNames   [7]string
	MonthNames            [12]string
	AbbreviatedMonthNames [12]string

	// 月名的属格形式。
	//
	// 有些语言里「三月」单独出现与出现在「3月5日」里要用不同的词形。
	MonthGenitiveNames            [12]string
	AbbreviatedMonthGenitiveNames [12]string

	// Calendar 是这种语言默认用哪一套历法。
	Calendar string

	// 纪元名，全名与缩写。
	EraName     string
	AbbrEraName string

	// TwoDigitYearMax 是两位年份归入哪一个百年段的上界。
	//
	// 比如 2049 表示 50..99 归 19xx，00..49 归 20xx。
	TwoDigitYearMax int

	// ExtraPatterns 是这种语言额外认识的日期模板，解析时逐个试。
	ExtraPatterns []string

	// LangName 是语言标签的第一段。
	LangName string

	// FrenchCanadianSuffixes 标记一处只在加拿大法语下出现的后缀规则。
	FrenchCanadianSuffixes bool

	// OrdinalTokens 表示比对名字时区分大小写。
	//
	// 只有 POSIX 那一套如此；其余语言一律不区分。
	OrdinalTokens bool

	// 解析时用得上的词表，从模板里扫出来，第一次用到时才扫。
	dateWordsOnce sync.Once
	dateWordSet   *dateWordSet

	// FormatFlags 是这种语言的若干开关位，见 [flagDigitPrefix]。
	FormatFlags int
}

// flagDigitPrefix 表示这种语言的日期里数字写在名字前面。
const flagDigitPrefix = 32

const (
	// 四个不随语言变的标准模板。
	//
	// 分隔符都用引号括起来，免得被当前语言的分隔符替换掉——
	// 这些写法要产出可交换的文本。
	rfc1123Pattern           = "ddd, dd MMM yyyy HH':'mm':'ss 'GMT'"
	sortablePattern          = "yyyy'-'MM'-'dd'T'HH':'mm':'ss"
	universalSortablePattern = "yyyy'-'MM'-'dd HH':'mm':'ss'Z'"
	roundtripPattern         = "yyyy'-'MM'-'dd'T'HH':'mm':'ss'.'fffffffK"
)

var (
	// 英文的星期名与月名，不随语言变的那一套用它们。
	englishDays      = [7]string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
	englishDaysAbbr  = [7]string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}
	englishMonths    = [12]string{"January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"}
	englishMonthAbbr = [12]string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}
)

// InvariantDate 返回不随语言变的日期规则。
func InvariantDate() *DateTimeFormat {
	return &DateTimeFormat{
		AMDesignator:  "AM",
		PMDesignator:  "PM",
		DateSeparator: "/",
		TimeSeparator: ":",

		ShortDatePattern:    "MM/dd/yyyy",
		LongDatePattern:     "dddd, dd MMMM yyyy",
		ShortTimePattern:    "HH:mm",
		LongTimePattern:     "HH:mm:ss",
		FullDateTimePattern: "dddd, dd MMMM yyyy HH:mm:ss",
		MonthDayPattern:     "MMMM dd",
		YearMonthPattern:    "yyyy MMMM",

		DayNames:              englishDays,
		AbbreviatedDayNames:   englishDaysAbbr,
		MonthNames:            englishMonths,
		AbbreviatedMonthNames: englishMonthAbbr,

		MonthGenitiveNames:            englishMonths,
		AbbreviatedMonthGenitiveNames: englishMonthAbbr,
		Calendar:                      "GregorianCalendar",

		EraName:         "A.D.",
		AbbrEraName:     "AD",
		TwoDigitYearMax: 2049,
	}
}

// dateCultureTable 是各语言的日期规则表，编译期嵌进来。
//
//go:embed datecultures.tsv
var dateCultureTable string

// dateCultureLines 把日期规则表切成行，第一次用到时才切。
var dateCultureLines = sync.OnceValue(func() []string {
	return strings.Split(strings.TrimSuffix(dateCultureTable, "\n"), "\n")
})

// datePatternTable 是各语言额外认识的日期模板，编译期嵌进来。
//
//go:embed datepatterns.tsv
var datePatternTable string

// datePatternLines 把额外模板表切成行，第一次用到时才切。
var datePatternLines = sync.OnceValue(func() []string {
	return strings.Split(strings.TrimSuffix(datePatternTable, "\n"), "\n")
})

// findDatePatterns 查这个标签的额外日期模板。
//
// 表里的一行可以写成「= 另一个标签」，表示照用那一行——很多语言的
// 模板集是相同的，这样不必重复存。
func (tag cultureTag) findDatePatterns() []string {
	for t := tag; t != ""; t = t.parentTag() {
		line, ok := t.searchTable(datePatternLines())
		if !ok {
			continue
		}

		f := strings.Split(line, "\t")
		if len(f) >= 3 && f[1] == "=" {
			if line, ok = cultureTag(f[2]).searchTable(datePatternLines()); !ok {
				return nil
			}
			f = strings.Split(line, "\t")
		}
		return f[1:]
	}
	return nil
}

// findDateCulture 查这个标签的日期规则，同样支持指向另一行。
func (tag cultureTag) findDateCulture() (string, bool) {
	line, ok := tag.searchTable(dateCultureLines())
	if !ok {
		return "", false
	}
	if name, cut := strings.CutPrefix(afterField(line, 1), "=\t"); cut {
		return cultureTag(strings.ToLower(name)).searchTable(dateCultureLines())
	}
	return line, true
}

// afterField 跳过前 n 列，返回剩下的部分。
func afterField(line string, n int) string {
	for ; n > 0; n-- {
		_, rest, ok := strings.Cut(line, "\t")
		if !ok {
			return ""
		}
		line = rest
	}
	return line
}

// searchTable 在按名字排好序的表里二分查找。
//
// 表是编译期嵌进来的，本来就有序，所以不必先建索引。
func (tag cultureTag) searchTable(lines []string) (string, bool) {
	i, ok := slices.BinarySearchFunc(lines, tag, func(line string, want cultureTag) int {
		name, _, _ := strings.Cut(line, "\t")
		return strings.Compare(strings.ToLower(name), string(want))
	})
	if !ok {
		return "", false
	}
	return lines[i], true
}

// dateFields 是日期规则表每行的列数。
const dateFields = 79

// parseDateFormat 解析日期规则表的一行，列数对不上就退回不随语言变的那一套。
func parseDateFormat(line string) *DateTimeFormat {
	c := strings.Split(line, "\t")
	if len(c) != dateFields {
		return InvariantDate()
	}
	f := &DateTimeFormat{
		AMDesignator: c[1], PMDesignator: c[2], DateSeparator: c[3], TimeSeparator: c[4],
		ShortDatePattern: c[5], LongDatePattern: c[6],
		ShortTimePattern: c[7], LongTimePattern: c[8],
		FullDateTimePattern: c[9], MonthDayPattern: c[10], YearMonthPattern: c[11],
	}
	copy(f.DayNames[:], c[12:19])
	copy(f.AbbreviatedDayNames[:], c[19:26])
	copy(f.MonthNames[:], c[26:38])
	copy(f.AbbreviatedMonthNames[:], c[38:50])
	copy(f.MonthGenitiveNames[:], c[50:62])
	copy(f.AbbreviatedMonthGenitiveNames[:], c[62:74])
	f.EraName, f.AbbrEraName = c[74], c[75]
	f.TwoDigitYearMax, _ = strconv.Atoi(c[76])
	f.Calendar = c[77]
	f.FormatFlags, _ = strconv.Atoi(c[78])
	return f
}

// dateCultureCache 缓存各语言的日期规则。
var dateCultureCache formatCache

// invariant 是共享的那一份不随语言变的日期规则。
//
// 它会被多处并发读，所以只建一份、谁也不许改。
var invariant = sync.OnceValue(InvariantDate)

// DateCulture 取某种语言的日期规则，名字不区分大小写。
func DateCulture(name string) *DateTimeFormat {
	return dateCultureCache.get(cultureTag(strings.ToLower(name)), cultureTag.resolveDateCulture)
}

// resolveDateCulture 查出基础规则，再补上额外模板与几个语言相关的开关。
func (tag cultureTag) resolveDateCulture() *DateTimeFormat {
	f := tag.resolveDateCultureBase()
	f.ExtraPatterns = tag.findDatePatterns()
	f.LangName, _, _ = strings.Cut(string(tag), "-")
	f.FrenchCanadianSuffixes = tag == "fr-ca"

	for _, seg := range strings.Split(string(tag), "-") {
		if seg == "posix" {
			f.OrdinalTokens = true
			break
		}
	}
	return f
}

// resolveDateCultureBase 沿标签逐级往上找日期规则。
func (tag cultureTag) resolveDateCultureBase() *DateTimeFormat {
	if invariantNames[tag] {
		return InvariantDate()
	}
	for t := tag; t != ""; t = t.parentTag() {
		if line, ok := t.findDateCulture(); ok {
			return parseDateFormat(line)
		}
	}
	return UnknownDate()
}

// eqName 比对月名、星期名这类词，除 POSIX 外都不区分大小写。
func (df *DateTimeFormat) eqName(a, b string) bool {
	if df.OrdinalTokens {
		return a == b
	}
	return strings.EqualFold(a, b)
}

// UnknownDate 返回认不出来的语言该用的日期规则。
func UnknownDate() *DateTimeFormat {
	f := InvariantDate()
	f.ShortDatePattern = "M/d/yyyy"
	f.LongDatePattern = "dddd, MMMM d, yyyy"
	f.FullDateTimePattern = "dddd, MMMM d, yyyy HH:mm:ss"
	f.MonthDayPattern = "MMMM d"
	f.YearMonthPattern = "MMMM yyyy"
	f.EraName, f.AbbrEraName = "AD", "A"
	return f
}

// currentDate 是当前环境的日期规则，只解析一次。
var currentDate = sync.OnceValue(func() *DateTimeFormat {
	return DateCulture(CurrentCultureName())
})

// CurrentDate 返回当前环境的日期规则，进程期间固定。
func CurrentDate() *DateTimeFormat { return currentDate() }

// dateWordSet 是从日期模板里扫出来的字面词。
//
// 解析日期时要跳过「年」「de」「日」这类固定词，而它们各语言不同，
// 只能从模板里反推。
type dateWordSet struct {
	words        []string
	postfixes    []string
	ignorableDot bool
}

// scanDateWords 扫一批模板，收集其中的字面词。
func scanDateWords(patterns ...string) dateWordSet {
	var sc dateWordSet
	for _, p := range patterns {
		sc.scan([]rune(p))
	}
	return sc
}

// scan 扫一个模板。
//
// 引号里的内容是字面词。跟在四个以上 M 之后的引号内容记作后缀词——
// 那是「月」这类紧贴月名的字。
//
// 年月日三者都出现过之后再碰到的点，记作「可忽略的点」：那说明这种语言
// 的日期以点结尾（比如 2024. 03. 05.），解析时不该把它当分隔符。
func (sc *dateWordSet) scan(p []rune) {
	var y, m, d bool
	all := func() bool { return y && m && d }
	for i := 0; i < len(p); {
		switch c := p[i]; c {
		case '\'':
			i = sc.addWords(p, i+1, false)
		case 'M':
			j := i
			for j < len(p) && p[j] == 'M' {
				j++
			}
			n := j - i
			i = j

			if n >= 4 && i < len(p) && p[i] == '\'' {
				i = sc.addWords(p, i+1, true)
			}
			m = true
		case 'y':
			for i < len(p) && p[i] == 'y' {
				i++
			}
			y = true
		case 'd':
			j := i
			for j < len(p) && p[j] == 'd' {
				j++
			}
			if j-i <= 2 {
				d = true
			}
			i = j
		case '\\':
			i += 2
		case '.':
			if all() {
				sc.ignorableDot = true
				y, m, d = false, false, false
			}
			i++
		default:
			if all() && !unicode.IsSpace(c) {
				y, m, d = false, false, false
			}
			i++
		}
	}
}

// addWords 读出引号里的内容，按空白切成若干个词。
func (sc *dateWordSet) addWords(p []rune, i int, postfix bool) int {
	ni := skipToWordStart(p, i)
	if ni != i {
		postfix = false
	}
	i = ni
	var w []rune
	for i < len(p) {
		switch c := p[i]; {
		case c == '\'':
			sc.add(string(w), postfix)
			return i + 1
		case c == '\\':
			i++
			if i < len(p) {
				w = append(w, p[i])
				i++
			}
		case unicode.IsSpace(c):
			sc.add(string(w), postfix)
			postfix = false
			w = w[:0]
			i++
		default:
			w = append(w, c)
			i++
		}
	}
	return i
}

// skipToWordStart 跳过引号内容开头的非字母字符。
func skipToWordStart(p []rune, i int) int {
	for i < len(p) {
		c := p[i]
		if c == '\\' {
			i++
			if i >= len(p) {
				break
			}
			c = p[i]
			if c == '\'' {
				continue
			}
		}
		if unicode.IsLetter(c) || c == '\'' || c == '.' {
			break
		}
		i++
	}
	return i
}

// add 收下一个词，重复的不再加。
//
// 单个字符的分隔符与 CJK 的年月日时分秒不收：它们在解析时另有处理，
// 收进词表反而会把日期切错。
//
// 以点结尾的词额外再收一个去掉点的版本：缩写月名有时带点有时不带。
func (sc *dateWordSet) add(w string, postfix bool) {
	if w == "" {
		return
	}
	if r := []rune(w); len(r) == 1 {
		switch r[0] {
		case '.':
			sc.ignorableDot = true
			return
		case '/', '-':
			return
		case '年', '月', '日', '년', '월', '일', '時', '时', '分', '秒', '시', '분', '초':
			return
		}
	}
	if postfix {
		if !slices.Contains(sc.postfixes, w) {
			sc.postfixes = append(sc.postfixes, w)
		}
		return
	}
	if !slices.Contains(sc.words, w) {
		sc.words = append(sc.words, w)
	}

	if strings.HasSuffix(w, ".") {
		if t := strings.TrimSuffix(w, "."); !slices.Contains(sc.words, t) {
			sc.words = append(sc.words, t)
		}
	}
}

// dateWordsOf 取这套规则的字面词表，第一次用到时才扫。
//
// 没有额外模板时退回用几个标准模板去扫。
func (df *DateTimeFormat) dateWordsOf() *dateWordSet {
	df.dateWordsOnce.Do(func() {
		pats := df.ExtraPatterns
		if len(pats) == 0 {
			pats = []string{df.LongDatePattern, df.ShortDatePattern, df.YearMonthPattern,
				df.MonthDayPattern, df.LongTimePattern, df.ShortTimePattern}
		}
		sc := scanDateWords(pats...)
		df.dateWordSet = &sc
	})
	return df.dateWordSet
}

// DateWords 返回解析日期时可以跳过的字面词。
func (df *DateTimeFormat) DateWords() []string { return df.dateWordsOf().words }

// MonthPostfixes 返回紧跟在月名之后的字面词。
func (df *DateTimeFormat) MonthPostfixes() []string { return df.dateWordsOf().postfixes }

// ignorableSym 报告一个符号在解析时能不能跳过。
//
// 只有点与逗号可以，而且它不能正好是这种语言的时间分隔符——
// 那时它是有含义的。
func (df *DateTimeFormat) ignorableSym(c rune) bool {
	if c != '.' && c != ',' {
		return false
	}
	return strings.TrimSpace(df.TimeSeparator) != string(c)
}

// DateSepIgnorable 报告这种语言的日期分隔符（点）能不能被跳过。
func (df *DateTimeFormat) DateSepIgnorable() bool {
	return df.dateWordsOf().ignorableDot && strings.TrimSpace(df.DateSeparator) == "."
}
