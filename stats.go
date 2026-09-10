package xdoc

import "time"

// Stats 是一个库句柄的运行期统计，由 [DB.Stats] 读出。
//
// 计数类字段从打开句柄起一直累加，[DB.Rebuild] 换掉底层实例、共享模式反复开关文件都不清零；
// 其余字段是读的那一刻的状态。各项分别读，彼此之间不保证是同一瞬间。
type Stats struct {
	// CacheHits、CacheMisses 是读页时页缓存命中与未命中的次数。
	CacheHits, CacheMisses uint64

	// LogPages 是日志文件此刻有几页，即还没搬回数据文件的量。
	LogPages int64

	// Checkpoints 是做成的检查点次数，只算日志非空、真正搬过页的那些；
	// CheckpointDuration 是它们的累计耗时。CheckpointFailures 是失败的次数。
	Checkpoints        uint64
	CheckpointDuration time.Duration
	CheckpointFailures uint64

	// LockWaits 是拿闸门或集合锁时真的挂起等过的次数，一次获取无论醒来几回只算一次。
	LockWaits uint64

	// LockTimeouts 是等锁等到超时（或 ctx 先结束）的次数。
	LockTimeouts uint64

	// Deadlocks 是检测到事务互等的次数，[DB.Transaction] 自动重试掉的也算。
	Deadlocks uint64

	// OpenTransactions、OpenCursors 是此刻开着的事务数与没走完的查询数。
	OpenTransactions, OpenCursors int
}

// Stats 读出这个句柄的运行期统计。
//
// 不开事务、不碰文件，可以随时并发调用。共享模式下底层文件没开着时，
// LogPages 与 OpenTransactions 为 0；直连模式下正在重建时会等重建换完实例。
func (db *DB) Stats() Stats {
	var out Stats
	collect := func() {
		c := db.opts.stats.Snapshot()
		out.CacheHits, out.CacheMisses = c.CacheHits, c.CacheMisses
		out.Checkpoints, out.CheckpointDuration = c.Checkpoints, c.CheckpointDuration
		out.CheckpointFailures = c.CheckpointFailures
		out.LockWaits, out.LockTimeouts, out.Deadlocks = c.LockWaits, c.LockTimeouts, c.Deadlocks
		if db.core != nil {
			out.LogPages = db.core.Disk().LogPageCount()
			out.OpenTransactions = db.core.OpenTransactions()
		}
	}
	if s := db.shared; s != nil {
		// 不走 enter：那会取跨进程锁并把文件开出来。核心的开关都在 s.mu 之下。
		s.mu.Lock()
		collect()
		s.mu.Unlock()
	} else {
		db.withCore(collect)
	}
	out.OpenCursors = len(db.cursors.list())
	return out
}
