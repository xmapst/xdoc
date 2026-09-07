// Package xwal 是预写日志的内存索引。
//
// 写入先追加到日志文件，再由搬运把最新版写回数据文件。索引记着每一页的
// 每个版本落在日志的哪个偏移上，读者按自己的读版本号取到「事务开始那一刻」
// 的那一版——这就是快照隔离的落点。
//
// 一个事务的页连着写，最后一页带确认标记；没有确认标记的那些在崩溃恢复时
// 整批丢掉，所以事务要么全生效、要么全不生效。
package xwal

import (
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/xmapst/xdoc/internal/xpage"
)

// entry 记着某一页的某个版本落在日志文件的哪个偏移上。
type entry struct {
	version int64
	offset  int64
}

// Index 是日志文件的内存索引：哪一页的哪个版本在日志的什么位置。
//
// 它让读者能按自己那份读版本号取到「当时」的那一版页，
// 而写者可以同时往日志尾部追加新版本——两边互不阻塞。
//
// 只在内存里，进程启动时靠 [Index.Restore] 扫一遍日志重建。
type Index struct {
	mu sync.RWMutex

	// pages 是每一页的历次版本，按版本号递增排列。
	//
	// 追加时天然有序（版本号只增），所以查找可以从尾部往回扫。
	pages map[uint32][]entry

	// confirmed 是已经提交的事务号。
	confirmed map[uint32]struct{}

	// readVersion 是当前最新的读版本号，每提交一个事务加一。
	readVersion int64

	// lastTxID 是已经发出去的最大事务号。
	lastTxID uint32
}

// NewIndex 建一个空索引。
func NewIndex() *Index {
	return &Index{pages: map[uint32][]entry{}, confirmed: map[uint32]struct{}{}}
}

// NextTransactionID 发一个新的事务号。
func (x *Index) NextTransactionID() uint32 {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.lastTxID++
	return x.lastTxID
}

// LastTransactionID 返回已经发出去的最大事务号。
func (x *Index) LastTransactionID() uint32 {
	x.mu.RLock()
	defer x.mu.RUnlock()
	return x.lastTxID
}

// ReadVersion 返回当前最新的读版本号，新事务以它为快照。
func (x *Index) ReadVersion() int64 {
	x.mu.RLock()
	defer x.mu.RUnlock()
	return x.readVersion
}

// ConfirmedCount 返回日志里已提交事务的条数。
//
// 它是自动搬运的触发依据。
func (x *Index) ConfirmedCount() int {
	x.mu.RLock()
	defer x.mu.RUnlock()
	return len(x.confirmed)
}

// IsConfirmed 报告某个事务提交了没有。
func (x *Index) IsConfirmed(txID uint32) bool {
	x.mu.RLock()
	defer x.mu.RUnlock()
	_, ok := x.confirmed[txID]
	return ok
}

// Lookup 查某一页在给定读版本下该读日志的哪个位置。
func (x *Index) Lookup(pageID uint32, version int64) (offset int64, ok bool) {
	offset, _, ok = x.LookupVersion(pageID, version)
	return offset, ok
}

// LookupVersion 与 [Index.Lookup] 一样，另外返回命中的那个版本号。
//
// **取不大于 version 的最新那一版**：这就是快照隔离——事务开始之后
// 别人提交的新版本，它看不见。
//
// 从尾部往回扫：多数查找命中的是最近的版本，而版本历史通常很短。
//
// version 为 0 表示「不看日志」，直接给未命中。
func (x *Index) LookupVersion(pageID uint32, version int64) (offset int64, ver int64, ok bool) {
	if version == 0 {
		return 0, 0, false
	}
	x.mu.RLock()
	defer x.mu.RUnlock()
	es := x.pages[pageID]

	for _, e := range slices.Backward(es) {
		if e.version <= version {
			return e.offset, e.version, true
		}
	}
	return 0, 0, false
}

// PagePosition 是一页在日志文件里的位置。
type PagePosition struct {
	PageID uint32
	Offset int64
}

// Confirm 把一个事务写下的页整批登记进索引，返回新的读版本号。
//
// **整批一起加**：读版本号在这一刻才递增，所以别的读者要么看到
// 这个事务的全部改动，要么一个也看不到。
func (x *Index) Confirm(txID uint32, positions []PagePosition) int64 {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.readVersion++
	v := x.readVersion
	for _, p := range positions {
		x.pages[p.PageID] = append(x.pages[p.PageID], entry{version: v, offset: p.Offset})
	}
	x.confirmed[txID] = struct{}{}
	return v
}

// Positions 返回每一页最新那一版在日志里的位置。
//
// 搬运时用：只有最新版要写回数据文件，中间那些版本直接丢掉。
func (x *Index) Positions() []PagePosition {
	x.mu.RLock()
	defer x.mu.RUnlock()
	out := make([]PagePosition, 0, len(x.pages))
	for id, es := range x.pages {
		if len(es) == 0 {
			continue
		}
		out = append(out, PagePosition{PageID: id, Offset: es[len(es)-1].offset})
	}
	return out
}

// Clear 清空索引，搬运完成、日志被截断之后调用。
//
// 读版本号也归零：日志空了，之后的版本号从头开始记。
func (x *Index) Clear() {
	x.mu.Lock()
	defer x.mu.Unlock()
	clear(x.pages)
	clear(x.confirmed)
	x.readVersion = 0
}

// LogReader 是 [Index.Restore] 需要的日志文件读取能力。
type LogReader interface {
	ReadLogPage(off int64, buf []byte) error
	LogPageCount() int64
}

// HeaderSink 接收恢复过程中遇到的头页，让上层把全库配置也跟着回放。
type HeaderSink func(buf []byte) error

// Restore 扫一遍日志文件，重建索引。
//
// 日志是顺序追加的，一个事务的页连着写，最后一页带确认标记。所以做法是：
// 按事务号把页攒起来，碰到带确认标记的那一页就整批登记；
// 扫完之后**还攒着的那些属于没提交完的事务，整批丢掉**——
// 崩溃恢复靠的就是这一条。
//
// 空白页跳过：日志文件可能被预先撑大过。
func (x *Index) Restore(r LogReader, onHeader HeaderSink) error {
	x.mu.Lock()
	defer x.mu.Unlock()

	pending := map[uint32][]PagePosition{}
	buf := make(xpage.RawPage, xpage.PageSize)
	pages := r.LogPageCount()

	for i := range pages {
		off := i * xpage.PageSize
		if err := r.ReadLogPage(off, buf); err != nil {
			return fmt.Errorf("xwal: restore: %w", err)
		}
		if buf.IsBlank() {
			continue
		}
		id := xpage.PeekPageID(buf)
		txID := buf.TransactionID()
		confirmed := buf.IsConfirmed()
		pending[txID] = append(pending[txID], PagePosition{PageID: id, Offset: off})

		x.lastTxID = max(x.lastTxID, txID)

		if !confirmed {
			continue
		}
		x.readVersion++
		v := x.readVersion
		for _, p := range pending[txID] {
			x.pages[p.PageID] = append(x.pages[p.PageID], entry{version: v, offset: p.Offset})
		}
		x.confirmed[txID] = struct{}{}

		delete(pending, txID)

		if onHeader != nil && xpage.PageType(buf[4]) == xpage.PageHeader {
			if err := onHeader(buf); err != nil {
				return fmt.Errorf("xwal: restore header page: %w", err)
			}
		}
	}
	return nil
}

// ErrTooManyTransactions 表示同时打开的事务太多。
var ErrTooManyTransactions = errors.New("xwal: too many open transactions")

// HasPage 报告日志里有没有这一页的任何版本。
func (x *Index) HasPage(pageID uint32) bool {
	x.mu.RLock()
	defer x.mu.RUnlock()
	return len(x.pages[pageID]) > 0
}
