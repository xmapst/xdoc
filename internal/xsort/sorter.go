package xsort

import (
	"cmp"
	"errors"
	"fmt"
	"iter"
	"slices"
	"unsafe"

	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xcoll"
	"github.com/xmapst/xdoc/internal/xpage"
)

// Order 是一个排序键的方向。
type Order int8

const (
	// Ascending 是升序，Descending 是降序。取值 ±1，比较结果乘上它就完成了方向反转。
	Ascending Order = 1

	Descending Order = -1
)

// maxRecord 是一条记录最多占多少字节：一个索引键加一个地址。
const maxRecord = xbson.MaxIndexKeyLength + xpage.AddressSize

// item 是一条待排序的记录：排序键与它指向的文档位置。
type item struct {
	key  *xbson.Value
	addr xpage.Address
}

// Sorter 是外部排序器：内存放不下就分段落盘，最后归并。
//
// 用法是 [Sorter.Insert] 一次灌完，再 [Sorter.All] 取出来。
type Sorter struct {
	// disk 是临时空间，coll 是字符串排序规则，orders 是各键的方向。
	disk   *Disk
	coll   xcoll.Collation
	orders []Order

	// runs 是已经排好的各段，buf 是复用的编码缓冲，count 是总记录数。
	runs     []*run
	buf      []byte
	count    int
	inserted bool
	err      error
	closed   bool
}

// NewSorter 建一个排序器，至少要给一个方向。
func (d *Disk) NewSorter(coll xcoll.Collation, orders ...Order) (*Sorter, error) {
	if d == nil {
		return nil, fmt.Errorf("xsort: nil disk")
	}
	if len(orders) == 0 {
		return nil, fmt.Errorf("xsort: at least one sort order is required")
	}
	for i, o := range orders {
		if o != Ascending && o != Descending {
			return nil, fmt.Errorf("xsort: order[%d] = %d is neither ascending nor descending", i, o)
		}
	}
	return &Sorter{disk: d, coll: coll, orders: slices.Clone(orders)}, nil
}

// Compare 按各键的方向比较两个排序键。
func (s *Sorter) Compare(a, b *xbson.Value) int {
	if len(s.orders) == 1 {
		return int(s.orders[0]) * a.Compare(b, s.coll)
	}
	return s.compareSegments(a, b)
}

// compareSegments 比较多键排序的复合键。
//
// 复合键是一个数组，每一项对应一个方向。有一边不是数组时退回按整体比——
// 那说明取键表达式没有产出预期的形状，此时至少要给出一个确定的次序。
func (s *Sorter) compareSegments(a, b *xbson.Value) int {
	x, okX := a.AsArray()
	y, okY := b.AsArray()
	if !okX || !okY {
		return a.Compare(b, s.coll)
	}
	n := min(x.Len(), y.Len(), len(s.orders))
	for i := range n {
		d := x.At(i).Compare(y.At(i), s.coll)
		if d == 0 {
			continue
		}
		return int(s.orders[i]) * d
	}
	return cmp.Compare(x.Len(), y.Len())
}

// Count 返回灌进来多少条记录。
func (s *Sorter) Count() int { return s.count }

// Runs 返回分了几段。
func (s *Sorter) Runs() int { return len(s.runs) }

// Spilled 报告有没有落过盘。
//
// 只有一段且它还在内存里时是 false——那种情形完全没碰磁盘。
func (s *Sorter) Spilled() bool {
	return slices.ContainsFunc(s.runs, func(r *run) bool { return r.data == nil })
}

// Err 返回 [Sorter.All] 遍历过程中出的错。
func (s *Sorter) Err() error { return s.err }

// Insert 灌入全部待排序的记录，只能调用一次。
//
// 攒满一段就地排序并落盘。**最后一段在只有它一段时留在内存里**：
// 数据量小于一段时全程不碰磁盘。
//
// nil 键当作空值：排序要给每条记录一个确定的位置。
func (s *Sorter) Insert(items iter.Seq2[*xbson.Value, xpage.Address]) error {
	if s.inserted {
		return fmt.Errorf("xsort: Insert called twice")
	}
	s.inserted = true

	var (
		batch []item
		size  int
		err   error
	)
	items(func(key *xbson.Value, addr xpage.Address) bool {
		if key == nil {
			key = xbson.Null
		}
		var n int
		if n, err = s.itemSize(key); err != nil {
			return false
		}
		s.count++

		if len(batch) > 0 && size+n > s.disk.runSize {
			if err = s.flush(batch, false); err != nil {
				return false
			}
			batch, size = batch[:0], 0
		}
		size += n
		batch = append(batch, item{key: key, addr: addr})
		return true
	})
	if err != nil {
		return err
	}
	if len(batch) == 0 {
		return nil
	}

	return s.flush(batch, len(s.runs) == 0)
}

// itemSize 估算一条记录在内存里占多少字节，键太大或类型不能当键时报错。
//
// 批里存的是解码后的键，**堆上的占用是编码字节的好几倍**，只按编码大小攒段
// 管不住内存。所以在编码大小之上再加批里的槽位和键的对象开销；
// 估算值不小于编码大小，攒出来的段照样放得进磁盘上的一段。
func (s *Sorter) itemSize(key *xbson.Value) (int, error) {
	n, err := key.IndexKeySize()
	switch {
	case errors.Is(err, xbson.ErrIndexKeyTooLong):
		return 0, fmt.Errorf("xsort: sort key must be less than %d bytes: %w", xbson.MaxIndexKeyLength, err)
	case err != nil:
		return 0, fmt.Errorf("xsort: cannot use %s as a sort key: %w", key.Type(), err)
	}
	return n + xpage.AddressSize + itemOverhead + heapOverhead(key), nil
}

// 估算堆占用用到的几个对象大小。
var (
	itemOverhead     = int(unsafe.Sizeof(item{}))
	valueOverhead    = int(unsafe.Sizeof(xbson.Value{}))
	arrayOverhead    = int(unsafe.Sizeof(xbson.Array{}))
	documentOverhead = int(unsafe.Sizeof(xbson.Document{}))
	pointerOverhead  = int(unsafe.Sizeof(uintptr(0)))
	stringOverhead   = int(unsafe.Sizeof(""))
)

// heapOverhead 估算一个值在堆上比编码多占的字节：值对象本身，
// 数组与文档再加上容器和各项。字符串之类的载荷已经算在编码大小里了。
func heapOverhead(v *xbson.Value) int {
	n := valueOverhead
	if a, ok := v.AsArray(); ok {
		n += arrayOverhead
		for _, it := range a.Items() {
			n += pointerOverhead + heapOverhead(it)
		}
	} else if d, ok := v.AsDocument(); ok {
		n += documentOverhead
		for _, val := range d.Elements() {
			n += stringOverhead + pointerOverhead + heapOverhead(val)
		}
	}
	return n
}

// flush 把一批记录排好序并编成字节。
//
// 用**稳定**排序：同键的记录保持灌入时的先后，这样整个排序结果是确定的。
//
// 落盘那条路把缓冲留给下一段复用；留在内存那条路把缓冲交出去，
// 下一段（如果有）重新分配。
func (s *Sorter) flush(batch []item, inMemory bool) error {
	slices.SortStableFunc(batch, func(x, y item) int { return s.Compare(x.key, y.key) })

	buf := s.buf[:0]
	for _, it := range batch {
		next, err := it.key.AppendIndexKey(buf)
		if err != nil {
			return fmt.Errorf("xsort: encode sort key: %w", err)
		}
		buf = it.addr.AppendAddress(next)
	}

	r := &run{count: len(batch), size: len(buf)}
	if inMemory {
		r.data = buf
		s.buf = nil
	} else {
		r.pos = s.disk.alloc()
		if err := s.disk.write(r.pos, buf); err != nil {
			s.disk.release(r.pos)
			return err
		}
		s.buf = buf
	}
	s.runs = append(s.runs, r)
	return nil
}

// Close 回收全部落盘的段。重复调用是空操作。
func (s *Sorter) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	for _, r := range s.runs {
		if r.data == nil {
			s.disk.release(r.pos)
		}
	}
	s.runs, s.buf = nil, nil
	return nil
}
