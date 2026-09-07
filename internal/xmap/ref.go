package xmap

import (
	"reflect"

	"github.com/xmapst/xdoc/internal/xbson"
)

const (
	// 引用在文档里存成一个壳：$id 是被引对象的主键，$ref 是它所在的集合。
	//
	// 查询时若把引用展开了，壳里会填上完整内容；被引对象已经不在了，
	// 则填上 $missing。
	refIDKey      = "$id"
	refCollKey    = "$ref"
	refMissingKey = "$missing"
)

// encodeRef 把一个引用字段编成壳，只存主键和集合名，不存对象本身。
//
// 切片和数组编成一串壳，其中的空指针项**直接跳过**，不占位置；
// 单个字段是空指针时编成 Null。
func (m *Mapper) encodeRef(rv reflect.Value, f *field, st *state) (*xbson.Value, error) {
	t := rv.Type()
	if t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		if t.Kind() == reflect.Slice && rv.IsNil() {
			return xbson.Null, nil
		}
		arr := xbson.NewArray()
		for i := range rv.Len() {
			st.push("[" + itoa(i) + "]")
			shell, err := m.refShell(rv.Index(i), f, st)
			st.pop()
			if err != nil {
				return nil, err
			}
			if shell == nil {
				continue
			}
			arr.Append(shell)
		}
		return arr.Value(), nil
	}
	shell, err := m.refShell(rv, f, st)
	if err != nil {
		return nil, err
	}
	if shell == nil {
		return xbson.Null, nil
	}
	return shell, nil
}

// refShell 编出一个引用壳。空指针返回 nil 让调用方决定怎么办；不是结构体就报错。
func (m *Mapper) refShell(rv reflect.Value, f *field, st *state) (*xbson.Value, error) {
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return nil, nil
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil, st.failf(rv.Type(), ErrUnsupportedType, "a ref field must be a struct (or a pointer or slice of one)")
	}
	id, err := m.idValue(rv, st)
	if err != nil {
		return nil, err
	}
	return xbson.DocumentOf(refIDKey, id, refCollKey, xbson.String(f.ref)).Value(), nil
}

// idValue 取一个结构体的主键值。没有主键字段就报错；字段取不到时算 Null。
func (m *Mapper) idValue(rv reflect.Value, st *state) (*xbson.Value, error) {
	p, err := m.planFor(rv.Type())
	if err != nil {
		return nil, st.fail(rv.Type(), err)
	}
	if p.idIdx < 0 {
		return nil, st.fail(rv.Type(), ErrNoPrimaryKey)
	}
	fv := p.fields[p.idIdx].index.valueIn(rv)
	if !fv.IsValid() {
		return xbson.Null, nil
	}
	return m.encode(fv, st)
}

// decodeRef 把引用壳解回目标。
//
// 标着 $missing 的项跳过，切片因此会比文档里短。剥壳的活儿见 [refBody]。
func (m *Mapper) decodeRef(v *xbson.Value, rv reflect.Value, st *state) error {
	t := rv.Type()
	if t.Kind() == reflect.Slice {
		arr, ok := v.AsArray()
		if !ok {
			if v.IsNull() {
				rv.SetZero()
				return nil
			}
			return st.mismatch(t, v)
		}
		out := reflect.MakeSlice(t, 0, arr.Len())
		elem := reflect.New(t.Elem()).Elem()
		for i, e := range arr.Items() {
			if refMissing(e) {
				continue
			}
			elem.SetZero()
			st.push("[" + itoa(i) + "]")
			err := m.decode(refBody(e), elem, st)
			st.pop()
			if err != nil {
				return err
			}
			out = reflect.Append(out, elem)
		}
		rv.Set(out)
		return nil
	}
	if v.IsNull() || refMissing(v) {
		rv.SetZero()
		return nil
	}
	return m.decode(refBody(v), rv, st)
}

// refMissing 判断这个壳是不是标着「被引对象已经没了」。
func refMissing(v *xbson.Value) bool {
	doc, ok := v.AsDocument()
	if !ok {
		return false
	}
	b, _ := doc.Get(refMissingKey).AsBoolean()
	return b
}

// refBody 把引用壳换成一篇能直接解码的文档：$id 改名成 _id，$ref 去掉。
//
// 引用没被展开时，剥出来的就只有一个主键字段，解码的结果是个只填了主键的对象。
// 不带 $id 的值原样返回。
func refBody(v *xbson.Value) *xbson.Value {
	doc, ok := v.AsDocument()
	if !ok {
		return v
	}
	if !doc.Has(refIDKey) {
		return v
	}
	body := doc.Clone()
	id := body.Get(refIDKey)
	body.Delete(refIDKey)
	body.Delete(refCollKey)
	body.Set(idFieldName, id)
	return body.Value()
}
