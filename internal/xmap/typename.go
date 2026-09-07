package xmap

import (
	"reflect"

	"github.com/xmapst/xdoc/internal/xbson"
)

// TypeKey 是文档里记实际类型名的字段，接口字段靠它还原成具体类型。
const TypeKey = "_type"

// RegisterSubtype 给一个具体类型登记短名，让它能存进接口字段再取回来。
//
// 登记会清空取值方案缓存。只有登记过的类型才认得，见 [Mapper.decodeSubtype]。
func (m *Mapper) RegisterSubtype[T any](name string) {
	t := reflect.TypeFor[T]()

	t = derefType(t)
	if m.subtypeNames == nil {
		m.subtypeNames = make(map[reflect.Type]string)
		m.subtypes = make(map[string]reflect.Type)
	}
	m.subtypeNames[t] = name
	m.subtypes[name] = t
	m.cache.Clear()
}

// derefType 剥掉所有指针层。
func derefType(t reflect.Type) reflect.Type {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

// typeName 取一个类型该写进文档的名字。
//
// 登记过就用登记的短名；否则用「包路径.类型名」——但那种名字解码时认不回来，
// 因为反查表只装登记过的类型。
func (m *Mapper) typeName(t reflect.Type) string {
	t = derefType(t)
	if n, ok := m.subtypeNames[t]; ok {
		return n
	}
	if p := t.PkgPath(); p != "" {
		return p + "." + t.Name()
	}
	return t.String()
}

// typeByName 按名字反查类型。
func (m *Mapper) typeByName(name string) (reflect.Type, bool) {
	t, ok := m.subtypes[name]
	return t, ok
}

// tagSubtype 给接口字段编出来的文档补上类型名。
//
// **类型名放在最前面**：另建一篇文档把它写在第一位，再抄进原有字段。
// 文档里已经有这个键，或者里层不是结构体，都原样返回。
func (m *Mapper) tagSubtype(v *xbson.Value, elem reflect.Value) *xbson.Value {
	if v == nil {
		return v
	}
	d, ok := v.AsDocument()
	if !ok {
		return v
	}
	t := derefType(elem.Type())
	if t == nil || t.Kind() != reflect.Struct {
		return v
	}

	if d.Has(TypeKey) {
		return v
	}

	out := xbson.NewDocument()
	out.Set(TypeKey, xbson.String(m.typeName(t)))
	for k, e := range d.Elements() {
		out.Set(k, e)
	}
	return out.Value()
}

// decodeSubtype 按文档里的类型名还原出具体类型，第一个返回值说明有没有处理。
//
// 文档没带类型名就交回给调用方另想办法。带了却没登记，或者登记的类型
// 不满足目标接口，都报错——那两种情况下继续往下走只会得到更含糊的错误。
//
// 具体类型只有指针才实现接口时，存进去的是指针；否则存值本身。
func (m *Mapper) decodeSubtype(v *xbson.Value, rv reflect.Value, st *state) (bool, error) {
	d, ok := v.AsDocument()
	if !ok {
		return false, nil
	}
	nameV := d.Get(TypeKey)
	if nameV == nil {
		return false, nil
	}
	name, ok := nameV.AsString()
	if !ok {
		return false, nil
	}
	iface := rv.Type()
	rt, ok := m.typeByName(name)
	if !ok {
		return true, st.failf(iface, ErrTypeMismatch,
			"the document's %s is %q, which is not registered; register it with Mapper.RegisterSubtype, or use a concrete target type",
			TypeKey, name)
	}

	if !assignableToInterface(rt, iface) {
		return true, st.failf(iface, ErrTypeMismatch, "%s does not implement %s", rt, iface)
	}
	nv := reflect.New(rt)
	if err := m.decode(v, nv.Elem(), st); err != nil {
		return true, err
	}
	if reflect.PointerTo(rt).Implements(iface) && !rt.Implements(iface) {
		rv.Set(nv)
		return true, nil
	}
	rv.Set(nv.Elem())
	return true, nil
}

// assignableToInterface 判断一个类型（或它的指针）能不能塞进目标接口。空接口一律可以。
func assignableToInterface(rt, iface reflect.Type) bool {
	if iface.NumMethod() == 0 {
		return true
	}
	return rt.Implements(iface) || reflect.PointerTo(rt).Implements(iface)
}
