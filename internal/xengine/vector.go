package xengine

import (
	"context"
	"errors"
	"fmt"

	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xpage"
	"github.com/xmapst/xdoc/internal/xstore"
	"github.com/xmapst/xdoc/internal/xtx"
	"github.com/xmapst/xdoc/internal/xvector"
)

// hasVectorIndexes 判断集合上有没有向量索引。没有就跳过所有向量相关的活儿。
func (w work) hasVectorIndexes() bool {
	cp := w.CollectionPage()
	return cp != nil && len(cp.VectorIndexes()) > 0
}

// syncVectors 把一篇文档的向量写进各个向量索引。
//
// 算不出向量（表达式取不到值、维数不对）时传 nil 进去，等于把它从图里摘掉——
// 文档改得不再带向量了，图里也不该留着旧的。
func (e *Engine) syncVectors(s *xtx.Snapshot, docVal *xbson.Value, addr xpage.Address) error {
	cp := s.CollectionPage()
	for _, meta := range cp.VectorIndexes() {
		vx, ok := cp.VectorIndexByName(meta.Name)
		if !ok {
			continue
		}
		ix, ok := cp.Index(meta.Name)
		if !ok {
			return fmt.Errorf("xengine: vector index %q has no matching index entry", meta.Name)
		}
		vec, err := e.vectorOf(ix, vx, docVal)
		if err != nil {
			return indexErr(meta.Name, err)
		}

		if err := xstore.New(s).Vector(vx).Upsert(addr, vec); err != nil {
			return indexErr(meta.Name, err)
		}
	}
	return nil
}

// vectorOf 按索引表达式从一篇文档里取出向量。
//
// 取不到、或者维数与索引不符，都返回 nil 而不报错——那只是说明这篇文档
// 不参与这个索引。没注入求值器却有向量索引时才是真错误。
func (e *Engine) vectorOf(ix *xpage.CollectionIndex, vx *xpage.VectorIndex, docVal *xbson.Value) ([]float32, error) {
	if e.scalar == nil {
		return nil, fmt.Errorf("xengine: no scalar evaluator configured; vector index %q needs one", ix.Name)
	}
	v, err := e.scalar(ix.Expression, docVal, e.coll)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, nil
	}
	vec, ok := xvector.ExtractVector(v, vx.Dimensions)
	if !ok {
		return nil, nil
	}
	return vec, nil
}

// dropVectors 把一篇文档从所有向量索引里摘掉。
func (e *Engine) dropVectors(s *xtx.Snapshot, addr xpage.Address) error {
	cp := s.CollectionPage()
	for _, meta := range cp.VectorIndexes() {
		vx, ok := cp.VectorIndexByName(meta.Name)
		if !ok {
			continue
		}
		if err := xstore.New(s).Vector(vx).Delete(addr); err != nil {
			return indexErr(meta.Name, err)
		}
	}
	return nil
}

// EnsureVectorIndex 建一个向量索引，自开一个事务。返回值说明是不是本次新建的。
func (e *Engine) EnsureVectorIndex(ctx context.Context, coll, name, expr string,
	dims uint16, metric xvector.Metric) (bool, error) {
	var created bool
	err := e.inTx(ctx, func(tx *xtx.Transaction) error {
		var err error
		created, err = e.EnsureVectorIndexIn(ctx, tx, coll, name, expr, dims, metric)
		return err
	})
	return created, err
}

// EnsureVectorIndexIn 在给定事务里建一个向量索引。
//
// 同名索引已经存在时逐项核对：得是向量索引、表达式一致、维数一致、度量一致，
// 全对上就当已经建好，返回未新建；任一项不同则报错——**不会**悄悄改掉现有索引。
//
// 新建之后立刻回填已有文档。
func (e *Engine) EnsureVectorIndexIn(ctx context.Context, tx *xtx.Transaction,
	coll, name, expr string, dims uint16, metric xvector.Metric) (bool, error) {
	if err := e.checkIndexSpec(name, expr, false); err != nil {
		return false, err
	}
	if dims == 0 {
		return false, fmt.Errorf("xengine: vector index %q needs at least one dimension", name)
	}
	if !metric.Valid() {
		return false, fmt.Errorf("xengine: vector index %q has unknown metric %d", name, uint8(metric))
	}
	if name == PrimaryIndexName {
		return false, fmt.Errorf("xengine: %q cannot be a vector index", name)
	}
	expr = normIndexExpr(expr)

	s, err := e.writeSnapshot(ctx, tx, coll, true)
	if err != nil {
		return false, err
	}
	cp := s.CollectionPage()
	if old, ok := cp.Index(name); ok {
		vx, isVector := cp.VectorIndexByName(name)
		switch {
		case !isVector:
			return false, fmt.Errorf("%w: %q is a regular index, not a vector index", ErrIndexExists, name)
		case normIndexExpr(old.Expression) != expr:
			return false, fmt.Errorf("%w: %q is %q, not %q", ErrIndexExists, name, old.Expression, expr)
		case vx.Dimensions != dims:
			return false, fmt.Errorf("%w: %q has %d dimensions, not %d", ErrIndexExists, name, vx.Dimensions, dims)
		case vx.Metric != uint8(metric):
			return false, fmt.Errorf("%w: %q uses metric %s, not %s", ErrIndexExists,
				name, xvector.Metric(vx.Metric), metric)
		}
		return false, nil
	}
	ix, vx, err := cp.AddVectorIndex(name, expr, dims, uint8(metric))
	if err != nil {
		return false, err
	}
	return true, e.backfillVector(s, tx, ix, vx)
}

// backfillVector 给已有文档补上向量节点。
//
// **先把主键节点的地址全收齐再逐个处理**：一边遍历跳表一边往图里插节点，
// 会改动正在遍历的那些页。每篇之间设一个安全点。
func (e *Engine) backfillVector(s *xtx.Snapshot, tx *xtx.Transaction,
	ix *xpage.CollectionIndex, vx *xpage.VectorIndex) error {
	pk, err := (work{s}).primaryIndex()
	if err != nil {
		return err
	}
	st := xstore.New(s)
	var pkAddrs []xpage.Address
	for node, err := range st.List(pk).FindAll(xstore.Asc) {
		if err != nil {
			return err
		}
		pkAddrs = append(pkAddrs, node.Addr)
	}

	var buf []byte
	for _, pkAddr := range pkAddrs {
		if err := tx.Safepoint(); err != nil {
			return err
		}
		pkNode, err := st.NodeAt(pkAddr)
		if err != nil {
			return err
		}
		block := pkNode.DataBlock()
		if buf, err = st.ReadDocument(block, buf); err != nil {
			return err
		}
		doc, err := e.decode(buf)
		if err != nil {
			return err
		}
		vec, err := e.vectorOf(ix, vx, doc.Value())
		if err != nil {
			return indexErr(ix.Name, err)
		}
		if vec == nil {
			continue
		}
		if err := st.Vector(vx).Upsert(block, vec); err != nil {
			return indexErr(ix.Name, err)
		}
	}
	return nil
}

// VectorSearch 在向量索引里找最近的若干篇文档，自开一个只读事务。
func (e *Engine) VectorSearch(ctx context.Context, coll, index string,
	target []float32, maxDistance float64, limit int) ([]*xbson.Document, error) {
	tx, err := e.core.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	return e.VectorSearchIn(ctx, tx, coll, index, target, maxDistance, limit)
}

// VectorSearchIn 在给定事务里做向量检索。
//
// 集合不存在时返回零结果；索引不存在、或者查询向量的维数不对，则报错。
func (e *Engine) VectorSearchIn(ctx context.Context, tx *xtx.Transaction, coll, index string,
	target []float32, maxDistance float64, limit int) ([]*xbson.Document, error) {
	s, err := e.readSnapshot(ctx, tx, coll)
	if err != nil {
		if errors.Is(err, ErrCollectionNotFound) {
			return nil, nil
		}
		return nil, err
	}
	cp := s.CollectionPage()
	if cp == nil {
		return nil, nil
	}
	vx, ok := cp.VectorIndexByName(index)
	if !ok {
		return nil, fmt.Errorf("%w: %q is not a vector index on collection %q",
			ErrIndexNotFound, index, coll)
	}
	if len(target) != int(vx.Dimensions) {
		return nil, fmt.Errorf("xengine: query vector has %d dimensions, index %q expects %d",
			len(target), index, vx.Dimensions)
	}
	st := xstore.New(s)
	hits, err := st.Vector(vx).Search(target, maxDistance, limit)
	if err != nil {
		return nil, err
	}
	out := make([]*xbson.Document, 0, len(hits))
	var buf []byte
	for _, h := range hits {
		if err := tx.Safepoint(); err != nil {
			return nil, err
		}
		if buf, err = st.ReadDocument(h.DataBlock, buf); err != nil {
			return nil, err
		}
		doc, err := e.decode(buf)
		if err != nil {
			return nil, err
		}
		out = append(out, doc)
	}
	return out, nil
}
