package xtx

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// Stats 是运行期计数，全用原子变量，读写都不拿锁。
//
// 它可以跨实例存活：库句柄重建或在共享模式下反复开关核心时，把同一份交给每个新实例，
// 计数就接着累加。
type Stats struct {
	// cache 按缓存分片各记一份，下标就是片号：并发命中不同的片时不会抢同一条缓存行。
	cache [maxCacheShards]cacheCounter

	lockWaits          atomic.Uint64
	lockTimeouts       atomic.Uint64
	deadlocks          atomic.Uint64
	checkpoints        atomic.Uint64
	checkpointFailures atomic.Uint64
	checkpointNanos    atomic.Int64
}

// cacheCounter 是一片缓存的命中计数，补齐到一条缓存行。
type cacheCounter struct {
	hits   atomic.Uint64
	misses atomic.Uint64
	_      [48]byte
}

// Counters 是 [Stats] 某一刻的读数。
type Counters struct {
	CacheHits, CacheMisses uint64

	LockWaits, LockTimeouts, Deadlocks uint64

	Checkpoints, CheckpointFailures uint64
	CheckpointDuration              time.Duration
}

// Snapshot 读出当前计数。各项分别读，彼此之间不保证是同一瞬间。
func (s *Stats) Snapshot() Counters {
	var c Counters
	for i := range s.cache {
		c.CacheHits += s.cache[i].hits.Load()
		c.CacheMisses += s.cache[i].misses.Load()
	}
	c.LockWaits = s.lockWaits.Load()
	c.LockTimeouts = s.lockTimeouts.Load()
	c.Deadlocks = s.deadlocks.Load()
	c.Checkpoints = s.checkpoints.Load()
	c.CheckpointFailures = s.checkpointFailures.Load()
	c.CheckpointDuration = time.Duration(s.checkpointNanos.Load())
	return c
}

// logEvent 记一条结构化日志；没配日志器就什么也不做。只用在少见的事件上。
func logEvent(ctx context.Context, l *slog.Logger, level slog.Level, msg string, attrs ...slog.Attr) {
	if l == nil {
		return
	}
	l.LogAttrs(ctx, level, msg, attrs...)
}

// Stats 返回这个实例累加计数用的那一份。
func (c *Core) Stats() *Stats { return c.stats }

// Logger 返回这个实例的日志器，没配时为 nil。
func (c *Core) Logger() *slog.Logger { return c.logger }

// observeCheckpoint 记下一次真正动手的检查点：成功的计次数与耗时，失败的计数并记日志。
func (c *Core) observeCheckpoint(start time.Time, pages int64, moved int, err error) {
	if err != nil {
		c.stats.checkpointFailures.Add(1)
		logEvent(context.Background(), c.logger, slog.LevelError, "xdoc: checkpoint failed",
			slog.Int64("logPages", pages), slog.Int("moved", moved), slog.Any("error", err))
		return
	}
	c.stats.checkpoints.Add(1)
	c.stats.checkpointNanos.Add(int64(time.Since(start)))
}
