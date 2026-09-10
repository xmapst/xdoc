package xpage

import (
	"bytes"
	"fmt"
	"slices"

	"github.com/xmapst/xdoc/internal/xbin"
)

const (
	// 集合页里各部分的偏移：先是数据页空闲链的链头数组，
	// 从 96 字节起是索引表。
	offFreeDataPageList = 32

	offIndexCount = 96
)

// IndexKind 是索引的种类。
type IndexKind uint8

const (
	// IndexSkipList 是按键排序的跳表索引；IndexVector 是向量索引。
	IndexSkipList IndexKind = 0

	IndexVector IndexKind = 1
)

// MaxIndexNameLength 是索引名最多几个字符。
const MaxIndexNameLength = 32

// MaxIndexes 是一个集合最多几条索引。
const MaxIndexes = 255

// collectionContentLimit 是索引表能占的字节数。
const collectionContentLimit = PageSize - offIndexCount

// CollectionIndex 是一条索引的元信息。
type CollectionIndex struct {
	// Name 是索引名，在集合内唯一。
	Name string

	// Expression 是取键表达式的归一后源文本，查询靠它匹配索引。
	Expression string

	// Head、Tail 是跳表的头尾哨兵节点。
	Head, Tail Address

	// FreeIndexPageList 是这条索引的空闲索引页链的链头。
	FreeIndexPageList uint32

	// Slot 是这条索引的槽号，索引节点里记的就是它。
	//
	// **槽号只增不减**：删掉一条索引不会释放它的槽号，
	// 见 [CollectionPage.AddIndex]。
	Slot   uint8
	Kind   IndexKind
	Unique bool
}

// VectorIndex 是一条向量索引的额外信息。
//
// 它与同名的 [CollectionIndex] 并存：那一条记通用的元信息，
// 这一条记维数、度量与图的根节点。
type VectorIndex struct {
	// Name 与对应的 [CollectionIndex] 同名，Root 是图的入口节点。
	Name string
	Root Address

	// 维数、度量与空闲页链。维数在建索引时定死。
	FreePageList uint32
	Dimensions   uint16
	Slot         uint8
	Metric       uint8
}

// CollectionPage 是一个集合的元信息页：数据页空闲链与索引表。
//
// 索引表解出来放在内存里，改动之后靠 [CollectionPage.Flush] 写回。
type CollectionPage struct {
	*Page
	freeDataPages [DataFreeSlotCount]uint32
	indexes       []CollectionIndex
	vectorIndexes []VectorIndex
}

// NewCollectionPage 初始化一个集合页，空闲链全部置空。
func NewCollectionPage(buf []byte, id uint32) (*CollectionPage, error) {
	p, err := PageCollection.NewPage(buf, id)
	if err != nil {
		return nil, err
	}
	c := &CollectionPage{Page: p}
	for i := range c.freeDataPages {
		c.freeDataPages[i] = EmptyPageID
	}
	return c, c.Flush()
}

// LoadCollectionPage 读出集合页并解出索引表。
func LoadCollectionPage(buf []byte) (*CollectionPage, error) {
	p, err := Load(buf)
	if err != nil {
		return nil, err
	}
	if p.Type() != PageCollection {
		return nil, fmt.Errorf("%w: page %d has type %s, want %s",
			ErrCorrupt, p.ID(), p.Type(), PageCollection)
	}
	c := &CollectionPage{Page: p}
	for i := range c.freeDataPages {
		c.freeDataPages[i] = c.u32(offFreeDataPageList + i*4)
	}
	r := &reader{b: buf, p: offIndexCount}
	n, err := r.u8()
	if err != nil {
		return nil, err
	}
	for range int(n) {
		var ix CollectionIndex
		if ix, err = r.indexEntry(); err != nil {
			return nil, err
		}
		c.indexes = append(c.indexes, ix)
	}

	vn, err := r.u8()
	if err != nil {
		return nil, err
	}
	for range int(vn) {
		var vx VectorIndex
		if vx, err = r.vectorEntry(); err != nil {
			return nil, err
		}
		c.vectorIndexes = append(c.vectorIndexes, vx)
	}
	return c, nil
}

// FreeDataPage 返回某一档空闲数据页链的链头。
func (c *CollectionPage) FreeDataPage(slot uint8) uint32 { return c.freeDataPages[slot] }

// SetFreeDataPage 设置某一档空闲数据页链的链头。
func (c *CollectionPage) SetFreeDataPage(slot uint8, id uint32) {
	c.freeDataPages[slot] = id
	c.putU32(offFreeDataPageList+int(slot)*4, id)
	c.dirty.Store(true)
}

// Indexes 返回全部索引，第一条是主键索引。
func (c *CollectionPage) Indexes() []CollectionIndex { return c.indexes }

// VectorIndexes 返回全部向量索引的额外信息。
func (c *CollectionPage) VectorIndexes() []VectorIndex { return c.vectorIndexes }

// Index 按名字查一条索引，返回的是内部切片里的指针。
func (c *CollectionPage) Index(name string) (*CollectionIndex, bool) {
	for i := range c.indexes {
		if c.indexes[i].Name == name {
			return &c.indexes[i], true
		}
	}
	return nil, false
}

// PrimaryIndex 返回主键索引，它总是第一条。
func (c *CollectionPage) PrimaryIndex() (*CollectionIndex, bool) {
	if len(c.indexes) == 0 {
		return nil, false
	}
	return &c.indexes[0], true
}

// AddIndex 加一条索引。
//
// 槽号取「当前最大槽号加一」，所以**删掉的索引不会把槽号还回来**：
// 一个集合反复建删索引 255 次之后就再也建不了新索引，
// 要靠重建来回收。这样做是因为索引节点里记着槽号，
// 重用槽号会让残留的旧节点被当成新索引的一部分。
//
// 写回失败时把索引表还原回去：集合页装不下更多索引名时会走到这里。
func (c *CollectionPage) AddIndex(ix CollectionIndex) (*CollectionIndex, error) {
	if len([]rune(ix.Name)) > MaxIndexNameLength {
		return nil, fmt.Errorf("xpage: index name %q is longer than %d characters",
			ix.Name, MaxIndexNameLength)
	}
	if err := xbin.ValidateCString(ix.Name); err != nil {
		return nil, fmt.Errorf("xpage: index name: %w", err)
	}
	if err := xbin.ValidateCString(ix.Expression); err != nil {
		return nil, fmt.Errorf("xpage: index expression: %w", err)
	}
	if _, ok := c.Index(ix.Name); ok {
		return nil, fmt.Errorf("xpage: index %q already exists", ix.Name)
	}
	if len(c.indexes) >= MaxIndexes {
		return nil, fmt.Errorf("xpage: collection already has %d indexes", len(c.indexes))
	}

	ix.Slot = 0
	if len(c.indexes) > 0 {
		slot := c.indexes[0].Slot
		for _, e := range c.indexes[1:] {
			slot = max2(slot, e.Slot)
		}
		if slot == MaxIndexes-1 {
			return nil, fmt.Errorf("xpage: index slots exhausted (deleted indexes do not free their slot)")
		}
		ix.Slot = slot + 1
	}
	saved := c.indexes
	c.indexes = append(slices.Clone(c.indexes), ix)
	if err := c.Flush(); err != nil {
		c.indexes = saved
		_ = c.Flush()
		return nil, err
	}
	return &c.indexes[len(c.indexes)-1], nil
}

// max2 返回两者中较大的那个。
func max2(a, b uint8) uint8 {
	if a > b {
		return a
	}
	return b
}

// DeleteIndex 删一条索引，主键索引不许删。
//
// 同名的向量索引信息一并删掉。写回失败时两边都还原。
func (c *CollectionPage) DeleteIndex(name string) error {
	i := slices.IndexFunc(c.indexes, func(e CollectionIndex) bool { return e.Name == name })
	if i < 0 {
		return fmt.Errorf("xpage: index %q not found", name)
	}
	if i == 0 {
		return fmt.Errorf("xpage: cannot drop the primary index")
	}

	savedIx, savedVx := c.indexes, c.vectorIndexes
	c.indexes = slices.Delete(slices.Clone(c.indexes), i, i+1)
	if j := slices.IndexFunc(c.vectorIndexes, func(e VectorIndex) bool { return e.Name == name }); j >= 0 {
		c.vectorIndexes = slices.Delete(slices.Clone(c.vectorIndexes), j, j+1)
	}

	if err := c.Flush(); err != nil {
		c.indexes, c.vectorIndexes = savedIx, savedVx
		_ = c.Flush()
		return err
	}
	return nil
}

// UpdateIndex 按槽号更新一条索引的元信息并写回。
func (c *CollectionPage) UpdateIndex(ix *CollectionIndex) error {
	for i := range c.indexes {
		if c.indexes[i].Slot == ix.Slot {
			c.indexes[i] = *ix
			return c.Flush()
		}
	}
	return fmt.Errorf("xpage: index slot %d not found", ix.Slot)
}

// AddVectorIndex 建一条向量索引：先加通用条目，再加向量专属条目。
//
// 第二步失败时把第一步也撤掉，不留下一条没有维数信息的向量索引。
func (c *CollectionPage) AddVectorIndex(name, expr string, dims uint16, metric uint8) (*CollectionIndex, *VectorIndex, error) {
	if dims == 0 {
		return nil, nil, fmt.Errorf("xpage: vector index %q needs at least one dimension", name)
	}
	ix, err := c.AddIndex(CollectionIndex{Name: name, Expression: expr, Kind: IndexVector})
	if err != nil {
		return nil, nil, err
	}
	saved := c.vectorIndexes
	c.vectorIndexes = append(slices.Clone(c.vectorIndexes), VectorIndex{
		Name:       name,
		Root:       EmptyAddress,
		Dimensions: dims,
		Slot:       ix.Slot,
		Metric:     metric,

		FreePageList: EmptyPageID,
	})
	if err := c.Flush(); err != nil {
		c.vectorIndexes = saved
		_ = c.DeleteIndex(name)
		return nil, nil, err
	}
	return ix, &c.vectorIndexes[len(c.vectorIndexes)-1], nil
}

// VectorIndexByName 按名字查向量索引的额外信息。
func (c *CollectionPage) VectorIndexByName(name string) (*VectorIndex, bool) {
	if i := slices.IndexFunc(c.vectorIndexes, func(e VectorIndex) bool { return e.Name == name }); i >= 0 {
		return &c.vectorIndexes[i], true
	}
	return nil, false
}

// UpdateVectorIndex 按槽号更新向量索引的额外信息并写回。
func (c *CollectionPage) UpdateVectorIndex(vx *VectorIndex) error {
	for i := range c.vectorIndexes {
		if c.vectorIndexes[i].Slot == vx.Slot {
			c.vectorIndexes[i] = *vx
			return c.Flush()
		}
	}
	return fmt.Errorf("xpage: vector index slot %d not found", vx.Slot)
}

// Flush 把内存里的空闲链与索引表写回页里。
//
// 页已经被回收成空页时只置脏标记就返回：那时页头已经清过，
// 再往里写索引表会让一个空页看起来还有内容。
//
// 先清空整块再写：新表比旧表短时，残留的旧字节会让下次解析
// 读到多余的条目。
func (c *CollectionPage) Flush() error {
	c.dirty.Store(true)
	if c.Type() == PageEmpty {
		return nil
	}
	for i, id := range c.freeDataPages {
		c.putU32(offFreeDataPageList+i*4, id)
	}
	var body []byte
	body = append(body, uint8(len(c.indexes)))
	for _, ix := range c.indexes {
		body = ix.appendIndexEntry(body)
	}
	body = append(body, uint8(len(c.vectorIndexes)))
	for _, vx := range c.vectorIndexes {
		body = vx.appendVectorEntry(body)
	}
	if len(body) >= collectionContentLimit {
		return fmt.Errorf("xpage: index metadata needs %d bytes, only %d available",
			len(body), collectionContentLimit-1)
	}
	clear(c.buf[offIndexCount:])
	copy(c.buf[offIndexCount:], body)
	return nil
}

// appendIndexEntry 把一条索引编成字节。
//
// 尾部那个固定的 1 是格式里的一个占位字节，读的一侧读掉就丢。
func (ix CollectionIndex) appendIndexEntry(dst []byte) []byte {
	dst = append(dst, ix.Slot, uint8(ix.Kind))
	dst = append(dst, ix.Name...)
	dst = append(dst, 0)
	dst = append(dst, ix.Expression...)
	dst = append(dst, 0)
	u := uint8(0)
	if ix.Unique {
		u = 1
	}
	dst = append(dst, u)
	dst = ix.Head.AppendAddress(dst)
	dst = ix.Tail.AppendAddress(dst)

	dst = append(dst, 1)
	return appendU32(dst, ix.FreeIndexPageList)
}

// indexEntry 解出一条索引。
func (r *reader) indexEntry() (CollectionIndex, error) {
	var ix CollectionIndex
	var err error
	if ix.Slot, err = r.u8(); err != nil {
		return ix, err
	}
	k, err := r.u8()
	if err != nil {
		return ix, err
	}
	ix.Kind = IndexKind(k)
	if ix.Name, err = r.cstring(); err != nil {
		return ix, err
	}
	if ix.Expression, err = r.cstring(); err != nil {
		return ix, err
	}
	u, err := r.u8()
	if err != nil {
		return ix, err
	}
	ix.Unique = u != 0
	if ix.Head, err = r.addr(); err != nil {
		return ix, err
	}
	if ix.Tail, err = r.addr(); err != nil {
		return ix, err
	}
	if _, err = r.u8(); err != nil {
		return ix, err
	}
	ix.FreeIndexPageList, err = r.u32()
	return ix, err
}

// appendVectorEntry 把一条向量索引的额外信息编成字节。
func (vx VectorIndex) appendVectorEntry(dst []byte) []byte {
	dst = append(dst, vx.Name...)
	dst = append(dst, 0)
	dst = append(dst, vx.Slot)
	dst = appendU16(dst, vx.Dimensions)
	dst = append(dst, vx.Metric)
	dst = vx.Root.AppendAddress(dst)
	return appendU32(dst, vx.FreePageList)
}

// vectorEntry 解出一条向量索引的额外信息。
func (r *reader) vectorEntry() (VectorIndex, error) {
	var vx VectorIndex
	var err error
	if vx.Name, err = r.cstring(); err != nil {
		return vx, err
	}
	if vx.Slot, err = r.u8(); err != nil {
		return vx, err
	}
	if vx.Dimensions, err = r.u16(); err != nil {
		return vx, err
	}
	if vx.Metric, err = r.u8(); err != nil {
		return vx, err
	}
	if vx.Root, err = r.addr(); err != nil {
		return vx, err
	}
	vx.FreePageList, err = r.u32()
	return vx, err
}

// reader 是一个带边界检查的顺序读取器。
//
// 集合页里的索引表是变长的，逐字段读时每一步都可能越过页尾；
// 集中在这里检查，各解析函数就不必自己算剩余长度。
type reader struct {
	b []byte
	p int
}

// take 取出接下来的 n 个字节，不够则报损坏。
func (r *reader) take(n int) ([]byte, error) {
	if r.p+n > len(r.b) {
		return nil, fmt.Errorf("%w: read of %d bytes at %d runs past the page", ErrCorrupt, n, r.p)
	}
	s := r.b[r.p : r.p+n]
	r.p += n
	return s, nil
}

// u8 读出一个字节。
func (r *reader) u8() (uint8, error) {
	s, err := r.take(1)
	if err != nil {
		return 0, err
	}
	return s[0], nil
}

// u16 按小端读出两个字节。
func (r *reader) u16() (uint16, error) {
	s, err := r.take(2)
	if err != nil {
		return 0, err
	}
	return uint16(s[0]) | uint16(s[1])<<8, nil
}

// u32 按小端读出四个字节。
func (r *reader) u32() (uint32, error) {
	s, err := r.take(4)
	if err != nil {
		return 0, err
	}
	return uint32(s[0]) | uint32(s[1])<<8 | uint32(s[2])<<16 | uint32(s[3])<<24, nil
}

// addr 读出一个地址。
func (r *reader) addr() (Address, error) {
	s, err := r.take(AddressSize)
	if err != nil {
		return Address{}, err
	}
	return ReadAddress(s), nil
}

// cstring 读出一个以 0 结尾的串，顺带查一次 UTF-8。
func (r *reader) cstring() (string, error) {
	i := bytes.IndexByte(r.b[r.p:], 0)
	if i < 0 {
		return "", fmt.Errorf("%w: unterminated string at %d", ErrCorrupt, r.p)
	}
	s := string(r.b[r.p : r.p+i])
	r.p += i + 1
	if err := xbin.ValidateString(s); err != nil {
		return "", fmt.Errorf("%w: %w", ErrCorrupt, err)
	}
	return s, nil
}

// 集合页里的多字节整数一律小端。
func appendU16(dst []byte, v uint16) []byte { return append(dst, byte(v), byte(v>>8)) }
func appendU32(dst []byte, v uint32) []byte {
	return append(dst, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}
