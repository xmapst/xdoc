package xbexpr

// IsIndexable 判断一个表达式能不能拿来建索引。
//
// 三个条件：至少引用一个文档字段，不含参数，不含易变方法。
// 参数和易变方法（比如取当前时间）每次算出来都可能不同，
// 拿它们建的索引第二天就对不上了。
func IsIndexable(n Node) bool {
	hasField := false
	for x := range Walk(n) {
		switch t := x.(type) {
		case *PathNode:
			if t != nil && t.Root == RootDocument {
				hasField = true
			}
		case *ParameterNode:
			return false
		case *CallNode:
			if t == nil {
				continue
			}
			if info, ok := t.MethodInfo(); ok && info.Volatile {
				return false
			}
		}
	}
	return hasField
}
