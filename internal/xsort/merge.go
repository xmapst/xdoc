package xsort

import (
	"fmt"
	"iter"

	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xpage"
)

// All 按排序次序交出全部记录。
//
// 多路归并：每段各开一个读取器、各取一条，用一个小顶堆挑出最小的那条，
// 交出去之后再从那一段补一条。堆里始终只有段数那么多条记录，
// 所以内存占用与总记录数无关。
//
// 遍历中途出错时停下，错误从 [Sorter.Err] 取。
func (s *Sorter) All() iter.Seq2[*xbson.Value, xpage.Address] {
	return func(yield func(*xbson.Value, xpage.Address) bool) {
		s.err = nil
		if s.closed {
			s.err = fmt.Errorf("xsort: sorter is closed")
			return
		}

		m := merger{cmp: s.Compare}
		for _, r := range s.runs {
			rr := s.disk.newRunReader(r)
			ok, err := rr.next()
			if err != nil {
				s.err = err
				return
			}
			if !ok {
				continue
			}
			m.idx = append(m.idx, len(m.rs))
			m.rs = append(m.rs, rr)
		}
		m.init()

		for len(m.idx) > 0 {
			rr := m.rs[m.idx[0]]
			if !yield(rr.key, rr.addr) {
				return
			}
			ok, err := rr.next()
			if err != nil {
				s.err = err
				return
			}
			if ok {
				m.down(0)
			} else {
				m.pop()
			}
		}
	}
}

// merger 是归并用的小顶堆，堆里存的是段的下标。
//
// 存下标而不是记录本身：记录要跟着段一起前进，存下标就不必在
// 每次前进时把记录搬进堆里。
type merger struct {
	rs  []*runReader
	idx []int
	cmp func(a, b *xbson.Value) int
}

// less 比较两段当前的记录。
//
// 键相等时按段号定次序：这让归并是**稳定的**——同键的记录仍按
// 它们进入排序时的先后出来。
func (m *merger) less(a, b int) bool {
	if d := m.cmp(m.rs[a].key, m.rs[b].key); d != 0 {
		return d < 0
	}
	return a < b
}

// init 把堆建起来。
func (m *merger) init() {
	for i := len(m.idx)/2 - 1; i >= 0; i-- {
		m.down(i)
	}
}

// down 把第 i 个元素往下沉到合适的位置。
func (m *merger) down(i int) {
	for {
		l := 2*i + 1
		if l >= len(m.idx) {
			return
		}
		j := l
		if r := l + 1; r < len(m.idx) && m.less(m.idx[r], m.idx[l]) {
			j = r
		}
		if !m.less(m.idx[j], m.idx[i]) {
			return
		}
		m.idx[i], m.idx[j] = m.idx[j], m.idx[i]
		i = j
	}
}

// pop 去掉堆顶那一段——它已经读完了。
func (m *merger) pop() {
	last := len(m.idx) - 1
	m.idx[0] = m.idx[last]
	m.idx = m.idx[:last]
	m.down(0)
}
