// Package xbson 是文档模型：值、文档、数组，以及它们的编解码与比较。
//
// 三套字节形态，用途不同：
//
//   - 文档编码（[Document.Encode]）——存进数据页的形态；
//   - 索引键（[Value.AppendIndexKey]）——存进索引页的形态，更紧凑；
//   - 可读形式（[Value.String]）——只供人看，不保证稳定。
//
// 比较次序（[Value.Compare]）是格式的一部分：索引就是按它排的，改动它
// 等于让已有的索引全部失效。
package xbson

import (
	"fmt"
	"time"

	"github.com/xmapst/xdoc/internal/xbin"
)

// Value 是一个文档值。
//
// 各类型共用一份存储：整数、布尔、日期的计时单位放 num，浮点放 flt，
// 字符串放 str，其余放 ref。这样一个值就是一个结构体，不必按类型各建一个。
//
// **值是只读的**：造出来之后不再改，所以可以随意共享，
// [Null]、[True] 这些常量值也因此能被反复引用。
type Value struct {
	// t 是这个值的类型，决定下面哪个字段有意义。
	t Type

	// unspec 只对日期有意义：表示这个时刻不带时区含义。
	//
	// 见 [Value.Unspecified]。
	unspec bool

	// num 存整数、布尔与日期的计时单位；flt 存浮点；str 存字符串。
	num int64
	flt float64
	str string

	// ref 存其余类型的载荷：十进制数、二进制、标识、文档、数组、向量，
	// 以及日期的时区。
	ref any
}

var (
	// 几个不带载荷的常量值，可以放心共享。
	MinValue = &Value{t: TypeMinValue}
	Null     = &Value{t: TypeNull}
	MaxValue = &Value{t: TypeMaxValue}
	True     = &Value{t: TypeBoolean, num: 1}
	False    = &Value{t: TypeBoolean, num: 0}
)

// Type 返回这个值的类型。
func (v *Value) Type() Type { return v.t }

// IsNull 报告这是不是空值。
func (v *Value) IsNull() bool { return v.t == TypeNull }

// Int32 造一个 32 位整数值。
func Int32(n int32) *Value { return &Value{t: TypeInt32, num: int64(n)} }

// Int64 造一个 64 位整数值。
func Int64(n int64) *Value { return &Value{t: TypeInt64, num: n} }

// Double 造一个双精度浮点值。
func Double(f float64) *Value { return &Value{t: TypeDouble, flt: f} }

// Decimal 造一个十进制数值。
func Decimal(d xbin.Decimal) *Value { return &Value{t: TypeDecimal, ref: d} }

// String 造一个字符串值。
func String(s string) *Value { return &Value{t: TypeString, str: s} }

// Boolean 返回共享的 [True] 或 [False]。
func Boolean(b bool) *Value {
	if b {
		return True
	}
	return False
}

// Binary 造一个二进制值。
//
// **切片不拷贝**：造完之后别再改它，那会连带改掉这个值。
func Binary(b []byte) *Value { return &Value{t: TypeBinary, ref: b} }

// OID 造一个 [ObjectID] 值。
func OID(id ObjectID) *Value { return &Value{t: TypeObjectID, ref: id} }

// GUID 造一个标识值。
func GUID(g xbin.Guid) *Value { return &Value{t: TypeGUID, ref: g} }

// DateTime 造一个日期值，时区一并记住。
//
// **截到毫秒**：文件里只存到毫秒，不先截的话，存进去再读出来会不相等。
//
// 超出可表示范围时报错，而不是钉在边界上——那会静默地把一个错误的
// 时间存成一个合法的时间。
func DateTime(t time.Time) (*Value, error) {
	loc := t.Location()
	ticks, err := xbin.TimeToTicks(xbin.TruncateToMillis(t))
	if err != nil {
		return nil, err
	}
	return dateTimeTicks(ticks, loc), nil
}

// dateTimeTicks 用计时单位与时区直接造一个日期值。
func dateTimeTicks(ticks int64, loc *time.Location) *Value {
	return &Value{t: TypeDateTime, num: ticks, ref: loc}
}

// Value 把文档包成值，nil 当空文档。
func (d *Document) Value() *Value {
	if d == nil {
		d = NewDocument()
	}
	return &Value{t: TypeDocument, ref: d}
}

// Value 把数组包成值，nil 当空数组。
func (a *Array) Value() *Value {
	if a == nil {
		a = NewArray()
	}
	return &Value{t: TypeArray, ref: a}
}

// Vector 造一个向量值，切片不拷贝。
func Vector(v []float32) *Value { return &Value{t: TypeVector, ref: v} }

// AsInt32 取出 32 位整数。
//
// **类型不对就给 false，不做转换**：一个 int64 值取不出 int32。
// 要跨类型取数字，用上层的转换函数。
func (v *Value) AsInt32() (int32, bool) {
	if v.t != TypeInt32 {
		return 0, false
	}
	return int32(v.num), true
}

// AsInt64 取出 64 位整数，类型不对给 false。
func (v *Value) AsInt64() (int64, bool) {
	if v.t != TypeInt64 {
		return 0, false
	}
	return v.num, true
}

// AsDouble 取出双精度浮点，类型不对给 false。
func (v *Value) AsDouble() (float64, bool) {
	if v.t != TypeDouble {
		return 0, false
	}
	return v.flt, true
}

// asRef 从 ref 里取出指定类型的载荷。
//
// 类型标记与载荷的实际类型两边都要对上：只看其中一个，
// 一个构造错误的值就会在这里悄悄通过。
func (v *Value) asRef[T any](want Type) (T, bool) {
	x, ok := v.ref.(T)
	return x, ok && v.t == want
}

// AsDecimal 取出十进制数。
func (v *Value) AsDecimal() (xbin.Decimal, bool) { return v.asRef[xbin.Decimal](TypeDecimal) }

// AsString 取出字符串。
func (v *Value) AsString() (string, bool) {
	if v.t != TypeString {
		return "", false
	}
	return v.str, true
}

// AsBoolean 取出布尔值。
func (v *Value) AsBoolean() (bool, bool) {
	if v.t != TypeBoolean {
		return false, false
	}
	return v.num != 0, true
}

// AsBinary 取出二进制内容，返回的是内部切片，别改它。
func (v *Value) AsBinary() ([]byte, bool) { return v.asRef[[]byte](TypeBinary) }

// AsObjectID 取出 [ObjectID]。
func (v *Value) AsObjectID() (ObjectID, bool) { return v.asRef[ObjectID](TypeObjectID) }

// AsGUID 取出标识。
func (v *Value) AsGUID() (xbin.Guid, bool) { return v.asRef[xbin.Guid](TypeGUID) }

// AsTime 取出时刻，落在这个值记着的时区里。
func (v *Value) AsTime() (time.Time, bool) {
	if v.t != TypeDateTime {
		return time.Time{}, false
	}
	t, err := xbin.TicksToTime(v.num)
	if err != nil {
		return time.Time{}, false
	}
	return t.In(v.Location()), true
}

// Location 返回这个日期值记着的时区，没有则为 UTC。
func (v *Value) Location() *time.Location {
	loc, _ := v.ref.(*time.Location)
	if loc == nil || v.t != TypeDateTime {
		return time.UTC
	}
	return loc
}

// Unspecified 报告这个日期是不是「不带时区含义」。
//
// 这一位不写进文件，只在一次运算过程中区分「明确是 UTC」与
// 「没说是哪个时区」——转换本地时间时两者的处理不同。
func (v *Value) Unspecified() bool { return v.t == TypeDateTime && v.unspec }

// AsUnspecified 返回一份标记为不带时区含义的副本。
func (v *Value) AsUnspecified() *Value {
	if v.t != TypeDateTime {
		return v
	}
	out := dateTimeTicks(v.num, v.Location())
	out.unspec = true
	return out
}

// In 返回换到另一个时区的副本。
//
// 计时单位不变——换的是解读方式，不是时刻本身。
func (v *Value) In(loc *time.Location) *Value {
	if v.t != TypeDateTime {
		return v
	}
	return dateTimeTicks(v.num, loc)
}

// Ticks 取出日期的计时单位。
func (v *Value) Ticks() (int64, bool) {
	if v.t != TypeDateTime {
		return 0, false
	}
	return v.num, true
}

// AsDocument 取出文档。
func (v *Value) AsDocument() (*Document, bool) { return v.asRef[*Document](TypeDocument) }

// AsArray 取出数组。
func (v *Value) AsArray() (*Array, bool) { return v.asRef[*Array](TypeArray) }

// AsVector 取出向量，返回的是内部切片，别改它。
func (v *Value) AsVector() ([]float32, bool) { return v.asRef[[]float32](TypeVector) }

// String 给出一个可读形式，供错误消息与调试用。
//
// 不是可解析的格式，也不保证稳定——要产出可交换文本走 JSON 那条路。
func (v *Value) String() string {
	switch v.t {
	case TypeMinValue:
		return "MinValue"
	case TypeMaxValue:
		return "MaxValue"
	case TypeNull:
		return "null"
	case TypeInt32, TypeInt64:
		return fmt.Sprint(v.num)
	case TypeDouble:
		return fmt.Sprint(v.flt)
	case TypeBoolean:
		return fmt.Sprint(v.num != 0)
	case TypeString:
		return v.str
	case TypeDateTime:
		t, _ := v.AsTime()
		return t.Format(time.RFC3339Nano)
	default:
		return fmt.Sprintf("%v", v.ref)
	}
}
