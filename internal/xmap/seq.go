package xmap

import (
	"iter"

	"github.com/xmapst/xdoc/internal/xbson"
)

// UnmarshalAs 用默认映射器把一个文档值解成 T。
func UnmarshalAs[T any](v *xbson.Value) (T, error) { return Default.UnmarshalAs[T](v) }

// UnmarshalAs 把一个文档值解成 T。出错时返回 T 的零值。
func (m *Mapper) UnmarshalAs[T any](v *xbson.Value) (T, error) {
	var out T
	if err := m.Unmarshal(v, &out); err != nil {
		var zero T
		return zero, err
	}
	return out, nil
}

// UnmarshalSeq 把一串文档值逐个解成 T。
//
// 上游给出错误时原样传下去，不再尝试解码。产出错误之后立刻停止——
// 后面的值多半也解不出来。
func (m *Mapper) UnmarshalSeq[T any](src iter.Seq2[*xbson.Value, error]) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		for v, err := range src {
			var out T
			if err == nil {
				out, err = m.UnmarshalAs[T](v)
			}
			if !yield(out, err) {
				return
			}
			if err != nil {
				return
			}
		}
	}
}
