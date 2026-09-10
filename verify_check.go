package xdoc

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"maps"
	"slices"
	"strconv"
	"unicode/utf8"

	"github.com/xmapst/xdoc/internal/xbexpr"
	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xcoll"
	"github.com/xmapst/xdoc/internal/xengine"
	"github.com/xmapst/xdoc/internal/xpage"
	"github.com/xmapst/xdoc/internal/xstore"
	"github.com/xmapst/xdoc/internal/xtx"
)

const (
	// verifyIssueCap 是同一集合、同一索引、同一类问题最多逐条记几条，多出来的只计数。
	verifyIssueCap = 100

	// verifyPollEvery 是每走多少步问一次上下文有没有被取消。
	verifyPollEvery = 64

	// verifyMaxBlocks 是一篇文档的块链最多几块，与读文档时的上界一致。
	verifyMaxBlocks = xpage.MaxDocumentSize/xpage.MaxDataBytesPerPage + 2

	// verifyScanName 是校验用的快照名。以 \x00 打头，不会与真实集合重名。
	verifyScanName = "\x00verify"
)

// verifyGroup 是给问题计数分组的键：同一集合、同一索引、同一类。
type verifyGroup struct{ coll, index, kind string }

// verifyVectorNode 是扫描时记下的一个向量节点：它在哪、指向哪篇文档。
type verifyVectorNode struct{ addr, block xpage.Address }

// verifyCollection 是正在校验的那个集合。
type verifyCollection struct {
	name string
	cp   *xpage.CollectionPage

	// multi 按索引名记着这个集合的多键索引。
	multi map[string]*verifyMultiKey
}

// verifyMultiKey 是一条多键索引在校验里的账：各篇文档按写入时的规则应有几个键。
type verifyMultiKey struct {
	ix xpage.CollectionIndex

	// want 是各篇文档应有的键数之和。unknown 为真表示有文档读不出来或求不出键，
	// 说不准应有几个节点，这条索引就不核对条数。
	want    int
	unknown bool
}

// verifyList 描述一条要顺着后继指针走的页链。
type verifyList struct {
	// coll、index 是问题记在谁名下，what 是链的名字，owner 是链上的页归谁。
	coll, index, what, owner string

	// from 是链头记在哪一页上，出问题时报在那里。
	from uint32

	// want 是链上的页该有的类型；不是空页时还要求它属于 colID 这个集合页。
	want  xpage.PageType
	colID uint32

	// slot 是链上的页头该记着的档次，负数表示不查。
	slot int
}

// verifier 是一次校验的全部状态。
//
// 所有页都从同一个只读快照里读，头页则按快照的版本另读一份：内存里那份头页会被
// 还没提交的写事务先改掉（新占的页号、摘下的空页），拿它当依据会把别人写到一半的
// 状态误报成损坏。
type verifier struct {
	ctx context.Context

	// eng 给多键索引求键，走的是写入时的同一段代码。
	eng  *xengine.Engine
	core *xtx.Core
	snap *xtx.Snapshot
	coll xcoll.Collation
	rep  *VerifyReport

	// pages 是快照版本下已分配的页数，dataPages 是数据文件里实有的页数。
	pages, dataPages uint32

	// dataByCol、vectorByCol 按页头记着的所属集合页把数据页、向量索引页归拢起来。
	dataByCol, vectorByCol map[uint32][]uint32

	// owners 是每一页的属主编号，0 表示还没人认领；ownerNames 按编号存属主名。
	owners     []int32
	ownerNames []string
	ownerIDs   map[string]int32
	conflicts  map[uint32]bool

	groups map[verifyGroup]int
	steps  int

	// err 是让校验做不下去的错误：上下文取消，或者库被关掉。
	err error
}

// newVerifier 在一个只读快照上准备一次校验。
func newVerifier(ctx context.Context, eng *xengine.Engine, snap *xtx.Snapshot) *verifier {
	return &verifier{
		ctx:         ctx,
		eng:         eng,
		core:        eng.Core(),
		snap:        snap,
		coll:        eng.Collation(),
		rep:         &VerifyReport{},
		dataByCol:   map[uint32][]uint32{},
		vectorByCol: map[uint32][]uint32{},
		ownerNames:  []string{""},
		ownerIDs:    map[string]int32{},
		conflicts:   map[uint32]bool{},
		groups:      map[verifyGroup]int{},
	}
}

// run 依次做完各项检查。
//
// 文件里的问题一律记进报告；error 只在校验本身做不下去时返回。
func (v *verifier) run() (*VerifyReport, error) {
	h, err := v.header()
	if err != nil {
		v.issue(0, "", "", VerifyKindPage, fmt.Sprintf("header page cannot be read: %v", err))
		return v.rep, nil
	}
	if v.dataPages, err = v.core.Disk().DataPageCount(); err != nil {
		return nil, err
	}
	if last := h.LastPageID(); last != xpage.EmptyPageID {
		v.pages = last + 1
	}
	v.rep.Pages = int(v.pages)
	v.owners = make([]int32, v.pages)
	v.claim(0, "", "", "the header")

	v.scanPages()
	v.walkList(h.FreeEmptyPageList(), verifyList{what: "the free page list", owner: "the free page list", want: xpage.PageEmpty, slot: -1})
	known := map[uint32]bool{}
	for name, id := range h.Collections() {
		if v.stopped() {
			break
		}
		v.rep.Collections++
		known[id] = true
		v.checkCollection(name, id)
	}
	v.checkOrphans(known)
	if v.err != nil {
		return nil, v.err
	}
	return v.rep, nil
}

// header 读出快照版本下的头页：日志里有不晚于这个版本的头页就用它，否则用数据文件里那份。
func (v *verifier) header() (*xpage.HeaderPage, error) {
	buf := make([]byte, xpage.PageSize)
	disk := v.core.Disk()
	if off, ok := v.core.WAL().Lookup(0, v.snap.Version()); ok {
		if err := disk.ReadLogPage(off, buf); err != nil {
			return nil, err
		}
	} else if err := disk.ReadDataPage(0, buf); err != nil {
		return nil, err
	}
	return xpage.LoadHeaderPage(buf)
}

// stopped 报告校验该不该停下。每走 verifyPollEvery 步才真去问一次上下文。
func (v *verifier) stopped() bool {
	if v.err != nil {
		return true
	}
	if v.steps++; v.steps%verifyPollEvery == 0 {
		v.err = v.ctx.Err()
	}
	return v.err != nil
}

// page 从快照里取一页。
//
// 库被关掉时把错误记成校验的错误：否则往后每一页都会被报成读不出来。
func (v *verifier) page(id uint32) (*xpage.Page, error) {
	p, err := v.snap.GetPage(id)
	if errors.Is(err, xtx.ErrClosed) && v.err == nil {
		v.err = err
	}
	return p, err
}

// typedPage 取一页，并核对它是 colID 这个集合的 want 类页。
func (v *verifier) typedPage(colID, id uint32, want xpage.PageType) (*xpage.Page, error) {
	if id == 0 || id >= v.pages {
		return nil, fmt.Errorf("page %d is not among the allocated pages 1..%d", id, v.pages-1)
	}
	p, err := v.page(id)
	if err != nil {
		return nil, err
	}
	if p.Type() != want || p.ColID() != colID {
		return nil, fmt.Errorf("page %d is a %s page of collection page %d, want a %s page of collection page %d",
			id, p.Type(), p.ColID(), want, colID)
	}
	return p, nil
}

// issue 记一处问题；同组超过 verifyIssueCap 条之后只计数。校验已经做不下去时不再记。
func (v *verifier) issue(page uint32, coll, index, kind, msg string) {
	if v.err != nil {
		return
	}
	g := verifyGroup{coll, index, kind}
	if v.groups[g] >= verifyIssueCap {
		v.rep.Omitted++
		return
	}
	v.groups[g]++
	v.rep.Issues = append(v.rep.Issues, VerifyIssue{PageID: page, Collection: coll, Index: index, Kind: kind, Message: msg})
}

// claim 让 owner 认领一页；已经归了别人就报一次冲突，同一页只报一次。
func (v *verifier) claim(id uint32, coll, index, owner string) {
	if id >= v.pages {
		return
	}
	o, ok := v.ownerIDs[owner]
	if !ok {
		o = int32(len(v.ownerNames))
		v.ownerNames = append(v.ownerNames, owner)
		v.ownerIDs[owner] = o
	}
	switch cur := v.owners[id]; {
	case cur == 0:
		v.owners[id] = o
	case cur != o && !v.conflicts[id]:
		v.conflicts[id] = true
		v.issue(id, coll, index, VerifyKindPageOwner,
			fmt.Sprintf("page %d is used by both %s and %s", id, v.ownerNames[cur], owner))
	}
}

// scanPages 把第 1 页到末页逐页读一遍查页头，顺带按所属集合把数据页、向量索引页归拢起来。
//
// 日志里没有、又落在数据文件之外的页跳过：那是写事务先占下的页号，还没写出去过，
// 在快照里本来就不存在。
func (v *verifier) scanPages() {
	wal, version := v.core.WAL(), v.snap.Version()
	for id := uint32(1); id < v.pages; id++ {
		if v.stopped() {
			return
		}
		if _, ok := wal.Lookup(id, version); !ok && id >= v.dataPages {
			continue
		}
		p, err := v.page(id)
		if err != nil {
			v.issue(id, "", "", VerifyKindPage, fmt.Sprintf("page %d cannot be read: %v", id, err))
			continue
		}
		t := p.Type()
		switch {
		case p.ID() != id:
			v.issue(id, "", "", VerifyKindPage, fmt.Sprintf("page %d says it is page %d", id, p.ID()))
			continue
		case t == xpage.PageHeader:
			v.issue(id, "", "", VerifyKindPage, fmt.Sprintf("page %d is marked as a header page; only page 0 may be", id))
			continue
		case t > xpage.PageVectorIndex:
			v.issue(id, "", "", VerifyKindPage, fmt.Sprintf("page %d has unknown page type %d", id, uint8(t)))
			continue
		case t == xpage.PageEmpty && p.ItemsCount() != 0:
			v.issue(id, "", "", VerifyKindPage, fmt.Sprintf("empty page %d still holds %d items", id, p.ItemsCount()))
		}
		switch t {
		case xpage.PageData:
			v.dataByCol[p.ColID()] = append(v.dataByCol[p.ColID()], id)
		case xpage.PageVectorIndex:
			v.vectorByCol[p.ColID()] = append(v.vectorByCol[p.ColID()], id)
		}
	}
}

// walkList 顺着后继指针走一条页链：查环、查越界，查链上每一页的类型、所属与档次。
//
// 碰到类型或所属不对的页就停：那一页已经不是这条链上的东西，再往下走的是别的链。
func (v *verifier) walkList(head uint32, l verifyList) {
	seen := map[uint32]bool{}
	from := l.from
	for id := head; id != xpage.EmptyPageID; {
		if v.stopped() {
			return
		}
		if id == 0 || id >= v.pages {
			v.issue(from, l.coll, l.index, VerifyKindFreeList, fmt.Sprintf(
				"%s reaches page %d from page %d, but only pages 1..%d can be on it", l.what, id, from, v.pages-1))
			return
		}
		if seen[id] {
			v.issue(from, l.coll, l.index, VerifyKindFreeList, fmt.Sprintf(
				"%s loops back from page %d to page %d", l.what, from, id))
			return
		}
		seen[id] = true
		p, err := v.page(id)
		if err != nil {
			v.issue(id, l.coll, l.index, VerifyKindFreeList, fmt.Sprintf(
				"%s reaches page %d, which cannot be read: %v", l.what, id, err))
			return
		}
		v.claim(id, l.coll, l.index, l.owner)
		switch {
		case p.Type() != l.want:
			v.issue(id, l.coll, l.index, VerifyKindFreeList, fmt.Sprintf(
				"%s holds page %d, a %s page, want %s", l.what, id, p.Type(), l.want))
			return
		case l.want != xpage.PageEmpty && p.ColID() != l.colID:
			v.issue(id, l.coll, l.index, VerifyKindFreeList, fmt.Sprintf(
				"%s holds page %d of collection page %d, want %d", l.what, id, p.ColID(), l.colID))
			return
		case l.slot >= 0 && int(p.PageListSlot()) != l.slot:
			v.issue(id, l.coll, l.index, VerifyKindFreeList, fmt.Sprintf(
				"%s holds page %d, whose header says it belongs on list slot %d, want %d",
				l.what, id, p.PageListSlot(), l.slot))
		}
		from, id = id, p.NextPageID()
	}
}

// checkCollection 校验一个集合：集合页、数据页分档链、文档、跳表索引与向量索引。
func (v *verifier) checkCollection(name string, id uint32) {
	if id == 0 || id >= v.pages {
		v.issue(0, name, "", VerifyKindCollection, fmt.Sprintf(
			"the collection table maps %q to page %d, outside the allocated pages 1..%d", name, id, v.pages-1))
		return
	}
	v.claim(id, name, "", "collection "+strconv.Quote(name))
	p, err := v.page(id)
	if err != nil {
		v.issue(id, name, "", VerifyKindCollection, fmt.Sprintf("collection page %d cannot be read: %v", id, err))
		return
	}
	cp, err := xpage.LoadCollectionPage(p.Bytes())
	if err != nil {
		v.issue(id, name, "", VerifyKindCollection, fmt.Sprintf("collection page %d cannot be loaded: %v", id, err))
		return
	}
	c := &verifyCollection{name: name, cp: cp, multi: map[string]*verifyMultiKey{}}
	for i, ix := range cp.Indexes() {
		if ix.Kind == xpage.IndexSkipList && !verifyPrimary(i, ix) && !verifyOneKeyPerDocument(ix.Expression) {
			c.multi[ix.Name] = &verifyMultiKey{ix: ix}
		}
	}
	owner := "data pages of " + strconv.Quote(name)
	for slot := range xpage.DataFreeSlotCount {
		v.walkList(cp.FreeDataPage(uint8(slot)), verifyList{
			coll: name, what: fmt.Sprintf("free data page list %d of %q", slot, name), owner: owner,
			from: id, want: xpage.PageData, colID: id, slot: slot,
		})
	}
	nodes, externals := v.scanVectorNodes(c)
	docs := v.scanDocuments(c, owner, externals)
	for i, ix := range cp.Indexes() {
		switch ix.Kind {
		case xpage.IndexSkipList:
			v.checkIndex(c, ix, verifyPrimary(i, ix), docs)
		case xpage.IndexVector:
		default:
			v.issue(id, name, ix.Name, VerifyKindCollection, fmt.Sprintf("index %q has unknown kind %d", ix.Name, ix.Kind))
		}
	}
	v.checkVectors(c, nodes, docs)
}

// verifySlots 遍历一页上可能在用的槽号。
//
// 空槽取出来会报错，调用方跳过即可：页读进来时已经整页校验过，在用的槽不会取不出来。
func verifySlots(p *xpage.Page) iter.Seq[uint8] {
	return func(yield func(uint8) bool) {
		hi := p.HighestIndex()
		if hi == xpage.EmptyIndex {
			return
		}
		for i := 0; i <= int(hi); i++ {
			if !yield(uint8(i)) {
				return
			}
		}
	}
}

// scanVectorNodes 扫出这个集合所有向量索引页上的节点，连同外存向量占着的数据块。
//
// 外存向量是按文档的写法存进数据页的一串浮点字节，扫文档时要把它们认出来跳过。
func (v *verifier) scanVectorNodes(c *verifyCollection) ([]verifyVectorNode, map[xpage.Address]bool) {
	var nodes []verifyVectorNode
	externals := map[xpage.Address]bool{}
	for _, id := range v.vectorByCol[c.cp.ID()] {
		if v.stopped() {
			break
		}
		p, err := v.page(id)
		if err != nil {
			continue
		}
		for i := range verifySlots(p) {
			seg, err := p.Get(i)
			if err != nil {
				continue
			}
			addr := xpage.Address{PageID: id, Index: i}
			n, err := xpage.AsVectorNode(seg)
			if err != nil {
				v.issue(id, c.name, "", VerifyKindIndexLink, fmt.Sprintf("vector node %s cannot be read: %v", addr, err))
				continue
			}
			nodes = append(nodes, verifyVectorNode{addr: addr, block: n.DataBlock()})
			if ext := n.ExternalVector(); !ext.IsEmpty() {
				externals[ext] = true
			}
		}
	}
	return nodes, externals
}

// scanDocuments 扫这个集合的每一页数据页，找出每篇文档的首块，顺着块链读完再解码。
//
// 返回首块地址的集合，索引节点指向的必须是其中之一。解不开的文档也算在内——
// 它确实在那里，只是坏了，已经按 [VerifyKindDocument] 报过。
func (v *verifier) scanDocuments(c *verifyCollection, owner string, externals map[xpage.Address]bool) map[xpage.Address]bool {
	docs := map[xpage.Address]bool{}
	var buf []byte
	for _, id := range v.dataByCol[c.cp.ID()] {
		v.claim(id, c.name, "", owner)
		p, err := v.page(id)
		if err != nil {
			continue
		}
		for i := range verifySlots(p) {
			if v.stopped() {
				return docs
			}
			seg, err := p.Get(i)
			if err != nil {
				continue
			}
			addr := xpage.Address{PageID: id, Index: i}
			blk, err := xpage.AsDataBlock(seg)
			if err != nil {
				v.issue(id, c.name, "", VerifyKindDocument, fmt.Sprintf("data block %s cannot be read: %v", addr, err))
				continue
			}
			if blk.Extend() || externals[addr] {
				continue
			}
			docs[addr] = true
			v.rep.Documents++
			var doc *xbson.Document
			if buf, err = v.readDocument(c, addr, buf); err != nil {
				v.issue(id, c.name, "", VerifyKindDocument, fmt.Sprintf("document at %s: %v", addr, err))
			} else if doc, err = xbson.DecodeIn(buf, v.eng.DateLocation()); err != nil {
				v.issue(id, c.name, "", VerifyKindDocument, fmt.Sprintf("document at %s cannot be decoded: %v", addr, err))
			}
			v.countKeys(c, doc)
		}
	}
	return docs
}

// verifyPrimary 报告索引表里第 i 条是不是主键索引。
func verifyPrimary(i int, ix xpage.CollectionIndex) bool {
	return i == 0 || ix.Name == xengine.PrimaryIndexName
}

// countKeys 把一篇文档在各条多键索引里应有的键数记到账上；doc 为空表示这篇读不出来。
//
// 求键走引擎写入时的同一段代码，判重规则、排序规则与 UTC_DATE 都与写入时一致。
// 键数一记下键就丢掉，内存里只留一个数。
func (v *verifier) countKeys(c *verifyCollection, doc *xbson.Document) {
	for _, mk := range c.multi {
		if mk.unknown {
			continue
		}
		if doc == nil {
			mk.unknown = true
			continue
		}
		keys, err := v.eng.IndexKeys(&mk.ix, doc.Value())
		if err != nil {
			mk.unknown = true
			continue
		}
		mk.want += len(keys)
	}
}

// eachDocument 按数据页的次序把这个集合每篇读得出来的文档交给 fn；读不出来的已经报过，跳过。
func (v *verifier) eachDocument(c *verifyCollection, docs map[xpage.Address]bool, fn func(xpage.Address, *xbson.Document)) {
	for _, id := range v.dataByCol[c.cp.ID()] {
		p, err := v.page(id)
		if err != nil {
			continue
		}
		for i := range verifySlots(p) {
			if v.stopped() {
				return
			}
			addr := xpage.Address{PageID: id, Index: i}
			if !docs[addr] {
				continue
			}
			// 每篇另起一段缓冲：交出去的文档可能还引用着这些字节。
			raw, err := v.readDocument(c, addr, nil)
			if err != nil {
				continue
			}
			if doc, err := xbson.DecodeIn(raw, v.eng.DateLocation()); err == nil {
				fn(addr, doc)
			}
		}
	}
}

// verifyDocID 把文档的主键写成进消息的短文本。
func verifyDocID(doc *xbson.Document) string {
	if id := doc.Get(xengine.IDField); id != nil {
		return verifyKeyText(id)
	}
	return "(none)"
}

// readDocument 顺着块链把一篇文档的字节拼起来。
//
// 比常规读取多查两件事：每一块都得落在这个集合的数据页上，首块之后的每一块都得
// 标着续接——块链串到别的集合或者别的文档身上时，常规读取照样拼得出一串字节。
func (v *verifier) readDocument(c *verifyCollection, first xpage.Address, dst []byte) ([]byte, error) {
	dst = dst[:0]
	addr := first
	for hops := 0; !addr.IsEmpty(); hops++ {
		if hops >= verifyMaxBlocks {
			return dst, fmt.Errorf("block chain is longer than %d blocks", verifyMaxBlocks)
		}
		p, err := v.typedPage(c.cp.ID(), addr.PageID, xpage.PageData)
		if err != nil {
			return dst, fmt.Errorf("block %d at %s: %w", hops, addr, err)
		}
		blk, err := p.GetDataBlock(addr.Index)
		if err != nil {
			return dst, fmt.Errorf("block %d at %s: %w", hops, addr, err)
		}
		if hops > 0 && !blk.Extend() {
			return dst, fmt.Errorf("block %d at %s is the head of another document", hops, addr)
		}
		dst = append(dst, blk.Data()...)
		addr = blk.NextBlock()
	}
	return dst, nil
}

// verifyOneKeyPerDocument 报告一条索引的表达式是不是每篇文档正好取出一个键。
//
// 解析不了的表达式当它不是：那时说不准它该有几个节点，宁可不核对条数。
func verifyOneKeyPerDocument(expr string) bool {
	n, err := xbexpr.Parse(expr)
	return err == nil && n.Cardinality() == xbexpr.Scalar
}

// verifyKeyText 把一个索引键写成进消息的短文本，太长的截断。
func verifyKeyText(k *xbson.Value) string {
	const limit = 64
	s := k.String()
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// checkIndex 校验一条跳表索引。
//
// 先沿第 0 层从头走到尾：查环、查前驱指针、查槽号、查键按库的排序规则递增
// （唯一索引严格递增）、查每个节点指向的文档确实在。第 0 层走通了再查各高层链，
// 最后核对节点数——主键索引与单键索引每篇文档正好一个节点，多键索引对的是各篇应有的键数之和。
func (v *verifier) checkIndex(c *verifyCollection, ix xpage.CollectionIndex, primary bool, docs map[xpage.Address]bool) {
	colID := c.cp.ID()
	owner := "index " + strconv.Quote(c.name) + "." + strconv.Quote(ix.Name)
	v.walkList(ix.FreeIndexPageList, verifyList{
		coll: c.name, index: ix.Name, what: fmt.Sprintf("free page list of index %q", ix.Name), owner: owner,
		from: colID, want: xpage.PageIndex, colID: colID, slot: 0,
	})
	link := func(page uint32, format string, args ...any) {
		v.issue(page, c.name, ix.Name, VerifyKindIndexLink, fmt.Sprintf(format, args...))
	}

	head, err := v.indexNode(colID, ix.Head)
	if err != nil {
		link(colID, "head node %s cannot be read: %v", ix.Head, err)
		return
	}
	tail, err := v.indexNode(colID, ix.Tail)
	if err != nil {
		link(colID, "tail node %s cannot be read: %v", ix.Tail, err)
		return
	}
	v.claim(ix.Head.PageID, c.name, ix.Name, owner)
	v.claim(ix.Tail.PageID, c.name, ix.Name, owner)
	if k, err := head.Key(); err != nil || k.Type() != xbson.TypeMinValue {
		link(ix.Head.PageID, "head node %s does not carry the minimum key", ix.Head)
	}
	if k, err := tail.Key(); err != nil || k.Type() != xbson.TypeMaxValue {
		link(ix.Tail.PageID, "tail node %s does not carry the maximum key", ix.Tail)
	}

	pos := map[xpage.Address]int{ix.Head: 0}
	prev, key := new(xbson.Value), new(xbson.Value)
	prevAddr, havePrev, count := ix.Head, false, 0
	for cur := head.Next(0); cur != ix.Tail; {
		if v.stopped() {
			return
		}
		if cur.IsEmpty() {
			link(prevAddr.PageID, "level 0 ends at %s without reaching the tail node %s", prevAddr, ix.Tail)
			return
		}
		if _, ok := pos[cur]; ok {
			link(prevAddr.PageID, "level 0 loops back from %s to %s", prevAddr, cur)
			return
		}
		n, err := v.indexNode(colID, cur)
		if err != nil {
			link(prevAddr.PageID, "node %s after %s cannot be read: %v", cur, prevAddr, err)
			return
		}
		v.claim(cur.PageID, c.name, ix.Name, owner)
		count++
		pos[cur] = count
		if n.Slot() != ix.Slot {
			link(cur.PageID, "node %s carries index slot %d, the index uses slot %d", cur, n.Slot(), ix.Slot)
		}
		if back := n.Prev(0); back != prevAddr {
			link(cur.PageID, "node %s points back to %s, but level 0 reaches it from %s", cur, back, prevAddr)
		}
		if _, err := key.ReadIndexKeyInto(n.KeyBytes()); err != nil {
			link(cur.PageID, "node %s has a key that cannot be read: %v", cur, err)
			havePrev = false
		} else {
			if havePrev {
				switch d := prev.Compare(key, v.coll); {
				case d > 0:
					v.issue(cur.PageID, c.name, ix.Name, VerifyKindIndexOrder, fmt.Sprintf(
						"node %s has key %s, which sorts before the previous key %s",
						cur, verifyKeyText(key), verifyKeyText(prev)))
				case d == 0 && ix.Unique:
					v.issue(cur.PageID, c.name, ix.Name, VerifyKindIndexOrder, fmt.Sprintf(
						"node %s repeats key %s in a unique index", cur, verifyKeyText(key)))
				}
			}
			prev, key = key, prev
			havePrev = true
		}
		if block := n.DataBlock(); !docs[block] {
			v.issue(cur.PageID, c.name, ix.Name, VerifyKindDangling, fmt.Sprintf(
				"node %s points at %s, where there is no document", cur, block))
		}
		prevAddr, cur = cur, n.Next(0)
	}
	if back := tail.Prev(0); back != prevAddr {
		link(ix.Tail.PageID, "tail node %s points back to %s, but level 0 reaches it from %s", ix.Tail, back, prevAddr)
	}

	v.checkLevels(c, ix, head, tail, pos)
	if mk, ok := c.multi[ix.Name]; ok {
		if !mk.unknown && count != mk.want {
			v.issue(colID, c.name, ix.Name, VerifyKindIndexCount, fmt.Sprintf(
				"index %q has %d nodes but the documents produce %d keys", ix.Name, count, mk.want))
			v.locateKeys(c, mk, docs, count)
		}
	} else if want := len(docs); count != want && (primary || verifyOneKeyPerDocument(ix.Expression)) {
		v.issue(colID, c.name, ix.Name, VerifyKindIndexCount, fmt.Sprintf(
			"index %q has %d nodes but the collection has %d documents", ix.Name, count, want))
	}
}

// locateKeys 在多键索引条数对不上时逐篇找出缺了哪些节点、多了哪些节点。
//
// 先沿第 0 层数一遍每篇文档挂着几个节点，再逐篇求键：键数与节点数相符的文档跳过，
// 不符的逐个键到跳表里找指向这篇文档、键完全相同的节点，找不到就是缺；这篇文档挂着的
// 节点比认领到的多，再走一遍第 0 层把没被认领的指出来。内存里只有每篇文档的节点数和
// 出问题的那几篇认领到的节点地址，不留全库的键。指向没有文档之处的节点已经按
// [VerifyKindDangling] 报过，这里不再算。
func (v *verifier) locateKeys(c *verifyCollection, mk *verifyMultiKey, docs map[xpage.Address]bool, count int) {
	colID := c.cp.ID()
	held := map[xpage.Address]int{}
	v.eachNode(colID, mk.ix, count, func(_ xpage.Address, n xpage.IndexNode) {
		if b := n.DataBlock(); docs[b] {
			held[b]++
		}
	})

	// suspect 是节点比认领到的多的一篇文档：主键文本、应有键数、已被认领的节点。
	type suspect struct {
		id      string
		keys    int
		claimed map[xpage.Address]bool
	}
	suspects := map[xpage.Address]*suspect{}
	list := xstore.New(v.snap).List(&mk.ix)
	v.eachDocument(c, docs, func(addr xpage.Address, doc *xbson.Document) {
		keys, err := v.eng.IndexKeys(&mk.ix, doc.Value())
		if err != nil || len(keys) == held[addr] {
			return
		}
		id := verifyDocID(doc)
		claimed := map[xpage.Address]bool{}
		for _, k := range keys {
			if a, ok := v.findKeyNode(colID, list, k, addr, claimed, count); ok {
				claimed[a] = true
				continue
			}
			v.issue(colID, c.name, mk.ix.Name, VerifyKindIndexCount, fmt.Sprintf(
				"index %q is missing the node for key %s of document _id %s at %s (it produces %d keys, the index holds %d nodes for it)",
				mk.ix.Name, verifyKeyText(k), id, addr, len(keys), held[addr]))
		}
		if held[addr] > len(claimed) {
			suspects[addr] = &suspect{id: id, keys: len(keys), claimed: claimed}
		}
	})
	if len(suspects) == 0 {
		return
	}
	var key xbson.Value
	v.eachNode(colID, mk.ix, count, func(a xpage.Address, n xpage.IndexNode) {
		block := n.DataBlock()
		s := suspects[block]
		if s == nil || s.claimed[a] {
			return
		}
		text := "(unreadable)"
		if _, err := key.ReadIndexKeyInto(n.KeyBytes()); err == nil {
			text = verifyKeyText(&key)
		}
		v.issue(a.PageID, c.name, mk.ix.Name, VerifyKindIndexCount, fmt.Sprintf(
			"node %s with key %s is an extra node for document _id %s at %s (it produces %d keys, the index holds %d nodes for it)",
			a, text, s.id, block, s.keys, held[block]))
	})
}

// eachNode 沿第 0 层把一条索引的真实节点依次交给 fn。
//
// 只在 checkIndex 走通第 0 层之后用，快照不会变；步数仍以 limit 为限，读不出来就停。
func (v *verifier) eachNode(colID uint32, ix xpage.CollectionIndex, limit int, fn func(xpage.Address, xpage.IndexNode)) {
	head, err := v.indexNode(colID, ix.Head)
	if err != nil {
		return
	}
	cur := head.Next(0)
	for hops := 0; hops < limit && cur != ix.Tail && !cur.IsEmpty(); hops++ {
		if v.stopped() {
			return
		}
		n, err := v.indexNode(colID, cur)
		if err != nil {
			return
		}
		fn(cur, n)
		cur = n.Next(0)
	}
}

// findKeyNode 在跳表里找一个指向 block、键与 k 完全相同、还没被认领的节点。
//
// 跳表只按排序规则排，相等的键挨在一起、彼此却没有次序，所以从查到的那个节点出发往两边
// 把相等的一段都看一遍。「完全相同」按写入时判重的口径：类型相同，按二进制序值相等——
// 忽略大小写的库里 'a' 与 'A'、数值上相等的 5 与 5.0 都各算各的。
func (v *verifier) findKeyNode(colID uint32, list xstore.SkipList, k *xbson.Value, block xpage.Address,
	claimed map[xpage.Address]bool, limit int) (xpage.Address, bool) {
	hit, err := list.Find(k, false, xstore.Asc, v.coll)
	if err != nil || hit == nil {
		return xpage.Address{}, false
	}
	var rk xbson.Value
	scan := func(cur xpage.Address, back bool) (xpage.Address, bool) {
		for hops := 0; hops <= limit && !cur.IsEmpty(); hops++ {
			n, err := v.indexNode(colID, cur)
			if err != nil {
				break
			}
			if _, err := rk.ReadIndexKeyInto(n.KeyBytes()); err != nil || rk.Compare(k, v.coll) != 0 {
				break
			}
			if n.DataBlock() == block && !claimed[cur] && rk.Type() == k.Type() && rk.Compare(k, xcoll.Binary) == 0 {
				return cur, true
			}
			if back {
				cur = n.Prev(0)
			} else {
				cur = n.Next(0)
			}
		}
		return xpage.Address{}, false
	}
	if a, ok := scan(hit.Addr, true); ok {
		return a, true
	}
	return scan(hit.Next0(), false)
}

// checkLevels 校验第 1 层往上的各层链。
//
// 每一步都得跳到第 0 层上更靠后的节点，节点得真有这一层，前驱指针得指回上一步。
// 链可以停在空地址上而不到尾节点：另一种写法建出来的高层链就是这样收尾的。
func (v *verifier) checkLevels(c *verifyCollection, ix xpage.CollectionIndex, head, tail xpage.IndexNode, pos map[xpage.Address]int) {
	colID := c.cp.ID()
	link := func(page uint32, format string, args ...any) {
		v.issue(page, c.name, ix.Name, VerifyKindIndexLink, fmt.Sprintf(format, args...))
	}
	for level := 1; level < head.Levels(); level++ {
		prevAddr, last := ix.Head, 0
		cur := head.Next(level)
		for !cur.IsEmpty() && cur != ix.Tail {
			if v.stopped() {
				return
			}
			p, ok := pos[cur]
			if !ok {
				link(prevAddr.PageID, "level %d links %s to %s, which is not on level 0", level, prevAddr, cur)
				break
			}
			if p <= last {
				link(prevAddr.PageID, "level %d goes back from %s to %s", level, prevAddr, cur)
				break
			}
			n, err := v.indexNode(colID, cur)
			if err != nil {
				break
			}
			if n.Levels() <= level {
				link(cur.PageID, "node %s has %d levels but is linked on level %d", cur, n.Levels(), level)
				break
			}
			if back := n.Prev(level); back != prevAddr {
				link(cur.PageID, "on level %d node %s points back to %s, but the level reaches it from %s",
					level, cur, back, prevAddr)
			}
			prevAddr, last, cur = cur, p, n.Next(level)
		}
		if cur == ix.Tail && level < tail.Levels() {
			if back := tail.Prev(level); back != prevAddr {
				link(ix.Tail.PageID, "on level %d the tail node points back to %s, but the level reaches it from %s",
					level, back, prevAddr)
			}
		}
	}
}

// indexNode 取一个跳表节点，并核对它落在这个集合的索引页上。
func (v *verifier) indexNode(colID uint32, a xpage.Address) (xpage.IndexNode, error) {
	p, err := v.typedPage(colID, a.PageID, xpage.PageIndex)
	if err != nil {
		return xpage.IndexNode{}, err
	}
	return p.GetIndexNode(a.Index)
}

// checkVectors 校验这个集合的向量索引：空闲页链、从根出发走得到的图，以及每个向量节点
// 指向的文档确实在。
//
// 走不到的节点不算问题——删掉根之后图本来就可能裂成几块；但它们照样不能指向已删的文档。
func (v *verifier) checkVectors(c *verifyCollection, nodes []verifyVectorNode, docs map[xpage.Address]bool) {
	colID := c.cp.ID()
	vxs := c.cp.VectorIndexes()
	reached := map[xpage.Address]string{}
	for _, vx := range vxs {
		owner := "vector index " + strconv.Quote(c.name) + "." + strconv.Quote(vx.Name)
		v.walkList(vx.FreePageList, verifyList{
			coll: c.name, index: vx.Name, what: fmt.Sprintf("free page list of vector index %q", vx.Name), owner: owner,
			from: colID, want: xpage.PageVectorIndex, colID: colID, slot: 0,
		})
		v.walkGraph(c, vx, owner, reached)
	}
	only := ""
	if len(vxs) == 1 {
		only = vxs[0].Name
	}
	for _, n := range nodes {
		if v.stopped() {
			return
		}
		if docs[n.block] {
			continue
		}
		index, ok := reached[n.addr]
		if !ok {
			index = only
		}
		v.issue(n.addr.PageID, c.name, index, VerifyKindDangling, fmt.Sprintf(
			"vector node %s points at %s, where there is no document", n.addr, n.block))
	}
}

// walkGraph 从根出发广度优先地走一条向量索引的图，认领走到的节点所在的页。
func (v *verifier) walkGraph(c *verifyCollection, vx xpage.VectorIndex, owner string, reached map[xpage.Address]string) {
	if vx.Root.IsEmpty() {
		return
	}
	colID := c.cp.ID()
	seen := map[xpage.Address]bool{vx.Root: true}
	queue := []xpage.Address{vx.Root}
	for len(queue) > 0 {
		if v.stopped() {
			return
		}
		a := queue[0]
		queue = queue[1:]
		p, err := v.typedPage(colID, a.PageID, xpage.PageVectorIndex)
		var n xpage.VectorNode
		if err == nil {
			n, err = p.GetVectorNode(a.Index)
		}
		if err != nil {
			at := colID
			if a.PageID < v.pages {
				at = a.PageID
			}
			v.issue(at, c.name, vx.Name, VerifyKindIndexLink, fmt.Sprintf("vector node %s cannot be read: %v", a, err))
			continue
		}
		v.claim(a.PageID, c.name, vx.Name, owner)
		reached[a] = vx.Name
		for level := range n.LevelCount() {
			ns, err := n.Neighbors(level)
			if err != nil {
				v.issue(a.PageID, c.name, vx.Name, VerifyKindIndexLink, fmt.Sprintf("vector node %s: %v", a, err))
				break
			}
			for _, b := range ns {
				if !b.IsEmpty() && !seen[b] {
					seen[b] = true
					queue = append(queue, b)
				}
			}
		}
	}
}

// checkOrphans 报出属于集合表里没有登记的集合的数据页。
//
// 集合表丢了登记时文档还躺在各自的数据页上，只有重建按页扫描才捡得回来。
func (v *verifier) checkOrphans(known map[uint32]bool) {
	for _, col := range slices.Sorted(maps.Keys(v.dataByCol)) {
		if known[col] {
			continue
		}
		for _, id := range v.dataByCol[col] {
			if v.stopped() {
				return
			}
			v.issue(id, "", "", VerifyKindOrphanPage, fmt.Sprintf(
				"data page %d belongs to collection page %d, which the collection table does not list", id, col))
		}
	}
}
