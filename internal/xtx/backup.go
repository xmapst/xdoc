package xtx

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/xmapst/xdoc/internal/xdisk"
	"github.com/xmapst/xdoc/internal/xpage"
)

// Backup 把库在当前已提交版本上的全部页，按页号从 0 到最后一页依次交给 emit。
//
// 这串页连起来就是一份刚做完检查点的数据文件，不要日志就能打开。
//
// 只进共享闸门、定住一个读版本，不挡提交：之后的提交照常写日志，只是这份备份看不见。
// 检查点要排他闸门，得等备份做完，所以途中读到的数据页不会被改写——代价是这段时间日志只涨不缩。
// 每一页按快照的查法取：日志里不晚于这个版本的最新一版，没有再读数据文件。从日志来的页
// 清掉事务号和确认标记，与检查点搬回数据文件时一样。页缓存不碰，免得一趟全库读把热页挤出去。
//
// 在途事务扩出来的页号可能已经进了头页：共享的头页由别的事务提交时一起写进日志。这样的页
// 在这个版本里哪儿都没有内容，写成一页不在空页链上的空页——与在这一刻崩溃后恢复出来的库
// 一样只是漏几页空间，而不是留下读不出来的页号。
//
// emit 拿到的缓冲在它返回后会被复用，不能改。ctx 每页看一次，取消时返回 ctx.Err()。
func (c *Core) Backup(ctx context.Context, emit func(page []byte) error) error {
	if err := c.checkBroken(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	lctx, cancel := c.withTimeout(ctx)
	err := c.locks.EnterShared(lctx)
	cancel()
	if err != nil {
		return err
	}
	defer func() { _ = c.locks.ExitShared() }()

	// 版本号必须在进了闸门之后取：之前取的话，中间插进来的检查点会把日志和版本号一起清零。
	version := c.wal.ReadVersion()
	dataPages, err := c.disk.DataPageCount()
	if err != nil {
		return err
	}
	buf := make([]byte, xpage.PageSize)
	if err := c.backupPage(0, version, dataPages, buf); err != nil {
		return err
	}
	h, err := xpage.LoadHeaderPage(bytes.Clone(buf))
	if err != nil {
		return fmt.Errorf("xtx: backup header page: %w", err)
	}
	last := int64(h.LastPageID())
	for id := int64(0); id <= last; id++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if id > 0 {
			if err := c.backupPage(uint32(id), version, dataPages, buf); err != nil {
				return err
			}
		}
		if err := emit(buf); err != nil {
			return err
		}
	}
	return nil
}

// backupPage 把第 id 页在 version 那一版的内容读进 buf，见 [Core.Backup]。
//
// 数据文件里读到全零的页说明文件在那里被撑大过、却从没写过，与文件末尾之外一样当成没有内容。
func (c *Core) backupPage(id uint32, version int64, dataPages uint32, buf []byte) error {
	if c.closed.Load() {
		return ErrClosed
	}
	if off, ok := c.wal.Lookup(id, version); ok {
		if err := c.disk.ReadLogPage(off, buf); err != nil {
			return err
		}
		if got := xpage.PeekPageID(buf); got != id {
			return fmt.Errorf("%w: log offset %d holds page %d, wanted %d", xpage.ErrCorrupt, off, got, id)
		}
		raw := xpage.RawPage(buf)
		raw.SetTransactionID(xpage.EmptyPageID)
		raw.SetConfirmed(false)
		return nil
	}
	if id < dataPages {
		err := c.disk.ReadDataPage(id, buf)
		if err == nil || !errors.Is(err, xdisk.ErrMisplacedPage) || !xpage.RawPage(buf).IsBlank() {
			return err
		}
	}
	clear(buf)
	_, err := xpage.PageEmpty.NewPage(buf, id)
	return err
}
