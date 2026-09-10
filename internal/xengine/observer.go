package xengine

import (
	"context"
	"strconv"

	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xtx"
)

// ChangeOp 是一条变更的种类。
type ChangeOp uint8

const (
	// ChangeInsert 插入了一篇文档。
	ChangeInsert ChangeOp = iota + 1

	// ChangeUpdate 改写了一篇文档。
	ChangeUpdate

	// ChangeDelete 删掉了一篇文档。
	ChangeDelete

	// ChangeDeleteAll 清空了整个集合，不逐篇列主键。
	ChangeDeleteAll

	// ChangeDropCollection 删掉了整个集合。
	ChangeDropCollection

	// ChangeRenameCollection 给集合改了名。
	ChangeRenameCollection
)

// String 返回种类的小写驼峰名，未知值写成数字。
func (op ChangeOp) String() string {
	switch op {
	case ChangeInsert:
		return "insert"
	case ChangeUpdate:
		return "update"
	case ChangeDelete:
		return "delete"
	case ChangeDeleteAll:
		return "deleteAll"
	case ChangeDropCollection:
		return "dropCollection"
	case ChangeRenameCollection:
		return "renameCollection"
	}
	return "ChangeOp(" + strconv.Itoa(int(op)) + ")"
}

// Change 是引擎里发生的一条变更。集合级的种类 ID 为 nil。
type Change struct {
	Collection string
	Op         ChangeOp
	ID         *xbson.Value

	// NewName 只在 [ChangeRenameCollection] 上有值。
	NewName string
}

// Observer 旁听引擎里的写入与它所在事务的结局，供上层攒提交后通知。
//
// 两个方法都在写入者的 goroutine 里、持着锁时被调到，**不能回头调引擎**，
// 只该把东西记下来。
type Observer interface {
	// Changed 报告 tx 里真的落下去的一条变更。
	Changed(ctx context.Context, tx *xtx.Transaction, c Change)

	// Finished 报告引擎自开的事务怎么收的尾。调用方给的事务由调用方自己收尾，不走这里。
	Finished(ctx context.Context, tx *xtx.Transaction, committed bool)
}

// SetObserver 装上旁听者，nil 表示不旁听。要在引擎投入使用之前调。
func (e *Engine) SetObserver(o Observer) { e.obs = o }

// changed 在装了旁听者时报告一条变更。
func (e *Engine) changed(ctx context.Context, tx *xtx.Transaction, c Change) {
	if e.obs != nil {
		e.obs.Changed(ctx, tx, c)
	}
}

// finished 在装了旁听者时报告自开事务的结局。
func (e *Engine) finished(ctx context.Context, tx *xtx.Transaction, committed bool) {
	if e.obs != nil {
		e.obs.Finished(ctx, tx, committed)
	}
}
