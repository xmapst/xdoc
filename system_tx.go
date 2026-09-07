package xdoc

import (
	"context"
	"iter"

	"github.com/xmapst/xdoc/internal/xbson"
)

// sysTransactions 产出 $transactions：每个在途事务一行。
//
// 拿它回答「谁开着事务没结束」。这里没有 threadID 字段：事务不绑线程，
// 一个事务可以在任意 goroutine 上推进。
func (db *DB) sysTransactions(_ context.Context, _ *Tx, _ sysOpts) iter.Seq2[*Document, error] {
	infos := db.core.Transactions()
	out := make([]*Document, 0, len(infos))
	for _, t := range infos {
		st, err := xbson.DateTime(t.StartTime)
		if err != nil {
			return seqErr[*Document](err)
		}
		d := xbson.NewDocument()
		d.Set("transactionID", xbson.Int32(int32(t.ID)))
		d.Set("startTime", st)
		d.Set("mode", xbson.String(t.Mode))
		d.Set("transactionSize", xbson.Int32(int32(t.Size)))
		d.Set("maxTransactionSize", xbson.Int32(int32(t.Quota)))
		d.Set("pagesInLogFile", xbson.Int32(int32(t.LoggedPages)))
		d.Set("newPages", xbson.Int32(int32(t.NewPages)))
		d.Set("deletedPages", xbson.Int32(int32(t.DeletedPages)))
		d.Set("modifiedPages", xbson.Int32(int32(t.ModifiedPages)))
		out = append(out, d)
	}
	return seqOf(out)
}

// sysSnapshots 产出 $snapshots：每个事务对每个集合的视图各一行。
//
// 拿它回答「谁占着这个集合」。同样没有 threadID。
func (db *DB) sysSnapshots(_ context.Context, _ *Tx, _ sysOpts) iter.Seq2[*Document, error] {
	var out []*Document
	for _, t := range db.core.Transactions() {
		for _, s := range t.Snapshots {
			d := xbson.NewDocument()
			d.Set("transactionID", xbson.Int32(int32(s.TxID)))
			d.Set("collection", xbson.String(s.Name))
			d.Set("mode", xbson.String(s.Mode))
			d.Set("readVersion", xbson.Int32(int32(s.Version)))
			d.Set("pagesInMemory", xbson.Int32(int32(s.Pages)))
			d.Set("collectionDirty", xbson.Boolean(s.CollectionDirty))
			out = append(out, d)
		}
	}
	return seqOf(out)
}

// sysOpenCursors 产出 $open_cursors：每一次没走完的查询一行。
//
// 它是排查「写操作卡住」的第一站，见 [cursor]。running 为 false 表示引擎停在
// 那儿等调用方；elapsedMS 不含调用方处理每行的时间。
//
// 这里边遍历边产出而不是先收成切片：游标集合本来就在变，
// 拿一份快照逐条交出去即可。
func (db *DB) sysOpenCursors(_ context.Context, _ *Tx, _ sysOpts) iter.Seq2[*Document, error] {
	return func(yield func(*Document, error) bool) {
		for _, c := range db.cursors.list() {
			d := xbson.NewDocument()
			d.Set("transactionID", xbson.Int32(int32(c.txID)))
			d.Set("elapsedMS", xbson.Int32(int32(c.elapsed().Milliseconds())))
			d.Set("collection", xbson.String(c.collection))
			d.Set("mode", xbson.String(c.mode))
			d.Set("sql", xbson.String(c.sql))
			d.Set("running", xbson.Boolean(c.running()))
			d.Set("fetched", xbson.Int32(int32(c.fetchedCount())))
			if !yield(d, nil) {
				return
			}
		}
	}
}
