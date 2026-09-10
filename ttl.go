package xdoc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xmapst/xdoc/internal/xbexpr"
	"github.com/xmapst/xdoc/internal/xbson"
)

// ttlBatch 是过期清理一个事务最多删多少篇。是变量只为测试能调小它。
var ttlBatch = manyBatch

// ttlYield 是两批之间停多久：集合锁的等待方不排队，刚放开就立刻再取，
// 等着写这个集合的人可能一直抢不到。
const ttlYield = time.Millisecond

// ttlDateMin 是日期能表示的最早时刻，清理区间的下界。
//
// 下界不能省：值按类型排序，数字、字符串、空值都排在日期前面，单写 expr <= now
// 会把它们一并圈进来。
var ttlDateMin = time.Date(1, time.January, 1, 0, 0, 0, 0, time.UTC)

// ttlSet 记着一个库上注册过的过期清理，只在内存里。
type ttlSet struct {
	mu     sync.Mutex
	closed bool
	exprs  map[string]string // 小写集合名 → 规范化后的表达式
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// running 是正在跑的清理 goroutine 数，测试用它断言关库后都退出了。
	running atomic.Int32
}

// EnsureTTL 让 expr 求出的日期一过，文档就被后台自动删掉。
//
// 先按 expr 建一条非唯一索引（同 [Collection.EnsureIndexOn]），再起一个 goroutine
// 每隔 every 清一次：沿索引找出 expr 在「最早日期」与现在之间的文档，每批最多
// 一千篇、一批一个事务，批与批之间放开集合锁，大量同时过期也不会长时间挡住别的写入。
// expr 不是日期的文档（字符串、数字、缺失）永远不删。
//
// 注册只在内存里，不落盘：与 EnsureIndex 一样**每次启动调一次**。同一集合同一表达式
// 重复调什么也不做（every 以第一次为准）；表达式不同报错。只读库报错。
//
// 后台的错误无处可报，本轮放弃、下个周期再试。[DB.Close] 停下并等清理 goroutine
// 退出；重建不影响它。
func (c *Collection) EnsureTTL(ctx context.Context, expr string, every time.Duration) error {
	if every <= 0 {
		return fmt.Errorf("xdoc: TTL interval must be positive, got %s", every)
	}
	readOnly := false
	c.db.withCore(func() { readOnly = c.db.opts.readOnly })
	if readOnly {
		return fmt.Errorf("xdoc: cannot enable TTL on %q: database opened read-only", c.name)
	}
	node, err := xbexpr.Parse(expr)
	if err != nil {
		return fmt.Errorf("xdoc: TTL expression %q: %w", expr, err)
	}
	canon := xbexpr.Print(node)
	key := strings.ToLower(c.name)

	s := &c.db.ttl
	if done, err := s.check(c.name, key, canon); done || err != nil {
		return err
	}
	if _, err := c.EnsureIndexOn(ctx, canon, false); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// 建索引期间可能有人抢先注册或关了库，再判一次。
	if done, err := s.checkLocked(c.name, key, canon); done || err != nil {
		return err
	}
	if s.exprs == nil {
		s.exprs = map[string]string{}
		s.ctx, s.cancel = context.WithCancel(context.Background())
	}
	s.exprs[key] = canon
	s.wg.Add(1)
	s.running.Add(1)
	go s.run(c.db, c.name, canon, every)
	return nil
}

// check 在锁下调 [ttlSet.checkLocked]。
func (s *ttlSet) check(name, key, expr string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkLocked(name, key, expr)
}

// checkLocked 判这次注册能不能做：已关库报错，已注册同一表达式返回 done，不同表达式报错。
func (s *ttlSet) checkLocked(name, key, expr string) (done bool, err error) {
	if s.closed {
		return false, fmt.Errorf("xdoc: cannot enable TTL on %q: %w", name, ErrClosed)
	}
	old, ok := s.exprs[key]
	switch {
	case !ok:
		return false, nil
	case old == expr:
		return true, nil
	default:
		return false, fmt.Errorf("xdoc: collection %q already has TTL on %s, cannot switch to %s", name, old, expr)
	}
}

// stop 停下全部清理并等它们退出，可重复调。之后的注册一律报 [ErrClosed]。
func (s *ttlSet) stop() {
	s.mu.Lock()
	s.closed = true
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// run 是一个集合的清理循环：注册时先清一次，之后每个周期一次。
func (s *ttlSet) run(db *DB, name, expr string, every time.Duration) {
	defer s.wg.Done()
	defer s.running.Add(-1)
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		_, _ = db.ttlSweep(s.ctx, name, expr)
		select {
		case <-s.ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// ttlSweep 清一轮：一批一个事务，直到取出的不足一批，返回删了几篇。
//
// 「现在」整轮只取一次，清理途中才到期的留给下一轮，免得一轮停不下来。
func (db *DB) ttlSweep(ctx context.Context, name, expr string) (int, error) {
	var now *Value
	total := 0
	for {
		n, err := db.ttlDeleteBatch(ctx, name, expr, &now)
		total += n
		if err != nil || n < ttlBatch {
			return total, err
		}
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		case <-time.After(ttlYield):
		}
	}
}

// ttlDeleteBatch 在一个事务里删掉至多一批过期文档。*now 为 nil 表示本轮第一批，
// 这时才取当前时间，并先不开事务看一眼有没有要删的——没有就不去抢集合写锁。
//
// 不走 [DB.Transaction]：共享模式下事务里的每次进出都会把跨进程锁多攥一层
// （见 [leakTransactionLockDepth]），后台清理若照此办理，删过一次就会让同句柄上
// 正在排队取锁的其它 goroutine 永远等下去。这里整批只取一次持有权，事务开在它里面，
// 批内的进出都能配对归还；提交通知照常发。
func (db *DB) ttlDeleteBatch(ctx context.Context, name, expr string, now **Value) (int, error) {
	rel, err := db.enter(ctx)
	if err != nil {
		return 0, err
	}
	defer rel()

	if *now == nil {
		v, err := xbson.DateTime(db.opts.nowOrDefault())
		if err != nil {
			return 0, err
		}
		*now = v
		has, err := ttlBatchQuery(db.Collection(name).Query(), expr, v).Exists(ctx)
		if err != nil || !has {
			return 0, err
		}
	}

	inner, err := db.beginCore(ctx)
	if err != nil {
		return 0, err
	}
	n, err := func() (int, error) {
		t := &Tx{db: db, tx: inner, ctx: ctx}
		ids := make([]*Value, 0, ttlBatch)
		for d, err := range ttlBatchQuery(t.Collection(name).Query(), expr, *now).ForUpdate().All(ctx) {
			if err != nil {
				return 0, err
			}
			if id := d.Get("_id"); id != nil {
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 {
			return 0, nil
		}
		return db.engine.DeleteIn(ctx, inner, name, ids)
	}()
	if err != nil {
		err = errors.Join(err, inner.Rollback())
		db.hooks.settle(ctx, inner, false)
		return 0, fmt.Errorf("xdoc: TTL sweep on %q: %w", name, err)
	}
	err = inner.Commit()
	db.hooks.settle(ctx, inner, err == nil)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// ttlBatchQuery 排出一批过期文档的查询：expr 落在最早日期与 now 之间，只取主键。
//
// 用 BETWEEN 而不是 expr <= now，优化器才会把它变成索引上只含日期的一段区间。
// expr 不能加括号：优化器按原文比对索引表达式，括号会让它认不出来。
func ttlBatchQuery(qb *QueryBuilder, expr string, now *Value) *QueryBuilder {
	lo, _ := xbson.DateTime(ttlDateMin)
	return qb.Where(expr+" BETWEEN @__xdoc_ttl_lo AND @__xdoc_ttl_hi").
		Param("__xdoc_ttl_lo", lo).
		Param("__xdoc_ttl_hi", now).
		Select("{ _id: $._id }").
		Limit(ttlBatch)
}
