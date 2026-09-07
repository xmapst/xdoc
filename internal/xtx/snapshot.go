package xtx

import (
	"context"
	"fmt"

	"github.com/xmapst/xdoc/internal/xpage"
)

// Mode 是快照的读写模式。
type Mode uint8

const (
	// ModeRead 只读，不拿集合锁，页缓冲与缓存共享。
	ModeRead Mode = iota

	// ModeWrite 可写，要拿集合锁，页缓冲各自私有一份。
	ModeWrite
)

// localPage 是快照手里的一页。
type localPage struct {
	page *xpage.Page

	// shared 表示这块缓冲是缓存里那一份，不能改。
	//
	// 只读快照直接用缓存里的缓冲；可写快照另拷一份私有的。
	shared bool
}

// Snapshot 是某个事务对某个集合的一份视图。
//
// version 定住了能看到日志里哪一段：这个版本之后别人写的页，本快照看不见。
type Snapshot struct {
	tx         *Transaction
	name       string
	mode       Mode
	version    int64
	local      map[uint32]localPage
	collection *xpage.CollectionPage
}

// Snapshot 取事务里某个集合的快照，同一个集合复用同一份。
//
// 已有的只读快照被要求升级成可写时**整个重建**：只读时拿的是缓存里的共享缓冲，
// 改不得。集合不存在且 create 为真时顺带建出来。
//
// 拿集合锁失败、或者建集合失败，都会把已经拿到的锁放掉。
func (t *Transaction) Snapshot(ctx context.Context, name string, mode Mode, create bool) (*Snapshot, error) {
	if err := t.checkActive(); err != nil {
		return nil, err
	}

	key := xpage.FoldName(name)
	if s, ok := t.snapshots[key]; ok {
		switch {
		case mode == ModeWrite && s.mode == ModeRead:
			t.pages.size -= len(s.local)

			s.close(false)
			delete(t.snapshots, key)
		case create && s.collection == nil:
			if err := s.loadCollection(true); err != nil {
				return nil, err
			}
			return s, nil
		default:
			return s, nil
		}
	}

	locked := false
	if mode == ModeWrite {
		lctx, cancel := t.core.withTimeout(ctx)
		defer cancel()
		if err := t.core.locks.EnterCollections(lctx, t.id, name); err != nil {
			return nil, err
		}
		locked = true
	}

	ok := false
	defer func() {
		if !ok && locked {
			_ = t.core.locks.ExitCollection(t.id, name)
		}
	}()

	s := &Snapshot{
		tx:      t,
		name:    name,
		mode:    mode,
		version: t.core.wal.ReadVersion(),
		local:   map[uint32]localPage{},
	}
	if err := s.loadCollection(create); err != nil {
		return nil, err
	}
	t.snapshots[key] = s
	ok = true
	return s, nil
}

// Name 返回集合名。
func (s *Snapshot) Name() string { return s.name }

// Mode 返回读写模式。
func (s *Snapshot) Mode() Mode { return s.mode }

// Version 返回这份快照定住的日志版本。
func (s *Snapshot) Version() int64 { return s.version }

// CollectionPage 返回集合页；集合不存在时为 nil。
func (s *Snapshot) CollectionPage() *xpage.CollectionPage { return s.collection }

// PageCount 返回文件里一共有几页。
func (s *Snapshot) PageCount() uint32 {
	n, _ := s.tx.core.HeaderValue(func(h *xpage.HeaderPage) (uint32, error) {
		if last := h.LastPageID(); last != xpage.EmptyPageID {
			return last + 1, nil
		}
		return 0, nil
	})
	return n
}

// loadCollection 找到集合页；找不到且 create 为真时新建一个。
//
// 集合页从本地页表里**删掉**：它由 s.collection 单独持有，留在表里会被
// 当成普通页两遍处理。新建的集合要等提交时才写进头页的集合表，
// 所以登记的是一个提交回调。
func (s *Snapshot) loadCollection(create bool) error {
	var pageID uint32
	var found bool
	if err := s.tx.core.WithHeader(func(h *xpage.HeaderPage) error {
		pageID, found = h.Collection(s.name)
		return nil
	}); err != nil {
		return err
	}
	if found {
		p, _, err := s.getPage(pageID, false)
		if err != nil {
			return err
		}
		cp, err := xpage.LoadCollectionPage(p.Bytes())
		if err != nil {
			return err
		}
		s.collection = cp

		delete(s.local, pageID)
		return nil
	}
	if !create {
		return nil
	}
	if s.mode != ModeWrite {
		return fmt.Errorf("xtx: cannot create collection %q from a read-only snapshot", s.name)
	}

	if err := s.tx.InspectHeader(func(h *xpage.HeaderPage) error {
		return h.CanAddCollection(s.name)
	}); err != nil {
		return err
	}
	p, err := s.NewPage(xpage.PageCollection)
	if err != nil {
		return err
	}
	id := p.ID()
	cp, err := xpage.NewCollectionPage(p.Bytes(), id)
	if err != nil {
		return err
	}

	cp.SetColID(id)
	s.collection = cp
	delete(s.local, id)
	name := s.name
	s.tx.OnCommit(func(h *xpage.HeaderPage) error { return h.AddCollection(name, id) })
	return nil
}

// GetPage 取一页。
func (s *Snapshot) GetPage(id uint32) (*xpage.Page, error) {
	p, _, err := s.getPage(id, false)
	return p, err
}

// PageInfo 说明一页是从哪读来的，诊断用。
type PageInfo struct {
	Origin   Origin
	Position int64
	Version  int64
}

// GetPageInfo 取一页，连同它的来源信息。
func (s *Snapshot) GetPageInfo(id uint32) (*xpage.Page, PageInfo, error) {
	return s.getPage(id, false)
}

// getPage 按四级次序找一页：头页、本地改动、本事务写进日志的、日志里可见版本、数据文件。
//
// latest 为真时不用快照定住的版本，而是取当前最新的——分配新页要看真实的空页链，
// 不能看一份旧视图。
func (s *Snapshot) getPage(id uint32, latest bool) (*xpage.Page, PageInfo, error) {
	if id == 0 {
		return s.tx.core.header.Page, PageInfo{}, nil
	}
	if lp, ok := s.local[id]; ok {
		return lp.page, PageInfo{}, nil
	}

	if off, ok := s.tx.pages.dirty[id]; ok {
		p, err := s.readLog(id, off, true)
		return p, PageInfo{Origin: OriginLog, Position: off, Version: s.version}, err
	}

	version := s.version
	if latest {
		version = s.tx.core.wal.ReadVersion()
	}
	if off, ver, ok := s.tx.core.wal.LookupVersion(id, version); ok {
		p, err := s.readLog(id, off, false)
		return p, PageInfo{Origin: OriginLog, Position: off, Version: ver}, err
	}

	p, err := s.readData(id)
	return p, PageInfo{Origin: OriginData, Position: int64(id) * xpage.PageSize}, err
}

// readLog 从日志某个偏移读一页，先查缓存。
//
// 读回来核对页号：对不上说明日志索引与日志本身脱节了。页的结构也要校验一遍。
func (s *Snapshot) readLog(id uint32, off int64, own bool) (*xpage.Page, error) {
	if buf, ok := s.tx.core.cache.Get(OriginLog, off); ok {
		return s.adopt(id, buf, true)
	}
	buf := getBuf()
	if err := s.tx.core.disk.ReadLogPage(off, buf); err != nil {
		putBuf(buf)
		return nil, err
	}
	if got := xpage.PeekPageID(buf); got != id {
		putBuf(buf)
		return nil, fmt.Errorf("%w: log offset %d holds page %d, wanted %d",
			xpage.ErrCorrupt, off, got, id)
	}

	if _, err := xpage.Load(buf); err != nil {
		putBuf(buf)
		return nil, fmt.Errorf("log page at %d: %w", off, err)
	}
	s.tx.core.cache.Put(OriginLog, off, buf)
	return s.adopt(id, buf, true)
}

// readData 从数据文件读一页，先查缓存。
func (s *Snapshot) readData(id uint32) (*xpage.Page, error) {
	pos := int64(id) * xpage.PageSize
	if buf, ok := s.tx.core.cache.Get(OriginData, pos); ok {
		return s.adopt(id, buf, false)
	}
	buf := getBuf()
	if err := s.tx.core.disk.ReadDataPage(id, buf); err != nil {
		putBuf(buf)
		return nil, err
	}

	if _, err := xpage.Load(buf); err != nil {
		putBuf(buf)
		return nil, fmt.Errorf("data page %d: %w", id, err)
	}
	s.tx.core.cache.Put(OriginData, pos, buf)
	return s.adopt(id, buf, false)
}

// adopt 把一块页缓冲收进本地页表。
//
// 只读快照直接用缓存里那块；可写快照拷一份私有的，并把从日志读来的页上的
// 事务标记清掉——那是上一个事务留下的，本事务要重新打。
//
// 本地页太多时先清一遍干净页，脏页不能丢。
func (s *Snapshot) adopt(id uint32, buf []byte, fromLog bool) (*xpage.Page, error) {
	shared := s.mode == ModeRead
	if !shared {
		private := getBuf()
		copy(private, buf)
		buf = private
	}

	p, err := xpage.Attach(buf)
	if err != nil {
		if !shared {
			putBuf(buf)
		}
		return nil, fmt.Errorf("page %d: %w", id, err)
	}
	if fromLog && !shared {
		p.ClearTransactionMark()
	}
	if len(s.local) >= maxLocalPages {
		s.evictClean()
	}
	s.local[id] = localPage{page: p, shared: shared}
	s.tx.pages.size++
	return p, nil
}

// maxLocalPages 是一份快照攒到多少页就该清一清干净页。
const maxLocalPages = 1000

// evictClean 丢掉本地页表里所有没改过的页。脏页必须留着，它们还没落盘。
func (s *Snapshot) evictClean() {
	n := 0
	for id, lp := range s.local {
		if lp.page.Dirty() {
			continue
		}
		delete(s.local, id)
		n++
	}
	s.tx.pages.size -= n
	if s.tx.pages.size < 0 {
		s.tx.pages.size = 0
	}
}

// NewPage 分配一页：优先从空页链取，链空了才往文件末尾扩。
//
// 从空页链取时会核对它确实是空页——不是就说明链已经坏了。
// 往后扩要先看会不会超过库的大小上限。新页归本快照的集合所有。
func (s *Snapshot) NewPage(t xpage.PageType) (*xpage.Page, error) {
	if s.mode != ModeWrite {
		return nil, fmt.Errorf("xtx: cannot allocate a page from a read-only snapshot")
	}
	s.tx.core.headerMu.Lock()
	defer s.tx.core.headerMu.Unlock()
	h := s.tx.core.header

	var buf []byte
	var id uint32
	if free := h.FreeEmptyPageList(); free != xpage.EmptyPageID {
		p, _, err := s.getPage(free, true)
		if err != nil {
			return nil, err
		}
		if p.Type() != xpage.PageEmpty {
			return nil, fmt.Errorf("%w: page %d is on the free list but has type %s",
				xpage.ErrCorrupt, free, p.Type())
		}
		h.SetFreeEmptyPageList(p.NextPageID())
		id = free
		buf = p.Bytes()
		delete(s.local, free)
	} else {
		next := h.LastPageID() + 1
		if int64(next+1)*xpage.PageSize > h.LimitSize() {
			return nil, fmt.Errorf("xtx: database would exceed its size limit of %d bytes",
				h.LimitSize())
		}
		h.SetLastPageID(next)
		id = next
		buf = getBuf()
	}
	s.tx.headerChanged = true

	p, err := t.NewPage(buf, id)
	if err != nil {
		return nil, err
	}
	if s.collection != nil {
		p.SetColID(s.collection.ID())
	}

	s.tx.pages.newPages = append(s.tx.pages.newPages, id)
	s.local[id] = localPage{page: p}
	s.tx.pages.size++
	return p, nil
}

// DeletePage 删一页，把它接进本事务的待删链。
//
// 删之前要求这一页已经从各条链上摘干净、里面也没有数据了——不满足说明
// 调用方漏了一步，此时删掉会让别的链断在半路。真正接进空页链要等提交，
// 见 [Transaction.spliceDeleted]。
func (s *Snapshot) DeletePage(p *xpage.Page) error {
	if p.PrevPageID() != xpage.EmptyPageID || p.NextPageID() != xpage.EmptyPageID {
		return fmt.Errorf("%w: page %d is still linked (prev=%d next=%d) when deleted",
			xpage.ErrCorrupt, p.ID(), p.PrevPageID(), p.NextPageID())
	}
	if p.ItemsCount() != 0 || p.UsedBytes() != 0 || p.FragmentedBytes() != 0 {
		return fmt.Errorf("%w: page %d still holds data when deleted (items=%d used=%d frag=%d)",
			xpage.ErrCorrupt, p.ID(), p.ItemsCount(), p.UsedBytes(), p.FragmentedBytes())
	}
	p.MarkEmpty()
	if s.tx.pages.firstDelete == xpage.EmptyPageID {
		s.tx.pages.firstDelete = p.ID()
		s.tx.pages.lastDelete = p.ID()
		s.tx.pages.lastDeleteColl = s.name
	} else {
		p.SetNextPageID(s.tx.pages.firstDelete)
		s.tx.pages.firstDelete = p.ID()
	}
	s.tx.pages.deleted++
	s.tx.headerChanged = true
	return nil
}

// DiscardVectorPages 扫过整个文件，删掉本集合所有向量索引页，返回删了几页。
//
// 这是一趟全库扫描，所以每页给一次设安全点的机会，让长事务能腾出内存、
// 也能被取消。只看本快照看得见的页，见 [Snapshot.canSee]。
func (s *Snapshot) DiscardVectorPages(safepoint func() error) (int, error) {
	if s.mode != ModeWrite {
		return 0, fmt.Errorf("xtx: cannot discard pages from a read-only snapshot")
	}
	if s.collection == nil {
		return 0, nil
	}
	colID := s.collection.ID()
	dataPages, err := s.tx.core.disk.DataPageCount()
	if err != nil {
		return 0, err
	}
	n := 0
	for id := uint32(1); id < s.PageCount(); id++ {
		if safepoint != nil {
			if err := safepoint(); err != nil {
				return n, err
			}
		}
		if !s.canSee(id, dataPages) {
			continue
		}
		p, _, err := s.getPage(id, false)
		if err != nil {
			return n, err
		}

		if p.Type() != xpage.PageVectorIndex || p.ColID() != colID {
			continue
		}

		p.MarkEmpty()
		if err := s.DeletePage(p); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// canSee 判断某个页号在本快照里存不存在。
//
// 本地改过的、本事务写进日志的、日志里可见版本有的，都算存在；
// 否则要看它在不在数据文件的范围内。没这一道判断，扫描会去读文件末尾之外的页。
func (s *Snapshot) canSee(id, dataPages uint32) bool {
	if _, ok := s.local[id]; ok {
		return true
	}
	if _, ok := s.tx.pages.dirty[id]; ok {
		return true
	}
	if _, ok := s.tx.core.wal.Lookup(id, s.version); ok {
		return true
	}
	return id < dataPages
}

// close 关掉快照。
//
// reuse 为真时把私有的干净页缓冲还回池里；脏页不能还——它们的内容
// 可能还挂在别处（比如日志缓存）。
func (s *Snapshot) close(reuse bool) {
	if reuse {
		for _, lp := range s.local {
			if !lp.shared && !lp.page.Dirty() {
				putBuf(lp.page.Bytes())
			}
		}
	}
	clear(s.local)
	s.collection = nil
}
