package xpage

import (
	"encoding/binary"
	"fmt"
	"math"
	"slices"
)

const (
	// VectorMaxLevels 是向量图有几层。
	VectorMaxLevels = 4

	// VectorMaxNeighborsPerLevel 是每层最多记几个邻居。
	//
	// 层数与邻居数都写死：节点因此是**定长**的，页里放得下几个一目了然，
	// 也不必在邻居增减时重新分配。代价是召回率有上限。
	VectorMaxNeighborsPerLevel = 8

	// vectorLevelStride 是每层占的字节数：1 字节计数加固定条数的地址。
	vectorLevelStride = 1 + VectorMaxNeighborsPerLevel*AddressSize

	// 向量节点各字段的偏移，各层的邻居表从 offVecLevels 起依次排列。
	offVecDataBlock  = 0
	offVecLevelCount = offVecDataBlock + AddressSize
	offVecLevels     = offVecLevelCount + 1
	offVecLength     = offVecLevels + VectorMaxLevels*vectorLevelStride
	offVecPayload    = offVecLength + 2

	// VectorNodeHeaderSize 是向量本体之前的固定部分。
	VectorNodeHeaderSize = offVecPayload

	// maxVectorNodeSize 是一个向量节点最多能占多少字节（独占一页时）。
	maxVectorNodeSize = PageSize - HeaderSize - SlotSize
)

// MaxInlineVectorDimensions 是向量能直接放进节点里的最大维数。
//
// 再长就放不进一页，只能另存一处、节点里只记地址。
const MaxInlineVectorDimensions = (maxVectorNodeSize - VectorNodeHeaderSize) / 4

// VectorNode 是向量图里的一个节点，指向页里的一块字节。
//
// 它不拷贝内容：改它就是改页。
type VectorNode struct {
	seg []byte
}

// VectorNodeSize 算出这么多维的节点要占多少字节，以及向量放不放得进去。
//
// 放不进去时节点只留一个地址的位置，向量另存。
func VectorNodeSize(dims int) (size int, inline bool) {
	if n := VectorNodeHeaderSize + dims*4; n <= maxVectorNodeSize {
		return n, true
	}
	return VectorNodeHeaderSize + AddressSize, false
}

// AsVectorNode 把页里的一段字节当成向量节点。
//
// 层数与维数都要先检查再用来算长度：坏掉的值会让后面的下标越界。
func AsVectorNode(seg []byte) (VectorNode, error) {
	if len(seg) < VectorNodeHeaderSize {
		return VectorNode{}, fmt.Errorf("%w: vector node is %d bytes, need at least %d",
			ErrCorrupt, len(seg), VectorNodeHeaderSize)
	}
	n := VectorNode{seg: seg}
	levels := int(seg[offVecLevelCount])
	if levels < 1 || levels > VectorMaxLevels {
		return VectorNode{}, fmt.Errorf("%w: vector node claims %d levels, want 1..%d",
			ErrCorrupt, levels, VectorMaxLevels)
	}
	need := VectorNodeHeaderSize
	if dims := n.Dimensions(); dims > 0 {
		need += dims * 4
	} else {
		need += AddressSize
	}
	if len(seg) < need {
		return VectorNode{}, fmt.Errorf("%w: vector node needs %d bytes, has %d",
			ErrCorrupt, need, len(seg))
	}
	return n, nil
}

// DataBlock 指向这个向量对应文档的第一块数据。
func (n VectorNode) DataBlock() Address { return ReadAddress(n.seg[offVecDataBlock:]) }

// SetDataBlock 设置数据块地址。
func (n VectorNode) SetDataBlock(a Address) { a.WriteAddress(n.seg[offVecDataBlock:]) }

// LevelCount 是这个节点参与到第几层。
func (n VectorNode) LevelCount() int { return int(n.seg[offVecLevelCount]) }

// SetLevelCount 设置这个节点的层数。
func (n VectorNode) SetLevelCount(levels int) error {
	if levels < 1 || levels > VectorMaxLevels {
		return fmt.Errorf("xpage: vector node levels %d out of range 1..%d", levels, VectorMaxLevels)
	}
	n.seg[offVecLevelCount] = uint8(levels)
	return nil
}

// Dimensions 是内联向量的维数，**0 表示向量另存在别处**。
//
// 维数与「存在哪里」共用一个字段：0 维的向量没有意义，
// 所以这个值可以兼作标记。
func (n VectorNode) Dimensions() int { return int(binary.LittleEndian.Uint16(n.seg[offVecLength:])) }

// HasInlineVector 报告向量是不是就存在这个节点里。
func (n VectorNode) HasInlineVector() bool { return n.Dimensions() > 0 }

// ExternalVector 返回另存的向量在哪里，内联时给空地址。
func (n VectorNode) ExternalVector() Address {
	if n.HasInlineVector() {
		return EmptyAddress
	}
	return ReadAddress(n.seg[offVecPayload:])
}

// vectorLevelOffset 返回某一层的邻居表在节点里的位置。
func vectorLevelOffset(level int) int { return offVecLevels + level*vectorLevelStride }

// Neighbors 读出某一层的邻居，计数超限时判成损坏。
func (n VectorNode) Neighbors(level int) ([]Address, error) {
	if level < 0 || level >= VectorMaxLevels {
		return nil, fmt.Errorf("xpage: vector level %d out of range 0..%d", level, VectorMaxLevels-1)
	}
	off := vectorLevelOffset(level)
	count := int(n.seg[off])
	if count > VectorMaxNeighborsPerLevel {
		return nil, fmt.Errorf("%w: vector node level %d claims %d neighbors, limit is %d",
			ErrCorrupt, level, count, VectorMaxNeighborsPerLevel)
	}
	out := make([]Address, count)
	for i := range out {
		out[i] = ReadAddress(n.seg[off+1+i*AddressSize:])
	}
	return out, nil
}

// SetNeighbors 写入某一层的邻居，多出来的丢掉。
//
// **尾部空位一律写成空地址**：邻居表是定长的，不清的话
// 残留的旧地址会在下次读出来时被当成还在的邻居。
func (n VectorNode) SetNeighbors(level int, addrs []Address) error {
	if level < 0 || level >= VectorMaxLevels {
		return fmt.Errorf("xpage: vector level %d out of range 0..%d", level, VectorMaxLevels-1)
	}
	off := vectorLevelOffset(level)
	count := min(len(addrs), VectorMaxNeighborsPerLevel)
	n.seg[off] = uint8(count)
	pos := off + 1
	for i := range count {
		addrs[i].WriteAddress(n.seg[pos:])
		pos += AddressSize
	}

	for i := count; i < VectorMaxNeighborsPerLevel; i++ {
		EmptyAddress.WriteAddress(n.seg[pos:])
		pos += AddressSize
	}
	return nil
}

// TryAddNeighbor 加一个邻居，满了或已经在里面则返回 false。
//
// 返回 false 不是错误：调用方据此决定要不要挤掉一个更远的邻居。
func (n VectorNode) TryAddNeighbor(level int, a Address) (bool, error) {
	cur, err := n.Neighbors(level)
	if err != nil {
		return false, err
	}
	if len(cur) >= VectorMaxNeighborsPerLevel {
		return false, nil
	}
	if slices.Contains(cur, a) {
		return false, nil
	}
	return true, n.SetNeighbors(level, append(cur, a))
}

// RemoveNeighbor 去掉一个邻居，不在里面则返回 false。
func (n VectorNode) RemoveNeighbor(level int, a Address) (bool, error) {
	cur, err := n.Neighbors(level)
	if err != nil {
		return false, err
	}
	out := cur[:0]
	found := false
	for _, x := range cur {
		if x == a {
			found = true
			continue
		}
		out = append(out, x)
	}
	if !found {
		return false, nil
	}
	return true, n.SetNeighbors(level, out)
}

// ReadVector 读出内联的向量；向量另存时报错。
func (n VectorNode) ReadVector() ([]float32, error) {
	dims := n.Dimensions()
	if dims == 0 {
		return nil, fmt.Errorf("xpage: vector is stored externally at %s", n.ExternalVector())
	}
	out := make([]float32, dims)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(n.seg[offVecPayload+i*4:]))
	}
	return out, nil
}

// UpdateVector 就地改写内联向量，维数必须与原来一致。
//
// 维数变了就不是同一条索引能容纳的东西——建索引时维数就定死了。
func (n VectorNode) UpdateVector(v []float32) error {
	dims := n.Dimensions()
	if dims == 0 {
		return fmt.Errorf("xpage: cannot update externally stored vector at %s", n.ExternalVector())
	}
	if len(v) != dims {
		return fmt.Errorf("xpage: vector has %d dimensions, node holds %d", len(v), dims)
	}
	n.writePayload(v)
	return nil
}

// writePayload 写入维数与向量本体。
func (n VectorNode) writePayload(v []float32) {
	binary.LittleEndian.PutUint16(n.seg[offVecLength:], uint16(len(v)))
	for i, f := range v {
		binary.LittleEndian.PutUint32(n.seg[offVecPayload+i*4:], math.Float32bits(f))
	}
}

// InsertVectorNode 在页里划出一个向量节点。
//
// 向量放得进去时 external 必须是空地址，放不进去时必须非空——
// 两者对不上直接报错，不猜调用方的意图。
//
// 各层邻居表都要初始化：新划出的段里可能残留旧字节。
func (p *Page) InsertVectorNode(dataBlock Address, v []float32, levels int, external Address) (VectorNode, uint8, error) {
	if levels < 1 || levels > VectorMaxLevels {
		return VectorNode{}, 0, fmt.Errorf("xpage: vector node levels %d out of range 1..%d",
			levels, VectorMaxLevels)
	}
	if len(v) > math.MaxUint16 {
		return VectorNode{}, 0, fmt.Errorf("xpage: vector has %d dimensions, limit is %d",
			len(v), math.MaxUint16)
	}
	size, inline := VectorNodeSize(len(v))
	if inline != external.IsEmpty() {
		if inline {
			return VectorNode{}, 0, fmt.Errorf("xpage: %d-dimension vector fits inline, external address must be empty", len(v))
		}
		return VectorNode{}, 0, fmt.Errorf("xpage: %d-dimension vector needs external storage, address is empty", len(v))
	}
	seg, i, err := p.Insert(size)
	if err != nil {
		return VectorNode{}, 0, err
	}
	dataBlock.WriteAddress(seg[offVecDataBlock:])
	seg[offVecLevelCount] = uint8(levels)
	n := VectorNode{seg: seg}
	for level := range VectorMaxLevels {
		if err := n.SetNeighbors(level, nil); err != nil {
			return VectorNode{}, 0, err
		}
	}
	if inline {
		n.writePayload(v)
	} else {
		binary.LittleEndian.PutUint16(seg[offVecLength:], 0)
		external.WriteAddress(seg[offVecPayload:])
	}
	return n, i, nil
}

// GetVectorNode 取出某个槽里的向量节点。
func (p *Page) GetVectorNode(i uint8) (VectorNode, error) {
	seg, err := p.Get(i)
	if err != nil {
		return VectorNode{}, err
	}
	return AsVectorNode(seg)
}
