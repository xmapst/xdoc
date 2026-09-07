// Package xstore 把页组织成文档、跳表索引和向量图。
//
// 文档超过一页就切成若干数据块，用块内的下一块地址串起来；索引是一张跳表，
// 节点直接落在索引页里；向量索引是一张分层近邻图，走 HNSW 那一套。三者都靠
// 空闲链找可用的页：数据页按剩余空间分档，索引页只分「放得下」与「放不下」。
//
// 这里不碰事务、不碰日志、不碰并发——页从哪来、脏了怎么写回，都由 [Pages]
// 的实现方负责。
package xstore

import (
	"fmt"

	"github.com/xmapst/xdoc/internal/xpage"
)

// Pages 是 [Store] 对页来源的要求：取页、建页、删页，外加当前集合页与总页数。
//
// 实现方（事务层）负责脏页登记、写日志和并发控制；这里只管页里的结构。
type Pages interface {
	GetPage(id uint32) (*xpage.Page, error)

	NewPage(t xpage.PageType) (*xpage.Page, error)

	DeletePage(p *xpage.Page) error

	CollectionPage() *xpage.CollectionPage

	PageCount() uint32
}

// Store 是建在页之上的一层：文档块链、跳表索引、向量图都由它来摆布。
//
// 它只是 [Pages] 的一个薄壳，本身没有状态，按值传递即可。
type Store struct {
	Pages
}

// New 把一个页来源包装成 [Store]。
func New(p Pages) Store { return Store{Pages: p} }

// ChainLimit 给链式遍历定一个上界：总页数乘每页最多的条目数。
//
// 正常的链绝不会长过这个数，超过就说明链成环或者已经损坏。
// 页数为零时按一页算，免得上界退化成 0 把第一步就掐死。
func (s Store) ChainLimit() int {
	n := int(s.PageCount())
	if n <= 0 {
		n = 1
	}
	return n * xpage.MaxItems
}

// addFreeList 把一页插到双向链表头部，返回新的头页号。
//
// 页上还挂着前后指针就说明它已经在别的链里，硬插会把两条链搅在一起，
// 所以直接报损坏。
func (s Store) addFreeList(page *xpage.Page, head uint32) (uint32, error) {
	if page.PrevPageID() != xpage.EmptyPageID || page.NextPageID() != xpage.EmptyPageID {
		return 0, fmt.Errorf("%w: page %d is already linked into a list", xpage.ErrCorrupt, page.ID())
	}
	if head != xpage.EmptyPageID {
		h, err := s.GetPage(head)
		if err != nil {
			return 0, err
		}
		h.SetPrevPageID(page.ID())
	}
	page.SetPrevPageID(xpage.EmptyPageID)
	page.SetNextPageID(head)
	return page.ID(), nil
}

// removeFreeList 把一页从双向链表里摘下来，返回新的头页号。
//
// 摘掉的是头页时，头就顺延到它的后继。
func (s Store) removeFreeList(page *xpage.Page, head uint32) (uint32, error) {
	if prev := page.PrevPageID(); prev != xpage.EmptyPageID {
		pp, err := s.GetPage(prev)
		if err != nil {
			return 0, err
		}
		pp.SetNextPageID(page.NextPageID())
	}
	if next := page.NextPageID(); next != xpage.EmptyPageID {
		np, err := s.GetPage(next)
		if err != nil {
			return 0, err
		}
		np.SetPrevPageID(page.PrevPageID())
	}
	if head == page.ID() {
		head = page.NextPageID()
	}
	page.SetPrevPageID(xpage.EmptyPageID)
	page.SetNextPageID(xpage.EmptyPageID)
	return head, nil
}

// syncDataFreeList 按数据页当前的剩余空间，把它挪到该去的空闲链上。
//
// 档次没变且页上还有东西就什么都不做。变了就先从旧链摘下；此时若页已空，
// 整页回收，不再挂链。
func (s Store) syncDataFreeList(page *xpage.Page) error {
	cp := s.CollectionPage()
	newSlot := page.DataFreeSlot()
	old := page.PageListSlot()
	if newSlot == old && page.ItemsCount() > 0 {
		return nil
	}
	if old != xpage.EmptyIndex {
		h, err := s.removeFreeList(page, cp.FreeDataPage(old))
		if err != nil {
			return err
		}
		cp.SetFreeDataPage(old, h)
		page.SetPageListSlot(xpage.EmptyIndex)
	}
	if page.ItemsCount() == 0 {
		if err := s.DeletePage(page); err != nil {
			return err
		}
		return nil
	}
	h, err := s.addFreeList(page, cp.FreeDataPage(newSlot))
	if err != nil {
		return err
	}
	cp.SetFreeDataPage(newSlot, h)
	page.SetPageListSlot(newSlot)
	return nil
}

// getFreeDataPage 找一页装得下 length 字节的数据页，找不到就新建。
//
// 算上一个槽位的开销，从「刚好够用」那一档开始往空的方向找，先用满一点的页，
// 把空页留给更大的块。取到的页要复核两件事：它自称的档次与所在的链一致，
// 剩余空间确实够——对不上说明空闲链已经和页头脱节了。
func (s Store) getFreeDataPage(length int) (*xpage.Page, error) {
	cp := s.CollectionPage()

	need := length + xpage.SlotSize

	for slot := xpage.MinimumDataSlot(need); slot >= 0; slot-- {
		head := cp.FreeDataPage(uint8(slot))
		if head == xpage.EmptyPageID {
			continue
		}
		page, err := s.GetPage(head)
		if err != nil {
			return nil, err
		}

		if got := int(page.PageListSlot()); got != slot {
			return nil, fmt.Errorf("%w: page %d is on free list slot %d but its header says %d",
				xpage.ErrCorrupt, page.ID(), slot, got)
		}
		if page.FreeBytes() < need {
			return nil, fmt.Errorf("%w: free list slot %d handed out page %d with %d free bytes, need %d",
				xpage.ErrCorrupt, slot, page.ID(), page.FreeBytes(), need)
		}
		return page, nil
	}
	return s.NewPage(xpage.PageData)
}

// getFreeIndexPage 取索引空闲链的头页；链空就新建一页。
func (s Store) getFreeIndexPage(head uint32) (*xpage.Page, error) {
	if head == xpage.EmptyPageID {
		return s.NewPage(xpage.PageIndex)
	}
	return s.GetPage(head)
}

// syncIndexFreeList 按索引页当前的剩余空间决定它该不该留在空闲链上，返回新的头页号。
//
// 索引页只有「放得下一个最大节点」和「放不下」两档，所以这里只有上链、下链、
// 不动三种结果。页空了就摘链并整页回收。
func (s Store) syncIndexFreeList(page *xpage.Page, head uint32) (uint32, error) {
	onList := page.PageListSlot() == 0
	keep := page.IndexFreeSlot() == 0

	if page.ItemsCount() == 0 {
		if onList {
			var err error
			if head, err = s.removeFreeList(page, head); err != nil {
				return 0, err
			}
		}
		if err := s.DeletePage(page); err != nil {
			return 0, err
		}
		return head, nil
	}
	var err error
	switch {
	case onList && !keep:
		if head, err = s.removeFreeList(page, head); err != nil {
			return 0, err
		}
	case !onList && keep:
		if head, err = s.addFreeList(page, head); err != nil {
			return 0, err
		}
	}
	slot := uint8(1)
	if keep {
		slot = 0
	}
	page.SetPageListSlot(slot)
	return head, nil
}
