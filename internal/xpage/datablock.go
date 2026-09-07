package xpage

import "fmt"

const (
	// DataBlockHeaderSize 是每个数据块开头的 6 字节：1 字节续接标记 + 5 字节下一块地址。
	DataBlockHeaderSize = 6

	// 数据块头里两个字段的偏移。
	offBlockExtend = 0
	offBlockNext   = 1
)

// MaxDataBytesPerPage 是一页最多能放多少篇文档正文字节。
const MaxDataBytesPerPage = PageSize - HeaderSize - SlotSize - DataBlockHeaderSize

// MaxDocumentSize 是一篇文档最多能有多大。
//
// 一篇文档拆成一条数据块链，块数上限定下了总大小。
const MaxDocumentSize = 2047 * MaxDataBytesPerPage

// DataBlock 是一篇文档的一段，指向页里的一块字节。
//
// 它不拷贝内容：改 [DataBlock.Data] 返回的切片就是改页本身。
type DataBlock struct {
	seg []byte
}

// AsDataBlock 把页里的一段字节当成数据块，太短则判成损坏。
func AsDataBlock(seg []byte) (DataBlock, error) {
	if len(seg) < DataBlockHeaderSize {
		return DataBlock{}, fmt.Errorf("%w: data block is %d bytes, need at least %d",
			ErrCorrupt, len(seg), DataBlockHeaderSize)
	}
	return DataBlock{seg: seg}, nil
}

// Extend 报告这一块是不是某篇文档的**续接块**（不是第一块）。
//
// 扫数据页抢救文档时靠它区分：只有非续接块才是一篇文档的开头。
func (d DataBlock) Extend() bool { return d.seg[offBlockExtend] != 0 }

// SetExtend 设置续接标记。
func (d DataBlock) SetExtend(v bool) {
	d.seg[offBlockExtend] = 0
	if v {
		d.seg[offBlockExtend] = 1
	}
}

// NextBlock 返回这篇文档下一块的位置，最后一块是空地址。
func (d DataBlock) NextBlock() Address { return ReadAddress(d.seg[offBlockNext:]) }

// SetNextBlock 设置下一块的位置。
func (d DataBlock) SetNextBlock(a Address) { a.WriteAddress(d.seg[offBlockNext:]) }

// Data 返回这一块的正文部分，是页里的切片，改它就是改页。
func (d DataBlock) Data() []byte { return d.seg[DataBlockHeaderSize:] }

// InsertDataBlock 在页里划出一个数据块，返回它与它的槽号。
func (p *Page) InsertDataBlock(payload int, extend bool) (DataBlock, uint8, error) {
	if payload < 0 || payload > MaxDataBytesPerPage {
		return DataBlock{}, 0, fmt.Errorf("xpage: data block payload %d out of range [0,%d]",
			payload, MaxDataBytesPerPage)
	}
	seg, i, err := p.Insert(payload + DataBlockHeaderSize)
	if err != nil {
		return DataBlock{}, 0, err
	}
	b := DataBlock{seg: seg}
	b.SetExtend(extend)
	b.SetNextBlock(EmptyAddress)
	return b, i, nil
}

// GetDataBlock 取出某个槽里的数据块。
func (p *Page) GetDataBlock(i uint8) (DataBlock, error) {
	seg, err := p.Get(i)
	if err != nil {
		return DataBlock{}, err
	}
	return AsDataBlock(seg)
}
