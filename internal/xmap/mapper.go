package xmap

import (
	"fmt"
	"net/url"
	"reflect"
	"regexp"
	"sync"
	"time"

	"github.com/xmapst/xdoc/internal/xbson"
)

// typeCodec 是一对登记好的自定义转换函数。
type typeCodec struct {
	enc func(reflect.Value) (*xbson.Value, error)
	dec func(*xbson.Value, reflect.Value) error
}

// Mapper 是一套映射规则。
//
// 零值不能直接用——内建转换器要靠 [New] 装上。取值方案缓存是并发安全的，
// 所以配好之后可以多协程共用；改配置或登记新类型会清空缓存，
// 那些操作应当在开始用之前做完。
type Mapper struct {
	// Naming 决定没写标签名的字段叫什么。为空时用 Go 字段名原样。
	Naming NameFunc

	// MaxDepth 是嵌套深度上限。不为正时用默认值 20。
	MaxDepth int

	// EmitNull 决定要不要把 Null 字段写进文档。默认不写，主键除外。
	EmitNull bool

	// TrimStrings 决定编码字符串时去不去首尾空白。[New] 默认打开。
	TrimStrings bool

	// EmptyStringToNull 决定空串是否编成 Null。[New] 默认打开，且在去空白之后判断。
	EmptyStringToNull bool

	// ResolveField 有机会逐字段改写映射设定，见 [FieldSpec]。
	//
	// 对每个候选字段都会调，包括从内嵌结构体提上来的。
	ResolveField func(owner reflect.Type, sf reflect.StructField, f *FieldSpec)

	// CollectionNaming 决定引用字段的默认集合名。为空时用 [CollectionNameOf]。
	CollectionNaming func(reflect.Type) string

	// BeforeDecode 在每次解码前有机会换掉输入值，返回 nil 表示不换。
	//
	// 对每一层都会调，不只是最外层。
	BeforeDecode func(target reflect.Type, v *xbson.Value) *xbson.Value

	// StrictDuplicateFields 决定字段名撞车时报错还是挑一个。
	//
	// 默认挑一个：层级浅的赢，同层则写了标签名的赢。真正分不出胜负
	// （同层、标签状态也相同）时，开了这个开关才报错。
	StrictDuplicateFields bool

	// types 是登记过的自定义转换器。
	types map[reflect.Type]typeCodec

	// subtypeNames 与 subtypes 是接口字段用的类型名双向表。
	subtypeNames map[reflect.Type]string
	subtypes     map[string]reflect.Type

	// cache 缓存每个结构体类型的取值方案，键是类型，值是 [planEntry]。
	cache sync.Map
}

// defaultMaxDepth 是没设 MaxDepth 时的嵌套深度上限。
const defaultMaxDepth = 20

// Default 是包级函数用的那个映射器。
var Default = New()

// New 造一个映射器，装上内建转换器，并打开字符串的去空白与空串转 Null。
func New() *Mapper {
	m := &Mapper{
		TrimStrings:       true,
		EmptyStringToNull: true,
		types:             make(map[reflect.Type]typeCodec),
	}
	m.registerBuiltins()
	return m
}

// registerBuiltins 装上几个标准库类型的转换器。
//
// 时间间隔按**百纳秒**存成 64 位整数——那是本库的时间刻度单位，
// 与 Go 的纳秒差一百倍。
func (m *Mapper) registerBuiltins() {
	m.RegisterType[url.URL](
		func(u url.URL) (*xbson.Value, error) { return xbson.String(u.String()), nil },
		func(v *xbson.Value) (url.URL, error) {
			s, ok := v.AsString()
			if !ok {
				return url.URL{}, fmt.Errorf("%w: a URL must be stored as a string, got %s", ErrTypeMismatch, v.Type())
			}
			u, err := url.Parse(s)
			if err != nil {
				return url.URL{}, err
			}
			return *u, nil
		})

	m.RegisterType[time.Duration](
		func(d time.Duration) (*xbson.Value, error) { return xbson.Int64(int64(d) / 100), nil },
		func(v *xbson.Value) (time.Duration, error) {
			n, ok := v.AsInt64()
			if !ok {
				return 0, fmt.Errorf("%w: a duration must be stored as a 64-bit integer, got %s", ErrTypeMismatch, v.Type())
			}
			return time.Duration(n) * 100, nil
		})

	m.RegisterType[regexp.Regexp](
		func(r regexp.Regexp) (*xbson.Value, error) { return xbson.String(r.String()), nil },
		func(v *xbson.Value) (regexp.Regexp, error) {
			s, ok := v.AsString()
			if !ok {
				return regexp.Regexp{}, fmt.Errorf("%w: a regular expression must be stored as a string, got %s",
					ErrTypeMismatch, v.Type())
			}
			re, err := regexp.Compile(s)
			if err != nil {
				return regexp.Regexp{}, err
			}
			return *re, nil
		})
}

// RegisterType 给一个类型登记编解码函数，此后它整体对应一个文档值。
//
// 登记会清空取值方案缓存。字段全未导出的结构体只有这一条路能编码。
func (m *Mapper) RegisterType[T any](enc func(T) (*xbson.Value, error), dec func(*xbson.Value) (T, error)) {
	t := reflect.TypeFor[T]()
	if m.types == nil {
		m.types = make(map[reflect.Type]typeCodec)
	}
	m.types[t] = typeCodec{
		enc: func(rv reflect.Value) (*xbson.Value, error) {
			v, ok := rv.Interface().(T)
			if !ok {
				return nil, fmt.Errorf("%w: registered converter received %s", ErrPanic, rv.Type())
			}
			return enc(v)
		},
		dec: func(v *xbson.Value, rv reflect.Value) error {
			out, err := dec(v)
			if err != nil {
				return err
			}
			rv.Set(reflect.ValueOf(out))
			return nil
		},
	}
	m.cache.Clear()
}

// maxDepth 返回生效的深度上限。
func (m *Mapper) maxDepth() int {
	if m.MaxDepth > 0 {
		return m.MaxDepth
	}
	return defaultMaxDepth
}

// FieldSpec 是 [Mapper] 的 ResolveField 回调能改的那部分字段设定。
type FieldSpec struct {
	// Name 是文档字段名。回调把它改成空串表示跳过这个字段。
	Name string

	OmitEmpty bool
	OmitZero  bool

	// AutoID 表示主键为零值时交给数据库生成。
	AutoID bool

	// IsRef 表示这是一个引用字段。
	IsRef bool
	// Ref 是被引对象所在的集合名。
	Ref string
}

// resolveCollection 求一个类型对应的集合名。
func (m *Mapper) resolveCollection(t reflect.Type) string {
	if m.CollectionNaming == nil {
		return CollectionNameOf(t)
	}
	return m.CollectionNaming(t)
}

// resolveName 把 Go 字段名映射成文档字段名。
func (m *Mapper) resolveName(goName string) string {
	if m.Naming == nil {
		return goName
	}
	return m.Naming(goName)
}

// Marshal 用默认映射器把一个 Go 值编成文档值。
func Marshal(v any) (*xbson.Value, error) { return Default.Marshal(v) }

// Unmarshal 用默认映射器把一个文档值解到 out 指向的地方。
func Unmarshal(v *xbson.Value, out any) error { return Default.Unmarshal(v, out) }

// MarshalDocument 用默认映射器编成一篇文档。
func MarshalDocument(v any) (*xbson.Document, error) { return Default.MarshalDocument(v) }

// Marshal 把一个 Go 值编成文档值。过程中的 panic 会被拦成错误。
func (m *Mapper) Marshal(v any) (val *xbson.Value, err error) {
	defer guard(&err)
	st := m.newState()
	return m.encode(reflect.ValueOf(v), st)
}

// MarshalDocument 编成一篇文档，编出来不是文档就报错。
func (m *Mapper) MarshalDocument(v any) (*xbson.Document, error) {
	val, err := m.Marshal(v)
	if err != nil {
		return nil, err
	}
	doc, ok := val.AsDocument()
	if !ok {
		return nil, &Error{Err: fmt.Errorf("%w: top level must be a document, got %s", ErrTypeMismatch, val.Type())}
	}
	return doc, nil
}

// Unmarshal 把一个文档值解到 out，out 必须是非空指针。v 为 nil 时当 Null。
func (m *Mapper) Unmarshal(v *xbson.Value, out any) (err error) {
	defer guard(&err)
	if out == nil {
		return &Error{Err: fmt.Errorf("%w: out is nil", ErrTarget)}
	}
	rv := reflect.ValueOf(out)
	if rv.Kind() != reflect.Pointer {
		return &Error{GoType: rv.Type().String(), Err: fmt.Errorf("%w: out must be a pointer", ErrTarget)}
	}
	if rv.IsNil() {
		return &Error{GoType: rv.Type().String(), Err: fmt.Errorf("%w: out is a nil pointer", ErrTarget)}
	}
	if v == nil {
		v = xbson.Null
	}
	return m.decode(v, rv.Elem(), m.newState())
}

// guard 把 panic 拦成 [ErrPanic]。
//
// 反射代码踩到意外的类型时会 panic，不该让它冲出库外。
func guard(err *error) {
	if r := recover(); r != nil {
		*err = &Error{Err: fmt.Errorf("%w: %v", ErrPanic, r)}
	}
}
