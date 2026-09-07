package xdoc

import (
	"context"
	"fmt"
	"math"

	"github.com/xmapst/xdoc/internal/xvector"
)

// VectorMetric 是向量索引比较两个向量用的度量。
type VectorMetric uint8

const (
	// Euclidean 是欧氏距离，越小越近。
	Euclidean VectorMetric = 0

	// Cosine 是余弦距离 1-cos(θ)，只看方向不看长度：同向 0、正交 1、反向 2，
	// 越小越近。
	Cosine VectorMetric = 1

	// DotProduct 是点积，**越大越近**，而且偏好模长大的向量。
	//
	// 它不是距离：内部取相反数当距离用，所以按"距离升序"排出来就是点积降序。
	// 拿一个向量查它自己，回来的很可能是别人；要「最像的」用 [Cosine]。
	DotProduct VectorMetric = 2
)

// String 返回距离度量的名字。
func (m VectorMetric) String() string { return xvector.Metric(m).String() }

// EnsureVectorIndex 在 expr 取出的向量字段上建一条向量索引。
//
// dims 建索引时定死。取出来的向量长度对不上的文档不进索引，既不报错也不参与
// 检索；字段缺失、元素不是数，同样只是不进索引。
//
// 底下是一张分层邻近图，检索是**近似的**：每个节点每层最多记八个邻居，
// 维度越高这个度数越撑不住连通性，召回率随维度下降。低维或小集合上才是准的。
//
// 图的层数用一条定种子的随机流采样，所以同一串操作建出同一张图；但换一个先做过
// 别的写入的进程，序列被前面那些操作推着走，图的形状就不同了。形状只影响召回，
// 不影响距离值本身——要对结果做断言，比距离的集合，别比返回顺序。
func (c *Collection) EnsureVectorIndex(ctx context.Context, name, expr string,
	dims uint16, metric VectorMetric) (bool, error) {
	rel, err := c.db.enter(ctx)
	if err != nil {
		return false, err
	}
	defer rel()
	return c.db.engine.EnsureVectorIndex(ctx, c.name, name, expr, dims, xvector.Metric(metric))
}

// FindNear 取出与 target 的距离不超过 maxDistance 的文档。
//
// 阈值的含义随索引的度量变：[Euclidean] 与 [Cosine] 下它是距离上限（含等于），
// [DotProduct] 下是相似度下限。
func (c *Collection) FindNear(ctx context.Context, index string, target []float32,
	maxDistance float64) ([]*Document, error) {
	return c.db.engine.VectorSearch(ctx, c.name, index, target, maxDistance, 0)
}

// TopKNear 取出与 target 最近的 k 篇文档。k 必须为正。
func (c *Collection) TopKNear(ctx context.Context, index string, target []float32, k int) ([]*Document, error) {
	if k <= 0 {
		return nil, fmt.Errorf("xdoc: TopKNear needs a positive k, got %d", k)
	}
	return c.db.engine.VectorSearch(ctx, c.name, index, target, noThreshold, k)
}

// noThreshold 表示"不按距离设限"，只由 k 截断。
const noThreshold = math.MaxFloat64
