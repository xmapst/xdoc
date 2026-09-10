package xdoc

import (
	"context"
	"log/slog"
	"time"
)

// WithLogger 让库把少见但值得知道的事件记成结构化日志。
//
// 记的是：自动重建的开始与结果（含跳过了几处）、检查点失败、句柄进入 [ErrBroken]、
// 等锁超时、[DB.Transaction] 撞上互等后的重试。热路径上不记任何东西。
// 不设或传 nil 时一条也不输出——不会退回到 [slog.Default]。
func WithLogger(l *slog.Logger) Option { return func(o *options) { o.logger = l } }

// autoRebuildFile 是 [WithAutoRebuild] 触发的那次重建，前后各记一条日志。
//
// 这条路上调用方接不到 [RebuildResult]，日志是 _rebuild_errors 集合之外唯一的报告。
func (o options) autoRebuildFile(path string, opts []Option) (RebuildResult, error) {
	l := o.logger
	if l == nil {
		return o.rebuildFile(path, opts)
	}
	ctx := context.Background()
	l.LogAttrs(ctx, slog.LevelWarn, "xdoc: auto rebuild started",
		slog.String("path", path), slog.String("reason", "database was not closed cleanly"))
	start := time.Now()
	res, err := o.rebuildFile(path, opts)
	elapsed := time.Since(start)
	if err != nil {
		l.LogAttrs(ctx, slog.LevelError, "xdoc: auto rebuild failed",
			slog.String("path", path), slog.Duration("elapsed", elapsed), slog.Any("error", err))
		return res, err
	}
	level := slog.LevelInfo
	if len(res.Skipped) > 0 || len(res.Salvaged) > 0 {
		level = slog.LevelWarn
	}
	l.LogAttrs(ctx, level, "xdoc: auto rebuild finished",
		slog.String("path", path),
		slog.String("backup", res.Backup),
		slog.Int("collections", res.Collections),
		slog.Int("documents", res.Documents),
		slog.Int("skipped", len(res.Skipped)),
		slog.Any("salvaged", res.Salvaged),
		slog.Duration("elapsed", elapsed))
	return res, nil
}

// logDeadlockRetry 记一次 [DB.Transaction] 撞上互等后的重试；retrying 为假表示放弃了。
//
// 日志器在持有权之下读：[DB.Rebuild] 会换掉整份选项。
func (db *DB) logDeadlockRetry(ctx context.Context, attempt int, delay time.Duration, retrying bool, err error) {
	var l *slog.Logger
	db.withCore(func() { l = db.opts.logger })
	if l == nil {
		return
	}
	if !retrying {
		l.LogAttrs(ctx, slog.LevelWarn, "xdoc: transaction deadlock, giving up",
			slog.Int("attempt", attempt), slog.Any("error", err))
		return
	}
	l.LogAttrs(ctx, slog.LevelInfo, "xdoc: transaction deadlock, retrying",
		slog.Int("attempt", attempt), slog.Duration("backoff", delay), slog.Any("error", err))
}
