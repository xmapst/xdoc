package xdoc

import (
	"io"

	"github.com/xmapst/xdoc/internal/xjson"
)

// MarshalJSON 把一个值写成紧凑 JSON，不带任何多余空白。
//
// 文档模型的类型比 JSON 多，多出来的那些借一个单键对象表示，键名以 $ 开头、
// 载荷是字符串，例如 64 位整数写成 {"$numberLong":"5"}。写出的文本任何标准
// JSON 解析器都读得动，只是读到的是包装对象而不是原类型；要还原成原类型走
// [UnmarshalJSON]。
//
// 向量没有自己的记法，写成普通浮点数组，读回来是双精度数组——类型在这一步就丢了。
//
// 写文档时主键字段（_id，大小写不敏感）被挪到最前，其余字段保持原有顺序。
func MarshalJSON(v *Value) ([]byte, error) { return xjson.Marshal(v) }

// MarshalJSONIndent 把一个值写成缩进 4 个空格的美化 JSON。
// 扩展类型的写法与字段顺序都与 [MarshalJSON] 一致。
func MarshalJSONIndent(v *Value) ([]byte, error) { return xjson.MarshalIndent(v) }

// MarshalJSONIndentWidth 与 [MarshalJSONIndent] 一样，但缩进宽度由调用方给。
// 宽度小于 0 按 0 算：各级不缩进，块的括号仍各占一行。
func MarshalJSONIndentWidth(v *Value, width int) ([]byte, error) {
	return xjson.MarshalIndentWidth(v, width)
}

// JSONWriter 往一个流里连续写多个值，边拼边刷，不必先在内存里拼出整篇文本。
// Pretty 与 Indent 两个字段控制排版。
type JSONWriter = xjson.Writer

// NewJSONWriter 建一个流式写入器。
func NewJSONWriter(w io.Writer) *JSONWriter { return xjson.NewWriter(w) }

// UnmarshalJSON 把一段 JSON 文本解析成一个值，认得 [MarshalJSON] 写出的扩展记法。
//
// 空输入与只有空白的输入解析成 Null。读完一个值就收工，值之后的内容不看：
// `{"a":1} 尾巴` 得到的是 {a:1}，不报错。
func UnmarshalJSON(b []byte) (*Value, error) { return xjson.Unmarshal(b) }

// JSONReader 从一个流里连续读多个值，不必把整份文本先读进内存。
type JSONReader = xjson.Reader

// NewJSONReader 建一个流式读取器。
func NewJSONReader(r io.Reader) *JSONReader { return xjson.NewReader(r) }
