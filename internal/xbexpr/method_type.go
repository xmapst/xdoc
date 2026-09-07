package xbexpr

import (
	"encoding/base64"
	"strings"
	"time"
	"unicode"

	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xfmt"
)

// init 登记类型构造、类型判断和取当前时刻这几类方法。
//
// 文件末尾那批是简写别名：INT 就是 INT32，LONG 就是 INT64，DATE 就是 DATETIME，
// 它们指向同一批实现。
func init() {
	one := scalars(1)
	two := scalars(2)
	three := scalars(3)
	zero := scalars(0)

	reg("MINVALUE", constFn(xbson.MinValue), Info{Params: zero})
	reg("MAXVALUE", constFn(xbson.MaxValue), Info{Params: zero})
	reg("OBJECTID", (*Ctx).mNEWOBJECTID, Info{Params: zero, Volatile: true})
	reg("GUID", (*Ctx).mNEWGUID, Info{Params: zero, Volatile: true})
	reg("NOW", (*Ctx).mNOW, Info{Params: zero, Volatile: true})
	reg("NOW_UTC", (*Ctx).mNOWUTC, Info{Params: zero, Volatile: true})
	reg("TODAY", (*Ctx).mTODAY, Info{Params: zero, Volatile: true})

	reg("INT32", (*Ctx).mINT32, Info{Params: one})
	reg("INT64", (*Ctx).mINT64, Info{Params: one})
	reg("DOUBLE", (*Ctx).mDOUBLE1, Info{Params: one})
	reg("DOUBLE", (*Ctx).mDOUBLE2, Info{Params: two})
	reg("DECIMAL", (*Ctx).mDECIMAL1, Info{Params: one})
	reg("DECIMAL", (*Ctx).mDECIMAL2, Info{Params: two})
	reg("STRING", (*Ctx).mSTRING, Info{Params: one})
	reg("ARRAY", (*Ctx).mARRAY, Info{Params: []ParamKind{ParamSeq}})
	reg("BINARY", (*Ctx).mBINARY, Info{Params: one})
	reg("OBJECTID", (*Ctx).mOBJECTID1, Info{Params: one})
	reg("GUID", (*Ctx).mGUID1, Info{Params: one})
	reg("BOOLEAN", (*Ctx).mBOOLEAN, Info{Params: one})
	reg("DATETIME", (*Ctx).mDATETIME1, Info{Params: one})
	reg("DATETIME", (*Ctx).mDATETIME2, Info{Params: two})
	reg("DATETIME_UTC", (*Ctx).mDATETIMEUTC1, Info{Params: one})
	reg("DATETIME_UTC", (*Ctx).mDATETIMEUTC2, Info{Params: two})
	reg("DATETIME", (*Ctx).mDATETIME3, Info{Params: three})
	reg("DATETIME_UTC", (*Ctx).mDATETIMEUTC3, Info{Params: three})

	for name, t := range map[string]xbson.Type{
		"IS_MINVALUE": xbson.TypeMinValue,
		"IS_NULL":     xbson.TypeNull,
		"IS_INT32":    xbson.TypeInt32,
		"IS_INT64":    xbson.TypeInt64,
		"IS_DOUBLE":   xbson.TypeDouble,
		"IS_DECIMAL":  xbson.TypeDecimal,
		"IS_STRING":   xbson.TypeString,
		"IS_DOCUMENT": xbson.TypeDocument,
		"IS_ARRAY":    xbson.TypeArray,
		"IS_BINARY":   xbson.TypeBinary,
		"IS_OBJECTID": xbson.TypeObjectID,
		"IS_GUID":     xbson.TypeGUID,
		"IS_BOOLEAN":  xbson.TypeBoolean,
		"IS_DATETIME": xbson.TypeDateTime,
		"IS_MAXVALUE": xbson.TypeMaxValue,
	} {
		reg(name, isType(t), Info{Params: one})
	}
	reg("IS_NUMBER", (*Ctx).mISNUMBER, Info{Params: one})

	reg("INT", (*Ctx).mINT32, Info{Params: one})
	reg("LONG", (*Ctx).mINT64, Info{Params: one})
	reg("BOOL", (*Ctx).mBOOLEAN, Info{Params: one})
	reg("DATE", (*Ctx).mDATETIME1, Info{Params: one})
	reg("DATE", (*Ctx).mDATETIME2, Info{Params: two})
	reg("DATE", (*Ctx).mDATETIME3, Info{Params: three})
	reg("DATE_UTC", (*Ctx).mDATETIMEUTC1, Info{Params: one})
	reg("DATE_UTC", (*Ctx).mDATETIMEUTC2, Info{Params: two})
	reg("DATE_UTC", (*Ctx).mDATETIMEUTC3, Info{Params: three})
	reg("IS_INT", isType(xbson.TypeInt32), Info{Params: one})
	reg("IS_LONG", isType(xbson.TypeInt64), Info{Params: one})
	reg("IS_BOOL", isType(xbson.TypeBoolean), Info{Params: one})
	reg("IS_DATE", isType(xbson.TypeDateTime), Info{Params: one})
}

// constFn 把一个固定值包成方法，供 MINVALUE、MAXVALUE 用。
func constFn(v *xbson.Value) Method {
	return func(*Ctx, []*xbson.Value) (*xbson.Value, error) { return v, nil }
}

// isType 造一个判类型的方法。判的是精确类型，不做任何隐式转换。
func isType(t xbson.Type) Method {
	return func(_ *Ctx, args []*xbson.Value) (*xbson.Value, error) {
		return xbson.Boolean(args[0].Type() == t), nil
	}
}

// mISNUMBER 判断是不是数，四种数值类型都算。
func (*Ctx) mISNUMBER(args []*xbson.Value) (*xbson.Value, error) {
	return xbson.Boolean(isNumber(args[0])), nil
}

// mNEWOBJECTID 生成一个新的 ObjectId。
func (*Ctx) mNEWOBJECTID([]*xbson.Value) (*xbson.Value, error) {
	return xbson.OID(xbson.NewObjectID()), nil
}

// mNEWGUID 生成一个随机 GUID。
func (*Ctx) mNEWGUID([]*xbson.Value) (*xbson.Value, error) {
	g, err := newGuidV4()
	if err != nil {
		return nil, err
	}
	return xbson.GUID(g), nil
}

// mNOW 取当前时刻，带本地时区。
func (*Ctx) mNOW([]*xbson.Value) (*xbson.Value, error) {
	return xbson.DateTime(timeNow())
}

// mNOWUTC 取当前时刻，换成 UTC。
func (*Ctx) mNOWUTC([]*xbson.Value) (*xbson.Value, error) {
	return xbson.DateTime(timeNow().UTC())
}

// mTODAY 取今天零点，带本地时区。
func (*Ctx) mTODAY([]*xbson.Value) (*xbson.Value, error) {
	n := timeNow()
	return xbson.DateTime(time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.Local))
}

// mINT32 转成 32 位整数。
//
// 数值越界时报错，字符串越界却只是返回 Null——字符串走的是「解析不出来」
// 这条路，与数值转换不是一回事。字符串只认纯整数写法，小数点和指数都不认。
func (*Ctx) mINT32(args []*xbson.Value) (*xbson.Value, error) {
	v := args[0]
	if isNumber(v) {
		n, err := int32Of(v)
		if err != nil {
			return nil, err
		}
		return xbson.Int32(n), nil
	}
	if s, ok := str(v); ok {
		if n, ok := parseIntegerText(s); ok && n.IsInt64() {
			x := n.Int64()
			if x >= -2147483648 && x <= 2147483647 {
				return xbson.Int32(int32(x)), nil
			}
		}
	}
	return xbson.Null, nil
}

// mINT64 转成 64 位整数，规则同 [Ctx.mINT32]。
func (*Ctx) mINT64(args []*xbson.Value) (*xbson.Value, error) {
	v := args[0]
	if isNumber(v) {
		n, err := int64Of(v)
		if err != nil {
			return nil, err
		}
		return xbson.Int64(n), nil
	}
	if s, ok := str(v); ok {
		if n, ok := parseIntegerText(s); ok && n.IsInt64() {
			return xbson.Int64(n.Int64()), nil
		}
	}
	return xbson.Null, nil
}

// mDOUBLE1 转成双精度。字符串按当前排序规则对应的语言环境解析，解析不出返回 Null。
func (ctx *Ctx) mDOUBLE1(args []*xbson.Value) (*xbson.Value, error) {
	v := args[0]
	if f, ok := float64Of(v); ok {
		return xbson.Double(f), nil
	}
	if s, ok := str(v); ok {
		if f, ok := parseFloatAny(s, ctx.collationNumbers()); ok {
			return xbson.Double(f), nil
		}
	}
	return xbson.Null, nil
}

// collationNumbers 取当前排序规则对应的数字格式。
func (ctx *Ctx) collationNumbers() *xfmt.NumberFormat {
	return xfmt.Culture(ctx.Collation.CultureName())
}

// mDOUBLE2 转成双精度，第二个实参指定语言环境。
//
// 第一个实参本来就是数时忽略语言环境。语言环境名写得不合法要报错，
// 而不是当成解析失败。
func (ctx *Ctx) mDOUBLE2(args []*xbson.Value) (*xbson.Value, error) {
	if isNumber(args[0]) {
		return ctx.mDOUBLE1(args[:1])
	}
	if isString(args[0]) && isString(args[1]) {
		nf, err := namedCulture(args[1])
		if err != nil {
			return nil, err
		}
		s, _ := args[0].AsString()
		if f, ok := parseFloatAny(s, nf); ok {
			return xbson.Double(f), nil
		}
	}
	return xbson.Null, nil
}

// mDECIMAL1 转成十进制。字符串按当前排序规则对应的语言环境解析。
func (ctx *Ctx) mDECIMAL1(args []*xbson.Value) (*xbson.Value, error) {
	v := args[0]
	if isNumber(v) {
		d, err := toDec(v)
		if err != nil {
			return nil, err
		}
		return d.toValue()
	}
	if s, ok := str(v); ok {
		if d, ok := decParseIn(s, ctx.collationNumbers()); ok {
			return d.toValue()
		}
	}
	return xbson.Null, nil
}

// mDECIMAL2 转成十进制，第二个实参指定语言环境。规则同 [Ctx.mDOUBLE2]。
func (ctx *Ctx) mDECIMAL2(args []*xbson.Value) (*xbson.Value, error) {
	if isNumber(args[0]) {
		return ctx.mDECIMAL1(args[:1])
	}
	if isString(args[0]) && isString(args[1]) {
		nf, err := namedCulture(args[1])
		if err != nil {
			return nil, err
		}
		s, _ := args[0].AsString()
		if d, ok := decParseIn(s, nf); ok {
			return d.toValue()
		}
	}
	return xbson.Null, nil
}

// namedCulture 按语言环境名取数字格式，名字不合法就报错。
func namedCulture(v *xbson.Value) (*xfmt.NumberFormat, error) {
	if err := checkCulture(v); err != nil {
		return nil, err
	}
	name, _ := v.AsString()
	return xfmt.Culture(name), nil
}

// checkCulture 校验语言环境名的写法。不是字符串时放行——那种情况由调用方另行处置。
func checkCulture(v *xbson.Value) error {
	name, ok := v.AsString()
	if !ok {
		return nil
	}
	if !wellFormedCultureName(name) {
		return errf("culture %q is not a well-formed culture name", name)
	}
	return nil
}

// maxCultureName 是语言环境名的长度上限。
const maxCultureName = 85

// maxCultureFirstSegment 是语言段的长度上限。
const maxCultureFirstSegment = 11

// wellFormedCultureName 判断语言环境名写得合不合法。
//
// 空串和 C 都算合法（分别指不变环境和 POSIX 那一个）。其余要满足：
// 不超长；不是 root；下划线最多一个；各段非空且只含字母数字；
// 不以私有用途的 x 段开头；语言段不超长；整体要么多于一段，要么那一段至少两个字符。
func wellFormedCultureName(name string) bool {
	if name == "" {
		return true
	}

	if name == "C" || name == "c" {
		return true
	}
	if len(name) > maxCultureName {
		return false
	}

	if strings.EqualFold(name, "root") {
		return false
	}

	if strings.Count(name, "_") > 1 {
		return false
	}
	segs := strings.Split(strings.ReplaceAll(name, "_", "-"), "-")
	for _, seg := range segs {
		if seg == "" {
			return false
		}
		for j := 0; j < len(seg); j++ {
			c := seg[j]
			if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
				continue
			}
			return false
		}
	}

	tagged := !strings.Contains(name, "_")

	if tagged && strings.EqualFold(segs[0], "x") {
		return false
	}

	lang := segs[0]
	if tagged && len(segs) > 1 && strings.EqualFold(segs[0], "i") {
		lang = segs[0] + "-" + segs[1]
	}
	if len(lang) > maxCultureFirstSegment {
		return false
	}

	return len(segs) > 1 || len(segs[0]) >= 2
}

// stringOf 把一个值转成文本。
//
// Null 转成空串，字符串取它自己，其余走 JSON 写法。
func stringOf(v *xbson.Value) string {
	switch v.Type() {
	case xbson.TypeNull:
		return ""
	case xbson.TypeString:
		s, _ := v.AsString()
		return s
	default:
		return jsonText(v)
	}
}

// mSTRING 转成字符串。
func (*Ctx) mSTRING(args []*xbson.Value) (*xbson.Value, error) {
	return xbson.String(stringOf(args[0])), nil
}

// mARRAY 把一串值收成一个数组。
func (*Ctx) mARRAY(args []*xbson.Value) (*xbson.Value, error) {
	return seqOf(items(args[0])), nil
}

// mBINARY 转成二进制：字符串按 Base64 解码。
//
// 解码前先去掉全部空白，所以带换行的 Base64 也认。解不出来返回 Null。
func (*Ctx) mBINARY(args []*xbson.Value) (*xbson.Value, error) {
	v := args[0]
	if v.Type() == xbson.TypeBinary {
		return v, nil
	}
	s, ok := str(v)
	if !ok {
		return xbson.Null, nil
	}
	clean := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
	b, err := base64.StdEncoding.DecodeString(clean)
	if err != nil {
		return xbson.Null, nil
	}
	return xbson.Binary(b), nil
}

// mOBJECTID1 转成 ObjectId：字符串按 24 位十六进制解析，前后空白和大写都容许。
func (*Ctx) mOBJECTID1(args []*xbson.Value) (*xbson.Value, error) {
	v := args[0]
	if v.Type() == xbson.TypeObjectID {
		return v, nil
	}
	s, ok := str(v)
	if !ok {
		return xbson.Null, nil
	}
	id, err := xbson.ParseObjectID(strings.ToLower(strings.TrimSpace(s)))
	if err != nil {
		return xbson.Null, nil
	}
	return xbson.OID(id), nil
}

// mGUID1 转成 GUID，字符串按宽松写法解析。解不出返回 Null。
func (*Ctx) mGUID1(args []*xbson.Value) (*xbson.Value, error) {
	v := args[0]
	if v.Type() == xbson.TypeGUID {
		return v, nil
	}
	s, ok := str(v)
	if !ok {
		return xbson.Null, nil
	}
	g, err := parseGuidLoose(s)
	if err != nil {
		return xbson.Null, nil
	}
	return xbson.GUID(g), nil
}

// mBOOLEAN 转成布尔。
//
// Null 是假。字符串只认 true 和 false 两个词（不分大小写，容许前后空白），
// 其余一律 Null——数字零不会转成假。
func (*Ctx) mBOOLEAN(args []*xbson.Value) (*xbson.Value, error) {
	v := args[0]
	if b, ok := v.AsBoolean(); ok {
		return xbson.Boolean(b), nil
	}
	if v.IsNull() {
		return xbson.False, nil
	}
	s, ok := str(v)
	if !ok {
		return xbson.Null, nil
	}
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true":
		return xbson.True, nil
	case "false":
		return xbson.False, nil
	default:
		return xbson.Null, nil
	}
}

// mDATETIME1 转成时间，字符串按当前排序规则对应的语言环境解析。
func (ctx *Ctx) mDATETIME1(args []*xbson.Value) (*xbson.Value, error) {
	return datetimeIn(args[0], ctx.collationDates())
}

// collationDates 取当前排序规则对应的日期时间格式。
func (ctx *Ctx) collationDates() *xfmt.DateTimeFormat {
	return xfmt.DateCulture(ctx.Collation.CultureName())
}

// datetimeIn 按给定格式把字符串解析成时间。
//
// 文本里没写时区的，结果标成「未指定时区」，而不是悄悄按本地时区落定。
// 解析失败一律返回 Null。
func datetimeIn(v *xbson.Value, df *xfmt.DateTimeFormat) (*xbson.Value, error) {
	if v.Type() == xbson.TypeDateTime {
		return v, nil
	}
	s, ok := str(v)
	if !ok {
		return xbson.Null, nil
	}
	t, hadZone, err := parseDateText(s, df)
	if err != nil {
		return xbson.Null, nil
	}
	dv, err := dateValueClamped(t)
	if err != nil {
		return xbson.Null, nil
	}
	if !hadZone {
		dv = dv.AsUnspecified()
	}
	return dv, nil
}

// mDATETIME2 转成时间，第二个实参指定语言环境。
func (ctx *Ctx) mDATETIME2(args []*xbson.Value) (*xbson.Value, error) {
	if args[0].Type() == xbson.TypeDateTime {
		return args[0], nil
	}
	if isString(args[0]) && isString(args[1]) {
		df, err := namedDateCulture(args[1])
		if err != nil {
			return nil, err
		}
		return datetimeIn(args[0], df)
	}
	return xbson.Null, nil
}

// namedDateCulture 按语言环境名取日期时间格式，名字不合法就报错。
func namedDateCulture(v *xbson.Value) (*xfmt.DateTimeFormat, error) {
	if err := checkCulture(v); err != nil {
		return nil, err
	}
	name, _ := v.AsString()
	return xfmt.DateCulture(name), nil
}

// mDATETIME3 由年月日三个数造一个时间，取当天零点。
//
// 日期不成立时报错，而不是返回 Null——三个数写错了是调用方的问题。
// 本地时区下超出可存范围（比如 1 年 1 月 1 日在东时区）时改用 UTC 再试一次。
// 结果标成未指定时区。
func (*Ctx) mDATETIME3(args []*xbson.Value) (*xbson.Value, error) {
	if !isNumber(args[0]) || !isNumber(args[1]) || !isNumber(args[2]) {
		return xbson.Null, nil
	}
	y, err := int32Of(args[0])
	if err != nil {
		return nil, err
	}
	m, err := int32Of(args[1])
	if err != nil {
		return nil, err
	}
	d, err := int32Of(args[2])
	if err != nil {
		return nil, err
	}
	if y < 1 || y > 9999 || m < 1 || m > 12 || d < 1 || int(d) > daysInMonth(int(y), time.Month(m)) {
		return nil, errf("DATETIME: %d-%d-%d is not a valid date", y, m, d)
	}

	v, err := xbson.DateTime(time.Date(int(y), time.Month(m), int(d), 0, 0, 0, 0, time.Local))
	if err != nil {
		v, err = xbson.DateTime(time.Date(int(y), time.Month(m), int(d), 0, 0, 0, 0, time.UTC))
		if err != nil {
			return nil, err
		}
	}
	return v.AsUnspecified(), nil
}

// mDATETIMEUTC1 转成时间，文本没写时区时按 UTC 算。
func (ctx *Ctx) mDATETIMEUTC1(args []*xbson.Value) (*xbson.Value, error) {
	return datetimeUTCIn(args[0], ctx.collationDates())
}

// datetimeUTCIn 同 [datetimeIn]，但没写时区的按 UTC 算，结果也不标成未指定。
func datetimeUTCIn(v *xbson.Value, df *xfmt.DateTimeFormat) (*xbson.Value, error) {
	if v.Type() == xbson.TypeDateTime {
		return v, nil
	}
	s, ok := str(v)
	if !ok {
		return xbson.Null, nil
	}
	t, err := parseDateTextUTC(s, df)
	if err != nil {
		return xbson.Null, nil
	}
	dv, err := dateValueClamped(t)
	if err != nil {
		return xbson.Null, nil
	}
	return dv, nil
}

// dateValueClamped 造一个时间值；本地时区下超出可存范围时保留墙上分量改用 UTC。
func dateValueClamped(t time.Time) (*xbson.Value, error) {
	v, err := xbson.DateTime(t)
	if err == nil {
		return v, nil
	}
	return xbson.DateTime(time.Date(t.Year(), t.Month(), t.Day(),
		t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC))
}

// mDATETIMEUTC2 转成时间并按 UTC 兜底，第二个实参指定语言环境。
func (ctx *Ctx) mDATETIMEUTC2(args []*xbson.Value) (*xbson.Value, error) {
	if args[0].Type() == xbson.TypeDateTime {
		return args[0], nil
	}
	if isString(args[0]) && isString(args[1]) {
		df, err := namedDateCulture(args[1])
		if err != nil {
			return nil, err
		}
		return datetimeUTCIn(args[0], df)
	}
	return xbson.Null, nil
}

// mDATETIMEUTC3 由年月日三个数造一个 UTC 零点时间。日期不成立时报错。
func (*Ctx) mDATETIMEUTC3(args []*xbson.Value) (*xbson.Value, error) {
	if !isNumber(args[0]) || !isNumber(args[1]) || !isNumber(args[2]) {
		return xbson.Null, nil
	}
	y, err := int32Of(args[0])
	if err != nil {
		return nil, err
	}
	m, err := int32Of(args[1])
	if err != nil {
		return nil, err
	}
	d, err := int32Of(args[2])
	if err != nil {
		return nil, err
	}
	if y < 1 || y > 9999 || m < 1 || m > 12 || d < 1 || int(d) > daysInMonth(int(y), time.Month(m)) {
		return nil, errf("DATETIME: %d-%d-%d is not a valid date", y, m, d)
	}
	return xbson.DateTime(time.Date(int(y), time.Month(m), int(d), 0, 0, 0, 0, time.UTC))
}
