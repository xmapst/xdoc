package xdoc

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/xmapst/xdoc/internal/xengine"
	"github.com/xmapst/xdoc/internal/xtx"
)

// ChangeOp 是提交通知里一条变更的种类。
type ChangeOp = xengine.ChangeOp

const (
	// ChangeInsert 插入了一篇文档，[Change.ID] 是它的主键（现发的也已填好）。
	ChangeInsert = xengine.ChangeInsert

	// ChangeUpdate 改写了一篇文档。
	ChangeUpdate = xengine.ChangeUpdate

	// ChangeDelete 删掉了一篇文档。
	ChangeDelete = xengine.ChangeDelete

	// ChangeDeleteAll 清空了集合：DeleteAll、空谓词的 DeleteMany、不带 WHERE 的 SQL DELETE。
	//
	// 不逐篇列主键，免得为清空一张大表在内存里攒下全部主键。
	ChangeDeleteAll = xengine.ChangeDeleteAll

	// ChangeDropCollection 删掉了整个集合。
	ChangeDropCollection = xengine.ChangeDropCollection

	// ChangeRenameCollection 给集合改了名：[Change.Collection] 是旧名，[Change.NewName] 是新名。
	ChangeRenameCollection = xengine.ChangeRenameCollection
)

// Change 是一次提交里的一条变更。
type Change struct {
	// Collection 是写入时给的集合名，大小写照原样。
	Collection string

	Op ChangeOp

	// ID 是文档主键；集合级的种类（清空、删集合、改名）为 nil。
	ID *Value

	// NewName 只在 [ChangeRenameCollection] 上有值。
	NewName string
}

// ChangeSet 是一次提交真正落下去的全部变更，按发生先后排列。
//
// 同一次提交的所有回调共用它，**只读**。
type ChangeSet struct {
	Changes []Change
}

// OnCommit 注册一个提交后回调，返回取消它的函数。
//
// 每次提交落盘成功、锁都放掉之后，在提交者的 goroutine 里**同步**调一次 fn，
// 写入调用要等它返回才返回。回滚、提交失败、ctx 取消都不调；什么都没改到的写入
// （找不到主键的 Update 之类）也不调。所有写路径都算：集合与类型化集合的增删改、
// Upsert、UpdateMany/DeleteMany、事务、SQL（含 BEGIN…COMMIT 与 SELECT INTO）、文件存储。
//
// fn 里可以读写这个库，不会死锁；它自己的写入提交后会再通知一次。fn 拿到的 ctx 是
// 提交那次调用的：[Tx.Commit] 用开事务时的，SQL 的 COMMIT 用那一句的。
//
// **只在进程内、只针对这个句柄**：变更只在内存里攒，别的进程、同一文件的别的句柄
// 上的写入收不到。fn panic 会原样传给写入调用方，但写入已经提交，排在它后面的回调
// 这一次收不到。注册之前已经开始的写入不保证通知。
//
// 取消可以重复调；取消返回之后开始的提交不再调 fn。
func (db *DB) OnCommit(fn func(ctx context.Context, cs ChangeSet)) (cancel func()) {
	return db.hooks.add(fn)
}

// commitHooks 攒着各事务的变更，并在提交后交给注册的回调。
//
// 它是引擎的旁听者（[xengine.Observer]）：引擎报告每条落下去的变更与自开事务的结局。
// 调用方自己开的事务（[Tx]）由 [Tx] 收尾时来取。没有回调时什么都不记。
type commitHooks struct {
	// mu 保护 subs。subs 写时复制，发通知时拿一份快照就放锁。
	mu   sync.Mutex
	subs []*commitSub

	// live 是还没取消的回调数，零时引擎报来的变更直接丢掉。
	live atomic.Int32

	// pending 是各事务攒下的变更，键是 *xtx.Transaction，值是 *[]Change。
	//
	// 一个事务只在一个 goroutine 里用，所以值本身不加锁。npending 让没有待发变更
	// 时的收尾跳过查表。
	pending  sync.Map
	npending atomic.Int32
}

// commitSub 是一个注册的回调。
type commitSub struct {
	fn  func(context.Context, ChangeSet)
	off atomic.Bool
}

var _ xengine.Observer = (*commitHooks)(nil)

// add 注册 fn 并返回取消函数。nil 的 fn 什么也不注册。
func (h *commitHooks) add(fn func(context.Context, ChangeSet)) func() {
	if fn == nil {
		return func() {}
	}
	s := &commitSub{fn: fn}
	h.mu.Lock()
	h.subs = append(slices.Clip(h.subs), s)
	h.mu.Unlock()
	h.live.Add(1)
	return func() {
		if !s.off.CompareAndSwap(false, true) {
			return
		}
		h.mu.Lock()
		h.subs = slices.DeleteFunc(slices.Clone(h.subs), func(x *commitSub) bool { return x == s })
		h.mu.Unlock()
		h.live.Add(-1)
	}
}

// Changed 记下 tx 里的一条变更。ctx 带着 [muteChanges] 标记时不记。
func (h *commitHooks) Changed(ctx context.Context, tx *xtx.Transaction, c xengine.Change) {
	if ctx.Value(muteChangesKey{}) != nil {
		return
	}
	h.note(tx, Change(c))
}

// Finished 收下引擎自开事务的结局：提交了就把变更转进 ctx 上的 [commitOutbox]。
//
// 这里还持着锁，不能调回调；没有信箱（注册晚于写入开始）就丢掉。
func (h *commitHooks) Finished(ctx context.Context, tx *xtx.Transaction, committed bool) {
	changes := h.take(tx)
	if !committed || len(changes) == 0 {
		return
	}
	if box, ok := ctx.Value(commitOutboxKey{}).(*commitOutbox); ok {
		box.changes = append(box.changes, changes...)
	}
}

// note 把一条变更记到 tx 名下，没有回调时不记。
func (h *commitHooks) note(tx *xtx.Transaction, c Change) {
	if h.live.Load() == 0 {
		return
	}
	if v, ok := h.pending.Load(tx); ok {
		p := v.(*[]Change)
		*p = append(*p, c)
		return
	}
	h.pending.Store(tx, &[]Change{c})
	h.npending.Add(1)
}

// take 取走 tx 名下攒的变更。
func (h *commitHooks) take(tx *xtx.Transaction) []Change {
	if h.npending.Load() == 0 {
		return nil
	}
	v, ok := h.pending.LoadAndDelete(tx)
	if !ok {
		return nil
	}
	h.npending.Add(-1)
	return *v.(*[]Change)
}

// settle 给调用方自开的事务收尾：提交了就发通知，否则丢掉。调用时锁要已经放掉。
func (h *commitHooks) settle(ctx context.Context, tx *xtx.Transaction, committed bool) {
	changes := h.take(tx)
	if committed && len(changes) > 0 {
		h.fire(ctx, changes)
	}
}

// fire 把一次提交的变更依次交给每个还没取消的回调。回调的 panic 不拦。
func (h *commitHooks) fire(ctx context.Context, changes []Change) {
	if ctx == nil {
		ctx = context.Background()
	}
	h.mu.Lock()
	subs := h.subs
	h.mu.Unlock()
	cs := ChangeSet{Changes: changes}
	for _, s := range subs {
		if !s.off.Load() {
			s.fn(ctx, cs)
		}
	}
}

// commitOutboxKey 是 ctx 上 [commitOutbox] 的键。
type commitOutboxKey struct{}

// commitOutbox 接住引擎自开事务提交后的变更，等调用方放掉锁再发。
type commitOutbox struct {
	hooks   *commitHooks
	ctx     context.Context
	changes []Change
}

// notifyScope 给一次走引擎自开事务的写入备一个信箱，配合 defer [commitOutbox.flush] 用。
//
// flush 要排在放锁之后：defer 先进后出，所以 notifyScope 要写在取锁之前。
// 没有回调时不备信箱，返回原 ctx 与 nil。
func (db *DB) notifyScope(ctx context.Context) (context.Context, *commitOutbox) {
	if db.hooks.live.Load() == 0 {
		return ctx, nil
	}
	b := &commitOutbox{hooks: &db.hooks, ctx: ctx}
	return context.WithValue(ctx, commitOutboxKey{}, b), b
}

// flush 发出信箱里的变更，nil 信箱什么也不做。
func (b *commitOutbox) flush() {
	if b != nil && len(b.changes) > 0 {
		b.hooks.fire(b.ctx, b.changes)
	}
}

// muteChangesKey 是 ctx 上「不逐篇记变更」标记的键。
type muteChangesKey struct{}

// muteChanges 让 ctx 下的逐篇变更不记，清空集合时由调用方改记一条集合级的。
func muteChanges(ctx context.Context) context.Context {
	return context.WithValue(ctx, muteChangesKey{}, true)
}
