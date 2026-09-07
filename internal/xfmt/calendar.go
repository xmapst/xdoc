package xfmt

import (
	"errors"
	"math/bits"
	"sync"
)

// CalendarKind 是排版日期时用哪一套历法。
//
// 日期本身始终按公历存放，只有排版与解析时才换算。
type CalendarKind int

const (
	// CalendarGregorian 是公历，不做换算。
	CalendarGregorian CalendarKind = iota

	// CalendarPersian 是波斯历，年月日与公历都不同。
	CalendarPersian

	// CalendarThaiBuddhist 是泰国佛历，只是年份加一个固定偏移。
	CalendarThaiBuddhist
)

// Kind 返回这套日期格式该用哪一套历法，认不出来的按公历。
func (df *DateTimeFormat) Kind() CalendarKind {
	switch df.Calendar {
	case "PersianCalendar":
		return CalendarPersian
	case "ThaiBuddhistCalendar":
		return CalendarThaiBuddhist
	}
	return CalendarGregorian
}

// thaiBuddhistOffset 是佛历年与公历年的差。
const thaiBuddhistOffset = 543

// persianEpochDay 是波斯历元年元旦的日序号（公历 622-03-22）。
const persianEpochDay = 226895

const (
	// gregorianMinDay、gregorianMaxDay 是可表示的日序号范围，对应 0001-01-01 到 9999-12-31。
	gregorianMinDay = 0
	gregorianMaxDay = 3652058
)

// daysFromCivil 把公历年月日换算成日序号，0 是 0001-01-01。
//
// 把三月当作一年的开头（二月挪到年末），闰日就落在年尾，
// 四百年一个周期里的天数因此可以直接用整数算式算出来，不必查表也不必循环。
func daysFromCivil(y, m, d int) int {
	if m <= 2 {
		y--
	}
	era := y / 400
	if y < 0 {
		era = (y - 399) / 400
	}
	yoe := y - era*400
	mp := m + 9
	if m > 2 {
		mp = m - 3
	}
	doy := (153*mp+2)/5 + d - 1
	doe := yoe*365 + yoe/4 - yoe/100 + doy

	return era*146097 + doe - 719468 + 719162
}

// civilFromDays 是 [daysFromCivil] 的逆运算。
func civilFromDays(z int) (y, m, d int) {
	z = z - 719162 + 719468
	era := z / 146097
	if z < 0 {
		era = (z - 146096) / 146097
	}
	doe := z - era*146097
	yoe := (doe - doe/1460 + doe/36524 - doe/146096) / 365
	y = yoe + era*400
	doy := doe - (365*yoe + yoe/4 - yoe/100)
	mp := (5*doy + 2) / 153
	d = doy - (153*mp+2)/5 + 1
	m = mp + 3
	if mp >= 10 {
		m = mp - 9
	}
	if m <= 2 {
		y++
	}
	return y, m, d
}

// persianLeap 报告某个波斯历年是不是闰年。
//
// 波斯历的闰年不是简单的周期规则，只能查表。
func persianLeap(y int) bool {
	if y < 1 || y > persianMaxYear {
		return false
	}
	i := y - 1
	return persianLeapBits[i/64]&(1<<uint(i%64)) != 0
}

// persianLeapPrefix 是闰年位图的前缀和，按 64 年一段。
//
// 算某年之前有多少个闰年时要数位，逐年数是线性的；有了前缀和，
// 只剩最后不足一段的部分要数。第一次用到时才建。
var persianLeapPrefix = sync.OnceValue(func() *[len(persianLeapBits) + 1]uint16 {
	var p [len(persianLeapBits) + 1]uint16
	n := 0
	for i, w := range persianLeapBits {
		n += bits.OnesCount64(w)
		p[i+1] = uint16(n)
	}
	return &p
})

// persianLeapsBefore 数出第 y 年之前有多少个闰年。
func persianLeapsBefore(y int) int {
	i := y - 1
	if i <= 0 {
		return 0
	}
	if i > persianMaxYear {
		i = persianMaxYear
	}
	p := persianLeapPrefix()
	n := int(p[i/64])
	if r := i % 64; r != 0 {
		n += bits.OnesCount64(persianLeapBits[i/64] & (1<<uint(r) - 1))
	}
	return n
}

// persianYearStart 返回某个波斯历年元旦的日序号。
func persianYearStart(y int) int {
	return persianEpochDay + (y-1)*365 + persianLeapsBefore(y)
}

// persianMonthStart 返回某月第一天距年初多少天。
//
// 前六个月各 31 天，之后各 30 天。
func persianMonthStart(m int) int {
	if m <= 7 {
		return (m - 1) * 31
	}
	return 186 + (m-7)*30
}

// persianDaysInMonth 返回某月有多少天，末月闰年 30 天、平年 29 天。
func persianDaysInMonth(y, m int) int {
	switch {
	case m <= 6:
		return 31
	case m <= 11:
		return 30
	case persianLeap(y):
		return 30
	default:
		return 29
	}
}

// persianFromDay 把日序号换算成波斯历年月日。
//
// 年份靠二分查找定位：年长不固定（闰年多一天），除不出来。
func persianFromDay(day int) (y, m, d int, ok bool) {
	if day < persianEpochDay {
		return 0, 0, 0, false
	}

	lo, hi := 1, persianMaxYear
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if persianYearStart(mid) <= day {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	y = lo
	n := day - persianYearStart(y)
	if n >= 365+boolInt(persianLeap(y)) {
		return 0, 0, 0, false
	}
	m = 1
	for m < 12 && n >= persianMonthStart(m+1) {
		m++
	}
	d = n - persianMonthStart(m) + 1
	return y, m, d, true
}

// persianToDay 把波斯历年月日换算成日序号，日期不合法时报 false。
func persianToDay(y, m, d int) (int, bool) {
	if y < 1 || y > persianMaxYear || m < 1 || m > 12 || d < 1 {
		return 0, false
	}
	if d > persianDaysInMonth(y, m) {
		return 0, false
	}
	return persianYearStart(y) + persianMonthStart(m) + d - 1, true
}

// boolInt 把布尔当 0/1 用。
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ToCalendar 把公历日期换算到这套历法。
func (k CalendarKind) ToCalendar(gy, gm, gd int) (y, m, d int, ok bool) {
	switch k {
	case CalendarThaiBuddhist:
		return gy + thaiBuddhistOffset, gm, gd, true
	case CalendarPersian:
		return persianFromDay(daysFromCivil(gy, gm, gd))
	default:
		return gy, gm, gd, true
	}
}

// FromCalendar 把这套历法的日期换算回公历。
//
// 换出来落在可表示范围之外时报 false：那说明输入虽然在本历法里合法，
// 但没有对应的公历日期可存。
func (k CalendarKind) FromCalendar(y, m, d int) (gy, gm, gd int, ok bool) {
	switch k {
	case CalendarThaiBuddhist:
		gy = y - thaiBuddhistOffset
		if gy < 1 || gy > 9999 {
			return 0, 0, 0, false
		}
		return gy, m, d, true
	case CalendarPersian:
		day, ok := persianToDay(y, m, d)
		if !ok || day < gregorianMinDay || day > gregorianMaxDay {
			return 0, 0, 0, false
		}
		gy, gm, gd = civilFromDays(day)
		return gy, gm, gd, true
	default:
		if y < 1 || y > 9999 {
			return 0, 0, 0, false
		}
		return y, m, d, true
	}
}

// ErrCalendarRange 表示日期超出了这套历法能表达的范围。
var ErrCalendarRange = errors.New("xfmt: date is outside the range of this culture's calendar")
