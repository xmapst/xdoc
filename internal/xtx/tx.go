package xtx

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
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

// checkActive 确认事务还开着。
func (t *Transaction) checkActive() error {
	if t.state != StateActive {
		return fmt.Errorf("%w (state %d)", ErrTransactionClosed, t.state)
	}
	return nil
}

// Commit 提交事务。
//
// 先把脏页连同头页写进日志并落盘，然后在日志索引里把这个事务标成已确认——
// **确认这一步才是提交点**：确认之前崩掉，检查点会跳过这些页；确认之后崩掉，
// 重启时能从日志把它们捞回来。
//
// 中途出错就把实例封死并回滚：此时内存里的头页与盘上可能已经不一致。
// 提交成功后视日志长度顺手做一次检查点。
func (t *Transaction) Commit() error {
	if err := t.checkActive(); err != nil {
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
	if err != nil {
		t.core.markBroken(err)
	}
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
func (t *Transaction) Rollback() error {
	if err := t.checkActive(); err != nil {
		return err
	}
	var err error
	if len(t.pages.newPages) > 0 {
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
// 恢复时看到确认标记才认这一批。
//
// 日志写完的页顺手放进缓存，本地页表清空，内存交还。
func (t *Transaction) persist(confirm bool) (int, error) {
	if confirm {
		if err := t.spliceDeleted(); err != nil {
			return 0, err
		}
	}

	pages := t.collectDirty()
	if len(pages) == 0 && !(confirm && t.headerChanged) {
		return 0, nil
	}

	markLast := confirm && !t.headerChanged

	bufs := make([][]byte, 0, len(pages)+1)
	ids := make([]uint32, 0, len(pages)+1)
	for i, p := range pages {
		p.SetTransactionID(t.id)
		p.SetConfirmed(markLast && i == len(pages)-1)
		bufs = append(bufs, p.Bytes())
		ids = append(ids, p.ID())
	}

	var headerClone []byte
	if confirm && t.headerChanged {
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
		return 0, err
	}
	for i, off := range offs {
		t.pages.dirty[ids[i]] = off

		t.core.cache.Put(OriginLog, off, bufs[i])
	}
	if confirm {
		if err := t.core.disk.CommitLog(); err != nil {
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
func (t *Transaction) returnNewPages() error {
	t.core.headerMu.Lock()
	defer t.core.headerMu.Unlock()

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
	t.pages.newPages = nil
	return nil
}

// maybeCheckpoint 日志够长且能立刻拿到排他闸门时做一次检查点。拿不到就算了，下次再说。
func (t *Transaction) maybeCheckpoint() error {
	n := t.core.CheckpointPages()
	if n <= 0 {
		return nil
	}
	if t.core.disk.LogPageCount() < int64(n) {
		return nil
	}
	if !t.core.locks.TryEnterExclusive() {
		return nil
	}
	defer func() { _ = t.core.locks.ExitExclusive() }()
	_, err := t.core.checkpointLocked()
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
func (t *Transaction) info() TxInfo {
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
