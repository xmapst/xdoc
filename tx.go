package xdoc

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"math/rand/v2"
	"time"

	"github.com/xmapst/xdoc/internal/xengine"
	"github.com/xmapst/xdoc/internal/xquery"
	"github.com/xmapst/xdoc/internal/xtx"
)

// Tx 是一个显式事务。
//
// 它**不能跨 goroutine 共用**：每一步都依赖前一步的状态。done 让提交与回滚幂等，
// 所以 defer 一个 Rollback 再正常 Commit 是安全写法。
type Tx struct {
	db   *DB
	tx   *xtx.Transaction
	done bool
}

// ErrDeadlock 表示两个事务以相反顺序访问同一批集合，构成了互等。
//
// [DB.Transaction] 会自己重试，一般见不到它；直接用 [DB.BeginTrans] 时看到它，
// 正确的反应是回滚并重试。
var ErrDeadlock = xtx.ErrDeadlock

// maxDeadlockRetries 是互等之后重跑 fn 的次数上限。
const maxDeadlockRetries = 10

// Transaction 在一个事务里跑 fn：返回 nil 就提交，返回错误就回滚。
//
// # fn 可能被调用多次
//
// 撞上互等时会回滚并**重新跑一遍 fn**（最多 [maxDeadlockRetries] 次）。所以 fn 要
// 能被重复执行：它对数据库的改动会随回滚一起消失，但它在数据库之外做的事
// （发消息、改内存里的计数）不会。**把那些副作用挪到 Transaction 返回之后。**
//
// # 回调里要用 tx.Collection
//
// [DB.Collection] 上的写入自开一个事务，在回调里调它等于拿一个新事务去等外层事务
// 手里的集合锁，而外层正等着回调返回——一直等到锁超时。写别的集合不受影响
// （锁按集合分），但那种情形回滚也带不走它，且不报错。
//
// 用回调形态而不是 Begin/Commit，是让"一定会收尾"由库保证：漏一次回滚不会报错，
// 而是那个事务永远占着集合锁，现场看不出是谁没放手。
func (db *DB) Transaction(ctx context.Context, fn func(*Tx) error) error {
	var err error
	for attempt := 0; attempt <= maxDeadlockRetries; attempt++ {
		err = db.transactionOnce(ctx, fn)
		if !errors.Is(err, ErrDeadlock) {
			return err
		}

		select {
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		case <-time.After(backoff(attempt)):
		}
	}
	return fmt.Errorf("gave up after %d retries: %w", maxDeadlockRetries, err)
}

// backoff 算重试前要等多久：翻倍增长、封顶 200ms，再加一个同量级的随机抖动。
//
// 抖动不能省：两个事务撞在一起之后若以同样的节奏醒来，会同样地再撞一次，
// 重试次数很快用光。
func backoff(attempt int) time.Duration {
	const base = 2 * time.Millisecond
	const cap = 200 * time.Millisecond
	d := base << min(attempt, 8)
	d = min(d, cap)
	return d + time.Duration(rand.Int64N(int64(d)))
}

// transactionOnce 跑一遍 fn，不重试。
//
// panic 也先回滚再原样抛出：不接管调用方的 panic，只保证事务不会挂在那里。
func (db *DB) transactionOnce(ctx context.Context, fn func(*Tx) error) error {
	if err := db.enterTx(ctx); err != nil {
		return err
	}
	inner, err := db.core.Begin(ctx)
	if err != nil {
		db.exitTx()
		return err
	}
	t := &Tx{db: db, tx: inner}
	defer func() {
		if r := recover(); r != nil {
			_ = t.rollback()
			panic(r)
		}
	}()
	if err := fn(t); err != nil {
		return errors.Join(err, t.rollback())
	}
	return t.commit()
}

// BeginTrans 开一个事务，由调用方自己 [Tx.Commit] 或 [Tx.Rollback]。
//
// 漏掉收尾的代价是那个事务一直占着集合锁，这个集合从此谁也写不进去，
// 每次尝试各自等到超时。撞上互等时也不会自动重试。
// 除非确实接不上回调形态，否则用 [DB.Transaction]。
func (db *DB) BeginTrans(ctx context.Context) (*Tx, error) {
	if err := db.enterTx(ctx); err != nil {
		return nil, err
	}
	inner, err := db.core.Begin(ctx)
	if err != nil {
		db.exitTx()
		return nil, err
	}
	return &Tx{db: db, tx: inner}, nil
}

// Commit 提交这个事务。已经收尾过的再调是空操作。
func (t *Tx) Commit() error { return t.commit() }

// Rollback 回滚这个事务。已经收尾过的再调是空操作，所以可以放心 defer。
func (t *Tx) Rollback() error { return t.rollback() }

// commit 提交并把库句柄从事务状态里退出来。
//
// 无论提交成败都要退：一次失败的提交也已经把事务结束了。
func (t *Tx) commit() error {
	if t.done {
		return nil
	}
	t.done = true
	defer t.db.exitTx()
	return t.tx.Commit()
}

// rollback 回滚并把库句柄从事务状态里退出来。
func (t *Tx) rollback() error {
	if t.done {
		return nil
	}
	t.done = true
	defer t.db.exitTx()
	return t.tx.Rollback()
}

// Collection 取一个走这个事务的集合句柄。
//
// 主键生成方式取库级默认——这个句柄不知道调用方手上是什么结构体类型，
// 要按 T 的主键字段类型来就显式 [TxCollection.WithAutoID]。
func (t *Tx) Collection(name string) *TxCollection {
	return &TxCollection{tx: t, name: name, auto: t.db.auto}
}

// TxCollection 是事务内的集合句柄，收发的都是 *Document。
//
// 事务里没有类型化的句柄：结构体与文档之间自己转，用 [DB.MarshalDocument] 与
// [DB.Decode]，那样用的还是库自己那个映射器。
//
// 与 [Tx] 一样不能跨 goroutine 共用。
type TxCollection struct {
	tx   *Tx
	name string
	auto AutoID

	includes []string
}

// Name 返回集合名。
func (c *TxCollection) Name() string { return c.name }

// AutoID 返回这个句柄用的主键生成方式。
func (c *TxCollection) AutoID() AutoID { return c.auto }

// Query 建一个走这个事务的查询构建器。
//
// 与库级的 [Collection.Query] 只差在事务上：它查出来的东西与事务里的改动互相
// 看得见，也不会在读完之前被外面改掉。
func (c *TxCollection) Query() *QueryBuilder {
	b := &QueryBuilder{c: c.tx.db.Collection(c.name), q: xquery.NewQuery(), tx: c.tx}
	for _, inc := range c.includes {
		b = b.Include(inc)
	}
	return b
}

// Include 返回一个之后的读都展开 expr 所指引用的新句柄，原句柄不受影响。
func (c *TxCollection) Include(expr string) *TxCollection {
	n := *c
	n.includes = append(append([]string(nil), c.includes...), expr)
	return &n
}

// WithAutoID 返回一个改用 a 生成主键的新句柄。
//
// 按结构体写入时**这一句省不得**：库级默认是 12 字节的 ObjectID，
// 一个 ID int64 的结构体被塞进 ObjectID 后插入照常成功，
// 要等到某次解回结构体才报「ObjectId 不是整数」，而那时这类记录已经攒了一批。
func (c *TxCollection) WithAutoID(a AutoID) *TxCollection {
	n := *c
	n.auto = a
	return &n
}

// Insert 在这个事务里写入若干篇文档，返回写进去的篇数。整批一个原子操作。
func (c *TxCollection) Insert(ctx context.Context, docs ...*Document) (int, error) {
	rel, err := c.tx.db.enter(ctx)
	if err != nil {
		return 0, err
	}
	defer rel()
	return c.tx.db.engine.InsertIn(ctx, c.tx.tx, c.name, docs, c.auto)
}

// InsertOne 在这个事务里写入一篇文档并返回它的主键。
func (c *TxCollection) InsertOne(ctx context.Context, doc *Document) (*Value, error) {
	if _, err := c.Insert(ctx, doc); err != nil {
		return nil, err
	}
	return doc.Get(IDField), nil
}

// Update 在这个事务里按主键整篇改写，返回真的改掉了几条。
func (c *TxCollection) Update(ctx context.Context, docs ...*Document) (int, error) {
	rel, err := c.tx.db.enter(ctx)
	if err != nil {
		return 0, err
	}
	defer rel()
	return c.tx.db.engine.UpdateIn(ctx, c.tx.tx, c.name, docs)
}

// Upsert 在这个事务里按主键存在与否分别插入或改写。
func (c *TxCollection) Upsert(ctx context.Context, docs ...*Document) (UpsertResult, error) {
	rel, err := c.tx.db.enter(ctx)
	if err != nil {
		return UpsertResult{}, err
	}
	defer rel()
	n, err := c.tx.db.engine.UpsertIn(ctx, c.tx.tx, c.name, docs, c.auto)
	if err != nil {
		return UpsertResult{}, err
	}
	return UpsertResult{Inserted: n, Updated: len(docs) - n}, nil
}

// Delete 在这个事务里按主键删除，静默跳过不存在的主键。
func (c *TxCollection) Delete(ctx context.Context, ids ...*Value) (int, error) {
	rel, err := c.tx.db.enter(ctx)
	if err != nil {
		return 0, err
	}
	defer rel()
	return c.tx.db.engine.DeleteIn(ctx, c.tx.tx, c.name, ids)
}

// FindByID 在这个事务里按主键取一篇文档，找不到返回 [ErrNotFound]。
func (c *TxCollection) FindByID(ctx context.Context, id *Value) (*Document, error) {
	rel, err := c.tx.db.enter(ctx)
	if err != nil {
		return nil, err
	}
	defer rel()
	d, err := c.tx.db.engine.FindIn(ctx, c.tx.tx, c.name, id)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, fmt.Errorf("%w: %s in %q", ErrNotFound, id, c.name)
	}
	return d, nil
}

// Exists 在这个事务里报告这个主键在不在。
func (c *TxCollection) Exists(ctx context.Context, id *Value) (bool, error) {
	rel, err := c.tx.db.enter(ctx)
	if err != nil {
		return false, err
	}
	defer rel()
	d, err := c.tx.db.engine.FindIn(ctx, c.tx.tx, c.name, id)
	return d != nil, err
}

// All 在这个事务里按主键顺序遍历整个集合。
func (c *TxCollection) All(ctx context.Context, order Order) iter.Seq2[*Document, error] {
	return c.tx.db.enterSeq(ctx, func() iter.Seq2[*Document, error] {
		return c.tx.db.engine.ScanIn(ctx, c.tx.tx, c.name, order)
	})
}

// EnsureIndex 在这个事务里建索引，本事务里立刻可见，回滚后消失。
//
// 其余语义同 [Collection.EnsureIndex]。
func (c *TxCollection) EnsureIndex(ctx context.Context, name, expr string, unique bool) (bool, error) {
	rel, err := c.tx.db.enter(ctx)
	if err != nil {
		return false, err
	}
	defer rel()
	return c.tx.db.engine.EnsureIndexIn(ctx, c.tx.tx, c.name, name, expr, unique)
}

// EnsureIndexOn 与上面一样，但索引名从表达式推出来，推法见 [Collection.EnsureIndexOn]。
func (c *TxCollection) EnsureIndexOn(ctx context.Context, expr string, unique bool) (bool, error) {
	name := xengine.DeriveIndexName(expr)
	if name == "" {
		return false, fmt.Errorf("xdoc: cannot derive an index name from %q, pass one explicitly", expr)
	}
	return c.EnsureIndex(ctx, name, expr, unique)
}

// DropIndex 在这个事务里删掉一条索引，返回它原先在不在。
func (c *TxCollection) DropIndex(ctx context.Context, name string) (bool, error) {
	rel, err := c.tx.db.enter(ctx)
	if err != nil {
		return false, err
	}
	defer rel()
	return c.tx.db.engine.DropIndexIn(ctx, c.tx.tx, c.name, name)
}
