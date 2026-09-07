package xquery

import (
	"context"
	"iter"
	"sync"
	"time"

	"github.com/xmapst/xdoc/internal/xbexpr"
	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xcoll"
	"github.com/xmapst/xdoc/internal/xdisk"
	"github.com/xmapst/xdoc/internal/xpage"
	"github.com/xmapst/xdoc/internal/xsort"
	"github.com/xmapst/xdoc/internal/xtx"
)

// evalOpts 是表达式求值要用的库级设定：排序规则和时区。
type evalOpts struct {
	coll xcoll.Collation
	loc  *time.Location
}

// Executor 按库持有查询要用的东西：事务核心、排序规则、外部排序用的临时空间。
//
// 字段的排列是按大小归类的，与用途无关。
type Executor struct {
	core *xtx.Core

	coll xcoll.Collation

	// 外部排序的临时空间按需开一次，开不出来的错误也一并记下。
	diskOnce sync.Once
	disk     *xsort.Disk

	password string
	diskErr  error

	// diskPath 是数据文件路径；为空表示排序用内存而不落盘。
	diskPath string
}

// New 开一个执行器。dataPath 为空时外部排序改用内存。
func New(core *xtx.Core, coll xcoll.Collation, dataPath, password string) *Executor {
	return &Executor{
		core:     core,
		coll:     coll,
		diskPath: dataPath,
		password: password,
	}
}

// evalOptions 取当前的求值设定。
func (e *Executor) evalOptions() evalOpts {
	return evalOpts{coll: e.coll, loc: e.core.DateLocation()}
}

// Close 关掉外部排序的临时空间；没开过就什么也不做。
func (e *Executor) Close() error {
	if e.disk == nil {
		return nil
	}
	return e.disk.Close()
}

// sortDisk 取外部排序用的临时空间，第一次调用时才开。
//
// 有数据文件路径就在它旁边开个加了密的临时文件，否则退回内存。
func (e *Executor) sortDisk() (*xsort.Disk, error) {
	e.diskOnce.Do(func() {
		if e.diskPath != "" {
			e.disk, e.diskErr = xsort.OpenTempDisk(e.diskPath, e.password, xsort.RunSize)
			return
		}
		e.disk, e.diskErr = xsort.NewDisk(xdisk.NewMemStorage(), xsort.RunSize)
	})
	return e.disk, e.diskErr
}

// Explain 只排计划不执行，把计划写成一篇文档。集合不存在时返回 nil。
func (e *Executor) Explain(ctx context.Context, collection string, q *Query) (*xbson.Document, error) {
	return e.explain(ctx, collection, nil, q)
}

// ExplainSource 是 [Executor.Explain] 的虚拟数据源版本。
func (e *Executor) ExplainSource(ctx context.Context, name string, source Source, q *Query) (*xbson.Document, error) {
	return e.explain(ctx, name, q.newVirtualSource(source), q)
}

// explain 是两个 Explain 的共同实现。
func (e *Executor) explain(ctx context.Context, collection string,
	source *virtualSource, q *Query) (*xbson.Document, error) {
	pr, err := e.prepareOn(ctx, nil, collection, source, q)
	if err != nil {
		return nil, err
	}
	defer pr.close()

	if pr.missing {
		if usesSource(q.Select) {
			return e.emptySourceResult(q)
		}
		return nil, nil
	}
	return pr.plan.Explain(), nil
}

// Query 执行一条查询，结果按需产出。**开的是自己的事务，读到边遍历边定的那一版。**
func (e *Executor) Query(ctx context.Context, collection string, q *Query) iter.Seq2[*xbson.Document, error] {
	return e.execute(ctx, collection, nil, q)
}

// QueryIn 在调用方给的事务里执行查询，能看到这个事务尚未提交的改动。
func (e *Executor) QueryIn(ctx context.Context, tx *xtx.Transaction, collection string,
	q *Query) iter.Seq2[*xbson.Document, error] {
	return e.executeIn(ctx, tx, collection, nil, q)
}

// QuerySource 对一串现成的文档执行查询，不碰任何集合。
func (e *Executor) QuerySource(ctx context.Context, source Source, q *Query) iter.Seq2[*xbson.Document, error] {
	return e.execute(ctx, "", q.newVirtualSource(source), q)
}

// newVirtualSource 包一个虚拟数据源。
//
// 查询要排序时才留存文档——排完拿到的是地址，得能按地址回查。
func (q *Query) newVirtualSource(seq Source) *virtualSource {
	if seq == nil {
		seq = func(func(*xbson.Document, error) bool) {}
	}
	return &virtualSource{seq: seq, retain: q != nil && len(q.OrderBy) > 0}
}

// execute 用自己开的事务执行查询。
func (e *Executor) execute(ctx context.Context, collection string,
	source *virtualSource, q *Query) iter.Seq2[*xbson.Document, error] {
	return e.executeIn(ctx, nil, collection, source, q)
}

// executeIn 是查询执行的主干：排计划、跑流水线、按需产出。
//
// 集合不存在时通常直接给空结果；但如果投影是聚合式的，
// 还要按空输入算一遍——`COUNT` 之类该给出 0 而不是什么都不给。
// 每产出一条前查一次取消。
func (e *Executor) executeIn(ctx context.Context, tx *xtx.Transaction, collection string,
	source *virtualSource, q *Query) iter.Seq2[*xbson.Document, error] {
	return func(yield func(*xbson.Document, error) bool) {
		pr, err := e.prepareOn(ctx, tx, collection, source, q)
		if err != nil {
			yield(nil, err)
			return
		}

		defer pr.close()

		if pr.missing {
			if usesSource(q.Select) {
				d, err := e.emptySourceResult(q)
				if err != nil || d != nil {
					yield(d, err)
				}
			}
			return
		}
		for d, err := range e.run(ctx, pr.tx, pr.snap, pr.cp, pr.plan, q, source) {
			if cerr := ctx.Err(); cerr != nil {
				yield(nil, cerr)
				return
			}
			if !yield(d, err) {
				return
			}
			if err != nil {
				return
			}
		}
	}
}

// prepared 是排好计划、开好事务的一次查询。
//
// 字段的排列是按大小归类的。用完必须调 close。
type prepared struct {
	tx      *xtx.Transaction
	snap    *xtx.Snapshot
	cp      *xpage.CollectionPage
	plan    *Plan
	owned   bool
	missing bool
}

// close 回滚自己开的事务；用调用方事务时什么也不做。
func (p *prepared) close() {
	if p.owned {
		_ = p.tx.Rollback()
	}
}

// prepare 用自己开的事务做准备工作。
func (e *Executor) prepare(ctx context.Context, collection string,
	source *virtualSource, q *Query) (*prepared, error) {
	return e.prepareOn(ctx, nil, collection, source, q)
}

// prepareOn 开事务、取快照、排出计划。
//
// 集合不存在且不是虚拟源时，回一份只标了 missing 的结果，由调用方决定怎么办。
// 中途出错会把自己开的事务回滚掉。
func (e *Executor) prepareOn(ctx context.Context, tx *xtx.Transaction, collection string,
	source *virtualSource, q *Query) (*prepared, error) {
	owned := tx == nil
	if owned {
		t, err := e.core.Begin(ctx)
		if err != nil {
			return nil, err
		}
		tx = t
	}
	ok := false
	defer func() {
		if !ok && owned {
			_ = tx.Rollback()
		}
	}()

	snap, err := e.snapshot(ctx, tx, collection, source, q)
	if err != nil {
		return nil, err
	}
	var cp *xpage.CollectionPage
	if snap != nil {
		cp = snap.CollectionPage()
	}
	if cp == nil && source == nil {
		ok = true
		return &prepared{tx: tx, owned: owned, missing: true}, nil
	}
	plan, err := q.optimize(collection, cp, source, e.evalOptions())
	if err != nil {
		return nil, err
	}
	ok = true
	return &prepared{tx: tx, owned: owned, snap: snap, cp: cp, plan: plan}, nil
}

// emptySourceResult 按空输入算一遍聚合投影。
//
// 集合不存在时用它兜底：`COUNT(*)` 该是 0，而不是一条结果都没有。
func (e *Executor) emptySourceResult(q *Query) (*xbson.Document, error) {
	node := substituteSource(q.Select, sourceParam)
	p := withParam(q.Params, sourceParam, xbson.NewArray().Value())
	var out *xbson.Document
	name := defaultFieldName(q.Select)
	for v, err := range xbexpr.Execute(node, nil, p, e.coll) {
		if err != nil {
			return nil, err
		}
		out = wrapValue(v, name)
		break
	}
	return out, nil
}

// snapshot 取集合快照；虚拟源不需要快照。查询声明了 ForUpdate 就按写模式取。
func (e *Executor) snapshot(ctx context.Context, tx *xtx.Transaction, collection string,
	source *virtualSource, q *Query) (*xtx.Snapshot, error) {
	if source != nil {
		return nil, nil
	}
	mode := xtx.ModeRead
	if q.ForUpdate {
		mode = xtx.ModeWrite
	}
	return tx.Snapshot(ctx, collection, mode, false)
}

// collectionPage 取集合的元信息页；集合不存在时返回 nil。
func (e *Executor) collectionPage(ctx context.Context, tx *xtx.Transaction, collection string,
	source *virtualSource, q *Query) (*xpage.CollectionPage, error) {
	s, err := e.snapshot(ctx, tx, collection, source, q)
	if err != nil || s == nil {
		return nil, err
	}
	return s.CollectionPage(), nil
}

// run 按计划搭出流水线：先出命中项、再取文档，然后分组或不分组地往下走。
func (e *Executor) run(ctx context.Context, tx *xtx.Transaction, snap *xtx.Snapshot,
	cp *xpage.CollectionPage, plan *Plan, q *Query,
	source *virtualSource) docStage {
	opts := e.evalOptions()
	lk := e.lookupFor(plan, snap, source, opts)
	hits := e.indexHits(plan, snap, cp)
	src := hits.loadDocs(lk, tx.Safepoint)

	if plan.GroupBy != nil {
		return e.runGrouped(src, plan, q, lk, opts)
	}
	return e.runFlat(ctx, tx, src, plan, q, lk, opts)
}

// indexHits 按计划里选中的方式产出命中项。
func (e *Executor) indexHits(plan *Plan, snap *xtx.Snapshot, cp *xpage.CollectionPage) hitSeq {
	if plan.isVirtual() {
		return hitSeq(plan.index.execute(nil, nil, e.coll))
	}
	return plan.runIndex(snap, cp, e.coll)
}

// lookupFor 挑取文档的方式：虚拟源直接拿，只要索引键时不读数据块，其余读整篇。
func (e *Executor) lookupFor(plan *Plan, snap *xtx.Snapshot, source *virtualSource, opts evalOpts) lookup {
	switch {
	case plan.isVirtual():
		return &virtualLookup{src: source}
	case plan.IsIndexKeyOnly:
		return &keyLookup{p: snap, field: plan.Fields[0], loc: opts.loc}
	}
	return &docLookup{p: snap, loc: opts.loc}
}

// runFlat 搭不分组的那条流水线。
//
// 次序是：展开引用、过滤、排序或分页、再展开一批引用、最后投影。
// **引用分两批展开**是有讲究的：过滤要用到的必须排在过滤之前，
// 其余的放到分页之后，能少展开很多条。
func (e *Executor) runFlat(ctx context.Context, tx *xtx.Transaction, src stage,
	plan *Plan, q *Query, lk lookup, opts evalOpts) docStage {
	resolve := e.refResolver(ctx, tx)
	for _, inc := range plan.IncludeBefore {
		src = src.include(inc, q.Params, opts, resolve)
	}
	for _, f := range plan.Filters {
		src = src.filter(f, q.Params, opts)
	}
	if len(plan.OrderBy) > 0 {
		disk, err := e.sortDisk()
		if err != nil {
			return docStage(errSeq[*xbson.Document](err))
		}
		src = src.sortBy(plan.OrderBy, plan.Offset, plan.Limit, lk, disk, q.Params, opts)
	} else {
		src = src.skipTake(plan.Offset, plan.Limit)
	}
	for _, inc := range plan.IncludeAfter {
		src = src.include(inc, q.Params, opts, resolve)
	}
	if plan.SelectAll {
		return src.selectAll(plan.Select, q.Params, opts)
	}
	return src.selectOne(plan.Select, q.Params, opts)
}

// runGrouped 搭分组的那条流水线。
//
// 分组只并相邻的，所以先按分组键排一遍，**这一遍不能分页**——
// 分页要等分组算完才做得。
func (e *Executor) runGrouped(src stage, plan *Plan, q *Query, lk lookup, opts evalOpts) docStage {
	for _, f := range plan.Filters {
		src = src.filter(f, q.Params, opts)
	}
	if len(plan.GroupBy.OrderBy) > 0 {
		disk, err := e.sortDisk()
		if err != nil {
			return docStage(errSeq[*xbson.Document](err))
		}

		src = src.sortBy(plan.GroupBy.OrderBy, 0, NoLimit, lk, disk, q.Params, opts)
	}
	groups := src.groupBy(plan.GroupBy.Expr, q.Params, opts)
	results := plan.GroupBy.selectGroupStage(groups, plan.OrderBy, q.Params, opts)
	if len(plan.OrderBy) > 0 {
		return results.sortBy(plan.OrderBy, plan.Offset, plan.Limit, opts.coll)
	}
	return results.docs().skipTake(plan.Offset, plan.Limit)
}

// refResolver 按集合名取出展开引用要用的入口。集合不在、或者没有主键索引时给个空壳。
func (e *Executor) refResolver(ctx context.Context, tx *xtx.Transaction) func(string) (*refTarget, error) {
	return func(name string) (*refTarget, error) {
		s, err := tx.Snapshot(ctx, name, xtx.ModeRead, false)
		if err != nil {
			return nil, err
		}
		cp := s.CollectionPage()
		if cp == nil {
			return &refTarget{}, nil
		}
		pk, ok := cp.PrimaryIndex()
		if !ok {
			return &refTarget{pages: s}, nil
		}
		return &refTarget{pages: s, pk: pk}, nil
	}
}

// errSeq 造一个只吐出一个错误的序列。
func errSeq[T any](err error) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		var zero T
		yield(zero, err)
	}
}

// Count 数一数查询能出多少条。
func (e *Executor) Count(ctx context.Context, collection string, q *Query) (int, error) {
	return e.count(ctx, nil, collection, nil, q)
}

// CountIn 在调用方给的事务里计数。
func (e *Executor) CountIn(ctx context.Context, tx *xtx.Transaction, collection string,
	q *Query) (int, error) {
	return e.count(ctx, tx, collection, nil, q)
}

// CountSource 对一串现成的文档计数。
func (e *Executor) CountSource(ctx context.Context, source Source, q *Query) (int, error) {
	return e.count(ctx, nil, "", q.newVirtualSource(source), q)
}

// count 是几个计数入口的共同实现。
//
// 能只数索引就不去读文档；不能的话老老实实跑一遍流水线数。
func (e *Executor) count(ctx context.Context, tx *xtx.Transaction, collection string,
	source *virtualSource, q *Query) (int, error) {
	pr, err := e.prepareOn(ctx, tx, collection, source, q)
	if err != nil {
		return 0, err
	}
	defer pr.close()

	if pr.missing {
		if usesSource(q.Select) {
			d, err := e.emptySourceResult(q)
			if err != nil {
				return 0, err
			}
			if d != nil {
				return 1, nil
			}
		}
		return 0, nil
	}

	if pr.plan.countableFromIndex() {
		return e.countHits(pr)
	}
	n := 0
	for _, err := range e.run(ctx, pr.tx, pr.snap, pr.cp, pr.plan, q, source) {
		if err != nil {
			return 0, err
		}
		n++
	}
	return n, nil
}

// countableFromIndex 判断能不能只数索引命中、不读文档。
//
// 条件是没有分组、没有过滤条件、也没有要在过滤前展开的引用，
// 且投影不是聚合式的——这几样都会改变条数。
// 排序和分页之后的展开不影响条数，所以不在检查之列。
func (p *Plan) countableFromIndex() bool {
	return p.GroupBy == nil &&
		len(p.Filters) == 0 &&
		len(p.IncludeBefore) == 0 &&
		!p.SelectAll
}

// countHits 只走索引数条数，连数据块都不读，中途够数就停。
func (e *Executor) countHits(pr *prepared) (int, error) {
	plan := pr.plan

	if plan.Limit <= 0 {
		return 0, nil
	}
	seen, n := 0, 0
	for _, err := range e.indexHits(plan, pr.snap, pr.cp) {
		if err != nil {
			return 0, err
		}

		seen++
		if seen <= plan.Offset {
			continue
		}
		n++
		if n >= plan.Limit {
			break
		}
	}
	return n, nil
}
