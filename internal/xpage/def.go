// Package xpage 是文件的页布局。
//
// 文件由等长的页组成，页号就是它在文件里的位置。每页开头是页头，页尾是槽表；
// 数据从页头之后往后长，槽表从页尾往前长，中间是空闲区。
//
// 页有几种用途：第 0 页是头页（格式标识、全库配置、集合表），每个集合有一个
// 集合页（数据页空闲链、索引表），其余是数据页、索引页与向量索引页。
//
// 这里的每一处布局都是文件格式的一部分：偏移、字节序、字段宽度、类型编号，
// 改动其中任何一个都会让已有的文件读不出来。
package xpage

import "errors"

const (
	// PageSize 是一页的字节数。文件长度总是它的整数倍。
	PageSize = 8192

	// HeaderSize 是每页开头的页头字节数。
	HeaderSize = 32

	// SlotSize 是页尾每个槽占的字节数：2 字节偏移 + 2 字节长度。
	//
	// 槽从页尾往前长，数据从页头往后长，中间是空闲区。
	SlotSize = 4

	// MaxSlotIndex 是最大槽号。255 留给 [EmptyIndex] 当哨兵。
	MaxSlotIndex = 254

	// MaxItems 是一页最多放多少项。
	MaxItems = MaxSlotIndex + 1
)

// EmptyIndex 表示「没有这个槽」。
const EmptyIndex uint8 = 0xFF

// EmptyPageID 表示「没有这一页」，页链的末端用它。
const EmptyPageID uint32 = 0xFFFFFFFF

const (
	// 页头里各字段的偏移。布局是格式的一部分，不能改。
	offPageID           = 0
	offPageType         = 4
	offPrevPageID       = 5
	offNextPageID       = 9
	offPageListSlot     = 13
	offTransactionID    = 14
	offIsConfirmed      = 18
	offColID            = 19
	offItemsCount       = 23
	offUsedBytes        = 24
	offFragmentedBytes  = 26
	offNextFreePosition = 28
	offHighestIndex     = 30
)

// PageType 是一页的用途，写在页头里。
type PageType uint8

const (
	// 页的用途。空页是回收之后待重用的页。
	PageEmpty       PageType = 0
	PageHeader      PageType = 1
	PageCollection  PageType = 2
	PageIndex       PageType = 3
	PageData        PageType = 4
	PageVectorIndex PageType = 5
)

// String 返回页类型的名字，会出现在 $dump 与错误消息里。
func (t PageType) String() string {
	switch t {
	case PageEmpty:
		return "Empty"
	case PageHeader:
		return "Header"
	case PageCollection:
		return "Collection"
	case PageIndex:
		return "Index"
	case PageData:
		return "Data"
	case PageVectorIndex:
		return "VectorIndex"
	default:
		return "Unknown"
	}
}

var (
	// ErrCorrupt 表示这一页的内容不自洽——数据坏了，重试没有意义。
	//
	// 后两个是容量问题，属于正常情况：调用方据此换一页写。
	ErrCorrupt = errors.New("xpage: corrupt page")

	ErrNoSpace = errors.New("xpage: not enough free space in page")

	ErrTooManyItems = errors.New("xpage: page is full")
)

// Address 定位到某一页里的某一项。
//
// 索引节点之间、数据块之间都用它串起来。
type Address struct {
	PageID uint32
	Index  uint8
}

// AddressSize 是一个地址写进文件占几字节。
const AddressSize = 5

// EmptyAddress 表示「不指向任何地方」。
var EmptyAddress = Address{PageID: EmptyPageID, Index: EmptyIndex}

// IsEmpty 报告这是不是空地址。
func (a Address) IsEmpty() bool { return a == EmptyAddress }
