package xbexpr

import (
	"math"
	"strconv"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/xmapst/xdoc/internal/xbson"
)

// Print 把语法树还原成表达式文本。
//
// 还原出来的文本再解析一遍应当得到同一棵树：这是索引表达式能按文本
// 存进集合页、下次打开再解析回来的前提。
func Print(n Node) string {
	if n == nil {
		return ""
	}
	return string(appendNode(make([]byte, 0, 64), n))
}

// String 还原成表达式文本。
func (n *ConstNode) String() string { return Print(n) }

// String 还原成表达式文本。
func (n *PathNode) String() string { return Print(n) }

// String 还原成表达式文本。
func (n *DocumentNode) String() string { return Print(n) }

// String 还原成表达式文本。
func (n *ArrayNode) String() string { return Print(n) }

// String 还原成表达式文本。
func (n *ParameterNode) String() string { return Print(n) }

// String 还原成表达式文本。
func (n *ParenNode) String() string { return Print(n) }

// String 还原成表达式文本。
func (n *CallNode) String() string { return Print(n) }

// String 还原成表达式文本。
func (n *FuncNode) String() string { return Print(n) }

// String 还原成表达式文本。
func (n *BinaryNode) String() string { return Print(n) }

// String 还原成表达式文本。
func (n *SourceNode) String() string { return Print(n) }

// appendNode 把一个节点写进字节缓冲；空节点什么也不写。
func appendNode(b []byte, n Node) []byte {
	if n == nil {
		return b
	}
	return n.appendTo(b)
}

// appendTo 写出字面量。
func (n *ConstNode) appendTo(b []byte) []byte {
	return appendConst(b, n.value())
}

// appendConst 写出一个常量值。没有专门写法的类型退回它自己的文本形式。
func appendConst(b []byte, v *xbson.Value) []byte {
	switch v.Type() {
	case xbson.TypeNull:
		return append(b, "null"...)
	case xbson.TypeBoolean:
		if ok, _ := v.AsBoolean(); ok {
			return append(b, "true"...)
		}
		return append(b, "false"...)
	case xbson.TypeInt32:
		i, _ := v.AsInt32()
		return strconv.AppendInt(b, int64(i), 10)
	case xbson.TypeInt64:
		i, _ := v.AsInt64()
		return strconv.AppendInt(b, i, 10)
	case xbson.TypeDouble:
		f, _ := v.AsDouble()
		return appendDouble(b, f)
	case xbson.TypeString:
		s, _ := v.AsString()
		return appendQuoted(b, s)
	}

	return append(b, v.String()...)
}

// appendDouble 写出一个双精度数。
//
// 绝对值适中时用定点写法，太大太小才用指数写法。写完若既没有小数点
// 也没有指数，补上 .0——否则再解析回来会变成整数。
func appendDouble(b []byte, f float64) []byte {
	format := byte('g')
	if a := math.Abs(f); a == 0 || (a >= 1e-6 && a < 1e21) {
		format = 'f'
	}
	start := len(b)
	b = strconv.AppendFloat(b, f, format, -1, 64)
	for _, c := range b[start:] {
		if c == '.' || c == 'e' || c == 'E' {
			return b
		}
	}
	return append(b, ".0"...)
}

// printableCategories 是可以直接写进字符串的那些 Unicode 分类。
//
// 字母、数字、空格、标点、符号原样写出；控制字符、格式字符、未分配码位
// 以及行分隔符都转义成 \u 形式。
var printableCategories = []*unicode.RangeTable{
	unicode.Lu, unicode.Ll, unicode.Lt, unicode.Lo,
	unicode.Nd, unicode.Nl, unicode.No,
	unicode.Zs,
	unicode.Pc, unicode.Pd, unicode.Ps, unicode.Pe, unicode.Pi, unicode.Pf, unicode.Po,
	unicode.Sm, unicode.Sc, unicode.Sk, unicode.So,
}

// appendQuoted 写出一个双引号字符串，该转义的转义。
//
// 超出基本多文种平面的字符拆成代理对，写成两个 \u 转义。
func appendQuoted(b []byte, s string) []byte {
	b = append(b, '"')
	for _, r := range s {
		switch r {
		case '"':
			b = append(b, '\\', '"')
			continue
		case '\\':
			b = append(b, '\\', '\\')
			continue
		case '\b':
			b = append(b, '\\', 'b')
			continue
		case '\f':
			b = append(b, '\\', 'f')
			continue
		case '\n':
			b = append(b, '\\', 'n')
			continue
		case '\r':
			b = append(b, '\\', 'r')
			continue
		case '\t':
			b = append(b, '\\', 't')
			continue
		}
		if r > 0xFFFF {
			hi, lo := utf16.EncodeRune(r)
			b = appendUnicodeEscape(b, uint16(hi))
			b = appendUnicodeEscape(b, uint16(lo))
			continue
		}
		if unicode.In(r, printableCategories...) {
			b = utf8.AppendRune(b, r)
			continue
		}
		b = appendUnicodeEscape(b, uint16(r))
	}
	return append(b, '"')
}

// hexDigits 是 \u 转义用的小写十六进制数字。
const hexDigits = "0123456789abcdef"

// appendUnicodeEscape 写出一个四位 \u 转义。
func appendUnicodeEscape(b []byte, u uint16) []byte {
	return append(b, '\\', 'u',
		hexDigits[u>>12&0xF], hexDigits[u>>8&0xF], hexDigits[u>>4&0xF], hexDigits[u&0xF])
}

// appendName 写出一个键名：是合法标识符就裸写，否则加引号。
func appendName(b []byte, name string) []byte {
	if isWord(name) {
		return append(b, name...)
	}
	return appendQuoted(b, name)
}

// appendTo 写出一条路径。
//
// 字段名不是合法标识符时写成 .["名字"] 的形式。
func (n *PathNode) appendTo(b []byte) []byte {
	if n == nil {
		return append(b, '$')
	}
	b = append(b, n.Root.String()...)
	for _, s := range n.Steps {
		switch s.Kind {
		case StepField:
			if isWord(s.Name) {
				b = append(b, '.')
				b = append(b, s.Name...)
			} else {
				b = append(b, '.', '[')
				b = appendQuoted(b, s.Name)
				b = append(b, ']')
			}
		case StepIndex:
			b = append(b, '[')
			b = strconv.AppendInt(b, int64(s.Index), 10)
			b = append(b, ']')
		case StepAll:
			b = append(b, '[', '*', ']')
		case StepParamIndex, StepFilter:
			b = append(b, '[')
			b = appendNode(b, s.Expr)
			b = append(b, ']')
		}
	}
	return b
}

// appendTo 写出文档字面量。键值一律写全，不用省略值的简写。
func (n *DocumentNode) appendTo(b []byte) []byte {
	b = append(b, '{')
	if n != nil {
		for i, f := range n.Fields {
			if i > 0 {
				b = append(b, ',')
			}
			b = appendName(b, f.Key)
			b = append(b, ':')
			b = appendNode(b, f.Value)
		}
	}
	return append(b, '}')
}

// appendTo 写出数组字面量。
func (n *ArrayNode) appendTo(b []byte) []byte {
	b = append(b, '[')
	if n != nil {
		for i, it := range n.Items {
			if i > 0 {
				b = append(b, ',')
			}
			b = appendNode(b, it)
		}
	}
	return append(b, ']')
}

// appendTo 写出具名参数。
func (n *ParameterNode) appendTo(b []byte) []byte {
	if n == nil {
		return b
	}
	return append(append(b, '@'), n.Name...)
}

// appendTo 写出括号。
func (n *ParenNode) appendTo(b []byte) []byte {
	if n == nil {
		return b
	}
	b = append(b, '(')
	b = appendNode(b, n.Inner)
	return append(b, ')')
}

// appendTo 写出方法调用，方法名一律大写。
//
// 解析时自动套上的 ITEMS() 写回原来的 [*] 形式，见 [wrapItems]。
func (n *CallNode) appendTo(b []byte) []byte {
	if n == nil {
		return b
	}

	if n.PathItems && len(n.Args) == 1 {
		return append(appendNode(b, n.Args[0]), '[', '*', ']')
	}
	b = append(b, upperASCII(n.Name)...)
	b = append(b, '(')
	for i, a := range n.Args {
		if i > 0 {
			b = append(b, ',')
		}
		b = appendNode(b, a)
	}
	return append(b, ')')
}

// appendTo 写出带 lambda 的内建函数调用。
func (n *FuncNode) appendTo(b []byte) []byte {
	if n == nil {
		return b
	}
	b = append(b, n.Name.String()...)
	b = append(b, '(')
	b = appendNode(b, n.Input)
	if n.Lambda != nil {
		b = append(b, '=', '>')
		b = appendNode(b, n.Lambda)
	}
	for _, a := range n.Args {
		b = append(b, ',')
		b = appendNode(b, a)
	}
	return append(b, ')')
}

// appendTo 写出二元运算。
//
// BETWEEN 的右边是打包过的两项数组，要拆回 `低 AND 高` 的写法。
// 减号后面紧跟负数字面量时补一个空格，否则会连成两个减号——那是行注释的开头。
func (n *BinaryNode) appendTo(b []byte) []byte {
	if n == nil {
		return b
	}
	b = appendNode(b, n.Left)
	op := n.Op
	b = append(b, op.Text()...)

	if op.Base() == OpBetween {
		if arr, ok := n.Right.(*ArrayNode); ok && arr != nil && len(arr.Items) == 2 {
			b = appendNode(b, arr.Items[0])
			b = append(b, " AND "...)
			return appendNode(b, arr.Items[1])
		}

		return appendNode(b, n.Right)
	}

	right := appendNode(nil, n.Right)

	if op == OpSub && len(right) > 0 && right[0] == '-' {
		b = append(b, ' ')
	}
	return append(b, right...)
}

// appendTo 写出数据源，也就是一个星号。
func (n *SourceNode) appendTo(b []byte) []byte {
	return append(b, '*')
}
