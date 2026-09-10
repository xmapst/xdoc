package xtx

import (
	"context"
	"time"

	"github.com/xmapst/xdoc/internal/xdisk"
	"github.com/xmapst/xdoc/internal/xpage"
)

// Checkpoint 把日志里已确认的页搬回数据文件，返回搬了几页。
//
// 要拿排他闸门，因此会等所有事务结束；排队期间新开的事务要等它做完或者超时放弃。
func (c *Core) Checkpoint(ctx context.Context) (int, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	if err := c.locks.EnterExclusive(ctx); err != nil {
		return 0, err
	}
	defer func() { _ = c.locks.ExitExclusive() }()
	return c.checkpointLocked()
}

// TryCheckpoint 同 [Core.Checkpoint]，但拿不到排他闸门时立刻返回，第二个返回值说明做没做。
func (c *Core) TryCheckpoint() (int, bool, error) {
	if !c.locks.TryEnterExclusive() {
		return 0, false, nil
	}
	defer func() { _ = c.locks.ExitExclusive() }()
	n, err := c.checkpointLocked()
	return n, true, err
}

// checkpointLocked 在已持有排他闸门时做检查点。
//
// 按 512 页一批读日志，跳过空白页和事务尚未确认的页——那些属于没提交完的
// 事务，搬过去就是把脏数据写进了数据文件。搬之前把页上的事务号和确认标记
// 清掉：落到数据文件里的页不再属于任何事务。
//
// 搬完先把数据文件刷到设备，再清日志——顺序反了的话，两步之间断电就两头落空。
//
// 日志非空、真正动手的那些计进 [Stats]；失败的另记一条日志。
func (c *Core) checkpointLocked() (n int, err error) {
	c.headerMu.Lock()
	defer c.headerMu.Unlock()

	pages := c.disk.LogPageCount()
	if pages == 0 {
		return 0, nil
	}
	start := time.Now()
	defer func() { c.observeCheckpoint(start, pages, n, err) }()

	const runPages = 512
	run := make([]byte, runPages*xpage.PageSize)
	batch := make([]xdisk.DataPageWrite, 0, runPages)
	moved := make(map[uint32]struct{}, runPages)
	for i := int64(0); i < pages; i += runPages {
		got := min(int64(runPages), pages-i)
		buf := run[:got*xpage.PageSize]
		if err := c.disk.ReadLogRun(i*xpage.PageSize, buf); err != nil {
			return n, err
		}
		batch = batch[:0]
		for k := range got {
			page := xpage.RawPage(buf[k*xpage.PageSize : (k+1)*xpage.PageSize])
			if page.IsBlank() {
				continue
			}

			if !c.wal.IsConfirmed(page.TransactionID()) {
				continue
			}

			page.SetTransactionID(xpage.EmptyPageID)
			page.SetConfirmed(false)
			id := xpage.PeekPageID(page)
			moved[id] = struct{}{}
			batch = append(batch, xdisk.DataPageWrite{ID: id, Buf: page})
		}
		if err := c.disk.WriteDataPages(batch); err != nil {
			return n, err
		}
		n += len(batch)
	}

	if err := c.disk.SyncData(); err != nil {
		return n, err
	}
	if err := c.disk.ClearLog(); err != nil {
		return n, err
	}
	c.wal.Clear()
	c.forceCheckpointAt.Store(0)

	c.cache.DropLogAndData(moved)
	return n, nil
}
