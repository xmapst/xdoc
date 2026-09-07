package xbexpr

import "strings"

// Fields 收集表达式引用到的顶层字段名，按出现次序去重。
//
// 只看路径的第一步：$.a.b.c 记的是 a。第一步不是字段（比如 $[0]），
// 或者整个就是 $、就是数据源，都记成 "$"——那表示要用到整篇文档。
//
// 去重按大小写不敏感比，但返回的是首次出现时的原样写法。
func Fields(n Node) []string {
	var out []string
	seen := map[string]bool{}
	add := func(name string) {
		k := strings.ToUpper(name)
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, name)
	}
	for node := range Walk(n) {
		switch t := node.(type) {
		case *PathNode:
			if t == nil || !t.IsRooted() {
				continue
			}
			if len(t.Steps) == 0 || t.Steps[0].Kind != StepField {
				add("$")
				continue
			}
			add(t.Steps[0].Name)
		case *SourceNode:
			add("$")
		}
	}
	return out
}

// IsRooted 判断这条路径是否从文档或数据源起步，而不是从当前项起步。
func (n *PathNode) IsRooted() bool {
	return n.Root == RootDocument || n.Scope == ScopeSource
}

// DefaultFieldName 给一个没写别名的表达式取个字段名：把引用到的字段名用下划线连起来。
//
// 只引用了整篇文档、或者什么字段都没引用时，退回 "expr"。
func DefaultFieldName(n Node) string {
	var parts []string
	for _, f := range Fields(n) {
		if f == "$" {
			continue
		}
		parts = append(parts, f)
	}
	if len(parts) == 0 {
		return "expr"
	}
	return strings.Join(parts, "_")
}
