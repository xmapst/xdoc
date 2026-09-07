// Package xquery 是查询层：一条查询怎么排成计划，再怎么一条条产出结果。
//
// [Query] 描述要查什么，optimize 把它排成 [Plan]——挑哪个索引、
// 哪些过滤留给流水线、排序和分组要不要自己再做一遍。[Executor] 按计划
// 搭出一条流水线跑起来。
//
// **结果是按需产出的**：走索引、取文档、过滤、投影这几步交错进行，
// 不会先把结果全攒起来。两处例外——排序和聚合式投影绕不开，
// 排序落到外部排序器上，聚合把文档攒成一个数组。
//
// [Executor.Explain] 只排计划不执行，把计划写成一篇文档。
package xquery

import (
	"errors"
	"fmt"
	"math"

	"github.com/xmapst/xdoc/internal/xbexpr"
	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xstore"
)

// Order 是遍历方向，与索引层用的是同一个类型。
type Order = xstore.Order

const (
	// Ascending 由小到大。
	Ascending = xstore.Asc

	// Descending 由大到小。
	Descending = xstore.Desc
)

// NoLimit 表示不限条数。
const NoLimit = math.MaxInt32

// ErrQuery 表示查询本身写得不对。
var ErrQuery = errors.New("xquery: invalid query")

// ErrInternal 表示走到了不该走到的分支，属于程序错误。
var ErrInternal = errors.New("xquery: internal error")

// OrderSegment 是排序里的一级：按什么表达式排，往哪个方向。
type OrderSegment struct {
	Expr  xbexpr.Node
	Order Order
}

// Query 是一条查询的描述，还没有排出执行计划。
//
// 各字段直接填也行，用 Add/Set 那几个方法能顺带做校验。
type Query struct {
	// Select 是投影表达式；为空时取整篇文档。
	Select xbexpr.Node

	// Includes 是要展开的引用字段，每个表达式只能引用一个字段。
	Includes []xbexpr.Node

	// Where 是过滤条件，多个之间是与的关系。
	Where []xbexpr.Node

	// OrderBy 是排序键，按书写次序。
	OrderBy []OrderSegment

	// GroupBy 是分组表达式，Having 是分组之后的过滤条件。
	GroupBy xbexpr.Node
	Having  xbexpr.Node

	// Params 是具名参数表。
	Params *xbson.Document

	// Offset 跳过几条，Limit 取几条。
	Offset int
	Limit  int

	// ForUpdate 表示查询要拿写锁。
	ForUpdate bool
}

// NewQuery 造一条默认查询：取整篇文档，不限条数。
func NewQuery() *Query {
	return &Query{Select: rootExpr(), Limit: NoLimit}
}

// rootExpr 返回指代整篇文档的表达式，也就是一个没有步骤的路径。
func rootExpr() xbexpr.Node { return &xbexpr.PathNode{} }

// isRootExpr 判断一个表达式是不是就指整篇文档。
func isRootExpr(n xbexpr.Node) bool {
	p, ok := n.(*xbexpr.PathNode)
	return ok && p != nil && len(p.Steps) == 0 && p.Root == xbexpr.RootDocument
}

// AddWhere 添一个过滤条件。
func (q *Query) AddWhere(n xbexpr.Node) error {
	if n == nil {
		return fmt.Errorf("%w: WHERE expression is nil", ErrQuery)
	}
	q.Where = append(q.Where, n)
	return nil
}

// AddInclude 添一个要展开的引用字段。表达式必须**正好**引用一个字段。
func (q *Query) AddInclude(n xbexpr.Node) error {
	if n == nil {
		return fmt.Errorf("%w: INCLUDE expression is nil", ErrQuery)
	}
	if f := fieldsOf(n); len(f) != 1 {
		return fmt.Errorf("%w: INCLUDE `%s` must reference exactly one field, it references %d",
			ErrQuery, xbexpr.Print(n), len(f))
	}
	q.Includes = append(q.Includes, n)
	return nil
}

// SetOrderBy 设第一级排序；已经设过就报错，后续用 [Query.ThenBy]。
func (q *Query) SetOrderBy(n xbexpr.Node, o Order) error {
	if len(q.OrderBy) > 0 {
		return fmt.Errorf("%w: ORDER BY is already defined, use ThenBy to add more segments", ErrQuery)
	}
	return q.ThenBy(n, o)
}

// ThenBy 再添一级排序。
func (q *Query) ThenBy(n xbexpr.Node, o Order) error {
	if n == nil {
		return fmt.Errorf("%w: ORDER BY expression is nil", ErrQuery)
	}
	if o != Ascending && o != Descending {
		return fmt.Errorf("%w: order %d is neither ascending nor descending", ErrQuery, o)
	}
	q.OrderBy = append(q.OrderBy, OrderSegment{Expr: n, Order: o})
	return nil
}

// SetGroupBy 设分组表达式。
func (q *Query) SetGroupBy(n xbexpr.Node) error {
	if q.GroupBy != nil {
		return fmt.Errorf("%w: GROUP BY is already defined", ErrQuery)
	}
	if n == nil {
		return fmt.Errorf("%w: GROUP BY expression is nil", ErrQuery)
	}
	q.GroupBy = n
	return nil
}

// SetHaving 设分组之后的过滤条件。
func (q *Query) SetHaving(n xbexpr.Node) error {
	if q.Having != nil {
		return fmt.Errorf("%w: HAVING is already defined", ErrQuery)
	}
	if n == nil {
		return fmt.Errorf("%w: HAVING expression is nil", ErrQuery)
	}
	q.Having = n
	return nil
}

// Skip 跳过前 n 条。
func (q *Query) Skip(n int) { q.Offset = n }

// Take 只取 n 条。
func (q *Query) Take(n int) { q.Limit = n }

// normalize 补默认值并做一遍自洽检查。
//
// 两条约束：分组查询不支持展开引用；HAVING 必须配 GROUP BY。
func (q *Query) normalize() error {
	if q.Select == nil {
		q.Select = rootExpr()
	}
	if q.Limit < 0 {
		return fmt.Errorf("%w: LIMIT %d is negative", ErrQuery, q.Limit)
	}
	if q.Offset < 0 {
		return fmt.Errorf("%w: OFFSET %d is negative", ErrQuery, q.Offset)
	}
	if q.GroupBy != nil && len(q.Includes) > 0 {
		return fmt.Errorf("%w: GROUP BY does not support INCLUDE", ErrQuery)
	}
	if q.Having != nil && q.GroupBy == nil {
		return fmt.Errorf("%w: HAVING requires GROUP BY", ErrQuery)
	}
	return nil
}
