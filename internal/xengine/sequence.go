package xengine

import (
	"crypto/rand"
	"errors"
	"fmt"
	"maps"

	"github.com/xmapst/xdoc/internal/xbin"
	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xstore"
	"github.com/xmapst/xdoc/internal/xtx"
)

// nextSequence 取下一个自增主键，不超过 limit。
//
// 内存里没有记录时先从主键索引里查出当前最大值，此后就在内存里加。
// **序列不落盘**：进程重启后重新查一次。到了上限就报错，序列原样不动。
func (e *Engine) nextSequence(s *xtx.Snapshot, name string, limit int64) (int64, error) {
	e.seqMu.Lock()
	defer e.seqMu.Unlock()
	v, ok := e.seq[name]
	if !ok {
		last, err := e.lastID(s)
		if err != nil {
			return 0, err
		}
		v = last
	}
	if v >= limit {
		return 0, fmt.Errorf("xengine: auto id sequence of %q exhausted: %d reached the limit %d", name, v, limit)
	}
	e.seq[name] = v + 1
	return v + 1, nil
}

// bumpSequence 把序列抬到至少 n。手写数字主键时调用，免得之后自增撞上它。
//
// 内存里没有记录时先从主键索引里查出当前最大值再比，不然重开库后手写一个小主键
// 会把序列压到它身上。主键里有非数字、推不出最大值时只能信手写的这个。
func (e *Engine) bumpSequence(s *xtx.Snapshot, name string, n int64) error {
	e.seqMu.Lock()
	defer e.seqMu.Unlock()
	v, ok := e.seq[name]
	if !ok {
		last, err := e.lastID(s)
		switch {
		case err == nil:
			v = last
		case errors.Is(err, ErrSequenceNotNumeric):
			v = n
		default:
			return err
		}
	}
	e.seq[name] = max(v, n)
	return nil
}

// dropSequence 忘掉某个集合的序列，集合被删或改名时调用。
func (e *Engine) dropSequence(name string) {
	e.seqMu.Lock()
	defer e.seqMu.Unlock()
	delete(e.seq, name)
}

// lastID 从主键索引里查出最大的主键。
//
// 走尾哨兵的前驱，一步到位，不用遍历。集合是空的（前驱就是头哨兵）时算 0。
// 最大的主键不是数就报错——那种集合上自增没有意义。
func (e *Engine) lastID(s *xtx.Snapshot) (int64, error) {
	pk, err := (work{s}).primaryIndex()
	if err != nil {
		return 0, err
	}
	st := xstore.New(s)

	tail, err := st.NodeAt(pk.Tail)
	if err != nil || tail == nil {
		return 0, err
	}
	prev, err := st.NodeAt(tail.Prev0())
	if err != nil || prev == nil {
		return 0, err
	}
	k, err := prev.Key()
	if err != nil {
		return 0, err
	}
	if k.Type() == xbson.TypeMinValue {
		return 0, nil
	}
	n, ok := asInt64(k)
	if !ok {
		return 0, fmt.Errorf("%w: largest id is %s (%s)", ErrSequenceNotNumeric, k, k.Type())
	}
	return n, nil
}

// asInt64 把一个值取成整数；双精度必须正好是整数才认。
func asInt64(v *xbson.Value) (int64, bool) {
	switch v.Type() {
	case xbson.TypeInt32:
		n, ok := v.AsInt32()
		return int64(n), ok
	case xbson.TypeInt64:
		return v.AsInt64()
	case xbson.TypeDouble:
		f, ok := v.AsDouble()
		if !ok || f != float64(int64(f)) {
			return 0, false
		}
		return int64(f), true
	default:
		return 0, false
	}
}

// newGUID 生成一个随机 GUID，版本位和变体位按 RFC 4122 置好。
func newGUID() (xbin.Guid, error) {
	var g xbin.Guid
	if _, err := rand.Read(g[:]); err != nil {
		return g, fmt.Errorf("xengine: generate id: %w", err)
	}

	g[6] = (g[6] & 0x0F) | 0x40
	g[8] = (g[8] & 0x3F) | 0x80
	return g, nil
}

// Sequences 返回各集合当前的序列值的副本。
func (e *Engine) Sequences() map[string]int64 {
	e.seqMu.Lock()
	defer e.seqMu.Unlock()
	out := make(map[string]int64, len(e.seq))
	maps.Copy(out, e.seq)
	return out
}
