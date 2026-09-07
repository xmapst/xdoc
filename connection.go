package xdoc

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"sync"

	"github.com/xmapst/xdoc/internal/xlock"
	"github.com/xmapst/xdoc/internal/xtx"
)

// ConnectionType 决定一个库句柄怎么持有底层文件。
type ConnectionType int

const (
	// ConnectionDirect 直连：本进程独占这个文件，句柄一直开着。默认。
	//
	// **跨进程没有任何保护**。两个进程同时直连同一个文件，各有一份内存里的头页、
	// 各自分配页号、各自推进提交序号，写出的日志互相覆盖——后关的那个赢，
	// 先关那个提交过的东西连同整个集合一起消失，而两侧的写入当时都返回成功。
	// 要跨进程共用一个库，用 [ConnectionShared]。
	ConnectionDirect ConnectionType = iota

	// ConnectionShared 共享：每个操作前取一把跨进程的锁、把库开出来，做完关掉再放锁。
	//
	// 多个进程可以轮流读写同一个库，代价是每个操作都要开关一次库——那笔开销几乎全是
	// fsync，不随库的大小增长。
	//
	// 几条不合直觉但确定的行为：打开时一个字节都不碰（文件不存在、口令不对这些全都
	// 推迟到第一个操作才报）；内存库在这个模式下每个操作都从一个全新的空库开始，
	// 写什么都留不住；事务期间独占，见 [leakTransactionLockDepth]。
	ConnectionShared
)

// String 返回连接方式的名字。
func (c ConnectionType) String() string {
	switch c {
	case ConnectionDirect:
		return "Direct"
	case ConnectionShared:
		return "Shared"
	}
	return fmt.Sprintf("ConnectionType(%d)", int(c))
}

// WithConnection 指定这个句柄的连接方式，不给就是 [ConnectionDirect]。
func WithConnection(c ConnectionType) Option {
	return func(o *options) { o.conn = c }
}

// sharedState 是共享模式下的那把跨进程锁与它守着的开关状态。
type sharedState struct {
	// lock 是按数据文件路径认的跨进程锁。
	lock *xlock.Lock

	// mu 守下面几个字段：跨进程锁挡的是别的进程，本进程内的并发还得自己挡。
	mu sync.Mutex

	// open 表示底层核心此刻是开着的。
	open bool

	// txRunning 表示本句柄有一个事务正在进行。
	txRunning bool

	// txRelease 是开事务那一次取锁对应的释放函数，留到事务收尾时才调。
	txRelease func()
}

// enter 取得执行一个操作所需的持有权，返回对应的释放函数。
//
// 直连模式下是空操作。共享模式下取跨进程锁，必要时把库开出来。
//
// 事务进行中的分支见 [leakTransactionLockDepth]：那时返回的释放函数**什么都不做**，
// 锁多攥了一次却不还。
func (db *DB) enter(ctx context.Context) (func(), error) {
	s := db.shared
	if s == nil {
		return func() {}, nil
	}
	if err := s.lock.Acquire(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.txRunning {
		if leakTransactionLockDepth {
			return func() {}, nil
		}
		return func() { s.lock.Release() }, nil
	}
	if s.open {
		return func() { s.lock.Release() }, nil
	}
	if err := db.openCore(); err != nil {
		s.lock.Release()
		return nil, err
	}
	s.open = true
	return func() { db.exitLocked() }, nil
}

// exitLocked 关掉核心并放锁，是共享模式下一个普通操作的收尾。
//
// 事务进行中不关核心：那个事务还要继续用它。
func (db *DB) exitLocked() {
	s := db.shared
	s.mu.Lock()
	if !s.txRunning && s.open {
		db.closeCore()
		s.open = false
	}
	s.mu.Unlock()
	s.lock.Release()
}

// leakTransactionLockDepth 决定共享模式下事务内的操作要不要归还它取的那次锁。
//
// 为真时不还：事务里每个操作都把锁多攥一次，而提交只还一次——于是**一个做过带
// 写入事务的进程，会把这个库攥到自己退出为止**，别的进程从此全被挡住。
//
// 这是共享模式的既定行为，改动点集中在这一个常量上：设成 false 就没有这回事。
const leakTransactionLockDepth = true

// openCore 把底层核心开出来，内存库与文件库各走一条路。
//
// 带「上次没有干净关闭」标记的文件，只有在 [WithAutoRebuild] 打开时才自动重建
// 再重开一次；否则原样把那个错误交出去。
func (db *DB) openCore() error {
	var (
		core *xtx.Core
		err  error
	)
	if db.dataPath == "" {
		core, err = db.opts.openOptions().OpenMemory()
	} else {
		core, err = db.opts.openOptions().OpenFile(db.dataPath)
		if err != nil && db.opts.autoRebuild && errors.Is(err, xtx.ErrNeedsRebuild) {
			if _, rerr := db.opts.rebuildFile(db.dataPath, []Option{db.opts.withOptions()}); rerr != nil {
				return errors.Join(err, rerr)
			}
			core, err = db.opts.openOptions().OpenFile(db.dataPath)
		}
	}
	if err != nil {
		return err
	}
	nd := db.opts.newDB(core)
	db.core, db.engine = nd.core, nd.engine
	db.execOnce = sync.Once{}
	db.exec = nil
	return nil
}

// closeCore 关掉核心与 SQL 执行器，并把它们连同那个 once 一起清空好让下次重开。
func (db *DB) closeCore() {
	if db.exec != nil {
		_ = db.exec.Close()
	}
	if db.core != nil {
		_ = db.core.Close()
	}
	db.core, db.engine, db.exec = nil, nil, nil
	db.execOnce = sync.Once{}
}

// seqErr 造一个只产出一个错误的迭代器。
//
// 用在"还没开始迭代就已经失败"的地方：接口交出的是迭代器，
// 错误只能顺着迭代交出去。
func seqErr[V any](err error) iter.Seq2[V, error] {
	return func(yield func(V, error) bool) {
		var zero V
		yield(zero, err)
	}
}

// seqOf 把一个已经收好的切片包成迭代器。
func seqOf[V any](vs []V) iter.Seq2[V, error] {
	return func(yield func(V, error) bool) {
		for _, v := range vs {
			if !yield(v, nil) {
				return
			}
		}
	}
}

// enterSeq 把一次迭代整个圈进持有权里：迭代开始时取，迭代结束或中途放弃时还。
//
// 直连模式下直接透传，不多包一层。
func (db *DB) enterSeq[V any](ctx context.Context, run func() iter.Seq2[V, error]) iter.Seq2[V, error] {
	if db.shared == nil {
		return run()
	}
	return func(yield func(V, error) bool) {
		rel, err := db.enter(ctx)
		if err != nil {
			var zero V
			yield(zero, err)
			return
		}
		defer rel()
		for v, e := range run() {
			if !yield(v, e) {
				return
			}
		}
	}
}

// enterTx 为一个事务取得持有权，并把释放函数留到 [DB.exitTx]。
//
// 事务期间别的进程一步也走不了，这正是事务在共享模式下的隔离来源。
func (db *DB) enterTx(ctx context.Context) error {
	if db.shared == nil {
		return nil
	}
	rel, err := db.enter(ctx)
	if err != nil {
		return err
	}
	s := db.shared
	s.mu.Lock()
	s.txRunning = true
	s.txRelease = rel
	s.mu.Unlock()
	return nil
}

// exitTx 归还开事务那一次取的锁，由提交与回滚各调一次（两者互斥，只会调一次）。
func (db *DB) exitTx() {
	if db.shared == nil {
		return
	}
	s := db.shared
	s.mu.Lock()
	rel := s.txRelease
	s.txRelease = nil
	s.txRunning = false
	s.mu.Unlock()
	if rel != nil {
		rel()
	}
}

// closeShared 关掉共享模式的句柄：把核心关掉并把锁彻底放开。
//
// 把 txRunning 一并清掉——关库时不管有没有事务挂着，锁都必须还回去，
// 否则这个库会被一个已经退出的句柄一直攥着。
func (db *DB) closeShared() error {
	s := db.shared
	s.mu.Lock()
	s.txRunning = false
	s.txRelease = nil
	var err error
	if s.open {
		if db.exec != nil {
			err = db.exec.Close()
		}
		if db.core != nil {
			err = errors.Join(err, db.core.Close())
		}
		db.core, db.engine, db.exec = nil, nil, nil
		db.execOnce = sync.Once{}
		s.open = false
		s.mu.Unlock()
		s.lock.Release()
		return err
	}
	s.mu.Unlock()
	return nil
}

// withCore 在持有权之下跑一段不返回错误的操作。
//
// 取不到就静默不跑：调用它的是那些没有错误出口的读取路径（读一项 pragma 之类），
// 那里宁可给一个零值也不该 panic。
func (db *DB) withCore(fn func()) {
	rel, err := db.enter(context.Background())
	if err != nil {
		return
	}
	defer rel()
	fn()
}
