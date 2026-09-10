package xtx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"

	"github.com/xmapst/xdoc/internal/xpage"
)

// ErrLockTimeout 表示等锁等超时了。
var ErrLockTimeout = errors.New("xtx: timed out waiting for lock")

// ErrNotHeld 表示释放了一把没拿到的锁，属于程序错误。
var ErrNotHeld = errors.New("xtx: releasing a lock that is not held")

// ErrDeadlock 表示等待关系成了环，再等下去谁也走不了。
var ErrDeadlock = errors.New("xtx: deadlock detected; roll back and retry")

// LockService 管两类锁：一道全局闸门，和按集合名的可重入锁。
//
// 闸门是共享／排他的：普通事务持共享，检查点之类的持排他。
// 集合锁归事务所有，同一个事务可以重复拿，按次数配对释放。
type LockService struct {
	mu sync.Mutex

	// changed 是一个每次状态变化就关掉重开的通道，用来唤醒所有等待者。
	//
	// 关通道能一次叫醒全部等待者，比逐个通知省事；等待者醒来自己再判一次条件。
	changed chan struct{}

	// waiters 是正挂在 changed 上等的个数。没人等时状态变了也不必换通道。
	waiters int

	exclusive bool
	shared    int

	// pending 是排队等排他闸门的个数。有人排队时新来的共享方要等它先走完，
	// 否则前后交叠的事务会让共享方永远清不了零，排他方一直等下去。
	pending int

	cols map[string]col

	// waiting 记着每个事务正在等哪个集合，死锁检测靠它。
	waiting map[uint32]string

	// stats 记等待、超时与互等的次数；logger 记超时，为 nil 时不记。
	stats  *Stats
	logger *slog.Logger
}

// col 是一把集合锁：谁拿着，重入了几层。
type col struct {
	owner uint32
	depth int
}

// NewLockService 开一套锁服务。
func NewLockService() *LockService { return newLockService(new(Stats), nil) }

// newLockService 同 [NewLockService]，计数记进 st，超时记到 logger。
func newLockService(st *Stats, logger *slog.Logger) *LockService {
	return &LockService{
		changed: make(chan struct{}),
		cols:    map[string]col{},
		waiting: map[uint32]string{},
		stats:   st,
		logger:  logger,
	}
}

// logTimeout 把一次等锁超时记进日志。不要在持有互斥锁时调用：处理器慢的话会拖住所有等锁的人。
func (l *LockService) logTimeout(ctx context.Context, what string, err error) {
	logEvent(ctx, l.logger, slog.LevelWarn, "xdoc: lock timeout",
		slog.String("lock", what), slog.Any("error", err))
}

// notify 唤醒所有等待者，没有等待者就什么也不做。必须在持有互斥锁时调用。
func (l *LockService) notify() {
	if l.waiters == 0 {
		return
	}
	close(l.changed)
	l.changed = make(chan struct{})
}

// park 挂到通道上等下一次状态变化，上下文结束返回假。
//
// 必须在持有互斥锁时调用；等的时候放开，返回时已重新拿回。
func (l *LockService) park(ctx context.Context) bool {
	ch := l.changed
	l.waiters++
	l.mu.Unlock()

	woken := false
	select {
	case <-ch:
		woken = true
	case <-ctx.Done():
	}

	l.mu.Lock()
	l.waiters--
	return woken
}

// wait 反复尝试 ready，直到成功或者上下文结束。
//
// ready 在持有互斥锁时调用，成功时它自己就把状态改好了。
// 超时的报错里提示了一种常见的踩法：在事务里调了库级方法，
// 那会去抢自己已经拿着的锁。
//
// 真的挂起过才记一次等待，醒来多少次都只算一次。report 为假时超时不计数、不记日志，
// 给内部落空了也无妨的软等待用。
func (l *LockService) wait(ctx context.Context, ready func() bool, what string, report bool) error {
	l.mu.Lock()
	waited := false
	for !ready() {
		if !waited {
			waited = true
			l.stats.lockWaits.Add(1)
		}
		if !l.park(ctx) {
			l.mu.Unlock()
			err := fmt.Errorf("%w: %s: %w"+
				" (if you called a database-level method from inside a transaction,"+
				" use the transaction's own handle instead)",
				ErrLockTimeout, what, ctx.Err())
			if report {
				l.stats.lockTimeouts.Add(1)
				l.logTimeout(ctx, what, err)
			}
			return err
		}
	}
	l.mu.Unlock()
	return nil
}

// EnterShared 进共享闸门，排他方拿着或者有排他方在排队时就等。
func (l *LockService) EnterShared(ctx context.Context) error {
	return l.wait(ctx, func() bool {
		if l.exclusive || l.pending > 0 {
			return false
		}
		l.shared++
		return true
	}, "shared gate", true)
}

// ExitShared 出共享闸门。
//
// 只有排他方在等共享方清零，没清零就不必叫醒谁。
func (l *LockService) ExitShared() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.shared == 0 {
		return fmt.Errorf("%w: shared gate", ErrNotHeld)
	}
	l.shared--
	if l.shared == 0 {
		l.notify()
	}
	return nil
}

// EnterExclusive 进排他闸门，要等所有共享方都出去。
//
// 排队期间新来的共享方先等着，在途的走完就轮到自己。等不到放弃时要叫醒它们，
// 别让它们陪着等到超时——自己拿着共享闸门又来要排他的，就是这样等到超时的。
func (l *LockService) EnterExclusive(ctx context.Context) error { return l.enterExclusive(ctx, true) }

// enterExclusive 同 [LockService.EnterExclusive]；report 为假时等不到不算超时，见 [LockService.wait]。
func (l *LockService) enterExclusive(ctx context.Context, report bool) error {
	l.mu.Lock()
	l.pending++
	l.mu.Unlock()

	err := l.wait(ctx, func() bool {
		if l.exclusive || l.shared > 0 {
			return false
		}
		l.exclusive = true
		return true
	}, "exclusive gate", report)

	l.mu.Lock()
	l.pending--
	if err != nil {
		l.notify()
	}
	l.mu.Unlock()
	return err
}

// TryEnterExclusive 试着进排他闸门，进不去立刻返回假。
func (l *LockService) TryEnterExclusive() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.exclusive || l.shared > 0 {
		return false
	}
	l.exclusive = true
	return true
}

// ExitExclusive 出排他闸门。
func (l *LockService) ExitExclusive() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.exclusive {
		return fmt.Errorf("%w: exclusive gate", ErrNotHeld)
	}
	l.exclusive = false
	l.notify()
	return nil
}

// EnterCollections 一次拿多把集合锁。
//
// **先排序去重再逐个拿**：所有事务按同样的次序上锁，就不会互相卡住。
// 中途失败会把已经拿到的都放掉，不留半截状态。
func (l *LockService) EnterCollections(ctx context.Context, txID uint32, names ...string) error {
	sorted := slices.Clone(names)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)

	acquired := make([]string, 0, len(sorted))
	for _, n := range sorted {
		if err := l.enterCollection(ctx, txID, n); err != nil {
			for _, got := range acquired {
				_ = l.ExitCollection(txID, got)
			}
			if errors.Is(err, ErrLockTimeout) {
				l.logTimeout(ctx, "collection "+n, err)
			}
			return err
		}
		acquired = append(acquired, n)
	}
	return nil
}

// enterCollection 拿一把集合锁，已经是自己的就加一层。
//
// 拿不到时先查等待关系有没有成环，成环就直接报死锁而不是干等到超时。
// 集合名按大小写不敏感折叠后作键。
func (l *LockService) enterCollection(ctx context.Context, txID uint32, name string) error {
	name = xpage.FoldName(name)
	l.mu.Lock()
	defer l.mu.Unlock()
	waited := false
	for {
		c, held := l.cols[name]
		if !held || c.owner == txID {
			c.owner = txID
			c.depth++
			l.cols[name] = c
			delete(l.waiting, txID)
			return nil
		}
		if l.wouldCycle(txID, c.owner) {
			delete(l.waiting, txID)
			l.stats.deadlocks.Add(1)
			return fmt.Errorf("%w: transaction %d wants %q, held by transaction %d",
				ErrDeadlock, txID, name, c.owner)
		}
		l.waiting[txID] = name
		if !waited {
			waited = true
			l.stats.lockWaits.Add(1)
		}

		if !l.park(ctx) {
			delete(l.waiting, txID)
			err := fmt.Errorf("%w: collection lock %s: %w"+
				" (if you called a database-level method from inside a transaction,"+
				" use the transaction's own collection handle instead)",
				ErrLockTimeout, name, ctx.Err())
			l.stats.lockTimeouts.Add(1)
			return err
		}
	}
}

// wouldCycle 顺着「谁在等谁」的链走，看会不会绕回自己。
//
// 步数上限是等待者个数加一，防住数据本身就有环时走不出来。
// 必须在持有互斥锁时调用。
func (l *LockService) wouldCycle(me, holder uint32) bool {
	for step := 0; step <= len(l.waiting)+1; step++ {
		if holder == me {
			return true
		}
		name, waiting := l.waiting[holder]
		if !waiting {
			return false
		}
		c, held := l.cols[name]
		if !held {
			return false
		}
		holder = c.owner
	}
	return false
}

// ExitCollection 放一把集合锁，重入的只减一层。不是自己的锁会报错。
func (l *LockService) ExitCollection(txID uint32, name string) error {
	name = xpage.FoldName(name)
	l.mu.Lock()
	defer l.mu.Unlock()
	c, held := l.cols[name]
	if !held {
		return fmt.Errorf("%w: collection %q", ErrNotHeld, name)
	}
	if c.owner != txID {
		return fmt.Errorf("%w: collection %q is held by transaction %d, not %d",
			ErrNotHeld, name, c.owner, txID)
	}
	c.depth--
	if c.depth > 0 {
		l.cols[name] = c
		return nil
	}
	delete(l.cols, name)
	l.notify()
	return nil
}

// HoldsCollection 判断某个事务是不是拿着某个集合的锁。
func (l *LockService) HoldsCollection(txID uint32, name string) bool {
	name = xpage.FoldName(name)
	l.mu.Lock()
	defer l.mu.Unlock()
	c, held := l.cols[name]
	return held && c.owner == txID
}
