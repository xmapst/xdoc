package xdoc

import (
	"time"

	"github.com/xmapst/xdoc/internal/xbson"
)

// 值的类型，[Value.Type] 返回的就是它们。
//
// 这些数值同时是**跨类型比较时的排序序**：一条索引里混着不同类型的键时，先后就按
// 这些数字定（数字之间例外，它们互相按数值比，不按类型比）。也就是说它们是磁盘
// 格式的一部分，不会再改——已经写出去的文件里的键顺序依赖于它们。
//
//	switch v.Type() {
//	case xdoc.TypeString:
//	case xdoc.TypeInt32, xdoc.TypeInt64:
//	}
const (
	// TypeMinValue 排在一切之前，见 [MinValue]。
	TypeMinValue = xbson.TypeMinValue

	// TypeNull 是空值。字段不存在时读出来的也是它。
	TypeNull = xbson.TypeNull

	// TypeInt32 是 32 位整数。
	TypeInt32 = xbson.TypeInt32

	// TypeInt64 是 64 位整数。
	TypeInt64 = xbson.TypeInt64

	// TypeDouble 是双精度浮点数。
	TypeDouble = xbson.TypeDouble

	// TypeDecimal 是十进制定点数，见 [DecimalValue]。
	TypeDecimal = xbson.TypeDecimal

	// TypeString 是字符串。
	TypeString = xbson.TypeString

	// TypeDocument 是嵌套的一篇文档。
	TypeDocument = xbson.TypeDocument

	// TypeArray 是值数组。
	TypeArray = xbson.TypeArray

	// TypeBinary 是一段字节。
	TypeBinary = xbson.TypeBinary

	// TypeObjectID 是 [ObjectID]。
	TypeObjectID = xbson.TypeObjectID

	// TypeGUID 是 16 字节的 GUID，见 [GUIDValue]。
	TypeGUID = xbson.TypeGUID

	// TypeBoolean 是布尔值。
	TypeBoolean = xbson.TypeBoolean

	// TypeDateTime 是时间，见 [DateTime]。
	TypeDateTime = xbson.TypeDateTime

	// TypeMaxValue 排在一切之后，见 [MaxValue]。
	TypeMaxValue = xbson.TypeMaxValue

	// TypeVector 是单精度浮点向量，见 [Vector]。
	//
	// 编号取 100 而不是接着上面那串连号往下排：那一串同时是排序序，往里插一个就
	// 挪动了它后面每一个类型，也就是改掉既有文件里的键顺序。代价是向量落在
	// [TypeMaxValue] 之后，而且越出了索引键类型字节能表示的范围，见 [Vector]。
	TypeVector = xbson.TypeVector
)

// MinValue 排在所有值之前，不论对方是什么类型。
//
// 它是跳表两端哨兵节点的键，不能做主键或索引键：拿它去查会正好命中哨兵。
func MinValue() *Value { return xbson.MinValue }

// MaxValue 排在所有值之后，除了向量（见 [TypeVector]）。同样不能做主键或索引键。
func MaxValue() *Value { return xbson.MaxValue }

// DateTime 用一个时刻构造时间值，截到毫秒，时区跟着 t 记下来。
//
// 截断发生在构造时而不是写盘时，所以"存进去再读出来"少掉的那点精度当场可见，
// 而不是等到下次打开才发现。超出可表示范围（见 [TimeMin] 与 [TimeMax]）报错。
func DateTime(t time.Time) (*Value, error) { return xbson.DateTime(t) }

// Vector 构造一个单精度浮点向量值，用于向量索引。
//
// # 别给向量字段建普通索引
//
// 索引键的类型字节只有低 6 位是类型，而向量的编号是 100，越出了这 6 位。
// 建索引不报错，写第一篇也不报错，写下的键读回来却被掩成一个不存在的类型——
// 从此每一次读到这个键的查找都报文档损坏，而报错处离那次写入已经隔了很远。
// [QueryBuilder.OrderBy] 的排序键走同一条路。
//
// 向量检索用 [Collection.EnsureVectorIndex]，那是另一套索引，不走索引键这条路。
func Vector(v []float32) *Value { return xbson.Vector(v) }

// DbRef 拼一层引用壳：{"$id": 主键, "$ref": 集合名}。
//
// 它只写这层壳，不展开被引对象；要在查询时展开用 [QueryBuilder.Include]。
func DbRef(id *Value, collection string) *Document {
	return xbson.DocumentOf("$id", id, "$ref", xbson.String(collection))
}

// DecimalValue 把一个 [Decimal] 包成值。
func DecimalValue(d Decimal) *Value { return xbson.Decimal(d) }

// GUIDValue 把一个 [Guid] 包成值。
//
// 它在盘上是 16 字节，字节序是「前三段小端、后八字节原样」的那一种。
// 要让库自动生成这种主键，用 [AutoIDGUID]。
func GUIDValue(g Guid) *Value { return xbson.GUID(g) }

// ArrValue 把一个数组包成值。
func ArrValue(a *Array) *Value { return a.Value() }

// NewArray 用给定元素建一个数组。不传元素得到空数组。
func NewArray(items ...*Value) *Array { return xbson.NewArray(items...) }

// ObjectIDNil 是全零的 [ObjectID]，可以拿来判一个主键有没有被赋过值。
var ObjectIDNil = xbson.ObjectIDNil

// ParseObjectID 解析 24 个十六进制字符的写法。
func ParseObjectID(s string) (ObjectID, error) { return xbson.ParseObjectID(s) }

// ObjectIDFromBytes 从 12 字节的落盘形态还原。
func ObjectIDFromBytes(b []byte) (ObjectID, error) { return xbson.ObjectIDFromBytes(b) }

// DecodeDocument 把一段文档编码解回文档，其中的时间落在本地时区。
//
// 时区只影响读出来的墙上读数，不影响它是哪个绝对时刻——要指定用
// [DecodeDocumentIn]。
func DecodeDocument(b []byte) (*Document, error) { return xbson.DecodeIn(b, time.Local) }

// DecodeDocumentIn 与 [DecodeDocument] 一样，但时间落在 loc 指定的时区。
func DecodeDocumentIn(b []byte, loc *time.Location) (*Document, error) {
	return xbson.DecodeIn(b, loc)
}

// ErrIndexKeyTooLong 表示某个值编码成索引键后超过了 1023 字节（含类型字节与长度字节）。
//
// 这条上限落在**被索引字段的值**上，不是索引名或取键表达式上：给一个可能很长的
// 字符串字段建索引（正文、URL、日志行），值超出上限的那篇文档写不进去，整次写入
// 失败。要按长文本查，索引它的一段前缀或一个哈希。
//
// 用 [errors.Is] 判：它会被包在「哪条索引」的上下文里返回。
var ErrIndexKeyTooLong = xbson.ErrIndexKeyTooLong
