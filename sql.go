package xdoc

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strconv"

	"github.com/xmapst/xdoc/internal/xbexpr"
	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xjson"
	"github.com/xmapst/xdoc/internal/xsql"
)

// Execute 跑一条 SQL，结果一行一个值地交出来。
//
// 只有 SELECT 会出多行；其余语句各出一行：改动类给出受影响的条数，
// DDL 与事务控制给出一个布尔。
//
// **语句里不要拼接外部输入**，用 args 传：位置参数在表达式里写 @0、@1，
// 传一个文档或 map 则按键名写 @name。
//
// BEGIN 之后到 COMMIT/ROLLBACK 之间，这个库句柄上的 SQL 都走那个事务；
// DROP、RENAME、CHECKPOINT、PRAGMA 写在其中会被拒绝。
//
// 对虚拟集合的 UPDATE 与 DELETE 不报错，直接返回 0：它们没有可改的存储。
func (db *DB) Execute(ctx context.Context, sql string, args ...any) iter.Seq2[*Value, error] {
	st, err := xsql.Parse(sql)
	if err != nil {
		return seqErr[*Value](err)
	}
	params, err := db.sqlParams(args)
	if err != nil {
		return seqErr[*Value](err)
	}
	switch st.Kind {
	case xsql.KindSelect:
		return db.execSelect(ctx, st, params)
	case xsql.KindInsert:
		return db.execInsert(ctx, st, params)
	case xsql.KindUpdate:
		return oneValue(func() (*Value, error) {
			if isSystemName(st.Collection) {
				return xbson.Int32(0), nil
			}
			var n int
			var err error
			if t := db.currentTx(); t != nil {
				n, err = t.updateWithParams(ctx, st, params)
			} else {
				n, err = db.updateWithParamsDB(ctx, st, params)
			}
			return xbson.Int32(int32(n)), err
		})
	case xsql.KindDelete:
		return oneValue(func() (*Value, error) {
			if isSystemName(st.Collection) {
				return xbson.Int32(0), nil
			}
			var n int
			var err error
			if t := db.currentTx(); t != nil {
				n, err = t.deleteManyIn(ctx, st.Collection, firstOf(st.Where), params)
			} else {
				n, err = db.deleteWithParamsDB(ctx, st, params)
			}
			return xbson.Int32(int32(n)), err
		})
	case xsql.KindCreateIndex:
		return oneValue(func() (*Value, error) {
			if t := db.currentTx(); t != nil {
				ok, err := t.Collection(st.Collection).EnsureIndex(ctx, st.IndexName, st.IndexExpr, st.Unique)
				return xbson.Boolean(ok), err
			}
			ok, err := db.Collection(st.Collection).EnsureIndex(ctx, st.IndexName, st.IndexExpr, st.Unique)
			return xbson.Boolean(ok), err
		})
	case xsql.KindDropIndex:
		return oneValue(func() (*Value, error) {
			if t := db.currentTx(); t != nil {
				ok, err := t.Collection(st.Collection).DropIndex(ctx, st.IndexName)
				return xbson.Boolean(ok), err
			}
			ok, err := db.Collection(st.Collection).DropIndex(ctx, st.IndexName)
			return xbson.Boolean(ok), err
		})
	case xsql.KindDropCollection:
		return oneValue(func() (*Value, error) {
			if err := db.rejectInTransaction(); err != nil {
				return nil, err
			}
			ok, err := db.DropCollection(ctx, st.Collection)
			return xbson.Boolean(ok), err
		})
	case xsql.KindRenameCollection:
		return oneValue(func() (*Value, error) {
			if err := db.rejectInTransaction(); err != nil {
				return nil, err
			}
			ok, err := db.RenameCollection(ctx, st.Collection, st.Into)
			return xbson.Boolean(ok), err
		})
	case xsql.KindCheckpoint:
		return oneValue(func() (*Value, error) {
			if err := db.rejectInTransaction(); err != nil {
				return nil, err
			}
			n, err := db.Checkpoint(ctx)
			return xbson.Int32(int32(n)), err
		})
	case xsql.KindPragma:
		return db.execPragma(ctx, st, params)
	case xsql.KindBegin:
		return oneValue(func() (*Value, error) {
			ok, err := db.sqlBegin(ctx)
			return xbson.Boolean(ok), err
		})
	case xsql.KindCommit:
		return oneValue(func() (*Value, error) {
			ok, err := db.sqlEnd(ctx, true)
			return xbson.Boolean(ok), err
		})
	case xsql.KindRollback:
		return oneValue(func() (*Value, error) {
			ok, err := db.sqlEnd(ctx, false)
			return xbson.Boolean(ok), err
		})
	case xsql.KindRebuild:
		return oneValue(func() (*Value, error) { return db.execRebuild(st) })
	}
	return seqErr[*Value](fmt.Errorf("xdoc: SQL statement is not supported yet: %q", sql))
}

// execSelect 按有没有 FROM、有没有 INTO 分派到三条路。
func (db *DB) execSelect(ctx context.Context, st *xsql.Statement, params *xbson.Document) iter.Seq2[*Value, error] {
	if st.NoFrom {
		return db.execSelectNoFrom(st, params)
	}
	if st.Into != "" {
		return db.execSelectInto(ctx, st, params)
	}
	qb := db.Collection(st.Collection).Query()
	if t := db.currentTx(); t != nil {
		qb = t.Collection(st.Collection).Query()
	}
	return qb.buildSelect(st, params).runSQL(ctx, st.Explain)
}

// execSelectNoFrom 求值一条不带 FROM 的 SELECT，拿空文档当根。
//
// **只取第一个结果**：表达式求值本身可以产出多个值（比如展开数组），
// 但没有数据源的 SELECT 语义上就是一行。
//
// 非文档的结果包一层 {"expr": ...}，与 [DB.Execute] 其它路径的形状对齐。
func (db *DB) execSelectNoFrom(st *xsql.Statement, params *xbson.Document) iter.Seq2[*Value, error] {
	return func(yield func(*Value, error) bool) {
		n, err := xbexpr.Parse(st.Select)
		if err != nil {
			yield(nil, err)
			return
		}

		root := xbson.NewDocument().Value()
		for v, err := range xbexpr.Execute(n, root, params, db.Collation()) {
			if err != nil {
				yield(nil, err)
				return
			}
			if _, ok := v.AsDocument(); !ok {
				d := xbson.NewDocument()
				d.Set("expr", v)
				v = d.Value()
			}
			yield(v, nil)
			return
		}
	}
}

// sqlParams 把调用方给的参数整理成一篇参数文档。
//
// 单个参数且是文档、文档值或 map 时按键名绑定；其余情况按位置绑定，
// 键是 "0"、"1"……所以表达式里写 @0、@1。
//
// 单个 map 里的值与位置参数都过这个库的映射器；直接给文档的那条路不再编码一次。
func (db *DB) sqlParams(args []any) (*xbson.Document, error) {
	if len(args) == 0 {
		return nil, nil
	}
	if len(args) == 1 {
		switch v := args[0].(type) {
		case *Document:
			return v, nil
		case *Value:
			if d, ok := v.AsDocument(); ok {
				return d, nil
			}
		case map[string]any:
			out := xbson.NewDocument()
			for k, x := range v {
				val, err := db.Marshal(x)
				if err != nil {
					return nil, fmt.Errorf("xdoc: SQL parameter %q: %w", k, err)
				}
				out.Set(k, val)
			}
			return out, nil
		}
	}
	out := xbson.NewDocument()
	for i, a := range args {
		val, err := db.Marshal(a)
		if err != nil {
			return nil, fmt.Errorf("xdoc: SQL parameter %d: %w", i, err)
		}
		out.Set(strconv.Itoa(i), val)
	}
	return out, nil
}

// updateWithParams 在给定事务里执行 UPDATE。
func (t *Tx) updateWithParams(ctx context.Context, st *xsql.Statement, params *xbson.Document) (int, error) {
	tn, err := parseTransform(st.Transform)
	if err != nil {
		return 0, err
	}
	return t.updateManyIn(ctx, st.Collection, tn, firstOf(st.Where), params)
}

// updateWithParamsDB 自开一个事务执行 UPDATE。
//
// 先解析变换表达式再开事务：语法错误不该留下一个开了又回滚的事务。
func (db *DB) updateWithParamsDB(ctx context.Context, st *xsql.Statement, params *xbson.Document) (int, error) {
	tn, err := parseTransform(st.Transform)
	if err != nil {
		return 0, err
	}
	total := 0
	err = db.Transaction(ctx, func(t *Tx) error {
		var e error
		total, e = t.updateManyIn(ctx, st.Collection, tn, firstOf(st.Where), params)
		return e
	})
	return total, err
}

// deleteWithParamsDB 自开一个事务执行 DELETE。
func (db *DB) deleteWithParamsDB(ctx context.Context, st *xsql.Statement, params *xbson.Document) (int, error) {
	total := 0
	err := db.Transaction(ctx, func(t *Tx) error {
		var e error
		total, e = t.deleteManyIn(ctx, st.Collection, firstOf(st.Where), params)
		return e
	})
	return total, err
}

// buildSelect 把一条解析好的 SELECT 铺到查询构建器上。
//
// 参数先绑：WHERE 与投影里可能引用它们。
//
// OFFSET 与 LIMIT 看的是 HasOffset/HasLimit 而不是值是否为零——
// `LIMIT 0` 与没写 LIMIT 是两回事，前者一行都不要。
func (b *QueryBuilder) buildSelect(st *xsql.Statement, params *xbson.Document) *QueryBuilder {
	if params != nil {
		for _, k := range params.Keys() {
			b = b.Param(k, params.Get(k))
		}
	}
	if st.Select != "" {
		b = b.Select(st.Select)
	}
	for _, inc := range st.Includes {
		b = b.Include(inc)
	}
	for _, w := range st.Where {
		b = b.Where(w)
	}
	if st.GroupBy != "" {
		b = b.GroupBy(st.GroupBy)
	}
	if st.Having != "" {
		b = b.Having(st.Having)
	}
	for i, k := range st.OrderBy {
		switch {
		case i == 0 && k.Desc:
			b = b.OrderByDesc(k.Expr)
		case i == 0:
			b = b.OrderBy(k.Expr)
		case k.Desc:
			b = b.ThenByDesc(k.Expr)
		default:
			b = b.ThenBy(k.Expr)
		}
	}
	if st.HasOffset {
		b = b.Skip(st.Offset)
	}
	if st.HasLimit {
		b = b.Limit(st.Limit)
	}
	if st.ForUpdate {
		b = b.ForUpdate()
	}
	return b
}

// runSQL 执行查询并把每篇文档转成值交出来。
//
// EXPLAIN 时只出计划那一篇；集合不存在时计划是 nil，那就一行都不出。
func (b *QueryBuilder) runSQL(ctx context.Context, explain bool) iter.Seq2[*Value, error] {
	if explain {
		return func(yield func(*Value, error) bool) {
			d, err := b.Explain(ctx)
			switch {
			case err != nil:
				yield(nil, err)
			case d != nil:
				yield(d.Value(), nil)
			}
		}
	}
	return func(yield func(*Value, error) bool) {
		for d, err := range b.All(ctx) {
			if err != nil {
				yield(nil, err)
				return
			}
			if !yield(d.Value(), nil) {
				return
			}
		}
	}
}

// execSelectInto 执行 SELECT ... INTO，返回写了几篇。
//
// 目标是虚拟集合时走它自己的写入口；否则写进普通集合，
// 主键生成方式取 INTO 子句里指定的那种，没指定就是 ObjectId。
func (db *DB) execSelectInto(ctx context.Context, st *xsql.Statement, params *xbson.Document) iter.Seq2[*Value, error] {
	return oneValue(func() (*Value, error) {
		if isSystemName(st.Into) {
			return db.selectIntoSystem(ctx, st, params)
		}

		auto := AutoIDObjectID
		if a, ok := autoIDByName(st.IntoAuto); ok {
			auto = a
		}
		qb := db.Collection(st.Collection).Query()
		if t := db.currentTx(); t != nil {
			qb = t.Collection(st.Collection).Query()
		}
		n, err := qb.buildSelect(st, params).IntoWithAutoID(ctx, st.Into, auto)
		return xbson.Int32(int32(n)), err
	})
}

// selectIntoSystem 把查询结果交给一个可写的虚拟集合，比如导出成文件。
func (db *DB) selectIntoSystem(ctx context.Context, st *xsql.Statement, params *xbson.Document) (*Value, error) {
	c, opts, err := lookupSystem(st.Into)
	if err != nil {
		return nil, err
	}
	if c.output == nil {
		return nil, fmt.Errorf("%s do not support as output collection", c.name)
	}
	qb := db.Collection(st.Collection).Query()
	if t := db.currentTx(); t != nil {
		qb = t.Collection(st.Collection).Query()
	}
	n, err := c.output(db, ctx, db.currentTx(), opts, qb.buildSelect(st, params).All(ctx))
	if err != nil {
		return nil, err
	}
	return xbson.Int32(int32(n)), nil
}

// execInsert 执行 INSERT，返回插了几篇。
//
// VALUES 里的每一项按严格 JSON 解析，**全部解析完才开始插**：
// 第三篇写错时前两篇也不会进库。
func (db *DB) execInsert(ctx context.Context, st *xsql.Statement, params *xbson.Document) iter.Seq2[*Value, error] {
	return oneValue(func() (*Value, error) {
		docs := make([]*Document, 0, len(st.Docs))
		for _, src := range st.Docs {
			v, err := xjson.UnmarshalExact([]byte(src))
			if err != nil {
				return nil, fmt.Errorf("xdoc: INSERT value %q: %w", src, err)
			}
			d, ok := v.AsDocument()
			if !ok {
				return nil, fmt.Errorf("xdoc: INSERT expects documents, got %s", v.Type())
			}
			docs = append(docs, d)
		}
		if t := db.currentTx(); t != nil {
			tc := t.Collection(st.Collection)
			if a, ok := autoIDByName(st.AutoID); ok {
				tc = tc.WithAutoID(a)
			}
			n, err := tc.Insert(ctx, docs...)
			return xbson.Int32(int32(n)), err
		}
		c := db.Collection(st.Collection)
		if a, ok := autoIDByName(st.AutoID); ok {
			c = c.WithAutoID(a)
		}
		n, err := c.Insert(ctx, docs...)
		return xbson.Int32(int32(n)), err
	})
}

// execPragma 读或写一个配置项。
//
// 不带值是读，出的是当前值；带值是写，出的是一个布尔说明有没有真的变过。
func (db *DB) execPragma(ctx context.Context, st *xsql.Statement, params *xbson.Document) iter.Seq2[*Value, error] {
	return oneValue(func() (*Value, error) {
		if st.PragmaValue == "" {
			return db.Pragma(st.PragmaName)
		}

		v, err := xjson.UnmarshalExact([]byte(st.PragmaValue))
		if err != nil {
			return nil, err
		}
		changed, err := db.SetPragma(ctx, st.PragmaName, v)
		if err != nil {
			return nil, err
		}
		return xbson.Boolean(changed), nil
	})
}

// currentTx 返回 BEGIN 开出来的那个事务，没有则为 nil。
func (db *DB) currentTx() *Tx {
	db.sqlMu.Lock()
	defer db.sqlMu.Unlock()
	return db.sqlTx
}

// sqlBegin 执行 BEGIN，已经在事务里就返回 false 而不报错。
//
// 登记与开事务分两步：开失败要把登记撤回去，否则这个库句柄会一直以为
// 自己在事务里，之后每一条 DDL 都被拒绝。
func (db *DB) sqlBegin(ctx context.Context) (bool, error) {
	db.sqlMu.Lock()
	defer db.sqlMu.Unlock()
	if db.sqlTx != nil {
		return false, nil
	}

	if err := db.enterTx(ctx); err != nil {
		return false, err
	}
	inner, err := db.beginCore(ctx)
	if err != nil {
		db.exitTx()
		return false, err
	}
	db.sqlTx = &Tx{db: db, tx: inner, ctx: ctx}
	return true, nil
}

// sqlEnd 执行 COMMIT 或 ROLLBACK，本来就不在事务里则返回 false。
//
// 先把事务从库句柄上摘下来再提交：提交失败时它也已经不是当前事务了，
// 不然一次失败的提交会把这个句柄永久卡在事务状态里。提交通知拿的是 COMMIT 这一句的 ctx。
func (db *DB) sqlEnd(ctx context.Context, commit bool) (bool, error) {
	db.sqlMu.Lock()
	t := db.sqlTx
	db.sqlTx = nil
	db.sqlMu.Unlock()
	if t == nil {
		return false, nil
	}
	t.ctx = ctx
	if commit {
		return true, t.Commit()
	}
	return true, t.Rollback()
}

// autoIDByName 把 SQL 里的主键类型关键字转成 [AutoID]。
func autoIDByName(name string) (AutoID, bool) {
	switch name {
	case "GUID":
		return AutoIDGUID, true
	case "INT":
		return AutoIDInt32, true
	case "LONG":
		return AutoIDInt64, true
	case "OBJECTID":
		return AutoIDObjectID, true
	}
	return AutoIDNone, false
}

// firstOf 取第一个 WHERE 子句。
//
// 解析器允许多个 WHERE，但改动类语句只用第一个。
func firstOf(ws []string) string {
	if len(ws) == 0 {
		return ""
	}
	return ws[0]
}

// oneValue 把一个一次性的结果包成序列。
//
// fn 在遍历开始时才执行——语句的副作用因此推迟到调用方真正开始取结果，
// 与 SELECT 那条路的时机一致。
func oneValue(fn func() (*Value, error)) iter.Seq2[*Value, error] {
	return func(yield func(*Value, error) bool) {
		v, err := fn()
		yield(v, err)
	}
}

// execRebuild 执行 REBUILD，返回回收了多少字节。
//
// 参数文档里只认 password 与 collation 两个键，别的静默忽略。
func (db *DB) execRebuild(st *xsql.Statement) (*Value, error) {
	var opts []Option
	if st.Options != "" {
		v, err := xjson.UnmarshalExact([]byte(st.Options))
		if err != nil {
			return nil, err
		}
		d, ok := v.AsDocument()
		if !ok {
			return nil, fmt.Errorf("xdoc: REBUILD expects a document parameter, got %s", v.Type())
		}
		if s, ok := d.Get("password").AsString(); ok {
			opts = append(opts, WithPassword(s))
		}
		if s, ok := d.Get("collation").AsString(); ok {
			opts = append(opts, WithCollation(s))
		}
	}
	res, err := db.Rebuild(opts...)
	if err != nil {
		return nil, err
	}
	return xbson.Int32(int32(res.Reclaimed())), nil
}

// rejectInTransaction 挡住那些不能写在事务里的语句。
//
// 它们要么改头页、要么整体重写文件，都不是事务能覆盖的范围。
func (db *DB) rejectInTransaction() error {
	if db.currentTx() == nil {
		return nil
	}
	return errors.New(
		"The current thread already contains an open transaction. " +
			"Use the Commit/Rollback method to release the previous transaction.")
}
