package xtx

import (
	"context"
	"errors"
	"fmt"
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

	exclusive bool
	shared    int
	cols      map[string]col

	// waiting 记着每个事务正在等哪个集合，死锁检测靠它。
	waiting map[uint32]string
}

// col 是一把集合锁：谁拿着，重入了几层。
type col struct {
	owner uint32
	depth int
}

// NewLockService 开一套锁服务。
func NewLockService() *LockService {
	return &LockService{
		changed: make(chan struct{}),
		cols:    map[string]col{},
		waiting: map[uint32]string{},
	}
}

// notify 唤醒所有等待者。必须在持有互斥锁时调用。
func (l *LockService) notify() {
	close(l.changed)
	l.changed = make(chan struct{})
}

// wait 反复尝试 ready，直到成功或者上下文结束。
//
// ready 在持有互斥锁时调用，成功时它自己就把状态改好了。
// 超时的报错里提示了一种常见的踩法：在事务里调了库级方法，
// 那会去抢自己已经拿着的锁。
func (l *LockService) wait(ctx context.Context, ready func() bool, what string) error {
	for {
		l.mu.Lock()
		if ready() {
			l.mu.Unlock()
			return nil
		}
		ch := l.changed
		l.mu.Unlock()

		select {
		case <-ch:
		case <-ctx.Done():
			return fmt.Errorf("%w: %s: %w"+
				" (if you called a database-level method from inside a transaction,"+
				" use the transaction's own handle instead)",
				ErrLockTimeout, what, ctx.Err())
		}
	}
}

// EnterShared 进共享闸门，排他方拿着时就等。
func (l *LockService) EnterShared(ctx context.Context) error {
	return l.wait(ctx, func() bool {
		if l.exclusive {
			return false
		}
		l.shared++
		return true
	}, "shared gate")
}

// ExitShared 出共享闸门。
func (l *LockService) ExitShared() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.shared == 0 {
		return fmt.Errorf("%w: shared gate", ErrNotHeld)
	}
	l.shared--
	l.notify()
	return nil
}

// EnterExclusive 进排他闸门，要等所有共享方都出去。
func (l *LockService) EnterExclusive(ctx context.Context) error {
	return l.wait(ctx, func() bool {
		if l.exclusive || l.shared > 0 {
			return false
		}
		l.exclusive = true
		return true
	}, "exclusive gate")
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
	for {
		l.mu.Lock()
		c, held := l.cols[name]
		if !held || c.owner == txID {
			c.owner = txID
			c.depth++
			l.cols[name] = c
			delete(l.waiting, txID)
			l.mu.Unlock()
			return nil
		}
		if l.wouldCycle(txID, c.owner) {
			delete(l.waiting, txID)
			l.mu.Unlock()
			return fmt.Errorf("%w: transaction %d wants %q, held by transaction %d",
				ErrDeadlock, txID, name, c.owner)
		}
		l.waiting[txID] = name
		ch := l.changed
		l.mu.Unlock()

		select {
		case <-ch:
		case <-ctx.Done():
			l.mu.Lock()
			delete(l.waiting, txID)
			l.mu.Unlock()
			return fmt.Errorf("%w: collection lock %s: %w"+
				" (if you called a database-level method from inside a transaction,"+
				" use the transaction's own collection handle instead)",
				ErrLockTimeout, name, ctx.Err())
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
