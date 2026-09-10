package xpage

import (
	"encoding/binary"
	"fmt"
	"slices"
	"sync/atomic"
)

// Page 是一页的内容：8 KB 缓冲加上页头与页尾槽表的解读。
//
// **它不拷贝内容**：所有读写都直接落在 buf 上，取出来的段就是页里的字节。
//
// 页里放若干「项」，每项在页尾有一个槽记着它的偏移与长度。
// 数据从页头之后往后长，槽表从页尾往前长，中间是空闲区。
type Page struct {
	// buf 是整页 8 KB，页头、数据区、槽表都在里面。
	buf []byte

	// startIndex 是找空闲槽时从哪里开始扫。
	//
	// 连续插入时能省下每次从头扫的开销；删除会把它归零，
	// 因为删出来的空槽可能在它前面。
	startIndex uint8

	// dirty 表示这一页改过，需要写回。
	//
	// 用原子值：属主改页时不拿事务锁，诊断视图却会从别的协程来读。
	dirty atomic.Bool
}

// NewPage 在一段缓冲上建一页新的，页头按类型初始化。
func (t PageType) NewPage(buf []byte, id uint32) (*Page, error) {
	if len(buf) != PageSize {
		return nil, fmt.Errorf("xpage: buffer is %d bytes, want %d", len(buf), PageSize)
	}
	p := &Page{buf: buf}
	p.reset(id, t)
	return p, nil
}

// Load 把一段缓冲当成一页读进来，**并检查内部账目**。
//
// 从文件读上来的页走这条：坏页要在这里挡住，而不是等到某次
// 取项时越界。
func Load(buf []byte) (*Page, error) {
	if len(buf) != PageSize {
		return nil, fmt.Errorf("%w: buffer is %d bytes, want %d", ErrCorrupt, len(buf), PageSize)
	}
	p := &Page{buf: buf}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}

// Attach 与 [Load] 一样，但**不检查**。
//
// 页刚写完、账目由调用方保证的场合走这条，省掉一次全页扫描。
func Attach(buf []byte) (*Page, error) {
	if len(buf) != PageSize {
		return nil, fmt.Errorf("%w: buffer is %d bytes, want %d", ErrCorrupt, len(buf), PageSize)
	}
	return &Page{buf: buf}, nil
}

// Bytes 返回整页缓冲。
func (p *Page) Bytes() []byte { return p.buf }

// Dirty 报告这一页改过没有。
func (p *Page) Dirty() bool { return p.dirty.Load() }

// MarkDirty 手工标记改过，绕开 [Page.Bytes] 直接改字节时要调它。
func (p *Page) MarkDirty() { p.dirty.Store(true) }

// reset 把一页清成指定类型的空页。
//
// 页头之后全部清零：残留的旧字节会让抢救扫描把已经删掉的内容
// 当成还在的数据捡回来。
func (p *Page) reset(id uint32, t PageType) {
	clear(p.buf[HeaderSize:])
	p.putU32(offPageID, id)
	p.buf[offPageType] = byte(t)
	p.putU32(offPrevPageID, EmptyPageID)
	p.putU32(offNextPageID, EmptyPageID)
	p.buf[offPageListSlot] = EmptyIndex
	p.putU32(offTransactionID, EmptyPageID)
	p.buf[offIsConfirmed] = 0
	p.putU32(offColID, EmptyPageID)
	p.buf[offItemsCount] = 0
	p.putU16(offUsedBytes, 0)
	p.putU16(offFragmentedBytes, 0)
	p.putU16(offNextFreePosition, HeaderSize)
	p.buf[offHighestIndex] = EmptyIndex
	p.buf[31] = 0
	p.startIndex = 0
	p.dirty.Store(true)
}

// MarkEmpty 把这一页清成空页，页号保留。
func (p *Page) MarkEmpty() { p.reset(p.ID(), PageEmpty) }

// ID 返回页号。页号写在页里，与它在文件中的位置一致。
func (p *Page) ID() uint32 { return p.u32(offPageID) }

// Type 返回页的用途。
func (p *Page) Type() PageType { return PageType(p.buf[offPageType]) }

// SetType 改页的用途。
func (p *Page) SetType(t PageType) { p.buf[offPageType] = byte(t); p.dirty.Store(true) }

// PrevPageID、NextPageID 是这一页在所属链上的前后邻居。
func (p *Page) PrevPageID() uint32 { return p.u32(offPrevPageID) }

// SetPrevPageID 设置链上的前一页。
func (p *Page) SetPrevPageID(v uint32) { p.putU32(offPrevPageID, v); p.dirty.Store(true) }

// NextPageID 返回链上的下一页。
func (p *Page) NextPageID() uint32 { return p.u32(offNextPageID) }

// SetNextPageID 设置链上的下一页。
func (p *Page) SetNextPageID(v uint32) { p.putU32(offNextPageID, v); p.dirty.Store(true) }

// PageListSlot 是这一页当前挂在哪一条空闲链上。
//
// 记在页里，是为了在剩余空间变化时知道要从哪条链上摘下来。
func (p *Page) PageListSlot() uint8 { return p.buf[offPageListSlot] }

// SetPageListSlot 设置这一页所在的空闲链。
func (p *Page) SetPageListSlot(v uint8) { p.buf[offPageListSlot] = v; p.dirty.Store(true) }

// TransactionID 是最后写这一页的事务号，[Page.IsConfirmed] 说明那个事务提交了没有。
//
// 两者一起构成崩溃恢复的依据：日志里那些属于未确认事务的页要丢掉。
func (p *Page) TransactionID() uint32 { return p.u32(offTransactionID) }

// SetTransactionID 记下写这一页的事务号。
func (p *Page) SetTransactionID(v uint32) { p.putU32(offTransactionID, v); p.dirty.Store(true) }

// ClearTransactionMark 清掉事务号与确认位。
//
// 页搬回数据文件之后不再需要它们；**这里不置脏标记**，
// 调用方本来就要写这一页。
func (p *Page) ClearTransactionMark() {
	p.putU32(offTransactionID, EmptyPageID)
	p.buf[offIsConfirmed] = 0
}

// IsConfirmed 报告写这一页的事务提交了没有。
func (p *Page) IsConfirmed() bool { return p.buf[offIsConfirmed] != 0 }

// SetConfirmed 设置确认位。
func (p *Page) SetConfirmed(v bool) {
	p.buf[offIsConfirmed] = 0
	if v {
		p.buf[offIsConfirmed] = 1
	}
	p.dirty.Store(true)
}

// ColID 是这一页属于哪个集合。
//
// 集合表坏掉时，抢救扫描靠它把散落的页认领回各自的集合。
func (p *Page) ColID() uint32 { return p.u32(offColID) }

// SetColID 设置这一页所属的集合。
func (p *Page) SetColID(v uint32) { p.putU32(offColID, v); p.dirty.Store(true) }

// ItemsCount 返回页里有几项。
func (p *Page) ItemsCount() int { return int(p.buf[offItemsCount]) }

// UsedBytes 是在用各段的长度之和，不含槽表。
func (p *Page) UsedBytes() int { return int(p.u16(offUsedBytes)) }

// FragmentedBytes 是删除留下的、夹在在用段之间的空洞总量。
//
// 这些字节是空闲的，但不连续，要整理之后才能用，见 [Page.Defrag]。
func (p *Page) FragmentedBytes() int { return int(p.u16(offFragmentedBytes)) }

// NextFreePosition 是连续空闲区的起点。
func (p *Page) NextFreePosition() int { return int(p.u16(offNextFreePosition)) }

// HighestIndex 是当前用到的最大槽号，[EmptyIndex] 表示一项都没有。
//
// 槽表的大小由它决定：中间的空槽照样占位置。
func (p *Page) HighestIndex() uint8 { return p.buf[offHighestIndex] }

// FooterSize 是槽表占多少字节。
func (p *Page) FooterSize() int {
	if p.HighestIndex() == EmptyIndex {
		return 0
	}
	return (int(p.HighestIndex()) + 1) * SlotSize
}

// FreeBytes 是这一页还能放多少字节（含碎片）。
//
// 项数满了就返回 0：即使还有空间，也没有槽号可用了。
func (p *Page) FreeBytes() int {
	if p.ItemsCount() == MaxItems {
		return 0
	}
	return PageSize - HeaderSize - p.UsedBytes() - p.FooterSize()
}

// slotLenOff、slotPosOff 是第 i 个槽的长度与偏移字段在页里的位置。
func slotLenOff(i uint8) int { return PageSize - (int(i)+1)*SlotSize }

// slotPosOff 返回第 i 个槽的偏移字段位置。
func slotPosOff(i uint8) int { return slotLenOff(i) + 2 }

// slot 读出第 i 个槽，偏移为 0 表示这个槽空着。
func (p *Page) slot(i uint8) (pos, length int) {
	return int(p.u16(slotPosOff(i))), int(p.u16(slotLenOff(i)))
}

// setSlot 写入第 i 个槽。
func (p *Page) setSlot(i uint8, pos, length int) {
	p.putU16(slotPosOff(i), uint16(pos))
	p.putU16(slotLenOff(i), uint16(length))
}

// Get 取出第 i 项，返回的是**页里的切片**，改它就是改页。
//
// 取之前查一遍这一段落在合法范围内：坏页上的槽可能指向页头或槽表里，
// 不查就会读到不该读的字节。
func (p *Page) Get(i uint8) ([]byte, error) {
	hi := p.HighestIndex()
	if hi == EmptyIndex || i > hi {
		return nil, fmt.Errorf("%w: slot %d beyond highest index %d", ErrCorrupt, i, hi)
	}
	pos, length := p.slot(i)
	if err := p.checkSegment(i, pos, length); err != nil {
		return nil, err
	}
	return p.buf[pos : pos+length], nil
}

// checkSegment 检查一段是否落在数据区之内、不侵入页头与槽表。
func (p *Page) checkSegment(i uint8, pos, length int) error {
	footer := p.FooterSize()
	if pos < HeaderSize || pos >= PageSize-footer {
		return fmt.Errorf("%w: slot %d position %d out of range [%d,%d)",
			ErrCorrupt, i, pos, HeaderSize, PageSize-footer)
	}
	if length <= 0 || length > PageSize-HeaderSize-footer {
		return fmt.Errorf("%w: slot %d length %d out of range", ErrCorrupt, i, length)
	}
	if pos+length > PageSize-footer {
		return fmt.Errorf("%w: slot %d spans into footer (%d+%d > %d)",
			ErrCorrupt, i, pos, length, PageSize-footer)
	}
	return nil
}

// Insert 在页里划出 length 字节，返回那一段与它的槽号。
func (p *Page) Insert(length int) ([]byte, uint8, error) {
	b, i, err := p.insertAt(EmptyIndex, length)
	return b, i, err
}

// insertAt 在指定槽号（或新槽）上划出一段。
//
// 新槽要多算 4 字节槽表开销——放得下数据却放不下槽的情形是有的。
//
// 连续空闲区不够、但算上碎片够时先整理一次：整理是搬字节，
// 不到必要不做。
func (p *Page) insertAt(index uint8, length int) ([]byte, uint8, error) {
	newSlot := index == EmptyIndex
	extra := 0
	if newSlot {
		extra = SlotSize
	}
	if length <= 0 {
		return nil, 0, fmt.Errorf("xpage: insert length must be positive, got %d", length)
	}
	if p.ItemsCount() >= MaxItems {
		return nil, 0, fmt.Errorf("%w: %d items", ErrTooManyItems, p.ItemsCount())
	}
	if p.FreeBytes() < length+extra {
		return nil, 0, fmt.Errorf("%w: need %d, have %d", ErrNoSpace, length+extra, p.FreeBytes())
	}
	if p.FreeBytes() < p.FragmentedBytes() {
		return nil, 0, fmt.Errorf("%w: free %d less than fragmented %d",
			ErrCorrupt, p.FreeBytes(), p.FragmentedBytes())
	}

	continuous := p.FreeBytes() - p.FragmentedBytes() - extra
	if length > continuous {
		if err := p.Defrag(); err != nil {
			return nil, 0, err
		}
	}

	if newSlot {
		index = p.freeIndex()
	}
	hi := p.HighestIndex()
	if hi == EmptyIndex || index > hi {
		if index != hi+1 {
			return nil, 0, fmt.Errorf("%w: slot %d is not one past highest index %d",
				ErrCorrupt, index, hi)
		}
		p.buf[offHighestIndex] = index
	}
	if pos, l := p.slot(index); pos != 0 || l != 0 {
		return nil, 0, fmt.Errorf("%w: slot %d already in use at %d+%d", ErrCorrupt, index, pos, l)
	}

	pos := p.NextFreePosition()
	p.setSlot(index, pos, length)
	p.buf[offItemsCount]++
	p.putU16(offUsedBytes, uint16(p.UsedBytes()+length))
	p.putU16(offNextFreePosition, uint16(pos+length))
	p.dirty.Store(true)

	if pos+length > PageSize-p.FooterSize() {
		return nil, 0, fmt.Errorf("%w: segment %d+%d overruns footer at %d",
			ErrCorrupt, pos, length, PageSize-p.FooterSize())
	}
	return p.buf[pos : pos+length], index, nil
}

// freeIndex 找一个空槽号。
//
// 从 startIndex 开始扫，找到就把起点推到它后面：连续插入时
// 不必每次都从 0 扫起。都占着就用最大槽号加一。
func (p *Page) freeIndex() uint8 {
	hi := p.HighestIndex()
	if hi != EmptyIndex {
		for i := p.startIndex; i <= hi; i++ {
			if pos, _ := p.slot(i); pos == 0 {
				p.startIndex = i + 1
				return i
			}
		}
	}

	return hi + 1
}

// Delete 删掉第 i 项。
//
// 删的是页尾那一段时直接把空闲起点往回收，那部分空间立刻可用；
// 删在中间则计入碎片，要整理之后才能用。
//
// 内容清零：残留的旧字节会被抢救扫描当成还在的数据。
func (p *Page) Delete(i uint8) error {
	hi := p.HighestIndex()
	if hi == EmptyIndex || i > hi {
		return fmt.Errorf("%w: slot %d beyond highest index %d", ErrCorrupt, i, hi)
	}
	pos, length := p.slot(i)
	if err := p.checkSegment(i, pos, length); err != nil {
		return err
	}

	p.setSlot(i, 0, 0)
	p.buf[offItemsCount]--
	p.putU16(offUsedBytes, uint16(p.UsedBytes()-length))
	clear(p.buf[pos : pos+length])

	if pos+length == p.NextFreePosition() {
		p.putU16(offNextFreePosition, uint16(pos))
	} else {
		p.putU16(offFragmentedBytes, uint16(p.FragmentedBytes()+length))
	}

	if hi == i {
		p.updateHighestIndex()
	}
	p.startIndex = 0

	if p.ItemsCount() == 0 {
		if p.HighestIndex() != EmptyIndex || p.UsedBytes() != 0 {
			return fmt.Errorf("%w: empty page still has highest index %d and %d used bytes",
				ErrCorrupt, p.HighestIndex(), p.UsedBytes())
		}
		p.putU16(offNextFreePosition, HeaderSize)
		p.putU16(offFragmentedBytes, 0)
	}
	p.dirty.Store(true)
	return nil
}

// updateHighestIndex 删掉最大槽之后，往前找新的最大槽。
func (p *Page) updateHighestIndex() {
	hi := p.HighestIndex()
	if hi == EmptyIndex {
		return
	}
	for i := int(hi) - 1; i >= 0; i-- {
		if pos, _ := p.slot(uint8(i)); pos != 0 {
			p.buf[offHighestIndex] = uint8(i)
			return
		}
	}
	p.buf[offHighestIndex] = EmptyIndex
}

// Update 把第 i 项改成 length 字节，返回新的那一段。
//
// 三种情形：一样长就地返回；变短就地截掉，多出来的按位置记成
// 可回收或碎片；变长则先删掉再重新划一段——**内容不保留**，
// 调用方要重新写。
func (p *Page) Update(i uint8, length int) ([]byte, error) {
	if length <= 0 {
		return nil, fmt.Errorf("xpage: update length must be positive, got %d", length)
	}
	hi := p.HighestIndex()
	if hi == EmptyIndex || i > hi {
		return nil, fmt.Errorf("%w: slot %d beyond highest index %d", ErrCorrupt, i, hi)
	}
	pos, old := p.slot(i)
	if err := p.checkSegment(i, pos, old); err != nil {
		return nil, err
	}
	p.dirty.Store(true)

	switch {
	case length == old:
		return p.buf[pos : pos+length], nil

	case length < old:
		diff := old - length
		if pos+old == p.NextFreePosition() {
			p.putU16(offNextFreePosition, uint16(pos+length))
		} else {
			p.putU16(offFragmentedBytes, uint16(p.FragmentedBytes()+diff))
		}
		p.putU16(offUsedBytes, uint16(p.UsedBytes()-diff))
		p.setSlot(i, pos, length)
		clear(p.buf[pos+length : pos+old])
		return p.buf[pos : pos+length], nil

	default:
		clear(p.buf[pos : pos+old])
		p.buf[offItemsCount]--
		p.putU16(offUsedBytes, uint16(p.UsedBytes()-old))
		if pos+old == p.NextFreePosition() {
			p.putU16(offNextFreePosition, uint16(pos))
		} else {
			p.putU16(offFragmentedBytes, uint16(p.FragmentedBytes()+old))
		}
		p.setSlot(i, 0, 0)
		b, _, err := p.insertAt(i, length)
		return b, err
	}
}

// Defrag 把在用的各段往页头方向压紧，消掉中间的空洞。
//
// 按当前位置排序之后依次前移，槽里的偏移跟着更新。压完之后
// 把尾部空出来的部分清零，免得抢救扫描把旧字节当成数据。
//
// 排序时发现两段位置相同或压缩区重叠，说明这一页的账目已经坏了。
func (p *Page) Defrag() error {
	if p.FragmentedBytes() == 0 {
		return nil
	}
	hi := p.HighestIndex()
	if hi == EmptyIndex {
		return fmt.Errorf("%w: page has %d fragmented bytes but no segments",
			ErrCorrupt, p.FragmentedBytes())
	}

	type seg struct {
		pos int
		idx uint8
	}
	segs := make([]seg, 0, int(hi)+1)
	for i := 0; i <= int(hi); i++ {
		if pos, _ := p.slot(uint8(i)); pos != 0 {
			segs = append(segs, seg{pos, uint8(i)})
		}
	}
	slices.SortFunc(segs, func(a, b seg) int { return a.pos - b.pos })

	next := HeaderSize
	for k, s := range segs {
		if k > 0 && s.pos == segs[k-1].pos {
			return fmt.Errorf("%w: slots %d and %d share position %d",
				ErrCorrupt, segs[k-1].idx, s.idx, s.pos)
		}
		_, length := p.slot(s.idx)
		if s.pos != next {
			if s.pos < next {
				return fmt.Errorf("%w: segment at %d overlaps compacted region ending at %d",
					ErrCorrupt, s.pos, next)
			}
			copy(p.buf[next:next+length], p.buf[s.pos:s.pos+length])
			p.putU16(slotPosOff(s.idx), uint16(next))
		}
		next += length
	}
	clear(p.buf[next : PageSize-p.FooterSize()])
	p.putU16(offFragmentedBytes, 0)
	p.putU16(offNextFreePosition, uint16(next))
	return nil
}

// 页里的多字节整数一律小端。
func (p *Page) u16(off int) uint16       { return binary.LittleEndian.Uint16(p.buf[off:]) }
func (p *Page) u32(off int) uint32       { return binary.LittleEndian.Uint32(p.buf[off:]) }
func (p *Page) putU16(off int, v uint16) { binary.LittleEndian.PutUint16(p.buf[off:], v) }
func (p *Page) putU32(off int, v uint32) { binary.LittleEndian.PutUint32(p.buf[off:], v) }

// RawPage 是还没解读成 [Page] 的一段页字节。
//
// 崩溃恢复时要在不信任页内容的前提下读写事务标记，
// 所以这几个访问器不做任何校验。
type RawPage []byte

// IsBlank 报告这一页看起来是不是从没写过。
//
// 只看前 16 字节：页号、类型、前后邻居都在那里，全零就说明
// 文件在这里被撑大过、但还没往里写。
func (r RawPage) IsBlank() bool {
	for _, b := range r[:16] {
		if b != 0 {
			return false
		}
	}
	return true
}

// TransactionID 直接从页字节里读出事务号。
func (r RawPage) TransactionID() uint32 {
	return binary.LittleEndian.Uint32(r[offTransactionID:])
}

// SetTransactionID 直接往页字节里写事务号。
func (r RawPage) SetTransactionID(v uint32) {
	binary.LittleEndian.PutUint32(r[offTransactionID:], v)
}

// IsConfirmed 直接从页字节里读出确认位。
func (r RawPage) IsConfirmed() bool { return r[offIsConfirmed] != 0 }

// SetConfirmed 直接往页字节里写确认位。
func (r RawPage) SetConfirmed(v bool) {
	r[offIsConfirmed] = 0
	if v {
		r[offIsConfirmed] = 1
	}
}

// PeekPageID 不做校验地读出一段页字节里的页号。
func PeekPageID(buf []byte) uint32 { return binary.LittleEndian.Uint32(buf[offPageID:]) }
