package xdoc

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xmapst/xdoc/internal/xbexpr"
	"github.com/xmapst/xdoc/internal/xquery"
)

// cursor 是一次还没走完的查询，供 $open_cursors 观察。
//
// 遍历期间查询占着快照与集合锁，调用方在 for range 里中途 break 去做别的事，
// 这条查询就一直挂着。不看这张表的话，现场只看得到「写操作在等锁」，
// 看不出是谁占着。
type cursor struct {
	id         uint64
	txID       uint32
	collection string
	mode       string
	sql        string

	// fetched 是已经交给调用方的行数。
	fetched atomic.Int64

	// accum 与 startedAt 一起算「引擎自己花掉的时间」：startedAt 非零表示引擎正在
	// 跑，停下来交给调用方时把这一段累进 accum。
	//
	// 所以 elapsedMS 里**不含**调用方处理每行的时间——要找的是「谁慢」，
	// 把等调用方的时间算进来会指向错的那一半。
	accum     atomic.Int64
	startedAt atomic.Int64
}

// start 开始计时。已经在计时就不动，所以重复调用是安全的。
func (c *cursor) start() { c.startedAt.CompareAndSwap(0, time.Now().UnixNano()) }

// stop 停止计时并把这一段累进 accum。没在计时就什么都不做。
func (c *cursor) stop() {
	if s := c.startedAt.Swap(0); s != 0 {
		c.accum.Add(time.Now().UnixNano() - s)
	}
}

// running 报告引擎此刻是在跑，还是停下来等调用方取下一行。
func (c *cursor) running() bool { return c.startedAt.Load() != 0 }

// fetchedCount 返回已经交出去多少行。
func (c *cursor) fetchedCount() int64 { return c.fetched.Load() }

// elapsed 返回引擎累计花掉的时间，含当前这一段（如果正在跑）。
func (c *cursor) elapsed() time.Duration {
	d := c.accum.Load()
	if s := c.startedAt.Load(); s != 0 {
		d += time.Now().UnixNano() - s
	}
	return time.Duration(d)
}

// cursorSet 是一个库上所有在途游标的集合，按自增号登记。
type cursorSet struct {
	mu   sync.Mutex
	next uint64
	open map[uint64]*cursor
}

// add 登记一个游标并给它一个自增号。
func (s *cursorSet) add(c *cursor) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open == nil {
		s.open = make(map[uint64]*cursor)
	}
	s.next++
	c.id = s.next
	s.open[c.id] = c
}

// remove 把一个游标从登记里去掉。
func (s *cursorSet) remove(c *cursor) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.open, c.id)
}

// list 取一份快照，按登记顺序排。
//
// 排序在锁外做：拿到切片之后就与集合无关了，没必要占着锁排。
func (s *cursorSet) list() []*cursor {
	s.mu.Lock()
	out := make([]*cursor, 0, len(s.open))
	for _, c := range s.open {
		out = append(out, c)
	}
	s.mu.Unlock()

	slices.SortFunc(out, func(a, b *cursor) int { return cmp.Compare(a.id, b.id) })
	return out
}

// track 把这次查询登记进 $open_cursors，返回它与一个注销函数。
//
// 调用方必须调那个函数，否则这条游标会一直挂在表里。t 为 nil 表示这次查询自开
// 一次性事务，不属于任何显式事务。
func (b *QueryBuilder) track(t *Tx) (*cursor, func()) {
	c := &cursor{
		collection: b.c.name,
		mode:       "read",
		sql:        queryToSQL(b.c.name, b.q),
	}
	if b.q.ForUpdate {
		c.mode = "write"
	}
	if t != nil {
		c.txID = t.tx.ID()
	}
	b.c.db.cursors.add(c)
	c.start()
	return c, func() {
		c.stop()
		b.c.db.cursors.remove(c)
	}
}

// queryToSQL 把一个查询还原成 SQL 文本，只用于 $open_cursors 的 sql 字段。
//
// 子句顺序刻意与 SQL 的书写顺序不同——WHERE 排在最后。这一行是给人看的，
// 不保证能再解析回来。
func queryToSQL(coll string, q *xquery.Query) string {
	var b strings.Builder
	b.WriteString("SELECT ")
	b.WriteString(xbexpr.Print(q.Select))
	b.WriteString(" FROM ")
	b.WriteString(coll)

	if len(q.Includes) > 0 {
		b.WriteString(" INCLUDE ")
		for i, n := range q.Includes {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(xbexpr.Print(n))
		}
	}
	if q.GroupBy != nil {
		b.WriteString(" GROUP BY ")
		b.WriteString(xbexpr.Print(q.GroupBy))
	}
	if q.Having != nil {
		b.WriteString(" HAVING ")
		b.WriteString(xbexpr.Print(q.Having))
	}
	if len(q.OrderBy) > 0 {
		b.WriteString(" ORDER BY ")
		for i, seg := range q.OrderBy {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(xbexpr.Print(seg.Expr))
			if seg.Order == xquery.Descending {
				b.WriteString(" DESC")
			} else {
				b.WriteString(" ASC")
			}
		}
	}
	if q.Limit != xquery.NoLimit {
		b.WriteString(" LIMIT ")
		b.WriteString(strconv.Itoa(q.Limit))
	}
	if q.Offset != 0 {
		b.WriteString(" OFFSET ")
		b.WriteString(strconv.Itoa(q.Offset))
	}
	if q.ForUpdate {
		b.WriteString(" FOR UPDATE")
	}
	if len(q.Where) > 0 {
		b.WriteString(" WHERE ")
		for i, n := range q.Where {
			if i > 0 {
				b.WriteString(" AND ")
			}
			b.WriteString(xbexpr.Print(n))
		}
	}
	return b.String()
}
