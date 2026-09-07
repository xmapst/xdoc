package xdoc

import (
	"bytes"
	"context"
	"fmt"
	"iter"
	"maps"
	"slices"

	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xpage"
	"github.com/xmapst/xdoc/internal/xtx"
)

// sysPageTx 给按页遍历的虚拟集合取一个事务，返回收尾函数。
//
// 已经在事务里就借用它，收尾函数什么也不做——别人的事务不该由这里结束。
// 自开的那个一律回滚：这些集合只读，回滚比提交少一次落盘。
func (db *DB) sysPageTx(ctx context.Context, tx *Tx) (*Tx, func(), error) {
	if tx != nil {
		return tx, func() {}, nil
	}
	own, err := db.BeginTrans(ctx)
	if err != nil {
		return nil, nil, err
	}

	return own, func() { _ = own.Rollback() }, nil
}

// sysGlobalSnapshot 取一份不限定集合的读快照。
//
// 集合名 "$" 是个占位：按页遍历跨越所有集合，锁不到具体某一个。
func (t *Tx) sysGlobalSnapshot(ctx context.Context) (*xtx.Snapshot, error) {
	return t.tx.Snapshot(ctx, "$", xtx.ModeRead, false)
}

// sysCollNames 从头页读出集合编号到集合名的对照表。
func (db *DB) sysCollNames() (map[uint32]string, error) {
	out := map[uint32]string{}
	err := db.core.WithHeader(func(h *xpage.HeaderPage) error {
		for name, id := range h.Collections() {
			out[id] = name
		}
		return nil
	})
	return out, err
}

// pageID 取出 pageID 选项，第二个返回值说明有没有给。
//
// 没给就是「全都要」，给了就是「只要这一页（或从这一页起）」。
func (so sysOpts) pageID(name string) (uint32, bool, error) {
	v, err := so.option("pageID", nil)
	if err != nil {
		return 0, false, err
	}
	if v == nil || v.Type() == xbson.TypeNull {
		return 0, false, nil
	}
	n, err := pragmaInt32(v)
	if err != nil {
		return 0, false, fmt.Errorf("xdoc: %s: `%s` is not a valid page number", name, v)
	}
	return uint32(n), true, nil
}

// sysPageList 沿页链走一遍，每经过一页产出一篇描述。
//
// 不带 pageID 时走的是全部**空闲页**链：先是头页记的空页链，然后按集合名排序，
// 逐个集合走它的数据页空闲槽链与各索引的空闲页链。带 pageID 时就从那一页
// 开始沿 next 往下走，不管它属于哪条链。
//
// 页号超过文件末页时报错——那不是「查不到」，是问了一个不存在的东西。
//
// 每走一页设一个安全点，让长链上的遍历能被取消，也让事务的页预算有机会回收。
func (db *DB) sysPageList(ctx context.Context, tx *Tx, opts sysOpts) iter.Seq2[*Document, error] {
	pageID, single, err := opts.pageID("$page_list")
	if err != nil {
		return seqErr[*Document](err)
	}
	return func(yield func(*Document, error) bool) {
		t, done, err := db.sysPageTx(ctx, tx)
		if err != nil {
			yield(nil, err)
			return
		}
		defer done()

		names, err := db.sysCollNames()
		if err != nil {
			yield(nil, err)
			return
		}
		snap, err := t.sysGlobalSnapshot(ctx)
		if err != nil {
			yield(nil, err)
			return
		}

		walk := func(s *xtx.Snapshot, head uint32, indexName string) bool {
			for id := head; id != xpage.EmptyPageID; {
				p, _, err := t.sysGetPage(s, id)
				if err != nil {
					return yield(nil, err)
				}
				d := xbson.NewDocument()
				d.Set("pageID", xbson.Int32(int32(p.ID())))
				d.Set("pageType", xbson.String(p.Type().String()))
				d.Set("slot", xbson.Int32(int32(p.PageListSlot())))

				if n, ok := names[p.ColID()]; ok {
					d.Set("collection", xbson.String(n))
				} else {
					d.Set("collection", xbson.Null)
				}
				if indexName != "" {
					d.Set("index", xbson.String(indexName))
				} else {
					d.Set("index", xbson.Null)
				}
				d.Set("freeBytes", xbson.Int32(int32(p.FreeBytes())))
				d.Set("itemsCount", xbson.Int32(int32(p.ItemsCount())))
				if !yield(d, nil) {
					return false
				}
				next := p.NextPageID()
				if next == xpage.EmptyPageID {
					break
				}
				if err := t.tx.Safepoint(); err != nil {
					return yield(nil, err)
				}
				id = next
			}
			return true
		}

		var freeEmpty, last uint32
		if err := db.core.WithHeader(func(h *xpage.HeaderPage) error {
			freeEmpty, last = h.FreeEmptyPageList(), h.LastPageID()
			return nil
		}); err != nil {
			yield(nil, err)
			return
		}

		if single {
			if pageID != xpage.EmptyPageID && pageID > last {
				yield(nil, fmt.Errorf("request page must be less or equals lastest page in data file"))
				return
			}
			walk(snap, pageID, "")
			return
		}
		if !walk(snap, freeEmpty, "") {
			return
		}

		for _, name := range sortedCollNames(names) {
			cs, err := t.tx.Snapshot(ctx, name, xtx.ModeRead, false)
			if err != nil {
				yield(nil, err)
				return
			}
			cp := cs.CollectionPage()
			if cp == nil {
				continue
			}
			for slot := range uint8(xpage.DataFreeSlotCount) {
				if !walk(cs, cp.FreeDataPage(slot), "") {
					return
				}
			}
			for _, ix := range cp.Indexes() {
				if !walk(cs, ix.FreeIndexPageList, ix.Name) {
					return
				}
			}
		}
	}
}

// sortedCollNames 把集合名排序，好让遍历次序稳定。
func sortedCollNames(names map[uint32]string) []string {
	return slices.Sorted(maps.Values(names))
}

// sysDump 按页号从 0 到文件末页逐页产出一篇描述，不管这一页有没有在用。
//
// 集合页会额外解出它的空闲数据页槽与索引表。
//
// **只有指定单页时才附上整页原始字节**：那是 8 KB 一篇，整库都带上会把内存吃光。
//
// 指定的页号超过末页时不报错，产出零行——end 取的是两者较小值，循环因此一次都不进。
// 末页恰好是 uint32 上限时，末尾的显式跳出挡住了自增回绕。
func (db *DB) sysDump(ctx context.Context, tx *Tx, opts sysOpts) iter.Seq2[*Document, error] {
	pageID, single, err := opts.pageID("$dump")
	if err != nil {
		return seqErr[*Document](err)
	}
	return func(yield func(*Document, error) bool) {
		t, done, err := db.sysPageTx(ctx, tx)
		if err != nil {
			yield(nil, err)
			return
		}
		defer done()

		names, err := db.sysCollNames()
		if err != nil {
			yield(nil, err)
			return
		}
		snap, err := t.sysGlobalSnapshot(ctx)
		if err != nil {
			yield(nil, err)
			return
		}

		last, err := db.core.HeaderValue(func(h *xpage.HeaderPage) (uint32, error) { return h.LastPageID(), nil })
		if err != nil {
			yield(nil, err)
			return
		}

		start, end := uint32(0), last
		if single {
			start, end = pageID, min(pageID, last)
		}
		for id := start; id <= end; id++ {
			p, info, err := t.sysGetPage(snap, id)
			if err != nil {
				yield(nil, err)
				return
			}
			d := xbson.NewDocument()
			d.Set("pageID", xbson.Int32(int32(p.ID())))
			d.Set("pageType", xbson.String(p.Type().String()))
			d.Set("_position", xbson.Int64(info.Position))
			d.Set("_origin", xbson.String(info.Origin.String()))
			d.Set("_version", xbson.Int32(int32(info.Version)))
			d.Set("prevPageID", xbson.Int32(int32(p.PrevPageID())))
			d.Set("nextPageID", xbson.Int32(int32(p.NextPageID())))
			d.Set("slot", xbson.Int32(int32(p.PageListSlot())))

			n, ok := names[p.ColID()]
			if !ok {
				n = "-"
			}
			d.Set("collection", xbson.String(n))
			d.Set("itemsCount", xbson.Int32(int32(p.ItemsCount())))
			d.Set("freeBytes", xbson.Int32(int32(p.FreeBytes())))
			d.Set("usedBytes", xbson.Int32(int32(p.UsedBytes())))
			d.Set("fragmentedBytes", xbson.Int32(int32(p.FragmentedBytes())))
			d.Set("nextFreePosition", xbson.Int32(int32(p.NextFreePosition())))
			d.Set("highestIndex", xbson.Int32(int32(p.HighestIndex())))

			if p.Type() == xpage.PageCollection {
				cp, err := xpage.LoadCollectionPage(p.Bytes())
				if err != nil {
					yield(nil, err)
					return
				}
				list := xbson.NewArray()
				for slot := range uint8(xpage.DataFreeSlotCount) {
					list.Append(xbson.Int32(int32(cp.FreeDataPage(slot))))
				}
				d.Set("dataPageList", list.Value())

				ixs := xbson.NewArray()
				for _, ix := range cp.Indexes() {
					e := xbson.NewDocument()
					e.Set("slot", xbson.Int32(int32(ix.Slot)))
					e.Set("empty", xbson.Boolean(ix.Name == ""))
					e.Set("indexType", xbson.Int32(int32(ix.Kind)))
					e.Set("name", xbson.String(ix.Name))
					e.Set("expression", xbson.String(ix.Expression))
					e.Set("unique", xbson.Boolean(ix.Unique))
					e.Set("head", sysAddress(ix.Head))
					e.Set("tail", sysAddress(ix.Tail))
					e.Set("freeIndexPageList", xbson.Int32(int32(ix.FreeIndexPageList)))
					ixs.Append(e.Value())
				}
				d.Set("indexes", ixs.Value())
			}

			if single {
				d.Set("buffer", xbson.Binary(bytes.Clone(p.Bytes())))
			}
			if !yield(d, nil) {
				return
			}
			if err := t.tx.Safepoint(); err != nil {
				yield(nil, err)
				return
			}

			if id == end {
				break
			}
		}
	}
}

// sysGetPage 取一页，本事务改过的以事务内那份为准。
//
// 事务内那份还没落盘，位置与版本无从谈起，所以返回一个空的页信息。
func (t *Tx) sysGetPage(s *xtx.Snapshot, id uint32) (*xpage.Page, xtx.PageInfo, error) {
	if p, ok := t.tx.LocalPage(id); ok {
		return p, xtx.PageInfo{}, nil
	}
	return s.GetPageInfo(id)
}

// sysAddress 把一个页内地址转成文档，空地址转成 Null。
func sysAddress(a xpage.Address) *Value {
	if a == xpage.EmptyAddress {
		return xbson.Null
	}
	d := xbson.NewDocument()
	d.Set("pageID", xbson.Int32(int32(a.PageID)))
	d.Set("index", xbson.Int32(int32(a.Index)))
	return d.Value()
}
