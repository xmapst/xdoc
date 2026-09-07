package xfmt

import "strings"

// ErrBadFormat 表示格式串不认识。
var ErrBadFormat = errBadFormat

// Guid 是 8-4-4-4-12 形式的标识文本，只为按各种写法排版而存在。
type Guid string

// Format 按写法代号排版：D 带连字符、N 不带、B 加花括号、P 加圆括号、
// X 排成 C 语言初始化式。大小写都认，空串等同 D。
func (g Guid) Format(format string) (string, error) {
	s := string(g)
	switch format {
	case "", "D", "d":
		return s, nil
	case "N", "n":
		return strings.ReplaceAll(s, "-", ""), nil
	case "B", "b":
		return "{" + s + "}", nil
	case "P", "p":
		return "(" + s + ")", nil
	case "X", "x":
		return g.hex(), nil
	}
	return "", errBadFormat
}

// hex 排成 C 语言初始化式：前三段各一个整数，后两段拆成一串字节。
//
// 段数不对时原样返回：这里不该拿一个残缺的输入去造一个看似合法的输出。
func (g Guid) hex() string {
	p := strings.Split(string(g), "-")
	if len(p) != 5 {
		return string(g)
	}
	var sb strings.Builder
	sb.WriteString("{0x" + p[0] + ",0x" + p[1] + ",0x" + p[2] + ",{")
	tail := p[3] + p[4]
	for i := 0; i+2 <= len(tail); i += 2 {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString("0x" + tail[i:i+2])
	}
	sb.WriteString("}}")
	return sb.String()
}
