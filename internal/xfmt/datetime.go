package xfmt

import (
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// DateKind 说明一个时刻的时区含义。
//
// 它不是时区本身，而是「这个时刻该怎么理解」：不指定、UTC、还是本地时区。
type DateKind uint8

const (
	// DateUnspecified 不带时区含义，排版时不输出时区信息。
	DateUnspecified DateKind = iota

	// DateUTC 是 UTC 时刻。
	DateUTC

	// DateLocal 是本地时区的时刻。
	DateLocal
)

// Date 是一个时刻加上它的时区含义。
type Date struct {
	T    time.Time
	Kind DateKind
}

// LocalDate 把一个时刻转到本地时区。
func LocalDate(t time.Time) Date { return Date{T: t.Local(), Kind: DateLocal} }

// minTime 是可表示的最早时刻。
var minTime = time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)

// Format 按格式串排版一个时刻。
//
// 一个字符（或空串）是标准写法，更长的按自定义模板处理。
//
// 含 `{` 或 `}` 直接报错：那是复合格式串的括号，这里不接受。
func (d Date) Format(format string, df *DateTimeFormat) (string, error) {
	if strings.ContainsAny(format, "{}") {
		return "", errBadFormat
	}

	if len(format) <= 1 {
		return d.standard(format, df)
	}
	return d.custom(format, df)
}

// standard 按写法代号排版。
//
// o/r/s/u 四个是**不随语言变的**写法：它们要产出可交换的文本，
// 所以一律用固定的模板与固定的格式数据，而不是当前语言那一套。
//
// U 先转成 UTC 再按完整写法排。
func (d Date) standard(format string, df *DateTimeFormat) (string, error) {
	c := byte('G')
	if format != "" {
		c = format[0]
	}

	switch c {
	case 'o', 'O':
		return d.custom(roundtripPattern, invariant())
	case 'r', 'R':
		return d.custom(rfc1123Pattern, invariant())
	case 's':
		return d.custom(sortablePattern, invariant())
	case 'u':
		return d.custom(universalSortablePattern, invariant())
	}

	var pattern string
	switch c {
	case 'd':
		pattern = df.ShortDatePattern
	case 'D':
		pattern = df.LongDatePattern
	case 'f':
		pattern = df.LongDatePattern + " " + df.ShortTimePattern
	case 'F':
		pattern = df.FullDateTimePattern
	case 'g':
		pattern = df.ShortDatePattern + " " + df.ShortTimePattern
	case 'G':
		pattern = df.ShortDatePattern + " " + df.LongTimePattern
	case 'm', 'M':
		pattern = df.MonthDayPattern
	case 't':
		pattern = df.ShortTimePattern
	case 'T':
		pattern = df.LongTimePattern
	case 'U':
		return d.universal().custom(df.FullDateTimePattern, df)
	case 'y', 'Y':
		pattern = df.YearMonthPattern
	default:
		return "", errBadFormat
	}
	return d.custom(pattern, df)
}

// universal 转成 UTC。
//
// 转过去早于可表示范围时钉在最早时刻，而不是回绕到一个很大的年份。
func (d Date) universal() Date {
	if d.Kind == DateUTC {
		return d
	}
	u := d.T.UTC()
	if u.Before(minTime) {
		u = minTime
	}
	return Date{T: u, Kind: DateUTC}
}

// offset 返回这个时刻相对 UTC 的偏移，UTC 一律为零。
func (d Date) offset() time.Duration {
	if d.Kind == DateUTC {
		return 0
	}
	_, secs := d.T.Zone()
	return time.Duration(secs) * time.Second
}

// custom 按自定义模板排版一个时刻。
//
// 模板里同一个字母连写几遍决定宽度：`M` 是月份数字、`MM` 补零、
// `MMM` 是缩写月名、`MMMM` 是全名。
//
// 历法换算按需做一次并记住结果：一个模板里可能出现好几处年月日，
// 每处都换算一遍既慢又可能得到不一致的结果（换算过程要取当前时刻）。
//
// timeOnly 记着模板里有没有出现过日期字段，供 [Date.zoneOffset] 判断
// 一个只有时间的模板该用哪个偏移。
func (d Date) custom(format string, df *DateTimeFormat) (string, error) {
	var b []byte
	t := d.T

	kind := df.Kind()
	var cy, cm, cd int
	var calDone bool
	var calErr error
	calDate := func() bool {
		if !calDone {
			calDone = true
			var ok bool
			if cy, cm, cd, ok = kind.ToCalendar(t.Year(), int(t.Month()), t.Day()); !ok {
				calErr = ErrCalendarRange
			}
		}
		return calErr == nil
	}

	timeOnly := true

	for i := 0; i < len(format); {
		c := format[i]
		n := repeatCount(format, i)
		switch c {
		case 'g':
			b = append(b, df.EraName...)
		case 'h':
			h := t.Hour() % 12
			if h == 0 {
				h = 12
			}
			b = appendDigits(b, h, n)
		case 'H':
			b = appendDigits(b, t.Hour(), n)
		case 'm':
			b = appendDigits(b, t.Minute(), n)
		case 's':
			b = appendDigits(b, t.Second(), n)
		case 'd':
			timeOnly = false
			switch {
			case n <= 2:
				if !calDate() {
					return "", calErr
				}
				b = appendDigits(b, cd, n)
			case n == 3:
				b = append(b, df.AbbreviatedDayNames[int(t.Weekday())]...)
			default:
				b = append(b, df.DayNames[int(t.Weekday())]...)
			}
		case 'M':
			timeOnly = false
			if !calDate() {
				return "", calErr
			}
			m := cm
			switch {
			case n <= 2:
				b = appendDigits(b, m, n)
			case n == 3:
				b = append(b, df.AbbreviatedMonthNames[m-1]...)
			default:
				b = append(b, df.MonthNames[m-1]...)
			}
		case 'y':
			timeOnly = false
			if !calDate() {
				return "", calErr
			}
			y := cy
			if n <= 2 {
				b = appendDigits(b, y%100, n)
			} else {
				b = appendPad(b, y, n)
			}
		case 'f', 'F':
			var err error
			if b, err = appendFraction(b, t, n, c == 'F'); err != nil {
				return "", err
			}
		case 't':
			des := df.AMDesignator
			if t.Hour() >= 12 {
				des = df.PMDesignator
			}
			if n == 1 {
				if des != "" {
					b = append(b, des[0])
				}
			} else {
				b = append(b, des...)
			}
		case 'z':
			b = appendOffset(b, d.zoneOffset(timeOnly), n)
		case 'K':
			n = 1
			b = d.appendRoundtripZone(b)
		case ':':
			n = 1
			b = append(b, df.TimeSeparator...)
		case '/':
			n = 1
			b = append(b, df.DateSeparator...)
		case '\'', '"':
			lit, w, ok := quotedLiteral(format[i:])
			if !ok {
				return "", errBadFormat
			}
			b = append(b, lit...)
			n = w
		case '%':
			if i+1 >= len(format) || format[i+1] == '%' {
				return "", errBadFormat
			}
			_, w := utf8.DecodeRuneInString(format[i+1:])
			s, err := d.custom(format[i+1:i+1+w], df)
			if err != nil {
				return "", err
			}
			b = append(b, s...)
			n = 1 + w
		case '\\':
			if i+1 >= len(format) {
				return "", errBadFormat
			}
			_, w := utf8.DecodeRuneInString(format[i+1:])
			b = append(b, format[i+1:i+1+w]...)
			n = 1 + w
		default:
			b = append(b, c)
			n = 1
		}
		i += n
	}
	return string(b), nil
}

// zoneOffset 返回排版时区字段要用的偏移。
//
// 模板里只有时间、时刻又停在可表示范围的第一天时，用**当下**的时区偏移：
// 那种值表示的是「一天里的某个钟点」，而不是公元一年的那一天，
// 按那一天的历史偏移排出来会得到一个奇怪的数。
func (d Date) zoneOffset(timeOnly bool) time.Duration {
	if timeOnly && d.Kind != DateUTC && d.T.Year() == 1 && d.T.YearDay() == 1 {
		_, secs := time.Now().Zone()
		return time.Duration(secs) * time.Second
	}
	return d.offset()
}

// appendRoundtripZone 输出往返写法的时区部分。
//
// UTC 写 Z，本地时区写 ±HH:MM，不指定时区的什么都不写——
// 那正是「不指定」与「UTC」的区别所在。
func (d Date) appendRoundtripZone(b []byte) []byte {
	switch d.Kind {
	case DateUTC:
		return append(b, 'Z')
	case DateLocal:
		return appendOffset(b, d.offset(), 3)
	default:
		return b
	}
}

// appendOffset 输出时区偏移，n 决定是 ±H、±HH 还是 ±HH:MM。
func appendOffset(b []byte, off time.Duration, n int) []byte {
	if off < 0 {
		b = append(b, '-')
		off = -off
	} else {
		b = append(b, '+')
	}
	h := int(off / time.Hour)
	if n <= 1 {
		return appendPad(b, h, 1)
	}
	b = appendPad(b, h, 2)
	if n >= 3 {
		b = append(b, ':')
		b = appendPad(b, int(off/time.Minute)%60, 2)
	}
	return b
}

// appendFraction 输出秒的小数部分，最多七位。
//
// trim 为真（模板里写 `F`）时去掉末尾的零；全被去光时，连它前面那个
// 小数点也一并抹掉——不然会留下一个孤零零的点。
func appendFraction(b []byte, t time.Time, n int, trim bool) ([]byte, error) {
	if n > 7 {
		return nil, errBadFormat
	}
	frac := t.Nanosecond() / 100
	for range 7 - n {
		frac /= 10
	}
	if !trim {
		return appendPad(b, frac, n), nil
	}
	for n > 0 && frac%10 == 0 {
		frac /= 10
		n--
	}
	if n > 0 {
		return appendPad(b, frac, n), nil
	}
	if len(b) > 0 && b[len(b)-1] == '.' {
		b = b[:len(b)-1]
	}
	return b, nil
}

// quotedLiteral 读出一段引号里的字面量，引号内可以用反斜杠转义。
func quotedLiteral(src string) (string, int, bool) {
	q := src[0]
	var lit strings.Builder
	for i := 1; i < len(src); i++ {
		switch src[i] {
		case q:
			return lit.String(), i + 1, true
		case '\\':
			if i+1 >= len(src) {
				return "", 0, false
			}
			i++
			lit.WriteByte(src[i])
		default:
			lit.WriteByte(src[i])
		}
	}
	return "", 0, false
}

// repeatCount 数出从 i 开始连续重复了几个相同字符。
func repeatCount(s string, i int) int {
	n := 1
	for i+n < len(s) && s[i+n] == s[i] {
		n++
	}
	return n
}

// appendDigits 输出一个数，补零到 n 位但**最多两位**。
//
// 时分秒这些字段连写三遍以上没有额外含义，宽度就停在两位。
func appendDigits(b []byte, v, n int) []byte {
	return appendPad(b, v, min(n, 2))
}

// appendPad 输出一个数，左边补零到至少 n 位。
func appendPad(b []byte, v, n int) []byte {
	s := strconv.Itoa(v)
	for range n - len(s) {
		b = append(b, '0')
	}
	return append(b, s...)
}
