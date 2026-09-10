package xtx

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xmapst/xdoc/internal/xcoll"
	"github.com/xmapst/xdoc/internal/xdisk"
	"github.com/xmapst/xdoc/internal/xerr"
	"github.com/xmapst/xdoc/internal/xpage"
	"github.com/xmapst/xdoc/internal/xwal"
)

// MaxOpenTransactions 是同时能开几个事务。
const MaxOpenTransactions = 100

// MaxTransactionPages 是所有事务加起来能占的页数总额。
const MaxTransactionPages = 100_000

// minTransactionQuota 是单个事务至少能分到的页数，免得事务多时人人饿死。
const minTransactionQuota = 64

// Core 是一个打开着的数据库实例：磁盘、日志索引、页缓存、锁，外加头页。
//
// 一份实例给所有事务共用，各字段自带同步。
type Core struct {
	disk  *xdisk.Disk
	wal   *xwal.Index
	cache *Cache
	locks *LockService

	// headerMu 保护头页；头页只有一份，所有事务读写的都是它。
	headerMu  sync.Mutex
	header    *xpage.HeaderPage
	headerBuf []byte

	coll xcoll.Collation

	// pathKey 是这份数据文件的规范路径，用来认出同一个文件的重复打开。
	pathKey string

	readOnly bool

	// closed 标记实例已经关闭。
	closed atomic.Bool

	// broken 记着让实例作废的那个错误。
	//
	// 提交写到一半失败之后，内存里的头页与盘上可能已经不一致，
	// 再用下去只会越错越远，所以整个实例封死，只能重开。
	broken atomic.Pointer[error]

	// needRebuild 标记文件已经损坏到要重建才能修。
	needRebuild atomic.Bool

	// 以下三项是从头页读来的运行期设置，PRAGMA 改动后会重新加载。
	timeout atomic.Int64

	checkpointPages atomic.Int32

	// forceCheckpointAt 是日志涨到几页时提交才再去排队等排他闸门，0 表示按阈值的
	// [forceCheckpointFactor] 倍。排队落空一次就翻倍，检查点做成后归零。
	forceCheckpointAt atomic.Int64

	dateLoc atomic.Pointer[time.Location]

	// stats 是运行期计数，logger 记少见的事件（为 nil 时不记），见 [Stats]。
	stats  *Stats
	logger *slog.Logger

	// txMu 保护 open 与 budget。
	txMu   sync.Mutex
	open   map[uint32]*Transaction
	budget int
}

// NewCore 组装一个实例，并从头页读入运行期设置。
func NewCore(d *xdisk.Disk, w *xwal.Index, h *xpage.HeaderPage, hbuf []byte,
	cacheSize int, coll xcoll.Collation) *Core {
	return newCore(d, w, h, hbuf, cacheSize, coll, nil, nil)
}

// newCore 同 [NewCore]，计数记进 st（为 nil 时自备一份），事件记到 logger。
func newCore(d *xdisk.Disk, w *xwal.Index, h *xpage.HeaderPage, hbuf []byte,
	cacheSize int, coll xcoll.Collation, st *Stats, logger *slog.Logger) *Core {
	if st == nil {
		st = new(Stats)
	}
	c := &Core{
		disk:      d,
		wal:       w,
		cache:     newCache(cacheSize, st),
		locks:     newLockService(st, logger),
		stats:     st,
		logger:    logger,
		header:    h,
		headerBuf: hbuf,
		coll:      coll,
		open:      map[uint32]*Transaction{},
		budget:    MaxTransactionPages,
	}
	c.loadPragmas(h)
	return c
}

// ReloadPragmas 重新从头页读入运行期设置，PRAGMA 改完后调用。
func (c *Core) ReloadPragmas() {
	_ = c.WithHeader(func(h *xpage.HeaderPage) error { c.loadPragmas(h); return nil })
}

// loadPragmas 把头页上的设置拷进原子变量，好让读取不必抢头页的锁。
func (c *Core) loadPragmas(h *xpage.HeaderPage) {
	c.timeout.Store(int64(h.Timeout()))
	c.checkpointPages.Store(h.Checkpoint())
	loc := time.Local
	if h.UTCDate() {
		loc = time.UTC
	}
	c.dateLoc.Store(loc)
}

// Timeout 返回等锁的超时时长；不为正表示不限时。
func (c *Core) Timeout() time.Duration { return time.Duration(c.timeout.Load()) }

// CheckpointPages 返回日志攒到几页就自动做检查点。
func (c *Core) CheckpointPages() int32 { return c.checkpointPages.Load() }

// DateLocation 返回时间该按哪个时区解读。
func (c *Core) DateLocation() *time.Location { return c.dateLoc.Load() }

// Collation 返回这份库的排序规则。
func (c *Core) Collation() xcoll.Collation { return c.coll }

// ErrClosed 表示实例已经关闭。
var ErrClosed = errors.New("xtx: database is closed")

// markBroken 把实例封死。只记第一个错误——后面那些多半是它引发的。
func (c *Core) markBroken(err error) {
	if c.broken.CompareAndSwap(nil, &err) {
		logEvent(context.Background(), c.logger, slog.LevelError,
			"xdoc: database instance broken; reopen it", slog.Any("error", err))
	}
	c.MarkNeedsRebuild(err)
}

// MarkNeedsRebuild 遇上文件状态错时打上「需要重建」的标记。
func (c *Core) MarkNeedsRebuild(err error) {
	if errors.Is(err, xerr.InvalidDatafileState) {
		c.needRebuild.Store(true)
	}
}

// checkBroken 判断实例还能不能用。
func (c *Core) checkBroken() error {
	if c.closed.Load() {
		return ErrClosed
	}
	if p := c.broken.Load(); p != nil {
		return fmt.Errorf("%w: %v", ErrBroken, *p)
	}
	return nil
}

// ErrBroken 表示实例已经作废，只能重开。
var ErrBroken = errors.New("xtx: database instance is unusable after a failed commit; reopen it")

// Locks 返回锁服务。
func (c *Core) Locks() *LockService { return c.locks }

// Cache 返回页缓存。
func (c *Core) Cache() *Cache { return c.cache }

// WAL 返回日志索引。
func (c *Core) WAL() *xwal.Index { return c.wal }

// WithHeader 在持有头页锁的情况下调 fn。fn 里不要再去拿别的锁。
func (c *Core) WithHeader(fn func(h *xpage.HeaderPage) error) error {
	c.headerMu.Lock()
	defer c.headerMu.Unlock()
	return fn(c.header)
}

// HeaderValue 同 [Core.WithHeader]，但能带一个返回值出来。
func (c *Core) HeaderValue[T any](fn func(h *xpage.HeaderPage) (T, error)) (T, error) {
	c.headerMu.Lock()
	defer c.headerMu.Unlock()
	return fn(c.header)
}

// ErrTransactionClosed 表示事务已经提交或回滚过了。
var ErrTransactionClosed = errors.New("xtx: transaction is already closed")

// Begin 开一个事务。
//
// 要进共享闸门，所以检查点期间会等。开的事务数到上限就报错。
// 每个事务分到一份页数配额：总额除以事务数上限，但不少于 [minTransactionQuota]，
// 也不多于剩下的总额。配额在事务结束时归还。
func (c *Core) Begin(ctx context.Context) (*Transaction, error) {
	if err := c.checkBroken(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()
	if err := c.locks.EnterShared(ctx); err != nil {
		return nil, err
	}

	c.txMu.Lock()
	if len(c.open) >= MaxOpenTransactions {
		c.txMu.Unlock()
		_ = c.locks.ExitShared()
		return nil, fmt.Errorf("%w: %d already open", xwal.ErrTooManyTransactions, len(c.open))
	}
	quota := c.budget / MaxOpenTransactions
	quota = max(quota, minTransactionQuota)
	quota = min(quota, c.budget)
	c.budget -= quota
	id := c.wal.NextTransactionID()
	tx := &Transaction{
		core:      c,
		id:        id,
		quota:     quota,
		started:   time.Now().UTC(),
		snapshots: map[string]*Snapshot{},
		pages: transPages{
			dirty:       map[uint32]int64{},
			firstDelete: xpage.EmptyPageID,
			lastDelete:  xpage.EmptyPageID,
		},
	}
	c.open[id] = tx
	c.txMu.Unlock()
	return tx, nil
}

// release 把事务从表里摘掉，归还配额并出共享闸门。重复调用是安全的。
func (c *Core) release(tx *Transaction) {
	c.txMu.Lock()
	if _, ok := c.open[tx.id]; ok {
		delete(c.open, tx.id)
		c.budget += tx.quota
	}
	c.txMu.Unlock()
	_ = c.locks.ExitShared()
}

// withTimeout 给上下文套上库设的超时；没设超时就原样返回。
func (c *Core) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	d := c.Timeout()
	if d <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, d)
}

// TxInfo 是一个事务的快照式信息，供诊断用。
type TxInfo struct {
	ID        uint32
	StartTime time.Time
	Mode      string

	// Size 是已经占了几页，Quota 是分到几页。
	Size, Quota int

	// LoggedPages 是已经写进日志的页数。
	LoggedPages int

	// NewPages 与 DeletedPages 是本事务新建和删除的页数。
	NewPages, DeletedPages int

	// ModifiedPages 是本事务改过的页数。
	ModifiedPages int

	Snapshots []SnapshotInfo
}

// SnapshotInfo 是一份快照的信息。
type SnapshotInfo struct {
	Name            string
	Mode            string
	Version         int64
	Pages           int
	TxID            uint32
	CollectionDirty bool
}

// Transactions 返回当前所有事务的信息，按事务号排序。
func (c *Core) Transactions() []TxInfo {
	c.txMu.Lock()
	txs := make([]*Transaction, 0, len(c.open))
	for _, t := range c.open {
		txs = append(txs, t)
	}
	c.txMu.Unlock()
	slices.SortFunc(txs, func(a, b *Transaction) int { return cmp.Compare(a.id, b.id) })

	out := make([]TxInfo, 0, len(txs))
	for _, t := range txs {
		out = append(out, t.info())
	}
	return out
}

// OpenTransactions 返回当前开着几个事务。
func (c *Core) OpenTransactions() int {
	c.txMu.Lock()
	defer c.txMu.Unlock()
	return len(c.open)
}

// Budget 返回还剩多少页配额没分出去。
func (c *Core) Budget() int {
	c.txMu.Lock()
	defer c.txMu.Unlock()
	return c.budget
}

// ReadOnly 判断这份库是不是只读打开的。
func (c *Core) ReadOnly() bool { return c.readOnly }

// Disk 返回底层的磁盘门面。
func (c *Core) Disk() *xdisk.Disk { return c.disk }
