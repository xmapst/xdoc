package xbson

// Type 是值的类型。
//
// 编号同时也是**排序时的类型次序**：不同类型的值相比时先比类型编号，
// 所以 MinValue 小于一切、MaxValue 大于一切。编号是格式的一部分，不能改。
type Type byte

const (
	// 类型编号。这套编号既是类型标记，也是跨类型比较时的次序。
	TypeMinValue Type = 0
	TypeNull     Type = 1
	TypeInt32    Type = 2
	TypeInt64    Type = 3
	TypeDouble   Type = 4
	TypeDecimal  Type = 5
	TypeString   Type = 6
	TypeDocument Type = 7
	TypeArray    Type = 8
	TypeBinary   Type = 9
	TypeObjectID Type = 10
	TypeGUID     Type = 11
	TypeBoolean  Type = 12
	TypeDateTime Type = 13
	TypeMaxValue Type = 14
	TypeVector   Type = 100
)

const (
	// 写进文件时的类型标记。
	//
	// 与 [Type] 是两套编号：这一套决定字节布局，那一套决定排序次序。
	// GUID 没有独立标记，它写成带 UUID 子类型的二进制。
	tagDouble   byte = 0x01
	tagString   byte = 0x02
	tagDocument byte = 0x03
	tagArray    byte = 0x04
	tagBinary   byte = 0x05
	tagObjectID byte = 0x07
	tagBoolean  byte = 0x08
	tagDateTime byte = 0x09
	tagNull     byte = 0x0A
	tagInt32    byte = 0x10
	tagInt64    byte = 0x12
	tagDecimal  byte = 0x13
	tagVector   byte = 0x64
	tagMaxValue byte = 0x7F
	tagMinValue byte = 0xFF

	// 二进制的子类型：一般二进制与 UUID。
	subtypeGeneric byte = 0x00
	subtypeUUID    byte = 0x04
)

// String 返回类型名。这些名字会出现在错误消息与查询结果里。
func (t Type) String() string {
	switch t {
	case TypeMinValue:
		return "MinValue"
	case TypeNull:
		return "Null"
	case TypeInt32:
		return "Int32"
	case TypeInt64:
		return "Int64"
	case TypeDouble:
		return "Double"
	case TypeDecimal:
		return "Decimal"
	case TypeString:
		return "String"
	case TypeDocument:
		return "Document"
	case TypeArray:
		return "Array"
	case TypeBinary:
		return "Binary"
	case TypeObjectID:
		return "ObjectId"
	case TypeGUID:
		return "Guid"
	case TypeBoolean:
		return "Boolean"
	case TypeDateTime:
		return "DateTime"
	case TypeMaxValue:
		return "MaxValue"
	case TypeVector:
		return "Vector"
	default:
		return "Unknown"
	}
}

// IsNumber 报告这是不是数字类型。
//
// 数字之间可以跨类型比较与运算，别的类型不行。
func (t Type) IsNumber() bool {
	return t == TypeInt32 || t == TypeInt64 || t == TypeDouble || t == TypeDecimal
}
