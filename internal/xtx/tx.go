package xtx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/xmapst/xdoc/internal/xpage"
	"github.com/xmapst/xdoc/internal/xwal"
)

// State 是事务的状态。
type State uint8

const (
	// StateActive 表示事务还开着。
	StateActive State = iota

	// StateCommitted 表示已提交。
	StateCommitted

	// StateAborted 表示已回滚，或者提交失败后被作废。
	StateAborted
)

// transPages 记着一个事务动过哪些页。
type transPages struct {
	// dirty 是已经写进日志的页：页号到它在日志里的字节偏移。
	dirty map[uint32]int64

	// newPages 是本事务向文件末尾新扩出来的页；回滚时要还回空页链。
	newPages []uint32

	// firstDelete 与 lastDelete 是本事务删掉的页串成的链的两端，提交时整条接进空页链。
	firstDelete, lastDelete uint32

	// lastDeleteColl 是链尾那一页属于哪个集合，提交时要靠它找回对应的快照。
	lastDeleteColl string
	deleted        int

	// size 是当前占着几页内存，与配额比较后决定要不要设安全点。
	size int
}

// Transaction 是一个事务。
//
// **不是并发安全的**：一个事务只该由一个协程使用。
type Transaction struct {
	core  *Core
	id    uint32
	state State
	quota int

	started time.Time

	// mu 保护 snapshots、各快照的本地页表与集合页，以及 pages 里的计数。
	//
	// 事务只归属主协程，但诊断视图（[Core.Transactions]）会从别的协程来读：
	// 属主改这些时拿着它，自己读不必。
	mu sync.Mutex

	snapshots map[string]*Snapshot
	pages     transPages

	// headerChanged 表示头页有改动，提交时要连头页一起写。
	headerChanged bool

	// onCommit 是提交时要对头页做的事，按登记次序执行。
	onCommit []func(*xpage.HeaderPage) error
}

// ID 返回事务号。
func (t *Transaction) ID() uint32 { return t.id }

// State 返回事务状态。
func (t *Transaction) State() State { return t.state }

// checkActive 确认事务还开着，实例也没关。
//
// 关闭实例时不去改在途事务的状态——事务只归属主协程——所以每次操作都看一眼实例。
func (t *Transaction) checkActive() error {
	if t.state != StateActive {
		return fmt.Errorf("%w (state %d)", ErrTransactionClosed, t.state)
	}
	if t.core.closed.Load() {
		return ErrClosed
	}
	return nil
}

// Commit 提交事务。
//
// 先把脏页连同头页写进日志并落盘，然后在日志索引里把这个事务标成已确认——
// **确认这一步才是提交点**：确认之前崩掉，检查点会跳过这些页；确认之后崩掉，
// 重启时能从日志把它们捞回来。
//
// 出错就回滚。写日志之前出错（比如集合表写满）只让本事务失败；写日志本身出错
// 还要把实例封死，见 [Transaction.persist]。
// 提交成功后视日志长度顺手做一次检查点。
func (t *Transaction) Commit() error {
	// 实例关了的不在这里返回，交给下面 checkBroken 那一支放掉快照和锁。
	if err := t.checkActive(); err != nil && !errors.Is(err, ErrClosed) {
		return err
	}

	if err := t.core.checkBroken(); err != nil {
		t.releaseSnapshots()
		var rerr error
		if len(t.pages.newPages) > 0 {
			rerr = t.returnNewPages()
		}
		t.finish(StateAborted)
		return errors.Join(err, rerr)
	}

	t.core.headerMu.Lock()
	n, err := t.persist(true)
	if err == nil && n > 0 {
		positions := make([]xwal.PagePosition, 0, len(t.pages.dirty))
		for id, off := range t.pages.dirty {
			positions = append(positions, xwal.PagePosition{PageID: id, Offset: off})
		}
		t.core.wal.Confirm(t.id, positions)
	}
	t.core.headerMu.Unlock()

	if err != nil {
		t.releaseSnapshots()
		var rerr error
		if len(t.pages.newPages) > 0 {
			rerr = t.returnNewPages()
		}
		t.finish(StateAborted)
		return errors.Join(err, rerr)
	}

	t.releaseSnapshots()

	t.finish(StateCommitted)
	return t.maybeCheckpoint()
}

// Rollback 回滚事务：新扩的页还回空页链，快照全放掉。已经写进日志的页留在那里，不会被确认。
//
// 实例关掉之后照样放掉快照和锁，只是不再往日志里还页，返回 [ErrClosed]。
func (t *Transaction) Rollback() error {
	err := t.checkActive()
	if err != nil && !errors.Is(err, ErrClosed) {
		return err
	}
	if err == nil && len(t.pages.newPages) > 0 {
		err = t.returnNewPages()
	}
	t.releaseSnapshots()
	t.finish(StateAborted)
	return err
}

// finish 落定状态并把事务从实例上摘掉。
func (t *Transaction) finish(s State) {
	t.state = s
	t.core.release(t)
}

// releaseSnapshots 关掉所有快照，写快照还要放掉对应的集合锁。
func (t *Transaction) releaseSnapshots() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for name, s := range t.snapshots {
		s.close(true)
		if s.mode == ModeWrite {
			_ = t.core.locks.ExitCollection(t.id, name)
		}
	}
	clear(t.snapshots)
}

// LocalPage 取本事务改过但还没落盘的那一页。
//
// 要遍历各个快照——同一个事务里的不同集合各有各的本地页表。
func (t *Transaction) LocalPage(id uint32) (*xpage.Page, bool) {
	for _, s := range t.snapshots {
		if lp, ok := s.local[id]; ok {
			return lp.page, true
		}

		if s.collection != nil && s.collection.ID() == id {
			return s.collection.Page, true
		}
	}
	return nil, false
}

// InspectHeader 在持有头页锁的情况下看一眼头页。
func (t *Transaction) InspectHeader(fn func(*xpage.HeaderPage) error) error {
	t.core.headerMu.Lock()
	defer t.core.headerMu.Unlock()
	return fn(t.core.header)
}

// HeaderValue 同 [Transaction.InspectHeader]，但能带一个返回值出来。
func (t *Transaction) HeaderValue[T any](fn func(*xpage.HeaderPage) (T, error)) (T, error) {
	t.core.headerMu.Lock()
	defer t.core.headerMu.Unlock()
	return fn(t.core.header)
}

// Safepoint 在占用超过配额时把脏页刷进日志，腾出内存。
//
// 长事务靠它把内存占用压住；刷出去的页还没确认，回滚仍然有效。
func (t *Transaction) Safepoint() error {
	if err := t.checkActive(); err != nil {
		return err
	}
	if t.pages.size < t.quota {
		return nil
	}
	t.core.headerMu.Lock()
	defer t.core.headerMu.Unlock()
	_, err := t.persist(false)
	return err
}

// persist 把脏页追加进日志，返回写了几页。
//
// confirm 为真时这是提交的一部分：先把删掉的页链接进空页链，然后在
// **最后一页**上打确认标记。头页有改动时，改动写在最后，确认标记就打在它上面——
// 恢复时看到确认标记才认这一批。安全点已经写出过页、提交时却没有新脏页的，
// 头页没改也照写一份来带确认标记，否则之前写出的那些页永远不会被认。
//
// 日志写完的页顺手放进缓存，本地页表清空，内存交还。
//
// 拼空页链和提交回调改的是所有事务共用的那份头页，所以提交先把头页存一份，
// 失败就还原——否则随后谁再写头页，都会把这半截改动带进日志。写日志之前失败的，
// 盘上什么也没动，只让本事务失败；提交时写日志失败，盘上留下了什么说不准，
// 实例封死。实例封死之后不再往日志写任何东西。
func (t *Transaction) persist(confirm bool) (n int, err error) {
	if err := t.core.checkBroken(); err != nil {
		return 0, err
	}
	if confirm {
		// 头页没改动的提交不会动头页，省掉这份拷贝。
		if t.headerChanged {
			save := t.core.header.Savepoint()
			defer func() {
				if err != nil {
					err = errors.Join(err, t.core.header.Restore(save))
				}
			}()
		}
		if err := t.spliceDeleted(); err != nil {
			return 0, err
		}
	}

	pages := t.collectDirty()
	// 安全点写出过页而此刻没有新脏页时，头页没改也要写：确认标记总得有一页来带。
	writeHeader := confirm && (t.headerChanged || (len(pages) == 0 && len(t.pages.dirty) > 0))
	if len(pages) == 0 && !writeHeader {
		return 0, nil
	}

	markLast := confirm && !writeHeader

	bufs := make([][]byte, 0, len(pages)+1)
	ids := make([]uint32, 0, len(pages)+1)
	// 打标记也会碰集合页的改动标记，诊断视图读得到它。
	t.mu.Lock()
	for i, p := range pages {
		p.SetTransactionID(t.id)
		p.SetConfirmed(markLast && i == len(pages)-1)
		bufs = append(bufs, p.Bytes())
		ids = append(ids, p.ID())
	}
	t.mu.Unlock()

	var headerClone []byte
	if writeHeader {
		for _, fn := range t.onCommit {
			if err := fn(t.core.header); err != nil {
				return 0, err
			}
		}
		if err := t.core.header.Flush(); err != nil {
			return 0, err
		}
		t.core.header.SetTransactionID(t.id)
		t.core.header.SetConfirmed(true)

		headerClone = bytes.Clone(t.core.headerBuf)
		bufs = append(bufs, headerClone)
		ids = append(ids, 0)

		t.core.header.SetTransactionID(xpage.EmptyPageID)
		t.core.header.SetConfirmed(false)
	}

	offs, err := t.core.disk.AppendLogPages(bufs...)
	if err != nil {
		if confirm {
			t.core.markBroken(err)
		}
		return 0, err
	}
	t.mu.Lock()
	for i, off := range offs {
		t.pages.dirty[ids[i]] = off

		t.core.cache.Put(OriginLog, off, bufs[i])
	}
	t.mu.Unlock()
	if confirm {
		if err := t.core.disk.CommitLog(); err != nil {
			t.core.markBroken(err)
			return 0, err
		}
	}

	t.dropLocalPages()
	return len(offs), nil
}

// spliceDeleted 把本事务删掉的那串页接到空页链的最前面。
//
// 只改链尾那一页的后继和头页的链首，中间的链在删除时就已经串好了。
func (t *Transaction) spliceDeleted() error {
	if t.pages.lastDelete == xpage.EmptyPageID {
		return nil
	}
	s := t.snapshots[xpage.FoldName(t.pages.lastDeleteColl)]
	if s == nil {
		return fmt.Errorf("xtx: snapshot %q holding the deleted page chain is gone",
			t.pages.lastDeleteColl)
	}
	p, err := s.GetPage(t.pages.lastDelete)
	if err != nil {
		return fmt.Errorf("xtx: splice deleted pages: %w", err)
	}
	p.SetNextPageID(t.core.header.FreeEmptyPageList())
	t.core.header.SetFreeEmptyPageList(t.pages.firstDelete)
	t.headerChanged = true

	t.pages.firstDelete = xpage.EmptyPageID
	t.pages.lastDelete = xpage.EmptyPageID
	return nil
}

// collectDirty 收集所有该落盘的脏页。
//
// **次序是定死的**：集合名排序，每个集合内页号排序。同样的操作因此
// 写出同样的日志，重放的结果也一致。共享的页不算——那是别人的。
func (t *Transaction) collectDirty() []*xpage.Page {
	names := make([]string, 0, len(t.snapshots))
	for n, s := range t.snapshots {
		if s.mode == ModeWrite {
			names = append(names, n)
		}
	}
	slices.Sort(names)

	var out []*xpage.Page
	for _, n := range names {
		s := t.snapshots[n]
		ids := make([]uint32, 0, len(s.local))
		for id, lp := range s.local {
			if lp.page.Dirty() && !lp.shared {
				ids = append(ids, id)
			}
		}
		slices.Sort(ids)
		for _, id := range ids {
			out = append(out, s.local[id].page)
		}

		if s.collection != nil && s.collection.Dirty() {
			out = append(out, s.collection.Page)
		}
	}
	return out
}

// dropLocalPages 清空各快照的本地页表并把占用计数归零。
func (t *Transaction) dropLocalPages() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, s := range t.snapshots {
		clear(s.local)
		t.pages.size = 0
	}
}

// returnNewPages 把本事务新扩出来的页还回空页链。
//
// 这本身就是一次小事务：另取一个事务号，把这些页写成空页串成链，
// 连同改过的头页一起写日志、落盘、确认。中途任何一步失败都把头页恢复到
// 动手之前的样子——否则内存里的头页会指向一条根本没落盘的链。
//
// 实例已经封死时什么也不写，只丢内存状态：那次失败的提交落没落进日志说不准，
// 照内存里的头页再确认一份，重放时可能与它的数据页对不上。新扩的页宁可泄漏。
// 实例已经关掉的同样不写，文件都关了。
func (t *Transaction) returnNewPages() error {
	t.core.headerMu.Lock()
	defer t.core.headerMu.Unlock()
	if t.core.broken.Load() != nil || t.core.closed.Load() {
		return nil
	}

	txID := t.core.wal.NextTransactionID()
	save := t.core.header.Savepoint()

	bufs := make([][]byte, 0, len(t.pages.newPages)+1)
	ids := make([]uint32, 0, len(t.pages.newPages)+1)
	for i, id := range t.pages.newPages {
		next := t.core.header.FreeEmptyPageList()
		if i < len(t.pages.newPages)-1 {
			next = t.pages.newPages[i+1]
		}
		buf := getBuf()
		p, err := xpage.PageEmpty.NewPage(buf, id)
		if err != nil {
			return errors.Join(err, t.core.header.Restore(save))
		}
		p.SetNextPageID(next)
		p.SetTransactionID(txID)
		bufs = append(bufs, buf)
		ids = append(ids, id)
	}
	t.core.header.SetTransactionID(txID)
	t.core.header.SetFreeEmptyPageList(t.pages.newPages[0])
	t.core.header.SetConfirmed(true)
	if err := t.core.header.Flush(); err != nil {
		return errors.Join(err, t.core.header.Restore(save))
	}
	bufs = append(bufs, bytes.Clone(t.core.headerBuf))
	ids = append(ids, 0)
	t.core.header.SetTransactionID(xpage.EmptyPageID)
	t.core.header.SetConfirmed(false)

	offs, err := t.core.disk.AppendLogPages(bufs...)
	if err != nil {
		return errors.Join(err, t.core.header.Restore(save))
	}
	if err := t.core.disk.CommitLog(); err != nil {
		return errors.Join(err, t.core.header.Restore(save))
	}
	positions := make([]xwal.PagePosition, len(offs))
	for i, off := range offs {
		positions[i] = xwal.PagePosition{PageID: ids[i], Offset: off}
		t.core.cache.Put(OriginLog, off, bufs[i])
	}
	t.core.wal.Confirm(txID, positions)
	t.mu.Lock()
	t.pages.newPages = nil
	t.mu.Unlock()
	return nil
}

const (
	// forceCheckpointFactor 是日志涨到自动检查点阈值的几倍时，提交改为排队等排他闸门。
	forceCheckpointFactor = 4

	// forceCheckpointWait 是提交排队等排他闸门最多等多久。
	forceCheckpointWait = time.Second
)

// maybeCheckpoint 日志够长时做一次检查点。
//
// 平时只试一下排他闸门，拿不到就算了，下次再说。可只要事务前后交叠地开着——
// 哪怕只是没走完的游标——就永远拿不到，日志无界地涨。所以日志涨到阈值的
// [forceCheckpointFactor] 倍时改为排队等，最多等 [forceCheckpointWait]：排队期间
// 新事务先等着，在途的走完就轮到检查点。等不到多半是本协程自己还开着别的事务，
// 下一次排队推到日志再翻一倍时，免得每次提交都陪着白等。
func (t *Transaction) maybeCheckpoint() error {
	c := t.core
	n := c.CheckpointPages()
	if n <= 0 {
		return nil
	}
	pages := c.disk.LogPageCount()
	if pages < int64(n) {
		return nil
	}
	if !c.locks.TryEnterExclusive() {
		if pages < max(int64(n)*forceCheckpointFactor, c.forceCheckpointAt.Load()) {
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), forceCheckpointWait)
		err := c.locks.enterExclusive(ctx, false)
		cancel()
		if err != nil {
			c.forceCheckpointAt.Store(2 * pages)
			return nil
		}
	}
	defer func() { _ = c.locks.ExitExclusive() }()
	_, err := c.checkpointLocked()
	return err
}

// OnCommit 登记一件提交时要对头页做的事，并把头页标成有改动。
func (t *Transaction) OnCommit(fn func(*xpage.HeaderPage) error) {
	t.onCommit = append(t.onCommit, fn)
	t.headerChanged = true
}

// PersistWithoutCommit 把脏页刷进日志但不确认。与 [Transaction.Safepoint] 的区别是它不看配额，一定刷。
func (t *Transaction) PersistWithoutCommit() error {
	if err := t.checkActive(); err != nil {
		return err
	}
	t.core.headerMu.Lock()
	defer t.core.headerMu.Unlock()
	_, err := t.persist(false)
	return err
}

// info 汇总本事务的信息。快照按名字排序，好让输出稳定。
//
// 它从别的协程调来，属主可能正改着这些，所以要拿 t.mu。
func (t *Transaction) info() TxInfo {
	t.mu.Lock()
	defer t.mu.Unlock()
	mode := "Read"
	for _, s := range t.snapshots {
		if s.mode == ModeWrite {
			mode = "Write"
			break
		}
	}
	inf := TxInfo{
		ID: t.id, StartTime: t.started, Mode: mode,
		Size: t.pages.size, Quota: t.quota,
		LoggedPages:  len(t.pages.dirty),
		NewPages:     len(t.pages.newPages),
		DeletedPages: t.pages.deleted,
	}
	names := make([]string, 0, len(t.snapshots))
	for k := range t.snapshots {
		names = append(names, k)
	}
	slices.Sort(names)
	for _, k := range names {
		s := t.snapshots[k]
		si := SnapshotInfo{TxID: t.id, Name: s.name, Mode: "Read", Version: s.version, Pages: len(s.local)}
		if s.mode == ModeWrite {
			si.Mode = "Write"
		}
		if s.collection != nil {
			si.CollectionDirty = s.collection.Dirty()
		}
		inf.Snapshots = append(inf.Snapshots, si)
		inf.ModifiedPages += si.Pages
	}
	return inf
}
