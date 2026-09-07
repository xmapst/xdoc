package xbexpr

import (
	"math"
	"strings"
	"time"

	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xfmt"
)

// init 登记日期方法。
func init() {
	one := scalars(1)
	three := scalars(3)
	reg("YEAR", datePart(func(t time.Time) int { return t.Year() }), Info{Params: one})
	reg("MONTH", datePart(func(t time.Time) int { return int(t.Month()) }), Info{Params: one})
	reg("DAY", datePart(func(t time.Time) int { return t.Day() }), Info{Params: one})
	reg("HOUR", datePart(func(t time.Time) int { return t.Hour() }), Info{Params: one})
	reg("MINUTE", datePart(func(t time.Time) int { return t.Minute() }), Info{Params: one})
	reg("SECOND", datePart(func(t time.Time) int { return t.Second() }), Info{Params: one})
	reg("DATEADD", (*Ctx).mDATEADD, Info{Params: three})
	reg("DATEDIFF", (*Ctx).mDATEDIFF, Info{Params: three})
	reg("TO_LOCAL", (*Ctx).mTOLOCAL, Info{Params: one})
	reg("TO_UTC", (*Ctx).mTOUTC, Info{Params: one})
}

// datePart 把一个取时间分量的函数包成方法；不是时间值时返回 Null。
func datePart(get func(time.Time) int) Method {
	return func(_ *Ctx, args []*xbson.Value) (*xbson.Value, error) {
		t, ok := args[0].AsTime()
		if !ok {
			return xbson.Null, nil
		}

		return xbson.Int32(int32(get(t))), nil
	}
}

// dateUnit 是 DATEADD 和 DATEDIFF 认的时间单位。
type dateUnit uint8

const (
	unitNone dateUnit = iota
	unitYear
	unitMonth
	unitDay
	unitHour
	unitMinute
	unitSecond
)

// parseUnit 解析单位名，认不出返回 [unitNone]。
//
// **大写的 M 是月，小写的 m 是分**——所以这一个字母先单独判，其余的才转小写比。
func parseUnit(s string) dateUnit {
	if s == "M" {
		return unitMonth
	}
	switch strings.ToLower(s) {
	case "y", "year":
		return unitYear
	case "month":
		return unitMonth
	case "d", "day":
		return unitDay
	case "h", "hour":
		return unitHour
	case "m", "minute":
		return unitMinute
	case "s", "second":
		return unitSecond
	default:
		return unitNone
	}
}

// mDATEADD 给时间加上若干个单位。
//
// 单位认不出、实参类型不对，都返回 Null。年和月的偏移量另有范围限制，超出报错；
// 天及以下不设限，由时间本身的范围去卡。
func (*Ctx) mDATEADD(args []*xbson.Value) (*xbson.Value, error) {
	unitText, ok := str(args[0])
	if !ok || !isNumber(args[1]) {
		return xbson.Null, nil
	}
	t, ok := args[2].AsTime()
	if !ok {
		return xbson.Null, nil
	}
	unit := parseUnit(unitText)
	if unit == unitNone {
		return xbson.Null, nil
	}
	n, err := int32Of(args[1])
	if err != nil {
		return nil, err
	}
	var out time.Time
	switch unit {
	case unitYear:
		if n < -10000 || n > 10000 {
			return nil, errf("DATEADD: year offset %d is out of range", n)
		}
		out = addMonths(t, int(n)*12)
	case unitMonth:
		if n < -120000 || n > 120000 {
			return nil, errf("DATEADD: month offset %d is out of range", n)
		}
		out = addMonths(t, int(n))
	case unitDay:
		out = t.AddDate(0, 0, int(n))
	case unitHour:
		out = t.Add(time.Duration(n) * time.Hour)
	case unitMinute:
		out = t.Add(time.Duration(n) * time.Minute)
	case unitSecond:
		out = t.Add(time.Duration(n) * time.Second)
	default:
	}
	return xbson.DateTime(out)
}

// addMonths 加若干个月，保持时刻和时区不变。
//
// **目标月没有那一天时落到该月最后一天**：1 月 31 日加一个月是 2 月 28 或 29 日。
// 这一点与直接加天数不同。
func addMonths(t time.Time, n int) time.Time {
	y, m, d := t.Date()
	total := int(m) - 1 + n
	ny := y + floorDiv(total, 12)
	nm := time.Month(floorMod(total, 12) + 1)
	if last := daysInMonth(ny, nm); d > last {
		d = last
	}

	return time.Date(ny, nm, d, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), t.Location())
}

// floorDiv 向下取整的整除，负数也往小里走。
func floorDiv(a, b int) int {
	q := a / b
	if a%b < 0 {
		q--
	}
	return q
}

// floorMod 向下取整的取模，结果与除数同号。
func floorMod(a, b int) int {
	m := a % b
	if m < 0 {
		m += b
	}
	return m
}

// daysInMonth 求某年某月有几天：取下个月的第 0 天，也就是本月最后一天。
func daysInMonth(y int, m time.Month) int {
	return time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// mDATEDIFF 求两个时间相差几个单位，结果向零取整。
//
// 先把两个时间都当成**墙上时间**再比：时区差异不计入差值。
// 年和月按日历分量算，天及以下按秒数换算。
func (*Ctx) mDATEDIFF(args []*xbson.Value) (*xbson.Value, error) {
	unitText, ok := str(args[0])
	if !ok {
		return xbson.Null, nil
	}
	start, ok := args[1].AsTime()
	if !ok {
		return xbson.Null, nil
	}
	end, ok := args[2].AsTime()
	if !ok {
		return xbson.Null, nil
	}
	unit := parseUnit(unitText)
	if unit == unitNone {
		return xbson.Null, nil
	}

	start, end = wallOf(start), wallOf(end)
	switch unit {
	case unitYear:
		return xbson.Int32(int32(yearDiff(start, end))), nil
	case unitMonth:
		return monthDiff(start, end)
	default:
	}
	per := int64(1)
	switch unit {
	case unitDay:
		per = 86400
	case unitHour:
		per = 3600
	case unitMinute:
		per = 60
	default:
	}
	return truncToInt32(float64(absSeconds(start, end)/per)*signOf(start, end), "DATEDIFF")
}

// wallOf 取时间的墙上分量，重新钉到 UTC 上，从而抹掉时区的影响。
func wallOf(t time.Time) time.Time {
	y, m, d := t.Date()
	hh, mm, ss := t.Clock()
	return time.Date(y, m, d, hh, mm, ss, t.Nanosecond(), time.UTC)
}

// yearDiff 求相差几个整年。
//
// 年份相减之后，末尾那年还没走到同一个月日就减一。
func yearDiff(start, end time.Time) int {
	years := end.Year() - start.Year()
	switch {
	case start.Month() == end.Month() && end.Day() < start.Day():
		years--
	case end.Month() < start.Month():
		years--
	}
	return years
}

// monthDiff 求相差几个月，不足一个月的部分按天折算后向零取整。
//
// 折算的分母是**结束时间往后一个月**的天数，因此同样的天数差在不同月份里
// 折出来的比例并不相同。
func monthDiff(start, end time.Time) (*xbson.Value, error) {
	comp := (int(end.Month()) + end.Year()*12) - (int(start.Month()) + start.Year()*12)

	span := -float64(int(addMonths(end, 1).Sub(end).Hours() / 24))
	if span == 0 {
		return xbson.Int32(int32(comp)), nil
	}
	months := float64(comp) + float64(start.Day()-end.Day())/span
	return truncToInt32(months, "DATEDIFF")
}

// truncToInt32 向零取整成 32 位整数，NaN 或越界时报错。
func truncToInt32(f float64, who string) (*xbson.Value, error) {
	t := math.Trunc(f)
	if math.IsNaN(t) || t < math.MinInt32 || t > math.MaxInt32 {
		return nil, errf("%s: result %v is out of range for a 32-bit integer", who, f)
	}
	return xbson.Int32(int32(t)), nil
}

// absSeconds 求两个时间相差多少整秒，不足一秒的部分向下舍。
func absSeconds(a, b time.Time) int64 {
	if b.Before(a) {
		a, b = b, a
	}
	s := b.Unix() - a.Unix()
	if b.Nanosecond() < a.Nanosecond() {
		s--
	}
	return s
}

// signOf 返回差值的符号：b 在 a 之前是 -1，否则 1。
func signOf(a, b time.Time) float64 {
	if b.Before(a) {
		return -1
	}
	return 1
}

// mTOLOCAL 把时间换成本地时区。
//
// **未标时区的时间按 UTC 解读再换算**：墙上时刻会随本地偏移量一起挪，
// 而不是原样保留分量、只补上一个时区标记。
func (*Ctx) mTOLOCAL(args []*xbson.Value) (*xbson.Value, error) {
	v := args[0]
	if v.Type() != xbson.TypeDateTime {
		return xbson.Null, nil
	}
	if v.Unspecified() {
		t, ok := v.AsTime()
		if !ok {
			return xbson.Null, nil
		}
		out, err := xbson.DateTime(wallOf(t).In(time.Local))
		if err != nil {
			return xbson.Null, nil
		}
		return out, nil
	}
	return v.In(time.Local), nil
}

// mTOUTC 把时间换成 UTC。
func (*Ctx) mTOUTC(args []*xbson.Value) (*xbson.Value, error) {
	if args[0].Type() != xbson.TypeDateTime {
		return xbson.Null, nil
	}
	return args[0].In(time.UTC), nil
}

// parseDateText 解析日期文本，第二个返回值说明文本里带没带时区。没带时按本地时区算。
func parseDateText(s string, df *xfmt.DateTimeFormat) (time.Time, bool, error) {
	return parseDateIn(s, df, false)
}

// parseDateTextUTC 同 [parseDateText]，但文本没带时区时按 UTC 算。
func parseDateTextUTC(s string, df *xfmt.DateTimeFormat) (time.Time, error) {
	t, _, err := parseDateIn(s, df, true)
	return t, err
}

// parseDateIn 解析日期文本，assumeUniversal 决定不带时区时按哪一边算。
//
// 几件事按顺序做：只有时间没有日期时补上今天；非 ISO 写法要按当前日历
// 换算成公历，年份缺省时也取今年；然后核对这确实是真实存在的一天，
// 写了星期几的还要对得上。
//
// 时区偏移不是整分钟时会被抹到整分钟——时间的存储精度到不了秒以下的偏移。
// 换算完落在 1 年到 9999 年之外就报错。
func parseDateIn(s string, df *xfmt.DateTimeFormat, assumeUniversal bool) (time.Time, bool, error) {
	p, ok := df.ParseDate(s)
	if !ok {
		return time.Time{}, false, errf("cannot parse %q as a date", s)
	}
	now := timeNow()
	kind := df.Kind()
	y, mo, d := p.Year, time.Month(p.Month), p.Day
	switch {
	case p.NoDate:
		y, mo, d = now.Date()
	case p.ISO:
	default:
		if y == 0 {
			cy, _, _, ok := kind.ToCalendar(now.Year(), int(now.Month()), now.Day())
			if !ok {
				return time.Time{}, false, errf("date %q is out of range", s)
			}
			y = cy
		}
		gy, gm, gd, ok := kind.FromCalendar(y, int(mo), d)
		if !ok {
			return time.Time{}, false, errf("date %q is out of range", s)
		}
		y, mo, d = gy, time.Month(gm), gd
	}

	if day := time.Date(y, mo, d, 0, 0, 0, 0, time.UTC); day.Year() != y ||
		day.Month() != mo || day.Day() != d {
		return time.Time{}, false, errf("date %q is not a real date", s)
	} else if p.Weekday >= 0 && int(day.Weekday()) != p.Weekday {
		return time.Time{}, false, errf("date %q has the wrong day of week", s)
	}
	loc, zoned := time.Local, true
	switch {
	case p.Zone == xfmt.ZoneUTC:
		loc = time.UTC
	case p.Zone == xfmt.ZoneOffset:
		loc = time.FixedZone("", p.OffsetMinutes*60)
	case assumeUniversal:
		loc = time.UTC
	default:
		zoned = false
	}
	t := time.Date(y, mo, d, p.Hour, p.Min, p.Sec, p.Frac*100, loc)

	if zoned {
		t = t.Local()
		if _, off := t.Zone(); off%60 != 0 {
			t = t.In(time.FixedZone("", off/60*60))
		}
	} else if _, off := t.Zone(); off%60 != 0 {
		t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(),
			t.Nanosecond(), time.FixedZone("", off/60*60))
	}

	if y := t.Year(); y < 1 || y > 9999 {
		return time.Time{}, false, errf("date %q is out of range after converting to local time", s)
	}
	return t, p.Zone != xfmt.ZoneNone, nil
}
