package xfmt

import "time"

// dtDTT 是驱动状态机的一次输入：一个记号，加上紧跟其后的分隔符。
//
// 「数字」本身不足以决定往哪走——后面跟的是日期分隔符、时间分隔符还是
// 空白，决定了它是月、是时、还是别的什么。所以记号与分隔符合成一个输入。
type dtDTT int

const (
	// 输入类别，编号就是状态转移表的列号，改动次序会让整张表错位。
	//
	// dttUnk 之后的两个不是真正的输入，只是占位：它们落在表外，
	// [dtDS.next] 会把它们直接判成错误。
	dttEnd dtDTT = iota
	dttNumEnd
	dttNumAmpm
	dttNumSpace
	dttNumDatesep
	dttNumTimesep
	dttMonthEnd
	dttMonthSpace
	dttMonthDatesep
	dttNumDatesuff
	dttNumTimesuff
	dttDayOfWeek
	dttYearSpace
	dttYearDateSep
	dttYearEnd
	dttTimeZone
	dttEra
	dttNumUTCTimeMark

	dttUnk
	dttNumLocalTimeMark
)

// dtDS 是状态机的状态。
//
// dsERROR 之前的是「还在读」的中间状态，之后的是**终结状态**——
// 到了那里就说明刚刚读完一个完整的成分（一个日期、一个时间），
// 该把攒下的数字兑现成年月日，然后回到起点接着读下一段。
type dtDS byte

const (
	// 状态编号就是转移表的行号，改动次序会让整张表错位。
	dsBEGIN dtDS = iota
	dsN
	dsNN
	dsDNd
	dsDNN
	dsDNNd
	dsDM
	dsDMN
	dsDNM
	dsDMNd
	dsDNDS
	dsDY
	dsDYN
	dsDYNd
	dsDYM
	dsDYMd
	dsDS
	dsTS
	dsTNt
	dsTNNt
	dsERROR
	dsDXNN
	dsDXNNN
	dsDXMN
	dsDXNM
	dsDXMNN
	dsDXDS
	dsDXDSN
	dsDXNDS
	dsDXNNDS
	dsDXYNN
	dsDXYMN
	dsDXYN
	dsDXYM
	dsTXN
	dsTXNN
	dsTXNNN
	dsTXTS
	dsDXNNY
)

// dtStateWidth 是转移表每行的列数，等于真正的输入类别个数。
const dtStateWidth = 18

// dtStates 是状态转移表，行是当前状态、列是输入类别。
//
// 只有前 20 个状态（dsERROR 之前的那些）会作为「当前状态」出现，
// 所以表只有 20 行；终结状态一到就立刻兑现并回到起点。
var dtStates = [20 * dtStateWidth]dtDS{
	dsBEGIN, dsERROR, dsTXN, dsN, dsDNd, dsTNt, dsERROR, dsDM, dsDM, dsDS, dsTS, dsBEGIN, dsDY, dsDY, dsERROR, dsBEGIN, dsBEGIN, dsERROR,
	dsERROR, dsDXNN, dsTXNN, dsNN, dsDNNd, dsERROR, dsDXNM, dsDNM, dsDMNd, dsDNDS, dsERROR, dsN, dsDYN, dsDYNd, dsDXYN, dsN, dsN, dsERROR,
	dsDXNN, dsDXNNN, dsTXNNN, dsDXNNN, dsERROR, dsTNt, dsDXMNN, dsDXMNN, dsERROR, dsERROR, dsTS, dsNN, dsDXNNY, dsERROR, dsDXNNY, dsNN, dsNN, dsERROR,
	dsERROR, dsDXNN, dsERROR, dsDNN, dsDNNd, dsERROR, dsDXNM, dsDMN, dsDMNd, dsERROR, dsERROR, dsDNd, dsDYN, dsDYNd, dsDXYN, dsERROR, dsDNd, dsERROR,
	dsDXNN, dsDXNNN, dsTXN, dsDXNNN, dsERROR, dsTNt, dsDXMNN, dsDXMNN, dsERROR, dsDXDS, dsTS, dsDNN, dsDXNNY, dsERROR, dsDXNNY, dsERROR, dsDNN, dsERROR,
	dsERROR, dsDXNNN, dsDXNNN, dsDXNNN, dsERROR, dsERROR, dsDXMNN, dsDXMNN, dsERROR, dsDXDS, dsERROR, dsDNNd, dsDXNNY, dsERROR, dsDXNNY, dsERROR, dsDNNd, dsERROR,
	dsERROR, dsDXMN, dsERROR, dsDMN, dsDMNd, dsERROR, dsERROR, dsERROR, dsERROR, dsERROR, dsERROR, dsDM, dsDYM, dsDYMd, dsDXYM, dsERROR, dsDM, dsERROR,
	dsDXMN, dsDXMNN, dsDXMNN, dsDXMNN, dsERROR, dsTNt, dsERROR, dsERROR, dsERROR, dsDXDS, dsTS, dsDMN, dsDXYMN, dsERROR, dsDXYMN, dsERROR, dsDMN, dsERROR,
	dsDXNM, dsDXMNN, dsDXMNN, dsDXMNN, dsERROR, dsTNt, dsERROR, dsERROR, dsERROR, dsDXDS, dsTS, dsDNM, dsDXYMN, dsERROR, dsDXYMN, dsERROR, dsDNM, dsERROR,
	dsERROR, dsDXMNN, dsERROR, dsDXMNN, dsERROR, dsERROR, dsERROR, dsERROR, dsERROR, dsERROR, dsERROR, dsDMNd, dsDXYMN, dsERROR, dsDXYMN, dsERROR, dsDMNd, dsERROR,
	dsDXNDS, dsDXNNDS, dsDXNNDS, dsDXNNDS, dsERROR, dsTNt, dsERROR, dsERROR, dsERROR, dsDNDS, dsTS, dsDNDS, dsERROR, dsERROR, dsERROR, dsERROR, dsDNDS, dsERROR,
	dsERROR, dsDXYN, dsERROR, dsDYN, dsDYNd, dsERROR, dsDXYM, dsDYM, dsDYMd, dsDYM, dsERROR, dsDY, dsERROR, dsERROR, dsERROR, dsERROR, dsDY, dsERROR,
	dsDXYN, dsDXYNN, dsDXYNN, dsDXYNN, dsERROR, dsERROR, dsDXYMN, dsDXYMN, dsERROR, dsERROR, dsERROR, dsDYN, dsERROR, dsERROR, dsERROR, dsERROR, dsDYN, dsERROR,
	dsERROR, dsDXYNN, dsDXYNN, dsDXYNN, dsERROR, dsERROR, dsDXYMN, dsDXYMN, dsERROR, dsERROR, dsERROR, dsDYN, dsERROR, dsERROR, dsERROR, dsERROR, dsDYN, dsERROR,
	dsDXYM, dsDXYMN, dsDXYMN, dsDXYMN, dsERROR, dsERROR, dsERROR, dsERROR, dsERROR, dsERROR, dsERROR, dsDYM, dsERROR, dsERROR, dsERROR, dsERROR, dsDYM, dsERROR,
	dsERROR, dsDXYMN, dsDXYMN, dsDXYMN, dsERROR, dsERROR, dsERROR, dsERROR, dsERROR, dsERROR, dsERROR, dsDYM, dsERROR, dsERROR, dsERROR, dsERROR, dsDYM, dsERROR,
	dsDXDS, dsDXDSN, dsTXN, dsTNt, dsERROR, dsTNt, dsERROR, dsERROR, dsERROR, dsDS, dsTS, dsDS, dsERROR, dsERROR, dsERROR, dsERROR, dsDS, dsERROR,
	dsTXTS, dsTXTS, dsTXTS, dsTNt, dsDNd, dsERROR, dsERROR, dsERROR, dsERROR, dsDS, dsTS, dsTS, dsERROR, dsERROR, dsERROR, dsTS, dsTS, dsERROR,
	dsERROR, dsTXNN, dsTXNN, dsTXNN, dsERROR, dsTNNt, dsDXNM, dsDNM, dsERROR, dsERROR, dsTS, dsERROR, dsERROR, dsERROR, dsERROR, dsTNt, dsTNt, dsTXNN,
	dsERROR, dsTXNNN, dsTXNNN, dsTXNNN, dsERROR, dsERROR, dsERROR, dsERROR, dsERROR, dsERROR, dsTS, dsTNNt, dsERROR, dsERROR, dsERROR, dsTNNt, dsTNNt, dsTXNNN,
}

// next 查转移表。超出真正输入类别的一律判错。
func (s dtDS) next(t dtDTT) dtDS {
	if t >= dttUnk {
		return dsERROR
	}
	return dtStates[int(s)*dtStateWidth+int(t)]
}

// dtSep 是记号之后紧跟的分隔符种类。
//
// sepDateOrOffset 是个悬而未决的情形：一个 `-` 既可能是日期分隔符，
// 也可能是时区偏移的负号，要看状态机走得通哪一边。
type dtSep int

const (
	// 分隔符种类。sepBad 只作占位，不会真正出现。
	sepEnd dtSep = iota
	sepSpace
	sepDate
	sepTime
	sepAm
	sepPm
	sepYearSuff
	sepMonthSuff
	sepDaySuff
	sepHourSuff
	sepMinuteSuff
	sepSecondSuff
	sepLocalTimeMark

	sepDateOrOffset
	sepBad
)

// dtRun 是一次解析的全部状态。
//
// nums 攒着还没兑现的数字——它们的含义要等到读出分隔符、走到终结状态
// 才能定下来（12/05 是几月几号，取决于这种语言的月日次序）。
//
// res 开头的是已经定下来的结果，have 开头的标记哪些成分已经出现过，
// 用来挡住「同一个成分出现两次」这种输入。
type dtRun struct {
	df *DateTimeFormat

	// nums 攒着还没定下含义的数字，最多三个；numCount 是已攒的个数。
	nums     [3]dtok
	numCount int
	year     int
	month    int
	dayName  int
	timeMark int
	frac     int

	// res 开头的是已经定下来的年月日时分秒，-1 表示还没定。
	resY, resM, resD   int
	resH, resMin, resS int
	// have 开头的标记这个成分已经出现过，再出现一次就是错的。
	haveDate, haveTime  bool
	haveY, haveM, haveD bool
	haveH, haveMin      bool
	haveS               bool

	// suffix 是刚读到的那个后缀标记（「年」「月」这类），供 [dtRun.applySuffix] 用。
	suffix dtSep

	// zone 与 offMin 是时区信息。
	zone   ZoneKind
	offMin int

	// yearDefaulted 表示年份是用今年补的，不是输入里写的。
	//
	// 补出来的年份最后要抹掉，让上层知道这是个「没写年」的日期。
	yearDefaulted bool
	// reachTerminal 表示至少走到过一次终结状态。
	//
	// 一次都没到过就说明整串没读出任何完整成分，那不算解析成功。
	reachTerminal bool
}

// validYMD 报告这一组年月日在当前历法下是不是真实存在的日期。
//
// 不只是范围检查：换算回公历之后还要落在可表示范围内，
// 否则存不下来。
func (df *DateTimeFormat) validYMD(y, m, d int) bool {
	if m < 1 || m > 12 || d < 1 {
		return false
	}
	switch df.Kind() {
	case CalendarPersian:
		if y < 1 || y > persianMaxYear || d > persianDaysInMonth(y, m) {
			return false
		}
		day, ok := persianToDay(y, m, d)
		return ok && day >= gregorianMinDay && day <= gregorianMaxDay
	case CalendarThaiBuddhist:
		gy := y - thaiBuddhistOffset
		return gy >= 1 && gy <= 9999 && d <= gregorianDaysInMonth(gy, m)
	default:
		return y >= 1 && y <= 9999 && d <= gregorianDaysInMonth(y, m)
	}
}

// gregorianDaysInMonth 返回公历某月有多少天。
func gregorianDaysInMonth(y, m int) int {
	switch m {
	case 1, 3, 5, 7, 8, 10, 12:
		return 31
	case 4, 6, 9, 11:
		return 30
	case 2:
		if y%4 == 0 && (y%100 != 0 || y%400 == 0) {
			return 29
		}
		return 28
	}
	return 0
}

// setDate 定下年月日，日期不成立时报 false。
func (r *dtRun) setDate(y, m, d int) bool {
	if !r.df.validYMD(y, m, d) {
		return false
	}
	r.resY, r.resM, r.resD = y, m, d
	return true
}

// adjYear 把一个年份记号补全，两位年按这种语言的百年段规则展开。
func (r *dtRun) adjYear(t dtok) (int, bool) { return (&dateBuilder{df: r.df}).yearOf(t) }

// terminal 走到终结状态时兑现攒下的数字。
//
// 各个终结状态对应一种组合（两个数字、三个数字、月名加一个数字……），
// 兑现之后清空计数，让状态机能接着读下一段。
func (r *dtRun) terminal(s dtDS) bool {
	ok := true
	switch s {
	case dsDXNN:
		ok = r.dayOfNN()
	case dsDXNNN:
		ok = r.dayOfNNN()
	case dsDXMN:
		ok = r.dayOfMN()
	case dsDXNM:
		ok = r.dayOfNM()
	case dsDXMNN:
		ok = r.dayOfMNN()
	case dsDXYNN:
		ok = r.dayOfYNN()
	case dsDXNNY:
		ok = r.dayOfNNY()
	case dsDXYMN:
		ok = r.setDateFlag(r.year, r.month, int(r.nums[0].num))
	case dsDXYN:
		ok = r.setDateFlag(r.year, int(r.nums[0].num), 1)
	case dsDXYM:
		ok = r.setDateFlag(r.year, r.month, 1)
	case dsTXN:
		ok = r.timeOf(1)
	case dsTXNN:
		ok = r.timeOf(2)
	case dsTXNNN:
		ok = r.timeOf(3)
	case dsDXDS, dsTXTS:
	case dsDXDSN:
		ok = r.dateOfDSN()
	case dsDXNDS:
		ok = r.dateOfNDS()
	case dsDXNNDS:
		ok = r.dateOfNNDS()
	}
	if !ok {
		return false
	}

	r.numCount = 0
	return true
}

// setDateFlag 定下日期并标记「日期已出现」，重复出现时报 false。
func (r *dtRun) setDateFlag(y, m, d int) bool {
	if r.haveDate {
		return false
	}
	if !r.setDate(y, m, d) {
		return false
	}
	r.haveDate = true
	return true
}

// defaultYear 用今年补上缺失的年份，并记下这是补的。
//
// 取的是当前历法下的今年，不是公历年。
func (r *dtRun) defaultYear() int {
	now := time.Now()
	y, _, _, ok := r.df.Kind().ToCalendar(now.Year(), int(now.Month()), now.Day())
	if !ok {
		return 0
	}
	r.yearDefaulted = true
	return y
}

// dayOfNN 兑现「两个数字」：按这种语言的月日次序判定哪个是月、哪个是日。
//
// 次序判定不出来就报失败，而不是猜一个——猜错会静默地把 3/4 读成四月三日。
func (r *dtRun) dayOfNN() bool {
	if r.haveDate {
		return false
	}
	n1, n2 := int(r.nums[0].num), int(r.nums[1].num)
	y := r.defaultYear()

	md, known := r.df.monthDayOrder()
	if !known {
		return false
	}
	if md == "Md" {
		if !r.setDate(y, n1, n2) {
			return false
		}
	} else if !r.setDate(y, n2, n1) {
		return false
	}
	r.haveDate = true
	return true
}

// dayOfNNN 兑现「三个数字」，按这种语言的短日期次序分配年月日。
func (r *dtRun) dayOfNNN() bool {
	if r.haveDate {
		return false
	}
	n1, n2, n3 := r.nums[0], r.nums[1], r.nums[2]
	order, known := r.df.shortDateOrder()
	if !known {
		return false
	}
	var y int
	var ok bool
	switch order {
	case "yMd":
		y, ok = r.adjYear(n1)
		ok = ok && r.setDate(y, int(n2.num), int(n3.num))
	case "Mdy":
		y, ok = r.adjYear(n3)
		ok = ok && r.setDate(y, int(n1.num), int(n2.num))
	case "dMy":
		y, ok = r.adjYear(n3)
		ok = ok && r.setDate(y, int(n2.num), int(n1.num))
	default:
		y, ok = r.adjYear(n1)
		ok = ok && r.setDate(y, int(n3.num), int(n2.num))
	}
	if !ok {
		return false
	}
	r.haveDate = true
	return true
}

// dayOfMN 兑现「月名 + 一个数字」。
//
// 那个数字可能是日，也可能是年：这种语言的月日次序是「日在前」而
// 年月次序是「月在前」时，读成年月更合理（比如 "March 2024"）。
func (r *dtRun) dayOfMN() bool {
	if r.haveDate {
		return false
	}
	md, known := r.df.monthDayOrder()
	if !known {
		return false
	}
	if md == "dM" {
		ym, ok := r.df.yearMonthOrder()
		if !ok {
			return false
		}
		if ym == "My" {
			y, ok := r.adjYear(r.nums[0])
			if !ok || !r.setDate(y, r.month, 1) {
				return false
			}
			r.haveDate = true
			return true
		}
	}
	if !r.setDate(r.defaultYear(), r.month, int(r.nums[0].num)) {
		return false
	}
	r.haveDate = true
	return true
}

// dayOfNM 兑现「一个数字 + 月名」，判定与 [dtRun.dayOfMN] 对称。
func (r *dtRun) dayOfNM() bool {
	if r.haveDate {
		return false
	}
	md, known := r.df.monthDayOrder()
	if !known {
		return false
	}
	if md == "Md" {
		ym, ok := r.df.yearMonthOrder()
		if !ok {
			return false
		}
		if ym == "yM" {
			y, ok := r.adjYear(r.nums[0])
			if !ok || !r.setDate(y, r.month, 1) {
				return false
			}
			r.haveDate = true
			return true
		}
	}
	if !r.setDate(r.defaultYear(), r.month, int(r.nums[0].num)) {
		return false
	}
	r.haveDate = true
	return true
}

// dayOfMNN 兑现「月名 + 两个数字」。
//
// 两个数字谁是日谁是年，先按这种语言的次序试，不成立再换过来试：
// "March 5 2024" 与 "March 2024 5" 在多数语言下只有一种解读成立。
func (r *dtRun) dayOfMNN() bool {
	if r.haveDate {
		return false
	}
	n1, n2 := r.nums[0], r.nums[1]
	order, known := r.df.shortDateOrder()
	if !known {
		return false
	}
	var first, second [2]dtok
	switch order {
	case "Mdy", "dMy":
		first, second = [2]dtok{n1, n2}, [2]dtok{n2, n1}
	case "yMd":
		first, second = [2]dtok{n2, n1}, [2]dtok{n1, n2}
	default:
		return false
	}
	for _, c := range [2][2]dtok{first, second} {
		if y, ok := r.adjYear(c[1]); ok && r.df.validYMD(y, r.month, int(c[0].num)) {
			if !r.setDate(y, r.month, int(c[0].num)) {
				return false
			}
			r.haveDate = true
			return true
		}
	}
	return false
}

// dayOfYNN 兑现「四位年 + 两个数字」。
func (r *dtRun) dayOfYNN() bool {
	if r.haveDate {
		return false
	}
	n1, n2 := int(r.nums[0].num), int(r.nums[1].num)

	if order, known := r.df.shortDateOrder(); known && order == "ydM" {
		n1, n2 = n2, n1
	}
	if !r.setDate(r.year, n1, n2) {
		return false
	}
	r.haveDate = true
	return true
}

// dayOfNNY 兑现「两个数字 + 四位年」。
func (r *dtRun) dayOfNNY() bool {
	if r.haveDate {
		return false
	}
	n1, n2 := int(r.nums[0].num), int(r.nums[1].num)
	order, known := r.df.shortDateOrder()
	if !known {
		return false
	}
	if order != "Mdy" && order != "yMd" {
		n1, n2 = n2, n1
	}
	if !r.setDate(r.year, n1, n2) {
		return false
	}
	r.haveDate = true
	return true
}

// timeOf 兑现时间：一到三个数字依次是时、分、秒。
//
// 只有一个数字时必须带上下午标记——不然 "5" 到底是五点还是别的什么，
// 无从判断。
func (r *dtRun) timeOf(n int) bool {
	if r.haveTime {
		return false
	}
	if n == 1 && r.timeMark < 0 {
		return false
	}
	r.resH = int(r.nums[0].num)
	if n >= 2 {
		r.resMin = int(r.nums[1].num)
	}
	if n >= 3 {
		r.resS = int(r.nums[2].num)
	}
	r.haveTime = true
	return true
}

// dateOfDSN 兑现后缀式日期尾部多出来的那个日。
func (r *dtRun) dateOfDSN() bool {
	if r.numCount != 1 || r.resD != -1 {
		return false
	}
	r.resD = int(r.nums[0].num)
	return true
}

// dateOfNDS 兑现后缀式日期前面多出来的那个年。
func (r *dtRun) dateOfNDS() bool {
	if r.resM == -1 || r.resY != -1 {
		return false
	}
	y, ok := r.adjYear(r.nums[0])
	if !ok {
		return false
	}
	r.resY, r.resD = y, 1
	return true
}

// dateOfNNDS 兑现后缀式日期前面多出来的两个数字。
//
// 已经有年时它们是月与日；已经有月时它们是年与日，次序按这种语言的
// 短日期次序定。
func (r *dtRun) dateOfNNDS() bool {
	n1, n2 := int(r.nums[0].num), int(r.nums[1].num)
	switch {
	case r.haveY && !r.haveM && !r.haveD:
		if y, ok := r.adjYear(dtok{num: int64(r.year), digits: digitsOf(r.year)}); ok &&
			r.setDate(y, n1, n2) {
			return true
		}
	case r.haveM && !r.haveY && !r.haveD:
		order, known := r.df.shortDateOrder()
		if !known {
			return false
		}
		if order != "yMd" {
			n1, n2 = n2, n1
		}
		if y, ok := r.adjYear(dtok{num: int64(n1), digits: digitsOf(n1)}); ok &&
			r.setDate(y, r.resM, n2) {
			return true
		}
	}
	return false
}

// digitsOf 返回一个数有几位。
func digitsOf(n int) int {
	d := 1
	for n >= 10 {
		n /= 10
		d++
	}
	return d
}

// sepAfter 跳过空白，看下一个记号是哪种分隔符，返回它与继续读的位置。
//
// 分隔符本身被吃掉；只是空白或已到末尾时位置不前进。
func (toks dtoks) sepAfter(i int) (dtSep, int) {
	j := toks.skipSpace(i)
	if j >= len(toks) {
		return sepEnd, j
	}
	switch toks[j].kind {
	case dtDateSep:
		return sepDate, j + 1
	case dtTimeSep:
		return sepTime, j + 1
	case dtAmPm:
		if toks[j].num == 0 {
			return sepAm, j + 1
		}
		return sepPm, j + 1
	case dtT:
		return sepLocalTimeMark, j + 1
	case dtDash:
		return sepDateOrOffset, j + 1
	case dtMark:
		return [6]dtSep{sepYearSuff, sepMonthSuff, sepDaySuff,
			sepHourSuff, sepMinuteSuff, sepSecondSuff}[toks[j].num], j + 1
	}
	return sepSpace, j
}

// isFracDelim 报告这个记号能不能当秒的小数点用——点与逗号都算。
func (t dtok) isFracDelim() bool {
	switch t.kind {
	case dtDot, dtComma:
		return true
	case dtDateSep, dtTimeSep:
		return t.raw == '.' || t.raw == ','
	}
	return false
}

// dtAssemble 用状态机把一串记号拼成日期各部分。
//
// 主循环：每读一个记号，连同它后面的分隔符算出一个输入类别，查转移表走一步；
// 走到终结状态就把攒下的数字兑现成年月日，回到起点接着读。
//
// 有两处是「先看走得通哪边」的回退：
//
//   - `-` 既可能是日期分隔符也可能是时区偏移的负号；
//   - 这种语言的日期分隔符与时间分隔符相同时（比如都用点），
//     一个分隔符的含义只能靠状态机试出来。
//
// 两处都是先试更具体的那种，走不通再退到更宽松的那种。
//
// 整串读完还要送一个「结束」输入，让最后一段有机会兑现；
// 一次终结状态都没到过则判失败。
func (df *DateTimeFormat) dtAssemble(toks dtoks) (DateParts, bool) {
	r := &dtRun{df: df, year: -1, month: -1, dayName: -1, timeMark: -1,
		resY: -1, resM: -1, resD: -1}
	dps := dsBEGIN
	sameSep := df.DateSeparator == df.TimeSeparator
	for i := 0; i < len(toks); {
		t := toks[i]
		dtt := dttUnk
		next := i + 1
		switch t.kind {
		case dtSpace, dtIgnorable:
			i++
			continue
		case dtComma, dtDot, dtDateSep, dtTimeSep:
			if !r.df.ignorableSym(t.raw) {
				return DateParts{}, false
			}
			i++
			continue
		case dtDash, dtPlus:
			if r.df.LangName == "ky" && t.kind == dtDash {
				if r.haveTime {
					if n, ok := r.readZone(toks, i); ok {
						i = n
						continue
					}
				}
				i++
				continue
			}
			if n, ok := r.readZone(toks, i); ok {
				i = n
				continue
			}
			return DateParts{}, false
		case dtT:
			return DateParts{}, false
		case dtEra:
			dtt = dttEra
		case dtDayName:
			if r.dayName >= 0 {
				return DateParts{}, false
			}
			r.dayName = int(t.num)
			dtt = dttDayOfWeek
		case dtZulu:
			if r.zone != ZoneNone {
				return DateParts{}, false
			}
			r.zone = ZoneUTC
			dtt = dttTimeZone
		case dtAmPm:
			if r.timeMark >= 0 {
				return DateParts{}, false
			}
			r.timeMark = int(t.num)
			i++
			continue
		case dtMonth:
			if r.month != -1 {
				return DateParts{}, false
			}
			var sep dtSep
			sep, next = toks.sepAfter(i + 1)
			switch sep {
			case sepEnd:
				dtt = dttMonthEnd
			case sepSpace:
				dtt = dttMonthSpace
			case sepDate:
				dtt = dttMonthDatesep
			case sepDateOrOffset:
				if dps.next(dttMonthDatesep) == dsERROR && dps.next(dttMonthSpace) > dsERROR {
					next = i + 1
					dtt = dttMonthSpace
				} else {
					dtt = dttMonthDatesep
				}
			case sepTime:
				if !sameSep {
					return DateParts{}, false
				}
				dtt = dttMonthDatesep
			default:
				return DateParts{}, false
			}
			r.month = int(t.num)
		case dtNum:
			if r.numCount == 3 || t.digits > 8 {
				return DateParts{}, false
			}

			from := i + 1
			if dps == dsTNNt && from < len(toks) && toks[from].isFracDelim() {
				if from+1 < len(toks) && toks[from+1].kind == dtNum {
					r.frac = toks[from+1].fracTicks()
					from += 2
				} else {
					from++
				}
			}
			var sep dtSep
			sep, next = toks.sepAfter(from)
			if t.digits >= 3 {
				if r.year != -1 {
					return DateParts{}, false
				}
				r.year = int(t.num)
				switch sep {
				case sepEnd:
					dtt = dttYearEnd
				case sepAm, sepPm:
					if r.timeMark >= 0 {
						return DateParts{}, false
					}
					r.timeMark = boolToInt(sep == sepPm)
					dtt = dttYearSpace
				case sepSpace:
					dtt = dttYearSpace
				case sepDate:
					dtt = dttYearDateSep
				case sepDateOrOffset:
					if dps.next(dttYearDateSep) == dsERROR && dps.next(dttYearSpace) > dsERROR {
						next = from
						dtt = dttYearSpace
					} else {
						dtt = dttYearDateSep
					}
				case sepTime:
					if !sameSep {
						return DateParts{}, false
					}
					dtt = dttYearDateSep
				case sepYearSuff, sepMonthSuff, sepDaySuff:
					dtt, r.suffix = dttNumDatesuff, sep
				case sepHourSuff, sepMinuteSuff, sepSecondSuff:
					dtt, r.suffix = dttNumTimesuff, sep
				default:
					return DateParts{}, false
				}
			} else {
				switch sep {
				case sepEnd:
					dtt = dttNumEnd
					r.addNum(t)
				case sepAm, sepPm:
					if r.timeMark >= 0 {
						return DateParts{}, false
					}
					r.timeMark = boolToInt(sep == sepPm)
					dtt = dttNumAmpm

					if dps == dsDNN {
						if !r.terminal(dsDXNN) {
							return DateParts{}, false
						}
					}
					r.addNum(t)
				case sepSpace:
					dtt = dttNumSpace
					r.addNum(t)
				case sepDate:
					dtt = dttNumDatesep
					r.addNum(t)
				case sepDateOrOffset:
					if dps.next(dttNumDatesep) == dsERROR && dps.next(dttNumSpace) > dsERROR {
						next = from
						dtt = dttNumSpace
					} else {
						dtt = dttNumDatesep
					}
					r.addNum(t)
				case sepTime:
					if sameSep && (dps == dsDY || dps == dsDYN || dps == dsDYNd ||
						dps == dsDYM || dps == dsDYMd) {
						dtt = dttNumDatesep
					} else {
						dtt = dttNumTimesep
					}
					r.addNum(t)
				case sepYearSuff:
					y, ok := r.adjYear(t)
					if !ok {
						return DateParts{}, false
					}
					t.num, t.digits = int64(y), digitsOf(y)
					dtt, r.suffix = dttNumDatesuff, sep
				case sepMonthSuff, sepDaySuff:
					dtt, r.suffix = dttNumDatesuff, sep
				case sepHourSuff, sepMinuteSuff, sepSecondSuff:
					dtt, r.suffix = dttNumTimesuff, sep
				default:
					return DateParts{}, false
				}
			}
			if dtt == dttNumDatesuff || dtt == dttNumTimesuff {
				if !r.applySuffix(int(t.num)) {
					return DateParts{}, false
				}
			}
		default:
			return DateParts{}, false
		}

		if sameSep {
			if dtt == dttYearEnd || dtt == dttYearSpace || dtt == dttYearDateSep {
				if dps == dsTNt {
					dps = dsDNd
				}
				if dps == dsTNNt {
					dps = dsDNNd
				}
			}
			atEnd := toks.skipSpace(next) == len(toks)
			if dps.next(dtt) == dsERROR || atEnd {
				switch dtt {
				case dttYearDateSep:
					dtt = pick(atEnd, dttYearEnd, dttYearSpace)
				case dttNumDatesep, dttNumTimesep:
					dtt = pick(atEnd, dttNumEnd, dttNumSpace)
				case dttMonthDatesep:
					dtt = pick(atEnd, dttMonthEnd, dttMonthSpace)
				}
			}
		}

		if dtt != dttUnk {
			dps = dps.next(dtt)
			if dps == dsERROR {
				return DateParts{}, false
			}
			if dps > dsERROR {
				if !r.terminal(dps) {
					return DateParts{}, false
				}
				r.reachTerminal = true
				dps = dsBEGIN
			}
		}
		i = next
	}

	dps = dps.next(dttEnd)
	if dps == dsERROR {
		return DateParts{}, false
	}
	if dps > dsERROR {
		if !r.terminal(dps) {
			return DateParts{}, false
		}
		r.reachTerminal = true
	}
	if !r.reachTerminal {
		return DateParts{}, false
	}
	return r.finish()
}

// boolToInt 把布尔当 0/1 用。
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// pick 按条件二选一。
func pick[T any](c bool, a, b T) T {
	if c {
		return a
	}
	return b
}

// addNum 把一个还没定含义的数字攒起来。
func (r *dtRun) addNum(t dtok) {
	r.nums[r.numCount] = t
	r.numCount++
}

// applySuffix 按刚读到的后缀标记（「年」「月」「日」「时」「分」「秒」）
// 把数字直接定到对应成分上。
//
// 带后缀的写法不必猜次序，所以不走攒数字那条路。同一个成分出现两次
// 报失败。
func (r *dtRun) applySuffix(n int) bool {
	switch r.suffix {
	case sepYearSuff:
		if r.haveY {
			return false
		}
		r.haveY, r.resY, r.year = true, n, n
	case sepMonthSuff:
		if r.haveM {
			return false
		}

		r.haveM, r.resM, r.month = true, n, n
	case sepDaySuff:
		if r.haveD {
			return false
		}
		r.haveD, r.resD = true, n
	case sepHourSuff:
		if r.haveH {
			return false
		}
		r.haveH, r.resH = true, n
	case sepMinuteSuff:
		if r.haveMin {
			return false
		}
		r.haveMin, r.resMin = true, n
	case sepSecondSuff:
		if r.haveS {
			return false
		}
		r.haveS, r.resS = true, n
	}
	return true
}

// readZone 试着从这里读出一段时区偏移。
func (r *dtRun) readZone(toks dtoks, i int) (int, bool) {
	b := &dateBuilder{df: r.df, hasTime: r.haveTime, zone: r.zone}
	b.nums = b.nums[:0]
	for k := 0; k < r.numCount; k++ {
		b.nums = append(b.nums, r.nums[k])
	}
	n, ok := b.readZoneNet(toks, i)
	if ok {
		r.zone, r.offMin = b.zone, b.offMin
	}
	return n, ok
}

// finish 收尾：处理上下午、检查各成分的范围、决定年月日怎么填。
//
// 上下午标记只有一个的语言（另一个是空串）当作总是那一个：
// 不然带那个标记的时间永远读不出来。
//
// 秒的小数进位到整一秒时秒数加一，交给上层去处理跨分钟。
//
// 年月日一个都没有时标记成「只有时间」；只有年时补成一月一日；
// 年份是补出来的则抹成 0，让上层知道输入里没写年。
func (r *dtRun) finish() (DateParts, bool) {
	if r.timeMark < 0 {
		if r.df.AMDesignator == "" && r.df.PMDesignator != "" {
			r.timeMark = 0
		}
		if r.df.PMDesignator == "" && r.df.AMDesignator != "" {
			r.timeMark = 1
		}
	}

	switch r.timeMark {
	case 0:
		if r.resH < 0 || r.resH > 12 {
			return DateParts{}, false
		}
		if r.resH == 12 {
			r.resH = 0
		}
	case 1:
		if r.resH < 0 || r.resH > 23 {
			return DateParts{}, false
		}
		if r.resH < 12 {
			r.resH += 12
		}
	}
	out := DateParts{Hour: r.resH, Min: r.resMin, Sec: r.resS, Frac: r.frac,
		Zone: r.zone, OffsetMinutes: r.offMin, Weekday: r.dayName}

	if out.Frac == 10000000 {
		out.Frac, out.Sec = 0, out.Sec+1
	}
	if out.Hour > 23 || out.Min > 59 || out.Sec > 60 {
		return DateParts{}, false
	}

	switch {
	case r.resY == -1 && r.resM == -1 && r.resD == -1:
		out.NoDate = true
	case r.resM == -1 && r.resD == -1:
		out.Year, out.Month, out.Day = r.resY, 1, 1
	default:
		out.Year, out.Month, out.Day = r.resY, max(r.resM, 1), max(r.resD, 1)
		if r.resY == -1 || r.yearDefaulted {
			out.Year = 0
		}
	}
	if !out.zoneInRange() {
		return DateParts{}, false
	}
	return out, true
}
