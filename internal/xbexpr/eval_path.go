package xbexpr

import (
	"github.com/xmapst/xdoc/internal/xbson"
)

// pathScalar 走完整条路径，取回一个值。空节点算 Null。
func (n *PathNode) pathScalar(e env) (*xbson.Value, error) {
	if n == nil {
		return xbson.Null, nil
	}
	v, _, err := n.walkSteps(len(n.Steps), e)
	return v, err
}

// pathSeq 走到倒数第二步，再按最后一步展开成一串值。
//
// 宿主不是数组时直接产出零项——路径取不到东西不是错误。
// 最后一步不是 [StepAll] 或 [StepFilter] 时才报错，那说明调用方走错了入口。
func (n *PathNode) pathSeq(e env, yield func(*xbson.Value, error) bool) {
	if n == nil || len(n.Steps) == 0 {
		return
	}
	host, _, err := n.walkSteps(len(n.Steps)-1, e)
	if err != nil {
		yield(nil, err)
		return
	}
	last := n.Steps[len(n.Steps)-1]

	arr, ok := host.AsArray()
	if !ok {
		return
	}

	switch last.Kind {
	case StepAll:
		for _, it := range arr.Items() {
			if !e.emit(yield, it) {
				return
			}
		}
	case StepFilter:
		for _, it := range arr.Items() {
			keep, err := e.withCurrent(it).evalScalar(last.Expr)
			if err != nil {
				yield(nil, err)
				return
			}
			if b, ok := keep.AsBoolean(); ok && b {
				if !e.emit(yield, it) {
					return
				}
			}
		}
	default:
		yield(nil, errf("path step of kind %d does not produce a sequence", last.Kind))
	}
}

// walkSteps 走前 steps 步，返回落到的值。
//
// 每走一条路径就下探一层，层数有上限，防住自我嵌套的表达式。
// 起点看 Root：从整篇文档还是从当前项起步。
//
// noRoot 是聚合查询里的约束：分组之后，没被聚合也没在 GROUP BY 里的字段
// 不许直接引用，此时报错而不是悄悄给 Null。
func (n *PathNode) walkSteps(steps int, e env) (*xbson.Value, env, error) {
	e, err := e.deeper()
	if err != nil {
		return nil, e, err
	}

	v := e.root
	if n.Root == RootCurrent {
		v = e.current
	}
	if v == nil {
		v = xbson.Null
	}

	if e.noRoot && n.Root == RootDocument && steps > 0 && n.Steps[0].Kind == StepField {
		return nil, e, errf("Field '%s' is invalid in the select list because it is not contained in either an aggregate function or the GROUP BY clause.",
			n.Steps[0].Name)
	}

	for i := range steps {
		s := n.Steps[i]
		switch s.Kind {
		case StepField:
			v = memberOf(v, s.Name)
		case StepIndex:
			var err error
			if v, err = elementAt(v, s.Index); err != nil {
				return nil, e, err
			}
		case StepParamIndex:
			if _, ok := v.AsArray(); !ok {
				v = xbson.Null
				break
			}
			idx, err := e.indexValue(s.Expr)
			if err != nil {
				return nil, e, err
			}
			if v, err = elementAt(v, idx); err != nil {
				return nil, e, err
			}
		default:
			return nil, e, errf("path step of kind %d must be the last step", s.Kind)
		}
	}
	return v, e, nil
}

// memberOf 取文档的一个字段。
//
// 不是文档就返回 Null——取不到字段不算错。字段名为空时原样返回，
// 那是 $ 本身这一步。
func memberOf(v *xbson.Value, name string) *xbson.Value {
	if name == "" {
		return v
	}
	d, ok := v.AsDocument()
	if !ok {
		return xbson.Null
	}
	return d.Get(name)
}

// elementAt 取数组的第 index 项，负数从末尾数起。
//
// 不是数组返回 Null；下标越过末尾也返回 Null；只有负得太多、
// 从末尾数回去仍然出界时才报错。
func elementAt(v *xbson.Value, index int) (*xbson.Value, error) {
	a, ok := v.AsArray()
	if !ok {
		return xbson.Null, nil
	}
	i := index
	if i < 0 {
		i += a.Len()
	}
	if i < 0 {
		return nil, errf("array index %d is out of range for a %d-element array", index, a.Len())
	}
	if i >= a.Len() {
		return xbson.Null, nil
	}
	return a.At(i), nil
}

// indexValue 把下标表达式算成一个整数，算出来不是数就报错。
func (e env) indexValue(n Node) (int, error) {
	v, err := e.evalScalar(n)
	if err != nil {
		return 0, err
	}
	if !isNumber(v) {
		return 0, errf("parameter expression must return a number when used as an array index, got %s", v.Type())
	}
	i, err := int32Of(v)
	if err != nil {
		return 0, err
	}
	return int(i), nil
}
