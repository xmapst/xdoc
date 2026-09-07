// Package xvector 是向量的取值与距离计算。
//
// 逐项乘积一律在 float32 里算，只有累加走 float64：索引里存的就是 float32，
// 全程用双精度会让检索与建索引时算出的距离对不上。
package xvector

import "math"

// Metric 是向量之间的距离度量。
//
// 编号存进索引，不能改。
type Metric uint8

const (
	// MetricEuclidean 是欧氏距离，越小越近。
	MetricEuclidean Metric = 0

	// MetricCosine 是余弦距离 1-cos(θ)，只看方向不看长度：同向 0、正交 1、反向 2。
	MetricCosine Metric = 1

	// MetricDotProduct 是点积。
	//
	// 点积**越大越近**，不是距离，所以这里取相反数当距离用。
	// 它偏好模长大的向量：拿一个向量查它自己，回来的很可能是别人。
	MetricDotProduct Metric = 2
)

// Valid 报告这个编号是不是认识的度量。
func (m Metric) Valid() bool { return m <= MetricDotProduct }

// String 返回度量的名字。
func (m Metric) String() string {
	switch m {
	case MetricEuclidean:
		return "euclidean"
	case MetricCosine:
		return "cosine"
	case MetricDotProduct:
		return "dotproduct"
	default:
		return "unknown"
	}
}

// Distance 算出候选向量到目标向量的距离，越小越近。
//
// similarity 只有点积度量才有值（就是点积本身），其余度量给 NaN——
// 那时距离本身就是可比的量，再造一个相似度只会有两种口径。
//
// 长度不同时给 NaN：那说明索引里的维数与查询给的对不上。
func (m Metric) Distance(candidate, target []float32) (distance, similarity float64) {
	similarity = math.NaN()
	if len(candidate) != len(target) {
		return math.NaN(), similarity
	}
	switch m {
	case MetricCosine:
		return cosine(candidate, target), similarity
	case MetricEuclidean:
		return euclidean(candidate, target), similarity
	case MetricDotProduct:
		similarity = dotProduct(candidate, target)
		return -similarity, similarity
	default:
		return math.NaN(), similarity
	}
}

// Normalize 把 NaN 换成正无穷。
//
// 排序时 NaN 与谁都比不出大小，会让结果次序不定；换成正无穷则稳定地
// 排到最后——算不出距离的候选本来就该垫底。
func Normalize(d float64) float64 {
	if math.IsNaN(d) {
		return math.Inf(1)
	}
	return d
}

// cosine 算余弦距离，零向量给 NaN。
//
// 逐项乘积在 float32 里算完再累加进 float64：这是索引里存的精度，
// 换成全程 float64 会让同一组向量算出与索引不一致的距离。
func cosine(candidate, target []float32) float64 {
	var dot, magC, magT float64
	for i, c := range candidate {
		t := target[i]
		dot += float64(c * t)
		magC += float64(c * c)
		magT += float64(t * t)
	}
	if magC == 0 || magT == 0 {
		return math.NaN()
	}
	return 1 - dot/(math.Sqrt(magC)*math.Sqrt(magT))
}

// euclidean 算欧氏距离。
func euclidean(candidate, target []float32) float64 {
	var sum float64
	for i, c := range candidate {
		d := c - target[i]
		sum += float64(d * d)
	}
	return math.Sqrt(sum)
}

// dotProduct 算点积。
func dotProduct(candidate, target []float32) float64 {
	var sum float64
	for i, c := range candidate {
		sum += float64(c * target[i])
	}
	return sum
}
