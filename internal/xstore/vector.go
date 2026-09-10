package xstore

import (
	"cmp"
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
	loc   *VectorLocator
}

// Vector 取某个向量索引的图视图。
func (s Store) Vector(vx *xpage.VectorIndex) VectorGraph { return VectorGraph{store: s, vx: vx} }

// WithLocator 让本视图按首块地址找节点时查 loc，不再每次都扫一遍文件。loc 为 nil 时照旧扫描。
func (g VectorGraph) WithLocator(loc *VectorLocator) VectorGraph {
	g.loc = loc
	return g
}

// vecCtx 是一次图操作的上下文，带一份向量缓存。
//
// 一次插入或查询会反复量同几个节点的距离，缓存省下重复的读页与解码；
// 它只活到本次操作结束，所以不必担心与页的改动脱节。
type vecCtx struct {
	store Store
	vx    *xpage.VectorIndex
	loc   *VectorLocator
	cache map[xpage.Address][]float32
}

// ctx 开一次操作的上下文。
func (g VectorGraph) ctx() *vecCtx {
	return &vecCtx{store: g.store, vx: g.vx, loc: g.loc, cache: map[xpage.Address][]float32{}}
}

// metric 返回本索引用的距离度量。
func (c *vecCtx) metric() xvector.Metric { return xvector.Metric(c.vx.Metric) }

// saveMeta 把改过的索引元数据写回集合页。
func (c *vecCtx) saveMeta() error {
	return c.store.CollectionPage().UpdateVectorIndex(c.vx)
}

// shared 报告集合上是不是不止本索引这一条向量索引。
//
// 节点上没记它属于哪条索引，只有这时按首块地址扫到的节点才可能是别的索引的。
func (c *vecCtx) shared() bool { return len(c.store.CollectionPage().VectorIndexes()) > 1 }

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
	c.loc.add(c.vx.Slot, dataBlock, newAddr)
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

// remove 把某篇文档在本索引上的向量节点从图里摘掉并回收。
//
// 集合上只有这一条向量索引、又没带定位表时，逐页扫到的第一个节点就是它；
// 否则经定位表挑出该摘的节点，见 [VectorLocator.targets]。
func (c *vecCtx) remove(dataBlock xpage.Address) error {
	if c.loc == nil && !c.shared() {
		addr, node, ok, err := c.findByDataBlock(dataBlock)
		if err != nil || !ok {
			return err
		}
		return c.removeNode(addr, node)
	}
	if c.loc == nil {
		c.loc = new(VectorLocator)
	}
	addrs, err := c.loc.targets(c, dataBlock)
	if err != nil {
		return err
	}
	for _, a := range addrs {
		node, _, err := c.node(a)
		if err != nil {
			return err
		}
		if err := c.removeNode(a, node); err != nil {
			return err
		}
	}
	return nil
}

// removeNode 把一个节点从图里摘掉并回收。
//
// 先记下一个还活着的邻居当作找新根的起点，再逐层把所有指向它的回边撤掉。
// 它正好是根时，从那个起点走一遍连通分量，挑层数最高的当新根；
// 没有邻居可走就把根置空——这个节点本来就是图里仅剩的一个。
func (c *vecCtx) removeNode(addr xpage.Address, node xpage.VectorNode) error {
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

// findByDataBlock 按文档首块地址逐页扫出它的向量节点，碰到第一个就停。
//
// 图里没有从文档到节点的反向索引，只能靠扫描。扫描不分索引，
// 所以只在集合上仅有一条向量索引时才能这样找。
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

// VectorLocator 是一批删改共用的节点定位表：文档首块地址 → 向量节点地址。
//
// 逐篇扫描的代价是篇数乘页数。定位表头一回用到时扫一趟，此后随节点的建立与回收
// 同步增删；同一个首块地址有几个节点时按扫描次序排好。集合上只有一条向量索引时，
// 查到的总是扫描会先碰到的那个——图怎么改、落盘什么字节，都与逐篇扫描一样。
//
// 扫描不分索引，看的是本集合所有向量索引页，所以同一批里的各个向量索引共用一份。
// 节点上又没记它属于哪条索引，同一个首块地址底下可能挂着几条索引各自的节点，
// 要分辨时另外认一回归属，见 [VectorLocator.claim]。
// 它只对建它的那个快照、那个集合有效，批次结束或中途出错就丢掉。零值即可用。
type VectorLocator struct {
	// nodes 为 nil 表示还没扫过。
	nodes map[xpage.Address][]xpage.Address

	// owners、pages 记着认得出归属的节点与页各属于哪条向量索引（记槽号），本批新建的随建随记；
	// claimed 为真表示已从各条索引的空闲链与根认过一回，此后没记着的就是认不出归属的。
	owners  map[xpage.Address]uint8
	pages   map[uint32]uint8
	claimed bool
}

// scan 头一回调用时扫一趟建表。
func (l *VectorLocator) scan(c *vecCtx) error {
	if l.nodes != nil {
		return nil
	}
	nodes := map[xpage.Address][]xpage.Address{}
	if err := c.scanNodes(func(a xpage.Address, n xpage.VectorNode) (bool, error) {
		nodes[n.DataBlock()] = append(nodes[n.DataBlock()], a)
		return false, nil
	}); err != nil {
		return err
	}
	l.nodes = nodes
	return nil
}

// targets 找出本索引这次该摘掉的节点，按扫描次序排。
//
// 集合上只有这一条向量索引、这篇文档也只挂着一个节点时，那个节点就是；否则先认归属，
// 再按 [VectorLocator.mine] 挑。
func (l *VectorLocator) targets(c *vecCtx, dataBlock xpage.Address) ([]xpage.Address, error) {
	if err := l.scan(c); err != nil {
		return nil, err
	}
	addrs := l.nodes[dataBlock]
	if len(addrs) == 0 || (len(addrs) == 1 && !c.shared()) {
		return slices.Clone(addrs), nil
	}
	if err := l.claim(c); err != nil {
		return nil, err
	}
	out := make([]xpage.Address, 0, len(addrs))
	for _, a := range addrs {
		if l.mine(a, c.vx.Slot) {
			out = append(out, a)
		}
	}
	return out, nil
}

// mine 判断认过归属之后，节点 a 该不该由槽号为 slot 的索引摘掉。
//
// 认得出归属的看归属：本索引的摘，别的索引的一个不碰。认不出的是从哪条索引的根都走不到的孤儿，
// 检索碰不到它、建图也再不会给它连边，趁这次清掉，免得文档删了它还指着；但只清落在本索引的页
// 或无主页上的，别的索引的页一概不碰——那条索引处理同一篇文档时自会清掉。
func (l *VectorLocator) mine(a xpage.Address, slot uint8) bool {
	if owner, ok := l.owners[a]; ok {
		return owner == slot
	}
	owner, ok := l.pages[a.PageID]
	return !ok || owner == slot
}

// claim 认一回归属，只认一次：先顺着每条向量索引的空闲页链认页，再从每条索引的根
// 广度优先走遍它的图，走到的节点归这条索引，节点所在的页没认过的也归它。本批新建的早已记着。
//
// 每条索引只从自己的空闲链或新页上划节点，边也只连在同一条索引的节点之间，所以这样认不会认错。
// 走不到的节点认不出归属，但它此后也不会再被走到：新边只连向从根搜得到的节点，
// 删节点、换根只会让走得到的变少。只放着这种节点、又不在空闲链上的页同样认不出。
func (l *VectorLocator) claim(c *vecCtx) error {
	if l.claimed {
		return nil
	}
	l.init()
	vxs := c.store.CollectionPage().VectorIndexes()
	limit := c.store.ChainLimit()
	for _, vx := range vxs {
		for id, n := vx.FreePageList, 0; id != xpage.EmptyPageID; n++ {
			if n > limit {
				return fmt.Errorf("%w: free page list of vector index %q does not end", xpage.ErrCorrupt, vx.Name)
			}
			page, err := c.store.GetPage(id)
			if err != nil {
				return err
			}
			if _, ok := l.pages[id]; !ok {
				l.pages[id] = vx.Slot
			}
			id = page.NextPageID()
		}
	}
	// 走过的节点另记一份：本批新建的早在 owners 里，拿它判重会漏走它们后面的邻居。
	seen := map[xpage.Address]bool{}
	for _, vx := range vxs {
		if vx.Root.IsEmpty() || seen[vx.Root] {
			continue
		}
		seen[vx.Root] = true
		queue := []xpage.Address{vx.Root}
		for len(queue) > 0 {
			a := queue[0]
			queue = queue[1:]
			n, _, err := c.node(a)
			if err != nil {
				return err
			}
			if _, ok := l.owners[a]; !ok {
				l.owners[a] = vx.Slot
			}
			if _, ok := l.pages[a.PageID]; !ok {
				l.pages[a.PageID] = vx.Slot
			}
			for level := range n.LevelCount() {
				ns, err := n.Neighbors(level)
				if err != nil {
					return err
				}
				for _, b := range ns {
					if !b.IsEmpty() && !seen[b] {
						seen[b] = true
						queue = append(queue, b)
					}
				}
			}
		}
	}
	l.claimed = true
	return nil
}

// init 备好记归属的两张表。
func (l *VectorLocator) init() {
	if l.owners == nil {
		l.owners = map[xpage.Address]uint8{}
		l.pages = map[uint32]uint8{}
	}
}

// add 记下槽号为 slot 的索引新建的节点：先记上它与所在页的归属——同一批里别的索引就不会把它
// 当成孤儿清掉；扫过的话再插进扫描次序里它该在的位置，还没扫过就不用插，扫的时候自然看得到。
func (l *VectorLocator) add(slot uint8, dataBlock, addr xpage.Address) {
	if l == nil {
		return
	}
	l.init()
	l.owners[addr] = slot
	l.pages[addr.PageID] = slot
	if l.nodes == nil {
		return
	}
	addrs := l.nodes[dataBlock]
	i, _ := slices.BinarySearchFunc(addrs, addr, compareScanOrder)
	l.nodes[dataBlock] = slices.Insert(addrs, i, addr)
}

// drop 抹掉一个已回收的节点。
func (l *VectorLocator) drop(dataBlock, addr xpage.Address) {
	if l == nil {
		return
	}
	delete(l.owners, addr)
	if l.nodes == nil {
		return
	}
	addrs := slices.DeleteFunc(l.nodes[dataBlock], func(a xpage.Address) bool { return a == addr })
	if len(addrs) == 0 {
		delete(l.nodes, dataBlock)
		return
	}
	l.nodes[dataBlock] = addrs
}

// ownPage 按一页重新挂链之后的样子更新它的归属：整页回收了就忘掉，挂在空闲链上就记给
// 槽号为 slot 的索引。没认过归属时什么也不做。
func (l *VectorLocator) ownPage(id uint32, deleted, onList bool, slot uint8) {
	switch {
	case l == nil || l.pages == nil:
	case deleted:
		delete(l.pages, id)
	case onList:
		l.pages[id] = slot
	}
}

// compareScanOrder 按 [vecCtx.scanNodes] 碰到的先后比较两个节点地址：先页号，再槽号。
func compareScanOrder(a, b xpage.Address) int {
	return cmp.Or(cmp.Compare(a.PageID, b.PageID), cmp.Compare(a.Index, b.Index))
}

// listOwner 返回某页的空闲链归哪条向量索引管：认出了归属的按归属，
// 认不出的（页上只剩孤儿节点、也不在哪条空闲链上）归本索引。
func (c *vecCtx) listOwner(pageID uint32) *xpage.VectorIndex {
	if c.loc == nil || c.loc.pages == nil {
		return c.vx
	}
	slot, ok := c.loc.pages[pageID]
	if !ok || slot == c.vx.Slot {
		return c.vx
	}
	vxs := c.store.CollectionPage().VectorIndexes()
	for i := range vxs {
		if vxs[i].Slot == slot {
			return &vxs[i]
		}
	}
	return c.vx
}

// release 删掉一个向量节点，连同它的外部向量文档，并更新空闲链。
//
// 页要挂回它所属那条索引的空闲链，哪怕这次是本索引替别的索引清孤儿：
// 一页只归一条索引，才不会把别的索引的节点划到本索引的页上。
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
	block := node.DataBlock()
	if err := page.Delete(addr.Index); err != nil {
		return err
	}
	page.MarkDirty()
	delete(c.cache, addr)
	c.loc.drop(block, addr)
	vx := c.listOwner(addr.PageID)
	deleted := page.ItemsCount() == 0
	if vx.FreePageList, err = c.store.syncIndexFreeList(page, vx.FreePageList); err != nil {
		return err
	}
	c.loc.ownPage(addr.PageID, deleted, !deleted && page.PageListSlot() == 0, vx.Slot)
	return c.store.CollectionPage().UpdateVectorIndex(vx)
}

// Drop 清空整个向量索引：删掉所有节点与外部向量，根置空。
//
// 先扫一遍把地址都收齐再删——边扫边删会改动正在扫的那些页。集合上还有别的向量索引时，
// 只删 [VectorLocator.mine] 挑出来的：本索引的节点，和落在本索引的页或无主页上的孤儿。
// 这样本索引的页全数腾空回收，别的索引的节点与页原样留着。
// safepoint 非空时每删一个节点之前调一次。
func (g VectorGraph) Drop(safepoint func() error) error {
	c := g.ctx()

	var addrs []xpage.Address
	if err := c.scanNodes(func(a xpage.Address, _ xpage.VectorNode) (bool, error) {
		addrs = append(addrs, a)
		return false, nil
	}); err != nil {
		return err
	}
	if c.shared() {
		if c.loc == nil {
			c.loc = new(VectorLocator)
		}
		if err := c.loc.claim(c); err != nil {
			return err
		}
		addrs = slices.DeleteFunc(addrs, func(a xpage.Address) bool { return !c.loc.mine(a, c.vx.Slot) })
	}
	for _, a := range addrs {
		if safepoint != nil {
			if err := safepoint(); err != nil {
				return err
			}
		}
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
