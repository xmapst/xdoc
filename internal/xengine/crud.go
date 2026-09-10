package xengine

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"math"

	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xcoll"
	"github.com/xmapst/xdoc/internal/xpage"
	"github.com/xmapst/xdoc/internal/xstore"
	"github.com/xmapst/xdoc/internal/xtx"
)

// Insert 插入若干文档，自开一个事务。集合不存在就建出来。
func (e *Engine) Insert(ctx context.Context, name string, docs []*xbson.Document, auto AutoID) (int, error) {
	return e.oneShot(ctx, func(tx *xtx.Transaction) (int, error) {
		return e.InsertIn(ctx, tx, name, docs, auto)
	})
}

// InsertIn 在给定事务里插入若干文档，返回插入了几篇。
//
// 出错时返回已经插入的篇数，但事务是否回滚由调用方决定。每篇之间设一个安全点，
// 让长批次能腾出内存。
func (e *Engine) InsertIn(ctx context.Context, tx *xtx.Transaction, name string,
	docs []*xbson.Document, auto AutoID) (int, error) {
	s, err := e.writeSnapshot(ctx, tx, name, true)
	if err != nil {
		return 0, err
	}
	if err := (work{s}).requireMaintainableIndexes(name); err != nil {
		return 0, err
	}
	n := 0
	loc := new(xstore.VectorLocator)
	for _, doc := range docs {
		if err := tx.Safepoint(); err != nil {
			return n, err
		}
		if err := e.insertDocument(s, name, doc, auto, loc); err != nil {
			return n, err
		}
		e.changed(ctx, tx, Change{Collection: name, Op: ChangeInsert, ID: doc.Get(IDField)})
		n++
	}
	return n, nil
}

// insertDocument 插入一篇文档：先定主键，再写数据块，最后建索引节点。
//
// 建索引失败时把刚写下的数据块删掉，两个错误一并返回。loc 是本批共用的向量节点定位表。
func (e *Engine) insertDocument(s *xtx.Snapshot, name string, doc *xbson.Document, auto AutoID,
	loc *xstore.VectorLocator) error {
	if doc == nil {
		return fmt.Errorf("%w: document is nil", ErrInvalidDocument)
	}
	id, err := e.resolveID(s, name, doc, auto)
	if err != nil {
		return err
	}
	if err := validateID(id); err != nil {
		return err
	}

	raw, err := doc.Encode()
	if err != nil {
		return err
	}
	st := xstore.New(s)
	addr, err := st.InsertDocument(raw)
	if err != nil {
		return err
	}
	if err := e.syncIndexesOnInsert(s, doc, addr, loc); err != nil {
		return errors.Join(err, st.DeleteDocument(addr))
	}
	return nil
}

// syncIndexesOnInsert 给一篇新文档在各索引里建节点，并串成同文档链。
//
// 任何一步失败都把已经建好的节点**按相反次序**摘掉——否则跳表里会留下
// 指向已删数据块的节点。向量索引单独走一遍。
func (e *Engine) syncIndexesOnInsert(s *xtx.Snapshot, doc *xbson.Document, addr xpage.Address,
	loc *xstore.VectorLocator) error {
	docVal := doc.Value()
	st := xstore.New(s)
	var last *xstore.Node

	type placed struct {
		ix   xpage.CollectionIndex
		addr xpage.Address
	}
	var done []placed
	undo := func() error {
		var errs []error
		for i := len(done) - 1; i >= 0; i-- {
			errs = append(errs, st.List(&done[i].ix).DeleteNode(done[i].addr))
		}
		return errors.Join(errs...)
	}
	for _, meta := range s.CollectionPage().Indexes() {
		if meta.Kind != xpage.IndexSkipList {
			continue
		}
		ix := meta
		keys, err := e.indexKeys(&ix, docVal)
		if err != nil {
			return errors.Join(err, undo())
		}
		for _, k := range keys {
			node, err := st.List(&ix).AddNode(k, addr, last, e.coll)
			if err != nil {
				return errors.Join(indexErr(ix.Name, err), undo())
			}
			done = append(done, placed{ix: ix, addr: node.Addr})
			last = node
		}
	}
	if (work{s}).hasVectorIndexes() {
		if err := e.syncVectors(s, docVal, addr, loc); err != nil {
			return errors.Join(err, undo())
		}
	}
	return nil
}

// indexKeys 算出一篇文档在某个索引里该占的那些键。
//
// 主键索引走快路径，直接取 _id 字段，不必求表达式。其余索引交给注入的
// [KeyFunc]；没注入而又有非主键索引时报错。
func (e *Engine) indexKeys(ix *xpage.CollectionIndex, doc *xbson.Value) ([]*xbson.Value, error) {
	if ix.Name == PrimaryIndexName {
		d, ok := doc.AsDocument()
		if !ok {
			return nil, fmt.Errorf("%w: value is %s, not a document", ErrInvalidID, doc.Type())
		}
		return []*xbson.Value{d.Get(IDField)}, nil
	}
	if e.keys == nil {
		return nil, fmt.Errorf("xengine: no key extractor configured; index %q needs one", ix.Name)
	}
	return e.keys(ix.Expression, doc, e.coll)
}

// IndexKeys 算出一篇文档在某个索引里该占的那些键，与插入、改写、回填建节点走的是同一段代码。
//
// 给校验核对多键索引的节点数用：求键与判重稍有出入，核对出来的条数就不作数。
func (e *Engine) IndexKeys(ix *xpage.CollectionIndex, doc *xbson.Value) ([]*xbson.Value, error) {
	return e.indexKeys(ix, doc)
}

// resolveID 定下一篇文档的主键，必要时按 auto 生成一个并写回文档。
//
// 文档自带数字主键时顺手把自增序列抬上去，免得之后生成的主键与它撞上。
func (e *Engine) resolveID(s *xtx.Snapshot, name string, doc *xbson.Document, auto AutoID) (*xbson.Value, error) {
	if doc.Has(IDField) {
		id := doc.Get(IDField)

		if id.Type().IsNumber() {
			if n, ok := asInt64(id); ok {
				if err := e.bumpSequence(s, name, n); err != nil {
					return nil, err
				}
			}
		}
		return id, nil
	}
	var id *xbson.Value
	switch auto {
	case AutoIDObjectID:
		id = xbson.OID(xbson.NewObjectID())
	case AutoIDGUID:
		g, err := newGUID()
		if err != nil {
			return nil, err
		}
		id = xbson.GUID(g)
	case AutoIDInt32, AutoIDInt64:
		limit := int64(math.MaxInt64)
		if auto == AutoIDInt32 {
			limit = math.MaxInt32
		}
		n, err := e.nextSequence(s, name, limit)
		if err != nil {
			return nil, err
		}
		if auto == AutoIDInt32 {
			id = xbson.Int32(int32(n))
		} else {
			id = xbson.Int64(n)
		}
	default:
		return nil, fmt.Errorf("%w: document has no %s and auto id is off", ErrInvalidID, IDField)
	}
	doc.Set(IDField, id)
	return id, nil
}

// Update 按主键更新若干文档，自开一个事务。
func (e *Engine) Update(ctx context.Context, name string, docs []*xbson.Document) (int, error) {
	return e.oneShot(ctx, func(tx *xtx.Transaction) (int, error) {
		return e.UpdateIn(ctx, tx, name, docs)
	})
}

// UpdateIn 在给定事务里更新若干文档，返回真正改到了几篇。
//
// 集合不存在时返回零而不报错；主键查不到的文档跳过，不计数。
func (e *Engine) UpdateIn(ctx context.Context, tx *xtx.Transaction, name string,
	docs []*xbson.Document) (int, error) {
	s, err := e.writeSnapshot(ctx, tx, name, false)
	if err != nil {
		if errors.Is(err, ErrCollectionNotFound) {
			return 0, nil
		}
		return 0, err
	}
	if err := (work{s}).requireMaintainableIndexes(name); err != nil {
		return 0, err
	}
	n := 0
	loc := new(xstore.VectorLocator)
	for _, doc := range docs {
		if err := tx.Safepoint(); err != nil {
			return n, err
		}
		ok, err := e.updateDocument(s, doc, loc)
		if err != nil {
			return n, err
		}
		if ok {
			e.changed(ctx, tx, Change{Collection: name, Op: ChangeUpdate, ID: doc.Get(IDField)})
			n++
		}
	}
	return n, nil
}

// updateDocument 按主键更新一篇文档，第一个返回值说明找没找到。
//
// 数据块就地改写，首块地址不变，所以指向它的索引节点不用动地址，
// 只需按新旧键的差异增删。
//
// **会失败的检查都在动手之前做完**：求索引键与向量、新键的键长与唯一性。
// 写到一半才报错的话，事务里忽略错误照样提交的调用方会留下新数据配旧索引。
func (e *Engine) updateDocument(s *xtx.Snapshot, doc *xbson.Document, loc *xstore.VectorLocator) (bool, error) {
	id := doc.Get(IDField)
	if err := validateID(id); err != nil {
		return false, err
	}
	pk, err := (work{s}).primaryIndex()
	if err != nil {
		return false, err
	}
	st := xstore.New(s)
	node, err := st.List(pk).Find(id, false, xstore.Asc, e.coll)
	if err != nil || node == nil {
		return false, err
	}
	addr := node.DataBlock()

	raw, err := doc.Encode()
	if err != nil {
		return false, err
	}
	plan, err := e.planIndexUpdate(s, node, doc)
	if err != nil {
		return false, err
	}

	if err := st.UpdateDocument(addr, raw); err != nil {
		return false, err
	}
	return true, e.applyIndexUpdate(s, node, addr, plan, loc)
}

// oldKey 是更新前某个索引节点的样子：属于哪个索引槽、键是什么、节点在哪。
type oldKey struct {
	slot uint8
	key  *xbson.Value
	addr xpage.Address
}

// newKey 是更新后某个索引该有的一个键。
type newKey struct {
	slot uint8
	name string
	key  *xbson.Value
}

// indexUpdate 是改写一篇文档时要对索引做的改动，预先算好、查过。
type indexUpdate struct {
	vectors  []pendingVector
	toDelete []xpage.Address
	toInsert []newKey
}

// planIndexUpdate 算出新旧索引键的差异，只读不写。
//
// 先沿同文档链收齐现有的非主键节点，再算出这篇文档现在该有哪些键，
// 两边对一遍：只在旧表里的删掉，只在新表里的加上，两边都有的原样不动——
// 键没变就没必要在跳表里挪一趟。要插的键逐个核对插不插得进去。
func (e *Engine) planIndexUpdate(s *xtx.Snapshot, pkNode *xstore.Node,
	doc *xbson.Document) (*indexUpdate, error) {
	cp := s.CollectionPage()
	st := xstore.New(s)

	var olds []oldKey
	cur := pkNode.NextNode()
	for hops := 0; !cur.IsEmpty(); hops++ {
		if hops > maxIndexNodesPerDocument {
			return nil, fmt.Errorf("%w: document index chain is too long or cyclic", xpage.ErrCorrupt)
		}
		n, err := st.NodeAt(cur)
		if err != nil {
			return nil, err
		}
		k, err := n.Key()
		if err != nil {
			return nil, err
		}
		olds = append(olds, oldKey{slot: n.Slot(), key: k, addr: cur})
		cur = n.NextNode()
	}

	docVal := doc.Value()

	plan := &indexUpdate{}
	if (work{s}).hasVectorIndexes() {
		vecs, err := e.vectorsOf(s, docVal)
		if err != nil {
			return nil, err
		}
		plan.vectors = vecs
	}
	var news []newKey
	for _, meta := range cp.Indexes() {
		if meta.Kind != xpage.IndexSkipList || meta.Name == PrimaryIndexName {
			continue
		}
		ix := meta
		keys, err := e.indexKeys(&ix, docVal)
		if err != nil {
			return nil, err
		}
		for _, k := range keys {
			news = append(news, newKey{slot: ix.Slot, name: ix.Name, key: k})
		}
	}
	if len(olds) == 0 && len(news) == 0 {
		return plan, nil
	}

	plan.toDelete, plan.toInsert = diffIndexKeys(olds, news)
	if err := e.checkInsertable(s, plan.toInsert, plan.toDelete); err != nil {
		return nil, err
	}
	return plan, nil
}

// diffIndexKeys 对比新旧索引键，返回要删的旧节点和要插的新键，次序与各自原表一致。
//
// 键的比较一律按二进制序，不看排序规则：索引里存的是字节。键多时按
// [xbson.BinaryKey] 放进 map 判存在，免得两两比较；有键给不出这种身份
// （小数、文档一类）或者键本来就少，仍旧两两比。
func diffIndexKeys(olds []oldKey, news []newKey) (toDelete []xpage.Address, toInsert []newKey) {
	if len(olds)*len(news) > 64 {
		if del, ins, ok := diffIndexKeysByID(olds, news); ok {
			return del, ins
		}
	}
	same := func(slot uint8, k *xbson.Value, o oldKey) bool {
		return o.slot == slot && o.key.Compare(k, xcoll.Binary) == 0
	}
	for _, o := range olds {
		keep := false
		for _, n := range news {
			if same(n.slot, n.key, o) {
				keep = true
				break
			}
		}
		if !keep {
			toDelete = append(toDelete, o.addr)
		}
	}
	for _, n := range news {
		exists := false
		for _, o := range olds {
			if same(n.slot, n.key, o) {
				exists = true
				break
			}
		}
		if !exists {
			toInsert = append(toInsert, n)
		}
	}
	return toDelete, toInsert
}

// diffIndexKeysByID 是 [diffIndexKeys] 的 map 版，有键没有 [xbson.BinaryKey] 时报 false。
func diffIndexKeysByID(olds []oldKey, news []newKey) ([]xpage.Address, []newKey, bool) {
	type slotKey struct {
		slot uint8
		key  xbson.BinaryKey
	}
	oldIDs := make([]slotKey, len(olds))
	inOld := make(map[slotKey]struct{}, len(olds))
	for i, o := range olds {
		k, ok := o.key.BinaryKey()
		if !ok {
			return nil, nil, false
		}
		oldIDs[i] = slotKey{slot: o.slot, key: k}
		inOld[oldIDs[i]] = struct{}{}
	}
	newIDs := make([]slotKey, len(news))
	inNew := make(map[slotKey]struct{}, len(news))
	for i, n := range news {
		k, ok := n.key.BinaryKey()
		if !ok {
			return nil, nil, false
		}
		newIDs[i] = slotKey{slot: n.slot, key: k}
		inNew[newIDs[i]] = struct{}{}
	}

	var toDelete []xpage.Address
	for i, o := range olds {
		if _, ok := inNew[oldIDs[i]]; !ok {
			toDelete = append(toDelete, o.addr)
		}
	}
	var toInsert []newKey
	for i, n := range news {
		if _, ok := inOld[newIDs[i]]; !ok {
			toInsert = append(toInsert, n)
		}
	}
	return toDelete, toInsert, true
}

// checkInsertable 预先确认 toInsert 里的键都插得进去，不动任何页。
//
// 唯一索引里撞上本文档马上要摘掉的旧节点不算重复——真插的时候它已经删了；
// 反过来，排在前面的新键插进去之后也会占位，得一并算上。
func (e *Engine) checkInsertable(s *xtx.Snapshot, toInsert []newKey, toDelete []xpage.Address) error {
	cp := s.CollectionPage()
	st := xstore.New(s)
	var dropping map[xpage.Address]bool
	ignore := func(a xpage.Address) bool {
		if dropping == nil {
			dropping = make(map[xpage.Address]bool, len(toDelete))
			for _, d := range toDelete {
				dropping[d] = true
			}
		}
		return dropping[a]
	}
	for i, n := range toInsert {
		ix, ok := cp.Index(n.name)
		if !ok {
			return fmt.Errorf("xengine: index %q disappeared mid-update", n.name)
		}
		if err := st.List(ix).CheckAdd(n.key, e.coll, ignore); err != nil {
			return indexErr(n.name, err)
		}
		if !ix.Unique {
			continue
		}
		for _, p := range toInsert[:i] {
			if p.slot == n.slot && p.key.Compare(n.key, e.coll) == 0 {
				return fmt.Errorf("%w: index %q", xstore.ErrDuplicateKey, n.name)
			}
		}
	}
	return nil
}

// applyIndexUpdate 照着 planIndexUpdate 算好的改动写向量、删旧节点、插新节点。
func (e *Engine) applyIndexUpdate(s *xtx.Snapshot, pkNode *xstore.Node,
	addr xpage.Address, plan *indexUpdate, loc *xstore.VectorLocator) error {
	if err := putVectors(s, plan.vectors, addr, loc); err != nil {
		return err
	}
	toDelete, toInsert := plan.toDelete, plan.toInsert
	if len(toDelete) == 0 && len(toInsert) == 0 {
		return nil
	}

	cp := s.CollectionPage()
	st := xstore.New(s)
	last, err := st.Chain(cp, pkNode.Addr).Delete(toDelete, e.coll)
	if err != nil {
		return err
	}
	for _, n := range toInsert {
		ix, ok := cp.Index(n.name)
		if !ok {
			return fmt.Errorf("xengine: index %q disappeared mid-update", n.name)
		}
		node, err := st.List(ix).AddNode(n.key, addr, last, e.coll)
		if err != nil {
			return indexErr(n.name, err)
		}
		last = node
	}
	return nil
}

// maxIndexNodesPerDocument 是同文档链的步数上限，防住成环的链。
const maxIndexNodesPerDocument = 1 << 16

// Upsert 有则更新、无则插入，自开一个事务。
func (e *Engine) Upsert(ctx context.Context, name string, docs []*xbson.Document, auto AutoID) (int, error) {
	return e.oneShot(ctx, func(tx *xtx.Transaction) (int, error) {
		return e.UpsertIn(ctx, tx, name, docs, auto)
	})
}

// UpsertIn 在给定事务里逐篇有则更新、无则插入。
//
// **返回的只是插入的篇数**，更新掉的那些不计入。文档带了非空主键时先试更新，
// 更新不到才走插入。
func (e *Engine) UpsertIn(ctx context.Context, tx *xtx.Transaction, name string,
	docs []*xbson.Document, auto AutoID) (int, error) {
	n := 0
	err := func() error {
		s, err := e.writeSnapshot(ctx, tx, name, true)
		if err != nil {
			return err
		}
		if err := (work{s}).requireMaintainableIndexes(name); err != nil {
			return err
		}
		loc := new(xstore.VectorLocator)
		for _, doc := range docs {
			if err := tx.Safepoint(); err != nil {
				return err
			}
			if doc == nil {
				return fmt.Errorf("%w: document is nil", ErrInvalidDocument)
			}

			if doc.Has(IDField) && doc.Get(IDField).Type() != xbson.TypeNull {
				ok, err := e.updateDocument(s, doc, loc)
				if err != nil {
					return err
				}
				if ok {
					e.changed(ctx, tx, Change{Collection: name, Op: ChangeUpdate, ID: doc.Get(IDField)})
					continue
				}
			}
			if err := e.insertDocument(s, name, doc, auto, loc); err != nil {
				return err
			}
			e.changed(ctx, tx, Change{Collection: name, Op: ChangeInsert, ID: doc.Get(IDField)})
			n++
		}
		return nil
	}()
	return n, err
}

// Delete 按主键删除若干文档，自开一个事务。
func (e *Engine) Delete(ctx context.Context, name string, ids []*xbson.Value) (int, error) {
	return e.oneShot(ctx, func(tx *xtx.Transaction) (int, error) {
		return e.DeleteIn(ctx, tx, name, ids)
	})
}

// DeleteIn 在给定事务里按主键删除若干文档，返回删了几篇。
//
// 集合不存在时返回零而不报错；查不到的主键跳过。每篇要做三件事：
// 摘掉向量节点、删掉数据块、顺着同文档链把所有索引节点摘干净。
func (e *Engine) DeleteIn(ctx context.Context, tx *xtx.Transaction, name string,
	ids []*xbson.Value) (int, error) {
	n := 0
	err := func() error {
		s, err := e.writeSnapshot(ctx, tx, name, false)
		if err != nil {
			if errors.Is(err, ErrCollectionNotFound) {
				return nil
			}
			return err
		}
		if err := (work{s}).requireMaintainableIndexes(name); err != nil {
			return err
		}
		pk, err := (work{s}).primaryIndex()
		if err != nil {
			return err
		}
		st := xstore.New(s)
		loc := new(xstore.VectorLocator)
		for _, id := range ids {
			if err := tx.Safepoint(); err != nil {
				return err
			}

			if err := validateID(id); err != nil {
				return err
			}
			node, err := st.List(pk).Find(id, false, xstore.Asc, e.coll)
			if err != nil {
				return err
			}
			if node == nil {
				continue
			}

			if (work{s}).hasVectorIndexes() {
				if err := e.dropVectors(s, node.DataBlock(), loc); err != nil {
					return err
				}
			}

			if err := st.DeleteDocument(node.DataBlock()); err != nil {
				return err
			}
			if err := st.Chain(s.CollectionPage(), node.Addr).DeleteAll(); err != nil {
				return err
			}
			e.changed(ctx, tx, Change{Collection: name, Op: ChangeDelete, ID: id})
			n++
		}
		return nil
	}()
	return n, err
}

// rangeDeleteBatch 是 [Engine.DeleteRangeIn] 每找齐多少个主键删一次。
const rangeDeleteBatch = 256

// DeleteRangeIn 在给定事务里删掉主键落在闭区间 [lo, hi] 里的全部文档，返回删了几篇。
//
// 先在只读快照上顺着主键索引找，找到了才交给 [Engine.DeleteIn] 转成写快照去删：
// 区间里一篇都没有时不拿集合锁、不让任何页变脏，所在事务写出的字节与没调它时一样。
// 本事务已经拿着这个集合的写快照时就在那一份上找。集合不存在时返回零。
//
// 找一批删一批，删完从 lo 重找，不在遍历途中改动跳表。
func (e *Engine) DeleteRangeIn(ctx context.Context, tx *xtx.Transaction, name string,
	lo, hi *xbson.Value) (int, error) {
	total := 0
	ids := make([]*xbson.Value, 0, rangeDeleteBatch)
	for {
		ids = ids[:0]
		s, err := e.readSnapshot(ctx, tx, name)
		if err != nil || s == nil {
			return total, err
		}
		pk, err := (work{s}).primaryIndex()
		if err != nil {
			return total, err
		}
		st := xstore.New(s)
		node, err := st.List(pk).Find(lo, true, xstore.Asc, e.coll)
		for err == nil && node != nil && len(ids) < rangeDeleteBatch {
			var k *xbson.Value
			if k, err = node.Key(); err != nil {
				break
			}
			// 尾哨兵本来就比 hi 大，这里再点名一次，免得 hi 自己是 MaxValue 时把它删了。
			if k.Type() == xbson.TypeMaxValue || k.Compare(hi, e.coll) > 0 {
				break
			}
			ids = append(ids, k)
			node, err = st.NodeAt(node.Next0())
		}
		if err != nil {
			return total, err
		}
		if len(ids) == 0 {
			return total, nil
		}

		n, err := e.DeleteIn(ctx, tx, name, ids)
		total += n
		if err != nil {
			return total, err
		}
		// 找得到却删不掉只能是索引坏了，接着重找会原地打转。
		if n == 0 {
			return total, fmt.Errorf("%w: collection %q: keys in range were found but none deleted",
				xpage.ErrCorrupt, name)
		}
	}
}

// Find 按主键取一篇文档，自开一个只读事务。查不到时返回 nil 且不报错。
func (e *Engine) Find(ctx context.Context, name string, id *xbson.Value) (*xbson.Document, error) {
	var out *xbson.Document
	tx, err := e.core.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	if err := validateID(id); err != nil {
		return nil, err
	}
	s, err := e.readSnapshot(ctx, tx, name)
	if err != nil || s == nil {
		return nil, err
	}
	pk, err := (work{s}).primaryIndex()
	if err != nil {
		return nil, err
	}
	st := xstore.New(s)
	node, err := st.List(pk).Find(id, false, xstore.Asc, e.coll)
	if err != nil || node == nil {
		return nil, err
	}
	raw, err := st.ReadDocument(node.DataBlock(), nil)
	if err != nil {
		return nil, err
	}
	if out, err = e.decode(raw); err != nil {
		return nil, err
	}
	return out, nil
}

// Scan 按主键次序遍历整个集合，自开一个只读事务。
//
// 事务在遍历结束（或提前跳出）时才回滚，所以整趟看到的是同一份快照。
// 每篇之间查一次上下文，好让长遍历能被取消。
func (e *Engine) Scan(ctx context.Context, name string, order xstore.Order) iter.Seq2[*xbson.Document, error] {
	return func(yield func(*xbson.Document, error) bool) {
		tx, err := e.core.Begin(ctx)
		if err != nil {
			yield(nil, err)
			return
		}
		defer func() { _ = tx.Rollback() }()

		s, err := e.readSnapshot(ctx, tx, name)
		if err != nil {
			yield(nil, err)
			return
		}
		if s == nil {
			return
		}
		pk, err := (work{s}).primaryIndex()
		if err != nil {
			yield(nil, err)
			return
		}
		st := xstore.New(s)
		var buf []byte
		for node, err := range st.List(pk).FindAll(order) {
			if cerr := ctx.Err(); cerr != nil {
				yield(nil, cerr)
				return
			}
			if err != nil {
				yield(nil, err)
				return
			}
			buf, err = st.ReadDocument(node.DataBlock(), buf)
			if err != nil {
				yield(nil, err)
				return
			}
			doc, err := e.decode(buf)
			if err != nil {
				yield(nil, err)
				return
			}
			if !yield(doc, nil) {
				return
			}
		}
	}
}

// FindIn 在给定事务里按主键取一篇文档。
func (e *Engine) FindIn(ctx context.Context, tx *xtx.Transaction, name string,
	id *xbson.Value) (*xbson.Document, error) {
	if err := validateID(id); err != nil {
		return nil, err
	}
	s, err := e.readSnapshot(ctx, tx, name)
	if err != nil || s == nil {
		return nil, err
	}
	pk, err := (work{s}).primaryIndex()
	if err != nil {
		return nil, err
	}
	st := xstore.New(s)
	node, err := st.List(pk).Find(id, false, xstore.Asc, e.coll)
	if err != nil || node == nil {
		return nil, err
	}
	raw, err := st.ReadDocument(node.DataBlock(), nil)
	if err != nil {
		return nil, err
	}
	return e.decode(raw)
}

// ScanIn 在给定事务里按主键次序遍历整个集合。
func (e *Engine) ScanIn(ctx context.Context, tx *xtx.Transaction, name string,
	order xstore.Order) iter.Seq2[*xbson.Document, error] {
	return func(yield func(*xbson.Document, error) bool) {
		s, err := tx.Snapshot(ctx, name, xtx.ModeRead, false)
		if err != nil {
			yield(nil, err)
			return
		}
		if s.CollectionPage() == nil {
			return
		}
		pk, err := (work{s}).primaryIndex()
		if err != nil {
			yield(nil, err)
			return
		}
		st := xstore.New(s)
		var buf []byte
		for node, err := range st.List(pk).FindAll(order) {
			if cerr := ctx.Err(); cerr != nil {
				yield(nil, cerr)
				return
			}
			if err != nil {
				yield(nil, err)
				return
			}
			buf, err = st.ReadDocument(node.DataBlock(), buf)
			if err != nil {
				yield(nil, err)
				return
			}
			doc, err := e.decode(buf)
			if err != nil {
				yield(nil, err)
				return
			}
			if !yield(doc, nil) {
				return
			}
		}
	}
}

// requireMaintainableIndexes 确认集合上的索引本实现都维护得了。
//
// 有维护不了的索引时宁可拒绝写入：写下去会让那个索引与数据脱节。
func (w work) requireMaintainableIndexes(name string) error {
	for _, meta := range w.CollectionPage().Indexes() {
		if meta.Kind != xpage.IndexSkipList && meta.Kind != xpage.IndexVector {
			return fmt.Errorf("%w: collection %q has index %q of kind %d",
				ErrUnsupportedIndex, name, meta.Name, meta.Kind)
		}
	}
	return nil
}
