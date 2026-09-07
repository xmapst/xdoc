package xmap

import (
	"cmp"
	"fmt"
	"iter"
	"reflect"
	"slices"
	"strings"
)

// fieldIndex 是一条通往字段的下标路径，跨内嵌结构体时不止一层。
type fieldIndex []int

// field 是取值方案里的一个字段。
type field struct {
	typ reflect.Type

	// name 是文档字段名，fold 是它的大写折叠形式，ref 是引用指向的集合。
	name string
	fold string
	ref  string

	index     fieldIndex
	omitEmpty bool
	omitZero  bool
	isID      bool
	autoID    bool
	isRef     bool
	vector    bool

	// tagged 表示名字来自标签而非推导，字段名撞车时它赢。
	tagged bool
}

// plan 是一个结构体类型的取值方案，只算一次然后缓存。
//
// fields 按下标路径排序，编码时因此有稳定的字段次序。
type plan struct {
	fields []field

	// byFold 按大写折叠后的名字反查字段，解码时靠它做不分大小写的匹配。
	byFold map[string]int

	// idIdx 是主键字段的下标，没有主键时为 -1。
	idIdx int
}

// planEntry 是缓存里的一项。失败的结果也缓存，免得每次都重算一遍再失败。
type planEntry struct {
	p   *plan
	err error
}

// planFor 取一个类型的取值方案，没有就现算并缓存。
//
// 并发算同一个类型时可能重复算，但只有一份会进缓存，取回来的是同一份。
func (m *Mapper) planFor(t reflect.Type) (*plan, error) {
	if e, ok := m.cache.Load(t); ok {
		pe := e.(*planEntry)
		return pe.p, pe.err
	}
	p, err := m.buildPlan(t)

	e, _ := m.cache.LoadOrStore(t, &planEntry{p: p, err: err})
	pe := e.(*planEntry)
	return pe.p, pe.err
}

// candidate 是待定的字段，带上它在第几层内嵌里。
type candidate struct {
	field
	depth int
}

// buildPlan 逐层展开结构体，收集所有候选字段。
//
// 按层广度优先地走：匿名内嵌且没写名字的、或者写了 inline 的结构体字段
// 不算一个字段，而是把它的字段提到外层来。同一个类型只展开一次，
// 免得相互内嵌绕不出来。
//
// 名字这么定：写了 id 选项就是 _id；写了标签名就用它；字段名叫 id
// （不分大小写）自动当主键；否则交给命名函数。
func (m *Mapper) buildPlan(t reflect.Type) (*plan, error) {
	type queued struct {
		typ   reflect.Type
		index fieldIndex
	}
	var (
		cands   []candidate
		cur     []queued
		next    = []queued{{typ: t}}
		visited = make(map[reflect.Type]bool)
		depth   int
	)
	for len(next) > 0 {
		cur, next = next, cur[:0]
		depth++
		for _, q := range cur {
			if visited[q.typ] {
				continue
			}
			visited[q.typ] = true
			for i := range q.typ.NumField() {
				sf := q.typ.Field(i)
				spec, err := parseTag(sf.Tag.Lookup(TagKey))
				if err != nil {
					return nil, fmt.Errorf("%s.%s: %w", q.typ, sf.Name, err)
				}
				if spec.ignore {
					continue
				}
				elem := sf.Type
				if elem.Kind() == reflect.Pointer {
					elem = elem.Elem()
				}

				flatten := (sf.Anonymous && !spec.hasName) || spec.inline
				if flatten && elem.Kind() == reflect.Struct && !m.isOpaque(elem) {
					next = append(next, queued{typ: elem, index: q.index.child(i)})
					continue
				}
				if spec.inline {
					return nil, fmt.Errorf("%s.%s: %w: inline is only valid on struct fields",
						q.typ, sf.Name, ErrInvalidTag)
				}
				if !sf.IsExported() {
					continue
				}
				c := candidate{depth: depth,
					index:     q.index.child(i),
					typ:       sf.Type,
					omitEmpty: spec.omitEmpty,
					omitZero:  spec.omitZero,
					isID:      spec.isID,
					autoID:    spec.autoID,
					isRef:     spec.isRef,
					ref:       spec.ref,
					vector:    spec.vector,
					tagged:    spec.hasName}
				switch {
				case spec.isID:
					c.name = idFieldName
				case spec.hasName:
					c.name = spec.name
				case strings.EqualFold(sf.Name, "id"):
					c.name, c.isID = idFieldName, true
				default:
					c.name = m.resolveName(sf.Name)
					if err := validName(c.name); err != nil {
						return nil, fmt.Errorf("%s.%s: %w", q.typ, sf.Name, err)
					}
				}
				if c.isRef && c.ref == "" {
					c.ref = m.resolveCollection(sf.Type)
				}
				if c.vector && !m.isFloat32Slice(sf.Type) {
					return nil, fmt.Errorf("%s.%s: %w: vector is only valid on []float32",
						q.typ, sf.Name, ErrInvalidTag)
				}

				if m.ResolveField != nil {
					fs := FieldSpec{Name: c.name, OmitEmpty: c.omitEmpty, OmitZero: c.omitZero,
						AutoID: c.autoID, IsRef: c.isRef, Ref: c.ref}
					m.ResolveField(q.typ, sf, &fs)
					if fs.Name == "" {
						continue
					}
					if err := validName(fs.Name); err != nil {
						return nil, fmt.Errorf("%s.%s: %w", q.typ, sf.Name, err)
					}
					c.name, c.omitEmpty, c.omitZero = fs.Name, fs.OmitEmpty, fs.OmitZero
					c.autoID, c.isRef, c.ref = fs.AutoID, fs.IsRef, fs.Ref

					c.isID = strings.EqualFold(c.name, idFieldName)
				}
				c.fold = foldName(c.name)
				cands = append(cands, c)
			}
		}
	}
	return m.finishPlan(t, cands)
}

// finishPlan 在候选里按名字挑出胜者，排好次序建成方案。
//
// 同名的挑一个：层级浅的赢，同层则写了标签名的赢。两条都分不出胜负时，
// 开了严格模式就报错，否则仍取排在前面那个。
//
// 胜出的字段最后按下标路径重排，好让编码出来的字段次序与结构体声明次序一致。
func (m *Mapper) finishPlan(t reflect.Type, cands []candidate) (*plan, error) {
	slices.SortStableFunc(cands, func(a, b candidate) int {
		if c := strings.Compare(a.fold, b.fold); c != 0 {
			return c
		}
		if c := cmp.Compare(a.depth, b.depth); c != 0 {
			return c
		}
		return cmp.Compare(boolRank(b.tagged), boolRank(a.tagged))
	})
	p := &plan{byFold: make(map[string]int), idIdx: -1}
	for i := 0; i < len(cands); {
		j := i + 1
		for j < len(cands) && cands[j].fold == cands[i].fold {
			j++
		}
		win := cands[i]
		if j-i > 1 {
			rival := cands[i+1]

			if rival.depth == win.depth && rival.tagged == win.tagged && m.StrictDuplicateFields {
				return nil, fmt.Errorf("%s: %w: %q comes from both %s and %s",
					t, ErrDuplicateField, win.name,
					win.index.goPath(t), rival.index.goPath(t))
			}
		}
		p.fields = append(p.fields, win.field)
		i = j
	}

	slices.SortFunc(p.fields, func(a, b field) int { return slices.Compare(a.index, b.index) })
	for i := range p.fields {
		f := &p.fields[i]
		p.byFold[f.fold] = i
		if f.isID {
			p.idIdx = i
		}
	}
	return p, nil
}

// boolRank 把布尔值变成可比较的 0 或 1。
func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

// child 在路径末尾接一层，返回新切片——不能就地追加，各条路径共享前缀。
func (fi fieldIndex) child(i int) fieldIndex {
	out := make(fieldIndex, len(fi)+1)
	copy(out, fi)
	out[len(fi)] = i
	return out
}

// valueIn 顺着路径取到字段值。
//
// 中途遇到空的内嵌指针就返回无效值——只读取，不分配。
func (fi fieldIndex) valueIn(rv reflect.Value) reflect.Value {
	for n, i := range fi {
		if n > 0 {
			if rv.Kind() == reflect.Pointer {
				if rv.IsNil() {
					return reflect.Value{}
				}
				rv = rv.Elem()
			}
		}
		rv = rv.Field(i)
	}
	return rv
}

// allocIn 顺着路径取到字段值，中途遇到空的内嵌指针就分配一个。
//
// 指针不可写时报错，那多半是未导出的内嵌字段。
func (fi fieldIndex) allocIn(rv reflect.Value) (reflect.Value, error) {
	for n, i := range fi {
		if n > 0 && rv.Kind() == reflect.Pointer {
			if rv.IsNil() {
				if !rv.CanSet() {
					return reflect.Value{}, fmt.Errorf("%w: cannot allocate unexported embedded pointer", ErrTarget)
				}
				rv.Set(reflect.New(rv.Type().Elem()))
			}
			rv = rv.Elem()
		}
		rv = rv.Field(i)
	}
	return rv, nil
}

// goPath 把下标路径还原成 Go 字段名的写法，报错时用。
func (fi fieldIndex) goPath(t reflect.Type) string {
	var b strings.Builder
	for n, i := range fi {
		if n > 0 {
			b.WriteByte('.')
		}
		if t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		sf := t.Field(i)
		b.WriteString(sf.Name)
		t = sf.Type
	}
	return b.String()
}

// Field 是对外暴露的一个字段说明，供外部按类型查看映射结果。
type Field struct {
	// Name 是文档字段名。
	Name string

	// GoName 是 Go 侧的字段路径，内嵌字段带上外层名字。
	GoName string

	// Type 是 Go 字段的类型。
	Type reflect.Type

	// IsID 表示这是主键字段。
	IsID bool

	// AutoID 表示主键为零值时交给数据库生成。
	AutoID bool

	// Ref 是引用指向的集合名；不是引用字段时为空。
	Ref string
}

// Fields 遍历一个结构体类型映射出来的字段。
//
// 不是结构体、或者建方案失败时，产出零项而不报错——调用方拿它来看一眼，
// 不该被一个坏类型卡住。
func (m *Mapper) Fields(t reflect.Type) iter.Seq[Field] {
	return func(yield func(Field) bool) {
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct {
			return
		}
		p, err := m.planFor(t)
		if err != nil {
			return
		}
		for i := range p.fields {
			f := &p.fields[i]
			if !yield(Field{
				Name:   f.name,
				GoName: f.index.goPath(t),
				Type:   f.typ,
				IsID:   f.isID,
				AutoID: f.autoID,
				Ref:    f.ref,
			}) {
				return
			}
		}
	}
}

// isFloat32Slice 判断是不是 []float32，vector 选项只对它有效。
func (m *Mapper) isFloat32Slice(t reflect.Type) bool {
	return t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Float32
}
