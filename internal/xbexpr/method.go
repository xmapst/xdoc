package xbexpr

import (
	"strconv"
	"strings"

	"github.com/xmapst/xdoc/internal/xbson"
	"github.com/xmapst/xdoc/internal/xcoll"
)

// Ctx 是方法求值时能看到的外部环境。
type Ctx struct {
	// Root 是整篇文档，路径里的 $ 指它。
	Root *xbson.Value
	// Collation 决定字符串怎么比大小。
	Collation xcoll.Collation
	// Params 是具名参数表，[ParameterNode] 从这里取值。
	Params *xbson.Document
}

// Method 是一个内建方法的实现。实参个数已经由 [Lookup] 核过。
type Method func(ctx *Ctx, args []*xbson.Value) (*xbson.Value, error)

// ParamKind 说明一个形参收的是一个值还是一串值。
type ParamKind uint8

const (
	// ParamScalar 收一个值。
	ParamScalar ParamKind = iota

	// ParamSeq 收一串值。
	ParamSeq
)

// Info 是一个方法的签名信息。
type Info struct {
	// Params 逐个说明各形参收什么，长度就是实参个数。
	Params []ParamKind
	// ReturnsSeq 表示这个方法产出一串值。
	ReturnsSeq bool

	// Volatile 表示同样的输入也可能算出不同结果，比如取当前时间。
	//
	// 易变的方法不能用来建索引，见 [IsIndexable]。
	Volatile bool
}

// def 是方法表里的一项：实现加签名。
type def struct {
	fn   Method
	info Info
}

// registry 是全部内建方法，键由方法名和实参个数拼成。
//
// 同名不同实参个数算两个方法，各自注册。各 method_*.go 在 init 里往里填。
var registry = map[string]def{}

// lambdaRegistry 是带 lambda 的那些方法，见 method_lambda.go。
var lambdaRegistry = map[string]lambdaDef{}

// key 把方法名与实参个数拼成表键，名字一律转大写——方法名不分大小写。
func key(name string, argc int) string {
	return strings.ToUpper(name) + "~" + strconv.Itoa(argc)
}

// reg 登记一个方法，实参个数取自签名。
func reg(name string, fn Method, info Info) {
	registry[key(name, len(info.Params))] = def{fn: fn, info: info}
}

// scalars 造一份 n 个标量形参的签名，[ParamScalar] 正好是零值。
func scalars(n int) []ParamKind { return make([]ParamKind, n) }

// Lookup 按名字和实参个数找方法，找不到返回 nil。
//
// 返回的是一层包装：实参个数不符时报错，ctx 为空时给一个最小的默认环境，
// nil 实参一律换成 Null——各方法实现因此不必自己判空。
func Lookup(name string, argc int) Method {
	d, ok := registry[key(name, argc)]
	if !ok {
		return nil
	}
	want := len(d.info.Params)
	fn := d.fn
	return func(ctx *Ctx, args []*xbson.Value) (*xbson.Value, error) {
		if len(args) != want {
			return nil, errf("method %s expects %d argument(s), got %d", strings.ToUpper(name), want, len(args))
		}
		if ctx == nil {
			ctx = &Ctx{Root: xbson.Null, Collation: xcoll.Binary}
		}
		for i, a := range args {
			if a == nil {
				args[i] = xbson.Null
			}
		}
		return fn(ctx, args)
	}
}

// MethodInfo 取这次调用对应的方法签名。
func (n *CallNode) MethodInfo() (Info, bool) {
	if n == nil {
		return Info{}, false
	}
	d, ok := registry[key(n.Name, len(n.Args))]
	if !ok {
		return Info{}, false
	}
	return d.info, true
}

// Names 返回全部方法的表键，已排序。键的形式是「大写名~实参个数」。
func Names() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

// sortStrings 就地插入排序。方法表不大，不值得为它引入排序包。
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// MethodCount 返回已登记的方法数，同名不同实参个数分别计数。
func MethodCount() int { return len(registry) }
