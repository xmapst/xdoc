package xdoc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/xmapst/xdoc/internal/xdisk"
	"github.com/xmapst/xdoc/internal/xpage"
	"github.com/xmapst/xdoc/internal/xtx"
)

// RebuildResult 汇报一次重建做了什么。
type RebuildResult struct {
	// Before、After 是重建前后的数据文件字节数。
	Before, After int64

	// Collections、Documents、Indexes 是搬过去的集合数、文档数与索引数。
	//
	// Indexes 不含主键索引——那个是新库自己建的。
	Collections, Documents, Indexes int

	// Backup 是原文件被改名后的路径。
	//
	// 它不会自动删：重建之后先核对新文件，确认无误再动它。
	Backup string

	// Salvaged 列出走了抢救路径的集合，即正常遍历读不完的那些。
	//
	// 这份名单非空就说明原文件有损坏，即使 error 是 nil。
	Salvaged []string

	// Skipped 是抢救时跳过的每一处，同时也会写进 [RebuildErrorsCollection]。
	Skipped []SalvageSkip
}

// Reclaimed 是重建省下来的字节数。
//
// 结果**可能是负的**：碎片本来就少的库，重建后反而可能因为页分配的落点不同而略大。
func (r RebuildResult) Reclaimed() int64 { return r.Before - r.After }

// Rebuild 把一份库文件整个重写一遍：回收碎片、丢掉删除留下的空洞，
// 顺带抢救读不出来的部分。
//
// 调用时这份文件不能被打开着。
//
// 改口令或改排序规则也走这里——那两样是整份文件的属性，只能在重写时换。
//
// 原文件不会被删，只是改名成备份；换名失败时会把已经做的改名一步步退回去。
func Rebuild(path string, opts ...Option) (RebuildResult, error) {
	return newOptions(opts).rebuildFile(path, opts)
}

// withOptions 把整份配置打包成一个选项，用来给重建出来的新库当起点。
func (o options) withOptions() Option { return func(dst *options) { *dst = o } }

// rebuildFile 是重建的主过程：建临时文件、搬内容、换名。
//
// 新库先沿用源库的排序规则，再让调用方给的选项覆盖——不然换排序规则就没法做了。
// 它以独占方式打开：重建期间不该有第二个连接进来。
//
// 一篇集合都没搬到、而源文件却分配过页时判定为失败，原文件保持不动：
// 那多半是集合表坏了，此时"成功"地产出一个空库等于把数据丢干净。
//
// 换名分三步（日志、数据、临时文件），每一步失败都把前面的退回去，
// 所以中途出错不会留下一个只换了一半的现场。
func (o options) rebuildFile(path string, dstOpts []Option) (RebuildResult, error) {
	var res RebuildResult
	if fi, err := os.Stat(path); err == nil {
		res.Before = fi.Size()
	} else {
		return res, fmt.Errorf("xdoc: rebuild %q: %w", path, err)
	}

	if o.readOnly {
		return res, errors.New("xdoc: cannot rebuild a database opened read-only")
	}
	src, err := o.openForRebuild(path)
	if err != nil {
		return res, err
	}
	defer src.Close()

	ctx := context.Background()

	tmp := path + "-rebuild.tmp"
	_ = os.Remove(tmp)
	_ = os.Remove(xdisk.LogPath(tmp))

	dstList := append([]Option{withCollation(src.core.Collation())}, dstOpts...)

	dstList = append(dstList, WithConnection(ConnectionDirect))
	dst, err := Open(tmp, dstList...)
	if err != nil {
		return res, fmt.Errorf("xdoc: rebuild: create %q: %w", tmp, err)
	}
	cleanup := func() {
		_ = dst.Close()
		_ = os.Remove(tmp)
		_ = os.Remove(xdisk.LogPath(tmp))
	}

	if res, err = src.copyAll(ctx, dst, res); err != nil {
		cleanup()
		return res, err
	}

	if res, err = src.copyOrphans(ctx, dst, res); err != nil {
		cleanup()
		return res, err
	}

	if res.Collections == 0 {
		lastPage, herr := src.core.HeaderValue(func(h *xpage.HeaderPage) (uint32, error) { return h.LastPageID(), nil })
		if herr != nil {
			cleanup()
			return res, fmt.Errorf("xdoc: rebuild: read source header: %w", herr)
		}
		if lastPage > 0 {
			cleanup()
			return res, fmt.Errorf(
				"xdoc: rebuild: %q allocated %d pages but no collection could be read; "+
					"either its collection table is damaged or every collection was dropped — "+
					"the original file was left untouched",
				path, lastPage+1)
		}
	}

	if len(res.Skipped) > 0 {
		if err := dst.writeRebuildErrors(ctx, res.Skipped, o.nowOrDefault()); err != nil {
			cleanup()
			return res, fmt.Errorf("xdoc: rebuild: write %s: %w", RebuildErrorsCollection, err)
		}
	}

	var (
		srcTimeout time.Duration
		srcCkpt    int32
		srcLimit   int64
		srcUTC     bool
		srcUV      int32
	)
	if herr := src.core.WithHeader(func(h *xpage.HeaderPage) error {
		srcTimeout, srcCkpt = h.Timeout(), h.Checkpoint()
		srcLimit, srcUTC, srcUV = h.LimitSize(), h.UTCDate(), h.UserVersion()
		return nil
	}); herr != nil {
		cleanup()
		return res, fmt.Errorf("xdoc: rebuild: read source settings: %w", herr)
	}
	ptx, err := dst.core.Begin(ctx)
	if err != nil {
		cleanup()
		return res, fmt.Errorf("xdoc: rebuild: carry settings: %w", err)
	}
	ptx.OnCommit(func(h *xpage.HeaderPage) error {
		h.SetTimeout(srcTimeout)
		h.SetCheckpoint(srcCkpt)
		h.SetLimitSize(srcLimit)
		h.SetUTCDate(srcUTC)
		h.SetUserVersion(srcUV)
		return nil
	})
	if err := ptx.Commit(); err != nil {
		cleanup()
		return res, fmt.Errorf("xdoc: rebuild: carry settings: %w", err)
	}
	if _, err := dst.Checkpoint(ctx); err != nil {
		cleanup()
		return res, fmt.Errorf("xdoc: rebuild: checkpoint target: %w", err)
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(tmp)
		return res, fmt.Errorf("xdoc: rebuild: close target: %w", err)
	}

	if err := src.Close(); err != nil {
		_ = os.Remove(tmp)
		return res, fmt.Errorf("xdoc: rebuild: close source: %w", err)
	}
	if fi, err := os.Stat(tmp); err == nil {
		res.After = fi.Size()
	}

	bak, bakLog := xdisk.BackupPath(path), xdisk.BackupPath(xdisk.LogPath(path))
	_ = os.Remove(bak)
	_ = os.Remove(bakLog)
	if _, serr := os.Stat(xdisk.LogPath(path)); serr == nil {
		if err := os.Rename(xdisk.LogPath(path), bakLog); err != nil {
			_ = os.Remove(tmp)
			return res, fmt.Errorf("xdoc: rebuild: back up log of %q: %w", path, err)
		}
	}
	if err := os.Rename(path, bak); err != nil {
		_ = os.Rename(bakLog, xdisk.LogPath(path))
		_ = os.Remove(tmp)
		return res, fmt.Errorf("xdoc: rebuild: back up %q: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Rename(bak, path)
		_ = os.Rename(bakLog, xdisk.LogPath(path))
		_ = os.Remove(tmp)
		return res, fmt.Errorf("xdoc: rebuild: replace %q: %w", path, err)
	}
	res.Backup = bak
	_ = os.Remove(xdisk.LogPath(tmp))
	return res, nil
}

// openForRebuild 以容错方式打开源库。
//
// 忽略状态异常、不检查排序规则、不预分配：源文件本来就可能是坏的，
// 开的时候越挑剔，能救回来的越少。
func (o options) openForRebuild(path string) (*DB, error) {
	oo := o.openOptions()
	oo.IgnoreInvalidState = true

	oo.CollationSet = false

	oo.InitialSize = 0
	core, err := oo.OpenFile(path)
	if err != nil {
		return nil, fmt.Errorf("xdoc: rebuild: open %q: %w", path, err)
	}
	db := o.newDB(core)
	db.dataPath = path
	return db, nil
}

// copyAll 把集合表里登记的每个集合搬到新库。
//
// 索引先建再灌数据。正常搬运中途出错就改走抢救路径：把这个集合在新库里
// 整个丢掉重来，已经计入的文档数也一并扣回去，免得重复计数。
func (db *DB) copyAll(ctx context.Context, dst *DB, res RebuildResult) (RebuildResult, error) {
	for _, name := range db.CollectionNames() {
		ixs, err := db.engine.Indexes(ctx, name)
		if err != nil {
			return res, fmt.Errorf("xdoc: rebuild: read indexes of %q: %w", name, err)
		}
		makeIndexes := func() (int, error) {
			to := dst.Collection(name)
			n := 0
			for _, ix := range ixs {
				if ix.Primary {
					continue
				}
				if _, err := to.EnsureIndex(ctx, ix.Name, ix.Expression, ix.Unique); err != nil {
					return n, fmt.Errorf("xdoc: rebuild: create index %q on %q: %w",
						ix.Name, name, err)
				}
				n++
			}
			return n, nil
		}
		nix, err := makeIndexes()
		if err != nil {
			return res, err
		}
		res.Indexes += nix

		moved, cerr := db.copyDocuments(ctx, dst, name)
		res.Documents += moved
		if cerr != nil {
			skips, moved2, serr := db.salvageCollection(ctx, dst, name, makeIndexes)
			res.Documents -= moved
			res.Documents += moved2
			res.Salvaged = append(res.Salvaged, name)
			res.Skipped = append(res.Skipped, skips...)
			if serr != nil {
				return res, fmt.Errorf("xdoc: rebuild: salvage %q after %v: %w", name, cerr, serr)
			}
		}
		res.Collections++
	}
	return res, nil
}

// copyDocuments 按主键顺序整段搬一个集合。
//
// 关掉主键自动生成：搬过来的文档要保住原来的主键。
//
// 错误里区分 read 与 write：读错说明源文件坏了，该转抢救；写错说明新库有问题，
// 转抢救也救不了。
func (db *DB) copyDocuments(ctx context.Context, dst *DB, name string) (int, error) {
	const batch = 500
	to := dst.Collection(name).WithAutoID(AutoIDNone)
	moved := 0
	buf := make([]*Document, 0, batch)
	flush := func() error {
		if len(buf) == 0 {
			return nil
		}

		if _, err := to.Insert(ctx, buf...); err != nil {
			return err
		}
		moved += len(buf)
		buf = buf[:0]
		return nil
	}
	for d, err := range db.Collection(name).All(ctx, Asc) {
		if err != nil {
			return moved, fmt.Errorf("read: %w", err)
		}
		buf = append(buf, d)
		if len(buf) >= batch {
			if err := flush(); err != nil {
				return moved, fmt.Errorf("write: %w", err)
			}
		}
	}
	if err := flush(); err != nil {
		return moved, fmt.Errorf("write: %w", err)
	}
	return moved, nil
}

// salvageCollection 绕开索引与页链，直接扫数据页把还能读的文档捡出来。
//
// 先把新库里这个集合丢掉重来：正常搬运可能已经写进去一部分，留着就会重复。
//
// 索引留到最后再建：一边扫一边维护索引，会让一篇坏文档连累整批。
func (db *DB) salvageCollection(ctx context.Context, dst *DB, name string,
	makeIndexes func() (int, error)) ([]SalvageSkip, int, error) {
	if _, err := dst.DropCollection(ctx, name); err != nil {
		return nil, 0, fmt.Errorf("clear target: %w", err)
	}
	stx, err := db.core.Begin(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("open source snapshot: %w", err)
	}
	defer stx.Rollback()
	s, err := stx.Snapshot(ctx, name, xtx.ModeRead, false)
	if err != nil {
		return nil, 0, fmt.Errorf("open source snapshot: %w", err)
	}
	cp := s.CollectionPage()
	if cp == nil {
		return nil, 0, fmt.Errorf("collection page of %q is unreadable", name)
	}
	colID := cp.ID()

	const batch = 500
	to := dst.Collection(name).WithAutoID(AutoIDNone)
	moved := 0
	buf := make([]*Document, 0, batch)
	var writeErr error
	flush := func() bool {
		if len(buf) == 0 {
			return true
		}
		if _, err := to.Insert(ctx, buf...); err != nil {
			writeErr = err
			return false
		}
		moved += len(buf)
		buf = buf[:0]
		return true
	}
	skips := salvage{s}.scan(colID, func(d *Document) bool {
		buf = append(buf, d)
		if len(buf) >= batch {
			return flush()
		}
		return true
	})
	if writeErr == nil {
		flush()
	}
	if writeErr != nil {
		return skips, moved, fmt.Errorf("write salvaged documents: %w", writeErr)
	}

	if _, err := makeIndexes(); err != nil {
		return skips, moved, err
	}
	return skips, moved, nil
}

// Rebuild 重建当前这个库，完成后这个句柄继续指向重建后的文件。
//
// 内存库直接返回空结果——没有文件可重写。
//
// 会先丢掉 BEGIN 开出来的那个事务：文件马上要被整个换掉，那个事务已经无处提交。
func (db *DB) Rebuild(opts ...Option) (RebuildResult, error) {
	if db.dataPath == "" {
		return RebuildResult{}, nil
	}
	if db.opts.readOnly {
		return RebuildResult{}, errors.New("xdoc: cannot rebuild a database opened read-only")
	}

	o := db.opts
	for _, fn := range opts {
		if fn != nil {
			fn(&o)
		}
	}
	if o.collErr != nil {
		return RebuildResult{}, o.collErr
	}

	db.sqlMu.Lock()
	db.sqlTx = nil
	db.sqlMu.Unlock()

	if db.shared != nil {
		return db.rebuildShared(o, opts)
	}

	if err := db.Close(); err != nil {
		return RebuildResult{}, err
	}

	res, err := db.opts.rebuildFile(db.dataPath, append([]Option{db.opts.withOptions()}, opts...))
	if err != nil {
		return res, err
	}

	core, err := o.openOptions().OpenFile(db.dataPath)
	if err != nil {
		return res, err
	}
	nd := o.newDB(core)
	nd.dataPath = db.dataPath

	db.core, db.engine, db.mapper, db.auto, db.password, db.opts =
		nd.core, nd.engine, nd.mapper, nd.auto, nd.password, nd.opts
	db.execOnce = sync.Once{}
	db.exec = nil
	return res, nil
}

// rebuildShared 在共享连接模式下重建。
//
// 先拿到跨进程锁再关掉底层文件：重建期间别的进程不能碰它。
// 锁本身留着不放，所以这个句柄之后还能接着用。
func (db *DB) rebuildShared(o options, opts []Option) (RebuildResult, error) {
	s := db.shared

	if err := s.lock.Acquire(context.Background()); err != nil {
		return RebuildResult{}, err
	}
	defer s.lock.Release()

	s.mu.Lock()
	rel := s.txRelease
	s.txRelease = nil
	s.txRunning = false
	s.mu.Unlock()
	if rel != nil {
		rel()
	}

	s.mu.Lock()
	if s.open {
		db.closeCore()
		s.open = false
	}
	s.mu.Unlock()

	res, err := db.opts.rebuildFile(db.dataPath, append([]Option{db.opts.withOptions()}, opts...))
	if err != nil {
		return res, err
	}

	db.mapper, db.auto, db.password, db.opts = o.mapper, o.auto, o.password, o
	return res, nil
}

// orphanName 给一个只剩编号、没有名字的集合起个名。
func orphanName(colID uint32) string { return "col_" + strconv.FormatUint(uint64(colID), 10) }

// copyOrphans 找出数据页上有内容、集合表里却没有登记的集合编号，把它们也救出来。
//
// 集合表在头页，只有一页；它坏掉时数据页还是好的。这一步捡回来的就是
// 那些"数据还在，只是没人认领"的文档，落在 col_<编号> 这样的集合里。
func (db *DB) copyOrphans(ctx context.Context, dst *DB, res RebuildResult) (RebuildResult, error) {
	known := make(map[uint32]bool)
	if err := db.core.WithHeader(func(h *xpage.HeaderPage) error {
		for _, id := range h.Collections() {
			known[id] = true
		}
		return nil
	}); err != nil {
		return res, fmt.Errorf("xdoc: rebuild: read source collection table: %w", err)
	}

	stx, err := db.core.Begin(ctx)
	if err != nil {
		return res, fmt.Errorf("xdoc: rebuild: scan for orphan collections: %w", err)
	}
	defer stx.Rollback()

	s, err := stx.Snapshot(ctx, "\x00orphan-scan", xtx.ModeRead, false)
	if err != nil {
		return res, fmt.Errorf("xdoc: rebuild: scan for orphan collections: %w", err)
	}
	orphanCols := salvage{s}.orphans(known)
	for _, colID := range orphanCols {
		name := orphanName(colID)

		if _, err := dst.DropCollection(ctx, name); err != nil {
			return res, fmt.Errorf("xdoc: rebuild: clear %q: %w", name, err)
		}
		moved, skips, err := dst.copySalvagedByID(ctx, s, colID, name)
		res.Documents += moved
		res.Salvaged = append(res.Salvaged, name)
		res.Skipped = append(res.Skipped, skips...)
		if err != nil {
			return res, err
		}
		res.Collections++
	}
	return res, nil
}

// copySalvagedByID 按集合编号扫数据页，把捡到的文档分批写进新库。
func (db *DB) copySalvagedByID(ctx context.Context, s *xtx.Snapshot,
	colID uint32, name string) (int, []SalvageSkip, error) {
	const batch = 500
	to := db.Collection(name).WithAutoID(AutoIDNone)
	moved := 0
	buf := make([]*Document, 0, batch)
	var writeErr error
	flush := func() bool {
		if len(buf) == 0 {
			return true
		}
		if _, err := to.Insert(ctx, buf...); err != nil {
			writeErr = err
			return false
		}
		moved += len(buf)
		buf = buf[:0]
		return true
	}
	skips := salvage{s}.scan(colID, func(d *Document) bool {
		buf = append(buf, d)
		if len(buf) >= batch {
			return flush()
		}
		return true
	})
	if writeErr == nil {
		flush()
	}
	if writeErr != nil {
		return moved, skips, fmt.Errorf("xdoc: rebuild: write %q: %w", name, writeErr)
	}
	return moved, skips, nil
}
