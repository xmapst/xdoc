package xstore

import (
	"encoding/binary"
	"fmt"
	"math"
	"slices"

	"github.com/xmapst/xdoc/internal/xpage"
	"github.com/xmapst/xdoc/internal/xvector"
)

const (
	// vectorEfConstruction 是建图时每层保留的候选数。
	//
	// 越大图建得越好、越慢；这是建图期的固定值，查询期另有 ef。
	vectorEfConstruction = 24

	// vectorDefaultEfSearch 是查询时候选队列的下限，limit 大时会按 limit 的四倍放大。
	vectorDefaultEfSearch = 32
)

// VectorHit 是一条向量检索结果：命中的文档首块地址，以及它的得分。
type VectorHit struct {
	DataBlock xpage.Address
	// Score 是距离还是相似度取决于度量：点积用相似度、越大越好，其余用距离、越小越好。
	Score float64
}

// nodeDist 是搜索过程中的一个候选：节点地址，加上它到目标的距离与相似度。
type nodeDist struct {
	addr xpage.Address
	dist float64
	sim  float64
}

// newNodeDist 造一个候选，顺手把 NaN 距离折成正无穷。
//
// 维数不一致之类的情况会算出 NaN；折成正无穷才能参与排序，也保证它排在最后。
func newNodeDist(a xpage.Address, dist, sim float64) nodeDist {
	if math.IsNaN(dist) {
		dist = math.Inf(1)
	}
	return nodeDist{addr: a, dist: dist, sim: sim}
}

// VectorGraph 是某一个向量索引的图视图：一个 [Store] 加一份索引元数据。
type VectorGraph struct {
	store Store
	vx    *xpage.VectorIndex
}

// Vector 取某个向量索引的图视图。
func (s Store) Vector(vx *xpage.VectorIndex) VectorGraph { return VectorGraph{store: s, vx: vx} }

// vecCtx 是一次图操作的上下文，带一份向量缓存。
//
// 一次插入或查询会反复量同几个节点的距离，缓存省下重复的读页与解码；
// 它只活到本次操作结束，所以不必担心与页的改动脱节。
type vecCtx struct {
	store Store
	vx    *xpage.VectorIndex
	cache map[xpage.Address][]float32
}

// ctx 开一次操作的上下文。
func (g VectorGraph) ctx() *vecCtx {
	return &vecCtx{store: g.store, vx: g.vx, cache: map[xpage.Address][]float32{}}
}

// metric 返回本索引用的距离度量。
func (c *vecCtx) metric() xvector.Metric { return xvector.Metric(c.vx.Metric) }

// saveMeta 把改过的索引元数据写回集合页。
func (c *vecCtx) saveMeta() error {
	return c.store.CollectionPage().UpdateVectorIndex(c.vx)
}

// getFreeVectorPage 取向量索引空闲链的头页；链空就新建一页。
func (s Store) getFreeVectorPage(head uint32) (*xpage.Page, error) {
	if head == xpage.EmptyPageID {
		return s.NewPage(xpage.PageVectorIndex)
	}
	return s.GetPage(head)
}

// node 取一个向量节点，顺便核对它确实落在向量索引页上。
func (c *vecCtx) node(a xpage.Address) (xpage.VectorNode, *xpage.Page, error) {
	page, err := c.store.GetPage(a.PageID)
	if err != nil {
		return xpage.VectorNode{}, nil, err
	}
	if page.Type() != xpage.PageVectorIndex {
		return xpage.VectorNode{}, nil, fmt.Errorf("xstore: page %d is %s, want vector index page",
			a.PageID, page.Type())
	}
	n, err := page.GetVectorNode(a.Index)
	if err != nil {
		return xpage.VectorNode{}, nil, err
	}
	return n, page, nil
}

// vectorAt 取某个节点的向量，先查缓存。向量可能就在节点里，也可能存在外部文档中。
func (c *vecCtx) vectorAt(a xpage.Address) ([]float32, error) {
	if v, ok := c.cache[a]; ok {
		return v, nil
	}
	n, _, err := c.node(a)
	if err != nil {
		return nil, err
	}
	var v []float32
	if n.HasInlineVector() {
		if v, err = n.ReadVector(); err != nil {
			return nil, err
		}
	} else if v, err = c.readExternal(n); err != nil {
		return nil, err
	}
	c.cache[a] = v
	return v, nil
}

// readExternal 从外部文档读回一个向量。
//
// 维数由索引元数据定死，字节数对不上就报错——不去猜是文档坏了还是维数改了。
func (c *vecCtx) readExternal(n xpage.VectorNode) ([]float32, error) {
	addr := n.ExternalVector()
	dims := int(c.vx.Dimensions)
	if addr.IsEmpty() || dims == 0 {
		return nil, nil
	}
	b, err := c.store.ReadDocument(addr, nil)
	if err != nil {
		return nil, err
	}
	if len(b) != dims*4 {
		return nil, fmt.Errorf("xstore: external vector at %s has %d bytes, want %d",
			addr, len(b), dims*4)
	}
	v := make([]float32, dims)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v, nil
}

// storeExternal 把放不进节点的向量写成一篇文档，按小端逐个存 float32。
func (c *vecCtx) storeExternal(v []float32) (xpage.Address, error) {
	if len(v) == 0 {
		return xpage.EmptyAddress, nil
	}
	b := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return c.store.InsertDocument(b)
}

// distance 量某个节点到目标的距离与相似度。
func (c *vecCtx) distance(a xpage.Address, target []float32) (float64, float64, error) {
	v, err := c.vectorAt(a)
	if err != nil {
		return 0, 0, err
	}
	d, s := c.metric().Distance(v, target)
	return d, s, nil
}

// Upsert 更新某篇文档的向量：先摘掉旧节点，再按新向量插一个。
//
// vec 为 nil 表示这篇文档不再参与向量检索，摘掉就完事。维数必须与索引一致。
func (g VectorGraph) Upsert(dataBlock xpage.Address, vec []float32) error {
	c := g.ctx()
	if err := c.remove(dataBlock); err != nil {
		return err
	}
	if vec == nil {
		return nil
	}
	if len(vec) != int(g.vx.Dimensions) {
		return fmt.Errorf("xstore: vector has %d dimensions, index %q expects %d",
			len(vec), g.vx.Name, g.vx.Dimensions)
	}
	return c.insert(dataBlock, vec)
}

// Delete 把某篇文档的向量节点从图里摘掉。
func (g VectorGraph) Delete(dataBlock xpage.Address) error {
	return g.ctx().remove(dataBlock)
}

// insert 建一个向量节点并把它接进图。
//
// 层数当场掷出，上限是 [xpage.VectorMaxLevels]。向量塞得进节点就内联，
// 否则另存成文档。空闲链的头页放不下时直接新建一页——头页的剩余空间
// 不足以放下一个节点，说明这条链该往前挪了。
//
// 图还是空的时候，新节点直接当根，不用连边。
func (c *vecCtx) insert(dataBlock xpage.Address, vec []float32) error {
	levels := defaultRandomizer.flipTo(xpage.VectorMaxLevels)
	size, inline := xpage.VectorNodeSize(len(vec))

	external := xpage.EmptyAddress
	if !inline {
		var err error
		if external, err = c.storeExternal(vec); err != nil {
			return err
		}
	}
	page, err := c.store.getFreeVectorPage(c.vx.FreePageList)
	if err != nil {
		return err
	}
	if page.FreeBytes() < size {
		if page, err = c.store.NewPage(xpage.PageVectorIndex); err != nil {
			return err
		}
	}
	node, idx, err := page.InsertVectorNode(dataBlock, vec, levels, external)
	if err != nil {
		return err
	}
	page.MarkDirty()
	newAddr := xpage.Address{PageID: page.ID(), Index: idx}
	if c.vx.FreePageList, err = c.store.syncIndexFreeList(page, c.vx.FreePageList); err != nil {
		return err
	}
	c.cache[newAddr] = vec

	if c.vx.Root.IsEmpty() {
		c.vx.Root = newAddr
		return c.saveMeta()
	}
	if err := c.saveMeta(); err != nil {
		return err
	}
	return c.link(newAddr, node, vec, levels)
}

// link 把新节点接进各层的近邻图。
//
// 新节点比当前根还高时先换根。然后从入口的最高层贪心下降到新节点的顶层，
// 再逐层搜出候选、挑出近邻、双向连边。连边可能因对方那边被裁剪而没连上，
// 此时把单向的那条也撤掉——图里不留有去无回的边。
func (c *vecCtx) link(newAddr xpage.Address, node xpage.VectorNode, vec []float32, levels int) error {
	entry := c.vx.Root
	entryNode, _, err := c.node(entry)
	if err != nil {
		return err
	}
	entryTop := entryNode.LevelCount() - 1
	newTop := levels - 1

	if newTop > entryTop {
		c.vx.Root = newAddr
		if err := c.saveMeta(); err != nil {
			return err
		}
		entryTop = newTop
	}

	cur := entry
	for level := entryTop; level > newTop; level-- {
		if cur, err = c.greedy(vec, cur, level, nil); err != nil {
			return err
		}
	}

	maxConnect := min(entryNode.LevelCount()-1, newTop)
	for level := maxConnect; level >= 0; level-- {
		cands, err := c.searchLayer(vec, cur, level, xpage.VectorMaxNeighborsPerLevel, vectorEfConstruction, nil)
		if err != nil {
			return err
		}
		cands = slices.DeleteFunc(cands, func(x nodeDist) bool { return x.addr == newAddr })
		selected := cands.selectNeighbors(xpage.VectorMaxNeighborsPerLevel)

		addrs := make([]xpage.Address, len(selected))
		for i, s := range selected {
			addrs[i] = s.addr
		}
		if err := c.setNeighbors(newAddr, level, addrs); err != nil {
			return err
		}
		for _, a := range addrs {
			ok, err := c.ensureBidirectional(a, newAddr, level)
			if err != nil {
				return err
			}
			if !ok {
				if _, err := c.removeNeighbor(newAddr, level, a); err != nil {
					return err
				}
			}
		}
		if len(selected) > 0 {
			cur = selected[0].addr
		}
	}
	_ = node
	return nil
}

// setNeighbors 改写某节点在某层的邻居表并把页标脏。
func (c *vecCtx) setNeighbors(a xpage.Address, level int, addrs []xpage.Address) error {
	n, page, err := c.node(a)
	if err != nil {
		return err
	}
	if err := n.SetNeighbors(level, addrs); err != nil {
		return err
	}
	page.MarkDirty()
	return nil
}

// removeNeighbor 从某节点某层的邻居表里去掉一个，返回是否真的去掉了。
func (c *vecCtx) removeNeighbor(a xpage.Address, level int, target xpage.Address) (bool, error) {
	n, page, err := c.node(a)
	if err != nil {
		return false, err
	}
	ok, err := n.RemoveNeighbor(level, target)
	if err != nil {
		return false, err
	}
	if ok {
		page.MarkDirty()
	}
	return ok, nil
}

// neighbors 取某节点某层的邻居，空位不算。
func (c *vecCtx) neighbors(a xpage.Address, level int) ([]xpage.Address, error) {
	n, _, err := c.node(a)
	if err != nil {
		return nil, err
	}
	ns, err := n.Neighbors(level)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(ns, func(x xpage.Address) bool { return x.IsEmpty() }), nil
}

// ensureBidirectional 试着在 source 那边也连上 target，返回是否连上了。
//
// 邻居数有上限，加进去要重新裁剪；被裁掉的旧邻居那一侧也要把回边撤掉。
// 裁剪后 target 不在里面，就说明这条边没连成——调用方据此撤掉正向的那条。
func (c *vecCtx) ensureBidirectional(source, target xpage.Address, level int) (bool, error) {
	before, err := c.neighbors(source, level)
	if err != nil {
		return false, err
	}
	next := slices.Clone(before)
	if !slices.Contains(next, target) {
		next = append(next, target)
	}
	pruned, err := c.prune(source, next)
	if err != nil {
		return false, err
	}
	if err := c.setNeighbors(source, level, pruned); err != nil {
		return false, err
	}
	for _, old := range before {
		if slices.Contains(pruned, old) {
			continue
		}

		on, _, err := c.node(old)
		if err != nil {
			return false, err
		}
		if level >= on.LevelCount() {
			continue
		}
		if _, err := c.removeNeighbor(old, level, source); err != nil {
			return false, err
		}
	}
	return slices.Contains(pruned, target), nil
}

// prune 从候选里挑出 source 该保留的邻居。
//
// 先去重、去空、去掉自己，再按到 source 的距离取最近的若干个。
func (c *vecCtx) prune(source xpage.Address, neighbors []xpage.Address) ([]xpage.Address, error) {
	uniq := make([]xpage.Address, 0, len(neighbors))
	for _, a := range neighbors {
		if a.IsEmpty() || a == source || slices.Contains(uniq, a) {
			continue
		}
		uniq = append(uniq, a)
	}
	if len(uniq) == 0 {
		return nil, nil
	}
	sv, err := c.vectorAt(source)
	if err != nil {
		return nil, err
	}
	scored := make(candList, 0, len(uniq))
	for _, a := range uniq {
		v, err := c.vectorAt(a)
		if err != nil {
			return nil, err
		}
		d, _ := c.metric().Distance(sv, v)
		scored = append(scored, newNodeDist(a, d, math.NaN()))
	}
	sel := scored.selectNeighbors(xpage.VectorMaxNeighborsPerLevel)
	out := make([]xpage.Address, len(sel))
	for i, s := range sel {
		out[i] = s.addr
	}
	return out, nil
}

// remove 把某篇文档的向量节点从图里摘掉并回收。
//
// 先记下一个还活着的邻居当作找新根的起点，再逐层把所有指向它的回边撤掉。
// 它正好是根时，从那个起点走一遍连通分量，挑层数最高的当新根；
// 没有邻居可走就把根置空——这个节点本来就是图里仅剩的一个。
func (c *vecCtx) remove(dataBlock xpage.Address) error {
	addr, node, ok, err := c.findByDataBlock(dataBlock)
	if err != nil || !ok {
		return err
	}

	start := xpage.EmptyAddress
	levels := node.LevelCount()
	for level := 0; level < levels && start.IsEmpty(); level++ {
		ns, err := c.neighbors(addr, level)
		if err != nil {
			return err
		}
		if len(ns) > 0 {
			start = ns[0]
		}
	}

	for level := range levels {
		ns, err := c.neighbors(addr, level)
		if err != nil {
			return err
		}
		for _, n := range ns {
			if _, err := c.removeNeighbor(n, level, addr); err != nil {
				return err
			}
		}
	}
	if c.vx.Root == addr {
		root, err := c.selectNewRoot(addr, start)
		if err != nil {
			return err
		}
		c.vx.Root = root
		if err := c.saveMeta(); err != nil {
			return err
		}
	}
	return c.release(addr, node)
}

// selectNewRoot 从 start 出发广度优先地走，挑层数最高的节点当新根。
//
// 只走得到与 start 连通的那部分。图若已经裂成几块，走不到的那些块就此
// 从入口失联——查询只从根出发。
func (c *vecCtx) selectNewRoot(removed, start xpage.Address) (xpage.Address, error) {
	if start.IsEmpty() {
		return xpage.EmptyAddress, nil
	}
	best, bestLevel := xpage.EmptyAddress, 0
	visited := map[xpage.Address]struct{}{}
	queue := []xpage.Address{start}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur == removed {
			continue
		}
		if _, seen := visited[cur]; seen {
			continue
		}
		visited[cur] = struct{}{}

		n, _, err := c.node(cur)
		if err != nil {
			return xpage.EmptyAddress, err
		}
		levels := n.LevelCount()
		if best.IsEmpty() || levels > bestLevel {
			best, bestLevel = cur, levels
		}
		for level := range levels {
			ns, err := n.Neighbors(level)
			if err != nil {
				return xpage.EmptyAddress, err
			}
			for _, a := range ns {
				if !a.IsEmpty() && a != removed {
					queue = append(queue, a)
				}
			}
		}
	}
	return best, nil
}

// scanNodes 扫过本集合所有向量索引页上的节点，fn 返回 true 即停。
//
// 从第 1 页扫到末页，取页失败的直接跳过——扫描是为了找节点，
// 不该被一页读不出来卡住。这是一趟全库扫描，代价与页数成正比。
func (c *vecCtx) scanNodes(fn func(addr xpage.Address, n xpage.VectorNode) (bool, error)) error {
	colID := c.store.CollectionPage().ID()
	for id := uint32(1); id < c.store.PageCount(); id++ {
		page, err := c.store.GetPage(id)
		if err != nil {
			continue
		}
		if page.Type() != xpage.PageVectorIndex || page.ColID() != colID {
			continue
		}
		high := page.HighestIndex()
		if high == xpage.EmptyIndex {
			continue
		}
		for i := range int(high) + 1 {
			n, err := page.GetVectorNode(uint8(i))
			if err != nil {
				continue
			}
			stop, err := fn(xpage.Address{PageID: id, Index: uint8(i)}, n)
			if err != nil {
				return err
			}
			if stop {
				return nil
			}
		}
	}
	return nil
}

// findByDataBlock 按文档首块地址找到它的向量节点。
//
// 只能靠扫描：图里没有从文档到节点的反向索引。
func (c *vecCtx) findByDataBlock(dataBlock xpage.Address) (xpage.Address, xpage.VectorNode, bool, error) {
	var (
		found xpage.Address
		node  xpage.VectorNode
		ok    bool
	)
	err := c.scanNodes(func(a xpage.Address, n xpage.VectorNode) (bool, error) {
		if n.DataBlock() == dataBlock {
			found, node, ok = a, n, true
			return true, nil
		}
		return false, nil
	})
	return found, node, ok, err
}

// release 删掉一个向量节点，连同它的外部向量文档，并更新空闲链。
func (c *vecCtx) release(addr xpage.Address, node xpage.VectorNode) error {
	if !node.HasInlineVector() {
		if ext := node.ExternalVector(); !ext.IsEmpty() {
			if err := c.store.DeleteDocument(ext); err != nil {
				return err
			}
		}
	}
	page, err := c.store.GetPage(addr.PageID)
	if err != nil {
		return err
	}
	if err := page.Delete(addr.Index); err != nil {
		return err
	}
	page.MarkDirty()
	delete(c.cache, addr)
	if c.vx.FreePageList, err = c.store.syncIndexFreeList(page, c.vx.FreePageList); err != nil {
		return err
	}
	return c.saveMeta()
}

// Drop 清空整个向量索引：删掉所有节点与外部向量，根置空。
//
// 先扫一遍把地址都收齐再删——边扫边删会改动正在扫的那些页。
func (g VectorGraph) Drop() error {
	c := g.ctx()

	var addrs []xpage.Address
	if err := c.scanNodes(func(a xpage.Address, _ xpage.VectorNode) (bool, error) {
		addrs = append(addrs, a)
		return false, nil
	}); err != nil {
		return err
	}
	for _, a := range addrs {
		n, _, err := c.node(a)
		if err != nil {
			return err
		}
		if err := c.release(a, n); err != nil {
			return err
		}
	}
	g.vx.Root = xpage.EmptyAddress
	return c.saveMeta()
}
