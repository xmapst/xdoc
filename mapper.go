package xdoc

import (
	"reflect"

	"github.com/xmapst/xdoc/internal/xmap"
)

// TagKey 是结构体标签的键名。映射规则写在 `bson:"..."` 里，形如：
//
//	bson:"名字,选项1,选项2=值"
//
// 逗号前是文档里的字段名，逗号后是零个或多个选项。名字这一段的完整规则：
//
//	bson:"-"            忽略该字段，既不写出也不读入
//	bson:"-,"           字段名就是一个减号（与上一条区分）
//	bson:""             等价于不写标签：字段名由命名策略从 Go 字段名推出
//	bson:"age"          字段名固定为 age，不过命名策略
//
// 选项顺序无关，重复出现按最后一次算：
//
//	omitempty   零长度、nil、数值 0、false、空串时不写出该字段
//	omitzero    整个值等于其类型零值时不写出；与 omitempty 的区别是它也认结构体
//	            （比如零值 time.Time），omitempty 对结构体一律视为非空
//	id          该字段是主键。名字写成 _id（大小写不敏感）时自动带上这一档
//	noauto      关掉主键自增，插入时不替调用方生成主键。自增开着（默认）时主键
//	            字段取零值就整个不写出——零值的意思正是「替我生成一个」；关掉之后
//	            零值是一个正当的主键取值，照常写出
//	ref=集合名   该字段是一个引用，只写出 {$id, $ref} 这层壳，不展开被引对象
//	ref         同上，但集合名由 [CollectionNameOf] 从被引类型推出
//	inline      把该字段的结构体成员摊平到当前这一层
//	vector      该字段（必须是 []float32）按向量类型写出，而不是浮点数组
//
// 除此之外的选项名一律报 [ErrInvalidTag]：拼错一个选项名如果被静默忽略，
// 表现出来就是「标签写了但没生效」，而这类问题往往要到数据已经写坏了才被发现。
// inline 与 id/ref 同用也报错。
//
// 字段名里不能带点号，也不能以美元号开头，否则同样是 [ErrInvalidTag]：点号是查询
// 表达式里的路径分隔符，美元号是引用壳的保留前缀，用了它们的字段写得出去却永远
// 选不中。
//
// 主键按两级规则找，命中即止：先是带 id 选项、或把名字直接写成 _id 的字段，
// 再是 Go 字段名（忽略大小写）为 ID 的字段。两级都没命中就是这个类型没有主键。
const TagKey = xmap.TagKey

// TypeKey 是多态字段用的类型判别字段名，值是 `_type`。
//
// 一个字段声明成接口时，写进文档的是**某一个**实现的形状，而文档本身记不住那是
// 哪一个——读回来时只知道「这里该放一个 Shape」，不知道当初放的是圆还是方。
// 这个字段就是把那件事记下来的地方，它排在文档最前面，名字与位置都是格式的一部分。
//
// 具体类型用 [Mapper] 的 RegisterSubtype 登记名字，登记过的才读得回来。
const TypeKey = xmap.TypeKey

// NameFunc 是命名策略：把 Go 字段名换算成文档里的字段名。
// 只在字段没有用标签显式指定名字时才被调用。
type NameFunc = xmap.NameFunc

// Field 是映射器算出来的一个字段的最终形态，由 [Mapper] 的 Fields 交出来。
type Field = xmap.Field

// FieldSpec 是 [Mapper] 的 ResolveField 钩子拿到的可改写的字段描述。
// 把它的 Name 置空表示这个字段不出现在文档里。
type FieldSpec = xmap.FieldSpec

// MappingError 带着出错的文档路径与 Go 类型，便于定位是哪一个字段。
type MappingError = xmap.Error

// 映射这一层的哨兵错误。用 [errors.Is] 判别，不要去比对错误文本。
var (
	// ErrUnsupportedType 表示 Go 类型没有对应的文档表示，比如通道、函数、复数。
	//
	// 字段全是非导出的结构体（netip.Addr、big.Int 这一类）也归它：按结构体展开
	// 只会得到一篇空文档，而写读两头都不报错，等发现时库里已经攒了一批取不回来
	// 的记录。给这类型注册一对转换函数即可。
	ErrUnsupportedType = xmap.ErrUnsupportedType

	// ErrMaxDepth 表示嵌套超过了 [Mapper] 的 MaxDepth。
	//
	// 成环的对象图也从这里报出来：本库不做环检测，只有深度上限这一道闸，
	// 所以环的症状是「太深了」而不是「哪个字段绕回去了」。
	ErrMaxDepth = xmap.ErrMaxDepth

	// ErrTarget 表示反序列化的目标不可用：不是指针、是 nil 指针、或者不可写。
	ErrTarget = xmap.ErrTarget

	// ErrTypeMismatch 表示文档里的值与目标 Go 类型对不上。
	ErrTypeMismatch = xmap.ErrTypeMismatch

	// ErrOverflow 表示数值放不进目标类型。
	ErrOverflow = xmap.ErrOverflow

	// ErrNoPrimaryKey 表示类型上找不到主键字段。
	//
	// 它未必是个问题：没有主键字段是很正常的一种设计，插入时由库替它生成一个。
	ErrNoPrimaryKey = xmap.ErrNoPrimaryKey

	// ErrInvalidTag 表示结构体标签语法错误，写法见 [TagKey]。
	ErrInvalidTag = xmap.ErrInvalidTag

	// ErrDuplicateField 表示同一层里两个字段抢同一个文档字段名。
	//
	// 只在把 [Mapper] 的 StrictDuplicateFields 打开之后才出现；默认档位静默丢掉
	// 后来者，而丢的是哪一个取决于字段的声明顺序。
	ErrDuplicateField = xmap.ErrDuplicateField

	// ErrPanic 表示反射路径上出现了本不该出现的 panic，已被兜底转成错误。
	//
	// 任何输入都只该得到错误而不是让进程挂掉，所以出现它一定是本库的缺陷，
	// 请连同触发的类型一起报上来。
	ErrPanic = xmap.ErrPanic
)

// NewMapper 建一个映射器，配好之后用 [WithMapper] 交给库。
//
// 它与 &Mapper{} 零值的差别只有三处：TrimStrings 与 EmptyStringToNull 被打开
// （两者都会改数据，见各自的说明），以及预置 url.URL、time.Duration、
// regexp.Regexp 三条类型转换。要一个什么都不做的映射器就直接用 &Mapper{}。
//
// 配置字段与类型注册都必须在第一次转换之前设完：字段计划按类型缓存，而计划里
// 已经烤进了当时的命名策略，事后改配置不会让缓存失效。定下来之后同一个映射器
// 可以被任意多个 goroutine 同时使用。
func NewMapper() *Mapper { return xmap.New() }

// DefaultMapper 是包级函数（[MarshalValue]、[Doc]、[Val] 这些）用的映射器。
//
// 它是进程级共享的可写变量：要改请在程序启动时改，之后当只读用。
// 库自己那份由 [WithMapper] 决定，与它无关。
func DefaultMapper() *Mapper { return xmap.Default }

// NameAsIs 原样使用 Go 字段名，是默认策略。
//
// 默认不做任何变换是为了可预测：读结构体定义就知道文档里长什么样，
// 不必先在脑子里跑一遍转换规则。
func NameAsIs(s string) string { return xmap.NameAsIs(s) }

// NameCamelCase 把首个字符转成小写，其余字符一个不动（ID → iD，HTTPPort → hTTPPort）。
//
// 它按字符算，不认单词，所以开头的连续大写不会整段降下来。
func NameCamelCase(s string) string { return xmap.NameCamelCase(s) }

// NameSnakeCase 在每个不在首位的大写字母前插入 delim，最后整串小写
// （UserID → user_i_d，HTTPPort → h_t_t_p_port）。
//
// 判据是逐字符的，不认单词边界，所以连续大写会被拆散。只有 ASCII 的 A-Z 触发
// 插入，非 ASCII 的大写字母不触发但仍会被最后那次整体小写降下来。
func NameSnakeCase(delim byte) NameFunc { return xmap.NameSnakeCase(delim) }

// CollectionNameOf 从类型推出默认集合名：指针与切片先剥到元素类型，再取类型名。
//
// 标签里的 ref 选项没写集合名时用它来定被引集合，它也是 [Mapper] 的
// CollectionNaming 字段留空时的行为。
func CollectionNameOf(t reflect.Type) string { return xmap.CollectionNameOf(t) }

// MarshalValue 把一个 Go 值转成字段值，用的是 [DefaultMapper]。
//
// 要用库自己那份映射器（[WithMapper] 换过的），走 [DB.Marshal]。
func MarshalValue(v any) (*Value, error) { return xmap.Marshal(v) }

// MarshalDocument 把一个 Go 结构体转成文档，用的是 [DefaultMapper]。
func MarshalDocument(v any) (*Document, error) { return xmap.MarshalDocument(v) }

// UnmarshalValue 把一个字段值解进 out，out 必须是非 nil 指针。
func UnmarshalValue(v *Value, out any) error { return xmap.Unmarshal(v, out) }

// UnmarshalAs 把一个字段值解成 T，类型从实参来而不是从 out 来。
func UnmarshalAs[T any](v *Value) (T, error) { return xmap.UnmarshalAs[T](v) }
