package xstore

import (
	"errors"
	"fmt"
	"iter"

	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xcoll"
	"github.com/xmapst/xdoc/internal/xpage"
)

// ErrDuplicateKey 表示往唯一索引里插入了已经存在的键。
var ErrDuplicateKey = errors.New("xstore: duplicate key in a unique index")

// ErrReservedKey 表示拿 MinValue 或 MaxValue 当索引键——那两个是跳表自己的头尾哨兵。
var ErrReservedKey = errors.New("xstore: min/max value cannot be used as an index key")

// Order 是遍历方向。它的取值同时也是比较结果的期望符号，[SkipList.Find] 直接拿它和比较结果比。
type Order int

const (
	// Asc 从头往尾，键由小到大。
	Asc Order = 1

	// Desc 从尾往头，键由大到小。
	Desc Order = -1
)

// Node 是跳表里的一个节点，连同它所在的页一起拿在手上。
//
// 页要留着，因为改动节点后得把页标脏。节点内容直接指向页缓冲，
// 页一旦被换出或重取，这份 Node 就不能再用了。
type Node struct {
	Addr xpage.Address
	node xpage.IndexNode
	page *xpage.Page
}

// Levels 返回该节点有几层。
func (n *Node) Levels() int { return n.node.Levels() }

// DataBlock 返回该节点指向的文档首块地址。
func (n *Node) DataBlock() xpage.Address { return n.node.DataBlock() }

// NextNode 返回同一篇文档在下一个索引里的节点地址。见 [DocNodes]。
func (n *Node) NextNode() xpage.Address { return n.node.NextNode() }

// SetNextNode 改写同文档链的后继，并把所在页标脏。
func (n *Node) SetNextNode(a xpage.Address) { n.node.SetNextNode(a); n.page.MarkDirty() }

// Slot 返回该节点属于哪个索引槽。
func (n *Node) Slot() uint8 { return n.node.Slot() }

// Key 解出该节点的索引键，每次调用都会新分配。
func (n *Node) Key() (*xbson.Value, error) { return n.node.Key() }

// KeyInto 把索引键解到调用方给的值里，供循环中复用同一个值、免去反复分配。
func (n *Node) KeyInto(dst *xbson.Value) error {
	_, err := dst.ReadIndexKeyInto(n.node.KeyBytes())
	return err
}

// fill 把 n 重新指向地址 a 处的节点，返回是否真的取到。
//
// 地址为空时返回 false 且不改动 n——这样调用方可以在循环里复用同一个 Node。
func (n *Node) fill(p Pages, a xpage.Address) (bool, error) {
	if a.IsEmpty() {
		return false, nil
	}
	page, err := p.GetPage(a.PageID)
	if err != nil {
		return false, err
	}

	nd, err := page.GetIndexNode(a.Index)
	if err != nil {
		return false, fmt.Errorf("index node %s: %w", a, err)
	}
	n.Addr, n.node, n.page = a, nd, page
	return true, nil
}

// getNode 取一个节点；地址为空时返回 nil 而不是错误。
func (s Store) getNode(a xpage.Address) (*Node, error) {
	var n Node
	ok, err := n.fill(s, a)
	if err != nil || !ok {
		return nil, err
	}
	return &n, nil
}

// CreateIndex 在集合页上登记一个跳表索引，并建好它的头尾哨兵。
//
// 头尾是两个满层的节点，键分别是 MinValue 和 MaxValue，只在第 0 层相连；
// 更高层一开始是空的，插入时才逐级接上。它们所在的页顺便成了该索引空闲链的头页。
func (s Store) CreateIndex(name, expr string, unique bool) (*xpage.CollectionIndex, error) {
	cp := s.CollectionPage()
	ix, err := cp.AddIndex(xpage.CollectionIndex{
		Kind:              xpage.IndexSkipList,
		Name:              name,
		Expression:        expr,
		Unique:            unique,
		Head:              xpage.EmptyAddress,
		Tail:              xpage.EmptyAddress,
		FreeIndexPageList: xpage.EmptyPageID,
	})
	if err != nil {
		return nil, err
	}
	page, err := s.NewPage(xpage.PageIndex)
	if err != nil {
		return nil, err
	}

	_, hi, err := page.InsertIndexNode(ix.Slot, xpage.MaxSkipLevel, xbson.MinValue)
	if err != nil {
		return nil, err
	}
	_, ti, err := page.InsertIndexNode(ix.Slot, xpage.MaxSkipLevel, xbson.MaxValue)
	if err != nil {
		return nil, err
	}
	head := xpage.Address{PageID: page.ID(), Index: hi}
	tail := xpage.Address{PageID: page.ID(), Index: ti}

	hn, err := s.getNode(head)
	if err != nil {
		return nil, err
	}
	tn, err := s.getNode(tail)
	if err != nil {
		return nil, err
	}

	hn.node.SetNext(0, tail)
	tn.node.SetPrev(0, head)
	page.MarkDirty()

	ix.Head, ix.Tail = head, tail
	ix.FreeIndexPageList = page.ID()
	page.SetPageListSlot(0)
	if err := cp.UpdateIndex(ix); err != nil {
		return nil, err
	}
	return ix, nil
}

// SkipList 是某一个索引的跳表视图：一个 [Store] 加一份索引元数据。
type SkipList struct {
	store Store
	ix    *xpage.CollectionIndex
}

// List 取某个索引的跳表视图。
func (s Store) List(ix *xpage.CollectionIndex) SkipList { return SkipList{store: s, ix: ix} }

// AddNode 往跳表里插一个键，指向 dataBlock，返回新节点。
//
// 层数当场掷出来；节点大小随层数和键长增长，超过单节点上限就报错。
// last 非空时把新节点接到同文档链的末尾。链接过程中出错会把刚建的节点删掉，
// 两个错误一并返回——否则跳表里会留下一个谁也够不着的孤儿。
func (l SkipList) AddNode(key *xbson.Value, dataBlock xpage.Address,
	last *Node, coll xcoll.Collation) (*Node, error) {
	if key.Type() == xbson.TypeMinValue || key.Type() == xbson.TypeMaxValue {
		return nil, fmt.Errorf("%w: those two are the skip list's own sentinels", ErrReservedKey)
	}

	levels := defaultRandomizer.flip()

	keyLen, err := key.IndexKeySize()
	if err != nil {
		return nil, err
	}
	size := xpage.IndexNodeSize(levels, keyLen)
	if size > xpage.MaxIndexNodeSize {
		return nil, fmt.Errorf("xstore: index node needs %d bytes, limit is %d",
			size, xpage.MaxIndexNodeSize)
	}

	page, err := l.store.getFreeIndexPage(l.ix.FreeIndexPageList)
	if err != nil {
		return nil, err
	}
	_, idx, err := page.InsertIndexNode(l.ix.Slot, levels, key)
	if err != nil {
		return nil, err
	}
	addr := xpage.Address{PageID: page.ID(), Index: idx}
	node, err := l.store.getNode(addr)
	if err != nil {
		return nil, err
	}
	node.node.SetDataBlock(dataBlock)
	page.MarkDirty()

	if err := l.linkNode(node, key, levels, coll); err != nil {
		return nil, errors.Join(err, l.DeleteNode(addr))
	}

	if last != nil {
		ln, err := l.store.getNode(last.Addr)
		if err != nil {
			return nil, err
		}
		ln.SetNextNode(addr)
	}

	head, err := l.store.syncIndexFreeList(page, l.ix.FreeIndexPageList)
	if err != nil {
		return nil, err
	}
	if head != l.ix.FreeIndexPageList {
		l.ix.FreeIndexPageList = head
		if err := l.store.CollectionPage().UpdateIndex(l.ix); err != nil {
			return nil, err
		}
	}
	return l.store.getNode(addr)
}

// CheckAdd 不动任何页，预先确认 [SkipList.AddNode] 不会因为键本身失败：
// 不是哨兵值、编码后不超长、唯一索引里没有相等的键。
//
// 撞上的节点若 ignore 认可（通常是同一次改写里马上要摘掉的旧节点），不算重复。
// 键长不超上限时最高层的节点也放得下，所以不必按层数再核对节点大小。
func (l SkipList) CheckAdd(key *xbson.Value, coll xcoll.Collation, ignore func(xpage.Address) bool) error {
	if key.Type() == xbson.TypeMinValue || key.Type() == xbson.TypeMaxValue {
		return fmt.Errorf("%w: those two are the skip list's own sentinels", ErrReservedKey)
	}
	if _, err := key.IndexKeySize(); err != nil {
		return err
	}
	if !l.ix.Unique {
		return nil
	}
	n, err := l.Find(key, false, Asc, coll)
	if err != nil {
		return err
	}
	if n != nil && (ignore == nil || !ignore(n.Addr)) {
		return fmt.Errorf("%w: index %q", ErrDuplicateKey, l.ix.Name)
	}
	return nil
}

// 最高层节点配最长的键也不超过单节点上限，[SkipList.CheckAdd] 靠这一点省掉节点大小的核对。
const _ = uint(xpage.MaxIndexNodeSize -
	(xpage.IndexNodeHeaderSize + xpage.MaxSkipLevel*2*xpage.AddressSize + xbson.MaxIndexKeyLength))

// linkNode 把新节点接进跳表。
//
// 先从最高层往下找每一层该插在谁后面，一路记进 lefts；这一趟顺带做唯一性检查，
// 撞上相等的键就报重复。再从新节点自己的最高层往下逐层接上前后指针。
//
// 即便新节点只有一层，找路也从最高层开始走——那才是跳表跳得快的原因。
func (l SkipList) linkNode(node *Node, key *xbson.Value,
	levels int, coll xcoll.Collation) error {
	var left, cur, other Node
	var rk xbson.Value

	var lefts [xpage.MaxSkipLevel]xpage.Address
	leftAddr := l.ix.Head
	for level := xpage.MaxSkipLevel - 1; level >= 0; level-- {
		if _, err := left.fill(l.store, leftAddr); err != nil {
			return err
		}
		right := left.node.Next(level)
		for !right.IsEmpty() && right != l.ix.Tail {
			if _, err := cur.fill(l.store, right); err != nil {
				return err
			}
			if err := cur.KeyInto(&rk); err != nil {
				return err
			}
			diff := rk.Compare(key, coll)
			if diff == 0 && l.ix.Unique {
				return fmt.Errorf("%w: index %q", ErrDuplicateKey, l.ix.Name)
			}
			if diff > 0 {
				break
			}

			leftAddr = right
			right = cur.node.Next(level)
		}
		lefts[level] = leftAddr
	}

	for level := levels - 1; level >= 0; level-- {
		if _, err := left.fill(l.store, lefts[level]); err != nil {
			return err
		}
		if err := left.checkLevel(level); err != nil {
			return err
		}
		next := left.node.Next(level)
		if next.IsEmpty() {
			next = l.ix.Tail
		}
		if _, err := cur.fill(l.store, node.Addr); err != nil {
			return err
		}
		cur.node.SetNext(level, next)
		cur.node.SetPrev(level, lefts[level])
		cur.page.MarkDirty()

		left.node.SetNext(level, node.Addr)
		left.page.MarkDirty()

		if ok, err := other.fill(l.store, next); err != nil {
			return err
		} else if ok {
			if err := other.checkLevel(level); err != nil {
				return err
			}
			other.node.SetPrev(level, node.Addr)
			other.page.MarkDirty()
		}
	}
	return nil
}

// checkLevel 确认该节点确实有这一层。
//
// 一个只有两层的节点被挂在第五层上，说明索引已经损坏；再往下写会越界。
func (n *Node) checkLevel(level int) error {
	if lv := n.node.Levels(); level >= lv {
		return fmt.Errorf("%w: node at %v has %d levels but is linked at level %d",
			xpage.ErrCorrupt, n.Addr, lv, level)
	}
	return nil
}

// DeleteNode 从跳表里摘掉一个节点并回收它的空间。
//
// 逐层把前驱和后继接起来。每层都重新取一次节点：前面几层的改动可能已经
// 让页缓冲失效。
func (l SkipList) DeleteNode(addr xpage.Address) error {
	node, err := l.store.getNode(addr)
	if err != nil || node == nil {
		return err
	}

	for level := node.Levels() - 1; level >= 0; level-- {
		n, err := l.store.getNode(addr)
		if err != nil {
			return err
		}
		prev, next := n.node.Prev(level), n.node.Next(level)
		if pn, err := l.store.getNode(prev); err != nil {
			return err
		} else if pn != nil {
			if err := pn.checkLevel(level); err != nil {
				return err
			}
			pn.node.SetNext(level, next)
			pn.page.MarkDirty()
		}
		if nn, err := l.store.getNode(next); err != nil {
			return err
		} else if nn != nil {
			if err := nn.checkLevel(level); err != nil {
				return err
			}
			nn.node.SetPrev(level, prev)
			nn.page.MarkDirty()
		}
	}
	page, err := l.store.GetPage(addr.PageID)
	if err != nil {
		return err
	}
	if err := page.Delete(addr.Index); err != nil {
		return err
	}
	head, err := l.store.syncIndexFreeList(page, l.ix.FreeIndexPageList)
	if err != nil {
		return err
	}
	if head != l.ix.FreeIndexPageList {
		l.ix.FreeIndexPageList = head
		return l.store.CollectionPage().UpdateIndex(l.ix)
	}
	return nil
}

// Find 在跳表里查一个键，找不到时按 sibling 决定返回什么。
//
// 从最高层往下逐层逼近。命中相等的键就直接返回；越过目标时，若 sibling 为真
// 且已经在第 0 层，就返回越过的那个节点（也就是紧邻的下一个键），
// 否则降一层继续。哨兵不作为邻居返回。
//
// order 决定走向：升序时向后走并在「比目标大」处停，降序时向前走并在「比目标小」处停。
// 跳的步数超过 [Store.ChainLimit] 就报损坏。
func (l SkipList) Find(value *xbson.Value, sibling bool,
	order Order, coll xcoll.Collation) (*Node, error) {
	var left, cur Node
	var rk xbson.Value

	limit, hops := l.store.ChainLimit(), 0
	leftAddr := l.ix.Head
	if order == Desc {
		leftAddr = l.ix.Tail
	}
	for level := xpage.MaxSkipLevel - 1; level >= 0; level-- {
		if _, err := left.fill(l.store, leftAddr); err != nil {
			return nil, err
		}
		right := left.step(level, order)
		for !right.IsEmpty() {
			if hops++; hops > limit {
				return nil, fmt.Errorf("%w: index %q chain still going after %d nodes at level %d",
					xpage.ErrCorrupt, l.ix.Name, limit, level)
			}
			if _, err := cur.fill(l.store, right); err != nil {
				return nil, err
			}
			if err := cur.KeyInto(&rk); err != nil {
				return nil, err
			}
			diff := rk.Compare(value, coll)
			if diff == int(order) {
				if level == 0 && sibling {
					if rk.Type() == xbson.TypeMinValue || rk.Type() == xbson.TypeMaxValue {
						return nil, nil
					}
					out := cur
					return &out, nil
				}
				break
			}
			if diff == 0 {
				out := cur
				return &out, nil
			}
			leftAddr = right
			right = cur.step(level, order)
		}
	}
	return nil, nil
}

// step 按方向取该节点在这一层的邻居。
func (n *Node) step(level int, order Order) xpage.Address {
	if order == Asc {
		return n.node.Next(level)
	}
	return n.node.Prev(level)
}

// FindAll 沿第 0 层遍历整个索引，从头哨兵或尾哨兵起步。
//
// 碰到另一头的哨兵就停，因此产出的都是真实的键。步数超过 [Store.ChainLimit] 时
// 产出损坏错误。
func (l SkipList) FindAll(order Order) iter.Seq2[*Node, error] {
	return func(yield func(*Node, error) bool) {
		start := l.ix.Head
		if order == Desc {
			start = l.ix.Tail
		}
		cur, err := l.store.getNode(start)
		if err != nil {
			yield(nil, err)
			return
		}

		limit, hops := l.store.ChainLimit(), 0
		for cur != nil {
			if hops++; hops > limit {
				yield(nil, fmt.Errorf("%w: index %q level-0 chain still going after %d nodes",
					xpage.ErrCorrupt, l.ix.Name, limit))
				return
			}
			addr := cur.step(0, order)
			if addr.IsEmpty() {
				return
			}
			n, err := l.store.getNode(addr)
			if err != nil {
				yield(nil, err)
				return
			}
			if n == nil {
				return
			}
			k, err := n.Key()
			if err != nil {
				yield(nil, err)
				return
			}

			if k.Type() == xbson.TypeMinValue || k.Type() == xbson.TypeMaxValue {
				return
			}
			if !yield(n, nil) {
				return
			}
			cur = n
		}
	}
}

// NodeAt 按地址取一个索引节点。
func (s Store) NodeAt(a xpage.Address) (*Node, error) { return s.getNode(a) }

// DocNodes 是一篇文档在各个索引里那些节点串成的链。
//
// 链头是主键索引的节点，靠每个节点的 NextNode 往下走。有了它，删一篇文档时
// 不必逐个索引去查，顺着链摘干净即可。
type DocNodes struct {
	store Store
	cp    *xpage.CollectionPage
	pk    xpage.Address
}

// Chain 取某篇文档的索引节点链，pk 是它主键节点的地址。
func (s Store) Chain(cp *xpage.CollectionPage, pk xpage.Address) DocNodes {
	return DocNodes{store: s, cp: cp, pk: pk}
}

// bySlot 把集合的索引按槽号建表，好由节点反查它属于哪个索引。
func (dn DocNodes) bySlot() map[uint8]*xpage.CollectionIndex {
	out := map[uint8]*xpage.CollectionIndex{}
	for i := range dn.cp.Indexes() {
		ix := dn.cp.Indexes()[i]
		out[ix.Slot] = &ix
	}
	return out
}

// DeleteAll 摘掉这篇文档在所有索引里的节点，包括主键那个。
//
// 节点的槽号在集合页上找不到对应索引时报损坏——那说明索引表和索引页对不上了。
func (dn DocNodes) DeleteAll() error {
	bySlot := dn.bySlot()
	addr := dn.pk
	for hops := 0; !addr.IsEmpty(); hops++ {
		if hops > maxNodesPerDocument {
			return fmt.Errorf("%w: document index chain is too long or cyclic", xpage.ErrCorrupt)
		}
		n, err := dn.store.getNode(addr)
		if err != nil {
			return err
		}
		if n == nil {
			return nil
		}
		next := n.NextNode()
		ix, ok := bySlot[n.Slot()]
		if !ok {
			return fmt.Errorf("%w: node at %s belongs to unknown index slot %d",
				xpage.ErrCorrupt, addr, n.Slot())
		}
		if err := dn.store.List(ix).DeleteNode(addr); err != nil {
			return err
		}
		addr = next
	}
	return nil
}

// Delete 只摘掉 remove 里点名的那些节点，返回剩下的链尾。
//
// 从主键节点的后继开始走，跳过主键本身。删一个就把前驱的 NextNode 接到它的后继，
// 链不会断。remove 为空时什么都不删，直接返回链尾——调用方拿它来往后追加新节点。
func (dn DocNodes) Delete(remove []xpage.Address, coll xcoll.Collation) (*Node, error) {
	if len(remove) == 0 {
		return dn.last()
	}
	drop := make(map[xpage.Address]bool, len(remove))
	for _, a := range remove {
		drop[a] = true
	}
	bySlot := dn.bySlot()

	prevAddr := dn.pk
	addr, err := dn.store.nextOf(dn.pk)
	if err != nil {
		return nil, err
	}
	for hops := 0; !addr.IsEmpty(); hops++ {
		if hops > maxNodesPerDocument {
			return nil, fmt.Errorf("%w: document index chain is too long or cyclic", xpage.ErrCorrupt)
		}
		n, err := dn.store.getNode(addr)
		if err != nil {
			return nil, err
		}
		next := n.NextNode()
		if !drop[addr] {
			prevAddr = addr
			addr = next
			continue
		}
		ix, ok := bySlot[n.Slot()]
		if !ok {
			return nil, fmt.Errorf("%w: node at %s belongs to unknown index slot %d",
				xpage.ErrCorrupt, addr, n.Slot())
		}

		pn, err := dn.store.getNode(prevAddr)
		if err != nil {
			return nil, err
		}
		pn.SetNextNode(next)
		if err := dn.store.List(ix).DeleteNode(addr); err != nil {
			return nil, err
		}
		addr = next
	}
	return dn.store.getNode(prevAddr)
}

// last 沿同文档链走到最后一个节点。
func (dn DocNodes) last() (*Node, error) {
	addr := dn.pk
	for hops := 0; ; hops++ {
		if hops > maxNodesPerDocument {
			return nil, fmt.Errorf("%w: document index chain is too long or cyclic", xpage.ErrCorrupt)
		}
		n, err := dn.store.getNode(addr)
		if err != nil || n == nil {
			return nil, err
		}
		next := n.NextNode()
		if next.IsEmpty() {
			return n, nil
		}
		addr = next
	}
}

// nextOf 取某个节点在同文档链上的后继；节点不存在时返回空地址。
func (s Store) nextOf(a xpage.Address) (xpage.Address, error) {
	n, err := s.getNode(a)
	if err != nil || n == nil {
		return xpage.EmptyAddress, err
	}
	return n.NextNode(), nil
}

// maxNodesPerDocument 是同文档链的步数上限，防住成环的链。
const maxNodesPerDocument = 1 << 16

// Prev0 返回第 0 层的前驱，也就是键序上紧邻的前一个。
func (n *Node) Prev0() xpage.Address { return n.node.Prev(0) }

// Next0 返回第 0 层的后继，也就是键序上紧邻的下一个。
func (n *Node) Next0() xpage.Address { return n.node.Next(0) }
