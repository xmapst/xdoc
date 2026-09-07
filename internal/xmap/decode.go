package xmap

import (
	"fmt"
	"math"
	"reflect"
	"slices"

	"github.com/xmapst/xdoc/internal/xbson"
)

// decode 把一个文档值写进目标。
//
// 分派次序同 [Mapper.encode]。Null 一律把目标置零——只有目标本身就是
// [xbson.Value] 时例外，那时 Null 是一个正经的值。
//
// 自定义转换器优先于内建类型；转换器收到 Null 时也走置零，不会被调到。
func (m *Mapper) decode(v *xbson.Value, rv reflect.Value, st *state) error {
	if v == nil {
		v = xbson.Null
	}
	if !rv.IsValid() {
		return st.failf(nil, ErrTarget, "invalid target")
	}
	if !rv.CanSet() {
		return st.failf(rv.Type(), ErrTarget, "field is not writable (possibly an unexported embedded field)")
	}
	t := rv.Type()

	if m.BeforeDecode != nil {
		if nv := m.BeforeDecode(t, v); nv != nil {
			v = nv
		}
	}

	if c, ok := m.types[t]; ok {
		if v.IsNull() {
			rv.SetZero()
			return nil
		}
		if err := c.dec(v, rv); err != nil {
			return st.fail(t, err)
		}
		return nil
	}

	if v.IsNull() && t != typeValue {
		rv.SetZero()
		return nil
	}

	switch t {
	case typeValue:
		rv.Set(reflect.ValueOf(v))
		return nil
	case typeDocument:
		doc, ok := v.AsDocument()
		if !ok {
			return st.mismatch(t, v)
		}
		rv.Set(reflect.ValueOf(doc))
		return nil
	case typeArray:
		arr, ok := v.AsArray()
		if !ok {
			return st.mismatch(t, v)
		}
		rv.Set(reflect.ValueOf(arr))
		return nil
	case typeDocumentVal, typeArrayVal:
		return st.failf(t, ErrUnsupportedType, "use the pointer form *%s", t)
	case typeTime:
		tv, ok := v.AsTime()
		if !ok {
			return st.mismatch(t, v)
		}
		rv.Set(reflect.ValueOf(tv))
		return nil
	case typeDecimal:
		d, ok := v.AsDecimal()
		if !ok {
			return st.mismatch(t, v)
		}
		rv.Set(reflect.ValueOf(d))
		return nil
	case typeGuid:
		g, ok := v.AsGUID()
		if !ok {
			return st.mismatch(t, v)
		}
		rv.Set(reflect.ValueOf(g))
		return nil
	case typeObjectID:
		id, ok := v.AsObjectID()
		if !ok {
			return st.mismatch(t, v)
		}
		rv.Set(reflect.ValueOf(id))
		return nil
	}

	switch t.Kind() {
	case reflect.Bool:
		b, ok := v.AsBoolean()
		if !ok {
			return st.mismatch(t, v)
		}
		rv.SetBool(b)
		return nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := asInt64(v)
		if err != nil {
			return st.fail(t, err)
		}
		if rv.OverflowInt(n) {
			return st.failf(t, ErrOverflow, "%d does not fit in %s", n, t)
		}
		rv.SetInt(n)
		return nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		n, err := asInt64(v)
		if err != nil {
			return st.fail(t, err)
		}
		if n < 0 {
			return st.failf(t, ErrOverflow, "%d is negative and does not fit in %s", n, t)
		}
		if rv.OverflowUint(uint64(n)) {
			return st.failf(t, ErrOverflow, "%d does not fit in %s", n, t)
		}
		rv.SetUint(uint64(n))
		return nil
	case reflect.Float32, reflect.Float64:
		f, err := asFloat64(v)
		if err != nil {
			return st.fail(t, err)
		}
		if rv.OverflowFloat(f) {
			return st.failf(t, ErrOverflow, "%v does not fit in %s", f, t)
		}
		rv.SetFloat(f)
		return nil
	case reflect.String:
		s, ok := v.AsString()
		if !ok {
			return st.mismatch(t, v)
		}
		rv.SetString(s)
		return nil
	case reflect.Pointer:
		if rv.IsNil() {
			rv.Set(reflect.New(t.Elem()))
		}
		leave, err := st.enter(rv)
		if err != nil {
			return err
		}
		defer leave()
		return m.decode(v, rv.Elem(), st)
	case reflect.Interface:
		return m.decodeInterface(v, rv, st)
	case reflect.Slice, reflect.Array:
		return m.decodeSlice(v, rv, st)
	case reflect.Map:
		return m.decodeMap(v, rv, st)
	case reflect.Struct:
		return m.decodeStruct(v, rv, st)
	default:
		return st.failf(t, ErrUnsupportedType, "%s has no document representation", t.Kind())
	}
}

// decodeInterface 往接口字段里写。
//
// 先看文档带没带类型名，带了就按它还原具体类型。没带的话：非空接口
// 只能报错——不知道该造什么；空接口则按值的类型选一个自然的 Go 类型。
//
// 接口里已经装着一个非空指针时，直接往它指向的东西上解，保住调用方
// 预先放好的实例。
func (m *Mapper) decodeInterface(v *xbson.Value, rv reflect.Value, st *state) error {
	t := rv.Type()

	if done, err := m.decodeSubtype(v, rv, st); done {
		return err
	}
	if t.NumMethod() != 0 {
		return st.failf(t, ErrTypeMismatch,
			"cannot restore a non-empty interface: the document has no %s discriminator; "+
				"register the implementing type with Mapper.RegisterSubtype (documents written afterwards carry it), or use a concrete target type", TypeKey)
	}

	if e := rv.Elem(); e.IsValid() && e.Kind() == reflect.Pointer && !e.IsNil() {
		return m.decode(v, e.Elem(), st)
	}
	out, err := m.natural(v, st)
	if err != nil {
		return err
	}
	if out == nil {
		rv.SetZero()
		return nil
	}
	rv.Set(reflect.ValueOf(out))
	return nil
}

// natural 把一个文档值转成最自然的 Go 类型，供空接口用。
//
// 文档变成 map[string]any，数组变成 []any，二进制和向量各自拷一份副本。
// 没有对应类型的（比如 MinValue、MaxValue）原样返回那个文档值。
func (m *Mapper) natural(v *xbson.Value, st *state) (any, error) {
	switch v.Type() {
	case xbson.TypeNull:
		return nil, nil
	case xbson.TypeBoolean:
		b, _ := v.AsBoolean()
		return b, nil
	case xbson.TypeInt32:
		n, _ := v.AsInt32()
		return n, nil
	case xbson.TypeInt64:
		n, _ := v.AsInt64()
		return n, nil
	case xbson.TypeDouble:
		f, _ := v.AsDouble()
		return f, nil
	case xbson.TypeDecimal:
		d, _ := v.AsDecimal()
		return d, nil
	case xbson.TypeString:
		s, _ := v.AsString()
		return s, nil
	case xbson.TypeBinary:
		b, _ := v.AsBinary()
		return slices.Clone(b), nil
	case xbson.TypeObjectID:
		id, _ := v.AsObjectID()
		return id, nil
	case xbson.TypeGUID:
		g, _ := v.AsGUID()
		return g, nil
	case xbson.TypeDateTime:
		t, ok := v.AsTime()
		if !ok {
			return nil, st.failf(nil, ErrTypeMismatch, "date value is out of representable range")
		}
		return t, nil
	case xbson.TypeVector:
		f, _ := v.AsVector()
		return slices.Clone(f), nil
	case xbson.TypeDocument:
		doc, _ := v.AsDocument()
		leave, err := st.enter(reflect.ValueOf(doc))
		if err != nil {
			return nil, err
		}
		defer leave()
		out := make(map[string]any, doc.Len())
		for k, e := range doc.Elements() {
			st.push(k)
			ev, err := m.natural(e, st)
			st.pop()
			if err != nil {
				return nil, err
			}
			out[k] = ev
		}
		return out, nil
	case xbson.TypeArray:
		arr, _ := v.AsArray()
		leave, err := st.enter(reflect.ValueOf(arr))
		if err != nil {
			return nil, err
		}
		defer leave()
		out := make([]any, 0, arr.Len())
		for i, e := range arr.Items() {
			st.push("[" + itoa(i) + "]")
			ev, err := m.natural(e, st)
			st.pop()
			if err != nil {
				return nil, err
			}
			out = append(out, ev)
		}
		return out, nil
	default:
		return v, nil
	}
}

// decodeSlice 往切片或数组里写。
//
// 二进制可以写进字节切片或字节数组，向量可以写进 []float32；
// 其余情况要求文档里是数组。定长数组装不下就报错，装不满的部分置零。
func (m *Mapper) decodeSlice(v *xbson.Value, rv reflect.Value, st *state) error {
	t := rv.Type()

	if t.Elem().Kind() == reflect.Uint8 && !m.isOpaque(t.Elem()) {
		if b, ok := v.AsBinary(); ok {
			return st.setBytes(rv, b)
		}
	}

	if t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Float32 {
		if f, ok := v.AsVector(); ok {
			rv.Set(reflect.ValueOf(slices.Clone(f)))
			return nil
		}
	}
	arr, ok := v.AsArray()
	if !ok {
		return st.mismatch(t, v)
	}
	leave, err := st.enter(reflect.ValueOf(arr))
	if err != nil {
		return err
	}
	defer leave()

	items := arr.Items()
	if t.Kind() == reflect.Array {
		if len(items) > t.Len() {
			return st.failf(t, ErrOverflow, "array has %d elements, the target holds only %d", len(items), t.Len())
		}
		for i, e := range items {
			st.push("[" + itoa(i) + "]")
			err := m.decode(e, rv.Index(i), st)
			st.pop()
			if err != nil {
				return err
			}
		}

		for i := len(items); i < t.Len(); i++ {
			rv.Index(i).SetZero()
		}
		return nil
	}
	out := reflect.MakeSlice(t, len(items), len(items))
	for i, e := range items {
		st.push("[" + itoa(i) + "]")
		err := m.decode(e, out.Index(i), st)
		st.pop()
		if err != nil {
			return err
		}
	}
	rv.Set(out)
	return nil
}

// setBytes 把一段字节写进切片或定长数组。切片存的是副本；数组装不下就报错。
func (s *state) setBytes(rv reflect.Value, b []byte) error {
	t := rv.Type()
	if t.Kind() == reflect.Slice {
		rv.Set(reflect.ValueOf(slices.Clone(b)))
		return nil
	}
	if len(b) > t.Len() {
		return s.failf(t, ErrOverflow, "binary has %d bytes, the target holds only %d", len(b), t.Len())
	}
	rv.SetZero()
	for i, c := range b {
		rv.Index(i).SetUint(uint64(c))
	}
	return nil
}

// decodeMap 往映射里写，字段名转成键。
//
// **整个重建一个映射再赋值**，原来的内容不保留。
func (m *Mapper) decodeMap(v *xbson.Value, rv reflect.Value, st *state) error {
	t := rv.Type()
	dec := keyDecoder(t.Key())
	if dec == nil {
		return st.failf(t, ErrUnsupportedType,
			"cannot convert a field name to the map key type %s; the key may be a string kind, an integer kind, "+
				"or a type implementing encoding.TextMarshaler and encoding.TextUnmarshaler", t.Key())
	}
	doc, ok := v.AsDocument()
	if !ok {
		return st.mismatch(t, v)
	}
	leave, err := st.enter(reflect.ValueOf(doc))
	if err != nil {
		return err
	}
	defer leave()

	out := reflect.MakeMapWithSize(t, doc.Len())
	elem := reflect.New(t.Elem()).Elem()
	for k, e := range doc.Elements() {
		elem.SetZero()
		st.push(k)
		err := m.decode(e, elem, st)
		st.pop()
		if err != nil {
			return err
		}
		key, err := dec(k)
		if err != nil {
			return st.failf(t, ErrTypeMismatch, "%v", err)
		}
		out.SetMapIndex(key, elem)
	}
	rv.Set(out)
	return nil
}

// decodeStruct 往结构体里写。
//
// 按文档里的字段逐个找对应的 Go 字段，**匹配不分大小写**。
// 文档里多出来的字段直接忽略；文档里没有的字段保持原样，不会被清零。
func (m *Mapper) decodeStruct(v *xbson.Value, rv reflect.Value, st *state) error {
	t := rv.Type()
	doc, ok := v.AsDocument()
	if !ok {
		return st.mismatch(t, v)
	}
	p, err := m.planFor(t)
	if err != nil {
		return st.fail(t, err)
	}

	if len(p.fields) == 0 && t.NumField() > 0 && !hasExportedField(t) {
		return st.failf(t, ErrUnsupportedType,
			"every field of %s is unexported, so nothing can be written; "+
				"register a converter pair for it with Mapper.RegisterType", t)
	}
	leave, err := st.enter(rv)
	if err != nil {
		return err
	}
	defer leave()

	for key, e := range doc.Elements() {
		i, ok := p.byFold[foldName(key)]
		if !ok {
			continue
		}
		f := &p.fields[i]
		fv, err := f.index.allocIn(rv)
		if err != nil {
			return st.fail(t, err)
		}
		st.push(f.name)
		if f.isRef {
			err = m.decodeRef(e, fv, st)
		} else {
			err = m.decode(e, fv, st)
		}
		st.pop()
		if err != nil {
			return err
		}
	}
	return nil
}

// asInt64 把一个文档值取成整数。
//
// 双精度必须正好是整数才认，带小数部分或者是 NaN、无穷都报错。
// 时间取的是它的计时刻度。
func asInt64(v *xbson.Value) (int64, error) {
	switch v.Type() {
	case xbson.TypeInt32:
		n, _ := v.AsInt32()
		return int64(n), nil
	case xbson.TypeInt64:
		return mustInt64(v)
	case xbson.TypeDouble:
		f, _ := v.AsDouble()
		if f != math.Trunc(f) || math.IsInf(f, 0) || math.IsNaN(f) {
			return 0, fmt.Errorf("%w: %v is not an integer", ErrTypeMismatch, f)
		}
		if f < math.MinInt64 || f > math.MaxInt64 {
			return 0, fmt.Errorf("%w: %v is out of int64 range", ErrOverflow, f)
		}
		return int64(f), nil
	case xbson.TypeDateTime:
		n, _ := v.Ticks()
		return n, nil
	default:
		return 0, fmt.Errorf("%w: %s is not an integer", ErrTypeMismatch, v.Type())
	}
}

// mustInt64 取 64 位整数值，取不到就报类型不符。
func mustInt64(v *xbson.Value) (int64, error) {
	n, ok := v.AsInt64()
	if !ok {
		return 0, fmt.Errorf("%w: %s is not an integer", ErrTypeMismatch, v.Type())
	}
	return n, nil
}

// asFloat64 把一个文档值取成双精度。整数会被转过去，可能丢精度；十进制不认。
func asFloat64(v *xbson.Value) (float64, error) {
	switch v.Type() {
	case xbson.TypeDouble:
		f, _ := v.AsDouble()
		return f, nil
	case xbson.TypeInt32:
		n, _ := v.AsInt32()
		return float64(n), nil
	case xbson.TypeInt64:
		n, _ := v.AsInt64()
		return float64(n), nil
	default:
		return 0, fmt.Errorf("%w: %s is not numeric", ErrTypeMismatch, v.Type())
	}
}

// mismatch 报「文档里装的是另一种类型」。
func (s *state) mismatch(t reflect.Type, v *xbson.Value) error {
	return s.failf(t, ErrTypeMismatch, "the document holds %s", v.Type())
}
