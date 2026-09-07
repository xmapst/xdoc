package xmap

import (
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/xmapst/xdoc/internal/xbin"
	"github.com/xmapst/xdoc/internal/xbson"
)

// encode 把一个 Go 值编成文档值。
//
// 分派次序是有讲究的：先看有没有登记过的转换器，再看几个内建类型，
// 最后才按种类走。无效值和空指针一律编成 Null。
//
// [xbson.Document] 与 [xbson.Array] 只收指针形式；值形式会报错并提示改用指针——
// 它们内部带状态，按值拷贝会拷出一个半截的东西。
func (m *Mapper) encode(rv reflect.Value, st *state) (*xbson.Value, error) {
	if !rv.IsValid() {
		return xbson.Null, nil
	}
	t := rv.Type()

	if c, ok := m.types[t]; ok && rv.CanInterface() {
		v, err := c.enc(rv)
		if err != nil {
			return nil, st.fail(t, err)
		}
		return orNull(v), nil
	}
	switch t {
	case typeValue:
		if rv.IsNil() {
			return xbson.Null, nil
		}
		return rv.Interface().(*xbson.Value), nil
	case typeDocument:
		if rv.IsNil() {
			return xbson.Null, nil
		}
		return (rv.Interface().(*xbson.Document)).Value(), nil
	case typeArray:
		if rv.IsNil() {
			return xbson.Null, nil
		}
		return (rv.Interface().(*xbson.Array)).Value(), nil
	case typeDocumentVal, typeArrayVal:
		return nil, st.failf(t, ErrUnsupportedType, "use the pointer form *%s", t)
	case typeTime:
		v, err := xbson.DateTime(rv.Interface().(time.Time))
		if err != nil {
			return nil, st.fail(t, err)
		}
		return v, nil
	case typeDecimal:
		return xbson.Decimal(rv.Interface().(xbin.Decimal)), nil
	case typeGuid:
		return xbson.GUID(rv.Interface().(xbin.Guid)), nil
	case typeObjectID:
		return xbson.OID(rv.Interface().(xbson.ObjectID)), nil
	}

	switch t.Kind() {
	case reflect.Bool:
		return xbson.Boolean(rv.Bool()), nil
	case reflect.Int8, reflect.Int16, reflect.Int32:
		return xbson.Int32(int32(rv.Int())), nil
	case reflect.Int, reflect.Int64:
		return xbson.Int64(rv.Int()), nil
	case reflect.Uint8, reflect.Uint16:
		return xbson.Int32(int32(rv.Uint())), nil
	case reflect.Uint, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		u := rv.Uint()

		if u > math.MaxInt64 {
			return nil, st.failf(t, ErrOverflow, "unsigned value %d does not fit in a signed 64-bit integer", u)
		}
		return xbson.Int64(int64(u)), nil
	case reflect.Float32, reflect.Float64:
		return xbson.Double(rv.Float()), nil
	case reflect.String:
		return m.encodeString(rv.String()), nil
	case reflect.Pointer, reflect.Interface:
		if rv.IsNil() {
			return xbson.Null, nil
		}
		leave, err := st.enter(rv)
		if err != nil {
			return nil, err
		}
		defer leave()
		v, err := m.encode(rv.Elem(), st)
		if err != nil {
			return nil, err
		}

		if t.Kind() == reflect.Interface {
			v = m.tagSubtype(v, rv.Elem())
		}
		return v, nil
	case reflect.Slice, reflect.Array:
		return m.encodeSlice(rv, st)
	case reflect.Map:
		return m.encodeMap(rv, st)
	case reflect.Struct:
		return m.encodeStruct(rv, st)
	default:
		return nil, st.failf(t, ErrUnsupportedType, "%s has no document representation", t.Kind())
	}
}

// encodeString 按 [Mapper] 的两个开关处理字符串：先去首尾空白，再看空串要不要变 Null。
func (m *Mapper) encodeString(s string) *xbson.Value {
	if m.TrimStrings {
		s = strings.TrimSpace(s)
	}
	if s == "" && m.EmptyStringToNull {
		return xbson.Null
	}
	return xbson.String(s)
}

// encodeSlice 编一个切片或数组。
//
// 空切片（nil）编成 Null，空数组编成空数组——数组没有 nil 这一说。
// 元素是字节且不是不透明类型时整体编成二进制，而不是一串整数。
func (m *Mapper) encodeSlice(rv reflect.Value, st *state) (*xbson.Value, error) {
	t := rv.Type()
	if t.Kind() == reflect.Slice && rv.IsNil() {
		return xbson.Null, nil
	}

	if t.Elem().Kind() == reflect.Uint8 && !m.isOpaque(t.Elem()) {
		return xbson.Binary(bytesOf(rv)), nil
	}
	leave, err := st.enter(rv)
	if err != nil {
		return nil, err
	}
	defer leave()

	arr := xbson.NewArray()
	for i := range rv.Len() {
		st.push("[" + itoa(i) + "]")
		v, err := m.encode(rv.Index(i), st)
		st.pop()
		if err != nil {
			return nil, err
		}
		arr.Append(v)
	}
	return arr.Value(), nil
}

// encodeMap 编一个映射，键转成字段名。
//
// 字段**按名字排序**输出：Go 的映射遍历次序是随机的，不排的话同一份数据
// 每次编出来的字节都不一样。键转不成字段名，或者转出来不合法，都报错。
func (m *Mapper) encodeMap(rv reflect.Value, st *state) (*xbson.Value, error) {
	t := rv.Type()
	if rv.IsNil() {
		return xbson.Null, nil
	}
	enc := keyEncoder(t.Key())
	if enc == nil {
		return nil, st.failf(t, ErrUnsupportedType,
			"cannot convert the map key type %s to a field name; the key may be a string kind, an integer kind, "+
				"or a type implementing encoding.TextMarshaler and encoding.TextUnmarshaler", t.Key())
	}
	leave, err := st.enter(rv)
	if err != nil {
		return nil, err
	}
	defer leave()

	type entry struct {
		name string
		key  reflect.Value
	}
	names := make([]entry, 0, rv.Len())
	for _, k := range rv.MapKeys() {
		name, err := enc(k)
		if err != nil {
			return nil, st.failf(t, ErrUnsupportedType, "cannot convert the map key to a field name: %v", err)
		}
		if err := validName(name); err != nil {
			return nil, st.fail(t, err)
		}
		names = append(names, entry{name, k})
	}

	slices.SortFunc(names, func(a, b entry) int { return strings.Compare(a.name, b.name) })

	doc := xbson.NewDocument()
	for _, e := range names {
		st.push(e.name)
		v, err := m.encode(rv.MapIndex(e.key), st)
		st.pop()
		if err != nil {
			return nil, err
		}
		if v.IsNull() && !m.EmitNull {
			continue
		}
		doc.Set(e.name, v)
	}
	return doc.Value(), nil
}

// encodeStruct 按取值方案编一个结构体。
//
// 字段全未导出时报错并提示登记转换器——否则会静悄悄编出一篇空文档。
//
// 主键有两条特殊规则：它是零值且允许自动生成时整个跳过（留给数据库填），
// 而它为 Null 时**照写不误**，不受 EmitNull 开关约束。
func (m *Mapper) encodeStruct(rv reflect.Value, st *state) (*xbson.Value, error) {
	t := rv.Type()
	p, err := m.planFor(t)
	if err != nil {
		return nil, st.fail(t, err)
	}

	if len(p.fields) == 0 && t.NumField() > 0 && !hasExportedField(t) {
		return nil, st.failf(t, ErrUnsupportedType,
			"every field of %s is unexported, so expanding it as a struct yields an empty document; "+
				"register a converter pair for it with Mapper.RegisterType", t)
	}
	leave, err := st.enter(rv)
	if err != nil {
		return nil, err
	}
	defer leave()

	doc := xbson.NewDocument()
	for i := range p.fields {
		f := &p.fields[i]
		fv := f.index.valueIn(rv)
		if !fv.IsValid() {
			if f.isID || m.EmitNull {
				doc.Set(f.name, xbson.Null)
			}
			continue
		}

		if f.isID && f.autoID && fv.IsZero() {
			continue
		}

		if !f.isID && ((f.omitEmpty && isEmpty(fv)) || (f.omitZero && fv.IsZero())) {
			continue
		}
		st.push(f.name)
		v, err := m.encodeField(fv, f, st)
		st.pop()
		if err != nil {
			return nil, err
		}
		if v.IsNull() && !m.EmitNull && !f.isID {
			continue
		}
		doc.Set(f.name, v)
	}
	return doc.Value(), nil
}

// encodeField 按字段的标记选择编法：引用编成壳，vector 编成向量，其余照常。
func (m *Mapper) encodeField(fv reflect.Value, f *field, st *state) (*xbson.Value, error) {
	switch {
	case f.isRef:
		return m.encodeRef(fv, f, st)
	case f.vector:
		if fv.IsNil() {
			return xbson.Null, nil
		}
		return xbson.Vector(slices.Clone(fv.Interface().([]float32))), nil
	default:
		return m.encode(fv, st)
	}
}

// bytesOf 取出字节切片的副本；定长数组逐个拷贝。
func bytesOf(rv reflect.Value) []byte {
	if rv.Kind() == reflect.Slice {
		return slices.Clone(rv.Bytes())
	}
	out := make([]byte, rv.Len())
	for i := range out {
		out[i] = byte(rv.Index(i).Uint())
	}
	return out
}

// isEmpty 判断一个值算不算「空」，供 omitempty 用。
//
// 长度为零、假、数值零、空指针都算空。结构体一律不算空——
// 那正是 omitzero 与它的分别。
func isEmpty(rv reflect.Value) bool {
	switch rv.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return rv.Len() == 0
	case reflect.Bool:
		return !rv.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return rv.Uint() == 0
	case reflect.Float32, reflect.Float64:
		return rv.Float() == 0
	case reflect.Pointer, reflect.Interface:
		return rv.IsNil()
	default:
		return false
	}
}

// orNull 把 nil 换成 Null，自定义转换器可能返回 nil。
func orNull(v *xbson.Value) *xbson.Value {
	if v == nil {
		return xbson.Null
	}
	return v
}

// itoa 把下标转成字符串，小数字走预置的表，省掉一次分配。
func itoa(i int) string {
	if i >= 0 && i < len(smallInts) {
		return smallInts[i]
	}
	return strconv.Itoa(i)
}

// smallInts 是 0 到 31 的字符串，覆盖常见的数组下标。
var smallInts = [...]string{
	"0", "1", "2", "3", "4", "5", "6", "7", "8", "9",
	"10", "11", "12", "13", "14", "15", "16", "17", "18", "19",
	"20", "21", "22", "23", "24", "25", "26", "27", "28", "29", "30", "31",
}

// hasExportedField 判断结构体有没有导出字段。
func hasExportedField(t reflect.Type) bool {
	for i := range t.NumField() {
		if t.Field(i).IsExported() {
			return true
		}
	}
	return false
}
