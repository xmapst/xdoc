package xdoc

import (
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xpage"
	"github.com/xmapst/xdoc/internal/xtx"
)

// SalvageSkip 是一次重建里没能搬过去的一处，由 [RebuildResult] 的 Skipped 带出来。
//
// 它同时会被写进新库的 [RebuildErrorsCollection]：Skipped 是给调用方当场看的，
// 那个集合是给**之后**看的——自动重建没有调用方接得住返回值。
type SalvageSkip struct {
	// PageID 是出问题的那一页。
	PageID uint32

	// Slot 是页内的槽号，负数表示整页都没读出来（那时连有几个槽都不知道）。
	// [SalvageSkip.pageTypeName] 靠它区分两种情形。
	Slot int

	// Err 是当时读到的错误。
	Err error
}

// String 给出一行可读的说明：整页读不出，还是某个槽读不出。
func (s SalvageSkip) String() string {
	if s.Slot < 0 {
		return fmt.Sprintf("page %d: %v", s.PageID, s.Err)
	}
	return fmt.Sprintf("page %d slot %d: %v", s.PageID, s.Slot, s.Err)
}

// salvage 是绕开索引、按页把够得着的文档捡回来的那条路。
//
// 常规搬运顺着主键索引走，中途断了就改走这里。两条路的分别在于依赖：
// 索引是一条链，断一环后面全够不着；按页扫不依赖任何链，每一页自成一体，
// 坏一页就只少那一页上的文档。
type salvage struct {
	*xtx.Snapshot
}

// scan 把属于 colID 的数据页逐页扫一遍，把解得出来的文档交给 yield。
//
// 解不出来的记进返回的 skips 里，不中断扫描——这条路的全部意义就是"能捡多少
// 是多少"。yield 返回 false 表示调用方不要了，就地停下。
//
// 跳过 Extend 块：那是上一篇文档的后续分片，从它自己的首块进入才是完整的一篇。
func (s salvage) scan(colID uint32, yield func(*Document) bool) []SalvageSkip {
	var skips []SalvageSkip
	for id := uint32(1); id < s.PageCount(); id++ {
		p, err := s.GetPage(id)
		if err != nil {
			skips = append(skips, SalvageSkip{PageID: id, Slot: -1, Err: err})
			continue
		}
		if p.Type() != xpage.PageData || p.ColID() != colID {
			continue
		}
		hi := p.HighestIndex()
		if hi == xpage.EmptyIndex {
			continue
		}
		for i := uint8(0); ; i++ {
			blk, err := p.GetDataBlock(i)
			if err != nil {
				if i >= hi {
					break
				}
				continue
			}
			if !blk.Extend() {
				doc, err := s.readDocument(p, i)
				if err != nil {
					skips = append(skips, SalvageSkip{PageID: id, Slot: int(i), Err: err})
				} else if !yield(doc) {
					return skips
				}
			}
			if i >= hi {
				break
			}
		}
	}
	return skips
}

// readDocument 顺着块链把一篇文档的字节拼起来再解码。
//
// maxHops 是给坏数据兜底的：块链的 next 指针若被写坏成一个环，
// 没有这道闸就是一个不返回的循环。
//
// 时间一律按 UTC 解：这里读出来的文档要原样搬进新库，不该被本地时区染上
// 一个与原文件不同的墙上读数。
func (s salvage) readDocument(first *xpage.Page, slot uint8) (*Document, error) {
	const maxHops = 4096
	var buf []byte
	p := first
	idx := slot
	for hops := 0; ; hops++ {
		if hops > maxHops {
			return nil, fmt.Errorf("block chain longer than %d blocks", maxHops)
		}
		blk, err := p.GetDataBlock(idx)
		if err != nil {
			return nil, err
		}
		buf = append(buf, blk.Data()...)
		next := blk.NextBlock()
		if next.IsEmpty() {
			break
		}
		p, err = s.GetPage(next.PageID)
		if err != nil {
			return nil, err
		}
		idx = next.Index
	}

	return xbson.DecodeIn(buf, time.UTC)
}

// orphans 找出「有数据页、集合表里却没登记」的集合号。
//
// 集合表是页 0 上的一篇文档，一次写到一半的页 0 写入能把它整块抹掉，
// 而文档一篇没少——它们躺在各自的数据页上，每页的页头都记着自己属于哪个集合。
// 重建搬完表里那批之后再扫一遍，就能把这些集合捡回来。
//
// 读不出来的页直接跳过：这一步只为找出还有哪些集合号，
// 读不出的页本来就没有可信的集合号可用。
func (s salvage) orphans(known map[uint32]bool) []uint32 {
	seen := make(map[uint32]bool)
	for id := uint32(1); id < s.PageCount(); id++ {
		p, err := s.GetPage(id)
		if err != nil {
			continue
		}
		if p.Type() != xpage.PageData {
			continue
		}
		if col := p.ColID(); !known[col] {
			seen[col] = true
		}
	}
	out := slices.Sorted(maps.Keys(seen))
	return out
}
