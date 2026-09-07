package xmap

import (
	"reflect"
	"strings"
	"unicode"
)

// NameFunc 把 Go 字段名映射成文档字段名。
type NameFunc func(goName string) string

// NameAsIs 原样保留 Go 字段名。
func NameAsIs(s string) string { return s }

// NameCamelCase 把首字母改成小写。
func NameCamelCase(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToLower(r[0])
	return string(r)
}

// NameSnakeCase 造一个下划线（或别的分隔符）风格的命名函数。
//
// 在每个大写字母前插入分隔符，最后整体转小写。连续大写会被逐个拆开：
// ID 变成 i_d。
func NameSnakeCase(delim byte) NameFunc {
	return func(s string) string {
		var b strings.Builder
		b.Grow(len(s) + 4)
		for i, c := range s {
			if i > 0 && c >= 'A' && c <= 'Z' {
				b.WriteByte(delim)
			}
			b.WriteRune(c)
		}
		return strings.ToLower(b.String())
	}
}

// CollectionNameOf 由类型推出集合名：剥掉指针、切片、数组，取里层的类型名。
//
// 匿名类型没有名字，退回它的完整写法。
func CollectionNameOf(t reflect.Type) string {
	for {
		switch t.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array:
			t = t.Elem()
		default:
			if name := t.Name(); name != "" {
				return name
			}
			return t.String()
		}
	}
}
