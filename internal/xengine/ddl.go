package xengine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/xmapst/xdoc/internal/xbexpr"
	"github.com/xmapst/xdoc/internal/xpage"
	"github.com/xmapst/xdoc/internal/xstore"
	"github.com/xmapst/xdoc/internal/xtx"
)

var (
	// ErrInvalidIndexName 表示索引名不合法。
	ErrInvalidIndexName = errors.New("xengine: invalid index name")

	// ErrIndexExists 表示同名索引已经存在但定义不同。
	ErrIndexExists = errors.New("xengine: index already exists with a different expression")

	// ErrIndexNotFound 表示索引不存在。
	ErrIndexNotFound = errors.New("xengine: index not found")

	// ErrDropPrimaryIndex 表示试图删掉主键索引。
	ErrDropPrimaryIndex = errors.New("xengine: the primary index cannot be dropped")

	// ErrIndexExprNotIndexable 表示表达式不能拿来建索引，判据见 [xbexpr.IsIndexable]。
	ErrIndexExprNotIndexable = errors.New(
		"xengine: index expression must reference at least one document field, use only immutable methods, and take no parameters")

	// ErrUniqueMultikeyIndex 表示一篇文档会产出多个键的表达式不能建唯一索引。
	ErrUniqueMultikeyIndex = errors.New("xengine: multikey index expression does not support the unique option")
)

// validateIndexName 校验索引名：非空、不超长、不以美元号开头，字符集同集合名。
func validateIndexName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: empty", ErrInvalidIndexName)
	}
	if len([]rune(name)) > xpage.MaxIndexNameLength {
		return fmt.Errorf("%w: %q is longer than %d characters",
			ErrInvalidIndexName, name, xpage.MaxIndexNameLength)
	}
	if strings.HasPrefix(name, "$") {
		return fmt.Errorf("%w: %q starts with $, which is reserved", ErrInvalidIndexName, name)
	}
	if r, ok := badNameChar(name); !ok {
		return fmt.Errorf("%w: %q cannot contain %q at that position", ErrInvalidIndexName, name, r)
	}
	return nil
}

// EnsureIndex 建一个索引，自开一个事务。返回值说明是不是本次新建的。
//
// 名字或表达式指向主键时直接返回未新建——主键索引本来就有。
func (e *Engine) EnsureIndex(ctx context.Context, coll, name, expr string, unique bool) (bool, error) {
	if err := e.checkIndexSpec(name, expr, unique); err != nil {
		return false, err
	}

	expr = normIndexExpr(expr)

	if name == PrimaryIndexName || expr == PrimaryIndexExpression {
		return false, nil
	}

	var created bool
	err := e.inTx(ctx, func(tx *xtx.Transaction) error {
		var err error
		created, err = e.ensureIndexIn(ctx, tx, coll, name, expr, unique)
		return err
	})
	if err != nil {
		return false, err
	}
	return created, nil
}

// EnsureIndexIn 在给定事务里建一个索引。
func (e *Engine) EnsureIndexIn(ctx context.Context, tx *xtx.Transaction,
	coll, name, expr string, unique bool) (bool, error) {
	if err := e.checkIndexSpec(name, expr, unique); err != nil {
		return false, err
	}
	expr = normIndexExpr(expr)
	if name == PrimaryIndexName || expr == PrimaryIndexExpression {
		return false, nil
	}
	return e.ensureIndexIn(ctx, tx, coll, name, expr, unique)
}

// ensureIndexIn 建索引的实际动作。
//
// 同名索引已经存在时比表达式：**规范化之后**相同就当已经建好，
// 不同则报错，不会改掉现有索引。新建之后立刻回填已有文档。
func (e *Engine) ensureIndexIn(ctx context.Context, tx *xtx.Transaction,
	coll, name, expr string, unique bool) (bool, error) {
	s, err := e.writeSnapshot(ctx, tx, coll, true)
	if err != nil {
		return false, err
	}
	if old, ok := s.CollectionPage().Index(name); ok {
		if normIndexExpr(old.Expression) == expr {
			return false, nil
		}
		return false, fmt.Errorf("%w: %q is %q, not %q", ErrIndexExists, name, old.Expression, expr)
	}
	ix, err := xstore.New(s).CreateIndex(name, expr, unique)
	if err != nil {
		return false, err
	}
	return true, e.backfillIndex(s, tx, ix)
}

// backfillIndex 给已有文档补上新索引的节点。
//
// **先把主键节点的地址全收齐再逐个处理**：一边遍历跳表一边往里插节点，
// 会改动正在遍历的那些页。
//
// 新节点插在同文档链里主键节点的**紧后面**：先把新链的尾巴接上主键原来的后继，
// 再让主键指向新链的头。每次都重新按地址取节点——前一步的插入可能已经
// 让页缓冲失效。
func (e *Engine) backfillIndex(s *xtx.Snapshot, tx *xtx.Transaction, ix *xpage.CollectionIndex) error {
	pk, err := (work{s}).primaryIndex()
	if err != nil {
		return err
	}
	st := xstore.New(s)
	var buf []byte

	var pkAddrs []xpage.Address
	for node, err := range st.List(pk).FindAll(xstore.Asc) {
		if err != nil {
			return err
		}
		pkAddrs = append(pkAddrs, node.Addr)
	}

	for _, pkAddr := range pkAddrs {
		if err := tx.Safepoint(); err != nil {
			return err
		}
		pkNode, err := st.NodeAt(pkAddr)
		if err != nil {
			return err
		}
		buf, err = st.ReadDocument(pkNode.DataBlock(), buf)
		if err != nil {
			return err
		}
		doc, err := e.decode(buf)
		if err != nil {
			return err
		}
		keys, err := e.indexKeys(ix, doc.Value())
		if err != nil {
			return err
		}
		if len(keys) == 0 {
			continue
		}
		dataBlock := pkNode.DataBlock()
		var first, last *xstore.Node
		for _, k := range keys {
			n, err := st.List(ix).AddNode(k, dataBlock, last, e.coll)
			if err != nil {
				return indexErr(ix.Name, err)
			}
			if first == nil {
				first = n
			}
			last = n
		}

		pkNode, err = st.NodeAt(pkAddr)
		if err != nil {
			return err
		}
		tailNode, err := st.NodeAt(last.Addr)
		if err != nil {
			return err
		}
		tailNode.SetNextNode(pkNode.NextNode())
		pkNode, err = st.NodeAt(pkAddr)
		if err != nil {
			return err
		}
		pkNode.SetNextNode(first.Addr)
	}
	return nil
}

// DropIndex 删一个索引，自开一个事务。主键索引删不得。
func (e *Engine) DropIndex(ctx context.Context, coll, name string) (bool, error) {
	if name == PrimaryIndexName {
		return false, ErrDropPrimaryIndex
	}
	dropped := false
	err := e.inTx(ctx, func(tx *xtx.Transaction) error {
		var err error
		dropped, err = e.dropIndexIn(ctx, tx, coll, name)
		return err
	})
	return dropped, err
}

// DropIndexIn 在给定事务里删一个索引。
func (e *Engine) DropIndexIn(ctx context.Context, tx *xtx.Transaction, coll, name string) (bool, error) {
	if name == PrimaryIndexName {
		return false, ErrDropPrimaryIndex
	}
	return e.dropIndexIn(ctx, tx, coll, name)
}

// dropIndexIn 删索引的实际动作。集合或索引不存在时返回未删除且不报错。
//
// 向量索引只需从索引表里摘掉；**删的是最后一个向量索引时**还要把整个集合的
// 向量索引页一并回收。跳表索引则要逐篇文档把该槽的节点从同文档链里摘掉，
// 再删掉头尾哨兵。
func (e *Engine) dropIndexIn(ctx context.Context, tx *xtx.Transaction, coll, name string) (bool, error) {
	dropped := false
	err := func() error {
		s, err := e.writeSnapshot(ctx, tx, coll, false)
		if err != nil {
			if errors.Is(err, ErrCollectionNotFound) {
				return nil
			}
			return err
		}
		cp := s.CollectionPage()
		ix, ok := cp.Index(name)
		if !ok {
			return nil
		}

		if ix.Kind != xpage.IndexSkipList {
			only := len(cp.VectorIndexes()) == 1
			if err := cp.DeleteIndex(name); err != nil {
				return err
			}
			if only {
				if _, err := s.DiscardVectorPages(tx.Safepoint); err != nil {
					return err
				}
			}
			dropped = true
			return nil
		}
		slot := ix.Slot
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
		for _, pkAddr := range pkAddrs {
			if err := tx.Safepoint(); err != nil {
				return err
			}
			if err := e.dropSlotFromChain(s, cp, pkAddr, slot); err != nil {
				return err
			}
		}

		if err := st.List(ix).DeleteNode(ix.Head); err != nil {
			return err
		}
		if err := st.List(ix).DeleteNode(ix.Tail); err != nil {
			return err
		}
		if err := cp.DeleteIndex(name); err != nil {
			return err
		}
		dropped = true
		return nil
	}()
	if err != nil {
		return false, err
	}
	return dropped, nil
}

// dropSlotFromChain 把某篇文档的同文档链上属于某个索引槽的节点全摘掉。
func (e *Engine) dropSlotFromChain(s *xtx.Snapshot, cp *xpage.CollectionPage,
	pk xpage.Address, slot uint8) error {
	var remove []xpage.Address
	st := xstore.New(s)
	cur, err := st.NodeAt(pk)
	if err != nil || cur == nil {
		return err
	}
	addr := cur.NextNode()
	for hops := 0; !addr.IsEmpty(); hops++ {
		if hops > maxIndexNodesPerDocument {
			return fmt.Errorf("%w: document index chain is too long or cyclic", xpage.ErrCorrupt)
		}
		n, err := st.NodeAt(addr)
		if err != nil {
			return err
		}
		if n.Slot() == slot {
			remove = append(remove, addr)
		}
		addr = n.NextNode()
	}
	if len(remove) == 0 {
		return nil
	}
	_, err = st.Chain(cp, pk).Delete(remove, e.coll)
	return err
}

// DropCollection 删掉整个集合，自开一个事务。集合不存在时返回未删除且不报错。
//
// 顺序是：逐篇删数据块与索引节点，再删各索引的头尾哨兵，回收向量索引页，
// 最后把集合页本身变成空页删掉。从头页的集合表里除名要等提交，所以登记的是
// 一个提交回调。地址同样先收齐再删。
func (e *Engine) DropCollection(ctx context.Context, name string) (bool, error) {
	dropped := false
	err := e.inTx(ctx, func(tx *xtx.Transaction) error {
		s, err := e.writeSnapshot(ctx, tx, name, false)
		if err != nil {
			if errors.Is(err, ErrCollectionNotFound) {
				return nil
			}
			return err
		}
		cp := s.CollectionPage()

		pk, err := (work{s}).primaryIndex()
		if err != nil {
			return err
		}
		st := xstore.New(s)
		var addrs []xpage.Address
		var blocks []xpage.Address
		for node, err := range st.List(pk).FindAll(xstore.Asc) {
			if err != nil {
				return err
			}
			addrs = append(addrs, node.Addr)
			blocks = append(blocks, node.DataBlock())
		}
		for i, a := range addrs {
			if err := tx.Safepoint(); err != nil {
				return err
			}
			if err := st.DeleteDocument(blocks[i]); err != nil {
				return err
			}
			if err := st.Chain(cp, a).DeleteAll(); err != nil {
				return err
			}
		}

		for _, meta := range cp.Indexes() {
			if meta.Kind != xpage.IndexSkipList {
				continue
			}
			ix := meta
			if err := st.List(&ix).DeleteNode(ix.Head); err != nil {
				return err
			}
			if err := st.List(&ix).DeleteNode(ix.Tail); err != nil {
				return err
			}
		}

		if len(cp.VectorIndexes()) > 0 {
			if _, err := s.DiscardVectorPages(tx.Safepoint); err != nil {
				return err
			}
		}

		cp.SetType(xpage.PageEmpty)
		if err := s.DeletePage(cp.Page); err != nil {
			return err
		}
		tx.OnCommit(func(h *xpage.HeaderPage) error {
			if !h.DeleteCollection(name) {
				return fmt.Errorf("xengine: collection %q vanished before commit", name)
			}
			return nil
		})
		e.dropSequence(name)
		dropped = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return dropped, nil
}

// RenameCollection 给集合改名，自开一个事务。源集合不存在时返回未改且不报错。
//
// 改名只动头页的集合表，集合页本身不动，所以要等提交才生效。
// 新旧名字只差大小写时报错——集合名的匹配本来就不分大小写。
// 内存里的自增序列跟着改到新名下。
func (e *Engine) RenameCollection(ctx context.Context, old, name string) (bool, error) {
	if strings.EqualFold(old, name) {
		return false, fmt.Errorf("%w: %q must be different from the current name",
			ErrInvalidCollectionName, name)
	}
	if err := ValidateCollectionName(name); err != nil {
		return false, err
	}
	renamed := false
	err := e.inTx(ctx, func(tx *xtx.Transaction) error {
		s, err := tx.Snapshot(ctx, old, xtx.ModeWrite, false)
		if err != nil {
			return err
		}
		if s.CollectionPage() == nil {
			return nil
		}

		if err := tx.InspectHeader(func(h *xpage.HeaderPage) error {
			return h.CanRenameCollection(old, name)
		}); err != nil {
			return err
		}
		tx.OnCommit(func(h *xpage.HeaderPage) error { return h.RenameCollection(old, name) })
		renamed = true
		return nil
	})
	if err != nil {
		return false, err
	}
	if !renamed {
		return false, nil
	}
	e.seqMu.Lock()
	if v, ok := e.seq[old]; ok {
		delete(e.seq, old)
		e.seq[name] = v
	}
	e.seqMu.Unlock()
	return true, nil
}

// CollectionNames 返回所有集合名。**次序不定**，调用方要排序自己排。
func (e *Engine) CollectionNames() []string {
	out, _ := e.core.HeaderValue(func(h *xpage.HeaderPage) ([]string, error) {
		var names []string
		for n := range h.Collections() {
			names = append(names, n)
		}
		return names, nil
	})
	return out
}

// IndexInfo 是一个索引的说明。
type IndexInfo struct {
	Name       string
	Expression string
	Unique     bool

	// Primary 表示这是主键索引。
	Primary bool
}

// Indexes 列出一个集合的索引，自开一个只读事务。
func (e *Engine) Indexes(ctx context.Context, name string) ([]IndexInfo, error) {
	tx, err := e.core.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	return e.IndexesIn(ctx, tx, name)
}

// IndexesIn 在给定事务里列出一个集合的跳表索引。向量索引不在此列。
func (e *Engine) IndexesIn(ctx context.Context, tx *xtx.Transaction, name string) ([]IndexInfo, error) {
	s, err := e.readSnapshot(ctx, tx, name)
	if err != nil || s == nil {
		return nil, err
	}
	metas := s.CollectionPage().Indexes()
	out := make([]IndexInfo, 0, len(metas))
	for _, m := range metas {
		if m.Kind != xpage.IndexSkipList {
			continue
		}
		out = append(out, IndexInfo{
			Name:       m.Name,
			Expression: m.Expression,
			Unique:     m.Unique,
			Primary:    m.Name == PrimaryIndexName,
		})
	}
	return out, nil
}

// checkIndexSpec 校验一份索引定义。
//
// 表达式要解析得通、能拿来建索引；唯一索引的表达式不能产出多个键——
// 同一篇文档占两个键时，唯一性无从谈起。最后拿空文档试跑一次键提取，
// 好在建索引之前就发现表达式用了不支持的方法。
func (e *Engine) checkIndexSpec(name, expr string, unique bool) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("%w: empty", ErrInvalidIndexName)
	}
	if strings.TrimSpace(expr) == "" {
		return fmt.Errorf("xengine: index %q has an empty expression", name)
	}

	n, err := xbexpr.Parse(expr)
	if err != nil {
		return fmt.Errorf("xengine: index %q expression %q: %w", name, expr, err)
	}
	if !xbexpr.IsIndexable(n) {
		return fmt.Errorf("%w: %q", ErrIndexExprNotIndexable, expr)
	}
	if err := validateIndexName(name); err != nil {
		return err
	}

	if unique && n.Cardinality() == xbexpr.Sequence {
		return fmt.Errorf("%w: %q", ErrUniqueMultikeyIndex, expr)
	}
	if name == PrimaryIndexName || expr == PrimaryIndexExpression {
		return nil
	}
	if e.keys == nil {
		return nil
	}

	if _, err := e.keys(expr, nil, e.coll); err != nil {
		return fmt.Errorf("xengine: index %q expression %q: %w", name, expr, err)
	}
	return nil
}

// DeriveIndexName 由表达式推一个索引名：规范化后只留字母和数字。
func DeriveIndexName(expr string) string {
	var b strings.Builder
	for _, r := range normIndexExpr(expr) {
		if r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// normIndexExpr 规范化索引表达式：解析再还原成文本。
//
// 比较两个索引是否同一个定义时靠它，写法上的空白与大小写差异因此不算数。
// 解析不通就原样返回。
func normIndexExpr(expr string) string {
	n, err := xbexpr.Parse(expr)
	if err != nil {
		return expr
	}
	return xbexpr.Print(n)
}
