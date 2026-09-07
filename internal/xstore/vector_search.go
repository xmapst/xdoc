package xstore

import (
	"cmp"
	"math"
	"slices"

	"github.com/xmapst/xdoc/internal/xpage"
	"github.com/xmapst/xdoc/internal/xvector"
)

// Search 在图里找与 target 最近的若干篇文档。
//
// 先从根的最高层贪心下降到第 1 层，再在第 0 层做一次带候选队列的搜索。
// 候选数取 limit 的四倍与 [vectorDefaultEfSearch] 的较大者：查得多就搜得宽一些。
//
// 点积度量在这里是反着的：它的「距离」不具可比性，所以筛选与排序都改用相似度，
// maxDistance 也随之当成相似度下限。其余度量按距离筛选、升序排。
func (g VectorGraph) Search(target []float32, maxDistance float64, limit int) ([]VectorHit, error) {
	c := g.ctx()
	if g.vx.Root.IsEmpty() {
		return nil, nil
	}
	entryNode, _, err := c.node(g.vx.Root)
	if err != nil {
		return nil, err
	}
	visited := map[xpage.Address]struct{}{}
	cur := g.vx.Root
	for level := entryNode.LevelCount() - 1; level > 0; level-- {
		if cur, err = c.greedy(target, cur, level, visited); err != nil {
			return nil, err
		}
	}

	ef := vectorDefaultEfSearch
	if limit > 0 {
		ef = max(limit*4, vectorDefaultEfSearch)
	}
	cands, err := c.searchLayer(target, cur, 0, ef, ef, visited)
	if err != nil {
		return nil, err
	}

	dot := c.metric() == xvector.MetricDotProduct

	pruneDistance := maxDistance
	minSimilarity := math.Inf(-1)
	if dot {
		pruneDistance = math.Inf(1)
		if !math.IsInf(maxDistance, 1) && maxDistance < math.MaxFloat64 {
			minSimilarity = maxDistance
		}
	}

	hits := make([]VectorHit, 0, len(cands))
	for _, x := range cands {
		ok := false
		if dot {
			ok = !math.IsNaN(x.sim) && x.sim >= minSimilarity
		} else {
			ok = !math.IsNaN(x.dist) && x.dist <= pruneDistance
		}
		if !ok {
			continue
		}
		n, _, err := c.node(x.addr)
		if err != nil {
			return nil, err
		}
		score := x.dist
		if dot {
			score = x.sim
		}
		hits = append(hits, VectorHit{DataBlock: n.DataBlock(), Score: score})
	}

	if dot {
		slices.SortStableFunc(hits, func(a, b VectorHit) int { return cmp.Compare(b.Score, a.Score) })
	} else {
		slices.SortStableFunc(hits, func(a, b VectorHit) int { return cmp.Compare(a.Score, b.Score) })
	}
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// greedy 在某一层从 start 出发，一直挪到邻居里没有更近的为止。
//
// 这是高层的粗定位：每层只留下最靠近目标的那个点，交给下一层继续。
// 距离先归一化再比，好让不同度量都能按「越小越近」处理。
func (c *vecCtx) greedy(target []float32, start xpage.Address, level int, visited map[xpage.Address]struct{}) (xpage.Address, error) {
	cur := start
	registerVisit(visited, cur)

	d, _, err := c.distance(cur, target)
	if err != nil {
		return xpage.EmptyAddress, err
	}
	curDist := xvector.Normalize(d)

	for improved := true; improved; {
		improved = false
		ns, err := c.neighbors(cur, level)
		if err != nil {
			return xpage.EmptyAddress, err
		}
		for _, a := range ns {
			registerVisit(visited, a)
			d, _, err := c.distance(a, target)
			if err != nil {
				return xpage.EmptyAddress, err
			}
			if nd := xvector.Normalize(d); nd < curDist {
				cur, curDist, improved = a, nd, true
			}
		}
	}
	return cur, nil
}

// searchLayer 在某一层做带候选队列的近邻搜索，返回最近的 maxResults 个。
//
// 从 entry 起，每次取出候选里最近的一个展开它的邻居；候选比当前结果集里
// 最差的还远时就丢掉——结果集已经满了，它带不来更好的邻居。
// seen 保证每个节点只展开一次。
func (c *vecCtx) searchLayer(target []float32, entry xpage.Address, level, maxResults, ef int, visited map[xpage.Address]struct{}) (candList, error) {
	if entry.IsEmpty() {
		return nil, nil
	}
	d, s, err := c.distance(entry, target)
	if err != nil {
		return nil, err
	}
	first := newNodeDist(entry, d, s)

	var results candList
	results, _ = results.insertOrdered(first, max(1, ef))
	candidates := candList{first}
	seen := map[xpage.Address]struct{}{entry: {}}
	registerVisit(visited, entry)

	for len(candidates) > 0 {
		i := candidates.minimumIndex()
		cur := candidates[i]
		candidates = slices.Delete(candidates, i, i+1)

		worst := math.Inf(1)
		if len(results) >= ef {
			worst = results[len(results)-1].dist
		}
		if cur.dist > worst {
			continue
		}
		ns, err := c.neighbors(cur.addr, level)
		if err != nil {
			return nil, err
		}
		for _, a := range ns {
			if _, dup := seen[a]; dup {
				continue
			}
			seen[a] = struct{}{}
			registerVisit(visited, a)

			d, s, err := c.distance(a, target)
			if err != nil {
				return nil, err
			}
			cand := newNodeDist(a, d, s)
			var ok bool
			if results, ok = results.insertOrdered(cand, max(1, ef)); ok {
				candidates = append(candidates, cand)
			}
		}
	}
	return results.selectNeighbors(max(1, maxResults)), nil
}

// registerVisit 记下访问过的节点；visited 为 nil 时什么也不做，建图时就不需要这份记录。
func registerVisit(visited map[xpage.Address]struct{}, a xpage.Address) {
	if visited == nil || a.IsEmpty() {
		return
	}
	visited[a] = struct{}{}
}

// candList 是一串候选，按距离升序维护。
type candList []nodeDist

// minimumIndex 返回距离最小那个候选的下标。空切片会 panic，调用方已先判过非空。
func (l candList) minimumIndex() int {
	idx, best := 0, l[0].dist
	for i := 1; i < len(l); i++ {
		if l[i].dist < best {
			idx, best = i, l[i].dist
		}
	}
	return idx
}

// insertOrdered 按距离把一个候选插进有序表，最多留 maxSize 个，返回是否插进去了。
//
// 比表里所有人都远且表已经满了，就插不进去——调用方据此判断这个候选
// 还值不值得展开。
func (l candList) insertOrdered(item nodeDist, maxSize int) (candList, bool) {
	if maxSize <= 0 {
		return l, false
	}
	inserted := false
	if i := slices.IndexFunc(l, func(x nodeDist) bool { return item.dist < x.dist }); i >= 0 {
		l = slices.Insert(l, i, item)
		inserted = true
	} else if len(l) < maxSize {
		l = append(l, item)
		inserted = true
	}
	if len(l) > maxSize {
		l = l[:len(l)-1]
	}
	return l, inserted
}

// selectNeighbors 按距离取最近的 k 个，同一地址只算一次。原表不动。
func (l candList) selectNeighbors(k int) candList {
	if len(l) == 0 || k <= 0 {
		return nil
	}
	sorted := slices.Clone(l)
	slices.SortStableFunc(sorted, func(a, b nodeDist) int { return cmp.Compare(a.dist, b.dist) })

	out := make(candList, 0, min(k, len(sorted)))
	seen := make(map[xpage.Address]struct{}, len(sorted))
	for _, x := range sorted {
		if _, dup := seen[x.addr]; dup {
			continue
		}
		seen[x.addr] = struct{}{}
		out = append(out, x)
		if len(out) == k {
			break
		}
	}
	return out
}
