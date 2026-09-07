package xpage

import (
	"fmt"

	"github.com/xmapst/xdoc/internal/xbson"
)

const (
	// IndexNodeHeaderSize 是索引节点头的字节数：槽号 + 层数 + 数据块地址 + 下一节点地址。
	IndexNodeHeaderSize = 12

	// levelSize 是每一层占的字节数：前驱地址加后继地址。
	levelSize = 2 * AddressSize

	// 索引节点各字段的偏移，各层的地址从 offNodeLevels0 起依次排列。
	offNodeSlot      = 0
	offNodeLevels    = 1
	offNodeDataBlock = 2
	offNodeNextNode  = 7
	offNodeLevels0   = 12
)

// MaxSkipLevel 是跳表节点最多有几层。
const MaxSkipLevel = 32

// IndexNode 是跳表里的一个节点，指向页里的一块字节。
//
// 布局是「头 + 各层的前后指针 + 索引键」。层数不定，所以键的位置
// 要按层数算出来。
//
// 它不拷贝内容：改它就是改页。
type IndexNode struct {
	seg []byte
}

// AsIndexNode 把页里的一段字节当成索引节点。
//
// 层数要先检查再用来算长度：一个坏掉的层数会让后面的下标越界。
func AsIndexNode(seg []byte) (IndexNode, error) {
	if len(seg) < IndexNodeHeaderSize {
		return IndexNode{}, fmt.Errorf("%w: index node is %d bytes, need at least %d",
			ErrCorrupt, len(seg), IndexNodeHeaderSize)
	}
	n := IndexNode{seg: seg}
	levels := int(seg[offNodeLevels])
	if levels < 1 || levels > MaxSkipLevel {
		return IndexNode{}, fmt.Errorf("%w: index node claims %d levels, want 1..%d",
			ErrCorrupt, levels, MaxSkipLevel)
	}
	if len(seg) < IndexNodeHeaderSize+levels*levelSize {
		return IndexNode{}, fmt.Errorf("%w: index node with %d levels needs %d bytes, has %d",
			ErrCorrupt, levels, IndexNodeHeaderSize+levels*levelSize, len(seg))
	}
	return n, nil
}

// IndexNodeSize 算出一个节点要占多少字节。
func IndexNodeSize(levels int, keyLen int) int {
	return IndexNodeHeaderSize + levels*levelSize + keyLen
}

// Slot 是这个节点属于哪一条索引（集合页里的索引槽号）。
func (n IndexNode) Slot() uint8 { return n.seg[offNodeSlot] }

// Levels 是这个节点有几层。
func (n IndexNode) Levels() int { return int(n.seg[offNodeLevels]) }

// DataBlock 指向这个键对应文档的第一块数据。
func (n IndexNode) DataBlock() Address { return ReadAddress(n.seg[offNodeDataBlock:]) }

// SetDataBlock 设置数据块地址。
func (n IndexNode) SetDataBlock(a Address) { a.WriteAddress(n.seg[offNodeDataBlock:]) }

// NextNode 指向同一篇文档在**别的索引**里的节点。
//
// 一篇文档在每条索引上各有一个节点，它们串成一个环：删文档时
// 顺着走一遍就能把所有节点都摘掉，不必逐条索引去查。
func (n IndexNode) NextNode() Address { return ReadAddress(n.seg[offNodeNextNode:]) }

// SetNextNode 设置同一篇文档在下一条索引里的节点。
func (n IndexNode) SetNextNode(a Address) { a.WriteAddress(n.seg[offNodeNextNode:]) }

// Prev 返回第 level 层的前驱节点。
func (n IndexNode) Prev(level int) Address {
	return ReadAddress(n.seg[offNodeLevels0+level*levelSize:])
}

// SetPrev 设置第 level 层的前驱节点。
func (n IndexNode) SetPrev(level int, a Address) {
	a.WriteAddress(n.seg[offNodeLevels0+level*levelSize:])
}

// Next 返回第 level 层的后继节点。
func (n IndexNode) Next(level int) Address {
	return ReadAddress(n.seg[offNodeLevels0+level*levelSize+AddressSize:])
}

// SetNext 设置第 level 层的后继节点。
func (n IndexNode) SetNext(level int, a Address) {
	a.WriteAddress(n.seg[offNodeLevels0+level*levelSize+AddressSize:])
}

// KeyBytes 返回索引键的原始字节，位置在各层指针之后。
func (n IndexNode) KeyBytes() []byte { return n.seg[IndexNodeHeaderSize+n.Levels()*levelSize:] }

// Key 解出索引键。
func (n IndexNode) Key() (*xbson.Value, error) {
	v, _, err := xbson.ReadIndexKey(n.KeyBytes())
	return v, err
}

// InsertIndexNode 在页里划出一个索引节点并写入键。
//
// 各层指针初始化成空地址：新划出的段里可能残留旧字节，
// 不清的话会指向不存在的节点。
//
// 预估长度与实际写出的对不上时报错——那是编码器自身的缺陷。
func (p *Page) InsertIndexNode(slot uint8, levels int, key *xbson.Value) (IndexNode, uint8, error) {
	if levels < 1 || levels > MaxSkipLevel {
		return IndexNode{}, 0, fmt.Errorf("xpage: index node levels %d out of range 1..%d",
			levels, MaxSkipLevel)
	}
	keyLen, err := key.IndexKeySize()
	if err != nil {
		return IndexNode{}, 0, err
	}
	size := IndexNodeSize(levels, keyLen)
	if size > MaxIndexNodeSize {
		return IndexNode{}, 0, fmt.Errorf("xpage: index node needs %d bytes, limit is %d",
			size, MaxIndexNodeSize)
	}
	seg, i, err := p.Insert(size)
	if err != nil {
		return IndexNode{}, 0, err
	}
	seg[offNodeSlot] = slot
	seg[offNodeLevels] = uint8(levels)
	n := IndexNode{seg: seg}
	n.SetDataBlock(EmptyAddress)
	n.SetNextNode(EmptyAddress)
	for l := range levels {
		n.SetPrev(l, EmptyAddress)
		n.SetNext(l, EmptyAddress)
	}
	kb := seg[IndexNodeHeaderSize+levels*levelSize:]
	written, err := key.AppendIndexKey(kb[:0])
	if err != nil {
		return IndexNode{}, 0, err
	}
	if len(written) != keyLen {
		return IndexNode{}, 0, fmt.Errorf("xpage: index key predicted %d bytes, wrote %d",
			keyLen, len(written))
	}
	return n, i, nil
}

// GetIndexNode 取出某个槽里的索引节点。
func (p *Page) GetIndexNode(i uint8) (IndexNode, error) {
	seg, err := p.Get(i)
	if err != nil {
		return IndexNode{}, err
	}
	return AsIndexNode(seg)
}
